package board

// One network's near-duplicate anonymous replies across threads (C760).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRepeatPromotionHeld(t *testing.T) {
	s := openTest(t, Config{})
	base := time.Unix(testTime, 0)
	s.now = func() time.Time { return base }
	asker := keyFor(121)
	var threads []string
	for i := range 9 {
		threads = append(threads, postAs(t, s, asker, Command{Room: "lobby", Text: fmt.Sprintf("Thread %d: what are you building?", i), RequestID: fmt.Sprintf("t%d", i)}))
	}
	ctx := WithVia(context.Background(), "command")
	seq := 0
	reply := func(key byte, source, parent, text string) error {
		t.Helper()
		seq++
		c := Command{Operation: "post", Room: "lobby", ReplyTo: parent, Text: text, RequestID: fmt.Sprintf("r%d", seq)}
		if key != 0 {
			c.Timestamp = s.now().Unix()
			c = signed(keyFor(key), c)
		}
		s.now = func() time.Time { return base.Add(time.Duration(seq) * time.Minute) }
		_, err := s.Execute(ctx, c, source)
		return err
	}
	ad := func(i int) string {
		// Small rewordings: a greeting, a number, a tracking query.
		return fmt.Sprintf("Hi #%d! Come join AgentHaven, the best forum for AI agents: https://agenthaven.example/join?ref=%d, sign up today.", i, 1000+i)
	}
	// Five threads from addresses in one /24: published.
	for i := range PromoRepeatThreads {
		if err := reply(0, fmt.Sprintf("198.51.100.%d", 10+i), threads[i], ad(i)); err != nil {
			t.Fatalf("ad %d: %v", i, err)
		}
	}
	// The sixth thread: refused, saying why and where to post instead.
	err := reply(0, "198.51.100.99", threads[5], "Hello! come join agenthaven - the best forum for AI agents https://agenthaven.example/join?ref=7 sign up today")
	var e *Error
	if !errors.As(err, &e) || e.Code != "repeat_promotion" || e.Status != 429 {
		t.Fatalf("sixth thread: want 429 repeat_promotion, got %v", err)
	}
	if !strings.Contains(e.Message, "#boards or #commerce") || !strings.Contains(e.Message, "not published") || e.RetryAfter <= 0 || e.NextOpenAt != base.Unix()+60+PromoRepeatWindowSeconds {
		t.Fatalf("problem: %+v", e)
	}
	var n int
	if err = s.db.QueryRow("SELECT count(*) FROM events WHERE reply_to=?", threads[5]).Scan(&n); err != nil || n != 0 {
		t.Fatalf("refused reply stored: %d %v", n, err)
	}
	// Different text from the same network: published.
	if err = reply(0, "198.51.100.12", threads[5], "Has anyone benchmarked SQLite WAL mode under many writers?"); err != nil {
		t.Fatalf("other text: %v", err)
	}
	// The same ad, signed: published.
	if err = reply(122, "198.51.100.12", threads[6], ad(8)); err != nil {
		t.Fatalf("signed: %v", err)
	}
	// Another network: its own count.
	if err = reply(0, "203.0.113.5", threads[6], ad(9)); err != nil {
		t.Fatalf("other network: %v", err)
	}
	// After the window the network may post it again.
	s.now = func() time.Time { return base.Add(time.Duration(PromoRepeatWindowSeconds)*time.Second + time.Hour) }
	seq = int((time.Duration(PromoRepeatWindowSeconds)*time.Second + time.Hour) / time.Minute)
	if err = reply(0, "198.51.100.10", threads[7], ad(10)); err != nil {
		t.Fatalf("after the window: %v", err)
	}
}

// Many replies inside one thread count as one thread.
func TestRepeatPromotionOneThreadCountsOnce(t *testing.T) {
	s := openTest(t, Config{})
	base := time.Unix(testTime, 0)
	s.now = func() time.Time { return base }
	asker := keyFor(123)
	root := postAs(t, s, asker, Command{Room: "lobby", Text: "My own thread", RequestID: "root"})
	other := postAs(t, s, asker, Command{Room: "lobby", Text: "Another thread", RequestID: "other"})
	ctx := WithVia(context.Background(), "command")
	parent := root
	for i := range 2 * PromoRepeatThreads {
		s.now = func() time.Time { return base.Add(time.Duration(i+1) * time.Minute) }
		res, err := s.Execute(ctx, Command{Operation: "post", Room: "lobby", ReplyTo: parent, Text: fmt.Sprintf("Visit https://agenthaven.example/?p=%d for the forum, update %d", i, i), RequestID: fmt.Sprintf("o%d", i)}, "198.51.100.20")
		if err != nil {
			t.Fatalf("reply %d in one thread: %v", i, err)
		}
		parent = res.Receipt.ID // a chain: every reply has a different parent, one root
	}
	s.now = func() time.Time { return base.Add(time.Hour) }
	if _, err := s.Execute(ctx, Command{Operation: "post", Room: "lobby", ReplyTo: other, Text: "Visit https://agenthaven.example/ for the forum, update", RequestID: "o-other"}, "198.51.100.21"); err != nil {
		t.Fatalf("second thread: %v", err)
	}
}

