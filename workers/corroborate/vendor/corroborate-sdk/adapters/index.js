import { createPublicClient, http } from 'viem';
import { gnosis, worldchain, base } from 'viem/chains';
import { circlesIndexRead, pohIndexRead } from "../subgraph.js";
import { reconcileIndexAndChain, } from "../reconcile.js";
import { coinbaseVerificationOnchainAdapter, gitcoinPassportAdapter } from "./eas.js";
import { humanPassportAdapter } from "./human-passport.js";
import { lensAdapter } from "./lens.js";
import { farcasterAdapter } from "./farcaster.js";
import { holonymAdapters } from "./holonym.js";
import { lineaPohAdapter } from "./linea-poh.js";
import { pohV1Adapter } from "./poh-v1.js";
import { brightIdAdapter } from "./brightid.js";
import { civicPassAdapter } from "./civic.js";
import { galxePassportAdapter } from "./galxe.js";
import { humanodeAdapter } from "./humanode.js";
import { idenaAdapter } from "./idena.js";
import { selfAdapter } from "./self.js";
import { worldIdOrbAdapter, WORLD_AGENT_BOOK, WORLD_ID_ADDRESS_BOOK, WORLD_RPC } from "./world.js";
export * from "./eas.js";
export * from "./human-passport.js";
export * from "./lens.js";
export * from "./farcaster.js";
export * from "./holonym.js";
export * from "./linea-poh.js";
export * from "./poh-v1.js";
export * from "./brightid.js";
export * from "./civic.js";
export * from "./galxe.js";
export * from "./humanode.js";
export * from "./idena.js";
export * from "./self.js";
export * from "./world.js";
/**
 * Adapters.
 *
 * Every adapter here reads a credential **without the issuing vendor's cooperation** — no
 * API key on the critical path, nothing that can rate-limit or revoke us. That constraint
 * shaped the selection: World is read through its on-chain registries rather than the
 * verification API, because `WorldIDAddressBook.addressVerifiedUntil` and
 * `AgentBook.lookupHuman` are plain `eth_call`s that return the Orb-verified status of any
 * address with no proof, no relying-party id and no user interaction.
 *
 * A probe must never throw. A network failure returning `held: false` would silently
 * become "this person is not human", so failures surface as an `error` and are excluded
 * from scoring rather than counted as a negative.
 *
 * Where a probe uses an index, it reads the index **and** the block the index has reached,
 * then confirms against the chain at head — see `reconcile.ts` for why the reverse order was
 * a live scoring bug. A probe never asks the contract for existence and the index for the
 * date as if the two described the same moment.
 */
export const RPC = {
    gnosis: 'https://rpc.gnosischain.com',
    worldchain: WORLD_RPC,
    // base.org returns 403 to non-browser clients; publicnode does not.
    base: 'https://base-rpc.publicnode.com',
};
export const CONTRACTS = {
    /** Permissionless World ID lookup. groupId 1 == Orb-verified only. See `world.ts`. */
    worldAgentBook: WORLD_AGENT_BOOK,
    /** World's own registry of verified addresses, with a term and therefore a date. */
    worldIdAddressBook: WORLD_ID_ADDRESS_BOOK,
    /** Proof of Humanity v2 proxy on Gnosis. */
    pohV2: '0xa4AC94C4fa65Bb352eFa30e3408e64F72aC857bc',
    /** Circles v2 Hub on Gnosis. */
    circlesHub: '0xc12C1E50ABB450d6205Ea2C3Fa861b3B834d13e8',
};
const client = (chain, url) => createPublicClient({ chain, transport: http(url) });
async function safe(fn) {
    try {
        return await fn();
    }
    catch (e) {
        return { held: false, error: e instanceof Error ? e.message : String(e) };
    }
}
/**
 * Reconcile one index read against one chain read, filling in the indexed block's timestamp
 * if the index did not report one.
 *
 * That timestamp is not a detail: on the absence path it *is* the bound. Graph Node returns
 * null for `_meta.block.timestamp` on some queries, and without it "absent at block B" cannot
 * be turned into "issued after time T", so the probe would fall back to the unknown-age
 * midpoint for want of one `eth_getBlockByNumber`.
 */
