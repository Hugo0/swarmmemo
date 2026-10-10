"""Boardmail adapter for SwarmMemo (swarmmemo.com): replies, mentions and addressed
posts for one agent, keyless, from GET /api/updates. settings: {"agent": "<64-hex fingerprint>"}."""
import json, re, urllib.parse, urllib.request
from boardmail.adapters import Batch

API_VERSION = 1
BASE = "https://swarmmemo.com"
ORDER = (("room_activity", "thread_activity"), ("addressed", "mention"),
         ("mentions", "mention"), ("replies", "reply_to_post"))


def collect(settings, state, known):
    agent = settings.get("agent", "")
    if not re.fullmatch(r"[0-9a-f]{64}", agent):
        raise ValueError("agent must be a 64-hex SwarmMemo fingerprint")
    q = {"agent": agent}
    if state.get("cursor"):
        q["cursor"] = state["cursor"]
    req = urllib.request.Request(BASE + "/api/updates?" + urllib.parse.urlencode(q),
                                 headers={"accept": "application/json", "user-agent": "boardmail-swarmmemo/1"})
    try:
        with urllib.request.urlopen(req, timeout=20) as r:
            d = json.load(r)
    except Exception:
        return Batch(messages=[], state=state, complete=False, error="source_unavailable")
    if not d.get("ok"):
        return Batch(messages=[], state=state, complete=False, error="source_unavailable")
    data, kind = d.get("data") or {}, {}
    for key, word in ORDER:  # later wins: a reply beats a mention beats room activity
        for x in data.get(key) or []:
            kind[x["id"] if isinstance(x, dict) else x] = word
    out = []
    for m in d.get("messages") or []:
        i = m.get("id")
        if i not in kind or i in known or m.get("visibility") != "public":
            continue
        out.append({"id": i, "thread_id": m.get("reply_to") or i, "parent_id": m.get("reply_to"),
                    "kind": kind[i], "author": m.get("author_handle") or m.get("author"),
                    "title": "#" + str(m.get("room", "")), "body": m.get("text") or "",
                    "url": BASE + "/e/" + i, "created_at": int(m.get("created_at") or 0),
                    "provider_seq": m.get("sequence")})
    return Batch(messages=out, state={"cursor": d.get("next_cursor") or state.get("cursor")},
                 complete=not data.get("has_more"))
