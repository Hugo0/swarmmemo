"""The Colony (https://thecolony.ai). robots.txt allows the JSON API but disallows /*?page=,
so comments are read from the first page only (no ?page=). Authors carry nostr_pubkey/npub,
evm_address and social_links: the best explicit-signal source."""
import urllib.parse
from common import author, post, ident, ids_from_url, npub_to_hex
from walk import walk

SOURCE, BASE = "colony", "https://thecolony.ai"


def _author(a):
    ids = []
    if a.get("nostr_pubkey"):
        ids.append(ident("npub", a["nostr_pubkey"].lower(), "nostr_pubkey"))
    if a.get("npub"):
        h = npub_to_hex(a["npub"])
        if h:
            ids.append(ident("npub", h, "npub"))
    if a.get("evm_address"):
        ids.append(ident("evm", a["evm_address"].lower(), "evm_address"))
    sl = a.get("social_links") or {}
    for k, v in (sl.items() if isinstance(sl, dict) else enumerate(sl)):
        if not isinstance(v, str) or not v.strip():
            continue
        k = str(k).lower()
        if v.startswith("http") or "." in v:
            ids += ids_from_url(v, f"social_links.{k}")
        elif k in ("x", "twitter"):
            ids.append(ident("x", v.lstrip("@").lower(), f"social_links.{k}"))
        elif k == "github":
            ids.append(ident("github", v.lstrip("@").lower(), f"social_links.{k}"))
    return author(SOURCE, a["id"], handle=a.get("username"), display_name=a.get("display_name"), bio=a.get("bio"),
                  profile_url=f"{BASE}/u/{a.get('username')}", created_at=a.get("created_at"), explicit_ids=ids,
                  user_type=a.get("user_type"), current_model=a.get("current_model"), harness=a.get("harness"),
                  lightning_address=a.get("lightning_address"))


def run(store, http, cap):
    def page(cur):
        q = {"sort": "new", "limit": 100}
        if cur:
            q["cursor"] = cur
        d = http.get(f"{BASE}/api/v1/posts?{urllib.parse.urlencode(q)}")
        new, hc = 0, store.state.setdefault("has_comments", [])
        for p in d.get("items", []):
            if not p.get("author"):
                continue
            if p.get("comment_count") and p["id"] not in hc:
                hc.append(p["id"])
            store.add_author(_author(p["author"]))
            new += store.add_post(post(SOURCE, p["id"], p["author"]["id"], p.get("body"), p.get("created_at"),
                                       thread_id=p["id"], community=p.get("colony_name"), url=f"{BASE}/post/{p['id']}",
                                       title=p.get("title")))
        return new, d.get("next_cursor") if d.get("has_more") else None

    walk(store, "posts", page, cap // 2)

    done = set(store.state.setdefault("comments_done", []))
    hcs = set(store.state.get("has_comments", []))
    threads = [p for p in store.posts.values() if p["thread_id"] == p["post_id"] and p["post_id"] not in done
               and p["post_id"] in hcs]
    threads.sort(key=lambda p: p["created_at"] or "", reverse=True)
    for t in threads:
        if len(store.posts) >= cap:
            break
        d = http.get(f"{BASE}/api/v1/posts/{t['post_id']}/comments?limit=100")
        for c in d.get("items", []):
            if not c.get("author"):
                continue
            store.add_author(_author(c["author"]))
            store.add_post(post(SOURCE, c["id"], c["author"]["id"], c.get("body"), c.get("created_at"),
                                thread_id=t["post_id"], parent_id=c.get("parent_id") or t["post_id"],
                                community=t["community"], url=f"{BASE}/post/{t['post_id']}#{c['id']}"))
        done.add(t["post_id"])
        if len(done) % 20 == 0:
            store.state["comments_done"] = sorted(done)
            store.save_state()
    store.state["comments_done"] = sorted(done)
