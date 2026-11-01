// Real-browser check of the Faro RUM wiring (README, "RUM (Grafana Faro)"). Normally started by run.sh,
// which builds the image, starts the frontend (and an OTLP sink) in throwaway containers and then calls this.
//
//   MODE=full          Faro on, server telemetry on: every assertion, incl. trace correlation with the server spans
//   MODE=no-telemetry  Faro on, server telemetry off (as in compose): no traceparent meta tag, RUM still works
//   MODE=faro-off      Faro off: no Faro script/meta in the page, nothing reaches the collector
//
// Env: BASE_URL (http://localhost:8091), COLLECTOR_PORT (8092), SINK_CONTAINER (full mode: `docker logs` of the
// OTLP sink), PLAYWRIGHT_DIR (directory holding node_modules/playwright), PLACE_ORDER=0 to skip the checkout step.
// Run with: env -u ELECTRON_RUN_AS_NODE node faro-check.mjs
import { createRequire } from 'node:module';
import { execFileSync } from 'node:child_process';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { startCollector } from './fake-collector.mjs';

const here = path.dirname(fileURLToPath(import.meta.url));
const require = createRequire(path.join(process.env.PLAYWRIGHT_DIR ?? path.join(here, '../../../../local'), 'x.js'));
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL ?? 'http://localhost:8091';
const PORT = Number(process.env.COLLECTOR_PORT ?? 8092);
const MODE = process.env.MODE ?? 'full';
const SINK = process.env.SINK_CONTAINER ?? '';
const PLACE_ORDER = process.env.PLACE_ORDER !== '0';
const NEEDLE = 'zebraneedle';   // the search text that must never reach the collector
const CARD = '4111111111111111';
const STREET = '1 Market Street';

let failed = 0;
const check = (name, ok, detail = '') => {
  console.log(`${ok ? 'PASS' : 'FAIL'}  ${name}${detail ? '  ' + detail : ''}`);
  if (!ok) failed++;
};
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const collector = await startCollector(PORT);
const browser = await chromium.launch();
const ctx = await browser.newContext({ viewport: { width: 1280, height: 900 } });
const page = await ctx.newPage();

// what the browser itself saw: requests with their headers, console errors
const sent = [];
const consoleErrors = [];
page.on('console', (m) => { if (m.type() === 'error' && !m.text().includes('500') && !m.text().includes('ERR_FAILED')) consoleErrors.push(m.text()); });
page.on('pageerror', (e) => consoleErrors.push(String(e)));
page.on('request', async (req) => {
  try { sent.push({ url: req.url(), method: req.method(), headers: await req.allHeaders() }); } catch { /* page closed */ }
});

const footerSession = async () => (await page.locator('.footer-debug').innerText()).match(/session-id: ([0-9a-f-]{36})/)?.[1];
const metaTraceparent = () => page.evaluate(() => document.querySelector('meta[name="traceparent"]')?.content ?? null);
const settle = async () => { await page.waitForLoadState('load'); await sleep(600); };
const firstProduct = () => page.locator('a[href^="/product/"]').first().getAttribute('href');

async function finish() {
  await browser.close();
  await collector.close();
  console.log(failed === 0 ? '\nALL CHECKS PASSED' : `\n${failed} CHECK(S) FAILED`);
  process.exit(failed === 0 ? 0 : 1);
}

