package main

import (
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/trace"

	"frontend/internal/obs"
)

// Grafana Faro (browser real-user monitoring), see README "RUM (Grafana Faro)".
//
// Everything here is switched on by FARO_COLLECTOR_URL. Empty means off: the
// pages carry no Faro script, no config block and no traceparent meta tag.

const (
	faroDefaultAppName = "everly-frontend"
	faroDefaultEnv     = "local"
	faroMaxFieldLen    = 128
)

// faro is the process configuration, nil when Faro is off (set once in main).
var faro *faroSettings

// faroSettings is the validated process configuration (nil when Faro is off).
type faroSettings struct {
	CollectorURL string
	AppName      string
	AppVersion   string
	Env          string
	SampleRate   float64
}

// faroSettingsFromEnv reads and validates the FARO_* variables. getenv is
// os.Getenv in main and a fake in the tests. A nil result with a nil error
// means Faro is off.
func faroSettingsFromEnv(getenv func(string) string) (*faroSettings, error) {
	raw := strings.TrimSpace(getenv("FARO_COLLECTOR_URL"))
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("FARO_COLLECTOR_URL %q is not a valid URL: %v", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("FARO_COLLECTOR_URL %q must be an http:// or https:// URL (scheme %q is not allowed)", raw, u.Scheme)
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("FARO_COLLECTOR_URL %q has no host", raw)
	}
	if u.User != nil {
		// the value is sent to every browser: no credentials in it
		return nil, fmt.Errorf("FARO_COLLECTOR_URL must not contain credentials (user:password@)")
	}

	// the browser app reports the same version as the server: SERVICE_VERSION, else build info, else "dev"
	version, _ := obs.ResolveBuild(getenv)
	s := &faroSettings{
		CollectorURL: u.String(),
		AppName:      faroDefaultAppName,
		AppVersion:   version,
		Env:          faroDefaultEnv,
		SampleRate:   1,
	}
	for _, f := range []struct {
		env string
		dst *string
	}{
		{"FARO_APP_NAME", &s.AppName},
		{"FARO_APP_VERSION", &s.AppVersion},
		{"FARO_ENV", &s.Env},
	} {
		if v := strings.TrimSpace(getenv(f.env)); v != "" {
			if len(v) > faroMaxFieldLen {
				return nil, fmt.Errorf("%s is longer than %d characters", f.env, faroMaxFieldLen)
			}
			*f.dst = v
		}
	}
	if v := strings.TrimSpace(getenv("FARO_SESSION_SAMPLE_RATE")); v != "" {
		rate, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(rate) || rate < 0 || rate > 1 {
			return nil, fmt.Errorf("FARO_SESSION_SAMPLE_RATE %q must be a number from 0 to 1", v)
		}
		s.SampleRate = rate
	}
	return s, nil
}

// faroPageConfig is what faro-init.js reads from <script type="application/json"
// id="faro-config">. html/template encodes it as JSON for a script context, so
// "<", ">" and "&" cannot end the block whatever the values are.
type faroPageConfig struct {
	CollectorURL      string  `json:"collectorUrl"`
	AppName           string  `json:"appName"`
	AppVersion        string  `json:"appVersion"`
	Environment       string  `json:"environment"`
	SessionSampleRate float64 `json:"sessionSampleRate"`
	ShopSessionID     string  `json:"shopSessionId"`
	ShopUserSignedIn  bool    `json:"shopUserSignedIn"`
}

// faroTemplateData adds the Faro keys to the common template data. Nothing is
// added when Faro is off, so the templates render exactly as before.
func faroTemplateData(r *http.Request, data map[string]interface{}) {
	if faro == nil {
		return
	}
	data["faro"] = faroPageConfig{
		CollectorURL:      faro.CollectorURL,
		AppName:           faro.AppName,
		AppVersion:        faro.AppVersion,
		Environment:       faro.Env,
		SessionSampleRate: faro.SampleRate,
		ShopSessionID:     sessionID(r),
		ShopUserSignedIn:  userOf(r) != nil,
	}
	if tp := traceparentOf(r); tp != "" {
		data["faro_traceparent"] = tp
	}
}

// traceparentOf formats the span of the CURRENT request (the otelhttp server
// span, which is in the request context while a template renders) as a W3C
// traceparent value: 00-<32 hex trace id>-<16 hex span id>-<2 hex flags>.
// Empty when there is no valid, sampled span created by this process, e.g.
// telemetry is off (the no-op tracer only passes an incoming remote context on).
func traceparentOf(r *http.Request) string {
	sc := trace.SpanContextFromContext(r.Context())
	if !sc.IsValid() || !sc.IsSampled() || sc.IsRemote() {
		return ""
	}
	return "00-" + sc.TraceID().String() + "-" + sc.SpanID().String() + "-" + sc.TraceFlags().String()
}
