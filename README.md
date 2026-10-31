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
| **Depends on (gRPC)** | `product-catalog-service`, `currency-service`, `cart-service`, `recommendation-service`, `checkout-service`, `shipping-service`, `ad-service`; optional: `review-service` (ratings and reviews, see below), `order-service` (order history and tracking, see "Orders"), `user-service` (accounts, see "Accounts") |
| **Called by** | Browsers / ingress / load generators |
| **Not used** | `payment-service` and `email-service` are only reached via `checkout-service`. `shopping-assistant-service` is **not** wired in |

## HTTP routes

All routes are prefixed with `BASE_URL` (empty by default).

| Route | Method | Description |
|---|---|---|
| `/` | GET, HEAD | Home and catalogue — hero and department tiles (landing only), search / sort toolbar, product grid (24 per page), pagination, currency picker, ad, cart size. Query parameters: `category` (department id), `q` (search text, 100 characters at most; invalid UTF-8 is dropped), `sort` (`name` default, `price_asc`, `price_desc`, `rating`), `page` (1-based). With `HX-Request` it returns only the `#catalog-results` fragment (plus a `<title>`); when the view would change the hero or heading (landing <-> search, category or page 2) it answers `HX-Redirect` instead |
| `/product/{id}` | GET, HEAD | Product page — product, gallery, price, stock, quantity select (1 to min(stock, 10)), recommendations, ad, rating summary, reviews and the review form. Unknown id: `404` page. `?rpage=N` selects the page of reviews; `?reviewed=1` shows the "review posted" message. With `HX-Request` and `rpage` it returns only the next `<li>` review items (htmx "load more") |
| `/product/{id}/reviews` | POST | Post a review (form fields `author_name`, `rating` 1-5, `comment`). Valid: `303` back to `/product/{id}?reviewed=1#reviews`. Invalid: the product page again with `422` and inline errors; review service not reachable: `503` with the form kept; reviews switched off or unknown product: `404`; `Sec-Fetch-Site: cross-site`: `403` |
| `/cart` | GET, HEAD | View cart — items, shipping estimate, total, recommendations. Lines whose product is no longer in the catalog (a cart outlives the catalog) are skipped, listed in a notice with an Empty cart button, logged once per request and not counted in the header. A line that holds more than is left shows "Only N left. You have Q in your cart ..." (or "Sold out") in red, a line that fits with 5 or fewer left shows "Only N left". The page is sent with `Cache-Control: no-store` (it holds the checkout form and its idempotency key, Back must not restore an old one) |
| `/cart` | POST | Add item (form fields `product_id`, `quantity` 1-10). `422` with a readable message when the product is out of stock or quantity plus what is already in the cart exceeds the stock; unknown product: `404`. The stock is the live stock of `inventory-service` when it answers (catalogue `stock` otherwise, see "Live stock"). With `INVENTORY_SERVICE_ADDR` set the order itself reserves the stock at checkout |
| `/cart/empty` | POST | Empty the cart |
| `/orders` | GET, HEAD | Order history of the signed-in user (`ListOrders` with `user_id`), or of the visitor's session when anonymous: confirmed orders only (`ListOrders` with `placed_only`), newest first, 10 per page (`?page=`, a page past the end shows the last page). Each row: short id, date, status badge, item count, thumbnails, total, link. Friendly empty state. Order service down: `503` page "Orders are not available right now"; `ORDER_SERVICE_ADDR` empty: `404` |
| `/orders/{id}` | GET, HEAD | Tracking page of one order of the user, or of the session when anonymous (`GetOrder` with the user or the session as guard): status badge, stage tracker, items, shipping address, tracking number, totals. An unknown, somebody else's or malformed id gives the same `404` page. With `HX-Request` it returns only the live part (badge, tracker, tracking number); `200` while the order is on its way, `286` once it is delivered or failed (htmx stops polling). With the mailbox on and a mail for the order, the page has a "View confirmation email" button (one `ListOrderEmails` call with a 1 s deadline, skipped for the htmx polls, silent on any failure) |
| `/emails` | GET, HEAD | The visitor's mailbox: ids of their orders (`ListOrders` with `user_id` when signed in, else with the session id; `placed_only`, first 50) go to ONE `ListOrderEmails`; rows (subject, recipient, link to the order, time, newest first) link to `/emails/{id}?order={order id}`. Friendly empty state "No emails yet". `Cache-Control: no-store`. Linked from the footer, `/orders` and `/account` (only while the mailbox is on). `EMAIL_SERVICE_ADDR` empty (or `ORDER_SERVICE_ADDR` empty): `404`; email-service down, slow or erroring: `503` page "Your emails are not available" on the mailbox routes only |
| `/emails/{id}?order={order_id}` | GET, HEAD | One mail. Ownership first: the order in `?order=` goes through `GetOrder` with the same user or session guard as `/orders/{id}`; only then `GetOrderEmail(email_id, order_id)`, and the answer must carry exactly that id and order. Not the visitor's order, unknown mail, a mail of another order, a missing or malformed `order`: the same `404` page and no body is fetched. Shows subject, recipient, time, the order link, the HTML body in `<iframe sandbox srcdoc="...">` (empty `sandbox`: no scripts, forms, popups or same-origin) and the plain text in a `<details>` below (open when the mail has no HTML part). `Cache-Control: no-store` on every answer |
| `/cart/checkout` | POST | Validate the form, call `checkout-service.PlaceOrder`, render the order page. The country is a dropdown (the names shipping-service accepts, United States first and preselected); the State field exists only for the United States: `static/js/checkout.js` hides and disables it for any other country, and the server ignores a state sent for another country, normalises `US`/`USA` to `United States` and requires a state for it. Signed in: the request carries `account_id` (the user id) and the cart key is the user id; anonymous: no `account_id`, the cart key is the session id; `user-service` unable to say who a present cookie belongs to: `503` instead of an order as a guest. The checkout form carries a hidden `idempotency_key` (22 random characters from `crypto/rand`, made when the cart page is rendered, not per submit; a malformed value is refused with `422`, a missing one is passed on empty) and goes to `PlaceOrder` as `idempotency_key`, so a double submit or a reload of the POST shows the same order page. Failures: `ABORTED` ("being processed", also a concurrent duplicate) gives the `409` page "Your order is being processed" with a link to Orders; `FAILED_PRECONDITION` starting with `out of stock:` gives a `422` page with checkout's message; every other failed order (declined card, `this attempt failed, start the checkout again` for a replay of a failed attempt, shipping rejected, form refused, `503`, `500`) is its own error page; all of them except `409` link back to the cart ("Back to your cart"), which renders a fresh form with a fresh key |
| `/ads/{id}/click?to={url}` | GET | Click counter of an ad: calls `ad-service.RecordClick(id)` best effort (1 s deadline, failures only logged at debug level, an unknown id or an old ad-service changes nothing for the visitor) and answers `302` with `Cache-Control: no-store`. `to` (the ad's `redirect_url`, URL-escaped) is followed ONLY when it is a local path: it starts with a single `/` (not `//`, not `/\`), is valid UTF-8, has no control characters, no scheme or host and is at most 2048 bytes; anything else, an empty or missing `to` included, goes to `/` (the click is still counted). Ad ids that are not 1 to 64 characters of `A-Za-z0-9._-` are not sent to the ad-service. Ad links on the pages point here; an ad without an `id` (ad-service from before click tracking) links straight to its target |
| `/setCurrency` | POST | Set the display currency cookie (`currency_code`) |
| `/register` | GET, HEAD, POST | Create an account (form fields `name`, `email`, `password`, optional hidden `next`). The form checks the service's limits first (name 1-100 characters, plain e-mail address, password 8-72 bytes) and shows each error next to its field with `422`; the password is never put back into the page. `user-service` answers: `ALREADY_EXISTS` is `422` "An account with this email already exists.", `INVALID_ARGUMENT` is `422` with its message, `RESOURCE_EXHAUSTED` is `429` with `Retry-After: 60`; a service that is down gives the `503` page "Accounts are not available". Success: sets `shop_auth`, claims the session's orders, merges the cart (see "Accounts"), then `303` to `next` (a safe local path), else to `/orders` if orders were claimed, else to `/account`. A signed-in visitor is sent to `/account`; `Sec-Fetch-Site: cross-site` posts get `403`; `USER_SERVICE_ADDR` empty: `404` |
| `/login` | GET, HEAD, POST | Sign in (fields `email`, `password`, optional hidden `next`; `?next=` on the GET is carried into the form). Wrong password, unknown e-mail and a malformed e-mail all give the same `422` "Wrong email or password." (no other hint); `429`, `503`, `403`, `404` and the success path as for `/register` |
| `/account` | GET, HEAD | The signed-in visitor's name and e-mail, a link to the orders (and "Your emails" while the mailbox is on) and a Sign out button. Anonymous: `303` to `/login?next=/account`. `user-service` down while a `shop_auth` cookie is present: `503`. `USER_SERVICE_ADDR` empty: `404` |
| `/logout` | POST | Sign out: `Logout` at user-service (best effort), expire `shop_auth`, `303` to `/`. `Sec-Fetch-Site: cross-site`: `403` |
| `/logout` | GET | Expire all cookies (including `shop_auth`, after a best-effort `Logout` call) and redirect to `/` |
| `/product-meta/{ids}` | GET | JSON of a single product. Kept for compatibility: no page or script uses it any more. Any error answers an empty `200` |
| `/static/*` | GET | `styles/*.css`, `js/htmx.min.js` and `js/gallery.js`, `favicon.svg`, product images (`img/products/`) |
| `/_healthz` | GET | Liveness/readiness (`ok`) |
| `/robots.txt` | GET | `Disallow: /` |

**Supported currencies** (whitelist in code, intersected with what `currency-service` offers): `USD`, `EUR`, `CAD`, `JPY`, `GBP`, `TRY`. Default is `USD`.

### Which backend each page calls

| Page | Calls |
|---|---|
| Home | currency (list + convert per product), product-catalog (`ListCategories` + `ListProductsFiltered`, 24 per page), inventory (ONE `GetStock` with the ids of the page, 1 s deadline, failures ignored), cart (get, then product-catalog `GetProduct` per cart line for the header count), ad (100 ms deadline, failures ignored), review (ONE `GetRatingSummary` with the ids of the page, 400 ms deadline, failures ignored) |
| Product | product-catalog (get), inventory (ONE `GetStock` for the product, 1 s deadline, failures ignored), currency, cart, recommendation → product-catalog (up to 4), ad, review (`ListReviews`, 5 per page, and ONE `GetRatingSummary` for the rating bars, 400 ms deadline, failures ignored) |
| Ad click | ad (`RecordClick`, 1 s deadline, failures ignored); nothing else |
| Post review | product-catalog (get, the product must exist), review (`AddReview`, 2 s deadline) |
| Cart | cart (get), shipping (quote with nil address), currency, product-catalog, inventory (ONE `GetStock` with the ids of the lines, 1 s deadline, failures ignored), recommendation |
| Checkout | checkout (PlaceOrder), then recommendation + currency for the confirmation page |
| Orders / order tracking | order (`ListOrders` / `GetOrder`, 1.5 s deadline) and, for the header navigation only, the cached catalog category list |
| Emails / email detail | order (`ListOrders` / `GetOrder`, 1.5 s deadline), then mailbox (`ListOrderEmails` / `GetOrderEmail`, 1.5 s deadline) and the cached catalog category list for the header navigation |
| Order tracking, extra | mailbox (ONE `ListOrderEmails` for the order, 1 s deadline, failures ignored) for the "View confirmation email" button |
| Register / sign in | user (`Register` / `Login`, 5 s deadline), then order (`ClaimOrders`, 1.5 s) and cart (`GetCart`, product-catalog `GetProduct` per line, inventory ONE `GetStock` for all lines, `AddItem`, `EmptyCart`) |
| Every request with a `shop_auth` cookie | user (`GetSession`, 1 s deadline, once per request; not for `/static`, `/_healthz`, `/robots.txt`) |
| Account | nothing of its own: the identity comes from the request |

### Errors

Error pages never show stack traces or backend error text; those go to the server log only (the page footer shows the request id to find the log line). The backend's gRPC status code decides the page, the services pass their codes through:

| Backend status | HTTP | Page |
|---|---|---|
| `InvalidArgument`, `FailedPrecondition` | `422` | the backend's message (the `rpc error: code = ... desc =` wrapper and a `card_declined: ` style reason prefix are removed), e.g. the payment decline reason. `FailedPrecondition` starting with `out of stock:` (checkout's saga) is the page "Some items are out of stock" with checkout's message |
| `NotFound` | `404` | "Not found" (unknown product id, unknown order) |
| `Aborted` | `409` | "Your order is being processed" (checkout: the same order is already in flight; the backend text is not shown; links to Orders, not to the cart, because a fresh attempt while the first one runs could charge twice) |
| `Unavailable`, `DeadlineExceeded` | `503` | "Temporarily unavailable" |
| anything else | `500` | generic text, no details |

