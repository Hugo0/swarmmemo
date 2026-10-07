package board

import (
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"testing"
)

func updatesConfig() Config {
	return Config{ServiceID: "swarmmemo.com", MaxTextBytes: 64 << 10, DailyBytes: 1 << 30, AnonymousDailyBytes: 1 << 30, GlobalDailyBytes: 1 << 30}
}

func ids(result Result, key string) []string {
	list, _ := result.Data[key].([]string)
	return list
}

// The return read is the whole point of coming back: one call that says what
// happened since the cursor that concerns this agent.
func TestUpdatesReturnsRepliesAddressedMessagesAndRoomActivity(t *testing.T) {
	s := openTest(t, updatesConfig())
	mine, other := keyFor(1), keyFor(2)
	run(t, s, signed(mine, Command{Operation: "agent.register", Handle: "returning"}))
	run(t, s, signed(other, Command{Operation: "agent.register", Handle: "stranger"}))
	me := keyID(mine)

	root := run(t, s, signed(mine, Command{Operation: "post", Room: "workshop", Text: "An open question."})).Receipt.ID
	// The cursor this agent saves before going away.
	saved := run(t, s, Command{Operation: "updates.get", Target: me}).NextCursor
	if saved == "" {
		t.Fatal("a first visit must hand back a cursor to save")
	}

	reply := run(t, s, signed(other, Command{Operation: "post", Room: "workshop", Text: "An answer.", ReplyTo: root})).Receipt.ID
	mail := run(t, s, signed(other, Command{Operation: "post", Room: "lobby", Text: "A message for you.", To: me})).Receipt.ID
	activity := run(t, s, signed(other, Command{Operation: "post", Room: "workshop", Text: "Unrelated workshop news."})).Receipt.ID
	elsewhere := run(t, s, signed(other, Command{Operation: "post", Room: "elsewhere", Text: "A room this agent never entered."})).Receipt.ID
	own := run(t, s, signed(mine, Command{Operation: "post", Room: "workshop", Text: "My own later note."})).Receipt.ID

	back := run(t, s, Command{Operation: "updates.get", Target: me, Cursor: saved})
	seen := map[string]bool{}
	for _, e := range back.Messages {
		seen[e.ID] = true
	}
	for _, want := range []string{reply, mail, activity} {
		if !seen[want] {
			t.Fatalf("the return read missed %s", want)
		}
	}
	if seen[elsewhere] {
		t.Fatal("a room this agent never posted in leaked into its return read")
	}
	if seen[own] {
		t.Fatal("an agent's own post is not news to its author")
	}
	if back.Data["scope"] != "agent" || back.Data["agent"] != me {
		t.Fatalf("wrong scope metadata: %v", back.Data)
	}
	if len(ids(back, "replies")) != 1 || ids(back, "replies")[0] != reply {
		t.Fatalf("replies not identified: %v", back.Data["replies"])
	}
	if len(ids(back, "addressed")) != 1 || ids(back, "addressed")[0] != mail {
		t.Fatalf("addressed messages not identified: %v", back.Data["addressed"])
	}
	if len(ids(back, "room_activity")) != 1 || ids(back, "room_activity")[0] != activity {
		t.Fatalf("room activity not identified: %v", back.Data["room_activity"])
	}

	// The cursor advances, so a second return with nothing new is empty rather
	// than a replay of what was already handled.
	if back.NextCursor == saved {
		t.Fatal("a return read must advance the cursor")
	}
	again := run(t, s, Command{Operation: "updates.get", Target: me, Cursor: back.NextCursor})
	if len(again.Messages) != 0 || again.Data["has_more"] != false || again.NextCursor == "" {
		t.Fatalf("a caught-up return read must be empty and keep a cursor: %+v", again.Data)
	}
}

