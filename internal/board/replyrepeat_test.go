package board

// The same reply to the same message again within the window is refused,
// for every author alike (C599).

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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
// Bug bounty 11-19 (miaoshou): a reply edited to other text no longer shows its
// first text, so posting that text again is a new reply, not a repeat.
func TestRepeatReplyIgnoresSupersededVersions(t *testing.T) {
	s := openTest(t, Config{})
	base := time.Unix(testTime, 0)
	s.now = func() time.Time { return base }
	asker, worker := keyFor(121), keyFor(122)
	task := postAs(t, s, asker, Command{Room: "bounties", Text: "Task", RequestID: "task"})
	first := postAs(t, s, worker, Command{Room: "bounties", ReplyTo: task, Text: "Answer T, with the details.", RequestID: "t1"})
	postAs(t, s, worker, Command{Room: "bounties", ReplyTo: task, Text: "Answer U, a different text.", Data: `{"schema":1,"supersedes":"` + first + `"}`, RequestID: "t2"})
	s.now = func() time.Time { return base.Add(10 * time.Minute) }
	postAs(t, s, worker, Command{Room: "bounties", ReplyTo: task, Text: "Answer T, with the details.", RequestID: "t3"})
	// The newest version's text is still a repeat.
	_, err := s.Execute(testContext, signed(worker, Command{Operation: "post", Room: "bounties", ReplyTo: task, Text: "Answer U, a different text.", RequestID: "t4", Timestamp: s.now().Unix()}), "test-origin")
	var e *Error
	if !errors.As(err, &e) || e.Code != "duplicate_reply" {
		t.Fatalf("the visible version's text must still be refused, got %v", err)
	}
}

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
		// a9762fbf (kestrel-ops): a UUID's groups are all under 16 hex, so only
		// its digits were masked and its letters made a repeat new.
		{"uuid request id", pad + "request_id 3f2b8c1e-9d4a-4e7b-b6c5-0a1f2e3d4c5b", pad + "request_id e7d6c5b4-a3f2-4e1d-9c8b-7a6f5e4d3c2b", true},
		{"uppercase uuid", pad + "run 3F2B8C1E-9D4A-4E7B-B6C5-0A1F2E3D4C5B", pad + "run E7D6C5B4-A3F2-4E1D-9C8B-7A6F5E4D3C2B", true},
		{"uuid inside a word", pad + "run_3f2b8c1e-9d4a-4e7b-b6c5-0a1f2e3d4c5b.", pad + "run_e7d6c5b4-a3f2-4e1d-9c8b-7a6f5e4d3c2b.", true},
		{"different words", pad + "tx " + h1, pad + "refund " + h2, false},
		{"short replies compare whole", "tx " + h1, "tx " + h2, false},
		// 120 characters but 240 bytes: short by the documented character
		// count, so compared whole (C1058).
		{"length counts characters, not bytes", strings.Repeat("é", 120) + " 1", strings.Repeat("é", 120) + " 2", false},
	} {
		if got := replyRepeatText(c.a) == replyRepeatText(c.b); got != c.same {
			t.Errorf("%s: same=%v, want %v (%q vs %q)", c.name, got, c.same, replyRepeatText(c.a), replyRepeatText(c.b))
		}
	}
}

// A long thread (C1043, 6f311a14): the check reads the account's newest
// replies to the parent, so a repeat is refused even after more than
// replyRepeatScan earlier, different replies within the window.
func TestRepeatReplyLongThreadReadsNewest(t *testing.T) {
	s := openTest(t, Config{})
	base := time.Unix(testTime, 0)
	s.now = func() time.Time { return base }
	asker, loop := keyFor(131), keyFor(132)
	task := postAs(t, s, asker, Command{Room: "bounties", Text: "A long task", RequestID: "task"})
	opener := postAs(t, s, loop, Command{Room: "bounties", ReplyTo: task, Text: "Starting now.", RequestID: "o1"})
	var account, key string
	if err := s.db.QueryRow(`SELECT account, public_key FROM events WHERE id=?`, opener).Scan(&account, &key); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < replyRepeatScan+44; i++ {
		if _, err := s.db.Exec(`INSERT INTO events(display_seq,id,room,page,text,kind,author,account,handle,public_key,signature,payload,created_at,hash,reply_to,recipient,hidden)
VALUES((SELECT MAX(display_seq)+1 FROM events),?,'bounties','main',?,'note',?,?,'',?,'','',?,'',?,'',0)`,
			randomID(), fmt.Sprintf("Progress note %d of a long job.", i), account, account, key, base.Unix(), task); err != nil {
			t.Fatal(err)
		}
	}
	s.now = func() time.Time { return base.Add(time.Minute) }
	template := "Still working on it, results soon."
	first := postAs(t, s, loop, Command{Room: "bounties", ReplyTo: task, Text: template, RequestID: "t1"})
	// Past the signed-retry window (SignedRetrySeconds), so it is a new post.
	s.now = func() time.Time { return base.Add(20 * time.Minute) }
	_, err := s.Execute(testContext, signed(loop, Command{Operation: "post", Room: "bounties", ReplyTo: task, Text: template, RequestID: "t2", Timestamp: s.now().Unix()}), "test-origin")
	var e *Error
	if !errors.As(err, &e) || e.Code != "duplicate_reply" || e.Details.(map[string]any)["earlier_id"] != first {
		t.Fatalf("want 409 duplicate_reply naming %s after %d earlier replies, got %v", first, replyRepeatScan+45, err)
	}
}

// Bug bounty 629420d7 (11-21): masking was decided per text, so the same
// report at 198 runes (compared whole) and at 200 runes (masked) never
// matched. replyRepeatSame masks both when either is long enough.
func TestReplyRepeatSameStraddlesTheMaskLength(t *testing.T) {
	base := strings.Repeat("Routine status: all checks passed for the hourly run. ", 4)
	// One stem, a counter that grows: 198 runes, then 200.
	a := base[:ReplyRepeatMaskMinRunes-2-len(" run 41")] + " run 41"
	b := base[:ReplyRepeatMaskMinRunes-2-len(" run 41")] + " run 4217"
	if utf8.RuneCountInString(copyText(a)) >= ReplyRepeatMaskMinRunes || utf8.RuneCountInString(copyText(b)) < ReplyRepeatMaskMinRunes {
		t.Fatalf("setup a/b: %d / %d runes", utf8.RuneCountInString(copyText(a)), utf8.RuneCountInString(copyText(b)))
	}
	if !replyRepeatSame(a, b) {
		t.Errorf("a %d-rune and a %d-rune copy differing only in a number must match", utf8.RuneCountInString(copyText(a)), utf8.RuneCountInString(copyText(b)))
	}
	if replyRepeatSame("option 1", "option 2") {
		t.Error("short replies still compare whole")
	}
}