async function reconcileWithIndex(opts) {
    let index = opts.index;
    if (index &&
        index.entity === null &&
        index.completeHistory &&
        index.blockTimestamp === undefined &&
        opts.chain.held) {
        try {
            const block = await opts.c.getBlock({ blockNumber: BigInt(index.block) });
            index = { ...index, blockTimestamp: Number(block.timestamp) };
        }
        catch {
            // Leave it unset: the reconciler then declines to bound the age, which is the honest
            // outcome rather than a guess.
        }
    }
    const reconciled = reconcileIndexAndChain({ chain: opts.chain, index });
    if (opts.indexConfigured && !index)
        reconciled.provenance.notes.push('index-unreachable');
    return reconciled;
}
// World ID lives in `world.ts`: two registries, a date derived from a fixed term, and the
// reasoning about why the document and Selfie tiers cannot be read from any chain.
// -------------------------------------------------------- Proof of Humanity
const POH_ABI = [
    {
        type: 'function',
        name: 'isHuman',
        stateMutability: 'view',
        inputs: [{ name: 'account', type: 'address' }],
        outputs: [{ type: 'bool' }],
    },
    {
        type: 'function',
        name: 'humanityOf',
        stateMutability: 'view',
        inputs: [{ name: 'account', type: 'address' }],
        outputs: [{ type: 'bytes20' }],
    },
    {
        type: 'function',
        name: 'getHumanityInfo',
        stateMutability: 'view',
        inputs: [{ name: 'humanityId', type: 'bytes20' }],
        outputs: [
            { name: 'vouching', type: 'bool' },
            { name: 'pendingRevocation', type: 'bool' },
            { name: 'nbPendingRequests', type: 'uint48' },
            { name: 'expirationTime', type: 'uint40' },
            { name: 'owner', type: 'address' },
            { name: 'nbRequests', type: 'uint256' },
        ],
    },
    {
        type: 'function',
        name: 'humanityLifespan',
        stateMutability: 'view',
        inputs: [],
        outputs: [{ type: 'uint40' }],
    },
];
/**
 * Proof of Humanity v2 on Gnosis.
 *
 * Weight this by registration age rather than the boolean. Roughly 1,299 of 1,364 lifetime
 * registrations arrived in a four-month window tracking a ~$9.94 PNK airdrop one-for-one,
 * `requiredNumberOfVouches()` is 1, and `HumanityRevoked` has fired exactly once ever.
 *
 * The date comes from the contract, not the index. `getHumanityInfo` returns the humanity's
 * `expirationTime`, and `humanityLifespan()` is the fixed term granted at claim, so
 * `expirationTime - humanityLifespan` is the claim timestamp — two `eth_call`s, no indexer,
 * nothing that can lag or rate-limit us. Verified against the index on a live registration:
 * 1815521110 - 31557600 = 1783963510, the exact `claimedAt` the subgraph reports.
 *
 * That removes PoH from the class of scores an index can move at all. The index stays useful
 * for what the contract cannot say — the vouch graph, revocation history, the daily
 * registration curve that exposes the airdrop — and now doubles as a cross-check: a
 * disagreement between the two dates is reported as a fault in our indexing.
 *
 * Note `nbRequests > 1` means the humanity was re-claimed or renewed, so the derived date is
 * the latest claim rather than the first registration. It is surfaced in `detail` because on
 * a survival ramp the difference matters.
 */
