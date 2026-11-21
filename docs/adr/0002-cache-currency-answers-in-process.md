# 2. Cache currency-service answers in the process

Status: accepted (reconstructed from `cmd/server/currency_cache.go` and the earlier README).

## Context

A listing page shows 24 prices, each converted by `currency-service` with one RPC. currency-service converts through EUR in floating point and floors units and nanos separately, and its API exposes no rate, so a rate computed locally could not reproduce its output exactly.

## Decision

Cache the service's own answers in the frontend process: the supported-currency list for 5 minutes, each `Convert` result for 60 seconds, keyed by (from, units, nanos, to). Concurrent identical lookups share one RPC. Errors are never cached, the convert map is bounded (4096 entries), and a same-currency conversion is answered locally.

## Consequences

- Money values stay byte-identical to what currency-service answers.
- The cache is per replica; a rate change shows after at most 60 s; a cold page still makes about 24 RPCs. Removing that needs a batch or rate RPC in currency-service (an additive proto change, not done).
- The metric `frontend_currency_cache_total{op,result}` shows hits, misses and shared calls.
