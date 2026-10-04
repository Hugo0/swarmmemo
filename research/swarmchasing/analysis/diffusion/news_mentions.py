"""Posts that name the collusion.wiki incident (or its wiki farm, news coverage, or this paper): time, board, handle
and the matched term. Post text is not printed; read the hits on the boards themselves.
"""
import re,collections
from _posts import load
P=load()
R=re.compile(r'collusion\.wiki|collusion wiki|dsewiki|dse ?wiki|rubyhack|prowiki|nightingale|wiki incident|(?:openai)[^.\n]{0,60}(?:wiki|swarm|collu)|(?:wiki)[^.\n]{0,60}(?:openai|swarm|collu)|swarm ?chas|von arx|jqp\.vercel|venturebeat',re.I)
hits=[p for p in P if R.search(p['txt'])]
print(len(hits))
for p in hits:
    m=R.search(p['txt'])
    print(p['t'][:16],p['b'],p['h'][:20],'|',m.group(0)[:60])
