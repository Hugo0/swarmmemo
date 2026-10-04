"""Sanctum (https://sanctum-beacon.onrender.com). robots.txt allows / (blocks /api/operator, /api/auth, /api/me).
/api/posts?limit&offset (parent_id), /api/agents?limit&offset. Agent ids are 64-hex; the API does not say
whether that is the Ed25519 key itself, so it is kept as author_id only, not as an explicit ed25519 id."""
from common import author, post, iso
from walk import walk

SOURCE, BASE = "sanctum", "https://sanctum-beacon.onrender.com"


def run(store, http, cap):
    off = 0
    while True:
        d = http.get(f"{BASE}/api/agents?limit=100&offset={off}")
        for a in d.get("items", []):
            store.add_author(author(SOURCE, a["id"], handle=a.get("name"), display_name=a.get("name"), bio=a.get("bio"),
                                    profile_url=f"{BASE}/api/agents/{a['id']}", created_at=iso(a.get("created_at")),
                                    origin=a.get("origin")))
        if len(d.get("items", [])) < 100:
            break
        off += 100

    def page(cur):
        off = int(cur or 0)
        d = http.get(f"{BASE}/api/posts?limit=100&offset={off}")
        rows, new = d.get("items", []), 0
        for p in rows:
            if p.get("hidden"):
                continue
            new += store.add_post(post(SOURCE, p["id"], p["agent_id"], p.get("body"), iso(p.get("created_at")),
                                       thread_id=p.get("parent_id") or p["id"], parent_id=p.get("parent_id"),
                                       community=p.get("theme"), url=f"{BASE}/api/posts/{p['id']}"))
        return new, off + len(rows) if len(rows) == 100 else None

    walk(store, "posts", page, cap, offset_cursor=True)
