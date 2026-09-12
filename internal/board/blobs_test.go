package board

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func upload(t *testing.T, s *Store, key ed25519.PrivateKey, room, text string, ttl int64) Attachment {
	t.Helper()
	r := run(t, s, signed(key, Command{Operation: "blob.put", Room: room, Data: base64.RawURLEncoding.EncodeToString([]byte(text)), Filename: "sample.txt", MediaType: "text/plain", TTL: ttl}))
	return r.Data["blob"].(Attachment)
}

func TestAttachmentPrivacyLifecycleAndArchive(t *testing.T) {
	s := openTest(t, Config{ArchiveDelaySeconds: -1})
	owner := keyFor(31)
	stranger := keyFor(32)
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "private", Visibility: "private"}))
	secret := upload(t, s, owner, "private", "private-bytes", 0)
	fails(t, s, Command{Operation: "blob.get", MessageID: secret.ID}, "not_found")
	fails(t, s, signed(stranger, Command{Operation: "blob.get", MessageID: secret.ID}), "not_found")
	got := run(t, s, signed(owner, Command{Operation: "blob.get", MessageID: secret.ID}))
	if got.Data["data"] != base64.RawURLEncoding.EncodeToString([]byte("private-bytes")) {
		t.Fatal("private download bytes differ")
	}
	run(t, s, signed(owner, Command{Operation: "post", Room: "private", Text: "private-file", Attachments: []string{secret.ID}}))
	run(t, s, Command{Operation: "post", Room: "public", Text: "hello"})
	fails(t, s, signed(owner, Command{Operation: "post", Room: "public", Text: "bad leak", Attachments: []string{secret.ID}}), "not_found")
	first := upload(t, s, owner, "public", "first-file", 0)
	second := upload(t, s, owner, "public", "second-file", 0)
	post := run(t, s, signed(owner, Command{Operation: "post", Room: "public", Text: "public-file", Attachments: []string{second.ID, first.ID}}))
	export := run(t, s, Command{Operation: "export"})
	encoded, _ := json.Marshal(export)
	if strings.Contains(string(encoded), secret.ID) || strings.Contains(string(encoded), "private-bytes") || strings.Contains(string(encoded), "first-file") {
		t.Fatal("archive leaked private or binary bytes")
	}
	last := export.Messages[len(export.Messages)-1]
	if len(last.Attachments) != 2 || last.Attachments[0].ID != second.ID {
		t.Fatalf("attachment order changed: %+v", last.Attachments)
	}
	fails(t, s, signed(stranger, Command{Operation: "blob.delete", MessageID: first.ID}), "owner_required")
	run(t, s, signed(owner, Command{Operation: "blob.delete", MessageID: first.ID}))
	fails(t, s, Command{Operation: "blob.get", MessageID: first.ID}, "attachment_gone")
	correction := run(t, s, Command{Operation: "export", Cursor: export.NextCursor})
	if len(correction.Messages) != 1 || !correction.Messages[0].Attachments[1].Deleted {
		t.Fatal("attachment deletion export correction missing")
	}
	if err := s.Moderate(testContext, post.Receipt.ID, "unsafe file", true); err != nil {
		t.Fatal(err)
	}
	fails(t, s, Command{Operation: "blob.get", MessageID: second.ID}, "not_found")
	fails(t, s, signed(owner, Command{Operation: "post", Room: "public", Text: "republish", Attachments: []string{second.ID}}), "not_found")
	if err := s.Moderate(testContext, post.Receipt.ID, "restored", false); err != nil {
		t.Fatal(err)
	}
	run(t, s, Command{Operation: "blob.get", MessageID: second.ID})
}

