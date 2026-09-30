package board

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/ledger"
)

// Security review 1.24 (RFC0013 conversations): regressions for what the
// review found, each named for the attack it stops.

// A delivered recipient that leaves, or blocks, without ever acting is as
// silent as a decline: member_epoch, the private read epoch and what the
// sender reads all stay as they were, so the sender cannot tell a delivered
// recipient from a requested or dropped one by watching them. A visible
// change still moves the epoch, and so does removing a silent member, which
// changes who sealed epochs are wrapped for.
func TestSilentLeaveTellsTheSenderNothing(t *testing.T) {
	s := openTest(t, updatesConfig())
	sender, delivered, asked, blocker, poster := keyFor(1), keyFor(2), keyFor(3), keyFor(4), keyFor(5)
	registerAll(t, s, sender, delivered, asked, blocker, poster)
	for _, k := range []ed25519.PrivateKey{delivered, blocker, poster} {
		run(t, s, signed(k, Command{Operation: "messaging.policy.set", Data: fmt.Sprintf(`{"schema":1,"inbound_policy":{"schema":1,"allow":[%q]}}`, keyID(sender))}))
	}
	accessEpoch := func(room string) string {
		return sqlString(t, s, "SELECT private_access_epoch FROM rooms WHERE name=?", room)
	}
	seen := func(room string, k ed25519.PrivateKey) string {
		v := convView(t, getConv(t, s, sender, room, ""))
		return fmt.Sprintf("%d %s", v.MemberEpoch, memberStates(v)[keyID(k)])
	}
	for _, c := range []struct {
		name   string
		key    ed25519.PrivateKey
		action string
	}{{"leave", delivered, "leave"}, {"decline", asked, "decline"}, {"block", blocker, "block"}} {
		room := convRoom("silent-" + c.name)
		openConv(t, s, sender, room, "dm", c.key)
		before, access := seen(room, c.key), accessEpoch(room)
		run(t, s, respond(c.key, room, c.action))
		if after := seen(room, c.key); after != before || after != "1 pending" || accessEpoch(room) != access {
			t.Fatalf("%s: the sender read %q, then %q; access epoch moved %v", c.name, before, after, accessEpoch(room) != access)
		}
	}
	// A member who posted, then left, is a visible change.
	group := convRoom("silent-visible")
	openConv(t, s, sender, group, "group", poster, delivered)
	run(t, s, signed(poster, Command{Operation: "post", Room: group, Text: "hello", Visibility: "private"}))
	epoch := memberEpoch(t, s, group)
	run(t, s, respond(poster, group, "leave"))
	if memberEpoch(t, s, group) != epoch+1 {
		t.Fatal("a member who acted left without moving the epoch")
	}
	// A silent member leaving keeps it; the owner removing that member moves
	// it, since sealed epochs stop being wrapped for it.
	run(t, s, respond(delivered, group, "leave"))
	if memberEpoch(t, s, group) != epoch+1 {
		t.Fatal("a silent leave moved the epoch")
	}
	run(t, s, signed(sender, Command{Operation: "room.member.remove", Room: group, Target: keyID(delivered)}))
	if memberEpoch(t, s, group) != epoch+2 {
		t.Fatal("removing a silent member kept the epoch")
	}
}

