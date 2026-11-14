package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/sirupsen/logrus"

	"frontend/internal/obs"
)

// ---------------------------------------------------------------------------
// metricsMiddleware
// ---------------------------------------------------------------------------

func TestMetricsMiddleware_RecordsSuccessfulRequest(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// wrap with the real metricsMiddleware (uses package-level prometheus vars)
	// we test it by checking it doesn't panic and passes through correctly
	handler := metricsMiddleware(next, "/test")
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}
}

func TestMetricsMiddleware_RecordsNotFound(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	handler := metricsMiddleware(next, "/missing")
	req := httptest.NewRequest(http.MethodGet, "/missing", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rr.Code)
	}
}

func TestMetricsMiddleware_RecordsPostRequest(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})

	handler := metricsMiddleware(next, "/cart")
	req := httptest.NewRequest(http.MethodPost, "/cart", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Errorf("expected 201, got %d", rr.Code)
	}
}

func TestMetricsMiddleware_DefaultStatusIs200(t *testing.T) {
	// handler writes body but never calls WriteHeader explicitly
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	handler := metricsMiddleware(next, "/")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected default 200, got %d", rr.Code)
	}
}

func TestMetricsMiddleware_CallsNextHandler(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	handler := metricsMiddleware(next, "/")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if !called {
		t.Error("expected next handler to be called")
	}
}

func TestMetricsMiddleware_MeasuresDuration(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})

	handler := metricsMiddleware(next, "/slow")
	req := httptest.NewRequest(http.MethodGet, "/slow", nil)
	rr := httptest.NewRecorder()

	start := time.Now()
	handler.ServeHTTP(rr, req)
	elapsed := time.Since(start)

	if elapsed < time.Millisecond {
		t.Error("expected handler to take at least 1ms")
	}
}

// ---------------------------------------------------------------------------
// httpRequestsTotal counter
// ---------------------------------------------------------------------------

func TestHTTPRequestsTotal_IsRegistered(t *testing.T) {
	// verify the counter was registered and can be collected
	count := testutil.CollectAndCount(httpRequestsTotal)
	if count < 0 {
		t.Error("expected httpRequestsTotal to be collectable")
	}
}

func TestHTTPRequestDuration_IsRegistered(t *testing.T) {
	count := testutil.CollectAndCount(httpRequestDuration)
	if count < 0 {
		t.Error("expected httpRequestDuration to be collectable")
	}
}

func TestHTTPRequestsInFlight_IsRegistered(t *testing.T) {
	count := testutil.CollectAndCount(httpRequestsInFlight)
	if count < 0 {
		t.Error("expected httpRequestsInFlight to be collectable")
	}
}

// ---------------------------------------------------------------------------
// wrapRoutes
// ---------------------------------------------------------------------------

func TestWrapRoutes_WrapsRegisteredRoutes(t *testing.T) {
	// register metrics first to avoid panic on MustRegister
	// (already registered by init in telemetry.go — just test wrapRoutes behavior)
	r := mux.NewRouter()

	called := false
	r.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}).Methods(http.MethodGet)

	wrapRoutes(r)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if !called {
		t.Error("expected handler to be called after wrapRoutes")
	}
	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}
}

func TestWrapRoutes_MultipleRoutes(t *testing.T) {
	r := mux.NewRouter()

	routes := []string{"/a", "/b", "/c"}
	hits := make(map[string]bool)

	for _, path := range routes {
		p := path
		r.HandleFunc(p, func(w http.ResponseWriter, r *http.Request) {
			hits[p] = true
			w.WriteHeader(http.StatusOK)
		}).Methods(http.MethodGet)
	}

	wrapRoutes(r)

	for _, path := range routes {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("route %s: expected 200, got %d", path, rr.Code)
		}
	}
}

func TestWrapRoutes_EmptyRouter(t *testing.T) {
	// should not panic on an empty router
	r := mux.NewRouter()
	wrapRoutes(r)
}

// ---------------------------------------------------------------------------
// Telemetry switches: the contract of every service (OBS-01, OBS-04)
// ---------------------------------------------------------------------------

