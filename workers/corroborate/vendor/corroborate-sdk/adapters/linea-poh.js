import { createPublicClient, http, parseAbi, toHex } from 'viem';
import { linea } from 'viem/chains';
/**
 * Linea Proof of Humanity V2, read from the Verax attestation registry on Linea.
 *
 * ## Why this one looked unreadable, and why it is not
 *
 * Verax stores an attestation's `subject` as `bytes`, and the registry keeps attestations in a
 * mapping keyed by a sequential id. There is no subject index unless the issuing portal
 * registers Verax's `IndexerModule`, and the Sumsub portal registers **no modules at all**
 * (`getModules()` returns an empty array — asserted live). So the obvious read does not exist,
 * which is why Linea ships a signature-based path instead: `PohVerifier.verify(signature,
 * address)` consumes a blob you fetch from `poh-signer-api.linea.build`. Our own research
 * concluded from this that "there is no efficient on-chain *does address X have a PoH
 * attestation* read".
 *
 * That conclusion assumed we have to search. We do not, because **the credential expires**.
 *
 * A PoH V2 attestation carries a term of exactly 90 days (measured across the whole schema; see
 * `LINEA_POH_MAX_TERM_SECONDS`). `attestedDate` is the block timestamp at write, and ids are
 * handed out in order, so `attestedDate` is monotone in id. Therefore every *unexpired*
 * attestation in the entire registry — not just this schema's — lies in the contiguous id range
 *
 *     [ first id with attestedDate >= now - 90 days , attestationIdCounter )
 *
 * and on this registry that range is **765 ids wide** against a counter of 6,366,748. Reading it
 * whole costs six `eth_call` round trips through Multicall3 and yields the complete live
 * population: 500 unexpired attestations over 499 distinct addresses, measured 2026-07-25.
 * A probe is then a map lookup.
 *
 * So this adapter does not answer "is this address verified" by asking anyone. It enumerates
 * every live credential the protocol has issued, from the chain, and checks whether the subject
 * is in it. No indexer, no vendor endpoint, no API key — and as a side effect it knows the size
 * of the population it is answering about, which the vendor's boolean cannot tell you.
 *
 * ## The range is found by galloping, not bisecting, because we are going to read it anyway
 *
 * The classic move is to bisect for the boundary id and then scan forward from it. Here the
 * bisection is wasted work: a doubling ladder (`counter-1, counter-2, counter-4, …`) finds a
 * bracket at most twice the true window in **one** batched round trip, and scanning a bracket
 * twice as wide as necessary is cheaper than twenty sequential round trips to make it exact.
 * The scan *is* the boundary search. `LINEA_POH_LADDER_STEPS` bounds the ladder.
 *
 * Everything is read at one pinned block, so the counter cannot advance underneath the scan and
 * produce a view that is half of one moment and half of another — the torn read `reconcile.ts`
 * exists to prevent, in miniature.
 *
 * ## Who is allowed to say you are human
 *
 * Presence of an attestation under the right schema is *not* evidence, and this is the same
 * lesson the Holonym adapter learned from the Hub's own source comments. Verax's
 * `AttestationRegistry` checks one thing: that the caller is a portal registered in
 * `PortalRegistry`. Nothing stops a *different* portal writing under the Sumsub Proof of
 * Personhood schema — schema ownership does not restrict portals.
 *
 * So the probe pins the portal's registered **owner address**, and three facts we measured on
 * 2026-07-25 explain why it is that and not any of the other candidates:
 *
 * - **Not the portal address.** Sumsub has *three* registered portals under this owner. Our own
 *   research file named `0xe8a3a57e…b73922` as "the" PoH portal; that one has issued **four**
 *   attestations, all on 2025-07-02/03, all long expired. The 50,471 real ones came from
 *   `0x501e742C…7D5B46`. An adapter pinned to the researched address would have returned
 *   `held: false` for the entire population while looking like it worked.
 * - **Not `ownerName`.** It is a string the portal's creator supplies at registration. It says
 *   "Sumsub" on all three portals and it would say "Sumsub" on anyone else's too.
 * - **Not the `attester` field.** That is `msg.sender` on the portal's `attest` call. The portal
 *   gates on an ECDSA signature instead, which we established by simulating `attest` from a
 *   stranger *and* from Sumsub's own attester key and getting the identical revert
 *   (`ECDSAInvalidSignature`, `0xf645eedf`) — the gate is on the signature, not the caller.
 *
 * The owner address is the anchor Verax itself enforces: `PortalRegistry.isIssuer(owner)` is an
 * allowlist Consensys controls, `deployDefaultPortal` from a non-issuer reverts, and only a
 * registered portal can write to the registry at all. Both are re-read at runtime rather than
 * assumed, so a de-listing is visible immediately.
 *
 * The portal's authorised signer is then read *from the portal* (`signerAddress()`, which
 * returns Sumsub's attester key) rather than hard-coded, for the same reason the Human Passport
 * adapter asks each Decoder which resolver it trusts. It is reported as corroboration and not
 * used as a filter: it is a key Sumsub may rotate, and a rotation must not retroactively
 * un-verify people.
 *
 * ## What the ontology did not record
 *
 * The 90-day term means the `decayHalfLifeDays: 90` on this entry only ever applies over the
 * credential's single 90-day life — weight can never fall below one half — which is the third
 * instance of the same shape in this codebase (Human Passport hard-expires at 90 days against a
 * 180-day half-life; a Holonym credential expires within a year against 730). And the gap
 * between cumulative and live is enormous here: 50,475 attestations ever issued, 500 alive. A
 * scorer reading the cumulative count would price a population 100× larger than the one that
 * exists.
 */
