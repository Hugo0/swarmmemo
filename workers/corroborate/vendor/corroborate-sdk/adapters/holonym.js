import { createPublicClient, encodePacked, http, keccak256, parseAbi } from 'viem';
import { optimism } from 'viem/chains';
/**
 * Holonym / Human ID, read from `Hub` V3 on OP Mainnet.
 *
 * Two credentials live in this file because they are one read against one contract: the
 * government-ID check (`holonym-gov-id`, rooted at an unattributed KYC vendor — Onfido,
 * Sumsub, iDenfy and Veriff are all wired into their issuer and the credential does not say
 * which one signed it) and the face scan (`holonym-biometrics`, rooted at FaceTec). They are
 * separate adapters because they are separate roots, and a subject can hold either, both or
 * neither.
 *
 * ## Why this one is readable without an action-id, which is what unblocked it
 *
 * Holonym's uniqueness is scoped per `actionId`: a human registers once *per action*, and two
 * apps using different action-ids cannot link the same person. That is a good privacy design
 * and it is why this adapter sat behind "we must first publish a stable action-id" — the
 * vendor's REST API (`/sybil-resistance/gov-id/optimism?action-id=…`) makes you name one, and
 * choosing it is a product decision rather than a lookup.
 *
 * The Hub itself does not work that way. `getSBT(address, circuitId)` is keyed on the *holder
 * and the circuit*, and the action-id comes back inside the proof's public values — so we
 * read whichever action the credential was minted for instead of asserting one, and the
 * design decision disappears. Every SBT sampled on 2026-07-25 carried Holonym's own default
 * action-id, 123,456,789; a credential minted for some other action is still a credential the
 * issuer signed, so it counts, and `detail.actionId` says which namespace its uniqueness
 * belongs to.
 *
 * Reading the Hub also avoids the vendor's REST endpoint entirely, which matters for a second
 * reason: that endpoint consults a DynamoDB blocklist before it answers, so it can return
 * `false` for an address whose credential is perfectly valid on chain. Their API answers a
 * question about their policy. This answers a question about the chain.
 *
 * ## Presence is not enough, and the contract says so
 *
 * `Hub.sol` carries the warning in its own source: *"make sure you check the public values
 * such as actionId from this. Someone can forge a proof if you don't check the public values,
 * e.g., by using a different issuer or actionId"*. The circuit proves that *some* issuer
 * signed the credential — anyone can run their own issuer key — so an SBT whose
 * `publicValues[4]` is not Holonym's issuer address is a self-issued credential wearing the
 * right circuit id. This probe pins the issuer per credential and returns `held: false` with
 * `detail.issuerMismatch` when it does not match. Presence alone would have been forgeable.
 *
 * Layout of the five public values, from `V3SybilResistance.circom` and confirmed against the
 * validation Holonym's own API performs: `[expiry, recipient, actionId, actionNullifier,
 * issuerAddress]`.
 *
 * ## Dating a credential whose date is deliberately fuzzed
 *
 * The Hub stores no issuance timestamp — only an expiry the verifier sets — and the circuit is
 * explicit about why: *"A time the user can choose for their credential to expire. Max is one
 * year from iat. To keep anonymity, the user should choose a random time slightly before iat"*.
 * So the expiry is not the issuance date plus a constant. It is a value the holder picked to
 * blur exactly the question we are asking.
 *
 * What it does give us is a *proof*. `V3.circom` constrains `expiry - iat < 31,536,001` with a
 * 25-bit range check, so any SBT the Hub accepted carries a ZK guarantee that
 *
 *     iat >= expiry - 31,536,000        (one year, in seconds)
 *
 * — the credential was issued no earlier than that. On a `Decay` curve, where weight falls with
 * age, the earliest possible issuance is the *oldest* the credential can be and therefore the
 * *lowest* weight it can support. So the bound is used as the date: it can only understate
 * freshness, never inflate it, and every bit of that slack is a privacy purchase the holder made.
 * Measured against thirteen real mints on 2026-07-25, the bound sat 4–29 days before the mint for
 * eleven of them and 187 and 257 days before it for the other two. The worst case is bounded, and
 * bounded on the right side: a held credential is at most a year old, so its weight here can never
 * fall below 2^(-365/halfLife) and can never be inflated at all.
 *
 * Two consequences the ontology did not record. A Holonym credential **hard-expires within a
 * year of the underlying check** — `getSBT` reverts the moment `expiry < block.timestamp` — so
 * the 730-day half-life on `holonym-gov-id` only ever applies over the first year of its life,
 * the same shape as Human Passport's 90-day expiry against a 180-day half-life. And nothing in
 * this probe touches an archive node: the whole read is two `eth_call`s at head, which leaves
 * the three keyless OP archive endpoints to the Farcaster adapter, which genuinely needs them.
 *
 * ## What the extra call buys
 *
 * `setSBT` only burns a nullifier `if (nullifier != 0)`, and the nullifier it burns is a
 * *parameter*, not `publicValues[3]` — the contract never checks that the two agree. So an SBT
 * can exist without the human's uniqueness slot for that action having been consumed, which
 * would make it a liveness credential wearing a uniqueness credential's clothes. One
 * `nullifiersToIdentifiers(publicValues[3])` read confirms the circuit's own nullifier is the
 * one registered against this holder. Ten sampled SBTs across the registry's whole life all
 * passed; `detail.uniquenessNullifierRegistered` reports it per subject rather than assuming it.
 *
 * ## What is deliberately not read
 *
 * The legacy v2 store (`SybilResistance` at `0xdD748977…Fce31`) still answers
 * `isUniqueForAction(address, actionId)`, and Holonym's API consults it before the Hub. We do
 * not: it returned `false` for every V3 holder sampled, it exposes no expiry and no issuance
 * evidence of any kind, and an undatable credential on a `Decay` curve scores at full weight —
 * the inflation direction. Adding a read we cannot date and could not find a positive for would
 * be a guess with a contract call in front of it. `research/protocols/holonym-human-id-onchain-read.md`
 * records the addresses so a later iteration can revisit it with a log index.
 */
