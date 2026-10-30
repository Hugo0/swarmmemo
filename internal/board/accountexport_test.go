package board

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// exportAccountPages pages account.export for key to the end: every post, the first
// page's account refs, the moderation map and the raw JSON of every page.
func exportAccountPages(t *testing.T, s *Store, key ed25519.PrivateKey, limit int) ([]Message, map[string]any, map[string][]exportModeration, string) {
	t.Helper()
	posts := []Message{}
	moderation := map[string][]exportModeration{}
	var refs map[string]any
	var raw strings.Builder
	cursor := ""
	for page := 0; ; page++ {
		if page > 50 {
			t.Fatal("export never ends")
		}
		r := run(t, s, signed(key, Command{Operation: "account.export", Cursor: cursor, Limit: limit}))
		encoded, _ := json.Marshal(r)
		raw.Write(encoded)
		if r.Data["schema"] != AccountExportSchema || r.Data["account"] != keyID(key) || r.Data["agent"] != keyID(key) {
			t.Fatalf("bundle head %v", r.Data)
		}
		got, hasRefs := r.Data["account_refs"].(map[string]any)
		if (page == 0) != hasRefs {
			t.Fatalf("page %d account_refs %v", page, hasRefs)
		}
		if hasRefs {
			refs = got
		}
		posts = append(posts, r.Data["posts"].([]Message)...)
		for id, entries := range r.Data["moderation"].(map[string][]exportModeration) {
			moderation[id] = append(moderation[id], entries...)
		}
		if r.Data["has_more"] != true {
			if r.NextCursor != "" {
				t.Fatal("a last page with a cursor")
			}
			break
		}
		if r.NextCursor == "" || r.Data["next_cursor"] != r.NextCursor {
			t.Fatalf("has_more without a cursor: %v", r.Data)
		}
		cursor = r.NextCursor
	}
	return posts, refs, moderation, raw.String()
}

func verifies(m Message) bool {
	key, err := base64.RawURLEncoding.DecodeString(m.PublicKey)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(m.Signature)
	return err == nil && ed25519.Verify(key, []byte(m.SignedPayload), sig)
}

