/**
 * Core types.
 *
 * Design note that governs everything here: we return evidence and a score, and we let
 * the caller decide. `isHuman` takes a required threshold rather than defaulting to one,
 * because at a plausible 2% sybil rate a 95%-specificity classifier is wrong about roughly
 * three-quarters of the people it flags. Denial is the caller's decision to own, not a
 * default we ship.
 */
export {};
