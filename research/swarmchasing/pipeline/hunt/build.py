#!/usr/bin/env python3
"""Build data/hunt/findings.json and data/hunt/graph.json from the hunt outputs (aggregates only).

graph.json follows collusion/build.py's compact parallel-array datasets format (schema 1).
Items (all kind 2, extra.type tells them apart): relay/echo hosts, wrapped-target hosts,
wikis, and time windows. Edges: active_in (host|wiki -> window), wraps (proxy -> target host
within the window), references (echo /base64/ payload -> host), co_burst (host -- host, same window).
Each candidate swarm is one cluster.
"""
import json, os
from collections import Counter
from datetime import datetime, timezone

HERE = os.path.dirname(os.path.abspath(__file__))
D = os.path.join(HERE, "..", "data", "hunt")
WEEK = 7 * 86400


def load(n):
    p = os.path.join(D, n)
    return json.load(open(p)) if os.path.exists(p) else {}


def t(day):  # 'YYYYMMDD' or 'YYYY-MM-DD'
    day = day.replace("-", "")
    return int(datetime.strptime(day, "%Y%m%d").replace(tzinfo=timezone.utc).timestamp())


def in_range(day, a, b):
    d = day.replace("-", "")
    return a <= d <= b


def main():
    cdx, shapes, wiki = load("cdx_series.json"), load("cdx_shapes.json"), load("wiki_sweep.json")
    urlscan, replay = load("urlscan_weekly.json"), load("replay_check.json")
    cands = json.load(open(os.path.join(HERE, "candidates.json")))

    items, idx, E = [], {}, {}

    def item(key, label, typ, extra=None):
        if key not in idx:
            idx[key] = len(items)
            items.append({"key": key, "label": label, "kind": 2, "posts": 0, "first": None, "last": None,
                          "weeks": Counter(), "cluster": -1, "extra": {"type": typ, **(extra or {})}})
        return items[idx[key]]

    def bump(it, day, n):
        ts = t(day)
        it["posts"] += n
        it["first"] = ts if it["first"] is None else min(it["first"], ts)
        it["last"] = ts if it["last"] is None else max(it["last"], ts)
        it["weeks"][ts // WEEK] += n

    def edge(typ, a, b, day, n, member):
        k = (typ, a, b)
        e = E.setdefault(k, {"w": 0, "first": t(day), "last": t(day), "weeks": Counter(), "member": member})
        e["w"] += n; e["first"] = min(e["first"], t(day)); e["last"] = max(e["last"], t(day)); e["weeks"][t(day) // WEEK] += n

    hosts = (cdx.get("hosts") or {})
    for ci, c in enumerate(cands):
        a, b = c["window"][0].replace("-", ""), c["window"][1].replace("-", "")
        wk = "window:" + c["id"]
        w = item(wk, f'{c["window"][0]}..{c["window"][1]}', "window", {"candidate": c["id"]})
        w["cluster"] = ci
        for h in c.get("hosts", []):
            hs = hosts.get(h, {})
            it = item("host:" + h + "@" + c["id"], h, "relay-host", {"category": hs.get("category")})
            it["cluster"] = ci if it["cluster"] == -1 else it["cluster"]
            for day, n in (hs.get("daily") or {}).items():
                if in_range(day, a, b):
                    bump(it, day, n); bump(w, day, n); edge("active_in", "host:" + h + "@" + c["id"], wk, day, n, True)
            sh = (shapes.get("wrappers") or {}).get(h, {})
            for day, tg in (sh.get("daily_targets") or {}).items():
                if in_range(day, a, b):
                    for th, n in tg.items():
                        if "." in th and not th.endswith((".ico", ".png", ".svg", ".txt", ".jpg")):
                            ti = item("target:" + th + "@" + c["id"], th, "wrapped-target"); ti["cluster"] = ci
                            bump(ti, day, n); edge("wraps", "host:" + h + "@" + c["id"], "target:" + th + "@" + c["id"], day, n, False)
            b6 = (shapes.get("base64") or {}).get(h, {})
            for day, rh in (b6.get("daily_ref_hosts") or {}).items():
                if in_range(day, a, b):
                    for th, n in rh.items():
                        ti = item("target:" + th + "@" + c["id"], th, "payload-ref"); ti["cluster"] = ci
                        bump(ti, day, n); edge("references", "host:" + h + "@" + c["id"], "target:" + th + "@" + c["id"], day, n, False)
        for wid in c.get("wikis", []):
            r = (wiki.get("wikis") or {}).get(wid, {})
            it = item("wiki:" + wid + "@" + c["id"], wid, "wiki", {"engine": r.get("engine"), "score_outside_known": r.get("score_outside_known")})
            it["cluster"] = ci if it["cluster"] == -1 else it["cluster"]
            for day, f in (r.get("days") or {}).items():
                if in_range(day, a, b):
                    bump(it, day, f["edits"]); bump(w, day, f["edits"]); edge("active_in", "wiki:" + wid + "@" + c["id"], wk, day, f["edits"], True)
        hl = c.get("hosts", [])
        for i in range(len(hl)):
            for j in range(i + 1, len(hl)):
                if all(("host:" + x + "@" + c["id"]) in idx and items[idx["host:" + x + "@" + c["id"]]]["posts"] for x in (hl[i], hl[j])):
                    edge("co_burst", "host:" + min(hl[i], hl[j]) + "@" + c["id"], "host:" + max(hl[i], hl[j]) + "@" + c["id"], c["window"][0], 1, False)

    keep = [i for i, it in enumerate(items) if it["posts"] > 0 or it["extra"]["type"] == "wiki"]
    remap = {old: new for new, old in enumerate(keep)}
    items = [items[i] for i in keep]
    keys = [it["key"] for it in items]
    pos = {k: i for i, k in enumerate(keys)}
    ed = {"src": [], "dst": [], "w": [], "first": [], "last": [], "member": [], "type": [], "weeks": []}
    for (typ, a, b), e in sorted(E.items(), key=lambda kv: (kv[1]["first"], kv[0])):
        if a in pos and b in pos:
            ed["src"].append(pos[a]); ed["dst"].append(pos[b]); ed["w"].append(e["w"]); ed["first"].append(e["first"])
            ed["last"].append(e["last"]); ed["member"].append(e["member"]); ed["type"].append(typ)
            ed["weeks"].append(sorted([w, n] for w, n in e["weeks"].items()))
    clusters = [{"id": ci, "candidate": c["id"], "title": c["title"], "verdict": c["verdict"], "confidence": c["confidence"],
                 "first": t(c["window"][0]), "last": t(c["window"][1]), "size": sum(1 for it in items if it["cluster"] == ci)}
                for ci, c in enumerate(cands)]
    ds = {
        "id": "swarmhunt", "title": "Swarm hunt, Feb-Oct 2026",
        "description": ("Candidate agent-swarm windows found in public exhaust: Wayback CDX captures of relay, reader and "
                        "echo hosts, the hosts they wrap or reference, and small-wiki RecentChanges bursts. One cluster per "
                        "candidate (including controls and resolved nulls). Counts only; no page text, no editor names."),
        "citation": "swarmgraph hunt, 3 Oct 2026", "url": "",
        "items": {"key": keys, "label": [it["label"] for it in items], "kind": [it["kind"] for it in items],
                  "posts": [it["posts"] for it in items], "first": [it["first"] or 0 for it in items],
                  "last": [it["last"] or 0 for it in items],
                  "weeks": [sorted([w, n] for w, n in it["weeks"].items()) for it in items],
                  "cluster": [it["cluster"] for it in items], "extra": [it["extra"] for it in items]},
        "edges": ed, "clusters": clusters,
        "meta": {"generated": int(datetime.now(timezone.utc).timestamp()), "week_s": WEEK,
                 "edge_types": dict(Counter(ed["type"])),
                 "privacy": "aggregate counts only; identifiers hashed in memory per run and discarded"},
    }
    with open(os.path.join(D, "graph.json"), "w") as f:
        json.dump({"schema": 1, "datasets": [ds], "bridges": []}, f, separators=(",", ":"))
    findings = {
        "generated": "2026-10-03", "candidates": cands,
        "sources": {
            "wayback_cdx": {"hosts": {h: {"captures": v["captures"], "weekly": v["weekly"]} for h, v in hosts.items()}},
            "urlscan_anonymous_30d": urlscan, "replay_check": replay,
            "wiki_sweep": {k: v for k, v in wiki.items() if k != "wikis"} | {"wikis": {
                k: {kk: vv for kk, vv in v.items() if kk != "days"} for k, v in (wiki.get("wikis") or {}).items()}},
        },
        "skipped": [
            {"source": "urlquery.net", "why": "search API needs an account key"},
            {"source": "urlscan.io beyond 30 days", "why": "anonymous search is limited to the last 30 days"},
            {"source": "Miraheze wikis", "why": "robots.txt disallows /w/ (API) for all agents"},
            {"source": "communitywiki.org, oddmuse.org, emacswiki.org", "why": "bot-check interstitial"},
            {"source": "WikiApiary", "why": "site down (database error)"},
        ],
    }
    with open(os.path.join(D, "findings.json"), "w") as f:
        json.dump(findings, f, indent=1)
    print(len(items), "items", len(ed["src"]), "edges", len(clusters), "clusters")


if __name__ == "__main__":
    main()
