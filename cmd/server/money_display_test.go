package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gorilla/mux"

	pb "frontend/genproto"
	"frontend/money"
)

func m(code string, units int64, nanos int32) *pb.Money {
	return &pb.Money{CurrencyCode: code, Units: units, Nanos: nanos}
}

func TestRenderMoney_RoundsToTheNearestMinorUnit(t *testing.T) {
	cases := []struct {
		in   *pb.Money
		want string
	}{
		// the bug: currency-service floors, 29.99 comes back as 29.989999999
		{m("USD", 29, 989999999), "$29.99"},
		{m("USD", 599, 989999999), "$599.99"},
		{m("USD", 0, 999999999), "$1.00"}, // carry into the units
		{m("USD", 1998, 999999999), "$1999.00"},
		{m("USD", 9, 999999999), "$10.00"},
		{m("USD", 99, 995000000), "$100.00"}, // exactly half: up
		{m("USD", 99, 994999999), "$99.99"},  // just below half: down
		{m("USD", 0, 4999999), "$0.00"},
		{m("USD", 0, 5000000), "$0.01"},
		{m("USD", 0, 0), "$0.00"},
		{m("USD", 12, 990000000), "$12.99"},
		{m("USD", 12, 10000000), "$12.01"},
		{m("USD", 200000000000000, 0), "$200000000000000.00"}, // order totals go up to 2e14 units: no overflow
		{m("USD", 9223372036854775807, 999999999), "$9223372036854775808.00"},
		// other currencies, symbols as before
		{m("EUR", 26, 543210123), "€26.54"},
		{m("EUR", 26, 545000000), "€26.55"},
		{m("EUR", 26, 544999999), "€26.54"},
		{m("GBP", 25, 839999999), "£25.84"},
		{m("CAD", 3, 997000000), "$4.00"},
		{m("TRY", 100, 0), "₺100.00"},
		{m("CHF", 10, 0), "$10.00"}, // unknown code: $ and two decimals as before
		// JPY has no minor unit
		{m("JPY", 1000, 0), "¥1000"},
		{m("JPY", 1999, 500000000), "¥2000"},
		{m("JPY", 1999, 499999999), "¥1999"},
		{m("JPY", 3639, 999999999), "¥3640"},
		{m("JPY", 0, 400000000), "¥0"},
		// negative amounts (refunds, never shown today): same rounding of the magnitude
		{m("USD", -29, -989999999), "-$29.99"},
		{m("USD", 0, -1000), "$0.00"},
		{m("USD", 0, -5000000), "-$0.01"},
		{m("USD", -1, 0), "-$1.00"},
		{nil, "$0.00"},
	}
	for _, c := range cases {
		if got := renderMoney(c.in); got != c.want {
			t.Errorf("renderMoney(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

type seedPrice struct {
	ID    string `json:"id"`
	Units int64  `json:"units"`
	Nanos int32  `json:"nanos"`
}

func loadSeedPrices(t *testing.T) []seedPrice {
	t.Helper()
	raw, err := os.ReadFile("testdata/seed-prices.json")
	if err != nil {
		t.Fatal(err)
	}
	var out []seedPrice
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func formatSeed(units int64, nanos int32) string {
	return fmt.Sprintf("$%d.%02d", units, nanos/10000000) // seed prices are whole cents
}

// All 117 seed prices of product-catalog-service (a copy in testdata; the drift test below checks it
// against the catalog repo when it sits next to this one).
func TestSeedPrices_RenderAsTheirSeedPrice(t *testing.T) {
	seed := loadSeedPrices(t)
	if len(seed) != 117 {
		t.Fatalf("%d seed prices, want 117", len(seed))
	}
	cur := &countingCurrency{}
	fe, _ := newCachedFrontend(cur)
	for _, p := range seed {
		if p.Nanos%10000000 != 0 {
			t.Fatalf("%s: seed price is not whole cents", p.ID)
		}
		want := formatSeed(p.Units, p.Nanos)
		price := m("USD", p.Units, p.Nanos)
		// 1. through the frontend, USD shop: no RPC, exact
		got, err := fe.convertCurrency(t.Context(), price, "USD")
		if err != nil {
			t.Fatal(err)
		}
		if r := renderMoney(got); r != want {
			t.Errorf("%s: USD shop shows %s, seed price %s", p.ID, r, want)
		}
		// 2. what the service answers for USD->USD (one nano low for many prices): still the seed price
		svc, _ := convertLikeTheService(price, "USD")
		if r := renderMoney(svc); r != want {
			t.Errorf("%s: service answer %v shows %s, seed price %s", p.ID, svc, r, want)
		}
	}
	if n := cur.convertCalls.Load(); n != 0 {
		t.Errorf("%d conversion RPCs for the USD shop", n)
	}
}

func TestSeedPrices_FileMatchesTheCatalogRepo(t *testing.T) {
	raw, err := os.ReadFile("../../../product-catalog-service/data/seed/products.json")
	if err != nil {
		t.Skip("catalog repo not next to this one (workspace layout only)")
	}
	var catalog []struct {
		ID    string `json:"id"`
		Price struct {
			Units int64 `json:"units"`
			Nanos int32 `json:"nanos"`
		} `json:"price_usd"`
	}
	if err := json.Unmarshal(raw, &catalog); err != nil {
		t.Fatal(err)
	}
	mine := map[string]seedPrice{}
	for _, p := range loadSeedPrices(t) {
		mine[p.ID] = p
	}
	if len(catalog) != len(mine) {
		t.Errorf("catalog has %d products, testdata %d: refresh testdata/seed-prices.json", len(catalog), len(mine))
	}
	for _, c := range catalog {
		if p, ok := mine[c.ID]; !ok || p.Units != c.Price.Units || p.Nanos != c.Price.Nanos {
			t.Errorf("%s differs from the catalog seed: refresh testdata/seed-prices.json", c.ID)
		}
	}
}

// Round trips through the (ported) service for the other whitelisted currencies: what the shop shows
// is the nearest cent of the service's own floored answer, a within-one-cent neighbour of the exact value.
func TestRenderMoney_ConvertedPricesAreWithinHalfAMinorUnitOfTheFloor(t *testing.T) {
	for _, to := range []string{"EUR", "GBP", "CAD", "TRY", "JPY"} {
		for _, p := range loadSeedPrices(t) {
			from := m("USD", p.Units, p.Nanos)
			got, err := convertLikeTheService(from, to)
			if err != nil {
				t.Fatal(err)
			}
			d := decimalsOf(to)
			shown := renderMoney(got)
			// exact value in nanos of the service answer, and the shown one back in nanos
			var units int64
			var frac int64
			rest := shown[len(renderCurrencyLogo(to)):]
			if d == 0 {
				_, _ = fmt.Sscanf(rest, "%d", &units)
			} else {
				var f string
				_, _ = fmt.Sscanf(rest, "%d.%s", &units, &f)
				_, _ = fmt.Sscanf(f, "%d", &frac)
			}
			scale := int64(1)
			for i := 0; i < d; i++ {
				scale *= 10
			}
			step := int64(1_000_000_000) / scale
			shownNanos := units*1_000_000_000 + frac*step
			svcNanos := got.GetUnits()*1_000_000_000 + int64(got.GetNanos())
			diff := shownNanos - svcNanos
			if diff < 0 {
				diff = -diff
			}
			if diff*2 > step {
				t.Errorf("USD %s -> %s: service %v, shown %s: off by more than half a minor unit", formatSeed(p.Units, p.Nanos), to, got, shown)
			}
		}
	}
	// spot checks written out
	if got, _ := convertLikeTheService(m("USD", 100, 0), "EUR"); renderMoney(got) != "€88.46" {
		t.Errorf("100 USD in EUR shows %s", renderMoney(got))
	}
	if got, _ := convertLikeTheService(m("USD", 100, 0), "JPY"); renderMoney(got) != "¥11181" {
		t.Errorf("100 USD in JPY shows %s", renderMoney(got))
	}
}

// Sums are made on exact units/nanos and rendered once.
func TestMoneySums_RenderOnceAfterSumming(t *testing.T) {
	unit := m("EUR", 26, 543210123)
	line := money.MultiplySlow(unit, 3) // 79.629630369
	ship := m("EUR", 8, 150000000)
	total := money.Must(money.Sum(line, ship))
	if got := renderMoney(line); got != "€79.63" {
		t.Errorf("line %s", got)
	}
	if got := renderMoney(total); got != "€87.78" { // 87.779630369
		t.Errorf("total %s", got)
	}
	// USD seed prices are whole cents, so with the shortcut the lines add up exactly
	usdUnit := m("USD", 29, 990000000)
	l := money.MultiplySlow(usdUnit, 3)
	tt := money.Must(money.Sum(l, m("USD", 5, 990000000)))
	if renderMoney(l) != "$89.97" || renderMoney(tt) != "$95.96" {
		t.Errorf("USD: %s %s", renderMoney(l), renderMoney(tt))
	}
}

// The cart page in a non-USD shop: line = qty x converted unit price and total = lines + shipping are
// summed on exact amounts and rendered once, in both USD (no RPC, exact) and EUR (the service's floored answers).
func TestCartPage_LinesAndTotalAreSummedThenRendered(t *testing.T) {
	for _, cur := range []string{"USD", "EUR", "JPY"} {
		fe := newFakeFrontendWith(t, sampleCatalog(), &fakeCart{items: []*pb.CartItem{{ProductId: "pan-07", Quantity: 3}, {ProductId: "pan-13", Quantity: 2}}}, &fakeCheckout{})
		svc := &countingCurrency{}
		fe.currencyClient = svc
		conv := func(p *pb.Money) *pb.Money {
			got, err := fe.convertCurrency(t.Context(), p, cur)
			if err != nil {
				t.Fatal(err)
			}
			return got
		}
		l1 := money.MultiplySlow(conv(m("USD", 7, 0)), 3)
		l2 := money.MultiplySlow(conv(m("USD", 13, 0)), 2)
		total := money.Must(money.Sum(money.Must(money.Sum(l1, l2)), conv(m("USD", 5, 0)))) // shipping quote is 5 USD in the fake

		r := mux.NewRouter()
		r.HandleFunc("/cart", fe.viewCartHandler)
		req := requestWithContext(http.MethodGet, "/cart", "")
		req.AddCookie(&http.Cookie{Name: cookieCurrency, Value: cur})
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("%s: %d", cur, rr.Code)
		}
		html := rr.Body.String()
		for _, want := range []string{renderMoney(l1), renderMoney(l2), renderMoney(total), renderMoney(conv(m("USD", 5, 0)))} {
			assertContains(t, html, ">"+want+"<")
		}
		t.Logf("%s: lines %s + %s, shipping %s, total %s", cur, renderMoney(l1), renderMoney(l2), renderMoney(conv(m("USD", 5, 0))), renderMoney(total))
	}
}
