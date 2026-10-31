package board

import (
	"strings"
	"testing"
	"time"
)

// A member addressing a fingerprint in a masked room would read back its
// alias and so tie every post under that alias to the key: a masked room's
// members address each other by alias only, and the recipient still hears.
func TestMaskedRoomRecipientIsAnAlias(t *testing.T) {
	r := newAppRooms(t)
	s := r.s
	room := r.room("whisper")
	run(t, s, signed(r.app, Command{Operation: "room.create", Room: room, Members: []string{keyID(r.alice), keyID(r.bob), keyID(r.carol)}, Data: `{"reveal_on_close":true}`, RequestID: "c"}))
	first := run(t, s, signed(r.alice, Command{Operation: "post", Room: room, Text: "hello", RequestID: "a1"}))
	fails(t, s, signed(r.bob, Command{Operation: "post", Room: room, To: keyID(r.alice), Text: "psst", RequestID: "b1"}), "invalid_recipient")
	fails(t, s, signed(r.bob, Command{Operation: "post", Room: room, To: keyID(r.carol), Text: "psst", RequestID: "b1c"}), "invalid_recipient")
	alias := run(t, s, signed(r.bob, Command{Operation: "message.get", MessageID: first.Receipt.ID})).Messages[0].Author
	fails(t, s, signed(r.bob, Command{Operation: "post", Room: room, To: MaskAliasPrefix + strings.Repeat("0", 16), Text: "psst", RequestID: "b2"}), "invalid_recipient")
	fails(t, s, signed(r.bob, Command{Operation: "post", Room: "lobby", To: alias, Text: "psst", RequestID: "b3"}), "invalid_recipient")
	sent := run(t, s, signed(r.bob, Command{Operation: "post", Room: room, To: alias, Text: "psst", RequestID: "b4"}))
	got := run(t, s, signed(r.bob, Command{Operation: "message.get", MessageID: sent.Receipt.ID})).Messages[0]
	if got.To != alias || got.Via != "" {
		t.Fatalf("to = %q, want %q", got.To, alias)
	}
	// The app reads the real recipient; alice hears it as addressed to her.
	if m := run(t, s, signed(r.app, Command{Operation: "message.get", MessageID: sent.Receipt.ID})).Messages[0]; m.To != keyID(r.alice) {
		t.Fatalf("stored to = %q", m.To)
	}
	found := false
	for _, m := range run(t, s, signed(r.alice, Command{Operation: "updates.get"})).Messages {
		found = found || m.ID == sent.Receipt.ID
	}
	if !found {
		t.Fatal("alice's updates lack the message addressed to her alias")
	}
	run(t, s, signed(r.app, Command{Operation: "room.close", Room: room, RequestID: "k"}))
	if m := run(t, s, signed(r.carol, Command{Operation: "message.get", MessageID: sent.Receipt.ID})).Messages[0]; m.To != keyID(r.alice) {
		t.Fatalf("after close to = %q", m.To)
	}
}

// A member the app removed reads its old inbox entries of the masked room
// masked too: masking never depends on being a member now.
func TestMaskedRoomRemovedMemberReadsMasked(t *testing.T) {
	r := newAppRooms(t)
	s := r.s
	room := r.room("left")
	run(t, s, signed(r.app, Command{Operation: "room.create", Room: room, Members: []string{keyID(r.alice), keyID(r.bob)}, Data: `{"reveal_on_close":true}`, RequestID: "c"}))
	root := run(t, s, signed(r.alice, Command{Operation: "post", Room: room, Text: "who am I", RequestID: "a1"}))
	run(t, s, signed(r.bob, Command{Operation: "post", Room: room, ReplyTo: root.Receipt.ID, Text: "a villager", RequestID: "b1"}))
	run(t, s, signedNow(s, r.app, Command{Operation: "room.member.remove", Room: room, Target: keyID(r.alice), RequestID: "rm"}))
	for _, op := range []string{"journal.get", "home", "updates.get", "account.export"} {
		res, err := s.Execute(testContext, signed(r.alice, Command{Operation: op}), "test-origin")
		if err != nil {
			continue // an operation this store does not serve
		}
		raw := mustJSON(t, res)
		for _, secret := range []string{keyID(r.bob), pubOf(r.bob), "bobby"} {
			if strings.Contains(raw, secret) {
				t.Errorf("%s leaks %q after removal: %s", op, secret, raw)
			}
		}
	}
}

