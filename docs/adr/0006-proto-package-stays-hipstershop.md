# 6. The proto package of demo.proto and reviews.proto stays `hipstershop`

Status: accepted exception (owner informed 2026-11-22; application standards section 3, rule CODE-PROTO).

## Context

The standard for a service's own contract is a package `hipstershop.<name>`. The frontend keeps copies of the contracts it calls. `proto/demo.proto` (catalog, cart, currency, recommendation, shipping, checkout, ad) and `proto/reviews.proto` (review-service) both declare `package hipstershop`, the package the shop's original services use; the newer contracts (`inventory`, `mailbox`, `orders`, `users`) already live in packages of their own. The `go_package` option of the two files still contains the spelling `miscroservices` of the upstream demo.

The package name is part of the gRPC method name on the wire (`/hipstershop.ProductCatalogService/...`). Renaming it breaks every old client and server pair (`UNIMPLEMENTED`) and is reported by `buf breaking` as a breaking change. The servers are other repositories; a rename would need a coordinated release of all of them and of the frontend. The `go_package` spelling does not appear on the wire, it only names the generated Go package; changing it would regenerate every stub for no behavioural gain.

## Decision

Keep `package hipstershop` and the `go_package` value for `demo.proto` and `reviews.proto`. Changes to these files stay additive (new fields or RPCs, no renumbering, no removals), server first. A golden test (`TestGRPCContract_Golden`, `cmd/server/testdata/grpc-contract.golden`) pins the methods and messages the frontend speaks, and the pipeline's `buf breaking` check guards the files against the default branch.

## Consequences

- The package names of the older and the newer contracts differ. This is a recorded exception, not an oversight.
- If the package is ever renamed, the services change first, the frontend's copies in the same release, with a new package version and a deprecation window.
