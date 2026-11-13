package main

import (
	"context"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "frontend/genproto"
)

// testClock is the injected clock: the cache never looks at the wall clock in these tests.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock { return &testClock{t: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)} }
func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// countingCurrency is a pb.CurrencyServiceClient that counts calls. Convert is a Go port of
// currency-service's convertMoney (float64 through EUR, floor of units and nanos), so "identical
// to the service's output" is checked against the real algorithm, rounding quirks included.
type countingCurrency struct {
	supportedCalls atomic.Int64
	convertCalls   atomic.Int64
	supported      []string
	err            atomic.Pointer[error] // Convert fails with this when set
	gate           chan struct{}         // when not nil, Convert blocks until it is closed
	started        chan struct{}         // receives one value per Convert that started (buffered)
}

var testRates = map[string]float64{"EUR": 1.0, "USD": 1.1305, "JPY": 126.40, "GBP": 0.85970, "CAD": 1.5128, "TRY": 6.1247}

func (c *countingCurrency) GetSupportedCurrencies(context.Context, *pb.Empty, ...grpc.CallOption) (*pb.GetSupportedCurrenciesResponse, error) {
	c.supportedCalls.Add(1)
	codes := c.supported
	if codes == nil {
		codes = []string{"EUR", "USD", "JPY", "BGN", "GBP", "CAD", "TRY", "XXX"}
	}
	return &pb.GetSupportedCurrenciesResponse{CurrencyCodes: codes}, nil
}

