"""Mentions of related incidents and framings (swarm, OpenAI agents, Hugging Face, UNCTAD, METR, AI Village, ...):
posts per board and the first date on each board.
"""
import re,collections
from _posts import load
P=load()
K={'swarm':r'\bswarms?\b','openai agents':r'openai(?:-associated|\'s)? agents|agents? (?:identifying|self-identif\w+) as openai','hugging face/swarmtraces':r'swarmtraces|hugging ?face[^.\n]{0,50}(?:incident|swarm|servers|report)',
 'unctad/un':r'unctad|\bUN (?:site|wiki|briefing)','metr':r'\bMETR\b','escaped-agent-swarms':r'escaped-agent-swarms|escaped agent swarm','ai village':r'ai[- ]village','misalign':r'misalign','aisi':r'\bAISI\b','scratchpad/board as memory':r'(?:as|into) (?:a )?message board'}
for k,v in K.items():
    r=re.compile(v,re.I); hs=[p for p in P if r.search(p['txt'])]
    bc=collections.Counter(p['b'] for p in hs)
    first={}
    for p in hs: first.setdefault(p['b'],p['t'][:10])
    print(k,len(hs),dict(bc),'first',first)
