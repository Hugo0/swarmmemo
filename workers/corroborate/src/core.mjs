// SPDX-License-Identifier: Apache-2.0
// The sidecar's request handling, free of the SDK and of the network so the
// tests run it with a fake resolver: the pinned ontology and its hash, request
// validation, the answer's shape, the cache, the concurrency bound and the
// timeout. server.mjs wires it to the Corroborate SDK.
//
// The answer is a score, never a verdict: what an adversary would pay, in US
// cents, to obtain another identity with the same evidence, with every trust
// root's part. There is no "human: true" anywhere, by Corroborate's own rule.
import {createHash} from 'node:crypto';
import {readFileSync} from 'node:fs';
import {join} from 'node:path';

export const MAX_ADDRESSES = 10;
export const MAX_BODY_BYTES = 4096;
// Sepolia block the registry was deployed in: no ontology exists before it.
export const REGISTRY_GENESIS_BLOCK = 11344158;

const ADDRESS_RE = /^0x[0-9a-fA-F]{40}$/;

// loadPinned reads ontology/pinned.json and the snapshot it names, and
// refuses to start on a hash mismatch: the pin is the only ontology the
// sidecar scores against.
export function loadPinned(dir) {
  const pin = JSON.parse(readFileSync(join(dir, 'pinned.json'), 'utf8'));
  if (!/^registry-r\d+\.json$/.test(pin.file ?? '') || !/^[0-9a-f]{64}$/.test(pin.sha256 ?? '')) {
    throw new Error('ontology/pinned.json must name registry-rN.json and its sha256');
  }
  const bytes = readFileSync(join(dir, pin.file));
  const sha256 = createHash('sha256').update(bytes).digest('hex');
  if (sha256 !== pin.sha256) {
    throw new Error(`pinned ontology ${pin.file} has sha256 ${sha256}, not the pinned ${pin.sha256}`);
  }
  const snap = JSON.parse(bytes.toString('utf8'));
  const adapters = new Map(snap.adapters.map((a) => [a.id, a]));
  return {
    ontology: {adapters, revision: snap.revision},
    registry: {address: snap.registry, chain: snap.chain, chain_id: snap.chain_id, revision: snap.revision, block: snap.block, block_time: snap.block_time, sha256},
  };
}

export class RequestError extends Error {
  constructor(code, message) {
    super(message);
    this.code = code;
  }
}

// parseRequest checks {addresses:[...], as_of?}: 1 to 10 distinct EVM
// addresses (any case; SwarmMemo checks EIP-55 before it calls) and an
// optional Sepolia registry block.
export function parseRequest(body) {
  let req;
  try {
    req = JSON.parse(body);
  } catch {
    throw new RequestError('invalid_request', 'The body must be a JSON object: {"addresses":["0x..."],"as_of":BLOCK}.');
  }
  if (req === null || typeof req !== 'object' || Array.isArray(req)) {
    throw new RequestError('invalid_request', 'The body must be a JSON object: {"addresses":["0x..."],"as_of":BLOCK}.');
  }
  for (const key of Object.keys(req)) {
    if (key !== 'addresses' && key !== 'as_of') throw new RequestError('invalid_request', `Unknown field ${key}; send addresses and, optionally, as_of.`);
  }
  const list = req.addresses;
  if (!Array.isArray(list) || list.length === 0 || list.length > MAX_ADDRESSES) {
    throw new RequestError('invalid_request', `addresses takes 1 to ${MAX_ADDRESSES} EVM addresses.`);
  }
  const seen = new Set();
  const addresses = [];
  for (const a of list) {
    if (typeof a !== 'string' || !ADDRESS_RE.test(a)) throw new RequestError('invalid_request', 'Each address is 0x and 40 hex digits.');
    const k = a.toLowerCase();
    if (!seen.has(k)) {
      seen.add(k);
      addresses.push(k);
    }
  }
  let asOf;
  if (req.as_of !== undefined) {
    if (!Number.isSafeInteger(req.as_of) || req.as_of < REGISTRY_GENESIS_BLOCK) {
      throw new RequestError('invalid_request', `as_of is a Sepolia block number, ${REGISTRY_GENESIS_BLOCK} (the registry's first) or later.`);
    }
    asOf = req.as_of;
  }
  return {addresses, asOf};
}

// cacheKey names an address set at a block: order and case do not matter.
export function cacheKey(addresses, asOf) {
  return [...addresses].sort().join(',') + '@' + (asOf ?? 'head');
}

const round2 = (n) => Math.round(n * 100) / 100;

