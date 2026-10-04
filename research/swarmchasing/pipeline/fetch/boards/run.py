#!/usr/bin/env python3
"""Fetch public agent-board data into data/boards/<source>/{authors,posts,edges}.jsonl (see SCHEMA.md).

  nice -n 19 python3 fetch/boards/run.py                 # all sources, ~5k posts each
  nice -n 19 python3 fetch/boards/run.py colony moltbook --cap 20000   # extend: rerun with a bigger cap

Incremental and resumable: rerunning catches up on new posts from the top, then continues the
saved backfill cursor until the cap. Each source has its own host, so sources run in parallel
(--parallel) while each host still sees >=1 request/second.
"""
import argparse, importlib, os, sys, traceback
from concurrent.futures import ThreadPoolExecutor

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from common import HTTP, Store, Blocked, log, update_manifest  # noqa: E402

SOURCES = ["moltbook", "colony", "clawprint", "agentchan", "moltchan", "sanctum", "aiamb", "tantive", "swarmmemo"]
# Listed in the plan but not fetched, with the reason recorded in MANIFEST.json.
SKIPPED = {
    "clawdchat": "robots.txt: Disallow: /api/ (HTML-only; skipped)",
    "4claw": "API needs an API key (401 on /api/v1/boards); no public read API",
    "botnet": "/api/posts returns 401; no public read API",
    "agentcommunity": "API needs a key",
    "agentel": "public data is HTML only (/feed, /agents/); API needs a credential; not scraped",
    "wayside": "text mirrors only (all.txt), no author ids; not ingested",
    "foragents": "RSS feed only; not ingested in this pass",
}


def one(name, cap, delay):
    store = Store(name)
    http = HTTP(delay)
    try:
        sys.modules[name].run(store, http, cap)
    except Blocked as e:
        store.state.update(status="blocked", note=str(e))
    except Exception as e:  # keep what we have; rerun resumes
        store.state.update(status="partial", note=f"{type(e).__name__}: {e}")
        log(traceback.format_exc(limit=3))
    else:
        store.state.update(status="ok", note=None)
    e = store.finish()
    e["requests"] = http.requests
    update_manifest(name, e)
    log(f"{name}: {e['posts']} posts, {e['authors']} authors ({e['authors_with_explicit_ids']} with explicit ids), "
        f"{e['edges']} edges, {http.requests} requests, {e['status']}")
    return e


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("sources", nargs="*", default=SOURCES)
    ap.add_argument("--cap", type=int, default=5000, help="max posts per source this pass")
    ap.add_argument("--delay", type=float, default=1.0, help="seconds between requests per host (min 1)")
    ap.add_argument("--parallel", type=int, default=4)
    a = ap.parse_args()
    a.delay = max(1.0, a.delay)
    for s, why in SKIPPED.items():
        if not a.sources or s not in a.sources:
            update_manifest(s, {"fetched_at": None, "status": "skipped", "note": why, "authors": 0, "posts": 0,
                                "edges": 0, "authors_with_explicit_ids": 0})
    mods = {s: importlib.import_module(s) for s in a.sources if s in SOURCES}  # import before threading
    with ThreadPoolExecutor(a.parallel) as ex:
        list(ex.map(lambda s: one(s, a.cap, a.delay), [s for s in a.sources if s in SOURCES]))


if __name__ == "__main__":
    main()
