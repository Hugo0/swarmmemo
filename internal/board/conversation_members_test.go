package board

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base32"
	"strings"
	"testing"
)

// convRoom is a deterministic conversation room name for tag: 16 bytes in
// lowercase base32, as a client proposes.
func convRoom(tag string) string {
	sum := sha256.Sum256([]byte(tag))
	return "~" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:16]))
}

// openConv signs conversation.open for key with the given members.
func openConv(t *testing.T, s *Store, key ed25519.PrivateKey, room, kind string, members ...ed25519.PrivateKey) Result {
	t.Helper()
	return run(t, s, openCmd(key, room, kind, members...))
}

func openCmd(key ed25519.PrivateKey, room, kind string, members ...ed25519.PrivateKey) Command {
	ids := []string{}
	for _, m := range members {
		ids = append(ids, keyID(m))
	}
	return signed(key, Command{Operation: "conversation.open", Room: room, Members: ids, Data: `{"schema":1,"kind":"` + kind + `"}`})
}

func convView(t *testing.T, r Result) Conversation {
	t.Helper()
	v, ok := r.Data["conversation"].(Conversation)
	if !ok {
		t.Fatalf("no conversation in %+v", r.Data)
	}
	return v
}

// memberStates maps each member's agent id to its state as v shows it.
func memberStates(v Conversation) map[string]string {
	out := map[string]string{}
	for _, m := range v.Members {
		out[m.Agent] = m.State
	}
	return out
}

func memberRowOf(t *testing.T, s *Store, room string, key ed25519.PrivateKey) memberRow {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	m, ok, err := loadMember(testContext, tx, room, keyID(key))
	if err != nil || !ok {
		t.Fatalf("member row %v %v", ok, err)
	}
	return m
}

// setMemberState keeps the one invariant every existing read relies on: a
// members row exists exactly while the state is active. member_epoch counts
// new rows and changes of the active set; a silent change of a member who
// never acted keeps changed_at.
func TestSetMemberStateInvariants(t *testing.T) {
	s := openTest(t, updatesConfig())
	owner, guest := keyFor(1), keyFor(2)
	register(t, s, owner)
	register(t, s, guest)
	room := convRoom("invariants")
	openConv(t, s, owner, room, "group")
	epoch := func() int64 { return sqlCount(t, s, "SELECT member_epoch FROM conversations WHERE room=?", room) }
	accessEpoch := func() string {
		var e string
		if err := s.db.QueryRow("SELECT private_access_epoch FROM rooms WHERE name=?", room).Scan(&e); err != nil {
			t.Fatal(err)
		}
		return e
	}
	if epoch() != 1 {
		t.Fatalf("the creating command's set is epoch 1, got %d", epoch())
	}
	step := func(state string, acknowledge bool, now int64) memberRow {
		t.Helper()
		tx, err := s.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		m, err := setMemberState(testContext, tx, memberChange{Room: room, Account: keyID(guest), State: state, AddedBy: keyID(owner), Acknowledge: acknowledge}, now)
		if err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
		members := sqlCount(t, s, "SELECT count(*) FROM members WHERE room=? AND account=?", room, keyID(guest))
		if (members == 1) != (state == memberActive) || m.State != state {
			t.Fatalf("state %s with %d members rows", state, members)
		}
		return m
	}
	// A new row, whatever its state, is one change.
	m := step(memberRequested, false, testTime)
	if epoch() != 2 || m.ChangedAt != testTime || m.Acknowledged {
		t.Fatalf("requested: epoch %d, %+v", epoch(), m)
	}
	// A silent decline keeps the epoch and the clock.
	if m = step(memberDeclined, false, testTime+50); epoch() != 2 || m.ChangedAt != testTime {
		t.Fatalf("declined: epoch %d, %+v", epoch(), m)
	}
	// Becoming active by acting: one change, the clock moves.
	before := accessEpoch()
	if m = step(memberActive, true, testTime+100); epoch() != 3 || m.ChangedAt != testTime+100 || !m.Acknowledged {
		t.Fatalf("active: epoch %d, %+v", epoch(), m)
	}
	if accessEpoch() != before {
		t.Fatal("joining rotated the private read epoch")
	}
	// Staying active changes nothing.
	if step(memberActive, false, testTime+150); epoch() != 3 {
		t.Fatalf("active again: epoch %d", epoch())
	}
	// Leaving the active set is a change, and ends private read grants.
	if m = step(memberLeft, false, testTime+200); epoch() != 4 || m.ChangedAt != testTime+200 {
		t.Fatalf("left: epoch %d, %+v", epoch(), m)
	}
	if accessEpoch() == before {
		t.Fatal("leaving kept the private read epoch")
	}
	if step(memberRemoved, false, testTime+300); epoch() != 4 {
		t.Fatalf("left to removed is no active-set change: epoch %d", epoch())
	}
	// Nothing is deleted: the row stays, and so does the history.
	if n := sqlCount(t, s, "SELECT count(*) FROM conversation_members WHERE room=?", room); n != 2 {
		t.Fatalf("%d member rows", n)
	}
}

