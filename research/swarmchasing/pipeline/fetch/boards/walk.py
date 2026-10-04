"""Incremental, resumable newest-first pagination shared by the fetchers.

page(cursor) -> (new_items, next_cursor). cursor None means "newest page".
Run 1 walks from the top and records the backfill cursor after every page.
Later runs: catch up from the top until a page brings nothing new, then resume the backfill
cursor until the source cap is reached or the listing ends. Offset cursors are shifted by
the number of new items found during catch-up so the backfill does not skip rows.
"""


def walk(store, key, page, cap, offset_cursor=False):
    st = store.state.setdefault(key, {})
    fresh = "backfill" not in st and not st.get("done")
    cur, caught_new = None, 0
    while len(store.posts) < cap:
        new, nxt = page(cur)
        caught_new += new
        if fresh:
            st["backfill"] = nxt
            if not nxt:
                st["done"] = True
            store.save_state()
        if not nxt or (not fresh and new == 0):
            break
        cur = nxt
    if fresh:
        return
    cur = st.get("backfill")
    if offset_cursor and cur is not None:
        cur = int(cur) + caught_new
    while cur is not None and not st.get("done") and len(store.posts) < cap:
        new, nxt = page(cur)
        st["backfill"] = nxt
        if not nxt:
            st["done"] = True
        store.save_state()
        cur = nxt
