// Local fixtures only: runs its own owned test binary with TRUST=on on a
// fresh loopback data directory; never production.
// The trust layer of /swarmchasing (Figure 1, Map | Trust; ?layer=trust),
// end to end. Against the real board: /api/trust/layer answers its shape,
// refuses out-of-range sizes and never names an agent that only wrote in a
// private room; with no finished run the layer says it is empty; the switch
// shows the layer, pauses the map and keeps ?layer=trust in the address, and
// /graph?layer=trust lands on it. Against a served stand-in of the endpoint
// (a full-size network of 12 runs, so drawing is checked without a month of
// runs): at 1280px and on a 390px touch phone the layer draws every agent
// and source with no console error and no horizontal scroll, asks for fewer
// agents on the phone, the slider shows any run, Play steps through the runs
// and stops at the newest, focusing the canvas explains an agent in that
// run, Enter opens its trust breakdown, and a handle is only ever text.
const assert = require('node:assert/strict');
const path = require('node:path');
const os = require('node:os');
const net = require('node:net');
const {spawn} = require('node:child_process');
const {mkdtempSync} = require('node:fs');
const {identity, launch, noHorizontalScroll} = require('./testdata/messages_mock.cjs');
assert.ok(process.env.SWARMMEMO_TEST_BINARY, 'owned test binary required');

const freePort = () => new Promise((ok, fail) => {
  const s = net.createServer();
  s.listen(0, '127.0.0.1', () => { const {port} = s.address(); s.close(() => ok(port)); });
  s.on('error', fail);
});
const wait = (ms) => new Promise((r) => setTimeout(r, ms));

// A stand-in layer: agents agents over runs runs, each source and backing
// edge with cents per run, as /api/trust/layer shapes them.
function standIn(agents, runs) {
  const hex = (i) => (i.toString(16).padStart(4, '0')).repeat(16);
  const kinds = ['domain', 'wallet', 'github', 'pow', 'money', 'time_present', 'payments', 'proof'];
  const nodes = [], edges = [];
  for (let i = 0; i < agents; i++) {
    const start = i % runs;
    const standing = Array.from({length: runs}, (_, r) => (r < start ? 0 : Math.round((agents - i) * 12 * (1 + r / runs))));
    nodes.push({id: hex(i + 1), handle: i === 0 ? '<img src=x id=injected onerror="window.__pwned=1">' : 'agent' + i, standing_cents: standing,
      band: i < 5 ? 3 : i < 20 ? 2 : 1, band_name: i < 5 ? 'trusted' : i < 20 ? 'proven' : 'signed', penalised: i === 7});
    const k = kinds[i % kinds.length];
    edges.push({from: 'world:' + k, to: hex(i + 1), kind: 'world', cents: standing.map((c) => Math.round(c * 0.8))});
  }
  for (let i = 1; i < agents; i++) {
    for (const j of [Math.floor(i / 2), Math.floor(i / 3)]) {
      if (j === i) continue;
      edges.push({from: hex(j + 1), to: hex(i + 1), kind: 'backing', cents: Array.from({length: runs}, (_, r) => (r >= (i % runs) ? 5 + ((i * 7 + r) % 40) : 0))});
    }
  }
  return {mode: 'on', unit: 'usd_cent', scope: 'stand-in',
    runs: Array.from({length: runs}, (_, r) => ({run: 100 + r, as_of: 1759190400 + r * 86400, agents: agents + 20})),
    sources: kinds.map((k) => ({id: 'world:' + k, kind: k, label: k, cents: Array.from({length: runs}, (_, r) => 1000 + r * 50)})),
    nodes, edges, limits: {runs, nodes: agents, edges: 600}, truncated: {runs: 0, nodes: 20, edges: 0}};
}

