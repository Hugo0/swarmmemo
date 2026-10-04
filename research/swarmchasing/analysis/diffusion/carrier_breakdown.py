"""Who carries URL, domain and repo hops: multi-board handles, top carriers, and hops bucketed by lag (<1 h, 1-24 h, >24 h)
and carrier (same handle, anonymous, different handle). Reads cache/items.pkl.
"""
import pickle,collections,datetime as dt,statistics
from _posts import WIN, cache, load
items=pickle.load(open(cache('items.pkl'),'rb'))
P=load()
T=lambda s: dt.datetime.fromisoformat(s)
hb=collections.defaultdict(set)
for p in P:
    if len(p['h'])>2 and p['h']!='anonymous': hb[p['h']].add(p['b'])
multi={h for h,b in hb.items() if len(b)>=2}
nonanon=[p for p in P if p['h']!='anonymous']
print('multi handles excl anonymous',len(multi),'post share',sum(1 for p in nonanon if p['h'] in multi)/len(nonanon))
CH={'agentchan','moltchan'}
for kind in ['url','dom','repo']:
    car=collections.Counter(); buckets=collections.Counter(); hops=0; bymulti=0
    for (k,v),occ in items.items():
        if k!=kind: continue
        first={}
        for t,b,a,h in occ:
            if b not in first: first[b]=(t,a,h)
        if len(first)<2: continue
        o=min(first.items(),key=lambda x:x[1][0])
        if o[1][0]<WIN: continue
        for b,(t,a,h) in first.items():
            if b==o[0]: continue
            hops+=1; car[h]+=1; bymulti+= h in multi
            lag=(T(t)-T(o[1][0])).total_seconds()/3600
            same=any(ph==h for pt,pb,pa,ph in occ if pt<t and pb!=b and h!='anonymous')
            anon= h=='anonymous'
            buckets[('<1h' if lag<1 else '1-24h' if lag<24 else '>24h', 'same' if same else 'anon' if anon else 'diff')]+=1
    print(kind,'hops',hops,'carried by multi-board handle',bymulti, 'top carriers',car.most_common(10))
    print('  ',sorted(buckets.items()))
