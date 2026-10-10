/**
 * Idena — a validated identity on the proof-of-person chain, read from its node RPC.
 *
 * ## Why this is readable for an Ethereum-address subject at all
 *
 * Idena addresses are secp256k1, derived exactly like Ethereum's: last 20 bytes of
 * keccak256(uncompressed pubkey). An Idena identity at address X is therefore a claim about
 * the *same keypair* our subject controls — no cross-chain account mapping is needed, which
 * is what separates this adapter from the Encointer refusal
 * (`research/protocols/encointer-onchain-read.md`).
 *
 * ## What the credential attests
 *
 * A validated state (`Newbie`, `Verified` or `Human`) means this key's holder solved a set of
 * AI-hard "flip" puzzles at the last synchronous validation ceremony — every participant
 * worldwide solves at the same announced instant, so one human cannot validate two identities.
 * The state is *not* durable: miss one ceremony and a Verified/Human identity drops to
 * Suspended, a Newbie is killed. **Recency is the signal** — a validated state is at most one
 * epoch old by construction, which is why `issuedAt` here is the *last* ceremony, not the
 * identity's first validation (its total tenure is reported as `detail.ageEpochs` instead).
 *
 * The honest caveat this file must carry: the network is ~124 validated identities worldwide
 * (epoch 214, 2026-07), down 99.2% from its 2022 peak, and the protocol's own founder
 * co-authored the paper showing that at 20x this size the network had collapsed into
 * puppeteered pools. The ontology prices that; the probe just reports the state faithfully —
 * including `delegatee`, because a pooled identity is exactly the puppeteering shape the
 * paper documented.
 *
 * ## The read, and its honest infrastructure position
 *
 * Idena is its own chain with its own JSON-RPC dialect (`dna_identity`, `dna_epoch`,
 * `bcn_blockAt`). There is no fully keyless public node: every Idena node gates RPC behind an
 * `api-key`. What exists is the **protocol's shared node** at `restricted.idena.io`, whose key
 * (`idena-restricted-node-key`) is a *public constant* baked into the deployed app.idena.io
 * web-client bundle (verified in `_app-fb1caf55….js`, 2026-07-25) — every keyless user of the
 * official web app authenticates with it. So the read is permissionless in every sense that
 * matters (nothing to register, nothing revocable per-caller) but, like the Lens Chain read,
 * it is protocol-operated and not infrastructure-independent: the operator could rotate the
 * key or the node globally, and `rpc.idena.io` / `node.idena.io` were both dead when checked
 * (2026-07-25). The restricted node also allowlists methods — `dna_identities` (list all) is
 * refused with a plain-text 403 — but the three methods this probe needs all answer.
 *
 * `api.idena.io` (the protocol's indexer) is deliberately **not** on any `held` path; the
 * live test suite uses it for candidate discovery only.
 *
 * ## Dating
 *
 * A validated state was necessarily earned at the most recent ceremony, and the current
 * epoch's `startBlock` is the first block after that ceremony, so its own timestamp — read
 * with `bcn_blockAt` — is a proven chain-state date for the validation, minutes-accurate.
 * Verified against epoch 215: startBlock 11,066,316, timestamp 1,784,648,021 =
 * 2026-07-21T~15:33Z, the epoch-214 ceremony afternoon. If the two dating calls fail after a
 * successful identity read, the probe degrades loudly: `held: true` stands (it was proven),
 * the date is dropped, and `detail.undated` says why.
 */
/** The protocol's shared node — the same one the official web client uses. */
export const IDENA_RPC_URL = 'https://restricted.idena.io';
/**
 * The shared node's API key: a public constant shipped in the app.idena.io client bundle,
 * not a personal credential. Override both url and key to use your own node.
 */
export const IDENA_SHARED_NODE_KEY = 'idena-restricted-node-key';
/**
 * States that count as held. The full ladder is Undefined → Invite → Candidate → Newbie →
 * Verified → Human, with Suspended/Zombie for validated identities that missed ceremonies.
 * Newbie counts: it *did* pass the last synchronous ceremony, which is the uniqueness claim;
 * the ladder above it measures tenure, which `ageEpochs` reports. Suspended/Zombie do not:
 * their holder provably missed the most recent ceremony, and for a liveness-shaped credential
 * that is the signal going dark.
 */
