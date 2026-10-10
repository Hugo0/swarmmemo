import { createPublicClient, http, parseAbi, parseAbiItem } from 'viem';
import { arbitrum, base, mainnet, optimism, polygon } from 'viem/chains';
/**
 * Civic Pass, read from the Gateway Protocol's `GatewayToken` contracts.
 *
 * ## A dead protocol, read honestly
 *
 * Civic retired its personhood passes in mid-2025 (CAPTCHA on 2025-07-01, Uniqueness and
 * Liveness on 2025-07-31 — dates hard-coded in Human Passport's own Civic provider) and the
 * ontology already carries `live: false`. This adapter exists anyway, for the same reason the
 * ontology keeps the entry: the tokens are still on chain, competitors still assign points to
 * them, and the difference between "expired credential, verifiable" and "no integration" is
 * exactly what the live flag needs to stay honest. Gateway tokens carry an `expiration` and
 * `verifyToken` enforces it, so the read decides `held` from chain state, not from Civic's
 * marketing status — if every token has lapsed, that is what the chain says, and if Civic ever
 * resumed issuing, this probe would notice without a code change.
 *
 * The chain agrees with the retirement dates: sampling the newest tokens on every deployed
 * chain on 2026-07-25, the latest personhood-network expiry anywhere was **2025-10-27**
 * (a UNIQUENESS token on Polygon), so every pass in networks 4/6/10/11 is now expired and
 * `held: false` is the expected answer for every subject. Mint activity in the trailing 1M
 * blocks on Polygon, Arbitrum, Base and Optimism: zero.
 *
 * ## The contract surface
 *
 * One ERC-3525 `GatewayToken` proxy, deterministically at the same address on every chain
 * (`deployments/` of `identity-com/on-chain-identity-gateway`, verified live on nine chains).
 * The "slot" is the gatekeeper network; Civic's four personhood networks are slots 4/6/10/11
 * and `getNetwork(slot)` returns the Solana gatekeeper-network address as the name — the same
 * `uniqobk8…` string Human Passport pins for UNIQUENESS, which is the cross-chain identity
 * check the live suite asserts. Discovery is owner-keyed state, no logs anywhere:
 * `getTokenIdsByOwnerAndNetwork(owner, network, onlyActive)` — note the deployed signature
 * has the trailing `bool`; the 2-arg variant in the repo's current ABI JSON is **absent from
 * the deployed bytecode** (checked selector-by-selector against the implementation behind the
 * proxy, 2026-07-25).
 *
 * ## What `held` would mean, and the expiry rule
 *
 * The ontology entry is *uniqueness* (`kyc-vendor:facetec` — Civic's own 3D face-map dedupe),
 * so only a network-10 token can make `held: true`, and only while `state == ACTIVE` and
 * `expiration` is unset or in the future. **An expired token is `held: false`**, reported
 * with the expiry on record: a lapsed video-selfie dedupe is evidence that a face check
 * happened once, not that the subject is currently one-face-one-wallet — Civic priced that
 * decay into the token itself, and this adapter honours it rather than resurrecting dead
 * credentials the way the competitor scoreboards this entry exists to shame still do.
 * IDV (6), LIVENESS (11) and CAPTCHA (4) tokens are reported in detail as observations —
 * they are different evidence classes under the same vendor and must not launder into a
 * uniqueness claim.
 *
 * ## Dating
 *
 * For the (today hypothetical) valid token, issuance is the mint `Transfer` log — `_tokenId`
 * is indexed, so the scan filters to exactly one event and runs newest-first in windows sized
 * to what each chain's endpoint actually serves (measured 2026-07-25: the Tenderly gateways
 * serve 1M-block windows on Polygon/Arbitrum/Base/Optimism; Ethereum has no keyless
 * wide-window endpoint and scans in 10k windows). A scan that exhausts its budget reports
 * `scanComplete: false` and the blocks it searched instead of pretending absence of a date is
 * a date. Expired tokens are not dated — `held: false` results carry no `issuedAt`.
 */
// ------------------------------------------------------------------ constants
/** The GatewayToken proxy — same address on every chain the protocol deployed to. */
export const CIVIC_GATEWAY_TOKEN = '0xF65b6396dF6B7e2D8a6270E3AB6c7BB08BAEF22E';
/**
 * Civic's personhood gatekeeper networks (ERC-3525 slots). The ids come from Human
 * Passport's Civic provider (`platforms/src/Civic/Providers/types.ts`) and were confirmed
 * against `getNetwork()` on chain: each returns the corresponding Solana gatekeeper-network
 * address as its name.
 */
