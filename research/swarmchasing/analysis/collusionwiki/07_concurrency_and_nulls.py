#!/usr/bin/env python3
"""Concurrency within 1 s / 10 s, birth bursts, hand-off latency against a within-page label-shuffle null, time to first contact per page family.

Findings: F2, F5, F6 (findings/swarm-findings.md). Standard library only; prints to stdout.
Inputs: see _paths.py. No wiki text is printed or written.
"""
import os, sys
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _paths import wiki, cache, PIPELINE_DATA, RESULTS  # noqa: E402
import pickle,random,statistics as st,bisect
from collections import Counter,defaultdict
revs,pages,dels=pickle.load(open(cache('cache.pkl'),'rb'))
bl,bp,first=pickle.load(open(cache('c2.pkl'),'rb'))
# concurrency: saves within 1s / 10s of a save by a different label (any page)
T=[(r['t'],r['label']) for r in revs if r['label']]
ts=[t for t,_ in T]
c1=c10=0
for i,(t,l) in enumerate(T):
    j=bisect.bisect_left(ts,t-10); k=bisect.bisect_right(ts,t+10)
    labs10={T[x][1] for x in range(j,k)}-{l}
    labs1={T[x][1] for x in range(j,k) if abs(T[x][0]-t)<=1}-{l}
    c1+=bool(labs1); c10+=bool(labs10)
print('saves with another label within 1s',c1/len(T),'within 10s',c10/len(T))
# births in bursts: new labels whose first save is within 60s of another label's birth
B=sorted(r['t'] for r in first.values())
nb=sum(1 for i,t in enumerate(B) if (i>0 and t-B[i-1]<=60) or (i+1<len(B) and B[i+1]-t<=60))
print('births within 60s of another birth',nb,len(B))
# handoff null: permute labels within page, keep times
def lat(bp,shuffle=False,seed=0):
    rnd=random.Random(seed); out=[]
    for p,v in bp.items():
        L=[r['label'] for r in v]; 
        if shuffle: rnd.shuffle(L)
        for i in range(len(v)):
            if not L[i]: continue
            for j in range(i+1,len(v)):
                if L[j] and L[j]!=L[i]: out.append(v[j]['t']-v[i]['t']); break
    return out
a=lat(bp); n=lat(bp,True)
print('observed median',st.median(a),'n',len(a),'null median',st.median(n),'n',len(n))
# first-reader latency: on pages created by A, time until first other label edit, by page family
fl=defaultdict(list)
for p,v in bp.items():
    a0=v[0]['label']
    for s in v[1:]:
        if s['label'] and s['label']!=a0: fl[pages[p]['page_family']].append(s['t']-v[0]['t']); break
tot=sum(len(x) for x in fl.values())
print('pages that got a 2nd label',tot,'of',len(bp))
for f,x in sorted(fl.items(),key=lambda kv:-len(kv[1]))[:6]: print(' ',f,len(x),'median s',st.median(x))