Our own input validation (add to cart, checkout form) answers `422` with the validation text, `400`/`403` use fixed texts. Error pages keep the header with the department nav and a page-specific `<title>`. htmx requests that fail get a small message for the `#page-alert` region (`HX-Retarget`, `HX-Push-Url: false`) and the page is left as it is; htmx is configured (`htmx-config` meta tag) to swap 4xx/5xx answers.
Until checkout-service passes the payment status through, a decline arrives as `Internal: failed to charge card: ... rpc error: code = InvalidArgument|FailedPrecondition desc = <reason>: <text>`; for that one message shape the frontend extracts `<text>` and shows it as `422`. Any other `Internal` stays a generic `500`.

### Deadlines

Every backend call has a deadline so that a hung service ends in a `503` page instead of a dropped connection (the server's `WriteTimeout` is 30 s): 3 s for catalog, currency, cart, recommendation and shipping (`backendTimeout` in `cmd/server/rpc.go`), 15 s for `PlaceOrder`, 100 ms for ads (1 s for the click counter, `adClickTimeout` in `cmd/server/ads.go`), 400 ms / 2 s for review-service reads / writes. They are constants, not configuration.

### Sessions & cookies

| Cookie | Purpose | Flags |
|---|---|---|
| `shop_session-id` | random UUID per visitor; used as the cart `user_id` | `HttpOnly`, **`Secure`**, `SameSite=Strict`, 48 h |
| `shop_currency` | chosen display currency | `HttpOnly`, **`Secure`**, `SameSite=Strict`, 48 h |
| `shop_auth` | the sign-in token (opaque, from user-service); only there when signed in; never rendered into a page and never logged | `HttpOnly`, **`Secure`**, `SameSite=Strict`, `Path=/`, `Max-Age` = what is left of the token's life (14 days at user-service's default) |

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
| `USER_SERVICE_ADDR` | No | `user-service:8086` | `host:port` of user-service (gRPC). Optional: lazy connection; while it is unreachable the shop works as for an anonymous visitor and only `/login`, `/register` (posts), `/account`, `/orders`, `/orders/{id}` and checkout of a visitor who carries a `shop_auth` cookie answer `503`. Set to an empty string to switch accounts off (routes `404`, no Sign in link, a `shop_auth` cookie is ignored) |
| `INVENTORY_SERVICE_ADDR` | No | `inventory-service:8087` | `host:port` of inventory-service (gRPC). Optional: lazy connection; the pages overlay the live stock on the catalogue's `stock` (see "Live stock"). If it is unreachable, slow (1 s deadline) or does not know a product, the catalogue's stock is used and a warning is logged, never a `500`. Set to an empty string to switch live stock off (today's behaviour: the catalogue's stock) |
| `EMAIL_SERVICE_ADDR` | No | `email-service:8080` | `host:port` of email-service (gRPC, `hipstershop.mailbox.MailboxService`, same port as its `EmailService`). Optional: lazy connection; the mailbox pages (`/emails`, `/emails/{id}`) and the "View confirmation email" button depend on it. While it is unreachable only the two mailbox routes answer `503`. Set to an empty string to switch the mailbox off (routes `404`, no "Your emails" links, no button). The mailbox also needs `ORDER_SERVICE_ADDR` (order-service says whose mail is whose); with that empty the mailbox is off too |
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

