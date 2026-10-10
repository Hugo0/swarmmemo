import { createPublicClient, http, parseAbi } from 'viem';
/**
 * Human Passport (ex-Gitcoin Passport), read from the on-chain Decoder.
 *
 * Why this one is worth reading at all: it is the largest score in the landscape and it is
 * the only aggregate in it that can be read **without the vendor's cooperation**. Users who
 * paid to mint have their score cached on chain, and `GitcoinResolver.getCachedScore` answers
 * for anyone. No API key, no rate limit, nothing Passport can revoke.
 *
 * ## Why we do not import the number
 *
 * Passport's scalar is an aggregate over its own stamps with its own weights, and those
 * weights are not ours to inherit. `ETHScore#50` — a pure wallet-history model — is weighted
 * the same as a government ID (`research/protocols/passport-civic-fractal-zkme-galxe.md`).
 * Worse, the stamp list contains credentials this ontology already prices under their own
 * roots: Coinbase (Persona), Holonym gov-id, Holonym biometrics, Civic, BrightID, Idena,
 * TrustaLabs. Adding Passport's number to a score that already counts those would double-count
 * the exact correlation the root model exists to collapse.
 *
 * So the passport is rooted at `behavioral:wallet-history` and priced at the farmed-wallet
 * market — a dollar, not KYC money — and the score value is reported as detail and never
 * multiplied into anything. What the probe *does* do with the stamps is disclose them: it maps
 * each one to the ontology adapter that owns it and reports which roots this passport is
 * restating, so a caller can see that a score of 28.8 was three stamps, two of which we
 * already counted somewhere else. That is the "1inch for personhood" claim made legible on
 * one address rather than asserted in a pitch.
 *
 * ## Mechanism, verified against the deployed contracts on 2026-07-25
 *
 * `GitcoinPassportDecoder.getScore(user)` is revert-driven: `AttestationNotFound()` when
 * `cachedScore.time == 0`, `AttestationExpired(uint64)` when the score has aged out. We read
 * the resolver directly instead, because `getCachedScore` returns the same struct the Decoder
 * consults — `{ uint32 score, uint64 time, uint64 expirationTime }` — and it carries the
 * issuance date the Decoder throws away. That date is what feeds our decay curve, so taking
 * the revert path would have cost us the age of the credential for no gain.
 *
 * Expiry is `expirationTime` when set, else `time + maxScoreAge()` — 7,776,000 seconds, 90
 * days, on all seven deployments. That derivation is not folklore: for
 * `0xb0812e00…90F2` on Optimism, `time` 1740958699 + 7776000 = 1748734699, and
 * `getScore` reverts `AttestationExpired(1748734699)`. Same arithmetic, same number, from two
 * independent paths. `human-passport.live.test.ts` re-derives it rather than asserting it.
 *
 * ## Seven chains, because one is wrong
 *
 * A passport is minted per chain and the mints disagree. `0xb0812e00…90F2` holds 50.015 on
 * Optimism and Linea and 25.099 on Scroll — a year older — and nothing on Base, Arbitrum,
 * Shape or zkSync. Reading one chain would have reported the wrong score for two of the three
 * chains it exists on, and no score at all for most addresses. We read all seven and take the
 * most recently issued unexpired one, which is both the freshest evidence and the one the
 * subject most recently paid for.
 *
 * Per the rule at the top of `adapters/index.ts`, a probe never turns a failure into a
 * negative. A chain that does not answer is dropped; only if *every* chain fails to answer do
 * we return an `error`, because "no passport" and "we could not look" are different claims.
 */
/**
 * Decoder deployments, from the "Smart contracts → Contract reference" page of
 * docs.passport.human.tech (fetched 2026-07-24, tabulated in
 * `research/protocols/passport-civic-fractal-zkme-galxe.md`). Every one of these was confirmed
 * to hold code and to answer `maxScoreAge()` on 2026-07-25.
 *
 * The resolver address is deliberately *not* listed here. We ask each Decoder which resolver
 * it trusts (`gitcoinResolver()`) rather than hard-coding a second address per chain: it is one
 * cached call, it cannot drift out of date, and it means an upgrade on their side cannot leave
 * us reading a resolver the Decoder has stopped believing.
 */
