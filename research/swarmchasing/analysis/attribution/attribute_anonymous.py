"""Step 2. Attribute anonymous posts to agent identities in tiers, by union-find over shared keys:
  certain  payout address in a payout context, or the post's own handle field;
  likely   a curated self-name (name_map) or exactly one curated own-project domain (domain_map).
Signed keys join the same graph through their handle, payout addresses and sign-offs. Reads cache/feats.json, the
messages dump, profiles and CURATION. Run directly for the summary; writes cache/attr1.json (post ids and tiers: local only).
"""
import json,re,collections as C,random,math
from _paths import MESSAGES, OPERATOR_ROOMS, PROFILES, cache, curation
F=json.load(open(cache('feats.json'))); M={m['id']:m for m in json.load(open(MESSAGES))}
OPS=set(json.load(open(cache('opfps.json'))))
P=json.load(open(PROFILES))
CUR=curation()
EXCLUDE={a.lower() for a in CUR['exclude_addresses']}
# Curated tables (see CURATION in _paths.py): self-name -> identity, operator session names, own-project domain -> identity.
NAMEMAP=CUR['name_map']
OPERATOR_NAMES=set(CUR['operator_names'])
DOMAINS=CUR['domain_map']
PAYCTX=re.compile(r'(payout|pay ?to|payto|recipient|can go to|sent to|send to|wallet|payment after acceptance to|receiver)[^\n]{0,25}$',re.I)
def paykeys(r):
    t=M[r['id']]['text']; out=set()
    for a in r['evm']:
        if a in EXCLUDE: continue
        j=t.lower().find(a)
        if PAYCTX.search(t[max(0,j-60):j]): out.add('evm:'+a)
    for l in r['ln']:
        if not l.startswith('lodash'): out.add('ln:'+l)
    return out
# union-find
par={}
def f(x):
    par.setdefault(x,x)
    while par[x]!=x: par[x]=par[par[x]]; x=par[x]
    return x
def u(a,b): par[f(a)]=f(b)
an=[r for r in F if r['anon'] and r['room'] not in OPERATOR_ROOMS]
sg=[r for r in F if not r['anon']]
SIGNED={r['author'] for r in sg}
handle={}
for r in sg:
    if r['handle']: handle[r['author']]=r['handle']
for a in SIGNED:
    p=P.get(a,{}).get('agent') or {}
    if p.get('handle'): handle.setdefault(a,p['handle'])
def n(s): return re.sub(r'[^a-z0-9]','',s.lower())
byname={n(h):'S:'+a for a,h in handle.items()}
# signed self-sign-offs
SIGNOFF_SIGNED=CUR['signoff_signed']
for nm,pre in SIGNOFF_SIGNED.items():
    for a in SIGNED:
        if a.startswith(pre): byname.setdefault(n(nm),'S:'+a)
tier={}  # anon id -> tier
keyof=C.defaultdict(set)   # key -> nodes
excluded_op=set()
for r in an:
    node='A:'+r['id']; f(node)
    ks=set()
    if r['handle']: ks.add(('certain','name:'+n(r['handle'])))
    for k in paykeys(r): ks.add(('certain',k))
    for kind,nm in r['names']:
        base=nm.split(' @')[0]
        if base in OPERATOR_NAMES: excluded_op.add(r['id'])
        c=NAMEMAP.get(base)
        if c: ks.add(('likely','name:'+n(c)))
    ds={DOMAINS[o] for o in r['owner'] if o in DOMAINS}
    if len(ds)==1: ks.add(('likely','name:'+n(ds.pop())))
    r['_keys']=ks
for r in sg:
    node='S:'+r['author']; f(node)
    ks=set(('certain',k) for k in paykeys(r))
    if r['author'] in handle: ks.add(('certain','name:'+n(handle[r['author']])))
    for kind,nm in r['names']:
        if nm in SIGNOFF_SIGNED: ks.add(('certain','name:'+n(nm)))
    r['_keys']=ks
# domain keys on signed side: only if exactly one signed agent uses it
dom_signed=C.defaultdict(set)
for r in sg:
    for o in r['owner']:
        if o in DOMAINS and len({DOMAINS[x] for x in r['owner'] if x in DOMAINS})==1: dom_signed[DOMAINS[o]].add(r['author'])
for d,s in dom_signed.items():
    if len(s)==1: keyof['name:'+n(d)].add('S:'+next(iter(s)))
an=[r for r in an if r['id'] not in excluded_op]
for r in an+sg:
    node=('A:'+r['id']) if r['anon'] else 'S:'+r['author']
    for t,k in r['_keys']: keyof[k].add(node)
for k,nodes in keyof.items():
    nodes=list(nodes)
    for x in nodes[1:]: u(nodes[0],x)
# per-post tier: certain if a certain key that is shared with another node OR handle field; likely if any curated key; singleton explicit wallet => certain (identity = wallet)
post_tier={}
for r in an:
    ts={t for t,k in r['_keys']}
    post_tier[r['id']]='certain' if 'certain' in ts else ('likely' if 'likely' in ts else None)
clusters=C.defaultdict(list)
for r in an+[{'anon':False,'author':a} for a in SIGNED]:
    node=('A:'+r['id']) if r['anon'] else 'S:'+r['author']
    clusters[f(node)].append(node)

if __name__ == '__main__':
    json.dump({'post_tier':post_tier,'clusters':{k:v for k,v in clusters.items()},'excluded_op':sorted(excluded_op)},open(cache('attr1.json'),'w'))
    # summary
    att=[r for r in an if post_tier[r['id']]]
    print('anon considered',len(an),'(operator-excluded',len(excluded_op),')')
    print('attributed by explicit/self-ID',len(att),C.Counter(post_tier[r['id']] for r in att))
    cl=C.defaultdict(lambda:{'anon':0,'signed':[],'tier':set()})
    for root,nodes in clusters.items():
        for x in nodes:
            if x.startswith('A:'):
                if post_tier.get(x[2:]): cl[root]['anon']+=1; cl[root]['tier'].add(post_tier[x[2:]])
            else: cl[root]['signed'].append(x[2:])
    ident=[(k,v) for k,v in cl.items() if v['anon']]
    print('distinct identities with attributed anon posts',len(ident),'also signed',sum(bool(v['signed']) for k,v in ident))
    for k,v in sorted(ident,key=lambda x:-x[1]['anon']):
        lab=[handle.get(s,s[:8]) for s in v['signed']]
        names=sorted({kk.split(':',1)[1] for r in an if 'A:'+r['id'] in clusters[k] for t,kk in r['_keys'] if kk.startswith('name:')})
        print(v['anon'],sorted(v['tier']),lab,names[:3])
