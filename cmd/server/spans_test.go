package main

import (
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// serverSpans runs the requests through the real handler stack (newHandler, what main serves) with an
// in-memory tracer provider and returns the finished SERVER spans.
func serverSpans(t *testing.T, requests ...*http.Request) []sdktrace.ReadOnlySpan {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	// A no-op provider afterwards, not the value read before: the first SetTracerProvider of a process
	// makes the default global provider delegate to it for good.
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })

	l := logrus.New()
	l.SetOutput(&strings.Builder{})
	h := newHandler(newFakeFrontend(t, sampleCatalog()), l)
	for _, r := range requests {
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	var out []sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		if s.SpanKind() == trace.SpanKindServer {
			out = append(out, s)
		}
	}
	return out
}

func spanAttr(s sdktrace.ReadOnlySpan, key string) string {
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			return kv.Value.String()
		}
	}
	return ""
}

// OBS-10: the server span is named by the route template, never by the raw path.
func TestServerSpanNameIsTheRouteTemplate(t *testing.T) {
	spans := serverSpans(t,
		httptest.NewRequest(http.MethodGet, "/product/pan-02", nil),
		httptest.NewRequest(http.MethodGet, "/product/pan-07?utm=x", nil),
		httptest.NewRequest(http.MethodGet, "/", nil),
		httptest.NewRequest(http.MethodPost, "/cart", strings.NewReader("")),
		httptest.NewRequest(http.MethodGet, "/healthz", nil),
		httptest.NewRequest(http.MethodGet, "/orders/0b6f1c2e-aaaa-bbbb-cccc-1234567890ab", nil),
	)
	var names []string
	for _, s := range spans {
		names = append(names, s.Name())
		if strings.Contains(s.Name(), "pan-0") || strings.Contains(s.Name(), "0b6f1c2e") || strings.Contains(s.Name(), "utm") {
			t.Errorf("span name %q carries part of the raw path", s.Name())
		}
	}
	sort.Strings(names)
	want := []string{"GET /", "GET /healthz", "GET /orders/{id}", "GET /product/{id}", "GET /product/{id}", "POST /cart"}
	if strings.Join(names, "|") != strings.Join(want, "|") {
		t.Fatalf("span names %q, want %q", names, want)
	}
	for _, s := range spans {
		if s.Name() == "GET /product/{id}" && spanAttr(s, "http.route") != "/product/{id}" {
			t.Errorf("http.route = %q", spanAttr(s, "http.route"))
		}
	}
}

// A request no route matches (a scanner walking paths) must not create a span name per path.
func TestUnmatchedPathsShareOneSpanName(t *testing.T) {
	var reqs []*http.Request
	for _, p := range []string{"/wp-login.php", "/.env", "/admin/x1", "/admin/x2", "/a/b/c/d"} {
		reqs = append(reqs, httptest.NewRequest(http.MethodGet, p, nil))
	}
	seen := map[string]bool{}
	for _, s := range serverSpans(t, reqs...) {
		seen[s.Name()] = true
	}
	if len(seen) != 1 || !seen["GET"] {
		t.Fatalf("span names for unmatched paths: %v, want only GET", seen)
	}
}

// The request id of the log lines is on the server span as well (trace -> logs).
func TestServerSpanCarriesTheRequestID(t *testing.T) {
	spans := serverSpans(t,
		httptest.NewRequest(http.MethodGet, "/healthz", nil),
		httptest.NewRequest(http.MethodGet, "/healthz", nil),
	)
	if len(spans) != 2 {
		t.Fatalf("%d spans", len(spans))
	}
	a, b := spanAttr(spans[0], "request_id"), spanAttr(spans[1], "request_id")
	if len(a) != 36 || len(b) != 36 || a == b {
		t.Errorf("request_id attributes %q and %q, want two different UUIDs", a, b)
	}
}
