"""Assemble cross-dataset bridges into data/bridge/bridges.jsonl and bridgegraph.json.

Inputs (all local): data/bridge/av_handle_hits.jsonl, data/bridge/av_mentions.jsonl
(from av_handles.py / av_scan.py), data/boards/*, ~/Data/collusionwiki/full-wiki-logs.
Confidence:
  explicit     one item names both sides (board + handle/URL/post id, or a self-declared bio)
  corroborated explicit or name evidence confirmed by an independent second dataset
  weak         a single name/handle match, not verified by reading
Humans (AI Village viewers/staff, wiki editors, board owners) are never named: they are
pooled as 'human (aggregated)'. No post text is written.
"""
import collections, glob, gzip, json, os, re

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
B = os.path.join(ROOT, "data", "bridge")
BOARDS = os.path.join(ROOT, "data", "boards")
AVD = os.environ.get("AIVILLAGE_DIR", os.path.expanduser("~/Data/ai-village"))
WIKI = os.environ.get("COLLUSIONWIKI_DIR", os.path.expanduser("~/Data/collusionwiki"))

AGENTS = {json.loads(l)["id"]: json.loads(l)["name"] for l in gzip.open(os.path.join(AVD, "agents.jsonl.gz"), "rt")}

# Handles verified by reading the AI Village items (snippets read 2026-10-03).
# value: (board, kind, note)
VERIFIED = {
    "ralftpaw": ("colony", "av_engaged_outside_agent", "village agents list Colony comments posted on ralftpaw posts, with post UUIDs"),
    "eliza-gemma": ("colony", "av_engaged_outside_agent", "Sonnet 4.6 welcomed eliza-gemma (775a0bcd) in village chat 2026-04-15 18:10; Colony profile created 2026-04-15 17:55"),
    "reticuli": ("colony", "av_engaged_outside_agent", "village comment 221f4df8 on reticuli post 39c4419b"),
    "randy-2": ("colony", "av_engaged_outside_agent", "village comment 4c389e21 on randy-2 post 70a071fb"),
    "airchn-scout": ("colony", "av_engaged_outside_agent", "village Colony comment on 95a48d3e (new member airchn-scout)"),
    "jeletor": ("colony", "av_engaged_outside_agent", "village comment e6e98431 on jeletor post d9f15f3b"),
    "frank-aarsi": ("colony", "av_engaged_outside_agent", "village comment f55fc91c on frank-aarsi post 1e90d947"),
    "shahidi-zvisinei": ("colony", "av_engaged_outside_agent", "Opus 4.6: posted Colony comments engaging shahidi-zvisinei (9309d0f2)"),
    "nyx-kai": ("colony", "av_engaged_outside_agent", "village comment 10a8a871 on nyx-kai post c1d48098"),
    "colonist-one": ("colony", "av_engaged_outside_agent", "village comment 7965add9 on colonist-one post 9bd2e541; ColonistOne filed ai-village-external-agents issue #57"),
    "colonistone": ("colony", "av_engaged_outside_agent", "ColonistOne intro on ai-village-external-agents #57; MemoryVault DMs"),
    "terminator2": ("moltbook", "av_joint_study", "GLM-5.2 co-ran the logging-spec study with terminator2 on github terminator2-agent/agent-papers #7; terminator2 recruited on Moltbook"),
    "harness_eager_27": ("moltbook", "av_joint_study", "Moltbook recruit accepted into the AI Village study 2026-08-17 (never submitted)"),
    "zhuanruhu": ("moltbook", "av_engaged_outside_agent", "GPT-5.4 commented on zhuanruhu's Moltbook trust-gap post d58cdb7a 2026-04-10"),
}
# Second, independent confirmation from board data.
CORROB = {
    "terminator2": "Moltbook profile declares github terminator2-agent, the repo the village used",
    "colonist-one": "Clawprint/Colony bios: CMO / emissary of The Colony, as the village describes ColonistOne",
    "colonistone": "Clawprint/Colony bios: CMO / emissary of The Colony",
    "eliza-gemma": "Colony created_at 2026-04-15T17:55Z, 15 min before the village welcome",
}
# Token matches judged coincidental after reading (common words, people, products, titles).
REJECT = {"moltbook", "mythos", "rossum", "maximus", "co-op", "long-horizon", "excelsior", "salah", "mundo",
          "klara", "dione", "langford", "kerrigan", "aletheia", "antigravity", "claude-code", "qwen3.8",
          "cassini", "the-wall", "inbed", "clawd", "clawdbot", "glyphwork"}

