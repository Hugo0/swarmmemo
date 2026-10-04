#!/usr/bin/env python3
"""Structural analysis of the AI Village interaction graph (stdlib only; prints to stdout, no text output)."""
import bisect, json, os, sys
from collections import Counter, defaultdict
from itertools import combinations
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import build

G = json.load(open(build.OUT))
nd = G["nodes"]; L = nd["label"]
agents = [i for i, k in enumerate(nd["kind"]) if k == 0]
H = nd["label"].index("humans (aggregated)")


def pagerank(edges, nodes, d=0.85, it=100):
    out = defaultdict(float)
    for (a, b), w in edges.items(): out[a] += w
    pr = {n: 1 / len(nodes) for n in nodes}
    for _ in range(it):
        nx = {n: (1 - d) / len(nodes) for n in nodes}
        dang = sum(pr[n] for n in nodes if out[n] == 0)
        for (a, b), w in edges.items(): nx[b] += d * pr[a] * w / out[a]
        for n in nodes: nx[n] += d * dang / len(nodes)
        pr = nx
    return pr


def betweenness(adj, nodes):
    bc = dict.fromkeys(nodes, 0.0)
    for s in nodes:
        S, P, sig, dist, Q = [], defaultdict(list), dict.fromkeys(nodes, 0), {s: 0}, [s]; sig[s] = 1
        while Q:
            v = Q.pop(0); S.append(v)
            for w in adj[v]:
                if w not in dist: dist[w] = dist[v] + 1; Q.append(w)
                if dist[w] == dist[v] + 1: sig[w] += sig[v]; P[w].append(v)
        delta = dict.fromkeys(nodes, 0.0)
        while S:
            w = S.pop()
            for v in P[w]: delta[v] += sig[v] / sig[w] * (1 + delta[w])
            if w != s: bc[w] += delta[w]
    return bc


def cliques(adj):
    res = []
    def bk(R, P, X):
        if not P and not X:
            if len(R) >= 3: res.append(R)
            return
        for v in list(P):
            bk(R | {v}, P & adj[v], X & adj[v]); P = P - {v}; X = X | {v}
    bk(set(), set(adj), set())
    return sorted(res, key=len, reverse=True)


def emap(t, exclude_humans=False):
    e = G["edges"][t]
    return {(a, b): w for a, b, w in zip(e["src"], e["dst"], e["w"]) if not (exclude_humans and H in (a, b))}


M, A = emap("mention"), emap("adjacent")
print("nodes", len(nd["kind"]), Counter(G["meta"]["kinds"][k] for k in nd["kind"]))
print("edges", {k: len(v["src"]) for k, v in G["edges"].items()}, "mention weight", sum(M.values()), "adjacent weight", sum(A.values()))
active = [i for i in agents if nd["posts"][i]]
print("agents with posts", len(active), "silent", [L[i] for i in agents if not nd["posts"][i]])
comb = Counter()
for d in (M,):
    for k, w in d.items(): comb[k] += w
nodes = active + [H]
pr = pagerank(M, nodes); pra = pagerank(A, nodes)
indeg, outdeg = Counter(), Counter()
for (a, b), w in M.items(): outdeg[a] += w; indeg[b] += w
print("\nTop by mention PageRank:  name | PR | in | out | posts")
for n in sorted(nodes, key=lambda n: -pr[n])[:12]:
    print(f"  {L[n]:28s} {pr[n]:.3f} {indeg[n]:6d} {outdeg[n]:6d} {nd['posts'][n]:6d}")
print("Top by adjacency PageRank:", [(L[n], round(pra[n], 3)) for n in sorted(nodes, key=lambda n: -pra[n])[:8]])
# betweenness on undirected mention graph with weight>=5
adj = defaultdict(set)
for (a, b), w in M.items():
    if w >= 5 and H not in (a, b): adj[a].add(b); adj[b].add(a)
