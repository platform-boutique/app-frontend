package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
)

// Content-Security-Policy (SEC-05), see README "Content-Security-Policy".
//
// The default is REPORT-ONLY: the browser reports what the policy would have blocked and blocks nothing.
// CSP_MODE=enforce turns it into a blocking policy once the reports of a real deployment are clean,
// CSP_MODE=off sends no header. Reports go to POST /csp-report on this service (counted in
// frontend_csp_reports_total, a rate-limited log line each), so no extra service is needed.
//
// The policy is as tight as the pages allow: no inline scripts and no inline event handlers (the
// theme and currency scripts are files), no inline <style> element (htmx's indicator style is turned
// off), style attributes are the one exception (the rating bars and stars set a width) and are
// allowed through style-src-attr only. The Faro collector (FARO_COLLECTOR_URL), when set, is the one
// extra connect-src origin, so browser telemetry keeps working.

type cspMode string

const (
	cspReportOnly cspMode = "report-only"
	cspEnforce    cspMode = "enforce"
	cspOff        cspMode = "off"

	cspReportPath    = "/csp-report"
	cspMaxReportSize = 16 << 10
)

// cspSetting is the process configuration (set once in main); report-only unless CSP_MODE says otherwise.
var cspSetting = cspReportOnly

// cspModeFromEnv reads CSP_MODE. Empty means report-only; an unknown value is an error (a typo must not
// silently switch the policy off).
func cspModeFromEnv(getenv func(string) string) (cspMode, error) {
	switch v := strings.ToLower(strings.TrimSpace(getenv("CSP_MODE"))); v {
	case "", string(cspReportOnly):
		return cspReportOnly, nil
	case string(cspEnforce):
		return cspEnforce, nil
	case string(cspOff):
		return cspOff, nil
	default:
		return "", fmt.Errorf("CSP_MODE %q must be report-only, enforce or off", v)
	}
}

// cspPolicy is the policy text for the current configuration.
func cspPolicy() string {
	connect := "'self'"
	if faro != nil {
		if u, err := url.Parse(faro.CollectorURL); err == nil && u.Host != "" {
			connect += " " + u.Scheme + "://" + u.Host // the origin the browser posts Faro payloads to
		}
	}
	return strings.Join([]string{
		"default-src 'self'",
		"script-src 'self'",
		"style-src 'self'",
		"style-src-attr 'unsafe-inline'",
		"img-src 'self' data:",
		"font-src 'self'",
		"connect-src " + connect,
		"object-src 'none'",
		"base-uri 'self'",
		"form-action 'self'",
		"frame-ancestors 'none'",
		"report-uri " + baseUrl + cspReportPath,
	}, "; ")
}

// withCSP sets the policy header on every response that is a page (assets and probes are not documents).
func withCSP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cspSetting != cspOff && !isStaticPath(r.URL.Path) {
			h := w.Header()
			name := "Content-Security-Policy-Report-Only"
			if cspSetting == cspEnforce {
				name = "Content-Security-Policy"
			}
			h.Set(name, cspPolicy())
		}
		next.ServeHTTP(w, r)
	})
}

var cspReportsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "frontend_csp_reports_total",
		Help: "Content-Security-Policy violation reports received from browsers, by violated directive (other = not a known directive name)",
	},
	[]string{"directive"},
)

var cspDirectives = map[string]bool{
	"default-src": true, "script-src": true, "script-src-elem": true, "script-src-attr": true,
	"style-src": true, "style-src-elem": true, "style-src-attr": true, "img-src": true, "font-src": true,
	"connect-src": true, "object-src": true, "base-uri": true, "form-action": true, "frame-ancestors": true,
	"frame-src": true, "media-src": true, "worker-src": true, "manifest-src": true,
}

const (
	cspLogWindow = 10 * time.Second
	cspLogBudget = 10 // log lines per window; the rest is only counted
)

// cspReporter receives the reports. Every report is counted; a log line is written for at most
// cspLogBudget of them per window, so nobody can flood the log through this public endpoint.
type cspReporter struct {
	now func() time.Time // nil = time.Now (tests inject a clock)

	mu     sync.Mutex
	window time.Time
	logged int
}

