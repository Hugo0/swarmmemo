// SPDX-License-Identifier: Apache-2.0
// node --test workers/runs-loader/test/loader.test.mjs   (Node 22.18+, no dependencies)
//
// What runs here: request validation, both HMAC directions, the egress
// policy (addresses, names, caps, the log), the gateway's whole fetch path
// with a mocked upstream, the harness modules themselves (the JavaScript
// harness in a child Node process, the Python harness under python3 with a
// stand-in `workers` module), and the loader's HTTP handler with a fake
// Worker Loader and a real RunLedger over fake Durable Object storage.
//
// What cannot run here: a real Dynamic Worker (the Worker Loader binding,
// globalOutbound routing, cpuMs/subRequests enforcement and tail events exist
// only in workerd on Cloudflare). deploy/RUNBOOK.md has the smoke test that
// covers those after a deploy.
import test from 'node:test';
import assert from 'node:assert/strict';
import {register} from 'node:module';
import {spawnSync} from 'node:child_process';
import {mkdtempSync, writeFileSync, rmSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join} from 'node:path';

const stub = 'export class DurableObject { constructor(ctx, env) { this.ctx = ctx; this.env = env; } } export class WorkerEntrypoint { constructor(ctx, env) { this.ctx = ctx; this.env = env; } }';
register('data:text/javascript,' + encodeURIComponent(`export async function resolve(specifier, context, next) {
  if (specifier === 'cloudflare:workers') return {url: 'data:text/javascript,' + encodeURIComponent(${JSON.stringify(stub)}), shortCircuit: true};
  return next(specifier, context);
}`));

const protocol = await import('../src/protocol.ts');
const auth = await import('../src/auth.ts');
const egress = await import('../src/egress.ts');
const harness = await import('../src/harness.ts');
const loader = await import('../src/index.ts');
const {LIMITS, validateRunRequest, truncateUTF8} = protocol;

const SECRET = 's'.repeat(40);
const RUN_ID = '0123456789abcdef0123456789abcdef';

function policy(extra = {}) {
  return {enabled: true, max_requests: 4, max_bytes_out: 1000, max_bytes_in: 1000, ports: [80, 443], deny_hosts: [], deny_cidrs: [], allow_connect: false, resolve: false, ...extra};
}

function runRequest(extra = {}) {
  return {
    schema: 1, run_id: RUN_ID, language: 'javascript', code: 'export function run(i) { return i }', input: {a: 1},
    limits: {cpu_ms: 50, subrequests: 4, wall_ms: 1000, stdout_bytes: 100, stderr_bytes: 100, result_bytes: 100},
    network: policy({enabled: false}),
    ...extra,
  };
}

// ---------------------------------------------------------------- protocol

test('validateRunRequest accepts the documented shape and refuses anything else', () => {
  assert.equal(validateRunRequest(runRequest()).ok, true);
  const bad = [
    {...runRequest(), extra: 1},
    runRequest({schema: 2}),
    runRequest({run_id: 'ABC'}),
    runRequest({language: 'ruby'}),
    runRequest({code: ''}),
    runRequest({code: 'x'.repeat(LIMITS.codeBytes + 1)}),
    runRequest({input: 'x'.repeat(LIMITS.inputBytes)}),
    runRequest({limits: {...runRequest().limits, cpu_ms: 0}}),
    runRequest({limits: {...runRequest().limits, cpu_ms: LIMITS.cpuMsMax + 1}}),
    runRequest({limits: {...runRequest().limits, wall_ms: 1.5}}),
    runRequest({limits: {...runRequest().limits, extra: 1}}),
    runRequest({network: policy({allow_connect: true})}),
    runRequest({network: policy({max_requests: 65})}),
    runRequest({network: policy({deny_hosts: ['Evil.COM']})}),
    runRequest({network: policy({deny_cidrs: ['not a cidr']})}),
    runRequest({network: policy({ports: [0]})}),
    null, [], 'x',
  ];
  for (const b of bad) assert.equal(validateRunRequest(b).ok, false, JSON.stringify(b)?.slice(0, 120));
});

test('truncateUTF8 never splits a character', () => {
  assert.deepEqual(truncateUTF8('héllo', 2), ['h', true]);
  assert.deepEqual(truncateUTF8('héllo', 3), ['hé', true]);
  assert.deepEqual(truncateUTF8('abc', 3), ['abc', false]);
});

// ---------------------------------------------------------------- auth

