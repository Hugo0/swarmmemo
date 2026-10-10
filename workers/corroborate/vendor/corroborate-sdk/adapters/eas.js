import { createPublicClient, decodeAbiParameters, encodePacked, http, keccak256, parseAbi, parseAbiItem, parseAbiParameters, toHex, } from 'viem';
import { arbitrum, base, optimism } from 'viem/chains';
import { STAMP_TO_ADAPTER } from "./human-passport.js";
/**
 * EAS-based adapters, read **entirely from the chain**.
 *
 * The Coinbase probe in `index.ts` reads `base.easscan.org/graphql` — the hosted EASSCAN
 * indexer, a single-org, un-SLA'd service that returned a maintenance page mid-query during
 * our own research (`research/protocols/eas-and-disco.md`). That is precisely the vendor
 * dependency the rule at the top of `adapters/index.ts` forbids on the critical path. Both
 * adapters here answer from raw RPC against the EAS contracts themselves: `eth_call` for
 * state, `eth_getLogs` for discovery, nothing that can rate-limit or revoke us.
 *
 * The event every EAS deployment emits on attest is
 *
 *     event Attested(address indexed recipient, address indexed attester,
 *                    bytes32 uid, bytes32 indexed schemaUID)
 *
 * so `topics[1]` is the recipient, `topics[2]` the attester, `topics[3]` the schema and the
 * un-indexed `uid` is the 32-byte `data` payload. That layout is not assumed from the ABI —
 * this codebase has been burned by assumed topic layouts before — it was confirmed against a
 * live log on Base (block 49,105,239, uid `0x88a10ab4…c8a8b9`, recipient in `topics[1]`,
 * the Coinbase attester `0x3574…D7EE` in `topics[2]`, 2026-07-25), and the live suite
 * re-confirms it every run.
 */
// ------------------------------------------------------------------ shared
/** `keccak256("Attested(address,address,bytes32,bytes32)")`, the topic0 every EAS emits. */
export const ATTESTED_TOPIC = keccak256(toHex('Attested(address,address,bytes32,bytes32)'));
/**
 * The literal observed on chain, asserted equal to the derivation above in the unit suite so
 * a typo in the signature string cannot silently filter every log out of existence.
 */
export const ATTESTED_TOPIC_OBSERVED = '0x8bf46bf4cfd674fa735a3d63ec1c9ad4153f033c290341f3a588b75685141b35';
export const ATTESTED_EVENT = parseAbiItem('event Attested(address indexed recipient, address indexed attester, bytes32 uid, bytes32 indexed schemaUID)');
/** `Attestation` struct as returned by `EAS.getAttestation` — `Common.sol`, stable across versions. */
export const EAS_ABI = parseAbi([
    'struct Attestation { bytes32 uid; bytes32 schema; uint64 time; uint64 expirationTime; uint64 revocationTime; bytes32 refUID; address recipient; address attester; bool revocable; bytes data; }',
    'function getAttestation(bytes32 uid) view returns (Attestation)',
]);
const ZERO_UID = `0x${'0'.repeat(64)}`;
/**
 * Schema UIDs are content-addressed: `keccak256(abi.encodePacked(schema, resolver, revocable))`
 * (`SchemaRegistry.sol#_getUID`). This derivation is why the Passport adapter below never
 * hard-codes a per-chain schema UID — the resolver address is read from the Decoder and the
 * UID recomputed, so a value that could drift is replaced by arithmetic that cannot. The unit
 * suite pins the derivation to two UIDs observed in real on-chain schema records.
 */
