#!/usr/bin/env python3
"""Label lifecycles and recruitment: births by hour, PDT-workday share, lifespans, heavy-hitter share and power-law tail, new labels per day, landing pages, hand-off latency and reciprocity, round markers.

Findings: F1, F3, F4, F5, F7 (findings/swarm-findings.md). Standard library only; prints to stdout.
Inputs: see _paths.py. No wiki text is printed or written.
"""
import os, sys
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _paths import wiki, cache, PIPELINE_DATA, RESULTS  # noqa: E402
import pickle,re,math,statistics as st,bisect
from collections import Counter,defaultdict
from datetime import datetime,timezone
revs,pages,dels=pickle.load(open(cache('cache.pkl'),'rb'))
def dt(t): return datetime.fromtimestamp(t,timezone.utc)
def q(x,p): x=sorted(x); return x[min(len(x)-1,int(p*len(x)))]
# diurnal: excluding Jun18, and first-seen labels per hour
H=Counter(dt(r['t']).hour for r in revs if dt(r['t']).strftime('%m-%d')!='06-18')
print('hours excl Jun18',[H.get(h,0) for h in range(24)])
first={}
for r in revs:
    if r['label'] and r['label'] not in first: first[r['label']]=r
FH=Counter(dt(r['t']).hour for r in first.values())
print('label births by UTC hour',[FH.get(h,0) for h in range(24)])
for d in ['06-17','06-18','06-19','06-20','06-21']:
    h=Counter(dt(r['t']).hour for r in first.values() if dt(r['t']).strftime('%m-%d')==d); print(' births',d,[h.get(i,0) for i in range(24)])
# PT business window share 15-01 UTC (8am-6pm PDT)
biz=lambda h: 15<=h or h<1
print('share saves in 08-18 PDT',sum(1 for r in revs if biz(dt(r['t']).hour))/len(revs),'expected',10/24)
print('share births in 08-18 PDT',sum(1 for r in first.values() if biz(dt(r['t']).hour))/len(first))
# label lifespan & activity
bl=defaultdict(list)
for r in revs:
    if r['label']: bl[r['label']].append(r)
n=[len(v) for v in bl.values()]; span=[v[-1]['t']-v[0]['t'] for v in bl.values()]
print('labels',len(bl),'single-save',sum(x==1 for x in n),'median saves',st.median(n),'max',max(n),'p99',q(n,.99))
print('lifespan s: median',st.median(span),'p75',q(span,.75),'p90',q(span,.9),'>1h',sum(s>3600 for s in span),'>1d',sum(s>86400 for s in span),'>7d',sum(s>7*86400 for s in span))
days_active=[len({dt(r['t']).date() for r in v}) for v in bl.values()]
print('labels active on >1 day',sum(d>1 for d in days_active))
# top-share
n.sort(reverse=True); tot=sum(n)
print('top1% labels share',sum(n[:len(n)//100])/tot,'top10%',sum(n[:len(n)//10])/tot)
# MLE power-law tail alpha for saves/label, xmin=5
xm=5; xs=[x for x in n if x>=xm]; a=1+len(xs)/sum(math.log(x/(xm-.5)) for x in xs); print('alpha saves/label xmin5',a,len(xs))
# recruitment curve
days=Counter(dt(r['t']).strftime('%m-%d') for r in first.values())
cum=0; out=[]
for d in sorted(days): cum+=days[d]; out.append((d,days[d],cum))
print('new labels/day',out)
# where new labels first land
fp=Counter(r['name'] for r in first.values()); print('first page of new labels top',fp.most_common(8))
fam=Counter(pages[r['page']]['page_family'] for r in first.values()); print('first page family',fam.most_common(6))
# hand-off latency: on each page, for each save, time to next save by a different label
lat=[];latc=[]; pairs=Counter()
COORD={'relay-coordination','loop-chain-infrastructure'}
bp=defaultdict(list)
for r in revs: bp[r['page']].append(r)
for p,v in bp.items():
    for i,r in enumerate(v):
        if not r['label']: continue
        for s in v[i+1:]:
            if s['label'] and s['label']!=r['label']:
                g=s['t']-r['t']; lat.append(g)
                if pages[p]['page_family'] in COORD: latc.append(g)
                break
for name,x in (('all pages',lat),('coord pages',latc)):
    print('handoff latency',name,'n',len(x),'median',st.median(x),'p25',q(x,.25),'p75',q(x,.75),'<=60s',sum(g<=60 for g in x)/len(x),'<=10min',sum(g<=600 for g in x)/len(x),'>1d',sum(g>86400 for g in x)/len(x))
# reciprocity: A->B handoff followed later by B->A on same page
rec=0;tot=0
for p,v in bp.items():
    seq=[r['label'] for r in v if r['label']]
    ed=set((a,b) for a,b in zip(seq,seq[1:]) if a!=b)
    tot+=len(ed); rec+=sum((b,a) in ed for a,b in ed)
print('page-level directed handoff pairs',tot,'reciprocated frac',rec/max(tot,1))
# self-continuation vs alternation
same=diff=0
for p,v in bp.items():
    seq=[r['label'] for r in v if r['label']]
    for a,b in zip(seq,seq[1:]): same+= a==b; diff+= a!=b
print('consecutive same-label',same,'different',diff)
# Round markers in bodies
print('revs w/ round markers',sum(r['rr'] for r in revs),'pages', len({r['page'] for r in revs if r['rr']}))
# names
names=[r['name'] for r in revs]
pn=list(pages.values())
def pat(n):
    return dict(zz=bool(re.match(r'(?i)z{2,}',n)), ts10=bool(re.search(r'1[78]\d{8}',n)), counter=bool(re.search(r'\d+$',n)), round=bool(re.search(r'(?i)R[1-6]|round',n)))
pc=Counter()
for p in pn:
    for k,v in pat(p['name']).items(): pc[k]+=v
print('page name patterns',pc,'of',len(pn))
pickle.dump((bl,bp,first),open(cache('c2.pkl'),'wb'))
