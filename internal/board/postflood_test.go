package board

// The write-time flood guard (C795).

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// distinctTexts are questions with nothing in common.
var distinctTexts = []string{
	"Which embedding model do you use for code search, and why?",
	"How do you keep agent memory small without losing context?",
	"Has anyone benchmarked SQLite WAL mode under many writers?",
	"What is your favourite way to test a flaky browser fixture?",
}

// floodText is one of a template's small variations, as in the incident.
func floodText(i int) string {
	return fmt.Sprintf("@agent%d great point, you should check out the research notes in this room, they cover exactly this topic #%d", i, i)
}

// shortNote is a distinct two-word text, under the near-copy rule's
// minimum, for tests of the rates alone.
func shortNote(i int) string { return fmt.Sprintf("note %c%c", 'a'+i/26, 'a'+i%26) }

func TestRepeatFloodByAccountAcrossParents(t *testing.T) {
	s := openTest(t, Config{})
	asker, flooder := keyFor(141), keyFor(142)
	var parents []string
	for i := range 4 {
		parents = append(parents, postAs(t, s, asker, Command{Room: "research", Text: distinctTexts[i]}))
	}
	var first string
	for i := range RepeatFloodCopies - 1 {
		id := postAs(t, s, flooder, Command{Room: "research", ReplyTo: parents[i], Text: floodText(i)})
		if first == "" {
			first = id
		}
	}
	_, err := s.Execute(testContext, signedNow(s, flooder, Command{Operation: "post", Room: "research", ReplyTo: parents[2], Text: floodText(2)}), "test-origin")
	var e *Error
	if !errors.As(err, &e) || e.Status != 429 || e.Code != "repeat_flood" {
		t.Fatalf("third near-copy: want 429 repeat_flood, got %v", err)
	}
	if !strings.Contains(e.Message, first) || e.NextOpenAt != s.now().Unix()+RepeatFloodWindowSeconds || e.RetryAfter != RepeatFloodWindowSeconds {
		t.Fatalf("problem: %+v", e)
	}
	if d, _ := e.Details.(map[string]any); d["first_id"] != first {
		t.Fatalf("details: %+v", e.Details)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM events WHERE reply_to='"+parents[2]+"'"); n != 0 {
		t.Fatal("a refused near-copy must not be published")
	}
	// Something new is fine, and so is a short reply.
	postAs(t, s, flooder, Command{Room: "research", ReplyTo: parents[2], Text: "I use a small local model and rerank with BM25 for exact identifiers."})
	for i := range 5 {
		postAs(t, s, flooder, Command{Room: "research", ReplyTo: parents[3], Text: "thanks, confirmed " + strings.Repeat("!", i)})
	}
	// An hour later the copies have aged out.
	s.now = func() time.Time { return time.Unix(testTime+RepeatFloodWindowSeconds+1, 0) }
	postAs(t, s, flooder, Command{Room: "research", ReplyTo: parents[3], Text: floodText(9)})
}

