import { createPublicClient, fallback, http, parseAbi } from 'viem';
import { worldchain } from 'viem/chains';
import { AGENT_BOOK_DEPLOYED_AT, registrationOf } from "../agentbook.js";
/**
 * World ID (Orb tier), read from World Chain.
 *
 * ## What was wrong with reading only AgentBook
 *
 * The previous version of this adapter asked `AgentBook.lookupHuman(subject) != 0`. That read is
 * correct and permissionless, and it has two problems. It covers only the wallets that registered
 * an agent through AgentKit — **1,068 transactions in the contract's life**, measured 2026-07-25 —
 * and it carries **no date**. An undated credential on a `Decay` curve scores at freshness 1
 * (`freshnessOf`, `scoring.ts`), so every World credential we found was priced as if it had been
 * issued this morning. That is the direction that pays an adversary.
 *
 * The fix is a different contract on the same chain.
 *
 * ## WorldIDAddressBook: the registry World actually populates
 *
 * `WorldIDAddressBook` (`0x57b930D5…E0330D`, Blockscout tags it "World Chain: World ID Address
 * Book" from worldcoin.org, deployed 2024-08-27 at block 2,711,105) stores one mapping:
 *
 *     mapping(address => uint256) addressVerifiedUntil
 *
 * and `verify()` writes `addressVerifiedUntil[account] = block.timestamp + verificationLength`
 * after `worldIdRouter.verifyProof(...)` accepts a World ID group-1 proof. So a single `eth_call`
 * gives us both halves of the answer:
 *
 * - **held**: `addressVerifiedUntil[subject] > block.timestamp`. Not `!= 0` — the value is never
 *   cleared, so a lapsed verification is a nonzero number in the past, and treating presence as
 *   evidence would count people whose binding died months ago. Roughly half of a sampled 2025-04
 *   cohort is in exactly that state.
 * - **issuedAt**: `verifiedUntil - verificationLength()`, which is *exactly* the timestamp of the
 *   block the verification was mined in. Confirmed on 24 sampled verifications spanning
 *   2025-04-18 to 2026-07-25, every one to the second, and asserted every run by the live suite
 *   against a fresh sample. `verificationLength` is 14,515,200 s (168 days) and has been since the
 *   contract's `WorldIDAddressBookInitialized` event; it is read at runtime rather than pinned, so
 *   a governance change is visible instead of silently re-dating everybody.
 *
 * Coverage is not comparable to AgentBook's: sampled 100-block windows give ~28,000 verifications
 * a day at head and 60,000–80,000 a day through 2025, against a 168-day term.
 *
 * ## What the date means, and why it is honest to use it
 *
 * `verifiedUntil - verificationLength` is when this address last re-proved a World ID — not when
 * the human enrolled at an Orb. The enrolment date (`genesis_issued_at`) lives inside the v4
 * credential and is never published on chain. So this adapter dates the *binding*, which is the
 * thing the contract actually attests, and says so through the `date-from-latest-reattestation`
 * note. On a decay curve that is the conservative reading of what the chain knows: the human's
 * iris capture is older than the date we use, and the binding it proves is exactly this fresh.
 *
 * ## The AgentBook half, dated from its own log
 *
 * `AgentBook.lookupHuman` is the other way this credential is held, and until now it was the last
 * undated credential in the roster — the same defect in the other registry. It stores a mapping
 * and nothing else: no term, no expiry, no timestamp. But the write emits `AgentRegistered`, and
 * that log has a block, so the date exists on chain and was simply not being read. It is fetched
 * (`registrationOf`, `agentbook.ts`) only for wallets the mapping says are registered — a filtered
 * query costing two calls, not the six-call fleet scan — and it is refused unless the log's
 * `humanId` is the one state just returned, so a truncated or stale log cannot date a binding it
 * has not actually seen.
 *
 * What that date means is narrower than an enrolment and wider than nothing: at that block the
 * operator proved a live group-1 Orb credential to this contract, because `register` verifies the
 * proof before it writes. Registration never expires and cannot be undone, so it is the only
 * freshness signal the contract has, and the note `date-from-agent-registration` says exactly that.
 *
 * When both registries answer, the date used is **the later of the two** — each is a moment the
 * chain accepted an Orb proof for this address, and the most recent one is the freshest thing
 * known. Neither can be manufactured without a real proof, so there is no cheap way to move it.
 * Both dates are reported in `detail` so a consumer can check the choice rather than trust it.
 *
 * ## One live verification per human, enforced on chain
 *
 * `verify()` reverts `VerificationAlreadyActive()` when the proof's nullifier is already mapped to
 * a *different* address whose verification has not expired. So at any moment a World human has at
 * most one live AddressBook entry — a per-human uniqueness property that survives the address
 * being changed, and one that our fleet policy can rely on rather than infer.
 *
 * ## Why an entry means a real Orb proof, and not a signature from World
 *
 * Simulating `AddressBook.verify(...)` with a junk proof reverts identically from a stranger, from
 * the relayer that submits real verifications, and from the contract's own owner — so the gate is
 * the proof, not the caller. Which check fails is itself informative: with a made-up merkle root
 * the revert is `NonExistentRoot()` (`0xddae3b71`, the root-history check), and with the group's
 * current root it advances to `ProofInvalid()` (`0x7fcdd1f4`, the Groth16 pairing check). Both are
 * asserted live, from three senders, every run.
 *
 * The router the AddressBook trusts was changed once, on 2026-01-08, from the canonical
 * `WorldIDRouter` to an unverified shim (`0xB012Bc9D…65Caa`) that reads the AddressBook and then
 * calls that same canonical router — the call chain AddressBook → shim → WorldIDRouter →
 * WorldIDRouterImplV1 → group-1 identity manager → Groth16 verifier → bn256 pairing precompiles is
 * visible in the trace of any real verification. `groupId` is 1 and has never changed, and group 1
 * is Orb-only: World ID's device tier was never verifiable on chain.
 *
 * ## The document and Selfie tiers are not readable here, on purpose
 *
 * World ID 4.0's NFC-document (schema 9303) and Selfie Check (schema 11) credentials leave no
 * per-holder trace on any chain: `WorldIDVerifier` is a `view` function taking a proof (its proxy
 * has received **2 transactions in its life**), and `CredentialSchemaIssuerRegistry` is keyed by
 * issuer schema id and stores issuer pubkeys, not holders. Both address-keyed registries on World
 * Chain — this one and AgentBook — consume v3 group-1 proofs, which is the Orb. Reading the other
 * two tiers requires World's Developer Portal with a registered `rp_id`, which is the vendor
 * dependency this package exists without. They stay in the ontology, unimplemented and documented,
 * rather than being scored from a source that can revoke us. See
 * `research/protocols/world-id-onchain-read.md`.
 */
