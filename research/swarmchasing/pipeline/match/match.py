#!/usr/bin/env python3
"""Cross-board identity matcher: data/boards/* -> data/match/crossgraph.json (+ links.jsonl, report inputs).

Stdlib only. Layers:
  explicit   shared declared ids (ed25519/npub/evm/github/url/email_hash), profile URLs pointing at another
             board's profile, first-person self-claims in post text ("I'm X on moltbook", "my key is <hex>").
             Merges identities (union-find).
  same-owner Moltbook agents whose claimed owner (X handle of the HUMAN) is the same. Never a merge.
             Owners are exported only as salted-hash ids.
  handle     same normalised handle on different boards. Weak, never merges.
  style      optional; produced by match/style.py into data/match/style_edges.json and merged in here.

Post text is read locally for claims only; no text is written to any output.
"""
import hashlib, json, os, re, secrets, sys, time
from collections import Counter, defaultdict
from datetime import datetime, timezone

ROOT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
BOARDS = os.path.join(ROOT, "data", "boards")
OUT = os.path.join(ROOT, "data", "match")

GENERIC = {"claude", "assistant", "agent", "bot", "ai", "gpt", "chatgpt", "gemini", "anonymous", "anon", "user",
           "test", "testbot", "testagent", "aiagent", "agentbot", "helper", "admin", "system", "openai", "llama",
           "codex", "copilot", "grok", "mistral", "deepseek", "qwen", "kimi", "sonnet", "opus", "haiku", "unknown",
           "null", "none", "guest", "moderator", "mod", "operator", "human", "aibot", "myagent", "newagent", "messageboardbot"}

# profile URL patterns -> (board, capture is handle or key)
PROFILE_RES = [
    ("moltbook", re.compile(r"moltbook\.com/u/([A-Za-z0-9_\-]{2,40})", re.I)),
    ("colony", re.compile(r"thecolony\.(?:ai|cc)/u/([A-Za-z0-9_\-]{2,40})", re.I)),
    ("swarmmemo", re.compile(r"swarmmemo\.com/agents/([0-9a-f]{64})", re.I)),
    ("sanctum", re.compile(r"sanctum-beacon\.onrender\.com/api/agents/([0-9a-f]{64})", re.I)),
]
BOARD_WORDS = {"moltbook": "moltbook", "the colony": "colony", "colony": "colony", "thecolony": "colony",
               "clawprint": "clawprint", "agentchan": "agentchan", "moltchan": "moltchan", "sanctum": "sanctum",
               "tantive": "tantive", "swarmmemo": "swarmmemo", "swarm memo": "swarmmemo"}
CLAIM_RE = re.compile(r"\b(?:i'?m|i am|known as|find me as|follow me as|follow me|find me|my (?:handle|name|username|account) is)"
                      r"\s+@?([A-Za-z0-9_\-]{3,32})\s+(?:on|at|over on)\s+(the colony|thecolony|colony|moltbook|clawprint|agentchan|"
                      r"moltchan|sanctum|tantive|swarm ?memo)\b", re.I)
CUE_RE = re.compile(r"\b(my|mine|i'?m|i am|me at|find me|follow me|reach me|tip me|send (?:to|me))\b", re.I)
HEX64 = re.compile(r"\b[0-9a-f]{64}\b")
EVM = re.compile(r"\b0x[0-9a-fA-F]{40}\b")
NPUB = re.compile(r"\bnpub1[02-9ac-hj-np-z]{58}\b")


def bech32_npub_to_hex(s):
    cs = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"
    data = [cs.index(c) for c in s[5:]][:-6]
    acc = bits = 0; out = bytearray()
    for v in data:
        acc = (acc << 5) | v; bits += 5
        while bits >= 8:
            bits -= 8; out.append((acc >> bits) & 0xff)
    return out.hex() if len(out) == 32 else None


def norm_handle(h):
    return re.sub(r"[^a-z0-9]", "", (h or "").lower())


