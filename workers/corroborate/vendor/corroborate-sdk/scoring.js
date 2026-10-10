/**
 * Root-cost aggregation.
 *
 * The rule is one line: **saturate within a trust root, sum across trust roots.**
 *
 * Additive scoring over credentials is wrong, and wrong specifically in the adversary's
 * favour. A farm's credentials are maximally correlated — one passport presented to every
 * protocol that reads passports — while a real person's credentials are diverse. Summing
 * therefore rewards exactly the pattern it is supposed to catch. Concretely: from one
 * government document a subject can hold World's document tier, ZKPassport and Self, and a
 * naive scorer credits three independent proofs for one trip to a passport office.
 *
 * We cannot fix this by deduplicating credentials, because we usually cannot tell that two
 * came from one document: ZKPassport scopes its nullifier per service and never publishes
 * an unscoped value, while Self publishes a global one — four protocols, four incompatible
 * derivations over the same chip. Dedup would require linking a user's credentials to each
 * other, which is the correlation their nullifier designs exist to prevent.
 *
 * So we work at the level of the credential *class* instead. We never need to know that
 * *this* subject's two credentials share a root — only that those two *protocols* read the
 * same root, which is a public fact about the world. Within a root we take the strongest
 * credential and discard the rest; across roots we add. Saturation is also the right move
 * under uncertainty: when correlation is unobservable it bounds the honest user's loss at
 * one root's worth, while the adversary's gain from additive scoring would grow without
 * limit in the number of protocols reading the same root.
 *
 * Cost is denominated in what it costs an adversary, not in what it proves. A credential is
 * only as strong as the cheaper of forging one and renting one, because a holder willing to
 * rent defeats any amount of cryptography.
 */
/** Adapter costs are in cents; below this a credential is treated as free. */
export const NEGLIGIBLE_COST_CENTS = 10;
/** Weight retained by an adapter whose upstream protocol is discontinued. */
const DEAD_PROTOCOL_MULTIPLIER = 0;
/**
 * Age weight from the adapter's curve and half-life.
 *
 * Decay: weight falls with age — a stale selfie or KYC check means less than a fresh one.
 * Ramp: weight RISES with survival — right for vouching registries, where the suspect
 * cohort is by definition the fresh one. PoH took ~95% of its lifetime registrations in a
 * four-month reward window; under Ramp a week-old registration weighs ~0.01 while one that
 * has survived challenge windows for two years approaches full weight. Uniform decay would
 * have given the airdrop cohort full weight and discounted the organic one — exactly
 * backwards.
 *
 * Unknown age: Decay returns 1 (the credential's existence was verified live on-chain, and
 * absence of a date must not silently penalise) but Ramp returns 0.5 — Ramp exists to
 * discount fresh farms, and granting full weight on missing data would make the subgraph
 * being unreachable strictly profitable for an attacker. Both cases are flagged by the
 * issuance-date-unknown caveat.
 *
 * `issuedAfter` is a proven lower bound on issuance — the credential was absent from an index
 * with complete history at a block that index named, so it cannot be older than that block.
 * On a Ramp curve, where weight rises with age, the curve evaluated at that bound is the
 * highest weight the evidence can support, so we take it as a *cap*, never as a grant:
 * `min(unknown-age policy, curve(bound))`. Two consequences, both wanted. A synced index
 * prices a brand-new credential as brand new (~0) instead of at the 0.5 midpoint, which is
 * the bug this fixes. And an attacker who makes our index lag can at best recover the 0.5 it
 * would have had anyway — lag is never worth more than it costs. On a Decay curve the same
 * bound is a lower bound on weight rather than an upper one (a younger credential decays
 * less), so it cannot tighten the unknown-age answer and is ignored there.
 */
