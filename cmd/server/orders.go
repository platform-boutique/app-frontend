package main

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "frontend/genproto"
	pbo "frontend/genproto/orders"
	"frontend/money"
)

// Order history (/orders) and order tracking (/orders/{id}), backed by order-service.
// Like the reviews, the service is optional: with ORDER_SERVICE_ADDR set to an empty
// string the routes answer 404 and the header has no Orders link; when the service is
// down the two routes (and only they) answer a friendly 503 page. Nothing else in the
// shop depends on it.

const (
	defaultOrderAddr = "order-service:8085"

	// both pages are the whole point of the visit, but a hung service must still end in
	// a quick 503 page, so the deadline is short
	orderReadTimeout = 1500 * time.Millisecond

	ordersPageSize = 10 // ListOrders accepts 50 at most

	// the tracking page asks for itself again at this interval while the order is on its way
	orderPollSeconds = 15

	// the reached stages are shown with the server's clock; the zone is said out loud
	orderTimeLayout = "2 Jan 2006, 15:04 MST"

	msgOrdersUnavailable = "Orders are not available right now. Please try again in a moment."

	picturePrefix = "/static/img/products/"
)

// ordersEnabled tells the header whether to show the Orders link (set once in main, like baseUrl).
var ordersEnabled bool

// orderIDPattern is order-service's order id rule; anything else cannot exist, so it is a 404 without asking.
var orderIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ordersAddrFromEnv returns the order-service address: ORDER_SERVICE_ADDR if set, the
// default otherwise. A variable that is set but empty switches the feature off.
func ordersAddrFromEnv() string {
	if v, ok := os.LookupEnv("ORDER_SERVICE_ADDR"); ok {
		return strings.TrimSpace(v)
	}
	return defaultOrderAddr
}

// ordersUnavailableError marks "order-service did not give a usable answer"; classifyError
// turns it into the friendly 503 page.
type ordersUnavailableError struct{ cause error }

func (e *ordersUnavailableError) Error() string {
	return "order service unavailable: " + e.cause.Error()
}
func (e *ordersUnavailableError) Unwrap() error { return e.cause }

// ---------------------------------------------------------------------------
// view models
// ---------------------------------------------------------------------------

// statusView is the badge of an order: the words carry the meaning, the class only the colour.
type statusView struct {
	Label string
	Class string // is-pending, is-placed, is-processing, is-shipped, is-delivered, is-failed
}

func statusOf(s pbo.OrderStatus) statusView {
	switch s {
	case pbo.OrderStatus_ORDER_STATUS_PENDING:
		return statusView{"Processing payment", "is-pending"}
	case pbo.OrderStatus_ORDER_STATUS_PLACED:
		return statusView{"Order placed", "is-placed"}
	case pbo.OrderStatus_ORDER_STATUS_PROCESSING:
		return statusView{"Processing", "is-processing"}
	case pbo.OrderStatus_ORDER_STATUS_SHIPPED:
		return statusView{"Shipped", "is-shipped"}
	case pbo.OrderStatus_ORDER_STATUS_DELIVERED:
		return statusView{"Delivered", "is-delivered"}
	case pbo.OrderStatus_ORDER_STATUS_FAILED:
		return statusView{"Failed", "is-failed"}
	}
	return statusView{"Unknown", "is-pending"}
}

// toMoney converts the order-service money (same fields) to the type the templates render.
func toMoney(m *pbo.Money) *pb.Money {
	return &pb.Money{CurrencyCode: m.GetCurrencyCode(), Units: m.GetUnits(), Nanos: m.GetNanos()}
}

