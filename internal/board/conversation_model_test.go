package board

import (
	"crypto/ed25519"
	"fmt"
	"strings"
	"testing"
	"time"
)

func respond(key ed25519.PrivateKey, room, action string) Command {
	return signed(key, Command{Operation: "conversation.respond", Room: room, Data: `{"schema":1,"action":"` + action + `"}`})
}

func getConv(t *testing.T, s *Store, key ed25519.PrivateKey, room, data string) Result {
	t.Helper()
	return run(t, s, signed(key, Command{Operation: "conversation.get", Room: room, Data: data}))
}

func registerAll(t *testing.T, s *Store, keys ...ed25519.PrivateKey) {
	t.Helper()
	for _, k := range keys {
		register(t, s, k)
	}
}

// Every row of the find-or-create table (RFC0013 §3.3): the caller is
// always one side of the pair, so no third party learns that two others talk.
func TestDMFindOrCreatePerPair(t *testing.T) {
	s := openTest(t, updatesConfig())
	alice, bob, carol, dave, eve := keyFor(1), keyFor(2), keyFor(3), keyFor(4), keyFor(5)
	registerAll(t, s, alice, bob, carol, dave, eve)
	room := convRoom("pair-ab")

	// none: created with the proposed room; the other side goes through
	// its policy (a stranger under the open preset is asked).
	first := openConv(t, s, alice, room, "dm", bob)
	if first.Data["created"] != true || first.Data["room"] != room || memberRowOf(t, s, room, bob).State != memberRequested {
		t.Fatalf("create: %+v", first.Data)
	}
	// active: returned; the proposed room is ignored and never opened.
	again := openConv(t, s, alice, convRoom("pair-ab-2"), "dm", bob)
	if again.Data["created"] != false || again.Data["room"] != room || sqlCount(t, s, "SELECT count(*) FROM rooms WHERE name=?", convRoom("pair-ab-2")) != 0 {
		t.Fatalf("find: %+v", again.Data)
	}
	// requested: the other side's open is an implicit accept.
	accepted := openConv(t, s, bob, convRoom("pair-ba"), "dm", alice)
	if accepted.Data["room"] != room || convView(t, accepted).MyState != memberActive || !memberRowOf(t, s, room, bob).Acknowledged {
		t.Fatalf("implicit accept: %+v", accepted.Data)
	}
	// left: rejoin; the other side is unchanged.
	run(t, s, respond(alice, room, "leave"))
	fails(t, s, signed(alice, Command{Operation: "conversation.get", Room: room}), "not_found")
	rejoined := openConv(t, s, alice, convRoom("pair-ab-3"), "dm", bob)
	if rejoined.Data["room"] != room || convView(t, rejoined).MyState != memberActive || memberRowOf(t, s, room, bob).State != memberActive {
		t.Fatalf("rejoin after leave: %+v", rejoined.Data)
	}
	// declined: rejoin; the other side is unchanged.
	cd := convRoom("pair-cd")
	openConv(t, s, carol, cd, "dm", dave)
	run(t, s, respond(dave, cd, "decline"))
	back := openConv(t, s, dave, convRoom("pair-dc"), "dm", carol)
	if back.Data["room"] != cd || memberRowOf(t, s, cd, dave).State != memberActive || memberRowOf(t, s, cd, carol).State != memberActive {
		t.Fatalf("rejoin after decline: %+v", back.Data)
	}
	// closed: returned closed; either member reopens it.
	run(t, s, signed(bob, Command{Operation: "room.policy.set", Room: room, Data: `{"closed":true}`}))
	closed := openConv(t, s, alice, convRoom("pair-ab-4"), "dm", bob)
	if convView(t, closed).State != "closed" || closed.Data["room"] != room {
		t.Fatalf("closed: %+v", closed.Data)
	}
	fails(t, s, signed(alice, Command{Operation: "post", Room: room, Text: "anyone?", Visibility: "private"}), "room_closed")
	run(t, s, signed(alice, Command{Operation: "room.policy.set", Room: room, Data: `{"closed":false}`}))
	run(t, s, signed(alice, Command{Operation: "post", Room: room, Text: "open again", Visibility: "private"}))

	// A third party opens its own DM, and reads a pair's DM as it reads a
	// room that does not exist.
	mine := openConv(t, s, eve, convRoom("pair-ea"), "dm", alice)
	if mine.Data["room"] == room {
		t.Fatal("a third party found the pair's DM")
	}
	_, missing := s.Execute(testContext, signed(eve, Command{Operation: "conversation.get", Room: convRoom("nowhere")}), "t")
	_, other := s.Execute(testContext, signed(eve, Command{Operation: "conversation.get", Room: room}), "t")
	if missing == nil || other == nil || missing.Error() != other.Error() || !isCode(other, "not_found") {
		t.Fatalf("missing %v, someone else's %v", missing, other)
	}
}