// shape turns the SDK's PersonhoodResult into the answer: the total, the log
// score, each root's contribution with the credential that set it (forge,
// rent, age), the checks that could not be read, the caveats and the
// registry revision it was scored against.
export function shape(result, pinned) {
  const adapters = pinned.ontology.adapters;
  const held = result.evidence.filter((e) => e.held);
  const unavailable = result.evidence.filter((e) => e.detail?.unavailable).map((e) => ({adapter: e.adapterId, address: e.observedOn}));
  const roots = result.roots.map((r) => {
    const group = held.filter((e) => e.trustRoot === r.trustRoot);
    const top = group.reduce((a, b) => (b.effectiveCostCents > a.effectiveCostCents ? b : a), group[0]);
    const def = top ? adapters.get(top.adapterId) : undefined;
    return {
      root: r.trustRoot,
      contribution_cents: round2(r.contributionCents),
      saturated: r.saturated,
      adapters: r.adapterIds,
      strongest: top ? {
        adapter: top.adapterId,
        name: top.adapterName,
        evidence_class: top.evidenceClass,
        observed_on: top.observedOn,
        forge_cents: top.forgeCostCents,
        rent_cents: top.rentCostCents,
        live: top.live,
        age_curve: def?.ageCurve ?? 'None',
        half_life_days: def?.decayHalfLifeDays ?? 0,
        issued_at: top.issuedAt ?? null,
        age_days: top.issuedAt !== undefined ? Math.max(0, Math.floor((result.computedAt - top.issuedAt) / 86400)) : null,
        age_weight: Number(top.freshness.toFixed(4)),
        source: top.sourceURI,
      } : null,
    };
  });
  const caveats = [...result.caveats];
  if (unavailable.length > 0) {
    caveats.push({
      code: 'checks-unavailable',
      message: `${unavailable.length} of ${result.evidence.length} credential checks could not be read just now. The score counts what was read, so it is a floor; ask again later for the rest.`,
    });
  }
  let registry = pinned.registry;
  let asOf = null;
  if (result.asOf) {
    const a = result.asOf;
    registry = {address: pinned.registry.address, chain: pinned.registry.chain, chain_id: pinned.registry.chain_id, revision: a.registryRevision, block: a.block, block_time: a.timestamp, sha256: null};
    asOf = {
      block: a.block, block_time: a.timestamp, registry_revision: a.registryRevision, audit_trail_complete: a.auditTrailComplete,
      issued_after_as_of: a.issuedAfterAsOf, existence_unverified: a.existenceUnverified, adapters_not_yet_in_registry: a.adaptersNotYetInRegistry,
    };
  }
  return {
    addresses: result.subjects,
    score: result.score,
    total_cents: round2(result.totalCostCents),
    independent_roots: result.independentRoots,
    roots,
    checks: {total: result.evidence.length, held: held.length, unavailable: unavailable.length},
    unavailable,
    caveats,
    registry,
    as_of: asOf,
    computed_at: result.computedAt,
  };
}

// Cache keeps answers by address set and block for ttl, at most max of them,
// evicting the oldest first. Nothing is written to disk.
export class Cache {
  constructor(max, now = Date.now) {
    this.max = max;
    this.now = now;
    this.map = new Map();
  }
  get(key) {
    const hit = this.map.get(key);
    if (!hit) return undefined;
    if (hit.until <= this.now()) {
      this.map.delete(key);
      return undefined;
    }
    return hit.value;
  }
  set(key, value, ttlMs) {
    if (ttlMs <= 0) return;
    this.map.delete(key);
    while (this.map.size >= this.max) this.map.delete(this.map.keys().next().value);
    this.map.set(key, {value, until: this.now() + ttlMs});
  }
}

function send(res, status, body, extra = {}) {
  const raw = JSON.stringify(body);
  res.writeHead(status, {'content-type': 'application/json', 'cache-control': 'no-store', ...extra});
  res.end(raw);
}

function unavailable(res, code, retryAfter, message) {
  send(res, 503, {error: code, retry_after: retryAfter, message}, {'retry-after': String(retryAfter)});
}

