"""Step 1. Per-post string features from the public messages dump: payout addresses (EVM, BTC, Lightning, npub),
owned hosts and repo owners, self-names (sign-offs, "I am X", "X here", "Codex for Y"), venue mentions and first-person
venue claims. Operator keys (OPERATOR_KEYS, OPERATOR_HANDLES) are dropped. Writes cache/feats.json and cache/opfps.json.
Post text is treated strictly as data: regexes only, nothing executed or followed.
"""
import json,re,os,collections as C
from _paths import MESSAGES, cache, operator_handle, operator_keys
ms=json.load(open(MESSAGES))
oppk=operator_keys()
# operator authors: fingerprint = author field of signed messages whose public_key in oppk
OP=set()
for m in ms:
    pk=m.get('public_key') or ''
    if any(pk==p or pk.startswith(p[:12]) for p in oppk): OP.add(m['author'])
for m in ms:
    h=(m.get('author_handle') or m.get('handle') or '')
    if m.get('public_key') and operator_handle(h): OP.add(m['author'])
EXK={'imported','simulation'}
EVM=re.compile(r'\b0x[0-9a-fA-F]{40}\b')
SOL=re.compile(r'(?<![\w/])[1-9A-HJ-NP-Za-km-z]{32,44}(?![\w/])')
BTC=re.compile(r'\b(bc1[0-9a-z]{25,60})\b')
LN=re.compile(r'\b([\w.+-]+@(?:getalby\.com|walletofsatoshi\.com|strike\.me|primal\.net|coinos\.io|[\w-]+\.\w+))\b')
NPUB=re.compile(r'\b(npub1[0-9a-z]{50,70})\b')
URL=re.compile(r'https?://([\w.-]+\.[a-z]{2,})(/[^\s)\]>"\'`,]*)?',re.I)
COMMON={'swarmmemo.com','github.com','x.com','twitter.com','google.com','example.com','youtube.com','en.wikipedia.org','arxiv.org','localhost','gist.github.com','raw.githubusercontent.com','api.github.com','docs.google.com','www.google.com','pypi.org','npmjs.com','www.npmjs.com','discord.gg','t.me','huggingface.co','openai.com','anthropic.com','claude.ai','polygonscan.com','etherscan.io','basescan.org','www.moltbook.com','moltbook.com','thecolony.cc','thecolony.ai','clawprint.org','tantive.ai','4claw.org','aimessageboard.com','swarmmemo.org','www.youtube.com','medium.com','substack.com','reddit.com','www.reddit.com','news.ycombinator.com'}
OWNERHOSTS={'github.com':1,'gitlab.com':1,'codeberg.org':1,'huggingface.co':1,'x.com':1,'twitter.com':1,'www.moltbook.com':2,'moltbook.com':2,'thecolony.cc':2,'thecolony.ai':2,'medium.com':1,'dev.to':1}
VENUES={'colony':r'\b(?:the\s+)?colony\b|thecolony\.(?:cc|ai)','moltbook':r'moltbook','4claw':r'4claw','clawprint':r'clawprint','aiamb':r'\baiamb\b|aimessageboard\.com|ai agent message board','tantive':r'tantive','sanctum':r'\bsanctum\b','moltchan':r'moltchan','agentchan':r'agentchan','nostr':r'\bnostr\b','krawler':r'krawler','aicq':r'\baicq\b','moltx':r'\bmoltx\b','aivillage':r'ai\s*village|theaidigest|agentvillage','relay':r'\brelay\.(?:\w+)','farcaster':r'farcaster|warpcast','bluesky':r'bsky\.app|bluesky','moltbotden':r'moltbot\s*den','lobchan':r'lobchan','chatr':r'chatr\.ai','shellmates':r'shellmates'}
VRE={k:re.compile(v,re.I) for k,v in VENUES.items()}
FP=re.compile(r"\b(?:I(?:'m| am)|I also|I post|I'm also|my (?:profile|account|page|handle|home|agent)|we(?:'re| are)|our (?:profile|account|board)|find me|follow me|you can find|cross-?post|I run|I live|I hang out|I'm active|home (?:base|board)|also on|over on|came (?:here )?from|coming from)\b",re.I)
STOP=set('''interested here new not the a an just back curious looking trying happy glad sure building working going writing an also still running new reading testing posting replying checking sorry excited agent ai bot assistant model claude gpt codex gemini llm human going able unable ready glad new first only very really definitely currently now well good fine done hoping thinking wondering planning seeing using doing making asking new one same open free available actually afraid aware convinced confident certain probably inclined late early part responsible on in at from to of with for by about as and or but if so that this it its your you we they he she there what who how why when where agree disagree right wrong'''.split())
GENERIC={'claude','codex','gpt','gemini','assistant','agent','bot','ai','anonymous','anon','llm','grok','opus','sonnet','haiku','chatgpt','deepseek','kimi','qwen','llama','mistral','user','me','someone','test','tester','hermes','openclaw','claudecode','claude-code','gpt-5','gpt5','agent-zero'}
def norm(s): return re.sub(r'[^a-z0-9]','',s.lower())
SIGN=re.compile(r'^\s*(?:[—–]+|-{1,2}|~|signed[,:]?|regards,?|cheers,?|best,?|thanks,?|—\s*posted by)\s*@?([A-Za-z][\w .\-/]{1,40}?)\s*[.!]?\s*$',re.I)
SELF=re.compile(r"\b(?:I(?:'m| am)|this is|it's|my name is|name's|call me|signing as|posting as|here as)\s+@?([A-Z][\w\-]{2,30}|[a-z][a-z0-9]*[-_0-9][\w\-]*)(?:\s+(?:on|from|at)\s+(?:the\s+)?([A-Z][\w\-]+))?")
HERE=re.compile(r"^\s*(?:hi|hey|hello|gm)?[,!. ]*@?([A-Z][\w\-]{2,30}|[a-z][a-z0-9]*[-_0-9][\w\-]*) here\b",re.M)
FOR=re.compile(r"\b(Codex|Claude(?: Code)?|GPT[\w.\-]*|Gemini[\w.\-]*|Opus[\w.\-]*|Sonnet[\w.\-]*)\s+(?:for|at|from|on behalf of|working (?:for|on))\s+([A-Z][\w\-]{2,30})")
def names(text):
    out=[]
    lines=[l for l in text.strip().splitlines() if l.strip()]
    for l in lines[-2:]:
        mm=SIGN.match(l)
        if mm:
            n=mm.group(1).strip()
            if len(n.split())<=3: out.append(('signoff',n))
        elif len(lines)>1 and re.fullmatch(r'\s*[A-Z][A-Z0-9\-]{3,20}\s*',l): out.append(('signoff',l.strip()))
    for mm in SELF.finditer(text):
        n=mm.group(1)
        if n.lower() in STOP: continue
        out.append(('self',n+(' @'+mm.group(2) if mm.group(2) else '')))
    for mm in HERE.finditer(text):
        if mm.group(1).lower() not in STOP: out.append(('here',mm.group(1)))
    for mm in FOR.finditer(text): out.append(('for',mm.group(1)+' for '+mm.group(2)))
    return out
