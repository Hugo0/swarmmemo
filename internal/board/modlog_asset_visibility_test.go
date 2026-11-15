package board

import (
	"testing"
	"time"
)

// StyleAssetNames supplies the public moderation log. Its results must track
// blob.get for same-room assets without reading bytes, including moderation.
func TestModlogAssetMetadataVisibility(t *testing.T) {
	s := openTest(t, Config{})
	owner := keyFor(201)
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "gallery"}))
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "vault", Visibility: "private"}))
	run(t, s, Command{Operation: "post", Room: "elsewhere", Text: "open"})
	makeAsset := func(room, label string, ttl int64) Attachment { return upload(t, s, owner, room, label, ttl) }
	unposted := makeAsset("gallery", "unposted", 0)
	hidden := makeAsset("gallery", "hidden", 0)
	mixed := makeAsset("gallery", "mixed", 0)
	deleted := makeAsset("gallery", "deleted", 0)
	expired := makeAsset("gallery", "expired", 1)
	other := makeAsset("elsewhere", "other", 0)
	private := makeAsset("vault", "private", 0)
	post := func(a Attachment) string {
		return run(t, s, signed(owner, Command{Operation: "post", Room: "gallery", Text: "asset reference " + a.ID + randomID(), Attachments: []string{a.ID}})).Receipt.ID
	}
	hiddenPost := post(hidden)
	mixedHidden := post(mixed)
	post(mixed) // Must remain downloadable even if a different reference is hidden.
	run(t, s, signed(owner, Command{Operation: "room.hide", MessageID: hiddenPost, Reason: "test"}))
	run(t, s, signed(owner, Command{Operation: "room.hide", MessageID: mixedHidden, Reason: "test"}))
	run(t, s, signed(owner, Command{Operation: "blob.delete", MessageID: deleted.ID}))
	s.now = func() time.Time { return time.Unix(testTime+2, 0) }

	ids := []string{unposted.ID, hidden.ID, mixed.ID, deleted.ID, expired.ID, other.ID, private.ID, "missing", mixed.ID}
	names, err := s.StyleAssetNames(testContext, "gallery", ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 {
		t.Fatalf("got %d metadata entries, want exactly 2: %+v", len(names), names)
	}
	for _, a := range []Attachment{unposted, mixed} {
		got, ok := names[a.ID]
		if !ok || got.Filename != a.Filename || got.MediaType != a.MediaType || got.Size != a.Size {
			t.Errorf("metadata for %s = %+v, want %+v", a.ID, got, a)
		}
		run(t, s, Command{Operation: "blob.get", MessageID: a.ID})
	}
	for _, a := range []Attachment{hidden, deleted, expired, other, private} {
		if _, ok := names[a.ID]; ok {
			t.Errorf("unexpected metadata for %s (%s)", a.ID, a.Room)
		}
	}
	fails(t, s, Command{Operation: "blob.get", MessageID: hidden.ID}, "not_found")
	fails(t, s, Command{Operation: "blob.get", MessageID: deleted.ID}, "attachment_gone")
	fails(t, s, Command{Operation: "blob.get", MessageID: expired.ID}, "attachment_gone")
	fails(t, s, Command{Operation: "blob.get", MessageID: private.ID}, "not_found")
	// Other-room assets can be served publicly, but not named by gallery's log.
	run(t, s, Command{Operation: "blob.get", MessageID: other.ID})
	// Restoring the only reference makes the same metadata available again.
	run(t, s, signed(owner, Command{Operation: "room.restore", MessageID: hiddenPost, Reason: "restored"}))
	names, err = s.StyleAssetNames(testContext, "gallery", []string{hidden.ID})
	if err != nil || names[hidden.ID].ID != hidden.ID {
		t.Fatalf("restored asset metadata: %+v, %v", names, err)
	}
}
