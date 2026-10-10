import enrollmentData from './enrollment.json' with { type: 'json' };
const NEGLIGIBLE_COST_CENTS = 10;
const byRoot = enrollmentData.byRoot;
/** Every root we can actually route someone to. */
export function enrollableRoots() {
    return Object.keys(byRoot);
}
/**
 * What would raise this subject's independence, and where to go.
 *
 * `adapters` is the ontology (from `Corroborate#ontology()`), used to price each unheld root at
 * its strongest available credential — the same figure scoring would credit.
 */
export function suggestEnrollment(result, adapters) {
    // A root counts as held only if it carries real weight. A discontinued protocol leaves the
    // root genuinely empty, so we should still route the person there.
    const held = new Set(result.evidence
        .filter((e) => e.held && e.effectiveCostCents >= NEGLIGIBLE_COST_CENTS)
        .map((e) => e.trustRoot));
    // Strongest credential per root, priced as scoring prices it: min(forge, rent).
    const strongestByRoot = new Map();
    for (const a of adapters.values()) {
        if (!a.live)
            continue;
        const cents = Math.min(a.forgeCostCents, a.rentCostCents);
        const prev = strongestByRoot.get(a.trustRoot) ?? 0;
        if (cents > prev)
            strongestByRoot.set(a.trustRoot, cents);
    }
    const suggestions = [];
    const wouldAddNothing = [];
    for (const [trustRoot, options] of Object.entries(byRoot)) {
        if (held.has(trustRoot)) {
            wouldAddNothing.push({ trustRoot, options });
            continue;
        }
        // Fall back to the enrolment list's own root if the ontology has no live adapter for it.
        const contributionCents = strongestByRoot.get(trustRoot) ?? 0;
        if (contributionCents < NEGLIGIBLE_COST_CENTS)
            continue;
        const projectedCents = result.totalCostCents + contributionCents;
        const projectedScore = Number(Math.log10(projectedCents + 1).toFixed(4));
        suggestions.push({
            trustRoot,
            contributionCents,
            projectedScore,
            scoreGain: Number((projectedScore - result.score).toFixed(4)),
            projectedRoots: result.independentRoots + 1,
            options,
        });
    }
    suggestions.sort((a, b) => b.scoreGain - a.scoreGain);
    return {
        currentScore: result.score,
        currentRoots: result.independentRoots,
        heldRoots: [...held],
        suggestions,
        wouldAddNothing,
        caveat: 'These are ranked by what they would add to your score, which is not the same as what they cost you. Each one is a real-world enrolment with a privacy price stated on it — a permanent public video, a biometric in someone else\'s registry, a KYC file at a vendor. Raising a number is a bad reason to hand over any of that. There is also no obligation: most people hold none of these credentials, and an absence of evidence is not evidence of absence.',
    };
}