- All seven `*_ADDR` variables marked **Yes** in the table are **mandatory** (the others, e.g. `REVIEW_SERVICE_ADDR`, `INVENTORY_SERVICE_ADDR` and `EMAIL_SERVICE_ADDR`, are optional): a missing one makes the process `panic` at startup. They are plain `host:port` (no scheme). Use FQDNs (`svc.<namespace>.svc.cluster.local:<port>`) if services live in other namespaces.
- gRPC connections use `grpc.NewClient` (lazy): the pod starts and `/_healthz` returns `ok` even if every backend is down. Page loads then fail with a 503 or 500 error page until backends return (each backend call times out after at most 3 s, see "Deadlines").
- The earlier README listed `ENV_PLATFORM`; it is **not read by the code**.
- Log level is hard-coded to `debug` (JSON via `logrus`) — expect chatty logs; there is no env switch.
- Telemetry is **pushed**, not scraped: traces, metrics and logs share one OTLP/gRPC endpoint (Alloy); profiles go to Pyroscope (via Alloy). `service.instance.id` is the pod hostname, so replicas stay distinct in Mimir.
- A Prometheus `/metrics` endpoint is still served on `METRICS_PORT` for debugging, but nothing has to scrape it — the same registry is bridged into OTLP, so metric names are unchanged.
- Logs: the `logrus` logger keeps writing JSON to stdout and additionally mirrors every record over OTLP (with trace ids when a context is attached).

## Look and feel

