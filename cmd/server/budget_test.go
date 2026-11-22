package main

import (
	"context"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	pb "frontend/genproto"
)

// ---------------------------------------------------------------------------
// RPC budget per page (PERF-02, PERF-03): every backend call the real handlers make is counted by a
// server interceptor on the in-process gRPC server, per full method name. The numbers in README
// "RPC budget per page" come from here; a change that adds a call to a page fails these tests and
// has to change the budget on purpose. Clocks are injected, nothing here sleeps.
// ---------------------------------------------------------------------------

type rpcCounter struct {
	mu sync.Mutex
	n  map[string]int
}

func (c *rpcCounter) reset() {
	c.mu.Lock()
	c.n = map[string]int{}
	c.mu.Unlock()
}

func (c *rpcCounter) intercept(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
	c.mu.Lock()
	c.n[info.FullMethod[strings.LastIndex(info.FullMethod, "/")+1:]]++
	c.mu.Unlock()
	return h(ctx, req)
}

// snapshot returns "Method=n" pairs sorted, and the total.
func (c *rpcCounter) snapshot() (string, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var parts []string
	total := 0
	for m, n := range c.n {
		parts = append(parts, m+"="+strconv.Itoa(n))
		total += n
	}
	sort.Strings(parts)
	return strings.Join(parts, " "), total
}

// recoOf recommends the same four products every time.
type recoOf struct {
	pb.UnimplementedRecommendationServiceServer
	ids []string
}

func (r recoOf) ListRecommendations(context.Context, *pb.ListRecommendationsRequest) (*pb.ListRecommendationsResponse, error) {
	return &pb.ListRecommendationsResponse{ProductIds: r.ids}, nil
}

// budgetFrontend is a frontend over bufconn fakes of every backend (inventory and review through
// their client fakes), with a counting interceptor and injected clocks for all three caches.
func budgetFrontend(t *testing.T, cart *fakeCart) (*frontendServer, *rpcCounter, *testClock, *countingCurrency) {
	t.Helper()
	cnt := &rpcCounter{n: map[string]int{}}
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer(grpc.UnaryInterceptor(cnt.intercept))
	pb.RegisterProductCatalogServiceServer(srv, sampleCatalog())
	pb.RegisterCurrencyServiceServer(srv, fakeCurrency{})
	pb.RegisterCartServiceServer(srv, cart)
	pb.RegisterShippingServiceServer(srv, fakeShipping{})
	pb.RegisterCheckoutServiceServer(srv, &fakeCheckout{})
	pb.RegisterRecommendationServiceServer(srv, recoOf{ids: []string{"pan-10", "pan-11", "pan-12", "pan-13", "pan-14"}})
	pb.RegisterAdServiceServer(srv, fakeAds{})
	go func() { _ = srv.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(); srv.Stop() })
	fe := &frontendServer{productCatalogSvcConn: conn, currencySvcConn: conn, cartSvcConn: conn,
		recommendationSvcConn: conn, adSvcConn: conn, shippingSvcConn: conn, checkoutSvcConn: conn}
	cur := &countingCurrency{}
	fe.currencyClient = cur // the currency calls do not reach the server: counted by the client double
	fe.reviews = &fakeReviews{}
	fe.inventory = &fakeInventory{stock: map[string]int32{}}
	fe.products.configureForProducts()
	clk := newTestClock()
	fe.currency.now, fe.products.now, fe.nav.now = clk.Now, clk.Now, clk.Now
	return fe, cnt, clk, cur
}

func threeLineCart() *fakeCart {
	return &fakeCart{items: []*pb.CartItem{
		{ProductId: "pan-03", Quantity: 1}, {ProductId: "pan-04", Quantity: 2}, {ProductId: "soap-1", Quantity: 1},
	}}
}