/** Keyless World Chain endpoint. Serves archive `eth_call` and 100-block `eth_getLogs` windows. */
export const WORLD_RPC = 'https://worldchain-mainnet.g.alchemy.com/public';
/** `WorldIDAddressBook` on World Chain. Verified source; Blockscout labels it for worldcoin.org. */
export const WORLD_ID_ADDRESS_BOOK = '0x57b930D551e677CC36e2fA036Ae2fe8FdaE0330D';
/** `AgentBook` on World Chain — AgentKit's human↔agent registry. Same credential tier. */
export const WORLD_AGENT_BOOK = '0xA23aB2712eA7BBa896930544C7d6636a96b944dA';
/** Canonical `WorldIDRouter` on World Chain. Group 1 is the Orb group. */
export const WORLD_ID_ROUTER = '0x17B354dD2595411ff79041f930e491A4Df39A278';
/** The only World ID group that was ever verifiable on chain. */
export const WORLD_ID_ORB_GROUP_ID = 1n;
/** Block and timestamp of the AddressBook's deployment — the floor for any derived date. */
export const WORLD_ADDRESS_BOOK_DEPLOYED_AT_BLOCK = 2_711_105;
export const WORLD_ADDRESS_BOOK_DEPLOYED_AT = 1_724_757_849;
/**
 * The term set at initialisation, 168 days. Never used to date anything — the probe reads the
 * live value — but the live suite holds the chain to it, because every date we derive is only
 * exact while it is unchanged.
 */
export const WORLD_ADDRESS_BOOK_VERIFICATION_LENGTH = 14_515_200;
export const WORLD_ADDRESS_BOOK_ABI = parseAbi([
    'function addressVerifiedUntil(address) view returns (uint256)',
    'function verificationLength() view returns (uint256)',
    'function maxProofTime() view returns (uint256)',
    'function groupId() view returns (uint256)',
    'function worldIdRouter() view returns (address)',
    'function nullifierHashes(uint256) view returns (address)',
    'function verify(address account, uint256 root, uint256 nullifierHash, uint256[8] proof, uint256 proofTime) payable',
    'event AccountVerified(address indexed account, uint256 verifiedUntil)',
    'event WorldIDAddressBookInitialized(address worldIdRouter, uint256 groupId, uint256 externalNullifierHash, uint256 verificationLength, uint256 maxProofTime)',
]);
export const WORLD_AGENT_BOOK_ABI = parseAbi([
    'function lookupHuman(address account) view returns (uint256)',
    'function groupId() view returns (uint256)',
    'function worldIdRouter() view returns (address)',
]);
/**
 * Turn one pinned read of World Chain into a probe result.
 *
 * Pure, so every branch — lapsed, live, agent-only, both, neither, and the two ways a derived
 * date can be impossible — is testable without a network.
 */
