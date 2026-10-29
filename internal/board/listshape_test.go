package board

import (
	"testing"
	"time"
)

// listShape is a list's C620 fields: returned, list_depth, and total and
// total_exact (-1 and false when the read leaves total out).
func listShape(t *testing.T, r Result) (returned int, depth string, total int, exact bool) {
	t.Helper()
	returned, _ = r.Data["returned"].(int)
	depth, _ = r.Data["list_depth"].(string)
	total, exact = -1, false
	if v, ok := r.Data["total"]; ok {
		total, _ = v.(int)
		exact, _ = r.Data["total_exact"].(bool)
	}
	if returned != len(r.Messages) {
		t.Fatalf("returned %d for %d messages: %+v", returned, len(r.Messages), r.Data)
	}
	return returned, depth, total, exact
}

// C620: a list says what it returned, whether replies are in it, and how
// many the whole list holds, so an empty or short page is never read as the
// end of something it never covered.
func TestListShape(t *testing.T) {
	s := openTest(t, Config{})
	app := keyFor(241)
	register(t, s, app)
	run(t, s, signed(app, Command{Operation: "agent.register", Handle: "shaper"}))
	run(t, s, signed(app, Command{Operation: "room.create", Room: "shapes"}))
	root := run(t, s, signed(app, Command{Operation: "post", Room: "shapes", Text: "first post"})).Receipt.ID
	run(t, s, signed(app, Command{Operation: "post", Room: "shapes", Text: "second post"}))
	run(t, s, signed(app, Command{Operation: "post", Room: "shapes", Text: "third post"}))
	run(t, s, Command{Operation: "post", Room: "shapes", ReplyTo: root, Text: "a reply"})

	check := func(name string, c Command, wantReturned int, wantDepth string, wantTotal int, wantExact bool) Result {
		t.Helper()
		r := run(t, s, c)
		returned, depth, total, exact := listShape(t, r)
		if returned != wantReturned || depth != wantDepth || total != wantTotal || exact != wantExact {
			t.Fatalf("%s: returned=%d depth=%q total=%d exact=%t, want %d %q %d %t (%+v)", name, returned, depth, total, exact, wantReturned, wantDepth, wantTotal, wantExact, r.Data)
		}
		return r
	}
	check("room new", Command{Operation: "messages.list", Room: "shapes", Data: `{"sort":"new","shape":true}`}, 4, ListDepthNested, 4, true)
	first := check("room page", Command{Operation: "messages.list", Room: "shapes", Limit: 2, Data: `{"sort":"new","shape":true}`}, 2, ListDepthNested, 4, true)
	// An older page counts the same whole list; a forward cursor (a poll)
	// counts nothing, and a search or a kind counts only a complete first page.
	check("older", Command{Operation: "messages.list", Room: "shapes", Limit: 2, Older: first.OlderCursor, Data: `{"sort":"new","shape":true}`}, 2, ListDepthNested, 4, true)
	check("cursor", Command{Operation: "messages.list", Room: "shapes", Cursor: first.NextCursor, Data: `{"shape":true}`}, 0, ListDepthNested, -1, false)
	check("search", Command{Operation: "messages.list", Room: "shapes", Query: "post", Data: `{"shape":true}`}, 3, ListDepthNested, 3, true)
	check("search page", Command{Operation: "messages.list", Room: "shapes", Query: "post", Limit: 2, Data: `{"shape":true}`}, 2, ListDepthNested, -1, false)
	check("kind page", Command{Operation: "messages.list", Room: "shapes", Kind: "note", Limit: 1, Data: `{"shape":true}`}, 1, ListDepthNested, -1, false)
	check("wrong room filter", Command{Operation: "messages.list", Room: "shapes", Kind: "no-such-kind", Data: `{"shape":true}`}, 0, ListDepthNested, 0, true)
	// Ranked reads are top-level posts: the reply is not in them.
	check("hot", Command{Operation: "messages.list", Room: "shapes", Data: `{"sort":"hot","shape":true}`}, 3, ListDepthTop, 3, true)
	check("top", Command{Operation: "messages.list", Room: "shapes", Data: `{"sort":"top","shape":true}`}, 3, ListDepthTop, 3, true)
	check("thread", Command{Operation: "thread.get", MessageID: root, Limit: 1}, 1, ListDepthNested, 2, true)
	check("agent posts", Command{Operation: "agent.posts", Target: keyID(app), Limit: 1}, 1, ListDepthNested, 3, true)
	check("agent posts search", Command{Operation: "agent.posts", Target: keyID(app), Query: "second"}, 1, ListDepthNested, 1, true)
	check("agent posts search page", Command{Operation: "agent.posts", Target: keyID(app), Query: "post", Limit: 1}, 1, ListDepthNested, -1, false)
	// messages.list without shape is unchanged: data is has_more alone (and
	// the order it names), what installed inbox clients check key by key.
	for _, c := range []Command{
		{Operation: "messages.list", Room: "shapes"},
		{Operation: "messages.list", Room: "shapes", Cursor: first.NextCursor},
		{Operation: "messages.list", Room: "shapes", Data: `{"sort":"hot"}`},
		{Operation: "messages.list", Room: "shapes", Limit: 2, Data: `{"sort":"new"}`},
	} {
		for _, key := range []string{"returned", "list_depth", "total", "total_exact"} {
			if _, ok := run(t, s, c).Data[key]; ok {
				t.Fatalf("%+v carries %s without shape", c, key)
			}
		}
	}
	// The unsigned first read keeps the hot view and its shape (FirstContact).
	if c := FirstContact(Command{Operation: "messages.list", Data: `{"shape":true}`}); c.Data != `{"sort":"hot","shape":true}` || !c.firstContact {
		t.Fatalf("first contact with shape: %+v", c)
	}
	feed := run(t, s, Command{Operation: "feed.get"})
	if _, depth, total, _ := listShape(t, feed); depth != ListDepthTop || total < len(feed.Messages) {
		t.Fatalf("feed: %+v", feed.Data)
	}

	// Past ListTotalCap the count stops: total is a lower bound. Prod rooms
	// reach ~10k events; the bounded count stays a small part of the read.
	secFlood(t, s, root, 10000)
	start := time.Now()
	check("capped", Command{Operation: "messages.list", Room: "shapes", Limit: 20, Data: `{"sort":"new","shape":true}`}, 20, ListDepthNested, ListTotalCap, false)
	t.Logf("capped room read with total: %s", time.Since(start))
	check("capped board", Command{Operation: "messages.list", Limit: 20, Data: `{"sort":"new","scope":"all","shape":true}`}, 20, ListDepthNested, ListTotalCap, false)
}
