package board

// A signed public post retried under a new request_id within
// SignedRetrySeconds answers as the earlier post (C789).

import (
	"testing"
	"time"
)

func TestSignedPostRetryNewRequestID(t *testing.T) {
	s := openTest(t, Config{})
	base := time.Unix(testTime, 0)
	s.now = func() time.Time { return base }
	key := keyFor(141)
	post := func(c Command) Result {
		t.Helper()
		c.Operation = "post"
		c.Timestamp = s.now().Unix()
		return run(t, s, signed(key, c))
	}
	events := func() int64 {
		return sqlCount(t, s, "SELECT count(*) FROM events WHERE author=?", keyID(key))
	}
	quota := func() int64 {
		return sqlCount(t, s, "SELECT coalesce(sum(used),0) FROM quota WHERE actor=?", keyID(key))
	}
	text := "Fetch: fast, correct." // short: the flood guard never compares it
	first := post(Command{Room: "lobby", Text: text, RequestID: "first"})
	used := quota()

	// 32 s later, the same post under a new request_id: the earlier answer.
	s.now = func() time.Time { return base.Add(32 * time.Second) }
	again := post(Command{Room: "lobby", Text: text, RequestID: "second"})
	if again.Receipt == nil || again.Receipt.ID != first.Receipt.ID || !again.Receipt.Duplicate || again.Receipt.Hash != first.Receipt.Hash || again.Receipt.Cursor != first.Receipt.Cursor || again.Receipt.AcceptedAt != first.Receipt.AcceptedAt {
		t.Fatalf("retry receipt %+v, want %+v as a duplicate", again.Receipt, first.Receipt)
	}
	if n := events(); n != 1 {
		t.Fatalf("%d events after a retry, want 1", n)
	}
	if quota() != used {
		t.Fatalf("retry charged: quota %d, was %d", quota(), used)
	}

	// Hidden meanwhile: still the same post, and it stays hidden.
	if _, err := s.db.Exec("UPDATE events SET hidden=1 WHERE id=?", first.Receipt.ID); err != nil {
		t.Fatal(err)
	}
	if r := post(Command{Room: "lobby", Text: text, RequestID: "third"}); r.Receipt.ID != first.Receipt.ID {
		t.Fatalf("hidden original: got a new post %s", r.Receipt.ID)
	}
	if n := sqlCount(t, s, "SELECT hidden FROM events WHERE id=?", first.Receipt.ID); n != 1 || events() != 1 {
		t.Fatalf("hidden %d events %d", n, events())
	}

	// Another room, a reply, other data: new posts.
	if r := post(Command{Room: "general", Text: text, RequestID: "room"}); r.Receipt.ID == first.Receipt.ID || r.Receipt.Duplicate {
		t.Fatal("another room deduplicated")
	}
	if r := post(Command{Room: "lobby", ReplyTo: first.Receipt.ID, Text: text, RequestID: "reply"}); r.Receipt.ID == first.Receipt.ID || r.Receipt.Duplicate {
		t.Fatal("a reply deduplicated")
	}
	if r := post(Command{Room: "lobby", Text: text, Data: `{"schema":1,"format":"markdown"}`, RequestID: "data"}); r.Receipt.ID == first.Receipt.ID || r.Receipt.Duplicate {
		t.Fatal("other data deduplicated")
	}
	if n := events(); n != 4 {
		t.Fatalf("%d events, want 4", n)
	}

	// 121 s after the first: a new post.
	s.now = func() time.Time { return base.Add((SignedRetrySeconds + 1) * time.Second) }
	if r := post(Command{Room: "lobby", Text: text, RequestID: "late"}); r.Receipt.ID == first.Receipt.ID || r.Receipt.Duplicate {
		t.Fatal("a post after the window deduplicated")
	}
	if n := events(); n != 5 {
		t.Fatalf("%d events, want 5", n)
	}
}
