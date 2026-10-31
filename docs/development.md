# Develop & test

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
python3 tools/check-contrast.py      # WCAG contrast of the colour tokens, both themes; exits 1 on a failure

curl -s localhost:8080/_healthz            # ok
curl -s localhost:9464/metrics | grep http_requests_total
```

Browsing to `http://localhost:8080` works with the `Secure` cookies because browsers treat `localhost` as a secure context.

## Project structure

```
├── cmd/server/
│   ├── main.go          # entrypoint: env wiring, gRPC dials, router, middleware chain, HTTP server
│   ├── handlers.go      # page handlers (home, product, cart, checkout, currency, logout, product-meta)
│   ├── rpc.go           # gRPC client helpers for each backend
│   ├── accounts.go      # accounts: shop_auth middleware, register/login/account/logout, claim + cart merge, safe `next`
│   ├── middleware.go    # request logging + session-id cookie
│   ├── otlp.go          # OTLP/gRPC setup: trace, metric (Prometheus bridge) and log (logrus hook) providers
│   ├── telemetry.go     # Prometheus HTTP metrics, /metrics server, route wrapping, Pyroscope
│   └── *_test.go        # unit tests for handlers, middleware, rpc helpers and telemetry
├── internal/validator/  # form payload validation (go-playground/validator) + tests
├── money/               # Money arithmetic helpers + tests
├── genproto/            # generated gRPC stubs for all backend services (demo.pb.go, demo_grpc.pb.go; orders/ and users/ are packages of their own)
├── templates/
│   ├── header.html, footer.html  # page shell: theme script, header (brand, nav, currency, theme toggle, cart), footer
│   ├── home.html, product.html, cart.html, order.html, error.html, account.html (sign in, register, account)  # pages
│   ├── ad.html, recommendations.html  # shared blocks
│   ├── icons.html       # inline SVG brand mark and icons (no image files)
│   └── catalog.html     # department nav and tiles, search/sort toolbar, results fragment, pagination, product card
├── static/
│   ├── styles/          # styles.css (design tokens "Evergreen & Oat", light/dark, layout, components), cart.css, order.css
│   ├── favicon.svg
│   └── img/products/    # product photos
├── tests/e2e/           # HTTP end-to-end tests (not run by CI; the kind-based job was removed)
├── .github/workflows/   # ci.yaml (pull requests), release.yaml (v* tags / manual) - callers of platform-demo-workflows
├── Dockerfile
└── go.mod / go.sum / .golangci.yaml
```
