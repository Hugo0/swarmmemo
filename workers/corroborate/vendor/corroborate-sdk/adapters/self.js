import { createPublicClient, decodeAbiParameters, http, keccak256, parseAbi, parseAbiParameters, toHex, } from 'viem';
import { celo } from 'viem/chains';
/**
 * Self Protocol (self.xyz), read from Celo with no vendor service anywhere.
 *
 * ## What exists on chain, and what deliberately does not
 *
 * Self's registration path writes an identity **commitment** into a merkle-tree registry
 * keyed by a per-document nullifier — nothing address-shaped. Addresses enter the picture
 * only at **disclosure**: a user proves predicates about their registered document to the
 * `IdentityVerificationHubV2` (`0xe57F…f5BF`, ERC-1967 proxy, upgradeable), which verifies the
 * Groth16 proof and calls back into the requesting integrator contract with a
 * `GenericDiscloseOutputV2` whose `userIdentifier` is, for on-chain integrations, the user's
 * wallet address. Two permissionless surfaces fall out of that, and this adapter reads both:
 *
 * 1. **Integrator registries** — contracts extending `SelfVerificationRoot` that store the
 *    verification keyed by address: soulbound tokens (`getTokenIdByAddress`), verifier
 *    mappings (`isVerified`/`verifiedAt`). The callback is hub-gated
 *    (`msg.sender == hub`, verified in Sourcify-matched source for every registry pinned
 *    below), so state in these contracts cannot exist without a proof having passed the
 *    canonical hub. `eth_call`, cheap, complete history for their own users.
 * 2. **The hub's own `DisclosureVerified` event** — emitted on every successful disclosure,
 *    whoever the integrator. The recipient (`userIdentifier`) is *not indexed*, so this path
 *    is a bounded backward log scan with client-side filtering, and a miss is honestly
 *    scoped to the scanned window rather than asserted over history.
 *
 * A full census on 2026-07-25 (hub deploy block 38,942,111, 2025-06-25, to head 73,104,000):
 * 6,212 `DisclosureVerified` events ever, split e-passport 3,032 / EU ID card 769 /
 * Aadhaar 1,636 / KYC 775 across ~15 integrator contracts. The registries pinned below
 * account for ~1,600 of those; the scan path covers the rest.
 *
 * ## What this is evidence of — and the document-type caveat
 *
 * `held: true` means: *someone completed a Self disclosure proof naming this address as the
 * recipient*. Behind that sits a registered government document — but not necessarily an
 * ICAO 9303 one: Self also registers Indian Aadhaar (attestation id 3) and KYC attestations
 * (id 4), which share neither the passport PKI nor its forgery cost. The hub-scan path knows
 * the document type (it is an indexed topic) and reports it in `detail.documentType`; the
 * registry path does not — an SBT does not record which document minted it — so a registry
 * positive is "state document, type unknown". The ontology prices this adapter under
 * `state-document:icao-9303`; the Aadhaar/KYC admixture is a known dilution documented in
 * `research/protocols/self-onchain-read.md` rather than something this probe can separate.
 *
 * Also inherited from the protocol, not fixable here: passive authentication only (a chip
 * *dump* suffices — no liveness against the physical document), per-document nullifiers
 * (renewal or a second passport mints a fresh identity), and disclosure requiring the
 * subject's own EIP-712 signature in the registries below — so unlike a Lens account, an SBT
 * cannot be planted on an address without its key.
 *
 * ## Dating
 *
 * Every date this adapter can reach is a **re-attestation** date: SBT expiries are
 * `lastVerification + validityPeriod` and renewals overwrite them; verifier mappings are
 * overwritten per proof; the newest scan hit may have older siblings outside the window. All
 * dates therefore carry `date-from-latest-reattestation` — under this adapter's `Decay`
 * curve a ceiling on age, which errs against the credential, never for it.
 */
// ------------------------------------------------------------------ hub
/** IdentityVerificationHubV2 proxy on Celo mainnet — the only place disclosures verify. */
export const SELF_HUB_V2 = '0xe57F4773bd9c9d8b6Cd70431117d353298B9f5BF';
/** The hub proxy's deployment block (binary-searched `eth_getCode`, 2026-07-25 = 2025-06-25). */
export const SELF_HUB_V2_DEPLOY_BLOCK = 38942111n;
/**
 * `keccak256` of the DisclosureVerified signature from `IdentityVerificationHubImplV2.sol`.
 * Indexed: requestor (the integrator), contractVersion, attestationId. The recipient is in
 * `data`, un-indexed — the reason the scan below filters client-side.
 */
