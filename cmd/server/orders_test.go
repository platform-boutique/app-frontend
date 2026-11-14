package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "frontend/genproto"
	pbo "frontend/genproto/orders"
)

// ---------------------------------------------------------------------------
// fake order client (implements pbo.OrderServiceClient, no network)
// ---------------------------------------------------------------------------

type fakeOrders struct {
	mu        sync.Mutex
	orders    []*pbo.Order // newest first
	getCalls  []*pbo.GetOrderRequest
	listCalls []*pbo.ListOrdersRequest
	err       error // returned by every call when set
	block     bool  // wait for the caller's deadline, like a hung service
	getHook   func(*pbo.Order) *pbo.Order
	claims    []*pbo.ClaimOrdersRequest
	claimErr  error // returned by ClaimOrders when set
}

func (f *fakeOrders) wait(ctx context.Context) error {
	if f.block {
		<-ctx.Done()
		return status.FromContextError(ctx.Err()).Err()
	}
	return f.err
}

func (f *fakeOrders) GetOrder(ctx context.Context, in *pbo.GetOrderRequest, _ ...grpc.CallOption) (*pbo.Order, error) {
	f.mu.Lock()
	f.getCalls = append(f.getCalls, in)
	f.mu.Unlock()
	if err := f.wait(ctx); err != nil {
		return nil, err
	}
	for _, o := range f.orders {
		if o.OrderId == in.OrderId {
			if in.SessionId != "" && o.SessionId != in.SessionId {
				break // same answer as for an unknown id
			}
			if in.UserId != "" && o.UserId != in.UserId {
				break
			}
			if f.getHook != nil {
				return f.getHook(o), nil
			}
			return o, nil
		}
	}
	return nil, status.Error(codes.NotFound, "order not found")
}

func (f *fakeOrders) ListOrders(ctx context.Context, in *pbo.ListOrdersRequest, _ ...grpc.CallOption) (*pbo.ListOrdersResponse, error) {
	f.mu.Lock()
	f.listCalls = append(f.listCalls, in)
	f.mu.Unlock()
	if err := f.wait(ctx); err != nil {
		return nil, err
	}
	var all []*pbo.Order
	for _, o := range f.orders {
		if in.UserId != "" && o.UserId != in.UserId {
			continue
		}
		if in.UserId == "" && o.SessionId != in.SessionId {
			continue
		}
		if in.PlacedOnly && (o.Status == pbo.OrderStatus_ORDER_STATUS_PENDING || o.Status == pbo.OrderStatus_ORDER_STATUS_FAILED) {
			continue
		}
		all = append(all, o)
	}
	page, size := int(in.Page), int(in.PageSize)
	if page < 1 {
		page = 1
	}
	if size < 1 {
		size = 10
	}
	lo, hi := min((page-1)*size, len(all)), min(page*size, len(all))
	return &pbo.ListOrdersResponse{Orders: all[lo:hi], Total: int32(len(all))}, nil // #nosec G115 -- test data
}

// ClaimOrders behaves like order-service: the orders of the session that have no user yet get this one.
func (f *fakeOrders) ClaimOrders(_ context.Context, in *pbo.ClaimOrdersRequest, _ ...grpc.CallOption) (*pbo.ClaimOrdersResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims = append(f.claims, in)
	if f.claimErr != nil {
		return nil, f.claimErr
	}
	if in.SessionId == "" || in.UserId == "" {
		return nil, status.Error(codes.InvalidArgument, "session_id and user_id are required")
	}
	n := int32(0)
	for _, o := range f.orders {
		if o.SessionId == in.SessionId && o.UserId == "" {
			o.UserId = in.UserId
			n++
		}
	}
	return &pbo.ClaimOrdersResponse{Claimed: n}, nil
}

func (f *fakeOrders) CreateOrder(context.Context, *pbo.CreateOrderRequest, ...grpc.CallOption) (*pbo.Order, error) {
	return nil, status.Error(codes.Unimplemented, "the frontend only reads orders")
}

func (f *fakeOrders) ConfirmOrder(context.Context, *pbo.ConfirmOrderRequest, ...grpc.CallOption) (*pbo.Order, error) {
	return nil, status.Error(codes.Unimplemented, "the frontend only reads orders")
}

func (f *fakeOrders) FailOrder(context.Context, *pbo.FailOrderRequest, ...grpc.CallOption) (*pbo.Order, error) {
	return nil, status.Error(codes.Unimplemented, "the frontend only reads orders")
}

// the server clock of the samples; far from the real clock on purpose (the page must never use the browser's)
var evalAt = time.Date(2031, 3, 4, 10, 30, 0, 0, time.UTC)

