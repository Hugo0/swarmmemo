import { createPublicClient, fallback, http, keccak256, toHex } from 'viem';
import { sepolia } from 'viem/chains';
/**
 * Loads the trust-root ontology from the on-chain registry.
 *
 * The weights live on-chain rather than in this package for two reasons. Protocols change
 * faster than releases — Proof of Humanity's credential lost most of its meaning inside four
 * months when an airdrop drove ~95% of its registrations — and every consumer needs to
 * correct at once rather than at their next `npm update`. And a weight nobody can audit is
 * just an opinion in a black box: on-chain, each carries its source and every change emits
 * an event, so a subject can ask why their score moved and get an answer with a block number.
 */
export const REGISTRY_ABI = [
    {
        type: 'function',
        name: 'allAdapters',
        stateMutability: 'view',
        inputs: [],
        outputs: [
            { name: 'ids', type: 'bytes32[]' },
            {
                name: 'adapters',
                type: 'tuple[]',
                components: [
                    { name: 'name', type: 'string' },
                    { name: 'evidenceClass', type: 'uint8' },
                    { name: 'trustRoot', type: 'bytes32' },
                    { name: 'forgeCostCents', type: 'uint64' },
                    { name: 'rentCostCents', type: 'uint64' },
                    { name: 'decayHalfLifeDays', type: 'uint32' },
                    { name: 'ageCurve', type: 'uint8' },
                    { name: 'live', type: 'bool' },
                    { name: 'exists', type: 'bool' },
                    { name: 'sourceURI', type: 'string' },
                ],
            },
        ],
    },
    { type: 'function', name: 'revision', stateMutability: 'view', inputs: [], outputs: [{ type: 'uint64' }] },
    {
        type: 'function',
        name: 'adaptersByTrustRoot',
        stateMutability: 'view',
        inputs: [{ name: 'trustRoot', type: 'bytes32' }],
        outputs: [{ type: 'bytes32[]' }],
    },
];
const EVIDENCE_CLASSES = [
    'Unspecified',
    'Uniqueness',
    'StateIdentity',
    'SocialTrust',
    'Liveness',
    'Behavioral',
];
const AGE_CURVES = ['None', 'Decay', 'Ramp'];
/**
 * The registry stores both enums as `uint8`. Decoding lives here rather than at each call
 * site because `as-of.ts` reads the same two fields out of the audit-trail subgraph, and two
 * copies of an ordinal table is exactly the thing that drifts when the contract gains a class.
 */
export const decodeEvidenceClass = (n) => (EVIDENCE_CLASSES[n] ?? 'Behavioral');
export const decodeAgeCurve = (n) => AGE_CURVES[n] ?? 'None';
/** Adapter and root ids are hashed identically on-chain and here. */
export const adapterKey = (id) => keccak256(toHex(`adapter:${id}`));
export const rootKey = (root) => keccak256(toHex(`root:${root}`));
/**
 * Reads the whole ontology in a single call.
 *
 * The registry stores hashed ids to keep storage cheap, so we reverse them against the
 * known-id list. An adapter we cannot name is still returned under its hash rather than
 * dropped — silently discarding an adapter would understate correlation, which fails in the
 * adversary's favour.
 */
/**
 * Default Sepolia endpoints for the registry read, tried in order. This is the single most
 * load-bearing call in the SDK — no ontology, no weights, no answer — so it does not get to
 * depend on one public endpoint's uptime. An explicit `rpcUrl` disables the fallback: a
 * caller who chose an endpoint chose it for a reason.
 */
export const REGISTRY_RPCS = [
    'https://ethereum-sepolia-rpc.publicnode.com',
    'https://sepolia.drpc.org',
    'https://1rpc.io/sepolia',
];
export async function loadOntology(opts) {
    const client = createPublicClient({
        chain: sepolia,
        transport: opts.rpcUrl ? http(opts.rpcUrl) : fallback(REGISTRY_RPCS.map((u) => http(u))),
    });
    const [[ids, rows], revision] = await Promise.all([
        client.readContract({ address: opts.registryAddress, abi: REGISTRY_ABI, functionName: 'allAdapters' }),
        client.readContract({ address: opts.registryAddress, abi: REGISTRY_ABI, functionName: 'revision' }),
    ]);
    const idByHash = new Map((opts.knownIds ?? []).map((id) => [adapterKey(id), id]));
    const rootByHash = new Map((opts.knownRoots ?? []).map((r) => [rootKey(r), r]));
    const adapters = new Map();
    ids.forEach((hash, i) => {
        const row = rows[i];
        if (!row)
            return;
        const id = idByHash.get(hash) ?? hash;
        adapters.set(id, {
            id,
            name: row.name,
            evidenceClass: decodeEvidenceClass(row.evidenceClass),
            trustRoot: rootByHash.get(row.trustRoot) ?? row.trustRoot,
            forgeCostCents: Number(row.forgeCostCents),
            rentCostCents: Number(row.rentCostCents),
            decayHalfLifeDays: row.decayHalfLifeDays,
            ageCurve: decodeAgeCurve(row.ageCurve),
            live: row.live,
            sourceURI: row.sourceURI,
        });
    });
    return { adapters, revision: Number(revision) };
}