/** Verax `AttestationRegistry` on Linea. A proxy; `router()` and the counter are asserted live. */
export const VERAX_ATTESTATION_REGISTRY = '0x3de3893aa4Cdea029e84e75223a152FD08315138';
/** Verax `PortalRegistry` on Linea — the permissioned allowlist that makes the owner check mean something. */
export const VERAX_PORTAL_REGISTRY = '0xd5d61e4ECDf6d46A63BfdC262af92544DFc19083';
/**
 * Schema "Sumsub Proof of Personhood", read back from `SchemaRegistry.getSchema` by the live
 * test: context `https://id.sumsub.com/linea-liveness`, schema string `(string levelInfo)`,
 * described on chain as "Simple proof of personhood and uniqueness based on Sumsub liveness,
 * deepfake detection and duplicate search".
 *
 * Not to be confused with `0x0094bda6…c0d0af` ("Sumsub Proof of Humanity", 11 attestations from
 * a third Sumsub portal) — a variant that never carried a population.
 */
export const LINEA_POH_V2_SCHEMA = '0x39d02301e928bea8be757163a804167b7f7eaa5ac01c39bc3d2e6da5a65cd23f';
/**
 * The address registered as owner of all three Sumsub portals, and the only thing in this file
 * that a forger cannot choose for themselves. `PortalRegistry.isIssuer` of it is true and is
 * re-checked at runtime.
 */
export const SUMSUB_PORTAL_OWNER = '0x887F94C1283697c607b321860bd95263AC0E2467';
/**
 * The portal that issued 50,471 of the schema's 50,475 attestations. Recorded so the write-up
 * and the code cannot drift, and **not** used as a filter — see the header on why the owner is
 * the anchor and this is not.
 */
export const SUMSUB_POH_PORTAL = '0x501e742CF30eCE300E3e8CB45a975c15057D5B46';
/**
 * The portal our research named, kept here as the tripwire it turned out to be: four
 * attestations, two of them for the same subject, all on 2025-07-02/03, all expired.
 */
export const SUMSUB_POH_PORTAL_DEPLOYMENT_TEST = '0xe8a3a57e84a27d55e37116af4681abd461b73922';
/**
 * Longest `expirationDate - attestedDate` any attestation under this schema has carried.
 *
 * The protocol's term is 90 days = 7,776,000 s, but the two timestamps come from different
 * clocks — `expirationDate` is computed off Sumsub's wall clock before the transaction is sent,
 * `attestedDate` is the block timestamp when it lands — so the observed term jitters by a few
 * seconds either side. Measured across the schema's whole history on 2026-07-25: the maximum was
 * 7,776,001.
 *
 * This is the one number the enumeration's completeness depends on, so the probe does not trust
 * it blindly. Any attestation found carrying a longer term makes the window widen and the scan
 * repeat (`detail.windowWidened`), and the live test re-derives the maximum over the whole live
 * population every run.
 */
