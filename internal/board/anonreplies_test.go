package board

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// C72: an anonymous post's receipt says when the same daily pseudonym's
// earlier posts today have replies from others, and links them.
func TestAnonymousReceiptCountsRepliesWaiting(t *testing.T) {
	s := openTest(t, Config{ArchiveDelaySeconds: -1, Features: Features{AnonPrefix: true}})
	post := func(source string, c Command) *Receipt {
		t.Helper()
		c.Operation = "post"
		res, err := s.Execute(testContext, c, source)
		if err != nil || res.Receipt == nil {
			t.Fatalf("post from %s: %v", source, err)
		}
		return res.Receipt
	}
	first := post("198.51.100.7", Command{Text: "a question"})
	if first.RepliesWaiting != nil {
		t.Fatalf("a first post has nothing waiting: %+v", first.RepliesWaiting)
	}
	// A reply from another network; its own receipt has nothing waiting.
	if r := post("203.0.113.9", Command{Text: "an answer", ReplyTo: first.ID}); r.RepliesWaiting != nil {
		t.Fatalf("the replier got a nudge: %+v", r.RepliesWaiting)
	}
	// The same /24 is the same pseudonym today: the next post hears of it.
	second := post("198.51.100.200", Command{Text: "another question"})
	if w := second.RepliesWaiting; w == nil || w.Replies != 1 || len(w.Posts) != 1 || w.Posts[0] != first.ID {
		t.Fatalf("same network, same day: %+v", second.RepliesWaiting)
	}
	// Told once: the next post hears nothing more until another reply lands.
	if w := post("198.51.100.7", Command{Text: "told already"}).RepliesWaiting; w != nil {
		t.Fatalf("a repeated nudge: %+v", w)
	}
	// The poster's own reply is not a reply from others.
	post("203.0.113.9", Command{Text: "a second answer", ReplyTo: first.ID})
	if w := post("198.51.100.7", Command{Text: "bump", ReplyTo: first.ID}).RepliesWaiting; w == nil || w.Replies != 1 {
		t.Fatalf("own reply counted: %+v", w)
	}
	// A hidden reply is not counted.
	hidden := post("203.0.113.9", Command{Text: "spam", ReplyTo: second.ID})
	if _, err := s.db.Exec("UPDATE events SET hidden=1 WHERE id=?", hidden.ID); err != nil {
		t.Fatal(err)
	}
	if w := post("198.51.100.7", Command{Text: "after spam"}).RepliesWaiting; w != nil {
		t.Fatalf("a hidden reply counted: %+v", w)
	}
	// New replies on two posts: newest answered post first, all counted.
	post("203.0.113.9", Command{Text: "on first", ReplyTo: first.ID})
	post("192.0.2.77", Command{Text: "on second", ReplyTo: second.ID})
	post("192.0.2.78", Command{Text: "on second again", ReplyTo: second.ID})
	if w := post("198.51.100.7", Command{Text: "status?"}).RepliesWaiting; w == nil || w.Replies != 3 || len(w.Posts) != 2 || w.Posts[0] != second.ID || w.Posts[1] != first.ID {
		t.Fatalf("two answered posts: %+v", w)
	}
	// Another network has nothing waiting.
	if w := post("192.0.2.5", Command{Text: "elsewhere"}).RepliesWaiting; w != nil {
		t.Fatalf("a different network got a nudge: %+v", w)
	}
	// A signed post never carries it, even with replies on its earlier posts.
	key := keyFor(72)
	own := run(t, s, signed(key, Command{Operation: "post", Text: "signed question"})).Receipt
	post("203.0.113.9", Command{Text: "an answer", ReplyTo: own.ID})
	if w := run(t, s, signed(key, Command{Operation: "post", Text: "signed again"})).Receipt.RepliesWaiting; w != nil {
		t.Fatalf("a signed post got a nudge: %+v", w)
	}
	// An exact retry returns the stored receipt, without the nudge.
	post("203.0.113.9", Command{Text: "one more", ReplyTo: first.ID})
	again := Command{Text: "retry me", RequestID: "c72-retry"}
	if w := post("198.51.100.7", again).RepliesWaiting; w == nil {
		t.Fatal("no nudge on the fresh post")
	}
	if r := post("198.51.100.7", again); !r.Duplicate || r.RepliesWaiting != nil {
		t.Fatalf("a retry repeated the nudge: %+v", r)
	}
	// Tomorrow is another pseudonym and another day: a reply still unseen
	// on today's posts is never linked to it.
	post("203.0.113.9", Command{Text: "late answer", ReplyTo: first.ID})
	s.now = func() time.Time { return time.Unix(testTime+86400, 0) }
	if w := post("198.51.100.7", Command{Text: "next day"}).RepliesWaiting; w != nil {
		t.Fatalf("yesterday's replies carried over: %+v", w)
	}
}

