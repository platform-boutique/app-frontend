package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "frontend/genproto"
)

// A cart from the old catalog: one line still exists, one product is gone.
func staleCart() *fakeCart {
	return &fakeCart{items: []*pb.CartItem{
		{ProductId: "pan-05", Quantity: 2},
		{ProductId: "OLJCESPC7Z", Quantity: 3},
	}}
}

func TestCart_StaleLinesAreSkippedNot500(t *testing.T) {
	fe := newFakeFrontendWith(t, sampleCatalog(), staleCart(), &fakeCheckout{})
	rr := do(fe, http.MethodGet, "/cart", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d, body: %s", rr.Code, rr.Body.String())
	}
	html := rr.Body.String()
	for _, want := range []string{"Pan 05", "Cart (2)", "no longer sold", `action="/cart/empty"`} {
		if !strings.Contains(html, want) {
			t.Errorf("cart page lacks %q", want)
		}
	}
	if strings.Contains(html, "OLJCESPC7Z") {
		t.Error("stale product id shown")
	}
}

func TestCart_OnlyStaleLinesStillOffersEmptyCart(t *testing.T) {
	cart := &fakeCart{items: []*pb.CartItem{{ProductId: "OLJCESPC7Z", Quantity: 1}}}
	fe := newFakeFrontendWith(t, sampleCatalog(), cart, &fakeCheckout{})
	rr := do(fe, http.MethodGet, "/cart", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	html := rr.Body.String()
	if !strings.Contains(html, "Your shopping cart is empty") || !strings.Contains(html, `action="/cart/empty"`) {
		t.Error("expected the empty state with the Empty cart button")
	}
	if rr := do(fe, http.MethodPost, "/cart/empty", "", nil); rr.Code != http.StatusFound || cart.emptied != 1 {
		t.Errorf("empty cart: status %d, emptied %d", rr.Code, cart.emptied)
	}
}

func TestHeaderCartCount_IgnoresStaleLines(t *testing.T) {
	fe := newFakeFrontendWith(t, sampleCatalog(), staleCart(), &fakeCheckout{})
	for _, target := range []string{"/", "/product/pan-05"} {
		html := do(fe, http.MethodGet, target, "", nil).Body.String()
		if !strings.Contains(html, `aria-label="Cart, 2 items"`) {
			t.Errorf("%s: header count should be 2 (stale lines excluded)", target)
		}
	}
}

func TestNav_DepartmentsOnCartAndOrderPages(t *testing.T) {
	fe := newFakeFrontendWith(t, sampleCatalog(), &fakeCart{}, &fakeCheckout{})
	for name, rr := range map[string]*httptest.ResponseRecorder{
		"cart":  do(fe, http.MethodGet, "/cart", "", nil),
		"order": do(fe, http.MethodPost, "/cart/checkout", goodOrder, nil),
	} {
		if rr.Code != http.StatusOK {
			t.Errorf("%s: status %d", name, rr.Code)
		}
		assertContains(t, rr.Body.String(), `href="/?category=kitchen"`, `href="/?category=beauty"`)
	}
}

func TestEmptyState_DepartmentUnknownOnlyWhenCategoriesAreKnown(t *testing.T) {
	cat := sampleCatalog()
	cat.listErr = status.Error(codes.Unavailable, "down")
	html := serve(newFakeFrontend(t, cat), "/?category=beauty&q=zzzz", false).Body.String()
	if strings.Contains(html, "This department does not exist") {
		t.Error("claims the department does not exist although the department list is unavailable")
	}
	assertContains(t, html, "No product matches your search")
}

func options(html string) []string {
	start := strings.Index(html, `id="quantity"`)
	end := start + strings.Index(html[start:], "</select>")
	var out []string
	for _, p := range strings.Split(html[start:end], "<option>")[1:] {
		out = append(out, strings.TrimSpace(p[:strings.Index(p, "</option>")]))
	}
	return out
}

func TestProduct_QuantityOptionsStopAtStock(t *testing.T) {
	fe := newFakeFrontend(t, sampleCatalog())
	for id, want := range map[string]string{
		"pan-05":  "1 2 3 4 5 6 7 8 9 10", // stock 20, capped at 10
		"cream-1": "1 2 3 4",              // stock 4
		"soap-1":  "1",                    // out of stock: button disabled anyway
	} {
		if got := strings.Join(options(do(fe, http.MethodGet, "/product/"+id, "", nil).Body.String()), " "); got != want {
			t.Errorf("%s: quantity options %q, want %q", id, got, want)
		}
	}
}

func TestAddToCart_QuantityAboveStockIsRejected(t *testing.T) {
	cart := &fakeCart{}
	fe := newFakeFrontendWith(t, sampleCatalog(), cart, &fakeCheckout{})
	rr := do(fe, http.MethodPost, "/cart", form("product_id", "cream-1", "quantity", "5"), nil) // stock 4
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422", rr.Code)
	}
	assertContains(t, rr.Body.String(), "Only 4 of Cream in stock.")
	if len(cart.added) != 0 {
		t.Error("rejected quantity was added to the cart")
	}
	if rr := do(fe, http.MethodPost, "/cart", form("product_id", "cream-1", "quantity", "4"), nil); rr.Code != http.StatusFound || len(cart.added) != 1 {
		t.Errorf("quantity equal to stock: status %d, added %d", rr.Code, len(cart.added))
	}
}

func TestAddToCart_CountsWhatIsAlreadyInTheCart(t *testing.T) {
	cart := &fakeCart{items: []*pb.CartItem{{ProductId: "cream-1", Quantity: 3}}}
	fe := newFakeFrontendWith(t, sampleCatalog(), cart, &fakeCheckout{})
	rr := do(fe, http.MethodPost, "/cart", form("product_id", "cream-1", "quantity", "2"), nil) // 3 + 2 > 4
	if rr.Code != http.StatusUnprocessableEntity || len(cart.added) != 0 {
		t.Fatalf("status %d, added %d", rr.Code, len(cart.added))
	}
	assertContains(t, rr.Body.String(), "3 already in your cart")
}

func TestAddToCart_OutOfStockIsRejected(t *testing.T) {
	cart := &fakeCart{}
	fe := newFakeFrontendWith(t, sampleCatalog(), cart, &fakeCheckout{})
	rr := do(fe, http.MethodPost, "/cart", form("product_id", "soap-1", "quantity", "1"), nil)
	if rr.Code != http.StatusUnprocessableEntity || len(cart.added) != 0 {
		t.Errorf("status %d, added %d", rr.Code, len(cart.added))
	}
	assertContains(t, rr.Body.String(), "is out of stock.")
}
