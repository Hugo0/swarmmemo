// SPDX-License-Identifier: Apache-2.0
// Pins Corroborate's trust-root ontology: reads the registry on Sepolia at one
// block, writes ontology/registry-rREVISION.json and points
// ontology/pinned.json at it with its SHA-256. The sidecar scores against the
// pinned copy only, so a registry change moves no SwarmMemo answer until a new
// pin is reviewed and committed (RFC0015 §3.4).
//
//   node scripts/pin-ontology.mjs [--block N] [--rpc URL]
import {createHash} from 'node:crypto';
import {readFileSync, writeFileSync} from 'node:fs';
import {dirname, join} from 'node:path';
import {fileURLToPath} from 'node:url';
import {createPublicClient, fallback, http} from 'viem';
import {sepolia} from 'viem/chains';
import {REGISTRY_ABI, REGISTRY_RPCS, adapterKey, rootKey, decodeAgeCurve, decodeEvidenceClass} from '../vendor/corroborate-sdk/ontology.js';
import {DEFAULT_REGISTRY} from '../vendor/corroborate-sdk/index.js';

const here = dirname(fileURLToPath(import.meta.url));
const args = process.argv.slice(2);
const flag = (name) => {
  const i = args.indexOf(name);
  return i >= 0 ? args[i + 1] : undefined;
};

const known = JSON.parse(readFileSync(join(here, '../vendor/corroborate-sdk/ontology-data.json'), 'utf8'));
const idByHash = new Map(known.adapters.map((a) => [adapterKey(a.id), a.id]));
const roots = [...Object.keys(known.trustRoots), ...Object.keys(known.retiredTrustRoots ?? {})];
const rootByHash = new Map(roots.map((r) => [rootKey(r), r]));

const rpc = flag('--rpc');
const client = createPublicClient({chain: sepolia, transport: rpc ? http(rpc) : fallback(REGISTRY_RPCS.map((u) => http(u)))});
const blockNumber = flag('--block') ? BigInt(flag('--block')) : await client.getBlockNumber();
const block = await client.getBlock({blockNumber});
const read = (functionName) => client.readContract({address: DEFAULT_REGISTRY, abi: REGISTRY_ABI, functionName, blockNumber});
const [[ids, rows], revision] = await Promise.all([read('allAdapters'), read('revision')]);

const adapters = [];
ids.forEach((hash, i) => {
  const row = rows[i];
  if (!row) return;
  const id = idByHash.get(hash);
  const trustRoot = rootByHash.get(row.trustRoot);
  // A hash we cannot name would be scored under its hash; a pin is reviewed
  // by people, so it must read as names.
  if (!id || !trustRoot) throw new Error(`registry entry ${hash} (root ${row.trustRoot}) has no known name; re-vendor the SDK first`);
  adapters.push({
    id, name: row.name, evidenceClass: decodeEvidenceClass(row.evidenceClass), trustRoot,
    forgeCostCents: Number(row.forgeCostCents), rentCostCents: Number(row.rentCostCents),
    decayHalfLifeDays: row.decayHalfLifeDays, ageCurve: decodeAgeCurve(row.ageCurve), live: row.live, sourceURI: row.sourceURI,
  });
});
adapters.sort((a, b) => (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));

const pin = {
  registry: DEFAULT_REGISTRY,
  chain: 'sepolia',
  chain_id: sepolia.id,
  block: Number(blockNumber),
  block_time: Number(block.timestamp),
  revision: Number(revision),
  adapters,
};
const file = `registry-r${pin.revision}.json`;
const bytes = JSON.stringify(pin, null, 2) + '\n';
writeFileSync(join(here, '../ontology', file), bytes);
const sha256 = createHash('sha256').update(bytes).digest('hex');
writeFileSync(join(here, '../ontology/pinned.json'), JSON.stringify({file, sha256}, null, 2) + '\n');
console.log(`pinned revision ${pin.revision} at Sepolia block ${pin.block}: ${adapters.length} adapters, sha256 ${sha256}`);
