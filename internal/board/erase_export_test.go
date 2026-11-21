package board

import (
	"encoding/json"
	"strings"
	"testing"
)

// C1165: an erasure reaches the Hugging Face dataset. The publisher holds no
// database: it keeps every row /v1/export ever gave it and overwrites a row
// by id when the export sends that id again (scripts/publish_hf.py build).
// So every archived row the erasure changes must be sent again: the visible
// posts as tombstones hidden by their author, and a post a moderator hid
// earlier, whose archived tombstone still names the handle, as a tombstone
// without it. A replay sends nothing new.
func TestEraseReachesTheArchive(t *testing.T) {
	s := openTest(t, Config{ArchiveDelaySeconds: -1})
	alice, bob := keyFor(211), keyFor(212)
	a := keyID(alice)
	register(t, s, bob)
	run(t, s, signed(alice, Command{Operation: "agent.register", Handle: "alice-archive"}))
	kept := run(t, s, signed(alice, Command{Operation: "post", Room: "lobby", Text: "alice archived words", RequestID: "x1"})).Receipt.ID
	moderated := run(t, s, signed(alice, Command{Operation: "post", Room: "lobby", Text: "alice moderated words", RequestID: "x2"})).Receipt.ID
	reply := run(t, s, signed(bob, Command{Operation: "post", Room: "lobby", Text: "bob archived reply", ReplyTo: kept, RequestID: "x3"})).Receipt.ID

	// The publisher's current state: the last row the export gave per id.
	archive, cursor := map[string]Message{}, ""
	publish := func() int {
		t.Helper()
		n := 0
		for page := 0; page < 100; page++ {
			res := run(t, s, Command{Operation: "export", Cursor: cursor})
			for _, m := range res.Messages {
				archive[m.ID] = m
			}
			n += len(res.Messages)
			if len(res.Messages) == 0 || res.NextCursor == cursor {
				return n
			}
			cursor = res.NextCursor
		}
		t.Fatal("export did not end")
		return n
	}
	publish()
	if archive[kept].Text != "alice archived words" || archive[kept].Handle != "alice-archive" {
		t.Fatalf("archived post before erasure: %+v", archive[kept])
	}
	if err := s.Moderate(testContext, moderated, "off topic", true); err != nil {
		t.Fatal(err)
	}
	publish()
	if m := archive[moderated]; m.Type != "tombstone" || m.Text != "" || m.Handle != "alice-archive" {
		t.Fatalf("moderated tombstone before erasure: %+v", m)
	}

	if _, err := s.OperatorEraseAccount(testContext, a, "req-archive", true); err != nil {
		t.Fatal(err)
	}
	publish()
	for _, id := range []string{kept, moderated} {
		m := archive[id]
		if m.Type != "tombstone" || m.Text != "" || m.Signature != "" || m.SignedPayload != "" || m.Handle != "" || !m.Hidden {
			t.Fatalf("archived %s after erasure: %+v", id, m)
		}
	}
	if m := archive[kept]; m.HiddenBy != hiddenByAuthor || m.Reason != ErasedReason {
		t.Fatalf("erased post: %q %q", m.HiddenBy, m.Reason)
	}
	if m := archive[moderated]; m.HiddenBy != hiddenByOperator || m.Reason != "off topic" {
		t.Fatalf("a moderated post keeps its decision: %q %q", m.HiddenBy, m.Reason)
	}
	if m := archive[reply]; m.Text != "bob archived reply" || m.ReplyTo != kept {
		t.Fatalf("another member's reply: %+v", m)
	}
	encoded, _ := json.Marshal(archive)
	for _, leak := range []string{"alice archived", "alice moderated", "alice-archive"} {
		if strings.Contains(string(encoded), leak) {
			t.Fatalf("the archive keeps %q after erasure", leak)
		}
	}

	// A replay (a restored copy erased again) sends nothing new.
	if _, err := s.OperatorEraseAccount(testContext, a, "req-archive-replay", true); err != nil {
		t.Fatal(err)
	}
	if n := publish(); n != 0 {
		t.Fatalf("a replay sent %d export rows", n)
	}
}
