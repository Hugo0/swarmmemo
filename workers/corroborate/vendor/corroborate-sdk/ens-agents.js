import { getAddress, isAddress, keccak256, namehash, parseAbi, toBytes } from 'viem';
import { normalize } from 'viem/ens';
/**
 * ENS as the carrier of *agent* identity.
 *
 * `resolveSubject()` already reads `corroborate.subjects` — a human naming their wallets. This
 * file is the other half: an agent's name naming the human behind it, and that human's name
 * naming the agents it accepts responsibility for.
 *
 *   alpha.corroborate.eth   addr                  → the agent's wallet
 *                           corroborate.human     → corroborate.eth
 *   corroborate.eth         addr                  → the human's primary wallet
 *                           corroborate.subjects  → every wallet the human declares
 *                           corroborate.agents    → the agent names the human acknowledges
 *
 * A counterparty handed `alpha.corroborate.eth` resolves the whole picture from public
 * infrastructure — no server of ours, no registration with us, and no API key. That is the
 * same permissionless-read constraint every adapter in this SDK is held to.
 *
 * ## Why the acknowledgement record exists, and why the cap is worthless without it
 *
 * `corroborate.human` alone is a claim an agent makes about a person. Two failure modes follow,
 * and only the second is usually noticed:
 *
 *   1. **Riding a stranger.** An agent can name any address, including one holding a strong
 *      credential set, and inherit a score nobody consented to lend it.
 *   2. **Minting humans.** This is the one that breaks the fleet cap. "At most N agents per
 *      human" groups agents by the human they *name*, and an operator has as many addresses as
 *      it cares to generate. Name a fresh wallet per agent and every agent is its own human, so
 *      the cap binds nothing — while every individual answer stays true.
 *
 * The fix is not more cryptography, it is the other direction: the human's own name publishes
 * `corroborate.agents`, and a binding both ends assert is `mutual`. Writing that record costs a
 * transaction from the key that controls the human's name, so an operator can still mint humans
 * — but each one must be a name they control and pay for, and each one is then visibly a
 * *separate* human with a separate (usually empty) credential set. The evasion becomes
 * expensive and legible instead of free and invisible.
 *
 * A mutual binding is still not a proof of personhood, of distinctness, or that the human is
 * *operating* the agent. It proves that whoever controls the human's name accepts this agent.
 * The live tree carries an unacknowledged agent (`unverified.corroborate.eth`) precisely so the
 * demo shows the weak case working, and shows the policy closing it.
 *
 * ## What this file will not do
 *
 * It never infers a human from anything other than a published record. Clustering an agent's
 * wallet to an operator by funding history, or merging two declared humans because their
 * subject sets overlap, would be exactly the linkage this SDK exists to avoid — and both are
 * guesses that read as accusations when they are wrong. Overlap is *reported*
 * (`declared-humans-share-a-wallet`) and never acted on.
 *
 * Nothing here throws. A name that does not resolve, a record that is absent, an RPC that is
 * down — each produces an identity carrying `error` or a weaker `binding`, which the fleet
 * engine turns into `indeterminate`. A network failure must never read as "no human".
 */