export const DISCLOSURE_VERIFIED_TOPIC = keccak256(toHex('DisclosureVerified(address,uint8,bytes32,uint256,bytes32,uint256,bytes,bytes)'));
/**
 * The literal observed on live hub logs (e.g. Celo tx `0xac44608a…6f20`, block 73,082,826,
 * 2026-07-25), asserted equal to the derivation in the unit suite so a typo in the signature
 * string cannot silently filter every disclosure out of existence.
 */
export const DISCLOSURE_VERIFIED_TOPIC_OBSERVED = '0x14b70ae0a2b984327e9bcd235341661b8f8e6f4bb6d93a2c09707ca9d890cba2';
/** `AttestationId.sol` (selfxyz/self, read 2026-07-25). Only 1 and 2 are ICAO 9303 documents. */
export const SELF_DOCUMENT_TYPES = {
    1: 'e-passport',
    2: 'eu-id-card',
    3: 'aadhaar',
    4: 'kyc',
};
/** Keyless public Celo endpoints, in `eth_call` preference order. All verified 2026-07-25. */
export const CELO_ENDPOINTS = [
    { url: 'https://forno.celo.org', maxLogRange: 4999n },
    { url: 'https://celo.gateway.tenderly.co', maxLogRange: 999999n },
    { url: 'https://celo.drpc.org', maxLogRange: 9999n },
];
/**
 * Integrator contracts with **address-keyed public state**, all Sourcify-verified
 * (creation + runtime match) and all observed as `DisclosureVerified` requestors in the
 * full-history census of 2026-07-25 (event counts in parentheses). Their only write path to
 * verification state is the hub callback — no owner-mint, no signature shortcut — checked in
 * source for each. Registries that keep only nullifier-keyed state (e.g. `MinimalDisclosure`,
 * the biggest requestor at 2,814 events) cannot be read by address and are reached by the
 * log scan instead.
 */
export const SELF_REGISTRIES = [
    // SelfSBTV2 (734): 180-day-validity soulbound token, `selfxyz/self-sbt` template.
    { label: 'self-sbt-v2', address: '0xF5A3278F665e5ec4762CDB2d0dc04f8eDDc68B91', kind: 'self-sbt' },
    // EspressoSelfSBT (401): Espresso's deployment of the same template.
    { label: 'espresso-self-sbt', address: '0x206738737b29aeb61045181ab5c11679ef41a56b', kind: 'self-sbt' },
    // Second SelfSBTV2 instance (48).
    { label: 'self-sbt-v2-b', address: '0xb69f2308f62f4e4b457cad4722da5ab0ea57b97a', kind: 'self-sbt' },
    // SelfVerifierV2 (44): plain mapping with a per-address verification timestamp.
    { label: 'self-verifier-v2', address: '0xf094051f9256a8479be6e0fd2042b58356727d9c', kind: 'verified-at' },
    // ProofOfHuman (287): Self's example integration; boolean only, no timestamp view.
    { label: 'proof-of-human', address: '0x5e05a5ccf9fe3ec0a4b602a56381d685d0f711a8', kind: 'boolean' },
];
const SBT_ABI = parseAbi([
    'function getTokenIdByAddress(address user) view returns (uint256)',
    'function getTokenExpiry(uint256 tokenId) view returns (uint256)',
    'function getValidityPeriod() view returns (uint256)',
]);
const VERIFIED_AT_ABI = parseAbi([
    'function isVerified(address user) view returns (bool)',
    'function verifiedAt(address user) view returns (uint64)',
]);
const BOOLEAN_ABI = parseAbi(['function isVerified(address user) view returns (bool)']);
// ------------------------------------------------------------------ pure parts
const DISCLOSURE_DATA_PARAMS = parseAbiParameters('uint256 destChainId, bytes32 configId, uint256 userIdentifier, bytes output, bytes userDataToPass');
/** Decode a `DisclosureVerified` payload. Exported pure so a real on-chain log pins it. */
export function decodeDisclosureVerified(data) {
    const [destChainId, configId, userIdentifier] = decodeAbiParameters(DISCLOSURE_DATA_PARAMS, data);
    const isAddress = userIdentifier > 0n && userIdentifier < 1n << 160n;
    return {
        destChainId,
        configId,
        userIdentifier,
        userAddress: isAddress ? `0x${userIdentifier.toString(16).padStart(40, '0')}` : null,
    };
}
/**
 * The SBT decision as a pure function. A token id of 0 is the contract's "never verified".
 * An *expired* token is still `held`: the disclosure proof happened at `expiry - validity`
 * and that fact does not expire — the adapter's own decay curve prices the staleness, and
 * `registryExpired` says what the integrator's 180-day policy thinks.
 */
