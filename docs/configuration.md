# Configuration

| Variable | Required | Default | Description |
|---|---|---|---|
| `PRODUCT_CATALOG_SERVICE_ADDR` | **Yes** | — | `host:port` of product-catalog (gRPC), e.g. `product-catalog-service:3550` |
| `CURRENCY_SERVICE_ADDR` | **Yes** | — | e.g. `currency-service:7000` |
| `CART_SERVICE_ADDR` | **Yes** | — | e.g. `cart-service:7070` |
| `RECOMMENDATION_SERVICE_ADDR` | **Yes** | — | e.g. `recommendation-service:8080` |
| `CHECKOUT_SERVICE_ADDR` | **Yes** | — | e.g. `checkout-service:5050` |
| `SHIPPING_SERVICE_ADDR` | **Yes** | — | e.g. `shipping-service:50051` |
| `AD_SERVICE_ADDR` | **Yes** | — | e.g. `ad-service:9555` |
| `ORDER_SERVICE_ADDR` | No | `order-service:8085` | `host:port` of order-service (gRPC). Optional: lazy connection; while it is unreachable only `/orders` and `/orders/{id}` answer `503`. Set to an empty string to switch order history and tracking off (routes `404`, no Orders link, no "Track your order" button) |
| `USER_SERVICE_ADDR` | No | `user-service:8086` | `host:port` of user-service (gRPC). Optional: lazy connection; while it is unreachable the shop works as for an anonymous visitor and only the account pages, the orders pages and checkout of a signed-in visitor answer `503`. Set to an empty string to switch accounts off (routes `404`, no Sign in link). See the README section "Accounts" |
| `REVIEW_SERVICE_ADDR` | No | `review-service:8084` | `host:port` of review-service (gRPC). Optional: if it is unreachable the pages render without stars and without reviews (see "Ratings and reviews"). Set to an empty string to switch ratings and reviews off completely |
| `PORT` | No | `8080` | HTTP listen port |
| `LISTEN_ADDR` | No | `` (all interfaces) | Bind address; leave empty in containers |
| `METRICS_PORT` | No | `9464` | Prometheus listener port |
| `BASE_URL` | No | `` | URL path prefix when served under a sub-path (e.g. `/shop`); all routes and static URLs move under it |
| `BRAND_NAME` | No | `Everly` | Store name shown in the header, footer, page title and home hero (working name; the one place to rename the store) |
| `BRAND_TAGLINE` | No | `Everyday things, thoughtfully chosen.` | Tagline shown in the page title, the home hero headline and the footer |
| `FRONTEND_MESSAGE` | No | — | Optional announcement bar text shown above the header on every page |
| `ENABLE_SINGLE_SHARED_SESSION` | No | `false` | `true` makes every new visitor share one hard-coded session id (cart shared by everyone). Demo/test workaround only |
| `TEMPLATE_PATH` | No | `templates/*.html` | Glob of HTML templates (relative to `/app`) |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | No | unset (telemetry off) | OTLP/**gRPC** endpoint for **traces, metrics and logs** (scheme required), e.g. `http://alloy.monitoring.svc.cluster.local:4317`. Legacy fallback: `COLLECTOR_SERVICE_ADDR` (`host:port`). Telemetry is on iff an endpoint is set |
| `ENABLE_TRACING` | No | — | Legacy switch: `0` forces all telemetry off even if an endpoint is set (it is no longer needed to turn telemetry on) |
| `OTEL_METRIC_EXPORT_INTERVAL` | No | `15000` | Milliseconds between metric pushes (Go also accepts durations such as `15s`) |
| `OTEL_SERVICE_NAME` | No | `frontend` | `service.name` — becomes the Prometheus `job` and the Loki `service_name`; keep the default, dashboards depend on it |
| `PYROSCOPE_ADDR` | No | `http://pyroscope:4040` | Profiles use the Pyroscope SDK push protocol (**not** OTLP). Point it at Alloy (`http://alloy.monitoring.svc.cluster.local:4040`), which forwards to Pyroscope, or directly at Pyroscope |
| `ENABLE_PROFILING` | No | off | `1` starts the Pyroscope SDK |

Notes:

- All seven `*_ADDR` variables are **mandatory**: a missing one makes the process `panic` at startup. They are plain `host:port` (no scheme). Use FQDNs (`svc.<namespace>.svc.cluster.local:<port>`) if services live in other namespaces.
- gRPC connections use `grpc.NewClient` (lazy): the pod starts and `/_healthz` returns `ok` even if every backend is down. Page loads then fail with a 503 or 500 error page until backends return (each backend call times out after at most 3 s).
- The earlier README listed `ENV_PLATFORM`; it is **not read by the code**.
- Log level is hard-coded to `debug` (JSON via `logrus`) — expect chatty logs; there is no env switch.
- Telemetry is **pushed**, not scraped: traces, metrics and logs share one OTLP/gRPC endpoint (Alloy); profiles go to Pyroscope (via Alloy). `service.instance.id` is the pod hostname, so replicas stay distinct in Mimir.
- A Prometheus `/metrics` endpoint is still served on `METRICS_PORT` for debugging, but nothing has to scrape it — the same registry is bridged into OTLP, so metric names are unchanged.
- Logs: the `logrus` logger keeps writing JSON to stdout and additionally mirrors every record over OTLP (with trace ids when a context is attached).