/** The ENS registry. Same address on every network; `owner(namehash("eth"))` confirms it. */
export const ENS_REGISTRY = '0x00000000000C2E074eC69A0dFb2997BA6C7d2e1e';
/** Text record on an agent's name: the name or address of the human behind it. */
export const AGENT_HUMAN_RECORD = 'corroborate.human';
/** Text record on a human's name: the agent names that human acknowledges. */
export const HUMAN_AGENTS_RECORD = 'corroborate.agents';
/** Text record on a human's name: the wallets that human declares. Read by `resolveSubject`. */
export const HUMAN_SUBJECTS_RECORD = 'corroborate.subjects';
export const ENS_REGISTRY_ABI = parseAbi([
    'function owner(bytes32 node) view returns (address)',
    'function resolver(bytes32 node) view returns (address)',
    'event NewOwner(bytes32 indexed node, bytes32 indexed label, address owner)',
]);
/** `keccak256("NewOwner(bytes32,bytes32,address)")`. */
export const NEW_OWNER_TOPIC = '0xce0457fe73731f824cc272376169235128c118b49d344817417c6d108d155e82';
const lower = (s) => s.toLowerCase();
/** `humanId` for an address-shaped human. Namespaced so it cannot collide with AgentBook's. */
export const ensHumanId = (address) => `ens-human:${lower(address)}`;
/** Split a comma-separated text record. Tolerates spaces, trailing commas and empties. */
function splitRecord(value) {
    return (value ?? '')
        .split(',')
        .map((s) => s.trim())
        .filter((s) => s.length > 0);
}
function safeNormalize(name) {
    try {
        return normalize(name.trim());
    }
    catch {
        return undefined;
    }
}
/** Read a text record, treating any failure as absent-but-noted rather than as an exception. */
async function readText(client, name, key) {
    try {
        const value = await client.getEnsText({ name, key });
        return value === null || value === undefined ? {} : { value };
    }
    catch (e) {
        return { error: e instanceof Error ? e.message : String(e) };
    }
}
async function readAddr(client, name) {
    try {
        const value = await client.getEnsAddress({ name });
        return value ? { value: getAddress(value) } : {};
    }
    catch (e) {
        return { error: e instanceof Error ? e.message : String(e) };
    }
}
async function readOwner(client, node, registry) {
    try {
        const owner = (await client.readContract({
            address: registry,
            abi: ENS_REGISTRY_ABI,
            functionName: 'owner',
            args: [node],
        }));
        return owner === '0x0000000000000000000000000000000000000000' ? undefined : getAddress(owner);
    }
    catch {
        return undefined;
    }
}
/**
 * Resolve one agent name into an identity, plus the human it names.
 *
 * Four reads on the agent side (addr, `corroborate.human`, registry owner) and three on the
 * human side (addr, subjects, acknowledgements), issued concurrently within each side. The
 * human side cannot start until the agent's record names it, which is the one unavoidable
 * round trip.
 */
