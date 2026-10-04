"""Cross-board co-timing: every (board, author) pair on different boards posting within 60 s, as episodes
(a 10-min gap starts a new one), against 50 draws of a +-6 h per-identity shift. Prints significant pairs.
Usage: python cross_board_cotime.py
"""
from _boards import *
import collections
import random
random.seed(12)
W=60
ev=[]; nm={}
for b in BOARDS:
    if b=='moltchan': pass
    P,A=load(b)
    for p in P:
        k=(b,p['author_id']); nm[k]=b[:4]+':'+name(A,p['author_id'])
        ev.append((p['t'],k))
by=collections.defaultdict(list)
for t,k in ev: by[k].append(t)
lo=min(t for t,k in ev); hi=max(t for t,k in ev); span=hi-lo
def count(by):
    E=sorted((t,k) for k,v in by.items() for t in v)
    c=collections.defaultdict(list)
    for i in range(len(E)):
        j=i+1
        while j<len(E) and E[j][0]-E[i][0]<=W:
            if E[j][1][0]!=E[i][1][0]: c[tuple(sorted((E[i][1],E[j][1])))].append(E[i][0])
            j+=1
    out={}
    for k,v in c.items():
        v.sort(); e=0; last=-1e18
        for t in v:
            if t-last>600: e+=1
            last=t
        out[k]=e
    return out
obs=count(by)
cand={k:o for k,o in obs.items() if o>=3}
R=50; nul=collections.defaultdict(list)
for r in range(R):
    b2={k:[t+random.uniform(-6*3600,6*3600)*0+s for t in v] for k,v in by.items() for s in [random.uniform(-6*3600,6*3600)]}
    c=count(b2)
    for k in cand: nul[k].append(c.get(k,0))
res=[]
for k,o in cand.items():
    m=sum(nul[k])/R; p=(1+sum(x>=o for x in nul[k]))/(R+1)
    if p<=0.02 and o>=3*max(m,0.5): res.append((o,m,k))
res.sort(key=lambda r:-r[0])
print('cross-board identity pairs with >=3 co-episodes:',len(cand),'significant:',len(res))
for o,m,k in res[:40]: print(f"  {o:3d} vs {m:.2f}  {nm[k[0]]} <> {nm[k[1]]}")