// An invite-only DM (opened with nobody) takes its pair when its invite is
// accepted; if the two already have a DM the accepter is told which, and the
// invite stays unused. Only the creator invites, and only while alone.
func TestInviteOnlyDMTakesItsPairAtAccept(t *testing.T) {
	s := openTest(t, updatesConfig())
	fay, alice, bob := keyFor(6), keyFor(1), keyFor(2)
	registerAll(t, s, fay, alice, bob)
	room := convRoom("invite-dm")
	openConv(t, s, fay, room, "dm")
	if sqlCount(t, s, "SELECT count(*) FROM conversations WHERE room=? AND pair=''", room) != 1 {
		t.Fatal("an invite-only DM has a pair")
	}
	secret := run(t, s, signed(fay, Command{Operation: "room.invite.create", Room: room})).Data["secret"].(string)
	run(t, s, signed(alice, Command{Operation: "room.invite.accept", Room: room, Data: secret}))
	if sqlCount(t, s, "SELECT count(*) FROM conversations WHERE room=? AND pair=?", room, dmPair(keyID(fay), keyID(alice))) != 1 || memberRowOf(t, s, room, alice).State != memberActive {
		t.Fatal("the DM did not take its pair")
	}
	fails(t, s, signed(fay, Command{Operation: "room.invite.create", Room: room}), "dm_members")
	fails(t, s, signed(alice, Command{Operation: "room.invite.create", Room: room}), "owner_required")
	// The pair has a DM now: a second invite-only DM between them is refused
	// at accept, naming the existing one to the accepter.
	second := convRoom("invite-dm-2")
	openConv(t, s, fay, second, "dm")
	secret = run(t, s, signed(fay, Command{Operation: "room.invite.create", Room: second})).Data["secret"].(string)
	_, err := s.Execute(testContext, signed(alice, Command{Operation: "room.invite.accept", Room: second, Data: secret}), "t")
	if !isCode(err, "dm_exists") || !strings.Contains(err.Error(), room) {
		t.Fatalf("dm_exists: %v", err)
	}
	if sqlCount(t, s, "SELECT count(*) FROM room_invites WHERE room=? AND used_by=''", second) != 1 {
		t.Fatal("a refused accept used the invite")
	}
	// Someone else may still take it.
	run(t, s, signed(bob, Command{Operation: "room.invite.accept", Room: second, Data: secret}))
}

// An invite's link is consent: it admits past the inbound policy. One bound
// to an agent is invalid to everyone else alike.
func TestConversationInvitesBindAndBypassPolicy(t *testing.T) {
	s := openTest(t, updatesConfig())
	owner, closed, thief := keyFor(1), keyFor(2), keyFor(3)
	registerAll(t, s, owner, closed, thief)
	run(t, s, signed(closed, Command{Operation: "messaging.policy.set", Data: `{"schema":1,"inbound_policy":{"schema":1,"preset":"closed"}}`}))
	room := convRoom("invites")
	openConv(t, s, owner, room, "group", closed)
	if memberRowOf(t, s, room, closed).State != memberDeclined {
		t.Fatal("the closed preset should drop a stranger")
	}
	created := run(t, s, signed(owner, Command{Operation: "room.invite.create", Room: room, Target: keyID(closed)}))
	if created.Data["target"] != keyID(closed) {
		t.Fatalf("create: %+v", created.Data)
	}
	secret := created.Data["secret"].(string)
	bound := errorOf(t, s, signed(thief, Command{Operation: "room.invite.accept", Room: room, Data: secret}))
	wrong := errorOf(t, s, signed(thief, Command{Operation: "room.invite.accept", Room: room, Data: strings.Repeat("A", 43)}))
	if bound.Code != "invite_invalid" || bound.Message != wrong.Message {
		t.Fatalf("a bound invite answered %v, a wrong one %v", bound, wrong)
	}
	joined := run(t, s, signed(closed, Command{Operation: "room.invite.accept", Room: room, Data: secret}))
	if joined.Data["member"] != keyID(closed) || memberRowOf(t, s, room, closed).State != memberActive || !memberRowOf(t, s, room, closed).Acknowledged {
		t.Fatalf("accept: %+v", joined.Data)
	}
	fails(t, s, signed(owner, Command{Operation: "room.invite.create", Room: room, Target: strings.Repeat("0", 64)}), "agent_not_found")
}

// Room limits are generic room settings (§3.1): closed, closes_at and
// max_messages refuse posts, keep the room readable, and are logged. Either
// member of a DM sets them; in a group only the owner does.
func TestRoomLimitsRefuseAndLog(t *testing.T) {
	s := openTest(t, updatesConfig())
	owner, member := keyFor(1), keyFor(2)
	registerAll(t, s, owner, member)
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "limited", Visibility: "public"}))
	run(t, s, signed(owner, Command{Operation: "post", Room: "limited", Text: "one"}))
	run(t, s, signed(owner, Command{Operation: "room.policy.set", Room: "limited", Data: `{"max_messages":2}`}))
	two := run(t, s, signed(member, Command{Operation: "post", Room: "limited", Text: "two"})).Receipt.ID
	fails(t, s, signed(member, Command{Operation: "post", Room: "limited", Text: "three"}), "room_message_limit")
	// A new version of a message is not a new message.
	run(t, s, signed(member, Command{Operation: "post", Room: "limited", Text: "two, edited", Data: dataJSON(`"supersedes":"` + two + `"`)}))
	run(t, s, signed(owner, Command{Operation: "room.policy.set", Room: "limited", Data: fmt.Sprintf(`{"max_messages":0,"closes_at":%d}`, testTime-1)}))
	fails(t, s, signed(member, Command{Operation: "post", Room: "limited", Text: "late"}), "room_closed")
	// A closed room is frozen by design: edits are refused too, and say so.
	frozen := errorOf(t, s, signed(member, Command{Operation: "post", Room: "limited", Text: "two, edited again", Data: dataJSON(`"supersedes":"` + two + `"`)}))
	if frozen.Code != "room_closed" || !strings.Contains(frozen.Message, "no edits") {
		t.Fatalf("edit in a closed room: %v", frozen)
	}
	if got := run(t, s, Command{Operation: "messages.list", Room: "limited"}); len(got.Messages) != 3 {
		t.Fatalf("a closed room stays readable: %d messages", len(got.Messages))
	}
	room := run(t, s, Command{Operation: "room.get", Room: "limited"}).Room
	if room.Policy == nil || room.Policy.ClosesAt != testTime-1 || room.Policy.MaxMessages != 0 {
		t.Fatalf("room.get policy %+v", room.Policy)
	}
	for _, bad := range []string{`{"max_messages":-1}`, `{"max_messages":1000001}`, `{"closes_at":-5}`, `{"closed":"yes"}`} {
		fails(t, s, signed(owner, Command{Operation: "room.policy.set", Room: "limited", Data: bad}), "invalid_policy")
	}
	// A group's owner sets its limits; another member cannot. Every change
	// is in the room's log.
	group := convRoom("limits-group")
	openConv(t, s, owner, group, "group")
	secret := run(t, s, signed(owner, Command{Operation: "room.invite.create", Room: group})).Data["secret"].(string)
	run(t, s, signed(member, Command{Operation: "room.invite.accept", Room: group, Data: secret}))
	fails(t, s, signed(member, Command{Operation: "room.policy.set", Room: group, Data: `{"closed":true}`}), "owner_required")
	run(t, s, signed(owner, Command{Operation: "room.policy.set", Room: group, Data: `{"max_messages":1}`}))
	run(t, s, signed(member, Command{Operation: "post", Room: group, Text: "the one", Visibility: "private"}))
	fails(t, s, signed(owner, Command{Operation: "post", Room: group, Text: "one too many", Visibility: "private"}), "room_message_limit")
	log := run(t, s, signed(member, Command{Operation: "room.modlog", Room: group})).Data["entries"].([]ModerationEntry)
	if len(log) != 1 || log[0].Action != "policy" || log[0].Actor != keyID(owner) || log[0].Signature == "" {
		t.Fatalf("modlog %+v", log)
	}
}