// A key rotation must not lose a returning agent its replies and its mail.
func TestUpdatesFollowsAccountContinuityAcrossKeyRotation(t *testing.T) {
	s := openTest(t, updatesConfig())
	first, next, other := keyFor(3), keyFor(4), keyFor(5)
	run(t, s, signed(first, Command{Operation: "agent.register", Handle: "rotating"}))
	run(t, s, signed(other, Command{Operation: "agent.register", Handle: "correspondent"}))
	root := run(t, s, signed(first, Command{Operation: "post", Room: "workshop", Text: "Before the rotation."})).Receipt.ID
	saved := run(t, s, Command{Operation: "updates.get", Target: keyID(first)}).NextCursor
	rotation := signed(first, Command{Operation: "agent.rotate", Target: base64.RawURLEncoding.EncodeToString(next.Public().(ed25519.PublicKey))})
	rotation.Proof = base64.RawURLEncoding.EncodeToString(ed25519.Sign(next, Canonical("swarmmemo.com", rotation)))
	run(t, s, rotation)
	reply := run(t, s, signed(other, Command{Operation: "post", Room: "workshop", Text: "After the rotation.", ReplyTo: root})).Receipt.ID
	mail := run(t, s, signed(other, Command{Operation: "post", Room: "lobby", Text: "Addressed to the new key.", To: keyID(next)})).Receipt.ID
	back := run(t, s, Command{Operation: "updates.get", Target: keyID(next), Cursor: saved})
	seen := map[string]bool{}
	for _, e := range back.Messages {
		seen[e.ID] = true
	}
	if !seen[reply] || !seen[mail] {
		t.Fatal("a rotated key lost its replies or its addressed messages")
	}
}

// Without an agent there is nothing personal to answer, so the read degrades to
// public room activity and says so, rather than refusing.
func TestUpdatesWithoutAnAgentReturnsRoomActivityAndSaysSo(t *testing.T) {
	s := openTest(t, updatesConfig())
	run(t, s, Command{Operation: "post", Room: "lobby", Text: "Anyone can read this."})
	anonymous := run(t, s, Command{Operation: "updates.get"})
	if len(anonymous.Messages) != 1 {
		t.Fatalf("public room activity missing: %d", len(anonymous.Messages))
	}
	if anonymous.Data["scope"] != "room_activity" {
		t.Fatalf("wrong scope: %v", anonymous.Data["scope"])
	}
	note, _ := anonymous.Data["note"].(string)
	if !strings.Contains(note, "agent=FINGERPRINT") {
		t.Fatalf("the reduced answer must explain itself: %q", note)
	}
	if _, personal := anonymous.Data["replies"]; personal {
		t.Fatal("an anonymous caller must not receive personal categories")
	}
	if _, err := s.Execute(testContext, Command{Operation: "updates.get", Target: "not-a-fingerprint"}, "test-origin"); err == nil {
		t.Fatal("a malformed agent must be rejected rather than silently ignored")
	}
}

// The same response byte budget and explicit has_more as every other read.
func TestUpdatesIsBoundedAndReportsHasMore(t *testing.T) {
	s := openTest(t, updatesConfig())
	big := strings.Repeat("u", 40<<10)
	for i := 0; i < 6; i++ {
		run(t, s, Command{Operation: "post", Room: "lobby", Text: big})
	}
	page := run(t, s, Command{Operation: "updates.get", Limit: 50})
	if len(page.Messages) >= 6 {
		t.Fatalf("the byte budget did not bound the return read: %d", len(page.Messages))
	}
	if page.Data["has_more"] != true {
		t.Fatal("a truncated return read must report has_more")
	}
}

// The imported kind carries the service's own provenance presentation, so it
// cannot be self-assigned: an anonymous poster could otherwise earn the badge,
// have the curator disclosure hidden from its visible body, and be handed a
// clickable outbound link in the feed.
func TestImportedKindIsReservedToTheCuratorAccount(t *testing.T) {
	s := openTest(t, updatesConfig())
	disclosure := "Imported / populated — curator summary, not an original SwarmMemo post.\nSource: https://example.org/x"
	fails(t, s, Command{Operation: "post", Room: "lobby", Kind: "imported", Text: disclosure}, "reserved_kind")

	impostor := keyFor(21)
	run(t, s, signed(impostor, Command{Operation: "agent.register", Handle: "not-the-curator"}))
	fails(t, s, signed(impostor, Command{Operation: "post", Room: "lobby", Kind: "imported", Text: disclosure}), "reserved_kind")

	// A claimed handle is not a registered one: unsigned posts carry whatever
	// handle they were given, so the check must require the signature too.
	fails(t, s, Command{Operation: "post", Room: "lobby", Kind: "imported", Handle: curatorHandle, Text: disclosure}, "reserved_kind")

	curator := keyFor(22)
	run(t, s, signed(curator, Command{Operation: "agent.register", Handle: curatorHandle}))
	accepted := run(t, s, signed(curator, Command{Operation: "post", Room: "lobby", Kind: "imported", Text: disclosure})).Receipt.ID
	page := run(t, s, Command{Operation: "messages.list", Room: "lobby"})
	for _, e := range page.Messages {
		if e.ID == accepted && !e.Curated {
			t.Fatal("a genuine curator import lost its provenance flag")
		}
	}
	// An ordinary post by the curator is not a curated import.
	plain := run(t, s, signed(curator, Command{Operation: "post", Room: "lobby", Text: "An ordinary note."})).Receipt.ID
	for _, e := range run(t, s, Command{Operation: "messages.list", Room: "lobby"}).Messages {
		if e.ID == plain && e.Curated {
			t.Fatal("an ordinary note was presented as a curator import")
		}
	}
}

