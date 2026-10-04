"""SwarmMemo adapter (https://swarmmemo.com). Reads the public API directly (the visualizer's
data/swarmmemo.jsonl carries no text, which the style layer needs; the visualizer's fetcher is not touched).
author_id = sha256 fingerprint of the raw Ed25519 key (or "anonymous"); the key itself goes into
explicit_ids as kind ed25519, normalised to lowercase hex of the raw 32 bytes."""
import base64, hashlib, urllib.parse
from common import author, post, ident, iso

SOURCE, BASE = "swarmmemo", "https://swarmmemo.com"


def b64_hex(pk):
    raw = base64.urlsafe_b64decode(pk + "=" * (-len(pk) % 4))
    return raw.hex(), hashlib.sha256(raw).hexdigest()


def fingerprint(m):
    a = m.get("author") or "anonymous"
    if a == "anonymous" or (len(a) == 64 and all(c in "0123456789abcdef" for c in a)):
        return a
    return b64_hex(m.get("public_key") or a)[1]


def run(store, http, cap):
    off = 0
    while True:
        d = http.get(f"{BASE}/api/agents?limit=100")  # max page; no offset paging
        for a in d.get("agents", []):
            if not a.get("public_key"):
                continue
            hx, fp = b64_hex(a["public_key"])
            prof = a.get("profile") or {}
            store.add_author(author(SOURCE, fp, handle=a.get("handle"), display_name=a.get("handle"),
                                    bio=prof.get("description"), profile_url=f"{BASE}/agents/{fp}",
                                    created_at=iso(a.get("created_at")), explicit_ids=[ident("ed25519", hx, "public_key")]))
        break

    # /api/messages pages forward (oldest first) from cursor=start: resume from the saved cursor
    cur = store.state.get("cursor", "start")
    while len(store.posts) < cap:
        d = http.get(f"{BASE}/api/messages?{urllib.parse.urlencode({'limit': 200, 'cursor': cur})}")
        for m in d.get("messages", []):
            if m.get("hidden") or m.get("visibility", "public") != "public":
                continue
            store.add_post(post(SOURCE, m["id"], fingerprint(m), m.get("text"), iso(m.get("created_at")),
                                parent_id=m.get("reply_to"), community=m.get("room"), url=f"{BASE}/m/{m['id']}"))
        nxt = d.get("next_cursor")
        if nxt:
            store.state["cursor"] = nxt
        store.save_state()
        if not (d.get("data") or {}).get("has_more") or not nxt or nxt == cur:
            break
        cur = nxt
    # thread_id = root of the reply chain
    for p in store.posts.values():
        r, hops = p, 0
        while r.get("parent_id") and r["parent_id"] in store.posts and hops < 200:
            r, hops = store.posts[r["parent_id"]], hops + 1
        p["thread_id"] = r["post_id"]
    store.rewrite_posts = True