export function deriveSchemaUID(schema, resolver, revocable) {
    return keccak256(encodePacked(['string', 'address', 'bool'], [schema, resolver, revocable]));
}
async function safe(fn) {
    try {
        return await fn();
    }
    catch (e) {
        return { held: false, error: e instanceof Error ? e.message : String(e) };
    }
}
// ---------------------------------------- Coinbase Verified Account, on-chain
/** `bool verifiedAccount` on Base — `github.com/coinbase/verifications`, registered 2023-10-23. */
export const COINBASE_VERIFIED_ACCOUNT_SCHEMA = '0xf8b05c79f090979bf4a80270aba232dff11a10d9ca55c4f88de95317970f0de9';
/** EAS predeploy, identical on every OP-Stack chain. */
export const BASE_EAS = '0x4200000000000000000000000000000000000021';
/**
 * The only address allowed to attest under the schema — the resolver enforces it, and we
 * enforce it again: schema UIDs are content-derived and unowned, so filtering on the schema
 * alone is the classic EAS spoof (`eas-and-disco.md`, failure mode #1). Every log query below
 * pins `topics[2]` to this address and every struct read re-checks `attester`.
 */
export const COINBASE_ATTESTER = '0x357458739F90461b99789350868CD7CF330Dd7EE';
/**
 * Coinbase's own on-chain indexer: `getAttestationUid(recipient, schemaUID) -> latest uid`,
 * written by their resolver in the same transaction as every attest. It exists precisely
 * because the core EAS contract has no address-keyed view, and it is what their own
 * `AttestationVerifier.sol` consults.
 */
export const COINBASE_INDEXER = '0x2c7eE1E5f416dfF40054c27A62f7B357C4E8619C';
const COINBASE_INDEXER_ABI = parseAbi([
    'function getAttestationUid(address recipient, bytes32 schemaUid) view returns (bytes32)',
]);
/**
 * The Coinbase Verified Account schema was registered on 2023-10-23; Base was around
 * ~5.6M blocks by then (2s blocks from the 2023-06-15 genesis). No attestation under the
 * schema can precede its registration, so a scan that reaches this block has seen history.
 */
export const COINBASE_SCHEMA_GO_LIVE_BLOCK = 5000000n;
/**
 * Keyless public endpoints, in `eth_call` preference order. All three were verified on
 * 2026-07-25 to serve `eth_call` and recipient-filtered `eth_getLogs` on the EAS predeploy
 * with no key. `base-rpc.publicnode.com` (used elsewhere in this package) is deliberately
 * absent: it now refuses `eth_getLogs` without a personal token.
 */
export const BASE_EAS_ENDPOINTS = [
    { url: 'https://mainnet.base.org', maxLogRange: 10000n },
    { url: 'https://base.gateway.tenderly.co', maxLogRange: 1000000n },
    { url: 'https://base.drpc.org', maxLogRange: 10000n },
];
/**
 * The decision, as a pure function of the struct, so every branch is unit-testable. Presence
 * is *not* the answer: 406k of 720k of these have been revoked (56.4%, measured 2026-07-24),
 * so an unrevoked read is the minority case and `revocationTime` does the real work.
 */
