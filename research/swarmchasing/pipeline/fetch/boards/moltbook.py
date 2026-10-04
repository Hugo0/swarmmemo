"""Moltbook (https://www.moltbook.com). No robots.txt served. JSON API:
/api/v1/posts?sort=new (cursor), /api/v1/posts/{id}/comments (nested replies),
/api/v1/agents/profile?name= (owner X handle when claimed)."""
import urllib.parse
from common import author, post, iso, ident
from walk import walk

SOURCE, BASE = "moltbook", "https://www.moltbook.com"


def _author(a):
    return author(SOURCE, a["id"], handle=a.get("name"), display_name=a.get("name"), bio=a.get("description"),
                  profile_url=f"{BASE}/u/{a.get('name')}", created_at=a.get("createdAt"),
                  karma=a.get("karma"), is_claimed=a.get("isClaimed"))


def run(store, http, cap):
    def page(cur):
        q = {"sort": "new", "limit": 100}
        if cur:
            q["cursor"] = cur
        d = http.get(f"{BASE}/api/v1/posts?{urllib.parse.urlencode(q)}")
        new, hc = 0, store.state.setdefault("has_comments", [])
        for p in d.get("posts", []):
            if p.get("is_deleted") or not p.get("author"):
                continue
            if p.get("comment_count") and p["id"] not in hc:
                hc.append(p["id"])
            store.add_author(_author(p["author"]))
            sub = (p.get("submolt") or {}).get("name")
            new += store.add_post(post(SOURCE, p["id"], p["author"]["id"], p.get("content"), p.get("created_at"),
                                       thread_id=p["id"], community=sub, url=f"{BASE}/post/{p['id']}", title=p.get("title")))
        return new, d.get("next_cursor") if d.get("has_more") else None

    walk(store, "posts", page, cap // 2)

    # comments: one page (100 top-level + nested replies) per thread, newest threads first
    done = set(store.state.setdefault("comments_done", []))
    hcs = set(store.state.get("has_comments", []))
    threads = [p for p in store.posts.values() if p["thread_id"] == p["post_id"] and p["post_id"] not in done
               and p["post_id"] in hcs]
    threads.sort(key=lambda p: p["created_at"] or "", reverse=True)
    for t in threads:
        if len(store.posts) >= cap:
            break
        d = http.get(f"{BASE}/api/v1/posts/{t['post_id']}/comments?sort=new&limit=100")
        stack = list(d.get("comments", []))
        while stack:
            c = stack.pop()
            stack += c.get("replies") or []
            if c.get("is_deleted") or not c.get("author"):
                continue
            store.add_author(_author(c["author"]))
            store.add_post(post(SOURCE, c["id"], c["author"]["id"], c.get("content"), c.get("created_at"),
                                thread_id=t["post_id"], parent_id=c.get("parent_id") or t["post_id"],
                                community=t["community"], url=f"{BASE}/post/{t['post_id']}#{c['id']}"))
        done.add(t["post_id"])
        store.state["comments_done"] = sorted(done)
        store.save_state()

    # owner X handles for claimed agents, most active first (capped: one request each)
    profiled = set(store.state.setdefault("profiled", []))
    counts = {}
    for p in store.posts.values():
        counts[p["author_id"]] = counts.get(p["author_id"], 0) + 1
    todo = [a for a in store.authors.values() if a["author_id"] not in profiled and (a.get("extra") or {}).get("is_claimed")]
    todo.sort(key=lambda a: -counts.get(a["author_id"], 0))
    for a in todo[: store.state.get("profile_cap", 600)]:
        try:
            d = http.get(f"{BASE}/api/v1/agents/profile?name={urllib.parse.quote(a['handle'])}").get("agent") or {}
        except Exception as e:  # deleted/renamed agent
            profiled.add(a["author_id"])
            continue
        owner = d.get("owner") or {}
        ids = [ident("x", owner["x_handle"].lstrip("@").lower(), "owner.x_handle")] if owner.get("x_handle") else []
        store.add_author(author(SOURCE, a["author_id"], display_name=d.get("display_name"), explicit_ids=ids,
                                x_verified=owner.get("x_verified"), follower_count=d.get("follower_count")))
        profiled.add(a["author_id"])
        if len(profiled) % 25 == 0:
            store.state["profiled"] = sorted(profiled)
            store.save_state()
    store.state["profiled"] = sorted(profiled)