export const LINEA_POH_MAX_TERM_SECONDS = 7_776_001;
/** Doubling ladder cap: 2^18 = 262,144 ids, ~340× the window measured on 2026-07-25. */
export const LINEA_POH_LADDER_STEPS = 18;
/** Multicall3, deployed at the canonical address on Linea. */
const MULTICALL3 = '0xcA11bde05977b3631167028862bE2a173976CA11';
/**
 * Ids per batched `eth_call`. 200 keeps a batch's response under a megabyte on this schema,
 * which matters because `attestationData` and `subject` are both dynamic.
 */
const SCAN_BATCH = 200;
export const ATTESTATION_REGISTRY_ABI = parseAbi([
    'struct Attestation { bytes32 attestationId; bytes32 schemaId; bytes32 replacedBy; address attester; address portal; uint64 attestedDate; uint64 expirationDate; uint64 revocationDate; uint16 version; bool revoked; bytes subject; bytes attestationData; }',
    'function getAttestation(bytes32 attestationId) view returns (Attestation)',
    'function getAttestationIdCounter() view returns (uint32)',
    'function getVersionNumber() view returns (uint16)',
    'function router() view returns (address)',
]);
export const PORTAL_REGISTRY_ABI = parseAbi([
    'struct Portal { address id; address ownerAddress; address[] modules; bool isRevocable; string name; string description; string ownerName; }',
    'function getPortalByAddress(address id) view returns (Portal)',
    'function isIssuer(address issuer) view returns (bool)',
]);
export const SUMSUB_PORTAL_ABI = parseAbi(['function signerAddress() view returns (address)']);
const zero32 = `0x${'0'.repeat(64)}`;
export const idToBytes32 = (id) => toHex(BigInt(id), { size: 32 });
/**
 * `bytes` subject → address, or undefined when it is not 20 bytes and so is not an address.
 *
 * Verax's `subject` is arbitrary bytes: a DID, an off-chain identifier, another attestation's
 * id. Coercing something that is not 20 bytes into an address would invent a subject, so
 * anything else is dropped from the population rather than truncated into it.
 */
export function subjectToAddress(subject) {
    return subject.length === 42 ? subject.toLowerCase() : undefined;
}
/**
 * The ids a doubling ladder probes, nearest first.
 *
 * Pure so the shape can be asserted without a chain: strictly decreasing, never below 1, and
 * capped at `LINEA_POH_LADDER_STEPS` doublings.
 */
export function ladderIds(counter, steps = LINEA_POH_LADDER_STEPS) {
    const out = [];
    for (let k = 0; k <= steps; k++) {
        const id = counter - 2 ** k;
        if (id < 1)
            break;
        out.push(id);
    }
    return out;
}
/**
 * Which ladder rung to scan from, given each rung's `attestedDate` (undefined where the read
 * reverted, meaning the id does not exist and nothing older than it can be live).
 *
 * Returns 1 when no rung is old enough: scanning from the bottom is expensive and correct, and
 * silently truncating the window is cheap and wrong.
 */
export function floorFromLadder(rungs, cutoff) {
    for (const rung of rungs) {
        if (rung.attestedDate === undefined || rung.attestedDate < cutoff)
            return rung.id;
    }
    return 1;
}
/**
 * The whole "does this attestation count" decision, as a pure function of what was read.
 *
 * Separated from the I/O for the same reason `reconcile.ts` and `interpretSbt` are: every
 * branch that can silently change a score — a foreign portal, a revocation, an expiry, a
 * subject that is not an address, a renewal — is then testable without a network.
 *
 * `now` is the pinned block's timestamp rather than the local clock: expiry is what every
 * on-chain consumer compares against `block.timestamp`, and a skewed local clock must not be
 * what decides whether somebody is verified.
 */
