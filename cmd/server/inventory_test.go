package main

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "frontend/genproto"
	pbi "frontend/genproto/inventory"
)

// ---------------------------------------------------------------------------
// fake inventory client (implements pbi.InventoryServiceClient, in memory, no network)
// ---------------------------------------------------------------------------

type fakeInventory struct {
	pbi.InventoryServiceClient // Reserve/Commit/Release are checkout's business: calling them panics

	mu    sync.Mutex
	stock map[string]int32 // live level by product id; a missing id is "unknown to inventory"
	calls [][]string       // product ids of every GetStock call
	err   error            // returned by GetStock when set
	block bool             // wait for the caller's deadline, like a hung service
}

func (f *fakeInventory) GetStock(ctx context.Context, in *pbi.GetStockRequest, _ ...grpc.CallOption) (*pbi.GetStockResponse, error) {
	f.mu.Lock()
	f.calls = append(f.calls, in.ProductIds)
	f.mu.Unlock()
	if f.block {
		<-ctx.Done()
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	if f.err != nil {
		return nil, f.err
	}
	out := &pbi.GetStockResponse{}
	for _, id := range in.ProductIds {
		if n, ok := f.stock[id]; ok {
			out.Levels = append(out.Levels, &pbi.StockLevel{ProductId: id, Available: n})
		}
	}
	return out, nil
}

func (f *fakeInventory) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// frontendWithInventory is the sample catalog (pan-01 0, pan-02 3, others 20, soap-1 0, cream-1 4 in the catalogue) plus inv.
func frontendWithInventory(t *testing.T, inv *fakeInventory, cart *fakeCart, co *fakeCheckout) *frontendServer {
	t.Helper()
	if cart == nil {
		cart = &fakeCart{}
	}
	if co == nil {
		co = &fakeCheckout{}
	}
	fe := newFakeFrontendWith(t, sampleCatalog(), cart, co)
	if inv != nil {
		fe.inventory = inv
	}
	return fe
}

// ---------------------------------------------------------------------------
// config
// ---------------------------------------------------------------------------

func TestInventoryAddrFromEnv(t *testing.T) {
	t.Setenv("INVENTORY_SERVICE_ADDR", "inv:1")
	if got := inventoryAddrFromEnv(); got != "inv:1" {
		t.Errorf("set: %q", got)
	}
	t.Setenv("INVENTORY_SERVICE_ADDR", "  ")
	if got := inventoryAddrFromEnv(); got != "" {
		t.Errorf("set but empty must switch live stock off, got %q", got)
	}
	// unset: the default (t.Setenv registered the cleanup, unset explicitly for the check)
	t.Setenv("INVENTORY_SERVICE_ADDR", "x")
	if err := unsetenv("INVENTORY_SERVICE_ADDR"); err != nil {
		t.Fatal(err)
	}
	if got := inventoryAddrFromEnv(); got != "inventory-service:8087" {
		t.Errorf("default: %q", got)
	}
}

// ---------------------------------------------------------------------------
// overlay
// ---------------------------------------------------------------------------

func TestOverlay_StockBadgeThresholds(t *testing.T) {
	for _, c := range []struct {
		n         int32
		out, low  bool
		badgeWant string
	}{
		{0, true, false, "Out of stock"},
		{-3, true, false, "Out of stock"},
		{1, false, true, "Only 1 left"},
		{5, false, true, "Only 5 left"},
		{6, false, false, ""},
		{500, false, false, ""},
	} {
		got := stockInfoOf(c.n)
		if got.Out != c.out || got.Low != c.low || got.N != c.n {
			t.Errorf("stockInfoOf(%d) = %+v", c.n, got)
		}
	}
}

func TestOverlay_ListingUsesLiveStockWithOneCall(t *testing.T) {
	inv := &fakeInventory{stock: map[string]int32{
		"pan-01": 7, // catalogue says 0: back in stock
		"pan-02": 0, // catalogue says 3: sold out
		"pan-03": 2, // catalogue says 20: low
		"pan-04": 5, // low, boundary
		"pan-05": 6, // nothing
	}}
	fe := frontendWithInventory(t, inv, nil, nil)
	rr := do(fe, http.MethodGet, "/?q=pan", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	html := rr.Body.String()
	if inv.callCount() != 1 {
		t.Fatalf("expected ONE GetStock call per page, got %d", inv.callCount())
	}
	if got := inv.calls[0]; len(got) != catalogPageSize {
		t.Errorf("the call should hold the %d ids of the page, got %d", catalogPageSize, len(got))
	}
	assertContains(t, html, "Only 2 left", "Only 5 left", "Out of stock")
	if strings.Contains(html, "Only 3 left") {
		t.Error("the catalogue's stock of pan-02 (3) is shown although the live stock is 0")
	}
	if strings.Contains(html, "Only 6 left") {
		t.Error("6 left must have no badge")
	}
	// pan-01 (catalogue 0, live 7) has no badge, pan-02 (catalogue 3, live 0) has "Out of stock": exactly one out badge
	// among the first 24 pans (the other zero-stock products are not on this page)
	if n := strings.Count(html, "stock-badge is-out"); n != 1 {
		t.Errorf("out-of-stock badges: %d, want 1 (pan-02)", n)
	}
}

func TestOverlay_FragmentAndSearchAlsoUseLiveStock(t *testing.T) {
	for _, target := range []string{"/?page=2", "/?category=beauty", "/?q=cream"} {
		inv := &fakeInventory{stock: map[string]int32{"cream-1": 1, "pan-29": 1, "pan-30": 1}}
		fe := frontendWithInventory(t, inv, nil, nil)
		rr := do(fe, http.MethodGet, target, "", map[string]string{"HX-Request": "true"})
		if rr.Code != http.StatusOK || inv.callCount() != 1 {
			t.Errorf("%s (htmx): status %d, %d calls", target, rr.Code, inv.callCount())
		}
		if target == "/?q=cream" {
			assertContains(t, rr.Body.String(), "Only 1 left")
		}
	}
}

func TestOverlay_NoCallForAnEmptyPage(t *testing.T) {
	inv := &fakeInventory{}
	fe := frontendWithInventory(t, inv, nil, nil)
	if rr := do(fe, http.MethodGet, "/?q=zzzz", "", nil); rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	if inv.callCount() != 0 {
		t.Errorf("no products, no GetStock call; got %d", inv.callCount())
	}
}

func TestOverlay_ProductPageBadgeAndQuantityLimit(t *testing.T) {
	inv := &fakeInventory{stock: map[string]int32{"pan-05": 3, "cream-1": 0, "soap-1": 9, "pan-06": 8}}
	fe := frontendWithInventory(t, inv, nil, nil)

	rr := do(fe, http.MethodGet, "/product/pan-05", "", nil) // catalogue 20, live 3
	html := rr.Body.String()
	assertContains(t, html, "Only 3 left in stock", "Add to cart</button>")
	if got := strings.Join(options(html), " "); got != "1 2 3" {
		t.Errorf("quantity options %q, want the live limit 1 2 3", got)
	}
	if inv.callCount() != 1 || len(inv.calls[0]) != 1 || inv.calls[0][0] != "pan-05" {
		t.Errorf("one GetStock call for the product, got %v", inv.calls)
	}

	html = do(fe, http.MethodGet, "/product/cream-1", "", nil).Body.String() // catalogue 4, live 0
	assertContains(t, html, "Out of stock", `disabled>Out of stock</button>`)
	if strings.Contains(html, "Add to cart</button>") {
		t.Error("sold out product must not offer add to cart")
	}

	html = do(fe, http.MethodGet, "/product/soap-1", "", nil).Body.String() // catalogue 0, live 9
	assertContains(t, html, "Add to cart</button>")
	if strings.Contains(html, "disabled>Out of stock") {
		t.Error("restocked product is still disabled")
	}

	html = do(fe, http.MethodGet, "/product/pan-06", "", nil).Body.String() // 8: no badge
	assertContains(t, html, "In stock")
	if strings.Contains(html, "left in stock") {
		t.Error("8 left must show no low-stock text")
	}
}

func TestOverlay_FallsBackToTheCatalogue(t *testing.T) {
	cases := map[string]*fakeInventory{
		"service not configured": nil,
		"service down":           {err: status.Error(codes.Unavailable, "connection refused")},
		"internal error":         {err: status.Error(codes.Internal, "pq: boom")},
		"product unknown":        {stock: map[string]int32{"other": 1}}, // answer without the ids: they are missing
	}
	for name, inv := range cases {
		var fe *frontendServer
		if inv == nil {
			fe = frontendWithInventory(t, nil, nil, nil)
		} else {
			fe = frontendWithInventory(t, inv, nil, nil)
		}
		rr := do(fe, http.MethodGet, "/?q=pan", "", nil)
		if rr.Code != http.StatusOK {
			t.Errorf("%s: home status %d", name, rr.Code)
			continue
		}
		assertContains(t, rr.Body.String(), "Only 3 left", "Out of stock") // catalogue: pan-02 3, pan-01 0
		html := do(fe, http.MethodGet, "/product/cream-1", "", nil)
		if html.Code != http.StatusOK {
			t.Errorf("%s: product status %d", name, html.Code)
		}
		assertContains(t, html.Body.String(), "Only 4 left in stock")
		if rr := do(fe, http.MethodPost, "/cart", form("product_id", "cream-1", "quantity", "5"), nil); rr.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: add above the catalogue stock: status %d", name, rr.Code)
		}
		if rr := do(fe, http.MethodGet, "/cart", "", nil); rr.Code != http.StatusOK {
			t.Errorf("%s: cart status %d", name, rr.Code)
		}
	}
}

func TestOverlay_HungServiceIsCutAtOneSecond(t *testing.T) {
	inv := &fakeInventory{block: true}
	fe := frontendWithInventory(t, inv, nil, nil)
	start := time.Now()
	rr := do(fe, http.MethodGet, "/product/cream-1", "", nil)
	took := time.Since(start)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	assertContains(t, rr.Body.String(), "Only 4 left in stock")
	if took < inventoryReadTimeout-100*time.Millisecond || took > inventoryReadTimeout+1500*time.Millisecond {
		t.Errorf("the page took %v, want about %v", took, inventoryReadTimeout)
	}
	if inventoryReadTimeout != time.Second {
		t.Errorf("the contract says a 1 s deadline, got %v", inventoryReadTimeout)
	}
}

func TestOverlay_DownServiceIsLoggedAtWarn(t *testing.T) {
	e := newAcct(t)
	e.fe.inventory = &fakeInventory{err: status.Error(codes.Unavailable, "down")}
	e.cart.carts["anon-1"] = []*pb.CartItem{{ProductId: "pan-05", Quantity: 1}}
	rr := e.serve(http.MethodGet, "/cart", "", anon("anon-1"), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	if !strings.Contains(e.logs.String(), "live stock unavailable") || !strings.Contains(e.logs.String(), "level=warning") {
		t.Errorf("expected a warn log, got %s", e.logs.String())
	}
}

func TestOverlay_HelperDedupesAndCapsIds(t *testing.T) {
	inv := &fakeInventory{}
	fe := &frontendServer{inventory: inv}
	var ps []*pb.Product
	for i := 0; i < 150; i++ {
		ps = append(ps, &pb.Product{Id: "p" + strings.Repeat("x", i%120)}) // 120 distinct ids, some repeated
	}
	ps = append(ps, nil, &pb.Product{}) // nil and id-less products are skipped
	fe.overlayStock(context.Background(), newTestLogger(), ps...)
	if inv.callCount() != 1 || len(inv.calls[0]) != maxStockIDs {
		t.Fatalf("calls %d, ids %d, want 1 call with %d ids", inv.callCount(), len(inv.calls[0]), maxStockIDs)
	}
	seen := map[string]bool{}
	for _, id := range inv.calls[0] {
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}

// ---------------------------------------------------------------------------
// add to cart limits
// ---------------------------------------------------------------------------

func TestAddToCart_UsesTheLiveStockForTheLimit(t *testing.T) {
	inv := &fakeInventory{stock: map[string]int32{"pan-05": 2, "cream-1": 9, "pan-06": 0}}
	cart := &fakeCart{}
	fe := frontendWithInventory(t, inv, cart, nil)

	rr := do(fe, http.MethodPost, "/cart", form("product_id", "pan-05", "quantity", "3"), nil) // catalogue 20, live 2
	if rr.Code != http.StatusUnprocessableEntity || len(cart.added) != 0 {
		t.Fatalf("status %d, added %d", rr.Code, len(cart.added))
	}
	assertContains(t, rr.Body.String(), "Only 2 of Pan 05 in stock.")

	if rr := do(fe, http.MethodPost, "/cart", form("product_id", "pan-05", "quantity", "2"), nil); rr.Code != http.StatusFound || len(cart.added) != 1 {
		t.Errorf("quantity equal to the live stock: status %d, added %d", rr.Code, len(cart.added))
	}

	// catalogue says 4, live 9: more than the catalogue's number is fine
	if rr := do(fe, http.MethodPost, "/cart", form("product_id", "cream-1", "quantity", "6"), nil); rr.Code != http.StatusFound {
		t.Errorf("live stock above the catalogue's: status %d", rr.Code)
	}

	rr = do(fe, http.MethodPost, "/cart", form("product_id", "pan-06", "quantity", "1"), nil) // sold out live
	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("sold out: status %d", rr.Code)
	}
	assertContains(t, rr.Body.String(), "Pan 06 is out of stock.")
}

func TestAddToCart_CountsTheCartAgainstTheLiveStock(t *testing.T) {
	inv := &fakeInventory{stock: map[string]int32{"pan-05": 4}}
	cart := &fakeCart{items: []*pb.CartItem{{ProductId: "pan-05", Quantity: 3}}}
	fe := frontendWithInventory(t, inv, cart, nil)
	rr := do(fe, http.MethodPost, "/cart", form("product_id", "pan-05", "quantity", "2"), nil)
	if rr.Code != http.StatusUnprocessableEntity || len(cart.added) != 0 {
		t.Fatalf("status %d, added %d", rr.Code, len(cart.added))
	}
	assertContains(t, rr.Body.String(), "Only 4 of Pan 05 in stock, and 3 already in your cart.")
	if rr := do(fe, http.MethodPost, "/cart", form("product_id", "pan-05", "quantity", "1"), nil); rr.Code != http.StatusFound {
		t.Errorf("the last unit: status %d", rr.Code)
	}
}

func TestAddToCart_ServiceDownUsesTheCatalogueLimit(t *testing.T) {
	inv := &fakeInventory{err: status.Error(codes.Unavailable, "down")}
	cart := &fakeCart{}
	fe := frontendWithInventory(t, inv, cart, nil)
	if rr := do(fe, http.MethodPost, "/cart", form("product_id", "cream-1", "quantity", "5"), nil); rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("above the catalogue stock: status %d", rr.Code)
	}
	if rr := do(fe, http.MethodPost, "/cart", form("product_id", "cream-1", "quantity", "4"), nil); rr.Code != http.StatusFound || len(cart.added) != 1 {
		t.Errorf("within the catalogue stock: status %d, added %d", rr.Code, len(cart.added))
	}
}

// ---------------------------------------------------------------------------
// cart page
// ---------------------------------------------------------------------------

func TestCart_ShowsLiveLimitsAndNotes(t *testing.T) {
	inv := &fakeInventory{stock: map[string]int32{
		"pan-05": 2, // line of 3: too many
		"pan-06": 0, // line of 1: sold out
		"pan-07": 4, // line of 2: fits, low
		"pan-08": 50,
	}}
	cart := &fakeCart{items: []*pb.CartItem{
		{ProductId: "pan-05", Quantity: 3}, {ProductId: "pan-06", Quantity: 1},
		{ProductId: "pan-07", Quantity: 2}, {ProductId: "pan-08", Quantity: 2},
	}}
	fe := frontendWithInventory(t, inv, cart, nil)
	rr := do(fe, http.MethodGet, "/cart", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	html := rr.Body.String()
	if inv.callCount() != 1 || len(inv.calls[0]) != 4 {
		t.Fatalf("expected ONE GetStock call with the 4 cart products, got %v", inv.calls)
	}
	assertContains(t, html,
		"Only 2 left. You have 3 in your cart", "empty the cart and add 2",
		"Sold out. This item can not be ordered right now.",
		`cart-item-note is-low">Only 4 left`)
	if n := strings.Count(html, "cart-item-note"); n != 3 {
		t.Errorf("notes: %d, want 3 (pan-08 has plenty left)", n)
	}
}

func TestCart_NoNoteWithoutInventoryWhenTheCatalogueFits(t *testing.T) {
	cart := &fakeCart{items: []*pb.CartItem{{ProductId: "pan-05", Quantity: 3}}}
	fe := frontendWithInventory(t, nil, cart, nil)
	html := do(fe, http.MethodGet, "/cart", "", nil).Body.String()
	if strings.Contains(html, "cart-item-note") {
		t.Error("note shown for a line that fits")
	}
	// catalogue fallback still flags a line above the catalogue stock (cream-1: 4)
	cart.items = []*pb.CartItem{{ProductId: "cream-1", Quantity: 6}}
	assertContains(t, do(fe, http.MethodGet, "/cart", "", nil).Body.String(), "Only 4 left. You have 6 in your cart")
}

func TestCart_ServiceDownStillRenders(t *testing.T) {
	cart := &fakeCart{items: []*pb.CartItem{{ProductId: "pan-05", Quantity: 3}}}
	fe := frontendWithInventory(t, &fakeInventory{err: status.Error(codes.DeadlineExceeded, "slow")}, cart, nil)
	rr := do(fe, http.MethodGet, "/cart", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	assertContains(t, rr.Body.String(), "Pan 05", "Place order")
}

// ---------------------------------------------------------------------------
// cart merge on sign-in
// ---------------------------------------------------------------------------

func TestCartMerge_UsesTheLiveStockWithOneCall(t *testing.T) {
	e := newAcct(t)
	inv := &fakeInventory{stock: map[string]int32{
		"pan-05": 1, // catalogue 20
		"pan-02": 8, // catalogue 3
		"pan-03": 0, // catalogue 20: sold out
		"pan-04": 5, // user holds 2: room for 3
	}}
	e.fe.inventory = inv
	u, _ := e.users.signIn("ada@example.com", "Ada", "pw-pw-pw-pw")
	e.users.sessions = map[string]*fakeUser{}
	e.cart.carts["anon-1"] = []*pb.CartItem{
		{ProductId: "pan-05", Quantity: 4}, {ProductId: "pan-02", Quantity: 6}, {ProductId: "pan-03", Quantity: 1},
		{ProductId: "pan-04", Quantity: 9}, {ProductId: "OLJCESPC7Z", Quantity: 1},
	}
	e.cart.carts[u.id] = []*pb.CartItem{{ProductId: "pan-04", Quantity: 2}}

	rr := e.serve(http.MethodPost, "/login", loginForm("ada@example.com", "pw-pw-pw-pw"), anon("anon-1"), nil)
	mustRedirect(t, rr, "/account")

	got := map[string]int32{}
	for _, it := range e.cart.carts[u.id] {
		got[it.ProductId] = it.Quantity
	}
	if got["pan-05"] != 1 || got["pan-02"] != 6 || got["pan-03"] != 0 || got["pan-04"] != 5 || len(got) != 3 {
		t.Errorf("user's cart %v: want pan-05 1, pan-02 6, pan-04 5 and no pan-03", got)
	}
	if inv.callCount() != 1 {
		t.Fatalf("expected ONE GetStock call for the whole merge, got %d", inv.callCount())
	}
	if len(inv.calls[0]) != 4 {
		t.Errorf("ids asked: %v (the product that left the catalogue is not asked)", inv.calls[0])
	}
	if _, left := e.cart.carts["anon-1"]; left {
		t.Error("the anonymous cart was not emptied")
	}
}

func TestCartMerge_InventoryDownFallsBackToTheCatalogueStock(t *testing.T) {
	e := newAcct(t)
	e.fe.inventory = &fakeInventory{err: status.Error(codes.Unavailable, "down")}
	u, _ := e.users.signIn("ada@example.com", "Ada", "pw-pw-pw-pw")
	e.users.sessions = map[string]*fakeUser{}
	e.cart.carts["anon-1"] = []*pb.CartItem{{ProductId: "pan-02", Quantity: 10}, {ProductId: "pan-05", Quantity: 2}}
	mustRedirect(t, e.serve(http.MethodPost, "/login", loginForm("ada@example.com", "pw-pw-pw-pw"), anon("anon-1"), nil), "/account")
	got := map[string]int32{}
	for _, it := range e.cart.carts[u.id] {
		got[it.ProductId] = it.Quantity
	}
	if got["pan-02"] != 3 || got["pan-05"] != 2 {
		t.Errorf("user's cart %v, want the catalogue caps (pan-02 3, pan-05 2)", got)
	}
}

// ---------------------------------------------------------------------------
// checkout idempotency key
// ---------------------------------------------------------------------------

var keyField = regexp.MustCompile(`<input type="hidden" name="idempotency_key" value="([^"]*)">`)

func renderedKey(t *testing.T, html string) string {
	t.Helper()
	m := keyField.FindAllStringSubmatch(html, -1)
	if len(m) != 1 {
		t.Fatalf("expected exactly one hidden idempotency_key input, got %d", len(m))
	}
	return m[0][1]
}

func TestIdempotencyKey_RenderedInTheCheckoutFormAndFreshPerRender(t *testing.T) {
	cart := &fakeCart{items: []*pb.CartItem{{ProductId: "pan-05", Quantity: 1}}}
	fe := frontendWithInventory(t, nil, cart, nil)
	k1 := renderedKey(t, do(fe, http.MethodGet, "/cart", "", nil).Body.String())
	k2 := renderedKey(t, do(fe, http.MethodGet, "/cart", "", nil).Body.String())
	if len(k1) < 22 || !validIdempotencyKey(k1) {
		t.Errorf("key %q: want 22+ characters of [A-Za-z0-9._-]", k1)
	}
	if k1 == k2 {
		t.Errorf("two renders must get two keys, both %q", k1)
	}
}

func TestIdempotencyKey_IsInsideTheCheckoutForm(t *testing.T) {
	cart := &fakeCart{items: []*pb.CartItem{{ProductId: "pan-05", Quantity: 1}}}
	html := do(frontendWithInventory(t, nil, cart, nil), http.MethodGet, "/cart", "", nil).Body.String()
	start := strings.Index(html, `class="checkout-form"`)
	end := start + strings.Index(html[start:], "</form>")
	if start < 0 || !strings.Contains(html[start:end], `name="idempotency_key"`) {
		t.Error("the key is not inside the checkout form")
	}
}

func TestNewIdempotencyKey_RandomAndValid(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		k := newIdempotencyKey()
		if len(k) != 22 || !validIdempotencyKey(k) || seen[k] {
			t.Fatalf("bad or repeated key %q", k)
		}
		seen[k] = true
	}
}

func TestValidIdempotencyKey(t *testing.T) {
	for k, want := range map[string]bool{
		"":                              true, // an old form: no replay protection
		"abc-DEF_123.x":                 true,
		strings.Repeat("a", 64):         true,
		strings.Repeat("a", 65):         false,
		"has space":                     false,
		"semi;colon":                    false,
		"ümlaut":                        false,
		"new\nline":                     false,
		"a\x00b":                        false,
		`"quote`:                        false,
		"22charactersAreEnough_ok":      true,
		strings.Repeat("-", 3) + "end.": true,
		"slash/not/allowed":             false,
		"<script>alert(1)</script>":     false,
	} {
		if got := validIdempotencyKey(k); got != want {
			t.Errorf("validIdempotencyKey(%q) = %v, want %v", k, got, want)
		}
	}
}

// keyedCheckout behaves like checkout with idempotency: the same key gives the same order.
type keyedCheckout struct {
	fakeCheckout
	mu     sync.Mutex
	byKey  map[string]string
	nextID int
	keys   []string
}

func (k *keyedCheckout) PlaceOrder(ctx context.Context, in *pb.PlaceOrderRequest) (*pb.PlaceOrderResponse, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.keys = append(k.keys, in.IdempotencyKey)
	if k.byKey == nil {
		k.byKey = map[string]string{}
	}
	id, ok := k.byKey[in.IdempotencyKey]
	if !ok || in.IdempotencyKey == "" {
		k.nextID++
		id = "order-" + string(rune('0'+k.nextID))
		k.byKey[in.IdempotencyKey] = id
	}
	return &pb.PlaceOrderResponse{Order: &pb.OrderResult{OrderId: id, ShippingTrackingId: "TR-1",
		ShippingCost: &pb.Money{CurrencyCode: "USD", Units: 5}}}, nil
}

func TestIdempotencyKey_PassedToCheckoutAndDoubleSubmitShowsTheSameOrder(t *testing.T) {
	kc := &keyedCheckout{}
	fe := newFakeFrontendWithCheckout(t, sampleCatalog(), &fakeCart{}, kc)

	sameKey := strings.Repeat("k", 24)
	body := goodOrder + "&idempotency_key="+sameKey
	r1 := do(fe, http.MethodPost, "/cart/checkout", body, nil)
	r2 := do(fe, http.MethodPost, "/cart/checkout", body, nil) // double click / reload of the POST
	if r1.Code != http.StatusOK || r2.Code != http.StatusOK {
		t.Fatalf("status %d / %d", r1.Code, r2.Code)
	}
	assertContains(t, r1.Body.String(), "order-1")
	assertContains(t, r2.Body.String(), "order-1")
	if strings.Contains(r2.Body.String(), "order-2") {
		t.Error("the second submit placed a second order")
	}
	if len(kc.keys) != 2 || kc.keys[0] != sameKey || kc.keys[1] != kc.keys[0] {
		t.Errorf("keys passed to checkout: %v", kc.keys)
	}

	// another render, another key: another order
	r3 := do(fe, http.MethodPost, "/cart/checkout", goodOrder+"&idempotency_key="+newIdempotencyKey(), nil)
	assertContains(t, r3.Body.String(), "order-2")
}

func TestIdempotencyKey_PlaceOrderRequestCarriesTheFormValue(t *testing.T) {
	co := &fakeCheckout{}
	fe := newFakeFrontendWith(t, sampleCatalog(), &fakeCart{}, co)
	formKey := strings.Repeat("a", 11) + "." + strings.Repeat("B", 5) + "_" + strings.Repeat("c", 5)
	rr := do(fe, http.MethodPost, "/cart/checkout", goodOrder+"&idempotency_key="+formKey, nil)
	if rr.Code != http.StatusOK || co.last.GetIdempotencyKey() != formKey {
		t.Errorf("status %d, key %q", rr.Code, co.last.GetIdempotencyKey())
	}
	// without a field (an old form) the request carries no key
	do(fe, http.MethodPost, "/cart/checkout", goodOrder, nil)
	if co.last.GetIdempotencyKey() != "" {
		t.Errorf("no field, but key %q", co.last.GetIdempotencyKey())
	}
}

func TestIdempotencyKey_MalformedValueIsRefusedBeforeCheckout(t *testing.T) {
	co := &fakeCheckout{}
	fe := newFakeFrontendWith(t, sampleCatalog(), &fakeCart{}, co)
	for _, bad := range []string{"has space", strings.Repeat("k", 65), "semi;colon"} {
		rr := do(fe, http.MethodPost, "/cart/checkout", goodOrder+"&idempotency_key="+url.QueryEscape(bad), nil)
		if rr.Code != http.StatusUnprocessableEntity {
			t.Errorf("%q: status %d, want 422", bad, rr.Code)
		}
		assertContains(t, rr.Body.String(), "checkout form is out of date")
	}
	if co.placed != 0 {
		t.Errorf("checkout was called %d times for a malformed key", co.placed)
	}
}

// ---------------------------------------------------------------------------
// error mapping of the saga's answers
// ---------------------------------------------------------------------------

func TestCheckout_AbortedGives409OrderInProgress(t *testing.T) {
	setOrdersEnabled(t, true)
	co := &fakeCheckout{err: status.Error(codes.Aborted, "this order is already being processed")}
	fe := newFakeFrontendWith(t, sampleCatalog(), &fakeCart{}, co)
	rr := do(fe, http.MethodPost, "/cart/checkout", goodOrder, nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409", rr.Code)
	}
	html := rr.Body.String()
	assertContains(t, html, "Your order is being processed", "Check Orders in a minute", "HTTP status 409")
	if strings.Contains(html, "already being processed") {
		t.Error("the backend's text must not be shown for 409")
	}

	setOrdersEnabled(t, false)
	rr = do(fe, http.MethodPost, "/cart/checkout", goodOrder, nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status %d", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "Check Orders") {
		t.Error("Orders is off: the page must not send the visitor there")
	}
}

func TestCheckout_OutOfStockGives422WithTheMessageAndALinkToTheCart(t *testing.T) {
	msg := `out of stock: "Wool Scarf" (only 2 left, you asked for 3), "Tote Bag" (sold out)`
	co := &fakeCheckout{err: status.Error(codes.FailedPrecondition, msg)}
	fe := newFakeFrontendWith(t, sampleCatalog(), &fakeCart{}, co)
	rr := do(fe, http.MethodPost, "/cart/checkout", goodOrder, nil)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422", rr.Code)
	}
	assertContains(t, rr.Body.String(),
		"Some items are out of stock",
		"Out of stock: &#34;Wool Scarf&#34; (only 2 left, you asked for 3), &#34;Tote Bag&#34; (sold out)",
		`href="/cart">Back to your cart</a>`)
	// wrapped like the handler wraps it
	if page := classifyError(status.Error(codes.FailedPrecondition, msg), 500); page.Code != 422 || !cartLinkFor(page) {
		t.Errorf("classify: %+v", page)
	}
}

func TestCheckout_OtherFailuresKeepTheirPages(t *testing.T) {
	// declined card: 422, no cart link; spent attempt: cart link; other 422: no link
	for _, c := range []struct {
		err  error
		want string
		link bool
	}{
		{status.Error(codes.FailedPrecondition, "card_declined: Your card (ending 0454) was declined by the issuer"), "Your card (ending 0454) was declined by the issuer", false},
		{status.Error(codes.FailedPrecondition, "this attempt failed, start the checkout again"), "This attempt failed, start the checkout again", true},
		{status.Error(codes.FailedPrecondition, "cart is empty"), "Cart is empty", false},
		{status.Error(codes.Unavailable, "stock check unavailable, try again"), msgUnavailable, false},
	} {
		fe := newFakeFrontendWith(t, sampleCatalog(), &fakeCart{}, &fakeCheckout{err: c.err})
		rr := do(fe, http.MethodPost, "/cart/checkout", goodOrder, nil)
		assertContains(t, rr.Body.String(), c.want)
		if has := strings.Contains(rr.Body.String(), "Back to your cart"); has != c.link {
			t.Errorf("%v: cart link %v, want %v", c.err, has, c.link)
		}
	}
}
