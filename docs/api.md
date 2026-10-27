# API

All routes are prefixed with `BASE_URL` (empty by default).

| Route | Method | Description |
|---|---|---|
| `/` | GET, HEAD | Home — product grid, currency picker, ad, cart size |
| `/product/{id}` | GET, HEAD | Product page — product, price, recommendations, ad |
| `/cart` | GET, HEAD | View cart — items, shipping estimate, total, recommendations |
| `/cart` | POST | Add item (form fields `product_id`, `quantity`) |
| `/cart/empty` | POST | Empty the cart |
| `/cart/checkout` | POST | Validate the form, call `checkout-service.PlaceOrder`, render the order page |
| `/setCurrency` | POST | Set the display currency cookie (`currency_code`) |
| `/logout` | GET | Expire all cookies and redirect to `/` |
| `/product-meta/{ids}` | GET | JSON of a single product (used by the page scripts) |
| `/static/*` | GET | CSS, favicon, product images (`static/img/products/`) |
| `/_healthz` | GET | Liveness/readiness (`ok`) |
| `/robots.txt` | GET | `Disallow: /` |

**Supported currencies** (whitelist in code, intersected with what `currency-service` offers): `USD`, `EUR`, `CAD`, `JPY`, `GBP`, `TRY`. Default is `USD`.

### Which backend each page calls

| Page | Calls |
|---|---|
| Home | currency (list + convert per product), product-catalog (list), cart (get), ad (100 ms deadline, failures ignored) |
| Product | product-catalog (get), currency, cart, recommendation → product-catalog (up to 4), ad |
| Cart | cart (get), shipping (quote with nil address), currency, product-catalog, recommendation |
| Checkout | checkout (PlaceOrder), then recommendation + currency for the confirmation page |

### Sessions & cookies

| Cookie | Purpose | Flags |
|---|---|---|
| `shop_session-id` | random UUID per visitor; used as the cart `user_id` | `HttpOnly`, **`Secure`**, `SameSite=Strict`, 48 h |
| `shop_currency` | chosen display currency | `HttpOnly`, **`Secure`**, `SameSite=Strict`, 48 h |

> **Kubernetes gotcha — cookies are `Secure`.** Browsers only store `Secure` cookies over HTTPS (or `http://localhost`). If you reach the frontend over plain HTTP at a LoadBalancer/NodePort IP or hostname, the session cookie is dropped, every request becomes a new session, and the cart appears to be empty after "Add to cart". Use `kubectl port-forward` (→ `localhost`), or terminate TLS at the ingress/gateway, or set `ENABLE_SINGLE_SHARED_SESSION=true` as a demo workaround (see below).