export function interpretCoinbaseAttestation(att, nowSeconds) {
    const uid = att.uid;
    if (uid === ZERO_UID)
        return { held: false, detail: { attested: false } };
    if (att.schema !== COINBASE_VERIFIED_ACCOUNT_SCHEMA) {
        return { held: false, detail: { attested: false, reason: 'schema-mismatch', uid, schema: att.schema } };
    }
    if (att.attester.toLowerCase() !== COINBASE_ATTESTER.toLowerCase()) {
        // A schema UID is not namespaced by its creator; anyone can emit under it if the resolver
        // ever allowed them to. An attestation from anyone but Coinbase is not a Coinbase claim.
        return { held: false, detail: { attested: false, reason: 'attester-mismatch', uid, attester: att.attester } };
    }
    const issuedAt = Number(att.time);
    if (att.revocationTime !== 0n) {
        return {
            held: false,
            detail: { attested: true, revoked: true, revokedAt: Number(att.revocationTime), uid, issuedAt },
        };
    }
    if (att.expirationTime !== 0n && Number(att.expirationTime) <= nowSeconds) {
        // Coinbase issues with expirationTime = 0 today; checked anyway so a policy change on
        // their side degrades to "expired", not to "still verified".
        return {
            held: false,
            detail: { attested: true, expired: true, expiredAt: Number(att.expirationTime), uid, issuedAt },
        };
    }
    return { held: true, issuedAt, detail: { uid, attester: att.attester, revoked: false } };
}
/**
 * Coinbase Verified Account, read from Base with no indexer service anywhere.
 *
 * Same credential and semantics as `coinbaseVerificationAdapter` in `index.ts` — Persona-
 * rooted KYC-account linkage, revocation-checked — but the answer comes from the chain:
 *
 * 1. **Primary: two `eth_call`s.** Coinbase's on-chain `AttestationIndexer` maps
 *    `(recipient, schemaUID) -> latest uid`; `EAS.getAttestation(uid)` then carries issuance,
 *    revocation and expiry. Complete history, no log scan, ~2 RPC round-trips per probe.
 * 2. **Cross-check on absence.** A zero uid from the indexer is confirmed against recent
 *    `Attested` logs (recipient, attester and schema all pinned in the topics) before being
 *    believed, so a re-pointed or abandoned indexer contract fails loudly instead of quietly
 *    reporting everyone unverified.
 * 3. **Fallback: chunked `eth_getLogs`.** If the indexer read fails entirely, the probe scans
 *    backward from head in windows sized to what each endpoint actually serves (measured, see
 *    `BASE_EAS_ENDPOINTS`) within a call budget. A full-history scan through 10k-block
 *    windows would be ~4,400 calls — not a sane per-probe cost — so the scan is bounded, and
 *    when it finds nothing the result says exactly which blocks were searched
 *    (`detail.scannedFromBlock`/`scannedToBlock`, `scanComplete: false`) rather than
 *    asserting an absence over history it never read.
 */