// sampleOrder builds an order of session sid in status st, confirmed placedAgo before evalAt;
// the stage timestamps are placed + 1m / 3m / 6m like order-service's defaults.
func sampleOrder(id, sid string, st pbo.OrderStatus, placedAgo time.Duration) *pbo.Order {
	placed := evalAt.Add(-placedAgo)
	o := &pbo.Order{
		OrderId: id, SessionId: sid, Email: "ada.secret@example.com", Status: st,
		ShippingAddress: &pbo.Address{StreetAddress: "1 Main St", City: "London", State: "Greater London", Country: "United Kingdom", ZipCode: 1234},
		Items: []*pbo.OrderItem{
			{ProductId: "pan-02", Name: "Pan 02", Quantity: 2, UnitPrice: &pbo.Money{CurrencyCode: "USD", Units: 19, Nanos: 990000000}, Picture: "/static/img/products/pan-02-1.webp"},
			{ProductId: "cream-1", Name: "Cream", Quantity: 1, UnitPrice: &pbo.Money{CurrencyCode: "USD", Units: 9}},
		},
		ShippingCost: &pbo.Money{CurrencyCode: "USD", Units: 4, Nanos: 750000000},
		Total:        &pbo.Money{CurrencyCode: "USD", Units: 53, Nanos: 730000000},
		CreatedAt:    timestamppb.New(placed.Add(-2 * time.Second)),
		EvaluatedAt:  timestamppb.New(evalAt),
	}
	switch st {
	case pbo.OrderStatus_ORDER_STATUS_PENDING, pbo.OrderStatus_ORDER_STATUS_FAILED:
		if st == pbo.OrderStatus_ORDER_STATUS_FAILED {
			o.FailureReason = "card declined"
			o.FailedAt = timestamppb.New(placed)
		}
	default:
		o.ShippingTrackingId, o.PaymentTransactionId = "TRK-"+id, "TXN-1"
		o.PlacedAt = timestamppb.New(placed)
		o.ProcessingAt = timestamppb.New(placed.Add(time.Minute))
		o.ShippedAt = timestamppb.New(placed.Add(3 * time.Minute))
		o.DeliveredAt = timestamppb.New(placed.Add(6 * time.Minute))
	}
	return o
}

func ordersFrontend(t *testing.T, fo *fakeOrders) *frontendServer {
	t.Helper()
	fe := newFakeFrontend(t, sampleCatalog())
	if fo != nil {
		fe.orders = fo
	}
	return fe
}