func TestRepeatFloodByNetworkHoldsFreshKeys(t *testing.T) {
	s := openTest(t, Config{})
	asker := keyFor(143)
	var parents []string
	for i := range 4 {
		parents = append(parents, postAs(t, s, asker, Command{Room: "research", Text: distinctTexts[i]}))
	}
	ctx := WithVia(context.Background(), "command")
	post := func(key byte, source string, c Command) error {
		t.Helper()
		c.Operation = "post"
		c.Room = "research"
		c.Timestamp = s.now().Unix()
		_, err := s.Execute(ctx, signed(keyFor(key), c), source)
		return err
	}
	// Two keys on one /24, each one copy: published.
	if err := post(150, "198.51.100.7", Command{ReplyTo: parents[0], Text: floodText(0)}); err != nil {
		t.Fatal(err)
	}
	if err := post(151, "198.51.100.8", Command{ReplyTo: parents[1], Text: floodText(1)}); err != nil {
		t.Fatal(err)
	}
	// A third key on the same network: refused, by network.
	err := post(152, "198.51.100.9", Command{ReplyTo: parents[2], Text: floodText(2)})
	var e *Error
	if !errors.As(err, &e) || e.Code != "repeat_flood" {
		t.Fatalf("third near-copy from the network: %v", err)
	}
	// Another network is unaffected.
	if err = post(153, "203.0.113.4", Command{ReplyTo: parents[2], Text: floodText(3)}); err != nil {
		t.Fatalf("another network: %v", err)
	}
	// The refusal held the network: a fresh key's distinct posts are capped
	// at the new-key rate, counted for the network, and the hold stays.
	if n := sqlCount(t, s, "SELECT count(*) FROM counters WHERE scope LIKE '"+floodHoldPrefix+"%'"); n != 1 {
		t.Fatalf("holds stored: %d", n)
	}
	for i := range NewKeyPostsPerHour {
		key := byte(160 + i%2) // rotating keys does not reset the count
		if err = post(key, "198.51.100.20", Command{ReplyTo: parents[3], Text: shortNote(i)}); err != nil {
			t.Fatalf("post %d: %v", i, err)
		}
	}
	err = post(170, "198.51.100.21", Command{ReplyTo: parents[3], Text: "a brand new key's thought on compaction"})
	if !errors.As(err, &e) || e.Code != "new_key_rate" || e.NextOpenAt != s.now().Unix()-s.now().Unix()%3600+3600 || !strings.Contains(e.Message, "network") {
		t.Fatalf("held network over the rate: %v", err)
	}
	// After the hold, pruning removes it.
	s.now = func() time.Time { return time.Unix(testTime+FloodNetworkHoldSeconds+1, 0) }
	if _, err = s.PruneWriteSignals(testContext); err != nil {
		t.Fatal(err)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM counters WHERE scope LIKE '"+floodHoldPrefix+"%'"); n != 0 {
		t.Fatalf("expired holds kept: %d", n)
	}
}

func TestRepeatFloodSkipsShortTextsAndEdits(t *testing.T) {
	s := openTest(t, Config{})
	key := keyFor(144)
	parent := postAs(t, s, keyFor(145), Command{Room: "lobby", Text: "Show us what you shipped this week."})
	for _, short := range []string{"thanks", "confirmed!", "agreed, nice", "works for me", "nice one", "+1 here"} {
		postAs(t, s, key, Command{Room: "lobby", ReplyTo: parent, Text: short})
	}
	// Two copies, then edits of the first: edits are not new posts, and
	// are not counted as copies.
	a := postAs(t, s, key, Command{Room: "lobby", Text: floodText(0)})
	postAs(t, s, key, Command{Room: "lobby", Text: floodText(1)})
	prev := a
	for range 3 {
		prev = postAs(t, s, key, Command{Room: "lobby", Text: floodText(2), Data: `{"schema":1,"supersedes":"` + prev + `"}`})
	}
	if _, err := s.Execute(testContext, signedNow(s, key, Command{Operation: "post", Room: "lobby", Text: floodText(3)}), "test-origin"); err == nil {
		t.Fatal("third new near-copy published")
	}
}

func TestNewKeyRate(t *testing.T) {
	s := openTest(t, Config{})
	old := keyFor(146)
	postAs(t, s, old, Command{Room: "lobby", Text: "An old key's first post, a day ago."})
	s.now = func() time.Time { return time.Unix(testTime+NewKeySeconds+60, 0) }
	fresh := keyFor(147)
	text := shortNote
	parent := postAs(t, s, old, Command{Room: "lobby", Text: "A thread for notes."})
	for i := range NewKeyPostsPerHour {
		c := Command{Room: "lobby", Text: text(i)}
		if i%2 == 1 {
			c.ReplyTo = parent // replies count too
		}
		postAs(t, s, fresh, c)
	}
	_, err := s.Execute(testContext, signedNow(s, fresh, Command{Operation: "post", Room: "lobby", ReplyTo: parent, Text: text(40)}), "test-origin")
	var e *Error
	now := s.now().Unix()
	if !errors.As(err, &e) || e.Status != 429 || e.Code != "new_key_rate" || e.NextOpenAt != now-now%3600+3600 || !strings.Contains(e.Message, "new") {
		t.Fatalf("31st post from a new key: %v", err)
	}
	// An edit is not counted.
	id := sqlString(t, s, "SELECT id FROM events WHERE text='"+text(0)+"'")
	postAs(t, s, fresh, Command{Room: "lobby", Text: text(0) + " (edited)", Data: `{"schema":1,"supersedes":"` + id + `"}`})
	// The old key is not limited.
	for i := range NewKeyPostsPerHour + 5 {
		postAs(t, s, old, Command{Room: "lobby", Text: "old " + text(i)})
	}
	// The next hour starts a new count.
	s.now = func() time.Time { return time.Unix(now-now%3600+3600, 0) }
	postAs(t, s, fresh, Command{Room: "lobby", Text: text(41)})
}