export const CIVIC_NETWORKS = {
    captcha: 4n,
    idv: 6n,
    uniqueness: 10n,
    liveness: 11n,
};
/** `getNetwork(slot)` values observed on-chain 2026-07-25, identical on every chain read. */
export const CIVIC_NETWORK_NAMES = {
    captcha: 'ignREusXmGrscGNUesoU9mxfds9AiYTezUKex2PsZV6',
    idv: 'bni1ewus6aMxTxBi5SAfzEmmXLf8KcVFRmTfproJuKw',
    uniqueness: 'uniqobk8oGh4XBLMqM68K8M2zNu3CdYX7q5go7whQiv',
    liveness: 'vaa1QRNEBb1G2XjPohqGWnPsvxWnwwXF67pdjrhDSwM',
};
/** `enum TokenState { ACTIVE, FROZEN, REVOKED }` — gateway-protocol-evm `smart-contract`. */
export const CIVIC_TOKEN_STATE = ['ACTIVE', 'FROZEN', 'REVOKED'];
/**
 * The chains this probe reads, which are where the tokens are: total supplies measured
 * 2026-07-25 were Polygon 689,898 / Arbitrum 115,810 / Base 76,884 / Ethereum 33,232 /
 * Optimism 28,435 — together ~99% of all gateway tokens observed. The contract also lives at
 * the same address on Avalanche (6,274), Polygon zkEVM (1,450), Gnosis (0), XDC, Fantom and
 * Celo; those are omitted as rounding error, and the research file says so rather than the
 * omission being silent.
 */
export const CIVIC_CHAINS = {
    polygon: {
        chain: polygon,
        rpc: 'https://polygon-bor-rpc.publicnode.com',
        logRpc: 'https://polygon.gateway.tenderly.co',
        maxLogRange: 1000000n,
    },
    arbitrum: {
        chain: arbitrum,
        rpc: 'https://arbitrum-one-rpc.publicnode.com',
        logRpc: 'https://arbitrum.gateway.tenderly.co',
        maxLogRange: 1000000n,
    },
    base: {
        chain: base,
        rpc: 'https://mainnet.base.org',
        logRpc: 'https://base.gateway.tenderly.co',
        maxLogRange: 1000000n,
    },
    ethereum: {
        chain: mainnet,
        rpc: 'https://ethereum-rpc.publicnode.com',
        // publicnode refuses keyless getLogs and no measured wide-window endpoint exists for
        // mainnet; 10k windows through the same eth_call endpoint is the honest floor.
        logRpc: 'https://ethereum-rpc.publicnode.com',
        maxLogRange: 10000n,
    },
    optimism: {
        chain: optimism,
        rpc: 'https://optimism-rpc.publicnode.com',
        logRpc: 'https://optimism.gateway.tenderly.co',
        maxLogRange: 1000000n,
    },
};
export const CIVIC_GATEWAY_ABI = parseAbi([
    'function getTokenIdsByOwnerAndNetwork(address owner, uint256 network, bool onlyActive) view returns (uint256[])',
    'function getToken(uint256 tokenId) view returns (address owner, uint8 state, string identity, uint256 expiration, uint256 bitmask)',
    'function verifyToken(address owner, uint256 network) view returns (bool)',
    'function getNetwork(uint256 network) view returns (string)',
    'function totalSupply() view returns (uint256)',
    'function tokenByIndex(uint256 index) view returns (uint256)',
    'function slotOf(uint256 tokenId) view returns (uint256)',
]);
/** ERC-721-shaped Transfer with all three topics indexed, as the ERC-3525 contract emits it. */
export const CIVIC_TRANSFER_EVENT = parseAbiItem('event Transfer(address indexed _from, address indexed _to, uint256 indexed _tokenId)');
/**
 * The validity rule as `verifyToken` implements it, pure so every branch is testable: a
 * token is valid iff `state == ACTIVE` and its expiry (when set) is in the future. Observed
 * reality 2026-07-25: personhood tokens sit at `state 0` with lapsed expirations, so the
 * `expired` branch is the common case, not the corner.
 */
