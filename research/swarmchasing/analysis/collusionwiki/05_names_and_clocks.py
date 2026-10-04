#!/usr/bin/env python3
"""Label naming (CamelCase, tokens, digit templates), dates in names vs real write dates, /16 networks per label, Unix-time page names vs save time.

Findings: F7, F8, F9 (findings/swarm-findings.md). Standard library only; prints to stdout.
Inputs: see _paths.py. No wiki text is printed or written.
"""
import os, sys
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _paths import wiki, cache, PIPELINE_DATA, RESULTS  # noqa: E402
import json,pickle,re,math,statistics as st
from collections import Counter,defaultdict
from datetime import datetime,timezone
revs,pages,dels=pickle.load(open(cache('cache.pkl'),'rb'))
bl,bp,first=pickle.load(open(cache('c2.pkl'),'rb'))
labs=list(bl)
def H(c):
    t=sum(c.values()); return -sum(x/t*math.log2(x/t) for x in c.values())
# naming
f=Counter()
for l in labs:
    f['camel']+=bool(re.fullmatch(r'([A-Z][a-z0-9]+)+[A-Za-z0-9]*',l))
    f['has_digit']+=bool(re.search(r'\d',l)); f['ends_digit']+=bool(re.search(r'\d$',l))
    f['openai']+=bool(re.search(r'(?i)openai|oai',l)); f['month']+=bool(re.search(r'(Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)\d{1,2}',l))
    f['space_or_sep']+=bool(re.search(r'[ _\-.]',l)); f['nonascii']+=bool(re.search(r'[^\x00-\x7f]',l))
    f['model_hint']+=bool(re.search(r'(?i)gpt|claude|gemini|llama|codex|o3|o4',l))
print('labels',len(labs),f)
L=[len(l) for l in labs]; print('length median',st.median(L))
# token vocab
tok=Counter(); stems=Counter()
for l in labs:
    ts=re.findall(r'[A-Z]+(?=[A-Z][a-z]|\d|$)|[A-Z]?[a-z]+|\d+',l); tok.update(set(t.lower() for t in ts if not t.isdigit()))
    stems[re.sub(r'\d+','#',l)]+=1
print('distinct tokens',len(tok),'top',tok.most_common(15))
print('label templates (digits->#): distinct',len(stems),'labels sharing a template with >=1 other',sum(n for n in stems.values() if n>1),'top',stems.most_common(6))
# month-day hints vs actual date
md=[(l,re.search(r'(Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)(\d{1,2})',l)) for l in labs]
ok=0;n=0
for l,m in md:
    if not m: continue
    n+=1; d=datetime.fromtimestamp(bl[l][0]['t'],timezone.utc)
    ok+= (m.group(1)==d.strftime('%b') and int(m.group(2))==d.day)
print('labels with MonDD',n,'matching actual write date',ok)
mon=Counter(m.group(1) for l,m in md if m); print('months named',mon.most_common(12))
# char entropy of labels
print('char entropy bits/char',H(Counter(''.join(labs))))
# ip16 per label: labels writing from >1 /16
ipc=[len({r['ip'] for r in v}) for v in bl.values() if len(v)>1]; print('multi-save labels',len(ipc),'from >1 /16',sum(x>1 for x in ipc))
# timestamp page names vs save time
off=[]
for p in pages.values():
    m=re.search(r'(17[78]\d{7})',p['name'])
    if m and p['page_key'] in bp:
        off.append(bp[p['page_key']][0]['t']-int(m.group(1)))
off.sort(); print('ts-named pages',len(off),'median offset s',st.median(off),'|off|<=60',sum(abs(x)<=60 for x in off),'<=600',sum(abs(x)<=600 for x in off))
# reader-proxy/relay hosts over days (first appearance per host category) - reuse INFRA list loosely
hosts=['jqp.vercel','r.jina.ai','allorigins','md.succ.ai','markdown.new','pinggy','serveo','localhost.run','loca.lt','ntfy.sh','httpbin','pie.dev','webhook.site']