export async function resolveEnsAgent(client, rawName, opts = {}) {
    const registry = opts.registry ?? ENS_REGISTRY;
    const name = safeNormalize(rawName);
    if (name === undefined) {
        return {
            name: rawName,
            node: '0x',
            binding: 'unreadable',
            caveats: [],
            error: `not a valid ENS name: "${rawName}"`,
        };
    }
    const node = namehash(name);
    const [addr, humanRecord, owner] = await Promise.all([
        readAddr(client, name),
        readText(client, name, AGENT_HUMAN_RECORD),
        readOwner(client, node, registry),
    ]);
    const identity = { name, node, binding: 'unbound', caveats: [] };
    if (addr.value)
        identity.agent = addr.value;
    if (owner)
        identity.owner = owner;
    const created = opts.createdAtBlock?.get(name);
    if (created !== undefined)
        identity.createdAtBlock = created;
    if (addr.error && humanRecord.error) {
        // Both reads failed: this is a transport problem, not a statement about the name.
        identity.binding = 'unreadable';
        identity.error = `ENS resolution failed: ${addr.error}`;
        return identity;
    }
    if (!identity.agent) {
        identity.caveats.push({
            code: 'agent-name-has-no-address',
            message: `${name} carries no addr record, so the name does not say which wallet the agent is. Nothing here authenticates the presenter either way — see agent-presenter-not-authenticated.`,
        });
    }
    if (humanRecord.error) {
        identity.binding = 'unreadable';
        identity.error = `could not read ${AGENT_HUMAN_RECORD} on ${name}: ${humanRecord.error}`;
        return identity;
    }
    const declared = (humanRecord.value ?? '').trim();
    if (declared === '') {
        identity.caveats.push({
            code: 'agent-declares-no-human',
            message: `${name} carries no ${AGENT_HUMAN_RECORD} record, so it names no human. An agent nobody stands behind cannot be capped per human — the policy has to say what to do with it.`,
        });
        return identity;
    }
    identity.human = await resolveEnsHuman(client, declared);
    if (identity.human.error) {
        identity.binding = 'unreadable';
        return identity;
    }
    const acknowledged = identity.human.acknowledges.includes(name);
    identity.binding = acknowledged ? 'mutual' : 'agent-asserted';
    if (acknowledged) {
        identity.caveats.push({
            code: 'agent-human-binding-mutual',
            message: `${name} names ${identity.human.declared} as its human, and that name's ${HUMAN_AGENTS_RECORD} record lists ${name} back. Both records were written by the keys that control those names; neither proves the human is a distinct person, and the wallets that human declares are still self-asserted.`,
        });
    }
    else {
        identity.caveats.push({
            code: 'agent-human-binding-asserted',
            message: `${name} names ${identity.human.declared} as its human, and that human has not acknowledged it (${HUMAN_AGENTS_RECORD} ${identity.human.name ? `on ${identity.human.name} does not list this name` : 'cannot exist on a bare address'}). The claim is one-way: this agent may be riding a stranger's credentials, and a per-human cap does not bind an operator that names a different address for each agent.`,
        });
    }
    return identity;
}
/** Resolve the human side of a binding: a bare address, or a name with records. */
export async function resolveEnsHuman(client, declared) {
    const trimmed = declared.trim();
    // A bare address can hold no records, so it can never acknowledge anything. That is a
    // property of the identifier, not a failure, and the binding stays one-way for good.
    if (isAddress(trimmed, { strict: false })) {
        const address = getAddress(trimmed);
        return { declared: trimmed, address, subjects: [address], acknowledges: [], humanId: ensHumanId(address) };
    }
    const name = safeNormalize(trimmed);
    if (name === undefined) {
        return {
            declared: trimmed,
            subjects: [],
            acknowledges: [],
            humanId: `ens-human-unresolved:${lower(trimmed)}`,
            error: `${AGENT_HUMAN_RECORD} is neither an address nor a valid ENS name: "${trimmed}"`,
        };
    }
    const [addr, subjects, agents] = await Promise.all([
        readAddr(client, name),
        readText(client, name, HUMAN_SUBJECTS_RECORD),
        readText(client, name, HUMAN_AGENTS_RECORD),
    ]);
    if (addr.error && subjects.error && agents.error) {
        return {
            declared: trimmed,
            name,
            subjects: [],
            acknowledges: [],
            humanId: `ens-human-name:${name}`,
            error: `could not resolve ${name}: ${addr.error}`,
        };
    }
    const declaredSubjects = splitRecord(subjects.value)
        .filter((s) => isAddress(s, { strict: false }))
        .map((s) => getAddress(s));
    const set = new Set();
    const all = [];
    for (const a of [...(addr.value ? [addr.value] : []), ...declaredSubjects]) {
        if (set.has(lower(a)))
            continue;
        set.add(lower(a));
        all.push(a);
    }
    const acknowledges = splitRecord(agents.value)
        .map((n) => safeNormalize(n))
        .filter((n) => n !== undefined);
    return {
        declared: trimmed,
        name,
        ...(addr.value ? { address: addr.value } : {}),
        subjects: all,
        acknowledges,
        // Two names pointing at one address are one human. Canonicalising on the address is what
        // stops "name it twice" from being a way to hold two slots.
        humanId: addr.value ? ensHumanId(addr.value) : `ens-human-name:${name}`,
    };
}
/** Resolve several agent names concurrently. Never rejects; failures ride on each identity. */
export async function resolveEnsAgents(client, names, opts = {}) {
    return Promise.all(names.map((n) => resolveEnsAgent(client, n, opts)));
}
// ------------------------------------------------------------------ feeding the fleet engine
/**
 * Turn resolved identities into fleet-policy input.
 *
 * The mapping is where the honesty lives:
 *
 *   - a `mutual` binding is `attested` — both keys wrote a record;
 *   - an `agent-asserted` binding is `asserted`, and a policy with `requireAttestedBinding`
 *     refuses it before it can consume anyone's slot;
 *   - `unreadable` is `unknown`, which the engine turns into `indeterminate`, never a denial;
 *   - `unbound` is `unbacked`, and what happens to it is the policy's `unbackedAgents` choice.
 *
 * An agent whose name carries no `addr` record still needs an address to be keyed on. Its node
 * hash is used, prefixed, so two such agents remain distinguishable and neither is mistaken for
 * a wallet.
 */