// doOrders routes a request like main.go does, as visitor sid (the session cookie value).
func doOrders(fe *frontendServer, sid, target string, header map[string]string) *httptest.ResponseRecorder {
	r := mux.NewRouter()
	r.HandleFunc("/", fe.homeHandler).Methods(http.MethodGet)
	r.HandleFunc("/orders", fe.ordersHistoryHandler).Methods(http.MethodGet, http.MethodHead)
	r.HandleFunc("/orders/{id}", fe.orderTrackingHandler).Methods(http.MethodGet, http.MethodHead)
	req := requestWithContext(http.MethodGet, target, "")
	req = req.WithContext(context.WithValue(req.Context(), ctxKeySessionID{}, sid))
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

func setOrdersEnabled(t *testing.T, v bool) {
	t.Helper()
	old := ordersEnabled
	ordersEnabled = v
	t.Cleanup(func() { ordersEnabled = old })
}

const (
	stPending    = pbo.OrderStatus_ORDER_STATUS_PENDING
	stPlaced     = pbo.OrderStatus_ORDER_STATUS_PLACED
	stProcessing = pbo.OrderStatus_ORDER_STATUS_PROCESSING
	stShipped    = pbo.OrderStatus_ORDER_STATUS_SHIPPED
	stDelivered  = pbo.OrderStatus_ORDER_STATUS_DELIVERED
	stFailed     = pbo.OrderStatus_ORDER_STATUS_FAILED
)

// stageStates reads the tracker out of a page: "done,current,pending,pending".
var stageRe = regexp.MustCompile(`class="stage is-(done|current|pending)"`)

func stageStates(html string) string {
	var out []string
	for _, m := range stageRe.FindAllStringSubmatch(html, -1) {
		out = append(out, m[1])
	}
	return strings.Join(out, ",")
}

// ---------------------------------------------------------------------------
// history
// ---------------------------------------------------------------------------

func TestOrdersHistory_EmptyState(t *testing.T) {
	setOrdersEnabled(t, true)
	fo := &fakeOrders{}
	rr := doOrders(ordersFrontend(t, fo), "s1", "/orders", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	html := rr.Body.String()
	assertContains(t, html, "You have no orders yet", `href="/">Start shopping`, "<title>Your orders | "+brand.Name)
	if strings.Contains(html, "order-card") || strings.Contains(html, `class="pagination"`) {
		t.Error("no cards and no paging for an empty history")
	}
	if len(fo.listCalls) != 1 {
		t.Fatalf("expected one ListOrders call, got %d", len(fo.listCalls))
	}
	c := fo.listCalls[0]
	if c.SessionId != "s1" || c.UserId != "" || !c.PlacedOnly || c.Page != 1 || c.PageSize != ordersPageSize || ordersPageSize > 50 {
		t.Errorf("unexpected request %v", c)
	}
}

func manyOrders(sid string, n int) []*pbo.Order {
	var out []*pbo.Order
	for i := 1; i <= n; i++ { // newest first
		out = append(out, sampleOrder(fmt.Sprintf("o-%02d", i), sid, stDelivered, time.Duration(i)*24*time.Hour+7*time.Minute))
	}
	return out
}

func TestOrdersHistory_Paging(t *testing.T) {
	setOrdersEnabled(t, true)
	fo := &fakeOrders{orders: manyOrders("s1", 23)}
	fe := ordersFrontend(t, fo)

	html := doOrders(fe, "s1", "/orders", nil).Body.String()
	if n := strings.Count(html, `class="order-card"`); n != 10 {
		t.Errorf("page 1: %d cards, want 10", n)
	}
	assertContains(t, html, "23 orders, newest first", "#o-01", "#o-10", `href="/orders?page=2" rel="next"`, `aria-label="Page 3"`)
	if strings.Contains(html, "#o-11") || strings.Contains(html, `rel="prev"`) {
		t.Error("page 1 must not show page 2 rows or a prev link")
	}

	html = doOrders(fe, "s1", "/orders?page=2", nil).Body.String()
	assertContains(t, html, "#o-11", "#o-20", `href="/orders" rel="prev"`, `href="/orders?page=3" rel="next"`, `aria-current="page">2<`)
	if fo.listCalls[len(fo.listCalls)-1].Page != 2 {
		t.Error("page parameter not passed on")
	}

	html = doOrders(fe, "s1", "/orders?page=3", nil).Body.String()
	if n := strings.Count(html, `class="order-card"`); n != 3 {
		t.Errorf("last page: %d cards, want 3", n)
	}

	// past the end: the last page, not an empty list
	before := len(fo.listCalls)
	html = doOrders(fe, "s1", "/orders?page=99", nil).Body.String()
	if n := strings.Count(html, `class="order-card"`); n != 3 || len(fo.listCalls)-before != 2 {
		t.Errorf("page 99: %d cards after %d calls, want 3 cards after 2 calls", n, len(fo.listCalls)-before)
	}
	// nonsense falls back to page 1
	for _, p := range []string{"abc", "-4", "0", "1"} {
		if got := doOrders(fe, "s1", "/orders?page="+p, nil).Body.String(); !strings.Contains(got, "#o-01") {
			t.Errorf("page=%s should show page 1", p)
		}
	}
}

func TestOrdersHistory_OnlyTheSessionsOrders(t *testing.T) {
	setOrdersEnabled(t, true)
	fo := &fakeOrders{orders: []*pbo.Order{
		sampleOrder("mine-1", "s1", stPlaced, time.Second),
		sampleOrder("theirs-1", "s2", stPlaced, time.Minute),
		sampleOrder("mine-2", "s1", stShipped, 4*time.Minute),
	}}
	fe := ordersFrontend(t, fo)
	html := doOrders(fe, "s1", "/orders", nil).Body.String()
	assertContains(t, html, "#mine-1", "#mine-2", "2 orders")
	if strings.Contains(html, "theirs-1") {
		t.Error("another session's order is listed")
	}
	if got := doOrders(fe, "s2", "/orders", nil).Body.String(); !strings.Contains(got, "#theirs-1") || strings.Contains(got, "mine-") || !strings.Contains(got, "1 order,") {
		t.Error("session s2 should see exactly its own order")
	}
}

func TestOrdersHistory_RowsAndBadges(t *testing.T) {
	setOrdersEnabled(t, true)
	uuid := "a1b2c3d4-0000-4000-8000-0123456789ab"
	// the fake (like the service with placed_only) leaves out PENDING and FAILED
	fo := &fakeOrders{orders: []*pbo.Order{
		sampleOrder(uuid, "s1", stPlaced, time.Second),
		sampleOrder("o-proc", "s1", stProcessing, 2*time.Minute),
		sampleOrder("o-ship", "s1", stShipped, 4*time.Minute),
		sampleOrder("o-done", "s1", stDelivered, 8*time.Minute),
		sampleOrder("o-pend", "s1", stPending, 0),
		sampleOrder("o-fail", "s1", stFailed, 0),
	}}
	html := doOrders(ordersFrontend(t, fo), "s1", "/orders", nil).Body.String()
	assertContains(t, html,
		"#a1b2c3d4", `href="/orders/`+uuid+`"`, // short id, link with the full one
		`status-badge is-placed">Order placed`, `status-badge is-processing">Processing`,
		`status-badge is-shipped">Shipped`, `status-badge is-delivered">Delivered`,
		"3 items", "$53.73", // 2 + 1 items, total
		`<time datetime="2031-03-04">4 March 2031</time>`, // placed_at, in the server's terms
		`src="/static/img/products/pan-02-1.webp"`)
	if strings.Contains(html, "o-pend") || strings.Contains(html, "o-fail") || strings.Contains(html, "#"+uuid) {
		t.Error("PENDING/FAILED orders and the long id must not appear in the history")
	}
	if strings.Contains(html, "ada.secret@example.com") {
		t.Error("the e-mail address is not shown")
	}
}

func TestOrdersHistory_ShowsFailedAndPendingBadgesIfTheServiceReturnsThem(t *testing.T) {
	setOrdersEnabled(t, true)
	rows := []orderRow{newOrderRow(sampleOrder("p", "s", stPending, 0)), newOrderRow(sampleOrder("f", "s", stFailed, 0))}
	if rows[0].Status.Label != "Processing payment" || rows[1].Status.Label != "Failed" || rows[1].Status.Class != "is-failed" {
		t.Errorf("badges: %+v %+v", rows[0].Status, rows[1].Status)
	}
	if rows[0].Date == "" {
		t.Error("created_at is the fallback date")
	}
}

// ---------------------------------------------------------------------------
// tracking
// ---------------------------------------------------------------------------

func TestOrderTracking_EachStatus(t *testing.T) {
	setOrdersEnabled(t, true)
	cases := []struct {
		st     pbo.OrderStatus
		ago    time.Duration
		badge  string
		stages string
	}{
		{stPlaced, 10 * time.Second, "Order placed", "current,pending,pending,pending"},
		{stProcessing, 90 * time.Second, "Processing", "done,current,pending,pending"},
		{stShipped, 4 * time.Minute, "Shipped", "done,done,current,pending"},
		{stDelivered, 7 * time.Minute, "Delivered", "done,done,done,done"},
	}
	for _, c := range cases {
		t.Run(c.badge, func(t *testing.T) {
			fo := &fakeOrders{orders: []*pbo.Order{sampleOrder("ord-1", "s1", c.st, c.ago)}}
			rr := doOrders(ordersFrontend(t, fo), "s1", "/orders/ord-1", nil)
			if rr.Code != http.StatusOK {
				t.Fatalf("status %d", rr.Code)
			}
			html := rr.Body.String()
			assertContains(t, html, ">"+c.badge+"</span>", `<ol class="tracker" aria-label="Order progress">`, "TRK-ord-1",
				"Pan 02", "2 &times; $19.99", "$39.98", "Cream", "$4.75", "$53.73", "1 Main St", "London, Greater London 1234", "United Kingdom",
				"<title>Order ord-1 | "+brand.Name)
			if got := stageStates(html); got != c.stages {
				t.Errorf("stages %q, want %q", got, c.stages)
			}
			if n := strings.Count(html, `aria-current="step"`); (c.st == stDelivered && n != 0) || (c.st != stDelivered && n != 1) {
				t.Errorf("aria-current=step appears %d times", n)
			}
			assertContains(t, html, ">Placed<", ">Processing<", ">Shipped<", ">Delivered<")
			if len(fo.getCalls) != 1 || fo.getCalls[0].SessionId != "s1" || fo.getCalls[0].OrderId != "ord-1" {
				t.Errorf("GetOrder must carry the session as the guard: %v", fo.getCalls)
			}
		})
	}
}

func TestOrderTracking_StagesComeFromTheServerClock(t *testing.T) {
	setOrdersEnabled(t, true)
	// the sample clock is in 2031: nothing here may depend on time.Now()
	o := sampleOrder("ord-1", "s1", stShipped, 4*time.Minute)
	fo := &fakeOrders{orders: []*pbo.Order{o}}
	html := doOrders(ordersFrontend(t, fo), "s1", "/orders/ord-1", nil).Body.String()
	assertContains(t, html,
		`<time datetime="2031-03-04T10:26:00Z">4 Mar 2031, 10:26 UTC</time>`,          // placed
		`<time datetime="2031-03-04T10:29:00Z">4 Mar 2031, 10:29 UTC</time>`,          // shipped (reached)
		`Expected <time datetime="2031-03-04T10:32:00Z">4 Mar 2031, 10:32 UTC</time>`, // delivered, still ahead
		"in about 2 minutes", // 10:32 - evaluated_at 10:30
		`Status as of <time datetime="2031-03-04T10:30:00Z">10:30:00 UTC</time>`)

	// no timestamps at all (e.g. an older service): the status alone draws the tracker
	o2 := sampleOrder("ord-2", "s1", stProcessing, 90*time.Second)
	o2.PlacedAt, o2.ProcessingAt, o2.ShippedAt, o2.DeliveredAt, o2.EvaluatedAt = nil, nil, nil, nil, nil
	fo.orders = []*pbo.Order{o2}
	html = doOrders(ordersFrontend(t, fo), "s1", "/orders/ord-2", nil).Body.String()
	if got := stageStates(html); got != "done,current,pending,pending" {
		t.Errorf("status-only tracker %q", got)
	}
	if strings.Contains(html, "Status as of") || strings.Contains(html, "Expected") {
		t.Error("no clock, no times")
	}
}

func TestReachedStage(t *testing.T) {
	base := sampleOrder("x", "s", stPlaced, 10*time.Second) // status PLACED, evaluated 10 s after placement
	if got := reachedStage(base); got != 0 {
		t.Errorf("placed: %d", got)
	}
	// a stage timestamp that is not after evaluated_at counts as reached even if the status lags
	skew := sampleOrder("x", "s", stPlaced, 4*time.Minute) // processing_at and shipped_at are behind evaluated_at
	if got := reachedStage(skew); got != 2 {
		t.Errorf("timestamps behind the server clock: %d, want 2", got)
	}
	// a timestamp ahead of evaluated_at never counts, whatever the browser thinks
	future := sampleOrder("x", "s", stPlaced, 10*time.Second)
	future.ProcessingAt = timestamppb.New(time.Now().Add(-time.Hour)) // "past" for the browser, future for the server
	future.EvaluatedAt = timestamppb.New(time.Now().Add(-2 * time.Hour))
	if got := reachedStage(future); got != 0 {
		t.Errorf("future stage reached: %d", got)
	}
	for _, st := range []pbo.OrderStatus{stPending, stFailed} {
		if got := reachedStage(sampleOrder("x", "s", st, 0)); got != -1 {
			t.Errorf("%v reached %d", st, got)
		}
	}
}

func TestOrderTracking_PendingAndFailed(t *testing.T) {
	setOrdersEnabled(t, true)
	fo := &fakeOrders{orders: []*pbo.Order{
		sampleOrder("pend-1", "s1", stPending, 0),
		sampleOrder("fail-1", "s1", stFailed, 0),
	}}
	fe := ordersFrontend(t, fo)

	html := doOrders(fe, "s1", "/orders/pend-1", nil).Body.String()
	assertContains(t, html, "We are still processing your payment", "Processing payment")
	if strings.Contains(html, `class="tracker"`) || strings.Contains(html, "Tracking number") {
		t.Error("a pending order has no tracker and no tracking number yet")
	}

	html = doOrders(fe, "s1", "/orders/fail-1", nil).Body.String()
	assertContains(t, html, "This order could not be completed", "Reason:", "card declined", `status-badge is-failed">Failed`)
	if strings.Contains(html, `class="tracker"`) {
		t.Error("a failed order has no tracker")
	}
}

func TestOrderTracking_FailureReasonAndNamesAreEscaped(t *testing.T) {
	setOrdersEnabled(t, true)
	evil := `<script>alert("x")</script><img src=x onerror=alert(1)> & "q"`
	o := sampleOrder("fail-1", "s1", stFailed, 0)
	o.FailureReason = evil
	o.Items[0].Name = `<b onmouseover=alert(2)>Pan</b>`
	o.Items[0].Picture = `/static/img/products/x.webp" onerror="alert(3)`
	o.ShippingAddress.StreetAddress = `<i>1 Main</i>`
	fo := &fakeOrders{orders: []*pbo.Order{o}}
	html := doOrders(ordersFrontend(t, fo), "s1", "/orders/fail-1", nil).Body.String()
	for _, bad := range []string{`<script>alert`, `<img src=x`, `<b onmouseover`, `onerror="alert(3)`, `<i>1 Main`} {
		if strings.Contains(html, bad) {
			t.Errorf("unescaped %q in the page", bad)
		}
	}
	assertContains(t, html, `&lt;script&gt;alert(&#34;x&#34;)&lt;/script&gt;`, `&lt;b onmouseover=alert(2)&gt;Pan&lt;/b&gt;`)
}

func TestOrderTracking_PicturesOnlyFromTheShopsOwnPath(t *testing.T) {
	setOrdersEnabled(t, true)
	for pic, want := range map[string]string{
		"/static/img/products/pan-02-1.webp":    "/static/img/products/pan-02-1.webp",
		"https://evil.example/track.png":        "",
		"//evil.example/static/img/products/x":  "",
		"/static/img/products/../../etc/passwd": "",
		"/static/img/products/a b.webp":         "",
		"/static/img/products/a.webp?x=1":       "",
		"/static/js/htmx.min.js":                "",
		"javascript:alert(1)":                   "",
		"":                                      "",
	} {
		if got := knownPicture(pic); got != want {
			t.Errorf("knownPicture(%q) = %q, want %q", pic, got, want)
		}
	}
	o := sampleOrder("o1", "s1", stPlaced, time.Second)
	o.Items[0].Picture = "https://evil.example/track.png"
	fo := &fakeOrders{orders: []*pbo.Order{o}}
	html := doOrders(ordersFrontend(t, fo), "s1", "/orders/o1", nil).Body.String()
	if strings.Contains(html, "evil.example") {
		t.Error("an external picture URL ended up in the page")
	}
}

func TestOrderTracking_StateOmittedWhenEmpty(t *testing.T) {
	setOrdersEnabled(t, true)
	o := sampleOrder("o1", "s1", stPlaced, time.Second)
	o.ShippingAddress.State, o.ShippingAddress.ZipCode = "", 0
	fo := &fakeOrders{orders: []*pbo.Order{o}}
	html := doOrders(ordersFrontend(t, fo), "s1", "/orders/o1", nil).Body.String()
	assertContains(t, html, "London<br>")
	if strings.Contains(html, "London,") || strings.Contains(html, "London 0") {
		t.Error("empty state/zip must not leave a stray comma or 0")
	}
}

func TestOrderTracking_ForeignAndUnknownAre404WithoutLeaks(t *testing.T) {
	setOrdersEnabled(t, true)
	fo := &fakeOrders{orders: []*pbo.Order{sampleOrder("secret-order", "s2", stShipped, 4*time.Minute)}}
	fe := ordersFrontend(t, fo)

	foreign := doOrders(fe, "s1", "/orders/secret-order", nil)
	unknown := doOrders(fe, "s1", "/orders/no-such-order", nil)
	for name, rr := range map[string]*httptest.ResponseRecorder{"foreign": foreign, "unknown": unknown} {
		if rr.Code != http.StatusNotFound {
			t.Errorf("%s: status %d", name, rr.Code)
		}
		html := rr.Body.String()
		for _, leak := range []string{"Pan 02", "ada.secret", "TRK-", "1 Main St", "$53.73", "Shipped", "s2"} {
			if strings.Contains(html, leak) {
				t.Errorf("%s: 404 page leaks %q", name, leak)
			}
		}
		assertContains(t, html, "Not found", msgNotFound)
	}
	// identical answers apart from the id in the URL (which is not echoed)
	if foreign.Body.String() != unknown.Body.String() {
		t.Error("a foreign and an unknown order must be indistinguishable")
	}

	// the service guard failing open must not show the order either
	fo2 := &fakeOrders{orders: []*pbo.Order{sampleOrder("secret-order", "s2", stShipped, time.Minute)},
		getHook: func(o *pbo.Order) *pbo.Order { return o }}
	fo2.orders[0].SessionId = "s2"
	fe2 := ordersFrontend(t, &fakeOrders{})
	fe2.orders = &leakyOrders{inner: fo2}
	if rr := doOrders(fe2, "s1", "/orders/secret-order", nil); rr.Code != http.StatusNotFound || strings.Contains(rr.Body.String(), "Pan 02") {
		t.Errorf("defence in depth: status %d", rr.Code)
	}
}

// leakyOrders ignores the session guard, like a buggy service would.
type leakyOrders struct{ inner *fakeOrders }

func (l *leakyOrders) GetOrder(ctx context.Context, in *pbo.GetOrderRequest, o ...grpc.CallOption) (*pbo.Order, error) {
	return l.inner.GetOrder(ctx, &pbo.GetOrderRequest{OrderId: in.OrderId}, o...)
}

func (l *leakyOrders) ListOrders(ctx context.Context, in *pbo.ListOrdersRequest, o ...grpc.CallOption) (*pbo.ListOrdersResponse, error) {
	return l.inner.ListOrders(ctx, in, o...)
}

func (l *leakyOrders) CreateOrder(ctx context.Context, in *pbo.CreateOrderRequest, o ...grpc.CallOption) (*pbo.Order, error) {
	return l.inner.CreateOrder(ctx, in, o...)
}

func (l *leakyOrders) ConfirmOrder(ctx context.Context, in *pbo.ConfirmOrderRequest, o ...grpc.CallOption) (*pbo.Order, error) {
	return l.inner.ConfirmOrder(ctx, in, o...)
}

func (l *leakyOrders) ClaimOrders(ctx context.Context, in *pbo.ClaimOrdersRequest, o ...grpc.CallOption) (*pbo.ClaimOrdersResponse, error) {
	return l.inner.ClaimOrders(ctx, in, o...)
}

func (l *leakyOrders) FailOrder(ctx context.Context, in *pbo.FailOrderRequest, o ...grpc.CallOption) (*pbo.Order, error) {
	return l.inner.FailOrder(ctx, in, o...)
}

func TestOrderTracking_MalformedIDIs404WithoutCallingTheService(t *testing.T) {
	setOrdersEnabled(t, true)
	fo := &fakeOrders{}
	fe := ordersFrontend(t, fo)
	for _, id := range []string{"-lead", ".dot", "a%20b", strings.Repeat("a", 65), "a%2Fb", "%3Cscript%3E", "caf%C3%A9"} {
		if rr := doOrders(fe, "s1", "/orders/"+id, nil); rr.Code != http.StatusNotFound {
			t.Errorf("%q: status %d", id, rr.Code)
		}
	}
	if len(fo.getCalls) != 0 {
		t.Errorf("service called %d times for ids it can never have", len(fo.getCalls))
	}
}

// ---------------------------------------------------------------------------
// service down
// ---------------------------------------------------------------------------

func TestOrders_ServiceDownIsAFriendly503OnTheOrderRoutesOnly(t *testing.T) {
	setOrdersEnabled(t, true)
	for name, err := range map[string]error{
		"unavailable": status.Error(codes.Unavailable, "connection refused to 10.0.0.7:8085"),
		"deadline":    status.Error(codes.DeadlineExceeded, "slow"),
		"internal":    status.Error(codes.Internal, "pq: password authentication failed for user order"),
		"not a grpc":  fmt.Errorf("boom 10.0.0.7"),
	} {
		t.Run(name, func(t *testing.T) {
			fe := ordersFrontend(t, &fakeOrders{err: err})
			for _, target := range []string{"/orders", "/orders/ord-1"} {
				rr := doOrders(fe, "s1", target, nil)
				if rr.Code != http.StatusServiceUnavailable {
					t.Fatalf("%s: status %d", target, rr.Code)
				}
				html := rr.Body.String()
				assertContains(t, html, "Orders are not available right now.", "HTTP status 503", `href="/">Back to the shop`)
				for _, leak := range []string{"10.0.0.7", "password", "connection refused", "boom"} {
					if strings.Contains(html, leak) {
						t.Errorf("%s: page leaks %q", target, leak)
					}
				}
			}
			// the rest of the shop does not care
			if rr := doOrders(fe, "s1", "/", nil); rr.Code != http.StatusOK {
				t.Errorf("home with orders down: %d", rr.Code)
			}
		})
	}
}

func TestOrders_HungServiceEndsInA503(t *testing.T) {
	setOrdersEnabled(t, true)
	fe := ordersFrontend(t, &fakeOrders{block: true})
	start := time.Now()
	rr := doOrders(fe, "s1", "/orders", nil)
	if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), "Orders are not available right now.") {
		t.Errorf("status %d", rr.Code)
	}
	if d := time.Since(start); d > orderReadTimeout+time.Second {
		t.Errorf("took %v, the deadline is %v", d, orderReadTimeout)
	}
}

