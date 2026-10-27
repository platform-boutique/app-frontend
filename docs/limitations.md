# Known limitations

- `Secure` + `SameSite=Strict` cookies break carts over plain HTTP outside `localhost`.
- Health is static; readiness doesn't reflect backend availability.
- Debug-level JSON logging is unconditional.
- Per-request gRPC calls have no retries; each has a deadline (3 s, 15 s for checkout, 100 ms for ads).
- Stock is shown and capped in the cart, but orders do not decrement it (inventory is a later slice).