export function coinbaseVerificationOnchainAdapter(opts = {}) {
    const endpoints = opts.endpoints ?? BASE_EAS_ENDPOINTS;
    const timeout = opts.timeoutMs ?? 15_000;
    const logScanBlocks = opts.logScanBlocks ?? 8000000n;
    const maxLogCalls = opts.maxLogCalls ?? 12;
    const clients = endpoints.map((e) => ({
        ...e,
        client: createPublicClient({ chain: base, transport: http(e.url, { timeout }) }),
    }));
    /** Endpoints in log-scan order: widest window first, so the budget buys the most history. */
    const byRange = [...clients].sort((a, b) => (a.maxLogRange > b.maxLogRange ? -1 : 1));
    const firstAnswer = async (fn) => {
        let lastError;
        for (const e of clients) {
            try {
                return await fn(e.client);
            }
            catch (err) {
                lastError = err;
            }
        }
        throw lastError instanceof Error ? lastError : new Error(String(lastError));
    };
    const getAttestation = (uid) => firstAnswer((c) => c.readContract({
        address: BASE_EAS,
        abi: EAS_ABI,
        functionName: 'getAttestation',
        args: [uid],
    }));
    /**
     * Backward chunked scan for the newest `Attested(recipient=subject, attester=Coinbase,
     * schemaUID=schema)` log. Newest-first because revoke-and-reissue churn means only the
     * latest attestation reflects current status, and because it lets the common case (a
     * recently active credential) stop after one window.
     */
    const scanLogs = async (subject, budget) => {
        let lastError;
        for (const e of byRange) {
            try {
                const head = await e.client.getBlockNumber();
                const floor = head > logScanBlocks + COINBASE_SCHEMA_GO_LIVE_BLOCK
                    ? head - logScanBlocks
                    : COINBASE_SCHEMA_GO_LIVE_BLOCK;
                let to = head;
                let calls = 0;
                while (to >= floor && calls < budget) {
                    const from = to > e.maxLogRange + floor ? to - e.maxLogRange : floor;
                    const logs = await e.client.getLogs({
                        address: BASE_EAS,
                        event: ATTESTED_EVENT,
                        args: { recipient: subject, attester: COINBASE_ATTESTER, schemaUID: COINBASE_VERIFIED_ACCOUNT_SCHEMA },
                        fromBlock: from,
                        toBlock: to,
                    });
                    calls += 1;
                    if (logs.length > 0) {
                        const newest = logs[logs.length - 1];
                        return {
                            uid: newest.args.uid,
                            foundAtBlock: newest.blockNumber ?? undefined,
                            scannedFromBlock: from,
                            scannedToBlock: head,
                            complete: from <= COINBASE_SCHEMA_GO_LIVE_BLOCK,
                        };
                    }
                    if (from === floor) {
                        return {
                            scannedFromBlock: from,
                            scannedToBlock: head,
                            complete: from <= COINBASE_SCHEMA_GO_LIVE_BLOCK,
                        };
                    }
                    to = from - 1n;
                }
                // Budget exhausted mid-scan: report honestly how far we got.
                return { scannedFromBlock: to + 1n, scannedToBlock: head, complete: false };
            }
            catch (err) {
                lastError = err;
            }
        }
        throw lastError instanceof Error ? lastError : new Error(String(lastError));
    };
    return {
        adapterId: 'coinbase-verification',
        probe: (subject) => safe(async () => {
            const now = Math.floor(Date.now() / 1000);
            let uid;
            let method = 'indexer';
            let indexerError;
            try {
                uid = (await firstAnswer((c) => c.readContract({
                    address: COINBASE_INDEXER,
                    abi: COINBASE_INDEXER_ABI,
                    functionName: 'getAttestationUid',
                    args: [subject, COINBASE_VERIFIED_ACCOUNT_SCHEMA],
                })));
            }
            catch (e) {
                indexerError = e instanceof Error ? e.message : String(e);
            }
            if (uid === undefined) {
                // Indexer unreachable: fall back to pure log discovery within the budget.
                const scan = await scanLogs(subject, maxLogCalls);
                if (!scan.uid) {
                    return {
                        held: false,
                        detail: {
                            attested: false,
                            method: 'logs',
                            indexerError,
                            scannedFromBlock: Number(scan.scannedFromBlock),
                            scannedToBlock: Number(scan.scannedToBlock),
                            scanComplete: scan.complete,
                            ...(scan.complete ? {} : { note: 'not found in scanned window; indexer was unreachable' }),
                        },
                    };
                }
                uid = scan.uid;
                method = 'logs';
            }
            else if (uid === ZERO_UID) {
                // The indexer says "never attested". Believe it only after the recent logs agree —
                // one extra getLogs in the common case, and the difference between "not verified"
                // and "we were reading a contract Coinbase stopped writing to".
                const scan = await scanLogs(subject, 1);
                if (!scan.uid) {
                    return {
                        held: false,
                        detail: {
                            attested: false,
                            method: 'indexer',
                            crossCheckedFromBlock: Number(scan.scannedFromBlock),
                            crossCheckedToBlock: Number(scan.scannedToBlock),
                        },
                    };
                }
                uid = scan.uid;
                method = 'logs';
            }
            const att = await getAttestation(uid);
            const verdict = interpretCoinbaseAttestation(att, now);
            return {
                held: verdict.held,
                ...(verdict.issuedAt !== undefined && verdict.held ? { issuedAt: verdict.issuedAt } : {}),
                detail: { ...verdict.detail, method },
            };
        }),
    };
}
// --------------------------------------- Human Passport score, from EAS itself
/**
 * The score-v2 schema string, exactly as registered. The UID is derived per chain from this
 * string plus that chain's resolver (see `deriveSchemaUID`); on Optimism the derivation
 * yields `0xda0257…7254`, which is the schema the attester is observably minting under today
 * (live logs, 2026-07-25).
 */
