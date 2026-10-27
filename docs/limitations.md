# Known limitations

- `Secure` + `SameSite=Strict` cookies break carts over plain HTTP outside `localhost`.
- Health is static; readiness doesn't reflect backend availability.
- Debug-level JSON logging is unconditional.
- Per-request gRPC calls have no retries; the ad call has a 100 ms deadline, the rest use the request context.
