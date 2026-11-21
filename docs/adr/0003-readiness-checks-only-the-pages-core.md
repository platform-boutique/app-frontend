# 3. Readiness checks only what every page needs

Status: accepted (from the comment in `cmd/server/health.go`).

## Context

`/readyz` decides whether a pod receives traffic. The frontend depends on up to twelve services; most of them break one page, not all of them.

## Decision

`/readyz` is `200` only while the gRPC connections to product-catalog, currency and cart are READY (the home, product, department and cart pages read them). Recommendation, ad, shipping, checkout and the optional services are not part of it. Liveness (`/healthz`) checks nothing.

## Consequences

- A failure of a non-core backend degrades single pages but never takes every frontend pod out of the Service.
- A pod can be `ready` while checkout is down; the checkout page then shows the `503` error page.
