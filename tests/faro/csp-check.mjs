// Real-browser check of the Content-Security-Policy (README, "Content-Security-Policy") against a RUNNING frontend
// with Faro on (FARO_COLLECTOR_URL=http://localhost:<COLLECTOR_PORT>/collect). Needs no other part of the stack.
//
//   MODE=report-only (default)  the header is Content-Security-Policy-Report-Only
//   MODE=enforce                the header is Content-Security-Policy (frontend started with CSP_MODE=enforce)
//
// Visits the pages, uses the scripts (theme toggle, currency select, htmx paging and search) and collects every
// `securitypolicyviolation` event of the browser; passes only with ZERO violations, Faro payloads still reaching the
// collector, and (enforce) the page features still working. Env: BASE_URL (http://localhost:8091), COLLECTOR_PORT
// (8092), PLAYWRIGHT_DIR. Run with: env -u ELECTRON_RUN_AS_NODE node csp-check.mjs
import { createRequire } from 'node:module';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { startCollector } from './fake-collector.mjs';

const here = path.dirname(fileURLToPath(import.meta.url));
const require = createRequire(path.join(process.env.PLAYWRIGHT_DIR ?? path.join(here, '../../../../local'), 'x.js'));
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL ?? 'http://localhost:8091';
const PORT = Number(process.env.COLLECTOR_PORT ?? 8092);
const MODE = process.env.MODE ?? 'report-only';
const HEADER = MODE === 'enforce' ? 'content-security-policy' : 'content-security-policy-report-only';

let failed = 0;
const check = (name, ok, detail = '') => {
  console.log(`${ok ? 'PASS' : 'FAIL'}  ${name}${detail ? '  ' + detail : ''}`);
  if (!ok) failed++;
};
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const collector = await startCollector(PORT);
const browser = await chromium.launch();
const page = await (await browser.newContext({ viewport: { width: 1280, height: 900 } })).newPage();

const violations = [];
const consoleErrors = []; // 404s of product pictures are ignored: a stack with a partial image set is not a policy problem
const reports = [];
await page.exposeFunction('__violation', (v) => violations.push(v));
await page.addInitScript(() => {
  document.addEventListener('securitypolicyviolation', (e) => window.__violation(
    `${e.effectiveDirective} blocked=${e.blockedURI} source=${e.sourceFile}:${e.lineNumber} disposition=${e.disposition}`));
});
page.on('console', (m) => { if (m.type() === 'error' && !/^htmx:|status of 404/.test(m.text())) consoleErrors.push(m.text()); });
page.on('pageerror', (e) => consoleErrors.push(String(e)));
page.on('request', (r) => { if (r.url().endsWith('/csp-report')) reports.push(r.url()); });

const settle = async () => { await page.waitForLoadState('load'); await sleep(1800); };
let headers = {};
page.on('response', (r) => { if (r.request().resourceType() === 'document') headers = r.headers(); });

try {
  await page.goto(BASE + '/');
  await settle();
  const policy = headers[HEADER] ?? '';
  check(`${HEADER} header on the page`, policy.startsWith("default-src 'self'"), policy.slice(0, 60));
  check('the other CSP header is absent', !headers[MODE === 'enforce' ? 'content-security-policy-report-only' : 'content-security-policy']);
  check('connect-src names the Faro collector origin', policy.includes(`connect-src 'self' http://localhost:${PORT};`));

  // theme toggle (shell.js) and the no-flash script (theme-init.js)
  await page.locator('#theme-toggle').click();
  const theme = await page.evaluate(() => document.documentElement.getAttribute('data-theme'));
  check('theme toggle works', theme === 'dark' || theme === 'light', theme);
  await page.reload();
  await settle();
  check('theme remembered by theme-init.js (data-theme set before first paint)', await page.evaluate(() => document.documentElement.getAttribute('data-theme') === localStorage.getItem('theme')));
  check('js class set by theme-init.js', await page.evaluate(() => document.documentElement.classList.contains('js')));

  // currency select (shell.js): choosing a value submits the form and the page comes back with it selected
  const nav = page.waitForNavigation();
  await page.locator('#currency_code').selectOption('EUR');
  await nav;
  await settle();
  check('currency select submits the form', await page.locator('#currency_code').inputValue() === 'EUR');

  // htmx paging: from page 2 the link to the neighbouring page is an XHR whose answer is swapped in (the
  // landing page would answer HX-Redirect, a full load, by design)
  await page.goto(BASE + '/?page=2');
  await settle();
  const link = page.locator('a.page-link[hx-get]').first();
  await Promise.all([page.waitForResponse((r) => r.request().headers()['hx-request'] === 'true'), link.click()]);
  await sleep(500);
  check('htmx paging swaps the results and pushes the URL', page.url() !== BASE + '/?page=2', page.url());

  // product page (gallery.js, reviews, rating bars with style attributes) and cart
  await page.goto(BASE + (await page.locator('a[href^="/product/"]').first().getAttribute('href')));
  await settle();
  await page.goto(BASE + '/cart');
  await settle();

  check('no securitypolicyviolation event in any page', violations.length === 0, violations.slice(0, 5).join(' | '));
  check('no console error or page error', consoleErrors.length === 0, consoleErrors.slice(0, 3).join(' | '));
  check('Faro payloads reached the collector under the policy', collector.payloads.length > 0, `${collector.payloads.length} payloads`);
  // control: an inline script IS a violation (the listener works), and it runs only in report-only mode
  const before = violations.length;
  const ran = await page.evaluate(async () => {
    window.__ran = false;
    const el = document.createElement('script');
    el.textContent = 'window.__ran = true';
    document.body.appendChild(el);
    await new Promise((r) => setTimeout(r, 300));
    return window.__ran;
  });
  await sleep(500);
  check('control: an injected inline script is reported', violations.length === before + 1 && violations[violations.length - 1].startsWith('script-src-elem'), violations.slice(before).join(' | '));
  check(`control: the injected script ${MODE === 'enforce' ? 'did not run' : 'ran (nothing is blocked in report-only mode)'}`, ran === (MODE !== 'enforce'));
  console.log(`(info) csp reports posted by the browser: ${reports.length}`);
} catch (e) {
  check('script ran to the end', false, String(e).split('\n')[0]);
}
await browser.close();
await collector.close();
console.log(failed === 0 ? '\nALL CHECKS PASSED' : `\n${failed} CHECK(S) FAILED`);
process.exit(failed === 0 ? 0 : 1);