export function freshnessOf(adapter, issuedAt, now, issuedAfter) {
    if (!adapter.decayHalfLifeDays || adapter.ageCurve === 'None')
        return 1;
    if (issuedAt === undefined) {
        const unknown = adapter.ageCurve === 'Ramp' ? 0.5 : 1;
        if (issuedAfter === undefined || adapter.ageCurve !== 'Ramp')
            return unknown;
        const maxAgeDays = Math.max(0, (now - issuedAfter) / 86_400);
        const atBound = 1 - 2 ** (-maxAgeDays / adapter.decayHalfLifeDays);
        return Math.min(unknown, atBound);
    }
    const ageDays = (now - issuedAt) / 86_400;
    if (ageDays <= 0)
        return adapter.ageCurve === 'Ramp' ? 0 : 1;
    const decay = 2 ** (-ageDays / adapter.decayHalfLifeDays);
    return adapter.ageCurve === 'Ramp' ? 1 - decay : decay;
}
/**
 * What this credential costs an adversary right now.
 *
 * The `min` of forge and rent is the crux: every protocol that hardened did so against
 * *sale* — World's user-presence check, Idena's identity staking — and none against
 * *rental*, because the human stays willing. Taking the minimum means security work that
 * only addresses sale cannot inflate a score.
 */
