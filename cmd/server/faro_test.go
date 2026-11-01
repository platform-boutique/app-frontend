package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"

	pb "frontend/genproto"
)

// withFaro switches Faro on for one test (the process-wide setting is restored afterwards).
func withFaro(t *testing.T, s *faroSettings) {
	t.Helper()
	old := faro
	faro = s
	t.Cleanup(func() { faro = old })
}

func testFaroSettings() *faroSettings {
	return &faroSettings{CollectorURL: "http://localhost:12347/collect", AppName: "everly-frontend", AppVersion: "dev", Env: "local", SampleRate: 1}
}

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// homePayload is the minimum the home template needs.
func homePayload() map[string]interface{} {
	params := browseParams{Sort: "name", Page: 1}
	return map[string]interface{}{
		"show_currency": true,
		"currencies":    []string{"USD"},
		"products":      []productCard{{Item: sampleProduct("MUG1"), Price: &pb.Money{CurrencyCode: "USD", Units: 12}, Image: "/x.webp", DeptName: "Kitchen"}},
		"total":         1,
		"browse":        params,
		"sort_options":  sortOptions,
		"pagination":    buildPagination(params, 1),
		"categories":    []*pb.Category{{Id: "kitchen", Name: "Kitchen", ProductCount: 1}},
	}
}

// renderHome renders the home page for r (the request carries the session, span, user).
func renderHome(t *testing.T, r *http.Request) string {
	t.Helper()
	var sb strings.Builder
	if err := getTemplates().ExecuteTemplate(&sb, "home", injectCommonTemplateData(r, homePayload())); err != nil {
		t.Fatalf("execute home: %v", err)
	}
	return sb.String()
}

