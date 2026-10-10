/**
 * Subgraph client — the SDK's source for what a boolean contract read cannot see.
 *
 * `isHuman(addr)` answers "does this credential exist"; it cannot answer "was it revoked", or
 * "what is this avatar's position in the trust graph". Those live in indexed history, and the
 * subgraph is where we read them.
 *
 * Every read here returns the **block the index had reached**, in the same request as the
 * entity, so the answer is a statement about a named block rather than about "now". That is
 * what lets `reconcile.ts` tell three different things apart that the old code collapsed into
 * one `undefined`: the index does not have this credential *at a block it names*, the index
 * cannot see this credential's history at all, and the index did not answer. The first is
 * evidence; the last two are not.
 */
/**
 * Coverage of the deployed subgraph, per data source.
 *
 * `completeHistory` is the load-bearing field: it is what makes "absent from the index" mean
 * "did not exist yet" rather than "we cannot see it". It is true only where the data source
 * starts at the protocol's own first block.
 *
 * PoH: `startBlock` 35846827 is the deployment block of the v2 proxy, verified on chain —
 * `eth_getCode` at 35846826 returns `0x` and at 35846827 returns the proxy bytecode.
 *
 * Circles: `startBlock` 46300000 is a deliberate ~2-month window (the Hub emits ~7,200 Trust
 * events per 60k blocks and full history would not sync inside a hackathon), while the Hub's
 * first `RegisterHuman` was at block 36501311. Absence therefore proves nothing about a
 * Circles avatar, and the reconciler must not treat it as evidence — the oldest, most
 * legitimate avatars are precisely the ones missing.
 */
export const SUBGRAPH_COVERAGE = {
    poh: { fromBlock: 35846827, completeHistory: true },
    circles: { fromBlock: 46300000, completeHistory: false },
};
async function query(url, q, timeoutMs = 10_000) {
    try {
        const res = await fetch(url, {
            method: 'POST',
            headers: { 'content-type': 'application/json' },
            body: JSON.stringify({ query: q }),
            signal: AbortSignal.timeout(timeoutMs),
        });
        if (!res.ok)
            return undefined;
        const json = (await res.json());
        if (json.errors?.length)
            return undefined;
        return json.data;
    }
    catch {
        return undefined;
    }
}
/**
 * One index read: the entity *and* the block the index had reached, in a single request.
 *
 * Two requests would reintroduce the tear this design exists to remove — the index can
 * advance between them, so the entity and the block would describe different worlds. The
 * `_meta` block is what makes `entity: null` usable as evidence.
 *
 * Returns `undefined` for "the index did not answer", which is categorically different from
 * `entity: null`, "the index answered and does not have it".
 */
async function indexRead(url, entityQuery, map, coverage) {
    const data = await query(url, `{ _meta { block { number timestamp } } entity: ${entityQuery} }`);
    // A missing _meta means we cannot name the block this answer belongs to, and an unnamed
    // answer is not usable as evidence of absence. Treat it as no answer at all.
    if (!data?._meta)
        return undefined;
    return {
        view: {
            block: Number(data._meta.block.number),
            ...(data._meta.block.timestamp ? { blockTimestamp: Number(data._meta.block.timestamp) } : {}),
            entity: data.entity ? map(data.entity) : null,
            completeHistory: coverage.completeHistory,
        },
        row: data.entity,
    };
}
/**
 * Proof of Humanity v2, as the index has it, at the block the index names.
 *
 * `issuanceObserved` is true because the schema cannot distinguish a claim-dated entity from
 * one the vouch handler materialised — `requestId` is the index of the request within the
 * humanity, so 0 is the ordinary first claim, not a sentinel. That is fine here: the PoH
 * adapter dates the credential from the contract, so an index entity carrying a vouch
 * timestamp instead of a claim timestamp surfaces as a flagged disagreement rather than as a
 * wrong score.
 */
