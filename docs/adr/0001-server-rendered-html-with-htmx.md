# 1. Server-rendered HTML with htmx

Status: accepted (reconstructed from the code and the earlier README).

## Context

The storefront needs search, paging, a cart and live order tracking, but no client-side application state: carts, orders and accounts live in backend services.

## Decision

Every page is rendered on the server with `html/template`. Small interactions (catalogue paging and sorting, "load more" reviews, order tracking refresh) use htmx, which is vendored in `static/js/` and asks the same routes for a fragment (`HX-Request`); the review "load more" was checked with JavaScript off (README). There is no SPA framework and no build step for the front end.

## Consequences

- No Node toolchain is needed to build the image; the front end is templates, CSS and one small vendored script.
- Every page is a fan-out of backend calls from the server, so each call needs a deadline and each page its own failure behaviour (see [Operations](../operations.md)).
- The scripts in `static/js/` are htmx, `gallery.js`, `checkout.js` (the State field of the checkout form) and Faro (`faro-init.js`, referenced only while `FARO_COLLECTOR_URL` is set).