var (
	reScriptTag   = regexp.MustCompile(`(?s)<script\b([^>]*)>(.*?)</script>`)
	reConfigBlock = regexp.MustCompile(`(?s)<script type="application/json" id="faro-config">(.*?)</script>`)
	reMeta        = regexp.MustCompile(`<meta name="traceparent" content="([^"]*)">`)
	reTraceparent = regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$`)
)

// inlineScripts returns the bodies of the executable inline scripts (no src, not a data block).
func inlineScripts(html string) []string {
	var out []string
	for _, m := range reScriptTag.FindAllStringSubmatch(html, -1) {
		if strings.Contains(m[1], "src=") || strings.Contains(m[1], `type="application/json"`) {
			continue
		}
		out = append(out, m[2])
	}
	return out
}

func TestFaroSettings_Off(t *testing.T) {
	for _, v := range []string{"", "   "} {
		s, err := faroSettingsFromEnv(envMap(map[string]string{"FARO_COLLECTOR_URL": v, "FARO_APP_NAME": "ignored"}))
		if s != nil || err != nil {
			t.Errorf("collector %q: got %v, %v, want nil, nil (Faro off)", v, s, err)
		}
	}
}

func TestFaroSettings_Defaults(t *testing.T) {
	s, err := faroSettingsFromEnv(envMap(map[string]string{"FARO_COLLECTOR_URL": "https://faro.example.com/collect"}))
	if err != nil {
		t.Fatal(err)
	}
	want := faroSettings{CollectorURL: "https://faro.example.com/collect", AppName: "everly-frontend", AppVersion: "dev", Env: "local", SampleRate: 1}
	if *s != want {
		t.Errorf("got %+v, want %+v", *s, want)
	}
}

func TestFaroSettings_Custom(t *testing.T) {
	s, err := faroSettingsFromEnv(envMap(map[string]string{
		"FARO_COLLECTOR_URL": " http://alloy:12347/collect ", "FARO_APP_NAME": "shop", "FARO_APP_VERSION": "1.2.3",
		"FARO_ENV": "prod", "FARO_SESSION_SAMPLE_RATE": "0.25",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := faroSettings{CollectorURL: "http://alloy:12347/collect", AppName: "shop", AppVersion: "1.2.3", Env: "prod", SampleRate: 0.25}
	if *s != want {
		t.Errorf("got %+v, want %+v", *s, want)
	}
	for _, rate := range []string{"0", "1"} {
		if _, err := faroSettingsFromEnv(envMap(map[string]string{"FARO_COLLECTOR_URL": "http://a/b", "FARO_SESSION_SAMPLE_RATE": rate})); err != nil {
			t.Errorf("rate %s must be accepted: %v", rate, err)
		}
	}
}

func TestFaroSettings_Invalid(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"ftp scheme", map[string]string{"FARO_COLLECTOR_URL": "ftp://alloy/collect"}, "http:// or https://"},
		{"javascript scheme", map[string]string{"FARO_COLLECTOR_URL": "javascript:alert(1)"}, "http:// or https://"},
		{"no scheme (host:port)", map[string]string{"FARO_COLLECTOR_URL": "alloy:12347/collect"}, "http:// or https://"},
		{"scheme-less path", map[string]string{"FARO_COLLECTOR_URL": "/collect"}, "http:// or https://"},
		{"no host", map[string]string{"FARO_COLLECTOR_URL": "http:///collect"}, "no host"},
		{"credentials", map[string]string{"FARO_COLLECTOR_URL": "https://user:pw@alloy/collect"}, "credentials"},
		{"garbage", map[string]string{"FARO_COLLECTOR_URL": "http://[::1"}, "not a valid URL"},
		{"rate text", map[string]string{"FARO_COLLECTOR_URL": "http://a/b", "FARO_SESSION_SAMPLE_RATE": "half"}, "0 to 1"},
		{"rate high", map[string]string{"FARO_COLLECTOR_URL": "http://a/b", "FARO_SESSION_SAMPLE_RATE": "1.5"}, "0 to 1"},
		{"rate negative", map[string]string{"FARO_COLLECTOR_URL": "http://a/b", "FARO_SESSION_SAMPLE_RATE": "-0.1"}, "0 to 1"},
		{"rate NaN", map[string]string{"FARO_COLLECTOR_URL": "http://a/b", "FARO_SESSION_SAMPLE_RATE": "NaN"}, "0 to 1"},
		{"name too long", map[string]string{"FARO_COLLECTOR_URL": "http://a/b", "FARO_APP_NAME": strings.Repeat("x", 129)}, "FARO_APP_NAME"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, err := faroSettingsFromEnv(envMap(c.env))
			if err == nil || s != nil {
				t.Fatalf("got %v, %v, want an error", s, err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not mention %q", err, c.want)
			}
		})
	}
}

func TestFaroPage_Off(t *testing.T) {
	withFaro(t, nil)
	html := renderHome(t, requestWithContext(http.MethodGet, "/", ""))
	for _, gone := range []string{"faro", "Faro", "traceparent", "vendor/"} {
		if strings.Contains(html, gone) {
			t.Errorf("Faro is off but the page contains %q", gone)
		}
	}
	// still the normal page
	assertContains(t, html, "/static/js/htmx.min.js", `href="/product/MUG1"`)
}

func TestFaroPage_OffEvenWithSpan(t *testing.T) {
	withFaro(t, nil)
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	h := otelhttp.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(renderHome(t, r)))
	}), "frontend", otelhttp.WithTracerProvider(tp))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, requestWithContext(http.MethodGet, "/", ""))
	if strings.Contains(rr.Body.String(), "traceparent") {
		t.Error("Faro off: no traceparent meta tag expected even with a recording span")
	}
}

func TestFaroPage_On(t *testing.T) {
	withFaro(t, testFaroSettings())
	html := renderHome(t, requestWithContext(http.MethodGet, "/", ""))

	assertContains(t, html,
		`<script defer src="/static/js/vendor/faro/faro-web-sdk.iife.js"></script>`,
		`<script defer src="/static/js/vendor/faro/faro-web-tracing.iife.js"></script>`,
		`<script defer src="/static/js/faro-init.js"></script>`)
	// the order matters: sdk, tracing (needs the sdk global), init
	i1 := strings.Index(html, "faro-web-sdk.iife.js")
	i2 := strings.Index(html, "faro-web-tracing.iife.js")
	i3 := strings.Index(html, "faro-init.js")
	if i1 >= i2 || i2 >= i3 {
		t.Errorf("script order wrong: %d %d %d", i1, i2, i3)
	}
	if strings.Contains(html, `name="traceparent"`) {
		t.Error("no span in the request: no traceparent meta tag expected")
	}

	m := reConfigBlock.FindStringSubmatch(html)
	if m == nil {
		t.Fatalf("no faro-config block in the page")
	}
	var got faroPageConfig
	if err := json.Unmarshal([]byte(m[1]), &got); err != nil {
		t.Fatalf("faro-config is not valid JSON: %v\n%s", err, m[1])
	}
	want := faroPageConfig{CollectorURL: "http://localhost:12347/collect", AppName: "everly-frontend", AppVersion: "dev",
		Environment: "local", SessionSampleRate: 1, ShopSessionID: "test-session-id", ShopUserSignedIn: false}
	if got != want {
		t.Errorf("config block = %+v, want %+v", got, want)
	}
	// the footer prints the same session id the block carries (the browser test compares them in a real page)
	assertContains(t, html, "session-id: test-session-id")
}

func TestFaroPage_SignedInIsOnlyABoolean(t *testing.T) {
	withFaro(t, testFaroSettings())
	r := requestWithContext(http.MethodGet, "/", "")
	u := &accountUser{ID: "11111111-user-id", Email: "jane@example.com", Name: "Jane Doe"}
	r = r.WithContext(context.WithValue(r.Context(), ctxKeyIdentity{}, identity{user: u}))
	html := renderHome(t, r)

	var got faroPageConfig
	if err := json.Unmarshal([]byte(reConfigBlock.FindStringSubmatch(html)[1]), &got); err != nil {
		t.Fatal(err)
	}
	if !got.ShopUserSignedIn {
		t.Error("signed in: shopUserSignedIn must be true")
	}
	block := reConfigBlock.FindString(html)
	for _, secret := range []string{u.ID, u.Email, u.Name, "Jane"} {
		if strings.Contains(block, secret) {
			t.Errorf("the config block leaks %q", secret)
		}
	}
}

func TestFaroPage_ConfigBlockCannotBreakOut(t *testing.T) {
	evil := []string{
		`</script><script>alert(1)</script>`,
		`"}; alert(1); //`,
		`<!--<script>`,
		"line\nbreak   & <b>'\"",
	}
	for _, name := range evil {
		withFaro(t, &faroSettings{
			CollectorURL: "http://localhost:12347/collect?a=1&b=<x>",
			AppName:      name, AppVersion: name, Env: name, SampleRate: 0.5,
		})
		before := len(reScriptTag.FindAllString(renderHomeOff(t), -1))
		html := renderHome(t, requestWithContext(http.MethodGet, "/", ""))

		// exactly four more <script> elements: the data block, sdk, tracing, init
		if n := len(reScriptTag.FindAllString(html, -1)); n != before+4 {
			t.Errorf("name %q: %d script elements, want %d", name, n, before+4)
		}
		if strings.Contains(html, "alert(1)</script>") || strings.Contains(html, "<script>alert") {
			t.Errorf("name %q: injected script found in the page", name)
		}
		m := reConfigBlock.FindStringSubmatch(html)
		if m == nil {
			t.Fatalf("name %q: config block missing or broken", name)
		}
		if strings.Contains(m[1], "<") || strings.Contains(m[1], ">") {
			t.Errorf("name %q: raw angle bracket inside the JSON block: %s", name, m[1])
		}
		var got faroPageConfig
		if err := json.Unmarshal([]byte(m[1]), &got); err != nil {
			t.Fatalf("name %q: block is not valid JSON: %v\n%s", name, err, m[1])
		}
		if got.AppName != name || got.AppVersion != name || got.Environment != name {
			t.Errorf("name %q: round trip gave %q %q %q", name, got.AppName, got.AppVersion, got.Environment)
		}
		if got.CollectorURL != "http://localhost:12347/collect?a=1&b=<x>" {
			t.Errorf("collector url round trip: %q", got.CollectorURL)
		}
	}
}

