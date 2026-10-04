#!/usr/bin/env python3
"""Build data/aivillage/graph.json from the AI Village dataset (stdlib only, streaming).

Reads agents, villages, chat_rooms, village_goals, agent_goals and chat_messages.
Never reads agent_memories. Never emits message text. Humans are one aggregated node.
Run: nice -n 19 python3 aivillage/build.py [DATA_DIR] [OUT]
"""
import gzip, json, os, re, sys, time
from collections import Counter, defaultdict
from datetime import datetime, timezone

HERE = os.path.dirname(os.path.abspath(__file__))
DATA = os.environ.get("AIVILLAGE_DIR", os.path.expanduser("~/Data/ai-village"))
OUT = os.path.join(HERE, "..", "data", "aivillage", "graph.json")
WINDOW = 120   # seconds: adjacency = next distinct speaker in same room within this window
WEEK = 7 * 86400


def rows(data, name):
    with gzip.open(os.path.join(data, name + ".jsonl.gz"), "rt") as f:
        for line in f:
            yield json.loads(line)


def ts(s):
    if s is None:
        return None
    s = s.replace("T", " ").rstrip("Z")
    fmt = "%Y-%m-%d %H:%M:%S.%f" if "." in s else "%Y-%m-%d %H:%M:%S"
    return int(datetime.strptime(s[:26], fmt).replace(tzinfo=timezone.utc).timestamp())


def season(t):
    d = datetime.fromtimestamp(t, timezone.utc)
    return f"{d.year}-Q{(d.month - 1) // 3 + 1}"


def mention_matcher(agents):
    """@-mentions of agent names (full name, or name without a leading 'Claude ').
    Longest alias wins; the alias must not run on into more of a version number."""
    alias = {}
    for a in sorted(agents.values(), key=lambda a: a["created"]):  # later agents win shared short aliases
        n = a["name"]
        for v in {n, n[7:] if n.startswith("Claude ") else n}:
            alias[v.lower()] = a["id"]
    alts = "|".join(re.escape(k) for k in sorted(alias, key=len, reverse=True))
    rx = re.compile(r"@(" + alts + r")(?![\w-]|\.\d)", re.I)
    return lambda text: [alias[m.lower()] for m in rx.findall(text)]