export const IDENA_VALIDATED_STATES = ['Newbie', 'Verified', 'Human'];
/**
 * The held decision as a pure function of the identity record, so every state is unit-testable.
 * An all-zero `Undefined` record is what the node returns for an address it has never seen —
 * `dna_identity` never errors on absence, it fabricates an empty identity — so `Undefined`
 * maps to "no identity" rather than "an identity in a bad state".
 */
export function interpretIdenaIdentity(identity) {
    const state = identity.state;
    if (state === 'Undefined') {
        return { held: false, detail: { identityFound: false } };
    }
    const stakeIdna = Number.parseFloat(identity.stake);
    const base = {
        identityFound: true,
        state,
        ageEpochs: identity.age,
        stakeIdna: Number.isFinite(stakeIdna) ? stakeIdna : 0,
        online: identity.online,
        pooled: identity.delegatee !== null,
        ...(identity.delegatee !== null ? { delegatee: identity.delegatee } : {}),
    };
    if (!IDENA_VALIDATED_STATES.includes(state)) {
        // Candidate never validated; Suspended/Zombie missed the last ceremony; Killed/Invite are
        // terminal or pre-entry. All are real observations, none is a validated human.
        return { held: false, detail: { ...base, reason: 'state-not-validated' } };
    }
    return { held: true, detail: base };
}
export function idenaAdapter(opts = {}) {
    const rpcUrl = opts.rpcUrl ?? IDENA_RPC_URL;
    const nodeKey = opts.nodeKey ?? IDENA_SHARED_NODE_KEY;
    const timeoutMs = opts.timeoutMs ?? 15_000;
    const call = opts.call ??
        (async (method, params) => {
            // Idena's dialect: the key rides in the body, not a header. Failures are plain text
            // ("API key is invalid", "method not available"), both with HTTP 403 — so the non-JSON
            // path below is a real error path, not defensive decoration.
            const res = await fetch(rpcUrl, {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ method, params, id: 1, key: nodeKey }),
                signal: AbortSignal.timeout(timeoutMs),
            });
            const text = await res.text();
            let json;
            try {
                json = JSON.parse(text);
            }
            catch {
                throw new Error(`${rpcUrl}: ${method} — HTTP ${res.status}, non-JSON reply: ${text.slice(0, 120)}`);
            }
            if (json.error) {
                throw new Error(`${rpcUrl}: ${method} — ${json.error.message ?? JSON.stringify(json.error)}`);
            }
            if (!res.ok)
                throw new Error(`${rpcUrl}: ${method} — HTTP ${res.status}`);
            return json.result;
        });
    return {
        adapterId: 'idena',
        probe: async (subject) => {
            try {
                const identity = (await call('dna_identity', [subject]));
                if (typeof identity?.state !== 'string') {
                    throw new Error(`${rpcUrl}: dna_identity returned no identity state for ${subject}`);
                }
                const verdict = interpretIdenaIdentity(identity);
                if (!verdict.held) {
                    const provenance = { heldFrom: 'chain', dateFrom: 'none', notes: [] };
                    return { held: false, provenance, detail: verdict.detail };
                }
                // Dating: a validated state was earned at the last ceremony, and the current epoch's
                // startBlock is the first block after it, so that block's own timestamp dates the
                // validation. Both reads are best-effort — held is already proven.
                let issuedAt;
                let epochDetail = {};
                let undated;
                try {
                    const epoch = (await call('dna_epoch', []));
                    if (typeof epoch?.startBlock !== 'number')
                        throw new Error('dna_epoch returned no startBlock');
                    const block = (await call('bcn_blockAt', [epoch.startBlock]));
                    if (typeof block?.timestamp !== 'number') {
                        throw new Error(`bcn_blockAt(${epoch.startBlock}) returned no timestamp`);
                    }
                    issuedAt = block.timestamp;
                    epochDetail = {
                        epoch: epoch.epoch,
                        lastValidationAt: issuedAt,
                        nextValidation: epoch.nextValidation,
                    };
                }
                catch (e) {
                    undated = e instanceof Error ? e.message : String(e);
                }
                const provenance = {
                    heldFrom: 'chain',
                    dateFrom: issuedAt !== undefined ? 'chain' : 'none',
                    notes: [],
                };
                return {
                    held: true,
                    ...(issuedAt !== undefined ? { issuedAt } : {}),
                    provenance,
                    detail: {
                        ...verdict.detail,
                        ...epochDetail,
                        ...(undated !== undefined ? { undated: `validation date unreadable — ${undated}` } : {}),
                    },
                };
            }
            catch (e) {
                return { held: false, error: e instanceof Error ? e.message : String(e) };
            }
        },
    };
}
