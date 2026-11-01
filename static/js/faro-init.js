/*
 * Starts Grafana Faro (browser real-user monitoring). Loaded only when the server has
 * FARO_COLLECTOR_URL set (see templates/header.html and the README, "RUM (Grafana Faro)").
 *
 * It runs after the two vendored bundles, which define the globals GrafanaFaroWebSdk and
 * GrafanaFaroWebTracing. The server writes the settings into <script id="faro-config"> as JSON.
 */
(function () {
    'use strict';

    var sdk = window.GrafanaFaroWebSdk;
    var tracing = window.GrafanaFaroWebTracing;
    var block = document.getElementById('faro-config');
    if (!sdk || !tracing || !block) { return; } // a blocked script (ad blocker) simply means: no RUM

    var cfg;
    try { cfg = JSON.parse(block.textContent); } catch (e) { return; }

    // PRIVACY. Everything Faro is about to send goes through scrub() first. It blanks the VALUE of the
    // search parameter (?q=<text> becomes ?q=) and of ?order=, and replaces the id in /orders/<id> and
    // /emails/<id> with ":id". It looks at every string in the item (page url, span attributes,
    // events, error messages, stack traces), so no single field can be forgotten.
    function clean(text) {
        return text
            .replace(/([?&])(q|order)=[^&#\s"']*/g, '$1$2=')
            .replace(/(\/(?:orders|emails)\/)[^\/?#\s"']+/g, '$1:id');
    }
    function scrub(value, depth) {
        if (typeof value === 'string') { return clean(value); }
        if (value && typeof value === 'object' && depth < 12) {
            Object.keys(value).forEach(function (k) {
                var cleaned = scrub(value[k], depth + 1);
                // only write when something changed: some objects Faro hands us are read-only
                if (cleaned !== value[k]) { value[k] = cleaned; }
            });
        }
        return value;
    }
    function beforeSend(item) {
        try {
            item.payload = scrub(item.payload, 0);
            item.meta = scrub(item.meta, 0);
            return item;
        } catch (e) {
            return null; // if in doubt, send nothing
        }
    }

    // Path of a request without the query string, ids removed. Used for the htmx failure events.
    function pathOf(url) {
        var a = document.createElement('a');
        a.href = url;
        return clean(a.pathname);
    }

    var instance = sdk.initializeFaro({
        url: cfg.collectorUrl,
        app: { name: cfg.appName, version: cfg.appVersion, environment: cfg.environment },
        beforeSend: beforeSend,
        // The shop session id travels as a session attribute (the cookie itself is HttpOnly), plus
        // only whether somebody is signed in, never who. Faro attributes must be strings.
        sessionTracking: {
            enabled: true,
            persistent: true, // pages are full page loads: keep ONE Faro session across them
            samplingRate: cfg.sessionSampleRate,
            session: {
                attributes: {
                    shop_session_id: String(cfg.shopSessionId || ''),
                    shop_user_signed_in: String(!!cfg.shopUserSignedIn)
                }
            }
        },
        instrumentations: sdk.getWebInstrumentations({
            captureConsole: true,                // console.error (and warnings) as logs
            enablePerformanceInstrumentation: true // web vitals, navigation and resource timings
        }).concat([
            // Traces: document load (child of the server span in <meta name="traceparent">) and every
            // fetch / XHR, which includes the htmx requests. A `traceparent` header is added to
            // same-origin requests only: propagateTraceHeaderCorsUrls stays empty, so other origins
            // never get one. omitTraceContext...: with a sampled-out session no header is sent,
            // so the server decides on its own.
            new tracing.TracingInstrumentation({ omitTraceContextForUnsampledSessions: true })
        ])
    });

    // A resumed (sticky) session keeps the attributes it was created with; the shop session can change
    // in between (the "reset" link, a new cookie), so write the current values again.
    var current = (instance.metas.value && instance.metas.value.session) || {};
    instance.api.setSession({
        id: current.id,
        attributes: Object.assign({}, current.attributes, {
            shop_session_id: String(cfg.shopSessionId || ''),
            shop_user_signed_in: String(!!cfg.shopUserSignedIn)
        })
    });

    // The page load as a trace. Faro 2.13 ships fetch and XHR tracing but no document-load instrumentation
    // (the OpenTelemetry one needs a JavaScript build), so this makes the one span that matters by hand:
    // "documentLoad", from the start of the navigation to the end of the load event. Its parent is the
    // server span of this page, from <meta name="traceparent"> (only there when the server has telemetry on),
    // so the browser and the Go server show up as ONE trace.
    function traceDocumentLoad() {
        var otel = instance.api.getOTEL();
        var nav = performance.getEntriesByType('navigation')[0];
        if (!otel || !nav) { return; }
        var meta = document.querySelector('meta[name="traceparent"]');
        var tp = meta && /^00-([0-9a-f]{32})-([0-9a-f]{16})-([0-9a-f]{2})$/.exec(meta.content);
        var parent = otel.context.active();
        if (tp) {
            parent = otel.trace.setSpanContext(parent, { traceId: tp[1], spanId: tp[2], traceFlags: parseInt(tp[3], 16), isRemote: true });
        }
        var span = otel.trace.getTracer('everly-document-load').startSpan('documentLoad', {
            startTime: performance.timeOrigin + nav.startTime,
            attributes: { 'http.url': location.href }
        }, parent);
        var finish = function () {
            span.setAttribute('dom_content_loaded_ms', Math.round(nav.domContentLoadedEventEnd));
            span.end(performance.timeOrigin + (nav.loadEventEnd || performance.now()));
        };
        if (document.readyState === 'complete') { finish(); } else {
            // loadEventEnd is only filled in after the load handlers ran: wait one tick
            window.addEventListener('load', function () { setTimeout(finish, 0); });
        }
    }
    traceDocumentLoad();

    // One event per page view (the pages are server-rendered, so a navigation is a page load).
    instance.api.pushEvent('page_view', { path: pathOf(location.href) });

    // htmx requests that fail: status and path only, never the response body.
    function htmxFailed(name) {
        return function (ev) {
            var d = ev.detail || {};
            instance.api.pushEvent(name, {
                status: String((d.xhr && d.xhr.status) || 0),
                path: pathOf((d.pathInfo && d.pathInfo.requestPath) || (d.requestConfig && d.requestConfig.path) || '')
            });
        };
    }
    document.addEventListener('htmx:responseError', htmxFailed('htmx_response_error'));
    document.addEventListener('htmx:sendError', htmxFailed('htmx_send_error'));
})();
