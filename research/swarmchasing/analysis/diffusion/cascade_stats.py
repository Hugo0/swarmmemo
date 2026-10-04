"""Cascades per key kind (url, dom, repo, term, ng): how many boards each key reaches, the origin board, first-sighting
lags to every other board, and who carried each hop (same handle, a linked identity from the matcher, or independent).
Only keys first seen on or after the window start count. Reads cache/items.pkl and the identity links; writes cache/res.pkl.
"""
import pickle,collections,json,os,datetime as dt
from _posts import BOARDS, LINKS, WIN, cache
items=pickle.load(open(cache('items.pkl'),'rb'))
T=lambda s: dt.datetime.fromisoformat(s)
# identity links
links=collections.defaultdict(set)
for l in open(LINKS):
    d=json.loads(l); a=tuple(d['a']); b=tuple(d['b']); links[a].add(b); links[b].add(a)
def pct(xs,q): 
    xs=sorted(xs); return xs[min(len(xs)-1,int(q*len(xs)))] if xs else None
res={}
for kind in ['url','dom','repo','term','ng']:
    casc=[];lags=[];origin=collections.Counter();hop=collections.Counter();carrier=collections.Counter();pairs=collections.Counter()
    nmulti=0;ntotal=0
    for (k,v),occ in items.items():
        if k!=kind: continue
        ntotal+=1
        first={}
        for t,b,a,h in occ:
            if b not in first: first[b]=(t,a,h)
        if len(first)<2: continue
        nmulti+=1
        o=min(first.items(),key=lambda x:x[1][0])
        if o[1][0]<WIN: continue   # left-censored
        if kind=='ng':
            # require >=2 distinct handles overall for ng to drop pure self-template? keep, but record
            pass
        origin[o[0]]+=1
        authors=set((b,a) for t,b,a,h in occ)
        casc.append((len(first),len(authors),len(occ),k,v,o[0]))
        seen=[(t,b,a,h) for t,b,a,h in occ]
        for b,(t,a,h) in first.items():
            if b==o[0]: continue
            lag=(T(t)-T(o[1][0])).total_seconds()/3600
            lags.append(lag); pairs[(o[0],b)]+=1
            prior=[(pb,pa,ph) for pt,pb,pa,ph in occ if pt<t and pb!=b]
            if any(ph==h and len(h)>2 for pb,pa,ph in prior): c='same-handle'
            elif any((pb,pa) in links[(b,a)] for pb,pa,ph in prior): c='linked-id'
            else: c='independent'
            carrier[c]+=1
    res[kind]=dict(total=ntotal,multi=nmulti,valid=len(casc),origin=origin,carrier=carrier,lags=lags,casc=casc,pairs=pairs)
    print('==',kind,'items',ntotal,'on>=2 boards',nmulti,'origin>=WIN',len(casc))
    if not casc: continue
    print(' origin',origin.most_common())
    print(' carrier',dict(carrier))
    print(' lag h p10/25/50/75/90', [round(pct(lags,q),1) for q in (.1,.25,.5,.75,.9)], 'n',len(lags), 'within1h',sum(l<1 for l in lags),'within24h',sum(l<24 for l in lags))
    sz=collections.Counter(c[0] for c in casc); print(' cascade boards dist',sorted(sz.items()))
    au=collections.Counter(min(c[1],20) for c in casc); print(' cascade authors dist',sorted(au.items()))
    for c in sorted(casc,reverse=True)[:15]: print('   ',c[:3],c[4][:90],c[5])
pickle.dump(res,open(cache('res.pkl'),'wb'))
