package board

import "testing"

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