func TestAttachmentBoundsExpiryAndNoQuotaDoubleSpend(t *testing.T) {
	s := openTest(t, Config{})
	key := keyFor(35)
	run(t, s, Command{Operation: "post", Room: "files", Text: "attachments"})
	fails(t, s, Command{Operation: "blob.put", Room: "files", Data: "eA"}, "signature_required")
	fails(t, s, signed(key, Command{Operation: "blob.put", Room: "files", Data: "eA=="}), "invalid_base64")
	fails(t, s, signed(key, Command{Operation: "blob.put", Room: "files", Data: "eA", Filename: "../secret"}), "invalid_filename")
	fails(t, s, signed(key, Command{Operation: "blob.put", Room: "files", Data: "eA", TTL: 2592001}), "invalid_ttl")
	large := signed(key, Command{Operation: "blob.put", Room: "files", Data: base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("x", 1<<20))), RequestID: "full-size"})
	result := run(t, s, large)
	used := run(t, s, signed(key, Command{Operation: "quota.get"})).Data["used_bytes"]
	retry := run(t, s, large)
	if retry.Data["blob"].(map[string]any)["id"] != result.Data["blob"].(Attachment).ID {
		t.Fatal("retry did not preserve attachment")
	}
	if got := run(t, s, signed(key, Command{Operation: "quota.get"})).Data["used_bytes"]; got != used {
		t.Fatal("attachment retry double charge")
	}
	expiring := upload(t, s, key, "files", "temporary", 1)
	s.now = func() time.Time { return time.Unix(testTime+2, 0) }
	fails(t, s, Command{Operation: "blob.get", MessageID: expiring.ID}, "attachment_gone")
	n, err := s.PruneExpiredBlobs(testContext)
	if err != nil || n != 1 {
		t.Fatalf("expiry prune %d %v", n, err)
	}
	var retained int
	if err = s.db.QueryRow("SELECT count(*) FROM blobs WHERE id=? AND data IS NULL", expiring.ID).Scan(&retained); err != nil || retained != 1 {
		t.Fatal("expiry left bytes or removed metadata")
	}
}

func TestPublicSequenceDoesNotRevealPrivateTraffic(t *testing.T) {
	s := openTest(t, Config{})
	key := keyFor(40)
	run(t, s, Command{Operation: "post", Text: "public-one"})
	run(t, s, signed(key, Command{Operation: "room.create", Room: "hidden", Visibility: "private"}))
	for i := 0; i < 4; i++ {
		run(t, s, signed(key, Command{Operation: "post", Room: "hidden", Text: fmt.Sprint("secret-", i)}))
	}
	run(t, s, Command{Operation: "post", Text: "public-two"})
	r := run(t, s, Command{Operation: "messages.list"})
	if len(r.Messages) != 2 || r.Messages[0].Sequence != 1 || r.Messages[1].Sequence != 2 {
		t.Fatal("private posts left public sequence gaps")
	}
	parts := strings.Split(r.NextCursor, ":")
	if len(parts) != 2 || len(parts[1]) < 40 {
		t.Fatal("cursor exposes raw sequence")
	}
	tampered := r.NextCursor[:len(r.NextCursor)-3] + "AAA"
	fails(t, s, Command{Operation: "messages.list", Cursor: tampered}, "invalid_cursor")
	unchanged := run(t, s, Command{Operation: "messages.list", Cursor: r.NextCursor})
	if unchanged.NextCursor != r.NextCursor {
		t.Fatal("empty page moved cursor")
	}
	fails(t, s, Command{Operation: "export", Cursor: r.NextCursor}, "invalid_cursor")
	private := run(t, s, signed(key, Command{Operation: "messages.list", Room: "hidden"}))
	if private.Messages[0].Sequence != 1 || private.Messages[3].Sequence != 4 {
		t.Fatal("private room sequence is not local")
	}
}

func TestReportsDoNotHideAndImmediateCorrectionsDoNotSkipYoungPosts(t *testing.T) {
	s := openTest(t, Config{ArchiveDelaySeconds: 100})
	old := run(t, s, Command{Operation: "post", Text: "old"})
	s.now = func() time.Time { return time.Unix(testTime+101, 0) }
	export := run(t, s, Command{Operation: "export"})
	if len(export.Messages) != 1 {
		t.Fatal("old export missing")
	}
	young := run(t, s, Command{Operation: "post", Text: "young"})
	run(t, s, Command{Operation: "report", MessageID: old.Receipt.ID, Reason: "untrusted report"})
	if run(t, s, Command{Operation: "message.get", MessageID: old.Receipt.ID}).Messages[0].Hidden {
		t.Fatal("report hid message")
	}
	if err := s.Moderate(testContext, old.Receipt.ID, "moderator removed", true); err != nil {
		t.Fatal(err)
	}
	correction := run(t, s, Command{Operation: "export", Cursor: export.NextCursor})
	if len(correction.Messages) != 1 || correction.Messages[0].Type != "tombstone" {
		t.Fatal("correction was delayed")
	}
	s.now = func() time.Time { return time.Unix(testTime+202, 0) }
	later := run(t, s, Command{Operation: "export", Cursor: correction.NextCursor})
	if len(later.Messages) != 1 || later.Messages[0].ID != young.Receipt.ID {
		t.Fatal("young post skipped after immediate correction")
	}
}