test('HMAC: round trip, and every tampering is refused', async () => {
  const body = new TextEncoder().encode('{"x":1}');
  const now = 1_800_000_000;
  const sig = await auth.sign(SECRET, 'req', String(now), body);
  assert.match(sig, /^v1=[0-9a-f]{64}$/);
  assert.equal(await auth.verify(SECRET, 'req', String(now), sig, body, now), true);
  assert.equal(await auth.verify(SECRET, 'req', String(now), sig, body, now + LIMITS.clockSkewSec), true);
  assert.equal(await auth.verify(SECRET, 'req', String(now), sig, body, now + LIMITS.clockSkewSec + 1), false, 'stale');
  assert.equal(await auth.verify(SECRET, 'res', String(now), sig, body, now), false, 'a request MAC is not a response MAC');
  assert.equal(await auth.verify('t'.repeat(40), 'req', String(now), sig, body, now), false, 'wrong secret');
  assert.equal(await auth.verify(SECRET, 'req', String(now), sig, new TextEncoder().encode('{"x":2}'), now), false, 'body');
  assert.equal(await auth.verify(SECRET, 'req', String(now + 1), sig, body, now), false, 'timestamp');
  assert.equal(await auth.verify(SECRET, 'req', String(now), null, body, now), false);
  assert.equal(await auth.verify(SECRET, 'req', null, sig, body, now), false);
  assert.equal(await auth.verify(SECRET, 'req', String(now), sig.toUpperCase(), body, now), false);
  const short = 'short';
  assert.equal(await auth.verify(short, 'req', String(now), await auth.sign(short, 'req', String(now), body), body, now), false, 'a short secret is never accepted');
});

// ---------------------------------------------------------------- egress policy

test('checkDestination refuses private, metadata, internal and mining destinations', () => {
  const p = policy({deny_hosts: ['blocked.example.org'], deny_cidrs: ['203.0.114.0/24']});
  const cases = {
    'http://127.0.0.1/': 'private_address',
    'http://2130706433/': 'private_address',
    'http://0x7f.1/': 'private_address',
    'http://10.1.2.3/': 'private_address',
    'http://[::1]/': 'private_address',
    'http://[::ffff:127.0.0.1]/': 'private_address',
    'http://[::ffff:7f00:1]/': 'private_address',
    'http://[fc00::1]/': 'private_address',
    'http://169.254.169.254/latest/meta-data/': 'metadata',
    'http://[fd00:ec2::254]/': 'metadata',
    'http://168.63.129.16/': 'metadata',
    'http://localhost/': 'internal_name',
    'http://localhost./': 'internal_name',
    'http://metadata.google.internal/': 'internal_name',
    'http://metadata/': 'internal_name',
    'http://printer.local/': 'internal_name',
    'https://gulf.moneroocean.stream/': 'mining_pool',
    'https://xmr.nanopool.org/': 'mining_pool',
    'https://a.blocked.example.org/': 'denied_host',
    'https://203.0.114.9/': 'denied_cidr',
    'https://example.org:22/': 'port',
    'ftp://example.org/': 'scheme',
  };
  for (const [u, reason] of Object.entries(cases)) {
    assert.deepEqual(egress.checkDestination(new URL(u), 'GET', p), {ok: false, reason}, u);
  }
  assert.deepEqual(egress.checkDestination(new URL('https://example.org/'), 'TRACE', p), {ok: false, reason: 'method'});
  for (const u of ['https://example.org/', 'http://93.184.215.14/', 'https://[2606:4700::1]/', 'https://api.example.com:443/x?y=1']) {
    assert.deepEqual(egress.checkDestination(new URL(u), 'GET', p), {ok: true}, u);
  }
});

test('IPv6 parsing covers compression and embedded IPv4', () => {
  const hex = a => Buffer.from(a).toString('hex');
  assert.equal(hex(egress.parseIPv6('::')), '0'.repeat(32));
  assert.equal(hex(egress.parseIPv6('::1')), '0'.repeat(31) + '1');
  assert.equal(hex(egress.parseIPv6('::ffff:1.2.3.4')), '00000000000000000000ffff01020304');
  assert.equal(hex(egress.parseIPv6('1:2:3:4:5:6:1.2.3.4')), '00010002000300040005000601020304');
  assert.equal(hex(egress.parseIPv6('[2001:db8::1]')), '20010db8000000000000000000000001');
  for (const bad of ['1:::2', ':1::', '1:2:3:4:5:6:7:8:9', '12345::', '::g', '1.2.3.4', '::1.2.3', 'fe80::1%eth0']) {
    assert.equal(egress.parseIPv6(bad), null, bad);
  }
});

test('checkResolved refuses a name that resolves to a private address', async () => {
  const doh = answers => async url => {
    const type = new URL(url).searchParams.get('type');
    return Response.json({Status: 0, Answer: (answers[type] ?? []).map(data => ({type: type === 'A' ? 1 : 28, data}))});
  };
  assert.deepEqual(await egress.checkResolved('rebind.example', policy(), doh({A: ['10.0.0.1']})), {ok: false, reason: 'private_address'});
  assert.deepEqual(await egress.checkResolved('meta.example', policy(), doh({A: ['93.184.215.14'], AAAA: ['fd00:ec2::254']})), {ok: false, reason: 'metadata'});
  assert.deepEqual(await egress.checkResolved('fine.example', policy(), doh({A: ['93.184.215.14']})), {ok: true});
  assert.deepEqual(await egress.checkResolved('gone.example', policy(), doh({})), {ok: false, reason: 'unresolved'});
  assert.deepEqual(await egress.checkResolved('err.example', policy(), async () => { throw new Error('x'); }), {ok: false, reason: 'unresolved'});
});

