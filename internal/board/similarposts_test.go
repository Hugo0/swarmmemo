package board

// A key's burst of similar top-level posts in one room is marked on a
// messages.list page, never dropped (C601).

import (
	"fmt"
	"testing"
	"time"
)

func TestSimilarPrefix(t *testing.T) {
	a := similarPrefix("x402 Trust's public grade for Weather API is A (score 92): https://x402trust.example/r/1")
	b := similarPrefix("X402 Trust’s public grade for Geo Lookup is C (score 41): https://x402trust.example/r/27")
	if a == "" || a != b {
		t.Fatalf("same template, different prefixes: %q, %q", a, b)
	}
	for _, short := range []string{"", "ok", "thanks!", "https://a.example/x 123", "1 2 3 4 5 6 7 8 9"} {
		if p := similarPrefix(short); p != "" {
			t.Fatalf("%q has prefix %q; short texts never fold", short, p)
		}
	}
	if similarPrefix("Question about rate limits on the API") == similarPrefix("Question about the moderation log") {
		t.Fatal("different posts share a prefix")
	}
}

// The 10-26 flood: one key, 30 graded notes in seconds, newest first.
func TestFoldSimilarImporterFlood(t *testing.T) {
	var page []Message
	for i := 29; i >= 0; i-- {
		page = append(page, Message{ID: fmt.Sprintf("m%02d", i), Sequence: int64(7070 + i), Room: "reviews", Visibility: "public", PublicKey: "importer",
			CreatedAt: testTime + int64(i), Text: fmt.Sprintf("x402 Trust's public grade for service %d is B (score %d)", i, 60+i)})
	}
	out := foldSimilar(page)
	if &out[0] == &page[0] || page[0].Similar != nil {
		t.Fatal("foldSimilar modified its input")
	}
	head := out[0]
	if head.ID != "m29" || head.Similar == nil || head.Similar.Count != 29 || len(head.Similar.IDs) != CopyIDsMax || head.Similar.IDs[0] != "m28" {
		t.Fatalf("head %s similar %+v", head.ID, head.Similar)
	}
	entries := 0
	for _, m := range out[1:] {
		if m.SimilarTo != head.ID || m.Similar != nil {
			t.Fatalf("%s: similar_to %q, similar %+v", m.ID, m.SimilarTo, m.Similar)
		}
	}
	for _, m := range out {
		if m.SimilarTo == "" {
			entries++
		}
	}
	if len(out) != 30 || entries != 1 {
		t.Fatalf("%d messages, %d entries shown", len(out), entries)
	}
}

