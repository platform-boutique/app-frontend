package main

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
)

// ---------------------------------------------------------------------------
// Prometheus HTTP metrics
// ---------------------------------------------------------------------------

var (
	httpRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total number of HTTP requests",
		},
		[]string{"method", "route", "status_code"},
	)

	httpRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "Duration of HTTP requests in seconds",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		},
		[]string{"method", "route", "status_code"},
	)

	httpRequestsInFlight = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "http_requests_in_flight",
			Help: "Number of HTTP requests currently being processed",
		},
	)
)

func initMetrics() {
	prometheus.MustRegister(httpRequestsTotal, httpRequestDuration, httpRequestsInFlight, currencyCacheTotal)
}

// metricsMiddleware wraps each route and records Prometheus HTTP metrics.
func metricsMiddleware(next http.Handler, route string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rr := &responseRecorder{w: w}
		httpRequestsInFlight.Inc()
		defer func() {
			httpRequestsInFlight.Dec()
			status := strconv.Itoa(rr.status)
			if rr.status == 0 {
				status = "200"
			}
			elapsed := time.Since(start).Seconds()
			httpRequestsTotal.WithLabelValues(r.Method, route, status).Inc()
			httpRequestDuration.WithLabelValues(r.Method, route, status).Observe(elapsed)
		}()
		next.ServeHTTP(rr, r)
	})
}

// wrapRoutes wraps all named routes in the router with metricsMiddleware.
func wrapRoutes(r *mux.Router) {
	_ = r.Walk(func(route *mux.Route, router *mux.Router, ancestors []*mux.Route) error {
		path, err := route.GetPathTemplate()
		if err != nil {
			return nil
		}
		handler := route.GetHandler()
		if handler != nil {
			route.Handler(metricsMiddleware(handler, path))
		}
		return nil
	})
}