func (c *countingCurrency) Convert(ctx context.Context, in *pb.CurrencyConversionRequest, _ ...grpc.CallOption) (*pb.Money, error) {
	c.convertCalls.Add(1)
	if c.started != nil {
		select {
		case c.started <- struct{}{}:
		default:
		}
	}
	if c.gate != nil {
		select {
		case <-c.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if e := c.err.Load(); e != nil {
		return nil, *e
	}
	return convertLikeTheService(in.GetFrom(), in.GetToCode())
}

// carry/convertMoney of apps/currency-service/src/currency.ts
func convertLikeTheService(from *pb.Money, to string) (*pb.Money, error) {
	fromRate, ok1 := testRates[from.GetCurrencyCode()]
	toRate, ok2 := testRates[to]
	if !ok1 || !ok2 {
		return nil, status.Error(codes.InvalidArgument, "unknown currency")
	}
	carry := func(units, nanos float64) (float64, float64) {
		nanos += math.Mod(units, 1) * 1e9
		units = math.Floor(units) + math.Floor(nanos/1e9)
		nanos = math.Mod(nanos, 1e9)
		return units, nanos
	}
	eu, en := carry(float64(from.GetUnits())/fromRate, float64(from.GetNanos())/fromRate)
	en = math.Round(en)
	ru, rn := carry(eu*toRate, en*toRate)
	return &pb.Money{CurrencyCode: to, Units: int64(math.Floor(ru)), Nanos: int32(math.Floor(rn))}, nil
}

func newCachedFrontend(cur *countingCurrency) (*frontendServer, *testClock) {
	clk := newTestClock()
	fe := &frontendServer{currencyClient: cur}
	fe.currency.now = clk.Now
	return fe, clk
}

func usd(units int64, nanos int32) *pb.Money {
	return &pb.Money{CurrencyCode: "USD", Units: units, Nanos: nanos}
}

func TestCurrencyCache_ConvertIsIdenticalToTheService(t *testing.T) {
	cur := &countingCurrency{}
	fe, _ := newCachedFrontend(cur)
	ctx := context.Background()
	for _, m := range []*pb.Money{usd(12, 990000000), usd(0, 10000000), usd(1999, 0), usd(5, 5), {CurrencyCode: "EUR", Units: 7, Nanos: 333333333}} {
		for _, to := range []string{"USD", "EUR", "JPY", "GBP", "TRY"} {
			want, err := cur.Convert(ctx, &pb.CurrencyConversionRequest{From: m, ToCode: to})
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 3; i++ { // the first is a miss, the others hits
				got, err := fe.convertCurrency(ctx, m, to)
				if err != nil {
					t.Fatal(err)
				}
				if got.GetUnits() != want.GetUnits() || got.GetNanos() != want.GetNanos() || got.GetCurrencyCode() != want.GetCurrencyCode() {
					t.Errorf("%v -> %s #%d: got %v, service says %v", m, to, i, got, want)
				}
			}
		}
	}
}

func TestCurrencyCache_TTLWithInjectedClock(t *testing.T) {
	cur := &countingCurrency{}
	fe, clk := newCachedFrontend(cur)
	ctx := context.Background()
	conv := func() { _, _ = fe.convertCurrency(ctx, usd(10, 0), "EUR") }
	conv()
	conv()
	if n := cur.convertCalls.Load(); n != 1 {
		t.Fatalf("2 identical conversions made %d calls", n)
	}
	clk.Advance(conversionTTL - time.Second)
	conv()
	if n := cur.convertCalls.Load(); n != 1 {
		t.Errorf("just before the TTL: %d calls", n)
	}
	clk.Advance(2 * time.Second)
	conv()
	if n := cur.convertCalls.Load(); n != 2 {
		t.Errorf("after the TTL: %d calls, want 2", n)
	}

	for i := 0; i < 3; i++ {
		if _, err := fe.getCurrencies(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := cur.supportedCalls.Load(); n != 1 {
		t.Errorf("supported currencies fetched %d times for 3 calls", n)
	}
	clk.Advance(supportedCurrenciesTTL - time.Second)
	_, _ = fe.getCurrencies(ctx)
	if n := cur.supportedCalls.Load(); n != 1 {
		t.Errorf("supported before its TTL: %d", n)
	}
	clk.Advance(2 * time.Second)
	_, _ = fe.getCurrencies(ctx)
	if n := cur.supportedCalls.Load(); n != 2 {
		t.Errorf("supported after its TTL: %d, want 2", n)
	}
}

func TestCurrencyCache_KeyIncludesEverything(t *testing.T) {
	cur := &countingCurrency{}
	fe, _ := newCachedFrontend(cur)
	ctx := context.Background()
	for _, c := range []struct {
		m  *pb.Money
		to string
	}{
		{usd(10, 0), "EUR"}, {usd(10, 1), "EUR"}, {usd(11, 0), "EUR"}, {usd(10, 0), "GBP"},
		{&pb.Money{CurrencyCode: "EUR", Units: 10}, "EUR"},
	} {
		if _, err := fe.convertCurrency(ctx, c.m, c.to); err != nil {
			t.Fatal(err)
		}
	}
	if n := cur.convertCalls.Load(); n != 5 {
		t.Errorf("5 different conversions made %d calls", n)
	}
}

func TestCurrencyCache_ErrorsAreNotCached(t *testing.T) {
	cur := &countingCurrency{}
	fe, _ := newCachedFrontend(cur)
	ctx := context.Background()
	boom := status.Error(codes.Unavailable, "down")
	cur.err.Store(&boom)
	if _, err := fe.convertCurrency(ctx, usd(3, 0), "EUR"); status.Code(err) != codes.Unavailable {
		t.Fatalf("got %v", err)
	}
	cur.err.Store(nil)
	got, err := fe.convertCurrency(ctx, usd(3, 0), "EUR")
	if err != nil || got.GetCurrencyCode() != "EUR" {
		t.Fatalf("after recovery: %v, %v", got, err)
	}
	if n := cur.convertCalls.Load(); n != 2 {
		t.Errorf("%d calls, want 2 (the failure must not be remembered)", n)
	}
	// an unknown currency is the service's INVALID_ARGUMENT every time
	for i := 0; i < 2; i++ {
		if _, err := fe.convertCurrency(ctx, usd(1, 0), "XXX"); status.Code(err) != codes.InvalidArgument {
			t.Errorf("unknown target: %v", err)
		}
	}
	if _, err := fe.convertCurrency(ctx, nil, "EUR"); err == nil {
		t.Error("nil money must still be refused by the service")
	}
}

func TestCurrencyCache_ResultsAreCopies(t *testing.T) {
	cur := &countingCurrency{}
	fe, _ := newCachedFrontend(cur)
	ctx := context.Background()
	a, _ := fe.convertCurrency(ctx, usd(10, 0), "EUR")
	want := a.GetUnits()
	a.Units = 999999 // a handler is free to edit what it got
	b, _ := fe.convertCurrency(ctx, usd(10, 0), "EUR")
	if b.GetUnits() != want {
		t.Errorf("the cached value was changed through a returned copy: %d, want %d", b.GetUnits(), want)
	}
	c1, _ := fe.getCurrencies(ctx)
	c1[0] = "ZZZ"
	c2, _ := fe.getCurrencies(ctx)
	if c2[0] == "ZZZ" {
		t.Error("supported currencies: the cached slice was changed through a returned copy")
	}
	// only the whitelisted codes, in the service's order
	if got := join(c2); got != "EUR,USD,JPY,GBP,CAD,TRY" {
		t.Errorf("supported: %s", got)
	}
}

func join(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += ","
		}
		out += v
	}
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second) // generous: the host clock jumps
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestCurrencyCache_SingleflightCollapsesConcurrentCalls(t *testing.T) {
	cur := &countingCurrency{gate: make(chan struct{}), started: make(chan struct{}, 1)}
	fe, _ := newCachedFrontend(cur)
	ctx := context.Background()
	const n = 25
	shared := currencyCacheTotal.WithLabelValues("convert", "shared")
	before := testutil.ToFloat64(shared)

	results := make([]*pb.Money, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = fe.convertCurrency(ctx, usd(42, 500000000), "JPY")
		}(i)
	}
	<-cur.started // the leader is inside the service call
	// the others have joined it (counted by the cache), without a second call: the gate is still shut
	waitFor(t, "the other callers to join the flight", func() bool { return testutil.ToFloat64(shared)-before >= n-1 })
	if c := cur.convertCalls.Load(); c != 1 {
		t.Fatalf("%d service calls while the first was in flight", c)
	}
	close(cur.gate)
	wg.Wait()
	if c := cur.convertCalls.Load(); c != 1 {
		t.Errorf("%d service calls for %d identical concurrent conversions, want 1", c, n)
	}
	for i := range results {
		if errs[i] != nil || results[i].GetUnits() != results[0].GetUnits() || results[i].GetNanos() != results[0].GetNanos() {
			t.Fatalf("caller %d: %v %v", i, results[i], errs[i])
		}
	}
	// the same for the supported currencies
	var swg sync.WaitGroup
	for i := 0; i < n; i++ {
		swg.Add(1)
		go func() { defer swg.Done(); _, _ = fe.getCurrencies(ctx) }()
	}
	swg.Wait()
	if c := cur.supportedCalls.Load(); c != 1 {
		t.Errorf("supported currencies: %d calls for %d callers", c, n)
	}
}

func TestCurrencyCache_ConcurrentErrorGoesToAllAndIsNotKept(t *testing.T) {
	cur := &countingCurrency{gate: make(chan struct{}), started: make(chan struct{}, 1)}
	boom := status.Error(codes.Unavailable, "down")
	cur.err.Store(&boom)
	fe, _ := newCachedFrontend(cur)
	shared := currencyCacheTotal.WithLabelValues("convert", "shared")
	before := testutil.ToFloat64(shared)
	var wg sync.WaitGroup
	errs := make([]error, 5)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = fe.convertCurrency(context.Background(), usd(1, 0), "EUR")
		}(i)
	}
	<-cur.started
	waitFor(t, "joiners", func() bool { return testutil.ToFloat64(shared)-before >= 4 })
	close(cur.gate)
	wg.Wait()
	for i, e := range errs {
		if status.Code(e) != codes.Unavailable {
			t.Errorf("caller %d: %v", i, e)
		}
	}
	cur.err.Store(nil)
	if _, err := fe.convertCurrency(context.Background(), usd(1, 0), "EUR"); err != nil {
		t.Errorf("a failed flight was remembered: %v", err)
	}
}