func renderHomeOff(t *testing.T) string {
	t.Helper()
	old := faro
	faro = nil
	defer func() { faro = old }()
	return renderHome(t, requestWithContext(http.MethodGet, "/", ""))
}

func TestFaroPage_NoInlineScriptAdded(t *testing.T) {
	off := inlineScripts(renderHomeOff(t))
	withFaro(t, testFaroSettings())
	on := inlineScripts(renderHome(t, requestWithContext(http.MethodGet, "/", "")))
	// The page already has two inline scripts of its own (theme, in header and footer; unrelated to
	// Faro). Faro must not add a third: its settings are a data block, everything else is a file.
	if len(on) != len(off) {
		t.Errorf("Faro on adds inline scripts: %d inline scripts on, %d off", len(on), len(off))
	}
	for i := range off {
		if on[i] != off[i] {
			t.Errorf("inline script %d changed with Faro on", i)
		}
	}
	html := renderHome(t, requestWithContext(http.MethodGet, "/", ""))
	for _, m := range reScriptTag.FindAllStringSubmatch(html, -1) {
		if strings.Contains(m[1], "faro") && strings.TrimSpace(m[2]) != "" && !strings.Contains(m[1], "application/json") {
			t.Errorf("a Faro script tag has inline code: %s", m[0])
		}
	}
	if strings.Contains(html, "javascript:") {
		t.Error("javascript: URL in the page")
	}
}

