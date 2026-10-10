#!/usr/bin/env python3
"""Independent verifier for SwarmMemo's trust runs (RFC0012 §4.8). Stdlib only.

Three commands:

  recompute.py verify EXPORT.jsonl [--service swarmmemo.com]
      Checks every line of /v1/export?stream=endorsements offline: the Ed25519
      signature over signed_payload with public_key (a pure-Python RFC 8032
      verifier, cofactorless like Go's crypto/ed25519), and that the signed
      command is the record (service, operation, message or target, value,
      sponsor). legacy_vote and unsigned records carry weight 0 and are counted,
      not verified. Exit status 1 if any signed record fails.

  recompute.py endorsements EXPORT.jsonl [--service swarmmemo.com]
      Verifies the export as above and prints each record as a trust input
      line ({"type":"endorsement",...}); unsigned votes become kind "unsigned".

  recompute.py run SNAPSHOT.jsonl
      Recomputes a trust run from its inputs and prints the output as canonical
      JSON (sorted keys, no spaces), which must equal the published run byte for
      byte. The input is the run's snapshot: one JSON record per line, typed
      meta, params, account, post, endorsement, proof, breaker, transfer, claim,
      prior, penalty and sponsorship, and from parameter version 2 edge and
      spend, the format of internal/trust's golden fixtures
      (testdata/golden_inputs.jsonl, testdata/golden_standing_v2_inputs.jsonl,
      testdata/golden_standing_v3_inputs.jsonl, testdata/golden_standing_v4_inputs.jsonl,
      testdata/golden_standing_inputs.jsonl). From parameter version 3 a proof
      may carry registered_at and a spend to and link_value (its payee); from
      version 4 a vouch may carry weight; from version 5 a proof of an assessed
      kind (wallet, github, pow) may carry root and assessed.

The algorithm is the published one (RFC0012 §4.2–4.5), restated here from the
specification rather than translated from the Go reference, and checked against
the reference's golden fixtures by test_recompute.py. From parameter version 2
the run also computes standing (RFC0015 §3): seeded personalized PageRank over
endorse edges, with oppose edges subtracted locally (compute_standing()); version
3 (standing.rule 1) returns a dangling node's pass to the seeds, reports
c / (1 − pass), leaves self-dealt spend out of the seed, caps spend's seed and
prices a domain by its registration age when known; version 4 (rule 2) gives
each act its kind's absolute weight, passes only pass × W / (K + W) along edges
of total weight W (the rest returns to the seeds) and reports c; version 6
(rule 3) replaces the propagation with stakes: a vouch, an accepted work item and
a verified witness move a share of the endorser's own standing to the target, and
votes, witness verdicts and those acts are positions settled by later independent
endorsement, losers paying winners within a pot (run_stakes()). Every amount is an
integer; divisions truncate toward zero as Go's do; curves are published daily
factors applied one whole day at a time with floor. The flow is Dinic's
algorithm specified down to edge insertion order and the path search, since a
maximum flow's split over sinks is not unique.
"""

from __future__ import annotations

import argparse
import base64
import hashlib
import json
import math
import sys

DAY = 86400
PPM = 1_000_000


# ---------------------------------------------------------------------------
# Integer and JSON helpers


def gdiv(a: int, b: int) -> int:
    """Go's integer division: truncates toward zero."""
    q = abs(a) // abs(b)
    return q if (a >= 0) == (b > 0) else -q


def day_of(t: int) -> int:
    return -gdiv(-t + DAY - 1, DAY) if t < 0 else t // DAY


def canonical(value) -> str:
    """Canonical JSON: sorted keys, no whitespace, UTF-8, no HTML escaping."""
    text = json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False)
    return text.replace("\u2028", "\\u2028").replace("\u2029", "\\u2029")


def _go_quote(s: str) -> str:
    """A string as Go's json.Marshal writes it (HTML characters escaped)."""
    text = json.dumps(s, ensure_ascii=False)
    for raw, esc in (("<", "\\u003c"), (">", "\\u003e"), ("&", "\\u0026"), ("\u2028", "\\u2028"), ("\u2029", "\\u2029")):
        text = text.replace(raw, esc)
    return text


# Record fields in the reference's declaration order, with their zero values:
# a record's sort key is its json.Marshal form, every field omitempty but type.
RECORD_FIELDS = [
    ("type", ""), ("schema", 0), ("as_of", 0), ("prior_runs", 0), ("pause_new_keys_since", 0), ("events_seq", 0),
    ("endorsements_seq", 0), ("ledger_seq", 0), ("version", 0), ("body", None), ("account", ""), ("created_at", 0),
    ("first_seen", 0), ("id", ""), ("reply_to", ""), ("reply_to_account", ""), ("seq", 0), ("kind", ""), ("voter", ""),
    ("target", ""), ("message_id", ""), ("value", 0), ("sponsor", False), ("weight", 0), ("link_value", ""), ("state", ""),
    ("checked_at", 0), ("link_account", ""), ("registered_at", 0), ("root", ""), ("assessed", 0), ("started_at", 0), ("trust_until", 0), ("from", ""), ("to", ""),
    ("amount", 0), ("day", 0), ("claimed", 0), ("spent", 0), ("flow_sum", 0), ("standing", False), ("evidence", ""),
    ("fraction_ppm", 0), ("ends_at", 0), ("invitee", ""), ("sponsor_account", ""), ("high_water", 0),
]
FIELD_ZERO = dict(RECORD_FIELDS)
RECORD_TYPES = {"meta", "params", "account", "post", "endorsement", "proof", "breaker", "transfer", "claim", "prior",
                "penalty", "sponsorship", "edge", "spend"}


def record_key(r: dict) -> str:
    parts = []
    for name, zero in RECORD_FIELDS:
        value = r.get(name, zero)
        if name != "type" and (value == zero or value is None):
            continue
        if isinstance(value, bool):
            encoded = "true" if value else "false"
        elif isinstance(value, int):
            encoded = str(value)
        elif isinstance(value, str):
            encoded = _go_quote(value)
        else:
            encoded = json.dumps(value, separators=(",", ":"), ensure_ascii=False)
        parts.append(_go_quote(name) + ":" + encoded)
    return "{" + ",".join(parts) + "}"


class Rec(dict):
    """A trust input record; absent fields read as their zero value."""

    def __getattr__(self, name):
        return self.get(name, FIELD_ZERO.get(name))


# ---------------------------------------------------------------------------
# Snapshot and parameters


class InputError(ValueError):
    pass


PARAM_KEYS = {
    "schema", "collateral_unit", "seeds", "seeds_reason", "service_accounts", "proofs", "domain_suffixes",
    "proof_fresh_days", "history", "window_days", "n_max", "k_out", "edges", "unit_per_share", "edge_cap_ppm",
    "lambda_ppm", "root_cap_shares", "fill_steps", "own_avg_runs", "active_days", "activity_days", "seeds_b",
    "max_work", "max_seconds", "theta_trusted", "theta_proven", "endorsement_unit_price", "weight_cap_ppm",
    "weight_per_unit_ppm", "detectors", "liability", "sponsor",
}
PARAM_SECTIONS = {
    "history": {"day_price", "days_cap", "half_life_days", "day_factor_ppm"},
    "seeds_b": {"min_roots", "theta_anchor", "min_age_days", "active_days", "of_days", "min_members"},
    "detectors": {"version", "funnel_k", "funnel_days", "funnel_spend_ppm", "ring_min", "ring_inside_ppm"},
    "liability": {"phi_ppm", "phi_max_ppm", "penalty_days", "edge_days"},
    "sponsor": {"window_days", "slots_per_share", "dividend_ppm", "dividend_days", "daily_cap", "resource"},
}
PROOF_KEYS = {"forge", "rent", "curve", "half_life_days", "day_factor_ppm"}
# From parameter version 5: the optional assessed rows and the rule of each.
ASSESSED_KINDS = {"wallet": "capped", "github": "capped", "pow": "saturating"}
ASSESSED_MAX = 10 ** 15
STANDING_KEYS = {"mode", "half_life_days", "day_factor_ppm", "pass_ppm", "iterations", "arbiter_seed_cents", "anon_seed_cents",
                 "credits_per_cent", "theta1_cents", "theta2_cents", "v0_ppm", "c_ref_cents"}
# From parameter version 3 (the simulation's fixes); each is omitted when zero.
STANDING_KEYS_V3 = {"rule", "v_floor_cents", "spend_cap_cents", "funded_days"}
# From parameter version 4 (rule 2, absolute weights).
STANDING_KEYS_V4 = {"keep_weight", "edge_weights", "vouch_weight_max"}
# From parameter version 6 (rule 3, endorsements are stakes).
STANDING_KEYS_V6 = {"stake_ppm", "stake_budget_ppm", "prior_min_posts"}
EDGE_WEIGHT_KINDS = {"vote", "down_vote", "vouch", "work_accept", "witness"}
EDGE_KEYS = {"base_ppm", "half_life_days", "day_factor_ppm"}


