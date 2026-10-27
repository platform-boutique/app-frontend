package main

import (
	"net/http"
	"strings"
	"testing"

	pb "frontend/genproto"
)

// renderPage executes a page template with the common data plus payload and
// returns the HTML.
func renderPage(t *testing.T, name string, payload map[string]interface{}) string {
	t.Helper()
	var sb strings.Builder
	req := requestWithContext(http.MethodGet, "/", "")
	if err := getTemplates().ExecuteTemplate(&sb, name, injectCommonTemplateData(req, payload)); err != nil {
		t.Fatalf("execute %q: %v", name, err)
	}
	return sb.String()
}

func sampleProduct(id string) *pb.Product {
	return &pb.Product{
		Id: id, Name: "Product " + id, Description: "A thing.",
		Picture:    "/static/img/products/" + id + ".jpg",
		PriceUsd:   &pb.Money{CurrencyCode: "USD", Units: 12, Nanos: 500000000},
		Categories: []string{"kitchen"},
	}
}

func assertContains(t *testing.T, html string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(html, w) {
			t.Errorf("rendered page is missing %q", w)
		}
	}
}

func TestTemplates_Home(t *testing.T) {
	type productView struct {
		Item  *pb.Product
		Price *pb.Money
	}
	html := renderPage(t, "home", map[string]interface{}{
		"show_currency": true,
		"currencies":    []string{"USD", "EUR"},
		"products":      []productView{{sampleProduct("MUG1"), &pb.Money{CurrencyCode: "USD", Units: 12}}},
		"cart_size":     2,
		"ad":            &pb.Ad{RedirectUrl: "/product/MUG1", Text: "Mugs on sale"},
	})
	assertContains(t, html,
		"<title>"+brand.Name, `href="/product/MUG1"`, `id="currency_form"`, `name="currency_code"`,
		`action="/setCurrency"`, `id="theme-toggle"`, `class="cart-count"`, "Mugs on sale", `id="main"`)
	for _, gone := range []string{"Boutique", "Cymbal", "Hipster", "Google", "googleapis", "bootstrapcdn"} {
		if strings.Contains(html, gone) {
			t.Errorf("home page still contains %q", gone)
		}
	}
}

func TestTemplates_Product(t *testing.T) {
	html := renderPage(t, "product", map[string]interface{}{
		"show_currency": true,
		"currencies":    []string{"USD"},
		"product": struct {
			Item  *pb.Product
			Price *pb.Money
		}{sampleProduct("MUG1"), &pb.Money{CurrencyCode: "USD", Units: 12}},
		"recommendations": []*pb.Product{sampleProduct("JAR2")},
		"cart_size":       0,
	})
	assertContains(t, html, `name="product_id" value="MUG1"`, `name="quantity"`, `action="/cart"`, `href="/product/JAR2"`)
}

func TestTemplates_CartWithItems(t *testing.T) {
	type cartItemView struct {
		Item     *pb.Product
		Quantity int32
		Price    *pb.Money
	}
	html := renderPage(t, "cart", map[string]interface{}{
		"show_currency":    true,
		"currencies":       []string{"USD"},
		"cart_size":        1,
		"items":            []cartItemView{{sampleProduct("MUG1"), 1, &pb.Money{CurrencyCode: "USD", Units: 12}}},
		"shipping_cost":    &pb.Money{CurrencyCode: "USD", Units: 5},
		"total_cost":       &pb.Money{CurrencyCode: "USD", Units: 17},
		"expiration_years": []int{2026, 2027},
	})
	assertContains(t, html, "Cart (1)", "MUG1", `action="/cart/checkout"`, `action="/cart/empty"`)
	for _, field := range []string{"email", "street_address", "zip_code", "city", "state", "country",
		"credit_card_number", "credit_card_expiration_month", "credit_card_expiration_year", "credit_card_cvv"} {
		assertContains(t, html, `name="`+field+`"`)
	}
}

func TestTemplates_CartEmpty(t *testing.T) {
	html := renderPage(t, "cart", map[string]interface{}{"items": []struct{}{}, "cart_size": 0})
	assertContains(t, html, "Your shopping cart is empty")
}

func TestTemplates_Order(t *testing.T) {
	paid := &pb.Money{CurrencyCode: "USD", Units: 17}
	html := renderPage(t, "order", map[string]interface{}{
		"order": &pb.OrderResult{OrderId: "0b5e6c0e-1111-2222-3333-444455556666", ShippingTrackingId: "TR-1",
			ShippingAddress: &pb.Address{StreetAddress: "1 Main St", City: "Town", Country: "US"}},
		"total_paid": paid,
	})
	assertContains(t, html, "Your order is complete", "Confirmation #", "0b5e6c0e-1111-2222-3333-444455556666", "TR-1")
	// local/smoke.sh greps the order id within three lines after "Confirmation #".
	lines := strings.Split(html, "\n")
	for i, l := range lines {
		if strings.Contains(l, "Confirmation #") {
			window := strings.Join(lines[i:min(i+4, len(lines))], "\n")
			if !strings.Contains(window, "0b5e6c0e-1111-2222-3333-444455556666") {
				t.Errorf("order id not within 3 lines after Confirmation #:\n%s", window)
			}
		}
	}
}

func TestTemplates_Error(t *testing.T) {
	html := renderPage(t, "error", map[string]interface{}{"error": "boom", "status_code": 500, "status": "Internal Server Error"})
	assertContains(t, html, "500 Internal Server Error", "boom")
}
