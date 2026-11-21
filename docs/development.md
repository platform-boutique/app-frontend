# Develop & test

## Checks and commands

Requirements: Go 1.26 (`GOTOOLCHAIN=auto` fetches the `go.mod` version; the host has Go 1.22), Docker for the image, Python 3.12 for the docs check. The commands below are the ones the shared pipeline (`platform-demo-workflows`) runs, so a failure can be reproduced without pushing.

| Step | Command | Result on 2026-11-21 |
|---|---|---|
| Format | `gofmt -l .` | run, no files listed |
| Vet and unit tests | `go vet ./... && go test -race -count=1 ./...` | run, passed (4 packages with tests) |
| Lint | `golangci-lint run ./...` (the repo's `.golangci.yaml`; CI pins golangci-lint v2.14.0) | not run (golangci-lint is not installed on the host) |
| Vulnerabilities and security | `govulncheck ./...`, `gosec -exclude-generated ./...` | not run |
| Vet with the e2e tag | `go vet -tags e2e ./...` | run, no findings |
| End-to-end tests | `go test -tags e2e ./tests/e2e/` against a running shop (not run by CI) | not run |
| Faro check in a real browser | `tests/faro/run.sh` (needs the compose stack, Node and Playwright; places one real order) | not run |
| Colour contrast | `python3 -I tools/check-contrast.py` | run, all pairs pass |
| Proto | the `protoc` commands for the order, user, inventory and mailbox contracts are in the README sections Orders, Accounts, Live stock and Mailbox (protoc 3.21.12, protoc-gen-go 1.36.11, protoc-gen-go-grpc 1.6.1); `demo.proto` and `reviews.proto` are in `proto/`, stubs in `genproto/` | not run. Changes are additive only, server first |
| Image | `docker build -t platform-demo/frontend:local .` | not run today |
| Image scan | `trivy image --severity CRITICAL,HIGH --exit-code 1 platform-demo/frontend:local` | not run today |
| Secret scan | `pre-commit run --all-files` (`.pre-commit-config.yaml`, gitleaks) or `gitleaks git --redact` | run with the gitleaks binary of the pre-commit cache (`gitleaks git --redact`, full history, 113 commits): no leaks found |
| Workflow lint | `actionlint` | run (actionlint 1.7.12), no findings |
| Docs | `mkdocs build --strict` (mkdocs 1.6.1 + mkdocs-techdocs-core 1.7.1, Python 3.12 venv) and the link check `python3 -I tools/check-md-links.py .` from `platform-demo-workflows` (relative links in `README.md` and `docs/`) | run, both passed |

## Running locally

Requirements: Go 1.26 (`go.mod` says `go 1.26.11`; with an older local Go use `GOTOOLCHAIN=auto`), Docker, and the seven backend services reachable (the quickest way is `platform-demo-observability/docker-compose.yaml`).

```bash
# from source (note: entrypoint is ./cmd/server)
export PRODUCT_CATALOG_SERVICE_ADDR=localhost:3550 CURRENCY_SERVICE_ADDR=localhost:7000 \
       CART_SERVICE_ADDR=localhost:7070 RECOMMENDATION_SERVICE_ADDR=localhost:8081 \
       CHECKOUT_SERVICE_ADDR=localhost:5050 SHIPPING_SERVICE_ADDR=localhost:50051 \
       AD_SERVICE_ADDR=localhost:9555
go run ./cmd/server          # http://localhost:8080

# via Docker
docker build -t frontend .
docker run --rm -p 8080:8080 -p 9090:9090 \
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

curl -s localhost:8080/healthz             # ok
curl -s localhost:8080/readyz              # ok, or 503 "not ready: ..." while a backend is down
curl -s localhost:8080/version            # {"service":"frontend","version":...}
curl -s localhost:9090/metrics | grep http_requests_total
```

Browsing to `http://localhost:8080` works with the `Secure` cookies because browsers treat `localhost` as a secure context.

## Project structure

```
├── cmd/server/
│   ├── main.go          # entrypoint: env wiring, gRPC dials, router, middleware chain, HTTP server
│   ├── health.go        # /healthz, /readyz, /version and the `healthcheck` subcommand
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
├── tests/faro/          # real-browser check of the Faro RUM wiring (run.sh, not run by CI) and its throwaway OTLP sink
├── .github/workflows/   # ci.yaml (pull requests), release.yaml (v* tags / manual) - callers of platform-demo-workflows
├── Dockerfile
└── go.mod / go.sum / .golangci.yaml
```
