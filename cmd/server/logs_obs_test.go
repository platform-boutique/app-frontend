package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/trace"

	"frontend/internal/obs"
)

const (
	testTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	testSpanID  = "00f067aa0ba902b7"
)

func jsonLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, ln := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if ln == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatalf("log line is not JSON: %v: %s", err, ln)
		}
		out = append(out, m)
	}
	return out
}

func spanContext(t *testing.T) context.Context {
	t.Helper()
	tid, _ := trace.TraceIDFromHex(testTraceID)
	sid, _ := trace.SpanIDFromHex(testSpanID)
	return trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled}))
}

// A handler logs through the logger it finds in the request context (the way every handler does). Inside a
// span the line carries trace_id, span_id and request_id (the same id the error page footer shows); outside
// one the ids are absent. The raw session id never appears, only its fingerprint.
func TestRequestLogsCarryTraceIDs(t *testing.T) {
	var buf bytes.Buffer
	l := logrus.New()
	l.SetFormatter(obs.NewFormatter("frontend", "test"))
	l.SetOutput(&buf)

	var seenReqID string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenReqID, _ = r.Context().Value(ctxKeyRequestID{}).(string)
		log := r.Context().Value(ctxKeyLog{}).(logrus.FieldLogger)
		log.WithField("error", errors.New("backend down")).Warn("something failed")
	})
	h := ensureSessionID(&logHandler{log: l, next: next})

	req := httptest.NewRequest(http.MethodGet, "/cart", nil).WithContext(spanContext(t))
	req.AddCookie(&http.Cookie{Name: cookieSessionID, Value: "secret-session-value"})
	h.ServeHTTP(httptest.NewRecorder(), req)

	lines := jsonLines(t, &buf)
	if len(lines) != 2 { // the handler's line and "request complete" (the Debug "request started" is below Info)
		t.Fatalf("got %d lines: %s", len(lines), buf.String())
	}
	for _, m := range lines {
		if m["trace_id"] != testTraceID || m["span_id"] != testSpanID || m["request_id"] != seenReqID || seenReqID == "" {
			t.Errorf("line without the request's ids: %v", m)
		}
		if m["service"] != "frontend" || m["version"] != "test" {
			t.Errorf("service/version missing: %v", m)
		}
		if m["session"] == "secret-session-value" || m["session"] == nil {
			t.Errorf("session must be a fingerprint, got %v", m["session"])
		}
	}
	if lines[0]["error"] != "backend down" || lines[0]["severity"] != "warning" {
		t.Errorf("error field/severity: %v", lines[0])
	}
	if lines[1]["message"] != "request complete" || lines[1]["http.resp.status"] != float64(200) {
		t.Errorf("access line: %v", lines[1])
	}
	if strings.Contains(buf.String(), "secret-session-value") {
		t.Error("the raw session id was logged")
	}

	buf.Reset()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/cart", nil)) // no span
	for _, m := range jsonLines(t, &buf) {
		if _, ok := m["trace_id"]; ok {
			t.Errorf("trace_id outside a span: %v", m)
		}
		if _, ok := m["span_id"]; ok {
			t.Errorf("span_id outside a span: %v", m)
		}
		if m["request_id"] == nil {
			t.Errorf("request_id is there without a span: %v", m)
		}
	}
}

// Assets and probes would flood the log: their access line stays at Debug.
func TestAccessLineOfStaticPathsIsDebug(t *testing.T) {
	var buf bytes.Buffer
	l := logrus.New()
	l.SetFormatter(obs.NewFormatter("frontend", "test"))
	l.SetOutput(&buf)
	h := &logHandler{log: l, next: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/static/styles/styles.css", nil))
	if buf.Len() != 0 {
		t.Errorf("static request logged at info: %s", buf.String())
	}
}

func TestScrubLogText(t *testing.T) {
	cases := map[string]string{
		"rpc error: user ada@example.com not found":      "rpc error: user [email] not found",
		"card 4242 4242 4242 4242 declined":              "card [number] declined",
		"card 4242-4242-4242-4242 declined":              "card [number] declined",
		"order 1234 failed with code 14":                 "order 1234 failed with code 14",
		"deadline exceeded after 5s calling cart:7070":   "deadline exceeded after 5s calling cart:7070",
		"no personal data here":                          "no personal data here",
		"two: a.b@c.io and x+y@sub.example.org together": "two: [email] and [email] together",
	}
	for in, want := range cases {
		if got := scrubLogText(in); got != want {
			t.Errorf("scrubLogText(%q) = %q, want %q", in, got, want)
		}
	}
}

// The error path logs the (scrubbed) error, never the raw e-mail the backend echoed.
func TestRenderErrorLogScrubsPII(t *testing.T) {
	var buf bytes.Buffer
	l := logrus.New()
	l.SetFormatter(obs.NewFormatter("frontend", "test"))
	l.SetOutput(&buf)
	fe := &frontendServer{}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	fe.renderHTTPError(l.WithContext(spanContext(t)), r, w, errors.New("user ada@example.com is locked"), http.StatusBadRequest)
	if strings.Contains(buf.String(), "ada@example.com") {
		t.Fatalf("e-mail address in the log: %s", buf.String())
	}
	lines := jsonLines(t, &buf)
	if len(lines) == 0 || lines[0]["trace_id"] != testTraceID || lines[0]["error"] != "user [email] is locked" {
		t.Fatalf("log: %v", lines)
	}
}