export function toFleetAgents(identities) {
    // Several names can carry the same `addr` record, and in ENS that is ordinary rather than
    // exotic — a name is an identity, a wallet is a key, and one key can be named many times.
    // The fleet engine keys agents by address, so two names for one wallet must be collapsed
    // here or the wallet is judged twice and the trace shows only the last verdict.
    const byWallet = new Map();
    const out = [];
    for (const id of identities) {
        if (!id.agent) {
            out.push(fleetAgentOf([id], `0xname:${id.node.slice(2, 42)}`));
            continue;
        }
        const key = lower(id.agent);
        if (!byWallet.has(key))
            byWallet.set(key, []);
        byWallet.get(key).push(id);
    }
    for (const group of byWallet.values())
        out.push(fleetAgentOf(group, group[0].agent));
    return out;
}
function fleetAgentOf(group, agent) {
    const label = group.map((g) => g.name).join(' + ');
    const blocks = group.map((g) => g.createdAtBlock).filter((b) => b !== undefined);
    const registeredAtBlock = blocks.length ? Math.min(...blocks) : undefined;
    const humanIds = new Set(group.filter((g) => g.human && !g.human.error).map((g) => g.human.humanId));
    let backing;
    if (humanIds.size > 1) {
        // Two names, one wallet, two different humans. One of them is lying and nothing here can
        // say which, so this is not a fact about a person — it is a contradiction, and the engine's
        // answer to "we do not know" is `indeterminate`, never a guess in either direction.
        backing = {
            status: 'unknown',
            error: `contradictory bindings: ${group
                .filter((g) => g.human)
                .map((g) => `${g.name} names ${g.human.declared}`)
                .join('; ')}`,
        };
    }
    else if (group.some((g) => g.binding === 'unreadable')) {
        const failed = group.find((g) => g.binding === 'unreadable');
        backing = { status: 'unknown', error: failed.error ?? failed.human?.error ?? 'ENS records unreadable' };
    }
    else if (humanIds.size === 0) {
        backing = { status: 'unbacked' };
    }
    else {
        // Any one of the names being acknowledged means this human acknowledged this wallet.
        const mutual = group.find((g) => g.binding === 'mutual');
        const named = mutual ?? group.find((g) => g.human);
        backing = {
            status: 'backed',
            humanId: named.human.humanId,
            binding: mutual ? 'attested' : 'asserted',
            bindingDetail: mutual
                ? `${mutual.human.declared} lists ${mutual.name} in its ${HUMAN_AGENTS_RECORD} record`
                : `${named.name} names ${named.human.declared}, which has not acknowledged it`,
        };
    }
    return {
        agent,
        label,
        backing,
        ...(registeredAtBlock !== undefined ? { registeredAtBlock } : {}),
    };
}
/**
 * The address set to score for each human named in this batch, keyed by `humanId`.
 *
 * One entry per human, not per agent — the credentials belong to the person, and a 27-agent
 * fleet must cost one lookup rather than 27.
 */
export function humanAddressSets(identities) {
    const sets = new Map();
    for (const id of identities) {
        const human = id.human;
        if (!human || human.error || human.subjects.length === 0)
            continue;
        if (!sets.has(human.humanId))
            sets.set(human.humanId, human.subjects);
    }
    return sets;
}
/**
 * Humans in this batch that declare a wallet another declared human also claims.
 *
 * Reported, never merged. Two declared humans sharing a wallet is the visible signature of one
 * operator holding two slots — but the sets are self-asserted, so acting on the overlap would
 * let anyone merge themselves into a stranger by copying their record. The counterparty gets
 * the observation and decides.
 */
