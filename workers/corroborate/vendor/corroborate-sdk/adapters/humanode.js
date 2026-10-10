import { createPublicClient, defineChain, http, keccak256, toHex } from 'viem';
/**
 * Humanode bioauth, read from the Humanode chain itself.
 *
 * ## What the chain is, and what the credential is
 *
 * Humanode (chain id 5234, a Substrate chain with a Frontier EVM layer) admits validators by
 * 3D face liveness: a human FaceTec-scans against the "robonode", receives a signed auth
 * ticket, and submits it on chain, which puts their validator public key into
 * `Bioauth.ActiveAuthentications` for **exactly seven days**
 * (`AUTHENTICATIONS_EXPIRE_AFTER = 7 * TIMESTAMP_DAY` in the runtime's `constants.rs`;
 * confirmed against live state on 2026-07-25 — every one of the 82 active authentications
 * expired within 6.96 days of head). One face, one active key, enforced by the biometric
 * dedup at enrollment. So "held" here is a **liveness statement with a seven-day shelf
 * life** — the strongest freshness guarantee of any credential in this directory, and the
 * reason the age curve is Decay with a half-life measured in days, not years.
 *
 * ## How an EVM address comes into it
 *
 * Bioauth state is keyed by 32-byte native accounts, not EVM addresses. The bridge is
 * `pallet-evm-accounts-mapping`: a **permanent, one-to-one, dual-consent** binding — the
 * native account signs the `claim_account` extrinsic *and* the Ethereum key signs an EIP-712
 * claim naming that native account (`pallet-evm-accounts-mapping/src/lib.rs`, verified
 * 2026-07-25). Unlike a Lens account, this credential cannot be planted on an address.
 *
 * The runtime exposes both halves of the read to the EVM as precompiles
 * (`frontier_precompiles.rs`):
 *
 *  - `0x…0801` (EvmAccountsMapping): input is the raw 20-byte address — **not** ABI-encoded —
 *    output is the raw 32-byte native account, or empty when unmapped.
 *  - `0x…0800` (Bioauth): `isAuthenticated(bytes32)` over the native account; the precompile
 *    runs the same membership test against `ActiveAuthentications` the consensus layer uses.
 *
 * Both verified live on 2026-07-25: an active validator key from state read `true`, the zero
 * key `false`, and an unmapped address returned empty output.
 *
 * ## Dating
 *
 * The precompile answers held but not when. The date comes from the same endpoint's
 * Substrate side: `state_getStorage` of `Bioauth.ActiveAuthentications` (storage key derived
 * by twox128 below, pinned to the observed literal in the unit suite) yields each active
 * authentication's `expires_at` in unix milliseconds, and because every authentication lives
 * exactly `AUTHENTICATIONS_EXPIRE_AFTER`, `issuedAt = expiresAt − 7 days` — exact, not a
 * bound. The live suite re-verifies the seven-day constant every run by asserting no expiry
 * sits further than seven days from head; a runtime upgrade that changed it would fail the
 * suite rather than silently mis-date.
 *
 * ## The honest catch: the credential has no holders yet
 *
 * On 2026-07-25 the `EvmAccountsMapping.Accounts` map contained **zero entries** — nobody on
 * Humanode mainnet has ever claimed an EVM↔native binding, while 82 validators were actively
 * bioauthenticated. The read path is real, verified end to end, and entirely permissionless;
 * the population that can currently answer `held: true` is empty. This adapter exists so
 * that a Humanode human who *does* claim their mapping is recognized, and it will honestly
 * report `held: false, detail.mapped: false` for everyone until then. See
 * `research/protocols/humanode-onchain-read.md` for the measurements.
 *
 * The only public RPC is Humanode's own (`explorer-rpc-http.mainnet.stages.humanode.io`) —
 * permissionless (no key, nothing to revoke per-caller) but not infrastructure-independent,
 * the same position as the Lens Chain read, and the file says so rather than implying
 * otherwise.
 */
/** Humanode mainnet EVM chain id, `eth_chainId` 0x1472, verified 2026-07-25. */
export const HUMANODE_CHAIN_ID = 5234;
/**
 * Humanode's own public endpoint — HTTP JSON-RPC serving both the `eth_*` and the Substrate
 * (`state_*`) namespaces, which this adapter needs both of. No independent third-party
 * endpoint for chain 5234 was found on 2026-07-25.
 */