export function interpretSbtRead(tokenId, expiry, validityPeriod, nowSeconds) {
    if (tokenId === 0n)
        return { held: false };
    const expiresAt = Number(expiry);
    return {
        held: true,
        issuedAt: Number(expiry - validityPeriod),
        expiresAt,
        registryExpired: nowSeconds >= expiresAt,
    };
}
/**
 * Which reading dates the credential: the newest dated hit — every date here is a
 * re-attestation, and under Decay the newest is the honest one because it is what the
 * protocol most recently accepted a proof for. Undated hits still decide `held`.
 */
export function pickBestReading(hits) {
    if (hits.length === 0)
        return undefined;
    const dated = hits.filter((h) => h.issuedAt !== undefined);
    if (dated.length === 0)
        return hits[0];
    return dated.reduce((a, b) => ((b.issuedAt ?? 0) > (a.issuedAt ?? 0) ? b : a));
}
// ------------------------------------------------------------------ adapter
async function safe(fn) {
    try {
        return await fn();
    }
    catch (e) {
        return { held: false, error: e instanceof Error ? e.message : String(e) };
    }
}
export function selfAdapter(opts = {}) {
    const endpoints = opts.endpoints ?? CELO_ENDPOINTS;
    const registries = opts.registries ?? SELF_REGISTRIES;
    const timeout = opts.timeoutMs ?? 15_000;
    const logScanBlocks = opts.logScanBlocks ?? 8000000n;
    const maxLogCalls = opts.maxLogCalls ?? 12;
    const clients = endpoints.map((e) => ({
        ...e,
        client: createPublicClient({
            chain: celo,
            transport: http(e.url, { timeout, retryCount: 0 }),
        }),
    }));
    /** Scan order: widest log window first, so the call budget buys the most history. */
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
    /** Per-registry validity period, cached for the process — it is a config read, not state. */
    const validityPeriods = new Map();
    const validityPeriodOf = (registry) => {
        let p = validityPeriods.get(registry.address);
        if (!p) {
            p = firstAnswer((c) => c.readContract({
                address: registry.address,
                abi: SBT_ABI,
                functionName: 'getValidityPeriod',
            }));
            p.catch(() => validityPeriods.delete(registry.address));
            validityPeriods.set(registry.address, p);
        }
        return p;
    };
    const readRegistry = async (registry, subject, now) => {
        if (registry.kind === 'self-sbt') {
            const tokenId = (await firstAnswer((c) => c.readContract({
                address: registry.address,
                abi: SBT_ABI,
                functionName: 'getTokenIdByAddress',
                args: [subject],
            })));
            if (tokenId === 0n)
                return { registry: registry.label, held: false };
            const [expiry, validity] = await Promise.all([
                firstAnswer((c) => c.readContract({
                    address: registry.address,
                    abi: SBT_ABI,
                    functionName: 'getTokenExpiry',
                    args: [tokenId],
                })),
                validityPeriodOf(registry),
            ]);
            const verdict = interpretSbtRead(tokenId, expiry, validity, now);
            return {
                registry: registry.label,
                held: verdict.held,
                ...(verdict.issuedAt !== undefined ? { issuedAt: verdict.issuedAt } : {}),
                detail: {
                    tokenId: Number(tokenId),
                    expiresAt: verdict.expiresAt,
                    registryExpired: verdict.registryExpired,
                },
            };
        }
        if (registry.kind === 'verified-at') {
            const [ok, at] = await Promise.all([
                firstAnswer((c) => c.readContract({ address: registry.address, abi: VERIFIED_AT_ABI, functionName: 'isVerified', args: [subject] })),
                firstAnswer((c) => c.readContract({ address: registry.address, abi: VERIFIED_AT_ABI, functionName: 'verifiedAt', args: [subject] })),
            ]);
            if (!ok)
                return { registry: registry.label, held: false };
            return { registry: registry.label, held: true, ...(at > 0n ? { issuedAt: Number(at) } : {}) };
        }
        const ok = (await firstAnswer((c) => c.readContract({ address: registry.address, abi: BOOLEAN_ABI, functionName: 'isVerified', args: [subject] })));
        return { registry: registry.label, held: ok };
    };
    /**
     * Backward chunked scan for the newest `DisclosureVerified` whose un-indexed
     * `userIdentifier` equals the subject. Newest-first so a recently active credential stops
     * after one window, and because only the latest disclosure dates the credential anyway.
     */
    const scanHub = async (subject, budget) => {
        const subjectId = BigInt(subject);
        let lastError;
        for (const e of byRange) {
            try {
                const head = await e.client.getBlockNumber();
                const floor = head > logScanBlocks + SELF_HUB_V2_DEPLOY_BLOCK ? head - logScanBlocks : SELF_HUB_V2_DEPLOY_BLOCK;
                let to = head;
                let calls = 0;
                while (to >= floor && calls < budget) {
                    const from = to > e.maxLogRange + floor ? to - e.maxLogRange : floor;
                    const logs = (await e.client.request({
                        method: 'eth_getLogs',
                        params: [
                            {
                                address: SELF_HUB_V2,
                                topics: [DISCLOSURE_VERIFIED_TOPIC],
                                fromBlock: `0x${from.toString(16)}`,
                                toBlock: `0x${to.toString(16)}`,
                            },
                        ],
                    }));
                    calls += 1;
                    const matches = logs.filter((l) => decodeDisclosureVerified(l.data).userIdentifier === subjectId);
                    if (matches.length > 0) {
                        const newest = matches[matches.length - 1];
                        return {
                            match: {
                                log: newest,
                                attestationId: Number(BigInt(newest.topics[3] ?? '0x0')),
                                requestor: `0x${(newest.topics[1] ?? '').slice(26)}`,
                            },
                            scannedFromBlock: from,
                            scannedToBlock: head,
                            complete: from <= SELF_HUB_V2_DEPLOY_BLOCK,
                        };
                    }
                    if (from === floor) {
                        return { scannedFromBlock: from, scannedToBlock: head, complete: from <= SELF_HUB_V2_DEPLOY_BLOCK };
                    }
                    to = from - 1n;
                }
                return { scannedFromBlock: to + 1n, scannedToBlock: head, complete: false };
            }
            catch (err) {
                lastError = err;
            }
        }
        throw lastError instanceof Error ? lastError : new Error(String(lastError));
    };
    return {
        adapterId: 'self-protocol',
        probe: (subject) => safe(async () => {
            const now = Math.floor(Date.now() / 1000);
            const headBlock = Number(await firstAnswer((c) => c.getBlockNumber()));
            const settled = await Promise.all(registries.map(async (r) => {
                try {
                    return { reading: await readRegistry(r, subject, now) };
                }
                catch (e) {
                    return { registry: r.label, error: e instanceof Error ? e.message : String(e) };
                }
            }));
            const failures = settled.filter((s) => 'error' in s);
            const readings = settled
                .map((s) => ('reading' in s ? s.reading : undefined))
                .filter((r) => r !== undefined);
            const unreadable = failures.length ? { registriesUnreadable: failures.map((f) => f.registry) } : {};
            const hits = readings.filter((r) => r.held);
            if (hits.length > 0) {
                const best = pickBestReading(hits);
                const notes = best.issuedAt !== undefined ? ['date-from-latest-reattestation'] : [];
                const provenance = {
                    heldFrom: 'chain',
                    dateFrom: best.issuedAt !== undefined ? 'chain' : 'none',
                    headBlock,
                    notes,
                };
                return {
                    held: true,
                    ...(best.issuedAt !== undefined ? { issuedAt: best.issuedAt } : {}),
                    provenance,
                    detail: {
                        source: 'registry',
                        registry: best.registry,
                        registries: Object.fromEntries(hits.map((h) => [h.registry, { issuedAt: h.issuedAt, ...(h.detail ?? {}) }])),
                        registriesChecked: readings.length,
                        ...unreadable,
                    },
                };
            }
            // No registry knows the address. The hub's own event stream is the wider net —
            // bounded, and honest about its bounds.
            const scan = await scanHub(subject, maxLogCalls);
            if (scan.match) {
                const block = await firstAnswer((c) => c.getBlock({ blockNumber: BigInt(scan.match.log.blockNumber) }));
                const provenance = {
                    heldFrom: 'chain',
                    dateFrom: 'chain',
                    headBlock,
                    notes: ['date-from-latest-reattestation'],
                };
                return {
                    held: true,
                    issuedAt: Number(block.timestamp),
                    provenance,
                    detail: {
                        source: 'hub-log',
                        requestor: scan.match.requestor,
                        documentType: SELF_DOCUMENT_TYPES[scan.match.attestationId] ?? `attestation-${scan.match.attestationId}`,
                        attestationId: scan.match.attestationId,
                        blockNumber: Number(BigInt(scan.match.log.blockNumber)),
                        transactionHash: scan.match.log.transactionHash,
                        registriesChecked: readings.length,
                        ...unreadable,
                    },
                };
            }
            const provenance = { heldFrom: 'chain', dateFrom: 'none', headBlock, notes: [] };
            return {
                held: false,
                provenance,
                detail: {
                    registriesChecked: readings.length,
                    ...unreadable,
                    scannedFromBlock: Number(scan.scannedFromBlock),
                    scannedToBlock: Number(scan.scannedToBlock),
                    scanComplete: scan.complete,
                    ...(scan.complete
                        ? {}
                        : { note: 'no disclosure in the scanned window; older history not searched' }),
                },
            };
        }),
    };
}