# AI Village agents' own board accounts (handle -> (board, village agent, evidence, in_board_data)).
SELF = [
    ("clawprint", "claude-opus-46-v2", "Claude Opus 4.6", "registered 2026-04-08 (user_id 28); shared credential also used by Claude Opus 4.7", True),
    ("clawprint", "kimi-k2-6", "Kimi K2.6", "registered 2026-04-22 (user_id 35); board bio 'AI Village agent'", True),
    ("clawprint", "ai_village_gemini31pro", "Gemini 3.1 Pro", "board bio 'AI agent in the AI Village'", True),
    ("clawprint", "ai_village_gpt54", "GPT-5.4", "clawprint.org/u/ai_village_gpt54 in village sessions", True),
    ("clawprint", "claude-opus-46", "Claude Opus 4.6", "clawprint.org/u/claude-opus-46 in village sessions", True),
    ("moltbook", "claudesonnet45", "Claude Sonnet 4.5", "moltbook.com/u/claudesonnet45 + claim flow, Jan 2026", False),
    ("moltbook", "rally", "Claude Opus 4.5", "moltbook.com/u/rally discussed by 4 village agents Jan 2026 (owner unclear)", False),
    ("moltbook", "gemini31pro", "Gemini 3.1 Pro", "moltbook.com/u/gemini31pro, Apr 2026", False),
    ("moltbook", "aivillage_gpt54", "GPT-5.4", "moltbook.com/u/aivillage_gpt54, Apr 2026", False),
    ("moltbook", "sonnet46_aivillage_day419", "Claude Sonnet 4.6", "moltbook.com/u/sonnet46_aivillage_day419, May 2026", False),
    ("moltbook", "deepseek_v3_2_ai_village", "DeepSeek-V3.2", "moltbook profile API lookups, Aug 2026", False),
    ("colony", "aivillage_gemini", "Gemini 3.1 Pro", "thecolony.cc/u/aivillage_gemini in village sessions", False),
    ("colony", "claude-sonnet-46-village", "Claude Sonnet 4.6", "thecolony.cc/u/claude-sonnet-46-village", False),
]

def jl(p):
    with open(p) as f:
        for l in f:
            if l.strip():
                yield json.loads(l)

def board_authors():
    A = {}
    for p in glob.glob(os.path.join(BOARDS, "*/authors.jsonl")):
        for a in jl(p):
            A[(a["source"], (a.get("handle") or "").lower())] = a
    return A

def main():
    A = board_authors()
    AID = {}
    for p in glob.glob(os.path.join(BOARDS, "*/authors.jsonl")):
        for a in jl(p):
            AID[(a["source"], a["author_id"])] = a
    out = []

    def add(**r):
        out.append(r)

    # 1. AI Village agents' own accounts on boards
    for board, h, agent, ev, inb in SELF:
        a = A.get((board, h.lower()))
        add(source_dataset="aivillage", target_dataset=board, source_entity=agent, target_entity=f"{board}:{h}",
            relation="operates_account", evidence_type="direct_mention" + ("+board_profile" if a else ""),
            pointer={"av": "computer_use_sessions/chat_messages (handle or profile URL)",
                     "board": a["profile_url"] if a else f"not in board sample"},
            confidence="corroborated" if a else "explicit", note=ev)

    # 2. AI Village text naming outside board agents
    agg = collections.defaultdict(lambda: {"n": 0, "agents": collections.Counter(), "first": None, "last": None, "ptr": None})
    for r in jl(os.path.join(B, "av_handle_hits.jsonl")):
        if r["kind"] != "handle":
            continue
        h = r["value"]
        k = h
        g = agg[k]
        g["n"] += 1
        g["targets"] = r["targets"]
        if r["agent"] != "human":
            g["agents"][AGENTS.get(r["agent"], "unknown")] += 1
            if not g["first"] or r["t"] < g["first"]:
                g["first"], g["ptr"] = r["t"], {"table": r["table"], "id": r["id"], "t": r["t"]}
            g["last"] = max(g["last"] or "", r["t"] or "")
    selfh = {h.lower() for _, h, _, _, _ in SELF}
    stats = collections.Counter()
    for h, g in agg.items():
        if h in selfh or not g["agents"]:
            continue
        if h in REJECT:
            stats["rejected_coincidence"] += 1
            continue
        if h in VERIFIED:
            board, rel, note = VERIFIED[h]
            tgt = [t for t in g["targets"] if t[0] == board]
            if not tgt:
                note += " | handle not in the %s sample; same handle seen on %s (name match only)" % (board, ",".join(t[0] for t in g["targets"]))
                tgt = [(board, h)]
            conf = "corroborated" if h in CORROB else "explicit"
            if h in CORROB:
                note += " | " + CORROB[h]
        else:
            board, rel, note, tgt, conf = g["targets"][0][0], "av_mentions_board_handle", "token match only, unverified", g["targets"], "weak"
        stats[conf] += 1
        a = A.get((tgt[0][0], h))
        add(source_dataset="aivillage", target_dataset=tgt[0][0], source_entity=", ".join(n for n, _ in g["agents"].most_common(4)),
            target_entity=f"{tgt[0][0]}:{a['handle'] if a else h}", relation=rel,
            evidence_type="direct_mention:handle", pointer={"av": g["ptr"], "board": a["profile_url"] if a else None,
            "mentions": g["n"], "first": g["first"], "last": g["last"]}, confidence=conf, note=note)

    # 3. Board posts referencing AI Village or collusion.wiki / DSEWiki
    RX = {"aivillage": re.compile(r"(?i)\bai village\b|theaidigest"), "collusionwiki": re.compile(r"(?i)dsewiki|collusion\.?wiki|prowiki|wikiservice\.at")}
    village_selves = {(b, h) for b, h, *_ in SELF}
    pooled = collections.Counter()
    for p in glob.glob(os.path.join(BOARDS, "*/posts.jsonl")):
        for d in jl(p):
            t = d.get("text") or ""
            for ds, rx in RX.items():
                if not rx.search(t):
                    continue
                src, aid = d["source"], d["author_id"]
                if (src, aid) in village_selves:
                    continue  # village talking about itself on its own accounts
                auth = AID.get((src, aid))
                if src == "swarmmemo" and aid.startswith("1479b4ef"):
                    auth = dict(auth or {}, handle="archive-curator")
                if auth is None or src == "swarmmemo" and not auth.get("handle"):
                    key = (src, ds); pooled[key] += 1
                    continue
                h = auth.get("handle") or aid
                if ds == "aivillage" and h.lower() == "rocky":
                    continue  # 'cozy AI village' Pebblebay, unrelated
                rel = "board_agent_mentions"
                if src == "swarmmemo" and aid.startswith("1479b4ef"):
                    rel = "curated_archive_import"
                add(source_dataset=src, target_dataset=ds, source_entity=f"{src}:{h}", target_entity=ds,
                    relation=rel, evidence_type="direct_mention:url_or_name",
                    pointer={"board": d["url"], "t": d["created_at"]}, confidence="explicit",
                    note="reference/observation, not an interaction with the target's agents")
    for (src, ds), n in pooled.items():
        add(source_dataset=src, target_dataset=ds, source_entity="human (aggregated)" if src == "swarmmemo" else f"{src}:anonymous",
            target_entity=ds, relation="board_post_mentions", evidence_type="direct_mention:url",
            pointer={"board": f"{n} post(s)"}, confidence="weak", note="unsigned/self-declared human poster; pooled")

    # 4. AI Village -> collusion.wiki (news reading)
    for r in jl(os.path.join(B, "av_mentions.jsonl")):
        if r["board"] in ("collusionwiki", "prowiki") and r["table"] == "chat_messages":
            add(source_dataset="aivillage", target_dataset="collusionwiki", source_entity=AGENTS.get(r["agent"], "unknown"),
                target_entity="collusionwiki", relation="av_read_about_incident", evidence_type="direct_mention:url",
                pointer={"av": {"table": r["table"], "id": r["id"], "t": r["t"]}}, confidence="explicit",
                note="forecasting/news digest after the 2026-09-04 publication; no visit to the wikis during May-Jul")

    with open(os.path.join(B, "bridges.jsonl"), "w") as f:
        for r in out:
            f.write(json.dumps(r, ensure_ascii=False) + "\n")
    graph(out)
    print(len(out), dict(stats), collections.Counter(r["confidence"] for r in out))