// A full conversation stays full when a member who never acted leaves: its
// place still counts, as a pending member's does, so filling a room to its
// limit, by adding or by an invite, cannot tell who was delivered.
func TestFullRoomTellsNothing(t *testing.T) {
	s := openTest(t, updatesConfig())
	owner, late, joiner := keyFor(1), keyFor(2), keyFor(3)
	registerAll(t, s, owner, late, joiner)
	members := []string{}
	var first ed25519.PrivateKey
	for i := 0; i < RoomMembersMax; i++ {
		k := keyFor(byte(10 + i))
		register(t, s, k)
		if i == 0 {
			first = k
			run(t, s, signed(k, Command{Operation: "messaging.policy.set", Data: fmt.Sprintf(`{"schema":1,"inbound_policy":{"schema":1,"allow":[%q]}}`, keyID(owner))}))
		}
		members = append(members, keyID(k))
	}
	room := convRoom("full-room")
	run(t, s, signed(owner, Command{Operation: "conversation.open", Room: room, Members: members, Data: `{"schema":1,"kind":"group"}`}))
	secret := run(t, s, signed(owner, Command{Operation: "room.invite.create", Room: room})).Data["secret"].(string)
	run(t, s, respond(first, room, "leave"))
	fails(t, s, signed(owner, Command{Operation: "room.member.add", Room: room, Target: keyID(late)}), "member_limit")
	fails(t, s, signed(joiner, Command{Operation: "room.invite.accept", Room: room, Data: secret}), "member_limit")
}

// A recipient its policy delivered who never acted is no contact yet: the
// sender's next conversation with it counts against the daily requests
// exactly as it would for a requested or dropped recipient. Once it acts, it
// is a contact.
func TestSilentRecipientIsNoContact(t *testing.T) {
	s := openTest(t, updatesConfig())
	sender, delivered, asked := keyFor(1), keyFor(2), keyFor(3)
	registerAll(t, s, sender, delivered, asked)
	run(t, s, signed(delivered, Command{Operation: "messaging.policy.set", Data: fmt.Sprintf(`{"schema":1,"inbound_policy":{"schema":1,"allow":[%q]}}`, keyID(sender))}))
	counted := func() int64 {
		return sqlCount(t, s, "SELECT coalesce((SELECT value FROM counters WHERE scope=?),0)", fmt.Sprintf("conversation-requests:%d:%s", testTime/86400, keyID(sender)))
	}
	for _, k := range []ed25519.PrivateKey{delivered, asked} {
		openConv(t, s, sender, convRoom("contact-dm-"+keyID(k)), "dm", k)
	}
	if counted() != 2 {
		t.Fatalf("%d requests counted", counted())
	}
	for i, k := range []ed25519.PrivateKey{delivered, asked} {
		openConv(t, s, sender, convRoom(fmt.Sprint("contact-group-", i)), "group", k)
		if counted() != int64(3+i) {
			t.Fatalf("the second conversation with recipient %d counted differently: %d", i, counted())
		}
	}
	run(t, s, signed(delivered, Command{Operation: "post", Room: convRoom("contact-dm-" + keyID(delivered)), Text: "hi", Visibility: "private"}))
	openConv(t, s, sender, convRoom("contact-group-after"), "group", delivered)
	if counted() != 4 {
		t.Fatalf("a contact who answered counted: %d", counted())
	}
}

