import { createPublicClient, encodeAbiParameters, http, keccak256, parseAbi, } from 'viem';
import { bsc } from 'viem/chains';
/**
 * Galxe Passport, read from the soulbound token on BNB Chain.
 *
 * ## Which contract, which chain — verified, not remembered
 *
 * The Galxe Passport SBT is `0xE840…3C012` on BNB Chain, deployed 2022-09-12 (block
 * 21,257,482, its own timestamp) and still the live artifact in 2026: name "Galxe Passport",
 * 1,047,302 minted / 1,044,375 outstanding, with fresh mints observed on 2026-07-24 and
 * 2026-07-25 (tokens 1,047,301–302, real `Transfer` logs). Galxe's Identity Protocol
 * registries exist on five chains including their own Gravity chain, but the SBT was **not**
 * redeployed there — `eth_getCode` at the same address on Gravity (chain 1625) returns empty,
 * checked 2026-07-25 on two RPCs. BNB Chain is the only passive read there is.
 *
 * ## What the credential attests
 *
 * A Sumsub KYC session (Persona in the v2 era) that Galxe accepted, then minted as a
 * non-transferable ERC-721. Non-transferability is enforced in code — `_transfer` is
 * `require(false, "GalxePassport: passport is not transferrable")`, confirmed by simulating a
 * `transferFrom` from a live holder (reverts with exactly that string, 2026-07-25). So unlike
 * Lens accounts this credential cannot be planted or traded; `held` is the result of a KYC
 * flow the subject completed. What it is *not* is uniqueness-by-construction: the dedupe is
 * Sumsub's applicant database, the same trust root as every other Sumsub reseller
 * (`kyc-vendor:sumsub`, shared with Linea PoH — see the ontology).
 *
 * ## The read
 *
 * `balanceOf(subject)` decides `held` — one `eth_call` on any keyless endpoint. The contract
 * keeps an owner-keyed index (`mapping(address => uint256) _passports`, storage slot 7), so
 * the subject's tokenId is one `eth_getStorageAt` away, cross-checked with `ownerOf`. Both
 * `revoke` (Galxe-initiated) and `burn` (holder-initiated) zero the balance and the mapping,
 * so a revoked passport reads `held: false` with no further work.
 *
 * ## Dating, from a counter the contract cannot help publishing
 *
 * Token ids are array indices (`_tokens.push`), so they are strictly sequential in mint
 * order, and `getNumMinted()` (`_tokens.length - 1`) is a monotone counter that burns never
 * decrement. That gives exact issuance without any log scan: binary-search the first block
 * where `getNumMinted() >= tokenId` and take that block's timestamp. The catch is that the
 * search needs archive state, and **no keyless BSC endpoint serves archive calls** (measured
 * 2026-07-25: 48.club, dataseed, publicnode, 1rpc, drpc all refuse historical `eth_call`;
 * publicnode names its price — "Archive requests require a personal token"). The one public
 * archive is NodeReal's community endpoint, whose URL carries the shared API key published in
 * chainlist for wallet users. It is public but revocable, so it is used for **dating only**,
 * never for `held`; when it fails the probe falls back to `issuedAfter` bounds from
 * `GALXE_MINT_ANCHORS` — (block, timestamp, numMinted) triples measured from that same
 * counter on 2026-07-25, immutable past facts a live test re-verifies. A tokenId above an
 * anchor's count was provably minted after that anchor's block.
 */
// ------------------------------------------------------------------ constants
/** The Galxe Passport SBT on BNB Chain. Verified source (Sourcify full match, chain 56). */
export const GALXE_PASSPORT = '0xE84050261CB0A35982Ea0f6F3D9DFF4b8ED3C012';
/** Deployment block and its own timestamp (2022-09-12T03:50:43Z), found by bisecting code presence. */
export const GALXE_PASSPORT_DEPLOY_BLOCK = 21257482n;
export const GALXE_PASSPORT_DEPLOY_TIMESTAMP = 1_662_954_643;
/**
 * Storage slot of `mapping(address => uint256) _passports` (owner → tokenId). Located
 * empirically on 2026-07-25 by scanning slots 0–14 for a known holder and matching the value
 * against the tokenId in their mint log; slot 6 (`_balances`) held 1, slot 7 held the id.
 * The contract is not a proxy and has no upgrade path, so the layout cannot move.
 */