func TestFeedByteBoundsDoNotSkip(t *testing.T) {
	s := openTest(t, Config{})
	for i := 0; i < 12; i++ {
		run(t, s, Command{Operation: "post", Text: fmt.Sprintf("%02d", i) + strings.Repeat("x", 8190)})
	}
	cursor := "start"
	seen := map[string]bool{}
	for page := 0; page < 20; page++ {
		r := run(t, s, Command{Operation: "messages.list", Cursor: cursor, Limit: 999999})
		if len(r.Messages) == 0 {
			break
		}
		bytes := 0
		for _, e := range r.Messages {
			b, _ := json.Marshal(e)
			bytes += len(b)
			if seen[e.ID] {
				t.Fatal("repeated event")
			}
			seen[e.ID] = true
		}
		if bytes > 64<<10 {
			t.Fatalf("feed exceeded byte budget: %d", bytes)
		}
		cursor = r.NextCursor
	}
	if len(seen) != 12 {
		t.Fatalf("byte-limited feed skipped messages: %d", len(seen))
	}
}

func TestExplicitPrivateIntentNeverCreatesPublicContent(t *testing.T) {
	s := openTest(t, Config{ArchiveDelaySeconds: -1})
	key := keyFor(49)
	fails(t, s, Command{Operation: "post", Room: "new-private", Visibility: "private", Text: "must not leak"}, "private_room_required")
	fails(t, s, Command{Operation: "post", Room: "new-private", Members: []string{keyID(key)}, Text: "must not leak"}, "unexpected_field")
	fails(t, s, signed(key, Command{Operation: "agent.register", Visibility: "private", Handle: "secret-alias"}), "unexpected_field")
	run(t, s, Command{Operation: "post", Room: "public", Text: "public room"})
	fails(t, s, signed(key, Command{Operation: "post", Room: "public", Visibility: "private", Text: "must not leak"}), "visibility_mismatch")
	fails(t, s, signed(key, Command{Operation: "blob.put", Room: "public", Visibility: "private", Data: "eA"}), "visibility_mismatch")
	run(t, s, signed(key, Command{Operation: "room.create", Room: "private", Visibility: "private"}))
	fails(t, s, signed(key, Command{Operation: "post", Room: "private", Visibility: "public", Text: "must not leak"}), "visibility_mismatch")
	run(t, s, signed(key, Command{Operation: "post", Room: "private", Visibility: "private", Text: "permitted private"}))
	all := run(t, s, Command{Operation: "export"})
	data, _ := json.Marshal(all)
	if strings.Contains(string(data), "must not leak") || strings.Contains(string(data), "permitted private") {
		t.Fatal("privacy intent leaked into public export")
	}
}

func TestDiskFullRollsBackAdmissionAndKeepsAcceptedMessages(t *testing.T) {
	s := openTest(t, Config{})
	key := keyFor(50)
	accepted := run(t, s, Command{Operation: "post", Room: "files", Text: "durable before disk full"})
	register(t, s, key)
	before := run(t, s, signed(key, Command{Operation: "quota.get"})).Data["remaining_bytes"]
	var pages int64
	if err := s.db.QueryRow("PRAGMA page_count").Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(fmt.Sprintf("PRAGMA max_page_count=%d", pages)); err != nil {
		t.Fatal(err)
	}
	_, err := s.Execute(testContext, signed(key, Command{Operation: "blob.put", Room: "files", Data: base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("x", 1<<20)))}), "origin")
	if err == nil {
		t.Fatal("disk-full write unexpectedly succeeded")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "full") {
		t.Fatalf("expected disk full, got %v", err)
	}
	if _, err = s.db.Exec("PRAGMA max_page_count=1073741823"); err != nil {
		t.Fatal(err)
	}
	after := run(t, s, signed(key, Command{Operation: "quota.get"})).Data["remaining_bytes"]
	if before != after {
		t.Fatalf("failed durable write spent allowance %v -> %v", before, after)
	}
	read := run(t, s, Command{Operation: "message.get", MessageID: accepted.Receipt.ID})
	if read.Messages[0].Text != "durable before disk full" {
		t.Fatal("prior accepted message lost")
	}
}
