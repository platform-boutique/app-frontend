# frontend

The HTTP web storefront for the platform-demo e-commerce platform. It serves server-rendered HTML pages and static assets, keeps a session cookie per visitor, and fans out to seven backend gRPC services (plus the optional `review-service`) to build every page. It is the **only** component users (and ingress) talk to directly; everything else is internal.

Part of the [platform-demo](https://github.com/platform-boutique) platform — microservices, observability (LGTM stack), GitOps (Argo CD), policy enforcement (Kyverno), infrastructure provisioning (Crossplane) and an internal developer portal (Backstage). See the master `README.md` in the platform-demo root for the whole-system view.

## At a glance

| | |
|---|---|
| **Language / runtime** | Go 1.26 (`gorilla/mux`, `html/template`), static binary |
| **Image** | `mladenovskistefan/frontend` |
| **Protocol / port** | HTTP/1.1 on `8080` |
| **Metrics** | Pushed over OTLP/gRPC (Prometheus registry bridged into OpenTelemetry); `/metrics` on `9464` is still served for debugging |
| **Health** | `GET /_healthz` → `200 ok` — static, no dependency checks |
| **State** | None server-side; session id lives in a browser cookie, carts live in `cart-service` |
| **Depends on (gRPC)** | `product-catalog-service`, `currency-service`, `cart-service`, `recommendation-service`, `checkout-service`, `shipping-service`, `ad-service`; optional: `review-service` (ratings and reviews, see below), `order-service` (order history and tracking, see "Orders") |
| **Called by** | Browsers / ingress / load generators |
| **Not used** | `payment-service` and `email-service` are only reached via `checkout-service`. `shopping-assistant-service` is **not** wired in |

## HTTP routes

All routes are prefixed with `BASE_URL` (empty by default).

| Route | Method | Description |
|---|---|---|
| `/` | GET, HEAD | Home and catalogue — hero and department tiles (landing only), search / sort toolbar, product grid (24 per page), pagination, currency picker, ad, cart size. Query parameters: `category` (department id), `q` (search text, 100 characters at most; invalid UTF-8 is dropped), `sort` (`name` default, `price_asc`, `price_desc`, `rating`), `page` (1-based). With `HX-Request` it returns only the `#catalog-results` fragment (plus a `<title>`); when the view would change the hero or heading (landing <-> search, category or page 2) it answers `HX-Redirect` instead |
| `/product/{id}` | GET, HEAD | Product page — product, gallery, price, stock, quantity select (1 to min(stock, 10)), recommendations, ad, rating summary, reviews and the review form. Unknown id: `404` page. `?rpage=N` selects the page of reviews; `?reviewed=1` shows the "review posted" message. With `HX-Request` and `rpage` it returns only the next `<li>` review items (htmx "load more") |
| `/product/{id}/reviews` | POST | Post a review (form fields `author_name`, `rating` 1-5, `comment`). Valid: `303` back to `/product/{id}?reviewed=1#reviews`. Invalid: the product page again with `422` and inline errors; review service not reachable: `503` with the form kept; reviews switched off or unknown product: `404`; `Sec-Fetch-Site: cross-site`: `403` |
| `/cart` | GET, HEAD | View cart — items, shipping estimate, total, recommendations. Lines whose product is no longer in the catalog (a cart outlives the catalog) are skipped, listed in a notice with an Empty cart button, logged once per request and not counted in the header |
| `/cart` | POST | Add item (form fields `product_id`, `quantity` 1-10). `422` with a readable message when the product is out of stock or quantity plus what is already in the cart exceeds the stock; unknown product: `404`. Stock is only checked here, orders do not decrement it yet |
| `/cart/empty` | POST | Empty the cart |
| `/orders` | GET, HEAD | Order history of the visitor's session: confirmed orders only (`ListOrders` with `placed_only`), newest first, 10 per page (`?page=`, a page past the end shows the last page). Each row: short id, date, status badge, item count, thumbnails, total, link. Friendly empty state. Order service down: `503` page "Orders are not available right now"; `ORDER_SERVICE_ADDR` empty: `404` |
| `/orders/{id}` | GET, HEAD | Tracking page of one order of the session (`GetOrder` with the session as guard): status badge, stage tracker, items, shipping address, tracking number, totals. An unknown, somebody else's or malformed id gives the same `404` page. With `HX-Request` it returns only the live part (badge, tracker, tracking number); `200` while the order is on its way, `286` once it is delivered or failed (htmx stops polling) |
| `/cart/checkout` | POST | Validate the form, call `checkout-service.PlaceOrder`, render the order page. The country is a dropdown (the names shipping-service accepts, United States first and preselected); the State field exists only for the United States: `static/js/checkout.js` hides and disables it for any other country, and the server ignores a state sent for another country, normalises `US`/`USA` to `United States` and requires a state for it |
| `/setCurrency` | POST | Set the display currency cookie (`currency_code`) |
| `/logout` | GET | Expire all cookies and redirect to `/` |
| `/product-meta/{ids}` | GET | JSON of a single product. Kept for compatibility: no page or script uses it any more. Any error answers an empty `200` |
| `/static/*` | GET | `styles/*.css`, `js/htmx.min.js` and `js/gallery.js`, `favicon.svg`, product images (`img/products/`) |
| `/_healthz` | GET | Liveness/readiness (`ok`) |
| `/robots.txt` | GET | `Disallow: /` |

**Supported currencies** (whitelist in code, intersected with what `currency-service` offers): `USD`, `EUR`, `CAD`, `JPY`, `GBP`, `TRY`. Default is `USD`.

### Which backend each page calls

| Page | Calls |
|---|---|
| Home | currency (list + convert per product), product-catalog (`ListCategories` + `ListProductsFiltered`, 24 per page), cart (get, then product-catalog `GetProduct` per cart line for the header count), ad (100 ms deadline, failures ignored), review (ONE `GetRatingSummary` with the ids of the page, 400 ms deadline, failures ignored) |
| Product | product-catalog (get), currency, cart, recommendation → product-catalog (up to 4), ad, review (`ListReviews`, 5 per page, 400 ms deadline, failures ignored) |
| Post review | product-catalog (get, the product must exist), review (`AddReview`, 2 s deadline) |
| Cart | cart (get), shipping (quote with nil address), currency, product-catalog, recommendation |
| Checkout | checkout (PlaceOrder), then recommendation + currency for the confirmation page |
| Orders / order tracking | order (`ListOrders` / `GetOrder`, 1.5 s deadline) and, for the header navigation only, the cached catalog category list |

### Errors

Error pages never show stack traces or backend error text; those go to the server log only (the page footer shows the request id to find the log line). The backend's gRPC status code decides the page, the services pass their codes through:

| Backend status | HTTP | Page |
|---|---|---|
| `InvalidArgument`, `FailedPrecondition` | `422` | the backend's message (the `rpc error: code = ... desc =` wrapper and a `card_declined: ` style reason prefix are removed), e.g. the payment decline reason |
| `NotFound` | `404` | "Not found" (unknown product id, unknown order) |
| `Unavailable`, `DeadlineExceeded` | `503` | "Temporarily unavailable" |
| anything else | `500` | generic text, no details |

Our own input validation (add to cart, checkout form) answers `422` with the validation text, `400`/`403` use fixed texts. Error pages keep the header with the department nav and a page-specific `<title>`. htmx requests that fail get a small message for the `#page-alert` region (`HX-Retarget`, `HX-Push-Url: false`) and the page is left as it is; htmx is configured (`htmx-config` meta tag) to swap 4xx/5xx answers.
Until checkout-service passes the payment status through, a decline arrives as `Internal: failed to charge card: ... rpc error: code = InvalidArgument|FailedPrecondition desc = <reason>: <text>`; for that one message shape the frontend extracts `<text>` and shows it as `422`. Any other `Internal` stays a generic `500`.

### Deadlines

Every backend call has a deadline so that a hung service ends in a `503` page instead of a dropped connection (the server's `WriteTimeout` is 30 s): 3 s for catalog, currency, cart, recommendation and shipping (`backendTimeout` in `cmd/server/rpc.go`), 15 s for `PlaceOrder`, 100 ms for ads, 400 ms / 2 s for review-service reads / writes. They are constants, not configuration.

### Sessions & cookies

| Cookie | Purpose | Flags |
|---|---|---|
| `shop_session-id` | random UUID per visitor; used as the cart `user_id` | `HttpOnly`, **`Secure`**, `SameSite=Strict`, 48 h |
| `shop_currency` | chosen display currency | `HttpOnly`, **`Secure`**, `SameSite=Strict`, 48 h |

> **Kubernetes gotcha — cookies are `Secure`.** Browsers only store `Secure` cookies over HTTPS (or `http://localhost`). If you reach the frontend over plain HTTP at a LoadBalancer/NodePort IP or hostname, the session cookie is dropped, every request becomes a new session, and the cart appears to be empty after "Add to cart". Use `kubectl port-forward` (→ `localhost`), or terminate TLS at the ingress/gateway, or set `ENABLE_SINGLE_SHARED_SESSION=true` as a demo workaround (see below).

## Configuration

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

- All seven `*_ADDR` variables except `REVIEW_SERVICE_ADDR` are **mandatory**: a missing one makes the process `panic` at startup. They are plain `host:port` (no scheme). Use FQDNs (`svc.<namespace>.svc.cluster.local:<port>`) if services live in other namespaces.
- gRPC connections use `grpc.NewClient` (lazy): the pod starts and `/_healthz` returns `ok` even if every backend is down. Page loads then fail with a 503 or 500 error page until backends return (each backend call times out after at most 3 s, see "Deadlines").
- The earlier README listed `ENV_PLATFORM`; it is **not read by the code**.
- Log level is hard-coded to `debug` (JSON via `logrus`) — expect chatty logs; there is no env switch.
- Telemetry is **pushed**, not scraped: traces, metrics and logs share one OTLP/gRPC endpoint (Alloy); profiles go to Pyroscope (via Alloy). `service.instance.id` is the pod hostname, so replicas stay distinct in Mimir.
- A Prometheus `/metrics` endpoint is still served on `METRICS_PORT` for debugging, but nothing has to scrape it — the same registry is bridged into OTLP, so metric names are unchanged.
- Logs: the `logrus` logger keeps writing JSON to stdout and additionally mirrors every record over OTLP (with trace ids when a context is attached).

## Look and feel

- **Brand:** one configuration point, `BRAND_NAME` / `BRAND_TAGLINE` (read once at startup in `cmd/server/brand.go`, passed to every template as `brand_name` / `brand_tagline`). The logo is an inline SVG mark (`templates/icons.html`, `static/favicon.svg`); no image files, fonts or CDNs are loaded from anywhere else.
- **Design system:** `static/styles/styles.css` defines CSS custom properties for colour, type scale, spacing, radius and shadows, then layout and components; `cart.css` and `order.css` hold page-specific rules. System font stacks only (serif headings, sans body).
- **Light / dark:** follows `prefers-color-scheme`; the header toggle stores an explicit choice in `localStorage` (`theme`), applied by a small inline script in `<head>` before the stylesheets so there is no flash of the wrong theme. Without JavaScript or storage the toggle is hidden or not remembered and the OS setting applies.
- **Responsive / accessible:** product grid 2 / 3 / 4 columns (phone / tablet / desktop), square photo frames (`aspect-ratio` + `object-fit`) so square webp and older non-square jpgs both fit; skip link, landmarks, visible focus rings, `prefers-reduced-motion` respected.
- **Catalogue UI:** `templates/catalog.html` holds the department nav (`nav_extra`, also on cart, order and error pages), department tiles, the search/sort toolbar (`catalog_toolbar`), the results area (`catalog_results`, the htmx fragment) and `catalog_pagination`; styles in `static/styles/browse.css`. Every control is a plain link or GET form; with htmx (`static/js/htmx.min.js`, version 2.0.11, vendored, sha256 `d6fdc75f204e6bdefa99b69bf1e6d4ac69b8a364f77929f45c13476b4000f717`; the audit of 2026-10-27 found it byte-identical to `https://unpkg.com/htmx.org@2.0.11/dist/htmx.min.js`) the toolbar and the pager replace `#catalog-results` (`hx-swap="outerHTML"`) and push the URL.
- **Page titles:** `<page> | brand` (product name, department, "Search results", "Cart", "Order complete", the error title); the landing page keeps `brand | tagline`.
- **Form controls:** the border of inputs, selects and secondary buttons (`--color-control-border`) has at least 3:1 contrast against page, surface and surface-2 in light and dark (computed in `cmd/server/css_test.go`).
- **Checked so far:** templates rendered with sample data by a throwaway preview server and screenshotted with headless Chromium (Playwright) at 360, 390, 430, 768 and 1280 px, light and dark, with no horizontal overflow; the theme toggle and reload persistence were checked the same way. Not yet checked against the running stack, and contrast ratios were calculated by hand, not measured with a tool.

## Orders

Order history and tracking come from `order-service` (proto copied to `proto/orders/orders.proto`, stubs in `genproto/orders/`: its own Go package `frontend/genproto/orders`, because `Money`, `Address` and `OrderItem` clash with `demo.proto`; only the `go_package` option differs from the service's file. Regenerate: `protoc -I proto/orders --go_out=genproto/orders --go_opt=paths=source_relative --go-grpc_out=genproto/orders --go-grpc_opt=paths=source_relative proto/orders/orders.proto`, protoc 3.21.12, protoc-gen-go 1.36.11, protoc-gen-go-grpc 1.6.1). The frontend only reads orders; creating and confirming them is checkout's job.

- **Whose orders:** the owner is the session cookie (`shop_session-id`, the same id the cart uses). The history asks for the session's orders; the tracking page passes the session as the `GetOrder` guard and additionally refuses an order whose `session_id` differs. Unknown, foreign and malformed ids (checked against the service's id rule before any call) all give the same `404` page.
- **Tracker:** an ordered list Placed, Processing, Shipped, Delivered. Each stage says its state in words (Done, Current step, Pending, `aria-current="step"` on the current one), not by colour only; reached stages show their time, pending ones "Expected <time>, in about N minutes". A stage counts as reached when `status` is at or past it or its timestamp is not after `evaluated_at`; the "in about" text and the "Status as of" line use `evaluated_at`, never the browser clock. Times are shown in UTC. PENDING shows "We are still processing your payment" (no tracker, no tracking number); FAILED shows the failure reason (HTML-escaped like every value). The history only lists confirmed orders (`placed_only`).
- **Live refresh:** while the order is not DELIVERED or FAILED the live part is wrapped with `hx-get` on the same URL and `hx-trigger="every 15s"` (`hx-swap="innerHTML"`); the answer is `286` once it is delivered or failed, which stops htmx. Without JavaScript the page says to reload. If a poll finds the service unavailable the page is left as it is (`204`) and the next poll tries again. Screen readers are not told about the changes (no live region: replacing the whole region every 15 s would re-read it).
- **Pictures:** an order line's picture is only used if it is a path under `/static/img/products/` (no `..`, no query), otherwise the line has an empty picture box.
- **Degradation:** like `REVIEW_SERVICE_ADDR`: lazy connection, 1.5 s deadline. Any failure of the call other than NotFound (down, slow, an error from the service) gives the `503` page "Orders are not available right now" on `/orders` and `/orders/{id}` only, with no backend text; nothing else in the shop calls the service. The header shows an Orders link (next to the cart) and the confirmation page a "Track your order" button linking to `/orders/<order id>` only when the feature is on.
- **Checked:** `GOTOOLCHAIN=auto go vet ./... && go test -race -count=1 ./...` and golangci-lint v2.11.4 (fake order client: history empty, paging, session isolation, badges; tracking per status, stages from `evaluated_at`, FAILED reason and names escaped, foreign and unknown `404` without leaks, service down or hung `503`, fragment shape, polling attributes, feature off). By hand with headless Chromium against the frontend binary on the host and a Go stub of all backends (stub orders advance with the wall clock): history, tracking in every state, light and dark, 1440 and 390 px, no horizontal overflow; the page refreshed itself without a reload from "Order placed" to "Delivered" and polling stopped (`286`); the confirmation page's "Track your order" link opened the new order's tracking page.
- **Not run:** against the real order-service, or with checkout creating real orders in it (checkout has to store them there first: the order id on the confirmation page must be the order-service id, and `session_id` the frontend's session id); `-tags e2e` tests (only vetted); the compose stack; Firefox/WebKit; a screen reader.

## Ratings and reviews

Stars and reviews come from `review-service` (proto copied to `proto/reviews.proto`, stubs in `genproto/reviews*.pb.go`; only the `go_package` option differs from the service's own file). The catalogue's own `rating` / `review_count` fields are no longer shown.

- **Grid** (home, department, search, paging and the htmx fragment): each card with reviews shows stars and the count; one `GetRatingSummary` call per page, not one per product. Products without reviews show nothing.
- **Product page:** the rating under the title (links to `#reviews`), then the average, the review count, the reviews newest first (5 per page) and the "Write a review" form. The API has no per-star counts, so there are no distribution bars. Paging: plain "Older reviews" / "Newer reviews" links (`?rpage=N`); with htmx, "Show more reviews" appends the next page in place.
- **Form:** name (1-80 characters), rating (radio buttons drawn as stars, arrow keys work), review text (1-2000 characters). The same limits as review-service are checked here first; errors are shown above the form and next to each field, the entered values are kept, and the post redirects back on success (post/redirect/get). Review text is always HTML-escaped (`html/template`); line breaks are kept by CSS.
- **Accessibility:** a rating reads "4.5 out of 5, 12 reviews" (the stars themselves are `aria-hidden`), form fields have labels, radio stars are real focusable inputs with a focus ring, colours come from the theme tokens (`--color-star` in `static/styles/reviews.css`) in light and dark.
- **CSRF:** like the other forms there is no token. Visitor state is in `SameSite=Strict` cookies and a review carries no identity; additionally a post with `Sec-Fetch-Site: cross-site` is refused.
- **Degradation:** the connection is lazy and `REVIEW_SERVICE_ADDR` defaults to `review-service:8084`, so a missing or down service never stops the pod or the pages: grid cards have no stars, the product page shows "Reviews are not available right now" (the form is still there and answers `503` if the service stays down), and reads are cut off after 400 ms. With `REVIEW_SERVICE_ADDR=` (empty) the reviews block and the stars are not rendered at all.
- **Checked:** `GOTOOLCHAIN=auto go vet ./... && go test -race ./...` (fake review client: one batched call, stars, service down or hung, switched off, validation, XSS, paging, fragment, post/redirect). By hand with headless Chromium against the real review-service image (seeded) and the frontend binary on the host with stub catalog/currency/cart backends: grid stars at 1280 px, product page with reviews, load more with htmx and with JavaScript off, invalid and valid post, arrow-key rating, light and dark, 390 px without horizontal scroll, review service stopped (200 without stars, post gives 503), `REVIEW_SERVICE_ADDR` unset (service not resolvable: 200, first request ~350 ms) and empty. Not run: the container image of this version, screen readers, contrast measured with a tool.
- **Not changed:** the "Top rated" sort still uses the catalogue's `rating` field, not review-service.

## Product images

The photos in `static/img/products/` (webp, 1000 px and 600 px thumbnails) come from DummyJSON (`https://cdn.dummyjson.com/product-images/...`, dataset retrieved 2026-10-27). DummyJSON itself is MIT licensed; the licence and provenance of the **images** on its CDN could not be found, so **the licence status of the product photos is UNVERIFIED** (assumed MIT, not confirmed). Brand and product names are the dataset's and are used as demo data only. Details and the selection are in `apps/product-catalog-service/data/seed/SOURCES.md` of the platform-demo repository. Do not publish this store outside a demo before the image licence is clarified.

## Container image

Multi-stage [`Dockerfile`](Dockerfile):

| Stage | Base | What happens |
|---|---|---|
| builder | `golang:1.26-alpine` | `go mod download`, `go build -ldflags="-s -w" -o /bin/frontend ./cmd/server` (`CGO_ENABLED=0`; target arch = builder's arch) |
| runtime | `alpine:3.24` | binary + `templates/` + `static/` under `/app`; adds `ca-certificates` |

- **Entrypoint:** `./frontend` (working dir `/app` — templates and static files are loaded by relative path, so don't change the working dir).
- **User:** `appuser` — UID `100`, GID `101` (verified). Set `runAsUser: 100` for `runAsNonRoot`.
- **EXPOSE:** `8080`.
- **Docker `HEALTHCHECK`:** `wget -qO- http://localhost:${PORT:-8080}/_healthz`. Alpine's busybox `wget` is in the image, so an exec probe is possible, but `httpGet` is simpler.
- **Image size:** 76.7 MB for `platform-demo/frontend:local` (`docker images`, the local compose build of this branch before the audit fixes; `static/` alone is 17 MB because of the product photos). The older ~15 MB figure predates them. Not rebuilt after the fixes.
- The repo root may contain a compiled `server` binary (git-ignored) — it is not used by the image.

## Kubernetes deployment notes

**Workload:** stateless `Deployment`; scale freely (sessions are in a cookie, carts in `cart-service`; no sticky sessions required). This is the one workload that needs external exposure.

**Ports to declare**

| Name | Port | Protocol | Expose via Service? |
|---|---|---|---|
| `http` | `8080` | TCP (HTTP/1.1) | Yes — Service port `80` → `8080`; front it with an Ingress / Gateway / LoadBalancer |
| `metrics` | `9464` | TCP (HTTP/1.1) | Optional (debugging/pprof). Not scraped by default — metrics are pushed over OTLP |

**Exposure:** serve over **HTTPS** (see cookie gotcha). Ingress `Service` target is `frontend:80` → pod `8080`. If you use a sub-path ingress, also set `BASE_URL` to match. Set the NetworkPolicy so that only the ingress controller can reach `8080`.

**Probes**

```yaml
startupProbe:
  httpGet: { path: /_healthz, port: 8080 }
  periodSeconds: 2
  failureThreshold: 15
readinessProbe:
  httpGet: { path: /_healthz, port: 8080 }
  periodSeconds: 5
livenessProbe:
  httpGet: { path: /_healthz, port: 8080 }
  periodSeconds: 10
  failureThreshold: 3
```

Health is static, so readiness doesn't reflect backend health. Pod startup is near-instant.

**Security context**

```yaml
securityContext:
  runAsNonRoot: true
  runAsUser: 100
  runAsGroup: 101
  readOnlyRootFilesystem: true       # nothing is written at runtime
  allowPrivilegeEscalation: false
  capabilities: { drop: ["ALL"] }
```

**Resources** — requests sized from measured idle usage in the test stack (see the chart's `values.yaml` for limits):

```yaml
resources:
  requests: { cpu: 100m, memory: 48Mi }
  limits:   { cpu: "500m", memory: 192Mi }
```

**Config / secrets:** no secrets. Service addresses, observability endpoints and branding vars belong in values → env.

**Telemetry:** pushed, not scraped — OTLP/gRPC to Alloy (`global.monitoring.otlpEndpoint`), profiles to Alloy `:4040`. Metrics carry `job="frontend"` and `instance=<pod>`; no scrape annotations or `ServiceMonitor` are needed.

**Helm chart:** [`platform-demo-helm/frontend`](https://github.com/platform-boutique/platform-demo-helm/tree/main/frontend) (built on the shared `platform-common` library chart). It ships with HA defaults — 2 replicas,
HPA, PodDisruptionBudget, topology spread, NetworkPolicy, restricted pod security context, probes and the full env contract (see Configuration) —
and was verified under the `restricted` Pod Security Standard on the earlier local 3-node kind cluster (since retired). The platform now targets AWS EKS, where this chart has not been deployed or verified yet.

**Deployment and admission policies:** Argo CD deploys the chart from `platform-demo-helm` (`envs/<env>/values/frontend.yaml` pins the image digest; promotion is a pull request). In the cluster Kyverno enforces, for this service: pods named `app.kubernetes.io/name: frontend` that run under the ServiceAccount of the same name (Cilium derives the network identity from that label), an image from this repository pinned by digest and signed by our release pipeline, a read-only root filesystem, resource requests and a memory limit, readiness and liveness probes and no API token. The Helm CI evaluates the rendered chart against these policies before merge; see [`platform-demo-helm/policies`](https://github.com/platform-boutique/platform-demo-helm/tree/main/policies).

## Observability

| Signal | Implementation | Destination |
|---|---|---|
| **Traces** | OpenTelemetry: `otelhttp` on the HTTP handler (inbound), `otelgrpc` client handler on every backend connection (outbound), OTLP/gRPC exporter, `AlwaysSample`, W3C TraceContext + Baggage. The trace starts here and propagates through every downstream service | Alloy `:4317` → Tempo |
| **Metrics** | `prometheus/client_golang` registry — `http_requests_total`, `http_request_duration_seconds` (buckets 1 ms … 10 s), `http_requests_in_flight`, labelled `method`, `route`, `status_code`, plus Go runtime/process collectors — bridged into OpenTelemetry and pushed over OTLP/gRPC every 15 s. `/metrics` is still served on `METRICS_PORT` for debugging | Alloy → Mimir |
| **Logs** | `logrus` JSON to stdout at `debug` level **and** an OTLP/gRPC hook (`otellogrus`): request path/method/id/session, response status/bytes/duration | Alloy → Loki |
| **Profiles** | Pyroscope Go SDK (CPU, alloc/inuse objects & space) when enabled | Alloy `:4040` → Pyroscope |

Every series carries `job="frontend"` (from `service.name`) and `instance=<pod name>` (`service.instance.id`), which is what the Grafana dashboards select on. Logs land in Loki with `service_name="frontend"` and trace/span ids, so a log line links to its trace.

## Running locally

Requirements: Go 1.26 (`go.mod` says `go 1.26.10`; with an older local Go use `GOTOOLCHAIN=auto`), Docker, and the seven backend services reachable (the quickest way is `platform-demo-observability/docker-compose.yaml`).

```bash
# from source (note: entrypoint is ./cmd/server)
export PRODUCT_CATALOG_SERVICE_ADDR=localhost:3550 CURRENCY_SERVICE_ADDR=localhost:7000 \
       CART_SERVICE_ADDR=localhost:7070 RECOMMENDATION_SERVICE_ADDR=localhost:8081 \
       CHECKOUT_SERVICE_ADDR=localhost:5050 SHIPPING_SERVICE_ADDR=localhost:50051 \
       AD_SERVICE_ADDR=localhost:9555
go run ./cmd/server          # http://localhost:8080

# via Docker
docker build -t frontend .
docker run --rm -p 8080:8080 -p 9464:9464 \
  -e PRODUCT_CATALOG_SERVICE_ADDR=product-catalog-service:3550 \
  -e CURRENCY_SERVICE_ADDR=currency-service:7000 \
  -e CART_SERVICE_ADDR=cart-service:7070 \
  -e RECOMMENDATION_SERVICE_ADDR=recommendation-service:8080 \
  -e CHECKOUT_SERVICE_ADDR=checkout-service:5050 \
  -e SHIPPING_SERVICE_ADDR=shipping-service:50051 \
  -e AD_SERVICE_ADDR=ad-service:9555 \
  frontend

go test ./...

curl -s localhost:8080/_healthz            # ok
curl -s localhost:9464/metrics | grep http_requests_total
```

Browsing to `http://localhost:8080` works with the `Secure` cookies because browsers treat `localhost` as a secure context.

## Project structure

```
├── cmd/server/
│   ├── main.go          # entrypoint: env wiring, gRPC dials, router, middleware chain, HTTP server
│   ├── handlers.go      # page handlers (home, product, cart, checkout, currency, logout, product-meta)
│   ├── rpc.go           # gRPC client helpers for each backend, with their deadlines
│   ├── browse.go        # catalogue query parameters, paging, view models for catalog.html
│   ├── cart.go          # cart lines checked against the catalog (stale lines skipped), header count
│   ├── errors.go        # gRPC status -> error page mapping, cached department nav
│   ├── reviews.go       # review-service client use: rating summaries, review list, review form validation and POST handler
│   ├── middleware.go    # request logging + session-id cookie
│   ├── otlp.go          # OTLP/gRPC setup: trace, metric (Prometheus bridge) and log (logrus hook) providers
│   ├── telemetry.go     # Prometheus HTTP metrics, /metrics server, route wrapping, Pyroscope
│   └── *_test.go        # unit tests for handlers, middleware, rpc helpers and telemetry
├── internal/validator/  # form payload validation (go-playground/validator) + tests
├── money/               # Money arithmetic helpers + tests
├── genproto/            # generated gRPC stubs for all backend services (demo.pb.go, demo_grpc.pb.go)
├── templates/
│   ├── header.html, footer.html  # page shell: theme script, header (brand, nav, currency, theme toggle, cart), footer
│   ├── home.html, product.html, cart.html, order.html, error.html  # pages
│   ├── ad.html, recommendations.html  # shared blocks
│   ├── reviews.html     # stars, rating badge, reviews block, review items (htmx fragment), review form
│   ├── icons.html       # inline SVG brand mark and icons (no image files)
│   └── catalog.html     # department nav and tiles, search/sort toolbar, results fragment, pagination, product card
├── static/
│   ├── styles/          # styles.css (design tokens, light/dark, layout, components), cart.css, order.css, browse.css, reviews.css, orders.css
│   ├── js/              # htmx.min.js (vendored), gallery.js
│   ├── favicon.svg
│   └── img/products/    # product photos (webp), see "Product images"
├── tests/e2e/           # HTTP end-to-end tests (not run by CI; the kind-based job was removed)
├── .github/workflows/   # ci.yaml (pull requests), release.yaml (v* tags / manual) - callers of platform-demo-workflows
├── Dockerfile
└── go.mod / go.sum / .golangci.yaml
```

## Checked for the audit fixes

Run: `GOTOOLCHAIN=auto go vet ./... && GOTOOLCHAIN=auto go test -race -count=1 ./...` and `golangci-lint run ./...` (v2.11.4, the repo's `.golangci.yaml`): clean. Each fix has a test that fails without it (stale cart lines, UTF-8 / long search text, status mapping and no leaks, htmx fragment shape and redirect, quantity above stock, empty ad list, hung backends, document titles, border contrast). The htmx swap (one `#catalog-results` and one `aria-live` after paging, scroll to the top of the results, title update, error message in `#page-alert`, no pushed URL on error, landing -> search loads the full page) was driven with headless Chromium (Playwright) against the real templates and static files over fake backends. Not run: `tests/e2e` (needs a live stack, only `go vet -tags e2e`), the container image of this version, a real cart-service with an old cart, axe/Lighthouse or a screen reader.

## Known limitations

- `Secure` + `SameSite=Strict` cookies break carts over plain HTTP outside `localhost`.
- Health is static; readiness doesn't reflect backend availability.
- Debug-level JSON logging is unconditional.
- Reviews: no spam protection, rate limiting or moderation (anyone who can load a product page can post); the review author is a free-text name, not an account.
- Per-request gRPC calls have no retries; each has a deadline (3 s, 15 s for checkout, 100 ms for ads, see "Deadlines").
- Stock is shown and capped in the cart, but orders do not decrement it (inventory is a later slice).
- `/setCurrency` redirects to the `Referer` header as is, and `/static/` lists directories without cache headers (found by the audit, not changed).

## Wide screens

The page container grows with the screen (1200 px up to 1359, 1440 px from 1360, 1680 px from 1720, 1960 px from 2200; `--container` in `static/styles/styles.css`), and the product and recommendation grids add columns with it (5 from 1360 px, 6 from 1720 px, 7 from 2200 px); the product gallery is up to 640 px wide from 1360 px. Phones and tablets are unchanged. Checked by hand with headless Chromium at 1920, 1440 and 390 px (home, product, cart/checkout with the United States and Germany); not run: 2200 px and wider, other browsers.
