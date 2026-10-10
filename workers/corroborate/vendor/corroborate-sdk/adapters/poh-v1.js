import { createPublicClient, fallback, http, parseAbi } from 'viem';
import { mainnet } from 'viem/chains';
/**
 * Proof of Humanity **v1**, read from the original registry on Ethereum mainnet.
 *
 * Same trust root as v2 (`social-vouching:poh`) and deliberately so: a subject registered in
 * both holds one vouched identity, not two, and saturation is the thing that says so. This
 * adapter exists to make that collapse observable on real data rather than asserted.
 *
 * ## `isRegistered` is a comparison, not a presence check
 *
 * The registry's own getter is the whole mechanism, and the verified source says it plainly:
 *
 *     function isRegistered(address _submissionID) external view returns (bool) {
 *         Submission storage submission = submissions[_submissionID];
 *         return submission.registered && now - submission.submissionTime <= submissionDuration;
 *     }
 *
 * `submission.registered` is **never cleared on expiry** — only a governor removal or a lost
 * revocation request clears it — so the struct's boolean stays true forever while the credential
 * dies of arithmetic. Of 215 addresses sampled from the registry's recent request history,
 * **33 have `registered == true` and `isRegistered() == false`**. Reading the struct field, which
 * is the obvious thing to do given `getSubmissionInfo` returns it, would count every one of them
 * as a verified human.
 *
 * The comparison is exact and was checked against history rather than trusted: for submission
 * `0xfc3e23d4…3563` (`submissionTime` 1,642,619,590, term 63,115,200) `isRegistered` is **true**
 * at block 19,046,504 — timestamp 1,705,734,779, eleven seconds before expiry — and **false** at
 * block 19,046,505, one second after. Nothing was written to the contract in between.
 *
 * `submissionDuration()` is governance-settable and *has* moved: it was 31,557,600 s (365.25 days)
 * at the registry's first submission in 2021-03 and is 63,115,200 s (730.5 days) today. So the
 * probe reads it rather than pinning it, and reports the expiry it implies.
 *
 * ## The ForkModule decides whether the credential is still the subject's to spend
 *
 * PoH v2 cannot write to the frozen v1 contract, so it keeps its own overlay: `ForkModule`
 * (`0x068a27Db…9cCB`, initialised 2024-09-05) with `mapping(address => bool) removed`. v2 sets it
 * through `tryRemove` when a v1 submission is claimed or transferred into v2, and through
 * `remove` when a humanity is revoked, loses a revocation request, or is punished for bad
 * vouching. In every one of those cases the PoH ecosystem has stopped honouring the v1
 * registration while the v1 contract itself goes on answering `true` until the term runs out.
 *
 * So `held` is `v1.isRegistered(subject) && !forkModule.removed(subject)`. Both directions of
 * getting this wrong are real: without the flag a human revoked by v2's arbitration keeps
 * scoring here for up to two years, and a migrated registration is counted a second time beside
 * the v2 humanity it became. Saturation would absorb the second case — same root — but not the
 * first, and reporting a revoked credential as held is a false statement about a person whether
 * or not it moves the number.
 *
 * We do **not** use `ForkModule.isRegistered`, which is the same two conditions plus
 * `submissionTime < forkTime`. That last one is v2's migration policy — "this registration is old
 * enough to be brought across" — and not a statement about whether a v1 credential exists. It
 * matters here rather than being a hypothetical: both of the registrations alive today were made
 * *after* the fork, so `ForkModule.isRegistered` is false for the entire live population.
 *
 * ## What is actually left of v1
 *
 * Enumerated from the chain at block 25,610,404 (2026-07-25): **20,740 submissions in the
 * registry's lifetime, 2 of them currently registered.** The enumeration is every address that
 * ever emitted `AddSubmission` or `ReapplySubmission` (20,682 distinct, full history from the
 * deployment block), which is complete for anything that can be alive today because
 * `submissionTime` is only ever written by a path one of those two events precedes — with one
 * exception that is provably harmless: `addSubmissionManually` emits no `AddSubmission` at all,
 * and the governor used it for 63 submissions on 2021-03-12 (visible as a permanent gap between
 * `submissionCounter` and the distinct event count, opened between blocks 12,023,878 and
 * 12,023,879). Those 63 are dated 2021 and expired years ago under either term; had any of them
 * renewed, the renewal would have emitted `ReapplySubmission` and be in the set.
 *
 * Both survivors expire in 2026 — 2026-09-07 and 2026-11-16 — after which the registry holds
 * nobody unless somebody registers afresh. That is what `live: true` means for this adapter: the
 * contract works, the population is two people, and the ontology note says so. A protocol that
 * still answers is not the same as a protocol that still has users, and the honest way to
 * aggregate the difference is to read it rather than to describe it.
 */