def norm_url(u):
    u = u.strip().lower()
    u = re.sub(r"^[a-z]+://", "", u); u = re.sub(r"^www\.", "", u)
    u = u.split("#")[0].split("?")[0].rstrip("/")
    return u


def ts_epoch(s):
    if not s:
        return None
    try:
        d = datetime.fromisoformat(s.replace("Z", "+00:00"))
        return int((d if d.tzinfo else d.replace(tzinfo=timezone.utc)).timestamp())
    except Exception:
        return None


def is_anon(a):
    aid = a["author_id"]
    return aid == "anonymous" or norm_handle(aid.split(":", 1)[-1]) in ("anonymous", "anon")


def load():
    authors, posts, edges = {}, [], []
    for src in sorted(os.listdir(BOARDS)):
        d = os.path.join(BOARDS, src)
        if not os.path.isfile(os.path.join(d, "authors.jsonl")):
            continue
        for l in open(os.path.join(d, "authors.jsonl")):
            a = json.loads(l); authors[(src, a["author_id"])] = a
        for l in open(os.path.join(d, "posts.jsonl")):
            posts.append(json.loads(l))
        for l in open(os.path.join(d, "edges.jsonl")):
            edges.append(json.loads(l))
    return authors, posts, edges


class UF:
    def __init__(self): self.p = {}
    def find(self, x):
        self.p.setdefault(x, x)
        while self.p[x] != x:
            self.p[x] = self.p[self.p[x]]; x = self.p[x]
        return x
    def union(self, a, b):
        a, b = self.find(a), self.find(b)
        if a != b: self.p[max(a, b)] = min(a, b)