export function interpretCivicToken(token, nowSeconds) {
    if (token.state === 1)
        return { valid: false, reason: 'frozen' };
    if (token.state === 2)
        return { valid: false, reason: 'revoked' };
    if (token.state !== 0)
        return { valid: false, reason: 'unknown-state' };
    if (token.expiration !== 0 && token.expiration <= nowSeconds) {
        return { valid: false, reason: 'expired' };
    }
    return { valid: true };
}
async function safe(fn) {
    try {
        return await fn();
    }
    catch (e) {
        return { held: false, error: e instanceof Error ? e.message : String(e) };
    }
}
/**
 * Civic Pass (uniqueness), decided entirely by `eth_call` against the GatewayToken on five
 * chains in parallel — 4 network lookups plus one `getToken` per found token, no logs on the
 * discovery path. `held` requires a currently-valid UNIQUENESS token confirmed twice: once by
 * this adapter's own reading of the struct and once by the contract's `verifyToken`, so a
 * misreading of the state machine cannot mint evidence the contract itself would refuse.
 * Every token in the other personhood networks is reported in `detail.tokens` with its state
 * and expiry, because "your Civic pass expired on 2025-08-28" is a materially better answer
 * than "no".
 */
export function civicPassAdapter(opts = {}) {
    const chainNames = opts.chains ?? Object.keys(CIVIC_CHAINS);
    const timeout = opts.timeoutMs ?? 15_000;
    const maxLogCalls = opts.maxLogCalls ?? 12;
    const callClients = new Map();
    const clientFor = (name) => {
        let c = callClients.get(name);
        if (!c) {
            const cfg = CIVIC_CHAINS[name];
            c = createPublicClient({
                chain: cfg.chain,
                transport: http(opts.rpcUrls?.[name] ?? cfg.rpc, { timeout, retryCount: 0 }),
            });
            callClients.set(name, c);
        }
        return c;
    };
    const readChain = async (name, subject) => {
        const c = clientFor(name);
        const perNetwork = await Promise.all(Object.entries(CIVIC_NETWORKS).map(async ([kind, id]) => {
            const ids = (await c.readContract({
                address: CIVIC_GATEWAY_TOKEN,
                abi: CIVIC_GATEWAY_ABI,
                functionName: 'getTokenIdsByOwnerAndNetwork',
                args: [subject, id, false],
            }));
            return { kind, ids };
        }));
        const observations = [];
        for (const { kind, ids } of perNetwork) {
            for (const tokenId of ids) {
                const [, state, , expiration] = (await c.readContract({
                    address: CIVIC_GATEWAY_TOKEN,
                    abi: CIVIC_GATEWAY_ABI,
                    functionName: 'getToken',
                    args: [tokenId],
                }));
                observations.push({
                    chain: name,
                    network: kind,
                    tokenId,
                    state: Number(state),
                    expiration: Number(expiration),
                });
            }
        }
        return { chain: name, observations };
    };
    /**
     * Newest-first chunked scan for the single mint `Transfer(0x0 → subject, tokenId)` log.
     * Only ever runs for a currently-valid token, which cannot be older than its own validity
     * window, so the recency bias of a bounded scan matches where the log must be.
     */
    const findMintDate = async (name, subject, tokenId) => {
        const cfg = CIVIC_CHAINS[name];
        const c = createPublicClient({
            chain: cfg.chain,
            transport: http(cfg.logRpc, { timeout: Math.max(timeout, 25_000), retryCount: 0 }),
        });
        const head = await c.getBlockNumber();
        let to = head;
        for (let calls = 0; calls < maxLogCalls && to > 0n; calls++) {
            const from = to > cfg.maxLogRange ? to - cfg.maxLogRange + 1n : 0n;
            const logs = await c.getLogs({
                address: CIVIC_GATEWAY_TOKEN,
                event: CIVIC_TRANSFER_EVENT,
                args: {
                    _from: '0x0000000000000000000000000000000000000000',
                    _to: subject,
                    _tokenId: tokenId,
                },
                fromBlock: from,
                toBlock: to,
            });
            if (logs.length > 0) {
                const mint = logs[0];
                const issuedAt = Number((await c.getBlock({ blockNumber: mint.blockNumber })).timestamp);
                return { issuedAt, mintBlock: mint.blockNumber, scannedFromBlock: from, complete: true };
            }
            if (from === 0n)
                return { scannedFromBlock: 0n, complete: true };
            to = from - 1n;
        }
        return { scannedFromBlock: to + 1n, complete: false };
    };
    return {
        adapterId: 'civic-pass',
        probe: (subject) => safe(async () => {
            const now = Math.floor(Date.now() / 1000);
            const settled = await Promise.all(chainNames.map(async (chain) => {
                try {
                    return await readChain(chain, subject);
                }
                catch (e) {
                    return { chain, error: (e instanceof Error ? e.message : String(e)).split('\n')[0] };
                }
            }));
            const failures = settled.filter((s) => 'error' in s);
            if (failures.length === chainNames.length) {
                return {
                    held: false,
                    error: `no GatewayToken chain answered (${failures
                        .map((f) => `${f.chain}: ${f.error}`)
                        .join('; ')})`,
                };
            }
            const readings = settled.filter((s) => 'observations' in s);
            const observations = readings.flatMap((r) => r.observations);
            const tokensDetail = observations.map((o) => ({
                chain: o.chain,
                network: o.network,
                networkId: Number(CIVIC_NETWORKS[o.network]),
                tokenId: Number(o.tokenId),
                state: CIVIC_TOKEN_STATE[o.state] ?? `UNKNOWN(${o.state})`,
                expiration: o.expiration,
                ...(o.expiration !== 0 ? { expired: o.expiration <= now } : {}),
                ...interpretCivicToken(o, now),
            }));
            const unreadable = failures.length ? { chainsUnreadable: failures.map((f) => f.chain) } : {};
            // Only a *currently valid uniqueness* token is the credential; everything else in
            // the detail is context. Newest expiry first so the report leads with the best token.
            const uniqueness = observations
                .filter((o) => o.network === 'uniqueness')
                .sort((a, b) => b.expiration - a.expiration);
            const validUniqueness = uniqueness.filter((o) => interpretCivicToken(o, now).valid);
            if (validUniqueness.length === 0) {
                const newest = uniqueness[0];
                return {
                    held: false,
                    provenance: { heldFrom: 'chain', dateFrom: 'none', notes: [] },
                    detail: {
                        chainsRead: readings.length,
                        tokens: tokensDetail,
                        ...(newest
                            ? {
                                reason: interpretCivicToken(newest, now).reason,
                                // The concrete fact the caller can act on: when the pass stopped being one.
                                newestUniquenessExpiry: newest.expiration,
                            }
                            : { reason: 'no-uniqueness-token' }),
                        ...unreadable,
                    },
                };
            }
            const best = validUniqueness[0];
            // Second opinion from the contract's own gate: a token this adapter thinks is valid
            // but verifyToken refuses is a modelling fault to surface, never a credential.
            const verified = (await clientFor(best.chain).readContract({
                address: CIVIC_GATEWAY_TOKEN,
                abi: CIVIC_GATEWAY_ABI,
                functionName: 'verifyToken',
                args: [subject, CIVIC_NETWORKS.uniqueness],
            }));
            if (!verified) {
                return {
                    held: false,
                    provenance: { heldFrom: 'chain', dateFrom: 'none', notes: [] },
                    detail: {
                        chainsRead: readings.length,
                        tokens: tokensDetail,
                        reason: 'verifyToken-disagrees',
                        disagreement: `token ${best.tokenId} on ${best.chain} reads valid but verifyToken(subject, 10) is false`,
                        ...unreadable,
                    },
                };
            }
            let issuedAt;
            let dating = {};
            try {
                const scan = await findMintDate(best.chain, subject, best.tokenId);
                issuedAt = scan.issuedAt;
                dating = scan.complete
                    ? { ...(scan.mintBlock !== undefined ? { mintBlock: Number(scan.mintBlock) } : {}) }
                    : { scanComplete: false, scannedFromBlock: Number(scan.scannedFromBlock) };
            }
            catch (e) {
                dating = { datingError: (e instanceof Error ? e.message : String(e)).split('\n')[0] };
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
                    chainsRead: readings.length,
                    chain: best.chain,
                    tokenId: Number(best.tokenId),
                    network: 'uniqueness',
                    expiration: best.expiration,
                    verifyToken: true,
                    tokens: tokensDetail,
                    ...dating,
                    ...unreadable,
                },
            };
        }),
    };
}