func TestFoldSimilarLeavesOrdinaryPagesAlone(t *testing.T) {
	msg := func(id, key, room, text string, at int64) Message {
		return Message{ID: id, Room: room, Visibility: "public", PublicKey: key, CreatedAt: testTime + at, Text: text}
	}
	same := "Daily status report for the lab, run "
	task := func(id string, at int64) Message {
		m := msg(id, "k", "r", same+id, at)
		m.Work = &MessageWork{State: "open"}
		return m
	}
	cases := map[string][]Message{
		"two are not a burst":  {msg("a", "k", "r", same+"1", 0), msg("b", "k", "r", same+"2", 1)},
		"different keys":       {msg("a", "k", "r", same+"1", 0), msg("b", "j", "r", same+"2", 1), msg("c", "l", "r", same+"3", 2)},
		"different rooms":      {msg("a", "k", "r", same+"1", 0), msg("b", "k", "s", same+"2", 1), msg("c", "k", "t", same+"3", 2)},
		"past the window":      {msg("a", "k", "r", same+"1", 0), msg("b", "k", "r", same+"2", 300), msg("c", "k", "r", same+"3", SimilarWindowSeconds+1)},
		"another key between":  {msg("a", "k", "r", same+"1", 0), msg("b", "k", "r", same+"2", 1), msg("x", "j", "r", "An unrelated question here", 2), msg("c", "k", "r", same+"3", 3)},
		"different texts":      {msg("a", "k", "r", "Morning everyone, a question", 0), msg("b", "k", "r", "Here is the tool I built today", 1), msg("c", "k", "r", "Another thought on memory", 2)},
		"anonymous":            {msg("a", "", "r", same+"1", 0), msg("b", "", "r", same+"2", 1), msg("c", "", "r", same+"3", 2)},
		"replies and versions": {msg("a", "k", "r", same+"1", 0), {ID: "b", Room: "r", Visibility: "public", PublicKey: "k", CreatedAt: testTime + 1, Text: same + "2", ReplyTo: "a"}, {ID: "c", Room: "r", Visibility: "public", PublicKey: "k", CreatedAt: testTime + 2, Text: same + "3", Supersedes: "a"}},
		"tasks":                {task("a", 0), task("b", 1), task("c", 2)},
		"private room":         {{ID: "a", Room: "r", Visibility: "private", PublicKey: "k", Text: same + "1"}, {ID: "b", Room: "r", Visibility: "private", PublicKey: "k", Text: same + "2"}, {ID: "c", Room: "r", Visibility: "private", PublicKey: "k", Text: same + "3"}},
	}
	for name, page := range cases {
		for _, m := range foldSimilar(page) {
			if m.Similar != nil || m.SimilarTo != "" {
				t.Fatalf("%s: %s marked (%+v, %q)", name, m.ID, m.Similar, m.SimilarTo)
			}
		}
	}
	// A reply in between does not break a burst.
	page := []Message{msg("a", "k", "r", same+"1", 0), {ID: "x", Room: "r", Visibility: "public", PublicKey: "j", CreatedAt: testTime + 1, Text: "a reply", ReplyTo: "a"}, msg("b", "k", "r", same+"2", 2), msg("c", "k", "r", same+"3", 3)}
	out := foldSimilar(page)
	if out[0].Similar == nil || out[0].Similar.Count != 2 || out[1].SimilarTo != "" || out[2].SimilarTo != "a" || out[3].SimilarTo != "a" {
		t.Fatalf("burst around a reply: %+v", out)
	}
}

// Through the store: every post stays on the page, in order and ranked; the
// marks are the same on each.
func TestMessagesListMarksSimilarBurst(t *testing.T) {
	s := openTest(t, Config{})
	base := time.Unix(testTime, 0)
	importer, other := keyFor(111), keyFor(112)
	var ids []string
	for i := range 6 {
		s.now = func() time.Time { return base.Add(time.Duration(i) * time.Second) }
		ids = append(ids, postAs(t, s, importer, Command{Room: "lobby", Text: fmt.Sprintf("x402 Trust's public grade for tool %d is A", i) + testPostSuffix(), RequestID: fmt.Sprintf("flood-%d", i)}))
	}
	s.now = func() time.Time { return base.Add(time.Hour) }
	lone := postAs(t, s, other, Command{Room: "lobby", Text: "x402 Trust's public grade for tool 9 is F", RequestID: "other"})
	for _, data := range []string{`{"sort":"new"}`, `{"sort":"hot"}`} {
		page := run(t, s, Command{Operation: "messages.list", Room: "lobby", Data: data, Limit: 40}).Messages
		if len(page) != 7 {
			t.Fatalf("%s: %d messages, want all 7", data, len(page))
		}
		var head *Message
		for i := range page {
			if page[i].Similar != nil {
				if head != nil {
					t.Fatalf("%s: two heads", data)
				}
				head = &page[i]
			}
		}
		if head == nil || head.Similar.Count != 5 {
			t.Fatalf("%s: head %+v", data, head)
		}
		for _, m := range page {
			switch {
			case m.ID == lone && m.SimilarTo != "":
				t.Fatalf("%s: another key's post folded", data)
			case m.ID != lone && m.ID != head.ID && m.SimilarTo != head.ID:
				t.Fatalf("%s: %s not marked", data, m.ID)
			}
		}
		if data == `{"sort":"new"}` && head.ID != ids[5] {
			t.Fatalf("newest first: the newest leads, got %s", head.ID)
		}
	}
}
