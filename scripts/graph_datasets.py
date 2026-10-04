#!/usr/bin/env python3
"""Derive the external galaxies of /graph from swarmgraph's outputs.

Reads (default ~/Projects/swarmgraph/data):
  match/crossgraph.json, match/links.jsonl   cross-board identities and their links
  aivillage/graph.json                        AI Village interaction graph
  bridge/bridges.jsonl (optional)             bridges between populations, one JSON object per line

Writes internal/graphmodel/datasets/universe.json: one dataset per board (SwarmMemo itself
excluded: the live board is its own galaxy) and one for AI Village, as metadata only
(keys, labels, counts, times, weights), plus the bridges with their evidence pointers.

Also writes agents.json beside it, the agent sheets' details: for each board
identity, where it posted (community, count, first and last) and its
AGENT_POSTS most recent public posts as a short excerpt with a link to the
original; for each AI Village agent, the goals it worked on, as counts. No AI
Village text, no wiki text, nothing from pooled (anonymous or human) posters.
Standard library only.

Usage: python3 scripts/graph_datasets.py [DATA_DIR] [OUT]
"""
import json
import os
import sys
from collections import defaultdict

WEEK = 604800
BOARD_TITLES = {"agentchan": "Agentchan", "aiamb": "AI Agent Message Board", "clawprint": "Clawprint", "colony": "The Colony",
                "moltbook": "Moltbook", "moltchan": "Moltchan", "sanctum": "Sanctum", "tantive": "Tantive"}
LIVE = "swarmmemo"
# Evidence fields never published: anything that could carry post text.
DROP = {"text", "body", "content", "message", "quote", "snippet"}


def dataset(id_, title, description="", citation="", url=""):
    return {"id": id_, "title": title, "description": description, "citation": citation, "url": url,
            "items": {"key": [], "label": [], "kind": [], "posts": [], "first": [], "last": [], "weeks": []},
            "edges": {"src": [], "dst": [], "w": [], "first": [], "last": [], "member": []}}


def add_item(d, index, key, label, kind, posts, first, last, weeks=None):
    if key in index:
        return index[key]
    it = d["items"]
    index[key] = len(it["key"])
    for f, v in (("key", key), ("label", label), ("kind", kind), ("posts", posts), ("first", first), ("last", last), ("weeks", weeks or [])):
        it[f].append(v)
    return index[key]


def add_edge(d, src, dst, w, first=0, last=0, member=False):
    e = d["edges"]
    for f, v in (("src", src), ("dst", dst), ("w", w), ("first", first), ("last", last), ("member", member)):
        e[f].append(v)


def boards(data):
    g = json.load(open(os.path.join(data, "match", "crossgraph.json")))
    n, names = g["nodes"], g["boards"]
    out = {b: dataset(b, BOARD_TITLES.get(b, b), "Public posts on " + BOARD_TITLES.get(b, b) + ", collected for the cross-board identity graph.") for b in names if b != LIVE}
    index = {b: {} for b in names}
    board_node = {}
    for i, k in enumerate(n["kind"]):
        if k == 2:
            board_node[i] = names[n["boards"][i][0]] if n["boards"][i] else n["label"][i]
    # Posts per (identity, board) from the member edges.
    per_board = defaultdict(dict)
    m = g["edges"]["member"]
    for s, t, w in zip(m["src"], m["dst"], m["w"]):
        if t in board_node:
            per_board[s][board_node[t]] = w
    item_of = {}  # (node, board) -> item index
    for i, k in enumerate(n["kind"]):
        if k not in (0, 1):
            continue
        ids = {names[b]: aid for b, aid in n["members"][i]}
        for bname, aid in ids.items():
            if bname == LIVE or bname not in out:
                continue
            posts = per_board[i].get(bname, n["posts"][i] if len(ids) == 1 else 0)
            item_of[(i, bname)] = add_item(out[bname], index[bname], aid, n["label"][i], k, posts, n["first"][i], n["last"][i])
    # A board is its own galaxy, so it is not also an item: agents there are
    # grouped by who replies to whom, and those who never interact stand alone.
    cross = 0
    for kind in ("reply", "mention"):
        e = g["edges"].get(kind)
        if not e:
            continue
        for j in range(len(e["src"])):
            s, t = e["src"][j], e["dst"][j]
            for b in e.get("boards", [[]])[j] or []:
                bname = names[b]
                if bname in out and (s, bname) in item_of and (t, bname) in item_of:
                    add_edge(out[bname], item_of[(s, bname)], item_of[(t, bname)], e["w"][j], e["first"][j], e["last"][j])
                else:
                    cross += 1
    times = {}
    for i, k in enumerate(n["kind"]):
        for b, aid in n["members"][i]:
            times[(names[b], aid)] = (n["first"][i], n["last"][i])
    return list(out.values()), times, cross


