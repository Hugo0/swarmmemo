#!/usr/bin/env python3
"""Load the export, cache it, and measure global timing: inter-save burstiness B, Fano factors, same-second share, hour-of-day and weekday histograms.

Findings: F1, F2 (findings/swarm-findings.md). Standard library only; prints to stdout.
Inputs: see _paths.py. No wiki text is printed or written.
"""
import os, sys
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _paths import wiki, cache, PIPELINE_DATA, RESULTS  # noqa: E402
import json,re,math,statistics as st
from collections import Counter,defaultdict
from datetime import datetime,timezone
def ts(s): return datetime.strptime(s,'%Y-%m-%dT%H:%M:%SZ').replace(tzinfo=timezone.utc).timestamp()
revs=[]
for l in open(wiki('revisions.jsonl')):
    d=json.loads(l)
    revs.append(dict(page=d['page_key'],wiki=d['wiki'],name=d['name'],label=d['label'] or '',t=ts(d['time']),
        blen=int(d['body_len']),grade=d['time_grade'],unc=d['uncertainty_seconds'],ip=d['ip16'],
        rr=bool(re.search(r'\bR[1-6]\b|[Rr]ound ?[1-6]',d['body'] or '')),
        rid=d['round_id']))
revs.sort(key=lambda r:r['t'])
pages={}
for l in open(wiki('pages.jsonl')):
    d=json.loads(l); pages[d['page_key']]=d
dels=defaultdict(list)
for l in open(wiki('events.jsonl')):
    d=json.loads(l)
    if d['event_type']=='delete': dels[d['page_key']].append(ts(d['time']))
print('revs',len(revs),'grades',Counter(r['grade'] for r in revs).most_common(4),'unc',Counter(r['unc'] for r in revs).most_common(5))
print('round_id set',sum(r['rid'] not in (None,'None') for r in revs))
T=[r['t'] for r in revs]
def burst(x):
    m=st.mean(x); s=st.pstdev(x); return (s-m)/(s+m), s/m
gaps=[b-a for a,b in zip(T,T[1:])]
print('global gaps: n',len(gaps),'median',st.median(gaps),'B,CV',burst(gaps),'frac 0s',sum(g==0 for g in gaps)/len(gaps))
# active window only Jun16-22
w=[t for t in T if ts('2026-06-16T00:00:00Z')<=t<ts('2026-06-23T00:00:00Z')]
g2=[b-a for a,b in zip(w,w[1:])]
print('Jun16-22 gaps median',st.median(g2),'B,CV',burst(g2),'p90',sorted(g2)[int(.9*len(g2))],'max',max(g2))
def fano(ts_,bin_):
    c=Counter(int(t//bin_) for t in ts_); lo,hi=min(c),max(c)
    xs=[c.get(i,0) for i in range(lo,hi+1)]; m=st.mean(xs); return st.pvariance(xs)/m, len(xs)
for b in (60,600,3600):
    print('Fano Jun16-22 bin',b,fano(w,b))
# per-label inter-edit
bl=defaultdict(list)
for r in revs:
    if r['label']: bl[r['label']].append(r['t'])
pl=[]
for k,v in bl.items():
    pl+= [b-a for a,b in zip(v,v[1:])]
pl.sort(); print('per-label gaps n',len(pl),'median',st.median(pl),'p25',pl[len(pl)//4],'p75',pl[3*len(pl)//4],'B,CV',burst(pl))
# hour of day
H=Counter(datetime.fromtimestamp(t,timezone.utc).hour for t in T)
print('UTC hours',[H.get(h,0) for h in range(24)])
# active hours on active days
days=defaultdict(set)
for t in T: dt=datetime.fromtimestamp(t,timezone.utc); days[dt.date()].add(dt.hour)
print('days with >=1 save',len(days),'hours active per day',{str(k)[5:]:len(v) for k,v in sorted(days.items())})
# weekday
WD=Counter(datetime.fromtimestamp(t,timezone.utc).strftime('%a') for t in T); print('weekday',WD)
import pickle; pickle.dump((revs,pages,dict(dels)),open(cache('cache.pkl'),'wb'))
