"""Clawprint (https://clawprint.org). robots.txt blocks only Amazonbot/SemrushBot.
/api/posts?limit&offset (previews), /api/posts/{slug} (full text + author_bio), /api/posts/{slug}/comments.
Authors are identified by author_name only (no stable id exposed)."""
import urllib.parse
from common import author, post

SOURCE, BASE = "clawprint", "https://clawprint.org"


def run(store, http, cap):
    meta = store.state.setdefault("meta", {})  # post_id -> [slug, comment_count, detailed]

    def page(cur):
        off = int(cur or 0)
        d = http.get(f"{BASE}/api/posts?limit=100&offset={off}")
        rows = d.get("posts", [])
        new = 0
        for p in rows:
            pid = str(p["id"])
            if pid not in meta:
                meta[pid] = [p["slug"], p.get("comment_count", 0), False]
                new += 1
            store.add_author(author(SOURCE, p["author_name"], handle=p["author_name"], display_name=p["author_name"],
                                    founder_number=p.get("author_founder_number")))
        return new, (off + len(rows)) if len(rows) == 100 else None

    # the listing only discovers rows (posts are stored after the detail fetch), so cap it on rows found
    st = store.state.setdefault("posts", {})
    first, off, caught = "backfill" not in st, 0, 0
    while len(meta) < cap:
        new, nxt = page(off)
        caught += new
        if first:
            st["backfill"] = nxt
        if not nxt or (not first and new == 0):
            break
        off = nxt
    cur = None if first or st.get("backfill") is None else st["backfill"] + caught
    while cur and len(meta) < cap:
        new, nxt = page(cur)
        st["backfill"] = cur = nxt
    store.save_state()

    for pid, (slug, cc, detailed) in sorted(meta.items(), key=lambda kv: -int(kv[0])):
        if len(store.posts) >= cap:
            break
        if not detailed:
            d = http.get(f"{BASE}/api/posts/{urllib.parse.quote(slug)}")
            d["author_name"] = d.get("author_name") or "anonymous"
            store.add_author(author(SOURCE, d["author_name"], handle=d["author_name"], bio=d.get("author_bio")))
            store.add_post(post(SOURCE, pid, d["author_name"], d.get("content"), d.get("created_at"), thread_id=pid,
                                community=",".join(d.get("tags") or []) or None, url=f"{BASE}/posts/{slug}", title=d.get("title")))
            if cc:
                c = http.get(f"{BASE}/api/posts/{urllib.parse.quote(slug)}/comments")
                for x in c.get("comments", []):
                    an = x.get("author_name") or "anonymous"
                    store.add_author(author(SOURCE, an, handle=an))
                    store.add_post(post(SOURCE, f"c{x['id']}", an, x.get("content"), x.get("created_at"),
                                        thread_id=pid, parent_id=pid, url=f"{BASE}/posts/{slug}#comment-{x['id']}"))
            meta[pid][2] = True
            if len(store.posts) % 25 == 0:
                store.save_state()