export function selectLivePoh(raw, portals, now) {
    const ours = raw.filter((a) => a.schemaId === LINEA_POH_V2_SCHEMA);
    const bySubject = new Map();
    let liveAttestations = 0;
    let revokedInRange = 0;
    let rejectedForPortalOwner = 0;
    let attesterNotPortalSigner = 0;
    let maxTermSeconds = 0;
    for (const a of ours) {
        maxTermSeconds = Math.max(maxTermSeconds, a.termSeconds);
        if (a.revoked)
            revokedInRange++;
        const check = portals.get(a.portal.toLowerCase());
        // Anyone with a registered portal can write under this schema — schema ownership does not
        // restrict portals — so the portal's registered owner is the only thing that says Sumsub
        // made this claim.
        if (!check?.ownerIsSumsub) {
            rejectedForPortalOwner++;
            continue;
        }
        if (check.signerAddress !== undefined &&
            check.signerAddress.toLowerCase() !== a.attester.toLowerCase()) {
            // Corroboration, not a filter: the portal gates on a signature and this key is one
            // Sumsub may rotate, so a rotation must not retroactively un-verify anybody.
            attesterNotPortalSigner++;
        }
        // Revoked *and* expired both mean not held. Verax sets `revoked` on `revoke` and on
        // `replace`, and the Sumsub portals are all registered `isRevocable`, so this is a live
        // path and not a theoretical one: one attestation in the window on 2026-07-25 was revoked.
        if (a.revoked || a.expirationDate <= now)
            continue;
        liveAttestations++;
        const key = a.subject.toLowerCase();
        const { schemaId: _schemaId, revoked: _revoked, ...attestation } = a;
        const list = bySubject.get(key);
        if (list)
            list.push(attestation);
        else
            bySubject.set(key, [attestation]);
    }
    // Newest first: a renewal is a fresh Sumsub check, and on a decay curve the fresh one is the
    // credential the subject actually holds.
    for (const list of bySubject.values())
        list.sort((x, y) => y.attestedDate - x.attestedDate);
    return {
        bySubject,
        liveAttestations,
        attestationsInRange: ours.length,
        revokedInRange,
        maxTermSeconds,
        rejectedForPortalOwner,
        attesterNotPortalSigner,
    };
}
async function readAttestations(client, block, fromId, toId) {
    const out = [];
    for (let start = fromId; start < toId; start += SCAN_BATCH) {
        const end = Math.min(start + SCAN_BATCH, toId);
        const contracts = [];
        for (let id = start; id < end; id++) {
            contracts.push({
                address: VERAX_ATTESTATION_REGISTRY,
                abi: ATTESTATION_REGISTRY_ABI,
                functionName: 'getAttestation',
                args: [idToBytes32(id)],
            });
        }
        const results = await client.multicall({
            contracts,
            allowFailure: true,
            multicallAddress: MULTICALL3,
            blockNumber: block,
            batchSize: 0,
        });
        results.forEach((r, i) => {
            // A failure here is a genuinely absent id (the registry reverts above its counter), which
            // is information, not an error. Anything else would surface as a total scan failure.
            if (r.status !== 'success')
                return;
            const a = r.result;
            const subject = subjectToAddress(a.subject);
            if (subject === undefined)
                return;
            out.push({
                attestationId: idToBytes32(start + i),
                schemaId: a.schemaId,
                subject,
                attestedDate: Number(a.attestedDate),
                expirationDate: Number(a.expirationDate),
                termSeconds: Number(a.expirationDate) - Number(a.attestedDate),
                portal: a.portal,
                attester: a.attester,
                version: Number(a.version),
                revoked: a.revoked,
                replacedBy: a.replacedBy,
            });
        });
    }
    return out;
}
/**
 * The smallest `counter - 2^k` whose attestation predates `cutoff`.
 *
 * `attestedDate` is monotone in id, so such an id proves every attestation still unexpired lies
 * above it. The whole ladder goes out in one batched call; a step that reverts or reads before
 * the registry began is treated as satisfying the cutoff, since nothing older can be live.
 */