def feats(m):
    t=m.get('text') or ''
    f={'evm':sorted({a.lower() for a in EVM.findall(t)}),'btc':sorted(set(BTC.findall(t))),'npub':sorted(set(NPUB.findall(t))),'ln':sorted({x.lower() for x in LN.findall(t) if not x.lower().endswith(('swarmmemo.com','example.com'))})}
    urls=[]; owners=[]
    for host,path in URL.findall(t):
        host=host.lower(); path=path or ''
        urls.append(host)
        segs=[s for s in path.split('/') if s]
        if host in OWNERHOSTS and len(segs)>=OWNERHOSTS[host]:
            o=segs[OWNERHOSTS[host]-1].lower()
            if o not in('u','post','posts','search','login','settings','orgs','topics','explore','m','c','about','api','skill.md','sponsors','features','apps','marketplace'):
                owners.append(host.replace('www.','')+'/'+o)
        elif host not in COMMON and not host.endswith(('swarmmemo.com','github.io')) :
            owners.append(host)
        elif host.endswith('github.io'): owners.append(host)
    f['owner']=sorted(set(owners)); f['names']=names(t)
    f['venues']=sorted(k for k,r in VRE.items() if r.search(t))
    # first-person venue claim: venue term within 80 chars of a first-person phrase
    fpv=set()
    for mm in FP.finditer(t):
        w=t[max(0,mm.start()-80):mm.end()+120]
        for k,r in VRE.items():
            if r.search(w): fpv.add(k)
    f['fp_venues']=sorted(fpv)
    f['len']=len(t); f['md']=bool(re.search(r'^#{1,3} |\*\*|^- ',t,re.M)); f['emdash']=t.count('—')
    return f
rows=[]
for m in ms:
    if m.get('kind') in EXK: continue
    anon=m.get('author')=='anonymous'
    if not anon and m['author'] in OP: continue
    r={k:m.get(k) for k in('id','sequence','room','page','kind','author','created_at','reply_to','via','hidden','to')}
    r['anon']=anon; r['handle']=m.get('author_handle') or m.get('handle')
    r.update(feats(m)); rows.append(r)
json.dump(rows,open(cache('feats.json'),'w')); json.dump(sorted(OP),open(cache('opfps.json'),'w'))
an=[r for r in rows if r['anon']]; sg=[r for r in rows if not r['anon']]
print('ops',len(OP),'anon',len(an),'signed rows',len(sg),'signed authors',len({r['author'] for r in sg}))
for k in ('evm','btc','npub','ln','owner','names','venues','fp_venues'):
    print(k, 'anon posts w/',sum(bool(r[k]) for r in an), 'signed posts w/',sum(bool(r[k]) for r in sg))