export const HUMANODE_RPCS = ['https://explorer-rpc-http.mainnet.stages.humanode.io'];
/** Bioauth precompile — `precompiles_constants::BIOAUTH = 2048`. */
export const HUMANODE_BIOAUTH_PRECOMPILE = '0x0000000000000000000000000000000000000800';
/** EvmAccountsMapping precompile — `precompiles_constants::EVM_ACCOUNTS_MAPPING = 2049`. */
export const HUMANODE_MAPPING_PRECOMPILE = '0x0000000000000000000000000000000000000801';
/** `keccak256("isAuthenticated(bytes32)")[0..4]`, the Bioauth precompile's one selector. */
export const IS_AUTHENTICATED_SELECTOR = keccak256(toHex('isAuthenticated(bytes32)')).slice(0, 10);
/** The literal observed answering on chain, asserted equal to the derivation in the tests. */
export const IS_AUTHENTICATED_SELECTOR_OBSERVED = '0xe3c90bb9';
/**
 * `AuthenticationsExpireAfter`: 7 days in milliseconds, from the runtime's `constants.rs`
 * (`7 * TIMESTAMP_DAY`). An authentication's `issuedAt` is exactly `expires_at` minus this.
 * The live suite re-checks it against state every run (no active expiry may sit further than
 * this from head), so a runtime upgrade that changes the constant fails loudly.
 */
export const AUTHENTICATIONS_EXPIRE_AFTER_MS = 7 * 24 * 60 * 60 * 1000;
// ---------------------------------------------------------------- twox128
const P1 = 0x9e3779b185ebca87n;
const P2 = 0xc2b2ae3d27d4eb4fn;
const P3 = 0x165667b19e3779f9n;
const P4 = 0x85ebca77c2b2ae63n;
const P5 = 0x27d4eb2f165667c5n;
const M = 0xffffffffffffffffn;
const rotl = (x, r) => ((x << r) & M) | (x >> (64n - r));
/** xxHash64 — the hash under Substrate's twox family. Inputs here are pallet names; BigInt cost is irrelevant. */
export function xxhash64(data, seed) {
    const len = BigInt(data.length);
    const view = new DataView(data.buffer, data.byteOffset, data.byteLength);
    let i = 0;
    let h;
    if (data.length >= 32) {
        let v1 = (seed + P1 + P2) & M;
        let v2 = (seed + P2) & M;
        let v3 = seed & M;
        let v4 = (seed - P1) & M;
        for (; i + 32 <= data.length; i += 32) {
            v1 = (rotl((v1 + ((view.getBigUint64(i, true) * P2) & M)) & M, 31n) * P1) & M;
            v2 = (rotl((v2 + ((view.getBigUint64(i + 8, true) * P2) & M)) & M, 31n) * P1) & M;
            v3 = (rotl((v3 + ((view.getBigUint64(i + 16, true) * P2) & M)) & M, 31n) * P1) & M;
            v4 = (rotl((v4 + ((view.getBigUint64(i + 24, true) * P2) & M)) & M, 31n) * P1) & M;
        }
        h = (rotl(v1, 1n) + rotl(v2, 7n) + rotl(v3, 12n) + rotl(v4, 18n)) & M;
        for (const v of [v1, v2, v3, v4]) {
            h = ((h ^ ((rotl((v * P2) & M, 31n) * P1) & M)) * P1 + P4) & M;
        }
    }
    else {
        h = (seed + P5) & M;
    }
    h = (h + len) & M;
    for (; i + 8 <= data.length; i += 8) {
        h = ((rotl(h ^ ((rotl((view.getBigUint64(i, true) * P2) & M, 31n) * P1) & M), 27n) * P1) + P4) & M;
    }
    for (; i + 4 <= data.length; i += 4) {
        h = ((rotl(h ^ ((BigInt(view.getUint32(i, true)) * P1) & M), 23n) * P2) + P3) & M;
    }
    for (; i < data.length; i++) {
        h = (rotl(h ^ ((BigInt(data[i]) * P5) & M), 11n) * P1) & M;
    }
    h = ((h ^ (h >> 33n)) * P2) & M;
    h = ((h ^ (h >> 29n)) * P3) & M;
    return h ^ (h >> 32n);
}
const le64 = (x) => {
    let s = '';
    for (let i = 0n; i < 8n; i++)
        s += ((x >> (8n * i)) & 0xffn).toString(16).padStart(2, '0');
    return s;
};
/** Substrate's twox128: xxh64(data, 0) ‖ xxh64(data, 1), each little-endian. */
export function twox128(name) {
    const bytes = new TextEncoder().encode(name);
    return le64(xxhash64(bytes, 0n)) + le64(xxhash64(bytes, 1n));
}
/** `twox128("Bioauth") ++ twox128("ActiveAuthentications")` — a StorageValue, so no key suffix. */
export const ACTIVE_AUTHENTICATIONS_KEY = `0x${twox128('Bioauth')}${twox128('ActiveAuthentications')}`;
/** The key observed serving live state on 2026-07-25, pinning the derivation above. */
export const ACTIVE_AUTHENTICATIONS_KEY_OBSERVED = '0x781b3ecf87d00064b2b25c4e058902f160519cb84486cfd81726674390a14b74';
/** Prefix of the `EvmAccountsMapping.Accounts` map — used by the live suite to enumerate holders. */
export const EVM_ACCOUNTS_PREFIX = `0x${twox128('EvmAccountsMapping')}${twox128('Accounts')}`;
/**
 * Decode the SCALE `Vec<Authentication<AccountId32, u64>>` payload: a compact length prefix,
 * then fixed 40-byte entries of 32-byte public key + little-endian u64 expiry. Pure, and
 * throws on malformed input — the probe's never-throw wrapper turns that into an error
 * result rather than a fabricated answer.
 */
