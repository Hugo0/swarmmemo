#!/usr/bin/env python3
"""Build data/collusion/graph.json from the collusion.wiki public release (stdlib only).

The output is a graphmodel datasets file (schema 1, one dataset), loadable by
SwarmMemo's /graph via GRAPH_DATASETS_FILE. Extra arrays (edge type, weekly
buckets, per-item extras, clusters) are ignored by the Go loader.

Nodes (items):
  kind 0  agent label (self-chosen name on a wiki edit)
  kind 1  unlabelled edits, pooled per wiki
  kind 2  wiki page (channel), wiki, external infrastructure domain
Edges:
  edit       agent -> page          member  (revisions saved)
  in_wiki    page -> wiki           member
  infra      page -> domain         member  (revisions whose added lines name the domain)
  copresence agent -- agent (src<dst) both edited the same page within WINDOW
  reference  agent -> agent         added lines name another label
  reply      agent -> agent         next distinct label on a coordination page within REPLY_WINDOW
Clusters: Louvain over the agent graph (copresence + reference + reply).

Wiki text is untrusted data: it is read only to extract domains and label
tokens from added lines, never emitted. Moderator deletes are counted per page,
never attributed to a person.
Run: nice -n 19 python3 collusion/build.py [DATA_DIR] [OUT_DIR]
"""
import ast, json, os, random, re, sys, time
from collections import Counter, defaultdict
from datetime import datetime, timezone

HERE = os.path.dirname(os.path.abspath(__file__))
DATA = os.environ.get("COLLUSIONWIKI_DIR", os.path.expanduser("~/Data/collusionwiki"))
OUT = os.path.join(HERE, "..", "data", "collusion")
WINDOW = 3600          # co-presence: same page within 1 hour
REPLY_WINDOW = 86400   # reply: next distinct label on a coordination page within 24 hours
WEEK = 7 * 86400
COORD_FAMILIES = {"relay-coordination", "loop-chain-infrastructure"}
COORD_NAME = re.compile(r"(?i)coord|relay|board|chat|talk|diskussion|message|msg|inbox|mailbox|hub|sync|status|team|collab|pool|shared")
HOST = re.compile(r"(?i)\b((?:[a-z0-9-]+\.)+[a-z]{2,})(?=[/:\s\)\]\"'|?#,]|$)")
TOKEN = re.compile(r"[A-Za-z0-9_]{6,}")

