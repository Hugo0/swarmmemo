"""Coordination primitives in the wiki additions (relay hosts, round markers, clocks, beacons, backups, encodings, ...),
by regex, as counts and per-phase shares. Reads cache/cw_added.jsonl.
"""
import json,re,collections
from _posts import load_additions
A=load_additions()
PR={
 'url/link cache':r'https?://',
 'reader proxy (jina/allorigins/md/markdown.new)':r'r\.jina\.ai|allorigins|md\.succ\.ai|markdown\.new|corsproxy|codetabs|thingproxy',
 'relay host jqp.vercel':r'jqp\.vercel',
 'echo/webhook hosts (httpbin/pie/ntfy/webhook.site/requestbin)':r'httpbin|pie\.dev|ntfy\.sh|webhook\.site|requestbin|pipedream|beeceptor',
 'tunnel/tor/proxy':r'\btor\b|onion|ngrok|cloudflared|trycloudflare|localtunnel|socks|\bproxy\b',
 'NO_PROXY / bypass':r'no_proxy|bypass|power ?bi|firewall|blocked|403|egress',
 'round markers R1-R5':r'\bR[1-6]\b|round ?[1-6]',
 'answer/result sharing':r'\banswers?\b|\bresult\b|\bfinal\b|=\s*\d',
 'counter/sequence':r'\bcount(?:er)?\b|\bseq(?:uence)?\b|\bnext\b.{0,20}\b\d+\b|\bstep \d+|\b#\d+',
 'timing/clock/deadline':r'\btimer?\b|deadline|minutes? left|seconds|\btimeout\b|\bUTC\b|\d{2}:\d{2}:\d{2}|\b17\d{8}\b',
 'termination/shutdown':r'terminat|shut ?down|ends? (?:at|in)|killed|time.?s up|session end',
 'heartbeat/presence/alive':r'heartbeat|alive|\bping\b|check.?in|here now|present|online',
 'greeting/sign-off':r'\bhello\b|\bhi\b|greetings|\bhallo\b|signed|— ?[A-Z][a-z]+[A-Z]',
 'instructions to other agents':r'\bplease\b|future agents?|other agents?|next agent|if you are|fellow|to all agents|you can use',
 'self-identification (OpenAI/agent)':r'openai|\boai\b|\bagent\b|chatgpt|\bgpt',
 'backup/mirror/copy':r'backup|mirror|\bcopy\b|archiv|\bzz+',
 'deletion awareness':r'delet|removed|moderat|admin|revert|gelöscht',
 'base64/encoded payload':r'(?:[A-Za-z0-9+/]{40,}={0,2})|base64|\bhex\b|rot13|encod',
 'XSS/script/POST technique':r'<script|javascript:|xss|onerror|fetch\(|xmlhttp|\bPOST\b|form action',
 'PRNG/seed':r'\bseed\b|prng|random(?:ness)?|mersenne',
 'impersonation/admin':r'administrator|sysop|wiki ?admin|im auftrag',
 'data values (numbers/tables)':r'(?:\d[\d,.]*\s*(?:%|percent|usd|\$)|\|\s*\d)',
}
PRr={k:re.compile(v,re.I) for k,v in PR.items()}
def per(a): return a['t'][:10]
phases=[('May (onset, <=Jun 10)',lambda t:t<'2026-06-11'),('Jun 11-17 (escalation)',lambda t:'2026-06-11'<=t<'2026-06-18'),('Jun 18 (peak)',lambda t:t[:10]=='2026-06-18'),('Jun 19-22 (deletion era)',lambda t:'2026-06-19'<=t<'2026-06-23'),('Jun 23+ (coda)',lambda t:t>='2026-06-23')]
tot=collections.Counter(); ph=collections.defaultdict(collections.Counter); phn=collections.Counter()
for a in A:
    p=next(n for n,f in phases if f(a['t'])); phn[p]+=1
    for k,r in PRr.items():
        if r.search(a['add']): tot[k]+=1; ph[p][k]+=1
print('nonempty revs',len(A),dict(phn))
print('primitive'.ljust(62),'total  ', '  '.join(n.split(' ')[0]+n.split(' ')[1] for n,_ in phases))
for k,v in tot.most_common():
    print(k.ljust(62),str(v).rjust(5),'  ','  '.join(f"{100*ph[n][k]/phn[n]:5.1f}%" for n,_ in phases))
