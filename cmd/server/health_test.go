package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"

	"frontend/internal/obs"
)

func TestLivenessAnswersOKOnBothPaths(t *testing.T) {
	for _, path := range []string{"/healthz", "/_healthz"} {
		rr := httptest.NewRecorder()
		livenessHandler(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusOK || rr.Body.String() != "ok" {
			t.Errorf("%s = %d %q, want 200 ok", path, rr.Code, rr.Body.String())
		}
		if got := rr.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s Cache-Control = %q, want no-store", path, got)
		}
	}
}

func TestProbePathsNeedNoSession(t *testing.T) {
	for _, p := range []string{"/healthz", "/_healthz", "/readyz", "/version", "/robots.txt", "/static/x.css"} {
		if !isStaticPath(p) {
			t.Errorf("isStaticPath(%q) = false: a probe must not set a cookie or call user-service", p)
		}
	}
	if isStaticPath("/cart") {
		t.Error("isStaticPath(/cart) = true")
	}
}

func TestReadyzReadyWhenBackendsAnswer(t *testing.T) {
	fe := newFakeFrontend(t, sampleCatalog())
	rr := httptest.NewRecorder()
	fe.readinessHandler(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rr.Code != http.StatusOK || rr.Body.String() != "ok" {
		t.Fatalf("/readyz = %d %q, want 200 ok", rr.Code, rr.Body.String())
	}
}

func TestReadyzNotReadyWhenBackendsAreDown(t *testing.T) {
	// A dialer that always fails: the channel goes to TRANSIENT_FAILURE, nothing is listening.
	conn, err := grpc.NewClient("passthrough:///nobody",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return nil, errors.New("connection refused") }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	fe := &frontendServer{productCatalogSvcConn: conn, currencySvcConn: conn, cartSvcConn: conn}

	rr := httptest.NewRecorder()
	fe.readinessHandler(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz = %d, want 503", rr.Code)
	}
	if got := rr.Body.String(); got != "not ready: cart, currency, product-catalog\n" {
		t.Errorf("body = %q: the names of the backends in order, no error text", got)
	}
}

// fakeConn walks through a fixed list of states, one step per WaitForStateChange.
type fakeConn struct {
	states    []connectivity.State
	i         int
	connected bool
}

func (f *fakeConn) Connect()                     { f.connected = true }
func (f *fakeConn) GetState() connectivity.State { return f.states[f.i] }
func (f *fakeConn) WaitForStateChange(ctx context.Context, s connectivity.State) bool {
	if ctx.Err() != nil || f.i+1 >= len(f.states) {
		return false
	}
	f.i++
	return true
}

func TestConnReady(t *testing.T) {
	cases := []struct {
		name   string
		states []connectivity.State
		want   bool
	}{
		{"idle then ready", []connectivity.State{connectivity.Idle, connectivity.Connecting, connectivity.Ready}, true},
		{"already ready", []connectivity.State{connectivity.Ready}, true},
		{"transient failure", []connectivity.State{connectivity.Connecting, connectivity.TransientFailure}, false},
		{"shutdown", []connectivity.State{connectivity.Shutdown}, false},
		{"stays connecting until the deadline", []connectivity.State{connectivity.Connecting}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeConn{states: c.states}
			if got := connReady(context.Background(), f); got != c.want {
				t.Errorf("connReady = %v, want %v", got, c.want)
			}
			if !f.connected {
				t.Error("Connect was not called: an idle channel would never leave IDLE")
			}
		})
	}
}

func TestNotReadyDeadlineCountsAsNotReady(t *testing.T) {
	// A cancelled parent context stands for the readyTimeout running out; no real waiting in the test.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := notReady(ctx, []namedConn{{"b", &fakeConn{states: []connectivity.State{connectivity.Connecting, connectivity.Ready}}}, {"a", &fakeConn{states: []connectivity.State{connectivity.Ready}}}})
	if len(got) != 1 || got[0] != "b" {
		t.Errorf("notReady = %v, want [b]", got)
	}
}

func TestVersionHandlerJSON(t *testing.T) {
	t.Setenv("SERVICE_VERSION", "1.2.3")
	old, oldC := obs.BuildTime, obs.Commit
	obs.BuildTime, obs.Commit = "2026-11-14T10:00:00Z", "abc1234"
	t.Cleanup(func() { obs.BuildTime, obs.Commit = old, oldC })

	rr := httptest.NewRecorder()
	versionHandler(rr, httptest.NewRequest(http.MethodGet, "/version", nil))
	if rr.Code != http.StatusOK || !strings.HasPrefix(rr.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("/version = %d %q", rr.Code, rr.Header().Get("Content-Type"))
	}
	var got map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %v: %s", err, rr.Body)
	}
	want := map[string]string{"service": "frontend", "version": "1.2.3", "commit": "abc1234", "build_time": "2026-11-14T10:00:00Z"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestBuildTimeFallbacks(t *testing.T) {
	old := obs.BuildTime
	t.Cleanup(func() { obs.BuildTime = old })
	vcs := func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.time", Value: "2026-01-01T00:00:00Z"}}}, true
	}
	none := func() (*debug.BuildInfo, bool) { return nil, false }

	obs.BuildTime = ""
	if got := buildTime(vcs); got != "2026-01-01T00:00:00Z" {
		t.Errorf("without ldflags = %q, want the vcs.time", got)
	}
	if got := buildTime(none); got != "unknown" {
		t.Errorf("without any data = %q, want unknown", got)
	}
	obs.BuildTime = " 2026-02-02T00:00:00Z "
	if got := buildTime(vcs); got != "2026-02-02T00:00:00Z" {
		t.Errorf("with ldflags = %q, want the ldflags value, trimmed", got)
	}
}

func probeServer(t *testing.T, h http.HandlerFunc) (port string) {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Port()
}

func TestHealthcheckSubcommand(t *testing.T) {
	old := baseUrl
	t.Cleanup(func() { baseUrl = old })

	baseUrl = ""
	var out bytes.Buffer
	port := probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		livenessHandler(w, r)
	})
	if got := healthcheck(port, &out); got != 0 {
		t.Errorf("healthcheck against a healthy server = %d, want 0 (%s)", got, out.String())
	}

	baseUrl = "/shop"
	port = probeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/shop/healthz" {
			http.NotFound(w, r)
			return
		}
		livenessHandler(w, r)
	})
	if got := healthcheck(port, &out); got != 0 {
		t.Errorf("healthcheck must follow BASE_URL, got %d", got)
	}

	baseUrl = ""
	port = probeServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	if got := healthcheck(port, &out); got != 1 {
		t.Errorf("healthcheck against a 500 = %d, want 1", got)
	}
}

func TestHealthcheckNothingListening(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	if got := healthcheck(strconv.Itoa(port), &bytes.Buffer{}); got != 1 {
		t.Errorf("healthcheck with no server = %d, want 1", got)
	}
}
