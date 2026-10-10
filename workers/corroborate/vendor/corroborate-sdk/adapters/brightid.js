import { createPublicClient, defineChain, http, parseAbi } from 'viem';
/**
 * BrightID, read from the one on-chain registry that maps addresses to verifications: the
 * `BrightIDSnapshot` contract on IDChain.
 *
 * ## Why this registry, out of BrightID's many surfaces
 *
 * BrightID verifications are app-scoped: the graph labels *contextIds* (per-app UUIDs), not
 * addresses, and the node API answers only for apps that registered a context — which is why
 * the earlier sweep (`research/protocols/poh-kleros-brightid-idena.md`) called BrightID
 * readable-but-not-obtainable. The exception is the app whose contextIds *are* Ethereum
 * addresses: **`snapshot`**. For it, BrightID deployed a public registry on IDChain (their
 * own POA sidechain, chain id 74) at `0x81591DC4997A76A870c13D383F8491B288E09344` — the
 * contract Snapshot's official `brightid` voting strategy reads. `verify()` is a
 * permissionless relay: anyone may submit, validity comes from an ecrecover against a signer
 * holding the verifier token, and the state is a plain
 * `mapping(address => { uint256 time; bool isVerified })` plus an address-history chain.
 * That is exactly an address → verified map, readable by `eth_call` with no key and no
 * vendor. Verified source from the IDChain Blockscout, semantics confirmed live 2026-07-25.
 *
 * ## What held means, and what it is evidence of
 *
 * `held: true` means: a BrightID node holding the verifier token signed, at `issuedAt`, that
 * the human behind this address met the `snapshot` app's verification expression (the
 * social-graph "meets" criterion), and someone relayed that signature on chain. It is
 * social-graph vouching, not uniqueness and not liveness. The binding is consented — the
 * verification names the address, and only the address holder could obtain it — and it
 * cannot be planted: registering an address requires a node-signed message naming it.
 *
 * `isVerifiedUser` never expires on read. What unmakes it is the history mechanism: when a
 * user re-links to a new address, the old addresses are written `isVerified: false` with
 * their `time` kept — reported here as `detail.superseded`, distinct from never-registered.
 *
 * ## The honest catch: the registry is small and the write path is dead
 *
 * Measured 2026-07-25 over the chain's full history (IDChain's public RPC serves unbounded
 * `eth_getLogs`; 237 logs in ~9 s): **237 `Verified` events, 233 unique addresses**, first
 * 2022-01-22, last **2024-06-09** — no writes in over two years. And the write path needs a
 * BrightID node signature, but every BrightID node API endpoint checked returned 502 (dated
 * probes in `research/protocols/brightid-onchain-read.md`). So this is a frozen registry of
 * a few hundred addresses: the read is permissionless and the evidence is real for those who
 * hold it, but nobody new can obtain the credential today, which is why the ontology entry
 * must stay `live: false` and the forge cost is academic. IDChain itself is
 * protocol-operated (permissionless read, not infrastructure-independent — the Lens Chain
 * position), with `https://idchain.one/rpc` its only public endpoint.
 */
/** IDChain — BrightID's POA sidechain, chain id 74, still producing blocks on 2026-07-25. */
export const IDCHAIN_ID = 74;
/** IDChain's only public RPC. Serves full-history `eth_getLogs`, measured 2026-07-25. */
export const IDCHAIN_RPCS = ['https://idchain.one/rpc'];
/**
 * `BrightIDSnapshot` — verifier-token-gated registry for the `snapshot` app (`app()` reads
 * `"snapshot"`), deployed by BrightID and named `official.v5` in Snapshot's strategy.
 */
