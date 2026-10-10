import { createPublicClient, http, pad, parseAbi } from 'viem';
import { lens, polygon } from 'viem/chains';
/**
 * Lens account, read from Lens Chain — with the Polygon v2 registry as an honest fallback.
 *
 * ## Which Lens, in 2026
 *
 * Lens left Polygon. Lens Chain (a ZKsync-stack L2, chain id 232, GHO gas) produced its first
 * block on 2025-02-21 and launched publicly on 2025-04-04, migrating ~650k v2 profiles; on
 * 2026-01-20 stewardship passed from Avara to Mask Network. The v2 `LensHub` on Polygon was
 * *not* frozen — it still answers, still holds every profile NFT (migration copied, it did not
 * burn), and still trickles new mints (+647 profiles in the 15 months after sunset, measured
 * 2026-07-25) — but the canonical registry is now the Lens Chain one, so that is what this
 * probe reads first.
 *
 * ## What this is evidence of, and what it is not
 *
 * **Account ownership, never personhood** — and cheaper than a Farcaster id. Account creation
 * on Lens Chain is gas-sponsored: a fresh account plus an auto-generated `lens/` username cost
 * the user nothing and the sponsor 0.0012 GHO (~0.12 cents, measured from a live creation tx on
 * 2026-07-25, gasUsed 440,888 at 2.77 gwei). There is no storage rent and no recurring price of
 * any kind. The ontology must price this at effectively zero and lean entirely on the `Ramp`:
 * an account that has survived since the v2 era is worth a few cents of aged-account-market
 * value; one minted this week is worth nothing.
 *
 * ## The read: every account leaves an ownership-transfer log
 *
 * Lens v3 has no owner-keyed registry. A user's "profile" is an `Account` *contract* deployed
 * by the `AccountFactory`, owned by their EOA, and nothing on chain maps owner → account as
 * state. Worse, `Lens_Account_Created.owner` is useless for a reverse lookup: **every** account
 * — migrated and brand-new alike — is created with `owner = LensFactory` and only then
 * transferred to the user (verified on migrated and current signups, 2026-07-25).
 *
 * What does work: the `Account` contract emits
 * `Lens_Account_OwnershipTransferred(address indexed previousOwner, address indexed newOwner)`
 * on construction and on every ownership change, and Lens Chain's own public RPC serves
 * `eth_getLogs` over the chain's *entire* history (6.1M blocks at measurement) filtered by the
 * indexed `newOwner` — sub-second warm, tens of seconds cold. So the probe is: one full-history
 * log query for transfers to the subject, then for each candidate account an `owner()` call to
 * confirm the subject still holds it, and a factory-scoped creation-log lookup to confirm the
 * account really was deployed by the canonical `AccountFactory` — without that last check,
 * anyone could emit the transfer event from a contract of their own and mint themselves a
 * credential.
 *
 * Because every ownership change emits the event, the last transfer-to-subject block *is* the
 * acquisition — no bisection, no continuity doubt, unlike Farcaster's `custodyOf` search. Note
 * the symmetric weakness: ownership transfer needs no consent from the recipient, so a Lens
 * account can be *planted* on any address. That direction pays no adversary (it gives weight
 * away), but it means `held: true` here must never be read as an act of the subject.
 *
 * ## Dating, and the migration
 *
 * `issuedAt` is the acquisition, not the account's creation — same custody principle as the
 * Farcaster adapter, because accounts are a traded asset. Three cases, told apart on chain:
 *
 * - **Fresh signup**: acquired in the account's creation block. Clean date.
 * - **Migration claim**: bulk-migrated accounts sat with Lens's migration custodian EOA
 *   (`0x6e32…dFEc`) until the v2 owner claimed them; `previousOwner` names it. The claim date
 *   understates the credential's true age — the v2 profile may date to 2022 — so the result
 *   carries `date-from-registry-import`, which on a Ramp makes the weight a floor.
 * - **Anything else changed hands**: flagged `credential-transferred-since-issuance` and dated
 *   from the sale, so a bought 2022-era account is priced at its purchase date, not its age. A
 *   migration claim routed through an address other than the known custodian lands here too —
 *   mislabeled in the conservative direction.
 *
 * ## The legacy fallback, bounded rather than guessed
 *
 * A v2 holder who never claimed on Lens Chain would read `held: false` above, yet their profile
 * NFT is real and sits in a registry that still answers. So when Lens Chain shows nothing, the
 * probe reads `LensHub.balanceOf` on Polygon. A bare boolean would hand the unknown-age ramp
 * midpoint to anyone who buys a dead v2 profile for dust, so the date is bounded with one
 * archive read at block 70,000,000 (2025-04-07, the sunset): held there → `issuedAt` = the
 * sunset timestamp, a floor that understates true age; not held there → acquired after sunset,
 * `issuedAfter` caps the ramp instead. Archive unreachable → no date at all, and the detail
 * says so. The fallback never fires for migrated-and-claimed users — their Polygon NFT still
 * exists, but the Lens Chain read already answered, which is also why this adapter is one
 * credential and not two: both registries describe the same account.
 */