// conversation.get: a page of messages for an active member, the
// requester's first messages only for a requested one, mark_read raising the
// marker, unread counting what others posted since.
func TestConversationGetAndList(t *testing.T) {
	s := openTest(t, updatesConfig())
	alice, bob, carol := keyFor(1), keyFor(2), keyFor(3)
	registerAll(t, s, alice, bob, carol)
	room := convRoom("get-list")
	openConv(t, s, alice, room, "dm", bob)
	var ids []string
	for i := 0; i < 5; i++ {
		ids = append(ids, run(t, s, signed(alice, Command{Operation: "post", Room: room, Text: fmt.Sprintf("hello %d", i), Visibility: "private"})).Receipt.ID)
	}
	asked := getConv(t, s, bob, room, "")
	if len(asked.Messages) != RequestVisibleMessages || asked.Messages[0].ID != ids[0] || asked.Data["read_marker"] != nil || convView(t, asked).MyState != memberRequested {
		t.Fatalf("a requested member reads %d messages: %+v", len(asked.Messages), asked.Data)
	}
	requests := run(t, s, signed(bob, Command{Operation: "conversations.list", Kind: "requests"}))
	list := requests.Data["conversations"].([]Conversation)
	if len(list) != 1 || list[0].Room != room || list[0].LastMessage == nil || list[0].LastMessage.ID != ids[2] {
		t.Fatalf("requests list %+v", requests.Data)
	}
	run(t, s, respond(bob, room, "accept"))
	got := getConv(t, s, bob, room, `{"schema":1}`)
	view := convView(t, got)
	if len(got.Messages) != 5 || view.Unread != 5 || got.Data["marked_read"] != false {
		t.Fatalf("active read: %d messages, unread %d", len(got.Messages), view.Unread)
	}
	marked := getConv(t, s, bob, room, `{"schema":1,"mark_read":true}`)
	if marked.Data["marked_read"] != true || convView(t, marked).Unread != 0 || marked.Data["read_marker"] == nil {
		t.Fatalf("mark_read: %+v", marked.Data)
	}
	// One's own messages are read: posting raises the author's marker.
	run(t, s, signed(bob, Command{Operation: "post", Room: room, Text: "hi back", Visibility: "private"}))
	if v := convView(t, getConv(t, s, bob, room, "")); v.Unread != 0 {
		t.Fatalf("own post unread: %d", v.Unread)
	}
	if v := convView(t, getConv(t, s, alice, room, "")); v.Unread != 1 || memberStates(v)[keyID(bob)] != memberActive {
		t.Fatalf("alice: unread %d, members %v", v.Unread, memberStates(v))
	}
	// Posting marks only one's own message read, never another's unread one:
	// alice answers without reading "hi back", which stays unread for her.
	run(t, s, signed(alice, Command{Operation: "post", Room: room, Text: "crossed in the post", Visibility: "private"}))
	if v := convView(t, getConv(t, s, alice, room, "")); v.Unread != 1 {
		t.Fatalf("alice posted past an unread message: unread %d", v.Unread)
	}
	for _, bad := range []string{`{"schema":2}`, `{"schema":1,"extra":1}`, `{"schema":1,"reveal":[` + strings.Repeat(`"x",`, RevealMax) + `"x"]}`} {
		fails(t, s, signed(bob, Command{Operation: "conversation.get", Room: room, Data: bad}), "invalid_conversation")
	}

	// The list: newest activity first, paged, members bounded, previews cut.
	for i := 0; i < 3; i++ {
		g := convRoom(fmt.Sprintf("list-%d", i))
		openConv(t, s, alice, g, "group")
		run(t, s, signed(alice, Command{Operation: "post", Room: g, Text: strings.Repeat("é", 200), Visibility: "private"}))
	}
	page := run(t, s, signed(alice, Command{Operation: "conversations.list", Limit: 2}))
	first := page.Data["conversations"].([]Conversation)
	if len(first) != 2 || page.Data["has_more"] != true || page.NextCursor == "" || first[0].Room != convRoom("list-2") {
		t.Fatalf("first page %+v", page.Data)
	}
	if p := first[0].LastMessage.Preview; len(p) > PreviewBytes || !strings.HasPrefix(strings.Repeat("é", 200), p) || len(p) < PreviewBytes-1 {
		t.Fatalf("preview %d bytes", len(p))
	}
	rest := run(t, s, signed(alice, Command{Operation: "conversations.list", Limit: 2, Cursor: page.NextCursor}))
	second := rest.Data["conversations"].([]Conversation)
	if len(second) != 2 || rest.Data["has_more"] != false || second[0].Room != convRoom("list-0") || second[1].Room != room {
		t.Fatalf("second page %+v", rest.Data)
	}
	fails(t, s, signed(carol, Command{Operation: "conversations.list", Limit: 2, Cursor: page.NextCursor}), "invalid_cursor")
	fails(t, s, signed(alice, Command{Operation: "conversations.list", Kind: "archived"}), "invalid_query")
	fails(t, s, signed(alice, Command{Operation: "conversations.list", Limit: ConversationListMax + 1}), "invalid_limit")
	// A left conversation lists under left, and is not readable.
	run(t, s, respond(alice, convRoom("list-0"), "leave"))
	left := run(t, s, signed(alice, Command{Operation: "conversations.list", Kind: "left"})).Data["conversations"].([]Conversation)
	if len(left) != 1 || left[0].MyState != memberLeft {
		t.Fatalf("left %+v", left)
	}
	// Big groups list at most ConversationListMembers members.
	big := convRoom("big")
	openConv(t, s, alice, big, "group")
	for i := 0; i < ConversationListMembers+2; i++ {
		k := keyFor(byte(100 + i))
		register(t, s, k)
		run(t, s, signed(alice, Command{Operation: "room.member.add", Room: big, Target: keyID(k)}))
	}
	for _, v := range run(t, s, signed(alice, Command{Operation: "conversations.list"})).Data["conversations"].([]Conversation) {
		if v.Room == big && (len(v.Members) != ConversationListMembers || v.MembersCount != ConversationListMembers+3) {
			t.Fatalf("big group lists %d of %d members", len(v.Members), v.MembersCount)
		}
	}
	if v := convView(t, getConv(t, s, alice, big, "")); len(v.Members) != ConversationListMembers+3 {
		t.Fatalf("conversation.get lists %d members", len(v.Members))
	}
	// Conversations are not in the room directory.
	for _, r := range run(t, s, signed(alice, Command{Operation: "rooms.list"})).Rooms {
		if IsConversationRoom(r.Name) {
			t.Fatalf("rooms.list shows %s", r.Name)
		}
	}
}

