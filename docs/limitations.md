# Known limitations

- `Secure` + `SameSite=Strict` cookies break carts over plain HTTP outside `localhost`.
- Health is static; readiness doesn't reflect backend availability.
- The log level comes from `LOG_LEVEL` (default `info`); detail lines of the handlers need `LOG_LEVEL=debug`.
- Per-request gRPC calls have no retries; each has a deadline (3 s, 15 s for checkout, 100 ms for ads).
- Live stock is only as fresh as the page: another visitor can take the last item between the page and the order. The order itself is safe (checkout reserves all or nothing and answers `422 out of stock`); the cart has no quantity editor, so a line that became too big is fixed by emptying the cart and adding again.