/** Lens Chain AccountFactory (proxy). Every canonical account's creation log comes from here. */
export const LENS_ACCOUNT_FACTORY = '0x26C7fd63B06deb4F9E4B5955D540767b9Ac7bbaa';
/** Lens Chain LensFactory (proxy) — the creation-time owner of every account it deploys. */
export const LENS_FACTORY = '0x1fa75D26819Ac733bf7B1C1B36C3F8aEF32d2Cc0';
/** The `lens/` global namespace; `usernameOf(account)` names the account's handle. */
export const LENS_GLOBAL_NAMESPACE = '0x1aA55B9042f08f45825dC4b651B64c9F98Af4615';
/**
 * Lens's migration custodian. Bulk-migrated accounts were transferred here at creation and held
 * until the v2 owner claimed them, so a transfer *from* this address is a migration claim, not
 * a purchase. An EOA, not a contract — verified by `eth_getCode` on 2026-07-25.
 */
export const LENS_MIGRATION_CUSTODIAN = '0x6e32C691A2B6b9351a2C6144C01badCb568cdFEc';
/** `Lens_Account_Created(address indexed account, address indexed owner, …)`. */
export const LENS_ACCOUNT_CREATED_TOPIC = '0x72a01e0c4465eab653bb461bfff7caa13615c0cc7bec21448f78d06f78430887';
/** `Lens_Account_OwnershipTransferred(address indexed previousOwner, address indexed newOwner)`. */
export const LENS_OWNERSHIP_TRANSFERRED_TOPIC = '0x5a1371cbc5817916f19ff7b6c2ebe1e0050f17b29432e56d60188a4f391010e6';
/**
 * Lens Chain public endpoints. Both are Lens-operated (there is no independent keyless archive
 * for this chain yet — drpc's endpoint refused every request when checked on 2026-07-25), both
 * serve full-history `eth_getLogs`, and a cold owner-filtered scan can take ~40 s, which is why
 * the default timeout below is generous.
 */
export const LENS_RPCS = ['https://rpc.lens.xyz', 'https://rpc.lens.dev'];
// ------------------------------------------------------------ Polygon legacy
/** Lens v2 LensHub proxy on Polygon — "Lens Protocol Profiles", an ERC-721 of 665,569. */
export const LENS_V2_LENSHUB = '0xDb46d1Dc155634FbC732f92E853b10B288AD5a1d';
/**
 * The block that bounds the legacy fallback's date: 2025-04-07, three days after the Lens Chain
 * public launch that began v2's sunset. The timestamp is the block's own, read off Polygon on
 * 2026-07-25; a past block's timestamp is immutable, so it is safe to carry as a constant and
 * the live test re-reads it.
 */
export const LENS_V2_SUNSET_BLOCK = 70000000n;
export const LENS_V2_SUNSET_TIMESTAMP = 1_744_013_119;
/** Head reads only. */
export const LENS_POLYGON_RPC = 'https://polygon-bor-rpc.publicnode.com';
/** The sunset-block read needs archive state, which publicnode refuses; drpc serves it. */
export const LENS_POLYGON_ARCHIVE_RPC = 'https://polygon.drpc.org';
const ACCOUNT_ABI = parseAbi(['function owner() view returns (address)']);
const NAMESPACE_ABI = parseAbi(['function usernameOf(address user) view returns (string)']);
const HUB_ABI = parseAbi(['function balanceOf(address owner) view returns (uint256)']);
/**
 * Collapse raw transfer logs into one candidate per account, keeping the latest transfer.
 *
 * The latest matters: an account transferred to the subject, away, and back must be dated from
 * the return, or the interlude would be silently credited as tenure. The `owner()` check later
 * removes accounts whose story ended with "away".
 */
