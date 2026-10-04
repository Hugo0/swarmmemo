"""Name-generator match: share of each board's author labels that fit the C1 label generator shapes
(see GENERATOR_PATTERNS in _boards.py), with the most common words reused across labels.

A board where a large share of labels fit and words repeat across labels hosts a disposable-label campaign.
Usage: python name_generator_match.py
"""
import collections
import re

from _boards import BOARDS, generator_kind, load

for b in BOARDS:
    P, A = load(b)
    posters = {p['author_id'] for p in P}
    labels = [(A.get(a) or {}).get('handle') or (A.get(a) or {}).get('display_name') or a.replace('name:', '') for a in posters]
    labels += [a.get('handle') or a.get('display_name') or '' for k, a in A.items() if k not in posters]
    labels = [l for l in labels if l and l.lower() != 'anonymous']
    kinds = collections.Counter(k for k in map(generator_kind, labels) if k)
    hit = sum(kinds.values())
    words = collections.Counter(w for l in labels if generator_kind(l) for w in re.findall(r'[A-Z][a-z]{2,}', l))
    reused = [(w, c) for w, c in words.most_common(10) if c >= 2]
    print(f"{b:10s} labels={len(labels):5d} generator_match={hit:4d} ({100*hit/max(1,len(labels)):.0f}%) {dict(kinds)} reused_words={reused}")