export const GALXE_PASSPORTS_MAPPING_SLOT = 7n;
/**
 * Keyless BNB Chain endpoints, all verified on 2026-07-25 to serve `eth_call` and
 * `eth_getStorageAt` at head. None serves archive state; 48.club additionally serves
 * `eth_getLogs` in ≤5,000-block windows over roughly the last 1.15M blocks (~6 days), which
 * the live suite uses to find real recent mints.
 */
export const GALXE_BSC_ENDPOINTS = [
    'https://rpc-bsc.48.club',
    'https://bsc-dataseed.bnbchain.org',
    'https://bsc-rpc.publicnode.com',
    'https://1rpc.io/bnb',
];
/**
 * NodeReal's community archive endpoint. The path component is the shared public key that
 * chainlist ships to wallet users — public in practice, revocable in principle, so this
 * endpoint is only ever consulted for dating and its failure costs the date, never `held`.
 */
export const GALXE_BSC_ARCHIVE_ENDPOINT = (globalThis.process?.env?.CORROBORATE_BNB_RPC_URL || 'https://bsc-dataseed.bnbchain.org');
/**
 * The mint counter sampled across the contract's whole life, every value read from
 * `getNumMinted()` at the named block via archive `eth_call` on 2026-07-25. Past state is
 * immutable, so these are constants in the same sense the Lens sunset timestamp is; the live
 * suite re-reads two of them. The curve they draw is the protocol's history: the 2023 quest
 * boom (117k → 409k in three months), the 2024 airdrop season (515k → 950k), and the 2026
 * trickle (~2 mints/day).
 */
export const GALXE_MINT_ANCHORS = [
    { block: GALXE_PASSPORT_DEPLOY_BLOCK, timestamp: GALXE_PASSPORT_DEPLOY_TIMESTAMP, numMinted: 0 },
    { block: 23000000n, timestamp: 1_668_281_702, numMinted: 46_850 },
    { block: 25000000n, timestamp: 1_674_369_493, numMinted: 72_409 },
    { block: 27000000n, timestamp: 1_680_440_989, numMinted: 116_831 },
    { block: 30000000n, timestamp: 1_689_471_382, numMinted: 409_002 },
    { block: 33000000n, timestamp: 1_698_502_272, numMinted: 465_448 },
    { block: 36000000n, timestamp: 1_707_528_314, numMinted: 514_533 },
    { block: 40000000n, timestamp: 1_719_558_579, numMinted: 950_258 },
    { block: 45000000n, timestamp: 1_734_581_019, numMinted: 978_532 },
    { block: 50000000n, timestamp: 1_747_743_395, numMinted: 1_016_929 },
    { block: 55000000n, timestamp: 1_753_247_936, numMinted: 1_038_071 },
    { block: 60000000n, timestamp: 1_756_998_493, numMinted: 1_042_887 },
    { block: 70000000n, timestamp: 1_764_501_498, numMinted: 1_045_825 },
    { block: 80000000n, timestamp: 1_770_545_597, numMinted: 1_046_129 },
    { block: 90000000n, timestamp: 1_775_049_329, numMinted: 1_046_645 },
    { block: 100000000n, timestamp: 1_779_555_432, numMinted: 1_047_231 },
    { block: 110000000n, timestamp: 1_784_059_341, numMinted: 1_047_295 },
];
export const GALXE_PASSPORT_ABI = parseAbi([
    'function balanceOf(address owner) view returns (uint256)',
    'function ownerOf(uint256 tokenId) view returns (address)',
    'function passportStatus(uint256 tokenId) view returns (uint32)',
    'function cid(uint256 tokenId) view returns (uint256)',
    'function getNumMinted() view returns (uint256)',
    'function totalSupply() view returns (uint256)',
]);
// ------------------------------------------------------------ pure derivations
/** The storage key of `_passports[owner]` — `keccak256(abi.encode(owner, slot))`. */
export function galxePassportSlotKey(owner) {
    return keccak256(encodeAbiParameters([{ type: 'address' }, { type: 'uint256' }], [owner, GALXE_PASSPORTS_MAPPING_SLOT]));
}
/**
 * Bracket a tokenId between anchors. Sound because ids are `_tokens.push` indices — strictly
 * sequential in mint order, a property of the verified source, not an observation. `low` is
 * always defined (the deployment anchor has `numMinted: 0` and ids start at 1); a missing
 * `high` means the token is newer than every anchor and the search must run to head.
 */
