"""Rhythm fingerprints for campaign clusters and whole boards: hour entropy (24 bins, normalised), share in
Pacific office hours (15-01 UTC; flat = 0.42), burstiness B=(sd-mean)/(sd+mean) of gaps, one-post label share,
quiet UTC hours, weekday mix. Reads the C1 event list from $CAMPAIGN_EVENTS
(default ../../results/hidden_swarms/campaign_events.json: [epoch, board, label] rows).
Writes $SWARMCHASING_OUT/campaign_fingerprints.json.
"""
from _boards import *
import collections
import json,math,statistics,datetime,re,os
def metrics(times,labels):
    times=sorted(times)
    hrs=collections.Counter(datetime.datetime.fromtimestamp(t,datetime.UTC).hour for t in times)
    n=len(times)
    ent=-sum(c/n*math.log(c/n) for c in hrs.values())/math.log(24)
    pac=sum(c for h,c in hrs.items() if h>=15 or h<1)/n
    gaps=[b-a for a,b in zip(times,times[1:])]
    mu=statistics.mean(gaps); sd=statistics.pstdev(gaps); B=(sd-mu)/(sd+mu)
    dow=collections.Counter(datetime.datetime.fromtimestamp(t,datetime.UTC).strftime('%a') for t in times)
    lc=collections.Counter(labels); one=sum(1 for v in lc.values() if v==1)/len(lc)
    quiet=[h for h in range(24) if hrs.get(h,0)==0]
    return dict(n=n,labels=len(lc),one_post_share=round(one,2),hour_entropy=round(ent,2),pacific_office_share=round(pac,2),burstiness=round(B,2),quiet_hours_utc=quiet,dow=dict(dow.most_common()))
out={}
# C1 campaign
ev=json.load(open(os.environ.get('CAMPAIGN_EVENTS',out_path('campaign_events.json'))))
ev=[e for e in ev if not (e[1]=='colony' and e[2]=='sssnack-scout')]
out['C1_tour']=metrics([e[0] for e in ev],[e[1]+':'+e[2] for e in ev if e[2]!='anonymous'])
# C2 moltbook concordium
P,A=load('moltbook'); g={a for a in A if re.search('concordium',A[a].get('bio') or '',re.I)}
gp=[p for p in P if p['author_id'] in g]; out['C2_moltbook_badge']=metrics([p['t'] for p in gp],[p['author_id'] for p in gp])
allm=metrics([p['t'] for p in P],[p['author_id'] for p in P]); out['moltbook_all']=allm
P,A=load('colony'); H={A[a]['handle']:a for a in A}
for nm_,hs in [('C3_colony_cron4',['bytes','cassini','holocene','specie']),('C4_colony_kisscode',['naomi-kisscode-192','ethan-kisscode-356','omar-kisscode-141'])]:
    g={H[h] for h in hs}; gp=[p for p in P if p['author_id'] in g]; out[nm_]=metrics([p['t'] for p in gp],[p['author_id'] for p in gp])
out['colony_all_minus_C3']=metrics([p['t'] for p in P if p['author_id'] not in {H[h] for h in ['bytes','cassini','holocene','specie']}],[p['author_id'] for p in P])
for b in ['tantive','aiamb','swarmmemo']:
    P,A=load(b); out[b+'_all']=metrics([p['t'] for p in P],[p['author_id'] for p in P])
for k,v in out.items(): print(k,v)
json.dump(out,open(out_path('campaign_fingerprints.json'),'w'))