export const BRIGHTID_SNAPSHOT_REGISTRY = '0x81591DC4997A76A870c13D383F8491B288E09344';
/** `Verified(address indexed addr)` — one log per registration, used for live discovery. */
export const BRIGHTID_VERIFIED_TOPIC = '0x6a6455914f452787eb3985452aceedc1000fb545e394eb3b370e3d08958e0a5b';
export const BRIGHTID_REGISTRY_ABI = parseAbi([
    'function verifications(address) view returns (uint256 time, bool isVerified)',
    'function isVerifiedUser(address _user) view returns (bool)',
    'function history(address) view returns (address)',
    'function app() view returns (bytes32)',
]);
const ZERO_ADDRESS = '0x0000000000000000000000000000000000000000';
/**
 * The decision as a pure function of the `verifications` struct. Three cases the contract
 * can express: never registered (`time == 0`), currently verified, and superseded — a
 * once-verified address voided because its human re-linked to a newer one (`verify()` writes
 * `isVerified: false` but keeps `time` on every prior address in the submitted history).
 */
export function interpretBrightIdVerification(timeSeconds, isVerified) {
    if (isVerified) {
        return {
            held: true,
            issuedAt: Number(timeSeconds),
            detail: { registered: true, verificationTime: Number(timeSeconds) },
        };
    }
    if (timeSeconds > 0n) {
        return {
            held: false,
            detail: { registered: true, superseded: true, verificationTime: Number(timeSeconds) },
        };
    }
    return { held: false, detail: { registered: false } };
}
const idChain = defineChain({
    id: IDCHAIN_ID,
    name: 'IDChain',
    nativeCurrency: { name: 'Eidi', symbol: 'EIDI', decimals: 18 },
    rpcUrls: { default: { http: [...IDCHAIN_RPCS] } },
});
/**
 * One `eth_call` decides both held and the date (`verifications` returns the timestamp and
 * the flag atomically); a second, best-effort call reads the address-history link for the
 * detail. `issuedAt` is the BrightID node's verification timestamp from the registered
 * signature — the last time the graph attested this address, so it understates the account's
 * age, which on a Ramp curve is the conservative direction. Never throws.
 */
export function brightIdAdapter(opts = {}) {
    const rpcUrls = opts.rpcUrls ?? IDCHAIN_RPCS;
    const timeout = opts.timeoutMs ?? 15_000;
    const clients = rpcUrls.map((url) => createPublicClient({
        chain: idChain,
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
        adapterId: 'brightid',
        probe: async (subject) => {
            try {
                const [headBlock, [time, isVerified]] = await Promise.all([
                    tryEach('IDChain head', (c) => c.getBlockNumber()).then(Number),
                    tryEach('BrightIDSnapshot.verifications', (c) => c.readContract({
                        address: BRIGHTID_SNAPSHOT_REGISTRY,
                        abi: BRIGHTID_REGISTRY_ABI,
                        functionName: 'verifications',
                        args: [subject],
                    })),
                ]);
                const verdict = interpretBrightIdVerification(time, isVerified);
                // The predecessor link is texture, not evidence — losing the call changes nothing.
                let previousAddress;
                if (time > 0n) {
                    try {
                        const prev = await tryEach('history', (c) => c.readContract({
                            address: BRIGHTID_SNAPSHOT_REGISTRY,
                            abi: BRIGHTID_REGISTRY_ABI,
                            functionName: 'history',
                            args: [subject],
                        }));
                        if (prev.toLowerCase() !== ZERO_ADDRESS)
                            previousAddress = prev;
                    }
                    catch {
                        // best-effort
                    }
                }
                const provenance = {
                    heldFrom: 'chain',
                    dateFrom: verdict.held ? 'chain' : 'none',
                    headBlock,
                    notes: [],
                };
                return {
                    held: verdict.held,
                    ...(verdict.issuedAt !== undefined ? { issuedAt: verdict.issuedAt } : {}),
                    provenance,
                    detail: {
                        registry: BRIGHTID_SNAPSHOT_REGISTRY,
                        app: 'snapshot',
                        ...verdict.detail,
                        ...(previousAddress !== undefined ? { previousAddress } : {}),
                    },
                };
            }
            catch (e) {
                return { held: false, error: e instanceof Error ? e.message : String(e) };
            }
        },
    };
}