/**
 * Keyless Ethereum mainnet endpoints, in a viem `fallback`.
 *
 * The probe reads at a pinned recent block, so it needs no archive access — the block is inside
 * every node's 128-block state window. A node that is *behind* the pinned block errors, which is
 * exactly what the fallback is for. Measured 2026-07-25: `cloudflare-eth.com` refuses
 * (`Cannot fulfill request`), `rpc.ankr.com/eth` and `1rpc.io/eth` demand a key, and
 * `eth.llamarpc.com` serves HTML. The four below answer.
 */
export const POH_V1_RPCS = [
    'https://gateway.tenderly.co/public/mainnet',
    'https://rpc.mevblocker.io',
    'https://eth.drpc.org',
    'https://ethereum-rpc.publicnode.com',
];
/** The original Proof of Humanity registry. Verified source, via Sourcify. */
export const POH_V1_REGISTRY = '0xC5E9dDebb09Cd64DfaCab4011A0D5cEDaf7c9BDb';
/** PoH v2's overlay on the frozen v1 registry. Proxy; implementation `0xfd6a2eda…4f66`. */
export const POH_V1_FORK_MODULE = '0x068a27Db9c3B8595D03be263d52c813cb2C99cCB';
/** Block of the registry's first `AddSubmission`, and the floor for any date it reports. */
export const POH_V1_FIRST_SUBMISSION_BLOCK = 12_012_815;
export const POH_V1_FIRST_SUBMISSION_AT = 1_615_406_218;
/**
 * `forkTime` on the ForkModule: the instant v2 froze which v1 registrations it would accept.
 * Never used to decide anything — the probe reads it — but the live suite holds the chain to it.
 */
export const POH_V1_FORK_TIME = 1_725_548_159;
/** `submissionDuration()` today. Read at runtime; here so the live suite can notice a change. */
export const POH_V1_SUBMISSION_DURATION = 63_115_200;
/** `submissionCounter()` — submissions ever created, including the 63 that emitted no event. */
export const POH_V1_LIFETIME_SUBMISSIONS = 20_740;
export const POH_V1_ABI = parseAbi([
    'function isRegistered(address) view returns (bool)',
    'function submissionDuration() view returns (uint64)',
    'function submissionCounter() view returns (uint256)',
    'function getSubmissionInfo(address) view returns (uint8 status, uint64 submissionTime, uint64 index, bool registered, bool hasVouched, uint256 numberOfRequests)',
    'event AddSubmission(address indexed _submissionID, uint256 _requestID)',
    'event ReapplySubmission(address indexed _submissionID, uint256 _requestID)',
]);
export const POH_V1_FORK_MODULE_ABI = parseAbi([
    'function removed(address) view returns (bool)',
    'function isRegistered(address) view returns (bool)',
    'function forkTime() view returns (uint40)',
    'function submissionDuration() view returns (uint40)',
    'function proofOfHumanityV1() view returns (address)',
    'function proofOfHumanityV2() view returns (address)',
]);
/** `Submission.status` in the v1 contract. Only `None` is a settled state. */
export const POH_V1_STATUS = ['None', 'Vouching', 'PendingRegistration', 'PendingRemoval'];
/**
 * Turn one pinned read of PoH v1 into a probe result.
 *
 * Pure, so every branch — never registered, registered and live, registered and lapsed,
 * retired into v2, renewed, and the two ways a date can be impossible — is testable without a
 * network.
 */