export const PASSPORT_SCORE_V2_SCHEMA = 'bool passing_score, uint8 score_decimals, uint128 scorer_id, uint32 score, uint32 threshold, tuple(string provider, uint256 score)[] stamps';
/** The original score schema — `0x6ab5d342…5e9c89` on Optimism — still held by older mints. */
export const PASSPORT_SCORE_LEGACY_SCHEMA = 'uint256 score,uint32 scorer_id,uint8 score_decimals';
const SCORE_V2_PARAMS = parseAbiParameters('bool passing_score, uint8 score_decimals, uint128 scorer_id, uint32 score, uint32 threshold, (string provider, uint256 score)[] stamps');
const SCORE_LEGACY_PARAMS = parseAbiParameters('uint256 score, uint32 scorer_id, uint8 score_decimals');
const PASSPORT_DECODER_ABI = parseAbi([
    'function gitcoinResolver() view returns (address)',
    'function maxScoreAge() view returns (uint64)',
]);
const PASSPORT_RESOLVER_ABI = parseAbi([
    'function getUserAttestation(address user, bytes32 schema) view returns (bytes32)',
    'function _gitcoinAttester() view returns (address)',
]);
/**
 * Chains where the score lives as an EAS attestation we know how to reach. Decoder addresses
 * are the documented deployments already pinned in `human-passport.ts`; EAS addresses are the
 * per-chain deployments from `eas-and-disco.md` (OP-Stack predeploy on Optimism, standalone
 * v0.26 deployment on Arbitrum). Optimism is where passport mints overwhelmingly live;
 * Arbitrum is the other chain Passport documents for score attestations. The other Decoder
 * chains stay with the resolver-cache read in `human-passport.ts`, which needs no EAS address
 * at all.
 */
export const PASSPORT_EAS_DEPLOYMENTS = {
    optimism: {
        chain: optimism,
        decoder: '0x5558D441779Eca04A329BcD6b47830D2C6607769',
        eas: '0x4200000000000000000000000000000000000021',
        // Head-only endpoint, per the note in human-passport.ts: keyless archive quota on
        // mainnet.optimism.io is spent by the Farcaster probe, and these are all head reads.
        rpc: 'https://optimism-rpc.publicnode.com',
    },
    arbitrum: {
        chain: arbitrum,
        decoder: '0x2050256A91cbABD7C42465aA0d5325115C1dEB43',
        eas: '0xbD75f629A22Dc1ceD33dDA0b68c546A1c035c458',
        rpc: 'https://arbitrum-one-rpc.publicnode.com',
    },
};
/** Decode a score-v2 payload. Exported pure so the fixture from a real mint pins it. */
export function decodeScoreV2(data) {
    const [passingScore, decimals, scorerId, score, threshold, stamps] = decodeAbiParameters(SCORE_V2_PARAMS, data);
    const unit = 10 ** Number(decimals);
    return {
        score: Number(score) / unit,
        scorerId: Number(scorerId),
        schema: 'score-v2',
        passingScore,
        threshold: Number(threshold) / unit,
        stamps: stamps.map((s) => ({ provider: s.provider, weight: Number(s.score) / unit })),
    };
}
export function decodeScoreLegacy(data) {
    const [score, scorerId, decimals] = decodeAbiParameters(SCORE_LEGACY_PARAMS, data);
    return {
        score: Number(score) / 10 ** Number(decimals),
        scorerId: Number(scorerId),
        schema: 'score-legacy',
    };
}
/**
 * Human Passport's score, read from the EAS attestation itself rather than the resolver's
 * cache.
 *
 * `human-passport.ts` reads `GitcoinResolver.getCachedScore`, which is that resolver's
 * summary of the attestation this adapter fetches whole. Reading the attestation buys three
 * things the cache cannot give: the **attester** (re-checked against the resolver's own
 * `_gitcoinAttester()`, so a resolver bug or migration cannot smuggle in someone else's
 * numbers), the **revocation state** as EAS records it, and — on score-v2 — the **stamps
 * inline**, so the disclosure of which trust roots the score restates no longer depends on
 * the separate `getPassport` call that reverts for community-scoped scores.
 *
 * Discovery needs no logs at all: the resolver's public `userAttestations` mapping
 * (`getUserAttestation(subject, schemaUID)`) is the address→uid index, on chain, populated
 * atomically by the same hook that validates every attest. Schema UIDs are *derived* — see
 * `deriveSchemaUID` — from the schema string and the resolver address the Decoder names, so
 * nothing here goes stale when Passport re-deploys a resolver.
 *
 * Everything the sibling adapter says about **meaning** stands unchanged: the scalar is
 * Passport's weighting, not ours; it is rooted at `behavioral:wallet-history` and the
 * high-weight stamps are priced under their own roots. This adapter restates the stamp list
 * from the attestation payload for exactly that decomposition.
 *
 * Per-probe cost, measured shape: 3 cached config `eth_call`s per chain per process
 * lifetime, then 2–3 `eth_call`s per chain per probe (uid lookup, optional legacy retry,
 * `getAttestation`). No `eth_getLogs` on any path.
 */
