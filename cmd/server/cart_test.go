package main

import (
	"net/http"
	"strings"
	"testing"

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
	fe := newFakeFrontendWith(t, sampleCatalog(), staleCart())
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
	fe := newFakeFrontendWith(t, sampleCatalog(), cart)
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
	fe := newFakeFrontendWith(t, sampleCatalog(), staleCart())
	for _, target := range []string{"/", "/product/pan-05"} {
		html := do(fe, http.MethodGet, target, "", nil).Body.String()
		if !strings.Contains(html, `aria-label="Cart, 2 items"`) {
			t.Errorf("%s: header count should be 2 (stale lines excluded)", target)
		}
	}
}