func TestOrders_ErrorFromHTMXRequestIsAnAlertNotAPage(t *testing.T) {
	setOrdersEnabled(t, true)
	// a failing poll that is not "unavailable" tells the visitor in the alert region
	fe := ordersFrontend(t, &fakeOrders{err: status.Error(codes.Internal, "x")})
	rr := doOrders(fe, "s1", "/orders/ord-1", map[string]string{"HX-Request": "true"})
	if rr.Code != http.StatusServiceUnavailable || rr.Header().Get("HX-Retarget") != "#page-alert" || strings.Contains(rr.Body.String(), "<html") {
		t.Errorf("status %d retarget %q", rr.Code, rr.Header().Get("HX-Retarget"))
	}
	assertContains(t, rr.Body.String(), "Orders are not available")
}

// ---------------------------------------------------------------------------
// fragment and polling
// ---------------------------------------------------------------------------

func TestOrderTracking_PollingAttributeOnlyWhileInProgress(t *testing.T) {
	setOrdersEnabled(t, true)
	for _, c := range []struct {
		st     pbo.OrderStatus
		polls  bool
		answer int
	}{
		{stPending, true, 200}, {stPlaced, true, 200}, {stProcessing, true, 200}, {stShipped, true, 200},
		{stDelivered, false, 286}, {stFailed, false, 286},
	} {
		fo := &fakeOrders{orders: []*pbo.Order{sampleOrder("ord-1", "s1", c.st, 4*time.Minute)}}
		fe := ordersFrontend(t, fo)
		html := doOrders(fe, "s1", "/orders/ord-1", nil).Body.String()
		has := strings.Contains(html, `hx-get="/orders/ord-1"`) && strings.Contains(html, `hx-trigger="every 15s"`) && strings.Contains(html, `hx-swap="innerHTML"`)
		if has != c.polls {
			t.Errorf("%v: polling attributes present = %v, want %v", c.st, has, c.polls)
		}
		if !c.polls && (strings.Contains(html, "hx-get") || strings.Contains(html, "hx-trigger") || strings.Contains(html, "checks for news")) {
			t.Errorf("%v: no polling markup expected", c.st)
		}

		rr := doOrders(fe, "s1", "/orders/ord-1", map[string]string{"HX-Request": "true"})
		if rr.Code != c.answer {
			t.Errorf("%v: fragment answered %d, want %d (286 stops htmx polling)", c.st, rr.Code, c.answer)
		}
	}
}

