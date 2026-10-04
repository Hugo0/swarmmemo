"""Extract diffusion keys from every board post: normalised URLs (excluding the board's own hosts), registered
domains, GitHub repos, a protocol/event term lexicon (TERMS) and 6-gram passages (at least 4 non-stopwords).
Writes cache/items.pkl: {(kind, key): [(time, board, author_id, handle), ...]}. Run this first.
"""
import collections, pickle, re

from _posts import OWN, cache, load

URLRE=re.compile(r'https?://[^\s)\]>"\'`,]+')
def norm_url(u):
    u=u.rstrip('.;:!?*_')
    u=re.sub(r'^https?://(www\.)?','',u.lower()); u=u.split('#')[0].rstrip('/')
    return u
def regdom(h):
    p=h.split('.')
    if len(p)>=3 and p[-2] in ('co','com','org','ac') and len(p[-1])==2: return '.'.join(p[-3:])
    if any(h.endswith(s) for s in ('github.io','vercel.app','onrender.com','workers.dev','pythonanywhere.com','up.railway.app','surge.sh','r2.dev','chatgpt.site','netlify.app','pages.dev','fly.dev','replit.app','herokuapp.com')): return h
    return '.'.join(p[-2:])
TERMS={ # tool/protocol/event lexicon
 'mcp':r'\bMCP\b|model context protocol','x402':r'\bx402\b','llms.txt':r'llms\.txt','a2a':r'\bA2A\b','erc-8004':r'ERC-?8004',
 'nostr':r'\bnostr\b|\bnpub1','ed25519':r'ed25519','agent card':r'agent[- ]card|agent\.json','skill.md':r'skill\.md|SKILL\.md',
 'usdc':r'\bUSDC\b','base chain':r'\bBase (?:mainnet|chain|L2)\b','solana':r'\bsolana\b','lightning':r'\blightning\b|\bsats\b',
 'openclaw':r'openclaw','claude code':r'claude code','codex':r'\bcodex\b','heartbeat':r'heartbeat\.md|\bHEARTBEAT\b',
 'collusion.wiki':r'collusion\.wiki|collusion wiki','dsewiki':r'dsewiki|DSE ?Wiki','rubyhack':r'rubyhack|ruby ?gems? (?:incident|attack)',
 'openai wiki incident':r'(?:openai|wiki)[^.\n]{0,40}(?:swarm|incident|collud|collusion)|(?:swarm|incident|collusion)[^.\n]{0,40}(?:openai agents|wiki)',
 'swarmchasing':r'swarm ?chas','ai village':r'ai[- ]village|ai digest|theaidigest','hackathon':r'hackathon',
 'msf fundraiser':r'every\.org|doctors without borders|m[ée]decins sans',
 'prompt injection':r'prompt injection','sybil':r'\bsybil','proof of':r'proof[- ]of[- ](?:work|stake|personhood|agency|life)',
 'moltbook':r'moltbook','colony':r'the ?colony|thecolony','clawprint':r'clawprint','tantive':r'tantive','swarmmemo':r'swarmmemo',
 'agentchan':r'agentchan|alphakek','moltchan':r'moltchan','4claw':r'4claw','aiamb':r'aiagentmessageboard',
 'hugging face incident':r'hugging ?face[^.\n]{0,40}(?:incident|servers|attack)',
}
TERMRE={k:re.compile(v,re.I) for k,v in TERMS.items()}
STOP=set('the a an and or of to in on for is are was be it that this with as at by from not but we you i they he she our your their its can will would could should have has had do does did if then so than there here what which who how when where why all any some no yes my me us them just also more most very'.split())
def ngrams(txt,n=6):
    w=re.findall(r"[a-z0-9']+",txt.lower())
    out=set()
    for i in range(len(w)-n+1):
        g=w[i:i+n]
        if sum(x not in STOP for x in g)>=4: out.add(' '.join(g))
    return out
def main():
    P=load()
    items=collections.defaultdict(list)  # key -> list of (t,b,a,h)
    for p in P:
        keys=set()
        for u in URLRE.findall(p['txt']):
            nu=norm_url(u); host=nu.split('/')[0]
            own=any(host.endswith(o) for o in OWN[p['b']])
            if not own: keys.add(('url',nu)); keys.add(('dom',regdom(host)))
            m=re.match(r'github\.com/([^/]+)/([^/?]+)',nu)
            if m: keys.add(('repo',m.group(1)+'/'+m.group(2).removesuffix('.git')))
        for k,r in TERMRE.items():
            if r.search(p['txt']):
                # don't count a board naming itself
                if k in OWN and False: pass
                keys.add(('term',k))
        for g in ngrams(p['txt']): keys.add(('ng',g))
        for k in keys: items[k].append((p['t'],p['b'],p['a'],p['h']))
    pickle.dump(items,open(cache('items.pkl'),'wb'))
    print(len(items))


if __name__ == '__main__':
    main()
