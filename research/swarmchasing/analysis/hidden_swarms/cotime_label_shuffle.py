"""Within-board co-timing, label-shuffle null.

Counts pairs of authors posting within W seconds of each other on each board, then compares with a null that
shuffles author labels among posts of the same UTC day (keeps per-day volume, breaks who-posts-with-whom).
Usage: python cotime_label_shuffle.py [W_SECONDS=10]   Writes $SWARMCHASING_OUT/cotime_label_shuffle_<W>.json.
"""
import sys,random,collections,math,json
from _boards import *
random.seed(1)
W=int(sys.argv[1]) if len(sys.argv)>1 else 10
EXCL={'anonymous','name:Anonymous'}
out={}
for b in BOARDS:
    P,A=load(b)
    P=[p for p in P if p['author_id'] not in EXCL]
    P.sort(key=lambda p:p['t'])
    T=[p['t'] for p in P]; au=[p['author_id'] for p in P]
    n=len(P)
    def pairs(au):
        c=collections.Counter(); j=0
        for i in range(n):
            # look forward
            k=i+1
            while k<n and T[k]-T[i]<=W:
                if au[k]!=au[i]:
                    c[tuple(sorted((au[i],au[k])))]+=1
                k+=1
        return c
    obs=pairs(au)
    tot=sum(obs.values())
    # null: permute author labels within day (keeps per-day activity) 
    R=200
    nullc=collections.defaultdict(list); nulltot=[]
    days=collections.defaultdict(list)
    for i,p in enumerate(P): days[int(p['t']//86400)].append(i)
    for r in range(R):
        a2=list(au)
        for d,idx in days.items():
            v=[au[i] for i in idx]; random.shuffle(v)
            for i,x in zip(idx,v): a2[i]=x
        c=pairs(a2); nulltot.append(sum(c.values()))
        for k in obs: nullc[k].append(c.get(k,0))
    res=[]
    for k,o in obs.items():
        v=nullc[k]; m=sum(v)/R; sd=(sum((x-m)**2 for x in v)/R)**.5
        p=(1+sum(x>=o for x in v))/(R+1)
        if o>=3: res.append((o,m,sd,p,k))
    sig=[r for r in res if r[3]<=1/(R+1)+1e-9 and r[0]>=max(3,3*r[1])]
    sig.sort(key=lambda r:-r[0])
    mt=sum(nulltot)/R
    print(f"== {b} W={W}s posts={n} pairs_events obs={tot} null={mt:.0f} ratio={tot/max(mt,1):.2f}  sigpairs={len(sig)} (of {len(res)} pairs with>=3)")
    for o,m,sd,p,k in sig[:12]:
        print(f"   {o:4d} vs null {m:6.2f}  {name(A,k[0])} <> {name(A,k[1])}")
    out[b]=dict(obs=tot,null=mt,sig=[(o,m,list(k)) for o,m,sd,p,k in sig])
json.dump(out,open(out_path(f"cotime_label_shuffle_{W}.json"),"w"))
