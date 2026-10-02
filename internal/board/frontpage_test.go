package board

import (
	"strings"
	"testing"
)

func feedIDs(r Result) map[string]bool {
	out := map[string]bool{}
	for _, m := range r.Messages {
		out[m.ID] = true
	}
	return out
}

// A bounded initial scan still merges operator-enabled rooms and keeps the
// newest delivered message as its forward polling cursor.
func TestNewestFrontPageAcrossExcludedFlood(t *testing.T) {
	s := openTest(t, Config{})
	run(t, s, Command{Operation: "post", Room: "lobby", Text: "oldest"})
	bounty := run(t, s, Command{Operation: "post", Room: "bounties", Text: "older bounty"}).Receipt.ID
	if _, err := s.OperatorRoom(testContext, Command{Operation: "room.policy.set", Room: "bounties", Data: `{"front_page":true}`}); err != nil {
		t.Fatal(err)
	}
	owner := keyFor(172)
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "noisy"}))
	run(t, s, signed(owner, Command{Operation: "room.policy.set", Room: "noisy", Data: `{"front_page":false}`}))
	flood := postAs(t, s, owner, Command{Room: "noisy", Text: "excluded", RequestID: "flood"})
	secFlood(t, s, flood, FrontScanRows+100)
	late := run(t, s, Command{Operation: "post", Room: "lobby", Text: "newest"}).Receipt.ID
	c := Command{Operation: "messages.list", Limit: 5, Data: `{"sort":"new"}`}
	first := run(t, s, c)
	if got := strings.Join(secIDs(first), ","); got != late || first.OlderCursor == "" || first.Data["has_more"] != true {
		t.Fatalf("initial page lost the newest or operator-enabled message: %v, %v", got, first.Data)
	}
	c.Older = first.OlderCursor
	seen := feedIDs(first)
	for c.Older != "" {
		older := run(t, s, c)
		for _, m := range older.Messages {
			if seen[m.ID] {
				t.Fatal("duplicate across bounded backward scans")
			}
			seen[m.ID] = true
		}
		c.Older = older.OlderCursor
	}
	if !seen[bounty] || len(seen) != 3 {
		t.Fatalf("bounded backward scan lost history: %v", seen)
	}
	c.Cursor = first.NextCursor
	if empty := run(t, s, c); len(empty.Messages) != 0 || empty.NextCursor != c.Cursor {
		t.Fatal("scan boundary must not move the cursor behind delivered messages")
	}
	newLobby := run(t, s, Command{Operation: "post", Room: "lobby", Text: "new lobby arrival"}).Receipt.ID
	newBounty := run(t, s, Command{Operation: "post", Room: "bounties", Text: "new bounty arrival"}).Receipt.ID
	for _, data := range []string{c.Data, ""} {
		c.Data = data
		second := run(t, s, c)
		if got := strings.Join(secIDs(second), ","); got != newLobby+","+newBounty || second.Data["has_more"] != false {
			t.Fatalf("forward poll skipped or repeated messages: %s, %v", got, second.Data)
		}
	}
	for _, room := range []string{"lobby", "bounties"} {
		if _, err := s.OperatorRoom(testContext, Command{Operation: "room.policy.set", Room: room, Data: `{"front_page":false}`}); err != nil {
			t.Fatal(err)
		}
	}
	c.Cursor, c.Older, c.Data = "", "", `{"sort":"new"}`
	if empty := run(t, s, c); len(empty.Messages) != 0 || empty.OlderCursor != "" {
		t.Fatal("excluded history must not advertise an older page")
	}
}