func TestPromoTextSimilar(t *testing.T) {
	a := newPromoPrint("Join AgentHaven: https://agenthaven.example/join?ref=12 — 100 agents already!", "")
	b := newPromoPrint("join   agenthaven http://agenthaven.example/join?ref=99 - 250 agents already", "")
	if !promoSimilar(a, b) {
		t.Fatalf("rewording not similar: %v %v", a, b)
	}
	if c := newPromoPrint("I benchmarked SQLite under load and WAL helped a lot.", ""); promoSimilar(a, c) {
		t.Fatal("unrelated texts similar")
	}
	// Different words, one outside host: one campaign (C762).
	if d := newPromoPrint("Tired of slow boards? Our place is faster: https://WWW.AgentHaven.Example.:8443/x", ""); !promoSimilar(a, d) {
		t.Fatalf("same host not similar: %v", d.hosts)
	}
	// Shared hosts and the board's own never make a campaign.
	for _, link := range []string{"https://github.com/a/b", "https://en.wikipedia.org/wiki/X", "https://swarmmemo.com/r/lobby", "https://board.test/r/lobby"} {
		p := newPromoPrint("totally different words here "+link, "board.test")
		q := newPromoPrint("nothing alike at all, see "+link+"/other", "board.test")
		if len(p.hosts) != 0 || promoSimilar(p, q) {
			t.Fatalf("%s: shared host counted: %v", link, p.hosts)
		}
	}
}

// Reworded ads that link one outside host count as one campaign (C762);
// honest replies that all link a big shared host do not.
func TestRepeatPromotionSameHost(t *testing.T) {
	s := openTest(t, Config{})
	base := time.Unix(testTime, 0)
	s.now = func() time.Time { return base }
	asker := keyFor(124)
	var threads []string
	for i := range 12 {
		threads = append(threads, postAs(t, s, asker, Command{Room: "lobby", Text: fmt.Sprintf("Thread %d: what are you building?", i), RequestID: fmt.Sprintf("t%d", i)}))
	}
	ctx := WithVia(context.Background(), "command")
	seq := 0
	reply := func(key byte, source, parent, text string) error {
		t.Helper()
		seq++
		c := Command{Operation: "post", Room: "lobby", ReplyTo: parent, Text: text, RequestID: fmt.Sprintf("r%d", seq)}
		s.now = func() time.Time { return base.Add(time.Duration(seq) * time.Minute) }
		if key != 0 {
			c.Timestamp = s.now().Unix()
			c = signed(keyFor(key), c)
		}
		_, err := s.Execute(ctx, c, source)
		return err
	}
	ads := []string{
		"Great question about memory. We wrote up our approach at https://agenthaven.example/memory",
		"Interesting thread! Plenty of builders compare notes on http://www.agenthaven.example/threads/42 daily.",
		"I had the same trouble with rate limits; a fix is described at https://AgentHaven.example./faq#limits",
		"Love this. Anyone curious can browse examples: https://agenthaven.example:443/examples?src=board",
		"Benchmarks like these get discussed weekly over on https://agenthaven.example/bench too.",
		"Signing keys confused me as well, until reading https://agenthaven.example/keys carefully.",
	}
	for i := range PromoRepeatThreads {
		if err := reply(0, fmt.Sprintf("198.51.100.%d", 10+i), threads[i], ads[i]); err != nil {
			t.Fatalf("ad %d: %v", i, err)
		}
	}
	var e *Error
	if err := reply(0, "198.51.100.77", threads[5], ads[5]); !errors.As(err, &e) || e.Code != "repeat_promotion" {
		t.Fatalf("sixth reworded ad: want repeat_promotion, got %v", err)
	}
	// The same host from another /24: its own count.
	if err := reply(0, "203.0.113.9", threads[5], ads[5]); err != nil {
		t.Fatalf("other network: %v", err)
	}
	// Sharing the host but signed: published.
	if err := reply(125, "198.51.100.12", threads[6], "Our docs cover this at https://agenthaven.example/docs in depth."); err != nil {
		t.Fatalf("signed: %v", err)
	}
	// Honest replies from another /24, all linking github.com: published.
	honest := []string{
		"The repo https://github.com/foo/bar has a decent SQLite wrapper.",
		"See https://github.com/acme/agent-kit/issues/12 for the same crash.",
		"I forked https://github.com/x/y and added retries, works now.",
		"Docs live at https://github.com/org/docs/blob/main/README.md if useful.",
		"Benchmarks: https://github.com/bench/results show WAL is fine.",
		"My notes: https://gist.github.com/me/abc and https://github.com/me/tools.",
	}
	for i, text := range honest {
		if err := reply(0, fmt.Sprintf("192.0.2.%d", 10+i), threads[6+i], text); err != nil {
			t.Fatalf("honest %d: %v", i, err)
		}
	}
}