export function decodeActiveAuthentications(hex) {
    const b = hexToBytes(hex);
    if (b.length === 0)
        return [];
    const [count, offset] = decodeCompact(b);
    const need = offset + count * 40;
    if (b.length < need) {
        throw new Error(`ActiveAuthentications truncated: ${b.length} bytes, need ${need} for ${count} entries`);
    }
    const out = [];
    for (let i = 0; i < count; i++) {
        const at = offset + i * 40;
        let exp = 0n;
        for (let j = 7; j >= 0; j--)
            exp = (exp << 8n) | BigInt(b[at + 32 + j]);
        out.push({
            publicKey: `0x${Array.from(b.slice(at, at + 32), (x) => x.toString(16).padStart(2, '0')).join('')}`,
            expiresAtMs: Number(exp),
        });
    }
    return out;
}
function hexToBytes(hex) {
    const clean = hex.startsWith('0x') ? hex.slice(2) : hex;
    const out = new Uint8Array(clean.length / 2);
    for (let i = 0; i < out.length; i++)
        out[i] = parseInt(clean.slice(i * 2, i * 2 + 2), 16);
    return out;
}
/** SCALE compact<u32> — the vector length. Returns [value, bytes consumed]. */
function decodeCompact(b) {
    const mode = b[0] & 3;
    if (mode === 0)
        return [b[0] >> 2, 1];
    if (mode === 1)
        return [(b[0] | (b[1] << 8)) >> 2, 2];
    if (mode === 2)
        return [((b[0] | (b[1] << 8) | (b[2] << 16) | (b[3] << 24)) >>> 2) >>> 0, 4];
    throw new Error('ActiveAuthentications length in big-integer compact mode: not a plausible validator count');
}
const humanodeChain = defineChain({
    id: HUMANODE_CHAIN_ID,
    name: 'Humanode',
    nativeCurrency: { name: 'eHMND', symbol: 'eHMND', decimals: 18 },
    rpcUrls: { default: { http: [...HUMANODE_RPCS] } },
});
/**
 * Held: two `eth_call`s against the runtime's own precompiles — address → native account
 * (`0x…0801`, raw in/out), then `isAuthenticated(nativeAccount)` (`0x…0800`), the exact
 * membership test the consensus layer applies. Date: one `state_getStorage` of
 * `Bioauth.ActiveAuthentications`, whose `expires_at` minus the seven-day authentication
 * lifetime is the authentication's issuance, exactly. Never throws; a dead endpoint is an
 * error result, not a negative.
 */
