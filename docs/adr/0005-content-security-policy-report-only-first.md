# 5. Content-Security-Policy: report-only first, reports to the service itself

Status: accepted (2026-11-22, audit row SEC-05).

## Context

The storefront sets cookies and renders visitor-supplied text (reviews, search), so a Content-Security-Policy is a useful second line behind output escaping. A policy that is switched on blocking without evidence breaks pages silently, and the pages had two inline scripts and an inline event handler that a strict `script-src` cannot allow. Browser telemetry (Grafana Faro) posts to a collector origin that the policy must allow.

## Decision

- Send the policy as `Content-Security-Policy-Report-Only` by default (`CSP_MODE=report-only`); `enforce` and `off` are one variable away, a typo stops the start instead of switching the policy off.
- Make the pages comply instead of weakening the policy: the inline scripts became files (`theme-init.js`, `shell.js`), htmx's inline indicator style is switched off. The one exception is `style-src-attr 'unsafe-inline'` (rating bars set a width in a style attribute); there is no `unsafe-inline` or `unsafe-eval` for scripts or style elements.
- `connect-src` gets the origin of `FARO_COLLECTOR_URL` and nothing else.
- Reports go to `POST /csp-report` of this service (`report-uri`): counted by directive, logged at most 10 per 10 s, bodies limited to 16 KiB, URLs stripped of query strings. No new service, and the reports land in the logs and metrics that already exist.

## Consequences

- Nothing is blocked until someone sets `CSP_MODE=enforce` after reading the reports of a real deployment; the Helm value is not part of this repository.
- `tests/faro/csp-check.mjs` and Go tests keep the pages compliant; a new inline script fails a test.
- `report-uri` is deprecated in the standard but is what browsers deliver reliably today; the receiver also reads the Reporting API format, so switching to `report-to` is a header change.
- The endpoint is public and unauthenticated like every other route; the rate limit on log lines and the bounded metric label keep it from being a log or cardinality flood.