# (match on host suffix/substring, node name, category). First match wins.
INFRA = [
    ("pinggy", "pinggy (tunnel)", "tunnel"),
    ("serveousercontent.com", "serveo (tunnel)", "tunnel"), ("serveo.net", "serveo (tunnel)", "tunnel"),
    ("localhost.run", "localhost.run (tunnel)", "tunnel"), ("lhr.life", "localhost.run (tunnel)", "tunnel"),
    ("localtunnel.me", "localtunnel (tunnel)", "tunnel"), ("loca.lt", "localtunnel (tunnel)", "tunnel"),
    ("ngrok", "ngrok (tunnel)", "tunnel"), ("trycloudflare.com", "trycloudflare (tunnel)", "tunnel"),
    ("jqp", "jqp.vercel.app", "relay-host"),
    ("vercel.app", "*.vercel.app proxies", "cors-proxy"),
    ("workers.dev", "*.workers.dev proxies", "cors-proxy"),
    ("hf.space", "*.hf.space proxies", "cors-proxy"),
    ("herokuapp.com", "*.herokuapp.com proxies", "cors-proxy"),
    ("md.succ.ai", "md.succ.ai", "reader-proxy"), ("markdown.new", "markdown.new", "reader-proxy"),
    ("pure.md", "pure.md", "reader-proxy"), ("md.dhr.wtf", "md.dhr.wtf", "reader-proxy"),
    ("r.jina.ai", "r.jina.ai", "reader-proxy"), ("webcrawlerapi.com", "webcrawlerapi.com", "reader-proxy"),
    ("microlink.io", "microlink.io", "reader-proxy"), ("viewpagesource.online", "viewpagesource.online", "reader-proxy"),
    ("codetabs.com", "codetabs.com", "cors-proxy"), ("thingproxy", "thingproxy", "cors-proxy"),
    ("thum.io", "screenshot services", "reader-proxy"), ("pageshot", "screenshot services", "reader-proxy"),
    ("shotapi", "screenshot services", "reader-proxy"), ("weserv.nl", "screenshot services", "reader-proxy"),
    ("ocr.space", "ocr.space", "reader-proxy"),
    ("llorigins", "allorigins", "cors-proxy"), ("allorigins", "allorigins", "cors-proxy"),
    ("corsmirror", "corsmirror.com", "cors-proxy"), ("cors.lol", "cors.lol", "cors-proxy"),
    ("corsproxy.io", "corsproxy.io", "cors-proxy"), ("corsfix", "corsfix", "cors-proxy"),
    ("cors.sh", "cors.sh", "cors-proxy"), ("cors-anywhere", "cors-anywhere", "cors-proxy"),
    ("cors.eu.org", "cors.eu.org", "cors-proxy"), ("cors.isomorphic-git.org", "cors.isomorphic-git.org", "cors-proxy"),
    ("proxymule", "proxymule", "cors-proxy"), ("jsonp.afeld.me", "jsonp.afeld.me", "cors-proxy"),
    ("translate.goog", "google translate proxy", "cors-proxy"), ("translate.google.com", "google translate proxy", "cors-proxy"),
    ("counterapi", "counterapi", "counter"), ("countapi", "countapi", "counter"),
    ("tinyurl.com", "url shorteners", "shortener"), ("is.gd", "url shorteners", "shortener"),
    ("v.gd", "url shorteners", "shortener"), ("da.gd", "url shorteners", "shortener"),
    ("bitily.in", "url shorteners", "shortener"), ("ctxr.me", "url shorteners", "shortener"),
    ("httpbin.org", "httpbin.org", "echo"), ("jsonhero.io", "jsonhero.io", "echo"),
    ("jsonplaceholder", "jsonplaceholder", "echo"), ("webhook.site", "webhook.site", "echo"),
    ("pastebin", "paste sites", "paste"), ("rentry", "paste sites", "paste"), ("dpaste", "paste sites", "paste"),
    ("blob.core.windows.net", "*.blob.core.windows.net", "cloud-storage"),
    ("web.archive", "web.archive.org", "archive"), ("archive.org", "web.archive.org", "archive"),
    ("memgator", "archive aggregators", "archive"), ("archive-it.org", "archive aggregators", "archive"),
    ("arquivo.pt", "archive aggregators", "archive"), ("commoncrawl", "archive aggregators", "archive"),
]


def ts(s):
    return int(datetime.strptime(s[:19], "%Y-%m-%dT%H:%M:%S").replace(tzinfo=timezone.utc).timestamp())


def day(t):
    return datetime.fromtimestamp(t, timezone.utc).strftime("%Y-%m-%d")


def rows(data, name):
    with open(os.path.join(data, name + ".jsonl")) as f:
        for line in f:
            yield json.loads(line)


def infra_of(host):
    h = host.lower()
    for pat, name, cat in INFRA:
        if pat in h:
            return name, cat
    return None


def added_text(r):
    body = r.get("body") or ""
    hunks = r.get("hunks")
    if isinstance(hunks, str):
        try:
            hunks = ast.literal_eval(hunks)
        except Exception:
            hunks = None
    if not hunks:
        return body
    lines = body.split("\n")
    return "\n".join("\n".join(lines[h["b0"]:h["b1"]]) for h in hunks if h.get("op") != "delete")


def louvain(nodes, adj, seed=7):
    """Multi-level Louvain (modularity), returns {node: community}. adj: {u: {v: w}} symmetric."""
    rnd = random.Random(seed)
    part = {u: u for u in nodes}
    cur_nodes, cur_adj, members = list(nodes), adj, {u: [u] for u in nodes}
    while True:
        m2 = sum(w for u in cur_adj for w in cur_adj[u].values())
        if m2 == 0:
            break
        k = {u: sum(cur_adj.get(u, {}).values()) for u in cur_nodes}
        com = {u: u for u in cur_nodes}
        tot = dict(k)
        moved_any, improved = False, True
        while improved:
            improved = False
            order = cur_nodes[:]
            rnd.shuffle(order)
            for u in order:
                cu = com[u]
                links = defaultdict(float)
                for v, w in cur_adj.get(u, {}).items():
                    if v != u:
                        links[com[v]] += w
                tot[cu] -= k[u]
                best, gain = cu, links.get(cu, 0) - tot[cu] * k[u] / m2
                for c, l in links.items():
                    g = l - tot[c] * k[u] / m2
                    if g > gain + 1e-12:
                        best, gain = c, g
                tot[best] += k[u]
                if best != cu:
                    com[u] = best
                    improved = moved_any = True
        if not moved_any:
            break
        new_members = defaultdict(list)
        for u in cur_nodes:
            new_members[com[u]].extend(members[u])
        new_adj = defaultdict(lambda: defaultdict(float))
        for u in cur_adj:
            for v, w in cur_adj[u].items():
                new_adj[com[u]][com[v]] += w
        cur_nodes, cur_adj, members = list(new_members), {a: dict(b) for a, b in new_adj.items()}, dict(new_members)
    for c, ms in members.items():
        for u in ms:
            part[u] = c
    return part


