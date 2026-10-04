"""Covert (wiki additions) against open (board posts) vocabulary: log-odds of document frequency, shared words with
similar rates, and hosts that appear in both. Writes cache/vocab.json.
"""
import json,re,collections,math
from _posts import cache, load, load_additions
P=load()
A=load_additions()
tok=lambda s: re.findall(r"[a-z][a-z0-9_\-]{2,}",s.lower())
cw=collections.Counter(); bd=collections.Counter()
cwdoc=collections.Counter(); bddoc=collections.Counter()
for a in A:
    t=set(tok(re.sub(r'https?://\S+',' ',a['add']))); cwdoc.update(t)
for p in P:
    t=set(tok(re.sub(r'https?://\S+',' ',p['txt']))); bddoc.update(t)
NA,NB=len(A),len(P)
# log-odds with prior
def lo(w):
    a=cwdoc[w]+0.5; b=bddoc[w]+0.5
    return math.log(a/NA)-math.log(b/NB)
cand=[w for w in cwdoc if cwdoc[w]>=40]
cov=sorted(cand,key=lo,reverse=True)
print('covert-distinctive (doc freq cw / boards):')
print([(w,cwdoc[w],bddoc[w]) for w in cov[:80]])
shared=[w for w in cand if bddoc[w]>=20 and abs(lo(w))<0.7]
print('shared, similar rate:',sorted(shared,key=lambda w:-cwdoc[w])[:60])
# hosts
def hosts(s): return set(h.lower().removeprefix('www.') for h in re.findall(r'https?://([A-Za-z0-9.-]+)',s))
ch=collections.Counter(); 
for a in A: ch.update(hosts(a['add']))
bh=collections.Counter()
for p in P: bh.update(hosts(p['txt']))
print('covert hosts n',len(ch))
ov=[(h,ch[h],bh[h]) for h in ch if bh[h]>0]
print('overlap hosts',len(ov),sorted(ov,key=lambda x:-x[1])[:60])
json.dump(dict(cov=[(w,cwdoc[w],bddoc[w]) for w in cov[:200]],ov=ov),open(cache('vocab.json'),'w'))