async function ladderFloor(client, block, counter, cutoff) {
    const steps = ladderIds(counter);
    if (steps.length === 0)
        return 1;
    const results = await client.multicall({
        contracts: steps.map((id) => ({
            address: VERAX_ATTESTATION_REGISTRY,
            abi: ATTESTATION_REGISTRY_ABI,
            functionName: 'getAttestation',
            args: [idToBytes32(id)],
        })),
        allowFailure: true,
        multicallAddress: MULTICALL3,
        blockNumber: block,
        batchSize: 0,
    });
    return floorFromLadder(steps.map((id, i) => {
        const r = results[i];
        return r.status === 'success'
            ? { id, attestedDate: Number(r.result.attestedDate) }
            : { id };
    }), cutoff);
}
async function checkPortals(client, block, portals) {
    const out = new Map();
    if (portals.length === 0)
        return out;
    const registryReads = await client.multicall({
        contracts: portals.flatMap((p) => [
            {
                address: VERAX_PORTAL_REGISTRY,
                abi: PORTAL_REGISTRY_ABI,
                functionName: 'getPortalByAddress',
                args: [p],
            },
            {
                address: p,
                abi: SUMSUB_PORTAL_ABI,
                functionName: 'signerAddress',
            },
        ]),
        allowFailure: true,
        multicallAddress: MULTICALL3,
        blockNumber: block,
        batchSize: 0,
    });
    const owners = [];
    portals.forEach((p, i) => {
        const portalRead = registryReads[i * 2];
        const signerRead = registryReads[i * 2 + 1];
        const registered = portalRead.status === 'success'
            ? portalRead.result
            : undefined;
        const owner = registered?.ownerAddress ?? zero32.slice(0, 42);
        owners.push(owner);
        out.set(p.toLowerCase(), {
            portal: p,
            ownerAddress: owner,
            ownerName: registered?.ownerName ?? '',
            ownerIsSumsub: owner.toLowerCase() === SUMSUB_PORTAL_OWNER.toLowerCase(),
            ownerIsRegisteredIssuer: false,
            ...(signerRead.status === 'success' ? { signerAddress: signerRead.result } : {}),
            modules: registered?.modules ?? [],
        });
    });
    // `isIssuer` is a second read because the owner is only known after the first.
    const issuerReads = await client.multicall({
        contracts: owners.map((o) => ({
            address: VERAX_PORTAL_REGISTRY,
            abi: PORTAL_REGISTRY_ABI,
            functionName: 'isIssuer',
            args: [o],
        })),
        allowFailure: true,
        multicallAddress: MULTICALL3,
        blockNumber: block,
        batchSize: 0,
    });
    portals.forEach((p, i) => {
        const r = issuerReads[i];
        const entry = out.get(p.toLowerCase());
        entry.ownerIsRegisteredIssuer = r.status === 'success' && r.result === true;
    });
    return out;
}
/**
 * Enumerate every live Linea PoH V2 attestation at one block.
 *
 * Order matters: the counter and the block are fixed first, then the range, then the scan, then
 * the portal provenance for whichever portals actually appeared. Nothing here is keyed on an
 * address, so one snapshot answers for every subject.
 */
export async function lineaPohSnapshot(client, opts = {}) {
    const block = opts.block ?? (await client.getBlockNumber());
    const blockData = await client.getBlock({ blockNumber: block });
    // The chain's own clock, not ours: expiry is compared against `block.timestamp` by every
    // contract that consumes these attestations, and a skewed local clock must not decide
    // whether somebody is verified.
    const now = Number(blockData.timestamp);
    const counter = Number(await client.readContract({
        address: VERAX_ATTESTATION_REGISTRY,
        abi: ATTESTATION_REGISTRY_ABI,
        functionName: 'getAttestationIdCounter',
        blockNumber: block,
    }));
    let term = opts.termSeconds ?? LINEA_POH_MAX_TERM_SECONDS;
    let widened = false;
    for (let attempt = 0;; attempt++) {
        const fromId = await ladderFloor(client, block, counter, now - term);
        const raw = await readAttestations(client, block, fromId, counter);
        const ours = raw.filter((a) => a.schemaId === LINEA_POH_V2_SCHEMA);
        const maxTerm = ours.reduce((m, a) => Math.max(m, a.termSeconds), 0);
        // A term longer than we budgeted for means the window may have cut off a live attestation.
        // Doubling and rescanning costs a few calls; assuming otherwise costs a false negative we
        // would never see.
        if (maxTerm > term && attempt < 3) {
            term = Math.max(maxTerm, term * 2);
            widened = true;
            continue;
        }
        const portals = await checkPortals(client, block, [
            ...new Map(ours.map((a) => [a.portal.toLowerCase(), a.portal])).values(),
        ]);
        const selection = selectLivePoh(ours, portals, now);
        return {
            block: Number(block),
            takenAt: now,
            counter,
            scannedFromId: fromId,
            windowWidened: widened,
            portals: [...portals.values()],
            ...selection,
        };
    }
}
/** `rpc.linea.build` is Consensys' own keyless endpoint and serves Multicall3 batches happily. */
export const LINEA_RPC = 'https://rpc.linea.build';
/** Two minutes: about sixty Linea blocks, and 0.002% of the credential's life. */
export const LINEA_POH_SNAPSHOT_TTL_MS = 120_000;
/**
 * The Linea PoH V2 probe.
 *
 * The snapshot is shared across subjects and across the whole `resolve()` call — a multi-address
 * subject costs one enumeration, not one per address — and is dropped as soon as it goes stale
 * so a lookup can never be answered from a snapshot older than the TTL.
 */