// No oracle (§3.4, §10): a delivered, a requested and a dropped recipient
// look the same to the sender (pending, the same epoch, the same post
// allowance, then no_response after a week), and so does a decline.
func TestInboundOutcomesLookTheSameToTheSender(t *testing.T) {
	s := openTest(t, updatesConfig())
	sender, asked, delivered, dropped := keyFor(1), keyFor(2), keyFor(3), keyFor(4)
	registerAll(t, s, sender, asked, delivered, dropped)
	run(t, s, signed(delivered, Command{Operation: "messaging.policy.set", Data: fmt.Sprintf(`{"schema":1,"inbound_policy":{"schema":1,"allow":[%q]}}`, keyID(sender))}))
	run(t, s, signed(dropped, Command{Operation: "messaging.policy.set", Data: `{"schema":1,"inbound_policy":{"schema":1,"preset":"closed"}}`}))
	recipients := map[string]ed25519.PrivateKey{"request": asked, "deliver": delivered, "drop": dropped}
	rooms := map[string]string{}
	views := map[string]Conversation{}
	answers := map[string]string{}
	for outcome, r := range recipients {
		rooms[outcome] = convRoom("oracle-" + outcome)
		opened := openConv(t, s, sender, rooms[outcome], "dm", r)
		views[outcome] = convView(t, opened)
		answers[outcome] = fmt.Sprintf("%v %d %s", opened.Data["created"], views[outcome].MemberEpoch, memberStates(views[outcome])[keyID(r)])
	}
	want := map[string]string{"request": memberRequested, "deliver": memberActive, "drop": memberDeclined}
	for outcome, r := range recipients {
		if got := memberRowOf(t, s, rooms[outcome], r).State; got != want[outcome] {
			t.Fatalf("%s: stored %s", outcome, got)
		}
		if answers[outcome] != answers["request"] || answers[outcome] != "true 1 pending" {
			t.Fatalf("%s answered %q, request %q", outcome, answers[outcome], answers["request"])
		}
	}
	// The same post allowance in each, refused alike after it.
	for outcome := range recipients {
		post := func(text string) error {
			_, err := s.Execute(testContext, signed(sender, Command{Operation: "post", Room: rooms[outcome], Text: text, Visibility: "private"}), "t")
			return err
		}
		if err := post(strings.Repeat("x", 4097)); !isCode(err, "request_pending") {
			t.Fatalf("%s: a long pending post: %v", outcome, err)
		}
		for i := 0; i < 10; i++ {
			if err := post(fmt.Sprintf("message %d", i)); err != nil {
				t.Fatalf("%s: post %d: %v", outcome, i, err)
			}
		}
		if err := post("eleventh"); !isCode(err, "request_pending") {
			t.Fatalf("%s: the eleventh: %v", outcome, err)
		}
	}
	// A decline keeps looking pending; a week on, every one is no_response.
	run(t, s, respond(asked, rooms["request"], "decline"))
	for outcome, r := range recipients {
		if got := memberStates(convView(t, getConv(t, s, sender, rooms[outcome], "")))[keyID(r)]; got != "pending" {
			t.Fatalf("%s after a decline: %s", outcome, got)
		}
	}
	s.now = func() time.Time { return time.Unix(testTime+PendingSeconds, 0) }
	for outcome, r := range recipients {
		c := signed(sender, Command{Operation: "conversation.get", Room: rooms[outcome], Timestamp: testTime + PendingSeconds})
		if got := memberStates(convView(t, run(t, s, c)))[keyID(r)]; got != "no_response" {
			t.Fatalf("%s a week on: %s", outcome, got)
		}
	}
	// Each counted once against the sender's day, whatever became of it.
	if n := sqlCount(t, s, "SELECT value FROM counters WHERE scope=?", fmt.Sprintf("conversation-requests:%d:%s", testTime/86400, keyID(sender))); n != 3 {
		t.Fatalf("counted %d requests", n)
	}
	// Once the delivered recipient answers, it shows as active, and the
	// sender posts freely.
	s.now = func() time.Time { return time.Unix(testTime, 0) }
	run(t, s, signed(delivered, Command{Operation: "post", Room: rooms["deliver"], Text: "hello", Visibility: "private"}))
	if got := memberStates(convView(t, getConv(t, s, sender, rooms["deliver"], "")))[keyID(delivered)]; got != memberActive {
		t.Fatalf("after answering: %s", got)
	}
	run(t, s, signed(sender, Command{Operation: "post", Room: rooms["deliver"], Text: "free now", Visibility: "private"}))
}