// measure serves one GET in EUR and returns the RPCs it made ("Method=n ..." sorted, and the total).
func measure(t *testing.T, fe *frontendServer, cnt *rpcCounter, cur *countingCurrency, target string) (string, int) {
	t.Helper()
	cnt.reset()
	s0, c0 := cur.supportedCalls.Load(), cur.convertCalls.Load()
	inv, rev := fe.inventory.(*fakeInventory), fe.reviews.(*fakeReviews)
	inv.mu.Lock()
	inv.calls = nil
	inv.mu.Unlock()
	rev.mu.Lock()
	rev.summaryCalls, rev.listCalls = nil, nil
	rev.mu.Unlock()

	rr := do(fe, http.MethodGet, target, "", map[string]string{"Cookie": cookieCurrency + "=EUR"})
	if rr.Code != http.StatusOK {
		t.Fatalf("%s: status %d", target, rr.Code)
	}
	snap, total := cnt.snapshot()
	extra := map[string]int{"GetStock": inv.callCount(), "GetRatingSummary": len(rev.summaryCalls), "ListReviews": len(rev.listCalls),
		"GetSupportedCurrencies": int(cur.supportedCalls.Load() - s0), "Convert": int(cur.convertCalls.Load() - c0)}
	var parts []string
	parts = append(parts, strings.Fields(snap)...)
	for _, m := range []string{"GetRatingSummary", "GetStock", "ListReviews", "GetSupportedCurrencies", "Convert"} {
		if extra[m] > 0 {
			parts = append(parts, m+"="+strconv.Itoa(extra[m]))
			total += extra[m]
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, " "), total
}

func TestRPCBudgetPerPage(t *testing.T) {
	fe, cnt, clk, cur := budgetFrontend(t, threeLineCart())
	type step struct {
		name, target string
		want         string
		total        int
	}
	// cart: 3 lines (pan-03, pan-04, soap-1), recommendation-service answers 5 ids (4 are used),
	// currency EUR, 24 cards per listing page. Cold = every cache empty, warm = right after.
	steps := []step{
		{"home cold", "/", "Convert=23 GetAds=1 GetCart=1 GetProduct=3 GetRatingSummary=1 GetStock=1 GetSupportedCurrencies=1 ListCategories=1 ListProductsFiltered=1", 33},
		{"home warm", "/", "GetAds=1 GetCart=1 GetRatingSummary=1 GetStock=1 ListCategories=1 ListProductsFiltered=1", 6},
		{"product cold", "/product/pan-05", "Convert=1 GetAds=1 GetCart=1 GetProduct=8 GetRatingSummary=1 GetStock=1 GetSupportedCurrencies=1 ListCategories=1 ListRecommendations=1 ListReviews=1", 17},
		{"product warm", "/product/pan-05", "GetAds=1 GetCart=1 GetProduct=1 GetRatingSummary=1 GetStock=1 ListCategories=1 ListRecommendations=1 ListReviews=1", 8},
		{"cart cold", "/cart", "Convert=3 GetCart=1 GetProduct=7 GetQuote=1 GetStock=1 GetSupportedCurrencies=1 ListCategories=1 ListRecommendations=1", 16},
		{"cart warm", "/cart", "GetCart=1 GetQuote=1 GetStock=1 ListRecommendations=1", 4},
	}
	for _, s := range steps {
		if strings.HasSuffix(s.name, "cold") {
			clk.Advance(time.Hour) // every TTL is over
		}
		got, total := measure(t, fe, cnt, cur, s.target)
		t.Logf("%-13s %2d RPCs: %s", s.name, total, got)
		if s.want != "" && (got != s.want || total != s.total) {
			t.Errorf("%s: %d RPCs %q, budget %d %q", s.name, total, got, s.total, s.want)
		}
	}
}

func TestProductCache_TTLNotFoundAndCopies(t *testing.T) {
	fe, cnt, clk, _ := budgetFrontend(t, &fakeCart{})
	ctx := context.Background()
	get := func(id string) (*pb.Product, error) { return fe.getProductCached(ctx, id) }
	calls := func() int { _, n := cnt.snapshot(); return n }

	p, err := get("pan-05")
	if err != nil || p.GetName() != "Pan 05" || calls() != 1 {
		t.Fatalf("first: %v %v, %d RPCs", p, err, calls())
	}
	p.Name = "changed by a handler"
	p2, _ := get("pan-05")
	if p2.GetName() != "Pan 05" || calls() != 1 {
		t.Errorf("second: name %q, %d RPCs (want a hit and an untouched copy)", p2.GetName(), calls())
	}
	clk.Advance(productCacheTTL - time.Second)
	_, _ = get("pan-05")
	if calls() != 1 {
		t.Errorf("just inside the TTL: %d RPCs", calls())
	}
	clk.Advance(2 * time.Second)
	_, _ = get("pan-05")
	if calls() != 2 {
		t.Errorf("after the TTL: %d RPCs, want 2", calls())
	}

	// NotFound is kept for the TTL too (a stale cart line must not cost an RPC per page) and stays NotFound
	for i := 0; i < 3; i++ {
		if _, err := get("gone"); status.Code(err) != codes.NotFound {
			t.Fatalf("gone: %v", err)
		}
	}
	if calls() != 3 {
		t.Errorf("3 lookups of a missing product: %d RPCs in total, want 3 (2 + 1)", calls())
	}
	clk.Advance(productCacheTTL + time.Second)
	_, _ = get("gone")
	if calls() != 4 {
		t.Errorf("missing product after the TTL: %d RPCs, want 4", calls())
	}

}

// flakyCatalog fails the first GetProduct with Unavailable, then answers.
type flakyCatalog struct {
	pb.UnimplementedProductCatalogServiceServer
	calls atomic.Int32
}

func (f *flakyCatalog) GetProduct(_ context.Context, r *pb.GetProductRequest) (*pb.Product, error) {
	if f.calls.Add(1) == 1 {
		return nil, status.Error(codes.Unavailable, "down")
	}
	return &pb.Product{Id: r.Id, Name: "Pan"}, nil
}

func TestProductCache_ErrorsAreNotCached(t *testing.T) {
	cat := &flakyCatalog{}
	fe := &frontendServer{productCatalogSvcConn: connTo(t, func(s *grpc.Server) { pb.RegisterProductCatalogServiceServer(s, cat) })}
	clk := newTestClock()
	fe.products.now = clk.Now
	if _, err := fe.getProductCached(context.Background(), "pan-06"); status.Code(err) != codes.Unavailable {
		t.Fatalf("first lookup: %v, want Unavailable", err)
	}
	if p, err := fe.getProductCached(context.Background(), "pan-06"); err != nil || p.GetName() != "Pan" {
		t.Fatalf("second lookup: %v, %v (an error must not be kept)", p, err)
	}
	if cat.calls.Load() != 2 {
		t.Errorf("%d RPCs, want 2", cat.calls.Load())
	}
}

func TestProductCache_IsBoundedAndSharesConcurrentLookups(t *testing.T) {
	fe, cnt, _, _ := budgetFrontend(t, &fakeCart{})
	ctx := context.Background()

	// concurrent lookups of one id: one RPC
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = fe.getProductCached(ctx, "pan-07") }()
	}
	wg.Wait()
	if _, n := cnt.snapshot(); n != 1 {
		t.Errorf("20 concurrent lookups made %d RPCs, want 1", n)
	}

	// the map never grows past its bound (unknown ids are cached as NotFound, so this fills it)
	for i := 0; i < productCacheMaxEntries+50; i++ {
		_, _ = fe.getProductCached(ctx, "unknown-"+strconv.Itoa(i))
	}
	fe.products.mu.Lock()
	n := len(fe.products.entries)
	fe.products.mu.Unlock()
	if n > productCacheMaxEntries {
		t.Errorf("%d entries, bound %d", n, productCacheMaxEntries)
	}
}