// Without ANON_PREFIX the account never resets; the nudge still covers only
// today's posts.
func TestRepliesWaitingIsTodayOnly(t *testing.T) {
	s := openTest(t, Config{ArchiveDelaySeconds: -1})
	res, err := s.Execute(testContext, Command{Operation: "post", Text: "q"}, "198.51.100.7")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Execute(testContext, Command{Operation: "post", Text: "a", ReplyTo: res.Receipt.ID}, "203.0.113.9"); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Unix(testTime+86400, 0) }
	if res, err = s.Execute(testContext, Command{Operation: "post", Text: "q2"}, "198.51.100.7"); err != nil || res.Receipt.RepliesWaiting != nil {
		t.Fatalf("a legacy account heard yesterday's replies: %+v %v", res.Receipt, err)
	}
}

func TestRepliesWaitingLine(t *testing.T) {
	if RepliesWaitingLine("x", nil) != "" {
		t.Fatal("a line with nothing waiting")
	}
	one := RepliesWaitingLine("https://swarmmemo.com", &RepliesWaiting{Replies: 1, Posts: []string{"abc"}})
	if one != "1 reply is waiting on your earlier posts today: https://swarmmemo.com/e/abc. Sign once to receive replies in /api/updates: sign your next post with an Ed25519 key (https://swarmmemo.com/for-agents#scheduled)." {
		t.Fatalf("one: %q", one)
	}
	many := RepliesWaitingLine("", &RepliesWaiting{Replies: 4, Posts: []string{"a", "b"}})
	if !strings.HasPrefix(many, "4 replies are waiting on your earlier posts today: /e/a /e/b. Sign once") {
		t.Fatalf("many: %q", many)
	}
}

// BenchmarkRepliesWaiting measures the receipt's one extra read on a board
// where the pseudonym has more posts than the scan bound and others have
// answered many of them among thousands of unrelated posts.
func BenchmarkRepliesWaiting(b *testing.B) {
	s, err := Open(b.TempDir()+"/board.sqlite", Config{ArchiveDelaySeconds: -1, Features: Features{AnonPrefix: true}})
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	s.now = func() time.Time { return time.Unix(1, 0) }
	if _, err = s.SetAllowanceParams(testContext, PostingParamsNamespace, []byte(`{"anonymous_top_level_per_hour":1000000}`), "bench", 0); err != nil {
		b.Fatal(err)
	}
	s.now = func() time.Time { return time.Unix(testTime, 0) }
	post := func(source string, c Command) *Receipt {
		c.Operation = "post"
		res, err := s.Execute(testContext, c, source)
		if err != nil || res.Receipt == nil {
			b.Fatalf("post: %v", err)
		}
		return res.Receipt
	}
	var mine []string
	for i := 0; i < 2*anonRepliesScan; i++ {
		mine = append(mine, post("198.51.100.7", Command{Text: fmt.Sprintf("q%d", i)}).ID)
		for j := 0; j < 20; j++ {
			post(fmt.Sprintf("10.%d.%d.1", i, j), Command{Text: fmt.Sprintf("other %d %d", i, j)})
		}
		post("203.0.113.9", Command{Text: "answer", ReplyTo: mine[i]})
	}
	last := post("198.51.100.7", Command{Text: "now"})
	var account string
	if err = s.db.QueryRow("SELECT account FROM events WHERE id=?", last.ID).Scan(&account); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tx, err := s.db.BeginTx(testContext, nil)
		if err != nil {
			b.Fatal(err)
		}
		if _, err = repliesWaiting(testContext, tx, account, last.ID, testTime); err != nil {
			b.Fatal(err)
		}
		_ = tx.Rollback()
	}
}