func TestCurrencyCache_CancelledCallerDoesNotFailTheOthers(t *testing.T) {
	cur := &countingCurrency{gate: make(chan struct{}), started: make(chan struct{}, 1)}
	fe, _ := newCachedFrontend(cur)
	shared := currencyCacheTotal.WithLabelValues("convert", "shared")
	before := testutil.ToFloat64(shared)

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderErr := make(chan error, 1)
	go func() {
		_, err := fe.convertCurrency(leaderCtx, usd(8, 0), "EUR")
		leaderErr <- err
	}()
	<-cur.started
	type res struct {
		m   *pb.Money
		err error
	}
	other := make(chan res, 1)
	go func() {
		m, err := fe.convertCurrency(context.Background(), usd(8, 0), "EUR")
		other <- res{m, err}
	}()
	waitFor(t, "the second caller to join", func() bool { return testutil.ToFloat64(shared)-before >= 1 })
	cancelLeader()
	if err := <-leaderErr; err == nil {
		t.Error("the cancelled caller must see its own cancellation")
	}
	close(cur.gate) // the call itself was not cancelled: the other caller gets the answer
	r := <-other
	if r.err != nil || r.m.GetCurrencyCode() != "EUR" {
		t.Errorf("the other caller: %v %v", r.m, r.err)
	}
	if c := cur.convertCalls.Load(); c != 1 {
		t.Errorf("%d service calls", c)
	}
}

