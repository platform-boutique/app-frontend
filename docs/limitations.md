# Known limitations

- `Secure` + `SameSite=Strict` cookies break carts over plain HTTP outside `localhost`.
- Liveness (`/healthz`) checks nothing; readiness (`/readyz`) looks only at the catalog, currency and cart connections, not at the other backends (deliberate).
- The log level comes from `LOG_LEVEL` (default `info`); detail lines of the handlers need `LOG_LEVEL=debug`.
- Per-request gRPC calls have no retries; each has a deadline (3 s, 15 s for checkout, 100 ms for ads).
- Live stock is only as fresh as the page: another visitor can take the last item between the page and the order. The order itself is safe (checkout reserves all or nothing and answers `422 out of stock`); the cart has no quantity editor, so a line that became too big is fixed by emptying the cart and adding again.
- Product cache: the header cart count and the cart page can show a catalog change up to 30 s late. A cold listing page still makes about 23 currency conversions (one RPC each, in parallel); a batch or rate RPC in currency-service would remove that (additive proto change, server first, not done).
- Content-Security-Policy is report-only by default and enforced nowhere until `CSP_MODE=enforce` is set; style attributes are allowed (`style-src-attr 'unsafe-inline'`).
