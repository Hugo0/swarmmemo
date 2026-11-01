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

// Masking is decided per room, for the reader's standing in that room,
// never by "is the reader in some app room": a key in no app room at all
// (a member the app removed, a key with no standing there) reads every Data
// value naming a masked room masked, as a member does; the app (the owner)
// reads it in full, and a value naming any other room is left as it is
// (C671). A private read grant never reaches an app room: private_read
// takes a slug room only.
func TestMaskedRoomDataMaskedForReaderInNoAppRoom(t *testing.T) {
	r := newAppRooms(t)
	s := r.s
	room := r.room("granted")
	run(t, s, signed(r.app, Command{Operation: "room.create", Room: room, Members: []string{keyID(r.alice), keyID(r.bob), keyID(r.carol)}, Data: `{"reveal_on_close":true}`, RequestID: "c"}))
	post := run(t, s, signed(r.bob, Command{Operation: "post", Room: room, Text: "a villager", RequestID: "b1"}))
	reader := keyFor(194)
	fails(t, s, privateReadEnrollCommand(s, r.app, reader, room), "invalid_private_read_data")
	run(t, s, signedNow(s, r.app, Command{Operation: "room.member.remove", Room: room, Target: keyID(r.alice), RequestID: "rm"}))
	for _, k := range []string{keyID(reader), keyID(r.alice)} {
		var inAppRoom bool
		if err := s.db.QueryRow("SELECT EXISTS(SELECT 1 FROM members WHERE account=? AND room LIKE 'app/%')", k).Scan(&inAppRoom); err != nil || inAppRoom {
			t.Fatalf("%s is in an app room (%v, %v)", k, inAppRoom, err)
		}
	}
	full := run(t, s, signed(r.app, Command{Operation: "message.get", MessageID: post.Receipt.ID})).Messages[0]
	// Data values naming the room: a journal item and a message list.
	read := func(a actor) string {
		t.Helper()
		res := &Result{Data: map[string]any{
			"unanswered": []map[string]any{{"id": full.ID, "room": room, "author": keyID(r.bob), "author_handle": "bobby"}},
			"messages":   []Message{full},
			"plain":      map[string]any{"room": "lobby", "author": keyID(r.bob)},
		}}
		tx, err := s.db.BeginTx(testContext, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if err = s.maskResult(testContext, tx, Command{Operation: "journal.get"}, a, res, s.now().Unix()); err != nil {
			t.Fatal(err)
		}
		return mustJSON(t, res.Data)
	}
	plain := `"plain":{"author":"` + keyID(r.bob) + `","room":"lobby"}`
	for name, a := range map[string]actor{
		"no standing":    {signed: true, id: keyID(reader), account: keyID(reader)},
		"removed member": {signed: true, id: keyID(r.alice), account: keyID(r.alice)},
		"member":         {signed: true, id: keyID(r.carol), account: keyID(r.carol)},
	} {
		raw := read(a)
		for _, secret := range []string{pubOf(r.bob), `"bobby"`, `"author":"` + keyID(r.bob) + `","author_handle"`, `"author":"` + keyID(r.bob) + `","room":"` + room} {
			if strings.Contains(raw, secret) {
				t.Errorf("%s reads %q unmasked: %s", name, secret, raw)
			}
		}
		if !strings.Contains(raw, plain) {
			t.Errorf("%s: another room's value changed: %s", name, raw)
		}
	}
	if raw := read(actor{signed: true, id: keyID(r.app), account: keyID(r.app)}); !strings.Contains(raw, `"bobby"`) || !strings.Contains(raw, pubOf(r.bob)) || strings.Contains(raw, MaskAliasPrefix) || !strings.Contains(raw, plain) {
		t.Errorf("the app reads masked: %s", raw)
	}
}
