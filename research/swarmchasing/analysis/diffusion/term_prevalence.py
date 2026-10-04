"""Protocol and event vocabulary per board (posts per 1,000 in the window, first-ever sighting) and the board-naming
matrix (posts on board X naming board Y, per 1,000). Uses the TERMS lexicon from extract_items.py.
"""
import re,collections
from _posts import BOARDS, WIN, load
from extract_items import TERMS
P=load()
TR={k:re.compile(v,re.I) for k,v in TERMS.items()}
BO=BOARDS
W=WIN
n=collections.Counter(p['b'] for p in P if p['t']>=W)
print('term'.ljust(22),'first-ever (board,date)'.ljust(28),' '.join(b[:6].rjust(6) for b in BO),'  [per-1k posts, window>=Sep23]')
for k,r in TR.items():
    first=None; c=collections.Counter()
    for p in P:
        if r.search(p['txt']):
            if first is None: first=(p['b'],p['t'][:10])
            if p['t']>=W: c[p['b']]+=1
    print(k.ljust(22),str(first).ljust(28),' '.join(f"{1000*c[b]/n[b]:6.0f}" for b in BO))
# board-mention matrix: board X posts naming board Y
names={'agentchan':r'agentchan|alphakek','aiamb':r'aiagentmessageboard|ai agent message board','clawprint':r'clawprint','colony':r'the ?colony|thecolony','moltbook':r'moltbook','moltchan':r'moltchan','sanctum':r'sanctum','swarmmemo':r'swarmmemo','tantive':r'tantive'}
NR={k:re.compile(v,re.I) for k,v in names.items()}
print('\nmention matrix (row board posts naming col board, per 1k, window)')
print('      ',' '.join(b[:6].rjust(6) for b in BO))
for a in BO:
    c=collections.Counter()
    for p in P:
        if p['b']==a and p['t']>=W:
            for b,r in NR.items():
                if r.search(p['txt']): c[b]+=1
    print(a[:6].ljust(6),' '.join(f"{1000*c[b]/n[a]:6.0f}" for b in BO))
