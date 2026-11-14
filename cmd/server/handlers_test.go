package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"
)

func TestMain(m *testing.M) {
	if err := os.Setenv("TEMPLATE_PATH", "../../templates/*.html"); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

// ---------------------------------------------------------------------------
// test helpers
// ---------------------------------------------------------------------------

// newTestLogger returns a logrus logger that discards output during tests.
func newTestLogger() logrus.FieldLogger {
	log := logrus.New()
	log.SetOutput(httptest.NewRecorder())
	return log
}

// requestWithContext builds an *http.Request pre-loaded with the context values
// that handlers expect (session ID, request ID, logger).
func requestWithContext(method, path string, body string) *http.Request {
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}

	ctx := req.Context()
	ctx = context.WithValue(ctx, ctxKeySessionID{}, "test-session-id")
	ctx = context.WithValue(ctx, ctxKeyRequestID{}, "test-request-id")
	ctx = context.WithValue(ctx, ctxKeyLog{}, newTestLogger())
	return req.WithContext(ctx)
}

// newFrontendServer returns a zero-value frontendServer suitable for tests
// that don't make gRPC calls.
func newFrontendServer() *frontendServer {
	return &frontendServer{}
}

// ---------------------------------------------------------------------------
// currentCurrency
// ---------------------------------------------------------------------------

func TestCurrentCurrency_NoCookie(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	got := currentCurrency(req)
	if got != defaultCurrency {
		t.Errorf("expected default currency %q, got %q", defaultCurrency, got)
	}
}

func TestCurrentCurrency_WithCookie(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: cookieCurrency, Value: "EUR"})
	got := currentCurrency(req)
	if got != "EUR" {
		t.Errorf("expected EUR, got %q", got)
	}
}

func TestCurrentCurrency_EmptyCookieValue(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: cookieCurrency, Value: ""})
	got := currentCurrency(req)
	// empty cookie value — still returns the cookie value (empty string)
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// sessionID
// ---------------------------------------------------------------------------

func TestSessionID_FromContext(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := context.WithValue(req.Context(), ctxKeySessionID{}, "my-session")
	req = req.WithContext(ctx)
	got := sessionID(req)
	if got != "my-session" {
		t.Errorf("expected my-session, got %q", got)
	}
}

func TestSessionID_NotInContext(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	got := sessionID(req)
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// injectCommonTemplateData
// ---------------------------------------------------------------------------

func TestInjectCommonTemplateData_ContainsRequiredKeys(t *testing.T) {
	req := requestWithContext(http.MethodGet, "/", "")
	payload := map[string]interface{}{
		"custom_key": "custom_value",
	}
	data := injectCommonTemplateData(req, payload)

	requiredKeys := []string{
		"session_id",
		"request_id",
		"user_currency",
		"brand_name",
		"brand_tagline",
		"frontendMessage",
		"currentYear",
		"baseUrl",
	}
	for _, key := range requiredKeys {
		if _, ok := data[key]; !ok {
			t.Errorf("missing required key %q in template data", key)
		}
	}
}

func TestInjectCommonTemplateData_PayloadMerged(t *testing.T) {
	req := requestWithContext(http.MethodGet, "/", "")
	payload := map[string]interface{}{
		"products": []string{"a", "b"},
		"count":    42,
	}
	data := injectCommonTemplateData(req, payload)

	if data["products"] == nil {
		t.Error("expected products key in merged data")
	}
	if data["count"] != 42 {
		t.Errorf("expected count=42, got %v", data["count"])
	}
}

func TestInjectCommonTemplateData_SessionID(t *testing.T) {
	req := requestWithContext(http.MethodGet, "/", "")
	data := injectCommonTemplateData(req, map[string]interface{}{})
	if data["session_id"] != "test-session-id" {
		t.Errorf("expected test-session-id, got %v", data["session_id"])
	}
}

func TestInjectCommonTemplateData_CurrentYear(t *testing.T) {
	req := requestWithContext(http.MethodGet, "/", "")
	data := injectCommonTemplateData(req, map[string]interface{}{})
	year, ok := data["currentYear"].(int)
	if !ok {
		t.Fatal("expected currentYear to be an int")
	}
	if year != time.Now().Year() {
		t.Errorf("expected year %d, got %d", time.Now().Year(), year)
	}
}

func TestInjectCommonTemplateData_UserCurrency(t *testing.T) {
	req := requestWithContext(http.MethodGet, "/", "")
	req.AddCookie(&http.Cookie{Name: cookieCurrency, Value: "JPY"})
	data := injectCommonTemplateData(req, map[string]interface{}{})
	if data["user_currency"] != "JPY" {
		t.Errorf("expected JPY, got %v", data["user_currency"])
	}
}

// ---------------------------------------------------------------------------
// setCurrencyHandler
// ---------------------------------------------------------------------------

func TestSetCurrencyHandler_ValidCurrency(t *testing.T) {
	fe := newFrontendServer()

	form := url.Values{}
	form.Set("currency_code", "EUR")
	req := requestWithContext(http.MethodPost, "/setCurrency", form.Encode())
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rr := httptest.NewRecorder()
	fe.setCurrencyHandler(rr, req)

	if rr.Code != http.StatusFound {
		t.Errorf("expected 302 redirect, got %d", rr.Code)
	}

	// cookie should be set
	cookies := rr.Result().Cookies()
	found := false
	for _, c := range cookies {
		if c.Name == cookieCurrency && c.Value == "EUR" {
			found = true
		}
	}
	if !found {
		t.Error("expected currency cookie to be set to EUR")
	}
}

func TestSetCurrencyHandler_InvalidCurrency(t *testing.T) {
	fe := newFrontendServer()

	form := url.Values{}
	form.Set("currency_code", "INVALID")
	req := requestWithContext(http.MethodPost, "/setCurrency", form.Encode())
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rr := httptest.NewRecorder()
	fe.setCurrencyHandler(rr, req)

	// invalid currency should return 422
	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("expected 422, got %d", rr.Code)
	}
}

