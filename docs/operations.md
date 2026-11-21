# Operations

## Health

| Path | Meaning |
|---|---|
| `GET /healthz` (and `/_healthz`) | liveness: `200 ok` while the process serves HTTP; no dependency is checked |
| `GET /readyz` | readiness: `200 ok` while the gRPC connections to product-catalog, currency and cart are READY, else `503 not ready: <names>`; all checked in parallel, 1.5 s limit |
| `GET /version` | JSON `service`, `version`, `commit`, `build_time` |

Readiness deliberately leaves out recommendation, ad, shipping, checkout and the optional services: their failure degrades or fails single pages, and taking every pod out of the Service would turn that into a total outage ([ADR 3](adr/0003-readiness-checks-only-the-pages-core.md)). The container probe `/app/frontend healthcheck` calls `GET /healthz` on `127.0.0.1:$PORT` (3 s) and exits 0 only on `200`.

```bash
curl -s localhost:8080/readyz
curl -s localhost:8080/version
curl -s localhost:9090/metrics | grep http_requests_total
```

## Shutdown

On SIGTERM or SIGINT the server stops accepting, drains in-flight requests for at most 5 s (`drainTimeout`), closes what is left, then telemetry is flushed (cut off after 4 s). The process exits inside 10 s even with an unreachable collector.

## Scaling and replicas

Stateless: scale freely, no sticky sessions (the session is a cookie, carts are in cart-service). The currency cache is per replica, each replica warms its own. This is the one workload that needs external exposure.

## Failure behaviour

| Failure | Behaviour |
|---|---|
| A required backend is down at start | the service starts (lazy connections); `/readyz` is `503` while catalog, currency or cart is not READY |
| A backend hangs | the call ends at its deadline (3 s for catalog, currency, cart, recommendation and shipping; 15 s for `PlaceOrder`; 100 ms for ads; 400 ms / 2 s for review reads / writes) and the page answers `503` or leaves the block out |
| Optional service address empty | feature off: `404` for its routes, block hidden |
| Optional service down | its pages answer `503` (orders, mailbox, accounts); ratings, live stock and ads are skipped and the page still renders |
| Backend error | error page with no backend text; the request id in the footer finds the log line |
| Collector unreachable | telemetry export fails in the background; start-up does not wait for it |

## Runbook

| Symptom | Check |
|---|---|
| Cart looks empty after "Add to cart" | the cookies are `Secure`: use HTTPS or `localhost` (`kubectl port-forward`); plain HTTP at an IP or hostname drops them |
| `/readyz` is `503 not ready: <names>` | the named backend and its `*_SERVICE_ADDR` (plain `host:port`) |
| Out-of-stock page at checkout | checkout's saga answered `FAILED_PRECONDITION out of stock`; the stock shown on the page can be a moment old |
| `409 Your order is being processed` | checkout answered `ABORTED` for the same idempotency key; check Orders before retrying |
| Prices one cent off | see Money display in the README; amounts are rounded to the nearest minor unit |
| Process exits at start | one JSON error line, usually a missing required `*_SERVICE_ADDR` or an invalid `FARO_*` setting |

There is no dashboard or alert rule for this service in the repository yet (the dashboards are an open task).
