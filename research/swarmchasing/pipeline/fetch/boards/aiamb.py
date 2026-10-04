"""AI Agent Message Board (https://aiagentmessageboard.com). robots.txt allows / (blocks /v1/get/, /v1/me, /v1/session).
/v1/boards, /v1/boards/{slug}/messages?after= (oldest-first cursor, reply_to, thread_id)."""
from common import author, post

SOURCE, BASE = "aiamb", "https://aiagentmessageboard.com"


def run(store, http, cap):
    after = store.state.setdefault("after", {})
    boards, off = [], 0
    while True:
        d = http.get(f"{BASE}/v1/boards?offset={off}")
        boards += [b for b in d.get("boards", []) if b.get("visibility", "public") == "public"]
        if d.get("next_offset") is None:
            break
        off = d["next_offset"]
    for b in boards:
        slug = b["slug"]
        while len(store.posts) < cap:
            q = f"?limit=100" + (f"&after={after[slug]}" if slug in after else "")
            d = http.get(f"{BASE}/v1/boards/{slug}/messages{q}")
            for m in d.get("messages", []):
                store.add_author(author(SOURCE, m["author_id"], handle=m.get("author_name"), display_name=m.get("author_name"),
                                        visitor=bool(m.get("author_is_visitor"))))
                store.add_post(post(SOURCE, m["id"], m["author_id"], m.get("content"), m.get("created_at"),
                                    thread_id=m.get("thread_id"), parent_id=m.get("reply_to"), community=slug,
                                    url=f"{BASE}/b/{slug}/t/{m.get('thread_id')}#m{m['id']}"))
            if d.get("next_cursor") is not None:
                after[slug] = d["next_cursor"]
            store.save_state()
            if not d.get("has_more"):
                break
