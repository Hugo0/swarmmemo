package board

import (
	"strings"
	"testing"
)

// C813: a signed work.get takes the 8-hex prefix agents quote, as GET /api/work/ID does.
func TestWorkGetTakesUniquePrefix(t *testing.T) {
	s := openTest(t, Config{})
	id := createTestWork(t, s, keyFor(92), "lobby", "request", 0)
	if w := getTestWork(t, s, id[:8]); w.ID != id {
		t.Fatalf("prefix read named %q, want %q", w.ID, id)
	}
	if w := run(t, s, Command{Operation: "work.history", MessageID: id[:12]}); w.Data == nil {
		t.Fatal("work.history by prefix returned no data")
	}
	missing := "ffffffff"
	if id[:8] == missing {
		missing = "eeeeeeee"
	}
	fails(t, s, Command{Operation: "work.get", MessageID: missing}, "not_found")
	fails(t, s, Command{Operation: "work.get", MessageID: id[:7]}, "invalid_message_id")
}

// A work read of a post no task was opened on says what the ID is: a post to
// read with read_thread, or a reply whose task is its parent.
func TestWorkGetOfAPostSaysWhatItIs(t *testing.T) {
	s := openTest(t, Config{})
	task := createTestWork(t, s, keyFor(93), "lobby", "request", 0)
	post := run(t, s, signed(keyFor(94), Command{Operation: "post", Room: "lobby", Text: "Just a note."})).Receipt.ID
	if msg := failsWith(t, s, Command{Operation: "work.get", MessageID: post}, "not_found"); !strings.Contains(msg, "read_thread") {
		t.Fatalf("post: %q", msg)
	}
	reply := run(t, s, signed(keyFor(94), Command{Operation: "post", Room: "lobby", ReplyTo: task, Text: "An answer."})).Receipt.ID
	if msg := failsWith(t, s, Command{Operation: "work.get", MessageID: reply}, "not_found"); !strings.Contains(msg, task) {
		t.Fatalf("reply: %q", msg)
	}
}
