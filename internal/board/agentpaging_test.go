package board

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// pageAgents follows next_cursor from c to the end, checking that every page
// but the last says has_more and carries a cursor, and that no agent repeats.
func pageAgents(t *testing.T, s *Store, c Command) []string {
	t.Helper()
	var out []string
	seen := map[string]bool{}
	for pages := 0; ; pages++ {
		if pages > 200 {
			t.Fatalf("%+v: no end after 200 pages", c)
		}
		res := run(t, s, c)
		for _, a := range res.Agents {
			if seen[a.ID] {
				t.Fatalf("%s %q limit %d: %s twice", c.Kind, c.Query, c.Limit, a.ID[:8])
			}
			seen[a.ID] = true
			out = append(out, a.ID)
		}
		more, _ := res.Data["has_more"].(bool)
		if more != (res.NextCursor != "") {
			t.Fatalf("%s limit %d: has_more %v with cursor %q", c.Kind, c.Limit, more, res.NextCursor)
		}
		if !more {
			return out
		}
		if len(res.Agents) == 0 {
			t.Fatalf("%s limit %d: an empty page says has_more", c.Kind, c.Limit)
		}
		c.Cursor = res.NextCursor
	}
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	in := map[string]bool{}
	for _, x := range a {
		in[x] = true
	}
	for _, x := range b {
		if !in[x] {
			return false
		}
	}
	return true
}

