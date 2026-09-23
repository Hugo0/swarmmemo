package board

import (
	"strings"
	"testing"
)

// Attacks on RFC0010 room governance, supersession and delegation.
// Each attack below must be refused; a pass means the defence held.
func TestRoomGovernanceAttacks(t *testing.T) {
	s := openTest(t, Config{})
	owner, mod, stranger, child := keyFor(211), keyFor(212), keyFor(213), keyFor(214)
	register(t, s, owner)
	register(t, s, mod)
	register(t, s, stranger)
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "club"}))
	run(t, s, signed(owner, Command{Operation: "room.moderator.add", Room: "club", Target: keyID(mod)}))
	ownerPost := run(t, s, signed(owner, Command{Operation: "post", Room: "club", Text: "owner root"})).Receipt.ID
	strangerPost := run(t, s, signed(stranger, Command{Operation: "post", Room: "club", Text: "stranger root"})).Receipt.ID
	run(t, s, policySet(owner, "club", `{"write":"owner"}`))

	// Non-owner (stranger, moderator, anonymous) cannot govern.
	for _, c := range []Command{
		policySet(stranger, "club", `{"write":"open"}`),
		policySet(mod, "club", `{"write":"open"}`),
		signed(stranger, Command{Operation: "room.style.set", Room: "club", Data: `{"css":".x{color:red}"}`}),
		signed(mod, Command{Operation: "room.style.clear", Room: "club"}),
		signed(stranger, Command{Operation: "room.moderator.add", Room: "club", Target: keyID(stranger)}),
		signed(mod, Command{Operation: "room.moderator.remove", Room: "club", Target: keyID(mod)}),
		signed(mod, Command{Operation: "room.owner.transfer", Room: "club", Target: keyID(mod)}),
	} {
		fails(t, s, c, "owner_required")
	}
	fails(t, s, Command{Operation: "room.policy.set", Room: "club", Data: `{"write":"open"}`}, "signature_required")
	// Case / Unicode variants never name the room or a new one beside it.
	for _, name := range []string{"Club", "CLUB", "club ", "club́", "ｃｌｕｂ", "club\x00"} {
		_, err := s.Execute(testContext, policySet(stranger, name, `{"write":"open"}`), "x")
		if err == nil {
			t.Fatalf("variant %q accepted", name)
		}
	}
	// Moderator cannot hide the owner's post; stranger cannot hide at all.
	fails(t, s, signed(mod, Command{Operation: "room.hide", MessageID: ownerPost, Reason: "x"}), "moderator_required")
	fails(t, s, signed(stranger, Command{Operation: "room.hide", MessageID: strangerPost, Reason: "x"}), "moderator_required")

	// A worker key delegated by the owner, with every delegable operation,
	// cannot govern, hide, or edit the owner's own posts.
	grant := enroll(t, s, owner, child, "club", 3600, 1<<20)
	for _, c := range []Command{
		{Operation: "room.policy.set", Room: "club", Data: `{"write":"open"}`},
		{Operation: "room.hide", MessageID: strangerPost, Reason: "x"},
		{Operation: "room.moderator.add", Room: "club", Target: keyID(stranger)},
	} {
		fails(t, s, childCommand(s, child, grant, c), "delegation_forbidden")
	}
	fails(t, s, childCommand(s, child, grant, Command{Operation: "post", Room: "club", Visibility: "public", Text: "edit", Data: dataJSON(`"supersedes":"` + ownerPost + `"`)}), "supersede_forbidden")

	// Supersession: a non-author cannot edit; an author cannot move an edit to
	// another room, page, or turn a reply into a root.
	fails(t, s, signed(mod, Command{Operation: "post", Room: "club", Text: "hijack", Data: dataJSON(`"supersedes":"` + strangerPost + `"`)}), "supersede_forbidden")
	fails(t, s, signed(stranger, Command{Operation: "post", Room: "lobby", Text: "moved", Data: dataJSON(`"supersedes":"` + strangerPost + `"`)}), "not_found")
	fails(t, s, signed(stranger, Command{Operation: "post", Room: "club", Page: "other", Text: "moved", Data: dataJSON(`"supersedes":"` + strangerPost + `"`)}), "supersede_mismatch")
	reply := run(t, s, signed(stranger, Command{Operation: "post", Room: "club", Text: "a reply", ReplyTo: ownerPost})).Receipt.ID
	fails(t, s, signed(stranger, Command{Operation: "post", Room: "club", Text: "now a root", Data: dataJSON(`"supersedes":"` + reply + `"`)}), "supersede_mismatch")
	// Hiding any version (here the head) stops further edits.
	v2 := run(t, s, signed(stranger, Command{Operation: "post", Room: "club", Text: "v2", Data: dataJSON(`"supersedes":"` + strangerPost + `"`)})).Receipt.ID
	run(t, s, signed(mod, Command{Operation: "room.hide", MessageID: v2, Reason: "abuse"}))
	fails(t, s, signed(stranger, Command{Operation: "post", Room: "club", Text: "v3", Data: dataJSON(`"supersedes":"` + v2 + `"`)}), "supersede_hidden")
	if got := run(t, s, Command{Operation: "message.get", MessageID: v2}).Messages[0]; got.Text != "" || !got.Hidden {
		t.Fatalf("hidden version leaked: %+v", got)
	}

	// Personal rooms: no one else opens, styles or starts posts in them.
	fails(t, s, signed(stranger, Command{Operation: "room.style.set", Room: personal(owner), Data: `{"css":".x{color:red}"}`}), "room_write_restricted")
	fails(t, s, signed(stranger, Command{Operation: "room.member.add", Room: personal(owner), Target: keyID(stranger)}), "not_found")
	if strings.Contains(roomGet(t, s, "club").Policy.Write, "open") {
		t.Fatal("policy changed by a non-owner")
	}
}