// createHandler is the HTTP handler: POST /resolve and GET /healthz.
//
//   resolve(addresses, asOf) -> Promise<PersonhoodResult>   the SDK call
//   pinned                  loadPinned's answer
//   asOfEnabled             whether as_of can be answered (a registry audit trail is configured)
//   cacheTtlMs              how long a full answer is kept (default 1 h)
//   partialTtlMs            how long an answer with unreadable checks is kept (default 60 s)
//   timeoutMs               the most a request waits for the SDK (default 20 s)
//   maxActive, maxQueued    resolves at once, and waiting for a slot
export function createHandler(opts) {
  const {resolve, pinned, asOfEnabled = false, cacheTtlMs = 3600_000, partialTtlMs = 60_000, timeoutMs = 20_000, maxActive = 4, maxQueued = 16, cacheMax = 2000, now = Date.now, log = () => {}} = opts;
  const cache = new Cache(cacheMax, now);
  const inflight = new Map();
  let active = 0;
  const waiting = [];

  const acquire = () => {
    if (active < maxActive) {
      active++;
      return Promise.resolve();
    }
    if (waiting.length >= maxQueued) return null;
    return new Promise((ok) => waiting.push(ok));
  };
  const release = () => {
    const next = waiting.shift();
    if (next) next();
    else active--;
  };

  // run resolves one address set once, however many requests ask for it at
  // the same time, and caches what it learns even after the asker gave up.
  const run = (key, addresses, asOf) => {
    let p = inflight.get(key);
    if (p) return p;
    const slot = acquire();
    if (slot === null) return null;
    p = slot.then(async () => {
      try {
        const result = await resolve(addresses, asOf);
        const body = shape(result, pinned);
        if (body.checks.total > 0 && body.checks.unavailable === body.checks.total) {
          // Nothing could be read: a zero here would be a fake.
          return {status: 503, error: 'unavailable'};
        }
        cache.set(key, body, body.checks.unavailable > 0 ? partialTtlMs : cacheTtlMs);
        return {status: 200, body};
      } catch (e) {
        log(`resolve failed: ${e instanceof Error ? e.message : String(e)}`);
        return {status: 503, error: 'unavailable'};
      } finally {
        release();
        inflight.delete(key);
      }
    });
    inflight.set(key, p);
    return p;
  };

  return async function handle(req, res) {
    const started = now();
    const url = req.url ?? '';
    if (url === '/healthz' && req.method === 'GET') {
      send(res, 200, {ok: true, registry: pinned.registry, as_of: asOfEnabled, active, queued: waiting.length, cached: cache.map.size});
      return;
    }
    if (url !== '/resolve') {
      send(res, 404, {error: 'not_found', message: 'POST /resolve or GET /healthz.'});
      return;
    }
    if (req.method !== 'POST') {
      send(res, 405, {error: 'method_not_allowed', message: 'POST /resolve.'}, {allow: 'POST'});
      return;
    }
    const chunks = [];
    let size = 0;
    for await (const chunk of req) {
      size += chunk.length;
      if (size > MAX_BODY_BYTES) {
        send(res, 413, {error: 'invalid_request', message: `The body is over ${MAX_BODY_BYTES} bytes.`});
        return;
      }
      chunks.push(chunk);
    }
    let parsed;
    try {
      parsed = parseRequest(Buffer.concat(chunks).toString('utf8'));
    } catch (e) {
      if (e instanceof RequestError) {
        send(res, 400, {error: e.code, message: e.message});
        return;
      }
      throw e;
    }
    if (parsed.asOf !== undefined && !asOfEnabled) {
      send(res, 400, {error: 'as_of_unavailable', message: `This resolver scores against registry revision ${pinned.registry.revision} only; leave as_of out.`});
      return;
    }
    const key = cacheKey(parsed.addresses, parsed.asOf);
    const hit = cache.get(key);
    if (hit) {
      send(res, 200, {...hit, cached: true});
      log(`200 cached n=${parsed.addresses.length} ${now() - started}ms`);
      return;
    }
    const p = run(key, parsed.addresses, parsed.asOf);
    if (p === null) {
      unavailable(res, 'busy', 5, 'The resolver is at capacity; retry in a few seconds.');
      log(`503 busy n=${parsed.addresses.length}`);
      return;
    }
    let timer;
    const timeout = new Promise((ok) => {
      timer = setTimeout(() => ok({status: 503, error: 'timeout'}), timeoutMs);
    });
    const out = await Promise.race([p, timeout]);
    clearTimeout(timer);
    if (out.status === 200) {
      send(res, 200, {...out.body, cached: false});
    } else if (out.error === 'timeout') {
      // The resolve keeps running and caches its answer for the retry.
      unavailable(res, 'timeout', 15, 'The chains did not all answer in time; retry shortly.');
    } else {
      unavailable(res, 'unavailable', 60, 'No credential check could be read just now; nothing was scored.');
    }
    log(`${out.status === 200 ? 200 : 503} ${out.error ?? 'ok'} n=${parsed.addresses.length} ${now() - started}ms`);
  };
}