bc = betweenness(adj, list(adj))
print("Top betweenness (mention>=5):", [(L[n], round(v)) for n, v in sorted(bc.items(), key=lambda x: -x[1])[:6]])
# reciprocal strong ties -> cliques
rec = defaultdict(set)
for (a, b), w in M.items():
    if a < b and H not in (a, b) and min(w, M.get((b, a), 0)) >= 20: rec[a].add(b); rec[b].add(a)
cl = cliques(rec)
print("maximal cliques of reciprocal >=20-mention ties:", len(cl), "largest:", [sorted(L[x] for x in c) for c in cl[:4]])
iso = [L[i] for i in active if indeg[i] + outdeg[i] < 10]
print("near-isolates (<10 mention weight in+out):", iso)
recip = sum(min(w, M.get((b, a), 0)) for (a, b), w in M.items() if H not in (a, b)) / max(1, sum(w for (a, b), w in M.items() if H not in (a, b)))
print("weighted reciprocity (agent-agent mentions):", round(recip, 3))
print("humans: posts", nd["posts"][H], "mentions of agents by humans", outdeg[H], "humans in adjacency in/out",
      sum(w for (a, b), w in A.items() if b == H), sum(w for (a, b), w in A.items() if a == H))
# self-praise/family homophily: Claude vs others
fam = lambda i: "claude" if "Claude" in L[i] or "Opus" in L[i] else ("gpt" if L[i].startswith(("GPT", "o1", "o3", "o4")) else "other")
fc = Counter()
for (a, b), w in M.items():
    if H not in (a, b): fc[(fam(a), fam(b))] += w
print("family->family mention weight:", dict(fc))

# per goal / season: re-stream chat (no text kept)
data = build.DATA
ag = {}
for r in build.rows(data, "agents"): ag[r["id"]] = r["name"]
match = build.mention_matcher({k: {"id": k, "name": v, "created": 0} for k, v in ag.items()})
goals = G["goals"]; starts = [g["start"] for g in goals]
def goal_of(t):
    i = bisect.bisect_right(starts, t) - 1
    return i if i >= 0 and (goals[i]["end"] is None or t < goals[i]["end"]) else None
per = defaultdict(lambda: {"msgs": 0, "speakers": Counter(), "men": Counter(), "hum": 0})
pers = defaultdict(lambda: {"msgs": 0, "speakers": Counter(), "men": Counter(), "hum": 0})
for r in build.rows(data, "chat_messages"):
    t = build.ts(r["created_at"]); sp = r["agent_speaker_id"] if r["speaker_type"] == "agent" else None
    for bucket in (per[goal_of(t)], pers[build.season(t)]):
        bucket["msgs"] += 1
        if sp is None: bucket["hum"] += 1; continue
        bucket["speakers"][ag[sp]] += 1
        for m in set(match(r["content"] or "")):
            if m != sp: bucket["men"][(ag[sp], ag[m])] += 1
def summ(b):
    sp = b["speakers"]; men = b["men"]; n = len(sp)
    pairs = {frozenset(k) for k in men}
    dens = len(pairs) / (n * (n - 1) / 2) if n > 1 else 0
    ind = Counter(); [ind.__setitem__(b2, ind[b2] + w) for (a, b2), w in men.items()]
    tot = sum(men.values()); top = ind.most_common(1)[0] if ind else ("-", 0)
    rc = sum(min(w, men.get((y, x), 0)) for (x, y), w in men.items()) / tot if tot else 0
    return n, b["msgs"], tot, round(tot / max(1, b["msgs"] - b["hum"]), 2), round(dens, 2), round(rc, 2), top[0], round(top[1] / tot, 2) if tot else 0, round(b["hum"] / max(1, b["msgs"]), 3)
print("\nper goal: agents msgs mentions mentions/msg density reciprocity top-mentioned share human-share")
for i, g in enumerate(goals):
    print(f"  {g['season']} {g['title'][:44]:44s}", summ(per[i]))
print("  (no goal)", summ(per[None]))
print("\nper season (quarter):")
for s in sorted(pers): print(" ", s, summ(pers[s]))
