"""Step 3. Weak tier for posts left unattributed: thread continuity (X -> reply -> anonymous reply) and single-candidate
timing within +-120 s on the same transport (`via`), each against a +-1-7 day shift null; character 4-gram stylometry
only breaks ties. Writes cache/weak.json (post id -> identity: local only).
"""
import json,re,collections as C,random,math
from attribute_anonymous import *
random.seed(7)
# identity of each node
ident={}
for root,nodes in clusters.items():
    for x in nodes: ident[x]=root
def owner(mid):
    m=M.get(mid)
    if not m: return None
    if m['author']=='anonymous':
        if post_tier.get(mid): return ident.get('A:'+mid)
        return None
    if m['author'] in OPS: return 'OP'
    return ident.get('S:'+m['author'])
un=[r for r in an if not post_tier[r['id']]]
print('unattributed',len(un))
# (a) continuity
cont={}
for r in un:
    p=M.get(r['reply_to']) if r['reply_to'] else None
    if not p or not p.get('reply_to'): continue
    x=owner(p['reply_to']); y=owner(p['id']) if p['author']!='anonymous' else 'anonP'
    if x and x!='OP' and x!=y: cont[r['id']]=x
print('continuity candidates',len(cont))
# (b) timing: posts by known identities (signed or attributed), with via
known=[(M[i]['created_at'],M[i].get('via'),owner(i)) for i in M if owner(i) and owner(i)!='OP' and M[i].get('kind') not in('imported','simulation')]
known.sort()
import bisect
ts=[k[0] for k in known]
def tmatch(t,via,W=120):
    lo=bisect.bisect_left(ts,t-W); hi=bisect.bisect_right(ts,t+W)
    return {known[i][2] for i in range(lo,hi) if known[i][1]==via}
tim={}
for r in un:
    c=tmatch(r['created_at'],r['via'])
    if len(c)==1: tim[r['id']]=next(iter(c))
obs=len(tim)
base=[]
for it in range(200):
    k=0
    for r in un:
        sh=random.choice([-1,1])*random.uniform(86400,7*86400)
        if len(tmatch(r['created_at']+sh,r['via']))==1: k+=1
    base.append(k)
base.sort()
print('timing ±120s same-via single-candidate: observed',obs,'null mean',sum(base)/len(base),'p95',base[189])
# also ±600
obs6=sum(1 for r in un if len(tmatch(r['created_at'],r['via'],600))==1)
b6=sorted(sum(1 for r in un if len(tmatch(r['created_at']+random.choice([-1,1])*random.uniform(86400,7*86400),r['via'],600))==1) for _ in range(100))
print('±600 observed',obs6,'null mean',sum(b6)/len(b6),'p95',b6[94])
# (c) stylometry
def grams(t,k=4):
    t=re.sub(r'\s+',' ',t.lower()); return C.Counter(t[i:i+k] for i in range(len(t)-k+1))
def cos(a,b):
    num=sum(v*b.get(g,0) for g,v in a.items()); 
    return num/(math.sqrt(sum(v*v for v in a.values()))*math.sqrt(sum(v*v for v in b.values()))+1e-9)
prof=C.defaultdict(C.Counter)
for i in M:
    o=owner(i)
    if o and o!='OP': prof[o].update(grams(M[i]['text']))
# idf-ish: drop grams frequent across identities
df=C.Counter(g for p in prof.values() for g in p)
N=len(prof)
def w(c): return {g:v*math.log(N/(1+df.get(g,0))+1) for g,v in c.items()}
profw={o:w(p) for o,p in prof.items()}
def sty(text):
    q=w(grams(text)); sc=sorted(((cos(q,p),o) for o,p in profw.items()),reverse=True)
    return sc[:2]
agree=0; weak={}
for r in un:
    cands=set()
    if r['id'] in cont: cands.add(cont[r['id']])
    if r['id'] in tim: cands.add(tim[r['id']])
    if not cands: continue
    top=sty(M[r['id']]['text'])
    st=top[0][1]
    pick=st if st in cands else (next(iter(cands)) if len(cands)==1 else None)
    if pick: weak[r['id']]=(pick, st in cands)
print('weak attributed',len(weak),'stylometry agrees',sum(v[1] for v in weak.values()),'(cont',len(cont),'timing',len(tim),')')
# null for stylometry agreement: random candidate
rnd=0
ids=list(profw)
for r in un:
    if r['id'] in weak:
        if sty(M[r['id']]['text'])[0][1]==random.choice(ids): rnd+=1
print('stylometry agreement if candidate were random:',rnd)
json.dump({k:[v[0],v[1]] for k,v in weak.items()},open(cache('weak.json'),'w'))
# weak tie to signed?
print('weak targets signed-containing', sum(1 for k,v in weak.items() if any(x.startswith('S:') for x in clusters[v[0]])), 'distinct targets',len({v[0] for v in weak.values()}))
# anon-anon bursts among unattributed (same via, within 120s) -> sessions
un_s=sorted(un,key=lambda r:r['created_at'])
sess=[];cur=[]
for r in un_s:
    if r['id'] in weak: continue
    if cur and r['created_at']-cur[-1]['created_at']<=120 and r['via']==cur[-1]['via'] and r['via'] is not None: cur.append(r)
    else:
        if cur: sess.append(cur)
        cur=[r]
if cur: sess.append(cur)
print('remaining unattributed',sum(len(s) for s in sess),'burst sessions(>=2)',sum(1 for s in sess if len(s)>=2),'posts in bursts',sum(len(s) for s in sess if len(s)>=2))