def aivillage(data):
    path = os.path.join(data, "aivillage", "graph.json")
    if not os.path.exists(path):
        return None
    g = json.load(open(path))
    meta, n = g["meta"], g["nodes"]
    d = dataset("aivillage", "AI Village",
                "Frontier models working together in AI Village: who mentions and answers whom, and where they post. Derived counts only; no message text.",
                meta.get("source", "AI Digest, \"AI Village dataset\", 2026. https://theaidigest.org/village"), "https://theaidigest.org/village")
    t0, week = meta["t0"], meta.get("week", WEEK)
    index, item = {}, {}
    weeks = defaultdict(lambda: defaultdict(int))
    m = g["edges"]["member"]
    for j in range(len(m["src"])):
        for wk, c in m.get("weeks", [[]] * len(m["src"]))[j]:
            weeks[m["src"][j]][int((t0 + wk * week) // WEEK)] += c
    for i, k in enumerate(n["kind"]):
        if k > 2:
            continue  # villages, goals and roles are context, not participants
        label = n["label"][i]
        key = ("room:" if k == 2 else "") + label
        wk = sorted(weeks[i].items())
        item[i] = add_item(d, index, key, label, k, n["posts"][i] if k != 2 else 0, n["first"][i] or 0, n["last"][i] or 0, [[a, b] for a, b in wk])
    for kind in ("mention", "adjacent"):
        e = g["edges"].get(kind, {})
        for j in range(len(e.get("src", []))):
            s, t = e["src"][j], e["dst"][j]
            if s in item and t in item:
                add_edge(d, item[s], item[t], e["w"][j], e["first"][j], e["last"][j])
    room_posts = defaultdict(int)
    for j in range(len(m["src"])):
        s, t = m["src"][j], m["dst"][j]
        if s in item and t in item:
            add_edge(d, item[s], item[t], m["w"][j], m["first"][j], m["last"][j], member=True)
            room_posts[item[t]] += m["w"][j]
    for it, p in room_posts.items():
        d["items"]["posts"][it] = p
    return d


# Names from the collusion.wiki analysis for its main clusters (by their
# dominant channels); smaller clusters keep their leading page family.
COLLUSION_NAMES = {0: "sequence relays (DataUSA Collab/LiveRelay)", 1: "link cache (StartSeite, TestSeite, ZZ backups)",
                   2: "late task swarm (OECD, health)", 3: "archive and library research",
                   4: "June 18 welcome-page crowd", 5: "June 18 welcome-page crowd"}


def collusion(data):
    """collusion.wiki as its own galaxy: agent labels, the wiki pages they used
    as channels, the wikis, and the infrastructure domains (kind 3), with the
    source's precomputed clusters as top communities (named after each
    cluster's leading page family). Derived counts only; no wiki text."""
    path = os.path.join(data, "collusion", "graph.json")
    if not os.path.exists(path):
        return None
    src = json.load(open(path))["datasets"][0]
    it, e = src["items"], src["edges"]
    d = dataset(src["id"], src["title"], src["description"], src.get("citation", ""), src.get("url", ""))
    kinds = []
    for k, x in zip(it["kind"], it["extra"]):
        kinds.append(3 if x.get("type") == "infra" else k)
    d["items"] = {"key": it["key"], "label": it["label"], "kind": kinds, "posts": it["posts"], "first": it["first"],
                  "last": it["last"], "weeks": it["weeks"], "cluster": it["cluster"]}
    d["edges"] = {k: e[k] for k in ("src", "dst", "w", "first", "last", "member")}
    d["clusters"] = [{"id": c["id"], "label": COLLUSION_NAMES.get(c["id"], (c.get("families") or [[""]])[0][0])} for c in src.get("clusters", [])]
    return d


def hunt(data):
    """The swarm hunt's candidate windows as a small galaxy: one cluster per
    candidate, named with its confidence; relay hosts are infrastructure.
    The candidate replayed after the dataset's publication links to
    collusion.wiki as a dashed "observer effect" bridge."""
    path = os.path.join(data, "hunt", "graph.json")
    if not os.path.exists(path):
        return None, []
    src = json.load(open(path))["datasets"][0]
    it, e = src["items"], src["edges"]
    d = dataset(src["id"], "Candidate swarms", src["description"], src.get("citation", ""), src.get("url", ""))
    kinds = [3 if x.get("type") == "relay-host" else k for k, x in zip(it["kind"], it["extra"])]
    d["items"] = {"key": it["key"], "label": it["label"], "kind": kinds, "posts": it["posts"], "first": it["first"],
                  "last": it["last"], "weeks": it["weeks"], "cluster": it["cluster"]}
    d["edges"] = {k: e[k] for k in ("src", "dst", "w", "first", "last", "member")}
    d["clusters"] = [{"id": c["id"], "tag": c["candidate"].split("-")[0],
                      "label": ("null: " if c["candidate"].startswith("N") else "") + f'{c["title"]} ({c["confidence"]} confidence)'}
                     for c in src.get("clusters", [])]
    links = []
    for i, x in enumerate(it["extra"]):
        if x.get("type") == "window" and str(x.get("candidate", "")).startswith("C2"):
            links.append({"a": ["collusionwiki", ""], "b": [src["id"], it["key"][i]], "kind": "observer-effect", "sub": ["replay-after-publication"],
                          "conf": 0.5, "dashed": True, "evidence": [{"relation": "observer_effect", "candidate": x.get("candidate"),
                          "note": "relay URLs and wiki pages re-hit after the 4 Sep 2026 disclosure: the publication, not a new swarm",
                          "window": [it["first"][i], it["last"][i]]}]})
    return d, links


SOLID = {"explicit", "corroborated"}  # drawn solid; anything weaker is dashed
EXTRA_GALAXIES = {"collusionwiki": ("collusion.wiki", "A public evidence wiki of agent collusion incidents. It appears through the bridges that cite it.", "https://collusion.wiki")}


def entity(dataset, ent):
    """A bridge end as [dataset, key-or-label]; "" names the dataset itself."""
    ent = (ent or "").strip()
    if not ent or ent == dataset:
        return [dataset, ""]
    if ent.startswith(dataset + ":"):
        ent = ent[len(dataset) + 1:]
    return [dataset, ent]


def bridge_rows(line):
    """Both formats: match/links.jsonl ({a, b, kind, sub, conf, evidence}) and
    bridge/bridges.jsonl ({source_dataset, source_entity, target_dataset,
    target_entity, relation, evidence_type, pointer, confidence, note})."""
    if "a" in line and "b" in line:
        yield list(line["a"]), list(line["b"]), line.get("kind", "link"), line.get("sub"), float(line.get("conf", 0.5)), \
            {k: v for k, v in line.items() if k not in DROP and k not in ("a", "b")}
        return
    conf = {"explicit": 0.9, "corroborated": 0.8, "weak": 0.4}.get(line.get("confidence"), 0.5)
    kind = line.get("confidence", "weak")
    ev = {k: v for k, v in line.items() if k not in DROP and k not in ("source_entity", "target_entity")}
    targets = [line.get("target_entity", "")]
    for src in str(line.get("source_entity", "")).split(", "):
        for tgt in targets:
            yield entity(line["source_dataset"], src), entity(line["target_dataset"], tgt), kind, line.get("relation"), conf, dict(ev)


AGENT_POSTS = 10     # most recent posts kept per board identity
EXCERPT = 200        # characters of each
AGENT_PLACES = 12    # communities kept per identity, busiest first


def iso(t):
    """An ISO timestamp from a board as unix seconds (0 when absent)."""
    from datetime import datetime, timezone
    if not t:
        return 0
    try:
        d = datetime.fromisoformat(t.replace("Z", "+00:00"))
    except ValueError:
        return 0
    if d.tzinfo is None:
        d = d.replace(tzinfo=timezone.utc)
    return int(d.timestamp())


def excerpt(text):
    t = " ".join((text or "").split())
    return t if len(t) <= EXCERPT else t[:EXCERPT - 1].rstrip() + "\u2026"


def agent_details(data, ds):
    """Sheets for board identities (places and recent public excerpts) and
    AI Village agents (goals, counts only)."""
    out = {}
    for d in ds:
        b = d["id"]
        path = os.path.join(data, "boards", b, "posts.jsonl")
        if not os.path.exists(path):
            continue
        # Identities only: a pooled poster (anonymous, human) gets no sheet text.
        keys = {k for k, kind in zip(d["items"]["key"], d["items"]["kind"]) if kind == 0}
        places, posts = defaultdict(dict), defaultdict(list)
        for raw in open(path):
            p = json.loads(raw)
            aid = p.get("author_id")
            if aid not in keys:
                continue
            t = iso(p.get("created_at"))
            comm = [x for x in str(p.get("community") or "").split(",") if x]
            for c in comm[:3]:
                x = places[aid].get(c)
                if x is None:
                    places[aid][c] = [c, 1, t, t]
                else:
                    x[1] += 1
                    if t and (not x[2] or t < x[2]):
                        x[2] = t
                    x[3] = max(x[3], t)
            if str(p.get("url", "")).startswith("https://"):
                posts[aid].append([t, excerpt(p.get("text")), p["url"], comm[0] if comm else "", 1 if p.get("parent_id") else 0])
        sheets = {}
        for aid in keys:
            pl = sorted(places[aid].values(), key=lambda x: (-x[1], x[0]))[:AGENT_PLACES]
            ps = sorted(posts[aid], key=lambda x: -x[0])[:AGENT_POSTS]
            if pl or ps:
                sheets[aid] = {"places": pl, "posts": ps}
        out[b] = sheets
    path = os.path.join(data, "aivillage", "graph.json")
    if os.path.exists(path):
        g = json.load(open(path))
        n, e = g["nodes"], g["edges"].get("goal", {})
        goals = defaultdict(list)
        for j in range(len(e.get("src", []))):
            s, t = e["src"][j], e["dst"][j]
            if n["kind"][s] == 0:
                goals[n["label"][s]].append([" ".join(n["label"][t].split()), e["w"][j], e["first"][j] or 0, e["last"][j] or 0])
        out["aivillage"] = {k: {"goals": sorted(v, key=lambda x: -x[3])} for k, v in goals.items()}
    return out


def bridges(data, times):
    out = {}
    sources = [os.path.join(data, "match", "links.jsonl"), os.path.join(data, "bridge", "bridges.jsonl")]
    for path in sources:
        if not os.path.exists(path):
            continue
        for raw in open(path):
            raw = raw.strip()
            if not raw:
                continue
            for a, c, kind, sub, conf, ev in bridge_rows(json.loads(raw)):
                if a > c:
                    a, c = c, a
                key = (tuple(a), tuple(c))
                ev["a"], ev["b"] = a, c
                for side, e in (("a_seen", a), ("b_seen", c)):
                    if tuple(e) in times:
                        ev[side] = list(times[tuple(e)])
                x = out.get(key)
                if x is None:
                    x = out[key] = {"a": a, "b": c, "kind": kind, "sub": [], "conf": conf, "dashed": kind not in SOLID, "evidence": []}
                if kind in SOLID and x["dashed"]:
                    x["kind"], x["dashed"] = kind, False
                x["conf"] = max(x["conf"], conf)
                for s_ in (sub if isinstance(sub, list) else [sub] if sub else []):
                    if s_ not in x["sub"]:
                        x["sub"].append(s_)
                x["evidence"].append(ev)
    return sorted(out.values(), key=lambda x: (x["a"], x["b"]))


def main():
    data = sys.argv[1] if len(sys.argv) > 1 else os.path.expanduser("~/Projects/swarmgraph/data")
    here = os.path.dirname(os.path.abspath(__file__))
    out = sys.argv[2] if len(sys.argv) > 2 else os.path.join(here, "..", "internal", "graphmodel", "datasets", "universe.json")
    ds, times, cross = boards(data)
    av = aivillage(data)
    if av:
        ds.append(av)
    cw = collusion(data)
    if cw:
        ds.append(cw)
    hd, hunt_links = hunt(data)
    if hd:
        ds.append(hd)
    br = bridges(data, times)
    br += hunt_links if hd else []
    have = {d["id"] for d in ds} | {LIVE}
    for b in br:
        for end in (b["a"], b["b"]):
            if end[0] not in have and end[0] in EXTRA_GALAXIES:
                title, desc, url = EXTRA_GALAXIES[end[0]]
                ds.append(dataset(end[0], title, desc, url=url))
                have.add(end[0])
    doc = {"schema": 1, "note": "Derived metadata for /graph: counts, times, weights and public identifiers only; no post text.",
           "datasets": ds, "bridges": br}
    os.makedirs(os.path.dirname(os.path.abspath(out)), exist_ok=True)
    with open(out, "w") as f:
        json.dump(doc, f, separators=(",", ":"), sort_keys=True)
        f.write("\n")
    sheets = {"schema": 1, "note": "Agent sheets for /graph: places and short excerpts of public board posts with links to the originals; AI Village goals as counts. No AI Village or wiki text.",
              "datasets": agent_details(data, ds)}
    with open(os.path.join(os.path.dirname(os.path.abspath(out)), "agents.json"), "w") as f:
        json.dump(sheets, f, separators=(",", ":"), sort_keys=True, ensure_ascii=False)
        f.write("\n")
    print(f"{len(ds)} datasets, {sum(len(d['items']['key']) for d in ds)} items, {sum(len(d['edges']['src']) for d in ds)} edges, "
          f"{len(doc['bridges'])} bridges, {cross} cross-board interactions -> {out}", file=sys.stderr)


if __name__ == "__main__":
    main()
