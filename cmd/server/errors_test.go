package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	pkgerrors "github.com/pkg/errors"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestClassifyError(t *testing.T) {
	declineMsg := "failed to charge card: could not charge the card: rpc error: code = FailedPrecondition desc = card_declined: Your card (ending 0454) was declined by the issuer"
	cases := []struct {
		name     string
		err      error
		fallback int
		code     int
		message  string // exact, or "" = any generic text that must not contain leak
	}{
		{"invalid argument", status.Error(codes.InvalidArgument, "Your credit card (ending 0454) expired on 1/2020"), 500, 422, "Your credit card (ending 0454) expired on 1/2020"},
		{"failed precondition wrapped", pkgerrors.Wrap(status.Error(codes.FailedPrecondition, "cart is empty"), "failed to complete the order"), 500, 422, "Cart is empty"},
		{"wrapper in the text is stripped", status.Error(codes.InvalidArgument, "rpc error: code = InvalidArgument desc = Nope"), 500, 422, "Nope"},
		{"payment reason prefix is stripped", status.Error(codes.FailedPrecondition, "card_declined: Your card was declined"), 500, 422, "Your card was declined"},
		{"not found", pkgerrors.Wrap(status.Error(codes.NotFound, "no product 'x' in table products"), "could not retrieve product"), 500, 404, msgNotFound},
		{"unavailable", status.Error(codes.Unavailable, "dial tcp 10.0.3.7:5432: refused"), 500, 503, msgUnavailable},
		{"deadline", status.Error(codes.DeadlineExceeded, "context deadline exceeded"), 500, 503, msgUnavailable},
		{"internal", status.Error(codes.Internal, "pq: password authentication failed for user shop"), 500, 500, msgServerError},
		{"unknown error", errors.New("pq: password authentication failed"), 500, 500, msgServerError},
		{"internal decline (checkout today)", status.Error(codes.Internal, declineMsg), 500, 422, "Your card (ending 0454) was declined by the issuer"},
		{"internal that is not a decline", status.Error(codes.Internal, "failed to charge card: dial tcp 10.1.1.1: refused"), 500, 500, msgServerError},
		{"decline text in a different internal error", status.Error(codes.Internal, "failed to ship order: rpc error: code = InvalidArgument desc = secret"), 500, 500, msgServerError},
		{"own validation error", errors.New("Field 'Quantity' is invalid: lte"), 422, 422, "Field 'Quantity' is invalid: lte"},
		{"own 400", pkgerrors.New("internal detail"), 400, 400, msgBadRequest},
		{"own 404", pkgerrors.New("internal detail"), 404, 404, msgNotFound},
		{"own 5xx detail hidden", pkgerrors.New("internal detail"), 500, 500, msgServerError},
	}
	for _, c := range cases {
		got := classifyError(c.err, c.fallback)
		if got.Code != c.code || got.Message != c.message {
			t.Errorf("%s: got %d %q, want %d %q", c.name, got.Code, got.Message, c.code, c.message)
		}
	}
}

func assertNoLeak(t *testing.T, html string, secrets ...string) {
	t.Helper()
	for _, s := range append(secrets, "goroutine", ".go:", "rpc error", "Technical details", "<pre>") {
		if strings.Contains(html, s) {
			t.Errorf("error page leaks %q", s)
		}
	}
}

func TestProduct_UnknownIDIs404PageWithNav(t *testing.T) {
	fe := newFakeFrontend(t, sampleCatalog())
	rr := do(fe, http.MethodGet, "/product/nope", "", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rr.Code)
	}
	html := rr.Body.String()
	assertContains(t, html, "Not found", msgNotFound, `href="/?category=kitchen"`, "request-id: test-request-id")
	assertNoLeak(t, html, "no product")
}

func TestAddToCart_UnknownProductIs404(t *testing.T) {
	fe := newFakeFrontend(t, sampleCatalog())
	rr := do(fe, http.MethodPost, "/cart", form("product_id", "OLJCESPC7Z", "quantity", "1"), nil)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status %d, want 404", rr.Code)
	}
}