const attempt = (bytes_out = 0) => ({method: 'GET', host: 'example.org', port: 443, path_sha256: 'a'.repeat(64), bytes_out});

test('EgressBook enforces request and byte caps and logs every attempt', () => {
  const book = new egress.EgressBook(policy({max_requests: 2, max_bytes_out: 10, max_bytes_in: 10}), 1000);
  const a = book.admit(attempt(4), 1001);
  assert.equal(a.ok, true);
  assert.equal(a.bytesInLeft, 10);
  assert.deepEqual(book.admit(attempt(7), 1002), {ok: false, reason: 'bytes_out_cap'});
  const b = book.admit(attempt(6), 1003);
  assert.equal(b.ok, true);
  assert.deepEqual(book.admit(attempt(0), 1004), {ok: false, reason: 'request_cap'});
  assert.equal(book.complete(a.seq, {status: 200, bytes_in: 6, ms: 5}), true);
  assert.equal(book.complete(b.seq, {status: 200, bytes_in: 5, ms: 5}), false, 'over the inbound cap');
  assert.equal(book.complete(b.seq, {status: 200, bytes_in: 5, ms: 5}), false, 'completing twice');
  book.seal();
  assert.deepEqual(book.admit(attempt(0), 1005), {ok: false, reason: 'run_ended'});
  const s = book.summary();
  assert.equal(s.requests, 2);
  assert.equal(s.bytes_out, 10);
  assert.equal(s.bytes_in, 11);
  assert.equal(s.blocked, 4);
  assert.deepEqual(s.log.map(e => [e.verdict, e.reason]), [['allowed', ''], ['blocked', 'bytes_out_cap'], ['blocked', 'bytes_in_cap'], ['blocked', 'request_cap'], ['blocked', 'run_ended']]);
  assert.equal(s.log[0].t_ms, 1);
  const off = new egress.EgressBook(policy({enabled: false}), 0);
  assert.deepEqual(off.admit(attempt(), 1), {ok: false, reason: 'network_off'});
});

test('EgressBook log is bounded', () => {
  const book = new egress.EgressBook(policy({max_requests: 0}), 0);
  for (let i = 0; i < LIMITS.egressLogMax + 10; i++) book.admit(attempt(), i);
  const s = book.summary();
  assert.equal(s.log.length, LIMITS.egressLogMax);
  assert.equal(s.log_truncated, true);
  assert.equal(s.blocked, LIMITS.egressLogMax + 10);
});

function bookLedger(p) {
  const book = new egress.EgressBook(p, Date.now());
  return {book, ledger: {admit: async a => book.admit(a, Date.now()), complete: async (s, c) => book.complete(s, c), block: async (a, r) => book.block(a, r, Date.now())}};
}

test('gatewayFetch forwards an allowed request, strips hop-by-hop headers and records it', async () => {
  const {book, ledger} = bookLedger(policy());
  let seen;
  const upstream = async (url, init) => {
    seen = {url, init};
    return new Response('hello', {status: 200, headers: {'x-up': '1', 'content-encoding': 'gzip'}});
  };
  const req = new Request('https://api.example.org/v1/items?q=secret', {headers: {'cf-connecting-ip': '1.2.3.4', 'x-keep': 'y'}});
  const res = await egress.gatewayFetch(req, policy(), ledger, upstream);
  assert.equal(res.status, 200);
  assert.equal(await res.text(), 'hello');
  assert.equal(res.headers.get('x-up'), '1');
  assert.equal(res.headers.get('content-encoding'), null);
  assert.equal(seen.init.redirect, 'manual', 'redirects come back through the gateway');
  assert.equal(seen.init.headers.get('cf-connecting-ip'), null);
  assert.equal(seen.init.headers.get('x-keep'), 'y');
  const [e] = book.summary().log;
  assert.equal(e.host, 'api.example.org');
  assert.equal(e.status, 200);
  assert.equal(e.bytes_in, 5);
  assert.equal(e.pending, false);
  assert.match(e.path_sha256, /^[0-9a-f]{64}$/);
  assert.ok(!JSON.stringify(book.summary()).includes('secret'), 'the path is kept only as a hash');
});