export function effectiveCost(adapter, freshness) {
    if (!adapter.live)
        return DEAD_PROTOCOL_MULTIPLIER;
    const cheapestAttack = Math.min(adapter.forgeCostCents, adapter.rentCostCents);
    return cheapestAttack * freshness;
}
export function score(input) {
    const now = input.now ?? Math.floor(Date.now() / 1000);
    const held = input.evidence.filter((e) => e.held);
    // Group by trust root, then saturate: strongest credential wins, the rest are discarded
    // as already-counted evidence.
    const byRoot = new Map();
    for (const e of held) {
        const bucket = byRoot.get(e.trustRoot);
        if (bucket)
            bucket.push(e);
        else
            byRoot.set(e.trustRoot, [e]);
    }
    const roots = [];
    for (const [trustRoot, group] of byRoot) {
        const strongest = group.reduce((a, b) => (b.effectiveCostCents > a.effectiveCostCents ? b : a));
        // Saturated means value was actually discarded: more than one credential in this root
        // carried positive cost, so keeping the strongest genuinely dropped a real one. Two
        // discontinued (zero-cost) credentials sharing a root are NOT saturation — nothing of
        // value was collapsed, and the discontinued-protocol caveat already explains them.
        const contributors = group.filter((e) => e.effectiveCostCents > 0).length;
        roots.push({
            trustRoot,
            adapterIds: group.map((e) => e.adapterId),
            contributionCents: strongest.effectiveCostCents,
            saturated: contributors > 1,
        });
    }
    roots.sort((a, b) => b.contributionCents - a.contributionCents);
    const totalCostCents = roots.reduce((sum, r) => sum + r.contributionCents, 0);
    const independentRoots = roots.filter((r) => r.contributionCents >= NEGLIGIBLE_COST_CENTS).length;
    // log10 keeps the scale readable and stops one expensive credential from dwarfing
    // everything else. +1 so an empty result is 0 rather than -Infinity.
    const scoreValue = totalCostCents <= 0 ? 0 : Math.log10(totalCostCents + 1);
    return {
        subject: input.subjects[0],
        subjects: input.subjects,
        ...(input.name !== undefined ? { name: input.name } : {}),
        score: Number(scoreValue.toFixed(4)),
        totalCostCents,
        independentRoots,
        evidence: input.evidence,
        roots,
        caveats: caveatsFor(input.evidence, roots, input.adapters, input.asOf),
        ...(input.registryRevision !== undefined ? { registryRevision: input.registryRevision } : {}),
        ...(input.asOf ? { asOf: input.asOf } : {}),
        computedAt: now,
        isHuman(threshold) {
            if (!Number.isFinite(threshold)) {
                throw new TypeError('isHuman requires an explicit numeric threshold');
            }
            return scoreValue >= threshold;
        },
    };
}
function caveatsFor(evidence, roots, adapters, asOf) {
    const caveats = [];
    // Always present, and deliberately not suppressible. Nothing in the roster attests that a
    // verified human acts for themselves rather than as a paid puppet, and the on-chain
    // signature of puppeteering is identical to that of voluntary delegation.
    caveats.push({
        code: 'independent-control-not-attested',
        message: 'No protocol here proves the subject controls their own credentials. A verified unique human may still be operated by someone else, and that is not detectable from this evidence.',
    });
    for (const r of roots) {
        if (r.saturated) {
            caveats.push({
                code: 'correlated-evidence-saturated',
                message: `${r.adapterIds.length} credentials share the trust root "${r.trustRoot}" (${r.adapterIds.join(', ')}). Counted once — they are one piece of evidence, not ${r.adapterIds.length}.`,
            });
        }
    }
    // Saturation deliberately spans addresses: one passport presented from two wallets is
    // still one passport, so splitting credentials across wallets cannot inflate a score.
    const addressesWithEvidence = new Set(evidence.filter((e) => e.held).map((e) => e.observedOn));
    if (addressesWithEvidence.size > 1) {
        caveats.push({
            code: 'multi-address-subject',
            message: `Evidence aggregated across ${addressesWithEvidence.size} addresses supplied by the caller. Correlated roots still saturate across them, so distributing credentials over wallets does not raise the score.`,
        });
    }
    const dead = evidence.filter((e) => e.held && !e.live);
    if (dead.length) {
        caveats.push({
            code: 'discontinued-protocol',
            message: `Held but scored at zero because the upstream protocol is discontinued: ${dead.map((e) => e.adapterId).join(', ')}.`,
        });
    }
    const unknownAge = evidence.filter((e) => e.held && e.issuedAt === undefined && e.issuedAfter === undefined);
    if (unknownAge.length) {
        caveats.push({
            code: 'issuance-date-unknown',
            message: `Issue date unavailable for: ${unknownAge.map((e) => e.adapterId).join(', ')}. Decay-class credentials keep full weight and may be stale; survival-ramp credentials are held at the 0.5 midpoint rather than granted full weight.`,
        });
    }
    caveats.push(...restatementCaveats(evidence, adapters));
    caveats.push(...indexCaveats(evidence));
    if (asOf)
        caveats.push(...asOfCaveats(asOf));
    const unknownRoot = roots.find((r) => r.trustRoot === 'unknown');
    if (unknownRoot) {
        caveats.push({
            code: 'unresolved-trust-root',
            message: `Evidence from ${unknownRoot.adapterIds.join(', ')} has an unestablished trust root. Scored as independent, but it may in fact be correlated with another credential here.`,
        });
    }
    if (roots.length === 0) {
        caveats.push({
            code: 'no-evidence',
            message: 'No credentials found. This is an absence of evidence, not evidence of absence — most humans hold none of these.',
        });
    }
    return caveats;
}
/**
 * Caveats for aggregates that restate credentials we price elsewhere.
 *
 * Some of the things we probe are themselves aggregators. Human Passport's score is built from
 * stamps, and several of those stamps are credentials with their own entry in this ontology and
 * their own trust root — Coinbase on Persona, Holonym on FaceTec, BrightID, Idena. An
 * aggregate is not independent evidence of the things inside it.
 *
 * The arithmetic is already safe: an aggregate is rooted and priced at what it can honestly
 * claim on its own (Passport at wallet-history rates, a dollar), so even a passport made
 * entirely of restated identity stamps cannot contribute identity money. This caveat exists
 * because safe is not the same as legible — a caller reading "Human Passport: 28.8" deserves to
 * be told that two of its three stamps are credentials counted under other roots, and that the
 * remaining wallet history is what the dollar is for.
 */
function restatementCaveats(evidence, adapters) {
    const out = [];
    for (const e of evidence) {
        if (!e.held)
            continue;
        const restated = e.detail?.['restatesAdapters'];
        if (!Array.isArray(restated) || restated.length === 0)
            continue;
        const named = restated
            .filter((id) => typeof id === 'string')
            .map((id) => {
            const root = adapters.get(id)?.trustRoot;
            return root ? `${id} (${root})` : id;
        });
        if (named.length === 0)
            continue;
        out.push({
            code: 'aggregate-restates-other-credentials',
            message: `${e.adapterId} is an aggregate whose evidence includes credentials priced separately here: ${named.join(', ')}. It is scored only for what it can claim on its own — ${e.trustRoot} — so those credentials count under their own roots and not twice.`,
        });
    }
    return out;
}
/**
 * Caveats for a score computed as of a past block.
 *
 * The first one is unconditional and deliberately blunt. An as-of score reconstructs the
 * *ontology* exactly and the *credential state* only partially, and a reader who does not know
 * which half is which will take the whole thing as a historical fact. So the result says, every
 * time, which half is reconstructed and which direction the remaining error runs in.
 */
