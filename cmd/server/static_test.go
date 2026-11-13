package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"

	pb "frontend/genproto"
)

// fixedMod is the modification time of every file the tests create: the "clock" of Last-Modified.
var fixedMod = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

// staticSite writes files (path -> content) under a temp dir with a fixed mtime and returns the
// handler chain of main (session middleware, logger, identity, router with /static/ and /favicon.ico).
func staticSite(t *testing.T, files map[string]string) (http.Handler, *staticServer) {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, fixedMod, fixedMod); err != nil {
			t.Fatal(err)
		}
	}
	return staticChain(newStaticServer(http.Dir(dir)))
}

func staticChain(s *staticServer) (http.Handler, *staticServer) {
	log := logrus.New()
	log.Out = io.Discard
	fe := &frontendServer{} // users == nil: identity is not looked up
	r := mux.NewRouter()
	r.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("home")) })
	r.PathPrefix("/static/").Handler(http.StripPrefix("/static/", s))
	r.Handle("/favicon.ico", s.file("/favicon.svg"))
	return ensureSessionID(&logHandler{log: log, next: fe.withUser(r)}), s
}

func get(h http.Handler, method, target string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func bigCSS() string { return strings.Repeat("body { color: #123456; margin: 0 }\n", 100) }

var siteFiles = func() map[string]string {
	return map[string]string{
		"styles/styles.css":                bigCSS(),
		"styles/tiny.css":                  "a{b:c}",
		"js/app.js":                        strings.Repeat("console.log('x');\n", 50),
		"js/vendor/faro/sdk.iife.js":       strings.Repeat("var a=1;\n", 100),
		"img/products/pan-1-thumb.webp":    strings.Repeat("\x00", 4096), // very compressible bytes, must still be served as is
		"favicon.svg":                      `<svg xmlns="http://www.w3.org/2000/svg">` + strings.Repeat(" ", 300) + `</svg>`,
		"data/list.json":                   `{"a":"` + strings.Repeat("b", 400) + `"}`,
		"img/products/sub/also-a-dir.webp": "x",
	}
}

func gunzip(t *testing.T, b []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("not gzip: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestStatic_NoCookieEver(t *testing.T) {
	h, _ := staticSite(t, siteFiles())
	for _, target := range []string{
		"/static/styles/styles.css", "/static/img/products/pan-1-thumb.webp", "/static/js/vendor/faro/sdk.iife.js",
		"/favicon.ico", "/static/nope.css", "/static/img/", "/static/styles/tiny.css",
	} {
		for _, hdr := range [][]string{nil, {"Cookie", cookieSessionID + "=abc"}, {"Accept-Encoding", "gzip"}} {
			rr := get(h, http.MethodGet, target, hdr...)
			if v := rr.Result().Header.Values("Set-Cookie"); len(v) != 0 {
				t.Errorf("%s %v: Set-Cookie %v on a static response (404s included)", target, hdr, v)
			}
		}
	}
	// the pages keep their session cookie
	if rr := get(h, http.MethodGet, "/"); len(rr.Result().Cookies()) != 1 {
		t.Errorf("/ must still set the session cookie, got %v", rr.Result().Cookies())
	}
}

func TestStatic_CacheControl(t *testing.T) {
	h, _ := staticSite(t, siteFiles())
	cases := map[string]string{
		"/static/styles/styles.css":             staticCacheShort,
		"/static/js/app.js":                     staticCacheShort,
		"/static/styles/tiny.css":               staticCacheShort,
		"/static/img/products/pan-1-thumb.webp": staticCacheLong,
		"/static/js/vendor/faro/sdk.iife.js":    staticCacheLong,
		"/favicon.ico":                          staticCacheShort,
	}
	for target, want := range cases {
		rr := get(h, http.MethodGet, target)
		if rr.Code != http.StatusOK {
			t.Errorf("%s: status %d", target, rr.Code)
		}
		if got := rr.Header().Get("Cache-Control"); got != want {
			t.Errorf("%s: Cache-Control %q, want %q", target, got, want)
		}
		if rr.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: no nosniff", target)
		}
	}
	// the policy itself: a window of at least a day for images/vendor, minutes plus revalidation for ours
	if !strings.Contains(staticCacheLong, "max-age=86400") || !strings.Contains(staticCacheShort, "must-revalidate") {
		t.Error("cache policy constants changed: update the README")
	}
}

func TestStatic_ValidatorsAnd304(t *testing.T) {
	h, _ := staticSite(t, siteFiles())
	for _, target := range []string{"/static/styles/styles.css", "/static/img/products/pan-1-thumb.webp", "/favicon.ico"} {
		first := get(h, http.MethodGet, target)
		etag, lm := first.Header().Get("ETag"), first.Header().Get("Last-Modified")
		if etag == "" || !strings.HasPrefix(etag, `"`) {
			t.Fatalf("%s: ETag %q", target, etag)
		}
		if lm != fixedMod.Format(http.TimeFormat) {
			t.Fatalf("%s: Last-Modified %q, want the file's mtime", target, lm)
		}
		// If-None-Match
		rr := get(h, http.MethodGet, target, "If-None-Match", etag)
		if rr.Code != http.StatusNotModified || rr.Body.Len() != 0 {
			t.Errorf("%s: If-None-Match -> %d, %d body bytes", target, rr.Code, rr.Body.Len())
		}
		for _, k := range []string{"Cache-Control", "ETag"} {
			if rr.Header().Get(k) == "" {
				t.Errorf("%s: 304 lacks %s", target, k)
			}
		}
		// If-Modified-Since
		if rr := get(h, http.MethodGet, target, "If-Modified-Since", lm); rr.Code != http.StatusNotModified {
			t.Errorf("%s: If-Modified-Since -> %d", target, rr.Code)
		}
		// an older copy is refreshed
		old := fixedMod.Add(-time.Hour).Format(http.TimeFormat)
		if rr := get(h, http.MethodGet, target, "If-Modified-Since", old); rr.Code != http.StatusOK {
			t.Errorf("%s: older copy -> %d", target, rr.Code)
		}
		// a different ETag is refreshed
		if rr := get(h, http.MethodGet, target, "If-None-Match", `"other"`); rr.Code != http.StatusOK {
			t.Errorf("%s: other etag -> %d", target, rr.Code)
		}
	}
}

func TestStatic_GzipTextTypes(t *testing.T) {
	files := siteFiles()
	h, _ := staticSite(t, files)
	for target, name := range map[string]string{
		"/static/styles/styles.css": "styles/styles.css", "/static/js/app.js": "js/app.js",
		"/static/data/list.json": "data/list.json", "/favicon.ico": "favicon.svg",
		"/static/js/vendor/faro/sdk.iife.js": "js/vendor/faro/sdk.iife.js",
	} {
		plain := get(h, http.MethodGet, target)
		if plain.Header().Get("Content-Encoding") != "" || plain.Body.String() != files[name] {
			t.Errorf("%s: without Accept-Encoding the body must be the file as is", target)
		}
		if !strings.Contains(plain.Header().Get("Vary"), "Accept-Encoding") {
			t.Errorf("%s: identity response lacks Vary: Accept-Encoding (a cache would serve it to gzip clients)", target)
		}
		z := get(h, http.MethodGet, target, "Accept-Encoding", "br, gzip;q=0.8")
		if z.Header().Get("Content-Encoding") != "gzip" || !strings.Contains(z.Header().Get("Vary"), "Accept-Encoding") {
			t.Fatalf("%s: Content-Encoding %q Vary %q", target, z.Header().Get("Content-Encoding"), z.Header().Get("Vary"))
		}
		if got := gunzip(t, z.Body.Bytes()); got != files[name] {
			t.Errorf("%s: gunzipped body differs from the file", target)
		}
		if z.Body.Len() >= len(files[name]) {
			t.Errorf("%s: gzip %d bytes is not smaller than %d", target, z.Body.Len(), len(files[name]))
		}
		if ct := z.Header().Get("Content-Type"); ct != plain.Header().Get("Content-Type") || ct == "" {
			t.Errorf("%s: content type changed by gzip: %q vs %q", target, ct, plain.Header().Get("Content-Type"))
		}
		if z.Header().Get("Content-Length") != itoa(z.Body.Len()) {
			t.Errorf("%s: Content-Length %q for %d bytes", target, z.Header().Get("Content-Length"), z.Body.Len())
		}
		// the ETag of the variants differ, each validates only itself
		if z.Header().Get("ETag") == plain.Header().Get("ETag") {
			t.Errorf("%s: gzip and identity share an ETag", target)
		}
		if rr := get(h, http.MethodGet, target, "Accept-Encoding", "gzip", "If-None-Match", z.Header().Get("ETag")); rr.Code != http.StatusNotModified {
			t.Errorf("%s: gzip revalidation -> %d", target, rr.Code)
		}
		if rr := get(h, http.MethodGet, target, "Accept-Encoding", "gzip", "If-None-Match", plain.Header().Get("ETag")); rr.Code != http.StatusOK {
			t.Errorf("%s: identity ETag must not validate the gzip variant, got %d", target, rr.Code)
		}
		if rr := get(h, http.MethodGet, target, "Accept-Encoding", "gzip", "If-None-Match", z.Header().Get("ETag")); rr.Header().Get("Vary") == "" {
			t.Errorf("%s: 304 lost Vary", target)
		}
	}
}

func itoa(n int) string { return fmtInt(n) }

func fmtInt(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

func TestStatic_NoGzipWhenNotWanted(t *testing.T) {
	files := siteFiles()
	h, _ := staticSite(t, files)
	// images are never compressed again, however compressible the bytes and whatever the client says
	rr := get(h, http.MethodGet, "/static/img/products/pan-1-thumb.webp", "Accept-Encoding", "gzip, deflate, br")
	if rr.Header().Get("Content-Encoding") != "" || rr.Body.String() != files["img/products/pan-1-thumb.webp"] {
		t.Error("webp was compressed")
	}
	if strings.Contains(rr.Header().Get("Vary"), "Accept-Encoding") {
		t.Error("an uncompressed type does not vary by encoding")
	}
	if ct := rr.Header().Get("Content-Type"); ct != "image/webp" {
		t.Errorf("webp content type %q", ct)
	}
	// tiny text files are not worth it
	if rr := get(h, http.MethodGet, "/static/styles/tiny.css", "Accept-Encoding", "gzip"); rr.Header().Get("Content-Encoding") != "" {
		t.Error("a 6 byte file was compressed")
	}
	// client says no
	for _, ae := range []string{"", "identity", "gzip;q=0", "br", "deflate", "*;q=0", "gzip;q=0, *"} {
		if rr := get(h, http.MethodGet, "/static/styles/styles.css", "Accept-Encoding", ae); rr.Header().Get("Content-Encoding") != "" {
			t.Errorf("Accept-Encoding %q: got %q", ae, rr.Header().Get("Content-Encoding"))
		}
	}
	for _, ae := range []string{"gzip", "GZIP", "x-gzip", "gzip;q=0.5", "*", "br, gzip", "deflate, gzip;q=1.0", " gzip , br"} {
		if rr := get(h, http.MethodGet, "/static/styles/styles.css", "Accept-Encoding", ae); rr.Header().Get("Content-Encoding") != "gzip" {
			t.Errorf("Accept-Encoding %q: want gzip", ae)
		}
	}
}

func TestStatic_RangeOfGzipAndHead(t *testing.T) {
	h, _ := staticSite(t, siteFiles())
	rr := get(h, http.MethodHead, "/static/styles/styles.css", "Accept-Encoding", "gzip")
	if rr.Code != 200 || rr.Body.Len() != 0 || rr.Header().Get("Content-Encoding") != "gzip" || rr.Header().Get("Content-Length") == "" {
		t.Errorf("HEAD: %d, body %d, headers %v", rr.Code, rr.Body.Len(), rr.Header())
	}
	if rr := get(h, http.MethodHead, "/static/styles/styles.css"); rr.Code != 200 || rr.Body.Len() != 0 {
		t.Errorf("HEAD plain: %d", rr.Code)
	}
	if rr := get(h, http.MethodGet, "/static/styles/styles.css", "Accept-Encoding", "gzip", "Range", "bytes=0-9"); rr.Code != http.StatusPartialContent || rr.Body.Len() != 10 {
		t.Errorf("range: %d, %d bytes", rr.Code, rr.Body.Len())
	}
}

func TestStatic_NoDirectoryListing(t *testing.T) {
	h, _ := staticSite(t, siteFiles())
	for _, target := range []string{
		"/static/", "/static/img/", "/static/img", "/static/img/products/", "/static/img/products", "/static/js/vendor/",
		"/static/styles", "/static/js/vendor/faro/", "/static//", "/static/img/products/sub/", "/static/index.html",
	} {
		rr := get(h, http.MethodGet, target)
		if rr.Code != http.StatusNotFound {
			// "/static/img" without a slash is not matched by the /static/ prefix route at all: also 404 (or a clean redirect)
			if rr.Code != http.StatusMovedPermanently || strings.Contains(rr.Body.String(), "pan-1") {
				t.Errorf("%s: status %d", target, rr.Code)
			}
			continue
		}
		body := rr.Body.String()
		for _, leak := range []string{"pan-1-thumb", "styles.css", "<a href", "vendor"} {
			if strings.Contains(body, leak) {
				t.Errorf("%s: 404 body leaks %q: %s", target, leak, body)
			}
		}
	}
}

func TestStatic_NotFoundTraversalAndMethods(t *testing.T) {
	h, s := staticSite(t, siteFiles())
	if rr := get(h, http.MethodGet, "/static/missing.css"); rr.Code != 404 {
		t.Errorf("missing: %d", rr.Code)
	}
	// ".." can not leave the root: straight at the handler (the router would clean it first)
	for _, p := range []string{"../static_test.go", "/../../etc/passwd", "styles/../../x", "%2e%2e/x"} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.URL.Path = p
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, req)
		if rr.Code == 200 {
			t.Errorf("%q served: %s", p, rr.Body.String())
		}
	}
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rr := get(h, m, "/static/styles/styles.css")
		if rr.Code != http.StatusMethodNotAllowed || rr.Header().Get("Allow") != "GET, HEAD" {
			t.Errorf("%s: %d allow %q", m, rr.Code, rr.Header().Get("Allow"))
		}
	}
}