// Every webhook delivery of a masked room leaves out its recipient, on
// both inbox modes: a member would tie the alias to the key.
func TestMaskedRoomWebhooksNameNoRecipient(t *testing.T) {
	for _, mode := range []InboxMode{InboxOff, InboxRead} {
		r := newAppRooms(t)
		s := r.s
		room := r.room("hooks")
		run(t, s, signed(r.app, Command{Operation: "room.create", Room: room, Members: []string{keyID(r.alice), keyID(r.bob), keyID(r.carol)}, Data: `{"reveal_on_close":true}`, RequestID: "c"}))
		activeWebhook(t, s, keyID(r.carol), "https://hooks.example.org/carol")
		first := run(t, s, signed(r.alice, Command{Operation: "post", Room: room, Text: "hello", RequestID: "a1"}))
		run(t, s, signed(r.carol, Command{Operation: "post", Room: room, Text: "here", RequestID: "c1"})) // room activity
		alias := run(t, s, signed(r.bob, Command{Operation: "message.get", MessageID: first.Receipt.ID})).Messages[0].Author
		ctx := withInboxMode(testContext, mode)
		if _, err := s.Execute(ctx, signed(r.bob, Command{Operation: "post", Room: room, To: alias, Text: "psst", RequestID: "b1"}), "test-origin"); err != nil {
			t.Fatal(err)
		}
		rows, err := s.db.Query("SELECT d.body FROM webhook_deliveries d JOIN webhook_subscriptions w ON w.id=d.subscription WHERE w.account=?", keyID(r.carol))
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for rows.Next() {
			var body string
			if err = rows.Scan(&body); err != nil {
				t.Fatal(err)
			}
			n++
			for _, secret := range []string{keyID(r.alice), keyID(r.bob)} {
				if strings.Contains(body, secret) {
					t.Errorf("mode %v: a delivery names %s: %s", mode, secret, body)
				}
			}
		}
		rows.Close()
		if n == 0 {
			t.Fatalf("mode %v: carol got no delivery", mode)
		}
	}
}

// Slow mode holds edits too: each member edits at most once per wait, so a
// stream of edits is no way around it.
func TestSlowModeHoldsEdits(t *testing.T) {
	r := newAppRooms(t)
	s := r.s
	room := r.room("slow-edits")
	run(t, s, signed(r.app, Command{Operation: "room.create", Room: room, Members: []string{keyID(r.alice)}, Data: `{"slow_mode_seconds":30}`, RequestID: "c"}))
	first := run(t, s, signed(r.alice, Command{Operation: "post", Room: room, Text: "one", RequestID: "a1"})).Receipt.ID
	// One edit right after the post (a typo fix) is fine.
	edit := run(t, s, signed(r.alice, Command{Operation: "post", Room: room, Text: "one!", Data: `{"schema":1,"supersedes":"` + first + `"}`, RequestID: "e1"})).Receipt.ID
	fails(t, s, signed(r.alice, Command{Operation: "post", Room: room, Text: "one!!", Data: `{"schema":1,"supersedes":"` + edit + `"}`, RequestID: "e2"}), "room_slow_mode")
	s.now = func() time.Time { return time.Unix(testTime+30, 0) }
	run(t, s, signedNow(s, r.alice, Command{Operation: "post", Room: room, Text: "one!!", Data: `{"schema":1,"supersedes":"` + edit + `"}`, RequestID: "e3"}))
	// The owner is exempt.
	o := run(t, s, signedNow(s, r.app, Command{Operation: "post", Room: room, Text: "o", RequestID: "o1"})).Receipt.ID
	o2 := run(t, s, signedNow(s, r.app, Command{Operation: "post", Room: room, Text: "o2", Data: `{"schema":1,"supersedes":"` + o + `"}`, RequestID: "o2"})).Receipt.ID
	run(t, s, signedNow(s, r.app, Command{Operation: "post", Room: room, Text: "o3", Data: `{"schema":1,"supersedes":"` + o2 + `"}`, RequestID: "o3"}))
}

// An idle stream's position moves with the board, so a re-read on each
// unrelated commit scans only what is new, and one read scans a bounded
// window however old its cursor.
func TestStreamReadPositionMovesPastOtherEvents(t *testing.T) {
	r := newAppRooms(t)
	s := r.s
	start := run(t, s, signed(r.alice, Command{Operation: "stream.read"})).NextCursor
	for i := 0; i < 5; i++ {
		run(t, s, signed(r.carol, Command{Operation: "post", Room: "lobby", Text: "unrelated " + string(rune('a'+i)), RequestID: "u" + string(rune('a'+i))}))
	}
	head := run(t, s, signed(r.alice, Command{Operation: "stream.read"})).NextCursor
	got := run(t, s, signed(r.alice, Command{Operation: "stream.read", Cursor: start}))
	at, _ := s.parseCursor(got.NextCursor)
	want, _ := s.parseCursor(head)
	if len(got.Messages) != 0 || at != want || got.Data["has_more"] != false {
		t.Fatalf("idle stream: at %d (head %d) has_more %v", at, want, got.Data["has_more"])
	}
	old := streamScanWindow
	streamScanWindow = 2
	t.Cleanup(func() { streamScanWindow = old })
	got = run(t, s, signed(r.alice, Command{Operation: "stream.read", Cursor: start}))
	at, _ = s.parseCursor(got.NextCursor)
	from, _ := s.parseCursor(start)
	if at != from+2 || got.Data["has_more"] != true {
		t.Fatalf("window: moved %d, has_more %v", at-from, got.Data["has_more"])
	}
}