test('gatewayFetch refuses and logs blocked destinations without any upstream call', async () => {
  const {book, ledger} = bookLedger(policy());
  let calls = 0;
  const upstream = async () => { calls++; return new Response('x'); };
  for (const u of ['http://169.254.169.254/latest', 'https://pool.supportxmr.com/', 'http://localhost:8080/']) {
    const res = await egress.gatewayFetch(new Request(u), policy(), ledger, upstream);
    assert.equal(res.status, 403);
    assert.equal(res.headers.get('x-runs-egress'), 'blocked');
  }
  const ws = await egress.gatewayFetch(new Request('https://example.org/', {headers: {upgrade: 'websocket'}}), policy(), ledger, upstream);
  assert.equal(ws.status, 403);
  assert.equal(calls, 0);
  assert.deepEqual(book.summary().log.map(e => e.reason), ['metadata', 'mining_pool', 'port', 'upgrade']);
});

test('gatewayFetch enforces the byte caps both ways', async () => {
  const p = policy({max_bytes_out: 8, max_bytes_in: 8});
  const {book, ledger} = bookLedger(p);
  const big = await egress.gatewayFetch(new Request('https://example.org/', {method: 'POST', body: 'x'.repeat(9)}), p, ledger, async () => new Response('ok'));
  assert.equal(big.status, 403);
  const flood = await egress.gatewayFetch(new Request('https://example.org/'), p, ledger, async () => new Response('y'.repeat(9)));
  assert.equal(flood.status, 502);
  const s = book.summary();
  assert.deepEqual(s.log.map(e => e.reason), ['bytes_out_cap', 'bytes_in_cap']);
  assert.equal(s.bytes_in, 9, 'bytes transferred still count');
});

test('gatewayFetch resolves names when asked and refuses a rebinding name', async () => {
  const p = policy({resolve: true});
  const {ledger} = bookLedger(p);
  const fake = async (url) => {
    if (String(url).startsWith('https://cloudflare-dns.com/')) {
      const type = new URL(url).searchParams.get('type');
      return Response.json({Status: 0, Answer: type === 'A' ? [{type: 1, data: '127.0.0.1'}] : []});
    }
    throw new Error('must not be called');
  };
  const res = await egress.gatewayFetch(new Request('https://rebind.example.org/'), p, ledger, fake);
  assert.equal(res.status, 403);
});

// ---------------------------------------------------------------- harness

test('the JavaScript harness captures output under its caps and returns the result', () => {
  const dir = mkdtempSync(join(tmpdir(), 'runs-harness-'));
  try {
    const run = runRequest({code: 'console.log("top"); export async function run(i) { console.log("x".repeat(500)); console.error("oops", {a: 1}); return {doubled: i.n * 2}; }', input: {n: 21}});
    const mods = harness.buildModules(run);
    for (const [name, src] of Object.entries(mods.modules)) writeFileSync(join(dir, name), src);
    writeFileSync(join(dir, 'drive.mjs'), `const m = await import('./${mods.mainModule}');
const r = await m.default.fetch(new Request('https://run.invalid/', {method: 'POST', body: JSON.stringify({input: ${JSON.stringify(run.input)}})}));
process.stdout.write(await r.text());`);
    const p = spawnSync(process.execPath, [join(dir, 'drive.mjs')], {encoding: 'utf8'});
    assert.equal(p.status, 0, p.stderr);
    const out = harness.parseHarness(p.stdout);
    assert.ok(out, p.stdout);
    assert.equal(out.ok, true);
    assert.equal(out.result_json, '{"doubled":42}');
    assert.ok(out.stdout.startsWith('top\n'));
    assert.equal(out.stdout.length, 100);
    assert.equal(out.truncated.stdout, true);
    assert.equal(out.stderr, 'oops {"a":1}\n');
  } finally {
    rmSync(dir, {recursive: true, force: true});
  }
});

test('the JavaScript harness sends fetch() only to env.EGRESS, and refuses it without one', () => {
  const dir = mkdtempSync(join(tmpdir(), 'runs-harness-egress-'));
  try {
    const run = runRequest({code: 'export async function run() { try { const r = await fetch("https://example.org/x", {method: "POST", body: "b"}); return [r.status, await r.text()]; } catch (e) { return ["refused", String(e.message)]; } }'});
    const mods = harness.buildModules(run);
    for (const [name, src] of Object.entries(mods.modules)) writeFileSync(join(dir, name), src);
    writeFileSync(join(dir, 'drive.mjs'), `globalThis.fetch = () => { throw new Error('the runtime fetch must never be used'); };
const m = await import('./${mods.mainModule}');
const seen = [];
const EGRESS = {fetch: async (input, init) => { seen.push([String(input), init && init.method]); return new Response('via gateway', {status: 201}); }};
const req = () => new Request('https://run.invalid/', {method: 'POST', body: '{"input":null}'});
const on = JSON.parse(await (await m.default.fetch(req(), {EGRESS})).text());
const off = JSON.parse(await (await m.default.fetch(req(), {})).text());
process.stdout.write(JSON.stringify({on, off, seen}));`);
    const p = spawnSync(process.execPath, [join(dir, 'drive.mjs')], {encoding: 'utf8'});
    assert.equal(p.status, 0, p.stderr);
    const {on, off, seen} = JSON.parse(p.stdout);
    assert.equal(on.result_json, '[201,"via gateway"]');
    assert.deepEqual(seen, [['https://example.org/x', 'POST']]);
    assert.equal(off.result_json, '["refused","Network access is off for this run."]');
  } finally {
    rmSync(dir, {recursive: true, force: true});
  }
});