def check_params(body: dict) -> None:
    """Strict shape check: every field present, none unknown, integers where
    integers belong. Bounds are the reference's to publish; a verifier only
    needs to refuse what it cannot read the same way."""
    def exact(obj, keys, where):
        if not isinstance(obj, dict) or set(obj) != keys:
            raise InputError(f"params {where}: fields must be exactly {sorted(keys)}")
    if "standing" in body:
        exact(body, PARAM_KEYS | {"standing"}, "body")
        st = body["standing"]
        extra = STANDING_KEYS_V3 | STANDING_KEYS_V4 | STANDING_KEYS_V6
        if not isinstance(st, dict) or not STANDING_KEYS <= set(st) <= STANDING_KEYS | extra:
            raise InputError(f"params standing: fields must be {sorted(STANDING_KEYS)}, plus any of {sorted(extra)}")
        if body["standing"]["mode"] not in ("shadow", "active"):
            raise InputError("params standing.mode must be shadow or active")
        if st.get("rule", 0) >= 2:
            if set(st) & STANDING_KEYS_V4 != STANDING_KEYS_V4:
                raise InputError(f"params standing: rule 2 needs {sorted(STANDING_KEYS_V4)}")
            exact(st["edge_weights"], EDGE_WEIGHT_KINDS, "standing.edge_weights")
        elif set(st) & STANDING_KEYS_V4:
            raise InputError(f"params standing: {sorted(STANDING_KEYS_V4)} need rule 2")
        if st.get("rule", 0) >= 3:
            if set(st) & STANDING_KEYS_V6 != STANDING_KEYS_V6:
                raise InputError(f"params standing: rule 3 needs {sorted(STANDING_KEYS_V6)}")
        elif set(st) & STANDING_KEYS_V6:
            raise InputError(f"params standing: {sorted(STANDING_KEYS_V6)} need rule 3")
    else:
        exact(body, PARAM_KEYS, "body")
    for section, keys in PARAM_SECTIONS.items():
        exact(body[section], keys, section)
    for kind in ("domain", "ed25519", "board", "url", "nostr"):
        exact(body["proofs"].get(kind), PROOF_KEYS, "proofs." + kind)
    for kind, rule in ASSESSED_KINDS.items():
        if kind in body["proofs"]:
            exact(body["proofs"][kind], PROOF_KEYS | {"assess", "units_per_cent"}, "proofs." + kind)
            if body["proofs"][kind]["assess"] != rule or not isinstance(body["proofs"][kind]["units_per_cent"], int) \
                    or body["proofs"][kind]["units_per_cent"] < 1:
                raise InputError(f"params proofs.{kind}: assess must be {rule} with a positive units_per_cent")
    if not {"domain", "ed25519", "board", "url", "nostr"} <= set(body["proofs"]) <= {"domain", "ed25519", "board", "url", "nostr"} | set(ASSESSED_KINDS):
        raise InputError("params proofs: exactly domain, ed25519, board, url and nostr, and optionally wallet, github and pow")
    for kind in ("vote", "vouch", "reply", "legacy_vote"):
        exact(body["edges"].get(kind), EDGE_KEYS, "edges." + kind)
    if set(body["edges"]) != {"vote", "vouch", "reply", "legacy_vote"}:
        raise InputError("params edges: exactly vote, vouch, reply and legacy_vote")
    if body["schema"] != 1:
        raise InputError("params schema must be 1")

    def ints(obj, where):
        for k, v in obj.items():
            if isinstance(v, dict):
                ints(v, where + "." + k)
            elif isinstance(v, bool) or (isinstance(v, float)):
                raise InputError(f"params {where}.{k} must be an integer")
    ints(body, "body")


class Snapshot:
    def __init__(self):
        self.meta = Rec()
        self.params = None
        self.params_version = 0
        self.lists = {t: [] for t in RECORD_TYPES - {"meta", "params"}}

    def __getattr__(self, name):
        lists = self.__dict__.get("lists", {})
        singular = {"accounts": "account", "posts": "post", "endorsements": "endorsement", "proofs": "proof",
                    "breakers": "breaker", "transfers": "transfer", "claims": "claim", "priors": "prior",
                    "penalties": "penalty", "sponsorships": "sponsorship", "acts": "edge", "spends": "spend"}
        if name in singular:
            return lists[singular[name]]
        raise AttributeError(name)


def read_snapshot(lines) -> Snapshot:
    snap = Snapshot()
    for n, line in enumerate(lines, 1):
        line = line.strip()
        if not line:
            continue
        try:
            raw = json.loads(line)
        except json.JSONDecodeError as e:
            raise InputError(f"line {n}: {e}") from e
        if not isinstance(raw, dict) or raw.get("type") not in RECORD_TYPES:
            raise InputError(f"line {n}: unknown record type {raw.get('type') if isinstance(raw, dict) else raw!r}")
        unknown = set(raw) - set(FIELD_ZERO)
        if unknown:
            raise InputError(f"line {n}: unknown field(s) {sorted(unknown)}")
        for k, v in raw.items():
            zero = FIELD_ZERO[k]
            if k == "body":
                continue
            if isinstance(zero, bool) != isinstance(v, bool) or type(v) is not type(zero):
                raise InputError(f"line {n}: field {k} has the wrong type")
        rec = Rec(raw)
        if rec.type == "meta":
            snap.meta = rec
        elif rec.type == "params":
            check_params(rec.body)
            snap.params = rec.body
            snap.params_version = rec.version
        else:
            snap.lists[rec.type].append(rec)
    if snap.params is None:
        raise InputError("the snapshot has no params record")
    return snap


# ---------------------------------------------------------------------------
# Curves, roots