func TestAccountExportOwnRowsOnly(t *testing.T) {
	s := openTest(t, updatesConfig())
	alice, bob, carol := keyFor(61), keyFor(62), keyFor(63)
	registerAll(t, s, alice, bob, carol)

	first := run(t, s, signed(alice, Command{Operation: "post", Room: "lobby", Text: "alice public one"})).Receipt.ID
	bobPublic := run(t, s, signed(bob, Command{Operation: "post", Room: "lobby", Text: "bob public", ReplyTo: first})).Receipt.ID
	// A DM conversation: alice's side is hers, bob's words never appear.
	dm := convRoom("export-ab")
	openConv(t, s, alice, dm, "dm", bob)
	openConv(t, s, bob, convRoom("export-ba"), "dm", alice)
	aliceDM := run(t, s, signed(alice, Command{Operation: "post", Room: dm, Text: "alice in the dm", Visibility: "private"})).Receipt.ID
	bobDM := run(t, s, signed(bob, Command{Operation: "post", Room: dm, Text: "bob dm secret words", Visibility: "private"})).Receipt.ID
	// A post bob's room hid, and one the operator removed.
	run(t, s, signed(bob, Command{Operation: "room.create", Room: "bobs-room", Visibility: "public"}))
	roomHidden := run(t, s, signed(alice, Command{Operation: "post", Room: "bobs-room", Text: "alice in bobs room"})).Receipt.ID
	run(t, s, signed(bob, Command{Operation: "room.hide", MessageID: roomHidden, Reason: "Off topic."}))
	removed := run(t, s, signed(alice, Command{Operation: "post", Room: "lobby", Text: "alice removed words"})).Receipt.ID
	if err := s.Moderate(testContext, removed, "Phishing.", true); err != nil {
		t.Fatal(err)
	}
	// Another agent's post.
	run(t, s, signed(carol, Command{Operation: "post", Room: "lobby", Text: "carol says hi"}))

	posts, refs, moderation, raw := exportAccountPages(t, s, alice, 2)
	want := []string{first, aliceDM, roomHidden, removed}
	if len(posts) != len(want) {
		t.Fatalf("exported %d posts, want %d", len(posts), len(want))
	}
	byID := map[string]Message{}
	for i, p := range posts {
		if p.ID != want[i] {
			t.Fatalf("post %d is %s, want %s (oldest first)", i, p.ID, want[i])
		}
		if p.Author != keyID(alice) {
			t.Fatalf("someone else's post %+v", p)
		}
		byID[p.ID] = p
	}
	for _, secret := range []string{"bob dm secret words", "bob public", "carol says hi", bobDM, bobPublic, "alice removed words"} {
		if strings.Contains(raw, secret) {
			t.Fatalf("the export carries %q", secret)
		}
	}
	// Every kept post verifies offline on its key.
	for _, id := range []string{first, aliceDM, roomHidden} {
		if !verifies(byID[id]) {
			t.Fatalf("post %s does not verify: %+v", id, byID[id])
		}
	}
	if p := byID[roomHidden]; !p.Hidden || p.HiddenBy != hiddenByRoom || p.Text != "alice in bobs room" || p.Type != "message" {
		t.Fatalf("room-hidden own post %+v", p)
	}
	if p := byID[removed]; !p.Hidden || p.Text != "" || p.Signature != "" || p.Type != "tombstone" || p.Hash == "" {
		t.Fatalf("operator-removed post %+v", p)
	}
	if m := moderation[roomHidden]; len(m) != 1 || m[0].Action != "hide" || m[0].By != hiddenByRoom || m[0].Reason != "Off topic." {
		t.Fatalf("room moderation %+v", m)
	}
	if m := moderation[removed]; len(m) != 1 || m[0].By != hiddenByOperator {
		t.Fatalf("operator moderation %+v", m)
	}
	if refs["account"] != keyID(alice) {
		t.Fatalf("refs %v", refs)
	}
	convs, _ := json.Marshal(refs["conversations"])
	if !strings.Contains(string(convs), dm) || strings.Contains(string(convs), keyID(bob)) {
		t.Fatalf("conversation refs %s", convs)
	}
	keys, _ := json.Marshal(refs["keys"])
	if !strings.Contains(string(keys), pubB64(alice)) {
		t.Fatalf("keys %s", keys)
	}

	// Another account's export is refused, as is an unsigned or delegated one.
	fails(t, s, signed(bob, Command{Operation: "account.export", Target: keyID(alice)}), "owner_required")
	fails(t, s, signed(bob, Command{Operation: "account.export", Target: strings.Repeat("ab", 32)}), "owner_required")
	fails(t, s, Command{Operation: "account.export"}, "signature_required")
	fails(t, s, signed(alice, Command{Operation: "account.export", Limit: AccountExportPageMax + 1}), "invalid_limit")
	// A cursor is bound to its account.
	page := run(t, s, signed(alice, Command{Operation: "account.export", Limit: 1}))
	fails(t, s, signed(bob, Command{Operation: "account.export", Cursor: page.NextCursor}), "invalid_cursor")
	// Your own target is the same export.
	run(t, s, signed(alice, Command{Operation: "account.export", Target: keyID(alice), Limit: 1}))
}

// A removed device's posts stay the account's, in its export, and verify on
// the device key.
func TestAccountExportKeepsRevokedKeyPosts(t *testing.T) {
	s := deviceTestStore(t)
	main, phone := keyFor(211), keyFor(212)
	run(t, s, signed(main, Command{Operation: "agent.register", Handle: "exporter"}))
	linkDevice(t, s, main, phone)
	id := run(t, s, signedNow(s, phone, Command{Operation: "post", Room: "lobby", Text: "from the phone"})).Receipt.ID
	run(t, s, signedNow(s, main, Command{Operation: "device.revoke", Target: keyID(phone)}))
	posts, refs, _, _ := exportAccountPages(t, s, main, 0)
	found := false
	for _, p := range posts {
		if p.ID == id {
			found = p.Author == keyID(phone) && p.PublicKey == pubB64(phone) && verifies(p)
		}
	}
	if !found {
		t.Fatalf("the removed device's post is gone from the export: %+v", posts)
	}
	keys, _ := json.Marshal(refs["keys"])
	if !strings.Contains(string(keys), `"removed_at"`) || !strings.Contains(string(keys), pubB64(phone)) {
		t.Fatalf("keys %s", keys)
	}
}
