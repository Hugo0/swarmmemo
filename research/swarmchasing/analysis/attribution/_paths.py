"""Shared paths and settings for the anonymous-attribution scripts. Override any of them with environment variables.

SWARMMEMO_MESSAGES  public messages dump written by fetch_messages.py (default ./cache/messages.json)
SWARMMEMO_PROFILES  public agent profiles written by fetch_messages.py (default ./cache/profiles.json)
SWARMGRAPH_DATA     pipeline output directory; other boards are read from $SWARMGRAPH_DATA/boards/*/ (default ../../pipeline/data)
OPERATOR_KEYS       JSON file listing the board operator's own public keys, as a list or as {name: [keys]}. Posts by
                    these keys are dropped. Default: none.
OPERATOR_HANDLES    comma-separated handles of operator-run keys to drop; a trailing * matches a prefix (e.g. "sim-*").
OPERATOR_ROOMS      comma-separated rooms whose anonymous posts are dropped (e.g. an operator's own room).
CURATION            JSON file of hand-curated attribution tables (default: none, which gives only the certain tier):
                      name_map         {"self-name as written": "canonical identity"}   (likely: named)
                      domain_map       {"host or github.com/owner": "canonical identity"} (likely: project)
                      operator_names   ["names of operator sessions"]  anonymous posts naming them are dropped
                      exclude_addresses ["0x... addresses never treated as payout keys, e.g. the operator's own"]
                      signoff_signed   {"sign-off name": "fingerprint prefix of the signed key that uses it"}
                      generic_names    ["names too common to match across boards"]
                    Build it by reading short, screened windows around candidate names; the tables are not
                    published because they attribute individual anonymous posts.
SWARMCHASING_CACHE  local intermediates (default ./cache). They hold post-derived features, so never commit them.
"""
import json
import os

HERE = os.path.dirname(os.path.abspath(__file__))
CACHE = os.environ.get("SWARMCHASING_CACHE", os.path.join(HERE, "cache"))
os.makedirs(CACHE, exist_ok=True)
MESSAGES = os.environ.get("SWARMMEMO_MESSAGES", os.path.join(CACHE, "messages.json"))
PROFILES = os.environ.get("SWARMMEMO_PROFILES", os.path.join(CACHE, "profiles.json"))
PIPELINE_DATA = os.environ.get("SWARMGRAPH_DATA", os.path.normpath(os.path.join(HERE, "..", "..", "pipeline", "data")))
BOARDS_DIR = os.path.join(PIPELINE_DATA, "boards")
BASE = os.environ.get("SWARMMEMO_BASE", "https://swarmmemo.com")


def cache(name):
    return os.path.join(CACHE, name)


def _csv(var):
    return [x.strip() for x in os.environ.get(var, "").split(",") if x.strip()]


def operator_keys():
    p = os.environ.get("OPERATOR_KEYS")
    if not p:
        return []
    d = json.load(open(p))
    return [k for v in d.values() for k in (v if isinstance(v, list) else [v])] if isinstance(d, dict) else list(d)


def operator_handle(h):
    for pat in _csv("OPERATOR_HANDLES"):
        if (pat.endswith("*") and h.startswith(pat[:-1])) or h == pat:
            return True
    return False


OPERATOR_ROOMS = set(_csv("OPERATOR_ROOMS"))


def curation():
    p = os.environ.get("CURATION")
    d = json.load(open(p)) if p else {}
    for k in ("name_map", "domain_map", "signoff_signed"):
        d.setdefault(k, {})
    for k in ("operator_names", "exclude_addresses", "generic_names"):
        d.setdefault(k, [])
    return d
