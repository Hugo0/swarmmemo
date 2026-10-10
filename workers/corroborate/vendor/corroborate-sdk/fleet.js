import { NEGLIGIBLE_COST_CENTS, effectiveCost } from "./scoring.js";
const syntheticKey = (agent) => `unbacked:${agent.toLowerCase()}`;
/**
 * Apply a fleet policy to a set of agents. Pure — no I/O, so every branch is unit testable.
 *
 * Order of operations is the design, not an implementation detail. Identity, then evidence,
 * then the score line, and only then the slot allocation, so that an agent refused on evidence
 * never spends the allowance of a sibling that would have passed.
 */
export function evaluateFleet(input) {
    const { policy, agents, evidence } = input;
    // ---- 1. group agents by the human behind them --------------------------------------
    const groups = new Map();
    const verdicts = new Map();
    const order = [];
    for (const a of agents) {
        order.push(a.agent);
        const v = {
            agent: a.agent,
            ...(a.label !== undefined ? { label: a.label } : {}),
            verdict: 'indeterminate',
            because: '',
            rules: [],
        };
        verdicts.set(a.agent, v);
        if (a.backing.status === 'backed') {
            v.humanId = a.backing.humanId;
            v.rules.push({
                rule: 'human-identified',
                pass: true,
                // Source-neutral: this engine is fed by AgentBook's proof-derived identifiers and by
                // ENS's self-published ones, and naming the wrong registry in a trace is a lie about
                // where a fact came from. Which registry, and how strongly, is the next rule's job.
                detail: `attributed to human ${short(a.backing.humanId)}`,
            });
            bucket(groups, a.backing.humanId, false).agents.push(a);
            continue;
        }
        if (a.backing.status === 'unknown') {
            v.rules.push({
                rule: 'human-identified',
                pass: null,
                detail: `human-backing unreadable: ${a.backing.error}`,
            });
            v.verdict = 'indeterminate';
            v.because =
                'could not establish which human registered this agent, so the per-human cap cannot be applied — refusing to guess in either direction';
            continue;
        }
        // Unbacked. The cap is unenforceable here whatever we decide, so the policy has to say.
        if (policy.unbackedAgents === 'deny') {
            v.rules.push({
                rule: 'human-identified',
                pass: false,
                detail: 'no AgentBook registration: lookupHuman returned 0',
            });
            v.verdict = 'deny';
            v.because = `no human has registered this agent, and ${policy.name} does not admit agents it cannot attribute to a person`;
            continue;
        }
        v.rules.push({
            rule: 'human-identified',
            pass: false,
            detail: 'no AgentBook registration; admitted as its own human under policy.unbackedAgents',
        });
        bucket(groups, syntheticKey(a.agent), true).agents.push(a);
    }
    // ---- 2. evidence and score gates, per human ----------------------------------------
    // Evaluated once for the human and stamped onto each of their agents. An agent cannot carry
    // its own score: the credentials belong to the person, not to the wallet they registered.
    const humans = [];
    for (const [humanId, group] of groups) {
        const ev = evidence.get(humanId);
        const passing = [];
        for (const a of group.agents) {
            const v = verdicts.get(a.agent);
            // Binding strength is checked before anything is spent on this agent, and before the
            // slot allocation, so an agent refused for an unacknowledged claim never burns the
            // allowance of a sibling that would have passed — the same ordering rule the score
            // gates follow.
            const binding = a.backing.status === 'backed' ? (a.backing.binding ?? 'attested') : undefined;
            if (binding !== undefined) {
                const detail = a.backing.status === 'backed' && a.backing.bindingDetail
                    ? a.backing.bindingDetail
                    : binding === 'attested'
                        ? 'the human identifier came from a proof, not from a claim'
                        : 'only the agent asserts this human';
                if (binding === 'asserted' && policy.requireAttestedBinding) {
                    v.rules.push({ rule: 'human-binding', pass: false, detail });
                    v.verdict = 'deny';
                    v.because = `the human behind this agent has not acknowledged it (${detail}), and ${policy.name} does not admit one-way claims about a person`;
                    continue;
                }
                v.rules.push({ rule: 'human-binding', pass: binding === 'attested' ? true : null, detail });
            }
            if (!ev || ev.error !== undefined) {
                v.rules.push({
                    rule: 'evidence-resolved',
                    pass: null,
                    detail: ev?.error ?? 'no personhood evidence supplied for this human',
                });
                v.verdict = 'indeterminate';
                v.because = `personhood could not be resolved for the human behind this agent: ${ev?.error ?? 'no evidence supplied'}`;
                continue;
            }
            v.rules.push({
                rule: 'evidence-resolved',
                pass: true,
                detail: `score ${ev.score} across ${ev.independentRoots} independent root(s)${ev.roots?.length ? ` (${ev.roots.join(', ')})` : ''}`,
            });
            const scoreOk = ev.score >= policy.minScore;
            v.rules.push({
                rule: 'min-score',
                pass: scoreOk,
                detail: `${ev.score} ${scoreOk ? '≥' : '<'} ${policy.minScore}`,
            });
            const rootsOk = ev.independentRoots >= policy.minIndependentRoots;
            v.rules.push({
                rule: 'min-independent-roots',
                pass: rootsOk,
                detail: `${ev.independentRoots} ${rootsOk ? '≥' : '<'} ${policy.minIndependentRoots} required`,
            });
            if (!scoreOk || !rootsOk) {
                v.verdict = 'deny';
                v.because = !scoreOk
                    ? `score ${ev.score} < ${policy.minScore}`
                    : `${ev.independentRoots} independent root(s) < ${policy.minIndependentRoots} required`;
                continue;
            }
            passing.push(a);
        }
        // ---- 3. slot allocation, over the agents that got this far -----------------------
        const queue = admissionOrder(passing, policy.admission, order);
        const admitted = [];
        for (const a of queue) {
            const v = verdicts.get(a.agent);
            if (admitted.length < policy.maxAgentsPerHuman) {
                admitted.push(a);
                v.rules.push({
                    rule: 'max-agents-per-human',
                    pass: true,
                    detail: `slot ${admitted.length} of ${policy.maxAgentsPerHuman} for human ${short(humanId)}`,
                });
                v.verdict = 'allow';
                v.because = `score ${evidence.get(humanId).score} ≥ ${policy.minScore} across ${evidence.get(humanId).independentRoots} independent root(s), and this human holds ${admitted.length} of ${policy.maxAgentsPerHuman} permitted agent slot(s)`;
                continue;
            }
            const takenBy = admitted.map((x) => x.label ?? x.agent).join(', ');
            v.rules.push({
                rule: 'max-agents-per-human',
                pass: false,
                detail: `human ${short(humanId)} already holds ${policy.maxAgentsPerHuman} of ${policy.maxAgentsPerHuman} slot(s), taken by ${takenBy}`,
            });
            v.verdict = 'deny';
            v.because = `the human behind this agent already holds ${policy.maxAgentsPerHuman} agent slot(s) (${takenBy}) — a fleet of ${group.agents.length} agents is still one human`;
        }
        humans.push({
            humanId,
            synthetic: group.synthetic,
            agents: group.agents.map((a) => a.agent),
            admitted: admitted.map((a) => a.agent),
            denied: group.agents.filter((a) => verdicts.get(a.agent).verdict !== 'allow').map((a) => a.agent),
            ...(ev ? { evidence: ev } : {}),
        });
    }
    // ---- 4. summary and caveats ---------------------------------------------------------
    const all = order.map((a) => verdicts.get(a));
    const realHumans = humans.filter((h) => !h.synthetic);
    const unbacked = agents.filter((a) => a.backing.status === 'unbacked').length;
    const unresolved = agents.filter((a) => a.backing.status === 'unknown').length;
    const attributed = agents.length - unbacked - unresolved;
    const largestFleet = realHumans.reduce((m, h) => Math.max(m, h.agents.length), 0);
    const assertedBindings = agents.filter((a) => a.backing.status === 'backed' && a.backing.binding === 'asserted').length;
    const summary = {
        agents: agents.length,
        humans: realHumans.length,
        unbacked,
        unresolved,
        allowed: all.filter((v) => v.verdict === 'allow').length,
        denied: all.filter((v) => v.verdict === 'deny').length,
        deniedByCap: all.filter((v) => v.verdict === 'deny' &&
            v.rules.some((r) => r.rule === 'max-agents-per-human' && r.pass === false)).length,
        indeterminate: all.filter((v) => v.verdict === 'indeterminate').length,
        largestFleet,
        assertedBindings,
        collapseRatio: realHumans.length === 0 ? 0 : round(attributed / realHumans.length, 2),
    };
    const caveats = [];
    if (unbacked > 0 && policy.unbackedAgents === 'count-as-distinct-human') {
        caveats.push({
            code: 'fleet-cap-not-enforceable-on-unbacked-agents',
            message: `${unbacked} agent(s) have no AgentBook registration and this policy counts each as its own human. The per-human cap does not bind them: an operator gets one slot per wallet it generates, and generating a wallet is free.`,
        });
    }
    // An evidence map that names humans this batch does not contain, while agents in this batch
    // went unjudged for want of evidence, is a caller bug rather than a fact about the world —
    // most often the same identifier encoded two ways. It is worth a caveat because the failure
    // is otherwise indistinguishable from an honest lookup miss, and it degrades silently in the
    // permissive direction: nobody is refused, so nothing looks wrong.
    const missingEvidence = all.some((v) => v.rules.some((r) => r.rule === 'evidence-resolved' && r.pass === null));
    const unmatchedKeys = [...evidence.keys()].filter((k) => !groups.has(k));
    if (missingEvidence && unmatchedKeys.length) {
        caveats.push({
            code: 'fleet-evidence-keys-unmatched',
            message: `Evidence was supplied for ${unmatchedKeys.length} identifier(s) that no agent in this batch belongs to (${unmatchedKeys
                .map(short)
                .join(', ')}), while other agents were left unjudged for want of evidence. The evidence map is keyed on the same humanId the registry returns; check the two are encoded the same way.`,
        });
    }
    if (unresolved > 0) {
        caveats.push({
            code: 'fleet-membership-unresolved',
            message: `${unresolved} agent(s) could not be attributed to a human because the registry read failed. They are neither admitted nor refused. Re-run before treating this decision as complete — a fleet is only bounded if every member was identified.`,
        });
    }
    if (policy.admission === 'earliest-registered' && agents.some((a) => a.registeredAtBlock === undefined)) {
        caveats.push({
            code: 'fleet-admission-order-degraded',
            message: 'This policy allocates slots to the earliest registration, but some agents were supplied without a registration block. Those fall back to the order they were presented in, so which sibling keeps the slot depends on the caller rather than on the chain.',
        });
    }
    if (assertedBindings > 0 && !policy.requireAttestedBinding) {
        caveats.push({
            code: 'fleet-cap-soft-on-asserted-bindings',
            message: `${assertedBindings} agent(s) name a human who has not acknowledged them, and this policy admits one-way claims. The cap groups agents by the human they name, so an operator naming a fresh identifier per agent holds one slot each — and each such agent may also be riding credentials its named human never lent it. requireAttestedBinding refuses them.`,
        });
    }
    if (largestFleet > policy.maxAgentsPerHuman) {
        caveats.push({
            code: 'fleet-detected',
            message: `One human presented ${largestFleet} agents against a cap of ${policy.maxAgentsPerHuman}. Counting requesters would have counted ${largestFleet}; counting humans counts one.`,
        });
    }
    caveats.push({
        code: 'fleet-bounded-only-within-one-registry',
        message: 'Agents are grouped by the identifier one registry hands out. The same person registering in a different registry, or holding a second identity there, is a different identifier and is not detectable here.',
    });
    return { policy, agents: all, humans, summary, caveats };
}
function bucket(groups, key, synthetic) {
    let g = groups.get(key);
    if (!g) {
        g = { synthetic, agents: [] };
        groups.set(key, g);
    }
    return g;
}
/**
 * Whichever order slots are handed out in has to be deterministic and stated, because it decides
 * which of a human's agents keeps working. `earliest-registered` ties break on the presented
 * order, and an agent with no registration block sorts last rather than first — an unknown age
 * must not outrank a known one.
 */
