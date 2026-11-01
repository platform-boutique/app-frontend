// A fake Faro collector: records every JSON body POSTed to /collect and answers like Alloy's faro receiver
// would (202, CORS open). Also GET /ping (records its headers, for the "no trace header to other origins"
// check). Used by faro-check.mjs; `node fake-collector.mjs 8092` runs it on its own.
import http from 'node:http';

export function startCollector(port) {
  const payloads = []; // parsed bodies, in arrival order
  const pings = [];    // { headers, preflight } of requests to /ping
  const server = http.createServer((req, res) => {
    res.setHeader('Access-Control-Allow-Origin', '*');
    res.setHeader('Access-Control-Allow-Headers', req.headers['access-control-request-headers'] ?? '*');
    res.setHeader('Access-Control-Allow-Methods', 'GET, POST, OPTIONS');
    if (req.url.startsWith('/ping')) {
      pings.push({ headers: req.headers, preflight: req.method === 'OPTIONS' });
      res.writeHead(req.method === 'OPTIONS' ? 204 : 200).end('pong');
      return;
    }
    if (req.method === 'OPTIONS') { res.writeHead(204).end(); return; }
    if (req.method !== 'POST' || !req.url.startsWith('/collect')) { res.writeHead(404).end(); return; }
    const chunks = [];
    req.on('data', (c) => chunks.push(c));
    req.on('end', () => {
      try { payloads.push({ sessionHeader: req.headers['x-faro-session-id'], body: JSON.parse(Buffer.concat(chunks).toString()) }); } catch { /* not JSON: ignore */ }
      res.writeHead(202).end('{}');
    });
  });
  return new Promise((resolve) => server.listen(port, '0.0.0.0', () => resolve({
    payloads, pings, close: () => new Promise((r) => server.close(r)),
  })));
}

if (import.meta.url === `file://${process.argv[1]}`) {
  const port = Number(process.argv[2] ?? 8092);
  const c = await startCollector(port);
  console.log(`fake collector on :${port}`);
  setInterval(() => console.log(`${c.payloads.length} payloads`), 5000);
}
