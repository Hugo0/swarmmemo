"""Multi-board handles, origin rates per 1,000 window posts, and 6-gram passage hops (lag, same-handle share).
Reads cache/items.pkl and cache/res.pkl (from cascade_stats.py).
"""
import pickle,collections,json,datetime as dt
from _posts import WIN, cache, load
items=pickle.load(open(cache('items.pkl'),'rb')); res=pickle.load(open(cache('res.pkl'),'rb'))
P=load()
win=collections.Counter(p['b'] for p in P if p['t']>=WIN)
print('posts in window',dict(win))
# multi-board handles
hb=collections.defaultdict(set); hposts=collections.Counter()
for p in P:
    if len(p['h'])>2: hb[p['h']].add(p['b']); hposts[p['h']]+=1
multi={h for h,b in hb.items() if len(b)>=2}
print('handles',len(hb),'multi-board handles',len(multi),'their posts share',sum(hposts[h] for h in multi)/len(P))
print(sorted(((len(hb[h]),hposts[h],h) for h in multi),reverse=True)[:30])
# origins normalized
for kind in ['url','dom','ng']:
    o=res[kind]['origin']; tot=sum(o.values())
    print(kind,'origin per 1k window posts',{b:round(1000*o[b]/win[b],1) for b in win})
# n-gram passages: group by origin (t,b,a)
T=lambda s: dt.datetime.fromisoformat(s)
pas=collections.defaultdict(lambda: dict(boards=set(),hops={},ngr=0,auth=set()))
for (k,v),occ in items.items():
    if k!='ng': continue
    first={}
    for t,b,a,h in occ:
        if b not in first: first[b]=(t,a,h)
    if len(first)<2: continue
    o=min(first.items(),key=lambda x:x[1][0])
    if o[1][0]<WIN: continue
    key=(o[1][0],o[0],o[1][1])
    P_=pas[key]; P_['boards']|=set(first); P_['ngr']+=1; P_['auth']|=set((b,a) for t,b,a,h in occ)
    for b,(t,a,h) in first.items():
        if b!=o[0]:
            lag=(T(t)-T(o[1][0])).total_seconds()/3600
            same = h==o[1][2]
            P_['hops'][b]=min(P_['hops'].get(b,(1e9,0)),(lag,same))
print('passages',len(pas))
# keep passages with >=3 ngrams (robust)
pp=[v for v in pas.values() if v['ngr']>=3]
print('passages >=3 shared ngrams',len(pp))
hops=[h for v in pp for h in v['hops'].values()]
same=sum(1 for l,s in hops if s); print('hops',len(hops),'same handle as origin',same)
lags=sorted(l for l,s in hops); 
import statistics
print('lag med',statistics.median(lags),'p75',lags[3*len(lags)//4],'p90',lags[9*len(lags)//10],'<1h',sum(l<1 for l in lags))
ind=sorted(l for l,s in hops if not s); print('indep hops',len(ind),'med lag',statistics.median(ind) if ind else None)
print('passage boards dist',sorted(collections.Counter(len(v['boards']) for v in pp).items()))
print('passage origin',collections.Counter(k[1] for k,v in pas.items() if v['ngr']>=3).most_common())