- **Brand:** one configuration point, `BRAND_NAME` / `BRAND_TAGLINE` (read once at startup in `cmd/server/brand.go`, passed to every template as `brand_name` / `brand_tagline`). The logo is an inline SVG mark (`templates/icons.html`, `static/favicon.svg`); no image files, fonts or CDNs are loaded from anywhere else.
- **Design system:** `static/styles/styles.css` defines CSS custom properties for colour, type scale, spacing, radius and shadows, then layout and components; `cart.css` and `order.css` hold page-specific rules. System font stacks only: one system-ui sans for everything (`--font-display` for headings and the wordmark: weight 700, letter-spacing -0.02em; `--font-sans` for body; `--font-mono` for ids), no web fonts.
- **Palette ("Evergreen & Oat"):** one theme in two modes. Light: oat page `#f4f3ee`, white cards, text `#17211b`, evergreen accent `#1f5a43` (logo, primary buttons, links, focus), soft green `#dfeadf` (hero, badges), brass `#a58334` for stars and small highlights (`--color-brass`). Dark: `#0f1512` page, `#171f1b` cards, accent `#7fcf9f`, brass `#d4b25e`. Success is a teal-blue (`#0a6578` / `#66d0d8`) so it does not read as the brand accent; danger stays red. Everything is a token in `styles.css`: the `:root` block (light), the `prefers-color-scheme: dark` block and the `:root[data-theme="dark"]` block (keep the two dark blocks identical; the checker fails when they differ). The logo in `templates/icons.html` paints with the accent tokens; `static/favicon.svg` carries the same two colours as literals (a favicon cannot read the page tokens, and it follows the OS setting, not the header toggle). Star glyphs are CSS masks painted with `--color-star` / `--color-star-empty`. The only literal colour left in the stylesheets is the white page behind the mail iframe (the mail brings its own page).
- **Light / dark:** follows `prefers-color-scheme`; the header toggle stores an explicit choice in `localStorage` (`theme`), applied by a small inline script in `<head>` before the stylesheets so there is no flash of the wrong theme. Without JavaScript or storage the toggle is hidden or not remembered and the OS setting applies.
- **Responsive / accessible:** product grid 2 / 3 / 4 columns (phone / tablet / desktop), square photo frames (`aspect-ratio` + `object-fit`) so square webp and older non-square jpgs both fit; skip link, landmarks, visible focus rings, `prefers-reduced-motion` respected.
- **Catalogue UI:** `templates/catalog.html` holds the department nav (`nav_extra`, also on cart, order and error pages), department tiles, the search/sort toolbar (`catalog_toolbar`), the results area (`catalog_results`, the htmx fragment) and `catalog_pagination`; styles in `static/styles/browse.css`. Every control is a plain link or GET form; with htmx (`static/js/htmx.min.js`, version 2.0.11, vendored, sha256 `d6fdc75f204e6bdefa99b69bf1e6d4ac69b8a364f77929f45c13476b4000f717`; the audit of 2026-10-27 found it byte-identical to `https://unpkg.com/htmx.org@2.0.11/dist/htmx.min.js`) the toolbar and the pager replace `#catalog-results` (`hx-swap="outerHTML"`) and push the URL.
- **Page titles:** `<page> | brand` (product name, department, "Search results", "Cart", "Order complete", the error title); the landing page keeps `brand | tagline`.
- **Form controls:** the border of inputs, selects and secondary buttons (`--color-control-border`) has at least 3:1 contrast against page, surface and surface-2 in light and dark (computed in `cmd/server/css_test.go` and in `tools/check-contrast.py`).
- **Contrast check:** `python3 tools/check-contrast.py` (stdlib only, no install) reads the tokens from `static/styles/styles.css` and prints the WCAG ratio of every pair that matters in both themes (text and muted text on page and cards, links, button label on accent, text on soft backgrounds, danger and success text, control border and focus ring and logo >= 3:1, brass >= 3:1 as a graphic, success hue away from the accent); it exits 1 when one fails or the two dark blocks differ. Run it after any colour change.
- **Checked so far:** templates rendered with sample data by a throwaway preview server and screenshotted with headless Chromium (Playwright) at 360, 390, 430, 768 and 1280 px, light and dark, with no horizontal overflow; the theme toggle and reload persistence were checked the same way. Not yet checked against the running stack, and that screenshot run predates the Evergreen & Oat theme (not re-taken here: the integrator does that). Contrast ratios of the current palette are computed by `tools/check-contrast.py` (all pairs pass in both themes when it was last run); no axe/Lighthouse run.

## Orders

