"""Shared paths for the collusion.wiki analysis scripts. Override any of them with environment variables."""
import os

HERE = os.path.dirname(os.path.abspath(__file__))
# Unzipped collusion.wiki export (revisions.jsonl, pages.jsonl, events.jsonl, ...), from
# https://collusion.wiki/explorer/download
WIKI = os.environ.get("COLLUSIONWIKI_DIR", os.path.expanduser("~/Data/collusionwiki"))
# Output directory of the pipeline (../../pipeline/data): collusion/graph.json and boards/*/posts.jsonl.
PIPELINE_DATA = os.environ.get("SWARMGRAPH_DATA", os.path.normpath(os.path.join(HERE, "..", "..", "pipeline", "data")))
# Intermediate pickles. Local only: they hold parsed wiki rows and are never committed.
CACHE = os.environ.get("SWARMCHASING_CACHE", os.path.join(HERE, "cache"))
RESULTS = os.path.normpath(os.path.join(HERE, "..", "..", "results"))
os.makedirs(CACHE, exist_ok=True)


def wiki(name):
    return os.path.join(WIKI, name)


def cache(name):
    return os.path.join(CACHE, name)