func TestBackendFailureIsGenericAndKeepsDetailsInTheLog(t *testing.T) {
	cat := sampleCatalog()
	cat.filterErr = status.Error(codes.Internal, "pq: password authentication failed for user shop; db at 10.0.3.7:5432")
	fe := newFakeFrontend(t, cat)
	rr := do(fe, http.MethodGet, "/", "", nil)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", rr.Code)
	}
	html := rr.Body.String()
	assertContains(t, html, msgServerError)
	assertNoLeak(t, html, "pq:", "10.0.3.7", "password")
}

func TestBackendUnavailableIs503(t *testing.T) {
	cat := sampleCatalog()
	cat.filterErr = status.Error(codes.Unavailable, "connection refused 10.0.3.7")
	rr := do(newFakeFrontend(t, cat), http.MethodGet, "/", "", nil)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", rr.Code)
	}
	assertContains(t, rr.Body.String(), "Temporarily unavailable", msgUnavailable)
	assertNoLeak(t, rr.Body.String(), "10.0.3.7")
}

var goodOrder = form("email", "a@example.com", "street_address", "1 Main St", "zip_code", "12345", "city", "Town", "state", "IL",
	"country", "US", "credit_card_number", "4432801561520454", "credit_card_expiration_month", "1", "credit_card_expiration_year", "2040",
	"credit_card_cvv", "672")

func TestCheckout_ErrorsAreMappedFromTheBackendStatus(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code int
		want string
	}{
		{"decline passed through", status.Error(codes.FailedPrecondition, "card_declined: Your card (ending 0454) was declined by the issuer"), 422, "Your card (ending 0454) was declined by the issuer"},
		{"invalid card", status.Error(codes.InvalidArgument, "invalid_card: Credit card info is invalid"), 422, "Credit card info is invalid"},
		{"unknown order or product", status.Error(codes.NotFound, "product x"), 404, msgNotFound},
		{"payment outage", status.Error(codes.Unavailable, "dial tcp 10.2.2.2"), 503, msgUnavailable},
		{"decline inside Internal (checkout before the change)", status.Error(codes.Internal, "failed to charge card: could not charge the card: rpc error: code = InvalidArgument desc = expired_card: Your credit card (ending 0454) expired on 1/2020"), 422, "Your credit card (ending 0454) expired on 1/2020"},
		{"other", status.Error(codes.Internal, "failed to ship: tracking db at 10.9.9.9 down"), 500, msgServerError},
	}
	for _, c := range cases {
		co := &fakeCheckout{err: c.err}
		fe := newFakeFrontendWith(t, sampleCatalog(), &fakeCart{}, co)
		rr := do(fe, http.MethodPost, "/cart/checkout", goodOrder, nil)
		if rr.Code != c.code {
			t.Errorf("%s: status %d, want %d", c.name, rr.Code, c.code)
		}
		html := rr.Body.String()
		if !strings.Contains(strings.ReplaceAll(html, "&#39;", "'"), c.want) {
			t.Errorf("%s: page lacks %q", c.name, c.want)
		}
		assertNoLeak(t, html, "10.2.2.2", "10.9.9.9", "tracking db")
	}
}

func TestHTMXRequestThatFailsGetsAnAlertFragment(t *testing.T) {
	cat := sampleCatalog()
	cat.filterErr = status.Error(codes.Unavailable, "10.0.0.1 refused")
	fe := newFakeFrontend(t, cat)
	rr := do(fe, http.MethodGet, "/?q=pan", "", map[string]string{"HX-Request": "true"})
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d", rr.Code)
	}
	h := rr.Header()
	if h.Get("HX-Retarget") != "#page-alert" || !strings.HasPrefix(h.Get("HX-Reswap"), "innerHTML") || h.Get("HX-Push-Url") != "false" {
		t.Errorf("htmx headers: %v", h)
	}
	body := rr.Body.String()
	assertContains(t, body, msgUnavailable)
	if strings.Contains(body, "<html") || strings.Contains(body, "10.0.0.1") {
		t.Errorf("fragment should be a small message without page chrome or details:\n%s", body)
	}
}
