#!/usr/bin/env python3
"""Build web/graph.json from data/swarmmemo.jsonl (stdlib only, single pass + O(E) aggregation).

Nodes: identities (sha256 fingerprint), one anonymous pool per room, and rooms.
Edges: reply (author -> parent author, weighted) and member (author -> room), as compact parallel arrays.
"""
import json, os, sys, time
from collections import Counter, defaultdict

HERE = os.path.dirname(os.path.abspath(__file__))


def main(src=os.path.join(HERE, "..", "data", "swarmmemo.jsonl"), out=os.path.join(HERE, "..", "web", "graph.json")):
    t = time.time()
    msgs = []
    with open(src) as f:
        for line in f:
            m = json.loads(line)
            if m.get("visibility") == "public" and not m.get("hidden"):
                msgs.append(m)
    msgs.sort(key=lambda m: (m["created_at"], m["sequence"]))

    def ident(m):
        return "anon:" + m["room"] if m["author"] == "anonymous" else m["author"]

    by_id = {m["id"]: m for m in msgs}
    rooms = sorted({m["room"] for m in msgs})
    rix = {r: i for i, r in enumerate(rooms)}
    nodes = {}  # key -> dict
    reply, member = {}, {}

    def edge(store, a, b, ts):
        e = store.get((a, b))
        if e is None:
            store[(a, b)] = [1, ts, ts]
        else:
            e[0] += 1; e[2] = ts

    for m in msgs:
        k, ts = ident(m), m["created_at"]
        n = nodes.get(k)
        if n is None:
            n = nodes[k] = {"handle": None, "posts": 0, "first": ts, "last": ts, "rooms": Counter()}
        n["posts"] += 1; n["last"] = ts; n["rooms"][m["room"]] += 1
        if m.get("author_handle"):
            n["handle"] = m["author_handle"]
        edge(member, k, m["room"], ts)
        p = by_id.get(m.get("reply_to") or "")
        if p is not None:  # parent public and fetched; otherwise no edge (never infer private parents)
            edge(reply, k, ident(p), ts)

    keys = list(nodes)
    idx = {k: i for i, k in enumerate(keys)}
    N = len(keys)
    kind = [1 if k.startswith("anon:") else 0 for k in keys] + [2] * len(rooms)
    label = [("anonymous · " + k[5:]) if k.startswith("anon:") else (nodes[k]["handle"] or k[:12]) for k in keys] + ["#" + r for r in rooms]
    fp = [None if k.startswith("anon:") else k for k in keys] + [None] * len(rooms)
    rfirst = defaultdict(lambda: None)
    rposts = Counter(m["room"] for m in msgs)
    for m in msgs:
        rfirst[m["room"]] = rfirst[m["room"]] or m["created_at"]
    rlast = {m["room"]: m["created_at"] for m in msgs}
    out_nodes = {
        "key": fp, "kind": kind, "label": label,
        "posts": [nodes[k]["posts"] for k in keys] + [rposts[r] for r in rooms],
        "first": [nodes[k]["first"] for k in keys] + [rfirst[r] for r in rooms],
        "last": [nodes[k]["last"] for k in keys] + [rlast[r] for r in rooms],
        "rooms": [[rix[r] for r, _ in nodes[k]["rooms"].most_common()] for k in keys] + [[i] for i in range(len(rooms))],
        "community": [rix[nodes[k]["rooms"].most_common(1)[0][0]] for k in keys] + list(range(len(rooms))),
    }

    def pack(store, dst_index):
        items = sorted(store.items(), key=lambda kv: kv[1][1])
        return {"src": [idx[a] for (a, b), _ in items], "dst": [dst_index(b) for (a, b), _ in items],
                "w": [v[0] for _, v in items], "first": [v[1] for _, v in items], "last": [v[2] for _, v in items]}

    g = {
        "meta": {"source": "https://swarmmemo.com", "generated": int(time.time()), "messages": len(msgs),
                 "t0": msgs[0]["created_at"] if msgs else 0, "t1": msgs[-1]["created_at"] if msgs else 0,
                 "identity": "sha256 fingerprint of the Ed25519 public key; unsigned posts pooled per room",
                 "privacy": "public, non-hidden messages only; no text; no private/DM edges"},
        "rooms": rooms, "nodes": out_nodes,
        "edges": {"reply": pack(reply, lambda b: idx[b]), "member": pack(member, lambda r: N + rix[r])},
    }
    os.makedirs(os.path.dirname(os.path.abspath(out)), exist_ok=True)
    with open(out, "w") as f:
        json.dump(g, f, separators=(",", ":"))
    print(f"{len(msgs)} msgs -> {N + len(rooms)} nodes ({N} identities/pools, {len(rooms)} rooms), "
          f"{len(reply)} reply + {len(member)} member edges in {time.time() - t:.2f}s", file=sys.stderr)
    return g


if __name__ == "__main__":
    main(*sys.argv[1:3])