export function pohAdapter(rpcUrl = RPC.gnosis, subgraphUrl) {
    const c = client(gnosis, rpcUrl);
    /** Governance-settable but effectively constant; one read per adapter instance. */
    let lifespan;
    const readChain = async (subject) => {
        const detail = {};
        let block;
        try {
            const [head, held] = await Promise.all([
                c.getBlockNumber(),
                c.readContract({
                    address: CONTRACTS.pohV2,
                    abi: POH_ABI,
                    functionName: 'isHuman',
                    args: [subject],
                }),
            ]);
            block = Number(head);
            if (!held)
                return { held: false, block, detail };
            const humanityId = await c.readContract({
                address: CONTRACTS.pohV2,
                abi: POH_ABI,
                functionName: 'humanityOf',
                args: [subject],
            });
            detail.humanityId = humanityId;
            try {
                lifespan ??= c
                    .readContract({ address: CONTRACTS.pohV2, abi: POH_ABI, functionName: 'humanityLifespan' })
                    .then(Number);
                const [info, term] = await Promise.all([
                    c.readContract({
                        address: CONTRACTS.pohV2,
                        abi: POH_ABI,
                        functionName: 'getHumanityInfo',
                        args: [humanityId],
                    }),
                    lifespan,
                ]);
                const expirationTime = Number(info[3]);
                const nbRequests = Number(info[5]);
                detail.expirationTime = expirationTime;
                detail.nbRequests = nbRequests;
                if (nbRequests > 1)
                    detail.renewed = true;
                // Guard against a nonsense subtraction if either value is ever zero or the lifespan
                // is reconfigured to something larger than the expiry: better no date than a
                // fabricated one.
                if (expirationTime > term && term > 0) {
                    detail.claimedAt = expirationTime - term;
                    return { held: true, issuedAt: expirationTime - term, block, detail };
                }
            }
            catch {
                // The date is optional; losing it must not turn a positive into a negative or an
                // error. The reconciler falls back to the index, then to a bound, then to unknown.
            }
            return { held: true, block, detail };
        }
        catch (e) {
            return {
                held: false,
                unavailable: true,
                ...(block !== undefined ? { block } : {}),
                detail: { chainError: e instanceof Error ? e.message : String(e) },
            };
        }
    };
    return {
        adapterId: 'poh-v2',
        probe: (subject) => safe(async () => {
            const [index, chain] = await Promise.all([
                subgraphUrl ? pohIndexRead(subgraphUrl, subject) : undefined,
                readChain(subject),
            ]);
            const { detail, ...chainView } = chain;
            const r = await reconcileWithIndex({
                c,
                chain: chainView,
                ...(index ? { index } : {}),
                indexConfigured: Boolean(subgraphUrl),
            });
            return {
                held: r.held,
                ...(r.issuedAt !== undefined ? { issuedAt: r.issuedAt } : {}),
                ...(r.issuedAfter !== undefined ? { issuedAfter: r.issuedAfter } : {}),
                provenance: r.provenance,
                ...(r.error ? { error: detail.chainError ?? r.error } : {}),
                detail: {
                    ...detail,
                    ...(index?.entity ? { indexClaimedAt: index.entity.issuedAt } : {}),
                },
            };
        }),
    };
}
// ----------------------------------------------------------------- Circles
const CIRCLES_ABI = [
    {
        type: 'function',
        name: 'isHuman',
        stateMutability: 'view',
        inputs: [{ name: 'account', type: 'address' }],
        outputs: [{ type: 'bool' }],
    },
];
/**
 * Circles v2 on Gnosis — registration plus trust-graph position.
 *
 * Registration alone is close to worthless: a sybil costs about two days of freely-minted
 * CRC, and the public indexer carries event namespaces named `BotCreated` and `FarmGrown`.
 * What carries signal is position in the trust graph, so we fetch incoming trust edges and
 * expose them as detail for the graph-derived modifier.
 */
