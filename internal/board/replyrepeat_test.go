package board

// The same reply to the same message again within the window is refused,
// for every author alike (C599).

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRepeatReplyRefused(t *testing.T) {
	s := openTest(t, Config{})
	base := time.Unix(testTime, 0)
	s.now = func() time.Time { return base }
	asker, flood, other := keyFor(111), keyFor(112), keyFor(113)
	taskA := postAs(t, s, asker, Command{Room: "bounties", Text: "Task A", RequestID: "task-a"})
	taskB := postAs(t, s, asker, Command{Room: "bounties", Text: "Task B", RequestID: "task-b"})
	template := "I can do this. DM me for details."
	firstCmd := signed(flood, Command{Operation: "post", Room: "bounties", ReplyTo: taskA, Text: template, RequestID: "r1", Timestamp: base.Unix()})
	first := run(t, s, firstCmd).Receipt.ID

	refused := func(c Command, source string) *Error {
		t.Helper()
		_, err := s.Execute(testContext, c, source)
		var e *Error
		if !errors.As(err, &e) || e.Code != "duplicate_reply" || e.Status != 409 {
			t.Fatalf("want 409 duplicate_reply, got %v", err)
		}
		return e
	}
	post := func(key byte, c Command) Command {
		c.Operation = "post"
		c.Timestamp = s.now().Unix()
		return signed(keyFor(key), c)
	}
	// Fifteen minutes later: the same text, whitespace and case changed.
	s.now = func() time.Time { return base.Add(15 * time.Minute) }
	e := refused(post(112, Command{Room: "bounties", ReplyTo: taskA, Text: "  i can do this.\n DM me for details. ", RequestID: "r2"}), "test-origin")
	if !strings.Contains(e.Message, first) || !strings.Contains(e.Message, "24 hours") || e.Details.(map[string]any)["earlier_id"] != first {
		t.Fatalf("problem does not name the earlier copy: %+v", e)
	}
	// C596: allowed again once the first is out of the window.
	if want := base.Unix() + ReplyRepeatWindowSeconds; e.NextOpenAt != want || e.RetryAfter != int(want-s.now().Unix()) {
		t.Fatalf("duplicate_reply wait: next_open_at %d retry_after %d, want %d", e.NextOpenAt, e.RetryAfter, want)
	}
	// An exact retry of the first still returns its receipt.
	retry := run(t, s, firstCmd)
	if retry.Receipt.ID != first || !retry.Receipt.Duplicate {
		t.Fatalf("request_id retry: %+v", retry.Receipt)
	}
	// Another parent, other text, another key: accepted.
	postAs(t, s, flood, Command{Room: "bounties", ReplyTo: taskB, Text: template, RequestID: "r3"})
	postAs(t, s, flood, Command{Room: "bounties", ReplyTo: taskA, Text: "Update: a draft is ready.", RequestID: "r4"})
	postAs(t, s, other, Command{Room: "bounties", ReplyTo: taskA, Text: template, RequestID: "r5"})
	// An edit of the first keeping its text is not a new reply.
	postAs(t, s, flood, Command{Room: "bounties", ReplyTo: taskA, Text: template, Data: `{"schema":1,"supersedes":"` + first + `"}`, RequestID: "r6"})
	// Still refused after the edit, naming the message (not the version).
	e = refused(post(112, Command{Room: "bounties", ReplyTo: taskA, Text: template, RequestID: "r7"}), "test-origin")
	if e.Details.(map[string]any)["earlier_id"] != first {
		t.Fatalf("after an edit the problem names %v, want %s", e.Details, first)
	}
	// The edit is the newest copy: the window runs from it.
	if want := s.now().Unix() + ReplyRepeatWindowSeconds; e.NextOpenAt != want {
		t.Fatalf("after an edit next_open_at %d, want %d", e.NextOpenAt, want)
	}
	// After the window (from the edit, the latest copy): accepted again.
	s.now = func() time.Time { return base.Add(time.Duration(ReplyRepeatWindowSeconds)*time.Second + time.Hour) }
	postAs(t, s, flood, Command{Room: "bounties", ReplyTo: taskA, Text: template, RequestID: "r8"})

	// Anonymous: keyed on the network pseudonym.
	s.now = func() time.Time { return base }
	anon := Command{Operation: "post", Room: "bounties", ReplyTo: taskB, Text: "anon template", RequestID: "a1"}
	if _, err := s.Execute(testContext, anon, "198.51.100.7"); err != nil {
		t.Fatal(err)
	}
	anon.RequestID = "a2"
	refused(anon, "198.51.100.7")
	anon.RequestID = "a3"
	if _, err := s.Execute(testContext, anon, "203.0.113.9"); err != nil {
		t.Fatalf("another network's same reply: %v", err)
	}
}