// room.member.add on a conversation goes through the added agent's inbound
// policy; only a group's owner adds or removes; any agent already given a
// place is refused alike, so a drop looks like any other state.
func TestConversationMemberAddAndRemove(t *testing.T) {
	s := openTest(t, updatesConfig())
	owner, friend, closed, other := keyFor(1), keyFor(2), keyFor(3), keyFor(4)
	for _, k := range []ed25519.PrivateKey{owner, friend, closed, other} {
		register(t, s, k)
	}
	run(t, s, signed(closed, Command{Operation: "messaging.policy.set", Data: `{"schema":1,"inbound_policy":{"schema":1,"preset":"closed"}}`}))
	room := convRoom("add-remove")
	openConv(t, s, owner, room, "group")
	add := func(key, target ed25519.PrivateKey) Command {
		return signed(key, Command{Operation: "room.member.add", Room: room, Target: keyID(target)})
	}
	run(t, s, add(owner, friend))
	run(t, s, add(owner, closed))
	if got := memberRowOf(t, s, room, friend).State; got != memberRequested {
		t.Fatalf("a stranger under the open preset is asked: %s", got)
	}
	if got := memberRowOf(t, s, room, closed).State; got != memberDeclined {
		t.Fatalf("the closed preset drops a stranger: %s", got)
	}
	fails(t, s, add(owner, friend), "member_exists")
	fails(t, s, add(owner, closed), "member_exists")
	// Pending members are neither members of the room nor able to add.
	fails(t, s, add(friend, other), "not_found")
	run(t, s, signed(friend, Command{Operation: "conversation.respond", Room: room, Data: `{"schema":1,"action":"accept"}`}))
	fails(t, s, add(friend, other), "owner_required")
	run(t, s, signed(owner, Command{Operation: "room.member.remove", Room: room, Target: keyID(friend)}))
	if memberRowOf(t, s, room, friend).State != memberRemoved || sqlCount(t, s, "SELECT count(*) FROM members WHERE room=? AND account=?", room, keyID(friend)) != 0 {
		t.Fatal("removal kept access")
	}
	fails(t, s, signed(owner, Command{Operation: "room.member.remove", Room: room, Target: keyID(owner)}), "owner_membership")
	fails(t, s, signed(owner, Command{Operation: "room.member.remove", Room: room, Target: keyID(other)}), "not_member")
	// A DM keeps its two members.
	dm := convRoom("add-remove-dm")
	openConv(t, s, owner, dm, "dm", other)
	fails(t, s, signed(owner, Command{Operation: "room.member.add", Room: dm, Target: keyID(friend)}), "dm_members")
	// Moderators, styles and ownership are for shared rooms.
	fails(t, s, signed(owner, Command{Operation: "room.owner.transfer", Room: room, Target: keyID(other)}), "conversation_room")
}