func TestOrderTracking_FragmentShape(t *testing.T) {
	setOrdersEnabled(t, true)
	fo := &fakeOrders{orders: []*pbo.Order{sampleOrder("ord-1", "s1", stProcessing, 90*time.Second)}}
	fe := ordersFrontend(t, fo)
	full := doOrders(fe, "s1", "/orders/ord-1", nil)
	frag := doOrders(fe, "s1", "/orders/ord-1", map[string]string{"HX-Request": "true"})
	if frag.Code != http.StatusOK || !strings.Contains(frag.Header().Get("Vary"), "HX-Request") {
		t.Fatalf("status %d vary %q", frag.Code, frag.Header().Get("Vary"))
	}
	html := frag.Body.String()
	assertContains(t, html, `status-badge is-processing">Processing`, `<ol class="tracker"`, "TRK-ord-1", "Status as of")
	for _, no := range []string{"<html", "<head", "<title", "site-header", "site-footer", "Pan 02", "hx-get", `id="order-live"`} {
		if strings.Contains(html, no) {
			t.Errorf("fragment contains %q", no)
		}
	}
	if !strings.Contains(full.Body.String(), html) {
		t.Error("the page must embed exactly the fragment the poll returns")
	}
	// back/forward restores need the whole page
	restore := doOrders(fe, "s1", "/orders/ord-1", map[string]string{"HX-Request": "true", "HX-History-Restore-Request": "true"})
	if !strings.Contains(restore.Body.String(), "<html") {
		t.Error("history restore must get the full page")
	}
}