function asOfCaveats(asOf) {
    const out = [
        {
            code: 'scored-as-of-past-block',
            message: `Scored against the ontology as it stood at Sepolia block ${asOf.block} (${new Date(asOf.timestamp * 1000).toISOString()}), registry revision ${asOf.registryRevision} with ${asOf.adapterCount} adapters. The weights and trust roots are reconstructed exactly from the registry's own event history. Credentials, though, were read from their chains at head: a credential dated after that instant has been excluded, but one held then and revoked since cannot be seen, so this can understate the subject and never the adversary.`,
        },
    ];
    if (asOf.adaptersNotYetInRegistry.length) {
        out.push({
            code: 'adapter-not-in-registry-at-asof',
            message: `${asOf.adaptersNotYetInRegistry.join(', ')} had no entry in the registry at revision ${asOf.registryRevision}, so any credential found for them is unpriced and contributes nothing. They were added later; that is a change in what we knew, not in the subject.`,
        });
    }
    if (asOf.issuedAfterAsOf.length) {
        out.push({
            code: 'credential-issued-after-asof',
            message: `Excluded as not yet existing at this block: ${asOf.issuedAfterAsOf.join(', ')}. Each is held today and dated after ${new Date(asOf.timestamp * 1000).toISOString()}.`,
        });
    }
    if (asOf.existenceUnverified.length) {
        out.push({
            code: 'asof-existence-unverified',
            message: `${asOf.existenceUnverified.join(', ')} publish no issuance date, so nothing here shows they were held at this block rather than acquired since. They are counted, because dropping every undated credential would penalise the subject for a field the protocol does not store — but this part of the score is a statement about today.`,
        });
    }
    if (!asOf.auditTrailComplete) {
        out.push({
            code: 'registry-audit-trail-incomplete',
            message: `The registry counted revisions ${(asOf.missingRevisions ?? []).join(', ')} that the audit trail has no record of. Those are liveness flips, which the indexer stores only by hash, so a protocol marked live here may have been marked dead at this block. The reconstruction is not exact.`,
        });
    }
    return out;
}
/**
 * Caveats derived from probe provenance — how each answer was reached, and at which block.
 *
 * These exist because the index-first inversion trades silence for noise on purpose. The old
 * behaviour was quiet and wrong: a lagging index moved scores with nothing in the result to
 * say so. A degraded read is now always visible, and always names the block it came from, so
 * "why did my score move?" has an answer a subject can check.
 */
