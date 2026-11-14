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
| `INVENTORY_SERVICE_ADDR` | No | `inventory-service:8087` | `host:port` of inventory-service (gRPC). Optional: lazy connection; the live stock is overlaid on the catalogue's `stock` with one `GetStock` per page (1 s deadline). Unreachable, slow or unknown product: the catalogue's stock is used and a warning is logged. Set to an empty string to switch live stock off. See the README section "Live stock" |
| `EMAIL_SERVICE_ADDR` | No | `email-service:8080` | `host:port` of email-service (gRPC, `hipstershop.mailbox.MailboxService`, same port as its `EmailService`). Optional: lazy connection; the mailbox pages (`/emails`, `/emails/{id}`) and the "View confirmation email" button depend on it. While it is unreachable only the two mailbox routes answer `503`. Set to an empty string to switch the mailbox off (routes `404`, no "Your emails" links, no button). The mailbox also needs `ORDER_SERVICE_ADDR` (order-service says whose mail is whose); with that empty the mailbox is off too |
| `REVIEW_SERVICE_ADDR` | No | `review-service:8084` | `host:port` of review-service (gRPC). Optional: if it is unreachable the pages render without stars and without reviews (see "Ratings and reviews"). Set to an empty string to switch ratings and reviews off completely |
| `PORT` | No | `8080` | HTTP listen port |
| `LISTEN_ADDR` | No | `` (all interfaces) | Bind address; leave empty in containers |
| `BASE_URL` | No | `` | URL path prefix when served under a sub-path (e.g. `/shop`); all routes and static URLs move under it |
| `BRAND_NAME` | No | `Everly` | Store name shown in the header, footer, page title and home hero (working name; the one place to rename the store) |
| `BRAND_TAGLINE` | No | `Everyday things, thoughtfully chosen.` | Tagline shown in the page title, the home hero headline and the footer |
| `FRONTEND_MESSAGE` | No | — | Optional announcement bar text shown above the header on every page |
| `ENABLE_SINGLE_SHARED_SESSION` | No | `false` | `true` makes every new visitor share one hard-coded session id (cart shared by everyone). Demo/test workaround only |
| `TEMPLATE_PATH` | No | `templates/*.html` | Glob of HTML templates (relative to `/app`) |
| `METRICS_PORT` | No | `9090` | Port of the Prometheus `/metrics` endpoint (scraped by Alloy). A value that is not a port falls back to `9090` with a warning |
| `PPROF_ADDR` | No | unset (no pprof) | `host:port` of a separate pprof listener, e.g. `127.0.0.1:6060`. Off unless set; never expose it. pprof is never served on the metrics port |
| `LOG_LEVEL` | No | `info` | `debug`, `info`, `warn` or `error`; anything else falls back to `info` with a warning |
| `SERVICE_VERSION` | No | build info, else `dev` | `service.version`, the `version` log field and the `build_info` label. Fallbacks in order: ldflags `-X <module>/internal/obs.Version=...` (and `.Commit=`, `.BuildTime=`), the VCS revision Go embeds in the binary (12 characters, `-dirty` if modified), `dev` |
| `DEPLOYMENT_ENVIRONMENT` | No | `local` | `deployment.environment` on every signal and the Pyroscope tag `environment` |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | No | unset (telemetry off) | OTLP/**gRPC** endpoint for **traces, metrics and logs**, e.g. `http://alloy:4317`. Telemetry is on only if this is a valid absolute http(s) URL; empty or malformed means off (one warning, never a crash). The legacy `COLLECTOR_SERVICE_ADDR` and `ENABLE_TRACING` are no longer read |
| `OTEL_SDK_DISABLED` | No | unset | `true` (trimmed, case-insensitive) forces everything off: OTLP export and profiling, even if an endpoint or `PYROSCOPE_ADDR` is set |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | No | `grpc` | Documented knob; only gRPC is implemented, the value is not read |
| `OTEL_TRACES_SAMPLER`, `OTEL_TRACES_SAMPLER_ARG` | No | SDK default (`parentbased_always_on`) | Standard OpenTelemetry sampler variables, read by the SDK (tested for `always_on` and `always_off`) |
| `OTEL_RESOURCE_ATTRIBUTES` | No | unset | Extra resource attributes (`k=v,...`); applied after the defaults, so they can override them |
| `OTEL_SERVICE_NAME` | No | `frontend` | `service.name`; keep the default, dashboards depend on it |
| `OTEL_METRIC_EXPORT_INTERVAL` | No | `15000` | Milliseconds between metric pushes (also accepts durations such as `15s`) |
| `PYROSCOPE_ADDR` | No | unset (profiling off) | Pyroscope URL (SDK push protocol, not OTLP). The only switch: no default address, `ENABLE_PROFILING=1` alone starts nothing |
| `ENABLE_PROFILING` | No | unset | `0` forces profiling off even with `PYROSCOPE_ADDR`; any other value changes nothing |
| `FARO_COLLECTOR_URL` | No | empty (Faro off) | Grafana Faro receiver URL **as the visitor's browser reaches it**, e.g. `https://faro.example.com/collect` (Alloy `faro.receiver`). Only `http://` or `https://`, a host, no credentials; anything else stops the process at start with a message. Empty: no Faro script, config block or `traceparent` meta tag is rendered. See the README section "RUM (Grafana Faro)" |
| `FARO_APP_NAME` | No | `everly-frontend` | Faro `app.name` (at most 128 characters) |
| `FARO_APP_VERSION` | No | `SERVICE_VERSION`, else build info, else `dev` | Faro `app.version` (the same version the server reports) |
| `FARO_ENV` | No | `local` | Faro `app.environment` |
| `FARO_SESSION_SAMPLE_RATE` | No | `1` | Share of browser sessions that report, a number from 0 to 1 (checked at start) |

Notes:

- All seven `*_ADDR` variables are **mandatory**: a missing one stops the process at startup with one JSON error line (`invalid configuration`) and exit code 1. They are plain `host:port` (no scheme). Use FQDNs (`svc.<namespace>.svc.cluster.local:<port>`) if services live in other namespaces.
- gRPC connections use `grpc.NewClient` (lazy): the pod starts and `/healthz` returns `ok` even if every backend is down (`/readyz` answers `503` while the catalogue, currency or cart service is unreachable). Page loads then fail with a 503 or 500 error page until backends return (each backend call times out after at most 3 s).
- The earlier README listed `ENV_PLATFORM`; it is **not read by the code**.
- Log level comes from `LOG_LEVEL` (default `info`). It used to be hard-coded to `debug`: the per-request `request started` line and the handlers' debug lines are now only visible with `LOG_LEVEL=debug`; the per-request access line `request complete` stays at `info`.
- Traces, metrics and logs share one OTLP/gRPC endpoint (Alloy); profiles go to Pyroscope (via Alloy). Metrics are also scraped from `/metrics` on `METRICS_PORT`. `service.instance.id` is the pod hostname, so replicas stay distinct in Mimir.
- `/metrics` is served on `METRICS_PORT` (default `9090`, was `9464`; the metric names did not change). The same registry is bridged into OTLP, so a scrape and a push carry the same names; the local Alloy scrapes `:9090`.
- Logs: the `logrus` logger writes the shared JSON shape to stdout and mirrors every record over OTLP; see Observability for the fields.
