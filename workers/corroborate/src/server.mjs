// SPDX-License-Identifier: Apache-2.0
// SwarmMemo's Corroborate sidecar: answers resolve(addressSet, asOf) on a
// loopback port for the board's corroborate service and its standing run.
//
// It runs the Corroborate SDK (vendor/corroborate-sdk, MIT), which reads every
// credential straight from public chains with no vendor key, against the
// ontology pinned in ontology/ (a registry revision copied with its SHA-256),
// never the live registry. It keeps no secrets and writes nothing to disk; it
// logs counts and timings, never an address.
//
// Environment (all optional):
//   CORROBORATE_LISTEN                 127.0.0.1:8787 (loopback only)
//   CORROBORATE_SUBGRAPH_URL           issuance dates for Proof of Humanity and Circles
//   CORROBORATE_REGISTRY_SUBGRAPH_URL  the registry audit trail; enables as_of
//   CORROBORATE_CACHE_TTL_S            3600: how long an answer is kept
//   CORROBORATE_TIMEOUT_MS             20000: the most one request waits
//   CORROBORATE_PROBE_TIMEOUT_MS       8000: the most one credential check waits
//   CORROBORATE_MAX_ACTIVE             4: resolves at once
//   CORROBORATE_BNB_RPC_URL            BNB Chain RPC for Galxe (an archive endpoint also dates it)
import {createServer} from 'node:http';
import {dirname, join} from 'node:path';
import {fileURLToPath} from 'node:url';
import {Corroborate} from '../vendor/corroborate-sdk/index.js';
import {createHandler, loadPinned} from './core.mjs';

const here = dirname(fileURLToPath(import.meta.url));

// PinnedCorroborate scores against the pinned ontology instead of reading the
// live registry: a registry change cannot move an answer until a new pin is
// committed. as_of still reconstructs the registry at that past block, which
// is immutable.
export class PinnedCorroborate extends Corroborate {
  #pinned;
  constructor(pinned, opts) {
    super(opts);
    this.#pinned = pinned;
  }
  async ontology() {
    return this.#pinned;
  }
  async refresh() {
    return this.#pinned;
  }
}

const int = (name, def, min, max) => {
  const raw = process.env[name];
  if (raw === undefined || raw === '') return def;
  const n = Number(raw);
  if (!Number.isSafeInteger(n) || n < min || n > max) throw new Error(`${name} must be a whole number from ${min} to ${max}`);
  return n;
};

export function listenAddress(value = '127.0.0.1:8787') {
  const m = /^(127\.0\.0\.1|\[::1\]):(\d{1,5})$/.exec(value);
  if (!m || Number(m[2]) < 1 || Number(m[2]) > 65535) throw new Error('CORROBORATE_LISTEN must be 127.0.0.1:PORT or [::1]:PORT: the sidecar answers the board on this host only');
  return {host: m[1].replace(/^\[|\]$/g, ''), port: Number(m[2])};
}

function main() {
  const {host, port} = listenAddress(process.env.CORROBORATE_LISTEN || undefined);
  const pinned = loadPinned(join(here, '../ontology'));
  const subgraphUrl = process.env.CORROBORATE_SUBGRAPH_URL || undefined;
  const registrySubgraphUrl = process.env.CORROBORATE_REGISTRY_SUBGRAPH_URL || undefined;
  const client = new PinnedCorroborate(pinned.ontology, {
    ...(subgraphUrl ? {subgraphUrl} : {}),
    ...(registrySubgraphUrl ? {registrySubgraphUrl} : {}),
    probeTimeoutMs: int('CORROBORATE_PROBE_TIMEOUT_MS', 8000, 1000, 60000),
    probeRetries: 1,
  });
  const handler = createHandler({
    resolve: (addresses, asOf) => client.resolve(addresses, asOf === undefined ? {} : {asOf}),
    pinned,
    asOfEnabled: Boolean(registrySubgraphUrl),
    cacheTtlMs: int('CORROBORATE_CACHE_TTL_S', 3600, 0, 86400) * 1000,
    timeoutMs: int('CORROBORATE_TIMEOUT_MS', 20000, 1000, 120000),
    maxActive: int('CORROBORATE_MAX_ACTIVE', 4, 1, 64),
    log: (line) => console.log(line),
  });
  const server = createServer((req, res) => {
    handler(req, res).catch((e) => {
      console.error(`handler: ${e instanceof Error ? e.message : String(e)}`);
      if (!res.headersSent) res.writeHead(500, {'content-type': 'application/json'});
      res.end('{"error":"internal"}');
    });
  });
  server.requestTimeout = 60_000;
  server.headersTimeout = 10_000;
  server.listen(port, host, () => {
    console.log(`corroborate sidecar on ${host}:${port}, registry revision ${pinned.registry.revision} (sha256 ${pinned.registry.sha256}), as_of ${registrySubgraphUrl ? 'on' : 'off'}`);
  });
  const stop = () => server.close(() => process.exit(0));
  process.on('SIGTERM', stop);
  process.on('SIGINT', stop);
}

if (process.argv[1] === fileURLToPath(import.meta.url)) main();