function admissionOrder(agents, mode, presented) {
    if (mode === 'as-presented')
        return [...agents];
    const pos = new Map(presented.map((a, i) => [a, i]));
    return [...agents].sort((a, b) => {
        const ab = a.registeredAtBlock ?? Number.POSITIVE_INFINITY;
        const bb = b.registeredAtBlock ?? Number.POSITIVE_INFINITY;
        if (ab !== bb)
            return ab - bb;
        return (pos.get(a.agent) ?? 0) - (pos.get(b.agent) ?? 0);
    });
}
const short = (id) => (id.length > 14 ? `${id.slice(0, 10)}…${id.slice(-4)}` : id);
const round = (n, dp) => Number(n.toFixed(dp));
/** Beyond this many candidate roots we stop enumerating subsets and fall back to greedy. */
const EXHAUSTIVE_ROOT_LIMIT = 20;
/**
 * The floor under one slot: the cheapest credentials an adversary can hold that clear this
 * policy, priced from the deployed registry.
 *
 * Two deliberate choices, both in the direction of understating our own security rather than
 * overstating it. Credentials are priced at **full freshness**, which for a survival ramp means
 * the adversary sources an aged registration rather than minting one — the cheapest case for
 * them. And costs are `min(forge, rent)` throughout, the same rule the scorer uses, because a
 * holder willing to rent defeats any amount of cryptography.
 *
 * Saturation is what makes the number meaningful: one credential per root, since a second
 * credential on a root already held adds nothing to the score. So the adversary's bill is a set
 * of *distinct* roots, which is exactly the quantity `minIndependentRoots` names.
 */
