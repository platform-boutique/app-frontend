# 4. Cache product lookups in the process instead of adding a batch RPC

Status: accepted (2026-11-22, audit rows PERF-02 and PERF-03).

## Context

The header cart count is on every page and the cart page lists the lines, so every request asked product-catalog for one product per cart line (`GetProduct`); the recommendations asked once per recommended id; the cart merge at sign-in asked one by one. A cart of three lines and four recommendations made 7 to 8 catalog calls per page view, warm or not. product-catalog-service has no batch RPC.

## Decision

Do not change the proto. Fetch the lookups in parallel (`fetchProducts`) and put the cart-line and recommendation lookups behind a 30 s in-process cache (`cmd/server/product_cache.go`): bounded (1024), singleflight, injected clock, `NotFound` kept for the TTL, other errors never cached, callers get copies. The product page itself, add to cart, the review form and the cart merge keep reading the catalog directly. The generic part is shared with the currency cache (`ttlCache`, [ADR 2](0002-cache-currency-answers-in-process.md)).

## Consequences

- Warm pages make no product lookups (home 33 to 6 RPCs cold to warm, cart 16 to 4, product 17 to 8); `TestRPCBudgetPerPage` pins the numbers.
- The header count and the cart page can show a catalog change (name, price, stock without inventory-service) up to 30 s late; checkout always uses the catalog's current data.
- Per replica, like the currency cache. A batch `GetProducts` RPC (additive, server first) would still make a cold cart page cheaper; it is not needed for the warm path.
- Metric `frontend_product_cache_total{op,result}`.