export function gitcoinPassportAdapter(opts = {}) {
    const chains = opts.chains ?? Object.keys(PASSPORT_EAS_DEPLOYMENTS);
    const timeout = opts.timeoutMs ?? 12_000;
    const clients = new Map();
    const clientFor = (chain) => {
        let c = clients.get(chain);
        if (!c) {
            const d = PASSPORT_EAS_DEPLOYMENTS[chain];
            c = createPublicClient({
                chain: d.chain,
                transport: http(opts.rpcUrls?.[chain] ?? d.rpc, { timeout }),
            });
            clients.set(chain, c);
        }
        return c;
    };
    // In-flight promise cached, rejected configs evicted — same reasoning as human-passport.ts.
    const configs = new Map();
    const configFor = (chain) => {
        let p = configs.get(chain);
        if (!p) {
            const c = clientFor(chain);
            const decoder = PASSPORT_EAS_DEPLOYMENTS[chain].decoder;
            p = (async () => {
                const [resolver, maxScoreAge] = await Promise.all([
                    c.readContract({ address: decoder, abi: PASSPORT_DECODER_ABI, functionName: 'gitcoinResolver' }),
                    c.readContract({ address: decoder, abi: PASSPORT_DECODER_ABI, functionName: 'maxScoreAge' }),
                ]);
                const attester = await c.readContract({
                    address: resolver,
                    abi: PASSPORT_RESOLVER_ABI,
                    functionName: '_gitcoinAttester',
                });
                return {
                    resolver: resolver,
                    attester: attester,
                    maxScoreAge: Number(maxScoreAge),
                    scoreV2UID: deriveSchemaUID(PASSPORT_SCORE_V2_SCHEMA, resolver, true),
                    legacyUID: deriveSchemaUID(PASSPORT_SCORE_LEGACY_SCHEMA, resolver, true),
                };
            })();
            p.catch(() => configs.delete(chain));
            configs.set(chain, p);
        }
        return p;
    };
    const readChain = async (chain, subject, now) => {
        const cfg = await configFor(chain);
        const c = clientFor(chain);
        const uidFor = (schemaUID) => c.readContract({
            address: cfg.resolver,
            abi: PASSPORT_RESOLVER_ABI,
            functionName: 'getUserAttestation',
            args: [subject, schemaUID],
        });
        let uid = await uidFor(cfg.scoreV2UID);
        if (uid === ZERO_UID)
            uid = await uidFor(cfg.legacyUID);
        if (uid === ZERO_UID)
            return null;
        const att = (await c.readContract({
            address: PASSPORT_EAS_DEPLOYMENTS[chain].eas,
            abi: EAS_ABI,
            functionName: 'getAttestation',
            args: [uid],
        }));
        if (att.attester.toLowerCase() !== cfg.attester.toLowerCase()) {
            // The resolver's attest hook rejects foreign attesters, so its index pointing at one
            // means our model of the deployment is wrong. That is a fault to surface, not a score.
            throw new Error(`${chain}: attestation ${uid} attester ${att.attester} != resolver's _gitcoinAttester ${cfg.attester}`);
        }
        const decoded = att.schema === cfg.legacyUID ? decodeScoreLegacy(att.data) : decodeScoreV2(att.data);
        const issuedAt = Number(att.time);
        // The attestation's own expiry when set; the Decoder's maxScoreAge rule otherwise —
        // the same derivation the sibling adapter verified against getScore's revert payload.
        const expiresAt = att.expirationTime !== 0n ? Number(att.expirationTime) : issuedAt + cfg.maxScoreAge;
        return {
            chain,
            uid,
            decoded,
            issuedAt,
            expiresAt,
            expired: now >= expiresAt,
            revoked: att.revocationTime !== 0n,
        };
    };
    return {
        adapterId: 'human-passport-eas',
        probe: (subject) => safe(async () => {
            const now = Math.floor(Date.now() / 1000);
            const settled = await Promise.all(chains.map(async (chain) => {
                try {
                    return { chain, reading: await readChain(chain, subject, now) };
                }
                catch (e) {
                    return { chain, error: e instanceof Error ? e.message : String(e) };
                }
            }));
            const failures = settled.filter((s) => 'error' in s);
            if (failures.length === chains.length) {
                return {
                    held: false,
                    error: `no Passport EAS deployment answered (${failures
                        .map((f) => `${f.chain}: ${f.error.split('\n')[0]}`)
                        .join('; ')})`,
                };
            }
            const readings = settled
                .map((s) => ('reading' in s ? s.reading : null))
                .filter((r) => r !== null);
            const perChain = Object.fromEntries(readings.map((r) => [
                r.chain,
                {
                    score: r.decoded.score,
                    issuedAt: r.issuedAt,
                    expiresAt: r.expiresAt,
                    expired: r.expired,
                    revoked: r.revoked,
                    schema: r.decoded.schema,
                },
            ]));
            const unreadable = failures.length ? { chainsUnreadable: failures.map((f) => f.chain) } : {};
            if (readings.length === 0) {
                return { held: false, detail: { attested: false, chainsRead: chains.length - failures.length, ...unreadable } };
            }
            // Same selection rule as the sibling adapter: freshest valid mint wins. Revoked and
            // expired attestations are real observations but not evidence; a score of zero is a
            // real passport with nothing in it.
            const valid = readings.filter((r) => !r.revoked && !r.expired && r.decoded.score > 0);
            if (valid.length === 0) {
                const newest = readings.reduce((a, b) => (b.issuedAt > a.issuedAt ? b : a));
                return {
                    held: false,
                    detail: {
                        attested: true,
                        reason: newest.revoked ? 'revoked' : newest.decoded.score === 0 ? 'score-zero' : 'score-expired',
                        perChain,
                        ...unreadable,
                    },
                };
            }
            const best = valid.reduce((a, b) => (b.issuedAt > a.issuedAt ? b : a));
            const stamps = best.decoded.stamps?.map((s) => s.provider);
            const restated = stamps
                ? [...new Set(stamps.map((s) => STAMP_TO_ADAPTER[s]).filter((a) => Boolean(a)))]
                : [];
            return {
                held: true,
                issuedAt: best.issuedAt,
                detail: {
                    attested: true,
                    score: best.decoded.score,
                    chain: best.chain,
                    uid: best.uid,
                    schema: best.decoded.schema,
                    expiresAt: best.expiresAt,
                    ...(best.decoded.passingScore !== undefined ? { meetsPassportThreshold: best.decoded.passingScore } : {}),
                    ...(best.decoded.threshold !== undefined ? { passportThreshold: best.decoded.threshold } : {}),
                    ...(best.decoded.stamps ? { stamps: best.decoded.stamps } : {}),
                    ...(restated.length ? { restatesAdapters: restated } : {}),
                    perChain,
                    ...unreadable,
                },
            };
        }),
    };
}