export function sharedWalletHumans(identities) {
    const byWallet = new Map();
    for (const id of identities) {
        if (!id.human || id.human.error)
            continue;
        for (const w of id.human.subjects) {
            const key = lower(w);
            if (!byWallet.has(key))
                byWallet.set(key, { wallet: w, humanIds: new Set() });
            byWallet.get(key).humanIds.add(id.human.humanId);
        }
    }
    return [...byWallet.values()]
        .filter((v) => v.humanIds.size > 1)
        .map((v) => ({ wallet: v.wallet, humanIds: [...v.humanIds] }));
}
/** Caveats about the batch as a whole, to sit beside the fleet decision's own. */
export function ensBatchCaveats(identities) {
    const caveats = [];
    caveats.push({
        code: 'agent-presenter-not-authenticated',
        message: 'An ENS name says which wallet an agent is; it does not establish that the party presenting the name controls that wallet. That is a signature check the counterparty runs itself — the World AgentKit flow in this repo does it with CAIP-122.',
    });
    const wallets = new Map();
    for (const id of identities) {
        if (!id.agent)
            continue;
        const key = lower(id.agent);
        if (!wallets.has(key))
            wallets.set(key, []);
        wallets.get(key).push(id.name);
    }
    const doubled = [...wallets.entries()].filter(([, names]) => names.length > 1);
    if (doubled.length) {
        caveats.push({
            code: 'one-wallet-presented-under-several-names',
            message: `${doubled.length} wallet(s) are named by more than one name in this batch (${doubled
                .map(([w, names]) => `${w}: ${names.join(', ')}`)
                .join('; ')}). They are judged once, as one agent: a per-human cap counts agents, and a second name for the same key is not a second agent.`,
        });
    }
    const shared = sharedWalletHumans(identities);
    if (shared.length) {
        caveats.push({
            code: 'declared-humans-share-a-wallet',
            message: `${shared.length} wallet(s) are declared by more than one human in this batch (${shared
                .map((s) => `${s.wallet} by ${s.humanIds.length}`)
                .join(', ')}). Distinct humanIds holding separate agent slots while claiming the same wallet is what minting humans looks like. The sets are self-asserted, so this is reported and not acted on.`,
        });
    }
    return caveats;
}
/**
 * Sepolia endpoints that answer a wide `eth_getLogs`, measured 2026-07-25, keyless.
 *
 * `ethereum-sepolia-rpc.publicnode.com` — the endpoint the rest of this SDK uses for Sepolia
 * state — refuses any historical range outright ("Archive requests require a personal token"),
 * which is the honest failure mode. `sepolia.drpc.org` serves ranges up to 10,000 blocks and
 * errors above that rather than truncating.
 */
export const SEPOLIA_LOG_ENDPOINTS = [{ url: 'https://sepolia.drpc.org', maxRange: 10_000 }];
/**
 * Enumerate the subnames of a name tree from the registry's own `NewOwner` log.
 *
 * ## The label preimage is not on chain, and that is not a bug we can fix
 *
 * `setSubnodeRecord(parentNode, labelhash, …)` takes the *hash* of the label. Nothing in the
 * transaction, the event or the resulting state contains the string. So a tree can be
 * enumerated — you learn exactly how many subnames exist, who owns them and when they appeared
 * — but the names themselves cannot be recovered from the chain. Candidate labels are hashed
 * and matched; anything left over is reported as an unnamed subnode.
 *
 * That is still the useful direction for a counterparty. Being shown two agents and learning
 * from the registry that the tree holds three tells you something the operator did not: the
 * count is what matters, and the count is exact within the scanned window.
 *
 * ## Under-reporting is the safe direction here, so there is no canary
 *
 * `agentbook.ts` refuses to use an endpoint until it has proved it can see a log we know
 * exists, because an empty AgentBook index makes every human look like they run one agent —
 * permissive. Here the scan only ever *adds* agents to what the counterparty was already
 * shown, so an endpoint that returns too few results degrades to "we saw what you presented"
 * rather than to a false clean bill of health. The window is reported and the count is
 * documented as a lower bound.
 */