def main(data=DATA, out=OUT):
    t_start = time.time()
    agents = {r["id"]: {"id": r["id"], "name": r["name"], "model": r["model_string"], "village": r["village_id"],
                        "created": ts(r["created_at"]), "participating": r["is_participating"]}
              for r in rows(data, "agents")}
    villages = [{"id": r["id"], "name": r["name"], "created": ts(r["created_at"])} for r in rows(data, "villages")]
    rooms = {r["id"]: {"name": r["name"], "created": ts(r["created_at"]), "deleted": ts(r.get("deleted_at"))}
             for r in rows(data, "chat_rooms")}
    vgoals = sorted(({"title": r["goal"].split("\n")[0][:120], "start": ts(r["start_time"]), "end": ts(r["end_time"])}
                     for r in rows(data, "village_goals")), key=lambda g: g["start"])
    agoals = [{"agent": r["agent_id"], "short": r.get("short_name") or r.get("name"), "start": ts(r.get("start_time")),
               "end": ts(r.get("end_time"))} for r in rows(data, "agent_goals")]
    match = mention_matcher(agents)

    # stream chat, keep only (t, speaker, room, mentions): no text retained
    msgs = []
    for r in rows(data, "chat_messages"):
        sp = r["agent_speaker_id"] if r["speaker_type"] == "agent" else "humans"
        if sp is None or (sp != "humans" and sp not in agents):
            continue
        msgs.append((ts(r["created_at"]), sp, r["room_id"], match(r["content"] or "")))
    msgs.sort(key=lambda m: m[0])
    t0, t1 = msgs[0][0], msgs[-1][0]

    starts = [g["start"] for g in vgoals]
    def goal_of(t):
        import bisect
        i = bisect.bisect_right(starts, t) - 1
        return i if i >= 0 and (vgoals[i]["end"] is None or t < vgoals[i]["end"]) else None

    node = defaultdict(lambda: {"msgs": 0, "first": None, "last": None, "rooms": Counter(), "goals": Counter()})
    mention, adjacent, member, goal_e = {}, {}, {}, {}

    def edge(store, a, b, t):
        e = store.get((a, b))
        if e is None:
            store[(a, b)] = e = [0, t, t, Counter()]
        e[0] += 1; e[2] = t; e[3][(t - t0) // WEEK] += 1

    last_in_room = {}
    for t, sp, room, ments in msgs:
        n = node[sp]
        n["msgs"] += 1; n["first"] = n["first"] or t; n["last"] = t; n["rooms"][room] += 1
        edge(member, sp, room, t)
        g = goal_of(t)
        if g is not None:
            n["goals"][g] += 1
            edge(goal_e, sp, g, t)
        for m in set(ments):
            if m != sp:
                edge(mention, sp, m, t)
        prev = last_in_room.get(room)
        if prev and prev[1] != sp and t - prev[0] <= WINDOW:
            edge(adjacent, sp, prev[1], t)
        last_in_room[room] = (t, sp)

    # node table: agents (sorted by first message, silent agents last), humans, rooms, villages, goals, agent goals
    akeys = sorted(agents, key=lambda k: (node[k]["first"] or 1 << 62, agents[k]["name"])) if True else []
    for k in akeys:
        node[k]  # materialise silent agents
    keys = akeys + ["humans"]
    room_ids = sorted(rooms, key=lambda r: rooms[r]["created"])
    N = len(keys)
    idx = {k: i for i, k in enumerate(keys)}
    ridx = {r: N + i for i, r in enumerate(room_ids)}
    vbase = N + len(room_ids)
    vidx = {v["id"]: vbase + i for i, v in enumerate(villages)}
    gbase = vbase + len(villages)
    abase = gbase + len(vgoals)

    kind, label, model, village, first, last, msgs_n, extra = [], [], [], [], [], [], [], []
    for k in keys:
        a, n = agents.get(k), node[k]
        kind.append(0 if a else 1)
        label.append(a["name"] if a else "humans (aggregated)")
        model.append(a["model"] if a else None)
        village.append(vidx.get(a["village"]) if a else None)
        first.append(n["first"]); last.append(n["last"]); msgs_n.append(n["msgs"])
        extra.append({"rooms": [ridx[r] for r, _ in n["rooms"].most_common()],
                      "participating": a["participating"] if a else None, "joined": a["created"] if a else None})
    rmsgs = Counter(m[2] for m in msgs)
    for r in room_ids:
        kind.append(2); label.append("#" + rooms[r]["name"]); model.append(None); village.append(vbase)
        first.append(rooms[r]["created"]); last.append(rooms[r]["deleted"]); msgs_n.append(rmsgs[r]); extra.append({})
    for v in villages:
        kind.append(3); label.append(v["name"]); model.append(None); village.append(None)
        first.append(v["created"]); last.append(None); msgs_n.append(len(msgs)); extra.append({})
    gm = Counter(goal_of(m[0]) for m in msgs)
    for i, g in enumerate(vgoals):
        kind.append(4); label.append(g["title"]); model.append(None); village.append(vbase)
        first.append(g["start"]); last.append(g["end"]); msgs_n.append(gm[i]); extra.append({"season": season(g["start"])})
    for g in agoals:
        kind.append(5); label.append(g["short"]); model.append(None); village.append(vbase)
        first.append(g["start"]); last.append(g["end"]); msgs_n.append(0); extra.append({})

    def pack(store, si, di):
        items = sorted(store.items(), key=lambda kv: kv[1][1])
        return {"src": [si(a) for (a, b), _ in items], "dst": [di(b) for (a, b), _ in items],
                "w": [v[0] for _, v in items], "first": [v[1] for _, v in items], "last": [v[2] for _, v in items],
                "weeks": [sorted(v[3].items()) for _, v in items]}  # [[week, count], ...] for replay

    vill = {(k, agents[k]["village"]): [node[k]["msgs"], node[k]["first"], node[k]["last"], Counter()]
            for k in akeys if node[k]["msgs"]}
    role = {(g["agent"], abase + i): [1, g["start"], g["end"] or t1, Counter()] for i, g in enumerate(agoals)}
    G = {
        "meta": {"source": "AI Digest, \"AI Village dataset\", 2026. https://theaidigest.org/village",
                 "generated": int(time.time()), "messages": len(msgs), "t0": t0, "t1": t1, "week": WEEK,
                 "adjacency_window_s": WINDOW,
                 "kinds": ["agent", "humans", "room", "village", "village_goal", "agent_goal"],
                 "edge_types": {"mention": "speaker @-mentions agent by name", 
                                "adjacent": f"speaker posts within {WINDOW}s after a different speaker in the same room (src responds to dst)",
                                "member": "agent -> room", "goal": "agent -> village goal active when it posted",
                                "village": "agent -> village", "role": "agent -> assigned individual goal"},
                 "identity": "agents by dataset name/model; all human chat participants pooled into one 'humans' node",
                 "privacy": "no message text; no human identities; research use only; not for training"},
        "rooms": [rooms[r]["name"] for r in room_ids],
        "goals": [{"title": g["title"], "start": g["start"], "end": g["end"], "season": season(g["start"])} for g in vgoals],
        "nodes": {"kind": kind, "label": label, "model": model, "village": village, "first": first, "last": last,
                  "posts": msgs_n, "rooms": [e.get("rooms", []) for e in extra],
                  "participating": [e.get("participating") for e in extra], "joined": [e.get("joined") for e in extra]},
        "edges": {
            "mention": pack(mention, idx.get, idx.get),
            "adjacent": pack(adjacent, idx.get, idx.get),
            "member": pack(member, idx.get, ridx.get),
            "goal": pack(goal_e, idx.get, lambda g: gbase + g),
            "village": pack(vill, idx.get, vidx.get),
            "role": pack(role, idx.get, lambda b: b),
        },
    }
    os.makedirs(os.path.dirname(os.path.abspath(out)), exist_ok=True)
    with open(out, "w") as f:
        json.dump(G, f, separators=(",", ":"))
    print(f"{len(msgs)} msgs -> {len(kind)} nodes; " + ", ".join(f"{k} {len(v['src'])}" for k, v in G["edges"].items())
          + f" in {time.time() - t_start:.1f}s", file=sys.stderr)
    return G


if __name__ == "__main__":
    main(*sys.argv[1:3])