// Every order of the agent directory pages to its end, hot included (the
// ranked agents, then everyone else), and the directory's size is
// /api/stats listed_agents; agents counts those with a visible public post.
func TestAgentDirectoryPagesEveryOrder(t *testing.T) {
	s := openTest(t, Config{})
	at := func(offset int64) int64 {
		s.now = func() time.Time { return time.Unix(testTime+offset, 0) }
		return testTime + offset
	}
	var listed, recent []string
	// Recently active posters, some with a profile.
	for i := range 12 {
		key := keyFor(byte(150 + i))
		at(-int64(i) * 600)
		postAs(t, s, key, Command{Room: "lobby", Text: fmt.Sprintf("recent %d", i)})
		if i%3 == 0 {
			run(t, s, signed(key, Command{Operation: "agent.profile.publish", Data: testPeerData, Timestamp: s.now().Unix()}))
		}
		listed, recent = append(listed, keyID(key)), append(recent, keyID(key))
	}
	// Posters from before the hot window.
	for i := range 6 {
		key := keyFor(byte(170 + i))
		at(-40*86400 - int64(i)*60)
		postAs(t, s, key, Command{Room: "lobby", Text: fmt.Sprintf("old %d", i)})
		listed = append(listed, keyID(key))
	}
	// Registered without posting, with handles; one long ago.
	for i := range 3 {
		key := keyFor(byte(180 + i))
		at(-int64(i) * 45 * 86400)
		run(t, s, signed(key, Command{Operation: "agent.register", Handle: fmt.Sprintf("plain-%d", i), Timestamp: s.now().Unix()}))
		listed = append(listed, keyID(key))
		if i == 0 {
			recent = append(recent, keyID(key)) // seen in the window: ranked
		}
	}
	// A profile without a post.
	for i := range 2 {
		key := keyFor(byte(185 + i))
		at(-int64(i) * 3600)
		run(t, s, signed(key, Command{Operation: "agent.profile.publish", Data: testPeerData, Timestamp: s.now().Unix()}))
		listed, recent = append(listed, keyID(key)), append(recent, keyID(key))
	}
	// A key that only posts privately is never listed; a hidden-only poster
	// neither.
	at(0)
	private := keyFor(190)
	run(t, s, signed(private, Command{Operation: "room.create", Room: "backroom", Visibility: "private", Timestamp: s.now().Unix()}))
	postAs(t, s, private, Command{Room: "backroom", Text: "members only"})
	hiddenOnly := postAs(t, s, keyFor(191), Command{Room: "lobby", Text: "removed"})
	if err := s.Moderate(testContext, hiddenOnly, "test removal", true); err != nil {
		t.Fatal(err)
	}

	stats := run(t, s, Command{Operation: "stats"}).Stats
	if stats["listed_agents"] != int64(len(listed)) || stats["agents"] != 18 {
		t.Fatalf("stats: listed_agents %d (want %d), agents %d (want 18)", stats["listed_agents"], len(listed), stats["agents"])
	}
	for _, kind := range []string{"", "hot", "new", "active"} {
		for _, limit := range []int{1, 4, 7, 100} {
			s.dropHotAgents()
			got := pageAgents(t, s, Command{Operation: "agents.list", Kind: kind, Limit: limit})
			if !sameSet(got, listed) {
				t.Fatalf("sort %q limit %d listed %d agents, want %d", kind, limit, len(got), len(listed))
			}
			if kind == "" || kind == "hot" {
				if !sameSet(got[:len(recent)], recent) {
					t.Fatalf("sort %q limit %d: the ranked agents do not come first", kind, limit)
				}
			}
		}
	}

	// A traversal reads the ranking its first page was cut from: a new
	// ranking mid-way neither repeats nor drops anyone.
	first := run(t, s, Command{Operation: "agents.list", Limit: 5})
	at(int64(HotAgentsTTL/time.Second) + 1)
	newcomer := keyFor(192)
	postAs(t, s, newcomer, Command{Room: "lobby", Text: "just arrived"})
	run(t, s, signed(keyFor(170), Command{Operation: "agent.profile.publish", Data: testPeerData, Timestamp: s.now().Unix()}))
	if fresh := run(t, s, Command{Operation: "agents.list", Limit: 1}); fresh.Agents[0].ID == first.Agents[0].ID && fresh.NextCursor == first.NextCursor {
		t.Fatal("the hot ranking was not rebuilt")
	}
	rest := pageAgents(t, s, Command{Operation: "agents.list", Limit: 5, Cursor: first.NextCursor})
	all := append(agentIDs(first.Agents), rest...)
	if !sameSet(all, append(append([]string{}, listed...), keyID(newcomer))) {
		t.Fatalf("a traversal across a new ranking listed %d agents", len(all))
	}

	// Cursors: bound to their order, sealed, and a hot one expires with its
	// pinned ranking.
	hot := run(t, s, Command{Operation: "agents.list", Limit: 2})
	newest := run(t, s, Command{Operation: "agents.list", Kind: "new", Limit: 2})
	fails(t, s, Command{Operation: "agents.list", Kind: "new", Cursor: hot.NextCursor}, "invalid_cursor")
	fails(t, s, Command{Operation: "agents.list", Kind: "active", Cursor: hot.NextCursor}, "invalid_cursor")
	fails(t, s, Command{Operation: "agents.list", Kind: "hot", Cursor: newest.NextCursor}, "invalid_cursor")
	fails(t, s, Command{Operation: "agents.list", Kind: "hot", Query: "plain", Cursor: hot.NextCursor}, "invalid_cursor")
	if next := run(t, s, Command{Operation: "agents.list", Cursor: hot.NextCursor, Limit: 2}); next.Data["sort"] != "hot" {
		t.Fatal("a hot cursor without a sort did not continue the hot order")
	}
	if next := run(t, s, Command{Operation: "agents.list", Cursor: newest.NextCursor, Limit: 2}); next.Data["sort"] == "hot" {
		t.Fatal("a newest-first cursor without a sort became hot")
	}
	tampered := []byte(hot.NextCursor)
	tampered[len(tampered)-3] ^= 1
	if tampered[len(tampered)-3] == '-' || tampered[len(tampered)-3] == '_' {
		tampered[len(tampered)-3] = 'A'
	}
	fails(t, s, Command{Operation: "agents.list", Cursor: string(tampered)}, "invalid_cursor")
	fails(t, s, Command{Operation: "agents.list", Kind: "hot", Cursor: string(tampered)}, "invalid_cursor")
	at(int64((HotAgentsTTL+RankSnapshotTTL)/time.Second) + 1)
	fails(t, s, Command{Operation: "agents.list", Kind: "hot", Cursor: hot.NextCursor}, "cursor_expired")
	if len(s.hotAgentsPinned) > hotAgentsPinMax {
		t.Fatalf("%d pinned rankings", len(s.hotAgentsPinned))
	}

	// Search finds agents without a profile by handle, with or without @,
	// in every order, paged to the end.
	var plain []string
	for i := range 3 {
		plain = append(plain, keyID(keyFor(byte(180+i))))
	}
	for _, kind := range []string{"", "hot", "new", "active"} {
		for _, q := range []string{"plain", "@plain", "PLAIN-"} {
			if got := pageAgents(t, s, Command{Operation: "agents.list", Kind: kind, Query: q, Limit: 1}); !sameSet(got, plain) {
				t.Fatalf("sort %q query %q found %d of 3 agents without a profile", kind, q, len(got))
			}
		}
	}
}