export function galxeMintBracket(tokenId, anchors = GALXE_MINT_ANCHORS) {
    let low = anchors[0];
    for (const a of anchors) {
        if (BigInt(a.numMinted) < tokenId)
            low = a;
        else
            return { low, high: a };
    }
    return { low };
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
 * Galxe Passport as a passive on-chain read.
 *
 * Per-probe cost, measured shape: negatives are 1 `eth_call`; positives are 3 `eth_call`s +
 * 1 `eth_getStorageAt` on keyless endpoints, plus ~20–24 archive `eth_call`s and one header
 * fetch for the exact date (the binary search runs inside the anchor bracket, so the window
 * is at most 10M blocks and usually far less). Archive unavailability loses the exact date,
 * never the credential — the result then carries `issuedAfter` from the anchor table.
 *
 * `held` here means "completed a Sumsub KYC flow Galxe accepted, and has not burned or been
 * revoked since". The `Decay` pricing in the ontology is why the dating machinery earns its
 * keep: a 2022 KYC session restated in 2026 is a stale fact about a document, and the mint
 * date is what lets the curve say so.
 */
export function galxePassportAdapter(opts = {}) {
    const rpcUrls = opts.rpcUrls ?? GALXE_BSC_ENDPOINTS;
    const timeout = opts.timeoutMs ?? 15_000;
    const dateExactly = opts.dateExactly ?? true;
    const clients = rpcUrls.map((url) => createPublicClient({
        chain: bsc,
        transport: http(url, { timeout, retryCount: 0 }),
    }));
    const archiveClient = createPublicClient({
        chain: bsc,
        transport: http(opts.archiveRpcUrl ?? GALXE_BSC_ARCHIVE_ENDPOINT, { timeout, retryCount: 0 }),
    });
    let next = 0;
    /** Rotate across keyless endpoints, naming every one that failed. */
    async function tryEach(what, fn) {
        const errors = [];
        for (let i = 0; i < clients.length; i++) {
            const at = (next + i) % clients.length;
            try {
                const result = await fn(clients[at]);
                next = (at + 1) % clients.length;
                return result;
            }
            catch (e) {
                errors.push(`${rpcUrls[at]}: ${(e instanceof Error ? e.message : String(e)).split('\n')[0]}`);
            }
        }
        throw new Error(`${what} unreadable — ${[...new Set(errors)].join('; ')}`);
    }
    const numMintedAt = (blockNumber) => archiveClient.readContract({
        address: GALXE_PASSPORT,
        abi: GALXE_PASSPORT_ABI,
        functionName: 'getNumMinted',
        blockNumber,
    });
    /**
     * The first block where `getNumMinted() >= tokenId` is the mint block, by monotonicity of
     * both the counter and the id sequence. Runs on the archive endpoint inside the anchor
     * bracket; any failure is thrown to the caller, which degrades to the anchor bound.
     */
    async function findMintBlock(tokenId, headBlock) {
        const bracket = galxeMintBracket(tokenId);
        let lo = bracket.low.block; // numMinted < tokenId here, proven by the anchor
        let hi = bracket.high?.block ?? headBlock;
        while (hi - lo > 1n) {
            const mid = lo + (hi - lo) / 2n;
            if ((await numMintedAt(mid)) >= tokenId)
                hi = mid;
            else
                lo = mid;
        }
        return hi;
    }
    return {
        adapterId: 'galxe-passport',
        probe: (subject) => safe(async () => {
            const headBlock = await tryEach('BSC head', (c) => c.getBlockNumber());
            const balance = (await tryEach('balanceOf', (c) => c.readContract({
                address: GALXE_PASSPORT,
                abi: GALXE_PASSPORT_ABI,
                functionName: 'balanceOf',
                args: [subject],
            })));
            if (balance === 0n) {
                return {
                    held: false,
                    provenance: { heldFrom: 'chain', dateFrom: 'none', headBlock: Number(headBlock), notes: [] },
                    detail: { passports: 0 },
                };
            }
            // The owner-keyed index: one storage read, then ownerOf as the cross-check. A failure
            // here loses the tokenId and the date but not the credential — balanceOf already
            // answered from the same contract.
            let tokenId;
            let status;
            let campaignId;
            let identityError;
            try {
                const raw = await tryEach('passport slot', (c) => c.getStorageAt({ address: GALXE_PASSPORT, slot: galxePassportSlotKey(subject) }));
                const candidate = raw ? BigInt(raw) : 0n;
                if (candidate === 0n)
                    throw new Error('storage slot empty despite non-zero balance');
                const owner = (await tryEach('ownerOf', (c) => c.readContract({
                    address: GALXE_PASSPORT,
                    abi: GALXE_PASSPORT_ABI,
                    functionName: 'ownerOf',
                    args: [candidate],
                })));
                if (owner.toLowerCase() !== subject.toLowerCase()) {
                    throw new Error(`slot names token ${candidate} but ownerOf says ${owner}`);
                }
                tokenId = candidate;
                status = Number(await tryEach('passportStatus', (c) => c.readContract({
                    address: GALXE_PASSPORT,
                    abi: GALXE_PASSPORT_ABI,
                    functionName: 'passportStatus',
                    args: [candidate],
                })));
                campaignId = Number(await tryEach('cid', (c) => c.readContract({
                    address: GALXE_PASSPORT,
                    abi: GALXE_PASSPORT_ABI,
                    functionName: 'cid',
                    args: [candidate],
                })));
            }
            catch (e) {
                identityError = e instanceof Error ? e.message : String(e);
            }
            if (tokenId === undefined) {
                return {
                    held: true,
                    provenance: { heldFrom: 'chain', dateFrom: 'none', headBlock: Number(headBlock), notes: [] },
                    detail: {
                        passports: Number(balance),
                        undated: `tokenId unresolvable: ${identityError ?? 'unknown'}`,
                    },
                };
            }
            // Exact date via the archive counter; anchor bound when the archive is out of reach.
            const bracket = galxeMintBracket(tokenId);
            let issuedAt;
            let mintBlock;
            let datingError;
            if (dateExactly) {
                try {
                    mintBlock = await findMintBlock(tokenId, headBlock);
                    issuedAt = Number((await archiveClient.getBlock({ blockNumber: mintBlock })).timestamp);
                }
                catch (e) {
                    datingError = e instanceof Error ? e.message : String(e);
                }
            }
            const provenance = {
                heldFrom: 'chain',
                dateFrom: issuedAt !== undefined ? 'chain' : 'none',
                headBlock: Number(headBlock),
                notes: [],
            };
            return {
                held: true,
                ...(issuedAt !== undefined
                    ? { issuedAt }
                    : // Provable lower bound: the counter at the anchor block had not reached this id.
                        { issuedAfter: bracket.low.timestamp }),
                provenance,
                detail: {
                    passports: Number(balance),
                    tokenId: Number(tokenId),
                    passportStatus: status,
                    campaignId,
                    ...(mintBlock !== undefined ? { mintBlock: Number(mintBlock) } : {}),
                    dating: issuedAt !== undefined
                        ? 'archive-mint-counter-bisection'
                        : 'anchor-lower-bound',
                    ...(datingError !== undefined
                        ? { datingError: datingError.split('\n')[0], anchorBlock: Number(bracket.low.block) }
                        : {}),
                },
            };
        }),
    };
}
