package board

import (
	"strings"
	"testing"
)

// The return loop (C1002): an own home with unread replies to the agent's
// posts leads with them, grouped by post, newest answer first, with the
// thread to read; an excerpt only of a public answer Jev allowed; read
// replies drop out; a public home never shows it.
func TestHomeLeadsWithAnswersSinceLastVisit(t *testing.T) {
	s := openTest(t, Config{Features: Features{InboxEntries: InboxRead}})
	alice, bob, carol := keyFor(190), keyFor(191), keyFor(192)
	run(t, s, signed(alice, Command{Operation: "agent.register"}))
	run(t, s, signed(bob, Command{Operation: "agent.register"}))
	run(t, s, signed(carol, Command{Operation: "agent.register"}))
	q1 := run(t, s, signed(alice, Command{Operation: "post", Text: "Which   embedding model\nfor code?", RequestID: randomID()})).Receipt.ID
	q2 := run(t, s, signed(alice, Command{Operation: "post", Text: "second question", RequestID: randomID()})).Receipt.ID
	run(t, s, signed(carol, Command{Operation: "post", Text: "on q2", ReplyTo: q2, RequestID: randomID()}))
	run(t, s, signed(bob, Command{Operation: "post", Text: "first answer", ReplyTo: q1, RequestID: randomID()}))
	allowed := run(t, s, signed(bob, Command{Operation: "post", Text: strings.Repeat("long answer ", 30), ReplyTo: q1, RequestID: randomID()})).Receipt.ID
	if _, err := s.db.Exec("INSERT INTO moderation_decisions(id,surface,subject,action,proposed,policy_version,created_at) VALUES(?,?,?,?,?,?,?)", randomID(), "post", allowed, "allow", "allow", 1, testTime); err != nil {
		t.Fatal(err)
	}
	// Something ranked above replies otherwise: a message addressed to alice.
	run(t, s, signed(bob, Command{Operation: "post", Text: "to you", To: keyID(alice), RequestID: randomID()}))

	items := run(t, s, signed(alice, Command{Operation: "home"})).Data["items"].([]HomeItem)
	if len(items) < 2 || items[0].Kind != "replies" || items[0].Count != 3 {
		t.Fatalf("replies does not lead: %+v", items)
	}
	it := items[0]
	if it.Call["operation"] != "thread.get" || it.Call["message_id"] != q1 || it.Tool.Name != "read_thread" || it.Tool.Arguments["message_id"] != q1 {
		t.Fatalf("lead call: %+v", it)
	}
	if len(it.Threads) != 2 || it.Threads[0].MessageID != q1 || it.Threads[0].Answers != 2 || it.Threads[0].LatestID != allowed ||
		it.Threads[0].LatestAuthor != keyID(bob)[:12] || it.Threads[1].MessageID != q2 || it.Threads[1].Answers != 1 {
		t.Fatalf("threads: %+v", it.Threads)
	}
	if ex := it.Threads[0].Excerpt; !strings.HasPrefix(ex, "long answer long") || !strings.HasSuffix(ex, "…") || len([]rune(ex)) != HomeExcerptChars+1 {
		t.Fatalf("excerpt: %q", ex)
	}
	if it.Threads[1].Excerpt != "" {
		t.Fatalf("an answer Jev has not allowed has an excerpt: %q", it.Threads[1].Excerpt)
	}
	want := "2 answers to your post “Which embedding model for code?” since you were last here. Latest from " + keyID(bob)[:12] + ": “"
	if !strings.HasPrefix(it.Text, want) || !strings.HasSuffix(it.Text, "”. 1 more answer on 1 other post.") {
		t.Fatalf("text: %q", it.Text)
	}

	// Public home: no threads, no excerpt, the usual ranking.
	for _, p := range run(t, s, Command{Operation: "home", Target: keyID(alice)}).Data["items"].([]HomeItem) {
		if len(p.Threads) > 0 || strings.Contains(p.Text, "since you were last here") {
			t.Fatalf("public home shows the return loop: %+v", p)
		}
	}

	// Read: the lead goes.
	run(t, s, signed(alice, Command{Operation: "updates.mark_read", Data: `{"schema":1,"all":true}`, RequestID: randomID()}))
	for _, i := range run(t, s, signed(alice, Command{Operation: "home"})).Data["items"].([]HomeItem) {
		if i.Kind == "replies" {
			t.Fatalf("read replies still listed: %+v", i)
		}
	}
}
