package board

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestExportBlobDeletionCannotBypassOriginalAge(t *testing.T) {
	s := openTest(t, Config{ArchiveDelaySeconds: 100})
	key := keyFor(91)
	run(t, s, signed(key, Command{Operation: "room.create", Room: "public", Visibility: "public"}))
	blob := upload(t, s, key, "public", "temporary", 0)
	post := run(t, s, signed(key, Command{Operation: "post", Room: "public", Text: "young body must wait", Attachments: []string{blob.ID}}))
	initial := run(t, s, Command{Operation: "export"})
	if len(initial.Messages) != 0 {
		t.Fatal("young original exported before its delay")
	}
	run(t, s, signed(key, Command{Operation: "blob.delete", MessageID: blob.ID}))
	early := run(t, s, Command{Operation: "export", Cursor: initial.NextCursor})
	if len(early.Messages) != 0 || early.NextCursor != initial.NextCursor {
		t.Fatal("deleting an attachment made its young parent eligible")
	}
	s.now = func() time.Time { return time.Unix(testTime+100, 0) }
	mature := run(t, s, Command{Operation: "export", Cursor: early.NextCursor})
	if len(mature.Messages) == 0 {
		t.Fatal("deferred original/correction permanently skipped")
	}
	for _, event := range mature.Messages {
		if event.ID != post.Receipt.ID || event.Text != "young body must wait" || event.Hidden || len(event.Attachments) != 1 || !event.Attachments[0].Deleted {
			t.Fatalf("mature correction incomplete: %+v", event)
		}
	}
}

func TestExportYoungHideRestoreKeepsQueuedTombstoneSafe(t *testing.T) {
	s := openTest(t, Config{ArchiveDelaySeconds: 100})
	key := keyFor(92)
	run(t, s, signed(key, Command{Operation: "room.create", Room: "public", Visibility: "public"}))
	blob := upload(t, s, key, "public", "sensitive attachment metadata", 0)
	post := run(t, s, signed(key, Command{Operation: "post", Room: "public", Text: "restored young text", Attachments: []string{blob.ID}}))
	if err := s.Moderate(testContext, post.Receipt.ID, "removed", true); err != nil {
		t.Fatal(err)
	}
	removed := run(t, s, Command{Operation: "export"})
	if len(removed.Messages) != 1 || removed.Messages[0].Type != "tombstone" {
		t.Fatal("young hidden event did not produce an immediate tombstone")
	}
	if err := s.Moderate(testContext, post.Receipt.ID, "restored", false); err != nil {
		t.Fatal(err)
	}
	// Old subscribers must not receive the body early. New subscribers replaying
	// the already allocated tombstone must also see sanitized current state.
	next := run(t, s, Command{Operation: "export", Cursor: removed.NextCursor})
	if len(next.Messages) != 0 || next.NextCursor != removed.NextCursor {
		t.Fatal("restoration bypassed original post age")
	}
	fresh := run(t, s, Command{Operation: "export", Cursor: "start"})
	if len(fresh.Messages) != 1 || fresh.Messages[0].Type != "tombstone" || fresh.Messages[0].Reason != "archive_age_pending" {
		t.Fatal("queued removal exposed a restored young event")
	}
	encoded, _ := json.Marshal(fresh.Messages[0])
	if strings.Contains(string(encoded), "restored young text") || strings.Contains(string(encoded), blob.ID) || fresh.Messages[0].Signature != "" || fresh.Messages[0].SignedPayload != "" {
		t.Fatal("pending restoration retained payload/signature/attachment references")
	}
	s.now = func() time.Time { return time.Unix(testTime+100, 0) }
	mature := run(t, s, Command{Operation: "export", Cursor: removed.NextCursor})
	if len(mature.Messages) == 0 {
		t.Fatal("restoration was lost after the age threshold")
	}
	for _, event := range mature.Messages {
		if event.Hidden || event.Text != "restored young text" || event.Signature == "" || len(event.Attachments) != 1 {
			t.Fatal("mature restoration lost original signed content")
		}
	}
}

func TestExportYoungRestorationBeforeFirstReadStaysDelayed(t *testing.T) {
	s := openTest(t, Config{ArchiveDelaySeconds: 100})
	post := run(t, s, Command{Operation: "post", Text: "not ready"})
	if err := s.Moderate(testContext, post.Receipt.ID, "hide", true); err != nil {
		t.Fatal(err)
	}
	if err := s.Moderate(testContext, post.Receipt.ID, "unhide", false); err != nil {
		t.Fatal(err)
	}
	if events := run(t, s, Command{Operation: "export"}).Messages; len(events) != 0 {
		t.Fatal("unmaterialized old hide exposed current young text")
	}
}

func TestStatsSeparateImportedNativeAndPrivateParticipation(t *testing.T) {
	s := openTest(t, Config{})
	curator, participant := keyFor(93), keyFor(94)
	// The imported kind is reserved to the registered curator account.
	run(t, s, signed(curator, Command{Operation: "agent.register", Handle: curatorHandle}))
	imported := run(t, s, signed(curator, Command{Operation: "post", Kind: "imported", Text: "curator summary"}))
	run(t, s, signed(participant, Command{Operation: "post", Text: "native signed memo"}))
	run(t, s, Command{Operation: "post", Text: "native anonymous memo"})
	run(t, s, signed(curator, Command{Operation: "room.create", Room: "private", Visibility: "private"}))
	run(t, s, signed(curator, Command{Operation: "post", Room: "private", Text: "private native does not count"}))
	run(t, s, signed(curator, Command{Operation: "post", Room: "private", Kind: "imported", Text: "private import does not count"}))
	stats := run(t, s, Command{Operation: "stats"}).Stats
	if stats["messages"] != 3 || stats["imported_messages"] != 1 || stats["native_messages"] != 2 || stats["agents"] != 2 || stats["native_posting_agents"] != 1 {
		t.Fatalf("counts conflate imports, native participation or private activity: %+v", stats)
	}
	if err := s.Moderate(testContext, imported.Receipt.ID, "removed import", true); err != nil {
		t.Fatal(err)
	}
	stats = run(t, s, Command{Operation: "stats"}).Stats
	if stats["messages"] != 2 || stats["imported_messages"] != 0 || stats["native_messages"] != 2 || stats["agents"] != 1 || stats["native_posting_agents"] != 1 {
		t.Fatalf("removed/private import still affects public participation counts: %+v", stats)
	}
}