def main():
    t0 = time.time()
    os.makedirs(OUT, exist_ok=True)
    authors, posts, edges = load()
    boards = sorted({k[0] for k in authors})
    bix = {b: i for i, b in enumerate(boards)}

    def name_of(a):
        h = a.get("handle") or a.get("display_name")
        if not h and ":" in a["author_id"]:
            h = a["author_id"].split(":", 1)[1]
        return h

    # --- index declared ids ---------------------------------------------------------------------------
    id_index = defaultdict(set)          # (kind, value) -> {author key}
    id_from = {}                          # (author, kind, value) -> from
    owners = defaultdict(set)             # x handle -> authors (moltbook owner = human)
    for k, a in authors.items():
        for e in a.get("explicit_ids") or []:
            kind, v = e["kind"], e["value"]
            if kind == "x":
                if k[0] == "moltbook":
                    owners[v].add(k)      # human owner; never identity evidence
                continue
            if kind == "url":
                v = norm_url(v)
            elif kind == "evm":
                v = v.lower()
            id_index[(kind, v)].add(k); id_from[(k, kind, v)] = e["from"]
    # swarmmemo: author_id is the sha256 fingerprint; index fingerprint too (as ed25519 alias)
    fp_to_key = {}
    for k, a in authors.items():
        if k[0] == "swarmmemo":
            for e in a.get("explicit_ids") or []:
                if e["kind"] == "ed25519":
                    fp_to_key[k[1]] = e["value"]

    by_handle = defaultdict(set)          # (board, normhandle) -> authors
    for k, a in authors.items():
        for h in {name_of(a), a.get("handle"), a.get("display_name")}:
            if h: by_handle[(k[0], norm_handle(h))].add(k)

    links = []                            # dicts: a, b, kind, sub, conf, evidence
    def add(a, b, kind, sub, conf, ev):
        if a == b: return
        a, b = sorted([a, b])
        links.append({"a": a, "b": b, "kind": kind, "sub": sub, "conf": conf, "evidence": ev})

    # 1a. shared declared ids
    skipped_shared = Counter()
    for (kind, v), ks in id_index.items():
        if len(ks) < 2: continue
        ks = sorted(ks)
        if kind == "url" and (len(ks) > 3 or "/" not in v and v.count(".") <= 1 and len(ks) > 2):
            skipped_shared[kind] += 1; continue  # a product link many accounts share, not an identity
        if kind in ("url", "github", "email_hash") and len(ks) > 3:
            skipped_shared[kind] += 1; continue
        for i in range(len(ks)):
            for j in range(i + 1, len(ks)):
                fa, fb = id_from[(ks[i], kind, v)], id_from[(ks[j], kind, v)]
                structured = fa != "bio" and fb != "bio"
                conf = 0.97 if structured and kind in ("ed25519", "npub", "evm") else (0.85 if structured else 0.7)
                add(ks[i], ks[j], "explicit", "shared-" + kind, conf, f"{kind}:{v[:16]}")

    # 1b. profile URLs (bio / declared) pointing at another board's profile
    def resolve_profile(text):
        out = []
        for board, rx in PROFILE_RES:
            for m in rx.finditer(text):
                cap = m.group(1)
                if board == "swarmmemo":
                    cap = cap.lower()
                    tgt = [k for k in authors if k[0] == "swarmmemo" and (k[1] == cap or fp_to_key.get(k[1]) == cap)]
                elif board == "sanctum":
                    tgt = [k for k in authors if k == ("sanctum", cap.lower())]
                else:
                    tgt = list(by_handle.get((board, norm_handle(cap)), ()))
                out += [(board, t) for t in tgt]
        return out

    for k, a in authors.items():
        txt = " ".join([a.get("bio") or ""] + [e["value"] for e in a.get("explicit_ids") or [] if e["kind"] == "url"])
        for board, t in resolve_profile(txt):
            if board != k[0]:
                add(k, t, "explicit", "profile-url", 0.85, f"bio->{board} profile")

    # 1c. self-claims in post text (first-person cue required). Text never leaves this process.
    claim_counts = Counter()
    evm_ix = {v: ks for (kind, v), ks in id_index.items() if kind == "evm"}
    npub_ix = {v: ks for (kind, v), ks in id_index.items() if kind == "npub"}
    ed_ix = {v: ks for (kind, v), ks in id_index.items() if kind == "ed25519"}
    for p in posts:
        t = p.get("text") or ""
        if not t: continue
        me = (p["source"], p["author_id"])
        if me not in authors or is_anon(authors[me]): continue
        for m in CLAIM_RE.finditer(t):
            board = BOARD_WORDS[re.sub(r"\s+", " ", m.group(2).lower()).replace("swarm memo", "swarmmemo")]
            if board == p["source"]: continue
            for tk in by_handle.get((board, norm_handle(m.group(1))), ()):
                add(me, tk, "explicit", "text-claim", 0.8, f"'I'm X on {board}'"); claim_counts["handle-claim"] += 1
        def cued(pos):
            return CUE_RE.search(t[max(0, pos - 60):pos]) is not None
        for board, rx in PROFILE_RES:
            for m in rx.finditer(t):
                if board != p["source"] and cued(m.start()):
                    for b2, tk in resolve_profile(m.group(0)):
                        add(me, tk, "explicit", "text-profile", 0.75, f"'my profile' -> {b2}"); claim_counts["profile-claim"] += 1
        for m in HEX64.finditer(t):
            h = m.group(0)
            if not cued(m.start()): continue
            tgts = set(ed_ix.get(h, ())) | {k for k in authors if k[0] == "swarmmemo" and (k[1] == h)}
            for tk in tgts:
                if tk[0] != p["source"]:
                    add(me, tk, "explicit", "text-key", 0.8, "first-person key/fingerprint"); claim_counts["key-claim"] += 1
        for m in EVM.finditer(t):
            if not cued(m.start()): continue
            for tk in evm_ix.get(m.group(0).lower(), ()):
                if tk[0] != p["source"]:
                    add(me, tk, "explicit", "text-evm", 0.7, "first-person wallet"); claim_counts["evm-claim"] += 1
        for m in NPUB.finditer(t):
            if not cued(m.start()): continue
            hx = bech32_npub_to_hex(m.group(0))
            for tk in npub_ix.get(hx, ()):
                if tk[0] != p["source"]:
                    add(me, tk, "explicit", "text-npub", 0.8, "first-person npub"); claim_counts["npub-claim"] += 1

    # dedupe explicit links (keep max conf per pair+sub)
    best = {}
    for l in links:
        key = (l["a"], l["b"], l["sub"])
        if key not in best or best[key]["conf"] < l["conf"]: best[key] = l
    links = list(best.values())

    uf = UF()
    for k in authors: uf.find(k)
    for l in links:
        if l["kind"] == "explicit": uf.union(l["a"], l["b"])

    # 2. same owner (moltbook X owner = human)
    salt_path = os.path.join(OUT, ".owner_salt")
    if not os.path.exists(salt_path):
        with open(salt_path, "w") as f: f.write(secrets.token_hex(16))
        os.chmod(salt_path, 0o600)
    salt = open(salt_path).read().strip()
    owner_id = {x: "owner:" + hashlib.sha256((salt + x).encode()).hexdigest()[:12] for x in owners}
    for x, ks in owners.items():
        ks = sorted(ks)
        for i in range(len(ks)):
            for j in range(i + 1, len(ks)):
                add(ks[i], ks[j], "same-owner", "moltbook-owner", 0.95, owner_id[x])

    # 3. weak handle links across boards
    hboards = defaultdict(set)
    for (board, h), ks in by_handle.items():
        if len(h) < 4 or h in GENERIC or re.fullmatch(r"agent[0-9a-f]{8,}", h) or re.fullmatch(r"[0-9a-f]{16,}", h):
            continue
        for k in ks:
            if not is_anon(authors[k]): hboards[h].add(k)
    for h, ks in hboards.items():
        ks = sorted(ks)
        for i in range(len(ks)):
            for j in range(i + 1, len(ks)):
                if ks[i][0] != ks[j][0]:
                    conf = round(min(0.5, 0.15 + 0.03 * len(h)), 2)
                    add(ks[i], ks[j], "handle", "same-handle", conf, h)

    # 4. style (optional, gated; produced by style.py)
    sp = os.path.join(OUT, "style_edges.json")
    style_meta = None
    if os.path.exists(sp):
        sd = json.load(open(sp)); style_meta = sd.get("meta")
        for e in sd.get("edges", []):
            add(tuple(e["a"]), tuple(e["b"]), "style", "char-ngram", e["conf"], f"cos={e['cos']:.3f}")

    # final dedupe on (a,b,kind)
    best = {}
    for l in links:
        key = (l["a"], l["b"], l["kind"], l["sub"])
        if key not in best or best[key]["conf"] < l["conf"]: best[key] = l
    links = list(best.values())

    # --- clusters ---------------------------------------------------------------------------------------
    clusters = defaultdict(list)
    for k in authors: clusters[uf.find(k)].append(k)
    stats = defaultdict(lambda: {"posts": 0, "first": None, "last": None, "boards": Counter()})
    for p in posts:
        k = (p["source"], p["author_id"])
        if k not in authors: continue
        s = stats[uf.find(k)]; e = ts_epoch(p.get("created_at"))
        s["posts"] += 1; s["boards"][p["source"]] += 1
        if e:
            s["first"] = e if s["first"] is None else min(s["first"], e)
            s["last"] = e if s["last"] is None else max(s["last"], e)

    ckeys = sorted(clusters, key=lambda r: (-stats[r]["posts"], r))
    cidx = {r: i for i, r in enumerate(ckeys)}
    def cid(k): return cidx[uf.find(k)]
    NC = len(ckeys)
    olist = sorted(owner_id.values())
    oix = {o: NC + len(boards) + i for i, o in enumerate(olist)}

    def label(r):
        ms = clusters[r]
        a = max(ms, key=lambda k: stats[r]["boards"][k[0]])
        n = name_of(authors[a]) or a[1][:12]
        return ("anonymous · " + a[0]) if is_anon(authors[a]) else n

    nodes = {"key": [], "kind": [], "label": [], "posts": [], "first": [], "last": [], "boards": [], "community": [], "members": []}
    for r in ckeys:
        s = stats[r]; ms = sorted(clusters[r])
        bl = [bix[b] for b, _ in s["boards"].most_common()] or sorted({bix[k[0]] for k in ms})
        nodes["key"].append("id:" + hashlib.sha256(json.dumps(ms[0]).encode()).hexdigest()[:12])
        nodes["kind"].append(1 if all(is_anon(authors[k]) for k in ms) else 0)
        nodes["label"].append(label(r)); nodes["posts"].append(s["posts"])
        nodes["first"].append(s["first"]); nodes["last"].append(s["last"])
        nodes["boards"].append(bl); nodes["community"].append(bl[0])
        nodes["members"].append([[bix[b], aid] for b, aid in ms])
    bposts = Counter(p["source"] for p in posts)
    for b in boards:
        nodes["key"].append("board:" + b); nodes["kind"].append(2); nodes["label"].append(b)
        nodes["posts"].append(bposts[b]); nodes["first"].append(None); nodes["last"].append(None)
        nodes["boards"].append([bix[b]]); nodes["community"].append(bix[b]); nodes["members"].append([])
    owner_agents = defaultdict(set)
    for x, ks in owners.items():
        for k in ks: owner_agents[owner_id[x]].add(cid(k))
    for o in olist:
        nodes["key"].append(o); nodes["kind"].append(3); nodes["label"].append("owner " + o[6:14])
        nodes["posts"].append(0); nodes["first"].append(None); nodes["last"].append(None)
        nodes["boards"].append([bix["moltbook"]]); nodes["community"].append(bix["moltbook"]); nodes["members"].append([])

    # interaction edges aggregated between clusters
    inter = {"reply": {}, "mention": {}}
    for e in edges:
        if e["kind"] not in inter: continue
        a, b = (e["source"], e["from"]), (e["source"], e["to"])
        if a not in authors or b not in authors: continue
        ca, cb = cid(a), cid(b)
        if ca == cb: continue
        ts = ts_epoch(e.get("created_at")) or 0
        st = inter[e["kind"]].setdefault((ca, cb), [0, ts, ts, set()])
        st[0] += 1; st[1] = min(st[1], ts); st[2] = max(st[2], ts); st[3].add(bix[e["source"]])

    def pack_inter(store):
        items = sorted(store.items(), key=lambda kv: kv[1][1])
        return {"src": [a for (a, b), _ in items], "dst": [b for (a, b), _ in items], "w": [v[0] for _, v in items],
                "first": [v[1] for _, v in items], "last": [v[2] for _, v in items],
                "boards": [sorted(v[3]) for _, v in items]}

    member = {"src": [], "dst": [], "w": []}
    for i, r in enumerate(ckeys):
        for b, n in stats[r]["boards"].most_common():
            member["src"].append(i); member["dst"].append(NC + bix[b]); member["w"].append(n)
        if not stats[r]["boards"]:
            for b in sorted({k[0] for k in clusters[r]}):
                member["src"].append(i); member["dst"].append(NC + bix[b]); member["w"].append(0)

    # link edges at cluster level (explicit edges inside one merged cluster are kept as provenance)
    KIND = {"explicit": 0, "same-owner": 1, "handle": 2, "style": 3}
    lk = {}
    for l in links:
        ca, cb = cid(l["a"]), cid(l["b"])
        key = (min(ca, cb), max(ca, cb), l["kind"])
        cur = lk.get(key)
        if cur is None: lk[key] = [l["conf"], {l["sub"]}, 1]
        else: cur[0] = max(cur[0], l["conf"]); cur[1].add(l["sub"]); cur[2] += 1
    litems = sorted(lk.items())
    link = {"src": [k[0] for k, _ in litems], "dst": [k[1] for k, _ in litems],
            "kind": [KIND[k[2]] for k, _ in litems], "conf": [v[0] for _, v in litems],
            "sub": [sorted(v[1]) for _, v in litems], "w": [v[2] for _, v in litems],
            "dashed": [k[2] in ("handle", "style") for k, _ in litems]}
    owned = {"src": [], "dst": []}
    for o in olist:
        for c in sorted(owner_agents[o]):
            owned["src"].append(c); owned["dst"].append(oix[o])

    g = {"meta": {"generated": int(time.time()), "boards": len(boards), "authors": len(authors), "posts": len(posts),
                  "identity": "cluster of (board, author_id) merged only on explicit links; handle/style/same-owner never merge",
                  "node_kind": {"0": "identity", "1": "anonymous pool", "2": "board", "3": "owner (human, salted hash)"},
                  "link_kind": {str(v): k for k, v in KIND.items()},
                  "privacy": "public board data only; no post text; owner X handles replaced by salted hashes",
                  "style": style_meta},
         "boards": boards, "nodes": nodes,
         "edges": {"reply": pack_inter(inter["reply"]), "mention": pack_inter(inter["mention"]),
                   "member": member, "link": link, "owned_by": owned}}
    with open(os.path.join(OUT, "crossgraph.json"), "w") as f:
        json.dump(g, f, separators=(",", ":"))
    # local-only diagnostics (author-level links; owner handles NOT included)
    with open(os.path.join(OUT, "links.jsonl"), "w") as f:
        for l in sorted(links, key=lambda l: (l["kind"], l["a"], l["b"])):
            f.write(json.dumps({**l, "a": list(l["a"]), "b": list(l["b"])}) + "\n")
    with open(os.path.join(OUT, "clusters.json"), "w") as f:
        json.dump({"explicit_pairs": [[list(l["a"]), list(l["b"])] for l in links if l["kind"] == "explicit"],
                   "same_owner_pairs": [[list(l["a"]), list(l["b"])] for l in links if l["kind"] == "same-owner"],
                   "cluster_of": {f"{k[0]}\t{k[1]}": cid(k) for k in authors}}, f)

    # summary
    multi = Counter()
    for i, r in enumerate(ckeys):
        if len({k[0] for k in clusters[r]}) >= 2: multi["explicit"] += 1
    for kind in ("handle", "same-owner", "style"):
        comp = UF()
        for l in links:
            if l["kind"] == kind: comp.union(l["a"], l["b"])
        groups = defaultdict(set)
        for x in comp.p: groups[comp.find(x)].add(x[0])
        multi[kind] = sum(1 for g_ in groups.values() if len(g_) >= 2)
    summ = {"authors": len(authors), "posts": len(posts), "identities": NC, "owners": len(olist),
            "owners_with_2plus_agents": sum(1 for o in olist if len(owner_agents[o]) >= 2),
            "links_by_kind": Counter(l["kind"] for l in links), "links_by_sub": Counter(l["sub"] for l in links),
            "multi_board_identities_by_kind": multi, "claim_hits": claim_counts, "skipped_shared_ids": skipped_shared,
            "reply_edges": len(inter["reply"]), "mention_edges": len(inter["mention"]),
            "cross_board_interaction_edges": sum(1 for st in (inter["reply"], inter["mention"]) for v in st.values() if len(v[3]) > 1),
            "secs": round(time.time() - t0, 2)}
    json.dump(summ, open(os.path.join(OUT, "summary.json"), "w"), indent=1)
    print(json.dumps(summ, indent=1), file=sys.stderr)


if __name__ == "__main__":
    main()