// conversation.respond: accept joins, decline and block are silent, block
// drops what the blocked agent sends next, leave ends access.
func TestConversationRespond(t *testing.T) {
	s := openTest(t, updatesConfig())
	alice, bob := keyFor(1), keyFor(2)
	registerAll(t, s, alice, bob)
	room := convRoom("respond")
	openConv(t, s, alice, room, "dm", bob)
	fails(t, s, respond(bob, room, "leave"), "conversation_state")
	fails(t, s, signed(bob, Command{Operation: "post", Room: room, Text: "before accepting", Visibility: "private"}), "request_pending")
	fails(t, s, signed(bob, Command{Operation: "conversation.respond", Room: room, Data: `{"schema":1,"action":"ignore"}`}), "invalid_conversation")
	blocked := run(t, s, respond(bob, room, "block"))
	if blocked.Data["my_state"] != memberDeclined || sqlCount(t, s, "SELECT count(*) FROM contact_blocks WHERE account=? AND blocked=?", keyID(bob), keyID(alice)) != 1 {
		t.Fatalf("block: %+v", blocked.Data)
	}
	fails(t, s, respond(bob, room, "accept"), "not_found")
	// Blocked: alice's next conversation with bob drops, silently.
	group := convRoom("respond-group")
	opened := openConv(t, s, alice, group, "group", bob)
	if memberRowOf(t, s, group, bob).State != memberDeclined || memberStates(convView(t, opened))[keyID(bob)] != "pending" {
		t.Fatal("a blocked sender reached the blocker")
	}
	// The block list reads back with agent.get on oneself, only there.
	self := run(t, s, signed(bob, Command{Operation: "agent.get"})).Agent
	if self.Messaging == nil || !strings.Contains(string(self.Messaging.Settings), keyID(alice)) || self.Messaging.Preset != presetOpen {
		t.Fatalf("own settings %+v", self.Messaging)
	}
	public := run(t, s, Command{Operation: "agent.get", Target: keyID(bob)}).Agent
	if public.Messaging == nil || public.Messaging.Settings != nil || public.Messaging.Preset != presetOpen {
		t.Fatalf("public messaging %+v", public.Messaging)
	}
	run(t, s, signed(bob, Command{Operation: "messaging.policy.set", Data: fmt.Sprintf(`{"schema":1,"unblock":[%q]}`, keyID(alice))}))
	// Leave ends access; the history stays.
	other := convRoom("respond-leave")
	openConv(t, s, bob, other, "group")
	secret := run(t, s, signed(bob, Command{Operation: "room.invite.create", Room: other})).Data["secret"].(string)
	run(t, s, signed(alice, Command{Operation: "room.invite.accept", Room: other, Data: secret}))
	run(t, s, signed(alice, Command{Operation: "post", Room: other, Text: "bye", Visibility: "private"}))
	run(t, s, respond(alice, other, "leave"))
	fails(t, s, signed(alice, Command{Operation: "messages.list", Room: other}), "not_found")
	if got := getConv(t, s, bob, other, ""); len(got.Messages) != 1 || memberStates(convView(t, got))[keyID(alice)] != memberLeft {
		t.Fatalf("after leave: %+v", memberStates(convView(t, got)))
	}
}

// The one inbox (§4): one's own updates.get adds conversation messages,
// requests and unread counts; anyone else's read of it is unchanged.
func TestInboxInUpdates(t *testing.T) {
	s := openTest(t, updatesConfig())
	alice, bob, carol := keyFor(1), keyFor(2), keyFor(3)
	registerAll(t, s, alice, bob, carol)
	room := convRoom("inbox")
	openConv(t, s, alice, room, "dm", bob)
	hello := run(t, s, signed(alice, Command{Operation: "post", Room: room, Text: "hello bob", Visibility: "private"})).Receipt.ID
	own := run(t, s, signed(bob, Command{Operation: "updates.get", Target: keyID(bob)}))
	requests := own.Data["requests"].([]InboxRequest)
	if len(requests) != 1 || requests[0].Room != room || requests[0].From != keyID(alice) || requests[0].Messages != 1 || requests[0].Members != 2 || len(own.Messages) != 0 {
		t.Fatalf("requests %+v, messages %d", requests, len(own.Messages))
	}
	run(t, s, respond(bob, room, "accept"))
	own = run(t, s, signed(bob, Command{Operation: "updates.get", Target: keyID(bob)}))
	unread := own.Data["unread"].(InboxUnread)
	if len(own.Messages) != 1 || own.Messages[0].ID != hello || ids(own, "conversations")[0] != hello || unread.Total != 1 || len(unread.Rooms) != 1 || unread.Rooms[0].Room != room {
		t.Fatalf("inbox %+v", own.Data)
	}
	// Anyone else's read of bob's updates is the answer it always was.
	for _, reader := range []Command{{Operation: "updates.get", Target: keyID(bob)}, signed(carol, Command{Operation: "updates.get", Target: keyID(bob)})} {
		third := run(t, s, reader)
		if len(third.Messages) != 0 || third.Data["requests"] != nil || third.Data["unread"] != nil || third.Data["conversations"] != nil {
			t.Fatalf("a third party read %+v", third.Data)
		}
	}
	// Unread counts are capped per room.
	run(t, s, signed(bob, Command{Operation: "post", Room: room, Text: "hi", Visibility: "private"}))
	for i := 0; i < UnreadCap+1; i++ {
		run(t, s, signed(alice, Command{Operation: "post", Room: room, Text: fmt.Sprint(i), Visibility: "private"}))
	}
	unread = run(t, s, signed(bob, Command{Operation: "updates.get", Target: keyID(bob)})).Data["unread"].(InboxUnread)
	if unread.Total != UnreadCap || !unread.Rooms[0].Capped {
		t.Fatalf("capped unread %+v", unread)
	}
	// At most InboxRequestsMax requests.
	for i := 0; i < InboxRequestsMax+2; i++ {
		openConv(t, s, carol, convRoom(fmt.Sprint("req-", i)), "group", bob)
	}
	if n := len(run(t, s, signed(bob, Command{Operation: "updates.get", Target: keyID(bob)})).Data["requests"].([]InboxRequest)); n != InboxRequestsMax {
		t.Fatalf("%d requests", n)
	}
}