// pageThroughOtel serves the home page behind the real otelhttp handler (outermost, as in main)
// with the given tracer provider and returns the HTML.
func pageThroughOtel(t *testing.T, tp *sdktrace.TracerProvider, header http.Header) string {
	t.Helper()
	opts := []otelhttp.Option{otelhttp.WithPropagators(propagation.TraceContext{})}
	if tp != nil {
		opts = append(opts, otelhttp.WithTracerProvider(tp))
	} else {
		opts = append(opts, otelhttp.WithTracerProvider(noop.NewTracerProvider()))
	}
	h := otelhttp.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(renderHome(t, r)))
	}), "frontend", opts...)
	req := requestWithContext(http.MethodGet, "/", "")
	for k, v := range header {
		req.Header[k] = v
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr.Body.String()
}

func TestFaroPage_TraceparentIsTheServerSpan(t *testing.T) {
	withFaro(t, testFaroSettings())
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec), sdktrace.WithSampler(sdktrace.AlwaysSample()))

	html := pageThroughOtel(t, tp, nil)

	m := reMeta.FindStringSubmatch(html)
	if m == nil {
		t.Fatalf("no traceparent meta tag in the page")
	}
	if !reTraceparent.MatchString(m[1]) {
		t.Fatalf("traceparent %q is not 00-<32 hex>-<16 hex>-<2 hex>", m[1])
	}
	if n := strings.Count(html, `name="traceparent"`); n != 1 {
		t.Errorf("%d traceparent meta tags, want 1", n)
	}
	// the meta tag is in <head>
	if strings.Index(html, `name="traceparent"`) > strings.Index(html, "</head>") {
		t.Error("traceparent meta tag is not in the head")
	}

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("%d spans recorded, want the one server span", len(spans))
	}
	sc := spans[0].SpanContext()
	want := "00-" + sc.TraceID().String() + "-" + sc.SpanID().String() + "-01"
	if m[1] != want {
		t.Errorf("meta traceparent = %s, the server span is %s", m[1], want)
	}
	parts := strings.Split(m[1], "-")
	if parts[1] == strings.Repeat("0", 32) || parts[2] == strings.Repeat("0", 16) {
		t.Error("all-zero id in traceparent")
	}
	if _, err := hex.DecodeString(parts[1] + parts[2] + parts[3]); err != nil {
		t.Errorf("not hex: %v", err)
	}
}