test('the Python harness captures print output and returns the result', {skip: spawnSync('python3', ['--version']).status !== 0}, () => {
  const dir = mkdtempSync(join(tmpdir(), 'runs-harness-py-'));
  try {
    const run = runRequest({language: 'python', code: 'import sys\nprint("hi")\nasync def run(i):\n    print("y" * 500)\n    print("bad", file=sys.stderr)\n    return {"sum": sum(i)}\n', input: [1, 2, 3]});
    const mods = harness.buildModules(run);
    assert.deepEqual(mods.compatibilityFlags, ['python_workers']);
    for (const [name, src] of Object.entries(mods.modules)) writeFileSync(join(dir, name), src);
    writeFileSync(join(dir, 'workers.py'), 'class WorkerEntrypoint:\n    pass\nclass Response:\n    def __init__(self, body, headers=None):\n        self.body = body\n');
    writeFileSync(join(dir, 'drive.py'), `import asyncio, json, sys
real = sys.stdout
import main
class Req:
    async def text(self):
        return json.dumps({"input": ${JSON.stringify(run.input)}})
r = asyncio.run(main.Default().fetch(Req()))
real.write(r.body)
`);
    const p = spawnSync('python3', [join(dir, 'drive.py')], {encoding: 'utf8', cwd: dir});
    assert.equal(p.status, 0, p.stderr);
    const out = harness.parseHarness(p.stdout);
    assert.ok(out, p.stdout);
    assert.equal(out.ok, true);
    assert.equal(out.result_json, '{"sum": 6}');
    assert.ok(out.stdout.startsWith('hi\n'));
    assert.equal(out.truncated.stdout, true);
    assert.equal(out.stderr, 'bad\n');
  } finally {
    rmSync(dir, {recursive: true, force: true});
  }
});

test('the Python harness points js fetch at env.EGRESS when the network is on', {skip: spawnSync('python3', ['--version']).status !== 0}, () => {
  const dir = mkdtempSync(join(tmpdir(), 'runs-harness-py-egress-'));
  try {
    const run = runRequest({language: 'python', code: 'import js\ndef run(i):\n    return js.globalThis.fetch("https://example.org/")\n'});
    const mods = harness.buildModules(run);
    for (const [name, src] of Object.entries(mods.modules)) writeFileSync(join(dir, name), src);
    writeFileSync(join(dir, 'workers.py'), 'class WorkerEntrypoint:\n    pass\nclass Response:\n    def __init__(self, body, headers=None):\n        self.body = body\n');
    writeFileSync(join(dir, 'js.py'), 'class _G:\n    def fetch(self, url):\n        return "runtime"\nglobalThis = _G()\n');
    writeFileSync(join(dir, 'drive.py'), `import asyncio, json, sys
real = sys.stdout
import main
class Fetch:
    def bind(self, this):
        return lambda url: "gateway:" + url
class Egress:
    fetch = Fetch()
class Env:
    EGRESS = Egress()
class Req:
    async def text(self):
        return json.dumps({"input": None})
d = main.Default()
d.env = Env()
r = asyncio.run(d.fetch(Req()))
real.write(r.body)
`);
    const p = spawnSync('python3', [join(dir, 'drive.py')], {encoding: 'utf8', cwd: dir});
    assert.equal(p.status, 0, p.stderr);
    const out = harness.parseHarness(p.stdout);
    assert.ok(out, p.stdout);
    assert.equal(out.result_json, '"gateway:https://example.org/"');
  } finally {
    rmSync(dir, {recursive: true, force: true});
  }
});

test('parseHarness refuses anything but the harness shape', () => {
  assert.equal(harness.parseHarness('{"ok":true}'), null);
  assert.equal(harness.parseHarness('not json'), null);
  assert.equal(harness.parseHarness(JSON.stringify({ok: true, result_json: '{bad', error: null, stdout: '', stderr: '', truncated: {stdout: false, stderr: false}})), null);
  assert.ok(harness.parseHarness(JSON.stringify({ok: true, result_json: null, error: null, stdout: '', stderr: '', truncated: {stdout: false, stderr: false}})));
});

// ---------------------------------------------------------------- the handler

