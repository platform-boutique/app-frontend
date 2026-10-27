# API

All routes are prefixed with `BASE_URL` (empty by default).

| Route | Method | Description |
|---|---|---|
| `/` | GET, HEAD | Home and catalogue — hero and department tiles (landing only), search / sort toolbar, product grid (24 per page), pagination, currency picker, ad, cart size. Query parameters: `category` (department id), `q` (search text, 100 characters at most; invalid UTF-8 is dropped), `sort` (`name` default, `price_asc`, `price_desc`, `rating`), `page` (1-based). With `HX-Request` it returns only the `#catalog-results` fragment (plus a `<title>`); when the view would change the hero or heading (landing <-> search, category or page 2) it answers `HX-Redirect` instead |
| `/product/{id}` | GET, HEAD | Product page — product, gallery, price, stock, quantity select (1 to min(stock, 10)), recommendations, ad. Unknown id: `404` page |
| `/cart` | GET, HEAD | View cart — items, shipping estimate, total, recommendations. Lines whose product is no longer in the catalog (a cart outlives the catalog) are skipped, listed in a notice with an Empty cart button, logged once per request and not counted in the header |
| `/cart` | POST | Add item (form fields `product_id`, `quantity` 1-10). `422` with a readable message when the product is out of stock or quantity plus what is already in the cart exceeds the stock; unknown product: `404`. Stock is only checked here, orders do not decrement it yet |
| `/cart/empty` | POST | Empty the cart |
| `/cart/checkout` | POST | Validate the form, call `checkout-service.PlaceOrder`, render the order page. The country is a dropdown (the names shipping-service accepts, United States first and preselected); the State field exists only for the United States: `static/js/checkout.js` hides and disables it for any other country, and the server ignores a state sent for another country, normalises `US`/`USA` to `United States` and requires a state for it |
| `/product/{id}/reviews` | POST | Post a review (form fields `author_name`, `rating` 1-5, `comment`). Valid: `303` back to `/product/{id}?reviewed=1#reviews`. Invalid: the product page again with `422` and inline errors; review service not reachable: `503` with the form kept; reviews switched off or unknown product: `404`; `Sec-Fetch-Site: cross-site`: `403` |
| `/orders` | GET, HEAD | Order history of the visitor's session: confirmed orders only (`ListOrders` with `placed_only`), newest first, 10 per page (`?page=`, a page past the end shows the last page). Each row: short id, date, status badge, item count, thumbnails, total, link. Friendly empty state. Order service down: `503` page "Orders are not available right now"; `ORDER_SERVICE_ADDR` empty: `404` |
| `/orders/{id}` | GET, HEAD | Tracking page of one order of the session (`GetOrder` with the session as guard): status badge, stage tracker, items, shipping address, tracking number, totals. An unknown, somebody else's or malformed id gives the same `404` page. With `HX-Request` it returns only the live part (badge, tracker, tracking number); `200` while the order is on its way, `286` once it is delivered or failed (htmx stops polling) |
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
| Home | currency (list + convert per product), product-catalog (`ListCategories` + `ListProductsFiltered`, 24 per page), cart (get, then product-catalog `GetProduct` per cart line for the header count), ad (100 ms deadline, failures ignored) |
| Product | product-catalog (get), currency, cart, recommendation → product-catalog (up to 4), ad |
| Cart | cart (get), shipping (quote with nil address), currency, product-catalog, recommendation |
| Checkout | checkout (PlaceOrder), then recommendation + currency for the confirmation page |

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