export function interpretPohV1Read(r) {
    const detail = { chain: 'ethereum', registry: POH_V1_REGISTRY };
    const notes = [];
    if (r.index > 0 || r.submissionTime > 0 || r.registeredFlag) {
        detail.submissionIndex = r.index;
        detail.status = POH_V1_STATUS[r.status] ?? `unknown(${r.status})`;
        detail.numberOfRequests = r.numberOfRequests;
    }
    if (r.submissionTime > 0) {
        detail.submissionTime = r.submissionTime;
        if (r.submissionDuration !== undefined && r.submissionDuration > 0) {
            const expiresAt = r.submissionTime + r.submissionDuration;
            detail.submissionDurationSeconds = r.submissionDuration;
            detail.expiresAt = expiresAt;
            detail[r.isRegistered ? 'expiresInDays' : 'lapsedDaysAgo'] =
                Math.round((Math.abs(expiresAt - r.now) / 86_400) * 10) / 10;
        }
    }
    // The struct's flag outliving the credential is the defect this adapter exists to avoid, so
    // say when we have just avoided it rather than only in a comment.
    if (r.registeredFlag && !r.isRegistered)
        detail.registeredFlagOutlivedTerm = true;
    if (r.forkRemoved)
        detail.retiredByPohV2 = true;
    if (r.forkRecognises !== undefined)
        detail.recognisedByPohV2 = r.forkRecognises;
    else
        detail.forkModuleUnreadable = true;
    const held = r.isRegistered && !r.forkRemoved;
    let issuedAt;
    if (held) {
        // A date before the registry took its first submission, or after the block we read, means
        // the struct is not saying what we think it is. Better no date than a fabricated one.
        if (r.submissionTime >= POH_V1_FIRST_SUBMISSION_AT && r.submissionTime <= r.now) {
            issuedAt = r.submissionTime;
            // `submissionTime` is rewritten by `executeRequest` on every accepted request, so a
            // renewal resets it. On this adapter's `Ramp` the reset understates survival and the
            // weight it produces is a floor — but a floor the subject is paying for, so say so.
            if (r.numberOfRequests > 1)
                notes.push('date-from-latest-reattestation');
        }
        else {
            detail.dateRejected = r.submissionTime;
        }
    }
    const provenance = {
        heldFrom: 'chain',
        dateFrom: issuedAt === undefined ? 'none' : 'chain',
        headBlock: r.block,
        notes,
    };
    if (held)
        detail.source = 'poh-v1-registry';
    return {
        held,
        ...(issuedAt === undefined ? {} : { issuedAt }),
        provenance,
        detail,
    };
}
/**
 * Proof of Humanity v1, read permissionlessly from Ethereum mainnet.
 *
 * Five values, one pinned block, one multicall round trip. Pinning matters for the same reason
 * it does in `world.ts` and `linea-poh.ts`: `submissionTime` from one block compared against a
 * `now` from another is a torn read in miniature, and here it decides whether somebody is
 * registered at all.
 *
 * The failure policy is asymmetric on purpose. Losing `isRegistered` or `getSubmissionInfo` is an
 * error, because a network failure must never read as "not a human". Losing `submissionDuration`
 * costs us the expiry we report and nothing else — the contract already applied the term inside
 * `isRegistered`. Losing the **ForkModule** read while v1 says registered is also an error: we
 * would otherwise have to choose between dropping a positive we can see and publishing one we
 * cannot confirm has not been retired, and an unreadable credential is a truer answer than
 * either.
 */
export function pohV1Adapter(rpcUrls = POH_V1_RPCS) {
    if (rpcUrls.length === 0)
        throw new Error('pohV1Adapter needs at least one RPC endpoint');
    const c = createPublicClient({
        chain: mainnet,
        transport: fallback(rpcUrls.map((url) => http(url, { timeout: 15_000, retryCount: 1 }))),
    });
    return {
        adapterId: 'poh-v1',
        probe: async (subject) => {
            try {
                const block = await c.getBlock();
                const [isRegistered, info, duration, forkRemoved, forkRecognises] = await c.multicall({
                    blockNumber: block.number,
                    allowFailure: true,
                    contracts: [
                        { address: POH_V1_REGISTRY, abi: POH_V1_ABI, functionName: 'isRegistered', args: [subject] },
                        { address: POH_V1_REGISTRY, abi: POH_V1_ABI, functionName: 'getSubmissionInfo', args: [subject] },
                        { address: POH_V1_REGISTRY, abi: POH_V1_ABI, functionName: 'submissionDuration' },
                        { address: POH_V1_FORK_MODULE, abi: POH_V1_FORK_MODULE_ABI, functionName: 'removed', args: [subject] },
                        { address: POH_V1_FORK_MODULE, abi: POH_V1_FORK_MODULE_ABI, functionName: 'isRegistered', args: [subject] },
                    ],
                });
                if (isRegistered.status === 'failure' || info.status === 'failure') {
                    const e = isRegistered.status === 'failure' ? isRegistered.error : info.error;
                    return {
                        held: false,
                        error: `Proof of Humanity v1 registry unreadable: ${e?.message ?? 'call failed'}`,
                    };
                }
                if (isRegistered.result && forkRemoved.status === 'failure') {
                    return {
                        held: false,
                        error: 'Proof of Humanity v1 registry says registered, but its v2 ForkModule is unreadable, ' +
                            `so we cannot tell whether the registration was retired: ${forkRemoved.error?.message ?? 'call failed'}`,
                    };
                }
                const [status, submissionTime, index, registeredFlag, , numberOfRequests] = info.result;
                return interpretPohV1Read({
                    block: Number(block.number),
                    now: Number(block.timestamp),
                    isRegistered: isRegistered.result,
                    registeredFlag,
                    submissionTime: Number(submissionTime),
                    index: Number(index),
                    numberOfRequests: Number(numberOfRequests),
                    status,
                    ...(duration.status === 'success' ? { submissionDuration: Number(duration.result) } : {}),
                    forkRemoved: forkRemoved.status === 'success' ? forkRemoved.result : false,
                    ...(forkRecognises.status === 'success' ? { forkRecognises: forkRecognises.result } : {}),
                });
            }
            catch (e) {
                return { held: false, error: e instanceof Error ? e.message : String(e) };
            }
        },
    };
}