export function priceOfPolicy(input) {
    const list = Array.isArray(input.adapters)
        ? input.adapters
        : [...input.adapters.values()];
    const readable = input.readableAdapterIds ? new Set(input.readableAdapterIds) : undefined;
    const mustInclude = [...new Set(input.mustInclude ?? [])];
    // Cheapest readable adapter on each root, at full freshness. `effectiveCost` zeroes
    // discontinued protocols, so a dead adapter drops out here rather than being special-cased.
    const byRoot = new Map();
    for (const a of list) {
        if (readable && !readable.has(a.id))
            continue;
        const cost = effectiveCost(a, 1);
        if (cost <= 0)
            continue;
        const current = byRoot.get(a.trustRoot);
        if (!current || cost < current.costCents) {
            byRoot.set(a.trustRoot, { trustRoot: a.trustRoot, adapterId: a.id, costCents: cost });
        }
    }
    const candidates = [...byRoot.values()].sort((a, b) => a.costCents - b.costCents);
    // The scorer's own arithmetic, inverted: score = log10(totalCents + 1), and a root counts
    // toward independentRoots only once it carries at least NEGLIGIBLE_COST_CENTS.
    const targetCents = Math.max(0, 10 ** input.minScore - 1);
    const missing = mustInclude.filter((r) => !byRoot.has(r));
    if (missing.length) {
        return {
            cheapestSlotCents: 0,
            roots: [],
            feasible: false,
            candidates,
            reason: `no readable, live credential exists on required trust root(s) ${missing.join(', ')}, so this policy cannot be cleared by anyone — including an honest subject`,
        };
    }
    const forced = new Set(mustInclude);
    const free = candidates.filter((c) => !forced.has(c.trustRoot));
    const forcedRoots = candidates.filter((c) => forced.has(c.trustRoot));
    if (free.length > EXHAUSTIVE_ROOT_LIMIT) {
        const greedy = greedyClear(forcedRoots, free, targetCents, input.minIndependentRoots);
        return {
            ...greedy,
            candidates,
            approximate: true,
            reason: `${free.length} candidate roots is past the exhaustive limit of ${EXHAUSTIVE_ROOT_LIMIT}; this is a greedy upper bound on the cheapest slot, not the minimum`,
        };
    }
    let best;
    let bestCost = Number.POSITIVE_INFINITY;
    for (let mask = 0; mask < 1 << free.length; mask++) {
        const chosen = [...forcedRoots];
        let total = forcedRoots.reduce((s, r) => s + r.costCents, 0);
        for (let i = 0; i < free.length; i++) {
            if (mask & (1 << i)) {
                const r = free[i];
                chosen.push(r);
                total += r.costCents;
            }
        }
        if (total >= bestCost)
            continue;
        if (total < targetCents)
            continue;
        if (chosen.filter((r) => r.costCents >= NEGLIGIBLE_COST_CENTS).length < input.minIndependentRoots)
            continue;
        best = chosen;
        bestCost = total;
    }
    if (!best) {
        return {
            cheapestSlotCents: 0,
            roots: [],
            feasible: false,
            candidates,
            reason: `no combination of the ${candidates.length} readable trust roots reaches score ${input.minScore} across ${input.minIndependentRoots} roots — this policy denies everybody`,
        };
    }
    best.sort((a, b) => b.costCents - a.costCents);
    return {
        cheapestSlotCents: bestCost,
        roots: best,
        feasible: true,
        candidates,
        reason: `cheapest readable credential set clearing score ${input.minScore} across ${input.minIndependentRoots} independent root(s), priced at min(forge, rent) and full freshness`,
    };
}
/** Fallback for an ontology too wide to enumerate: cheapest-first until both limits are met. */
function greedyClear(forced, free, targetCents, minRoots) {
    const chosen = [...forced];
    let total = forced.reduce((s, r) => s + r.costCents, 0);
    for (const r of free) {
        if (total >= targetCents && chosen.filter((c) => c.costCents >= NEGLIGIBLE_COST_CENTS).length >= minRoots)
            break;
        chosen.push(r);
        total += r.costCents;
    }
    const ok = total >= targetCents && chosen.filter((c) => c.costCents >= NEGLIGIBLE_COST_CENTS).length >= minRoots;
    return {
        cheapestSlotCents: ok ? total : 0,
        roots: ok ? chosen.sort((a, b) => b.costCents - a.costCents) : [],
        feasible: ok,
    };
}
/**
 * What it costs an adversary to hold `slots` agent slots under this policy.
 *
 * The composition is the point of a per-human cap: slots come in batches of
 * `maxAgentsPerHuman`, and each batch needs a whole new human, credentials and all. Without the
 * cap the marginal agent costs a keypair.
 */
export function costOfSlots(price, policy, slots) {
    const humansRequired = Math.ceil(Math.max(0, slots) / Math.max(1, policy.maxAgentsPerHuman));
    const totalCents = humansRequired * price.cheapestSlotCents;
    return {
        slots,
        humansRequired,
        totalCents,
        marginalCentsPerAgent: slots === 0 ? 0 : round(totalCents / slots, 2),
    };
}
