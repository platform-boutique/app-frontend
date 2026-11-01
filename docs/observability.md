# Observability

| Signal | Implementation | Destination |
|---|---|---|
| **Traces** | OpenTelemetry: `otelhttp` on the HTTP handler (inbound), `otelgrpc` client handler on every backend connection (outbound), OTLP/gRPC exporter, `AlwaysSample`, W3C TraceContext + Baggage. The trace starts here and propagates through every downstream service | Alloy `:4317` → Tempo |
| **Metrics** | `prometheus/client_golang` registry — `http_requests_total`, `http_request_duration_seconds` (buckets 1 ms … 10 s), `http_requests_in_flight`, labelled `method`, `route`, `status_code`, plus Go runtime/process collectors — bridged into OpenTelemetry and pushed over OTLP/gRPC every 15 s. `/metrics` is still served on `METRICS_PORT` for debugging | Alloy → Mimir |
| **Logs** | `logrus` JSON to stdout at `debug` level **and** an OTLP/gRPC hook (`otellogrus`): request path/method/id/session, response status/bytes/duration | Alloy → Loki |
| **Profiles** | Pyroscope Go SDK (CPU, alloc/inuse objects & space) when enabled | Alloy `:4040` → Pyroscope |
| **Browser (RUM)** | Grafana Faro Web SDK + Web Tracing in the page, only when `FARO_COLLECTOR_URL` is set: web vitals, errors, page views, fetch/XHR (htmx) traces linked to the server span of the page | Alloy `faro.receiver` → Loki / Tempo. See README "RUM (Grafana Faro)" |

Every series carries `job="frontend"` (from `service.name`) and `instance=<pod name>` (`service.instance.id`), which is what the Grafana dashboards select on. Logs land in Loki with `service_name="frontend"` and trace/span ids, so a log line links to its trace.