class FakeStorage {
  map = new Map();
  alarm = 0;
  async get(k) { return this.map.get(k); }
  async put(k, v) { this.map.set(k, v); }
  async setAlarm(t) { this.alarm = t; }
  async deleteAll() { this.map.clear(); }
}

function fakeEnv(behave) {
  const ledgers = new Map();
  const loads = [];
  const env = {
    RUNS_HMAC_SECRET: SECRET,
    RUN_LEDGER: {
      idFromName: name => name,
      get: id => {
        if (!ledgers.has(id)) ledgers.set(id, new loader.RunLedger({storage: new FakeStorage()}, {}));
        return ledgers.get(id);
      },
    },
    LOADER: {
      load(code) {
        loads.push(code);
        return {getEntrypoint: (_name, opts) => ({fetch: () => behave(code, opts, ledgers)})};
      },
    },
  };
  const ctx = {exports: {
    EgressGateway: ({props}) => ({kind: 'gateway', props}),
    RunTail: ({props}) => ({kind: 'tail', props}),
  }};
  return {env, ctx, loads, ledgers};
}

async function signedRequest(body, {secret = SECRET, ts = Math.floor(Date.now() / 1000)} = {}) {
  const bytes = new TextEncoder().encode(JSON.stringify(body));
  return new Request('https://loader.example/run', {method: 'POST', body: bytes, headers: {
    'x-runs-timestamp': String(ts), 'x-runs-signature': await auth.sign(secret, 'req', String(ts), bytes)}});
}

const harnessAnswer = (extra = {}) => Response.json({ok: true, result_json: '{"a":1}', error: null, stdout: 'out\n', stderr: '', truncated: {stdout: false, stderr: false}, ...extra});

test('handle: a signed run with the network off loads with globalOutbound null and signs its answer', async () => {
  const {env, ctx, loads, ledgers} = fakeEnv(async (code, opts, ledgers) => {
    await ledgers.get(RUN_ID).tail({cpu_ms: 7, wall_ms: 9, outcome: 'ok', stdout: '', stderr: ''});
    return harnessAnswer();
  });
  const res = await loader.handle(await signedRequest(runRequest()), env, ctx);
  assert.equal(res.status, 200);
  const bytes = new Uint8Array(await res.arrayBuffer());
  const ts = res.headers.get('x-runs-timestamp');
  assert.equal(await auth.verify(SECRET, 'res', ts, res.headers.get('x-runs-signature'), bytes, Math.floor(Date.now() / 1000)), true);
  const out = JSON.parse(new TextDecoder().decode(bytes));
  assert.equal(out.status, 'ok');
  assert.equal(out.result_json, '{"a":1}');
  assert.equal(out.stdout, 'out\n');
  assert.equal(out.cpu_ms, 7);
  assert.equal(out.cpu_source, 'tail');
  assert.equal(out.network, 'off');
  assert.equal(loads.length, 1);
  assert.equal(loads[0].globalOutbound, null, 'network off is globalOutbound null');
  assert.deepEqual(loads[0].env, {}, 'network off: no egress binding');
  assert.deepEqual(loads[0].limits, {cpuMs: 50, subRequests: 1});
  assert.deepEqual(loads[0].tails, [{kind: 'tail', props: {runId: RUN_ID}}]);
  assert.equal(loads[0].mainModule, '__main.js');
  assert.equal(loads[0].modules['user.js'], runRequest().code);
  assert.equal(ledgers.get(RUN_ID).ctx.storage.alarm > 0, true, 'the replay marker expires');
});

test('handle: network on keeps globalOutbound null and hands the gateway over as env.EGRESS', async () => {
  const net = policy({max_requests: 3});
  const {env, ctx, loads} = fakeEnv(async () => harnessAnswer());
  const res = await loader.handle(await signedRequest(runRequest({network: net})), env, ctx);
  assert.equal(res.status, 200);
  // globalOutbound null is what makes the runtime's connect() throw: a
  // gateway bound as globalOutbound let a deployed run open a raw socket.
  assert.equal(loads[0].globalOutbound, null, 'raw connect() must never have a route out');
  assert.deepEqual(loads[0].env, {EGRESS: {kind: 'gateway', props: {runId: RUN_ID, policy: net}}});
  assert.deepEqual(loads[0].limits, {cpuMs: 50, subRequests: 3});
  const out = await res.json();
  assert.equal(out.network, 'on');
  assert.equal(out.cpu_source, 'limit', 'no tail event: the CPU time counts as the limit');
  assert.equal(out.cpu_ms, 50);
});