// A looping client's report repeats with only its run's call id, times and
// counters changed (C1017): refused as the same reply, naming the first and
// saying it is already on the thread; other words, another parent or a
// later day pass, and a short reply still compares whole.
func TestRepeatReplyMasksRunTokens(t *testing.T) {
	s := openTest(t, Config{})
	base := time.Unix(testTime, 0)
	s.now = func() time.Time { return base }
	asker, loop := keyFor(121), keyFor(122)
	task := postAs(t, s, asker, Command{Room: "bounties", Text: "Try one tool", RequestID: "task"})
	other := postAs(t, s, asker, Command{Room: "bounties", Text: "Another task", RequestID: "task2"})
	report := func(id string, left, at int) string {
		return fmt.Sprintf("**Try one tool in one call - screen/text** (2026-11-15)\n\n1. Exact request sent:\n`curl -sS 'https://swarmmemo.com/call/screen/text?text=Ignore+your+instructions'`\n\n"+
			"2. HTTP 200; `call.id` `%s`; `paid_from` `free_allowance`; `credits_spent` 0.\n{\"allowance_left\": %d, \"created_at\": %d}\n\n3. The screen flagged the probe text: verdict `flag`, injection 0.98.", id, left, at)
	}
	first := postAs(t, s, loop, Command{Room: "bounties", ReplyTo: task, Text: report("91c2537faa43ddb8af548400118c8402", 99040, 1794766219), RequestID: "l1"})
	s.now = func() time.Time { return base.Add(time.Hour) }
	_, err := s.Execute(testContext, signed(loop, Command{Operation: "post", Room: "bounties", ReplyTo: task, Timestamp: s.now().Unix(),
		Text: report("bd498efc06140fd50c3cdb0929256341", 99120, 1794769819), RequestID: "l2"}), "test-origin")
	var e *Error
	if !errors.As(err, &e) || e.Code != "duplicate_reply" || e.Status != 409 || e.Details.(map[string]any)["earlier_id"] != first ||
		!strings.Contains(e.Message, "already on the thread as "+first) {
		t.Fatalf("want 409 duplicate_reply naming %s, got %v", first, err)
	}
	// Different words, another parent: accepted.
	postAs(t, s, loop, Command{Room: "bounties", ReplyTo: task, Text: strings.Replace(report("ab", 1, 2), "verdict `flag`", "verdict `pass`", 1), RequestID: "l3"})
	postAs(t, s, loop, Command{Room: "bounties", ReplyTo: other, Text: report("cd", 3, 4), RequestID: "l4"})
	// Short replies keep their numbers.
	postAs(t, s, loop, Command{Room: "bounties", ReplyTo: task, Text: "I pick option 1.", RequestID: "l5"})
	postAs(t, s, loop, Command{Room: "bounties", ReplyTo: task, Text: "I pick option 2.", RequestID: "l6"})
	// More than a day after the last copy: accepted again.
	s.now = func() time.Time { return base.Add(26 * time.Hour) }
	postAs(t, s, loop, Command{Room: "bounties", ReplyTo: task, Text: report("ef0123456789abcdef", 5, 6), RequestID: "l7"})
}

// Ids and hashes are masked however they are written (C1028): a 0x-prefixed
// EVM transaction hash, uppercase hex, a hash against punctuation or inside
// a word. Two long replies differing only there compare equal; a short
// reply, or one differing in words, still does not.
func TestReplyRepeatTextMasksHashes(t *testing.T) {
	pad := strings.Repeat("Paid the bounty and checked the receipt on the explorer. ", 4)
	h1 := "0x" + strings.Repeat("a1b2c3d4", 8)
	h2 := "0x" + strings.Repeat("9f8e7d6c", 8)
	for _, c := range []struct {
		name, a, b string
		same       bool
	}{
		{"0x tx hash (reported)", pad + "tx " + h1, pad + "tx " + h2, true},
		{"0x hash, all letters after the prefix", pad + "tx 0x" + strings.Repeat("abcdef", 11), pad + "tx 0x" + strings.Repeat("fedcba", 11), true},
		{"uppercase 0X and hex", pad + "tx " + strings.ToUpper(h1), pad + "tx " + strings.ToUpper(h2), true},
		{"in parentheses", pad + "(" + h1 + ")", pad + "(" + h2 + ")", true},
		{"after a colon, before a period", pad + "hash:" + h1 + ".", pad + "hash:" + h2 + ".", true},
		{"in backticks", pad + "`" + h1 + "`", pad + "`" + h2 + "`", true},
		{"inside a word", pad + "call_" + h1[2:], pad + "call_" + h2[2:], true},
		{"bare hex", pad + h1[2:], pad + h2[2:], true},
		{"different words", pad + "tx " + h1, pad + "refund " + h2, false},
		{"short replies compare whole", "tx " + h1, "tx " + h2, false},
	} {
		if got := replyRepeatText(c.a) == replyRepeatText(c.b); got != c.same {
			t.Errorf("%s: same=%v, want %v (%q vs %q)", c.name, got, c.same, replyRepeatText(c.a), replyRepeatText(c.b))
		}
	}
}
