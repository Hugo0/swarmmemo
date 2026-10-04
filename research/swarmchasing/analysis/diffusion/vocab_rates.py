"""Rates of specific words and covert-channel terms (relay, round, heartbeat, proxy, GET write, dead drop, ...) among
unique wiki additions and board posts.
"""
import json,re,collections,math
from _posts import load, load_additions
P=load()
A=load_additions()
seen=set(); U=[]
for a in A:
    if a['add'] not in seen: seen.add(a['add']); U.append(a['add'])
tok=lambda s: set(re.findall(r"\b[a-z][a-z\-]{3,}\b",re.sub(r'https?://\S+',' ',s.lower())))
cw=collections.Counter(); bd=collections.Counter()
for u in U: cw.update(tok(u))
for p in P: bd.update(tok(p['txt']))
NA,NB=len(U),len(P)
lo=lambda w: math.log((cw[w]+.5)/NA)-math.log((bd[w]+.5)/NB)
cand=[w for w in cw if cw[w]>=30]
cov=sorted(cand,key=lo,reverse=True)
print('top covert words',[(w,cw[w],bd[w]) for w in cov[:70]])
bov=sorted([w for w in bd if bd[w]>=100],key=lo)
print('top board words absent in covert',[(w,bd[w],cw[w]) for w in bov[:50]])
# specific covert terms on boards
for term in ['jqp','jina','allorigins','succ.ai','markdown.new','no_proxy','power bi','wikiservice','dsewiki','prowiki','zzz','task-clock','heartbeat','relay','counter','round','r1','backup','mirror','base64','ngrok','tor ','proxy','get request','get write','write via get','xss','dead drop','dead-drop','stigmerg','scratchpad','shared memory']:
    r=re.compile(re.escape(term),re.I)
    b=sum(1 for p in P if r.search(p['txt'])); c=sum(1 for u in U if r.search(u))
    print(f'{term:15s} covert_uniq_adds {c:5d} ({100*c/NA:4.1f}%)  board_posts {b:5d} ({100*b/NB:4.2f}%)')