// The default all-rooms feed shows front-page rooms only, in every order;
// a room read, scope=all, search and /api/updates still see the rest.
func TestFrontPageFeed(t *testing.T) {
	s := openTest(t, updatesConfig())
	author, other := keyFor(90), keyFor(91)
	lobby := postAs(t, s, author, Command{Room: "lobby", Text: "a discussion", RequestID: "l"})
	bounty := postAs(t, s, author, Command{Room: "bounties", Text: "a bounty", RequestID: "b"})
	sandbox := run(t, s, Command{Operation: "post", Room: "sandbox", Text: "test test"}).Receipt.ID
	personal := postAs(t, s, author, Command{Room: PersonalRoom(keyID(author)), Text: "my own room", RequestID: "p"})
	saved := run(t, s, Command{Operation: "updates.get", Target: keyID(author)}).NextCursor
	reply := postAs(t, s, other, Command{Room: "bounties", Text: "I will take it", ReplyTo: bounty, RequestID: "r"})

	for name, c := range map[string]Command{
		"hot":    FirstContact(Command{Operation: "messages.list"}),
		"new":    {Operation: "messages.list", Data: `{"sort":"new"}`},
		"cursor": {Operation: "messages.list", Cursor: "start"},
		"plain":  {Operation: "messages.list"},
	} {
		got := feedIDs(run(t, s, c))
		if !got[lobby] || got[bounty] || got[sandbox] || got[personal] || got[reply] {
			t.Errorf("%s: the front page shows utility or personal rooms: %v", name, got)
		}
	}
	for name, c := range map[string]Command{
		"scope all":     {Operation: "messages.list", Data: AllRooms},
		"scope all hot": FirstContact(Command{Operation: "messages.list", Data: AllRooms}),
		"room":          FirstContact(Command{Operation: "messages.list", Room: "bounties"}),
		"search":        {Operation: "messages.list", Query: "bounty"},
	} {
		if got := feedIDs(run(t, s, c)); !got[bounty] {
			t.Errorf("%s: a utility room is not one read away: %v", name, got)
		}
	}
	if FirstContact(Command{Operation: "messages.list", Data: AllRooms}).Data != `{"sort":"hot","scope":"all"}` {
		t.Error("scope=all loses the first-contact order")
	}
	// Replies to you always arrive, wherever they were posted.
	if back := run(t, s, Command{Operation: "updates.get", Target: keyID(author), Cursor: saved}); !feedIDs(back)[reply] {
		t.Fatalf("updates missed a reply in a room off the front page: %v", back.Data)
	}
	fails(t, s, Command{Operation: "messages.list", Data: `{"scope":"everything"}`}, "invalid_scope")
}