def assessed_contribution(price, assessed):
    """What an assessed root adds, in cents: min(cap, v) (capped) or
    cap * v / (v + cap) (saturating), v = assessed / units_per_cent, exact."""
    u = price.get("units_per_cent", 0)
    if assessed <= 0 or u <= 0:
        return 0
    assessed = min(assessed, ASSESSED_MAX)
    ceiling = min(price["forge"], price["rent"])
    if price["assess"] == "capped":
        return min(ceiling, assessed // u)
    if price["assess"] == "saturating":
        return ceiling * assessed // (assessed + ceiling * u) if ceiling > 0 else 0
    return 0


class Curves:
    """1e6 × f^days, one whole day at a time with floor; ages past 3650 days
    use 3650. The run never evaluates a float."""

    def __init__(self):
        self.tables = {}

    def decay(self, factor: int, days: int) -> int:
        if days <= 0:
            return PPM
        days = min(days, 3650)
        table = self.tables.setdefault(factor, [PPM])
        while len(table) <= days:
            table.append(table[-1] * factor // PPM)
        return table[days]

    def ramp(self, factor: int, days: int) -> int:
        return PPM - self.decay(factor, days)


class UnionFind(dict):
    """Union-find over strings; the smaller representative wins a union."""

    def find(self, x):
        while True:
            p = self.get(x)
            if p is None or p == x:
                return x
            gp = self.get(p, "")
            if gp != "" and gp != p:
                self[x] = gp
            x = p

    def union(self, a, b):
        ra, rb = self.find(a), self.find(b)
        if ra == rb:
            return
        if rb < ra:
            ra, rb = rb, ra
        self[ra] = ra
        self[rb] = ra


def domain_root(domain: str, suffixes: set) -> str:
    """"domain:" + one label under the longest listed multi-label public
    suffix, else the last two labels."""
    d = domain.lower()
    if d.endswith("."):
        d = d[:-1]
    labels = d.split(".")
    keep = 2
    for i in range(1, len(labels) - 1):
        if ".".join(labels[i:]) in suffixes:
            keep = len(labels) - i + 1
            break
    keep = min(keep, len(labels))
    return "domain:" + ".".join(labels[len(labels) - keep:])


# ---------------------------------------------------------------------------
# Dinic max-flow, specified exactly


class WorkExceeded(RuntimeError):
    pass


class Network:
    """add(u, v, c) appends edge e = u→v (capacity c) and its reverse e^1
    (capacity 0) to u's and v's lists. A phase is a BFS from s over each
    node's list in insertion order, then repeated single-path searches with a
    per-node pointer: at u take the first edge from it[u] on with residual > 0
    into level[u]+1; at a dead end step back and advance the parent's pointer;
    reaching t push the bottleneck. Every edge examined counts as work."""

    def __init__(self, n: int, max_work: int, work: int):
        self.adj = [[] for _ in range(n)]
        self.to = []
        self.cap = []
        self.work = work
        self.max_work = max_work

    def add(self, u: int, v: int, c: int) -> int:
        e = len(self.to)
        self.to += [v, u]
        self.cap += [c, 0]
        self.adj[u].append(e)
        self.adj[v].append(e + 1)
        return e

    def flow_on(self, e: int) -> int:
        return self.cap[e ^ 1]

    def tick(self):
        self.work += 1
        if self.work > self.max_work:
            raise WorkExceeded("max_work exceeded")

    def maxflow(self, s: int, t: int):
        n = len(self.adj)
        while True:
            level = [-1] * n
            level[s] = 0
            queue = [s]
            head = 0
            while head < len(queue):
                u = queue[head]
                head += 1
                for e in self.adj[u]:
                    self.tick()
                    v = self.to[e]
                    if self.cap[e] > 0 and level[v] < 0:
                        level[v] = level[u] + 1
                        queue.append(v)
            if level[t] < 0:
                return
            it = [0] * n
            while self._augment(s, t, level, it) > 0:
                pass

    def _augment(self, s, t, level, it) -> int:
        path = []
        u = s
        while True:
            if u == t:
                f = 1 << 62
                for e in path:
                    f = min(f, self.cap[e])
                for e in path:
                    self.cap[e] -= f
                    self.cap[e ^ 1] += f
                return f
            advanced = False
            edges = self.adj[u]
            while it[u] < len(edges):
                self.tick()
                e = edges[it[u]]
                v = self.to[e]
                if self.cap[e] > 0 and level[v] == level[u] + 1:
                    path.append(e)
                    u = v
                    advanced = True
                    break
                it[u] += 1
            if advanced:
                continue
            if not path:
                return 0
            e = path.pop()
            u = self.to[e ^ 1]
            it[u] += 1


def run_flow(graph: dict, seeds: list, pool: int, max_work: int, work: int):
    """Stake-bounded capacity flow from seeds (sorted node indices): in-node i,
    out-node n+i, hub j at 2n+j, then S and T. Inserted in order: in→out
    transit edges by node (a seed passes the whole pool); graph edges by (src,
    dst); sink edges by node, capacity 0, raised to U×k/steps at level k; hub→T
    edges; S→seed edges, pool/len(seeds) each."""
    n = graph["n"]
    node_flow, edge_flow = [0] * n, [0] * len(graph["src"])
    if not seeds or n == 0:
        return node_flow, edge_flow, work
    hubs = graph["hubs"]
    S, T = 2 * n + hubs, 2 * n + hubs + 1
    net = Network(T + 1, max_work, work)
    seed_set = set(seeds)
    for i in range(n):
        net.add(i, n + i, pool if i in seed_set else graph["transit"][i])
    edge_ids = []
    for k in range(len(graph["src"])):
        c = graph["cap"][k]
        edge_ids.append(net.add(n + graph["src"][k], graph["dst"][k], c) if c > 0 else -1)
    sink = []
    for i in range(n):
        hub = graph["hub"][i]
        sink.append(net.add(i, 2 * n + hub if hub >= 0 else T, 0))
    for j in range(hubs):
        net.add(2 * n + j, T, graph["hub_cap"])
    per_seed = max(1, pool // len(seeds))
    for s in seeds:
        net.add(S, s, per_seed)
    done = 0
    for k in range(1, graph["steps"] + 1):
        level = graph["node_cap"] * k // graph["steps"]
        for e in sink:
            net.cap[e] += level - done
        done = level
        net.maxflow(S, T)
    for i, e in enumerate(sink):
        node_flow[i] = net.flow_on(e)
    for k, e in enumerate(edge_ids):
        if e >= 0:
            edge_flow[k] = net.flow_on(e)
    return node_flow, edge_flow, net.work


# ---------------------------------------------------------------------------
# Detectors


def evidence_id(kind: str, D: int, members: list) -> str:
    digest = hashlib.sha256(f"{kind}\n{D}\n{','.join(members)}".encode()).hexdigest()
    return f"{kind}-{digest[:24]}"


def strongly_connected(n: int, edges: list) -> list:
    """Tarjan's components, visiting nodes 0..n-1 and each node's edges in
    (src, dst) order."""
    adj = [[] for _ in range(n)]
    for src, dst, _ in edges:
        adj[src].append(dst)
    index, low, on = [-1] * n, [0] * n, [False] * n
    stack, out, counter = [], [], [0]

    sys.setrecursionlimit(max(sys.getrecursionlimit(), 4 * n + 1000))

    def visit(v):
        index[v] = low[v] = counter[0]
        counter[0] += 1
        stack.append(v)
        on[v] = True
        for w in adj[v]:
            if index[w] < 0:
                visit(w)
                low[v] = min(low[v], low[w])
            elif on[w]:
                low[v] = min(low[v], index[w])
        if low[v] == index[v]:
            comp = []
            while True:
                w = stack.pop()
                on[w] = False
                comp.append(w)
                if w == v:
                    break
            out.append(comp)

    for v in range(n):
        if index[v] < 0:
            visit(v)
    return out


def detect(p, snap, as_of, D, service, nodes, edges) -> list:
    """funnel: a recipient of transfers from ≥ funnel_k distinct accounts in
    the last funnel_days, each of which spent ≤ funnel_spend_ppm of its claims
    those days. ring: a strongly connected set of ≥ ring_min nodes, each taking
    ≥ ring_inside_ppm of its inbound weight from inside, sharing a member with
    a funnel."""
    d = p["detectors"]
    out = []
    start = as_of - d["funnel_days"] * DAY
    claimed, spent = {}, {}
    for r in snap.claims:
        if D - d["funnel_days"] <= r.day < D:
            claimed[r.account] = claimed.get(r.account, 0) + r.claimed
            spent[r.account] = spent.get(r.account, 0) + r.spent
    senders = {}
    for r in snap.transfers:
        f, t = r["from"] if "from" in r else "", r.to
        if r.created_at < start or r.created_at >= as_of or f == t or f == "" or t == "" or f in service or t in service or r.amount <= 0:
            continue
        senders.setdefault(t, set()).add(f)
    in_funnel = {}
    for to in sorted(senders):
        farm = [s for s in senders[to] if claimed.get(s, 0) > 0 and spent.get(s, 0) * PPM <= d["funnel_spend_ppm"] * claimed[s]]
        if len(farm) < d["funnel_k"]:
            continue
        members = sorted(farm + [to])
        ev = {"id": evidence_id("funnel", D, members), "kind": "funnel", "members": members, "detector_version": d["version"],
              "detail": canonical({"recipient": to, "senders": len(farm), "window_days": d["funnel_days"]})}
        for m in members:
            in_funnel.setdefault(m, ev["id"])
        out.append(ev)
    if not in_funnel:
        return out
    for scc in strongly_connected(len(nodes), edges):
        if len(scc) < d["ring_min"]:
            continue
        inside = set(scc)
        inw, total = {}, {}
        for src, dst, w in edges:
            if dst in inside:
                total[dst] = total.get(dst, 0) + w
                if src in inside:
                    inw[dst] = inw.get(dst, 0) + w
        ok = all(total.get(v, 0) != 0 and inw.get(v, 0) * PPM >= d["ring_inside_ppm"] * total[v] for v in scc)
        members = sorted(nodes[v] for v in scc)
        funnels = [in_funnel[m] for m in members if m in in_funnel]
        if not ok or not funnels:
            continue
        out.append({"id": evidence_id("ring", D, members), "kind": "ring", "members": members, "detector_version": d["version"],
                    "detail": canonical({"size": len(members), "funnel": min(funnels)})})
    out.sort(key=lambda e: e["id"])
    return out


# ---------------------------------------------------------------------------
# The run


def compute(snap: Snapshot) -> dict:
    p = snap.params
    as_of = snap.meta.as_of
    D = day_of(as_of)
    ws = as_of - p["window_days"] * DAY
    U = p["unit_per_share"]
    cv = Curves()
    service = set(p["service_accounts"])
    edges_p = p["edges"]

    first_seen = {}
    for r in snap.accounts:
        if r.first_seen > 0 and (first_seen.get(r.account, 0) == 0 or r.first_seen < first_seen[r.account]):
            first_seen[r.account] = r.first_seen

    # Posts in the window, and activity.
    posts = sorted((r for r in snap.posts if ws <= r.created_at < as_of and r.account != "" and r.account not in service),
                   key=lambda r: (r.created_at, r.id))
    post_by_id, activity, last_seen = {}, {}, {}

    def active(a, t):
        activity.setdefault(a, set()).add(day_of(t))

    def seen(a, t):
        if t > last_seen.get(a, 0):
            last_seen[a] = t

    for r in posts:
        post_by_id[r.id] = r
        active(r.account, r.created_at)
        seen(r.account, r.created_at)

    # Endorsements: the latest record per vote (voter, message) and per vouch
    # (voter, target) decides.
    ends = sorted((r for r in snap.endorsements if r.created_at < as_of and r.voter != "" and r.voter not in service), key=lambda r: r.seq)
    latest_vote, latest_vouch = {}, {}
    for r in ends:
        if r.kind in ("vote", "legacy_vote", "unsigned"):
            latest_vote[(r.voter, r.message_id)] = r
        elif r.kind == "vouch":
            latest_vouch[(r.voter, r.target)] = r
        if r.created_at >= ws:
            active(r.voter, r.created_at)
            seen(r.voter, r.created_at)
            if r.target != "" and r.target not in service:
                seen(r.target, r.created_at)
    current = sorted(list(latest_vote.values()) + list(latest_vouch.values()), key=lambda r: r.seq)
    down_votes = {}
    for r in current:
        if r.kind == "vote" and r.value == -1 and r.created_at >= ws and r.target != "":
            down_votes[r.target] = down_votes.get(r.target, 0) + 1

    # Nodes: seeds first, then the most recently active, up to n_max.
    candidates = sorted((a for a in last_seen if a not in service), key=lambda a: (-last_seen[a], a))
    is_node = {s for s in p["seeds"] if s not in service}
    for a in candidates:
        if len(is_node) >= p["n_max"]:
            break
        is_node.add(a)
    nodes = sorted(is_node)
    index = {a: i for i, a in enumerate(nodes)}

    # Roots: fresh verified domains and attached ed25519 keys join accounts.
    suffixes = set(p["domain_suffixes"])
    proofs = sorted(snap.proofs, key=record_key)
    fresh = as_of - p["proof_fresh_days"] * DAY

    def counts(r):
        if p["proofs"].get(r.kind, {}).get("assess", ""):
            return r.state == "verified" and fresh <= r.checked_at <= as_of and r.created_at <= as_of
        if r.kind == "domain":
            return r.state == "verified" and fresh <= r.checked_at <= as_of and r.created_at <= as_of
        if r.kind == "ed25519":
            return r.state in ("proof_attached", "verified") and r.created_at <= as_of
        return r.state == "verified" and r.created_at <= as_of

    roots = UnionFind()
    for r in proofs:
        if r.account in service or not counts(r):
            continue
        if r.kind == "domain":
            roots.union(r.account, domain_root(r.link_value, suffixes))
        elif r.kind == "ed25519" and r.link_account != "" and r.link_account not in service:
            roots.union(r.account, r.link_account)
    label = {}
    for x in list(roots):
        rep = roots.find(x)
        cur = label.get(rep)
        is_domain = x.startswith("domain:")
        cur_domain = cur is not None and cur.startswith("domain:")
        if cur is None or (is_domain and not cur_domain) or (is_domain == cur_domain and x < cur):
            label[rep] = x

    def root_of(a):
        return label[roots.find(a)] if a in roots else a

    flow_sum, standing = {}, set()
    for r in snap.priors:
        flow_sum[r.account] = flow_sum.get(r.account, 0) + r.flow_sum
        if r.standing:
            standing.add(r.account)

    # Breakers, pause-new-keys and active penalties.
    reset = {r.account for r in snap.breakers if r.started_at <= as_of < r.trust_until}
    since = snap.meta.pause_new_keys_since
    if since > 0:
        reset |= {a for a, t in first_seen.items() if t > since}
    penalty = {}
    for r in snap.penalties:
        if r.ends_at > as_of and r.fraction_ppm > penalty.get(r.account, 0):
            penalty[r.account] = min(r.fraction_ppm, PPM)

    # Edge contributions.
    contribs = []
    for r in current:
        if r.created_at < ws or r.target == "" or r.value != 1:
            continue
        if r.kind in ("vote", "legacy_vote", "vouch"):
            contribs.append((r.voter, r.target, r.kind, r.created_at, edges_p[r.kind]["base_ppm"]))
    replied = set()
    for r in posts:
        if r.reply_to_account == "" or r.reply_to_account == r.account:
            continue
        key = (r.account, r.reply_to_account, day_of(r.created_at))
        if key in replied:
            continue
        replied.add(key)
        contribs.append((r.account, r.reply_to_account, "reply", r.created_at, edges_p["reply"]["base_ppm"]))
    pair_sum, pair_recent, pair_kinds = {}, {}, {}
    recent = as_of - p["liability"]["edge_days"] * DAY
    for src, dst, kind, at, base in contribs:
        if src == dst or src in service or dst in service or src not in is_node or dst not in is_node:
            continue
        if root_of(src) == root_of(dst) or src in reset or penalty.get(src, 0) >= PPM:
            continue
        v = base * cv.decay(edges_p[kind]["day_factor_ppm"], D - day_of(at)) // PPM
        if v <= 0:
            continue
        k = (src, dst)
        pair_sum[k] = pair_sum.get(k, 0) + v
        if at >= recent:
            pair_recent[k] = pair_recent.get(k, 0) + v
        pair_kinds.setdefault(k, set()).add("vote" if kind == "legacy_vote" else kind)

    def saturate(s):
        return PPM * s // (PPM + s)

    by_src = {}
    for (src, dst), s in pair_sum.items():
        w = saturate(s)
        if w > 0:
            by_src.setdefault(index[src], []).append((index[src], index[dst], w))
    edges = []
    for lst in by_src.values():
        lst.sort(key=lambda e: (-e[2], e[1]))
        edges += lst[:p["k_out"]]
    edges.sort(key=lambda e: (e[0], e[1]))

    # Own average and transit, from published runs.
    def active_days(a, lo, hi):
        return sum(1 for d in activity.get(a, ()) if lo <= d < hi)

    prior_runs = snap.meta.prior_runs
    own_avg, transit = [0] * len(nodes), [0] * len(nodes)
    for i, a in enumerate(nodes):
        if prior_runs <= 0:
            continue
        span = p["activity_days"]
        fs = first_seen.get(a, 0)
        if fs > 0:
            span = max(1, min(p["activity_days"], D - day_of(fs)))
        share = min(PPM, gdiv(active_days(a, D - p["activity_days"], D) * PPM, span))
        avg = min(U, gdiv(flow_sum.get(a, 0), prior_runs))
        own_avg[i] = gdiv(avg * share, PPM)
        if a not in reset:
            transit[i] = gdiv(gdiv(p["lambda_ppm"] * own_avg[i], PPM) * (PPM - penalty.get(a, 0)), PPM)

    # Proof collateral: min(forge, rent) × curve(age), saturating per root.
    account_proofs = {}
    for r in proofs:
        if r.account in service or r.created_at > as_of:
            continue
        price = p["proofs"].get(r.kind, {"forge": 0, "rent": 0, "curve": "", "day_factor_ppm": 0})
        part = {"kind": r.kind, "value": r.link_value, "state": r.state, "forge": price["forge"], "rent": price["rent"],
                "curve": price["curve"], "age_days": max(0, D - day_of(r.created_at)), "weight_ppm": 0, "contribution": 0,
                "saturated_by": "", "note": ""}
        if r.kind == "domain":
            part["root"] = domain_root(r.link_value, suffixes)
        elif r.kind == "ed25519":
            part["root"] = "key:" + r.link_account
        else:
            part["root"] = r.kind + ":" + r.link_value
            if price.get("assess", "") and r.root != "":
                part["root"] = r.root
        if not counts(r):
            part["note"] = "not counted in this state"
        elif price.get("assess", ""):
            # An assessed root (version 5): its record carries the value.
            part["weight_ppm"] = PPM
            part["note"] = f"assessed {r.assessed} ({price['assess']})"
            part["contribution"] = assessed_contribution(price, r.assessed)
            account_proofs.setdefault(r.account, []).append(part)
            continue
        elif price["curve"] == "ramp":
            part["weight_ppm"] = min(500000, cv.ramp(price["day_factor_ppm"], part["age_days"]))
            part["note"] = "verified age unknown: at most half weight, bounded by the link's age"
        else:
            part["weight_ppm"] = PPM
        part["contribution"] = min(price["forge"], price["rent"]) * part["weight_ppm"] // PPM
        account_proofs.setdefault(r.account, []).append(part)
    # History: days with a post that drew a reply or an up vote from another
    # root with standing in the previous run.
    qualified = {}

    def qualify(post, by):
        if by == "" or by == post.account or by in service or by not in standing or root_of(by) == root_of(post.account):
            return
        qualified.setdefault(post.account, set()).add(day_of(post.created_at))

    for r in posts:
        if r.reply_to != "" and r.reply_to in post_by_id:
            qualify(post_by_id[r.reply_to], r.account)
    for r in current:
        if r.kind == "vote" and r.value == 1 and r.message_id in post_by_id:
            qualify(post_by_id[r.message_id], r.voter)
    h = p["history"]
    for a, days in qualified.items():
        n = min(len(days), h["days_cap"])
        part = {"kind": "history", "value": f"{len(days)} days", "root": "history:" + a, "state": "computed",
                "forge": n * h["day_price"], "rent": n * h["day_price"], "curve": "ramp", "age_days": D - min(days),
                "saturated_by": "", "note": "lagged one run"}
        part["weight_ppm"] = cv.ramp(h["day_factor_ppm"], part["age_days"])
        part["contribution"] = part["forge"] * part["weight_ppm"] // PPM
        account_proofs.setdefault(a, []).append(part)
    proof_total, distinct_roots, non_domain_max = {}, {}, {}
    for a, parts in account_proofs.items():
        parts.sort(key=lambda x: (x["root"], -x["contribution"], x["kind"], x["value"]))
        total = 0
        for i, part in enumerate(parts):
            if i > 0 and parts[i - 1]["root"] == part["root"]:
                best = parts[i - 1]
                part["saturated_by"] = best["saturated_by"] or best["kind"] + ":" + best["value"]
                continue
            total += part["contribution"]
            if part["contribution"] > 0:
                distinct_roots[a] = distinct_roots.get(a, 0) + 1
                if not part["root"].startswith("domain:"):
                    non_domain_max[a] = max(non_domain_max.get(a, 0), part["contribution"])
        proof_total[a] = total

    # Seed sets A (the parameters) and B (the public anchor rule).
    seeds_a = sorted(index[s] for s in p["seeds"] if s in index)
    seeds_a_list = sorted(s for s in p["seeds"] if s in index)
    rule = p["seeds_b"]
    seeds_b, seeds_b_list = [], []
    for i, a in enumerate(nodes):
        fs = first_seen.get(a, 0)
        anchored = distinct_roots.get(a, 0) >= rule["min_roots"] or non_domain_max.get(a, 0) >= max(1, rule["theta_anchor"])
        if (anchored and fs > 0 and fs <= as_of - rule["min_age_days"] * DAY
                and active_days(a, D - rule["of_days"], D) >= rule["active_days"] and penalty.get(a, 0) == 0):
            seeds_b.append(i)
            seeds_b_list.append(a)
    use_b = len(seeds_b) >= rule["min_members"]
    H = sum(1 for a in nodes if active_days(a, D - p["active_days"], D) > 0)
    pool = U * max(H, 1)

    # The flow graph: per-root hubs for roots with two or more nodes.
    groups = {}
    for i, a in enumerate(nodes):
        groups.setdefault(root_of(a), []).append(i)
    hub_labels = sorted(l for l, members in groups.items() if len(members) >= 2)
    hub = [-1] * len(nodes)
    for j, l in enumerate(hub_labels):
        for i in groups[l]:
            hub[i] = j
    graph = {"n": len(nodes), "transit": transit, "hub": hub, "hubs": len(hub_labels), "hub_cap": p["root_cap_shares"] * U,
             "node_cap": U, "steps": p["fill_steps"], "src": [e[0] for e in edges], "dst": [e[1] for e in edges],
             "cap": [p["edge_cap_ppm"] * U * e[2] // 10**12 for e in edges]}
    fa_node, fa_edge, work = run_flow(graph, seeds_a, pool, p["max_work"], 0)
    fb_node, fb_edge = fa_node, fa_edge
    if use_b:
        fb_node, fb_edge, work = run_flow(graph, seeds_b, pool, p["max_work"], work)

    def flow_of(i):
        return min(fa_node[i], fb_node[i]) if use_b else fa_node[i]

    def edge_flow(k):
        return min(fa_edge[k], fb_edge[k]) if use_b else fa_edge[k]

    in_edges = {}
    for k, e in enumerate(edges):
        in_edges.setdefault(e[1], []).append(k)

    # Evidence and liability.
    evidence = detect(p, snap, as_of, D, service, nodes, edges)
    lia = p["liability"]
    penalties = []
    for ev in evidence:
        members = set(ev["members"])
        for m in ev["members"]:
            penalties.append({"account": m, "evidence": ev["id"], "fraction_ppm": PPM, "starts_at": as_of, "ends_at": as_of + lia["penalty_days"] * DAY})
        into = {}
        for (src, dst), s in pair_recent.items():
            if dst in members and src not in members and src not in service:
                into[src] = into.get(src, 0) + s
        for u, s in into.items():
            pen = min(lia["phi_max_ppm"], lia["phi_ppm"] * saturate(s) // PPM)
            if pen > 0:
                penalties.append({"account": u, "evidence": ev["id"], "fraction_ppm": pen, "starts_at": as_of, "ends_at": as_of + lia["penalty_days"] * DAY})
    penalties.sort(key=lambda x: (x["account"], x["evidence"]))
    effective = dict(penalty)
    for pen in penalties:
        effective[pen["account"]] = max(effective.get(pen["account"], 0), pen["fraction_ppm"])

    # Scores: every node and every account with a proof.
    seed_a, seed_b = set(seeds_a_list), set(seeds_b_list) if use_b else set()
    flow_total, scores = {}, []
    for a in sorted(set(nodes) | set(account_proofs)):
        parts = {"proofs": account_proofs.get(a, []), "endorsers": [], "endorsers_total": 0, "down_votes": down_votes.get(a, 0),
                 "penalty_ppm": effective.get(a, 0), "own_avg": 0, "transit": 0, "seed": "", "reset": a in reset}
        sc = {"account": a, "root": root_of(a), "proof_collateral": proof_total.get(a, 0), "flow_a": 0, "flow_b": None, "flow": 0, "parts": parts}
        if a in index:
            i = index[a]
            sc["flow_a"] = fa_node[i]
            if use_b:
                sc["flow_b"] = fb_node[i]
            sc["flow"] = flow_of(i)
            parts["own_avg"], parts["transit"] = own_avg[i], transit[i]
            endorsers = []
            for k in in_edges.get(i, []):
                src = nodes[edges[k][0]]
                endorsers.append({"agent": src, "kinds": sorted(pair_kinds[(src, a)]), "weight_ppm": edges[k][2], "flow": edge_flow(k)})
            endorsers.sort(key=lambda x: (-x["flow"], -x["weight_ppm"], x["agent"]))
            parts["endorsers_total"] = len(endorsers)
            parts["endorsers"] = endorsers[:20]
        parts["seed"] = ("a" if a in seed_a else "") + ("b" if a in seed_b else "")
        flow_total[a] = sc["flow"]
        sc["collateral"] = sc["proof_collateral"] + sc["flow"] * p["endorsement_unit_price"]
        if sc["flow"] > 0 and sc["flow"] >= p["theta_trusted"]:
            sc["tier"] = 1
        elif sc["proof_collateral"] > 0 and sc["proof_collateral"] >= p["theta_proven"]:
            sc["tier"] = 2
        else:
            sc["tier"] = 3
        sc["weight_ppm"] = gdiv((PPM + min(p["weight_cap_ppm"], sc["collateral"] * p["weight_per_unit_ppm"])) * (PPM - effective.get(a, 0)), PPM)
        scores.append(sc)

    standing_summary = None
    if "standing" in p:
        parts_by, standing_summary, extra = compute_standing(
            p, snap, as_of, D, service, root_of, reset, effective, posts, current, account_proofs,
            sorted(set(nodes) | set(account_proofs)), {sc["account"]: sc["tier"] for sc in scores},
            {sc["account"]: sc["collateral"] for sc in scores})
        for sc in scores:
            sc["parts"]["standing"] = parts_by[sc["account"]]
        # An account whose only evidence is a standing input is scored too.
        for a in extra:
            pen = effective.get(a, 0)
            scores.append({"account": a, "root": root_of(a), "proof_collateral": 0, "flow_a": 0, "flow_b": None, "flow": 0,
                           "collateral": 0, "tier": 3, "weight_ppm": PPM * (PPM - pen) // PPM,
                           "parts": {"proofs": [], "endorsers": [], "endorsers_total": 0, "down_votes": down_votes.get(a, 0),
                                     "penalty_ppm": pen, "own_avg": 0, "transit": 0, "seed": "", "reset": a in reset,
                                     "standing": parts_by[a]}})
        scores.sort(key=lambda sc: sc["account"])

    sponsorships, dividends = sponsor(p, snap, as_of, service, first_seen, reset, index, own_avg, root_of, pair_sum, flow_total,
                                      lambda x: [] if x not in index else [(nodes[edges[k][0]], edge_flow(k)) for k in in_edges.get(index[x], [])])

    max_transit = max([transit[i] for i, a in enumerate(nodes) if a not in seed_a and a not in seed_b] or [0])
    body = canonical(p)
    out = {
        "schema": 1, "as_of": as_of, "params_version": snap.params_version,
        "params_sha256": hashlib.sha256(body.encode()).hexdigest(),
        "inputs": {"events_seq": snap.meta.events_seq, "endorsements_seq": snap.meta.endorsements_seq, "ledger_seq": snap.meta.ledger_seq,
                   "prior_runs": prior_runs, "accounts": len(snap.accounts), "posts": len(posts), "endorsements": len(ends),
                   "proofs": len(snap.proofs), "transfers": len(snap.transfers)},
        "nodes": len(nodes), "edges": len(edges), "pool_units": pool, "active_accounts": H,
        "seeds_a": seeds_a_list, "seeds_b": seeds_b_list, "seeds_b_used": use_b,
        "capture_bound": {"unit_per_share": U, "edge_cap_units": p["edge_cap_ppm"] * U // PPM, "lambda_ppm": p["lambda_ppm"],
                          "max_transit_units": max_transit, "pool_units": pool,
                          "statement": "Whatever the number of sybils, a region behind k attack edges receives at most k x edge_cap_units flow units, and at most max_transit_units through any one non-seed endorser."},
        "scores": scores, "evidence": evidence, "penalties": penalties, "sponsorships": sponsorships, "dividends": dividends,
    }
    if standing_summary is not None:
        out["standing"] = standing_summary
    return out


# ---------------------------------------------------------------------------
# Standing (RFC0015 §3, trust parameter version 2)

MASS_PER_CENT = 1000  # the run's mass unit: a thousandth of a cent
INT64_MAX = 2**63 - 1


def mul_div(a: int, b: int, c: int) -> int:
    """floor(a × b / c) for non-negative a, b and positive c; a quotient past
    int64 saturates, as the reference's 128-bit mulDiv does."""
    if a <= 0 or b <= 0 or c <= 0:
        return 0
    return min(a * b // c, INT64_MAX)


def vote_weight_ppm(cents: int, v0: int, c_ref: int, floor: int = 0) -> int:
    """v(s) in ppm: 0 for C = 0 or C below the floor (v_floor_cents), else
    v0 + (1 − v0) × √min(1, C / C_ref)."""
    if cents <= 0 or c_ref <= 0 or cents < floor:
        return 0
    frac = PPM if cents >= c_ref else mul_div(cents, PPM, c_ref)
    return v0 + mul_div(PPM - v0, math.isqrt(frac * PPM), PPM)


def share_weight_ppm(cents: int, p) -> int:
    return PPM + min(p["weight_cap_ppm"], max(0, cents) * p["weight_per_unit_ppm"])


def band(cents: int, st) -> int:
    if cents >= st["theta1_cents"]:
        return 1
    if cents >= st["theta2_cents"]:
        return 2
    return 3


def compute_standing(p, snap, as_of, D, service, root_of, reset, penalty, posts, current, account_proofs, scored, tiers, collateral):
    """Seed mass on priced entities (split between their controllers), credit
    spent (at cost, decayed), and the arbiter's seed list;
    endorse edges (up votes, vouches, work.accept, verified witnesses) and
    oppose edges (down votes), one weight per act, one half-life, saturating
    per pair and polarity; personalized PageRank from the seeds; oppose
    shares subtracted at the target; penalties scale outflow and standing.
    Rule 2: an act weighs its kind's weight (a vouch the weight its author
    chose, capped), saturating per pair, polarity and kind at twice one act;
    a node passes pass × W / (K + W) along its edges and returns the rest to
    the seeds; standing is c, not rescaled."""
    st = p["standing"]
    rule, v_floor = st.get("rule", 0), st.get("v_floor_cents", 0)
    spend_cap, funded_days = st.get("spend_cap_cents", 0), st.get("funded_days", 0)
    cv = Curves()
    ws = as_of - p["window_days"] * DAY
    inputs = {"vote": 0, "vouch": 0, "work_accept": 0, "witness": 0, "down_vote": 0, "spend": 0}

    seeds = {}  # account -> [(root, kind, source, state, mass)]
    claimants = {}
    bests = []
    for a in sorted(account_proofs):
        for part in account_proofs[a]:
            if part["saturated_by"] != "" or part["kind"] == "history":
                continue
            bests.append((a, part))
            if part["contribution"] > 0:
                claimants[part["root"]] = claimants.get(part["root"], 0) + 1
    # Rule 1: a domain is priced by its registration age when known (in
    # full), else by the link's age (at most half), and the state says which.
    registered = {}
    if rule >= 1:
        for r in snap.proofs:
            if r.kind != "domain" or r.registered_at <= 0 or r.registered_at > as_of:
                continue
            k = (r.account, r.link_value)
            if k not in registered or r.registered_at < registered[k]:
                registered[k] = r.registered_at
    for a, part in bests:
        contribution, state = part["contribution"], part["state"]
        if rule >= 1 and part["kind"] == "domain" and part["note"] != "not counted in this state":
            if (a, part["value"]) in registered:
                price = p["proofs"]["domain"]
                age = max(0, D - day_of(registered[(a, part["value"])]))
                contribution = min(price["forge"], price["rent"]) * cv.ramp(price["day_factor_ppm"], age) // PPM
                state += " (registration age)"
            else:
                state += " (link age: registration date unknown)"
        n = claimants.get(part["root"], 0)
        mass = contribution * MASS_PER_CENT // n if n > 0 and contribution > 0 else 0
        state += " (not counted)" if part["note"] == "not counted in this state" else ""
        seeds.setdefault(a, []).append((part["root"], "imported", part["kind"], state, mass))
    # Rule 1: spend paid to oneself (the payee shares the spender's root, or
    # either funded the other within funded_days) is not seed.
    self_dealt = None
    funded = set()
    if rule >= 1:
        inputs["spend_self_dealt"] = 0
        lo = as_of - funded_days * DAY
        for r in snap.transfers:
            t_from = r["from"] if "from" in r else ""
            if lo <= r.created_at < as_of and t_from != "" and r.to != "" and t_from != r.to and r.amount > 0:
                funded.add((t_from, r.to))
                funded.add((r.to, t_from))
        suffixes = set(p["domain_suffixes"])

        def self_dealt(r):
            root = root_of(r.account)
            if r.to != "" and (r.to == r.account or root_of(r.to) == root or (r.account, r.to) in funded):
                return True
            return r.link_value != "" and root_of(domain_root(r.link_value, suffixes)) == root
    spend = {}
    for r in sorted(snap.spends, key=record_key):
        if r.account == "" or r.account in service or r.day >= D or r.amount <= 0:
            continue
        if self_dealt is not None and self_dealt(r):
            inputs["spend_self_dealt"] += 1
            continue
        inputs["spend"] += 1
        spend[r.account] = spend.get(r.account, 0) + mul_div(min(r.amount, 10**15), cv.decay(st["day_factor_ppm"], D - r.day) * MASS_PER_CENT,
                                                             PPM * st["credits_per_cent"])
    for a, m in spend.items():
        if spend_cap > 0:
            m = min(m, spend_cap * MASS_PER_CENT)
        seeds.setdefault(a, []).append(("spend:" + a, "earned", "spend", "computed", m))
    for s in p["seeds"]:
        if s not in service:
            seeds.setdefault(s, []).append(("arbiter", "earned", "arbiter_seed", "seed list", st["arbiter_seed_cents"] * MASS_PER_CENT))

    acts = []
    for r in current:
        if r.created_at < ws or r.target == "":
            continue
        if r.kind in ("vote", "vouch") and r.value == 1:
            acts.append((r.voter, r.target, r.kind, r.created_at, False, r.weight, r.message_id))
        elif r.kind == "vote" and r.value == -1:
            acts.append((r.voter, r.target, "down_vote", r.created_at, True, 0, r.message_id))
    for r in sorted(snap.acts, key=record_key):
        if r.created_at >= as_of or r.created_at < ws or r.kind not in ("work_accept", "witness"):
            continue
        # A failed witness verdict (value -1) is read from rule 3 only: a
        # position against the witnessed claim, no edge.
        if r.value < 0 and (rule < 3 or r.kind != "witness"):
            continue
        acts.append((r["from"] if "from" in r else "", r.to, r.kind, r.created_at, r.value < 0, 0, r.id))

    def act_weight(kind, chosen):
        if kind == "vouch" and chosen > 0:
            return min(chosen, st["vouch_weight_max"])
        return st["edge_weights"][kind]
    pair_sum = {}
    kind_sum, kind_weight = {}, {}
    staked = []
    if rule >= 3:
        inputs["witness_failed"] = 0
    for src, dst, kind, at, oppose, chosen, item in acts:
        if src == "" or dst == "" or src == dst or src in service or dst in service or src in reset:
            continue
        if root_of(src) == root_of(dst):
            continue
        v = cv.decay(st["day_factor_ppm"], D - day_of(at))
        if v <= 0:
            continue
        if kind == "witness" and oppose:
            inputs["witness_failed"] += 1
        else:
            inputs[kind] += 1
        if rule >= 3:
            staked.append((src, dst, kind, item, at, oppose, act_weight(kind, chosen) * v))
        if rule >= 2:
            k = (src, dst, oppose, kind)
            kind_sum[k] = kind_sum.get(k, 0) + v
            kind_weight[k] = max(kind_weight.get(k, 0), act_weight(kind, chosen))
            continue
        pair_sum[(src, dst, oppose)] = pair_sum.get((src, dst, oppose), 0) + v
    # Rule 2: each kind counts w × 2s / (1 + s), summed per pair and polarity.
    for k, total in kind_sum.items():
        w = kind_weight[k] * mul_div(2 * PPM, total, PPM + total)
        if w > 0:
            pair_sum[k[:3]] = pair_sum.get(k[:3], 0) + w

    nodes = sorted(set(seeds) | {k[0] for k in pair_sum} | {k[1] for k in pair_sum} | set(scored))
    index = {a: i for i, a in enumerate(nodes)}
    n = len(nodes)
    seed = [sum(r[4] for r in seeds.get(a, [])) for a in nodes]
    S = sum(seed)
    outs = [[] for _ in range(n)]
    wsum = [0] * n
    for (src, dst, oppose), total in pair_sum.items():
        w = total if rule >= 2 else PPM * total // (PPM + total)
        if w > 0:
            outs[index[src]].append((index[dst], w, oppose))
            wsum[index[src]] += w
    for lst in outs:
        lst.sort(key=lambda e: (e[0], e[2]))
    pen = [min(PPM, penalty.get(a, 0)) for a in nodes]

    stake_out = None
    if rule >= 3:
        stake_out = run_stakes(st, as_of, ws, nodes, seed, pen, staked,
                               lambda a, b: a != b and root_of(a) != root_of(b) and (a, b) not in funded)

    c = list(seed)
    keep = st.get("keep_weight", 0) * PPM
    restart = [x - mul_div(x, st["pass_ppm"], PPM) for x in seed]
    opposed = [0] * n
    for _ in range(st["iterations"] if rule < 3 else 0):
        nxt = list(restart)
        opposed = [0] * n
        back = 0
        for u in range(n):
            pas = mul_div(c[u], st["pass_ppm"], PPM)
            held = mul_div(pas, pen[u], PPM)
            pas -= held
            back += held
            if wsum[u] == 0:
                if rule >= 1:
                    back += pas  # dangling: returns to the seeds
                else:
                    nxt[u] += pas
                continue
            if rule >= 2:
                out = mul_div(pas, wsum[u], wsum[u] + keep)
                back += pas - out  # kept: returns to the seeds
                pas = out
            sent = 0
            for dst, w, oppose in outs[u]:
                share = mul_div(pas, w, wsum[u])
                sent += share
                if oppose:
                    opposed[dst] += share
                    back += share
                    continue
                nxt[dst] += share
            nxt[u] += pas - sent
        for i in range(n):
            if S > 0 and seed[i] > 0:
                nxt[i] += mul_div(back, seed[i], S)
        c = nxt

    seasoned = {r.account for r in posts if r.created_at <= as_of - DAY}
    scored_set = set(scored)
    v0, c_ref = st["v0_ppm"], st["c_ref_cents"]

    def vw(cents):
        return vote_weight_ppm(cents, v0, c_ref, v_floor)
    entities = {}
    for a in nodes:
        for root, _, _, _, mass in seeds.get(a, []):
            typ = root.split(":", 1)[0]
            e = entities.setdefault(typ, {"controls": 0, "seed_cents": 0})
            e["controls"] += 1
            e["seed_cents"] += mass // MASS_PER_CENT
    summary = {
        "mode": st["mode"], "seed_cents": S // MASS_PER_CENT, "inputs": inputs, "bands": {"1": 0, "2": 0, "3": 0},
        "would_be": {"allowance_tier": {"raised": 0, "lowered": 0, "same": 0},
                     "share_weight": {"raised": 0, "added_ppm": 0, "max_added_ppm": 0},
                     "vote_weight": {"zero": 0, "partial": 0, "full": 0, "votes": 0, "votes_weight_ppm": 0, "votes_floored_ppm": 0,
                                     "admitted_unseasoned": 0},
                     "inbox_known": {"today": 0, "would_be": 0, "raised": 0}},
        "anonymous": {"cents": st["anon_seed_cents"], "vote_weight_ppm": vw(st["anon_seed_cents"]),
                      "share_weight_ppm": share_weight_ppm(st["anon_seed_cents"], p)},
        "entities": entities, "largest_moves": [], "accounts": 0, "nonzero": 0,
    }
    wb = summary["would_be"]
    parts, extra, moves, total = {}, [], [], 0
    if rule >= 3:
        c = stake_out["c"]
    for i, a in enumerate(nodes):
        raw, opp = c[i], opposed[i]
        if rule >= 3:
            raw = stake_out["raw"][i]  # before the penalty; c[i] is after it
        if rule == 1:  # reported as c / (1 − pass); rule 2 reports c
            raw, opp = mul_div(raw, PPM, PPM - st["pass_ppm"]), mul_div(opp, PPM, PPM - st["pass_ppm"])
        cents = mul_div(max(0, raw - opp), PPM - pen[i], PPM) // MASS_PER_CENT
        if rule >= 3:
            cents = c[i] // MASS_PER_CENT
        part = {"cents": cents, "raw_cents": raw // MASS_PER_CENT, "opposed_cents": opp // MASS_PER_CENT, "penalty_ppm": pen[i],
                "seed_cents": seed[i] // MASS_PER_CENT, "received_cents": max(0, raw - seed[i]) // MASS_PER_CENT, "band": band(cents, st),
                "vote_weight_ppm": vw(cents), "share_weight_ppm": share_weight_ppm(cents, p), "breakdown": []}
        own = min(raw, seed[i])
        for root, kind, source, state, mass in seeds.get(a, []):
            contribution = mul_div(own, mass, seed[i]) // MASS_PER_CENT if seed[i] > 0 else 0
            part["breakdown"].append({"root": root, "kind": kind, "source": source, "state": state, "seed_cents": mass // MASS_PER_CENT,
                                      "contribution": contribution})
        if raw > seed[i] and rule >= 3:
            # What it holds above its seed: what its judgement earned
            # (settled stakes), and what endorsers moved to it.
            above = raw - seed[i]
            judged = min(above, max(0, stake_out["judged"][i]))
            if above - judged >= MASS_PER_CENT:
                part["breakdown"].append({"root": "endorsements", "kind": "earned", "source": "edges", "state": "computed", "seed_cents": 0,
                                          "contribution": (above - judged) // MASS_PER_CENT})
            if judged >= MASS_PER_CENT:
                part["breakdown"].append({"root": "judgement", "kind": "earned", "source": "stakes", "state": "computed", "seed_cents": 0,
                                          "contribution": judged // MASS_PER_CENT})
        elif raw > seed[i]:
            part["breakdown"].append({"root": "endorsements", "kind": "earned", "source": "edges", "state": "computed", "seed_cents": 0,
                                      "contribution": (raw - seed[i]) // MASS_PER_CENT})
        part["breakdown"].sort(key=lambda b: (-b["contribution"], -b["seed_cents"], b["root"]))
        parts[a] = part
        if a not in scored_set:
            if cents == 0 and seed[i] == 0:
                continue
            extra.append(a)
        total += cents
        summary["accounts"] += 1
        if cents > 0:
            summary["nonzero"] += 1
        summary["bands"][str(part["band"])] += 1
        tier = tiers.get(a, 3)
        if part["band"] < tier:
            wb["allowance_tier"]["raised"] += 1
        elif part["band"] > tier:
            wb["allowance_tier"]["lowered"] += 1
        else:
            wb["allowance_tier"]["same"] += 1
        added = part["share_weight_ppm"] - PPM
        if added > 0:
            wb["share_weight"]["raised"] += 1
            wb["share_weight"]["added_ppm"] += added
            wb["share_weight"]["max_added_ppm"] = max(wb["share_weight"]["max_added_ppm"], added)
        v = part["vote_weight_ppm"]
        wb["vote_weight"]["zero" if v == 0 else "partial" if v < PPM else "full"] += 1
        if a not in seasoned and v >= 500000:
            wb["vote_weight"]["admitted_unseasoned"] += 1
        today = collateral.get(a, 0) >= p["theta_proven"] and collateral.get(a, 0) > 0
        would = cents >= st["theta2_cents"]
        if today:
            wb["inbox_known"]["today"] += 1
        if would:
            wb["inbox_known"]["would_be"] += 1
            if not today:
                wb["inbox_known"]["raised"] += 1
        moves.append({"account": a, "cents": cents, "band": part["band"], "tier": tier, "share_weight_ppm": part["share_weight_ppm"],
                      "vote_weight_ppm": v, "seasoned": a in seasoned})
    summary["standing_cents"] = total
    for r in current:
        if r.kind != "vote" or r.created_at < ws or r.value not in (1, -1):
            continue
        wb["vote_weight"]["votes"] += 1
        cents = parts[r.voter]["cents"] if r.voter in parts else 0
        v = vw(cents)
        wb["vote_weight"]["votes_weight_ppm"] += v
        wb["vote_weight"]["votes_floored_ppm"] += max(PPM, v)
    moves.sort(key=lambda m: (-m["share_weight_ppm"], -m["cents"], m["account"]))
    for m in moves:
        if len(summary["largest_moves"]) == 10 or m["share_weight_ppm"] <= PPM:
            break
        summary["largest_moves"].append(m)
    return parts, summary, sorted(extra)


def _score(R: int, P: int) -> int:
    """(R − P) / (R + P) in ppm, P ≥ 1."""
    if R >= P:
        return mul_div(R - P, PPM, R + P)
    return -mul_div(P - R, PPM, R + P)


def _lower_median(xs: list) -> int:
    if not xs:
        return 0
    xs = sorted(xs)
    return xs[(len(xs) - 1) // 2]


def run_stakes(st, as_of, ws, nodes, seed, pen, acts, indep):
    """Rule 3: endorsements are stakes. Each act commits stake_ppm × weight ×
    decay of its author's standing (scaled down together above
    stake_budget_ppm). A vouch, an accepted work item and a verified witness
    move it to the target; a vote on a post, a witness verdict on a claim and
    a vouch or accept on its target are positions. A post opens at its
    author's median reception (the median over all posts until the author
    has prior_min_posts with any), priced at the larger of that and the stake
    already on it; a claim opens at nothing; an agent at its trajectory. R is
    the stake of independent accounts (another root, no transfer within
    funded_days): on the post, on the claim's side less against it, moved to
    the agent after the act. r = (R − P) / (R + P); within a pot (an author's
    posts, a claim, an agent) losers pay up to |r|/2 of their stake to
    winners, pro rata; a penalised agent's vouchers score −1 and lose half
    their stake times the penalty. Standing is the fixed point of seed +
    moved in − moved out + settled, iterated from the seed."""
    n = len(nodes)
    index = {a: i for i, a in enumerate(nodes)}
    acts = [a for a in acts if a[0] in index and a[1] in index and a[6] > 0]
    acts.sort(key=lambda a: (a[4], a[0], a[1], a[2], a[3], a[5]))
    m = len(acts)
    src = [index[a[0]] for a in acts]
    dst = [index[a[1]] for a in acts]
    transfer = [False] * m
    obj_of = [""] * m
    load = [0] * n
    objects, transfers_in, post_author = {}, {}, {}
    positions = 0
    for x, (s_, d_, kind, item, at, against, ld) in enumerate(acts):
        if kind in ("vouch", "work_accept"):
            transfer[x], obj_of[x] = True, "g\x00" + d_
        elif kind == "witness" and not against:
            transfer[x] = True
            if item != "":
                obj_of[x] = "c\x00" + d_ + "\x00" + item
        elif kind == "witness":
            if item != "":
                obj_of[x] = "c\x00" + d_ + "\x00" + item
        elif kind == "vote" and item != "":
            obj_of[x] = "p\x00" + d_ + "\x00" + item
            post_author[obj_of[x]] = dst[x]
        if transfer[x]:
            load[src[x]] += ld
            transfers_in.setdefault(dst[x], []).append(x)
        if obj_of[x][:1] in ("p", "c"):
            load[src[x]] += ld
        if obj_of[x] != "":
            objects.setdefault(obj_of[x], []).append(x)
            positions += 1
    scale = []
    for i in range(n):
        share = mul_div(st["stake_ppm"], load[i], PPM)
        scale.append(mul_div(st["stake_budget_ppm"], PPM, share) if share > st["stake_budget_ppm"] else PPM)
    keys = sorted(objects)
    agents = sorted(transfers_in)
    eff = [mul_div(s, PPM - pen[i], PPM) for i, s in enumerate(seed)]
    S = sum(eff)
    window = max(1, as_of - ws)
    c = list(seed)
    raw = [0] * n
    judged = [0] * n
    sig = [0] * m
    r = [0] * m
    on = [False] * m
    for _ in range(st["iterations"]):
        nxt = list(seed)
        judged = [0] * n
        for x, a in enumerate(acts):
            sig[x] = mul_div(mul_div(c[src[x]], st["stake_ppm"], PPM), mul_div(a[6], scale[src[x]], PPM), PPM)
            if transfer[x]:
                nxt[src[x]] -= sig[x]
                nxt[dst[x]] += sig[x]
        reception, everything = {}, []
        for k in keys:
            if k[0] != "p":
                continue
            mass = sum(sig[x] for x in objects[k])
            if mass > 0:
                reception.setdefault(post_author[k], []).append(mass)
                everything.append(mass)
        unknown = _lower_median(everything)
        totals = [t for t in (sum(sig[y] for y in transfers_in[d]) for d in agents) if t > 0]
        agent_median = _lower_median(totals)
        for k in keys:
            xs = objects[k]
            if k[0] == "p":
                rec = reception.get(post_author[k], [])
                p0 = _lower_median(rec) if len(rec) >= st["prior_min_posts"] else unknown
                for x in xs:
                    R = B = 0
                    for y in xs:
                        if acts[y][4] < acts[x][4]:
                            B += sig[y]
                        if y != x and indep(acts[x][0], acts[y][0]):
                            R += sig[y]
                    r[x], on[x] = _score(R, max(p0, B, 1)), True
            elif k[0] == "c":
                for x in xs:
                    same = other = B = 0
                    for y in xs:
                        if acts[y][5] == acts[x][5] and acts[y][4] < acts[x][4]:
                            B += sig[y]
                        if y == x or not indep(acts[x][0], acts[y][0]):
                            continue
                        if acts[y][5] == acts[x][5]:
                            same += sig[y]
                        else:
                            other += sig[y]
                    on[x] = same + other > 0
                    r[x] = _score(max(0, same - other), max(B, 1))
            else:
                for x in xs:
                    R = before = 0
                    for y in transfers_in[dst[x]]:
                        if acts[y][4] < acts[x][4]:
                            before += sig[y]
                        elif acts[y][4] > acts[x][4] and indep(acts[x][0], acts[y][0]):
                            R += sig[y]
                    left = max(0, as_of - acts[x][4])
                    P = mul_div(before, left, max(1, acts[x][4] - ws)) if before > 0 else mul_div(agent_median, left, window)
                    r[x], on[x] = _score(R, max(P, 1)), True
                    if pen[dst[x]] > 0:
                        r[x] = -PPM
        pots = {}
        for k in keys:
            pot = "a\x00" + acts[objects[k][0]][1] if k[0] == "p" else k
            pots.setdefault(pot, []).extend(objects[k])
        back = 0
        for pk in sorted(pots):
            xs = pots[pk]
            L = sum(mul_div(sig[x], -r[x], 2 * PPM) for x in xs if on[x] and r[x] < 0)
            G = sum(mul_div(sig[x], r[x], 2 * PPM) for x in xs if on[x] and r[x] > 0)
            paid = min(L, G)
            taken = given = 0
            if paid > 0:
                for x in xs:
                    if on[x] and r[x] < 0:
                        t = mul_div(mul_div(sig[x], -r[x], 2 * PPM), paid, L)
                        taken += t
                        judged[src[x]] -= t
                for x in xs:
                    if on[x] and r[x] > 0:
                        g = mul_div(mul_div(sig[x], r[x], 2 * PPM), taken, G)
                        given += g
                        judged[src[x]] += g
            back += taken - given
            if pk[0] == "g":
                for x in xs:
                    burn = mul_div(sig[x], pen[dst[x]], 2 * PPM)
                    if burn > 0:
                        judged[src[x]] -= burn
                        back += burn
        deficit = 0
        removed = [0] * n
        for i in range(n):
            nxt[i] += judged[i]
            removed[i] = mul_div(max(0, nxt[i]), pen[i], PPM)
            nxt[i] -= removed[i]
            back += removed[i]
            if nxt[i] < 0:
                deficit -= nxt[i]
                nxt[i] = 0
        back = max(0, back - deficit)
        for i in range(n):
            if S > 0 and eff[i] > 0:
                nxt[i] += mul_div(back, eff[i], S)
            raw[i] = nxt[i] + removed[i]
        c = nxt
    return {"c": c, "raw": raw, "judged": judged, "positions": positions}


def sponsor(p, snap, as_of, service, first_seen, reset, index, own_avg, root_of, pair_sum, flow_total, inflow):
    """A vouch with sponsor:true within the invitee's first window_days (the
    earliest wins, within the sponsor's daily slots) is a sponsorship. A new
    high of the invitee's independent inflow (edges from endorsers outside both
    roots and not endorsed by the sponsor) earns dividend_ppm of the rise,
    within dividend_days and daily_cap."""
    U, sp = p["unit_per_share"], p["sponsor"]
    prev = {}
    for r in snap.sponsorships:
        prev[r.invitee] = r
    vouches = sorted((r for r in snap.endorsements if r.kind == "vouch" and r.sponsor and r.value == 1 and r.created_at < as_of
                      and r.voter != r.target and r.voter != "" and r.target != "" and r.voter not in service and r.target not in service),
                     key=lambda r: r.seq)
    used, chosen = {}, {}
    for r in vouches:
        fs = first_seen.get(r.target, 0)
        if fs == 0 or r.created_at > fs + sp["window_days"] * DAY or root_of(r.voter) == root_of(r.target):
            continue
        if r.target in chosen:
            continue
        own = own_avg[index[r.voter]] if r.voter in index else 0
        slots = 0 if r.voter in reset else (2 * sp["slots_per_share"] * min(U, own) + U) // (2 * U)
        key = (r.voter, day_of(r.created_at))
        if used.get(key, 0) >= slots:
            continue
        used[key] = used.get(key, 0) + 1
        chosen[r.target] = r
    ships, divs, paid = [], [], {}
    for x in sorted(chosen):
        r = chosen[x]
        s = r.voter
        old = prev.get(x)
        hw = old.high_water if old is not None and old.sponsor_account == s else 0
        indep = 0
        for agent, flow in inflow(x):
            if agent == s or root_of(agent) == root_of(s) or root_of(agent) == root_of(x) or pair_sum.get((s, agent), 0) > 0:
                continue
            indep += flow
        indep = min(indep, flow_total.get(x, 0))
        ends = r.created_at + sp["dividend_days"] * DAY
        if as_of < ends and indep > hw:
            units = min(gdiv(sp["dividend_ppm"] * (indep - hw), PPM), sp["daily_cap"] - paid.get(s, 0))
            if units > 0:
                paid[s] = paid.get(s, 0) + units
                divs.append({"sponsor": s, "invitee": x, "units": units})
        ships.append({"invitee": x, "sponsor": s, "record_seq": r.seq, "created_at": r.created_at, "ends_at": ends, "high_water": max(hw, indep)})
    return ships, divs


# ---------------------------------------------------------------------------
# Ed25519 verification (RFC 8032 §5.1.7), cofactorless like Go's


_P = 2**255 - 19
_L = 2**252 + 27742317777372353535851937790883648493
_D = -121665 * pow(121666, _P - 2, _P) % _P
_SQRT_M1 = pow(2, (_P - 1) // 4, _P)


def _add(a, b):
    x1, y1, z1, t1 = a
    x2, y2, z2, t2 = b
    A = (y1 - x1) * (y2 - x2) % _P
    B = (y1 + x1) * (y2 + x2) % _P
    C = 2 * t1 * t2 * _D % _P
    Dd = 2 * z1 * z2 % _P
    E, F, G, H = B - A, Dd - C, Dd + C, B + A
    return (E * F % _P, G * H % _P, F * G % _P, E * H % _P)


def _mul(s, point):
    q = (0, 1, 1, 0)
    while s > 0:
        if s & 1:
            q = _add(q, point)
        point = _add(point, point)
        s >>= 1
    return q


def _decode_point(b: bytes):
    if len(b) != 32:
        return None
    y = int.from_bytes(b, "little")
    sign = y >> 255
    y = (y & ((1 << 255) - 1)) % _P  # like Go, accept a non-canonical y
    x2 = (y * y - 1) * pow(_D * y * y + 1, _P - 2, _P) % _P
    x = pow(x2, (_P + 3) // 8, _P)
    if (x * x - x2) % _P != 0:
        x = x * _SQRT_M1 % _P
    if (x * x - x2) % _P != 0:
        return None
    if x & 1 != sign:
        if x == 0:
            return None
        x = _P - x
    return (x, y, 1, x * y % _P)


def _encode_point(pt) -> bytes:
    x, y, z, _ = pt
    zi = pow(z, _P - 2, _P)
    x, y = x * zi % _P, y * zi % _P
    return (y | ((x & 1) << 255)).to_bytes(32, "little")


_BASE = _decode_point((4 * pow(5, _P - 2, _P) % _P).to_bytes(32, "little"))


def ed25519_verify(public: bytes, message: bytes, signature: bytes) -> bool:
    if len(public) != 32 or len(signature) != 64:
        return False
    a = _decode_point(public)
    if a is None:
        return False
    s = int.from_bytes(signature[32:], "little")
    if s >= _L:
        return False
    k = int.from_bytes(hashlib.sha512(signature[:32] + public + message).digest(), "little") % _L
    neg_a = ((_P - a[0]) % _P, a[1], a[2], (_P - a[3]) % _P)
    r = _add(_mul(s, _BASE), _mul(k, neg_a))
    return _encode_point(r) == signature[:32]


def b64url(text: str) -> bytes:
    """Unpadded base64url, canonical only."""
    raw = base64.urlsafe_b64decode(text + "=" * (-len(text) % 4))
    if base64.urlsafe_b64encode(raw).rstrip(b"=").decode() != text:
        raise ValueError("not canonical unpadded base64url")
    return raw


# ---------------------------------------------------------------------------
# Endorsement export


EXPORT_FIELDS = {"type", "seq", "message_id", "target", "voter", "public_key", "value", "sponsor", "weight", "created_at",
                 "signed_payload", "signature"}


def verify_record(record: dict, service: str) -> str | None:
    """None if a signed record verifies; otherwise why not. Unsigned and
    legacy records return "unsigned" (weight 0, nothing to verify)."""
    if set(record) - EXPORT_FIELDS:
        return "unknown fields"
    if record.get("signature") is None or record.get("type") == "legacy_vote":
        return "unsigned"
    try:
        key, sig = b64url(record["public_key"]), b64url(record["signature"])
    except (ValueError, KeyError, TypeError):
        return "public_key or signature is not base64url"
    payload = record.get("signed_payload", "")
    if not ed25519_verify(key, payload.encode(), sig):
        return "signature does not verify"
    try:
        envelope = json.loads(payload)
        command = envelope["command"]
    except (ValueError, KeyError, TypeError):
        return "signed_payload is not a command envelope"
    if envelope.get("service") != service:
        return "signed for another service"
    if command.get("public_key") != record["public_key"]:
        return "signed public_key differs"
    if command.get("operation") != record["type"]:
        return "signed operation differs"
    try:
        data = json.loads(command.get("data", ""))
    except ValueError:
        return "signed data is not JSON"
    if not isinstance(data, dict):
        return "signed data is not an object"
    if record["type"] == "vote":
        if command.get("message_id") != record.get("message_id") or data.get("value") != record["value"] or isinstance(data.get("value"), bool):
            return "signed vote differs"
    elif record["type"] == "vouch":
        weight = data.get("weight", 0)
        if set(data) - {"schema", "value", "sponsor", "weight"} or data.get("schema") != 1 or data.get("value") not in (0, 1) \
                or isinstance(data.get("value"), bool) or data.get("value") != record["value"] \
                or bool(data.get("sponsor", False)) != bool(record.get("sponsor")) \
                or isinstance(weight, bool) or not isinstance(weight, int) or weight != record.get("weight", 0) \
                or ("weight" in data and not (1 <= weight <= 50 and data.get("value") == 1)):
            return "signed vouch differs"
    else:
        return "unknown record type"
    return None


def read_export(lines, service: str):
    """Yields (record, problem) for each export line."""
    for n, line in enumerate(lines, 1):
        line = line.strip()
        if not line:
            continue
        try:
            record = json.loads(line)
        except ValueError:
            yield {"line": n}, "not JSON"
            continue
        if not isinstance(record, dict):
            yield {"line": n}, "not an object"
            continue
        yield record, verify_record(record, service)


def trust_input(record: dict) -> dict:
    """An export record as a trust input line; zero fields omitted."""
    kind = record["type"]
    if kind == "vote" and record.get("signature") is None:
        kind = "unsigned"
    out = {"type": "endorsement", "seq": record.get("seq", 0), "kind": kind, "voter": record.get("voter", ""),
           "target": record.get("target", ""), "message_id": record.get("message_id", ""), "value": record.get("value", 0),
           "sponsor": bool(record.get("sponsor", False)), "weight": record.get("weight", 0), "created_at": record.get("created_at", 0)}
    return {k: v for k, v in out.items() if k == "type" or v not in (0, "", False)}


# ---------------------------------------------------------------------------


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    sub = parser.add_subparsers(dest="command", required=True)
    for name in ("verify", "endorsements"):
        cmd = sub.add_parser(name)
        cmd.add_argument("export")
        cmd.add_argument("--service", default="swarmmemo.com")
    cmd = sub.add_parser("run")
    cmd.add_argument("snapshot")
    args = parser.parse_args(argv)
    if args.command == "run":
        with open(args.snapshot, encoding="utf-8") as f:
            snap = read_snapshot(f)
        sys.stdout.write(canonical(compute(snap)))
        return 0
    counts = {"verified": 0, "unsigned": 0, "failed": 0}
    out = []
    with open(args.export, encoding="utf-8") as f:
        for record, problem in read_export(f, args.service):
            if problem is None:
                counts["verified"] += 1
            elif problem == "unsigned":
                counts["unsigned"] += 1
            else:
                counts["failed"] += 1
                print(f"seq {record.get('seq', record.get('line'))}: {problem}", file=sys.stderr)
                continue
            out.append(record)
    if args.command == "endorsements":
        if counts["failed"]:
            return 1
        for record in out:
            print(canonical(trust_input(record)))
        return 0
    print(canonical(counts))
    return 1 if counts["failed"] else 0


if __name__ == "__main__":
    sys.exit(main())