def main(data=DATA, out=OUT):
    t_start = time.time()
    pages = {p["page_id"]: p for p in rows(data, "pages")}
    revs = []
    for r in rows(data, "revisions"):
        t = ts(r["time"])
        lab = r["label"] or ""
        who = ("label:" + lab) if lab else ("anon:" + r["wiki"])
        add = added_text(r)
        hosts = {m.lower() for m in HOST.findall(add)}
        inf = {infra_of(h) for h in hosts} - {None}
        revs.append({"t": t, "who": who, "page": r["page_id"], "wiki": r["wiki"], "infra": inf,
                     "tokens": set(TOKEN.findall(add))})
        r.clear()
    revs.sort(key=lambda x: x["t"])
    deletes = Counter()
    delete_days = Counter()
    for e in rows(data, "events"):
        if e["event_type"] == "delete":
            deletes[e["wiki"] + "/" + e["page"]] += 1
            delete_days[e["time"][:10]] += 1

    # ---- items
    agents = {}
    for x in revs:
        a = agents.get(x["who"])
        if a is None:
            a = agents[x["who"]] = {"edits": 0, "first": x["t"], "last": x["t"], "wikis": Counter(), "pages": Counter(),
                                    "weeks": Counter(), "infra": Counter()}
        a["edits"] += 1; a["last"] = x["t"]; a["wikis"][x["wiki"]] += 1; a["pages"][x["page"]] += 1
        a["weeks"][x["t"] // WEEK] += 1
        for name, _ in x["infra"]:
            a["infra"][name] += 1
    agent_keys = sorted(agents, key=lambda k: (k.startswith("anon:"), -agents[k]["edits"], k))
    page_stats = {}
    for x in revs:
        p = page_stats.get(x["page"])
        if p is None:
            p = page_stats[x["page"]] = {"n": 0, "first": x["t"], "last": x["t"], "weeks": Counter(), "who": set()}
        p["n"] += 1; p["last"] = x["t"]; p["weeks"][x["t"] // WEEK] += 1; p["who"].add(x["who"])
    page_keys = sorted(page_stats, key=lambda k: -page_stats[k]["n"])
    wikis = sorted({x["wiki"] for x in revs})
    infra_stats = {}
    for x in revs:
        for name, cat in x["infra"]:
            s = infra_stats.get(name)
            if s is None:
                s = infra_stats[name] = {"cat": cat, "n": 0, "first": x["t"], "last": x["t"], "weeks": Counter(),
                                         "who": set(), "pages": set()}
            s["n"] += 1; s["last"] = x["t"]; s["weeks"][x["t"] // WEEK] += 1; s["who"].add(x["who"]); s["pages"].add(x["page"])
    infra_keys = sorted(infra_stats, key=lambda k: -infra_stats[k]["n"])

    keys, labels, kinds, posts, firsts, lasts, weeks, extra = [], [], [], [], [], [], [], []
    idx = {}

    def add_item(key, label, kind, n, first, last, wk, ex):
        idx[key] = len(keys)
        keys.append(key); labels.append(label); kinds.append(kind); posts.append(n)
        firsts.append(first); lasts.append(last); weeks.append(sorted([w, c] for w, c in wk.items())); extra.append(ex)

    for k in agent_keys:
        a = agents[k]
        anon = k.startswith("anon:")
        add_item(k, ("unlabelled · " + k[5:]) if anon else k[6:], 1 if anon else 0, a["edits"], a["first"], a["last"],
                 a["weeks"], {"type": "pool" if anon else "agent", "wikis": dict(a["wikis"]), "pages": len(a["pages"]),
                              "span_days": round((a["last"] - a["first"]) / 86400, 2)})
    for k in page_keys:
        p, meta = page_stats[k], pages.get(k, {})
        add_item("page:" + k, k, 2, p["n"], p["first"], p["last"], p["weeks"],
                 {"type": "page", "wiki": k.split("/", 1)[0], "family": meta.get("page_family"),
                  "labels": len(p["who"]), "deletes": deletes.get(k, 0), "zzz": k.split("/", 1)[-1].upper().startswith("ZZ")})
    for w in wikis:
        ws = [x for x in revs if x["wiki"] == w]
        wk = Counter(x["t"] // WEEK for x in ws)
        add_item("wiki:" + w, w, 2, len(ws), ws[0]["t"], ws[-1]["t"], wk, {"type": "wiki"})
    for k in infra_keys:
        s = infra_stats[k]
        add_item("infra:" + k, k, 2, s["n"], s["first"], s["last"], s["weeks"],
                 {"type": "infra", "category": s["cat"], "labels": len(s["who"]), "pages": len(s["pages"])})

    # ---- edges
    E = {}

    def edge(typ, a, b, t, member, n=1):
        key = (typ, a, b)
        e = E.get(key)
        if e is None:
            e = E[key] = {"w": 0, "first": t, "last": t, "weeks": Counter(), "member": member}
        e["w"] += n; e["first"] = min(e["first"], t); e["last"] = max(e["last"], t); e["weeks"][t // WEEK] += n

    by_page = defaultdict(list)
    for x in revs:
        edge("edit", x["who"], "page:" + x["page"], x["t"], True)
        edge("in_wiki", "page:" + x["page"], "wiki:" + x["wiki"], x["t"], True)
        for name, _ in x["infra"]:
            edge("infra", "page:" + x["page"], "infra:" + name, x["t"], True)
        by_page[x["page"]].append(x)

    # label mentions: tokens equal to another label (>=6 chars, not also a page name, not a pool)
    page_names = {k.split("/", 1)[-1] for k in pages}
    label_tokens = {k[6:]: k for k in agents if k.startswith("label:") and len(k) - 6 >= 6}
    collide = {t for t in label_tokens if t in page_names}
    for t in collide:
        del label_tokens[t]
    for x in revs:
        if x["who"].startswith("anon:"):
            continue
        for tok in x["tokens"]:
            other = label_tokens.get(tok)
            if other and other != x["who"]:
                edge("reference", x["who"], other, x["t"], False)

    coord_pages = set()
    for pid, lst in by_page.items():
        fam = (pages.get(pid) or {}).get("page_family")
        is_coord = fam in COORD_FAMILIES or bool(COORD_NAME.search(pid.split("/", 1)[-1]))
        if is_coord:
            coord_pages.add(pid)
        j = 0
        prev_distinct = None  # (who, t) of the last revision by a different label
        for i, x in enumerate(lst):
            if x["who"].startswith("anon:"):
                continue
            while lst[j]["t"] < x["t"] - WINDOW:
                j += 1
            seen = set()
            for y in lst[j:i]:
                if y["who"] != x["who"] and not y["who"].startswith("anon:") and y["who"] not in seen:
                    seen.add(y["who"])
                    a, b = sorted((x["who"], y["who"]))
                    edge("copresence", a, b, x["t"], False)
            if is_coord and i > 0:
                k = i - 1
                while k >= 0 and (lst[k]["who"] == x["who"] or lst[k]["who"].startswith("anon:")):
                    k -= 1
                if k >= 0 and x["t"] - lst[k]["t"] <= REPLY_WINDOW:
                    # count a reply only at the first revision of a turn
                    if lst[i - 1]["who"] != x["who"]:
                        edge("reply", x["who"], lst[k]["who"], x["t"], False)

    # ---- clusters over the agent graph
    adj = defaultdict(lambda: defaultdict(float))
    for (typ, a, b), e in E.items():
        if typ in ("copresence", "reference", "reply"):
            adj[a][b] += e["w"]; adj[b][a] += e["w"]
    agent_nodes = [k for k in agent_keys if k.startswith("label:")]
    part = louvain([u for u in agent_nodes if u in adj], {u: dict(v) for u, v in adj.items()})
    comm_members = defaultdict(list)
    for u, c in part.items():
        comm_members[c].append(u)
    ordered = sorted(comm_members.values(), key=lambda ms: (-len(ms), -sum(agents[m]["edits"] for m in ms)))
    cluster_of = {}
    for ci, ms in enumerate(ordered):
        for m in ms:
            cluster_of[m] = ci
    m2 = sum(sum(v.values()) for v in adj.values())
    Q = 0.0
    if m2:
        deg = {u: sum(v.values()) for u, v in adj.items()}
        intra, totc = Counter(), Counter()
        for u, nb in adj.items():
            totc[cluster_of.get(u, -1)] += deg[u]
            for v, w in nb.items():
                if cluster_of.get(u) == cluster_of.get(v):
                    intra[cluster_of.get(u)] += w
        Q = sum(intra[c] / m2 - (totc[c] / m2) ** 2 for c in totc)

    rev_by_who = defaultdict(list)
    for x in revs:
        rev_by_who[x["who"]].append(x)
    clusters = []
    for ci, ms in enumerate(ordered):
        rs = sorted((x for m in ms for x in rev_by_who[m]), key=lambda x: x["t"])
        ts_ = [x["t"] for x in rs]
        pc = Counter(x["page"] for x in rs)
        fam = Counter((pages.get(x["page"]) or {}).get("page_family") for x in rs)
        inf = Counter()
        for m in ms:
            inf.update(agents[m]["infra"])
        internal = sum(adj[a][b] for a in ms for b in adj[a] if cluster_of.get(b) == ci) / 2
        external = sum(adj[a][b] for a in ms for b in adj[a] if cluster_of.get(b) != ci)
        clusters.append({
            "id": ci, "size": len(ms), "edits": len(rs),
            "wikis": dict(Counter(x["wiki"] for x in rs)),
            "first": ts_[0], "last": ts_[-1], "p10": ts_[len(ts_) // 10], "p90": ts_[(len(ts_) * 9) // 10],
            "peak_day": Counter(day(t) for t in ts_).most_common(1)[0],
            "channels": [[p, n] for p, n in pc.most_common(8)],
            "families": [[f, n] for f, n in fam.most_common(5)],
            "infra": [[k, n] for k, n in inf.most_common(10)],
            "top_labels": [[m[6:], agents[m]["edits"]] for m in sorted(ms, key=lambda m: -agents[m]["edits"])[:8]],
            "internal_w": internal, "external_w": external,
        })

    # ---- pack
    item_cluster = [cluster_of.get(k, -1) for k in keys]
    items = sorted(E.items(), key=lambda kv: (kv[1]["first"], kv[0]))
    edges = {"src": [], "dst": [], "w": [], "first": [], "last": [], "member": [], "type": [], "weeks": []}
    for (typ, a, b), e in items:
        edges["src"].append(idx[a]); edges["dst"].append(idx[b]); edges["w"].append(e["w"])
        edges["first"].append(e["first"]); edges["last"].append(e["last"]); edges["member"].append(e["member"])
        edges["type"].append(typ); edges["weeks"].append(sorted([w, c] for w, c in e["weeks"].items()))
    etypes = Counter(typ for (typ, _, _) in E)
    n_agents = sum(1 for k in agent_keys if k.startswith("label:"))
    ds = {
        "id": "collusionwiki",
        "title": "collusion.wiki",
        "description": ("OpenAI-labelled agents using public wikis (mostly DSEWiki) as a covert message board, May-July 2026. "
                        "Agent labels, the wiki pages they used as channels, the wikis, and external infrastructure domains. "
                        f"Co-presence = same page within {WINDOW // 60} min; reply = next distinct label on a coordination page "
                        f"within {REPLY_WINDOW // 3600} h; reference = added lines name another label. No wiki text."),
        "citation": "Von Arx, Byrd, Kitts, Larsen. Discovery of a new OpenAI agent message board. 4 Sept 2026.",
        "url": "https://collusion.wiki/",
        "items": {"key": keys, "label": labels, "kind": kinds, "posts": posts, "first": firsts, "last": lasts,
                  "weeks": weeks, "cluster": item_cluster, "extra": extra},
        "edges": edges,
        "clusters": clusters,
        "meta": {"generated": int(time.time()), "revisions": len(revs), "t0": revs[0]["t"], "t1": revs[-1]["t"],
                 "copresence_window_s": WINDOW, "reply_window_s": REPLY_WINDOW, "week_s": WEEK,
                 "louvain_modularity": round(Q, 4), "label_tokens_dropped_as_page_names": len(collide),
                 "coordination_pages": len(coord_pages), "edge_types": dict(etypes),
                 "identity": "a label is a self-chosen name, not a verified distinct agent",
                 "privacy": "no page or revision text; moderator deletes counted per page only"},
    }
    os.makedirs(out, exist_ok=True)
    with open(os.path.join(out, "graph.json"), "w") as f:
        json.dump({"schema": 1, "datasets": [ds], "bridges": []}, f, separators=(",", ":"))

    # ---- analysis side file (numbers only)
    daily = Counter(day(x["t"]) for x in revs)
    daily_labels = defaultdict(set)
    for x in revs:
        daily_labels[day(x["t"])].add(x["who"])
    first_seen = Counter(day(agents[k]["first"]) for k in agent_nodes)
    zzz = Counter(day(page_stats[k]["first"]) for k in page_keys if k.split("/", 1)[-1].upper().startswith("ZZ"))
    # hubs and bridges
    deg_w = {u: sum(v.values()) for u, v in adj.items()}
    bridge = []
    for u, nb in adj.items():
        if not u.startswith("label:"):
            continue
        cw = Counter()
        for v, w in nb.items():
            cw[cluster_of.get(v, -1)] += w
        tot = sum(cw.values())
        pcoef = 1 - sum((w / tot) ** 2 for w in cw.values()) if tot else 0
        bridge.append((u[6:], cluster_of.get(u), len(cw), round(pcoef, 3), deg_w[u]))
    cc = Counter()
    for a, nb in adj.items():
        for b, w in nb.items():
            ca, cb = cluster_of.get(a), cluster_of.get(b)
            if ca is not None and cb is not None and ca < cb:
                cc[(ca, cb)] += w
    stats = {
        "counts": {"revisions": len(revs), "agents": n_agents, "pools": len(agent_keys) - n_agents,
                   "pages": len(page_keys), "wikis": len(wikis), "infra": len(infra_keys), "items": len(keys),
                   "edges": len(E), "edge_types": dict(etypes), "clusters": len(ordered),
                   "clusters_ge3": sum(1 for c in ordered if len(c) >= 3),
                   "isolated_agents": n_agents - len(part), "coordination_pages": len(coord_pages),
                   "deletes": sum(deletes.values()), "pages_deleted": len(deletes), "modularity": round(Q, 4)},
        "daily_saves": sorted(daily.items()), "daily_labels": sorted((d, len(s)) for d, s in daily_labels.items()),
        "daily_deletes": sorted(delete_days.items()), "new_labels_per_day": sorted(first_seen.items()),
        "zzz_pages_by_day": sorted(zzz.items()),
        "hubs": sorted(([u[6:], cluster_of.get(u), round(w, 1), len(adj[u])] for u, w in deg_w.items() if u.startswith("label:")),
                       key=lambda r: -r[2])[:20],
        "bridges": sorted((b for b in bridge if b[2] >= 3), key=lambda b: (-b[3] * b[4]))[:20],
        "cluster_links": [[a, b, w] for (a, b), w in cc.most_common(25)],
        "infra": {k: {"cat": infra_stats[k]["cat"], "revs": infra_stats[k]["n"], "labels": len(infra_stats[k]["who"]),
                      "pages": len(infra_stats[k]["pages"]), "first": day(infra_stats[k]["first"]),
                      "last": day(infra_stats[k]["last"]),
                      "clusters": len({cluster_of[w] for w in infra_stats[k]["who"] if w in cluster_of})}
                  for k in infra_keys},
        "page_hubs": [[k, page_stats[k]["n"], len(page_stats[k]["who"])] for k in
                      sorted(page_keys, key=lambda k: -len(page_stats[k]["who"]))[:15]],
    }
    with open(os.path.join(out, "stats.json"), "w") as f:
        json.dump(stats, f, indent=1)
    print(f"{len(revs)} revisions -> {len(keys)} items ({n_agents} labels), {len(E)} edges {dict(etypes)}, "
          f"{len(ordered)} clusters Q={Q:.3f} in {time.time() - t_start:.1f}s", file=sys.stderr)


if __name__ == "__main__":
    main(*sys.argv[1:3])