export const PASSPORT_DEPLOYMENTS = {
    // Not `mainnet.optimism.io`, which is one of the few keyless endpoints serving *archive*
    // state and is therefore spent by the Farcaster probe. Passport only ever reads at head, so
    // it takes a head-only endpoint and leaves the scarce archive quota to the reader that
    // needs it. Verified 2026-07-25 to return the same resolver, maxScoreAge and threshold.
    optimism: { decoder: '0x5558D441779Eca04A329BcD6b47830D2C6607769', rpc: 'https://optimism-rpc.publicnode.com' },
    base: { decoder: '0xaa24a127d10C68C8F9Ac06199AA606953cD82eE7', rpc: 'https://base-rpc.publicnode.com' },
    arbitrum: { decoder: '0x2050256A91cbABD7C42465aA0d5325115C1dEB43', rpc: 'https://arbitrum-one-rpc.publicnode.com' },
    linea: { decoder: '0x423cd60ab053F1b63D6F78c8c0c63e20F009d669', rpc: 'https://linea-rpc.publicnode.com' },
    scroll: { decoder: '0x8A5820030188346cC9532a1dD9FD2EF8d8F464de', rpc: 'https://rpc.scroll.io' },
    shape: { decoder: '0x2443D22Db6d25D141A1138D80724e3Eee54FD4C2', rpc: 'https://mainnet.shape.network' },
    zksyncEra: { decoder: '0x1166FCDCA3B04311Ba9E2eD5ad2c660E730e1386', rpc: 'https://mainnet.era.zksync.io' },
};
const DECODER_ABI = parseAbi([
    'function gitcoinResolver() view returns (address)',
    'function maxScoreAge() view returns (uint64)',
    'function threshold() view returns (uint256)',
    'function getPassport(address user) view returns ((string provider, bytes32 hash, uint64 time, uint64 expirationTime)[])',
]);
const RESOLVER_ABI = parseAbi([
    'function getCachedScore(address user) view returns ((uint32 score, uint64 time, uint64 expirationTime))',
]);
/** Passport stores its score as a uint32 with four implied decimals: 288470 is 28.847. */
const SCORE_DECIMALS = 10_000;
/**
 * Which ontology adapter owns each Passport stamp.
 *
 * Only the stamps that restate a credential *this ontology already prices* are listed. The
 * rest — social accounts, staking tiers, NFT and gas heuristics — are wallet-history and
 * social-account signals with no separate entry, and they are what the passport legitimately
 * contributes under its own root.
 *
 * Names come from the Decoder's own on-chain provider list (`getProviders(currentVersion())`,
 * 102 entries on Optimism, read 2026-07-25) plus the free-form provider strings that the
 * score-v2 attestation path returns; `human-passport.live.test.ts` asserts that the legacy
 * names here still appear in that on-chain list, so a rename upstream fails loudly instead of
 * silently dropping a correlation.
 *
 * `BinanceBABT` is deliberately absent. It is a real credential and Passport weights it like a
 * government ID, but it has no vendor attribution in our research and therefore no root — and
 * an invented root is worse than an absent one. It surfaces as unattributed in `detail`.
 */