func (c *cspReporter) allowLog() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if c.now != nil {
		now = c.now()
	}
	if now.Sub(c.window) >= cspLogWindow {
		c.window, c.logged = now, 0
	}
	if c.logged >= cspLogBudget {
		return false
	}
	c.logged++
	return true
}

// cspViolation is what is kept of a report: no cookie, no sample of the blocked code, no query string.
type cspViolation struct {
	Directive   string
	Blocked     string
	Document    string
	Source      string
	Line        int
	Disposition string
}

// parseCSPReports reads both report formats: the `application/csp-report` object ({"csp-report": {...}}) that
// report-uri delivers (what the policy uses: a browser run showed it arrives at once, while the Reporting API
// `report-to` batches and was not delivered in the headless Chromium of the check) and the Reporting API list
// (`application/reports+json`), accepted so that switching the policy to report-to needs no server change.
func parseCSPReports(body []byte) ([]cspViolation, error) {
	type legacy struct {
		Document    string `json:"document-uri"`
		Blocked     string `json:"blocked-uri"`
		Violated    string `json:"violated-directive"`
		Effective   string `json:"effective-directive"`
		Source      string `json:"source-file"`
		Line        int    `json:"line-number"`
		Disposition string `json:"disposition"`
	}
	type modern struct {
		Document    string `json:"documentURL"`
		Blocked     string `json:"blockedURL"`
		Effective   string `json:"effectiveDirective"`
		Source      string `json:"sourceFile"`
		Line        int    `json:"lineNumber"`
		Disposition string `json:"disposition"`
	}
	var one struct {
		Report *legacy `json:"csp-report"`
	}
	if err := json.Unmarshal(body, &one); err == nil && one.Report != nil {
		r := one.Report
		d := r.Effective
		if d == "" {
			d = r.Violated
		}
		return []cspViolation{{d, r.Blocked, r.Document, r.Source, r.Line, r.Disposition}}, nil
	}
	var list []struct {
		Type string `json:"type"`
		Body modern `json:"body"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("not a CSP report")
	}
	var out []cspViolation
	for _, e := range list {
		if e.Type != "csp-violation" {
			continue
		}
		b := e.Body
		out = append(out, cspViolation{b.Effective, b.Blocked, b.Document, b.Source, b.Line, b.Disposition})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no CSP violation in the report")
	}
	return out, nil
}

// cleanCSPURL keeps scheme, host and path of an URL (a query string can carry a token), or the
// keyword the browser reports ("inline", "eval", "data", "self"). Short and printable.
func cleanCSPURL(s string) string {
	if u, err := url.Parse(s); err == nil && u.Scheme != "" && u.Host != "" {
		s = u.Scheme + "://" + u.Host + u.Path
	} else if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	return cleanCSPText(s, 200)
}

func cleanCSPText(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if len(s) > max {
		s = s[:max]
	}
	return s
}

// normalizeDirective maps whatever the browser sent to a known directive name, or "other", so the
// metric label cannot be filled by a client.
func normalizeDirective(s string) string {
	name, _, _ := strings.Cut(strings.TrimSpace(strings.ToLower(s)), " ")
	if cspDirectives[name] {
		return name
	}
	return "other"
}

func (c *cspReporter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, cspMaxReportSize))
	if err != nil {
		http.Error(w, "report too large or unreadable", http.StatusRequestEntityTooLarge)
		return
	}
	reports, err := parseCSPReports(body)
	if err != nil {
		http.Error(w, "bad report", http.StatusBadRequest)
		return
	}
	if len(reports) > 20 {
		reports = reports[:20]
	}
	log, _ := r.Context().Value(ctxKeyLog{}).(logrus.FieldLogger)
	for _, v := range reports {
		dir := normalizeDirective(v.Directive)
		cspReportsTotal.WithLabelValues(dir).Inc()
		if log != nil && c.allowLog() {
			log.WithFields(logrus.Fields{
				"csp.directive":   dir,
				"csp.blocked":     cleanCSPURL(v.Blocked),
				"csp.document":    cleanCSPURL(v.Document),
				"csp.source":      cleanCSPURL(v.Source),
				"csp.line":        v.Line,
				"csp.disposition": cleanCSPText(v.Disposition, 16),
			}).Warn("content security policy violation reported by a browser")
		}
	}
	w.WriteHeader(http.StatusNoContent)
}