export function humanodeAdapter(opts = {}) {
    const rpcUrls = opts.rpcUrls ?? HUMANODE_RPCS;
    const timeout = opts.timeoutMs ?? 15_000;
    const clients = rpcUrls.map((url) => createPublicClient({
        chain: humanodeChain,
        transport: http(url, { timeout, retryCount: 0 }),
    }));
    async function tryEach(what, fn) {
        const errors = [];
        for (const [i, client] of clients.entries()) {
            try {
                return await fn(client);
            }
            catch (e) {
                errors.push(`${rpcUrls[i]}: ${(e instanceof Error ? e.message : String(e)).split('\n')[0]}`);
            }
        }
        throw new Error(`${what} unreadable — ${errors.join('; ')}`);
    }
    return {
        adapterId: 'humanode',
        probe: async (subject) => {
            try {
                const now = Math.floor(Date.now() / 1000);
                const [headBlock, mappingOut] = await Promise.all([
                    tryEach('Humanode head', (c) => c.getBlockNumber()).then(Number),
                    tryEach('EvmAccountsMapping precompile', (c) => 
                    // The precompile reads its input raw: exactly the 20 address bytes, no ABI
                    // envelope. Empty output means no mapping was ever claimed for this address.
                    c.call({ to: HUMANODE_MAPPING_PRECOMPILE, data: subject })),
                ]);
                const provenance = (dateFrom) => ({
                    heldFrom: 'chain',
                    dateFrom,
                    headBlock,
                    notes: [],
                });
                const native = mappingOut.data;
                if (native === undefined || native === '0x') {
                    // No claimed EVM↔native binding. As of 2026-07-25 this is every address on the
                    // chain — the mapping pallet held zero entries — and the detail says which side
                    // of the two-step read was absent.
                    return { held: false, provenance: provenance('none'), detail: { mapped: false } };
                }
                if (native.length !== 66) {
                    throw new Error(`mapping precompile returned ${(native.length - 2) / 2} bytes; expected a 32-byte account`);
                }
                const authOut = await tryEach('Bioauth precompile', (c) => c.call({
                    to: HUMANODE_BIOAUTH_PRECOMPILE,
                    data: (IS_AUTHENTICATED_SELECTOR + native.slice(2)),
                }));
                const isAuthenticated = authOut.data !== undefined && BigInt(authOut.data) === 1n;
                if (!isAuthenticated) {
                    // A mapped human whose weekly bioauth has lapsed (or who deauthenticated). The
                    // mapping is permanent; the liveness is what expired.
                    return {
                        held: false,
                        provenance: provenance('none'),
                        detail: { mapped: true, nativeAccount: native, bioauthActive: false },
                    };
                }
                // Dating: the same endpoint's Substrate side. Best-effort — losing it loses the
                // date, never the credential.
                let expiresAtMs;
                let activeCount;
                try {
                    const raw = await tryEach('Bioauth.ActiveAuthentications', (c) => c.request({
                        method: 'state_getStorage',
                        params: [ACTIVE_AUTHENTICATIONS_KEY],
                    }));
                    if (typeof raw === 'string') {
                        const auths = decodeActiveAuthentications(raw);
                        activeCount = auths.length;
                        expiresAtMs = auths.find((a) => a.publicKey === native.toLowerCase())?.expiresAtMs;
                    }
                }
                catch {
                    // The eth_ side already decided held; the state read only dates it.
                }
                const issuedAt = expiresAtMs !== undefined
                    ? Math.min(Math.floor((expiresAtMs - AUTHENTICATIONS_EXPIRE_AFTER_MS) / 1000), now)
                    : undefined;
                return {
                    held: true,
                    ...(issuedAt !== undefined ? { issuedAt } : {}),
                    provenance: provenance(issuedAt !== undefined ? 'chain' : 'none'),
                    detail: {
                        mapped: true,
                        nativeAccount: native,
                        bioauthActive: true,
                        ...(expiresAtMs !== undefined ? { expiresAt: Math.floor(expiresAtMs / 1000) } : {}),
                        ...(activeCount !== undefined ? { activeAuthentications: activeCount } : {}),
                        ...(expiresAtMs === undefined
                            ? { undated: 'held via precompile; state read for the expiry did not answer' }
                            : {}),
                    },
                };
            }
            catch (e) {
                return { held: false, error: e instanceof Error ? e.message : String(e) };
            }
        },
    };
}