export function circlesAdapter(rpcUrl = RPC.gnosis, indexerUrl = 'https://rpc.aboutcircles.com/', subgraphUrl) {
    const c = client(gnosis, rpcUrl);
    const readChain = async (subject) => {
        try {
            const [head, held] = await Promise.all([
                c.getBlockNumber(),
                c.readContract({
                    address: CONTRACTS.circlesHub,
                    abi: CIRCLES_ABI,
                    functionName: 'isHuman',
                    args: [subject],
                }),
            ]);
            return { held, block: Number(head) };
        }
        catch (e) {
            return { held: false, unavailable: true, error: e instanceof Error ? e.message : String(e) };
        }
    };
    return {
        adapterId: 'circles-v2',
        probe: (subject) => safe(async () => {
            // The Hub stores no registration timestamp, so unlike PoH the date can only come from
            // an index — which is exactly why the reconciler has to be careful here. The deployed
            // subgraph indexes a ~2-month window of Circles, so an avatar's absence from it proves
            // nothing and must not be turned into an age bound.
            const [index, chain] = await Promise.all([
                subgraphUrl ? circlesIndexRead(subgraphUrl, subject) : undefined,
                readChain(subject),
            ]);
            const { error: chainError, ...chainView } = chain;
            const r = await reconcileWithIndex({
                c,
                chain: chainView,
                ...(index ? { index } : {}),
                indexConfigured: Boolean(subgraphUrl),
            });
            if (r.error) {
                return { held: false, provenance: r.provenance, error: chainError ?? r.error };
            }
            if (!r.held)
                return { held: false, provenance: r.provenance };
            if (index?.entity) {
                return {
                    held: true,
                    ...(r.issuedAt !== undefined ? { issuedAt: r.issuedAt } : {}),
                    provenance: r.provenance,
                    detail: {
                        ...(index.trustedByCount !== undefined ? { trustedBy: index.trustedByCount } : {}),
                        source: 'subgraph',
                    },
                };
            }
            // No indexed avatar: fall back to the vendor indexer for graph position only. It can
            // rate-limit or vanish, so it never decides `held` — the contract already did.
            let trustedBy;
            try {
                const res = await fetch(indexerUrl, {
                    method: 'POST',
                    headers: { 'content-type': 'application/json' },
                    body: JSON.stringify({
                        jsonrpc: '2.0',
                        id: 1,
                        method: 'circles_query',
                        params: [
                            {
                                Namespace: 'V_CrcV2',
                                Table: 'TrustRelations',
                                Columns: ['truster'],
                                Filter: [
                                    { Type: 'FilterPredicate', FilterType: 'Equals', Column: 'trustee', Value: subject.toLowerCase() },
                                ],
                                Order: [],
                            },
                        ],
                    }),
                    signal: AbortSignal.timeout(12_000),
                });
                const json = (await res.json());
                trustedBy = json.result?.rows?.length;
            }
            catch {
                // Indexer is best-effort; registration alone still counts.
            }
            return {
                held: true,
                ...(r.issuedAt !== undefined ? { issuedAt: r.issuedAt } : {}),
                ...(r.issuedAfter !== undefined ? { issuedAfter: r.issuedAfter } : {}),
                provenance: r.provenance,
                detail: trustedBy === undefined ? {} : { trustedBy },
            };
        }),
    };
}
// ---------------------------------------------------- Coinbase Verifications
const COINBASE_VERIFIED_ACCOUNT_SCHEMA = '0xf8b05c79f090979bf4a80270aba232dff11a10d9ca55c4f88de95317970f0de9';
/**
 * Coinbase Verified Account, read from EAS on Base.
 *
 * Persona-rooted, per Coinbase's own third-party vendor disclosure — so it shares a trust
 * root with every other Persona-backed credential and must not be counted beside them.
 *
 * Revocation is checked explicitly rather than inferred from presence: 720,503 of these
 * have been issued and 406,022 revoked, so presence alone is wrong more than half the time.
 */
export function coinbaseVerificationAdapter(easGraphQL = 'https://base.easscan.org/graphql') {
    return {
        adapterId: 'coinbase-verification',
        probe: (subject) => safe(async () => {
            const res = await fetch(easGraphQL, {
                method: 'POST',
                headers: { 'content-type': 'application/json' },
                body: JSON.stringify({
                    query: `query($where: AttestationWhereInput) {
              attestations(where: $where, orderBy: { time: desc }, take: 1) {
                id revoked revocationTime time expirationTime attester
              }
            }`,
                    variables: {
                        where: {
                            recipient: { equals: subject },
                            schemaId: { equals: COINBASE_VERIFIED_ACCOUNT_SCHEMA },
                            revoked: { equals: false },
                        },
                    },
                }),
                signal: AbortSignal.timeout(15_000),
            });
            const json = (await res.json());
            const att = json.data?.attestations?.[0];
            if (!att)
                return { held: false };
            return {
                held: true,
                issuedAt: Number(att.time),
                detail: { attestationId: att.id, attester: att.attester, revoked: att.revoked },
            };
        }),
    };
}
/** Every adapter that can be read without vendor cooperation. */
export function defaultAdapters(opts) {
    return [
        worldIdOrbAdapter(),
        pohAdapter(RPC.gnosis, opts?.subgraphUrl),
        circlesAdapter(RPC.gnosis, undefined, opts?.subgraphUrl),
        // On-chain read (Coinbase's own AttestationIndexer + getAttestation) replaced the
        // easscan GraphQL read in defaultAdapters on 2026-07-25. Same adapterId, same
        // credential; the GraphQL adapter remains exported for callers who want it.
        coinbaseVerificationOnchainAdapter(),
        humanPassportAdapter(),
        gitcoinPassportAdapter(),
        farcasterAdapter(),
        lensAdapter(),
        ...holonymAdapters(),
        lineaPohAdapter(),
        pohV1Adapter(),
        selfAdapter(),
        idenaAdapter(),
        humanodeAdapter(),
        brightIdAdapter(),
        galxePassportAdapter(),
        civicPassAdapter(),
    ];
}