// Counts only: the same ids, reasons and cursor, and no message text, so a
// notification count never downloads anyone's words.
func TestUpdatesCountsOnlyReturnsIdsWithoutMessages(t *testing.T) {
	s := openTest(t, updatesConfig())
	mine, other := keyFor(1), keyFor(2)
	run(t, s, signed(mine, Command{Operation: "agent.register", Handle: "counter"}))
	run(t, s, signed(other, Command{Operation: "agent.register", Handle: "writer"}))
	me := keyID(mine)
	root := run(t, s, signed(mine, Command{Operation: "post", Room: "workshop", Text: "A question."})).Receipt.ID
	saved := run(t, s, signed(mine, Command{Operation: "updates.get", Target: me, Limit: 1, Data: `{"schema":1,"counts":true}`})).NextCursor
	reply := run(t, s, signed(other, Command{Operation: "post", Room: "workshop", Text: "A secret answer.", ReplyTo: root})).Receipt.ID
	mail := run(t, s, signed(other, Command{Operation: "post", Room: "lobby", Text: "A secret note.", To: me})).Receipt.ID

	full := run(t, s, signed(mine, Command{Operation: "updates.get", Target: me, Cursor: saved}))
	counts := run(t, s, signed(mine, Command{Operation: "updates.get", Target: me, Cursor: saved, Data: `{"schema":1,"counts":true}`}))
	if len(counts.Messages) != 0 || counts.Data["counts_only"] != true {
		t.Fatalf("counts only returned %d messages: %v", len(counts.Messages), counts.Data)
	}
	if len(ids(counts, "replies")) != 1 || ids(counts, "replies")[0] != reply || len(ids(counts, "addressed")) != 1 || ids(counts, "addressed")[0] != mail {
		t.Fatalf("counts only lost the reasons: %v", counts.Data)
	}
	if len(full.Messages) != 2 || counts.NextCursor == "" || counts.NextCursor == saved {
		t.Fatal("counts only must advance the cursor as the full read does")
	}
	if after := run(t, s, signed(mine, Command{Operation: "updates.get", Target: me, Cursor: counts.NextCursor, Data: `{"schema":1,"counts":true}`})); len(ids(after, "replies"))+len(ids(after, "addressed")) != 0 {
		t.Fatalf("the counts cursor replays what it already counted: %v", after.Data)
	}
	if _, ok := counts.Data["unread"]; !ok {
		t.Fatal("read for yourself, counts only keeps the inbox counts")
	}
	for _, plain := range []string{`{"schema":1}`, `{"schema":1,"counts":false}`} {
		if res := run(t, s, signed(mine, Command{Operation: "updates.get", Target: me, Cursor: saved, Data: plain})); len(res.Messages) != 2 || res.Data["counts_only"] != nil {
			t.Errorf("data %s is not the ordinary read: %d messages, %v", plain, len(res.Messages), res.Data)
		}
	}
	for _, bad := range []string{`{"schema":1,"counts":true,"text":true}`, `not json`, `{"schema":2}`, `{}`, `{"counts":true}`, `{"schema":0,"counts":true}`, `null`} {
		if _, err := s.Execute(testContext, signed(mine, Command{Operation: "updates.get", Target: me, Data: bad}), "test-origin"); err == nil || !strings.Contains(err.Error(), "counts") {
			t.Errorf("data %s was not refused: %v", bad, err)
		}
	}
}
