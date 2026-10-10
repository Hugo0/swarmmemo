// SPDX-License-Identifier: Apache-2.0
// Live smoke test against public chains: starts the handler on an ephemeral
// loopback port in this process, resolves one address set over HTTP twice
// (the second answer must come from the cache) and stops.
//
//   node scripts/smoke.mjs [0xADDRESS ...]     (default: vitalik.eth's address)
import {createServer} from 'node:http';
import {dirname, join} from 'node:path';
import {fileURLToPath} from 'node:url';
import {createHandler, loadPinned} from '../src/core.mjs';
import {PinnedCorroborate} from '../src/server.mjs';

const here = dirname(fileURLToPath(import.meta.url));
const addresses = process.argv.slice(2);
if (addresses.length === 0) addresses.push('0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045');

const pinned = loadPinned(join(here, '../ontology'));
const subgraphUrl = process.env.CORROBORATE_SUBGRAPH_URL || undefined;
const client = new PinnedCorroborate(pinned.ontology, {...(subgraphUrl ? {subgraphUrl} : {}), probeTimeoutMs: 8000, probeRetries: 1});
const handler = createHandler({resolve: (a, asOf) => client.resolve(a, asOf === undefined ? {} : {asOf}), pinned, timeoutMs: 30000});
const server = createServer((req, res) => void handler(req, res));
await new Promise((ok) => server.listen(0, '127.0.0.1', ok));
const url = `http://127.0.0.1:${server.address().port}/resolve`;
try {
  for (let i = 0; i < 2; i++) {
    const t = Date.now();
    const r = await fetch(url, {method: 'POST', headers: {'content-type': 'application/json'}, body: JSON.stringify({addresses})});
    const body = await r.json();
    console.log(`HTTP ${r.status} in ${Date.now() - t} ms`);
    console.log(JSON.stringify(body, null, 2));
    if (r.status !== 200) process.exitCode = 1;
    if (i === 1 && body.cached !== true) {
      console.error('second answer was not cached');
      process.exitCode = 1;
    }
  }
} finally {
  server.close();
}
