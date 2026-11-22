package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func withCSPMode(t *testing.T, m cspMode) {
	t.Helper()
	old := cspSetting
	cspSetting = m
	t.Cleanup(func() { cspSetting = old })
}

func TestCSPModeFromEnv(t *testing.T) {
	for in, want := range map[string]cspMode{"": cspReportOnly, " Report-Only ": cspReportOnly, "enforce": cspEnforce, "OFF": cspOff} {
		got, err := cspModeFromEnv(func(string) string { return in })
		if err != nil || got != want {
			t.Errorf("CSP_MODE=%q: %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"on", "report", "enforced", "true"} {
		if _, err := cspModeFromEnv(func(string) string { return bad }); err == nil {
			t.Errorf("CSP_MODE=%q accepted: a typo must not switch the policy off", bad)
		}
	}
}

func TestCSPPolicy_ContentAndFaroCollector(t *testing.T) {
	withFaro(t, nil)
	p := cspPolicy()
	for _, want := range []string{"default-src 'self'", "script-src 'self'", "style-src 'self'", "style-src-attr 'unsafe-inline'",
		"img-src 'self' data:", "connect-src 'self';", "object-src 'none'", "base-uri 'self'", "form-action 'self'",
		"frame-ancestors 'none'", "report-uri /csp-report"} {
		if !strings.Contains(p, want) {
			t.Errorf("policy lacks %q: %s", want, p)
		}
	}
	// the only 'unsafe-' is the style ATTRIBUTE exception: scripts and style elements stay strict
	if strings.Count(p, "unsafe-") != 1 || strings.Contains(p, "unsafe-eval") || strings.Contains(p, "*") {
		t.Errorf("policy is looser than intended: %s", p)
	}

	// Faro on: the collector's ORIGIN (not its path or query) is allowed for connect-src, nothing else changes
	withFaro(t, &faroSettings{CollectorURL: "http://localhost:12347/collect?x=1"})
	on := cspPolicy()
	if !strings.Contains(on, "connect-src 'self' http://localhost:12347;") || strings.Contains(on, "/collect") {
		t.Errorf("Faro collector origin not in connect-src: %s", on)
	}
	withFaro(t, &faroSettings{CollectorURL: "https://faro.example.com/collect/abc"})
	if !strings.Contains(cspPolicy(), "connect-src 'self' https://faro.example.com;") {
		t.Errorf("https collector: %s", cspPolicy())
	}
}

func TestWithCSP_HeadersPerMode(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("x")) })
	h := withCSP(ok)
	get := func(path string) http.Header {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		return rr.Header()
	}

	withCSPMode(t, cspReportOnly)
	for _, page := range []string{"/", "/product/x", "/cart", "/orders", "/no-such-page"} {
		hd := get(page)
		if hd.Get("Content-Security-Policy-Report-Only") == "" || hd.Get("Content-Security-Policy") != "" {
			t.Errorf("%s: report-only mode headers: %v", page, hd)
		}
	}
	for _, asset := range []string{"/static/styles/styles.css", "/healthz", "/_healthz", "/readyz", "/version", "/robots.txt", "/favicon.ico", "/csp-report"} {
		if hd := get(asset); hd.Get("Content-Security-Policy-Report-Only") != "" || hd.Get("Content-Security-Policy") != "" {
			t.Errorf("%s is not a page but got a policy: %v", asset, hd)
		}
	}

	withCSPMode(t, cspEnforce)
	if hd := get("/"); hd.Get("Content-Security-Policy") == "" || hd.Get("Content-Security-Policy-Report-Only") != "" {
		t.Errorf("enforce mode headers: %v", hd)
	}
	withCSPMode(t, cspOff)
	if hd := get("/"); hd.Get("Content-Security-Policy") != "" || hd.Get("Content-Security-Policy-Report-Only") != "" {
		t.Errorf("off mode headers: %v", hd)
	}
}