func TestStatic_FaviconIco(t *testing.T) {
	files := siteFiles()
	h, _ := staticSite(t, files)
	rr := get(h, http.MethodGet, "/favicon.ico")
	if rr.Code != 200 || rr.Header().Get("Content-Type") != "image/svg+xml" || rr.Body.String() != files["favicon.svg"] {
		t.Errorf("favicon.ico: %d %q", rr.Code, rr.Header().Get("Content-Type"))
	}
	if len(rr.Result().Cookies()) != 0 {
		t.Error("favicon.ico sets a cookie")
	}
	// the same with the real files
	h2, _ := staticChain(newStaticServer(http.Dir("../../static/")))
	real := get(h2, http.MethodGet, "/favicon.ico")
	want, err := os.ReadFile("../../static/favicon.svg")
	if err != nil {
		t.Fatal(err)
	}
	if real.Code != 200 || real.Body.String() != string(want) {
		t.Errorf("real favicon.ico: %d", real.Code)
	}
}

func TestStatic_GzipCacheIsBoundedAndReused(t *testing.T) {
	files := map[string]string{}
	for i := 0; i < gzipMaxEntries+10; i++ {
		files["styles/s"+fmtInt(i)+".css"] = bigCSS()
	}
	h, s := staticSite(t, files)
	for i := 0; i < gzipMaxEntries+10; i++ {
		rr := get(h, http.MethodGet, "/static/styles/s"+fmtInt(i)+".css", "Accept-Encoding", "gzip")
		if rr.Header().Get("Content-Encoding") != "gzip" {
			t.Fatalf("file %d not gzip", i)
		}
	}
	s.mu.Lock()
	n := len(s.gz)
	s.mu.Unlock()
	if n > gzipMaxEntries || n == 0 {
		t.Errorf("cache holds %d entries, bound %d", n, gzipMaxEntries)
	}
	// reused: the same slice is served again
	get(h, http.MethodGet, "/static/styles/s0.css", "Accept-Encoding", "gzip")
	s.mu.Lock()
	a := s.gz["/styles/s0.css"].data
	s.mu.Unlock()
	get(h, http.MethodGet, "/static/styles/s0.css", "Accept-Encoding", "gzip")
	s.mu.Lock()
	b := s.gz["/styles/s0.css"].data
	s.mu.Unlock()
	if len(a) == 0 || &a[0] != &b[0] {
		t.Error("the compressed body was recomputed")
	}
}