export async function pohIndexRead(subgraphUrl, address) {
    const read = await indexRead(subgraphUrl, `pohHuman(id: "${address.toLowerCase()}") { claimedAt revoked }`, (row) => ({
        issuedAt: Number(row.claimedAt),
        issuanceObserved: true,
        ended: Boolean(row.revoked),
    }), SUBGRAPH_COVERAGE.poh);
    return read?.view;
}
/**
 * Circles v2 avatar, as the index has it, at the block the index names.
 *
 * `inviter` is the discriminator for whether the registration itself was indexed: the mapping
 * sets it only in `handleRegisterHuman`, while `handleTrust` materialises an avatar for the
 * trustee of an edge and leaves it null. A null inviter therefore means `registeredAt` is a
 * trust-edge timestamp — later than the real registration, so it understates the avatar's
 * age. Measured live: the Hub's first two registered humans (block 36501311) both appear in
 * the index with a `registeredAt` from mid-2026, ten million blocks late, because the window
 * only caught their trust edges.
 */
export async function circlesIndexRead(subgraphUrl, address) {
    const read = await indexRead(subgraphUrl, `circlesAvatar(id: "${address.toLowerCase()}") { registeredAt trustedByCount stopped inviter }`, (row) => ({
        issuedAt: Number(row.registeredAt),
        issuanceObserved: row.inviter != null,
        ended: Boolean(row.stopped),
    }), SUBGRAPH_COVERAGE.circles);
    if (!read)
        return undefined;
    // Graph position is not part of the credential's identity, so it rides alongside the
    // reconciled view rather than inside it — same request, no second round trip.
    const trustedByCount = read.row?.trustedByCount;
    return {
        ...read.view,
        ...(typeof trustedByCount === 'number' ? { trustedByCount } : {}),
    };
}
/** PoH v2 humanity ids are the claimant's address as bytes20, so the lookup key is the address. */
export async function pohEnrichment(subgraphUrl, address) {
    const data = await query(subgraphUrl, `{ pohHuman(id: "${address.toLowerCase()}") { claimedAt revoked } }`);
    if (!data?.pohHuman)
        return undefined;
    return { claimedAt: Number(data.pohHuman.claimedAt), revoked: data.pohHuman.revoked };
}
export async function circlesEnrichment(subgraphUrl, address) {
    const data = await query(subgraphUrl, `{ circlesAvatar(id: "${address.toLowerCase()}") { registeredAt trustedByCount stopped } }`);
    if (!data?.circlesAvatar)
        return undefined;
    return {
        registeredAt: Number(data.circlesAvatar.registeredAt),
        trustedByCount: data.circlesAvatar.trustedByCount,
        stopped: data.circlesAvatar.stopped,
    };
}
/** True once the subgraph has a synced head. Used by tests to skip rather than fail while syncing. */
export async function subgraphReady(subgraphUrl) {
    const data = await query(subgraphUrl, `{ _meta { block { number } } }`);
    return (data?._meta?.block?.number ?? 0) > 0;
}
/**
 * The audit trail for one adapter, from the registry subgraph — every weight the ontology
 * has ever assigned it, each with the source it was derived from and the block it landed in.
 *
 * This is the accountability half of curated weights: the weights are judgments, so the
 * least a subject is owed is the full history of those judgments and their sources. The
 * biggest sybil-filtering deployment to date drew its loudest criticism for having no
 * stated method and no appeal path; this is the opposite design.
 */
export async function weightHistory(registrySubgraphUrl, adapterId) {
    const data = await query(registrySubgraphUrl, `{ weightChanges(where: { adapter: "${adapterId}" }, orderBy: revision, orderDirection: asc) {
        revision forgeCostCents rentCostCents live sourceURI timestamp block txHash } }`);
    if (!data)
        return undefined;
    return data.weightChanges.map((w) => ({
        revision: Number(w.revision),
        forgeCostCents: Number(w.forgeCostCents),
        rentCostCents: Number(w.rentCostCents),
        live: w.live,
        sourceURI: w.sourceURI,
        timestamp: Number(w.timestamp),
        block: Number(w.block),
        txHash: w.txHash,
    }));
}