// The real handler stack: the policy is on the pages and on the error pages, the report endpoint needs no
// session (no Set-Cookie) and answers 204.
func TestCSP_ThroughTheRealStack(t *testing.T) {
	withCSPMode(t, cspReportOnly)
	withFaro(t, nil)
	l := logrus.New()
	l.SetOutput(&strings.Builder{})
	h := newHandler(newFakeFrontend(t, sampleCatalog()), l)

	for target, code := range map[string]int{"/": 200, "/product/pan-05": 200, "/product/nope": 404} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, target, nil))
		if rr.Code != code {
			t.Errorf("%s: status %d, want %d", target, rr.Code, code)
		}
		if !strings.HasPrefix(rr.Header().Get("Content-Security-Policy-Report-Only"), "default-src 'self'") {
			t.Errorf("%s: no policy header: %v", target, rr.Header())
		}
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/csp-report", strings.NewReader(`{"csp-report":{"effective-directive":"script-src-elem","blocked-uri":"inline"}}`))
	req.Header.Set("Content-Type", "application/csp-report")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent || rr.Header().Get("Set-Cookie") != "" || rr.Header().Get("Content-Security-Policy-Report-Only") != "" {
		t.Errorf("report: %d %v", rr.Code, rr.Header())
	}
}

func cspCounter(dir string) float64 { return testutil.ToFloat64(cspReportsTotal.WithLabelValues(dir)) }

func postReport(rep *cspReporter, log logrus.FieldLogger, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/csp-report", strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyLog{}, log))
	rr := httptest.NewRecorder()
	rep.ServeHTTP(rr, req)
	return rr
}

func TestCSPReport_BothFormatsAreCountedAndLoggedWithoutQueryStrings(t *testing.T) {
	log, hook := logtest.NewNullLogger()
	clk := newTestClock()
	rep := &cspReporter{now: clk.Now}

	s0, e0, o0 := cspCounter("script-src-elem"), cspCounter("style-src-attr"), cspCounter("other")
	legacy := `{"csp-report":{"document-uri":"https://shop.example/product/x?token=SECRET#frag","blocked-uri":"https://evil.example/a.js?k=SECRET",
		"violated-directive":"script-src-elem 'self'","effective-directive":"script-src-elem","source-file":"https://shop.example/static/js/a.js?v=1","line-number":7,"disposition":"report"}}`
	if rr := postReport(rep, log, legacy); rr.Code != http.StatusNoContent {
		t.Fatalf("legacy: %d", rr.Code)
	}
	modern := `[{"type":"csp-violation","body":{"documentURL":"https://shop.example/","blockedURL":"inline","effectiveDirective":"style-src-attr","disposition":"report"}},
		{"type":"deprecation","body":{}},
		{"type":"csp-violation","body":{"effectiveDirective":"made-up-by-a-client-` + strings.Repeat("x", 500) + `","blockedURL":"eval"}}]`
	if rr := postReport(rep, log, modern); rr.Code != http.StatusNoContent {
		t.Fatalf("reports+json: %d", rr.Code)
	}
	if cspCounter("script-src-elem")-s0 != 1 || cspCounter("style-src-attr")-e0 != 1 || cspCounter("other")-o0 != 1 {
		t.Errorf("counters: %v %v %v", cspCounter("script-src-elem")-s0, cspCounter("style-src-attr")-e0, cspCounter("other")-o0)
	}
	entries := hook.AllEntries()
	if len(entries) != 3 {
		t.Fatalf("%d log lines, want 3", len(entries))
	}
	first := entries[0]
	if first.Level != logrus.WarnLevel || first.Data["csp.directive"] != "script-src-elem" ||
		first.Data["csp.blocked"] != "https://evil.example/a.js" || first.Data["csp.document"] != "https://shop.example/product/x" ||
		first.Data["csp.source"] != "https://shop.example/static/js/a.js" || first.Data["csp.line"] != 7 {
		t.Errorf("log line: %v", first.Data)
	}
	for _, e := range entries {
		for k, v := range e.Data {
			if s, ok := v.(string); ok && (strings.Contains(s, "SECRET") || len(s) > 200) {
				t.Errorf("%s = %q leaks a query string or is unbounded", k, s)
			}
		}
	}
}

