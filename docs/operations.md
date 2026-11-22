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

Stateless: scale freely, no sticky sessions (the session is a cookie, carts are in cart-service). The currency cache and the product cache are per replica, each replica warms its own. This is the one workload that needs external exposure.

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
| Process exits at start | one JSON error line, usually a missing required `*_SERVICE_ADDR` or an invalid `FARO_*` or `CSP_MODE` setting |
| Header cart count or cart page shows an old product name, price or stock | the product cache keeps a product for 30 s (per replica); wait or look at `frontend_product_cache_total` |
| `content security policy violation reported by a browser` in the logs | a page loaded something the policy does not allow; `csp.directive`, `csp.blocked` and `csp.document` say what and where (`blocked=inline` with an empty `csp.source` is often a browser extension). Report-only blocks nothing; fix the page before `CSP_MODE=enforce` ([ADR 5](adr/0005-content-security-policy-report-only-first.md)) |
| Pages broke after `CSP_MODE=enforce` | set `CSP_MODE=report-only` (or `off`) and restart; read the reports |

There is no dashboard or alert rule for this service in the repository yet (the dashboards are an open task).

## Content-Security-Policy

A Content-Security-Policy for the pages, **report-only by default**: the browser reports what the policy would have blocked and blocks nothing. `CSP_MODE=report-only` (default, also for an empty value), `enforce` (blocking `Content-Security-Policy` header) or `off` (no header); any other value stops the process at start. Assets, probes and `/csp-report` itself get no policy header (they are not documents); every page, including error pages and 404s, does.

```
default-src 'self'; script-src 'self'; style-src 'self'; style-src-attr 'unsafe-inline';
img-src 'self' data:; font-src 'self'; connect-src 'self' <origin of FARO_COLLECTOR_URL>;
object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'; report-uri /csp-report
```

- **Scripts and style elements are strict.** The pages had two inline scripts and one inline `onchange`; they are now files (`static/js/theme-init.js`, blocking in the head so the theme is applied before the first paint, and `static/js/shell.js`, deferred: theme toggle and the currency select). htmx would add an inline `<style>` element for its indicator: turned off with `"includeIndicatorStyles":false` in the `htmx-config` meta tag (the page's CSS styles `.htmx-request` itself). The Faro settings are a JSON data block (`<script type="application/json">`), which the policy does not treat as script. A Go test (`TestCSP_TemplatesAndScriptsAreStrictPolicyClean`) fails when a template gets an inline script, an inline event handler, a `<style>` element or a `javascript:` URL.
- **One exception: style attributes** (`style-src-attr 'unsafe-inline'`): the rating bars and stars set a `width` in a `style` attribute. Style attributes cannot run code; the exception is limited to attributes, not to `<style>` elements.
- **Faro keeps working.** The collector (`FARO_COLLECTOR_URL`, where the browser posts its payloads, e.g. `http://localhost:12347` in the local stack) is the one extra origin in `connect-src` (scheme and host only, not the path). Everything else Faro does is same-origin (the `traceparent` header of htmx requests, the scripts under `/static/js/vendor/faro/`). In a real Chromium the Faro SDK and tracing bundle caused no violation and its payloads reached the collector with the policy in both modes (at the end of the list).
- **The mailbox iframe** (`/emails/{id}`) is a sandboxed `srcdoc` that inherits the policy; the mails of email-service use inline `style=` attributes only (no `<style>`, no `http`, see its rendering test), so they are covered by the attribute exception. Not run in a browser (see "Not run" in the list).
- **Reports** go to `POST /csp-report` of this service (`report-uri`; the receiver also reads the Reporting API format, so a later `report-to` needs no server change, but only `report-uri` was verified: in the headless Chromium used here the batched `report-to` reports were not delivered within the test window). Each report is counted in `frontend_csp_reports_total{directive}` (the label is a known directive name or `other`, never client text) and, for at most 10 reports per 10 s, logged as one `warning` line `content security policy violation reported by a browser` with `csp.directive`, `csp.blocked`, `csp.document`, `csp.source`, `csp.line`, `csp.disposition` (URLs reduced to scheme, host and path: no query string, 200 characters at most). A report body larger than 16 KiB is refused. In Grafana: Loki `{service_name="frontend"} |= "content security policy violation"` and the counter above.
- **Going to `enforce`:** run report-only in the target environment for a while, watch the counter and the log line (browser extensions produce `blocked=inline`/`eval` noise from their own injected code; look at `csp.source` and `csp.document`), fix what the application causes, then set `CSP_MODE=enforce` (a Helm value of the deployment, not part of this repository). Rolling back is `CSP_MODE=report-only` or `off`.
- **Checked** (2026-11-22): `go test -race` (policy text, Faro origin, header per mode and path, the real handler stack, both report formats, log budget with an injected clock, rejects, template and script sources) and `tests/faro/csp-check.mjs` against the real handler stack on the host (a throwaway Go test server over the in-process fakes, Faro on with a fake collector) in Chromium with Playwright: `report-only` and `enforce` both end with zero `securitypolicyviolation` events on the home, product and cart pages, theme toggle, theme memory, currency select and htmx paging working, 25 Faro payloads at the collector, and an injected inline script reported (and, in `enforce`, not run). One real browser report reached `/csp-report` and was logged. **Not run:** the container image, the local compose/Grafana stack and the Faro collector of Alloy (the collector in the check is a fake), a browser other than Chromium, a signed-in visitor, the mailbox page, `report-to` delivery, the Helm deployment.