func TestStatic_GzipRefreshesWhenTheFileChanges(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.css")
	write := func(content string, mod time.Time) {
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	h, _ := staticChain(newStaticServer(http.Dir(dir)))
	write(strings.Repeat("a", 500), fixedMod)
	first := get(h, http.MethodGet, "/static/a.css", "Accept-Encoding", "gzip")
	write(strings.Repeat("b", 500), fixedMod.Add(time.Minute))
	second := get(h, http.MethodGet, "/static/a.css", "Accept-Encoding", "gzip")
	if gunzip(t, second.Body.Bytes()) != strings.Repeat("b", 500) {
		t.Error("stale compressed copy served after the file changed")
	}
	if first.Header().Get("ETag") == second.Header().Get("ETag") {
		t.Error("ETag did not change with the file")
	}
}

func TestAcceptsGzip(t *testing.T) {
	for in, want := range map[string]bool{
		"": false, "gzip": true, "br": false, "*": true, "gzip;q=0": false, "gzip;q=0.0": false, "gzip;q=abc": false,
		"identity, gzip;q=0.1": true, "*;q=0.5, gzip;q=0": false, "br;q=1, *;q=0.1": true,
	} {
		if got := acceptsGzip(in); got != want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", in, got, want)
		}
	}
}

// The real files of the shop: the CSS, the htmx and Faro scripts shrink, the images are untouched.
func TestStatic_RealFiles(t *testing.T) {
	h, _ := staticChain(newStaticServer(http.Dir("../../static/")))
	for _, f := range []string{"styles/styles.css", "styles/browse.css", "js/htmx.min.js", "js/faro-init.js",
		"js/vendor/faro/faro-web-sdk.iife.js", "js/vendor/faro/faro-web-tracing.iife.js"} {
		raw, err := os.ReadFile("../../static/" + f)
		if err != nil {
			t.Fatal(err)
		}
		z := get(h, http.MethodGet, "/static/"+f, "Accept-Encoding", "gzip")
		if z.Code != 200 || z.Header().Get("Content-Encoding") != "gzip" || gunzip(t, z.Body.Bytes()) != string(raw) {
			t.Errorf("%s: %d %q", f, z.Code, z.Header().Get("Content-Encoding"))
		}
		if z.Body.Len() > len(raw)*7/10 {
			t.Errorf("%s: gzip %d of %d bytes", f, z.Body.Len(), len(raw))
		}
		if len(z.Result().Cookies()) != 0 {
			t.Errorf("%s: cookie", f)
		}
	}
	matches, _ := filepath.Glob("../../static/img/products/*-thumb.webp")
	if len(matches) == 0 {
		t.Fatal("no thumbnails found")
	}
	name := filepath.Base(matches[0])
	raw, _ := os.ReadFile(matches[0])
	rr := get(h, http.MethodGet, "/static/img/products/"+name, "Accept-Encoding", "gzip")
	if rr.Header().Get("Content-Encoding") != "" || !bytes.Equal(rr.Body.Bytes(), raw) || rr.Header().Get("Content-Type") != "image/webp" {
		t.Error("a real webp thumbnail was altered")
	}
	// directory listing off with the real tree
	for _, d := range []string{"/static/", "/static/img/products/", "/static/js/vendor/faro/"} {
		if rr := get(h, http.MethodGet, d); rr.Code != 404 || strings.Contains(rr.Body.String(), ".webp") {
			t.Errorf("%s: %d", d, rr.Code)
		}
	}
}