func TestCSPReport_LogBudgetPerWindowWithInjectedClock(t *testing.T) {
	log, hook := logtest.NewNullLogger()
	clk := newTestClock()
	rep := &cspReporter{now: clk.Now}
	one := `{"csp-report":{"effective-directive":"img-src","blocked-uri":"https://x.example/i.png"}}`
	before := cspCounter("img-src")
	for i := 0; i < 50; i++ {
		postReport(rep, log, one)
	}
	if n := len(hook.AllEntries()); n != cspLogBudget {
		t.Errorf("%d log lines in one window, want %d", n, cspLogBudget)
	}
	if got := cspCounter("img-src") - before; got != 50 {
		t.Errorf("counted %v reports, want all 50", got)
	}
	clk.Advance(cspLogWindow + time.Second)
	postReport(rep, log, one)
	if n := len(hook.AllEntries()); n != cspLogBudget+1 {
		t.Errorf("after the window: %d log lines, want %d", n, cspLogBudget+1)
	}
}

func TestCSPReport_RejectsWhatIsNotAReport(t *testing.T) {
	log, hook := logtest.NewNullLogger()
	rep := &cspReporter{}
	for body, want := range map[string]int{
		`not json`:                              http.StatusBadRequest,
		`{}`:                                    http.StatusBadRequest,
		`[]`:                                    http.StatusBadRequest,
		`[{"type":"network-error"}]`:            http.StatusBadRequest,
		strings.Repeat("a", cspMaxReportSize+1): http.StatusRequestEntityTooLarge,
	} {
		name := body
		if len(name) > 20 {
			name = name[:20]
		}
		if rr := postReport(rep, log, body); rr.Code != want {
			t.Errorf("%q: %d, want %d", name, rr.Code, want)
		}
	}
	rr := httptest.NewRecorder()
	rep.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/csp-report", nil))
	if rr.Code != http.StatusMethodNotAllowed || rr.Header().Get("Allow") != "POST" {
		t.Errorf("GET: %d %v", rr.Code, rr.Header())
	}
	if len(hook.AllEntries()) != 0 {
		t.Error("a rejected body was logged")
	}
}

// What the policy relies on, checked on the sources: no inline script, no inline event handler, no inline
// <style> element and no javascript: URL in any template; the first-party scripts do not use eval.
func TestCSP_TemplatesAndScriptsAreStrictPolicyClean(t *testing.T) {
	var (
		inlineScript = regexp.MustCompile(`(?is)<script\b[^>]*>`)
		handler      = regexp.MustCompile(`(?i)\s(on[a-z]+)\s*=\s*["']`)
	)
	files, _ := filepath.Glob("../../templates/*.html")
	if len(files) < 10 {
		t.Fatalf("found %d templates", len(files))
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src := regexp.MustCompile(`(?s){{/\*.*?\*/}}`).ReplaceAllString(string(b), "") // template comments are not output
		for _, tag := range inlineScript.FindAllString(src, -1) {
			if !strings.Contains(tag, " src=") && !strings.Contains(tag, `type="application/json"`) {
				t.Errorf("%s: inline script %s", filepath.Base(f), tag)
			}
		}
		if m := handler.FindStringSubmatch(src); m != nil {
			t.Errorf("%s: inline event handler %q", filepath.Base(f), m[1])
		}
		if strings.Contains(strings.ToLower(src), "<style") || strings.Contains(strings.ToLower(src), "javascript:") {
			t.Errorf("%s: inline <style> or javascript: URL", filepath.Base(f))
		}
	}
	js, _ := filepath.Glob("../../static/js/*.js")
	for _, f := range js {
		if strings.HasSuffix(f, ".min.js") {
			continue // htmx: evaluates only hx-on / js: expressions, which the templates do not use
		}
		b, _ := os.ReadFile(f)
		if regexp.MustCompile(`\beval\s*\(|new Function\s*\(|\binnerHTML\b|document\.write`).Match(b) {
			t.Errorf("%s uses eval, new Function, innerHTML or document.write", filepath.Base(f))
		}
	}
}