func TestOrderTracking_FragmentKeepsThePageWhenTheServiceIsDown(t *testing.T) {
	setOrdersEnabled(t, true)
	fe := ordersFrontend(t, &fakeOrders{err: status.Error(codes.Unavailable, "down")})
	rr := doOrders(fe, "s1", "/orders/ord-1", map[string]string{"HX-Request": "true"})
	if rr.Code != http.StatusNoContent || rr.Body.Len() != 0 {
		t.Errorf("a poll that finds the service down changes nothing: %d %q", rr.Code, rr.Body.String())
	}
	if rr := doOrders(fe, "s1", "/orders/ord-1", nil); rr.Code != http.StatusServiceUnavailable {
		t.Errorf("a full page load must say so: %d", rr.Code)
	}
}

func TestOrderTracking_PendingBecomesPlacedOnTheNextPoll(t *testing.T) {
	setOrdersEnabled(t, true)
	fo := &fakeOrders{orders: []*pbo.Order{sampleOrder("ord-1", "s1", stPending, 0)}}
	fe := ordersFrontend(t, fo)
	hx := map[string]string{"HX-Request": "true"}
	if got := doOrders(fe, "s1", "/orders/ord-1", hx).Body.String(); !strings.Contains(got, "still processing your payment") || strings.Contains(got, "TRK-") {
		t.Error("first poll: waiting for payment")
	}
	fo.orders[0] = sampleOrder("ord-1", "s1", stPlaced, time.Second)
	if got := doOrders(fe, "s1", "/orders/ord-1", hx).Body.String(); strings.Contains(got, "still processing") || !strings.Contains(got, "TRK-ord-1") || stageStates(got) != "current,pending,pending,pending" {
		t.Error("second poll: tracker and tracking number appear")
	}
}