func TestSetCurrencyHandler_RefererRedirect(t *testing.T) {
	fe := newFrontendServer()

	form := url.Values{}
	form.Set("currency_code", "GBP")
	req := requestWithContext(http.MethodPost, "/setCurrency", form.Encode())
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", "/product/abc123")

	rr := httptest.NewRecorder()
	fe.setCurrencyHandler(rr, req)

	if rr.Code != http.StatusFound {
		t.Errorf("expected 302, got %d", rr.Code)
	}
	location := rr.Header().Get("Location")
	if location != "/product/abc123" {
		t.Errorf("expected redirect to /product/abc123, got %q", location)
	}
}

func TestSetCurrencyHandler_NoRefererRedirectsToRoot(t *testing.T) {
	fe := newFrontendServer()

	form := url.Values{}
	form.Set("currency_code", "USD")
	req := requestWithContext(http.MethodPost, "/setCurrency", form.Encode())
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rr := httptest.NewRecorder()
	fe.setCurrencyHandler(rr, req)

	location := rr.Header().Get("Location")
	if location != baseUrl+"/" {
		t.Errorf("expected redirect to %q, got %q", baseUrl+"/", location)
	}
}

// ---------------------------------------------------------------------------
// logoutHandler
// ---------------------------------------------------------------------------

func TestLogoutHandler_ClearsCookies(t *testing.T) {
	fe := newFrontendServer()

	req := requestWithContext(http.MethodGet, "/logout", "")
	req.AddCookie(&http.Cookie{Name: cookieSessionID, Value: "some-session"})
	req.AddCookie(&http.Cookie{Name: cookieCurrency, Value: "EUR"})

	rr := httptest.NewRecorder()
	fe.logoutHandler(rr, req)

	if rr.Code != http.StatusFound {
		t.Errorf("expected 302 redirect, got %d", rr.Code)
	}

	// all cookies should be expired
	for _, c := range rr.Result().Cookies() {
		if c.MaxAge != -1 {
			t.Errorf("expected cookie %q to have MaxAge=-1, got %d", c.Name, c.MaxAge)
		}
	}
}

func TestLogoutHandler_RedirectsToRoot(t *testing.T) {
	fe := newFrontendServer()
	req := requestWithContext(http.MethodGet, "/logout", "")
	rr := httptest.NewRecorder()
	fe.logoutHandler(rr, req)

	location := rr.Header().Get("Location")
	if location != baseUrl+"/" {
		t.Errorf("expected redirect to %q, got %q", baseUrl+"/", location)
	}
}

// ---------------------------------------------------------------------------
// addToCartHandler — validation paths only (no gRPC)
// ---------------------------------------------------------------------------

func TestAddToCartHandler_InvalidQuantityZero(t *testing.T) {
	fe := newFrontendServer()

	form := url.Values{}
	form.Set("product_id", "abc123")
	form.Set("quantity", "0") // invalid
	req := requestWithContext(http.MethodPost, "/cart", form.Encode())
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rr := httptest.NewRecorder()
	fe.addToCartHandler(rr, req)

	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("expected 422 for zero quantity, got %d", rr.Code)
	}
}

func TestAddToCartHandler_MissingProductID(t *testing.T) {
	fe := newFrontendServer()

	form := url.Values{}
	form.Set("quantity", "1")
	// no product_id
	req := requestWithContext(http.MethodPost, "/cart", form.Encode())
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rr := httptest.NewRecorder()
	fe.addToCartHandler(rr, req)

	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("expected 422 for missing product_id, got %d", rr.Code)
	}
}

// ---------------------------------------------------------------------------
// getProductByID — empty ID returns early (no gRPC)
// ---------------------------------------------------------------------------

func TestGetProductByID_EmptyID(t *testing.T) {
	fe := newFrontendServer()

	r := mux.NewRouter()
	r.HandleFunc("/product-meta/{ids}", fe.getProductByID)

	// call with empty id segment — router won't match, so test directly
	req := requestWithContext(http.MethodGet, "/product-meta/", "")
	rr := httptest.NewRecorder()

	// call handler directly with empty vars
	fe.getProductByID(rr, req)

	// should return early with 200 (default) and empty body
	if rr.Body.Len() != 0 {
		t.Errorf("expected empty body for empty ID, got %q", rr.Body.String())
	}
}