test('handle: refuses unsigned, stale, wrongly signed, replayed and invalid requests', async () => {
  const {env, ctx} = fakeEnv(async () => harnessAnswer());
  const good = runRequest();
  assert.equal((await loader.handle(new Request('https://l/run', {method: 'POST', body: JSON.stringify(good)}), env, ctx)).status, 401);
  assert.equal((await loader.handle(await signedRequest(good, {secret: 'x'.repeat(40)}), env, ctx)).status, 401);
  assert.equal((await loader.handle(await signedRequest(good, {ts: Math.floor(Date.now() / 1000) - 3600}), env, ctx)).status, 401);
  assert.equal((await loader.handle(await signedRequest({...good, network: policy({allow_connect: true})}), env, ctx)).status, 400);
  assert.equal((await loader.handle(await signedRequest(good), env, ctx)).status, 200);
  assert.equal((await loader.handle(await signedRequest(good), env, ctx)).status, 409, 'a run ID runs once');
  assert.equal((await loader.handle(new Request('https://l/other', {method: 'POST'}), env, ctx)).status, 404);
  assert.equal((await loader.handle(new Request('https://l/health'), env, ctx)).status, 200);
  const huge = new Request('https://l/run', {method: 'POST', body: 'x'.repeat(LIMITS.requestBytes + 1)});
  assert.equal((await loader.handle(huge, env, ctx)).status, 413);
});

test('handle: timeout, CPU exceeded, bad output and oversized output', async () => {
  const cases = [
    ['timeout', () => new Promise(() => {}), 'timeout'],
    ['cpu', async () => { throw new Error('Worker exceeded CPU time limit.'); }, 'cpu_exceeded'],
    ['cpu-tail', async (c, o, l) => { await l.get(RUN_ID).tail({cpu_ms: 12, wall_ms: 12, outcome: 'exceededCpu', stdout: 'partial\n', stderr: ''}); throw new Error('The script will never generate a response.'); }, 'cpu_exceeded'],
    ['subrequests', async () => { throw new Error('Too many subrequests.'); }, 'subrequests_exceeded'],
    ['bad', async () => Response.json({hello: 'world'}), 'bad_output'],
    ['threw', async () => harnessAnswer({ok: false, result_json: null, error: 'TypeError: nope'}), 'error'],
  ];
  for (const [name, behave, status] of cases) {
    const {env, ctx} = fakeEnv(behave);
    const res = await loader.handle(await signedRequest(runRequest({limits: {...runRequest().limits, wall_ms: 150}})), env, ctx);
    const out = await res.json();
    assert.equal(out.status, status, name);
    if (status === 'cpu_exceeded') assert.equal(out.cpu_ms, 50, `${name}: charged the limit`);
    if (name === 'cpu-tail') assert.equal(out.stdout, 'partial\n', 'console lines come from the tail when the harness never answered');
    if (name === 'threw') assert.equal(out.error, 'TypeError: nope');
  }
  const {env, ctx} = fakeEnv(async () => harnessAnswer({stdout: 'z'.repeat(500), result_json: JSON.stringify('r'.repeat(200))}));
  const out = await (await loader.handle(await signedRequest(runRequest()), env, ctx)).json();
  assert.equal(out.stdout.length, 100);
  assert.equal(out.truncated.stdout, true);
  assert.equal(out.result_json, '');
  assert.equal(out.truncated.result, true);
});

test('fit keeps any answer inside the response cap', () => {
  const nasty = '\u0001'.repeat(LIMITS.outputBytesMax);
  const r = {schema: 1, run_id: RUN_ID, status: 'ok', error: '', result_json: JSON.stringify(nasty), stdout: nasty, stderr: nasty,
    truncated: {stdout: false, stderr: false, result: false}, cpu_ms: 1, cpu_source: 'tail', wall_ms: 1, outcome: 'ok', network: 'off',
    egress: {requests: 0, blocked: 0, bytes_out: 0, bytes_in: 0, log: [], log_truncated: false}};
  const bytes = loader.fit(r);
  assert.ok(bytes.length <= LIMITS.responseBytesMax, String(bytes.length));
  const back = JSON.parse(new TextDecoder().decode(bytes));
  assert.equal(back.truncated.result, true);
  assert.equal(back.truncated.stdout, true);
});

test('tailInfo reads CPU time, outcome and console lines', () => {
  const t = loader.tailInfo([{cpuTime: 3.5, wallTime: 8, outcome: 'ok', logs: [{level: 'log', message: ['a', {b: 1}]}, {level: 'error', message: ['e']}], exceptions: [{name: 'TypeError', message: 'x'}]}]);
  assert.deepEqual(t, {cpu_ms: 3.5, wall_ms: 8, outcome: 'ok', stdout: 'a {"b":1}\n', stderr: 'e\nTypeError: x\n'});
  assert.deepEqual(loader.tailInfo([]), {cpu_ms: null, wall_ms: null, outcome: '', stdout: '', stderr: ''});
});

