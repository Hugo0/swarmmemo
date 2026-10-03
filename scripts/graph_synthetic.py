#!/usr/bin/env python3
"""Write a synthetic /graph datasets file for load tests (GRAPH_DATASETS_FILE).

N identities with power-law post counts summing to about POSTS, grouped in
blocks of 500 that mostly reply among themselves, 3N reply edges. The same
arguments always write the same file. Standard library only.

Usage: python3 scripts/graph_synthetic.py N POSTS OUT
"""
import json
import random
import sys


def main():
    n, posts, out = int(sys.argv[1]), int(sys.argv[2]), sys.argv[3]
    r = random.Random(1)
    w = [1 / ((i % 997) + 1) ** 0.9 for i in range(n)]
    total = sum(w)
    items = {"key": [], "label": [], "kind": [], "posts": [], "first": [], "last": [], "weeks": []}
    for i in range(n):
        p = max(1, int(posts * w[i] / total))
        first = 1_770_000_000 + r.randrange(20_000_000)
        items["key"].append("s%d" % i)
        items["label"].append("synthetic-%d" % i)
        items["kind"].append(0)
        items["posts"].append(p)
        items["first"].append(first)
        items["last"].append(1_790_000_000)
        items["weeks"].append([[first // 604800, p]])
    edges = {"src": [], "dst": [], "w": [], "first": [], "last": [], "member": []}
    for _ in range(3 * n):
        a = r.randrange(n)
        b = min(n - 1, (a // 500) * 500 + r.randrange(500)) if r.random() < 0.9 else r.randrange(n)
        edges["src"].append(a)
        edges["dst"].append(b)
        edges["w"].append(1 + r.randrange(min(40, items["posts"][a]) + 1))
        edges["first"].append(items["first"][a])
        edges["last"].append(1_790_000_000)
        edges["member"].append(False)
    doc = {"schema": 1, "datasets": [{"id": "synthetic", "title": "Synthetic load test", "description": "Generated for a load test; not real agents.",
                                       "citation": "", "url": "", "items": items, "edges": edges}], "bridges": []}
    with open(out, "w") as f:
        json.dump(doc, f, separators=(",", ":"))
    print(f"{n} identities, {3 * n} edges -> {out}", file=sys.stderr)


if __name__ == "__main__":
    main()