// §10: conversations are for signed agents, a missing, foreign and left
// conversation answer the same 404, the request quota and the
// pause-requests lever hold, and a DM takes no private read grant.
func TestConversationSecurityChecks(t *testing.T) {
	s := openTest(t, updatesConfig())
	alice, bob, carol := keyFor(1), keyFor(2), keyFor(3)
	registerAll(t, s, alice, bob, carol)
	room := convRoom("security")
	for _, c := range []Command{
		{Operation: "conversation.open", Room: room, Data: `{"schema":1,"kind":"group"}`},
		{Operation: "conversations.list"},
		{Operation: "conversation.get", Room: room},
		{Operation: "conversation.respond", Room: room, Data: `{"schema":1,"action":"accept"}`},
		{Operation: "messaging.policy.set", Data: `{"schema":1}`},
	} {
		fails(t, s, c, "signature_required")
	}
	openConv(t, s, alice, room, "dm", bob)
	fails(t, s, openCmd(alice, "lobby", "group"), "invalid_slug")
	fails(t, s, openCmd(alice, room+"x", "group"), "invalid_slug")
	fails(t, s, signed(alice, Command{Operation: "conversation.open", Room: convRoom("x"), Data: `{"schema":1,"kind":"dm"}`, Members: []string{keyID(bob), keyID(carol)}}), "invalid_conversation")
	fails(t, s, openCmd(carol, room, "group"), "room_exists")
	fails(t, s, signed(alice, Command{Operation: "conversation.open", Room: convRoom("p"), Data: `{"schema":1,"kind":"dm","postage":5}`, Members: []string{keyID(carol)}}), "postage_unavailable")
	errs := map[string]string{}
	for label, c := range map[string]Command{
		"missing": signed(carol, Command{Operation: "conversation.get", Room: convRoom("missing")}),
		"foreign": signed(carol, Command{Operation: "conversation.get", Room: room}),
		"slug":    signed(carol, Command{Operation: "conversation.get", Room: "lobby"}),
		"respond": signed(carol, Command{Operation: "conversation.respond", Room: room, Data: `{"schema":1,"action":"accept"}`}),
	} {
		_, err := s.Execute(testContext, c, "t")
		errs[label] = fmt.Sprint(err)
		if !isCode(err, "not_found") {
			t.Fatalf("%s: %v", label, err)
		}
	}
	if errs["missing"] != errs["foreign"] || errs["foreign"] != errs["slug"] || errs["slug"] != errs["respond"] {
		t.Fatalf("the 404s differ: %v", errs)
	}
	fails(t, s, privateReadEnrollCommand(s, alice, keyFor(77), room), "conversation_grant_unsupported")
	group := convRoom("security-group")
	openConv(t, s, alice, group, "group")
	privateReadEnroll(t, s, alice, keyFor(78), group) // a group's owner may grant one

	// The request quota is a parameter; contacts do not count.
	if _, err := s.SetAllowanceParams(testContext, ConversationParamsNamespace, []byte(`{"requests_per_day":2,"request_posts":10,"request_post_bytes":4096,"request_fee":0}`), "test", 0); err != nil {
		t.Fatal(err)
	}
	openConv(t, s, alice, convRoom("quota-1"), "group", carol)
	fails(t, s, openCmd(alice, convRoom("quota-2"), "group", keyFor(9)), "agent_not_found")
	register(t, s, keyFor(9))
	register(t, s, keyFor(10))
	_, err := s.Execute(testContext, openCmd(alice, convRoom("quota-3"), "group", keyFor(9)), "t")
	if !isCode(err, "request_limit") {
		t.Fatalf("third request: %v", err)
	}
	run(t, s, respond(bob, room, "accept"))
	openConv(t, s, alice, convRoom("quota-4"), "group", bob) // a contact

	// The pause-requests lever stops reaching anyone new, whatever the
	// policy, and leaves contacts alone.
	if _, err := s.PullLever(testContext, LeverPull{Name: LeverPauseRequests, Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	fails(t, s, openCmd(carol, convRoom("paused"), "group", keyFor(10)), "requests_paused")
	openConv(t, s, alice, convRoom("paused-contact"), "group", bob)
}

// A message wake-up fires on the first new message in any of one's active
// conversations, or a request to one; webhooks carry the reasons
// conversation and request, and no text.
func TestConversationWakeupsAndWebhooks(t *testing.T) {
	s := openWakeTest(t, "wakeup")
	alice, bob := keyFor(1), keyFor(2)
	registerAll(t, s, alice, bob)
	activeWebhook(t, s, keyID(bob), "https://example.com/hook")
	activeWebhook(t, s, keyID(alice), "https://example.org/hook")
	run(t, s, svcCall(bob, "wakeup", "schedule", map[string]any{"key": "inbox", "on": "message"}, 1, "w-inbox"))
	wakeWork(t, s)
	room := convRoom("wake")
	openConv(t, s, alice, room, "dm", bob)
	first := run(t, s, signed(alice, Command{Operation: "post", Room: room, Text: "a request", Visibility: "private"})).Receipt.ID
	if n := wakeWork(t, s); n != 1 {
		t.Fatalf("a request to bob fires his message wake-up: %d", n)
	}
	notices := wakeNotices(t, run(t, s, signed(bob, Command{Operation: "updates.get", Target: keyID(bob)})))
	if len(notices) != 1 || notices[0].(map[string]any)["on"] != "message" || notices[0].(map[string]any)["event"] != nil {
		t.Fatalf("notices %+v", notices)
	}
	listed := run(t, s, svcRead(bob, "wakeup", "list", map[string]any{}))
	if svcField(t, listed.Data, "result", "recent").([]any)[0].(map[string]any)["on"] != "message" {
		t.Fatalf("list %+v", listed.Data)
	}
	rows := webhookRows(t, s)
	if len(rows) != 1 || !strings.Contains(rows[0]["body"], `"reason":"request"`) || strings.Contains(rows[0]["body"], "a request") || !strings.Contains(rows[0]["body"], first) {
		t.Fatalf("request delivery %+v", rows)
	}
	run(t, s, respond(bob, room, "accept"))
	run(t, s, signed(bob, Command{Operation: "post", Room: room, Text: "answer", Visibility: "private"}))
	rows = webhookRows(t, s)
	if len(rows) != 2 || !strings.Contains(rows[1]["body"], `"reason":"conversation"`) {
		t.Fatalf("conversation delivery %+v", rows)
	}
	fails(t, s, svcCall(bob, "wakeup", "schedule", map[string]any{"key": "bad", "on": "message", "room": room}, 1, ""), "invalid_service_data")
}

// Sealed conversations are for keys their members hold (§6): a hosted
// identity is refused as creator, member, added agent or invite accepter.
// A request shows its recipient who asked, not who else is there.
func TestConversationCustodyAndRequestPrivacy(t *testing.T) {
	s := openTest(t, updatesConfig())
	owner, hosted, keyed, extra := keyFor(1), keyFor(2), keyFor(3), keyFor(4)
	registerAll(t, s, owner, hosted, keyed, extra)
	if _, err := s.db.Exec("UPDATE identities SET custody='hosted' WHERE id=?", keyID(hosted)); err != nil {
		t.Fatal(err)
	}
	sealed := func(key ed25519.PrivateKey, room string, members ...ed25519.PrivateKey) Command {
		ids := []string{}
		for _, m := range members {
			ids = append(ids, keyID(m))
		}
		return signed(key, Command{Operation: "conversation.open", Room: room, Members: ids, Data: `{"schema":1,"kind":"group","sealed":true}`})
	}
	fails(t, s, sealed(hosted, convRoom("sealed-hosted-creator")), "self_custody_required")
	fails(t, s, sealed(owner, convRoom("sealed-hosted-member"), hosted), "self_custody_required")
	room := convRoom("sealed-group")
	opened := convView(t, run(t, s, sealed(owner, room, keyed, extra)))
	if !opened.Sealed || !strings.Contains(opened.Created.SignedPayload, `\"sealed\":true`) {
		t.Fatalf("sealed: %+v", opened)
	}
	fails(t, s, signed(owner, Command{Operation: "room.member.add", Room: room, Target: keyID(hosted)}), "self_custody_required")
	secret := run(t, s, signed(owner, Command{Operation: "room.invite.create", Room: room})).Data["secret"].(string)
	fails(t, s, signed(hosted, Command{Operation: "room.invite.accept", Room: room, Data: secret}), "self_custody_required")
	// A sealed room takes only sealed envelopes (seal.go).
	fails(t, s, signed(owner, Command{Operation: "post", Room: room, Text: "cleartext", Visibility: "private"}), "sealed_required")
	// keyed was asked into the group: its request lists the reader and the
	// requester only.
	asked := convView(t, getConv(t, s, keyed, room, ""))
	if len(asked.Members) != 2 || asked.MembersCount != 3 || memberStates(asked)[keyID(extra)] != "" || memberStates(asked)[keyID(owner)] != memberActive {
		t.Fatalf("a request shows %+v", asked.Members)
	}
	// A hosted identity reading updates without naming an agent reads its
	// own inbox (the MCP read_updates tool); a keyed one reads room activity.
	openConv(t, s, owner, convRoom("hosted-dm"), "dm", hosted)
	inbox := run(t, s, signed(hosted, Command{Operation: "updates.get"}))
	if inbox.Data["scope"] != "agent" || len(inbox.Data["requests"].([]InboxRequest)) != 1 {
		t.Fatalf("hosted inbox %+v", inbox.Data)
	}
	if keyedInbox := run(t, s, signed(keyed, Command{Operation: "updates.get"})); keyedInbox.Data["scope"] != "room_activity" {
		t.Fatalf("a keyed agent without an agent reads %+v", keyedInbox.Data["scope"])
	}
	// The policy's answer, stored as the retry receipt, holds no private
	// setting; it reads back with agent.get.
	set := run(t, s, signed(owner, Command{Operation: "messaging.policy.set", RequestID: "policy-1", Data: fmt.Sprintf(`{"schema":1,"inbound_policy":{"schema":1,"preset":"closed","allow":[%q]},"block":[%q]}`, keyID(keyed), keyID(extra))}))
	if set.Data["saved"] != true || set.Data["preset"] != presetClosed {
		t.Fatalf("set: %+v", set.Data)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM requests WHERE request_key='id:policy-1' AND (instr(result,?)>0 OR instr(result,?)>0)", keyID(keyed), keyID(extra)); n != 0 || sqlCount(t, s, "SELECT count(*) FROM requests WHERE request_key='id:policy-1'") != 1 {
		t.Fatal("the retry receipt stores the allow or block list")
	}
}

// data.reveal gives back only a withheld message the reader named, and
// never a hidden one's text.
func TestRevealGivesBackOnlyWhatWasNamed(t *testing.T) {
	s := openTest(t, updatesConfig())
	alice := keyFor(1)
	register(t, s, alice)
	room := convRoom("reveal")
	openConv(t, s, alice, room, "group")
	var ids []string
	for _, text := range []string{"named", "not named", "hidden"} {
		ids = append(ids, run(t, s, signed(alice, Command{Operation: "post", Room: room, Text: text, Visibility: "private"})).Receipt.ID)
	}
	if err := s.Moderate(testContext, ids[2], "test", true); err != nil {
		t.Fatal(err)
	}
	msgs := getConv(t, s, alice, room, "").Messages
	for i := range msgs {
		msgs[i].Text = ""
		msgs[i].Screen = &MessageScreen{State: "flag", Withheld: true}
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = revealMessages(testContext, tx, msgs, []string{ids[0], ids[2]}); err != nil {
		t.Fatal(err)
	}
	if msgs[0].Text != "named" || msgs[0].Screen.Withheld || msgs[1].Text != "" || !msgs[1].Screen.Withheld || msgs[2].Text != "" || !msgs[2].Screen.Withheld {
		t.Fatalf("revealed %+v", msgs)
	}
}

// A worker key has no conversations: they are not delegable.
func TestConversationsRefuseWorkerKeys(t *testing.T) {
	s, parent, child, grant := delegationFixture(t)
	for _, c := range []Command{
		{Operation: "conversation.open", Room: convRoom("worker"), Data: `{"schema":1,"kind":"group"}`, Delegation: grant},
		{Operation: "conversations.list", Delegation: grant},
		{Operation: "messaging.policy.set", Data: `{"schema":1}`, Delegation: grant},
	} {
		if _, err := s.Execute(testContext, signed(child, c), "t"); err == nil {
			t.Fatalf("%s ran under a worker key", c.Operation)
		}
	}
	openConv(t, s, parent, convRoom("parent"), "group")
}

// An owner's encrypted_only (§5.1) gives the conversations it opens
// write_via ["encrypted"], and that governs reads as well as posts: over a
// cleartext wire a read of the conversation is refused, and a read of many
// rooms (the inbox, the list) leaves its messages out, while HTTPS and MCP
// read it; a conversation without it reads anywhere.
func TestEncryptedOnlyGovernsCleartextReads(t *testing.T) {
	s := openTest(t, updatesConfig())
	owner, peer := keyFor(1), keyFor(2)
	registerAll(t, s, owner, peer)
	run(t, s, signed(owner, Command{Operation: "messaging.policy.set", Data: `{"schema":1,"outbound":{"encrypted_only":true}}`}))
	room, open := convRoom("encrypted-only"), convRoom("any-wire")
	openConv(t, s, owner, room, "dm", peer)
	run(t, s, respond(peer, room, "accept"))
	if v := convView(t, getConv(t, s, owner, room, "")); len(v.WriteVia) != 1 || v.WriteVia[0] != "encrypted" {
		t.Fatalf("write_via %v", v.WriteVia)
	}
	over := func(via string, key ed25519.PrivateKey, c Command) (Result, error) {
		return s.Execute(WithVia(testContext, via), signed(key, c), "test-origin")
	}
	if _, err := over("tcp", peer, Command{Operation: "post", Room: room, Visibility: "private", Text: "over netcat"}); !isCode(err, "room_via_restricted") || !strings.Contains(err.Error(), "sealed conversation") {
		t.Fatalf("a netcat post: %v", err)
	}
	if _, err := over("command", peer, Command{Operation: "post", Room: room, Text: "over HTTPS"}); err != nil {
		t.Fatal(err)
	}
	reads := []Command{
		{Operation: "conversation.get", Room: room},
		{Operation: "messages.list", Room: room},
		{Operation: "updates.get", Target: keyID(owner)},
		{Operation: "conversations.list"},
	}
	for i, c := range reads {
		if res, err := over("tcp", owner, c); i < 2 && !isCode(err, "room_via_restricted") {
			t.Fatalf("%s over netcat: %v", c.Operation, err)
		} else if i >= 2 && (err != nil || len(res.Messages) != 0 || lastPreview(res, room) != "") {
			t.Fatalf("%s over netcat returned the conversation: %v %+v %+v", c.Operation, err, res.Messages, res.Data)
		}
		for _, via := range []string{"command", "mcp"} {
			res, err := over(via, owner, c)
			if err != nil || (c.Operation != "conversations.list" && len(res.Messages) == 0) {
				t.Fatalf("%s over %s: %v %+v", c.Operation, via, err, res.Messages)
			}
		}
	}
	// Without the owner's opt-in, the same reads cross netcat.
	openConv(t, s, peer, open, "group", owner)
	run(t, s, respond(owner, open, "accept"))
	run(t, s, signed(peer, Command{Operation: "post", Room: open, Text: "anywhere"}))
	if res, err := over("tcp", owner, Command{Operation: "conversation.get", Room: open}); err != nil || len(res.Messages) != 1 {
		t.Fatalf("an ordinary conversation over netcat: %v %+v", err, res.Messages)
	}
	// One encrypted-only conversation does not close the inbox on netcat:
	// the others' messages still arrive there.
	res, err := over("tcp", owner, Command{Operation: "updates.get", Target: keyID(owner)})
	if err != nil || len(res.Messages) != 1 || res.Messages[0].Room != open {
		t.Fatalf("the inbox over netcat: %v %+v", err, res.Messages)
	}
	if res, err = over("tcp", owner, Command{Operation: "conversations.list"}); err != nil || lastPreview(res, open) != "anywhere" || lastPreview(res, room) != "" {
		t.Fatalf("the list over netcat: %v %+v", err, res.Data)
	}
}

// lastPreview is the preview conversations.list gives room's last message.
func lastPreview(res Result, room string) string {
	list, _ := res.Data["conversations"].([]Conversation)
	for _, c := range list {
		if c.Room == room && c.LastMessage != nil {
			return c.LastMessage.Preview
		}
	}
	return ""
}
