# Develop & test

## Checks and commands

Requirements: Go 1.26 (`GOTOOLCHAIN=auto` fetches the `go.mod` version; the host has Go 1.22), Docker for the image, Python 3.12 for the docs check. The commands below are the ones the shared pipeline (`platform-demo-workflows`) runs, so a failure can be reproduced without pushing.

| Step | Command | Result |
|---|---|---|
| Format | `gofmt -l .` | 2026-11-22: run on the final tree, no files listed |
| Vet and unit tests | `go vet ./... && go test -race -count=1 ./...` | 2026-11-22: run on the final tree, passed (4 packages with tests, `-race`); the obs test `TestGRPCRequestPathLogsCarryTraceIDs` 50 times with `-race`: passed; coverage with `-cover`: cmd/server 89.4%, internal/obs 88.2%, internal/validator 36.4%, money 68.8% (reported here only: the pipeline does not print it, no gate) |
| Lint | `golangci-lint run ./...` (the repo's `.golangci.yaml`; CI pins golangci-lint v2.14.0) | 2026-11-22: run with golangci-lint v2.14.0 (docker image) with `--build-tags e2e --max-same-issues 0` (covers all files): 0 issues, after fixing 3 `errcheck` findings in `cmd/server/budget_test.go` and the unchecked `resp.Body.Close` calls in `tests/e2e`; the run without the tag was not repeated after the fix |
| Vulnerabilities and security | `govulncheck ./...`, `gosec -exclude-generated ./...` | 2026-11-22: govulncheck: no vulnerabilities; gosec: 0 issues (34 files) |
| Vet with the e2e tag | `go vet -tags e2e ./...` | 2026-11-22: run, no findings |
| End-to-end tests | `FRONTEND_ADDR=http://localhost:8080 go test -tags e2e ./tests/e2e/` against a running shop (not run by CI; `FRONTEND_ADDR` needs the `http://` scheme) | 2026-11-22: run against the whole local shop with the final image, 16 tests passed |
| Faro check in a real browser | `tests/faro/run.sh` (needs the compose stack, Node and Playwright; places one real order) | 2026-11-22: run against the whole shop. Pass 2 (telemetry off), pass 3 (Faro off) and the bad-URL start check passed; pass 1 (Faro and server telemetry on) failed one step, `page.waitForResponse` for the htmx paging request timed out after 30 s (cause not found, not reproduced on its own; the same paging step passes in `csp-check.mjs` and in pass 2). `run.sh` pass 1 now sets `OTEL_SDK_DISABLED=false` (the compose environment switches telemetry off, so pass 1 could not turn it on) |
| CSP in a real browser | `tests/faro/csp-check.mjs` against a running frontend with Faro on (`MODE=report-only` or `MODE=enforce`; Node and Playwright with Chromium, no other part of the stack) | 2026-11-22: run against the final container image on the shop network (real backends), both modes: all checks passed, zero violations on the pages checked; the only report was the deliberate control script. The shop home, a product page and the cart (headless Chromium via the stack frontend) carry the `Content-Security-Policy-Report-Only` header and raised no violation event and no console error |
| Colour contrast | `python3 -I tools/check-contrast.py` | 2026-11-22: run, all pairs pass |
| Proto | the `protoc` commands for the order, user, inventory and mailbox contracts are in the README sections Orders, Accounts, Live stock and Mailbox (protoc 3.21.12, protoc-gen-go 1.36.11, protoc-gen-go-grpc 1.6.1); `demo.proto` and `reviews.proto` are in `proto/`, stubs in `genproto/` | not run. Changes are additive only, server first |
| gRPC contract (golden) | `go test ./cmd/server -run TestGRPCContract_Golden` (part of the unit tests; update on purpose with `-update-contract`) | 2026-11-22: run, passed |
| Image | `docker build --platform linux/amd64 -t platform-demo/frontend:local .` | 2026-11-22: built (amd64, base images by digest), 76.3 MB, user 10001:10001, exec-form HEALTHCHECK, healthy in the shop |
| Image scan | `trivy image --severity CRITICAL,HIGH --exit-code 1 platform-demo/frontend:local` | not run yet (queued behind other jobs when the work was stopped) |
| Secret scan | `pre-commit run --all-files` (`.pre-commit-config.yaml`, gitleaks) or `gitleaks git --redact` | 2026-11-22: gitleaks full history (126 commits): no leaks |
| Workflow lint | `actionlint` | 2026-11-21: run (actionlint 1.7.12), no findings |
| Docs | `mkdocs build --strict` (mkdocs 1.6.1 + mkdocs-techdocs-core 1.7.1, Python 3.12 venv) and the link check `python3 -I tools/check-md-links.py .` from `platform-demo-workflows` (relative links in `README.md` and `docs/`) | 2026-11-22: both passed (24 relative links in 18 pages, 0 broken) |
| Whole shop | `local/smoke.sh` with the final image in the compose stack | 2026-11-22: `SMOKE PASSED`, 87 steps, three runs in a row |

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
├── .github/workflows/   # ci.yaml (pull requests, merge button), publish.yaml (image, dev/stage/prod promotion) - callers of platform-demo-workflows
├── Dockerfile
└── go.mod / go.sum / .golangci.yaml
```