/** OP Mainnet. `name()` == "Holonym V3", asserted in the live test. */
export const HOLONYM_HUB_V3 = '0x2AA822e264F8cc31A2b9C22f39e5551241e94DfB';
/**
 * Legacy v2 uniqueness store, recorded and not read — see the note above. Holonym's own API
 * checks this before the Hub, so an address could in principle be v2-only.
 */
export const HOLONYM_SYBIL_RESISTANCE_V2 = '0xdD748977BAb5782625AF1466F4C5F02Eb92Fce31';
/**
 * The circuit's ceiling on `expiry - iat`, from `V3.circom`:
 *
 *     component lt = LessThan(25);
 *     lt.in[0] <== expiry - iat;
 *     lt.in[1] <== 31536001;      // 1 year + 1 second
 *     lt.out === 1;
 *
 * A constraint, not a convention: an SBT that violated it could not have been minted, so
 * `expiry - this` is a proven lower bound on issuance for every credential the Hub holds.
 */
export const HOLONYM_MAX_CREDENTIAL_TERM_SECONDS = 31_536_000;
/** Holonym's own default, and the only action-id observed on chain. Reported, never required. */
export const HOLONYM_DEFAULT_ACTION_ID = 123456789n;
/**
 * Circuit ids and issuer keys, transcribed from `holonym-foundation/id-server`
 * `src/constants/misc.ts` and `holonym-foundation/holonym-api` `src/constants/misc.js`
 * (both retrieved 2026-07-25), and re-read off live SBTs by the live test.
 */
export const HOLONYM_CREDENTIALS = {
    'holonym-gov-id': {
        circuitId: '0x729d660e1c02e4e419745e617d643f897a538673ccf1051e093bbfa58b0a120b',
        issuer: 0x03fae82f38bf01d9799d57fdda64fad4ac44e4c2c2f16c5bf8e1873d0a3e1993n,
        label: 'gov-id',
    },
    'holonym-biometrics': {
        circuitId: '0x0b5121226395e3b6c76eb8ddfb0bf2f2075e7f2c6956567e84b38a223c3a3d15',
        issuer: 0x0d4f849df782fb9e68d525fbda10b73e59180e59cb2a21ce5d70ccc45dbfd922n,
        label: 'biometrics',
    },
};
/**
 * Circuits the Hub serves that this ontology does not price, kept here so the write-up and the
 * probe cannot drift apart. `phone` is farmable and worth ~nothing; `clean-hands` is a
 * sanctions screen rather than personhood; both passport circuits are the ICAO chip, which is
 * `state-document:icao-9303` and would need its own ontology entry rather than being folded
 * into a KYC-vendor root.
 */