// uuidPattern is the shape of the ids checkout generates.
var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// shortOrderID is what the history shows: a UUID becomes its first block, other ids are
// cut at 12 characters (the tracking page always shows the whole id).
func shortOrderID(id string) string {
	if uuidPattern.MatchString(id) {
		return id[:8]
	}
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// knownPicture returns the picture of an order line if it is one of the shop's own static
// product images, otherwise "" (the snapshot may hold anything: never put it into an <img>).
func knownPicture(p string) string {
	if !strings.HasPrefix(p, picturePrefix) || path.Clean(p) != p || strings.ContainsAny(p, "?#\\ \"'<>") {
		return ""
	}
	return p
}

func orderURL(id string) string { return baseUrl + "/orders/" + url.PathEscape(id) }

func formatTime(t *timestamppb.Timestamp) (text, iso string) {
	if t == nil {
		return "", ""
	}
	u := t.AsTime().UTC()
	return u.Format(orderTimeLayout), u.Format(time.RFC3339)
}

// orderRow is one line of the history.
type orderRow struct {
	ID        string
	ShortID   string
	URL       string
	Date      string // "2 January 2026"
	ISODate   string
	Status    statusView
	ItemCount int32
	Total     *pb.Money
	Thumbs    []string
	More      int // lines without a thumbnail in the row
}

const maxThumbs = 4

func newOrderRow(o *pbo.Order) orderRow {
	row := orderRow{
		ID: o.GetOrderId(), ShortID: shortOrderID(o.GetOrderId()), URL: orderURL(o.GetOrderId()),
		Status: statusOf(o.GetStatus()), Total: toMoney(o.GetTotal()),
	}
	when := o.GetPlacedAt()
	if when == nil {
		when = o.GetCreatedAt()
	}
	if when != nil {
		t := when.AsTime().UTC()
		row.Date, row.ISODate = t.Format("2 January 2006"), t.Format("2006-01-02")
	}
	lines := 0
	for _, it := range o.GetItems() {
		row.ItemCount += it.GetQuantity()
		if pic := knownPicture(it.GetPicture()); pic != "" && len(row.Thumbs) < maxThumbs {
			row.Thumbs = append(row.Thumbs, pic)
		}
		lines++
	}
	if more := lines - len(row.Thumbs); more > 0 && len(row.Thumbs) > 0 {
		row.More = more
	}
	return row
}

// itemView is one line of an order.
type itemView struct {
	Name      string
	Quantity  int32
	UnitPrice *pb.Money
	LineTotal *pb.Money
	Picture   string
}

// stageView is one step of the tracker.
type stageView struct {
	Name    string
	State   string // done, current, pending
	Text    string // "Done", "Current step", "Pending": the state in words
	When    string // reached time, or the expected time of a pending stage
	ISO     string
	Expects string // pending only: "in about 2 minutes", relative to the server clock
}

// trackView is the data of the tracking page.
type trackView struct {
	ID          string
	ShortID     string
	Status      statusView
	Items       []itemView
	Street      string
	CityLine    string // "City, State 12345", state and zip left out when empty
	Country     string
	Shipping    *pb.Money
	Total       *pb.Money
	TrackingID  string
	PlacedDate  string
	Stages      []stageView
	Pending     bool // PENDING: payment not finished
	Failed      bool
	Reason      string
	Delivered   bool
	Active      bool   // still changing: the page refreshes itself
	AsOf        string // server clock of the status
	AsOfISO     string
	PollURL     string
	PollEvery   string // "15s"
	PollSeconds int
	EmailURL    string // "View confirmation email", empty when there is no mail (or no mailbox)
}

// stage names in order; the status of the order says how many of them are reached
var stageNames = [4]string{"Placed", "Processing", "Shipped", "Delivered"}

// reachedStage is the index of the last stage the order has reached, -1 for none. A stage is
// reached when the status is at or past it, or when its timestamp is not after the server
// clock (evaluated_at) the status was computed with - never the browser clock.
func reachedStage(o *pbo.Order) int {
	var statusIdx int
	switch o.GetStatus() {
	case pbo.OrderStatus_ORDER_STATUS_PLACED:
		statusIdx = 0
	case pbo.OrderStatus_ORDER_STATUS_PROCESSING:
		statusIdx = 1
	case pbo.OrderStatus_ORDER_STATUS_SHIPPED:
		statusIdx = 2
	case pbo.OrderStatus_ORDER_STATUS_DELIVERED:
		statusIdx = 3
	default:
		return -1 // PENDING, FAILED: nothing is reached
	}
	ts := [4]*timestamppb.Timestamp{o.GetPlacedAt(), o.GetProcessingAt(), o.GetShippedAt(), o.GetDeliveredAt()}
	last := statusIdx
	if now := o.GetEvaluatedAt(); now != nil {
		for i, t := range ts {
			if t != nil && !t.AsTime().After(now.AsTime()) && i > last {
				last = i
			}
		}
	}
	return last
}

func buildStages(o *pbo.Order) []stageView {
	last := reachedStage(o)
	ts := [4]*timestamppb.Timestamp{o.GetPlacedAt(), o.GetProcessingAt(), o.GetShippedAt(), o.GetDeliveredAt()}
	stages := make([]stageView, len(stageNames))
	for i, name := range stageNames {
		s := stageView{Name: name}
		s.When, s.ISO = formatTime(ts[i])
		switch {
		case i < last || (i == last && i == len(stageNames)-1):
			s.State, s.Text = "done", "Done"
		case i == last:
			s.State, s.Text = "current", "Current step"
		default:
			s.State, s.Text = "pending", "Pending"
			if t, now := ts[i], o.GetEvaluatedAt(); t != nil && now != nil {
				s.Expects = inAbout(t.AsTime().Sub(now.AsTime()))
			}
		}
		stages[i] = s
	}
	return stages
}

// inAbout words a duration ("in about 2 minutes"); empty when it is not in the future.
func inAbout(d time.Duration) string {
	plural := func(n int, unit string) string {
		if n == 1 {
			return "in about 1 " + unit
		}
		return "in about " + strconv.Itoa(n) + " " + unit + "s"
	}
	switch {
	case d <= 0:
		return ""
	case d < time.Minute:
		return "in less than a minute"
	case d < time.Hour:
		return plural(int((d+30*time.Second)/time.Minute), "minute")
	case d < 48*time.Hour:
		return plural(int((d+30*time.Minute)/time.Hour), "hour")
	}
	return plural(int(d/(24*time.Hour)), "day")
}

func newTrackView(o *pbo.Order) trackView {
	v := trackView{
		ID: o.GetOrderId(), ShortID: shortOrderID(o.GetOrderId()), Status: statusOf(o.GetStatus()),
		Shipping: toMoney(o.GetShippingCost()), Total: toMoney(o.GetTotal()),
		TrackingID: o.GetShippingTrackingId(), Reason: o.GetFailureReason(),
	}
	v.Pending = o.GetStatus() == pbo.OrderStatus_ORDER_STATUS_PENDING
	v.Failed = o.GetStatus() == pbo.OrderStatus_ORDER_STATUS_FAILED
	v.Delivered = o.GetStatus() == pbo.OrderStatus_ORDER_STATUS_DELIVERED
	v.Active = !v.Failed && !v.Delivered
	if v.Active {
		v.PollURL = orderURL(o.GetOrderId())
		v.PollEvery, v.PollSeconds = strconv.Itoa(orderPollSeconds)+"s", orderPollSeconds
	}
	if !v.Pending && !v.Failed {
		v.Stages = buildStages(o)
	}
	if t := o.GetPlacedAt(); t != nil {
		v.PlacedDate = t.AsTime().UTC().Format("2 January 2006")
	} else if t := o.GetCreatedAt(); t != nil {
		v.PlacedDate = t.AsTime().UTC().Format("2 January 2006")
	}
	if t := o.GetEvaluatedAt(); t != nil {
		u := t.AsTime().UTC()
		v.AsOf, v.AsOfISO = u.Format("15:04:05 MST"), u.Format(time.RFC3339)
	}
	for _, it := range o.GetItems() {
		unit := toMoney(it.GetUnitPrice())
		v.Items = append(v.Items, itemView{
			Name: it.GetName(), Quantity: it.GetQuantity(), UnitPrice: unit,
			LineTotal: money.MultiplySlow(unit, uint32(max(it.GetQuantity(), 0))), // #nosec G115 -- clamped to >= 0
			Picture:   knownPicture(it.GetPicture()),
		})
	}
	a := o.GetShippingAddress()
	v.Street, v.Country = a.GetStreetAddress(), a.GetCountry()
	v.CityLine = a.GetCity()
	if s := a.GetState(); s != "" {
		v.CityLine += ", " + s
	}
	if z := a.GetZipCode(); z > 0 {
		v.CityLine += " " + strconv.Itoa(int(z))
	}
	return v
}

// ---------------------------------------------------------------------------
// handlers
// ---------------------------------------------------------------------------

// ordersPageParam reads ?page= (1-based, bounded like the catalogue page).
func ordersPageParam(r *http.Request) int {
	if n, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && n > 1 && n < 100000 {
		return n
	}
	return 1
}

// failOrders answers a failed order-service call. A NotFound is a 404, anything else
// (service down, slow, or answering with an error) is the friendly 503 page: the visitor
// cannot do anything about the difference and no backend text is shown.
func (fe *frontendServer) failOrders(log logrus.FieldLogger, r *http.Request, w http.ResponseWriter, err error, what string) {
	if status.Code(err) == codes.NotFound {
		fe.renderHTTPError(log, r, w, errors.Wrap(err, what), http.StatusNotFound)
		return
	}
	if isFragmentRequest(r) && (status.Code(err) == codes.Unavailable || status.Code(err) == codes.DeadlineExceeded) {
		// a refresh of a page that is already showing: keep it, the next poll tries again
		log.WithField("error", err).Warn(what + ": order service unavailable, keeping the page as it is")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	fe.renderHTTPError(log, r, w, &ordersUnavailableError{cause: errors.Wrap(err, what)}, http.StatusServiceUnavailable)
}

// ordersHistoryHandler lists the confirmed orders of the visitor's session, newest first.
func (fe *frontendServer) ordersHistoryHandler(w http.ResponseWriter, r *http.Request) {
	log := r.Context().Value(ctxKeyLog{}).(logrus.FieldLogger)
	if fe.orders == nil {
		fe.renderHTTPError(log, r, w, errors.New("orders are not enabled"), http.StatusNotFound)
		return
	}
	if fe.identityUnavailable(log, r, w) {
		return
	}
	page := ordersPageParam(r)
	// owner: the signed-in user's orders, or the ones of the anonymous session
	owner := pbo.ListOrdersRequest{SessionId: sessionID(r)}
	if u := userOf(r); u != nil {
		owner = pbo.ListOrdersRequest{UserId: u.ID}
	}
	list := func(p int) (*pbo.ListOrdersResponse, error) {
		ctx, cancel := context.WithTimeout(r.Context(), orderReadTimeout)
		defer cancel()
		return fe.orders.ListOrders(ctx, &pbo.ListOrdersRequest{
			SessionId: owner.SessionId, UserId: owner.UserId, Page: int32(p), PageSize: ordersPageSize, PlacedOnly: true, // #nosec G115 -- bounded in ordersPageParam
		})
	}
	resp, err := list(page)
	if err == nil && page > 1 && len(resp.GetOrders()) == 0 && resp.GetTotal() > 0 {
		// a page past the end falls back to the last page, like the catalogue
		page = pagesOf(int(resp.GetTotal()), ordersPageSize)
		resp, err = list(page)
	}
	if err != nil {
		fe.failOrders(log, r, w, err, "could not list orders")
		return
	}
	rows := make([]orderRow, len(resp.GetOrders()))
	for i, o := range resp.GetOrders() {
		rows[i] = newOrderRow(o)
	}
	log.WithField("page", page).WithField("total", resp.GetTotal()).Info("orders")

	if err := getTemplates().ExecuteTemplate(w, "orders_history", injectCommonTemplateData(r, map[string]interface{}{
		"title":      "Your orders",
		"categories": fe.navCategories(r.Context()),
		"rows":       rows,
		"total":      resp.GetTotal(),
		"pagination": ordersPagination(page, int(resp.GetTotal())),
	})); err != nil {
		log.Println(err)
	}
}

func pagesOf(total, size int) int { return (total + size - 1) / size }

// ordersPagination numbers the pages of the history like the catalogue does.
func ordersPagination(page, total int) pagination {
	pages := pagesOf(total, ordersPageSize)
	pg := pagination{Page: page, Pages: pages}
	if pages <= 1 {
		return pg
	}
	link := func(n int) string {
		if n <= 1 {
			return baseUrl + "/orders"
		}
		return baseUrl + "/orders?page=" + strconv.Itoa(n)
	}
	if page > 1 {
		pg.PrevURL = link(page - 1)
	}
	if page < pages {
		pg.NextURL = link(page + 1)
	}
	last := 0
	for n := 1; n <= pages; n++ {
		if n != 1 && n != pages && (n < page-2 || n > page+2) {
			continue
		}
		if last != 0 && n > last+1 {
			pg.Links = append(pg.Links, pageLink{Gap: true})
		}
		pg.Links = append(pg.Links, pageLink{N: n, URL: link(n), Current: n == page})
		last = n
	}
	return pg
}

// orderTrackingHandler shows one order of the visitor's session. An unknown id, an id of
// somebody else's order and a malformed id all give the same 404 page. With HX-Request it
// answers only the live part (status and tracker); 286 tells htmx to stop polling.
func (fe *frontendServer) orderTrackingHandler(w http.ResponseWriter, r *http.Request) {
	log := r.Context().Value(ctxKeyLog{}).(logrus.FieldLogger)
	if fe.orders == nil {
		fe.renderHTTPError(log, r, w, errors.New("orders are not enabled"), http.StatusNotFound)
		return
	}
	id := mux.Vars(r)["id"]
	if !orderIDPattern.MatchString(id) {
		fe.renderHTTPError(log, r, w, errors.New("malformed order id"), http.StatusNotFound)
		return
	}
	if fe.identityUnavailable(log, r, w) {
		return
	}
	o := fe.ownedOrder(log, w, r, id)
	if o == nil {
		return
	}
	view := newTrackView(o)
	if !isFragmentRequest(r) {
		view.EmailURL = fe.confirmationEmailURL(r.Context(), log, id) // the polls do not ask again
	}
	log.WithField("order", id).WithField("status", o.GetStatus().String()).Info("order tracking")

	w.Header().Set("Vary", "HX-Request")
	if isFragmentRequest(r) {
		if !view.Active {
			w.WriteHeader(286) // htmx: swap this answer, then stop polling
		}
		if err := getTemplates().ExecuteTemplate(w, "order_live", injectCommonTemplateData(r, map[string]interface{}{"o": view})); err != nil {
			log.Println(err)
		}
		return
	}
	if err := getTemplates().ExecuteTemplate(w, "order_tracking", injectCommonTemplateData(r, map[string]interface{}{
		"title":      "Order " + view.ShortID,
		"categories": fe.navCategories(r.Context()),
		"o":          view,
	})); err != nil {
		log.Println(err)
	}
}

// ownedOrder fetches order id for the visitor: the guard is the signed-in user, or the session of
// an anonymous visitor. It answers the 404 / 503 page itself and returns nil when the visitor may not
// see the order (unknown, somebody else's, no session) or order-service could not answer. The caller has
// checked fe.orders, the id's shape and the identity already.
func (fe *frontendServer) ownedOrder(log logrus.FieldLogger, w http.ResponseWriter, r *http.Request, id string) *pbo.Order {
	req := &pbo.GetOrderRequest{OrderId: id}
	owns := func(o *pbo.Order) bool { return o.GetSessionId() == req.SessionId }
	if u := userOf(r); u != nil {
		req.UserId = u.ID
		owns = func(o *pbo.Order) bool { return o.GetUserId() == req.UserId }
	} else if req.SessionId = sessionID(r); req.SessionId == "" {
		fe.renderHTTPError(log, r, w, errors.New("no session"), http.StatusNotFound)
		return nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), orderReadTimeout)
	defer cancel()
	o, err := fe.orders.GetOrder(ctx, req)
	if err != nil {
		fe.failOrders(log, r, w, err, "could not get order")
		return nil
	}
	if !owns(o) {
		// the service guards this already; a second look costs nothing and keeps a wrong order off the page
		fe.renderHTTPError(log, r, w, errors.New("order belongs to somebody else"), http.StatusNotFound)
		return nil
	}
	return o
}