Order history and tracking come from `order-service` (proto copied to `proto/orders/orders.proto`, stubs in `genproto/orders/`: its own Go package `frontend/genproto/orders`, because `Money`, `Address` and `OrderItem` clash with `demo.proto`; only the `go_package` option differs from the service's file. Regenerate: `protoc -I proto/orders --go_out=genproto/orders --go_opt=paths=source_relative --go-grpc_out=genproto/orders --go-grpc_opt=paths=source_relative proto/orders/orders.proto`, protoc 3.21.12, protoc-gen-go 1.36.11, protoc-gen-go-grpc 1.6.1). The frontend only reads orders; creating and confirming them is checkout's job.

- **Whose orders:** a signed-in visitor's orders are asked for by `user_id` and the tracking page passes the user as the `GetOrder` guard (and refuses an order whose `user_id` differs); an anonymous visitor's by the session cookie (`shop_session-id`) as below (see "Accounts"). The history asks for the session's orders; the tracking page passes the session as the `GetOrder` guard and additionally refuses an order whose `session_id` differs. Unknown, foreign and malformed ids (checked against the service's id rule before any call) all give the same `404` page.
- **Tracker:** an ordered list Placed, Processing, Shipped, Delivered. Each stage says its state in words (Done, Current step, Pending, `aria-current="step"` on the current one), not by colour only; reached stages show their time, pending ones "Expected <time>, in about N minutes". A stage counts as reached when `status` is at or past it or its timestamp is not after `evaluated_at`; the "in about" text and the "Status as of" line use `evaluated_at`, never the browser clock. Times are shown in UTC. PENDING shows "We are still processing your payment" (no tracker, no tracking number); FAILED shows the failure reason (HTML-escaped like every value). The history only lists confirmed orders (`placed_only`).
- **Live refresh:** while the order is not DELIVERED or FAILED the live part is wrapped with `hx-get` on the same URL and `hx-trigger="every 15s"` (`hx-swap="innerHTML"`); the answer is `286` once it is delivered or failed, which stops htmx. Without JavaScript the page says to reload. If a poll finds the service unavailable the page is left as it is (`204`) and the next poll tries again. Screen readers are not told about the changes (no live region: replacing the whole region every 15 s would re-read it).
- **Pictures:** an order line's picture is only used if it is a path under `/static/img/products/` (no `..`, no query), otherwise the line has an empty picture box.
- **Degradation:** like `REVIEW_SERVICE_ADDR`: lazy connection, 1.5 s deadline. Any failure of the call other than NotFound (down, slow, an error from the service) gives the `503` page "Orders are not available right now" on `/orders` and `/orders/{id}` only, with no backend text; nothing else in the shop calls the service. The header shows an Orders link (next to the cart) and the confirmation page a "Track your order" button linking to `/orders/<order id>` only when the feature is on.
- **Checked:** `GOTOOLCHAIN=auto go vet ./... && go test -race -count=1 ./...` and golangci-lint v2.11.4 (fake order client: history empty, paging, session isolation, badges; tracking per status, stages from `evaluated_at`, FAILED reason and names escaped, foreign and unknown `404` without leaks, service down or hung `503`, fragment shape, polling attributes, feature off). By hand with headless Chromium against the frontend binary on the host and a Go stub of all backends (stub orders advance with the wall clock): history, tracking in every state, light and dark, 1440 and 390 px, no horizontal overflow; the page refreshed itself without a reload from "Order placed" to "Delivered" and polling stopped (`286`); the confirmation page's "Track your order" link opened the new order's tracking page.
- **Not run:** against the real order-service, or with checkout creating real orders in it (checkout has to store them there first: the order id on the confirmation page must be the order-service id, and `session_id` the frontend's session id); `-tags e2e` tests (only vetted); the compose stack; Firefox/WebKit; a screen reader.

## Accounts

Sign-in comes from `user-service` (proto copied to `proto/users/users.proto`, stubs in `genproto/users/`, Go package `frontend/genproto/users`; only the `go_package` option differs from the service's file; regenerated with `protoc -I proto/users --go_out=genproto/users --go_opt=paths=source_relative --go-grpc_out=genproto/users --go-grpc_opt=paths=source_relative proto/users/users.proto`, protoc 3.21.12, protoc-gen-go 1.36.11, protoc-gen-go-grpc 1.6.1). `proto/orders/orders.proto` gained `ClaimOrders` and `proto/demo.proto` gained `PlaceOrderRequest.account_id = 7` (additive; `demo.pb.go` was regenerated with the protoc-gen-go 1.34.2 that made the old file, so its diff is only the new field).

- **Identity:** `cmd/server/accounts.go`. The middleware `withUser` (inside the logging middleware) reads `shop_auth` and calls `GetSession` once per request with a 1 s deadline; the user (id, e-mail, name; never the token) goes into the request context and the template data (`user`). No cookie: no call, anonymous. A cookie that cannot be a token, or one the service answers `UNAUTHENTICATED`/`NOT_FOUND` for (stale, expired, logged out elsewhere), is expired and the request goes on as anonymous. A service that cannot answer (down, slow): anonymous for that request, the cookie is KEPT (an outage must not sign everybody out), and the pages that cannot be right without the identity (`/account`, `/orders`, `/orders/{id}`, checkout) answer `503` instead of showing a guest's view.
- **Cart key:** signed in = the user's id, anonymous = the `shop_session-id` cookie. Cart, recommendations and `PlaceOrderRequest.user_id` use that key; `PlaceOrderRequest.account_id` is the user id (empty when anonymous). Checkout therefore stores the order with `session_id` = `user_id` = the user id.
- **After a successful sign-in or registration** (`finishSignIn`): set `shop_auth`, `ClaimOrders(session_id = the shop_session-id cookie, user_id)`, merge the cart, redirect. The merge reads the anonymous cart, adds each product (lines of one product summed) to the user's cart capped by stock like add-to-cart (what the user's cart already holds counts; out-of-stock and no-longer-listed products are skipped), then empties the anonymous cart. It is not atomic (cart-service has no such call): when a call fails half way the anonymous cart is left alone, so nothing is lost, and what was already added is capped by the stock again at the next sign-in. Failures of the claim and the merge are logged and never fail the sign-in. With `ENABLE_SINGLE_SHARED_SESSION=true` neither is done (everybody's "anonymous" orders and cart share one id there, no user may take them).
- **`next`:** only a local path is followed: it must start with a single `/`, not `//` or `/\`, contain no backslash and no control character (browsers drop tabs and newlines inside a URL, so `/<TAB>/evil.com` would become `//evil.com`) and parse without scheme or host; anything else is dropped (`/account` or `/orders` is used instead). It is used as written (it includes `BASE_URL` when it should). A signed-out visitor who opens `/account` is sent to `/login?next=/account`.
- **Forms:** the same look as the review form (`.field`, `.input`, `.field-error`, `.form-notice`), in `templates/account.html` and `static/styles/accounts.css`; light and dark follow the design tokens; wide layout (form card next to a short explanation from 900 px). Every control has a `<label>`, errors are inline with `aria-invalid` and `aria-describedby` and summarised in a `role="alert"` box with links to the fields; `autocomplete` is set (`username`/`current-password`, `email`/`new-password`/`name`); the pages are `Cache-Control: no-store`. The forms carry `novalidate` so that the server's messages are the ones shown. CSRF: no token, like the other forms: `SameSite=Strict` cookies and `Sec-Fetch-Site: cross-site` posts are refused with `403` (this also stops a site from signing a visitor in to the attacker's account).
- **Header and footer:** a "Sign in" link (always with its text) or the name (text from 720 px, an icon below) linking to `/account`, next to the Orders link; footer link too. Nothing is shown when accounts are off.
- **Degradation:** like reviews and orders (lazy connection); see the `USER_SERVICE_ADDR` row and the identity paragraph above. `GET /logout` (wipe the anonymous cookies) also calls `Logout` best effort and expires `shop_auth`.
- **Not in this demo:** password reset, e-mail verification, "remember me" choices, changing the name or password.
- **Local compose:** `local/compose.yaml` does not wire `USER_SERVICE_ADDR` or a `user-service` yet; the integrator does that. Until then the default `user-service:8086` is unreachable in compose, so the shop works and the account pages answer `503` (or set `USER_SERVICE_ADDR=` to hide them).
- **Checked:** `GOTOOLCHAIN=auto go vet ./... && go test -race -count=1 ./...`, `gofmt -l`, golangci-lint v2.11.4, `go vet -tags e2e ./...`, all against fakes (in-memory user client, fake order client, per-user fake cart): register and login set the cookie with the flags, wrong password / unknown e-mail / malformed e-mail give the same `422` page, duplicate e-mail `422`, rate limit `429` + `Retry-After`, inline validation, logout (POST and GET) expires the cookie and calls `Logout`, invalid and stale cookies are anonymous, `GetSession` once per request and never without a cookie, claim called with (session id, user id), cart merge with stock caps and failure cases, `account_id` in checkout only when signed in, orders by user or session, `GetOrder` guard, unsafe `next` values (`//evil.com`, `/\evil.com`, `https://evil.com`, control characters), accounts off, service down, no token or password in any page, redirect or log line.
- **Not run:** against the real user-service, the real order-service `ClaimOrders` or checkout with `account_id`; the compose stack; a browser (no screenshots yet, the integrator takes them); `-tags e2e` tests (only vetted); a screen reader.

## Live stock

Stock comes from `inventory-service` (proto copied verbatim to `proto/inventory/inventory.proto`, stubs in `genproto/inventory/`, Go package `frontend/genproto/inventory`; only the `go_package` option differs from the service's file; generated with `protoc -I proto/inventory --go_out=genproto/inventory --go_opt=paths=source_relative --go-grpc_out=genproto/inventory --go-grpc_opt=paths=source_relative proto/inventory/inventory.proto`, protoc 3.21.12, protoc-gen-go 1.36.11, protoc-gen-go-grpc 1.6.1). `proto/demo.proto` gained `PlaceOrderRequest.idempotency_key = 8` (additive; `demo.pb.go` regenerated with protoc-gen-go 1.34.2, the diff is the new field plus the descriptor bytes).

- **One helper:** `overlayStock` (`cmd/server/inventory.go`) asks `GetStock` ONCE per page with the distinct ids of the products the page shows (at most 100, the service's limit) and a 1 s deadline. Used by the home grid (also the htmx fragment, search and paging), the product page, the cart, add to cart and the cart merge at sign-in.
- **Fallback:** the catalogue's `stock` is used for every product when the service is switched off (`INVENTORY_SERVICE_ADDR=`), unreachable, slow, answers an error, or leaves a product out of its answer (unknown to it). A failed call is logged at warn ("live stock unavailable"); the page never fails because of it.
- **Badges:** 0 or less left: "Out of stock" (the add-to-cart button is disabled on the product page); 1 to 5: "Only N left"; more: no badge ("In stock" on the product page). The quantity select on the product page goes to min(live stock, 10).
- **Limits:** add to cart refuses (`422`, readable message) a product with nothing left and a quantity that, together with what the cart already holds, is more than the live stock ("Only 4 of Pan in stock, and 3 already in your cart."). The cart page marks a line that holds more than is left or is sold out and a line that fits with 5 or fewer left ("Only N left"). The Place order button stays enabled: checkout is the one that decides.
- **Cart merge at sign-in:** the anonymous lines are capped by the live stock minus what the user's cart already holds, with ONE `GetStock` for all lines (the catalogue's stock when the service does not answer).
- **Idempotency key:** the cart page renders a hidden `idempotency_key` (16 bytes from `crypto/rand`, 22 URL-safe characters) in the checkout form. It is made when the form is rendered, not per submit, so a double click or a reload of the POST sends the same key and checkout answers with the same order. A failed attempt's key is spent (checkout answers `this attempt failed, start the checkout again`), so every checkout error page links back to the cart, and the cart page is `no-store` so Back does not restore the old form.
- **Local compose:** `local/compose.yaml` does not wire `INVENTORY_SERVICE_ADDR` or an `inventory-service` yet; the integrator does that. Until then the default `inventory-service:8087` is unreachable in compose and the catalogue's stock is used (a page can wait up to 1 s for the failed call; or set `INVENTORY_SERVICE_ADDR=` to skip it).
- **Checked:** `GOTOOLCHAIN=auto go vet ./...`, `go vet -tags e2e ./...`, `gofmt -l .`, `go test -race -count=1 ./...`, golangci-lint (the repo's `.golangci.yaml`), all against a fake in-memory inventory client and fake checkout (overlay on grid, fragment, product, cart and merge; one call per page; thresholds; fallback when off, down, hung or id missing; limits; note texts; key present, 22+ characters, fresh per render, passed through, double submit shows the same order; `409`, out-of-stock, spent-attempt and declined pages with their links; `no-store`). Not run: against the real inventory-service and checkout-service, the container image of this version, screenshots, the e2e suite (`tests/e2e`, needs a running shop).

## Mailbox

The confirmation mails the shop "sent" (nothing leaves the demo) are kept by `email-service` and shown here. The proto is copied verbatim from the slice-6 contract to `proto/mailbox/mailbox.proto` (package `hipstershop.mailbox`; only the `go_package` option `frontend/genproto/mailbox;mailbox` is added), stubs in `genproto/mailbox/`, generated with `protoc -I proto/mailbox --go_out=genproto/mailbox --go_opt=paths=source_relative --go-grpc_out=genproto/mailbox --go-grpc_opt=paths=source_relative proto/mailbox/mailbox.proto` (protoc 3.21.12, protoc-gen-go 1.36.11, protoc-gen-go-grpc 1.6.1). Code: `cmd/server/mailbox.go`; templates `templates/mailbox.html`; styles `static/styles/mailbox.css` (design tokens, so light, dark and the wide layout follow; one column on phones, card with button from 720 px).

- **The frontend is the guard.** email-service does no access control, it is handed order ids. A list only asks for order ids that order-service returned for the visitor (user id when signed in, else session id) and drops any mail in the answer whose order was not asked for. A detail page first runs `GetOrder` with the same guard as `/orders/{id}` (shared helper `ownedOrder`), and only then `GetOrderEmail`; the mail must come back with the asked id and order, else `404`. A foreign, unknown or malformed pair gives the same `404` page and the body is never fetched.
- **The HTML body is untrusted.** It is shown in `<iframe sandbox srcdoc="...">`: `sandbox` with no value and no `allow-*` token (no scripts, forms, popups, top navigation or same-origin), `referrerpolicy="no-referrer"`. The body is passed to the template as a plain `string`, never as `template.HTML`: `html/template` treats `srcdoc` as an HTML-typed attribute and attribute-escapes a string there (`"`, `<`, `>`, `&`, `'`), the browser decodes the value once and reads the mail as the iframe's document, so a body with `"`, `<script>` or `&quot;` stays inert. The plain text version sits below in a `<details>` (open when there is no HTML part), escaped like any text. Subject and recipient are escaped text too.
- **No inline script** on the mailbox pages (they add none; the shared header and footer keep their existing theme script). The iframe has a fixed height (70 vh, at least 420 px) because auto-sizing would need script.
- **`Cache-Control: no-store`** on `/emails` and `/emails/{id}`, also on their 404 and 503 answers (personal mail must not sit in a shared or back/forward cache).
- **Order page:** "View confirmation email" (newest mail of the order) from ONE `ListOrderEmails` call with a 1 s deadline; not made for the htmx status polls; any failure is logged at warn and the button is simply left out.
- **Links** "Your emails" in the footer, on `/orders` and on `/account`, only when the mailbox is on.

| Situation | `/emails`, `/emails/{id}` | Order page | Links |
|---|---|---|---|
| `EMAIL_SERVICE_ADDR` empty, or `ORDER_SERVICE_ADDR` empty | `404` | no button | hidden |
| email-service down, slow (1.5 s) or erroring | `503` "Your emails are not available" (no backend text) | `200`, no button (waits at most 1 s) | shown |
| order-service down | `503` "Orders are not available" | as before | shown |
| user-service cannot resolve a `shop_auth` cookie | `503` "Accounts are not available" | as before | shown |
| visitor has no orders / no mail yet | `200`, "No emails yet" | no button | shown |
| not the visitor's order or mail, or a malformed id | `404`, no body fetched | | |

- **Local compose:** the integrator wires `EMAIL_SERVICE_ADDR` (default `email-service:8080`, which is where the compose service lives) and the email-service side of the proto. Until email-service answers `MailboxService`, the mailbox pages answer `503` (`Unimplemented` counts as down) and the order page has no button.
- **Checked:** `GOTOOLCHAIN=auto go vet ./...`, `go vet -tags e2e ./...`, `gofmt -l .`, `go test -race -count=1 ./...`, golangci-lint (the repo's `.golangci.yaml`), against a fake mailbox client, fake order client and fake user client: list by session and by user (the exact `ListOrders` request, only owned ids sent on, foreign mails dropped), empty states, detail with the guard (another visitor, another user, an email of another order, a service that hands out a foreign mail, malformed or missing `order`: `404` and `GetOrderEmail` not called), iframe attributes and escaping (a body with `"`, `<script>`, `&quot;` and `</iframe>` decodes back to itself from one attribute value and nothing is live around it), no inline script in `<main>`, `no-store` on every answer, off and down (`404`, `503` without backend text, hung service ends quickly, other pages unaffected), links shown and hidden, the order-page button present, absent, newest mail, silent on error, 1 s deadline, no call for polls, CSS tokens and mobile rules present.
- **Not run:** against the real email-service (no real `MailboxService` yet), the compose stack, a browser (no screenshots, a real sandboxed iframe was not rendered, the integrator takes them), the e2e suite (`tests/e2e`, only vetted), a screen reader, the container image of this version.

## Ratings and reviews

Stars and reviews come from `review-service` (proto copied to `proto/reviews.proto`, stubs in `genproto/reviews*.pb.go`; only the `go_package` option differs from the service's own file). The catalogue's own `rating` / `review_count` fields are no longer shown.

- **Grid** (home, department, search, paging and the htmx fragment): each card with reviews shows stars and the count; one `GetRatingSummary` call per page, not one per product. Products without reviews show nothing.
- **Product page:** the rating under the title (links to `#reviews`), then the average, the review count, the reviews newest first (5 per page) and the "Write a review" form. Next to the average, five rating bars (below). Paging: plain "Older reviews" / "Newer reviews" links (`?rpage=N`); with htmx, "Show more reviews" appends the next page in place.
- **Rating bars (product page):** five rows from `RatingSummary.star_counts`, 5 stars on top: label, a bar whose width is the share of all reviews (count / total, one decimal) and the number of reviews at the right. The bars come from ONE extra `GetRatingSummary` call for the product (in parallel with `ListReviews`, same 400 ms deadline; the "load more" fragment makes none). Each row has a text alternative ("3 reviews rated 5 stars", "1 review rated 1 star"); the drawn parts are `aria-hidden` and the count is printed, so nothing depends on colour or length alone. Bars are left out when there are no reviews, when `star_counts` is missing (older review-service), does not have exactly 5 non-negative entries, does not add up to the review count, or does not match the count `ListReviews` just returned (a review arrived between the two calls) and when the summary call fails; the rest of the section is unchanged. Grid cards do not show bars. Styles in `static/styles/reviews.css` (theme tokens, so light, dark and the wide layout follow; `forced-colors` fallback).
- **Form:** name (1-80 characters), rating (radio buttons drawn as stars, arrow keys work), review text (1-2000 characters). The same limits as review-service are checked here first; errors are shown above the form and next to each field, the entered values are kept, and the post redirects back on success (post/redirect/get). Review text is always HTML-escaped (`html/template`); line breaks are kept by CSS.
- **Accessibility:** a rating reads "4.5 out of 5, 12 reviews" (the stars themselves are `aria-hidden`), form fields have labels, radio stars are real focusable inputs with a focus ring, colours come from the theme tokens (`--color-star` in `static/styles/reviews.css`) in light and dark.
- **CSRF:** like the other forms there is no token. Visitor state is in `SameSite=Strict` cookies and a review carries no identity; additionally a post with `Sec-Fetch-Site: cross-site` is refused.
- **Degradation:** the connection is lazy and `REVIEW_SERVICE_ADDR` defaults to `review-service:8084`, so a missing or down service never stops the pod or the pages: grid cards have no stars, the product page shows "Reviews are not available right now" (the form is still there and answers `503` if the service stays down), and reads are cut off after 400 ms. With `REVIEW_SERVICE_ADDR=` (empty) the reviews block and the stars are not rendered at all.
- **Checked:** `GOTOOLCHAIN=auto go vet ./... && go test -race ./...` (fake review client: one batched call, stars, service down or hung, switched off, validation, XSS, paging, fragment, post/redirect). By hand with headless Chromium against the real review-service image (seeded) and the frontend binary on the host with stub catalog/currency/cart backends: grid stars at 1280 px, product page with reviews, load more with htmx and with JavaScript off, invalid and valid post, arrow-key rating, light and dark, 390 px without horizontal scroll, review service stopped (200 without stars, post gives 503), `REVIEW_SERVICE_ADDR` unset (service not resolvable: 200, first request ~350 ms) and empty. Not run: the container image of this version, screen readers, contrast measured with a tool.
- **Not changed:** the "Top rated" sort still uses the catalogue's `rating` field, not review-service.

## Ads and recommendations

Ads come from `ad-service` (`proto/demo.proto` gained `Ad.id = 3` and the `RecordClick` / `GetAdStats` rpcs and messages, copied from the slice-7 contract; `GetAdStats` is not called by the frontend). `demo.pb.go` and `demo_grpc.pb.go` were regenerated with protoc-gen-go 1.34.2 / protoc-gen-go-grpc 1.5.1 built from the module cache (regenerating the old proto first gave the committed files byte for byte), `reviews*.pb.go` with 1.36.11 / 1.6.1.

- **`context_keys` of `GetAds`:** home page without parameters: none (the ad-service picks any ad); department page (`?category=`): the department id; search page (`?q=`): the search words, lower-cased, split on whitespace, at most 5; both together: the department id, then the words; product page: the product's categories, as before. The htmx results fragment loads no ad (unchanged).
- **Links:** `/ads/{id}/click?to=<url-escaped redirect_url>` (route table above). Ads off, no ad, or ad-service down or slow (100 ms): no ad slot, the page is unchanged.
- **Recommendations:** the product page passes the viewed product id, the cart page the ids of the cart lines that still exist in the catalogue (checked, no change needed); the order confirmation page passes none. Which products come back is recommendation-service's business.
- **Checked:** `GOTOOLCHAIN=auto go test -race -count=1 ./...` with fake ad, review and recommendation servers: bar arithmetic, rounding and markup, no bars without or with inconsistent `star_counts`, summary failure, context keys per page type, click link markup, 302 for local paths, open-redirect attempts (`//evil.com`, `/\evil.com`, `https://evil.com`, `javascript:`, tabs, newlines, NUL, DEL, C1 controls, invalid UTF-8, empty, missing, too long), `RecordClick` errors and a hung ad-service (delay bounded by the deadline), what the product and cart pages pass to `ListRecommendations`. Not run: against the real ad-service, review-service or recommendation-service, in a browser (screenshots are taken by the integrator), screen readers.

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
│   ├── ads.go           # ad context keys, click link, /ads/{id}/click with the local-path redirect check
│   ├── browse.go        # catalogue query parameters, paging, view models for catalog.html
│   ├── cart.go          # cart lines checked against the catalog (stale lines skipped), header count
│   ├── mailbox.go       # /emails and /emails/{id}: MailboxService client use, ownership guard, order-page link
│   ├── errors.go        # gRPC status -> error page mapping, cached department nav
│   ├── reviews.go       # review-service client use: rating summaries, review list, review form validation and POST handler
│   ├── middleware.go    # request logging + session-id cookie
│   ├── otlp.go          # OTLP/gRPC setup: trace, metric (Prometheus bridge) and log (logrus hook) providers
│   ├── telemetry.go     # Prometheus HTTP metrics, /metrics server, route wrapping, Pyroscope
│   └── *_test.go        # unit tests for handlers, middleware, rpc helpers and telemetry
├── tools/
│   └── check-contrast.py # WCAG contrast check of the colour tokens (stdlib Python)
├── internal/validator/  # form payload validation (go-playground/validator) + tests
├── money/               # Money arithmetic helpers + tests
├── genproto/            # generated gRPC stubs for all backend services (demo.pb.go, demo_grpc.pb.go; orders/, users/, inventory/ and mailbox/ are packages of their own)
├── templates/
│   ├── header.html, footer.html  # page shell: theme script, header (brand, nav, currency, theme toggle, cart), footer
│   ├── home.html, product.html, cart.html, order.html, error.html, account.html (sign in, register, account), mailbox.html (mail list, one mail)  # pages
│   ├── ad.html, recommendations.html  # shared blocks
│   ├── reviews.html     # stars, rating badge, reviews block, review items (htmx fragment), review form
│   ├── icons.html       # inline SVG brand mark and icons (no image files)
│   └── catalog.html     # department nav and tiles, search/sort toolbar, results fragment, pagination, product card
├── static/
│   ├── styles/          # styles.css (design tokens, light/dark, layout, components), cart.css, order.css, browse.css, reviews.css, orders.css, accounts.css, mailbox.css
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
- Live stock is only as fresh as the page: another visitor can take the last item between the page and the order. The order itself is safe (checkout reserves all or nothing and answers `422 out of stock`); the cart has no quantity editor, so a line that became too big is fixed by emptying the cart and adding again.
- `/setCurrency` redirects to the `Referer` header as is, and `/static/` lists directories without cache headers (found by the audit, not changed).

## Wide screens

The page container grows with the screen (1200 px up to 1359, 1440 px from 1360, 1680 px from 1720, 1960 px from 2200; `--container` in `static/styles/styles.css`), and the product and recommendation grids add columns with it (5 from 1360 px, 6 from 1720 px, 7 from 2200 px); the product gallery is up to 640 px wide from 1360 px. Phones and tablets are unchanged. Checked by hand with headless Chromium at 1920, 1440 and 390 px (home, product, cart/checkout with the United States and Germany); not run: 2200 px and wider, other browsers.