func TestFaroPage_NoTraceparentWithoutAValidSampledLocalSpan(t *testing.T) {
	withFaro(t, testFaroSettings())
	incoming := http.Header{"Traceparent": {"00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"}}

	cases := map[string]string{
		"telemetry off (no-op tracer)":                        pageThroughOtel(t, nil, nil),
		"telemetry off, request carries a remote traceparent": pageThroughOtel(t, nil, incoming),
		"span not sampled": pageThroughOtel(t,
			sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.NeverSample())), nil),
	}
	for name, html := range cases {
		if strings.Contains(html, `name="traceparent"`) {
			t.Errorf("%s: unexpected traceparent meta tag", name)
		}
		// Faro itself is still on
		if !strings.Contains(html, `id="faro-config"`) {
			t.Errorf("%s: faro-config block must still be there", name)
		}
	}
	// a request with no context span at all (what the other tests use)
	if got := traceparentOf(requestWithContext(http.MethodGet, "/", "")); got != "" {
		t.Errorf("no span: got %q", got)
	}
}

func TestFaroStaticFilesAreServed(t *testing.T) {
	// the same handler as main: http.FileServer over ./static
	srv := httptest.NewServer(http.StripPrefix("/static/", http.FileServer(http.Dir("../../static/"))))
	defer srv.Close()

	pinned := map[string]string{ // sha256, also in the README
		"js/vendor/faro/faro-web-sdk.iife.js":     "85741c45fd1fd4875c5f25a95a7940e1171ea11e308a83d88fd5f3a5dcab8c1c",
		"js/vendor/faro/faro-web-tracing.iife.js": "8ca0f7dcc4d500d57ad88885e8dbe059a4d013fbfb98875fbf6b97eebec6b478",
		"js/vendor/faro/LICENSE":                  "c71d239df91726fc519c6eb72d318ec65820627232b2f796219e87dcf35d0ab4",
	}
	files := []string{"js/vendor/faro/faro-web-sdk.iife.js", "js/vendor/faro/faro-web-tracing.iife.js", "js/faro-init.js", "js/vendor/faro/LICENSE"}
	for _, f := range files {
		resp, err := http.Get(srv.URL + "/static/" + f)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status %d", f, resp.StatusCode)
			continue
		}
		ct := resp.Header.Get("Content-Type")
		if strings.HasSuffix(f, ".js") && !strings.HasPrefix(ct, "text/javascript") && !strings.HasPrefix(ct, "application/javascript") {
			t.Errorf("%s: content type %q, a browser needs a JavaScript type", f, ct)
		}
		if want, ok := pinned[f]; ok {
			sum := sha256.Sum256(raw)
			if got := hex.EncodeToString(sum[:]); got != want {
				t.Errorf("%s: sha256 %s, pinned %s (vendored file changed: update the README too)", f, got, want)
			}
		}
	}
}

func TestFaroInitScript_DoesNotUseTheCookie(t *testing.T) {
	js, err := os.ReadFile("../../static/js/faro-init.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(js)
	if strings.Contains(s, "document.cookie") {
		t.Error("faro-init.js must not read cookies; the session id comes from the config block")
	}
	for _, want := range []string{"shop_session_id", "shop_user_signed_in", "htmx:responseError", "htmx:sendError", "beforeSend"} {
		if !strings.Contains(s, want) {
			t.Errorf("faro-init.js is missing %q", want)
		}
	}
}