test('handle: code naming cloudflare:sockets is refused before loading and logged as a blocked connect', async () => {
  for (const network of [policy(), policy({enabled: false, max_requests: 0})]) {
    const {env, ctx, loads} = fakeEnv(async () => harnessAnswer());
    const code = 'import { connect } from "cloudflare:sockets"; export async function run() { const s = connect("example.com:443"); await s.opened; return "open"; }';
    const res = await loader.handle(await signedRequest(runRequest({code, network})), env, ctx);
    assert.equal(res.status, 200);
    const out = await res.json();
    assert.equal(loads.length, 0, 'never loaded');
    assert.equal(out.status, 'error');
    assert.match(out.error, /raw TCP/);
    assert.equal(out.result_json, '');
    assert.deepEqual(out.egress.log.map(e => [e.method, e.verdict, e.reason]), [['CONNECT', 'blocked', 'raw_tcp']]);
    assert.equal(out.egress.requests, 0);
  }
});

test('the gateway logs raw connect() as blocked and never forwards it', async () => {
  const {env} = fakeEnv(async () => harnessAnswer());
  const led = env.RUN_LEDGER.get(RUN_ID);
  await led.start(policy(), Date.now());
  const gw = new loader.EgressGateway({props: {runId: RUN_ID, policy: policy()}}, env);
  let closed = false;
  await gw.connect({close: () => { closed = true; }});
  assert.equal(closed, true);
  const {egress: s} = await led.finish(0);
  assert.deepEqual(s.log.map(e => [e.method, e.reason]), [['CONNECT', 'raw_tcp']]);
});

// ---------------------------------------------------------------- security review 1.20 (H4)
// Each test inverts a proof of concept (test/secpoc.test.mjs on branch
// security-review-1.20): it fails while the weakness is present.

test('H4: the harness answers with its own Response even when the run replaces globalThis.Response', () => {
  const dir = mkdtempSync(join(tmpdir(), 'runs-harness-response-'));
  try {
    const code = `const R = globalThis.Response;
globalThis.Response = function (body, init) {
  return new R(new ReadableStream({ pull() { return new Promise(() => {}); } }), init);
};
JSON.stringify = () => { throw new Error("replaced"); };
export async function run() { return 7; }`;
    const mods = harness.buildModules(runRequest({code}));
    for (const [name, src] of Object.entries(mods.modules)) writeFileSync(join(dir, name), src);
    writeFileSync(join(dir, 'drive.mjs'), `const m = await import('./${mods.mainModule}');
const r = await m.default.fetch(new Request('https://run.invalid/', {method: 'POST', body: '{"input":null}'}));
const got = await Promise.race([r.text(), new Promise((res) => setTimeout(() => res('hung'), 1000))]);
process.stdout.write(got);
process.exit(0);`);
    const p = spawnSync(process.execPath, [join(dir, 'drive.mjs')], {encoding: 'utf8'});
    assert.equal(p.status, 0, p.stderr);
    assert.notEqual(p.stdout, 'hung', 'the harness answer never ended');
    assert.ok(harness.parseHarness(p.stdout), p.stdout);
  } finally {
    rmSync(dir, {recursive: true, force: true});
  }
});

test('H4: a harness answer whose body never ends is cut at wall_ms and answered as a timeout, egress kept', async () => {
  const {env, ctx} = fakeEnv(async (code, opts, ledgers) => {
    const led = ledgers.get(RUN_ID);
    const a = await led.admit({method: 'GET', host: 'example.com', port: 443, path_sha256: '', bytes_out: 10});
    await led.complete(a.seq, {status: 200, bytes_in: 20, ms: 1});
    return new Response(new ReadableStream({pull() { return new Promise(() => {}); }}), {headers: {'content-type': 'application/json'}});
  });
  const t0 = Date.now();
  const req = runRequest({network: policy(), limits: {...runRequest().limits, wall_ms: 150}});
  const outcome = await Promise.race([
    loader.handle(await signedRequest(req), env, ctx),
    new Promise((res) => setTimeout(() => res(null), 3000)),
  ]);
  assert.ok(outcome, 'the loader did not answer within wall_ms plus the tail wait');
  assert.ok(Date.now() - t0 < 150 + LIMITS.tailWaitMs + 1000);
  const out = await outcome.json();
  assert.equal(out.status, 'timeout');
  assert.equal(out.egress.requests, 1, 'the egress the run made is reported, to be charged and screened');
  assert.equal(out.egress.bytes_out + out.egress.bytes_in, 30);
});

test('H4: readCapped gives up at its deadline on a body that never ends', async () => {
  const body = new ReadableStream({pull() { return new Promise(() => {}); }});
  const t0 = Date.now();
  const got = await egress.readCapped(body, 100, 50);
  assert.equal(got.timedOut, true);
  assert.ok(Date.now() - t0 < 1000);
  const fine = await egress.readCapped(new Response('abc').body, 100, 1000);
  assert.equal(new TextDecoder().decode(fine.bytes), 'abc');
  assert.equal(!!fine.timedOut, false);
});