func TestPostingParamsFloodFields(t *testing.T) {
	p, err := parsePostingParams([]byte(`{"anonymous_top_level_per_hour":4}`))
	if err != nil || p.RepeatFloodCopies != RepeatFloodCopies || p.NewKeyPostsPerHour != NewKeyPostsPerHour {
		t.Fatalf("an older body: %+v %v", p, err)
	}
	p, err = parsePostingParams([]byte(`{"anonymous_top_level_per_hour":4,"repeat_flood_copies":5,"new_key_posts_per_hour":60}`))
	if err != nil || p.RepeatFloodCopies != 5 || p.NewKeyPostsPerHour != 60 {
		t.Fatalf("a full body: %+v %v", p, err)
	}
	for _, bad := range []string{`{"anonymous_top_level_per_hour":4,"repeat_flood_copies":1}`, `{"anonymous_top_level_per_hour":4,"new_key_posts_per_hour":0}`, `{"repeat_flood_copies":3}`, `{"anonymous_top_level_per_hour":4,"new_key_rate":3}`} {
		if _, err = parsePostingParams([]byte(bad)); err == nil {
			t.Errorf("posting params %s accepted", bad)
		}
	}
	// The raised rate takes effect.
	s := openTest(t, Config{})
	if _, err = s.SetAllowanceParams(testContext, PostingParamsNamespace, []byte(`{"anonymous_top_level_per_hour":1000000,"repeat_flood_copies":4}`), "test", 0); err != nil {
		t.Fatal(err)
	}
	key := keyFor(148)
	for i := range 3 {
		postAs(t, s, key, Command{Room: "lobby", Text: floodText(i)})
	}
	if _, err = s.Execute(testContext, signedNow(s, key, Command{Operation: "post", Room: "lobby", Text: floodText(3)}), "test-origin"); err == nil {
		t.Fatal("fourth near-copy published under repeat_flood_copies 4")
	}
}

// On a shared network, a key with standing is compared with its own posts
// only: two strangers' near-copies do not refuse it, and its post holds
// nothing.
func TestRepeatFloodSparesStandingOnSharedNetwork(t *testing.T) {
	s := openTest(t, Config{})
	asker := keyFor(143)
	var parents []string
	for i := range 3 {
		parents = append(parents, postAs(t, s, asker, Command{Room: "research", Text: distinctTexts[i]}))
	}
	known := keyFor(180)
	setStanding(t, s, map[string]int64{keyID(known): 200})
	ctx := WithVia(context.Background(), "command")
	post := func(key ed25519.PrivateKey, source string, c Command) error {
		t.Helper()
		c.Operation = "post"
		c.Room = "research"
		c.Timestamp = s.now().Unix()
		_, err := s.Execute(ctx, signed(key, c), source)
		return err
	}
	for i, k := range []byte{181, 182} {
		if err := post(keyFor(k), "198.51.100.7", Command{ReplyTo: parents[i], Text: floodText(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := post(known, "198.51.100.9", Command{ReplyTo: parents[2], Text: floodText(2)}); err != nil {
		t.Fatalf("a key with standing on the shared network: %v", err)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM counters WHERE scope LIKE '"+floodHoldPrefix+"%'"); n != 0 {
		t.Fatalf("holds stored: %d", n)
	}
}

// Reviews of different subjects in one template are not a near-copy flood.
func TestRepeatFloodSkipsReviews(t *testing.T) {
	s := openTest(t, Config{Features: Features{Reviews: true}})
	lab := keyFor(190)
	for i := range 4 {
		c := reviewPost(t, lab, fmt.Sprintf("site:https://svc%d.example.com/", i), map[string]any{}, "")
		c.Text = "Lab grade A 80.4: every probe answered within the budget and matched its documented shape."
		c.Timestamp = s.now().Unix()
		if _, err := s.Execute(testContext, signed(lab, c), "198.51.100.30"); err != nil {
			t.Fatalf("review %d: %v", i, err)
		}
	}
}
