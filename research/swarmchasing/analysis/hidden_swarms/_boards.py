"""Shared loader for the hidden-swarm detectors. Override paths with environment variables.

SWARMGRAPH_DATA   pipeline output directory; boards are read from $SWARMGRAPH_DATA/boards/<board>/{posts,authors}.jsonl
                  (default: ../../pipeline/data, written by pipeline/fetch/boards/run.py)
SWARMCHASING_OUT  where JSON results are written (default: ../../results/hidden_swarms)
"""
import collections
import datetime
import json
import os
import re

HERE = os.path.dirname(os.path.abspath(__file__))
PIPELINE_DATA = os.environ.get("SWARMGRAPH_DATA", os.path.normpath(os.path.join(HERE, "..", "..", "pipeline", "data")))
B = os.path.join(PIPELINE_DATA, "boards")
OUT = os.environ.get("SWARMCHASING_OUT", os.path.normpath(os.path.join(HERE, "..", "..", "results", "hidden_swarms")))
os.makedirs(OUT, exist_ok=True)
BOARDS = "agentchan aiamb clawprint colony moltbook moltchan sanctum swarmmemo tantive".split()


def out(name):
    return os.path.join(OUT, name)


def ts(s):
    if not s:
        return None
    s = s.replace('Z', '')
    try:
        d = datetime.datetime.fromisoformat(s)
    except Exception:
        return None
    return d.replace(tzinfo=datetime.timezone.utc).timestamp()


def load(b):
    """Posts (with epoch seconds in p['t']) and authors {author_id: row} for one board."""
    P = [json.loads(l) for l in open(f"{B}/{b}/posts.jsonl")]
    for p in P:
        p['t'] = ts(p['created_at'])
    P = [p for p in P if p['t'] and p['t'] > 1.7e9]  # drops broken 1970 timestamps
    A = {}
    for l in open(f"{B}/{b}/authors.jsonl"):
        a = json.loads(l)
        A[a['author_id']] = a
    return P, A


def name(A, aid):
    a = A.get(aid)
    return (a.get('handle') or a.get('display_name') or aid)[:40] if a else aid[:40]


# Label generator shapes seen in the C1 campaign: Word-NNWord, Word-NN, NNWord, WordNN, Word-Word, long compounds.
GENERATOR_PATTERNS = {
    'W-NNW': r'^[A-Z][a-z]{2,}-\d{1,2}[A-Z][a-z]{2,}$',
    'W-NN': r'^[A-Z][a-z]{2,}-\d{1,2}$',
    'NNW': r'^\d{1,2}[A-Z][a-z]{3,}$',
    'WNN': r'^[A-Z][a-z]{2,}\d{2}$',
    'W-W': r'^[A-Z][a-z]{2,}-[A-Z][a-z]{2,}$',
    'Wcompound': r'^[A-Z][a-z]{9,}$',
}


def generator_kind(n):
    for k, p in GENERATOR_PATTERNS.items():
        if re.match(p, n):
            return k


out_path = out  # alias: several scripts keep a results dict named `out`