export const HOLONYM_UNSCORED_CIRCUITS = {
    phone: '0xbce052cf723dca06a21bd3cf838bc518931730fb3db7859fc9cc86f0d5483495',
    cleanHands: '0x1c98fc4f7f1ad3805aefa81ad25fa466f8342292accf69566b43691d12742a19',
    zkPassport: '0x14c3513390f8a03993c848621b1840d58c27fd50bbddba73265e22d17b0b747e',
    ePassport: '0xf2ce248b529343e105f7b3c16459da619281c5f81cf716d28f7df9f87667364d',
};
/**
 * `getSBT` reverts for expired, revoked and never-existed alike, so the automatic getter for
 * the `sbtOwners` mapping is what tells the three apart: it returns the struct's non-array
 * members without any of the checks, and `expiry == 0` is the only value a holder can never
 * have.
 */
export const HUB_ABI = parseAbi([
    'function sbtOwners(bytes32 identifier) view returns (uint256 expiry, bool revoked)',
    'function getSBT(address holder, bytes32 circuitId) view returns ((uint256 expiry, uint256[] publicValues, bool revoked) sbt)',
    'function getIdentifier(address user, bytes32 circuitId) pure returns (bytes32)',
    'function nullifiersToIdentifiers(uint256 nullifier) view returns (bytes32)',
    'function name() view returns (string)',
]);
/**
 * `keccak256(abi.encodePacked(user, circuitId))`, the Hub's own key derivation, computed here
 * rather than fetched. `getIdentifier` is `pure`, so this saves a round trip per credential and
 * the live test holds the local result against the contract's.
 */
export function holonymIdentifier(subject, circuitId) {
    return keccak256(encodePacked(['address', 'bytes32'], [subject, circuitId]));
}
/**
 * The whole decision, as a pure function of what the Hub returned.
 *
 * Kept separate from the I/O so every branch — absent, expired, revoked, forged issuer, an
 * expiry the proof does not agree with — is unit-testable without a network, the same reason
 * `reconcile.ts` exists.
 */
export function interpretSbt(credential, record, now) {
    const base = { credential: credential.label };
    if (!record || record.expiry === 0)
        return { held: false, detail: { ...base, sbt: 'none' } };
    if (record.revoked) {
        return { held: false, detail: { ...base, sbt: 'revoked', expiresAt: record.expiry } };
    }
    if (record.expiry < now) {
        return { held: false, detail: { ...base, sbt: 'expired', expiredAt: record.expiry } };
    }
    const pv = record.publicValues;
    if (!pv || pv.length < 5) {
        return { held: false, detail: { ...base, sbt: 'public-values-unreadable' } };
    }
    if (pv[4] !== credential.issuer) {
        // Anyone can run an issuer key and prove a credential they signed themselves. The circuit
        // id alone does not say who vouched for the document, so an SBT under a different issuer
        // is evidence of nothing at all.
        return {
            held: false,
            detail: {
                ...base,
                sbt: 'issuer-mismatch',
                issuerInProof: `0x${pv[4].toString(16).padStart(64, '0')}`,
                expectedIssuer: `0x${credential.issuer.toString(16).padStart(64, '0')}`,
            },
        };
    }
    // The Hub's stored expiry is a `setSBT` argument and the proof's is a public value; nothing
    // on chain forces them to agree. Take the earlier of the two, which is the older issuance
    // and so the lower weight, and say when they differed.
    const proofExpiry = Number(pv[0]);
    const expiry = Math.min(record.expiry, proofExpiry);
    const issuedAt = expiry - HOLONYM_MAX_CREDENTIAL_TERM_SECONDS;
    const actionId = pv[2];
    return {
        held: true,
        // The circuit's ceiling makes a future date impossible; the guard is here so that if the
        // ceiling ever changed we would lose the date rather than invent a fresh credential.
        ...(issuedAt <= now ? { issuedAt } : {}),
        detail: {
            ...base,
            expiresAt: expiry,
            issuedNoEarlierThan: issuedAt,
            maxCredentialTermSeconds: HOLONYM_MAX_CREDENTIAL_TERM_SECONDS,
            actionId: actionId.toString(),
            actionIdIsHolonymDefault: actionId === HOLONYM_DEFAULT_ACTION_ID,
            ...(proofExpiry !== record.expiry
                ? { proofExpiry, recordExpiry: record.expiry, expiryDisagreesWithProof: true }
                : {}),
        },
    };
}
/**
 * `optimism-rpc.publicnode.com` and not `mainnet.optimism.io`: this probe only ever reads at
 * head, and `mainnet.optimism.io` is one of the three keyless endpoints that serve *archive*
 * state, which the Farcaster adapter needs and this one does not.
 */
