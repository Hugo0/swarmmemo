// SPDX-License-Identifier: Apache-2.0
// node --test workers/corroborate/test/sidecar.test.mjs   (Node 22.12+; npm ci first)
//
// The handler runs with a fake resolver (validation, cache, single flight,
// concurrency, timeout, the no-fake-zero rule), and the vendored SDK runs
// with fake credential probes standing in for the chains, so nothing here
// touches the network. scripts/smoke.mjs is the live check.
import test from 'node:test';
import assert from 'node:assert/strict';
import {createServer} from 'node:http';
import {mkdtempSync, writeFileSync, readFileSync, readdirSync, rmSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {dirname, join} from 'node:path';
import {fileURLToPath} from 'node:url';
import {createHandler, loadPinned, parseRequest, cacheKey, RequestError} from '../src/core.mjs';
import {PinnedCorroborate, listenAddress} from '../src/server.mjs';

const here = dirname(fileURLToPath(import.meta.url));
const ontologyDir = join(here, '../ontology');
const pinned = loadPinned(ontologyDir);
const A = '0x1111111111111111111111111111111111111111';
const B = '0x2222222222222222222222222222222222222222';

// fakeResult is a PersonhoodResult as the SDK makes it: one held credential
// and one check that could not be read.
function fakeResult(addresses, {unavailableAll = false} = {}) {
  const ev = (adapterId, held, extra = {}) => ({
    adapterId, adapterName: adapterId, evidenceClass: 'Uniqueness', trustRoot: 'iris-registry:world-orb', observedOn: addresses[0],
    held, freshness: held ? 0.5 : 0, effectiveCostCents: held ? 25 : 0, forgeCostCents: 50000, rentCostCents: 50, live: true, sourceURI: 'research/x.md', ...extra,
  });
  const evidence = unavailableAll
    ? [ev('world-id-orb', false, {detail: {error: 'down', unavailable: true}})]
    : [ev('world-id-orb', true, {issuedAt: 1_700_000_000}), ev('poh-v2', false, {detail: {error: 'down', unavailable: true}})];
  return {
    subject: addresses[0], subjects: addresses, score: 1.4150, totalCostCents: unavailableAll ? 0 : 25, independentRoots: unavailableAll ? 0 : 1, evidence,
    roots: unavailableAll ? [] : [{trustRoot: 'iris-registry:world-orb', adapterIds: ['world-id-orb'], contributionCents: 25, saturated: false}],
    caveats: [{code: 'independent-control-not-attested', message: 'x'}], registryRevision: pinned.registry.revision, computedAt: 1_800_000_000,
    isHuman() { throw new Error('never called'); },
  };
}

async function serve(opts) {
  const handler = createHandler({pinned, ...opts});
  const server = createServer((req, res) => void handler(req, res));
  await new Promise((ok) => server.listen(0, '127.0.0.1', ok));
  const base = `http://127.0.0.1:${server.address().port}`;
  const post = async (body, path = '/resolve') => {
    const r = await fetch(base + path, {method: 'POST', headers: {'content-type': 'application/json'}, body: typeof body === 'string' ? body : JSON.stringify(body)});
    return {status: r.status, retryAfter: r.headers.get('retry-after'), body: await r.json()};
  };
  return {base, post, close: () => new Promise((ok) => server.close(ok))};
}

test('pinned ontology loads with its hash and refuses a changed copy', () => {
  assert.equal(pinned.registry.chain, 'sepolia');
  assert.match(pinned.registry.sha256, /^[0-9a-f]{64}$/);
  assert.ok(pinned.ontology.adapters.size >= 30);
  const dir = mkdtempSync(join(tmpdir(), 'corroborate-pin-'));
  try {
    const pin = JSON.parse(readFileSync(join(ontologyDir, 'pinned.json'), 'utf8'));
    writeFileSync(join(dir, 'pinned.json'), JSON.stringify(pin));
    writeFileSync(join(dir, pin.file), readFileSync(join(ontologyDir, pin.file), 'utf8').replace('"rentCostCents": 50,', '"rentCostCents": 51,'));
    assert.throws(() => loadPinned(dir), /sha256/);
  } finally {
    rmSync(dir, {recursive: true});
  }
});

test('requests: 1 to 10 EVM addresses, deduplicated, and a block no older than the registry', () => {
  assert.deepEqual(parseRequest(JSON.stringify({addresses: [A, A.toUpperCase().replace('0X', '0x'), B]})).addresses, [A, B]);
  for (const bad of [
    {addresses: []}, {addresses: Array(11).fill(0).map((_, i) => '0x' + String(i).padStart(40, '0'))}, {addresses: ['vitalik.eth']},
    {addresses: ['0x123']}, {addresses: [A], as_of: 5}, {addresses: [A], as_of: '11400000'}, {addresses: [A], extra: 1}, [A],
  ]) {
    assert.throws(() => parseRequest(JSON.stringify(bad)), RequestError, JSON.stringify(bad));
  }
  assert.equal(parseRequest(JSON.stringify({addresses: [A], as_of: 11400000})).asOf, 11400000);
  assert.equal(cacheKey([B, A]), cacheKey([A, B]));
  assert.notEqual(cacheKey([A]), cacheKey([A], 11400000));
});

test('answers a score with every root, the unreadable checks and the pinned revision; caches it', async () => {
  let calls = 0;
  const s = await serve({resolve: async (addresses) => (calls++, fakeResult(addresses))});
  try {
    const r = await s.post({addresses: [B, A]});
    assert.equal(r.status, 200);
    assert.equal(r.body.total_cents, 25);
    assert.equal(r.body.score, 1.415);
    assert.equal(r.body.roots[0].strongest.forge_cents, 50000);
    assert.equal(r.body.roots[0].strongest.rent_cents, 50);
    assert.equal(r.body.roots[0].strongest.age_days, Math.floor(100_000_000 / 86400));
    assert.equal(r.body.roots[0].strongest.age_curve, pinned.ontology.adapters.get('world-id-orb').ageCurve);
    assert.deepEqual(r.body.unavailable, [{adapter: 'poh-v2', address: B}]);
    assert.ok(r.body.caveats.some((c) => c.code === 'checks-unavailable'));
    assert.equal(r.body.registry.sha256, pinned.registry.sha256);
    assert.equal(r.body.cached, false);
    assert.ok(!JSON.stringify(r.body).includes('isHuman') && !('human' in r.body) && !('verified' in r.body));
    const again = await s.post({addresses: [A, B]});
    assert.equal(again.body.cached, true);
    assert.equal(calls, 1);
  } finally {
    await s.close();
  }
});

test('no check readable is a 503, never a zero, and is not cached', async () => {
  let calls = 0;
  const s = await serve({resolve: async (addresses) => (calls++, fakeResult(addresses, {unavailableAll: true}))});
  try {
    for (let i = 0; i < 2; i++) {
      const r = await s.post({addresses: [A]});
      assert.equal(r.status, 503);
      assert.equal(r.body.error, 'unavailable');
      assert.equal(r.retryAfter, '60');
    }
    assert.equal(calls, 2);
    const thrown = await serve({resolve: async () => { throw new Error('rpc down'); }});
    try {
      assert.equal((await thrown.post({addresses: [A]})).status, 503);
    } finally {
      await thrown.close();
    }
  } finally {
    await s.close();
  }
});

test('a slow resolve times out with retry_after, then serves the retry from cache', async () => {
  let release;
  const gate = new Promise((ok) => { release = ok; });
  const s = await serve({resolve: async (addresses) => (await gate, fakeResult(addresses)), timeoutMs: 50});
  try {
    const r = await s.post({addresses: [A]});
    assert.equal(r.status, 503);
    assert.equal(r.body.error, 'timeout');
    assert.equal(r.retryAfter, '15');
    release();
    await new Promise((ok) => setTimeout(ok, 20));
    const again = await s.post({addresses: [A]});
    assert.equal(again.status, 200);
    assert.equal(again.body.cached, true);
  } finally {
    await s.close();
  }
});

test('one resolve per address set at a time, and busy past the queue', async () => {
  let calls = 0;
  let release;
  const gate = new Promise((ok) => { release = ok; });
  const s = await serve({resolve: async (addresses) => (calls++, await gate, fakeResult(addresses)), maxActive: 1, maxQueued: 0, timeoutMs: 5000});
  try {
    const first = s.post({addresses: [A]});
    const same = s.post({addresses: [A]});
    await new Promise((ok) => setTimeout(ok, 30));
    const other = await s.post({addresses: [B]});
    assert.equal(other.status, 503);
    assert.equal(other.body.error, 'busy');
    release();
    assert.equal((await first).status, 200);
    assert.equal((await same).status, 200);
    assert.equal(calls, 1);
  } finally {
    await s.close();
  }
});

test('as_of is refused while no registry audit trail is configured; bad requests are 400', async () => {
  const s = await serve({resolve: async (a) => fakeResult(a)});
  try {
    assert.equal((await s.post({addresses: [A], as_of: 11400000})).body.error, 'as_of_unavailable');
    assert.equal((await s.post('not json')).status, 400);
    assert.equal((await s.post({addresses: [A], pad: 'x'.repeat(5000)})).status, 413);
    assert.equal((await fetch(s.base + '/resolve')).status, 405);
    const health = await (await fetch(s.base + '/healthz')).json();
    assert.equal(health.registry.revision, pinned.registry.revision);
  } finally {
    await s.close();
  }
});

// No RPC URL with a key in its path ships in vendor/: the Galxe adapter's
// keyed BNB archive endpoint is CORROBORATE_BNB_RPC_URL (scripts/vendor-sdk.sh).
test('vendor/ carries no keyed RPC URL', () => {
  const walk = (dir) => readdirSync(dir, {withFileTypes: true}).flatMap((e) => (e.isDirectory() ? walk(join(dir, e.name)) : [join(dir, e.name)]));
  for (const file of walk(join(here, '../vendor'))) {
    const text = readFileSync(file, 'utf8');
    assert.doesNotMatch(text, /nodereal\.io/i, file);
    assert.doesNotMatch(text, /https?:\/\/[^'"\s]+\/v\d+\/[0-9a-fA-F]{32}/, file);
  }
  const galxe = readFileSync(join(here, '../vendor/corroborate-sdk/adapters/galxe.js'), 'utf8');
  assert.match(galxe, /CORROBORATE_BNB_RPC_URL/);
});

test('listens on loopback only', () => {
  assert.deepEqual(listenAddress('127.0.0.1:8787'), {host: '127.0.0.1', port: 8787});
  assert.deepEqual(listenAddress('[::1]:9000'), {host: '::1', port: 9000});
  for (const bad of ['0.0.0.0:8787', '10.0.0.1:8787', 'localhost:8787', '127.0.0.1:0', ':8787']) assert.throws(() => listenAddress(bad));
});

// The vendored SDK with fake probes in place of the chains: saturation within
// a root, sums across roots, the pinned weights, and a failed probe reported
// as unreadable rather than as absent.
test('the SDK scores fake chain reads against the pinned ontology', async () => {
  const probe = (adapterId, answer) => ({adapterId, probe: async (address) => answer(address)});
  const day = 86400;
  const now = Math.floor(Date.now() / 1000);
  const client = new PinnedCorroborate(pinned.ontology, {
    probeRetries: 0,
    adapters: [
      probe('world-id-orb', (a) => (a === '0x1111111111111111111111111111111111111111' ? {held: true, issuedAt: now - 10 * day} : {held: false})),
      probe('self-protocol', (a) => (a === '0x2222222222222222222222222222222222222222' ? {held: true, issuedAt: now - 10 * day} : {held: false})),
      probe('holonym-gov-id', () => ({held: false, error: 'rpc timeout'})),
      probe('farcaster-account', () => ({held: false})),
    ],
  });
  const result = await client.resolve([A, B]);
  const handler = createHandler({pinned, resolve: async () => result});
  const body = await new Promise((ok) => {
    const res = {writeHead() {}, end: (raw) => ok(JSON.parse(raw))};
    const req = (async function* () { yield Buffer.from(JSON.stringify({addresses: [A, B]})); })();
    req.url = '/resolve';
    req.method = 'POST';
    void handler(req, res);
  });
  const orb = pinned.ontology.adapters.get('world-id-orb');
  const self = pinned.ontology.adapters.get('self-protocol');
  assert.equal(body.registry.revision, pinned.registry.revision);
  assert.equal(body.roots.length, orb.trustRoot === self.trustRoot ? 1 : 2);
  const expected = body.roots.reduce((s, r) => s + r.contribution_cents, 0);
  assert.ok(Math.abs(body.total_cents - expected) < 0.02);
  const orbRoot = body.roots.find((r) => r.root === orb.trustRoot);
  assert.equal(orbRoot.strongest.forge_cents, orb.forgeCostCents);
  assert.equal(orbRoot.strongest.rent_cents, orb.rentCostCents);
  assert.equal(orbRoot.strongest.age_days, 10);
  assert.deepEqual(body.unavailable.map((u) => u.adapter).sort(), ['holonym-gov-id', 'holonym-gov-id']);
  assert.equal(body.checks.total, 8);
  assert.equal(body.score, Number(Math.log10(result.totalCostCents + 1).toFixed(4)));
});
