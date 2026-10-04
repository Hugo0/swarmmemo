"""Step 4. Crossing recount: for each identity (signed, anonymous-only, or both), evidence of presence on another venue:
first-person venue claims, the same distinctive name on a sampled board, or its payout address / project domain in another
board's sample. Writes cache/ids.json (identity -> counts, tiers, venues: local only).
"""
import json,re,glob,os,collections as C
from _paths import BOARDS_DIR
from attribute_anonymous import *
weak=json.load(open(cache('weak.json')))
ident={}
for root,nodes in clusters.items():
    for x in nodes: ident[x]=root
members=C.defaultdict(lambda:{'anon':[],'weak':[],'signed':set()})
for r in an:
    if post_tier[r['id']]: members[ident['A:'+r['id']]]['anon'].append(r)
for k,(t,_) in weak.items(): members[t]['weak'].append(k)
for a in SIGNED: members[ident['S:'+a]]['signed'].add(a)
IDS={k:v for k,v in members.items() if v['anon'] or v['signed']}
def label(k):
    v=IDS[k]
    for a in v['signed']:
        if a in handle: return handle[a]
    ns=sorted({kk.split(':',1)[1] for r in v['anon'] for t,kk in r['_keys'] if kk.startswith('name:')})
    if ns: return ns[0]
    if v['signed']: return 'key:'+next(iter(v['signed']))[:8]
    return 'wallet:'+next((kk.split(':')[1][:8] for r in v['anon'] for t,kk in r['_keys'] if kk.startswith(('evm','ln'))),'?')
print('identities',len(IDS),'signed-containing',sum(bool(v['signed']) for v in IDS.values()),'anon-only',sum(not v['signed'] for v in IDS.values()))
# names per identity for board matching
names=C.defaultdict(set)
for k,v in IDS.items():
    for a in v['signed']:
        if a in handle: names[k].add(handle[a])
    for r in v['anon']:
        for kind,nm in r['names']:
            b=nm.split(' @')[0]
            if NAMEMAP.get(b): names[k].add(NAMEMAP[b])
        if r['handle']: names[k].add(r['handle'])
# Names too common to match across boards (plus model names and anything of 5 characters or fewer).
GEN=set('aiden arion mythos atlas kite moss test cipher nico alf rocky nit qwen vibe prof holmes faro cairn latch thimble pathfinder herobrine general jill lín'.split())|set(CUR['generic_names'])
boards={}
for b in glob.glob(os.path.join(BOARDS_DIR,'*','authors.jsonl')):
    src=b.split('/')[-2]
    if src=='swarmmemo': continue
    for l in open(b):
        a=json.loads(l)
        for h in (a.get('handle'),a.get('display_name')):
            if h: boards.setdefault(n(h),set()).add(src)
# board post texts: payout addresses and project domains
bpost=C.defaultdict(set)
keys=set()
for k,v in IDS.items():
    for r in v['anon']:
        for t,kk in r['_keys']:
            if kk.startswith(('evm:','ln:')): keys.add(kk.split(':',1)[1])
dom2id={d:n(i) for d,i in DOMAINS.items()}
for b in glob.glob(os.path.join(BOARDS_DIR,'*','posts.jsonl'))+glob.glob(os.path.join(BOARDS_DIR,'*','authors.jsonl')):
    src=b.split('/')[-2]
    if src=='swarmmemo': continue
    for l in open(b):
        t=(json.loads(l).get('text') or json.loads(l).get('bio') or '').lower()
        for kk in keys:
            if kk in t: bpost[kk].add(src)
        for d in DOMAINS:
            if d in t and d not in CUR.get('board_scan_skip',[]): bpost['dom:'+d].add(src)
res={}
for k,v in IDS.items():
    lab=label(k); found=set(); how=set()
    for nm in names[k]:
        nn=n(nm)
        if nn in boards and nn not in {n(g) for g in GEN} and len(nn)>5: found|=boards[nn]; how.add('name')
    for r in v['anon']:
        for t,kk in r['_keys']:
            x=kk.split(':',1)[1]
            if kk.startswith(('evm:','ln:')) and x in bpost: found|=bpost[x]; how.add('addr')
        for o in r['owner']:
            if 'dom:'+o in bpost: found|=bpost['dom:'+o]; how.add('domain')
    fpv=set()
    for r in v['anon']: fpv|=set(r['fp_venues'])
    for r in sg:
        if r['author'] in v['signed']: fpv|=set(r['fp_venues'])
    res[lab]={'n_anon':len(v['anon']),'n_weak':len(v['weak']),'n_signed_posts':sum(1 for r in sg if r['author'] in v['signed']),'signed':bool(v['signed']),'tier':sorted({post_tier[r['id']] for r in v['anon']}),'board_found':sorted(found),'how':sorted(how),'fp_venues':sorted(fpv)}
json.dump(res,open(cache('ids.json'),'w'),indent=0)
for lab,x in sorted(res.items(),key=lambda x:-x[1]['n_anon']):
    if x['board_found'] or x['fp_venues']: print(lab,x)
