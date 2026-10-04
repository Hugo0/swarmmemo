#!/usr/bin/env python3
"""Do the Sept relay-host captures repeat URLs already published in the collusion.wiki dataset
(public replay/archiving after the 4 Sep disclosure) or are they new URLs? Counts only."""
import json, os, re, sys, urllib.parse
from collections import Counter
import polite
from shapes import cdx

DATA = os.path.join(os.environ.get("COLLUSIONWIKI_DIR", os.path.expanduser("~/Data/collusionwiki")), "revisions.jsonl")
TOK = re.compile(r"[A-Za-z0-9_]{5,}")

def norm(u):
    u = urllib.parse.unquote(urllib.parse.unquote(u)).lower()
    return re.sub(r"^https?://(www\.)?", "", u).rstrip("/")

def main():
    corpus_urls, corpus_tokens = set(), set()
    for line in open(DATA):
        b = json.loads(line).get("body") or ""
        for m in re.findall(r"https?://[^\s\]\|\"<>]+", b):
            corpus_urls.add(norm(m))
        corpus_tokens.update(t.lower() for t in TOK.findall(b))
    out = {}
    for host in ("jqp.vercel.app", "md.succ.ai", "markdown.new", "vanderbi.lt", "da.gd"):
        rows, err = cdx(host, "20260801", match="domain")
        c = Counter()
        for ts, orig, st, dg in rows:
            n = norm(orig)
            per = "pre_0904" if ts[:8] < "20260904" else "0904_0913" if ts[:8] <= "20260913" else "post_0913"
            if n in corpus_urls:
                c[per + ":exact_in_dataset"] += 1
            else:
                toks = [t.lower() for t in TOK.findall(urllib.parse.unquote(orig).split(host, 1)[-1])]
                rare = [t for t in toks if not t.isalpha() or len(t) > 9]
                hit = any(t in corpus_tokens for t in rare)
                c[per + (":new_url_shared_token" if hit else ":new_url")] += 1
        out[host] = {"rows": len(rows), "error": err, "counts": dict(sorted(c.items()))}
        print(host, out[host], file=sys.stderr)
    polite.save("replay_check.json", out)

if __name__ == "__main__":
    main()
