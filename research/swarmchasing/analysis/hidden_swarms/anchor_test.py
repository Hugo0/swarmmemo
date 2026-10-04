"""Cross-board anchor test.

Anchors: Sanctum profiles registered from outside after ANCHOR_SINCE (default 2026-09-25T21:00:00). For each other
board, count anchors followed by at least one post in [-30 s, +W s] (W=300) and compare with 500 draws where every
anchor is moved by 1-6 h in a random direction. Reports hits, null mean, p and the authors most often in the window.
Usage: python anchor_test.py   (env ANCHOR_BOARD, ANCHOR_SINCE)
"""
from _boards import *
import collections
import bisect,random,datetime,os
random.seed(4)
AB=os.environ.get('ANCHOR_BOARD','sanctum'); SINCE=os.environ.get('ANCHOR_SINCE','2026-09-25T21:00:00')
SP,SA=load(AB)
anch=sorted(ts(a['created_at']) for a in SA.values() if a.get('created_at') and a.get('extra',{}).get('origin')=='external' and ts(a['created_at'])>ts(SINCE))
print('anchors',len(anch))
W=300
for b in BOARDS:
    if b==AB: continue
    P,_A=load(b)
    T=sorted((p['t'],p['author_id']) for p in P); tt=[x[0] for x in T]
    lo,hi=tt[0],tt[-1]
    an=[a for a in anch if lo<=a<=hi]
    if not an: print(b,'no overlap'); continue
    def hits(arr):
        h=0; who=collections.Counter()
        for a in arr:
            i=bisect.bisect_left(tt,a-30); j=bisect.bisect_right(tt,a+W)
            if j>i:
                h+=1
                for k in set(T[x][1] for x in range(i,j)): who[k]+=1
        return h,who
    h,who=hits(an)
    nh=[]
    for r in range(500):
        sh=[a+random.choice([-1,1])*random.uniform(3600,6*3600) for a in an]
        nh.append(hits(sh)[0])
    m=sum(nh)/len(nh); p=(1+sum(x>=h for x in nh))/(len(nh)+1)
    print(f"{b:10s} anchors_in_window={len(an)} hit[-30s,+{W}s]={h} null={m:.1f} p={p:.3f}  top={[(name(_A,k),c) for k,c in who.most_common(4)]}")