try {
  // ---- 1. home ----
  await page.goto(BASE + '/');
  await settle();
  const session = await footerSession();
  const homeTP = await metaTraceparent();
  const hasFaroScripts = await page.evaluate(() => [...document.scripts].some((s) => s.src.includes('faro')));
  const hasConfig = await page.evaluate(() => !!document.getElementById('faro-config'));

  if (MODE === 'faro-off') {
    check('Faro off: no faro script tags', !hasFaroScripts);
    check('Faro off: no faro-config block', !hasConfig);
    check('Faro off: no traceparent meta tag', homeTP === null);
    check('Faro off: window.faro not defined', await page.evaluate(() => typeof window.faro === 'undefined'));
    await page.goto(BASE + (await firstProduct()));
    await settle();
    await sleep(1500);
    check('Faro off: nothing reached the collector', collector.payloads.length === 0, `${collector.payloads.length} payloads`);
    check('Faro off: no traceparent header sent by the browser', sent.every((r) => !r.headers.traceparent));
    await finish();
  }

  check('Faro scripts and config block present', hasFaroScripts && hasConfig);
  const cfg = await page.evaluate(() => JSON.parse(document.getElementById('faro-config').textContent));
  check('config block session id equals the footer session id', !!session && cfg.shopSessionId === session, session);
  check('config block says: not signed in (boolean only)', cfg.shopUserSignedIn === false && !JSON.stringify(cfg).includes('@'));

  if (MODE === 'no-telemetry') {
    check('server telemetry off: no traceparent meta tag', homeTP === null);
  } else {
    check('server telemetry on: traceparent meta tag, W3C format, sampled',
      /^00-[0-9a-f]{32}-[0-9a-f]{16}-01$/.test(homeTP ?? ''), homeTP ?? 'missing');
  }

  // ---- 2. product page (a full page load) ----
  const productHref = await firstProduct();
  await page.goto(BASE + productHref);
  await settle();
  const productTP = await metaTraceparent();
  if (MODE === 'full') check('each page has its own server span', !!productTP && productTP !== homeTP && productTP.split('-')[1] !== homeTP.split('-')[1]);

  // ---- 3. htmx requests on the home page: paging, search, then two forced failures ----
  await page.goto(BASE + '/');
  await settle();
  const htmxReqs = () => sent.filter((r) => r.headers['hx-request'] === 'true');
  await page.locator('a.page-next').first().click();
  await page.waitForResponse((r) => r.url().includes('page=2') && r.request().headers()['hx-request'] === 'true');
  await sleep(500);
  await page.locator('#q').fill(NEEDLE);
  await page.locator('#q').press('Enter');
  await page.waitForResponse((r) => r.url().includes('q=' + NEEDLE) && r.request().headers()['hx-request'] === 'true');
  await sleep(500);
  await page.route((u) => u.search.includes('boom=1'), (route) => route.fulfill({ status: 500, contentType: 'text/plain', body: 'secret failure body' }));
  await page.route((u) => u.search.includes('boom=2'), (route) => route.abort());
  await page.evaluate(() => window.htmx.ajax('GET', '/?boom=1&q=zebraneedle', { target: '#page-alert', swap: 'innerHTML' }));
  await sleep(500);
  await page.evaluate(() => window.htmx.ajax('GET', '/orders/00000000-0000-0000-0000-000000000000?boom=2', { target: '#page-alert', swap: 'innerHTML' }));
  await sleep(500);

  // ---- 4. cross-origin request: must NOT get a traceparent ----
  const other = `http://127.0.0.1:${PORT}/ping`; // another origin than BASE (127.0.0.1 vs localhost, other port)
  await page.evaluate((u) => fetch(u).then((r) => r.text()), other);
  await sleep(500);

  // ---- 5. add to cart (plain form post), checkout, order page with the live poll ----
  let orderId = null;
  if (PLACE_ORDER) {
    await page.goto(BASE + productHref);
    await page.locator('form[action$="/cart"] button[type="submit"]').first().click();
    await page.waitForLoadState('load');
    await page.goto(BASE + '/cart');
    const fill = async (name, value) => {
      const el = page.locator(`[name="${name}"]`).first();
      if ((await el.evaluate((e) => e.tagName)) === 'SELECT') await el.selectOption(String(value)); else await el.fill(String(value));
    };
    const year = String(new Date().getFullYear() + 2);
    await fill('email', 'someone@example.com'); await fill('street_address', STREET); await fill('zip_code', '94043');
    await fill('city', 'Springfield'); await fill('state', 'CA'); await fill('country', 'United States');
    await fill('credit_card_number', CARD); await fill('credit_card_expiration_month', '1'); await fill('credit_card_expiration_year', year);
    await fill('credit_card_cvv', '672');
    await page.locator('form[action$="/cart/checkout"] button[type="submit"]').first().click();
    await page.getByText('Your order is complete').waitFor({ timeout: 20000 });
    orderId = (await page.locator('body').innerText()).match(/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/g)?.find((u) => u !== session);
    check('order placed in the browser', !!orderId, orderId);
    await page.goto(`${BASE}/orders/${orderId}`);
    await settle();
    // the status block polls its own URL every 15 s with htmx
    const poll = await page.waitForResponse((r) => r.url().includes(`/orders/${orderId}`) && r.request().headers()['hx-request'] === 'true', { timeout: 25000 }).catch(() => null);
    check('order page polled itself with htmx', !!poll);
    await sleep(500);
  }

  // ---- 6. leave the page so web vitals are flushed, wait for the collector ----
  await page.goto('about:blank');
  await sleep(2500);

  // ======== assertions on what the collector received ========
  const bodies = collector.payloads.map((p) => p.body);
  const everything = JSON.stringify(bodies);
  const metas = bodies.map((b) => b.meta).filter(Boolean);
  const events = bodies.flatMap((b) => b.events ?? []);
  const measurements = bodies.flatMap((b) => b.measurements ?? []);
  const spans = bodies.flatMap((b) => (b.traces?.resourceSpans ?? []).flatMap((rs) => (rs.scopeSpans ?? []).flatMap((ss) => ss.spans ?? [])));
  const attr = (span, key) => {
    const a = (span.attributes ?? []).find((x) => x.key === key);
    return a ? (a.value.stringValue ?? a.value.intValue ?? a.value.boolValue) : undefined;
  };
  const urlOf = (span) => String(attr(span, 'http.url') ?? attr(span, 'url.full') ?? attr(span, 'http.target') ?? '');

  check('collector received payloads', collector.payloads.length > 0, `${collector.payloads.length} payloads, ${events.length} events, ${spans.length} spans`);

  // session
  const sessionMetas = metas.filter((m) => m.session);
  const sessionIds = new Set(sessionMetas.map((m) => m.session.id));
  const withAttr = sessionMetas.filter((m) => m.session.attributes?.shop_session_id === session);
  check('session carries attribute shop_session_id = footer session id', withAttr.length > 0 && withAttr.length === sessionMetas.length,
    `${withAttr.length}/${sessionMetas.length} payloads`);
  check('session attribute shop_user_signed_in is "false" (a boolean, no user id)', sessionMetas.every((m) => m.session.attributes?.shop_user_signed_in === 'false'));
  check('ONE Faro session across all full page loads', sessionIds.size === 1, [...sessionIds].join(','));
  check('app meta: name, version, environment', metas.every((m) => m.app?.name === 'everly-frontend' && m.app?.version === 'dev' && m.app?.environment === 'local'), JSON.stringify(metas[0]?.app));

  // events
  const names = new Set(events.map((e) => e.name));
  check('event session_start', names.has('session_start'));
  const pageViews = events.filter((e) => e.name === 'page_view').map((e) => e.attributes?.path);
  check('page_view events for the pages visited', pageViews.includes('/') && pageViews.some((p) => p?.startsWith('/product/')) && (pageViews.includes('/orders/:id') === PLACE_ORDER),
    pageViews.join(' '));
  check('navigation timing event (faro.performance.navigation)', names.has('faro.performance.navigation'));
  const vitals = measurements.filter((m) => m.type === 'web-vitals');
  check('web vitals measurements', vitals.length > 0, [...new Set(vitals.flatMap((m) => Object.keys(m.values ?? {})))].join(','));
  const respErr = events.find((e) => e.name === 'htmx_response_error');
  const sendErr = events.find((e) => e.name === 'htmx_send_error');
  check('htmx_response_error event: status 500 and path only', respErr?.attributes?.status === '500' && respErr?.attributes?.path === '/' && Object.keys(respErr.attributes).length === 2, JSON.stringify(respErr?.attributes));
  check('htmx_send_error event: status 0 and path with the order id removed', sendErr?.attributes?.status === '0' && sendErr?.attributes?.path === '/orders/:id', JSON.stringify(sendErr?.attributes));
  check('no response body in any payload', !everything.includes('secret failure body'));

  // traces
  if (MODE === 'full') {
    const [, traceId, serverSpanId] = homeTP.split('-');
    const inTrace = spans.filter((s) => s.traceId === traceId);
    check('a browser span has the trace id of the page\'s traceparent meta tag', inTrace.length > 0, `${inTrace.length} spans in trace ${traceId}`);
    const rootDoc = inTrace.find((s) => s.name === 'documentLoad');
    check('documentLoad span is a child of the server span (parentSpanId = meta span id)', rootDoc?.parentSpanId === serverSpanId, `parent ${rootDoc?.parentSpanId} meta ${serverSpanId}`);
  } else {
    check('no-telemetry: documentLoad span starts its own trace', spans.some((s) => s.name === 'documentLoad' && !s.parentSpanId));
  }

  // htmx requests carry traceparent, same origin only
  const hx = htmxReqs().filter((r) => !r.url.includes('boom=2'));
  check('every htmx request carried a traceparent header', hx.length >= 3 && hx.every((r) => /^00-[0-9a-f]{32}-[0-9a-f]{16}-0[01]$/.test(r.headers.traceparent ?? '')), `${hx.length} htmx requests`);
  const hxSpans = hx.map((r) => {
    const [, t, s] = (r.headers.traceparent ?? '--').split('-');
    return spans.find((sp) => sp.traceId === t && sp.spanId === s);
  });
  check('each htmx traceparent is the id of a browser XHR/fetch span in the collector', hxSpans.length > 0 && hxSpans.every(Boolean), `${hxSpans.filter(Boolean).length}/${hxSpans.length}`);
  const otherReq = sent.find((r) => r.url === other);
  check('cross-origin request got NO traceparent header', !!otherReq && !otherReq.headers.traceparent, otherReq ? 'request seen' : 'request not seen');
  check('the other origin never saw a traceparent (nor a preflight asking for it)', collector.pings.length > 0 && collector.pings.every((p) => !p.headers.traceparent && !(p.headers['access-control-request-headers'] ?? '').includes('traceparent')), `${collector.pings.length} pings`);
  check('requests to the collector itself carry no traceparent', sent.filter((r) => r.url.includes('/collect')).every((r) => !r.headers.traceparent));

  // server side: the frontend's own spans prove the header arrived
  if (MODE === 'full') {
    await sleep(6000); // the frontend's batch span processor exports every 5 s
    const serverSpans = execFileSync('docker', ['logs', SINK], { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] })
      .split('\n').filter((l) => l.startsWith('{')).map((l) => JSON.parse(l)).filter((s) => s.service === 'frontend');
    const [, traceId, serverSpanId] = homeTP.split('-');
    check('the page\'s meta tag is a real exported server span (sink saw trace+span id)', serverSpans.some((s) => s.traceId === traceId && s.spanId === serverSpanId), `${serverSpans.length} frontend spans in the sink`);
    const arrived = hx.map((r) => {
      const [, t, s] = r.headers.traceparent.split('-');
      return serverSpans.some((sp) => sp.traceId === t && sp.parentSpanId === s);
    });
    check('server received the htmx traceparent: its span is a child of the browser span', arrived.length > 0 && arrived.every(Boolean), `${arrived.filter(Boolean).length}/${arrived.length}`);
  }

  // privacy
  check('search text never left the browser', !everything.includes(NEEDLE));
  check('a URL with the q parameter was captured with its value blank (proves the scrub ran)',
    spans.some((s) => /[?&]q=(["&#]|$)/.test(urlOf(s))) || /[?&]q=(\\"|&|#)/.test(everything),
    spans.map(urlOf).filter((u) => u.includes('q=')).slice(0, 2).join(' '));
  if (PLACE_ORDER) {
    check('order id never left the browser', !everything.includes(orderId));
    check('path shows /orders/:id instead', everything.includes('/orders/:id'));
  }
  check('no form field values (card, email, street)', ![CARD, 'someone@example.com', STREET].some((v) => everything.includes(v)));
  check('shop session id is only sent as the attribute (not in any URL)', !spans.some((s) => urlOf(s).includes(session)));
  check('no console errors / page errors in the browser', consoleErrors.length === 0, consoleErrors.slice(0, 3).join(' | '));
} catch (e) {
  console.log('FAIL  script error: ' + (e.stack ?? e));
  failed++;
}
await finish();