func TestTemplates_CartUsesTheThumbnail(t *testing.T) {
	type line struct {
		Item     *pb.Product
		Quantity int32
		Price    *pb.Money
		Left     int32
		Short    bool
		Low      bool
	}
	withThumb := sampleProduct("MUG1")
	withThumb.Picture = "/static/img/products/mug-1-1.webp"
	withThumb.Thumbnail = "/static/img/products/mug-1-thumb.webp"
	without := sampleProduct("JAR2")
	without.Picture = "/static/img/products/jar-2-1.webp"
	html := renderPage(t, "cart", map[string]interface{}{
		"currencies": []string{"USD"}, "cart_size": 2, "show_currency": true,
		"items": []line{
			{Item: withThumb, Quantity: 1, Price: &pb.Money{CurrencyCode: "USD", Units: 12}, Left: 20},
			{Item: without, Quantity: 1, Price: &pb.Money{CurrencyCode: "USD", Units: 12}, Left: 20},
		},
		"shipping_cost": &pb.Money{CurrencyCode: "USD", Units: 5}, "total_cost": &pb.Money{CurrencyCode: "USD", Units: 29},
		"expiration_years": []int{2026}, "countries": []string{usCountry}, "us_country": usCountry,
	})
	assertContains(t, html, `src="/static/img/products/mug-1-thumb.webp"`, `src="/static/img/products/jar-2-1.webp"`)
	if strings.Contains(html, "mug-1-1.webp") {
		t.Error("the cart line still loads the 1000 px image although a thumbnail exists")
	}
}