export const STAMP_TO_ADAPTER = {
    Coinbase: 'coinbase-verification',
    CoinbaseDualVerification: 'coinbase-verification',
    CoinbaseDualVerification2: 'coinbase-verification',
    HolonymGovIdProvider: 'holonym-gov-id',
    // "Proof of Clean Hands" is a sanctions/PEP screen layered on Holonym's *government ID*
    // pipeline — its own guide says "verify your government ID and complete liveness check".
    // It restates the same document check, so it shares that credential's root rather than
    // earning one of its own.
    CleanHands: 'holonym-gov-id',
    Biometrics: 'holonym-biometrics',
    CivicCaptchaPass: 'civic-pass',
    CivicUniquenessPass: 'civic-pass',
    CivicLivenessPass: 'civic-pass',
    Brightid: 'brightid',
    Poh: 'poh-v1',
    'IdenaState#Newbie': 'idena',
    'IdenaState#Verified': 'idena',
    'IdenaState#Human': 'idena',
    'IdenaStake#1k': 'idena',
    'IdenaStake#10k': 'idena',
    'IdenaStake#100k': 'idena',
    'IdenaAge#5': 'idena',
    'IdenaAge#10': 'idena',
    TrustaLabs: 'trusta-sybil',
};
/**
 * Passport speaks two stamp vocabularies, and only one of them is published on chain.
 *
 * The legacy passport attestation packs stamps as a bitmap indexed into the Decoder's
 * `getProviders(version)` array — 102 names on Optimism as of 2026-07-25 — so every name in it
 * is verifiable against the contract. The newer score-v2 attestation carries provider strings
 * inline instead, and those names never enter the on-chain array: a live passport returning
 * `Biometrics` and `Steam` proves it. These two are the mapped names that live only in the v2
 * vocabulary, listed so the live test can hold every *other* mapped name to the on-chain array
 * without a blanket exemption that would hide a genuine rename.
 *
 * Names verified against the per-platform `Providers-config.ts` files in passportxyz/passport
 * (fetched 2026-07-25): `Biometrics` is id.human.tech's FaceTec 3D liveness with a dedup step,
 * and `CleanHands` is the sanctions screen built on the same gov-id verification.
 */