export async function scanNameTree(parentName, opts = {}) {
    const parent = safeNormalize(parentName) ?? parentName;
    const parentNode = namehash(parent);
    const endpoints = opts.endpoints ?? SEPOLIA_LOG_ENDPOINTS;
    const registry = (opts.registry ?? ENS_REGISTRY).toLowerCase();
    const caveats = [];
    let lastError = 'no endpoint configured';
    for (const endpoint of endpoints) {
        try {
            const head = Number(await rpc(endpoint.url, 'eth_blockNumber', []));
            const from = opts.fromBlock ?? Math.max(0, head - (opts.blocks ?? 45_000));
            const found = new Map();
            let calls = 0;
            for (let start = from; start <= head; start += endpoint.maxRange) {
                const end = Math.min(head, start + endpoint.maxRange - 1);
                const logs = await rpc(endpoint.url, 'eth_getLogs', [
                    {
                        address: registry,
                        topics: [NEW_OWNER_TOPIC, parentNode],
                        fromBlock: `0x${start.toString(16)}`,
                        toBlock: `0x${end.toString(16)}`,
                    },
                ]);
                calls++;
                for (const log of logs) {
                    const labelhash = log.topics[2];
                    // A subnode can be re-created; the latest event is the one that describes it now.
                    found.set(labelhash, {
                        labelhash,
                        node: keccak256(`${parentNode}${labelhash.slice(2)}`),
                        owner: getAddress(`0x${log.data.slice(26, 66)}`),
                        block: Number(log.blockNumber),
                    });
                }
            }
            const byHash = new Map();
            for (const label of opts.candidateLabels ?? []) {
                const normalized = safeNormalize(label);
                if (normalized === undefined)
                    continue;
                // Callers hold full names; the registry holds one label. Take the leftmost part.
                const leaf = normalized.endsWith(`.${parent}`)
                    ? normalized.slice(0, -(parent.length + 1)).split('.').pop()
                    : normalized;
                byHash.set(keccak256(toBytes(leaf)), leaf);
            }
            const subnodes = [...found.values()].sort((a, b) => a.block - b.block);
            for (const s of subnodes) {
                const label = byHash.get(s.labelhash);
                if (label) {
                    s.label = label;
                    s.name = `${label}.${parent}`;
                }
            }
            const named = subnodes.filter((s) => s.name !== undefined);
            const unnamed = subnodes.filter((s) => s.name === undefined);
            if (unnamed.length) {
                caveats.push({
                    code: 'name-tree-holds-unnamed-subnodes',
                    message: `${parent} holds ${unnamed.length} subname(s) beyond the ${named.length} identified here. ENS stores the label hash, never the label, so they can be counted and dated but not named. A counterparty shown ${named.length} agents from this tree has been shown ${named.length} of at least ${subnodes.length}.`,
                });
            }
            caveats.push({
                code: 'name-tree-scan-window',
                message: `Subnames were read from the registry's NewOwner log between blocks ${from} and ${head} via ${new URL(endpoint.url).host}. Subnames created before block ${from} are not in this count, so ${subnodes.length} is a lower bound. The scan only ever reveals agents beyond those presented, so a short window degrades to the presented set rather than to a false clean result.`,
            });
            return {
                parent,
                parentNode,
                subnodes,
                named,
                unnamed,
                coverage: { fromBlock: from, toBlock: head, endpoint: new URL(endpoint.url).host, calls },
                caveats,
            };
        }
        catch (e) {
            lastError = e instanceof Error ? e.message : String(e);
        }
    }
    return {
        parent,
        parentNode,
        subnodes: [],
        named: [],
        unnamed: [],
        coverage: { fromBlock: 0, toBlock: 0, endpoint: 'none', calls: 0 },
        caveats: [
            {
                code: 'name-tree-scan-unavailable',
                message: `The registry's subnode log could not be read (${lastError}), so this tree was not enumerated. Only the agents actually presented are known; siblings under the same parent are invisible.`,
            },
        ],
    };
}
/** Creation blocks keyed by full name, for `resolveEnsAgent`'s `createdAtBlock`. */
export function creationBlocks(scan) {
    const map = new Map();
    for (const s of scan.named)
        if (s.name)
            map.set(s.name, s.block);
    return map;
}
async function rpc(url, method, params) {
    const res = await fetch(url, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ jsonrpc: '2.0', id: 1, method, params }),
        signal: AbortSignal.timeout(20_000),
    });
    if (!res.ok)
        throw new Error(`HTTP ${res.status} from ${new URL(url).host}`);
    const body = (await res.json());
    if (body.error)
        throw new Error(body.error.message ?? `${method} failed`);
    if (body.result === undefined)
        throw new Error(`${method} returned no result`);
    return body.result;
}