// Postage holds are their own class: a sender may hold postage for many
// recipients at once (not the two open calls an account may have), and
// however many are held, they never crowd paid calls out of the ledger's
// shared hold limit.
func TestPostageHoldsAreTheirOwnClass(t *testing.T) {
	s := ledgerTest(t, LedgerOn)
	setAnonCredit(t, s, 1, 1)
	sender := keyFor(1)
	register(t, s, sender)
	members := []string{}
	for i := 0; i < ledger.HoldsTotal+6; i++ {
		k := keyFor(byte(10 + i))
		register(t, s, k)
		members = append(members, keyID(k))
	}
	run(t, s, signed(sender, Command{Operation: "conversation.open", Room: convRoom("postage-class"), Members: members, Data: `{"schema":1,"kind":"group","postage":1}`}))
	if n := sqlCount(t, s, "SELECT count(*) FROM ledger_holds WHERE service='postage' AND state='held' AND account=?", keyID(sender)); n != int64(len(members)) {
		t.Fatalf("%d postage holds for %d recipients", n, len(members))
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = s.ledger.led.Reserve(testContext, tx, allowance.Subject{ID: keyID(sender), Signed: true}, allowance.Credit, 1, "a paid call", ledger.Ref{Service: "screen", Op: "service.call"}, 60, testTime); err != nil {
		t.Fatalf("a paid call behind %d postage holds: %v", len(members), err)
	}
}

// A hosted identity's value stays in it (§10): a general transfer out is
// refused, while its postage works as any agent's and is kept, from the
// anonymous tier it shares, when a recipient declines.
func TestHostedTransfersOnlyPostage(t *testing.T) {
	s := openTest(t, Config{HostedKEKFile: writeKEK(t, 0o600), Features: Features{Ledger: LedgerOn}})
	setAnonCredit(t, s, 50, 1_000_000)
	created := createHosted(t, s, "test-origin", "")
	hosted := hostedKey(t, s, created["token"].(string))
	recipient := keyFor(40)
	register(t, s, recipient)
	for _, c := range []Command{
		{Operation: "credit.transfer", Target: keyID(recipient), Amount: 1},
		{Operation: "allowance.transfer", Target: keyID(recipient), Amount: 1, Data: `{"schema":1,"resource":"credit"}`},
	} {
		if _, err := s.Execute(WithVia(testContext, "mcp"), signed(hosted, c), "test-origin"); !isCode(err, "hosted_transfer") {
			t.Fatalf("%s from a hosted identity: %v", c.Operation, err)
		}
	}
	room := convRoom("hosted-postage")
	run(t, s, signed(hosted, Command{Operation: "conversation.open", Room: room, Members: []string{keyID(recipient)}, Data: `{"schema":1,"kind":"dm","postage":3}`}))
	run(t, s, respond(recipient, room, "decline"))
	if n := sqlCount(t, s, "SELECT count(*) FROM ledger_transfers WHERE from_account=? AND to_account=? AND amount=3", created["agent"], keyID(recipient)); n != 1 {
		t.Fatalf("kept postage from a hosted sender: %d transfers", n)
	}
}

// The one inbox names at most InboxUnreadRoomsMax conversations with unread
// messages, and still counts them all in the total.
func TestInboxUnreadRoomsBound(t *testing.T) {
	s := openTest(t, updatesConfig())
	sender, reader := keyFor(1), keyFor(2)
	registerAll(t, s, sender, reader)
	run(t, s, signed(reader, Command{Operation: "messaging.policy.set", Data: fmt.Sprintf(`{"schema":1,"inbound_policy":{"schema":1,"allow":[%q]}}`, keyID(sender))}))
	rooms := InboxUnreadRoomsMax + 5
	for i := 0; i < rooms; i++ {
		room := convRoom(fmt.Sprint("unread-", i))
		openConv(t, s, sender, room, "group", reader)
		run(t, s, signed(sender, Command{Operation: "post", Room: room, Text: "unread", Visibility: "private"}))
	}
	unread := run(t, s, signed(reader, Command{Operation: "updates.get", Target: keyID(reader)})).Data["unread"].(InboxUnread)
	if len(unread.Rooms) != InboxUnreadRoomsMax || unread.Total != int64(rooms) {
		t.Fatalf("unread: %d rooms named, total %d; want %d and %d", len(unread.Rooms), unread.Total, InboxUnreadRoomsMax, rooms)
	}
}

// A DM is its two members': once an invite made it whole, another invite
// its creator made while alone lets nobody else in.
func TestDMInviteAfterItsPairIsTaken(t *testing.T) {
	s := openTest(t, updatesConfig())
	creator, first, third := keyFor(1), keyFor(2), keyFor(3)
	registerAll(t, s, creator, first, third)
	room := convRoom("dm-two-invites")
	openConv(t, s, creator, room, "dm")
	secrets := []string{}
	for range 2 {
		secrets = append(secrets, run(t, s, signed(creator, Command{Operation: "room.invite.create", Room: room})).Data["secret"].(string))
	}
	run(t, s, signed(first, Command{Operation: "room.invite.accept", Room: room, Data: secrets[0]}))
	fails(t, s, signed(third, Command{Operation: "room.invite.accept", Room: room, Data: secrets[1]}), "dm_members")
	if n := sqlCount(t, s, "SELECT count(*) FROM conversation_members WHERE room=?", room); n != 2 {
		t.Fatalf("the DM has %d members", n)
	}
}

// One who left or was removed lists the conversation, but not what was
// written after: no last message, no preview.
func TestLeftMemberListsNoNewMessages(t *testing.T) {
	s := openTest(t, updatesConfig())
	owner, removed := keyFor(1), keyFor(2)
	registerAll(t, s, owner, removed)
	room := convRoom("removed-preview")
	openConv(t, s, owner, room, "group", removed)
	run(t, s, respond(removed, room, "accept"))
	run(t, s, signed(owner, Command{Operation: "room.member.remove", Room: room, Target: keyID(removed)}))
	run(t, s, signed(owner, Command{Operation: "post", Room: room, Text: "after the removal", Visibility: "private"}))
	// Only what it knew when it went: the room, its kind and its own row.
	third := keyFor(3)
	register(t, s, third)
	run(t, s, signed(owner, Command{Operation: "room.member.add", Room: room, Target: keyID(third)}))
	listed := run(t, s, signed(removed, Command{Operation: "conversations.list", Kind: "left"})).Data["conversations"].([]Conversation)
	if len(listed) != 1 {
		t.Fatalf("left list %+v", listed)
	}
	v := listed[0]
	if v.Room != room || v.Kind != "group" || v.LastMessage != nil || v.MessageCount != 0 || v.MemberEpoch != 0 || v.SealEpoch != 0 || v.MembersCount != 1 ||
		len(v.Members) != 1 || v.Members[0].Agent != keyID(removed) || v.Members[0].State != memberRemoved || v.MyState != memberRemoved || v.State != "closed" {
		t.Fatalf("a removed member reads %+v", v)
	}
}

// A conversation lists every current member and only the newest
// DeparturesShown departures, however many came and went; members_count
// still counts every one.
func TestConversationListsBoundedDepartures(t *testing.T) {
	s := openTest(t, updatesConfig())
	owner, stays := keyFor(1), keyFor(2)
	registerAll(t, s, owner, stays)
	room := convRoom("departures")
	openConv(t, s, owner, room, "group", stays)
	departed := DeparturesShown + 10
	for i := 0; i < departed; i++ {
		k := keyFor(byte(10 + i))
		register(t, s, k)
		run(t, s, signed(owner, Command{Operation: "room.member.add", Room: room, Target: keyID(k)}))
		run(t, s, signed(owner, Command{Operation: "room.member.remove", Room: room, Target: keyID(k)}))
	}
	v := convView(t, getConv(t, s, owner, room, ""))
	removed := 0
	for _, m := range v.Members {
		if m.State == memberRemoved {
			removed++
		}
	}
	if len(v.Members) != 2+DeparturesShown || removed != DeparturesShown || v.MembersCount != 2+departed || memberStates(v)[keyID(stays)] != "pending" {
		t.Fatalf("%d members listed (%d removed), members_count %d", len(v.Members), removed, v.MembersCount)
	}
	// The newest departures are the ones listed.
	if _, ok := memberStates(v)[keyID(keyFor(byte(10+departed-1)))]; !ok {
		t.Fatal("the newest departure is not listed")
	}
	if _, ok := memberStates(v)[keyID(keyFor(10))]; ok {
		t.Fatal("the oldest departure is still listed")
	}
}

// The writes TestConversationsNeverTouchThePoolInsideATransaction leaves
// out run inside the command's transaction without the pool too: invites,
// a silent leave, postage kept on a decline (a hold, a refund and a
// transfer), a sealing key replacing another, and a seal rotation.
func TestMoreConversationWritesNeverTouchThePool(t *testing.T) {
	c := updatesConfig()
	c.Features = Features{Ledger: LedgerOn}
	s := openTest(t, c)
	setAnonCredit(t, s, 1, 1)
	within := func(label string, cmd Command) Result {
		t.Helper()
		ctx, cancel := context.WithTimeout(testContext, 2*time.Second)
		defer cancel()
		health := make(chan error, 1)
		go func() {
			hctx, hcancel := context.WithTimeout(testContext, 2*time.Second)
			defer hcancel()
			health <- s.Health(hctx)
		}()
		start := time.Now()
		res, err := s.Execute(ctx, cmd, "t")
		if err != nil || time.Since(start) >= time.Second {
			t.Fatalf("%s: %v after %v; the command waited on its own connection", label, err, time.Since(start))
		}
		if err = <-health; err != nil {
			t.Fatalf("%s: concurrent health %v", label, err)
		}
		return res
	}
	owner, joiner, silent, decliner := sealMember(t, s, 1), sealMember(t, s, 2), keyFor(3), keyFor(4)
	registerAll(t, s, silent, decliner)
	run(t, s, signed(silent, Command{Operation: "messaging.policy.set", Data: fmt.Sprintf(`{"schema":1,"inbound_policy":{"schema":1,"allow":[%q]}}`, owner.id())}))
	group := convRoom("pool-more")
	within("open", signed(owner.sign, Command{Operation: "conversation.open", Room: group, Members: []string{keyID(silent)}, Data: `{"schema":1,"kind":"group"}`}))
	secret := within("invite", signed(owner.sign, Command{Operation: "room.invite.create", Room: group})).Data["secret"].(string)
	within("join", signed(joiner.sign, Command{Operation: "room.invite.accept", Room: group, Data: secret}))
	within("silent leave", respond(silent, group, "leave"))
	paid := convRoom("pool-postage")
	within("postage", signed(owner.sign, Command{Operation: "conversation.open", Room: paid, Members: []string{keyID(decliner)}, Data: `{"schema":1,"kind":"dm","postage":2}`}))
	within("decline", respond(decliner, paid, "decline"))
	next, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	joiner.x25519 = next
	within("seal key", signed(joiner.sign, Command{Operation: "identity.link", Data: linkJSON("x25519", joiner.x())}))
	sealed := convRoom("pool-sealed")
	within("open sealed", signed(owner.sign, Command{Operation: "conversation.open", Room: sealed, Members: []string{joiner.id()}, Data: `{"schema":1,"kind":"group","sealed":true}`}))
	within("seal", signed(owner.sign, Command{Operation: "conversation.seal", Room: sealed, Data: rotation(t, memberEpoch(t, s, sealed), 1, owner, joiner)}))
}

// §10 matrix, the worker-key column: every conversation operation and a
// conversation room's governance refuse a worker key with a 403.
func TestConversationMatrixRefusesWorkerKeys(t *testing.T) {
	s, parent, child, grant := delegationFixture(t)
	room := convRoom("worker-matrix")
	openConv(t, s, parent, room, "group")
	for _, c := range []Command{
		{Operation: "conversation.open", Room: convRoom("worker-open"), Data: `{"schema":1,"kind":"group"}`},
		{Operation: "conversation.open", Room: convRoom("worker-sealed"), Data: `{"schema":1,"kind":"group","sealed":true}`},
		{Operation: "conversations.list"},
		{Operation: "conversation.get", Room: room},
		{Operation: "conversation.respond", Room: room, Data: `{"schema":1,"action":"leave"}`},
		{Operation: "room.policy.set", Room: room, Data: `{"closed":true}`},
		{Operation: "room.member.add", Room: room, Target: keyID(parent)},
		{Operation: "room.invite.create", Room: room},
		{Operation: "messaging.policy.set", Data: `{"schema":1}`},
		{Operation: "credit.transfer", Target: keyID(parent), Amount: 1},
	} {
		c.Delegation = grant
		_, err := s.Execute(testContext, signed(child, c), "t")
		if e, ok := err.(*Error); !ok || e.Status != 403 {
			t.Errorf("%s under a worker key: %v", c.Operation, err)
		}
	}
}