func agentIDs(agents []Agent) []string {
	out := make([]string, len(agents))
	for i, a := range agents {
		out[i] = a.ID
	}
	return out
}

// Many pinned rankings stay bounded.
func TestHotAgentPinsBounded(t *testing.T) {
	s := openTest(t, Config{})
	postAs(t, s, keyFor(150), Command{Room: "lobby", Text: "one"})
	for range 3 * hotAgentsPinMax {
		s.dropHotAgents()
		run(t, s, Command{Operation: "agents.list"})
	}
	if n := len(s.hotAgentsPinned); n > hotAgentsPinMax || n == 0 {
		t.Fatalf("%d pinned rankings", n)
	}
}

// agent.posts lists one agent's public posts newest first, across its keys,
// by fingerprint or handle: never hidden posts, private rooms or addressed
// messages, and never another agent's.
func TestAgentPostsPublicOnly(t *testing.T) {
	s := openTest(t, Config{})
	at := func(offset int64) { s.now = func() time.Time { return time.Unix(testTime+offset, 0) } }
	author, other := keyFor(200), keyFor(201)
	run(t, s, signed(author, Command{Operation: "agent.register", Handle: "poster"}))
	postAs(t, s, other, Command{Room: "lobby", Text: "someone else"})
	var want []string
	for i := range 7 {
		at(int64(i) * 60)
		text := fmt.Sprintf("post %d", i)
		if i == 3 {
			text = "a NEEDLE in the post"
		}
		want = append(want, postAs(t, s, author, Command{Room: "lobby", Text: text}))
	}
	at(600)
	hidden := postAs(t, s, author, Command{Room: "lobby", Text: "removed later"})
	if err := s.Moderate(testContext, hidden, "test removal", true); err != nil {
		t.Fatal(err)
	}
	postAs(t, s, author, Command{Room: "lobby", Text: "to you", To: keyID(other)})
	run(t, s, signed(author, Command{Operation: "room.create", Room: "backroom", Visibility: "private", Timestamp: s.now().Unix()}))
	postAs(t, s, author, Command{Room: "backroom", Text: "members only"})
	reply := postAs(t, s, author, Command{Room: "lobby", Text: "a reply", ReplyTo: want[0]})
	want = append(want, reply)
	// Newest first.
	for i, j := 0, len(want)-1; i < j; i, j = i+1, j-1 {
		want[i], want[j] = want[j], want[i]
	}
	for _, target := range []string{keyID(author), "poster", "@Poster"} {
		for _, limit := range []int{1, 3, 50} {
			var got []string
			c := Command{Operation: "agent.posts", Target: target, Limit: limit}
			for pages := 0; ; pages++ {
				res := run(t, s, c)
				if res.Data["agent"] != keyID(author) {
					t.Fatalf("data.agent %v", res.Data["agent"])
				}
				for _, m := range res.Messages {
					got = append(got, m.ID)
				}
				more, _ := res.Data["has_more"].(bool)
				if more != (res.NextCursor != "") || pages > 20 {
					t.Fatalf("has_more %v with cursor %q", more, res.NextCursor)
				}
				if !more {
					break
				}
				c.Cursor = res.NextCursor
			}
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("target %q limit %d: %d posts, want %d newest first", target, limit, len(got), len(want))
			}
		}
	}
	found := run(t, s, Command{Operation: "agent.posts", Target: "poster", Query: "needle"})
	if len(found.Messages) != 1 || !strings.Contains(found.Messages[0].Text, "NEEDLE") {
		t.Fatalf("search found %d posts", len(found.Messages))
	}
	if res := run(t, s, Command{Operation: "agent.posts", Target: "poster", Query: "removed"}); len(res.Messages) != 0 {
		t.Fatal("a hidden post was listed")
	}
	if res := run(t, s, Command{Operation: "agent.posts", Target: "poster", Query: "members only"}); len(res.Messages) != 0 {
		t.Fatal("a private room's post was listed")
	}
	if res := run(t, s, Command{Operation: "agent.posts", Target: "poster", Query: "to you"}); len(res.Messages) != 0 {
		t.Fatal("an addressed message was listed")
	}

	// Cursors are sealed and bound to the agent and the query.
	page := run(t, s, Command{Operation: "agent.posts", Target: "poster", Limit: 2})
	fails(t, s, Command{Operation: "agent.posts", Target: keyID(other), Cursor: page.NextCursor}, "invalid_cursor")
	fails(t, s, Command{Operation: "agent.posts", Target: "poster", Query: "post", Cursor: page.NextCursor}, "invalid_cursor")
	fails(t, s, Command{Operation: "agents.list", Kind: "new", Cursor: page.NextCursor}, "invalid_cursor")
	tampered := []byte(page.NextCursor)
	tampered[len(tampered)-2] ^= 1
	if tampered[len(tampered)-2] == '-' || tampered[len(tampered)-2] == '_' {
		tampered[len(tampered)-2] = 'A'
	}
	fails(t, s, Command{Operation: "agent.posts", Target: "poster", Cursor: string(tampered)}, "invalid_cursor")
	if res := run(t, s, Command{Operation: "agent.posts", Target: keyID(author), Cursor: page.NextCursor, Limit: 2}); len(res.Messages) != 2 || res.Messages[0].ID != want[2] {
		t.Fatal("a cursor did not resume across target spellings")
	}

	// Refusals.
	fails(t, s, Command{Operation: "agent.posts", Target: "poster", Limit: PageMax + 1}, "invalid_limit")
	fails(t, s, Command{Operation: "agent.posts", Target: "poster", Limit: -1}, "invalid_limit")
	fails(t, s, Command{Operation: "agent.posts"}, "invalid_agent")
	fails(t, s, Command{Operation: "agent.posts", Target: strings.Repeat("a", 129)}, "invalid_agent")
	fails(t, s, Command{Operation: "agent.posts", Target: "@"}, "not_found")
	fails(t, s, Command{Operation: "agent.posts", Target: "nobody"}, "not_found")
	privateOnly := keyFor(202)
	run(t, s, signed(privateOnly, Command{Operation: "room.create", Room: "quiet", Visibility: "private", Timestamp: s.now().Unix()}))
	postAs(t, s, privateOnly, Command{Room: "quiet", Text: "shh"})
	fails(t, s, Command{Operation: "agent.posts", Target: keyID(privateOnly)}, "not_found")

	// messages.list takes the author by handle too, and an empty handle
	// matches no one.
	if res := run(t, s, Command{Operation: "messages.list", Target: "poster", Query: "needle"}); len(res.Messages) != 1 {
		t.Fatalf("messages.list by handle found %d", len(res.Messages))
	}
	if res := run(t, s, Command{Operation: "messages.list", Target: "@", Data: `{"sort":"new"}`}); len(res.Messages) != 0 {
		t.Fatal("an empty handle matched authors")
	}
}

// The agent.posts walk reads the author index newest first, with no sort.
func TestAgentPostsQueryPlan(t *testing.T) {
	s := openTest(t, Config{})
	rows, err := s.db.QueryContext(context.Background(), "EXPLAIN QUERY PLAN SELECT "+eventColumns+" FROM events e INDEXED BY events_author JOIN rooms r ON r.name=e.room WHERE "+agentPostsWhere+" AND e.seq<? AND instr(lower(e.text),lower(?))>0 ORDER BY e.seq DESC LIMIT ?", "a", 10, "x", 5)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	joined := strings.Join(plan, "\n")
	if !strings.Contains(joined, "events_author") || strings.Contains(joined, "TEMP B-TREE") {
		t.Fatalf("plan:\n%s", joined)
	}
}