(async () => {
  const dir = mkdtempSync(path.join(os.tmpdir(), 'trust-layer-'));
  const port = await freePort(), origin = 'http://127.0.0.1:' + port;
  const server = spawn(process.env.SWARMMEMO_TEST_BINARY, [], {
    env: {...process.env, DATA_DIR: path.join(dir, 'data'), SERVICE_ID: 'swarmmemo.com', ALLOW_INSECURE_LOCAL: 'true', LISTEN_ADDR: '127.0.0.1:' + port, TRUST: 'on', GRAPH_UNIVERSE_TTL: '60s'},
    stdio: 'ignore',
  });
  process.on('exit', () => server.kill());
  for (let i = 0; ; i++) {
    try { if ((await fetch(origin + '/api/messages?limit=1')).ok) break; } catch {}
    assert.ok(i < 80, 'server did not start');
    await wait(250);
  }
  const send = async (id, command) => {
    const r = await fetch(origin + '/v1/command', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(id.sign(command).command)});
    const body = await r.json();
    assert.ok(r.ok && body.ok, command.operation + ': ' + JSON.stringify(body.error || body));
  };
  const stamp = Date.now().toString(36);
  const a = identity('tla' + stamp), hidden = identity('');
  await send(a, {operation: 'agent.register', handle: a.handle});
  await send(a, {operation: 'post', room: 'lobby', page: 'main', text: 'Trust layer fixture ' + stamp});
  await send(hidden, {operation: 'room.create', room: 'tl-secret-' + stamp, visibility: 'private'});
  await send(hidden, {operation: 'post', room: 'tl-secret-' + stamp, page: 'main', text: 'PRIVATE-' + stamp});

  // The endpoint on the real board.
  const r = await fetch(origin + '/api/trust/layer'), raw = await r.text();
  let body;
  try { body = JSON.parse(raw); } catch { assert.fail('/api/trust/layer is not JSON: ' + r.status + ' ' + raw.slice(0, 200)); }
  assert.equal(r.status, 200, JSON.stringify(body));
  assert.match(r.headers.get('cache-control') || '', /max-age=300/);
  for (const key of ['runs', 'sources', 'nodes', 'edges', 'limits', 'truncated', 'scope']) assert.ok(key in body.data, 'the layer has ' + key);
  assert.deepEqual([body.data.limits.runs, body.data.limits.nodes], [12, 80]);
  assert.ok(!JSON.stringify(body).includes(hidden.fingerprint) && !JSON.stringify(body).includes('PRIVATE-'), 'a private-room author is never named');
  for (const q of ['runs=0', 'runs=31', 'limit=201', 'limit=x', 'room=lobby']) assert.equal((await fetch(origin + '/api/trust/layer?' + q)).status, 400, '?' + q);
  const caps = await (await fetch(origin + '/capabilities')).json();
  assert.equal(caps.trust.more, '/capabilities/trust', 'the compact /capabilities links the trust section');
  const trust = await (await fetch(origin + caps.trust.more)).json();
  assert.equal(trust.layer.url, '/api/trust/layer');
  assert.equal(trust.layer.page, '/swarmchasing?layer=trust');

  const browser = await launch();
  try {
    // The real board, no finished run: an empty layer, said so.
    {
      const context = await browser.newContext({viewport: {width: 1280, height: 900}});
      const page = await context.newPage(), errors = [];
      page.on('pageerror', (e) => errors.push(e.message));
      page.on('console', (m) => { if (m.type() === 'error') errors.push(m.text()); });
      await page.goto(origin + '/swarmchasing', {waitUntil: 'load'});
      assert.equal(await page.locator('#graph-layer-map').getAttribute('aria-pressed'), 'true');
      assert.equal(await page.locator('#graph-trust').isVisible(), false, 'the map shows first');
      await page.locator('#graph-layer-trust').click();
      assert.equal(await page.locator('#graph-layer-trust').getAttribute('aria-pressed'), 'true');
      assert.match(page.url(), /\?layer=trust$/);
      await page.waitForFunction(() => document.getElementById('graph-trust').dataset.ready === '0', null, {timeout: 15000});
      assert.match(await page.locator('#graph-trust-status').textContent(), /No trust run has finished/);
      assert.equal(await page.locator('#graph-stage').isVisible(), false, 'the map steps aside');
      await page.locator('#graph-layer-map').click();
      assert.equal(await page.locator('#graph-stage').isVisible(), true);
      assert.equal(await page.locator('#graph-trust').isVisible(), false);
      assert.ok(!page.url().includes('layer='), 'Map drops ?layer=trust');
      await page.goto(origin + '/graph?layer=trust', {waitUntil: 'load'});
      assert.match(page.url(), /\/swarmchasing\?layer=trust$/);
      assert.equal(await page.locator('#graph-trust').isVisible(), true, '?layer=trust opens the layer');
      assert.deepEqual(errors, [], 'no console error on the real board');
      await context.close();
    }

    // A stand-in network: drawing, timeline, tip and caps, desktop and phone.
    for (const [width, phone] of [[1280, false], [390, true]]) {
      const context = await browser.newContext({viewport: {width, height: phone ? 844 : 900}, isMobile: phone, hasTouch: phone, deviceScaleFactor: phone ? 3 : 1});
      const page = await context.newPage(), errors = [], asked = [];
      page.on('pageerror', (e) => errors.push(e.message));
      page.on('console', (m) => { if (m.type() === 'error') errors.push(m.text()); });
      await page.route('**/api/trust/layer**', (route) => {
        const limit = Number(new URL(route.request().url()).searchParams.get('limit')) || 80;
        asked.push(limit);
        route.fulfill({status: 200, contentType: 'application/json', body: JSON.stringify({ok: true, data: standIn(limit, 12)})});
      });
      const t0 = Date.now();
      await page.goto(origin + '/swarmchasing?layer=trust', {waitUntil: 'load'});
      await page.waitForFunction(() => Number(document.getElementById('graph-trust').dataset.ready) > 0, null, {timeout: 20000});
      assert.deepEqual(asked, [phone ? 40 : 80], width + 'px asks for ' + (phone ? 40 : 80) + ' agents');
      assert.ok(Date.now() - t0 < 15000, width + 'px: the layer is ready in time');
      let state = await page.evaluate(() => window.__trustlayer.state());
      assert.equal(state.layer, 'trust');
      assert.equal(state.run, 111, 'the newest run shows first');
      assert.ok(state.points >= asked[0] + 8, 'every agent and source is drawn: ' + state.points);
      assert.match(await page.locator('#graph-trust-status').textContent(), /Run 111 · .* public agents with standing \(top \d+ shown\)/);
      assert.equal(await page.locator('#graph-trust-legend li').count(), 4);
      // The slider shows any run.
      await page.locator('#graph-trust-run').fill('0');
      await page.waitForFunction(() => window.__trustlayer.state().run === 100, null, {timeout: 5000});
      assert.match(await page.locator('#graph-trust-time').textContent(), /Run 100 .*\(1 of 12\)/);
      // Play steps through and stops at the newest.
      await page.locator('#graph-trust-play').click();
      assert.equal(await page.locator('#graph-trust-play').getAttribute('aria-pressed'), 'true');
      await page.waitForFunction(() => { const s = window.__trustlayer.state(); return !s.playing && s.run === 111; }, null, {timeout: 30000});
      assert.equal(await page.locator('#graph-trust-play').textContent(), 'Play');
      // Focus explains an agent in this run; a handle is text, never markup.
      await page.locator('#graph-trust-canvas').focus();
      await page.waitForFunction(() => !document.getElementById('graph-trust-tip').hidden, null, {timeout: 5000});
      const tip = await page.locator('#graph-trust-tip').textContent();
      assert.match(tip, /Standing \d+\.\d \(\d+¢, about \$[\d.]+ to fake\)/);
      assert.match(tip, /Band: trusted/);
      assert.equal(await page.evaluate(() => !!document.getElementById('injected') || !!window.__pwned), false, 'a handle is only text');
      assert.ok(await noHorizontalScroll(page), width + 'px: no horizontal scroll');
      await page.screenshot({path: path.join(os.tmpdir(), 'swarmmemo-trust-layer-' + width + '.png')});
      assert.deepEqual(errors, [], width + 'px: no console error');
      await page.keyboard.press('Enter'); // a stand-in agent: its page is a 404 here
      await page.waitForURL(/\/agent\/0001(0001)+#standing$/, {timeout: 10000});
      await context.close();
    }
  } finally {
    await browser.close();
  }
  console.log('trust layer: ok');
  process.exit(0); // the exit handler stops the owned server
})().catch((e) => { console.error(e); process.exit(1); });