// Owners and moderators take a room off the front page; only the operator
// puts one on.
func TestFrontPagePolicy(t *testing.T) {
	s := openTest(t, Config{})
	owner, mod := keyFor(92), keyFor(93)
	register(t, s, mod)
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "workshop"}))
	run(t, s, signed(owner, Command{Operation: "room.moderator.add", Room: "workshop", Target: keyID(mod)}))
	post := postAs(t, s, owner, Command{Room: "workshop", Text: "hello", RequestID: "w"})
	front := func() bool { return feedIDs(run(t, s, Command{Operation: "messages.list"}))[post] }
	policy := func() RoomPolicy { return *run(t, s, Command{Operation: "room.get", Room: "workshop"}).Room.Policy }
	if !front() || !policy().FrontPage {
		t.Fatal("a new room starts on the front page")
	}
	// A moderator may opt the room out, and do nothing else with the policy.
	run(t, s, signed(mod, Command{Operation: "room.policy.set", Room: "workshop", Data: `{"front_page":false}`}))
	if front() || policy().FrontPage {
		t.Fatal("opting out left the room on the front page")
	}
	fails(t, s, signed(mod, Command{Operation: "room.policy.set", Room: "workshop", Data: `{"front_page":false,"write":"owner"}`}), "owner_required")
	// A moderator cannot opt back in; the owner can, to the default (true
	// while the default is on, or null), so one moderator's opt-out is not
	// final.
	fails(t, s, signed(mod, Command{Operation: "room.policy.set", Room: "workshop", Data: `{"front_page":true}`}), "owner_required")
	run(t, s, signed(owner, Command{Operation: "room.policy.set", Room: "workshop", Data: `{"front_page":true}`}))
	if !front() || !policy().FrontPage {
		t.Fatal("the owner could not undo the opt-out")
	}
	run(t, s, signed(mod, Command{Operation: "room.policy.set", Room: "workshop", Data: `{"front_page":false}`}))
	run(t, s, signed(owner, Command{Operation: "room.policy.set", Room: "workshop", Data: `{"front_page":null}`}))
	if !front() {
		t.Fatal("null did not restore the default")
	}
	fails(t, s, signed(owner, Command{Operation: "room.policy.set", Room: "workshop", Data: `{"front_page":"yes"}`}), "invalid_policy")
	run(t, s, signed(mod, Command{Operation: "room.policy.set", Room: "workshop", Data: `{"front_page":false}`}))
	// Other policy changes keep the setting.
	run(t, s, signed(owner, Command{Operation: "room.policy.set", Room: "workshop", Data: `{"rules":"be kind"}`}))
	if policy().FrontPage {
		t.Fatal("a policy change reset front_page")
	}
	// A personal room is off by default and its owner cannot put it on.
	personal := PersonalRoom(keyID(owner))
	fails(t, s, signed(owner, Command{Operation: "room.policy.set", Room: personal, Data: `{"front_page":true}`}), "front_page_operator")
	// The operator decides for operator rooms, both ways.
	bountyID := postAs(t, s, owner, Command{Room: "bounties", Text: "a bounty", RequestID: "b"})
	if got := run(t, s, Command{Operation: "rooms.list", Query: "bounties"}).Rooms; len(got) != 1 || got[0].Policy.FrontPage {
		t.Fatalf("bounties starts off the front page: %+v", got)
	}
	if _, err := s.OperatorRoom(testContext, Command{Operation: "room.policy.set", Room: "bounties", Data: `{"front_page":true}`}); err != nil {
		t.Fatal(err)
	}
	if got := run(t, s, Command{Operation: "rooms.list", Query: "bounties"}).Rooms; !got[0].Policy.FrontPage {
		t.Fatal("the operator could not put a room on the front page")
	}
	// A room put on by the operator is read by both front-page reads, though
	// its default keeps it out of their indexes.
	chrono := feedIDs(run(t, s, Command{Operation: "messages.list"}))
	hot := feedIDs(run(t, s, Command{Operation: "messages.list", Data: `{"sort":"hot"}`}))
	if !chrono[bountyID] || !hot[bountyID] {
		t.Fatalf("the operator's room is missing from the front page: new %v hot %v", chrono, hot)
	}
	// The operator may set front_page, and only front_page, on a room a key
	// owns; the operator's opt-out is the operator's to reverse.
	if _, err := s.OperatorRoom(testContext, Command{Operation: "room.policy.set", Room: "workshop", Data: `{"rules":"mine"}`}); errCode(err) != "owner_required" {
		t.Fatalf("operator changed a key-owned room's rules: %v", err)
	}
	if _, err := s.OperatorRoom(testContext, Command{Operation: "room.policy.set", Room: "workshop", Data: `{"front_page":false}`}); err != nil {
		t.Fatal(err)
	}
	fails(t, s, signed(owner, Command{Operation: "room.policy.set", Room: "workshop", Data: `{"front_page":null}`}), "front_page_operator")
	fails(t, s, signed(owner, Command{Operation: "room.policy.set", Room: "workshop", Data: `{"front_page":true}`}), "front_page_operator")
	run(t, s, signed(owner, Command{Operation: "room.policy.set", Room: "workshop", Data: `{"front_page":false}`}))
	if front() {
		t.Fatal("the owner's opt-out lifted the operator's")
	}
	if _, err := s.OperatorRoom(testContext, Command{Operation: "room.policy.set", Room: "workshop", Data: `{"front_page":null}`}); err != nil {
		t.Fatal(err)
	}
	if !front() {
		t.Fatal("the operator could not restore a key-owned room's default")
	}
	log := run(t, s, Command{Operation: "room.modlog", Room: "workshop"}).Data["entries"].([]ModerationEntry)
	logged := false
	for _, e := range log {
		logged = logged || (e.Actor == keyID(mod) && strings.Contains(e.Detail, `"front_page":false`))
	}
	if !logged {
		t.Fatalf("the moderator's opt-out is not in the room's public log: %+v", log)
	}
}