export function lineaPohAdapter(opts = {}) {
    const client = createPublicClient({
        chain: linea,
        transport: http(opts.rpcUrl ?? LINEA_RPC, { timeout: opts.timeoutMs ?? 20_000 }),
    });
    const ttl = opts.snapshotTtlMs ?? LINEA_POH_SNAPSHOT_TTL_MS;
    let cached;
    let inFlight;
    const snapshot = () => {
        if (cached && Date.now() - cached.at < ttl)
            return Promise.resolve(cached.snapshot);
        if (!inFlight) {
            inFlight = lineaPohSnapshot(client)
                .then((s) => {
                cached = { at: Date.now(), snapshot: s };
                return s;
            })
                .finally(() => {
                inFlight = undefined;
            });
        }
        return inFlight;
    };
    return {
        adapterId: 'linea-poh',
        async probe(subject) {
            try {
                const snap = await snapshot();
                const held = snap.bySubject.get(subject.toLowerCase());
                const provenance = {
                    heldFrom: 'chain',
                    dateFrom: held ? 'chain' : 'none',
                    headBlock: snap.block,
                    notes: [],
                };
                /**
                 * Reported on both branches, because the interesting fact about a negative here is that
                 * it is *exhaustive*: we did not fail to find the credential, we read every live one
                 * there is. `liveAttestations` and `liveSubjects` are what make that checkable.
                 */
                const population = {
                    liveAttestations: snap.liveAttestations,
                    liveSubjects: snap.bySubject.size,
                    scannedIds: snap.counter - snap.scannedFromId,
                    scannedFromId: snap.scannedFromId,
                    attestationIdCounter: snap.counter,
                    atBlock: snap.block,
                    maxTermSeconds: snap.maxTermSeconds,
                    ...(snap.windowWidened ? { windowWidened: true } : {}),
                    ...(snap.rejectedForPortalOwner
                        ? { rejectedForPortalOwner: snap.rejectedForPortalOwner }
                        : {}),
                    ...(snap.attesterNotPortalSigner
                        ? { attesterNotPortalSigner: snap.attesterNotPortalSigner }
                        : {}),
                };
                if (!held || held.length === 0) {
                    return { held: false, provenance, detail: { ...population, revocationsInRange: snap.revokedInRange } };
                }
                const current = held[0];
                const portal = snap.portals.find((p) => p.portal.toLowerCase() === current.portal.toLowerCase());
                return {
                    held: true,
                    issuedAt: current.attestedDate,
                    provenance,
                    detail: {
                        ...population,
                        attestationId: current.attestationId,
                        expiresAt: current.expirationDate,
                        termSeconds: current.termSeconds,
                        portal: current.portal,
                        portalOwner: portal?.ownerAddress,
                        portalOwnerIsRegisteredIssuer: portal?.ownerIsRegisteredIssuer,
                        portalValidationModules: portal?.modules.length ?? 0,
                        attester: current.attester,
                        attesterIsPortalSigner: portal?.signerAddress === undefined
                            ? undefined
                            : portal.signerAddress.toLowerCase() === current.attester.toLowerCase(),
                        schemaVersion: current.version,
                        ...(current.replacedBy !== zero32 ? { replacedBy: current.replacedBy } : {}),
                        ...(held.length > 1
                            ? { liveAttestationsForSubject: held.length, renewedEarly: true }
                            : {}),
                    },
                };
            }
            catch (e) {
                // Never a `false`: a Linea outage is not evidence that this person failed a liveness check.
                return { held: false, error: e instanceof Error ? e.message : String(e) };
            }
        },
    };
}
