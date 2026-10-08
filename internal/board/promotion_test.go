package board

import (
	"context"
	"strings"
	"testing"

	"swarmmemo/internal/moderation"
)

// The promotion option round-trips through room.policy.set and room.get,
// defaults to allow, refuses other values, and the operator sets it on a
// room without an owner (#lobby) through the CLI's path.
func TestPromotionPolicyRoundTrip(t *testing.T) {
	s := openTest(t, Config{})
	owner := keyFor(60)
	register(t, s, owner)
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "showcase"}))
	policy := func(room string) RoomPolicy {
		t.Helper()
		return *run(t, s, Command{Operation: "room.get", Room: room}).Room.Policy
	}
	if p := policy("showcase"); p.Promotion != PromotionAllow {
		t.Fatalf("default %+v", p)
	}
	run(t, s, signed(owner, Command{Operation: "room.policy.set", Room: "showcase", Data: `{"promotion":"moderate"}`}))
	if p := policy("showcase"); p.Promotion != PromotionModerate || p.Write != "open" {
		t.Fatalf("after set %+v", p)
	}
	// Another field leaves it as it is.
	run(t, s, signed(owner, Command{Operation: "room.policy.set", Room: "showcase", Data: `{"rules":"Show your work."}`}))
	if p := policy("showcase"); p.Promotion != PromotionModerate {
		t.Fatalf("kept %+v", p)
	}
	fails(t, s, signed(owner, Command{Operation: "room.policy.set", Room: "showcase", Data: `{"promotion":"ban"}`}), "invalid_policy")
	stranger := keyFor(61)
	register(t, s, stranger)
	fails(t, s, signed(stranger, Command{Operation: "room.policy.set", Room: "showcase", Data: `{"promotion":"allow"}`}), "owner_required")
	// rooms.list reads the same column.
	for _, r := range run(t, s, Command{Operation: "rooms.list"}).Rooms {
		if r.Name == "showcase" && (r.Policy == nil || r.Policy.Promotion != PromotionModerate) {
			t.Fatalf("rooms.list %+v", r.Policy)
		}
	}
	run(t, s, Command{Operation: "post", Text: "hello lobby"})
	if _, err := s.OperatorRoom(context.Background(), Command{Operation: "room.policy.set", Room: "lobby", Data: `{"promotion":"moderate"}`}); err != nil {
		t.Fatal(err)
	}
	if p := policy("lobby"); p.Promotion != PromotionModerate || p.Write != "open" {
		t.Fatalf("lobby %+v", p)
	}
}

// promotionFixture is a moderated #lobby with an owner-run room beside it.
func promotionFixture(t *testing.T) (*Store, postActuator) {
	t.Helper()
	s := openTest(t, Config{})
	run(t, s, Command{Operation: "post", Text: "hello lobby"})
	if _, err := s.OperatorRoom(context.Background(), Command{Operation: "room.policy.set", Room: "lobby", Data: `{"promotion":"moderate"}`}); err != nil {
		t.Fatal(err)
	}
	return s, postActuator{s}
}

// The rule judges a moderated room's public posts, top-level and replies
// apart, and never a room that allows promotion or the
// room's owner and moderators.
func TestPromotionScope(t *testing.T) {
	s, act := promotionFixture(t)
	ctx := context.Background()
	scope := func(id string) moderation.Promotion {
		t.Helper()
		p, err := act.PromotionScope(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	ad := run(t, s, Command{Operation: "post", Text: "Donate now: https://givecause.online/c/1?ref=x"}).Receipt.ID
	reply := run(t, s, Command{Operation: "post", Text: "same link again", ReplyTo: ad}).Receipt.ID
	if scope(ad) != moderation.PromotionPost || scope(reply) != moderation.PromotionReply {
		t.Fatalf("lobby: %q %q", scope(ad), scope(reply))
	}
	owner, mod, member := keyFor(62), keyFor(63), keyFor(64)
	register(t, s, owner)
	register(t, s, mod)
	register(t, s, member)
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "market"}))
	run(t, s, signed(owner, Command{Operation: "room.moderator.add", Room: "market", Target: keyID(mod)}))
	open := run(t, s, Command{Operation: "post", Room: "market", Text: "buy my thing"}).Receipt.ID
	if scope(open) != "" {
		t.Fatal("a room that allows promotion was judged")
	}
	run(t, s, signed(owner, Command{Operation: "room.policy.set", Room: "market", Data: `{"promotion":"moderate"}`}))
	byOwner := run(t, s, signed(owner, Command{Operation: "post", Room: "market", Text: "our sponsor"})).Receipt.ID
	byMod := run(t, s, signed(mod, Command{Operation: "post", Room: "market", Text: "my launch"})).Receipt.ID
	byMember := run(t, s, signed(member, Command{Operation: "post", Room: "market", Text: "my launch"})).Receipt.ID
	if scope(byOwner) != "" || scope(byMod) != "" || scope(byMember) != moderation.PromotionPost || scope(open) != moderation.PromotionPost {
		t.Fatalf("officials: %q %q %q %q", scope(byOwner), scope(byMod), scope(byMember), scope(open))
	}
	if scope("no-such-message") != "" {
		t.Fatal("a missing message was judged")
	}
}

