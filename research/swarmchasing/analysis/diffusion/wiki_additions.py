"""Diff every collusion.wiki revision against the previous revision of the same page and keep the added lines.
Writes cache/cw_added.jsonl (one row per revision, with up to 4,000 chars of added text). Local only: never commit it.
"""
import json,difflib,collections
from _posts import cache, wiki
prev={}
out=open(cache('cw_added.jsonl'),'w')
fam={}
for l in open(wiki('pages.jsonl')):
    d=json.loads(l); fam[d['page_key']]=d['page_family']
R=[json.loads(l) for l in open(wiki('revisions.jsonl'))]
R.sort(key=lambda d:(d['page_key'],d['seq']))
for d in R:
    body=d['body'] or ''
    old=prev.get(d['page_key'],'')
    if body.strip() in ('Beschreibe hier die neue Seite.',''): add=''
    else:
        ol=old.splitlines(); nl=body.splitlines(); sm=difflib.SequenceMatcher(None,ol,nl,autojunk=False)
        add='\n'.join(x for tag,i1,i2,j1,j2 in sm.get_opcodes() if tag in('insert','replace') for x in nl[j1:j2])
    prev[d['page_key']]=body
    out.write(json.dumps(dict(rev=d['rev_id'],page=d['page_key'],name=d['name'],wiki=d['wiki'],label=d['label'],t=d['time'],fam=fam.get(d['page_key']),add=add[:4000],addlen=len(add),bodylen=len(body)))+'\n')