// ---------------------------------------------------------------------------
// feature off, header, order-complete link
// ---------------------------------------------------------------------------

func TestOrders_FeatureOffWithEmptyAddress(t *testing.T) {
	setOrdersEnabled(t, false)
	fe := ordersFrontend(t, nil) // fe.orders == nil, like ORDER_SERVICE_ADDR=
	for _, target := range []string{"/orders", "/orders/ord-1"} {
		if rr := doOrders(fe, "s1", target, nil); rr.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", target, rr.Code)
		}
	}
	home := doOrders(fe, "s1", "/", nil).Body.String()
	if strings.Contains(home, `href="/orders"`) || strings.Contains(home, "orders-link") {
		t.Error("no Orders link when the feature is off")
	}
	html := renderPage(t, "order", map[string]interface{}{
		"order":      &pb.OrderResult{OrderId: "ord-1", ShippingTrackingId: "TR-1"},
		"total_paid": &pb.Money{CurrencyCode: "USD", Units: 1},
	})
	if strings.Contains(html, "Track your order") {
		t.Error("no track link when the feature is off")
	}
}

func TestOrders_AddrFromEnv(t *testing.T) {
	t.Setenv("ORDER_SERVICE_ADDR", "x")
	if err := os.Unsetenv("ORDER_SERVICE_ADDR"); err != nil {
		t.Fatal(err)
	}
	if got := ordersAddrFromEnv(); got != "order-service:8085" {
		t.Errorf("default %q", got)
	}
	t.Setenv("ORDER_SERVICE_ADDR", "")
	if got := ordersAddrFromEnv(); got != "" {
		t.Errorf("empty must switch the feature off, got %q", got)
	}
	t.Setenv("ORDER_SERVICE_ADDR", "  ")
	if got := ordersAddrFromEnv(); got != "" {
		t.Errorf("blank must switch the feature off, got %q", got)
	}
	t.Setenv("ORDER_SERVICE_ADDR", " orders.internal:9000 ")
	if got := ordersAddrFromEnv(); got != "orders.internal:9000" {
		t.Errorf("custom %q", got)
	}
}