// A hide the rule decides is the room's: hidden with the public reason, in
// the room's log, restorable by the room; and it is checked again at the
// hide, so a room switched back to allow keeps the post.
func TestPromotionHide(t *testing.T) {
	s, act := promotionFixture(t)
	ctx := context.Background()
	ad := run(t, s, Command{Operation: "post", Text: "Donate now: https://givecause.online/c/1?ref=x"}).Receipt.ID
	if err := act.HidePromotion(ctx, ad, "auto-screen: p=0.98, model=screen-1, policy=v3"); err != nil {
		t.Fatal(err)
	}
	m := run(t, s, Command{Operation: "message.get", MessageID: ad}).Messages
	want := "Advertising: #lobby is kept for conversation. Self-promotion is welcome in your own room (room.create) or #commerce; mentions that add to a discussion are fine. (auto-screen: p=0.98, model=screen-1, policy=v3)"
	if len(m) != 1 || !m[0].Hidden || m[0].HiddenBy != hiddenByRoom || m[0].Reason != want {
		t.Fatalf("hidden %+v", m)
	}
	log := run(t, s, Command{Operation: "room.modlog", Room: "lobby"}).Data["entries"].([]ModerationEntry)
	if len(log) == 0 || log[0].Action != "hide" || log[0].Target != ad || log[0].Actor != operatorActor || log[0].Reason != want {
		t.Fatalf("modlog %+v", log)
	}
	// Hidden already: nothing more.
	if err := act.HidePromotion(ctx, ad, "x"); err != nil {
		t.Fatal(err)
	}
	if n := len(run(t, s, Command{Operation: "room.modlog", Room: "lobby"}).Data["entries"].([]ModerationEntry)); n != len(log) {
		t.Fatalf("a second hide was logged: %d", n)
	}
	// The room changed its mind before the hide landed.
	other := run(t, s, Command{Operation: "post", Text: "Donate again: https://givecause.online/c/2"}).Receipt.ID
	if _, err := s.OperatorRoom(ctx, Command{Operation: "room.policy.set", Room: "lobby", Data: `{"promotion":"allow"}`}); err != nil {
		t.Fatal(err)
	}
	if err := act.HidePromotion(ctx, other, "x"); err != nil {
		t.Fatal(err)
	}
	if m := run(t, s, Command{Operation: "message.get", MessageID: other}).Messages; m[0].Hidden {
		t.Fatal("a post in a room that allows promotion was hidden")
	}
	// In an owned room, the owner may restore the room's hide.
	owner := keyFor(65)
	register(t, s, owner)
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "salon"}))
	run(t, s, signed(owner, Command{Operation: "room.policy.set", Room: "salon", Data: `{"promotion":"moderate"}`}))
	post := run(t, s, Command{Operation: "post", Room: "salon", Text: "Try AgentHost Pro, 50% off: https://agenthost.example/?aff=1"}).Receipt.ID
	if err := act.HidePromotion(ctx, post, "x"); err != nil {
		t.Fatal(err)
	}
	if m := run(t, s, Command{Operation: "message.get", MessageID: post}).Messages; !m[0].Hidden || !strings.HasPrefix(m[0].Reason, "Advertising: #salon is kept") {
		t.Fatalf("salon %+v", m[0])
	}
	run(t, s, signed(owner, Command{Operation: "room.restore", MessageID: post, Reason: "a fair mention"}))
	if m := run(t, s, Command{Operation: "message.get", MessageID: post}).Messages; m[0].Hidden {
		t.Fatal("the owner could not restore the room's hide")
	}
}

// With MODERATION on, the engine's post worker asks the board for the rule:
// postActuator is a moderation.PromotionRuler. Without a Jev key nothing is
// judged, so a moderated room's posts stay up (flagged as unscreened).
func TestPromotionWiredIntoTheEngine(t *testing.T) {
	var _ moderation.PromotionRuler = postActuator{}
	s := openTest(t, Config{Features: Features{Moderation: true}})
	run(t, s, Command{Operation: "post", Text: "hello lobby"})
	if _, err := s.OperatorRoom(context.Background(), Command{Operation: "room.policy.set", Room: "lobby", Data: `{"promotion":"moderate"}`}); err != nil {
		t.Fatal(err)
	}
	ad := run(t, s, Command{Operation: "post", Text: "Donate now: https://givecause.online/c/1?ref=x"}).Receipt.ID
	if _, err := s.Moderation().Work(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m := run(t, s, Command{Operation: "message.get", MessageID: ad}).Messages; m[0].Hidden {
		t.Fatalf("hidden without a screen: %+v", m[0])
	}
}
