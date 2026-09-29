// SPDX-License-Identifier: Apache-2.0
// Post-deploy smoke test for the runs loader: the parts node --test cannot
// cover (a real Dynamic Worker, globalOutbound, cpuMs, the tail event, the
// Durable Object). It sends signed runs to the deployed loader and checks the
// answers. It needs network access and the loader secret; it prints no secret.
//
//   RUNS_LOADER_URL=https://swarmmemo-runs-loader.SUBDOMAIN.workers.dev/run \
//   RUNS_SECRET_FILE=/etc/swarmmemo/runs-loader.secret \
//   node workers/runs-loader/test/smoke.mjs [--python]
import {readFileSync} from 'node:fs';
import {randomBytes, createHmac} from 'node:crypto';

const url = process.env.RUNS_LOADER_URL;
const secret = readFileSync(process.env.RUNS_SECRET_FILE ?? '', 'utf8').replace(/[\r\n]+$/, '');
if (!url || secret.length < 32) throw new Error('set RUNS_LOADER_URL and RUNS_SECRET_FILE');

const policy = (enabled) => ({enabled, max_requests: enabled ? 4 : 0, max_bytes_out: 4096, max_bytes_in: 262144, ports: [80, 443], deny_hosts: [], deny_cidrs: [], allow_connect: false, resolve: true});

async function send(bytes) {
  const ts = String(Math.floor(Date.now() / 1000));
  const sig = 'v1=' + createHmac('sha256', secret).update(`req.${ts}.`).update(bytes).digest('hex');
  const res = await fetch(url, {method: 'POST', body: bytes, headers: {'content-type': 'application/json', 'x-runs-timestamp': ts, 'x-runs-signature': sig}});
  const body = Buffer.from(await res.arrayBuffer());
  if (res.status === 200) {
    const rts = res.headers.get('x-runs-timestamp');
    const want = 'v1=' + createHmac('sha256', secret).update(`res.${rts}.`).update(body).digest('hex');
    if (res.headers.get('x-runs-signature') !== want) throw new Error('the answer signature does not verify');
  }
  return {status: res.status, json: res.status === 200 ? JSON.parse(body.toString('utf8')) : body.toString('utf8')};
}

async function run(name, code, {network = false, cpu = 200, language = 'javascript'} = {}) {
  const req = {schema: 1, run_id: randomBytes(16).toString('hex'), language, code, input: {n: 2},
    limits: {cpu_ms: cpu, subrequests: 4, wall_ms: 10000, stdout_bytes: 4096, stderr_bytes: 4096, result_bytes: 4096}, network: policy(network)};
  const bytes = Buffer.from(JSON.stringify(req));
  const out = await send(bytes);
  return {name, bytes, ...out};
}

const results = [];
function check(name, ok, detail) {
  results.push(ok);
  console.log(`${ok ? 'ok  ' : 'FAIL'} ${name}${ok ? '' : ' ' + JSON.stringify(detail).slice(0, 400)}`);
}

const off = await run('network off', `export async function run(i) { console.log("hi"); try { await fetch("https://example.com/"); return "fetched"; } catch (e) { return "blocked"; } }`);
check('network off: fetch throws', off.json.status === 'ok' && off.json.result_json === '"blocked"' && off.json.stdout === 'hi\n', off.json);
check('tail event: CPU time from the runtime', off.json.cpu_source === 'tail' && off.json.outcome !== '', off.json);
check('replay refused', (await send(off.bytes)).status === 409, null);

const on = await run('network on', `export async function run() { const r = await fetch("https://example.com/"); return r.status; }`, {network: true});
check('network on: fetch through the gateway', on.json.status === 'ok' && on.json.result_json === '200' && on.json.egress.requests === 1 && on.json.egress.log[0]?.verdict === 'allowed', on.json);

const meta = await run('metadata', `export async function run() { const r = await fetch("http://169.254.169.254/latest/meta-data/"); return [r.status, r.headers.get("x-runs-egress")]; }`, {network: true});
check('metadata address refused and logged', meta.json.result_json === '[403,"blocked"]' && meta.json.egress.log[0]?.reason === 'metadata', meta.json);

// Raw TCP. The fix is globalOutbound null: the runtime's connect() throws.
// A computed module name passes the loader's static check, so this reaches
// the runtime (network on, the case that once opened a socket).
const tcp = await run('raw tcp', `export async function run() { try { const { connect } = await import("cloudflare:" + "sock" + "ets"); const s = connect({hostname: "example.com", port: 443}); await s.opened; return "open"; } catch (e) { return "refused"; } }`, {network: true});
check('raw connect() refused', tcp.json.status === 'ok' && tcp.json.result_json === '"refused"', tcp.json);
const tcpOff = await run('raw tcp, network off', `export async function run() { try { const { connect } = await import("cloudflare:" + "sock" + "ets"); const s = connect({hostname: "example.com", port: 443}); await s.opened; return "open"; } catch (e) { return "refused"; } }`);
check('raw connect() refused with the network off', tcpOff.json.status === 'ok' && tcpOff.json.result_json === '"refused"', tcpOff.json);
// Defence in depth: code naming the module is refused before loading.
const named = await run('raw tcp, named', `import { connect } from "cloudflare:sockets"; export async function run() { return "loaded"; }`, {network: true});
check('cloudflare:sockets refused before loading and logged', named.json.status === 'error' && named.json.egress.log[0]?.reason === 'raw_tcp', named.json);
// A socket on the egress binding itself reaches the gateway, which closes it.
const binding = await run('raw tcp via EGRESS', `import { env } from "cloudflare:workers"; export async function run() { try { const s = env.EGRESS.connect("example.com:443"); await s.opened; const w = s.writable.getWriter(); await w.write(new TextEncoder().encode("GET / HTTP/1.0\\r\\nHost: example.com\\r\\n\\r\\n")); const r = await s.readable.getReader().read(); return r.done ? "closed" : "data"; } catch (e) { return "refused"; } }`, {network: true});
check('connect() on the egress binding never carries data', binding.json.result_json === '"closed"' || binding.json.result_json === '"refused"', binding.json);

const spin = await run('cpu', `export function run() { for (;;) {} }`, {cpu: 50});
check('cpuMs enforced', spin.json.status === 'cpu_exceeded', spin.json);

if (process.argv.includes('--python')) {
  const py = await run('python', `def run(i):\n    print("py")\n    return i["n"] * 21\n`, {language: 'python'});
  check('python', py.json.status === 'ok' && py.json.result_json === '42', py.json);
  const pyNet = await run('python network on', `import js\nasync def run(i):\n    r = await js.fetch("https://example.com/")\n    return r.status\n`, {language: 'python', network: true});
  check('python: fetch through the gateway', pyNet.json.status === 'ok' && pyNet.json.result_json === '200' && pyNet.json.egress.requests === 1, pyNet.json);
}

const failed = results.filter((ok) => !ok).length;
console.log(failed ? `${failed} check(s) failed` : 'all checks passed');
process.exit(failed ? 1 : 0);
