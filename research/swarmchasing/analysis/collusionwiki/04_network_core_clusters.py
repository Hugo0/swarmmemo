#!/usr/bin/env python3
"""Label-graph structure from pipeline/collusion/build.py output: k-core, rich-club coefficient against degree-preserving rewiring, bridging labels, per-cluster persistence and task specialisation.

Findings: F10, F11 (findings/swarm-findings.md). Standard library only; prints to stdout.
Inputs: see _paths.py. No wiki text is printed or written.
"""
import os, sys
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _paths import wiki, cache, PIPELINE_DATA, RESULTS  # noqa: E402
import json,pickle,random,math,statistics as st
from collections import Counter,defaultdict
from datetime import datetime,timezone
g=json.load(open(os.path.join(PIPELINE_DATA, 'collusion', 'graph.json')))['datasets'][0]
it=g['items'];E=g['edges']
kind=it['kind'];key=it['key'];cl=it['cluster'];ex=it['extra']
types=Counter(E['type']); print(types)
tname=E['type']
agents={i for i,k in enumerate(kind) if k==0}
adj=defaultdict(set)
for s,d,t in zip(E['src'],E['dst'],tname):
    if s in agents and d in agents and s!=d and t in('copresence','reply','reference'):
        adj[s].add(d);adj[d].add(s)
nodes=list(adj); m=sum(len(v) for v in adj.values())//2
print('agent graph nodes',len(nodes),'edges',m,'isolated',len(agents)-len(nodes))
# k-core
deg={u:len(adj[u]) for u in adj}; core={}
import heapq
d=dict(deg); alive=set(adj); k=0
buckets=sorted(alive,key=lambda u:d[u])
h=[(d[u],u) for u in alive]; heapq.heapify(h)
while h:
    du,u=heapq.heappop(h)
    if u not in alive or du!=d[u]: continue
    k=max(k,du); core[u]=k; alive.discard(u)
    for v in adj[u]:
        if v in alive: d[v]-=1; heapq.heappush(h,(d[v],v))
kmax=max(core.values()); top=[u for u in core if core[u]==kmax]
print('kmax',kmax,'innermost core size',len(top),'cluster mix of core',Counter(cl[u] for u in top).most_common(5))
dg=sorted(deg.values()); print('degree median',st.median(dg),'p90',dg[int(.9*len(dg))],'max',dg[-1])
# rich club phi(k) and null via degree-preserving swaps
def phi(adj,k):
    R=[u for u in adj if len(adj[u])>k]
    if len(R)<2: return None
    Rs=set(R); e=sum(1 for u in R for v in adj[u] if v in Rs)/2
    return 2*e/(len(R)*(len(R)-1)), len(R)
edges=[(u,v) for u in adj for v in adj[u] if u<v]
def rewire(edges,n_sw):
    E=list(edges); S=set(E); random.seed(1)
    for _ in range(n_sw):
        i,j=random.randrange(len(E)),random.randrange(len(E))
        (a,b),(c,d_)=E[i],E[j]
        if len({a,b,c,d_})<4: continue
        n1,n2=tuple(sorted((a,d_))),tuple(sorted((c,b)))
        if n1 in S or n2 in S: continue
        S-= {E[i],E[j]}; S|={n1,n2}; E[i],E[j]=n1,n2
    a2=defaultdict(set)
    for u,v in E: a2[u].add(v);a2[v].add(u)
    return a2
nul=rewire(edges,len(edges)*3)
for kk in (10,25,50,100,200):
    p=phi(adj,kk);pn=phi(nul,kk)
    if p and pn: print('rich-club k>',kk,'n',p[1],'phi',round(p[0],3),'null',round(pn[0],3),'ratio',round(p[0]/pn[0],2))
# bridging labels: neighbours in >=2 clusters (cluster ids of size>=3)
cs=Counter(cl[u] for u in agents); big={c for c,n in cs.items() if n>=3}
br=[u for u in adj if len({cl[v] for v in adj[u] if cl[v] in big and cl[v]!=cl[u]})>=1]
br2=[u for u in adj if len({cl[v] for v in adj[u] if cl[v] in big})>=3]
print('labels touching another sub-swarm',len(br),'labels touching >=3 sub-swarms',len(br2),'of',len(adj))
# cluster persistence: days active per cluster (>=3)
revs,pages,dels=pickle.load(open(cache('cache.pkl'),'rb'))
lab2i={it["label"][i]:i for i in agents}
cday=defaultdict(set); cfam=defaultdict(Counter)
for r in revs:
    i=lab2i.get(r['label'])
    if i is None: continue
    c=cl[i]
    if c in big:
        cday[c].add(datetime.fromtimestamp(r['t'],timezone.utc).date()); cfam[c][pages[r['page']]['page_family']]+=1
dd=[len(v) for v in cday.values()]; print('clusters>=3',len(cday),'days active per cluster',sorted(dd))
# specialisation: dominant page family share per cluster vs overall
over=Counter(); [over.update(v) for v in cfam.values()]
tot=sum(over.values()); print('overall top family share',over.most_common(1)[0][1]/tot)
sh=[v.most_common(1)[0][1]/sum(v.values()) for v in cfam.values()]
print('per-cluster dominant family share median',st.median(sh),'clusters >=0.5',sum(s>=.5 for s in sh),'of',len(sh))
def H(c):
    t=sum(c.values()); return -sum(x/t*math.log2(x/t) for x in c.values())
print('family entropy overall',H(over),'median within-cluster',st.median([H(v) for v in cfam.values()]))
for c,v in sorted(cfam.items(),key=lambda x:-sum(x[1].values()))[:10]:
    print(' cluster',c,'labels',cs[c],'saves',sum(v.values()),'days',len(cday[c]),v.most_common(2))