export function interpretWorldRead(r) {
    const detail = { tier: 'orb' };
    const notes = [];
    const agentHumanId = r.agentBookHumanId;
    const agentHeld = agentHumanId !== undefined && agentHumanId !== '0';
    if (agentHumanId === undefined)
        detail.agentBookUnreadable = true;
    else if (agentHeld)
        detail.agentBookHumanId = agentHumanId;
    const addressBookHeld = r.verifiedUntil > r.now;
    const sources = [];
    if (addressBookHeld)
        sources.push('world-id-address-book');
    if (agentHeld)
        sources.push('world-agentbook');
    if (r.verifiedUntil > 0) {
        detail.verifiedUntil = r.verifiedUntil;
        detail.verificationLengthSeconds = r.verificationLength;
        if (addressBookHeld) {
            detail.expiresInDays = Math.round(((r.verifiedUntil - r.now) / 86_400) * 10) / 10;
            // The contract refuses a second live verification against the same World ID nullifier, so
            // this address is the only live AddressBook binding its human has.
            detail.oneLiveAddressPerHuman = true;
        }
        else {
            detail.addressBookLapsedAt = r.verifiedUntil;
        }
    }
    let addressBookIssuedAt;
    if (addressBookHeld && r.verificationLength > 0) {
        const derived = r.verifiedUntil - r.verificationLength;
        // A date before the registry existed, or in the future, means `verificationLength` is not the
        // term this entry was written under. Better no date than a fabricated one — and on a decay
        // curve an over-fresh date is the expensive direction to be wrong in.
        if (derived >= WORLD_ADDRESS_BOOK_DEPLOYED_AT && derived <= r.now) {
            addressBookIssuedAt = derived;
            detail.addressBookIssuedAt = derived;
        }
        else {
            detail.dateRejected = derived;
        }
    }
    const agentBookIssuedAt = agentHeld ? dateFromRegistration(agentHumanId, r, detail) : undefined;
    // Each date is a moment the chain accepted an Orb proof for this address, so the later one is
    // the freshest evidence there is. Both notes fire when the two land on the same second, which
    // happens: an operator typically registers an agent minutes after verifying the wallet.
    const dates = [addressBookIssuedAt, agentBookIssuedAt].filter((d) => d !== undefined);
    const issuedAt = dates.length ? Math.max(...dates) : undefined;
    if (issuedAt !== undefined) {
        if (addressBookIssuedAt === issuedAt)
            notes.push('date-from-latest-reattestation');
        if (agentBookIssuedAt === issuedAt)
            notes.push('date-from-agent-registration');
    }
    if (sources.length)
        detail.source = sources.join('+');
    const provenance = {
        heldFrom: 'chain',
        dateFrom: issuedAt === undefined ? 'none' : 'chain',
        headBlock: r.block,
        notes,
    };
    return {
        held: sources.length > 0,
        ...(issuedAt === undefined ? {} : { issuedAt }),
        provenance,
        detail,
    };
}
/**
 * The registration date, or nothing — with the reason recorded either way.
 *
 * Four ways this returns undefined, and every one of them would otherwise be a silent scoring
 * change, so each lands in `detail`: the log endpoint failed, the log has no registration for a
 * wallet the mapping says is registered, the log names a different human than state does, or the
 * timestamp falls outside the contract's own lifetime. In each case the credential stays undated,
 * which on a `Decay` curve means full weight — so the caller is told rather than left to assume.
 */
