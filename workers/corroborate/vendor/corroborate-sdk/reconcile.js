/**
 * Reconciling an index against a chain head.
 *
 * The bug this file exists to kill: probing the contract for *whether* a credential is held
 * and the index for *when* it was issued is a torn read. When the contract says held at
 * chain head but the index has not seen the credential yet, the old code returned
 * held-with-unknown-age, and unknown age on a `Ramp` curve scores at the 0.5 midpoint. So
 * index lag silently moved scores — and moved them upward for exactly the fresh cohort the
 * ramp exists to discount. Making our index lag was a cheap way for an attacker to buy 0.5
 * weight on a week-old registration worth ~0.02.
 *
 * The fix is to stop treating the two reads as one. Each read is a statement about a
 * *named block*: the index knows the world as of the block it reports, and the contract read
 * knows it at head. Absence in the index is only informative when the index has complete
 * history for that credential — and then it is genuinely informative, because "not present
 * at block B" means "created after block B", which bounds the credential's age from above.
 * Where the index cannot see the credential's history at all (a windowed data source), the
 * fallback is the contract read exactly as before, and it says so.
 *
 * Nothing here does I/O; it is the decision table, so every branch is unit-testable without
 * a network. The rule it encodes:
 *
 *   - the chain decides whether a credential is held *now* (a revocation must not be
 *     invisible for as long as the index lags)
 *   - the date comes from the most authoritative source available: the contract when the
 *     protocol exposes one, else the index at its named block, else a bound derived from
 *     the index's own absence, else nothing — and which one it was is always reported
 *   - a disagreement is never averaged away or silently resolved; it is flagged
 */
/**
 * Dates from two sources never match to the second — the index stamps the block timestamp of
 * the event, the contract derives it from an expiry — so only a real disagreement should be
 * flagged. One hour is far below the resolution of any age curve in the ontology (the
 * shortest half-life is 90 days) and far above block-time jitter.
 */
export const DATE_AGREEMENT_TOLERANCE_SECONDS = 3600;
export function reconcileIndexAndChain(input) {
    const { chain, index } = input;
    const notes = [];
    const base = {
        ...(index ? { indexedBlock: index.block } : {}),
        ...(index?.blockTimestamp !== undefined ? { indexedBlockTimestamp: index.blockTimestamp } : {}),
        ...(chain.block !== undefined ? { headBlock: chain.block } : {}),
    };
    // ---- the freshness check failed. The index, if it answered, is all we have.
    if (chain.unavailable) {
        if (!index) {
            return {
                held: false,
                provenance: { heldFrom: 'chain', dateFrom: 'none', ...base, notes: ['index-unavailable'] },
                error: 'contract read failed and no index answered',
            };
        }
        notes.push('freshness-check-unavailable');
        if (!index.entity) {
            // Nothing says this subject holds the credential. Absence in the index is not a
            // negative unless the index can actually see the history, so this is an error rather
            // than a `false` — a failed probe must never read as "not a human".
            if (!index.completeHistory)
                notes.push('index-outside-coverage');
            return {
                held: false,
                provenance: { heldFrom: 'index', dateFrom: 'none', ...base, notes },
                error: 'contract read failed and the index has no record to fall back on',
            };
        }
        if (index.entity.ended) {
            return { held: false, provenance: { heldFrom: 'index', dateFrom: 'none', ...base, notes } };
        }
        if (!index.entity.issuanceObserved)
            notes.push('index-date-is-lower-bound');
        return {
            held: true,
            issuedAt: index.entity.issuedAt,
            provenance: { heldFrom: 'index', dateFrom: 'index', ...base, notes },
        };
    }
    // ---- no index answered: contract only, exactly as before, and it says so.
    if (!index) {
        notes.push('index-unavailable');
        return {
            held: chain.held,
            ...(chain.issuedAt !== undefined ? { issuedAt: chain.issuedAt } : {}),
            provenance: {
                heldFrom: 'chain',
                dateFrom: chain.issuedAt !== undefined ? 'chain' : 'none',
                ...base,
                notes,
            },
        };
    }
    // ---- the credential is gone at head but the index still lists it as live.
    if (!chain.held) {
        if (index.entity && !index.entity.ended)
            notes.push('credential-ceased-since-index');
        return { held: false, provenance: { heldFrom: 'chain', dateFrom: 'none', ...base, notes } };
    }
    // ---- held at head. Date it as precisely as the evidence allows.
    if (chain.issuedAt !== undefined) {
        // The contract dates it itself, so index lag cannot move this score at all. The index
        // becomes a cross-check: a disagreement is a fact about our own pipeline and is reported.
        if (index.entity &&
            Math.abs(index.entity.issuedAt - chain.issuedAt) > DATE_AGREEMENT_TOLERANCE_SECONDS) {
            notes.push(index.entity.issuanceObserved ? 'index-date-disagrees-with-chain' : 'index-date-is-lower-bound');
        }
        return {
            held: true,
            issuedAt: chain.issuedAt,
            provenance: { heldFrom: 'chain', dateFrom: 'chain', ...base, notes },
        };
    }
    if (index.entity && !index.entity.issuanceObserved) {
        // The entity exists because something else touched it (a vouch, a trust edge), and that
        // event happened after issuance. Using its timestamp understates the credential's age,
        // which on a survival ramp understates its weight — wrong, but wrong in the subject's
        // disfavour rather than the adversary's, so we keep it and flag it.
        notes.push('index-date-is-lower-bound');
        return {
            held: true,
            issuedAt: index.entity.issuedAt,
            provenance: { heldFrom: 'chain', dateFrom: 'index', ...base, notes },
        };
    }
    if (index.entity) {
        return {
            held: true,
            issuedAt: index.entity.issuedAt,
            provenance: { heldFrom: 'chain', dateFrom: 'index', ...base, notes },
        };
    }
    // ---- the torn read. Held at head, absent from the index.
    if (index.completeHistory && index.blockTimestamp !== undefined) {
        // The index has complete history and does not have this credential at `block`, so the
        // credential was issued after that block. That is a real fact, not a guess, and it caps
        // the age at the index's lag — which for a synced index means "brand new", which is
        // exactly what it is.
        notes.push('credential-not-yet-indexed');
        return {
            held: true,
            issuedAfter: index.blockTimestamp,
            provenance: { heldFrom: 'chain', dateFrom: 'index-absence-bound', ...base, notes },
        };
    }
    // A windowed index (or one whose block timestamp we could not establish) tells us nothing
    // by absence. Fall back to the contract read alone and carry the caveat.
    notes.push('index-outside-coverage');
    return {
        held: true,
        provenance: { heldFrom: 'chain', dateFrom: 'none', ...base, notes },
    };
}