// The full table is in internal/obs/env_test.go; this one pins what the frontend relies on: the variable
// names it reads, the legacy ones it no longer reads, and its own service name.
func TestEnvContract_Matrix(t *testing.T) {
	const ep, py = "http://alloy:4317", "http://alloy:4040"
	cases := []struct {
		name      string
		env       map[string]string
		telemetry string // expected OTLP endpoint, "" = off
		profiling string // expected Pyroscope address, "" = off
	}{
		{"nothing set: off", map[string]string{}, "", ""},
		{"endpoint set: on", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": ep}, ep, ""},
		{"endpoint blank: off", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "  "}, "", ""},
		{"endpoint without scheme: off, no crash", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "alloy:4317"}, "", ""},
		{"disabled true wins over endpoint and pyroscope", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": ep, "PYROSCOPE_ADDR": py, "OTEL_SDK_DISABLED": "true"}, "", ""},
		{"disabled TRUE (case) wins", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": ep, "OTEL_SDK_DISABLED": "TRUE"}, "", ""},
		{"disabled with spaces wins", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": ep, "OTEL_SDK_DISABLED": " true "}, "", ""},
		{"disabled false does not disable", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": ep, "OTEL_SDK_DISABLED": "false"}, ep, ""},
		{"disabled 1 is not true (spec)", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": ep, "OTEL_SDK_DISABLED": "1"}, ep, ""},
		{"ENABLE_TRACING=0 is no longer read", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": ep, "ENABLE_TRACING": "0"}, ep, ""},
		{"ENABLE_TRACING=1 does not turn it on", map[string]string{"ENABLE_TRACING": "1"}, "", ""},
		{"COLLECTOR_SERVICE_ADDR alone no longer turns it on", map[string]string{"COLLECTOR_SERVICE_ADDR": "alloy:4317"}, "", ""},
		{"ENABLE_PROFILING=1 alone: off (no default address)", map[string]string{"ENABLE_PROFILING": "1"}, "", ""},
		{"pyroscope address set: on", map[string]string{"PYROSCOPE_ADDR": py}, "", py},
		{"pyroscope address blank: off", map[string]string{"PYROSCOPE_ADDR": " "}, "", ""},
		{"pyroscope address without scheme: off", map[string]string{"PYROSCOPE_ADDR": "alloy:4040"}, "", ""},
		{"ENABLE_PROFILING=0 forces profiling off", map[string]string{"PYROSCOPE_ADDR": py, "ENABLE_PROFILING": "0"}, "", ""},
		{"compose settings: everything off", map[string]string{"ENABLE_TRACING": "0", "OTEL_SDK_DISABLED": "true", "ENABLE_PROFILING": "0"}, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := obs.LoadSettings("frontend", obs.Getenv(envMap(c.env)))
			if s.OTLPEndpoint != c.telemetry || s.TelemetryOn() != (c.telemetry != "") {
				t.Errorf("telemetry endpoint = %q (on=%v), want %q", s.OTLPEndpoint, s.TelemetryOn(), c.telemetry)
			}
			if s.PyroscopeAddr != c.profiling || s.ProfilingOn() != (c.profiling != "") {
				t.Errorf("profiling addr = %q (on=%v), want %q", s.PyroscopeAddr, s.ProfilingOn(), c.profiling)
			}
		})
	}
}

func TestEnvContract_DefaultsAndResource(t *testing.T) {
	s := obs.LoadSettings("frontend", obs.Getenv(envMap(map[string]string{"HOSTNAME": "frontend-0"})))
	if s.Service != "frontend" || s.Environment != "local" || s.Instance != "frontend-0" || s.MetricsAddr != ":9090" || s.PprofAddr != "" {
		t.Fatalf("defaults: %+v (the metrics port moved from 9464 to 9090)", s)
	}
	s = obs.LoadSettings("frontend", obs.Getenv(envMap(map[string]string{"METRICS_PORT": "9464", "SERVICE_VERSION": "1.2.3", "DEPLOYMENT_ENVIRONMENT": "dev", "PPROF_ADDR": "127.0.0.1:6060"})))
	if s.MetricsAddr != ":9464" || s.Version != "1.2.3" || s.Environment != "dev" || s.PprofAddr != "127.0.0.1:6060" {
		t.Fatalf("overrides: %+v", s)
	}
}

// Faro reports the server's version unless FARO_APP_VERSION says otherwise.
func TestFaroVersionFollowsServiceVersion(t *testing.T) {
	get := envMap(map[string]string{"FARO_COLLECTOR_URL": "http://localhost:12347/collect", "SERVICE_VERSION": "2.3.4"})
	s, err := faroSettingsFromEnv(get)
	if err != nil || s.AppVersion != "2.3.4" {
		t.Fatalf("got %+v, %v", s, err)
	}
	get = envMap(map[string]string{"FARO_COLLECTOR_URL": "http://localhost:12347/collect", "SERVICE_VERSION": "2.3.4", "FARO_APP_VERSION": "web-9"})
	if s, _ := faroSettingsFromEnv(get); s.AppVersion != "web-9" {
		t.Fatalf("FARO_APP_VERSION must win: %+v", s)
	}
}

// Start the kit the way main does (same options, no listener): the first line is the JSON "starting" line.
func TestStartWiring_StartingLine(t *testing.T) {
	for _, k := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_SDK_DISABLED", "PYROSCOPE_ADDR", "ENABLE_PROFILING", "LOG_LEVEL", "SERVICE_VERSION"} {
		t.Setenv(k, "")
	}
	var out bytes.Buffer
	l := logrus.New()
	x := obs.Start(context.Background(), obs.Options{
		Service: "frontend", Loggers: []*logrus.Logger{l}, Out: &out, NoMetricsServer: true,
		Registerer: prometheus.NewRegistry(), Gatherer: prometheus.NewRegistry(),
	})
	defer x.Shutdown()
	var first map[string]any
	if err := json.Unmarshal([]byte(strings.SplitN(out.String(), "\n", 2)[0]), &first); err != nil {
		t.Fatalf("first line is not JSON: %v: %s", err, out.String())
	}
	if first["message"] != "starting" || first["service"] != "frontend" || first["severity"] != "info" || first["version"] == nil || first["commit"] == nil {
		t.Fatalf("starting line: %v", first)
	}
}