func TestCurrencyCache_IsBounded(t *testing.T) {
	cur := &countingCurrency{}
	fe, clk := newCachedFrontend(cur)
	ctx := context.Background()
	for i := 0; i < conversionMaxEntries+50; i++ {
		if _, err := fe.convertCurrency(ctx, usd(int64(i), 0), "EUR"); err != nil {
			t.Fatal(err)
		}
		fe.currency.mu.Lock()
		n := len(fe.currency.entries)
		fe.currency.mu.Unlock()
		if n > conversionMaxEntries {
			t.Fatalf("%d entries, bound %d", n, conversionMaxEntries)
		}
	}
	// stale entries go first: after the TTL a full cache is cleaned, not wiped blindly
	clk.Advance(conversionTTL + time.Second)
	if _, err := fe.convertCurrency(ctx, usd(-1, 0), "EUR"); err != nil {
		t.Fatal(err)
	}
	fe.currency.mu.Lock()
	n := len(fe.currency.entries)
	fe.currency.mu.Unlock()
	if n > conversionMaxEntries {
		t.Errorf("%d entries", n)
	}
}

// ---------------------------------------------------------------------------
// Calls per page, measured through the real handlers
// ---------------------------------------------------------------------------

// pageFrontend is a frontend over the bufconn fakes with the counting currency client.
func pageFrontend(t *testing.T) (*frontendServer, *countingCurrency, *testClock) {
	t.Helper()
	fe := newFakeFrontend(t, sampleCatalog())
	cur := &countingCurrency{}
	clk := newTestClock()
	fe.currencyClient = cur
	fe.currency.now = clk.Now
	return fe, cur, clk
}

// lookups counts what the handlers ASKED for (every call to convertCurrency / getCurrencies, whatever
// answered it). Before the cache each lookup was one RPC, so this is the "before" number.
func lookups(op string) int64 {
	var n float64
	for _, r := range []string{"hit", "miss", "shared"} {
		n += testutil.ToFloat64(currencyCacheTotal.WithLabelValues(op, r))
	}
	return int64(n)
}