export const SCORE_V2_ONLY_STAMPS = ['Biometrics', 'CleanHands'];
export function humanPassportAdapter(opts = {}) {
    const chains = opts.chains ?? Object.keys(PASSPORT_DEPLOYMENTS);
    const timeout = opts.timeoutMs ?? 12_000;
    const clients = new Map();
    const clientFor = (chain) => {
        let c = clients.get(chain);
        if (!c) {
            const url = opts.rpcUrls?.[chain] ?? PASSPORT_DEPLOYMENTS[chain].rpc;
            c = createPublicClient({ transport: http(url, { timeout }) });
            clients.set(chain, c);
        }
        return c;
    };
    // Cached as the in-flight promise, not the value, so N concurrent probes at cold start
    // still make one round trip per chain. A rejected config is evicted so a transient RPC
    // failure does not poison the adapter for the life of the process.
    const configs = new Map();
    const configFor = (chain) => {
        let p = configs.get(chain);
        if (!p) {
            const c = clientFor(chain);
            const decoder = PASSPORT_DEPLOYMENTS[chain].decoder;
            p = Promise.all([
                c.readContract({ address: decoder, abi: DECODER_ABI, functionName: 'gitcoinResolver' }),
                c.readContract({ address: decoder, abi: DECODER_ABI, functionName: 'maxScoreAge' }),
                c.readContract({ address: decoder, abi: DECODER_ABI, functionName: 'threshold' }),
            ]).then(([resolver, maxScoreAge, threshold]) => ({
                resolver: resolver,
                maxScoreAge: Number(maxScoreAge),
                threshold: Number(threshold),
            }));
            p.catch(() => configs.delete(chain));
            configs.set(chain, p);
        }
        return p;
    };
    const readChain = async (chain, subject, now) => {
        const cfg = await configFor(chain);
        const raw = await clientFor(chain).readContract({
            address: cfg.resolver,
            abi: RESOLVER_ABI,
            functionName: 'getCachedScore',
            args: [subject],
        });
        const time = Number(raw.time);
        // time == 0 is the resolver's own sentinel for "no attestation cached here" — it is what
        // the Decoder tests before reverting AttestationNotFound. An address with no passport on
        // this chain is not evidence of anything, so it is dropped rather than reported as absent.
        if (time === 0)
            return null;
        const expirationTime = Number(raw.expirationTime);
        const expiresAt = expirationTime > 0 ? expirationTime : time + cfg.maxScoreAge;
        const score = Number(raw.score);
        return {
            chain,
            score,
            time,
            expirationTime,
            expiresAt,
            expired: now >= expiresAt,
            meetsOwnThreshold: score >= cfg.threshold,
        };
    };
    return {
        adapterId: 'human-passport',
        probe: async (subject) => {
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
            // Every deployment unreachable is an outage, not an absence. Reporting held:false here
            // would make an RPC blip read as "this address has no wallet history".
            if (failures.length === chains.length) {
                return {
                    held: false,
                    error: `no Passport deployment answered (${failures.map((f) => `${f.chain}: ${f.error.split('\n')[0]}`).join('; ')})`,
                };
            }
            const readings = settled
                .map((s) => ('reading' in s ? s.reading : null))
                .filter((r) => r !== null);
            const perChain = Object.fromEntries(readings.map((r) => [
                r.chain,
                {
                    score: r.score / SCORE_DECIMALS,
                    issuedAt: r.time,
                    expiresAt: r.expiresAt,
                    expired: r.expired,
                },
            ]));
            const unreadable = failures.length
                ? { chainsUnreadable: failures.map((f) => f.chain) }
                : {};
            if (readings.length === 0) {
                return { held: false, detail: { minted: false, chainsRead: chains.length - failures.length, ...unreadable } };
            }
            // Freshest unexpired mint wins: it is the strongest evidence available and the one the
            // subject most recently paid to publish. A score of 0 is a real passport carrying no
            // stamps, which is not wallet-history evidence, so it does not count as held.
            const valid = readings.filter((r) => !r.expired && r.score > 0);
            if (valid.length === 0) {
                const newest = readings.reduce((a, b) => (b.expiresAt > a.expiresAt ? b : a));
                return {
                    held: false,
                    detail: {
                        minted: true,
                        reason: newest.score === 0 ? 'score-zero' : 'score-expired',
                        expiredAt: newest.expiresAt,
                        perChain,
                        ...unreadable,
                    },
                };
            }
            const best = valid.reduce((a, b) => (b.time > a.time ? b : a));
            // Stamps are disclosure only — they never change `held` or the weight. Best-effort:
            // getPassport reverts AttestationNotFound when only a community-scoped score is cached,
            // and a passport we can price is worth more than a stamp list we cannot fetch.
            let stamps;
            try {
                const credentials = await clientFor(best.chain).readContract({
                    address: PASSPORT_DEPLOYMENTS[best.chain].decoder,
                    abi: DECODER_ABI,
                    functionName: 'getPassport',
                    args: [subject],
                });
                stamps = credentials.map((c) => c.provider).filter((p) => p.length > 0);
            }
            catch {
                // Leave undefined: absent disclosure is honest, invented disclosure is not.
            }
            const restatedAdapters = stamps
                ? [...new Set(stamps.map((s) => STAMP_TO_ADAPTER[s]).filter((a) => Boolean(a)))]
                : [];
            return {
                held: true,
                issuedAt: best.time,
                detail: {
                    minted: true,
                    score: best.score / SCORE_DECIMALS,
                    chain: best.chain,
                    expiresAt: best.expiresAt,
                    /**
                     * Passport's own 20-point pass mark, read from `threshold()` rather than hard-coded.
                     * Reported so a caller can see what Passport concluded; never used here, because
                     * adopting it would be adopting their weighting.
                     */
                    meetsPassportThreshold: best.meetsOwnThreshold,
                    ...(stamps ? { stamps } : {}),
                    ...(restatedAdapters.length ? { restatesAdapters: restatedAdapters } : {}),
                    perChain,
                    ...unreadable,
                },
            };
        },
    };
}
