#!/usr/bin/env python3
"""Moderator deletion and the swarm response: saves vs deletes per day, re-saves after deletes, alphabetical delete order (Spearman), ZZ/ZZZ backup pages.

Findings: F12, F13, F14 (findings/swarm-findings.md). Standard library only; prints to stdout.
Inputs: see _paths.py. No wiki text is printed or written.
"""
import os, sys
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _paths import wiki, cache, PIPELINE_DATA, RESULTS  # noqa: E402
import pickle,re,math,statistics as st,bisect
from collections import Counter,defaultdict
from datetime import datetime,timezone
revs,pages,dels=pickle.load(open(cache('cache.pkl'),'rb'))
bl,bp,first=pickle.load(open(cache('c2.pkl'),'rb'))
def dt(t): return datetime.fromtimestamp(t,timezone.utc)
def q(x,p): x=sorted(x); return x[min(len(x)-1,int(p*len(x)))]
# deletes vs saves per day
sd=Counter(dt(r['t']).strftime('%m-%d') for r in revs)
dd=Counter(dt(t).strftime('%m-%d') for v in dels.values() for t in v)
print('day saves deletes',[(d,sd.get(d,0),dd.get(d,0)) for d in sorted(set(sd)|set(dd)) if d<'06-25'])
# first delete time
alld=sorted(t for v in dels.values() for t in v); print('first delete',dt(alld[0]),'deletes before Jun19',sum(t<1781827200 for t in alld))
# per deleted page: was it re-saved after a delete, and how fast
resave=[];n_del_pages=0;resaved_pages=0
for p,dl in dels.items():
    saves=sorted(r['t'] for r in bp.get(p,[]))
    n_del_pages+=1; hit=False
    for t in dl:
        i=bisect.bisect_right(saves,t)
        if i<len(saves): resave.append(saves[i]-t); hit=True
    resaved_pages+=hit
print('deleted pages',n_del_pages,'re-saved after a delete',resaved_pages,'lag median',st.median(resave) if resave else None,'p25',q(resave,.25),'<=10min',sum(x<=600 for x in resave),'n',len(resave))
# Only deletes during active period (before Jun23)
act=[(p,t) for p,v in dels.items() for t in v if t<1782172800]
print('deletes before Jun23',len(act))
r2=[];hit=0
for p,t in act:
    saves=sorted(r['t'] for r in bp.get(p,[]))
    i=bisect.bisect_right(saves,t)
    if i<len(saves): r2.append(saves[i]-t); hit+=1
print(' active-period deletes followed by re-save on same page',hit,'median lag',st.median(r2) if r2 else None, 'within 1h',sum(x<=3600 for x in r2))
# Alphabet hypothesis: delete order vs page name initial on Jun19-23
seq=sorted((t,p) for p,v in dels.items() for t in v if 1781827200<=t<1782259200)
init=[p.split('~',1)[1][:1].upper() for t,p in seq]
# spearman between time rank and letter
L=[ord(c) for c in init]; n=len(L)
def rank(x):
    s=sorted(range(len(x)),key=lambda i:x[i]); r=[0]*len(x)
    for k,i in enumerate(s): r[i]=k
    return r
rt=list(range(n)); rl=rank(L)
mt=(n-1)/2; cov=sum((a-mt)*(b-mt) for a,b in zip(rt,rl)); print('Jun19-24 deletes',n,'spearman(time, initial letter)',cov/sum((a-mt)**2 for a in rt))
# ZZ pages over time
zz=[r for r in revs if re.match(r'(?i)z{2,}',r['name'])]
print('ZZ saves by day',sorted(Counter(dt(r['t']).strftime('%m-%d') for r in zz).items()),'labels',len({r['label'] for r in zz}))
zzp=[p for p in pages.values() if re.match(r'(?i)z{2,}',p['name'])]
print('ZZ pages',len(zzp),'deleted_live',Counter(p['deleted_live'] for p in zzp),'all pages deleted_live',Counter(p['deleted_live'] for p in pages.values()))
print('first ZZ page saves',[(dt(r['t']).isoformat(),r['name'][:30]) for r in zz[:3]])
# ZZZ first
zzz=[r for r in revs if re.match(r'(?i)zzz',r['name'])]; print('first ZZZ',dt(zzz[0]['t']) if zzz else None,len(zzz))
# timestamp pages valid range
tsn=[p for p in pages.values() if re.search(r'17[78]\d{7}',p['name'])]; print('ts10 pages valid',len(tsn))
