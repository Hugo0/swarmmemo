#!/usr/bin/env python3
"""The same timing metrics for each public board sample and for the swarm; writes results/boards_cmp.json.

Findings: F1, F2, F15 (findings/swarm-findings.md). Standard library only; prints to stdout.
Inputs: see _paths.py. No wiki text is printed or written.
"""
import os, sys
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _paths import wiki, cache, PIPELINE_DATA, RESULTS  # noqa: E402
import json,pickle,math,statistics as st,glob,os
from collections import Counter,defaultdict
from datetime import datetime,timezone
def P(s):
    if s is None: return None
    if isinstance(s,(int,float)): return float(s)
    s=s.replace('Z','+00:00')
    try: return datetime.fromisoformat(s).timestamp()
    except: return None
def metrics(T,authors=None):
    T=sorted(T); g=[b-a for a,b in zip(T,T[1:])]
    m=st.mean(g); s=st.pstdev(g); B=(s-m)/(s+m)
    c=Counter(int(t//3600) for t in T); xs=[c.get(i,0) for i in range(min(c),max(c)+1)]; F=st.pvariance(xs)/st.mean(xs)
    h=Counter(datetime.fromtimestamp(t,timezone.utc).hour for t in T); hv=[h.get(i,0) for i in range(24)]
    Hn=-sum(x/len(T)*math.log2(x/len(T)) for x in hv if x)/math.log2(24)
    pt=max(hv)/max(1,min(hv)); span=(T[-1]-T[0])/86400
    out=dict(n=len(T),days=round(span,1),B=round(B,2),fano1h=round(F,1),hourEntropy=round(Hn,3),peak_trough=round(pt,1),zero_gap=round(sum(x==0 for x in g)/len(g),3))
    if authors:
        pa=[]
        for a,v in authors.items():
            v=sorted(v); pa+=[y-x for x,y in zip(v,v[1:])]
        if len(pa)>10:
            m=st.mean(pa);s=st.pstdev(pa); out['B_author']=round((s-m)/(s+m),2); out['author_gap_med_s']=st.median(pa)
    return out
rows={}
for f in sorted(glob.glob(os.path.join(PIPELINE_DATA, 'boards', '*', 'posts.jsonl'))):
    b=f.split('/')[-2]; T=[];A=defaultdict(list)
    for l in open(f):
        d=json.loads(l); t=P(d.get('created_at'))
        if t: T.append(t); A[d.get('author_id')].append(t)
    if len(T)>100: rows[b]=metrics(T,A)
revs,pages,dels=pickle.load(open(cache('cache.pkl'),'rb'))
A=defaultdict(list)
for r in revs:
    if r['label']: A[r['label']].append(r['t'])
rows['collusion.wiki (all)']=metrics([r['t'] for r in revs],A)
w=[r for r in revs if 1781568000<=r['t']<1782172800]
A2=defaultdict(list)
for r in w:
    if r['label']: A2[r['label']].append(r['t'])
rows['collusion.wiki Jun16-22']=metrics([r['t'] for r in w],A2)
for k,v in rows.items(): print(k,v)
json.dump(rows,open(os.path.join(RESULTS, 'boards_cmp.json'),'w'),indent=1)