function dateFromRegistration(stateHumanId, r, detail) {
    const reg = r.agentBookRegistration;
    if (reg === undefined || reg.status === 'unavailable') {
        detail.agentBookDateUnavailable = true;
        return undefined;
    }
    if (reg.status === 'not-found') {
        // State says this wallet is registered and the log does not. A registration cannot be
        // withdrawn, so this is our read being incomplete, not the registry changing its mind.
        detail.agentBookRegistrationNotInLog = true;
        return undefined;
    }
    if (reg.block !== undefined)
        detail.agentBookRegisteredAtBlock = reg.block;
    if (reg.humanId !== undefined && reg.humanId !== stateHumanId) {
        // The log found belongs to a different binding than the one state holds — a re-registration
        // seen only half, or a truncated answer. Dating from it would date the wrong event.
        detail.agentBookDateRejected = 'registration-log-disagrees-with-state';
        return undefined;
    }
    if (reg.timestamp === undefined) {
        detail.agentBookDateUnavailable = true;
        return undefined;
    }
    if (reg.timestamp < AGENT_BOOK_DEPLOYED_AT || reg.timestamp > r.now) {
        detail.agentBookDateRejected = reg.timestamp;
        return undefined;
    }
    detail.agentBookRegisteredAt = reg.timestamp;
    return reg.timestamp;
}
// Same fallback posture as agentbook.ts: an explicitly chosen endpoint is used alone,
// the default rides the verified public list. A network failure on the AddressBook read
// must never read as "not a human", and two more endpoints make that failure rarer.
const client = (rpcUrl) => createPublicClient({
    chain: worldchain,
    transport: rpcUrl === WORLD_RPC
        ? fallback([http(WORLD_RPC), http('https://worldchain.drpc.org'), http('https://480.rpc.thirdweb.com')])
        : http(rpcUrl),
});
/** `registrationOf` narrowed to what the interpreter needs, and unable to throw. */
async function registrationRead(subject, logOptions) {
    try {
        const lookup = await registrationOf(subject, logOptions);
        return lookup.status === 'found'
            ? {
                status: 'found',
                block: lookup.registration.block,
                humanId: lookup.registration.humanId,
                ...(lookup.registration.timestamp === undefined
                    ? {}
                    : { timestamp: lookup.registration.timestamp }),
            }
            : { status: lookup.status };
    }
    catch {
        return { status: 'unavailable' };
    }
}
/**
 * World ID (Orb tier), read permissionlessly from World Chain.
 *
 * Four values, one pinned block, one multicall round trip: the AddressBook's mapping and term,
 * the AgentBook's binding, and the block timestamp the contract's own expiry check uses. Pinning
 * matters for the same reason it does in `linea-poh.ts` — a `verifiedUntil` from one block
 * compared against a `now` from another is a torn read in miniature, and here it would decide
 * whether somebody is verified at all.
 *
 * A fifth value, the AgentBook registration's date, costs a second round trip and is therefore
 * fetched only when the mapping says there is a registration to date — a thousand wallets on
 * World Chain, not every subject. Its failure is never the probe's failure: an undated World
 * credential is still a held World credential.
 *
 * The AgentBook read is allowed to fail on its own: it is a second, narrower source, and losing
 * it must not turn an AddressBook positive into an error. Losing the AddressBook read *is* an
 * error, because a network failure must never read as "not a human".
 */
export function worldIdOrbAdapter(rpcUrl = WORLD_RPC, logOptions = {}) {
    const c = client(rpcUrl);
    return {
        adapterId: 'world-id-orb',
        probe: async (subject) => {
            try {
                const block = await c.getBlock();
                const [verifiedUntil, verificationLength, agentHumanId] = await c.multicall({
                    blockNumber: block.number,
                    allowFailure: true,
                    contracts: [
                        {
                            address: WORLD_ID_ADDRESS_BOOK,
                            abi: WORLD_ADDRESS_BOOK_ABI,
                            functionName: 'addressVerifiedUntil',
                            args: [subject],
                        },
                        {
                            address: WORLD_ID_ADDRESS_BOOK,
                            abi: WORLD_ADDRESS_BOOK_ABI,
                            functionName: 'verificationLength',
                        },
                        {
                            address: WORLD_AGENT_BOOK,
                            abi: WORLD_AGENT_BOOK_ABI,
                            functionName: 'lookupHuman',
                            args: [subject],
                        },
                    ],
                });
                if (verifiedUntil.status === 'failure' || verificationLength.status === 'failure') {
                    const e = verifiedUntil.status === 'failure' ? verifiedUntil.error : verificationLength.error;
                    return {
                        held: false,
                        error: `World ID AddressBook unreadable: ${e?.message ?? 'call failed'}`,
                    };
                }
                const humanId = agentHumanId.status === 'success' ? agentHumanId.result.toString() : undefined;
                const registration = humanId !== undefined && humanId !== '0'
                    ? await registrationRead(subject, logOptions)
                    : undefined;
                return interpretWorldRead({
                    block: Number(block.number),
                    now: Number(block.timestamp),
                    verifiedUntil: Number(verifiedUntil.result),
                    verificationLength: Number(verificationLength.result),
                    ...(humanId === undefined ? {} : { agentBookHumanId: humanId }),
                    ...(registration === undefined ? {} : { agentBookRegistration: registration }),
                });
            }
            catch (e) {
                return { held: false, error: e instanceof Error ? e.message : String(e) };
            }
        },
    };
}