function indexCaveats(evidence) {
    const out = [];
    const withNote = (note) => evidence.filter((e) => e.provenance?.notes.includes(note));
    const ids = (es) => es.map((e) => e.adapterId).join(', ');
    const blocks = (es) => [...new Set(es.map((e) => e.provenance?.indexedBlock).filter((b) => b !== undefined))].join(', ');
    const notIndexed = withNote('credential-not-yet-indexed').filter((e) => e.held);
    if (notIndexed.length) {
        out.push({
            code: 'credential-not-yet-indexed',
            message: `Held on chain but absent from the index at block ${blocks(notIndexed)}: ${ids(notIndexed)}. The credential therefore did not exist at that block, so it is dated no earlier than it and priced at that upper bound on age — not at the unknown-age midpoint. Index lag cannot raise this score.`,
        });
    }
    const ceased = withNote('credential-ceased-since-index');
    if (ceased.length) {
        out.push({
            code: 'credential-ceased-since-index',
            message: `The index lists ${ids(ceased)} as held at block ${blocks(ceased)}, but the contract read at chain head does not. Scored as not held: a revocation must not stay invisible for as long as the index lags.`,
        });
    }
    const lowerBound = withNote('index-date-is-lower-bound').filter((e) => e.held);
    if (lowerBound.length) {
        out.push({
            code: 'issuance-date-lower-bound',
            message: `The index dated ${ids(lowerBound)} from a side-event (a vouch or a trust edge) rather than from the issuance event itself, because the issuance falls outside its indexed window. The real credential is therefore older than the date used, and on a survival ramp its weight here is a floor rather than an estimate.`,
        });
    }
    const disagrees = withNote('index-date-disagrees-with-chain').filter((e) => e.held);
    if (disagrees.length) {
        out.push({
            code: 'index-date-disagrees-with-chain',
            message: `Index and contract disagree about the issuance date of ${ids(disagrees)} by more than an hour. The contract read was used, since it needs no indexer; the disagreement is a fault in our indexing, not in the credential.`,
        });
    }
    const partial = withNote('index-outside-coverage').filter((e) => e.held);
    if (partial.length) {
        out.push({
            code: 'index-coverage-partial',
            message: `The index does not cover the full history of ${ids(partial)}, so its silence says nothing about this credential. Fell back to the contract read alone, exactly as if no index were configured.`,
        });
    }
    const unreachable = withNote('index-unreachable').filter((e) => e.held);
    if (unreachable.length) {
        out.push({
            code: 'index-unreachable',
            message: `An index was configured but did not answer for ${ids(unreachable)}. Held state comes from the contract read; anything only the index can supply — graph position, revocation history — is missing from this result.`,
        });
    }
    const imported = withNote('date-from-registry-import').filter((e) => e.held);
    if (imported.length) {
        out.push({
            code: 'credential-imported-from-predecessor-registry',
            message: `${ids(imported)} was carried into its current registry by a bulk import rather than issued there, so the only date the contract can give is the import. The credential is genuinely older than that, and on a survival ramp the weight here is a floor rather than an estimate.`,
        });
    }
    const transferred = withNote('credential-transferred-since-issuance').filter((e) => e.held);
    if (transferred.length) {
        out.push({
            code: 'credential-changed-hands',
            message: `${ids(transferred)} is transferable and was not issued to the address holding it. It is dated from when this address acquired it, not from when it was created, so an aged credential bought on a secondary market earns the age of the purchase. The acquisition was found by bisecting custody, which cannot rule out an earlier stint the subject no longer held.`,
        });
    }
    const fromExpiry = withNote('date-from-expiry-and-max-term').filter((e) => e.held);
    if (fromExpiry.length) {
        out.push({
            code: 'issuance-date-derived-from-expiry',
            message: `${ids(fromExpiry)} publishes an expiry and no issuance date — deliberately, so that the expiry does not reveal when the holder was verified. The date used is the expiry minus the longest term the protocol's circuit permits, which is the earliest the credential can have been issued and therefore the oldest it can be. On a decay curve the weight here is a floor rather than an estimate.`,
        });
    }
    const reattested = withNote('date-from-latest-reattestation').filter((e) => e.held);
    if (reattested.length) {
        out.push({
            code: 'issuance-date-is-latest-renewal',
            message: `${ids(reattested)} is a renewable on-chain attestation with a fixed term, and the date used is the last renewal — the enrolment behind it is older and the protocol does not publish it. So this measures how recently the address re-proved the credential, not how long ago the human was verified.`,
        });
    }
    const registered = withNote('date-from-agent-registration').filter((e) => e.held);
    if (registered.length) {
        out.push({
            code: 'issuance-date-is-registration',
            message: `${ids(registered)} is held here through a registry that records a binding and never an expiry, so the date used is the block that registration was mined in — the moment the protocol last accepted a proof for this address, not when the human was enrolled. The enrolment is older and is not published on chain, so on a decay curve the weight here is a ceiling rather than an estimate. Without this date the credential would carry full weight indefinitely.`,
        });
    }
    const stale = withNote('freshness-check-unavailable');
    if (stale.length) {
        out.push({
            code: 'freshness-check-unavailable',
            message: `The contract read failed for ${ids(stale)}, so this rests on the index alone at block ${blocks(stale)}. Nothing here confirms the credential has not been revoked since that block.`,
        });
    }
    return out;
}