export const HOLONYM_RPC = 'https://optimism-rpc.publicnode.com';
/**
 * Both Holonym probes, sharing one client and one in-flight head-block read.
 *
 * They are returned together because `defaultAdapters()` runs them concurrently against the
 * same subject: sharing the head-block promise means the pair costs one `eth_blockNumber`
 * rather than two, and the promise is dropped as soon as it settles so a later lookup can
 * never be answered from a stale block.
 */
export function holonymAdapters(opts = {}) {
    const client = createPublicClient({
        chain: optimism,
        transport: http(opts.rpcUrl ?? HOLONYM_RPC, { timeout: opts.timeoutMs ?? 12_000 }),
    });
    let headInFlight;
    const head = () => {
        if (!headInFlight) {
            headInFlight = client.getBlockNumber();
            headInFlight.finally(() => {
                headInFlight = undefined;
            });
        }
        return headInFlight;
    };
    /**
     * The mapping first, `getSBT` only when the mapping says the SBT is live.
     *
     * That order is not an optimisation. `getSBT` reverts for expired, revoked and absent alike,
     * and calling it blind would turn "this person's KYC check lapsed in January" into a probe
     * error — an unreadable credential rather than an expired one. Most addresses hold nothing,
     * so it is also one call instead of two for almost every subject.
     */
    const readRecord = async (subject, credential, now) => {
        const identifier = holonymIdentifier(subject, credential.circuitId);
        const [expiry, revoked] = await client.readContract({
            address: HOLONYM_HUB_V3,
            abi: HUB_ABI,
            functionName: 'sbtOwners',
            args: [identifier],
        });
        if (expiry === 0n || revoked || Number(expiry) < now)
            return { expiry: Number(expiry), revoked };
        const sbt = await client.readContract({
            address: HOLONYM_HUB_V3,
            abi: HUB_ABI,
            functionName: 'getSBT',
            args: [subject, credential.circuitId],
        });
        return { expiry: Number(expiry), revoked, publicValues: sbt.publicValues };
    };
    const probeOne = async (credential, subject) => {
        try {
            const now = Math.floor(Date.now() / 1000);
            const [headBlock, record] = await Promise.all([head(), readRecord(subject, credential, now)]);
            const verdict = interpretSbt(credential, record, now);
            const notes = [];
            const detail = { ...verdict.detail };
            if (verdict.held) {
                if (verdict.issuedAt !== undefined)
                    notes.push('date-from-expiry-and-max-term');
                // Only meaningful for a credential we are actually counting, and it costs a call.
                detail.uniquenessNullifierRegistered = await nullifierIsRegistered(client, subject, credential, record.publicValues[3]);
            }
            const provenance = {
                heldFrom: 'chain',
                dateFrom: verdict.issuedAt !== undefined ? 'chain' : 'none',
                headBlock: Number(headBlock),
                notes,
            };
            return {
                held: verdict.held,
                ...(verdict.issuedAt !== undefined ? { issuedAt: verdict.issuedAt } : {}),
                provenance,
                detail,
            };
        }
        catch (e) {
            // Never a `false`: an OP Mainnet outage is not evidence that this person never did KYC.
            return { held: false, error: e instanceof Error ? e.message : String(e) };
        }
    };
    return Object.entries(HOLONYM_CREDENTIALS).map(([adapterId, credential]) => ({
        adapterId,
        probe: (subject) => probeOne(credential, subject),
    }));
}
/**
 * Whether the proof's own action-nullifier is the one the Hub burned for this holder.
 *
 * `setSBT` writes `nullifiersToIdentifiers[nullifier] = identifier` from the *argument* it was
 * handed, which nothing constrains to equal `publicValues[3]`. If the two disagree, the
 * human's uniqueness slot for this action was never consumed and the same person could hold
 * the credential on any number of addresses.
 *
 * A failed read answers `undefined` rather than `false`: "we could not check" and "the check
 * failed" are different claims, and only one of them is about the subject.
 */
async function nullifierIsRegistered(client, subject, credential, nullifier) {
    if (nullifier === 0n)
        return false;
    try {
        const mapped = await client.readContract({
            address: HOLONYM_HUB_V3,
            abi: HUB_ABI,
            functionName: 'nullifiersToIdentifiers',
            args: [nullifier],
        });
        return mapped === holonymIdentifier(subject, credential.circuitId);
    }
    catch {
        return undefined;
    }
}
