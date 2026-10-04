"""Within-board co-timing, free circular-shift null.

Pairs posting within W s count as episodes (a 5-min gap starts a new one). Null: each author's whole timeline is
shifted by a uniform random offset in [-H, +H] hours, circularly within the board window. This null is fooled by
shared cron grids; use cotime_cron_preserving.py before calling a pair coordinated.
Usage: python cotime_circular_shift.py W_SECONDS SHIFT_HOURS [ROUNDS=100] [board,board,...]
"""
import sys,random,collections,math,json,bisect
from _boards import *
random.seed(2)
W=float(sys.argv[1]); SH=float(sys.argv[2])*3600; R=int(sys.argv[3]) if len(sys.argv)>3 else 100
only=sys.argv[4].split(',') if len(sys.argv)>4 else BOARDS
EXCL={'anonymous','name:Anonymous'}
out={}
def episodes(times,gap=300):
    times=sorted(times); e=0; last=-1e18
    for t in times:
        if t-last>gap: e+=1
        last=t
    return e
for b in only:
    P,A=load(b)
    P=[p for p in P if p['author_id'] not in EXCL]
    by=collections.defaultdict(list)
    for p in P: by[p['author_id']].append(p['t'])
    for k in by: by[k].sort()
    act=[k for k in by if len(by[k])>=3]
    lo=min(p['t'] for p in P); hi=max(p['t'] for p in P); span=hi-lo
    def count(times):
        # events: (t,author) merged sort
        ev=sorted((t,a) for a in act for t in times[a])
        c=collections.defaultdict(list); n=len(ev)
        for i in range(n):
            k=i+1
            while k<n and ev[k][0]-ev[i][0]<=W:
                if ev[k][1]!=ev[i][1]:
                    c[tuple(sorted((ev[i][1],ev[k][1])))].append(ev[i][0])
                k+=1
        return {k:episodes(v) for k,v in c.items()}
    obs=count(by)
    nulls=collections.defaultdict(list); ntot=[]
    for r in range(R):
        tm={}
        for a in act:
            s=random.uniform(-SH,SH)
            # circular within board window
            tm[a]=[lo+((t-lo+s)%span) for t in by[a]]
        c=count(tm); ntot.append(sum(c.values()))
        for k in obs: nulls[k].append(c.get(k,0))
    res=[]
    for k,o in obs.items():
        v=nulls[k]; m=sum(v)/R
        p=(1+sum(x>=o for x in v))/(R+1)
        res.append((o,m,p,k))
    sig=[r for r in res if r[2]<=0.01 and r[0]>=3 and r[0]>=3*r[1]]
    sig.sort(key=lambda r:-(r[0]-r[1]))
    tot=sum(obs.values()); mt=sum(ntot)/R
    print(f"== {b} W={W}s shift±{SH/3600}h authors>=3posts={len(act)} episodes obs={tot} null={mt:.1f} ratio={tot/max(mt,.1):.2f} sigpairs={len(sig)} (expected false ~{0.01*len(res):.1f} of {len(res)})")
    for o,m,p,k in sig[:15]:
        print(f"   {o:4d} ep vs null {m:6.2f}  {name(A,k[0])} <> {name(A,k[1])}  ({len(by[k[0]])},{len(by[k[1]])} posts)")
    out[b]=dict(obs=tot,null=mt,npairs=len(res),sig=[(o,m,list(k)) for o,m,p,k in sig])
json.dump(out,open(out_path(f"cotime_circular_shift_{int(W)}_{int(SH/3600)}.json"),"w"))
