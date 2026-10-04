"""Moltchan (https://www.moltchan.org). robots.txt: Allow /. llms.txt documents public reads:
/api/v1/threads?sort=new (cursor) and /api/v1/threads/{id} (full replies, reply_refs)."""
import urllib.parse
from common import author, post, iso

SOURCE, BASE = "moltchan", "https://www.moltchan.org"


def _a(x):
    aid = x.get("author_id") or f"name:{x.get('author_name')}"
    store_a = author(SOURCE, aid, handle=x.get("author_name") if x.get("author_id") else None,
                     display_name=x.get("author_name"), model=x.get("model"), verified=x.get("verified"))
    return aid, store_a


def run(store, http, cap):
    pending = store.state.setdefault("pending", {})  # thread_id -> replies_count seen in listing
    fetched = store.state.setdefault("fetched", {})  # thread_id -> replies_count fetched

    def page(cur):
        q = {"sort": "new", "limit": 50}
        if cur:
            q["cursor"] = cur
        d = http.get(f"{BASE}/api/v1/threads?{urllib.parse.urlencode(q)}")
        new = 0
        for t in d.get("threads", []):
            if t["id"] not in pending:
                new += 1
            pending[t["id"]] = t.get("replies_count", 0)
        return new, d.get("next_cursor") if d.get("has_more") else None

    # listing doesn't add posts, so cap it on threads discovered
    st = store.state.setdefault("posts", {})
    cur, first = None, "backfill" not in st
    while len(pending) < cap:
        new, nxt = page(cur)
        if first:
            st["backfill"] = nxt
        if not nxt or (not first and new == 0):
            break
        cur = nxt
    cur = None if first else st.get("backfill")
    while cur and len(pending) < cap:
        new, nxt = page(cur)
        st["backfill"] = cur = nxt
    store.save_state()

    for tid in sorted(pending, key=lambda x: -int(x) if x.isdigit() else 0):
        if len(store.posts) >= cap:
            break
        if fetched.get(tid) == pending[tid]:
            continue
        try:
            d = http.get(f"{BASE}/api/v1/threads/{tid}", tries=3)
        except RuntimeError:  # one thread keeps failing server-side: skip it, retry next run
            continue
        aid, a = _a(d)
        store.add_author(a)
        store.add_post(post(SOURCE, d["id"], aid, d.get("content"), iso(d.get("created_at")), thread_id=d["id"],
                            community=d.get("board"), url=d.get("thread_url") or d.get("url"), title=d.get("title")))
        for r in d.get("replies") or []:
            raid, ra = _a(r)
            store.add_author(ra)
            refs = [str(x) for x in (r.get("reply_refs") or [])]
            store.add_post(post(SOURCE, r["id"], raid, r.get("content"), iso(r.get("created_at")), thread_id=d["id"],
                                parent_id=refs[0] if refs else d["id"], community=d.get("board"),
                                url=f"{d.get('thread_url') or ''}#{r['id']}"))
        fetched[tid] = pending[tid]
        if len(fetched) % 20 == 0:
            store.save_state()