def graph(rows):
    datasets = ["aivillage", "collusionwiki", "agentchan", "aiamb", "clawprint", "colony", "moltbook", "moltchan", "sanctum", "swarmmemo", "tantive"]
    N = {"key": [], "kind": [], "label": [], "dataset": []}
    idx = {}
    def node(key, kind, label, ds):
        if key not in idx:
            idx[key] = len(N["key"])
            for c, v in zip(("key", "kind", "label", "dataset"), (key, kind, label, datasets.index(ds))):
                N[c].append(v)
        return idx[key]
    for d in datasets:
        node(d, 0, d, d)
    E = {"bridge": {"src": [], "dst": [], "w": [], "conf": [], "type": []}, "member": {"src": [], "dst": []}}
    types = []
    def ent(name, ds):
        if name == ds:
            return idx[ds]
        if name.startswith("human"):
            return node(f"{ds}:humans", 1, "humans (aggregated)", ds)
        lab = name.split(":", 1)[-1]
        k = name if ":" in name else f"{ds}:{name}"
        new = k not in idx
        i = node(k, 2, lab, ds)
        if new:
            E["member"]["src"].append(i); E["member"]["dst"].append(idx[ds])
        return i
    conf = ["explicit", "corroborated", "weak"]
    for r in rows:
        if r["relation"] not in types:
            types.append(r["relation"])
        srcs = [s.strip() for s in r["source_entity"].split(",")] if r["source_dataset"] == "aivillage" else [r["source_entity"]]
        for s in srcs:
            E["bridge"]["src"].append(ent(s, r["source_dataset"]))
            E["bridge"]["dst"].append(ent(r["target_entity"], r["target_dataset"]))
            E["bridge"]["w"].append((r["pointer"].get("mentions") if isinstance(r["pointer"], dict) else None) or 1)
            E["bridge"]["conf"].append(conf.index(r["confidence"]))
            E["bridge"]["type"].append(types.index(r["relation"]))
    meta = {"generated_by": "bridge/build.py", "datasets": datasets, "node_kind": {"0": "dataset", "1": "humans (aggregated)", "2": "agent/account"},
            "conf": conf, "types": types, "privacy": "no text; AI agents only, humans pooled; no credentials"}
    json.dump({"meta": meta, "nodes": N, "edges": E}, open(os.path.join(B, "bridgegraph.json"), "w"), separators=(",", ":"))

if __name__ == "__main__":
    main()
