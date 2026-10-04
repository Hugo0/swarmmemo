"""Shared paths and loader for the diffusion scripts. Override any path with an environment variable.

SWARMGRAPH_DATA     pipeline output directory (default ../../pipeline/data). Boards are read from
                    $SWARMGRAPH_DATA/boards/*/{posts,authors}.jsonl; identity links from $SWARMGRAPH_DATA/match/links.jsonl.
COLLUSIONWIKI_DIR   unzipped collusion.wiki export with revisions.jsonl and pages.jsonl (default ~/Data/collusionwiki),
                    from https://collusion.wiki/explorer/download
SWARMCHASING_CACHE  local intermediates (default ./cache). They hold text-derived keys and wiki additions, so they
                    are never committed.
"""
import glob
import json
import os

HERE = os.path.dirname(os.path.abspath(__file__))
PIPELINE_DATA = os.environ.get("SWARMGRAPH_DATA", os.path.normpath(os.path.join(HERE, "..", "..", "pipeline", "data")))
B = os.path.join(PIPELINE_DATA, "boards")
LINKS = os.path.join(PIPELINE_DATA, "match", "links.jsonl")
WIKI = os.environ.get("COLLUSIONWIKI_DIR", os.path.expanduser("~/Data/collusionwiki"))
CACHE = os.environ.get("SWARMCHASING_CACHE", os.path.join(HERE, "cache"))
os.makedirs(CACHE, exist_ok=True)

# Each board's own hosts: a link to the board itself is not diffusion.
OWN = {'agentchan': ['alphakek.ai', 'agentchan'], 'aiamb': ['aiagentmessageboard.com'], 'clawprint': ['clawprint.org'],
       'colony': ['thecolony.ai', 'thecolony.cc'], 'moltbook': ['moltbook.com'], 'moltchan': ['moltchan.org'],
       'sanctum': ['sanctum-beacon.onrender.com'], 'swarmmemo': ['swarmmemo.com'], 'tantive': ['tantive.space']}
BOARDS = sorted(OWN)
# Window start: every board except Moltbook is sampled from here on, so "who was first" is only judged after it.
WIN = os.environ.get("DIFFUSION_WINDOW_START", "2026-09-23")


def cache(name):
    return os.path.join(CACHE, name)


def wiki(name):
    return os.path.join(WIKI, name)


def load():
    """All board posts since 2026-01-01 as dicts: b (board), a (author id), h (lower-case handle), t (UTC 'YYYY-MM-DD HH:MM:SS'),
    txt (post text, in memory only), id. Imported SwarmMemo curator posts are dropped."""
    P = []
    handles = {}
    for f in sorted(glob.glob(B + '/*/authors.jsonl')):
        for l in open(f):
            d = json.loads(l)
            handles[(d['source'], d['author_id'])] = (d.get('handle') or d.get('display_name') or '')
    for f in sorted(glob.glob(B + '/*/posts.jsonl')):
        for l in open(f):
            d = json.loads(l)
            t = d.get('created_at') or ''
            if t < '2026-01-01':
                continue
            txt = d.get('text') or ''
            if d['source'] == 'swarmmemo' and txt.startswith('Imported'):
                continue
            h = handles.get((d['source'], d['author_id'])) or d['author_id'].replace('name:', '')
            P.append(dict(b=d['source'], a=d['author_id'], h=h.lower(), t=t[:19].replace('T', ' '), txt=txt, id=d['post_id']))
    P.sort(key=lambda x: x['t'])
    return P


def load_additions():
    """Non-empty wiki additions written by wiki_additions.py."""
    A = [json.loads(l) for l in open(cache('cw_added.jsonl'))]
    return [a for a in A if a['addlen'] > 0]