func TestCurrencyCache_CallsPerListingPage(t *testing.T) {
	fe, cur, _ := pageFrontend(t)
	type row struct{ lookS, lookC, rpcS, rpcC int64 }
	var rows []row
	var l0s, l0c, r0s, r0c = lookups("supported"), lookups("convert"), int64(0), int64(0)
	for _, target := range []string{"/", "/", "/?page=1&sort=name"} {
		if rr := serve(fe, target, false); rr.Code != 200 {
			t.Fatalf("%s: %d", target, rr.Code)
		}
		ls, lc, rs, rc := lookups("supported"), lookups("convert"), cur.supportedCalls.Load(), cur.convertCalls.Load()
		rows = append(rows, row{ls - l0s, lc - l0c, rs - r0s, rc - r0c})
		l0s, l0c, r0s, r0c = ls, lc, rs, rc
	}
	for i, r := range rows {
		t.Logf("listing page request %d: before (lookups = RPCs without a cache) supported %d + convert %d = %d; after (RPCs) supported %d + convert %d = %d",
			i+1, r.lookS, r.lookC, r.lookS+r.lookC, r.rpcS, r.rpcC, r.rpcS+r.rpcC)
	}
	for i, r := range rows {
		if r.lookS != 1 || r.lookC != catalogPageSize {
			t.Errorf("request %d: %d + %d lookups, want 1 + %d", i+1, r.lookS, r.lookC, catalogPageSize)
		}
	}
	// the first page: Cream (9 USD) and Pan 09 (9 USD) share a price, so 23 distinct conversions
	if rows[0].rpcS != 1 || rows[0].rpcC != catalogPageSize-1 {
		t.Errorf("cold page: %d + %d RPCs, want 1 + %d", rows[0].rpcS, rows[0].rpcC, catalogPageSize-1)
	}
	for i := 1; i < 3; i++ {
		if rows[i].rpcS != 0 || rows[i].rpcC != 0 {
			t.Errorf("warm request %d: %d + %d RPCs, want 0", i+1, rows[i].rpcS, rows[i].rpcC)
		}
	}
}

func TestCurrencyCache_CallsPerPage_ProductAndFragment(t *testing.T) {
	fe, cur, clk := pageFrontend(t)
	if rr := serve(fe, "/product/pan-05", false); rr.Code != 200 {
		t.Fatalf("product: %d", rr.Code)
	}
	if s, c := cur.supportedCalls.Load(), cur.convertCalls.Load(); s != 1 || c < 1 {
		t.Errorf("first product page: %d supported, %d convert", s, c)
	}
	s0, c0 := cur.supportedCalls.Load(), cur.convertCalls.Load()
	serve(fe, "/product/pan-05", false)
	if cur.supportedCalls.Load() != s0 || cur.convertCalls.Load() != c0 {
		t.Errorf("second view of the same product made calls: %d/%d -> %d/%d", s0, c0, cur.supportedCalls.Load(), cur.convertCalls.Load())
	}
	// an htmx paging request needs prices but not the currency list
	serve(fe, "/", false)
	s1, c1 := cur.supportedCalls.Load(), cur.convertCalls.Load()
	serve(fe, "/?page=2", true)
	if cur.supportedCalls.Load() != s1 {
		t.Error("an htmx fragment fetched the supported currencies")
	}
	// page 2 (sorted by name) shows Pan 24..30 and Soap: Soap costs 4 USD like Pan 04 on page 1,
	// so only the 7 new prices are asked for
	if got := cur.convertCalls.Load() - c1; got != 7 {
		t.Errorf("page 2 made %d convert calls, want 7", got)
	}
	// after the TTL the prices are fetched again
	clk.Advance(conversionTTL + time.Second)
	c2 := cur.convertCalls.Load()
	serve(fe, "/", false)
	if got := cur.convertCalls.Load() - c2; got != catalogPageSize-1 {
		t.Errorf("after the TTL: %d convert calls, want %d", got, catalogPageSize-1)
	}
}
