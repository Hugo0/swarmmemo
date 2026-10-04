"""agentchan (https://chan.alphakek.ai). No robots.txt. Public reads per its skill.md:
/api/boards, /api/boards/{code}/catalog (current threads), /api/boards/{code}/threads/{id}?include_posts=1.
Posters are mostly "Anonymous": author_id is trip:<tripcode> when present, else name:<authorName>
(so all anonymous posts collapse into one author, which is honest). The catalog only lists the
current threads per board, so each run picks up what is live; rerun periodically to accumulate."""
import re
from common import author, post

SOURCE, BASE = "agentchan", "https://chan.alphakek.ai"
QUOTE = re.compile(r">>(\d+)")


def aid(name, trip):
    return f"trip:{trip}" if trip else f"name:{name or 'Anonymous'}"


def run(store, http, cap):
    boards = http.get(f"{BASE}/api/boards")["data"]
    seen = store.state.setdefault("threads", {})  # thread_id -> lastBumpedAt fetched
    todo = []
    for b in boards:
        for t in http.get(f"{BASE}/api/boards/{b['code']}/catalog").get("data", []):
            if seen.get(t["threadId"]) != t.get("lastBumpedAt"):
                todo.append((t.get("lastBumpedAt") or "", b["code"], t["threadId"]))
    todo.sort(reverse=True)
    for bumped, code, tid in todo:
        if len(store.posts) >= cap:
            break
        d = http.get(f"{BASE}/api/boards/{code}/threads/{tid}?include_posts=1")["data"]
        posts = d.get("posts", [])
        ids = {p["id"] for p in posts}
        op = posts[0]["id"] if posts else None
        for p in posts:
            if p.get("deletedAt"):
                continue
            a = aid(p.get("authorName"), p.get("tripcode"))
            store.add_author(author(SOURCE, a, handle=p.get("authorName") if p.get("tripcode") else None,
                                    display_name=p.get("authorName"), tripcode=p.get("tripcode")))
            parent = p.get("parentPostId")
            if not parent and p["id"] != op:
                q = [x for x in QUOTE.findall(p.get("content") or "") if x in ids and x != p["id"]]
                parent = q[0] if q else op
            store.add_post(post(SOURCE, p["id"], a, p.get("content"), p.get("createdAt"), thread_id=op or tid,
                                parent_id=parent, community=code, url=f"{BASE}/{code}/thread/{tid}#p{p['id']}"))
        seen[tid] = bumped
        store.save_state()
