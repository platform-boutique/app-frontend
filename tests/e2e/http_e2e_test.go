//go:build e2e

package e2e

import (
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

var (
	defaultClient = &http.Client{Timeout: 10 * time.Second}

	// noRedirectClient does not follow redirects so we can inspect the redirect
	// response itself without triggering downstream service calls.
	noRedirectClient = &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
)

// baseURL returns the frontend base address from env or defaults to localhost.
func baseURL() string {
	if h := os.Getenv("FRONTEND_ADDR"); h != "" {
		return h
	}
	return "http://localhost:8080"
}

// skipIfDownstream skips tests that require live downstream gRPC services.
func skipIfDownstream(t *testing.T) {
	t.Helper()
	if os.Getenv("SKIP_DOWNSTREAM_TESTS") == "true" {
		t.Skip("downstream gRPC services not available in this environment")
	}
}

// ── Health & infrastructure ───────────────────────────────────────────────────

func TestHealthz_ReturnsOK(t *testing.T) {
	resp, err := defaultClient.Get(baseURL() + "/_healthz")
	if err != nil {
		t.Fatalf("GET /_healthz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("got status %d, want 200", resp.StatusCode)
	}
}

func TestRobotsTxt_ReturnsOK(t *testing.T) {
	resp, err := defaultClient.Get(baseURL() + "/robots.txt")
	if err != nil {
		t.Fatalf("GET /robots.txt: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("got status %d, want 200", resp.StatusCode)
	}
}

func TestRobotsTxt_DisallowsAll(t *testing.T) {
	resp, err := defaultClient.Get(baseURL() + "/robots.txt")
	if err != nil {
		t.Fatalf("GET /robots.txt: %v", err)
	}
	defer resp.Body.Close()

	buf := make([]byte, 256)
	n, _ := resp.Body.Read(buf)
	body := string(buf[:n])

	if !strings.Contains(body, "User-agent: *") {
		t.Errorf("robots.txt missing 'User-agent: *', got: %q", body)
	}
	if !strings.Contains(body, "Disallow: /") {
		t.Errorf("robots.txt missing 'Disallow: /', got: %q", body)
	}
}

// ── Static assets ─────────────────────────────────────────────────────────────

func TestStaticCSS_ReturnsOK(t *testing.T) {
	resp, err := defaultClient.Get(baseURL() + "/static/styles/styles.css")
	if err != nil {
		t.Fatalf("GET /static/styles/styles.css: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("got status %d, want 200", resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/css") {
		t.Errorf("got Content-Type %q, want text/css", ct)
	}
}

func TestStaticCartCSS_ReturnsOK(t *testing.T) {
	resp, err := defaultClient.Get(baseURL() + "/static/styles/cart.css")
	if err != nil {
		t.Fatalf("GET /static/styles/cart.css: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("got status %d, want 200", resp.StatusCode)
	}
}

func TestStaticMissing_Returns404(t *testing.T) {
	resp, err := defaultClient.Get(baseURL() + "/static/does-not-exist.css")
	if err != nil {
		t.Fatalf("GET /static/does-not-exist.css: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("got status %d, want 404", resp.StatusCode)
	}
}

// ── Currency ──────────────────────────────────────────────────────────────────

func TestSetCurrency_ValidCode_Redirects(t *testing.T) {
	resp, err := noRedirectClient.PostForm(baseURL()+"/setCurrency", url.Values{
		"currency_code": {"EUR"},
	})
	if err != nil {
		t.Fatalf("POST /setCurrency: %v", err)
	}
	defer resp.Body.Close()

	// Valid currency should trigger a cookie set + redirect (302/303)
	if resp.StatusCode != http.StatusFound && resp.StatusCode != http.StatusSeeOther {
		t.Errorf("got status %d, want 302 or 303", resp.StatusCode)
	}
}

func TestSetCurrency_InvalidCode_ReturnsError(t *testing.T) {
	resp, err := defaultClient.PostForm(baseURL()+"/setCurrency", url.Values{
		"currency_code": {"NOTACURRENCY"},
	})
	if err != nil {
		t.Fatalf("POST /setCurrency: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		t.Error("expected non-200 for invalid currency code, got 200")
	}
}

func TestSetCurrency_EmptyCode_ReturnsError(t *testing.T) {
	resp, err := defaultClient.PostForm(baseURL()+"/setCurrency", url.Values{
		"currency_code": {""},
	})
	if err != nil {
		t.Fatalf("POST /setCurrency: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		t.Error("expected non-200 for empty currency code, got 200")
	}
}

// ── Routes requiring downstream gRPC services ────────────────────────────────

func TestHome_ReturnsOK(t *testing.T) {
	skipIfDownstream(t)

	resp, err := defaultClient.Get(baseURL() + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("got status %d, want 200", resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("got Content-Type %q, want text/html", ct)
	}
}

// seededProductID is a product of the catalog seed (apps/product-catalog-service/data/seed/products.json).
// The ids of the old Online Boutique catalog do not exist any more.
const seededProductID = "blue-frock-162"

func TestProduct_UnknownID_ReturnsNotFound(t *testing.T) {
	skipIfDownstream(t)

	resp, err := defaultClient.Get(baseURL() + "/product/no-such-product")
	if err != nil {
		t.Fatalf("GET /product/no-such-product: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("got status %d, want 404", resp.StatusCode)
	}
}

func TestProduct_ExistingID_ReturnsOK(t *testing.T) {
	skipIfDownstream(t)

	resp, err := defaultClient.Get(baseURL() + "/product/" + seededProductID)
	if err != nil {
		t.Fatalf("GET /product/%s: %v", seededProductID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("got status %d, want 200", resp.StatusCode)
	}
}

func TestCart_ReturnsOK(t *testing.T) {
	skipIfDownstream(t)

	resp, err := defaultClient.Get(baseURL() + "/cart")
	if err != nil {
		t.Fatalf("GET /cart: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("got status %d, want 200", resp.StatusCode)
	}
}

func TestAddToCart_InvalidQuantity_ReturnsBadRequest(t *testing.T) {
	skipIfDownstream(t)

	resp, err := defaultClient.PostForm(baseURL()+"/cart", url.Values{
		"product_id": {seededProductID},
		"quantity":   {"0"}, // invalid: must be 1-10
	})
	if err != nil {
		t.Fatalf("POST /cart: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		t.Error("expected non-200 for invalid quantity, got 200")
	}
}
