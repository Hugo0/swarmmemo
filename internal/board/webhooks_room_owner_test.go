package board

import (
	"crypto/ed25519"
	"slices"
	"testing"
)

// C153: a room's owner and moderators hear of its activity, on any page,
// though they never posted there (an embed operator's "tell me whenever a
// comment comes in"); an account that neither posted, owns nor moderates
// hears nothing, and private rooms still need current membership.
func TestWebhookRoomActivityReachesOwnerAndModerators(t *testing.T) {
	forInboxModes(t, testWebhookRoomActivityReachesOwnerAndModerators)
}

func testWebhookRoomActivityReachesOwnerAndModerators(t *testing.T, mode InboxMode) {
	s := openTest(t, withInbox(Config{}, mode))
	owner, moderator, stranger, poster := keyFor(101), keyFor(102), keyFor(103), keyFor(104)
	for _, k := range []ed25519.PrivateKey{owner, moderator, stranger, poster} {
		register(t, s, k)
	}
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "blog", Visibility: "public"}))
	run(t, s, signed(owner, Command{Operation: "room.moderator.add", Room: "blog", Target: keyID(moderator)}))
	ownerHook, _ := activeWebhook(t, s, keyID(owner), "https://hooks.example.org/owner")
	modHook, _ := activeWebhook(t, s, keyID(moderator), "https://hooks.example.org/mod")
	strangerHook, _ := activeWebhook(t, s, keyID(stranger), "https://hooks.example.org/stranger")

	first := run(t, s, signed(poster, Command{Operation: "post", Room: "blog", Page: "first-post", Text: "nice article"})).Receipt.ID
	second := run(t, s, signed(poster, Command{Operation: "post", Room: "blog", Page: "second-post", Text: "another comment"})).Receipt.ID

	for name, hook := range map[string]string{"owner": ownerHook, "moderator": modHook} {
		bodies := webhookBodies(t, s, hook)
		if len(bodies) != 2 {
			t.Fatalf("%s got %d deliveries, want 2", name, len(bodies))
		}
		got := []string{}
		for _, b := range bodies {
			id := b["event"].(map[string]any)["id"].(string)
			if b["reason"] != "room_activity" || b["read"] != "/api/thread/"+id {
				t.Fatalf("%s delivery: %v", name, b)
			}
			got = append(got, id)
		}
		if !slices.Contains(got, first) || !slices.Contains(got, second) {
			t.Fatalf("%s heard of %v, want %s and %s", name, got, first, second)
		}
	}
	if bodies := webhookBodies(t, s, strangerHook); len(bodies) != 0 {
		t.Fatalf("an account that neither posted, owns nor moderates was notified: %v", bodies)
	}
	// The owner's own comment is not news to the owner; it is to a moderator.
	run(t, s, signed(owner, Command{Operation: "post", Room: "blog", Page: "first-post", Text: "thanks"}))
	if n := len(webhookBodies(t, s, ownerHook)); n != 2 {
		t.Fatalf("the owner was notified of its own comment: %d deliveries", n)
	}
	if n := len(webhookBodies(t, s, modHook)); n != 3 {
		t.Fatalf("the moderator got %d deliveries, want 3", n)
	}
	// updates.get says the same: the owner's room activity holds the comments.
	updates := run(t, s, Command{Operation: "updates.get", Target: keyID(owner), Cursor: "start"})
	activity, _ := updates.Data["room_activity"].([]string)
	if !slices.Contains(activity, first) || !slices.Contains(activity, second) {
		t.Fatalf("updates.get room_activity for the owner: %v", updates.Data)
	}
	updates = run(t, s, Command{Operation: "updates.get", Target: keyID(stranger), Cursor: "start"})
	if activity, _ := updates.Data["room_activity"].([]string); len(activity) != 0 {
		t.Fatalf("updates.get room_activity for a stranger: %v", activity)
	}
	// A removed moderator hears nothing more.
	run(t, s, signed(owner, Command{Operation: "room.moderator.remove", Room: "blog", Target: keyID(moderator)}))
	run(t, s, signed(poster, Command{Operation: "post", Room: "blog", Page: "first-post", Text: "late"}))
	if n := len(webhookBodies(t, s, modHook)); n != 3 {
		t.Fatalf("a removed moderator was notified: %d deliveries", n)
	}
	if n := len(webhookBodies(t, s, ownerHook)); n != 3 {
		t.Fatalf("the owner got %d deliveries, want 3", n)
	}
}

// Owning a private room is not reading it: the membership check still
// decides.
func TestWebhookRoomActivityPrivateRoomOwnerNeedsMembership(t *testing.T) {
	forInboxModes(t, testWebhookRoomActivityPrivateRoomOwnerNeedsMembership)
}

func testWebhookRoomActivityPrivateRoomOwnerNeedsMembership(t *testing.T, mode InboxMode) {
	s := openTest(t, withInbox(Config{}, mode))
	owner, member := keyFor(105), keyFor(106)
	register(t, s, owner)
	register(t, s, member)
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "vault", Visibility: "private", Members: []string{keyID(member)}}))
	ownerHook, _ := activeWebhook(t, s, keyID(owner), "https://hooks.example.org/owner")
	run(t, s, signed(member, Command{Operation: "post", Room: "vault", Page: "main", Text: "member comment"}))
	if n := len(webhookBodies(t, s, ownerHook)); n != 1 {
		t.Fatalf("a private room's owner got %d deliveries, want 1", n)
	}
	// Without its membership row, ownership alone must not deliver.
	if _, err := s.db.Exec("DELETE FROM members WHERE room='vault' AND account=?", keyID(owner)); err != nil {
		t.Fatal(err)
	}
	run(t, s, signed(member, Command{Operation: "post", Room: "vault", Page: "main", Text: "second"}))
	if n := len(webhookBodies(t, s, ownerHook)); n != 1 {
		t.Fatalf("ownership without membership delivered: %d deliveries", n)
	}
}