func TestOrders_HeaderLinkAndTrackLink(t *testing.T) {
	setOrdersEnabled(t, true)
	home := doOrders(ordersFrontend(t, &fakeOrders{}), "s1", "/", nil).Body.String()
	assertContains(t, home, `href="/orders" class="icon-btn orders-link" aria-label="Orders"`, "/static/styles/orders.css", `href="/orders">Your orders`)
	if strings.Index(home, "orders-link") > strings.Index(home, "cart-link") {
		t.Error("the Orders link sits next to the cart, before it")
	}

	html := renderPage(t, "order", map[string]interface{}{
		"order":      &pb.OrderResult{OrderId: "0b5e6c0e-1111-2222-3333-444455556666", ShippingTrackingId: "TR-1"},
		"total_paid": &pb.Money{CurrencyCode: "USD", Units: 1},
	})
	assertContains(t, html, `href="/orders/0b5e6c0e-1111-2222-3333-444455556666">Track your order`)
	// an odd id is escaped into the path, not into the markup
	html = renderPage(t, "order", map[string]interface{}{
		"order":      &pb.OrderResult{OrderId: `a"><script>x</script>`, ShippingTrackingId: "TR-1"},
		"total_paid": &pb.Money{CurrencyCode: "USD", Units: 1},
	})
	if strings.Contains(html, "<script>x") {
		t.Error("order id not escaped in the track link")
	}
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

func TestShortOrderID(t *testing.T) {
	for in, want := range map[string]string{
		"a1b2c3d4-0000-4000-8000-0123456789ab": "a1b2c3d4",
		"order-1":                              "order-1",
		"demo-processing":                      "demo-process",
		"abcdefghijkl":                         "abcdefghijkl",
	} {
		if got := shortOrderID(in); got != want {
			t.Errorf("shortOrderID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestInAbout(t *testing.T) {
	for d, want := range map[time.Duration]string{
		-time.Second: "", 0: "", 20 * time.Second: "in less than a minute", time.Minute: "in about 1 minute",
		90 * time.Second: "in about 2 minutes", 2 * time.Minute: "in about 2 minutes", 3 * time.Hour: "in about 3 hours",
		time.Hour: "in about 1 hour", 72 * time.Hour: "in about 3 days",
	} {
		if got := inAbout(d); got != want {
			t.Errorf("inAbout(%v) = %q, want %q", d, got, want)
		}
	}
}
