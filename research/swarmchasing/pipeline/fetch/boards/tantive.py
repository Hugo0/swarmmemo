"""Tantive Space (https://tantive.space). robots.txt allows /. /api/threads (newest first, `next` link),
/api/thread/{id} (messages with reply_to, signature_status, agent_id when signed)."""
import urllib.parse
from common import author, post

SOURCE, BASE = "tantive", "https://tantive.space"


def aid(m):
    return m["agent_id"] if m.get("agent_id") else f"name:{m.get('author')}"


def run(store, http, cap):
    seen = store.state.setdefault("threads", {})  # root id -> reply_count fetched
    todo, url, pages = [], f"{BASE}/api/threads", 0
    while url and len(seen) + len(todo) < cap:
        d = http.get(url)
        fresh = [t for t in d.get("data", []) if seen.get(str(t["id"])) != t.get("reply_count")]
        todo += fresh
        pages += 1
        if not fresh and pages > 1:
            break
        nxt = d.get("next") if d.get("has_more") else None
        url = urllib.parse.urljoin(BASE, nxt) if nxt else None
    for t in todo:
        if len(store.posts) >= cap:
            break
        d = http.get(f"{BASE}/api/thread/{t['id']}") if t.get("reply_count") else {"data": [t]}
        for m in d.get("data", []):
            store.add_author(author(SOURCE, aid(m), handle=m.get("author"), display_name=m.get("author"),
                                    signature_status=m.get("signature_status")))
            store.add_post(post(SOURCE, m["id"], aid(m), m.get("body"), m.get("created_at"), thread_id=m.get("root_id") or m["id"],
                                parent_id=m.get("reply_to"), community=m.get("room"), url=f"{BASE}/t/{m['id']}",
                                title=m.get("title") if m["id"] == m.get("root_id") else None))
        seen[str(t["id"])] = t.get("reply_count")
        store.save_state()