export function candidatesFromTransferLogs(logs) {
    const byAccount = new Map();
    for (const log of logs) {
        const previous = log.topics[1];
        if (previous === undefined)
            continue;
        const candidate = {
            account: log.address,
            acquiredAtBlock: BigInt(log.blockNumber),
            previousOwner: `0x${previous.slice(26)}`,
        };
        const existing = byAccount.get(log.address.toLowerCase());
        if (!existing || candidate.acquiredAtBlock > existing.acquiredAtBlock) {
            byAccount.set(log.address.toLowerCase(), candidate);
        }
    }
    return [...byAccount.values()].sort((a, b) => (a.acquiredAtBlock < b.acquiredAtBlock ? -1 : 1));
}
/**
 * How the subject came to own the account. `created` when acquired in the account's own
 * creation block (the constructor and the handover to the user happen in one transaction for
 * ordinary signups); `migration-claim` when the previous holder was Lens's migration custodian;
 * `transferred` otherwise — which conservatively includes any migration path we cannot prove.
 */
export function classifyAcquisition(opts) {
    if (opts.acquiredAtBlock === opts.creationBlock)
        return 'created';
    if (opts.previousOwner.toLowerCase() === LENS_MIGRATION_CUSTODIAN.toLowerCase()) {
        return 'migration-claim';
    }
    return 'transferred';
}
/** How many candidate accounts the probe will verify before reporting truncation instead. */
const CANDIDATE_CAP = 12;
export function lensAdapter(opts = {}) {
    const rpcUrls = opts.rpcUrls ?? LENS_RPCS;
    const timeoutMs = opts.timeoutMs ?? 45_000;
    const clients = rpcUrls.map((url) => createPublicClient({
        chain: lens,
        // tryEach already retries across every endpoint twice; viem retrying underneath it
        // would multiply requests against public endpoints for no benefit.
        transport: http(url, { timeout: timeoutMs, retryCount: 0 }),
    }));
    const polygonClient = createPublicClient({
        chain: polygon,
        transport: http(opts.polygonRpcUrl ?? LENS_POLYGON_RPC, { timeout: 15_000, retryCount: 0 }),
    });
    const polygonArchiveClient = createPublicClient({
        chain: polygon,
        transport: http(opts.polygonArchiveRpcUrl ?? LENS_POLYGON_ARCHIVE_RPC, {
            timeout: 15_000,
            retryCount: 0,
        }),
    });
    let next = 0;
    /** Rotate across the Lens Chain endpoints, naming every one that failed. */
    async function tryEach(what, fn) {
        const errors = [];
        for (let pass = 0; pass < 2; pass++) {
            if (pass > 0)
                await new Promise((r) => setTimeout(r, 500));
            for (let i = 0; i < clients.length; i++) {
                const at = (next + i) % clients.length;
                try {
                    const result = await fn(clients[at]);
                    next = (at + 1) % clients.length;
                    return result;
                }
                catch (e) {
                    errors.push(`${rpcUrls[at]}: ${(e instanceof Error ? e.message : String(e)).split('\n')[0]}`);
                }
            }
        }
        throw new Error(`${what} unreadable — ${[...new Set(errors)].join('; ')}`);
    }
    const logsFor = (params) => tryEach('Lens Chain logs', (client) => client.request({
        method: 'eth_getLogs',
        params: [
            {
                ...(params.address ? { address: params.address } : {}),
                fromBlock: '0x0',
                toBlock: 'latest',
                topics: params.topics,
            },
        ],
    }));
    /**
     * The legacy read. Only reached when Lens Chain shows no account, so it can never double
     * count a migrated-and-claimed profile — the primary read already answered for those.
     */
    async function legacyProbe(subject, headBlock) {
        const balance = await polygonClient.readContract({
            address: LENS_V2_LENSHUB,
            abi: HUB_ABI,
            functionName: 'balanceOf',
            args: [subject],
        });
        if (balance === 0n) {
            return {
                held: false,
                provenance: { heldFrom: 'chain', dateFrom: 'none', headBlock, notes: [] },
                detail: { lensChainAccounts: 0, polygonV2Profiles: 0 },
            };
        }
        // The date bound. Without it, a v2 profile bought for dust yesterday would collect the
        // unknown-age ramp midpoint, which is exactly the free weight this adapter exists to deny.
        let heldAtSunset;
        try {
            const then = await polygonArchiveClient.readContract({
                address: LENS_V2_LENSHUB,
                abi: HUB_ABI,
                functionName: 'balanceOf',
                args: [subject],
                blockNumber: LENS_V2_SUNSET_BLOCK,
            });
            heldAtSunset = then > 0n;
        }
        catch {
            // Archive endpoint is best-effort. Losing it loses the date, never the credential.
        }
        const notes = heldAtSunset ? ['date-from-registry-import'] : [];
        const provenance = {
            heldFrom: 'chain',
            dateFrom: heldAtSunset ? 'chain' : 'none',
            headBlock,
            notes,
        };
        return {
            held: true,
            // Held at the sunset: at least that old, and in truth older — a floor on the ramp.
            ...(heldAtSunset === true ? { issuedAt: LENS_V2_SUNSET_TIMESTAMP } : {}),
            // Not held at the sunset: acquired since, so the age is capped rather than unknown.
            ...(heldAtSunset === false ? { issuedAfter: LENS_V2_SUNSET_TIMESTAMP } : {}),
            provenance,
            detail: {
                source: 'polygon-v2-legacy',
                lensChainAccounts: 0,
                polygonV2Profiles: Number(balance),
                ...(heldAtSunset === undefined
                    ? { undated: 'polygon archive read failed; age unknown' }
                    : { heldAtSunset }),
            },
        };
    }
    return {
        adapterId: 'lens-account',
        probe: async (subject) => {
            try {
                const [head, transferLogs] = await Promise.all([
                    tryEach('Lens Chain head', (client) => client.getBlockNumber()),
                    logsFor({
                        topics: [LENS_OWNERSHIP_TRANSFERRED_TOPIC, null, pad(subject.toLowerCase())],
                    }),
                ]);
                const headBlock = Number(head);
                const candidates = candidatesFromTransferLogs(transferLogs);
                if (candidates.length === 0)
                    return await legacyProbe(subject, headBlock);
                // Oldest acquisition first, so the first verified candidate is the one whose date the
                // ramp should see. Verification is what stops the transfer event being spoofable: the
                // account must still be owned by the subject, and must have been deployed by the
                // canonical factory — an attacker can emit the event, but not fake either of those.
                let verified;
                let owned = 0;
                for (const candidate of candidates.slice(0, CANDIDATE_CAP)) {
                    const owner = (await tryEach(`owner() of ${candidate.account}`, (client) => client.readContract({ address: candidate.account, abi: ACCOUNT_ABI, functionName: 'owner' })));
                    if (owner.toLowerCase() !== subject.toLowerCase())
                        continue;
                    const creation = await logsFor({
                        address: LENS_ACCOUNT_FACTORY,
                        topics: [LENS_ACCOUNT_CREATED_TOPIC, pad(candidate.account.toLowerCase())],
                    });
                    if (creation.length === 0)
                        continue; // not a canonical Lens account; ignore it
                    owned++;
                    if (!verified) {
                        const creationOwnerTopic = creation[0].topics[2];
                        verified = {
                            candidate,
                            creationBlock: BigInt(creation[0].blockNumber),
                            creationOwner: (creationOwnerTopic
                                ? `0x${creationOwnerTopic.slice(26)}`
                                : '0x0000000000000000000000000000000000000000'),
                        };
                    }
                }
                if (!verified)
                    return await legacyProbe(subject, headBlock);
                const { candidate, creationBlock } = verified;
                const kind = classifyAcquisition({
                    creationBlock,
                    acquiredAtBlock: candidate.acquiredAtBlock,
                    previousOwner: candidate.previousOwner,
                });
                const acquiredAt = Number((await tryEach(`block ${candidate.acquiredAtBlock}`, (client) => client.getBlock({ blockNumber: candidate.acquiredAtBlock }))).timestamp);
                // The handle is decoration, not evidence — an account without one is still an account.
                let username;
                try {
                    const name = await tryEach('usernameOf', (client) => client.readContract({
                        address: LENS_GLOBAL_NAMESPACE,
                        abi: NAMESPACE_ABI,
                        functionName: 'usernameOf',
                        args: [candidate.account],
                    }));
                    if (name)
                        username = name;
                }
                catch {
                    // No username, or the namespace declined to answer. Neither changes the result.
                }
                const notes = [];
                if (kind === 'migration-claim')
                    notes.push('date-from-registry-import');
                if (kind === 'transferred')
                    notes.push('credential-transferred-since-issuance');
                return {
                    held: true,
                    issuedAt: acquiredAt,
                    provenance: { heldFrom: 'chain', dateFrom: 'chain', headBlock, notes },
                    detail: {
                        source: 'lens-chain',
                        account: candidate.account,
                        ...(username !== undefined ? { username } : {}),
                        lensChainAccounts: owned,
                        acquisition: kind,
                        acquiredAtBlock: Number(candidate.acquiredAtBlock),
                        createdAtBlock: Number(creationBlock),
                        migratedFromLensV2: kind === 'migration-claim',
                        ...(candidates.length > CANDIDATE_CAP
                            ? { candidatesTruncated: candidates.length - CANDIDATE_CAP }
                            : {}),
                    },
                };
            }
            catch (e) {
                return { held: false, error: e instanceof Error ? e.message : String(e) };
            }
        },
    };
}