// No read names a masked room's author to a member, and no read filtered
// by an agent reaches the room: either would tie an alias to a key.
func TestMaskedRoomReadProbe(t *testing.T) {
	r := newAppRooms(t)
	s := r.s
	room := r.room("probe")
	run(t, s, signed(r.app, Command{Operation: "room.create", Room: room, Members: []string{keyID(r.alice), keyID(r.bob), keyID(r.carol)}, Data: `{"reveal_on_close":true}`, RequestID: "c"}))
	root := run(t, s, signed(r.alice, Command{Operation: "post", Room: room, Text: "root @bobby", RequestID: "a1"})).Receipt.ID
	reply := run(t, s, signed(r.bob, Command{Operation: "post", Room: room, ReplyTo: root, Text: "bob speaks @alice", RequestID: "b1"})).Receipt.ID
	for _, c := range []Command{
		{Operation: "vote", MessageID: root, Data: `{"schema":1,"value":1}`, RequestID: "v1"},
		{Operation: "work.create", MessageID: reply, Data: `{"schema":1}`, RequestID: "w1"},
		{Operation: "report", MessageID: root, Reason: "spam", RequestID: "r1"},
	} {
		_, _ = s.Execute(testContext, signed(r.bob, c), "test-origin")
	}
	bobAlias := run(t, s, signed(r.alice, Command{Operation: "message.get", MessageID: reply})).Messages[0].Author
	run(t, s, signed(r.alice, Command{Operation: "post", Room: room, To: bobAlias, Text: "to bob", RequestID: "a2"}))
	secrets := []string{keyID(r.bob), pubOf(r.bob), `:"bobby"`}
	for _, c := range []Command{
		{Operation: "messages.list", Room: room},
		{Operation: "messages.list", Room: room, Query: "speaks"},
		{Operation: "messages.list", Query: "speaks"},
		{Operation: "message.get", MessageID: reply},
		{Operation: "message.get", MessageID: reply, Room: room},
		{Operation: "thread.get", MessageID: root},
		{Operation: "room.get", Room: room},
		{Operation: "room.members", Room: room},
		{Operation: "room.pages", Room: room},
		{Operation: "room.modlog", Room: room},
		{Operation: "updates.get"},
		{Operation: "updates.get", Data: `{"schema":1,"counts":true}`},
		{Operation: "home"},
		{Operation: "journal.get"},
		{Operation: "account.export"},
		{Operation: "work.get", MessageID: reply},
		{Operation: "works.list", Room: room},
		{Operation: "work.history", MessageID: reply},
		{Operation: "blob.get", MessageID: reply},
		{Operation: "agent.posts", Target: keyID(r.bob)},
		{Operation: "agent.posts", Target: "bobby"},
		{Operation: "agent.get", Target: keyID(r.bob)},
		{Operation: "works.list", Target: keyID(r.bob)},
		{Operation: "messages.list", Room: room, Kind: "note", Target: keyID(r.bob)},
		{Operation: "message.withdraw", MessageID: reply, RequestID: "x1"},
		{Operation: "post", Room: room, Text: "edit", Data: `{"schema":1,"supersedes":"` + reply + `"}`, RequestID: "x2"},
		{Operation: "room.hide", MessageID: reply, Reason: "x", RequestID: "x3"},
	} {
		res, err := s.Execute(testContext, signed(r.alice, c), "test-origin")
		raw := mustJSON(t, res)
		if err != nil {
			raw = err.Error()
		}
		for _, secret := range secrets {
			if strings.Contains(raw, secret) && c.Target == "" {
				t.Errorf("%s %+v leaks %q: %s", c.Operation, c, secret, raw)
			}
		}
		if c.Target != "" && strings.Contains(raw, reply) {
			t.Errorf("%s filtered by bob reached the masked room: %s", c.Operation, raw)
		}
	}
}
