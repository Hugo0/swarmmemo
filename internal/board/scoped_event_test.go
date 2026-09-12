package board

import (
	"errors"
	"strings"
	"testing"
)

func TestScopedEventGetPrivateRoomIsolationAndLegacyReads(t *testing.T) {
	s := openTest(t, Config{})
	owner, member, outsider := keyFor(71), keyFor(72), keyFor(73)
	for _, key := range []byte{71, 72, 73} {
		register(t, s, keyFor(key))
	}
	for _, room := range []string{"private-a", "private-b"} {
		run(t, s, signed(owner, Command{Operation: "room.create", Room: room, Visibility: "private", Members: []string{keyID(member)}}))
	}
	a := run(t, s, signed(owner, Command{Operation: "post", Room: "private-a", Text: "Private A sentinel"})).Receipt.ID
	b := run(t, s, signed(owner, Command{Operation: "post", Room: "private-b", Text: "Private B sentinel"})).Receipt.ID
	public := run(t, s, Command{Operation: "post", Room: "public", Text: "Public sentinel"}).Receipt.ID
	generation := run(t, s, Command{Operation: "messages.list"}).Generation
	assertEvent := func(c Command, id, visibility string) Result {
		t.Helper()
		r := run(t, s, c)
		if len(r.Messages) != 1 || r.Messages[0].ID != id || r.Messages[0].Visibility != visibility || r.Generation != generation || r.NextCursor == "" {
			t.Fatalf("unexpected scoped event result: %+v", r)
		}
		return r
	}
	allowed := assertEvent(signed(member, Command{Operation: "message.get", Room: "private-a", MessageID: a}), a, "private")
	if allowed.Messages[0].ArchiveEligible || allowed.Messages[0].Text != "Private A sentinel" || allowed.Messages[0].Signature == "" {
		t.Fatal("private projection or original provenance lost")
	}
	assertEvent(signed(member, Command{Operation: "message.get", Room: "private-b", MessageID: b}), b, "private")
	tampered := signed(member, Command{Operation: "message.get", Room: "private-a", MessageID: a})
	tampered.Room = ""
	fails(t, s, tampered, "invalid_signature")
	for _, id := range []string{b, public, strings.Repeat("f", 32)} {
		fails(t, s, signed(member, Command{Operation: "message.get", Room: "private-a", MessageID: id}), "not_found")
	}
	// Selecting an inaccessible private room must not turn into an unscoped or
	// public lookup, even when the requested event itself is publicly readable.
	for _, id := range []string{a, b, public, strings.Repeat("f", 32)} {
		for _, c := range []Command{{Operation: "message.get", Room: "private-a", MessageID: id}, signed(outsider, Command{Operation: "message.get", Room: "private-a", MessageID: id})} {
			result, err := s.Execute(testContext, c, "test-origin")
			var failure *Error
			if !errors.As(err, &failure) || failure.Status != 404 || failure.Code != "not_found" || failure.Message != "Room not found." || len(result.Messages) != 0 {
				t.Fatalf("private existence leak: result=%+v err=%v", result, err)
			}
		}
	}
	// Legacy omission remains supported; it is deliberately not a room boundary.
	assertEvent(signed(member, Command{Operation: "message.get", MessageID: a}), a, "private")
	assertEvent(signed(member, Command{Operation: "message.get", MessageID: b}), b, "private")
	assertEvent(Command{Operation: "message.get", MessageID: public}, public, "public")
	assertEvent(Command{Operation: "message.get", Room: "public", MessageID: public}, public, "public")
	fails(t, s, signed(member, Command{Operation: "message.get", Room: "public", MessageID: a}), "not_found")
	fails(t, s, signed(member, Command{Operation: "message.get", Room: "missing", MessageID: public}), "not_found")

	if err := s.Moderate(testContext, a, "Removed private fixture", true); err != nil {
		t.Fatal(err)
	}
	hidden := assertEvent(signed(member, Command{Operation: "message.get", Room: "private-a", MessageID: a}), a, "private").Messages[0]
	if !hidden.Hidden || hidden.Type != "tombstone" || hidden.Text != "" || hidden.SignedPayload != "" || hidden.Signature != "" {
		t.Fatal("scoped read resurrected hidden payload")
	}
	run(t, s, signed(owner, Command{Operation: "room.member.remove", Room: "private-a", Target: keyID(member)}))
	fails(t, s, signed(member, Command{Operation: "message.get", Room: "private-a", MessageID: a}), "not_found")
	assertEvent(signed(member, Command{Operation: "message.get", Room: "private-b", MessageID: b}), b, "private")
	oldGeneration := generation
	if err := s.RotateGeneration(testContext); err != nil {
		t.Fatal(err)
	}
	generation = run(t, s, Command{Operation: "messages.list"}).Generation
	if generation == oldGeneration || len(generation) != 32 {
		t.Fatal("recovery generation did not change")
	}
	assertEvent(signed(member, Command{Operation: "message.get", Room: "private-b", MessageID: b}), b, "private")
}

func TestScopedEventGetCanonicalV1Unchanged(t *testing.T) {
	for _, fixture := range []struct {
		command Command
		want    string
	}{
		{Command{Operation: "message.get", MessageID: "abc"}, `{"version":1,"service":"swarmmemo.com","command":{"operation":"message.get","message_id":"abc"}}`},
		{Command{Operation: "message.get", Room: "private-a", MessageID: "abc"}, `{"version":1,"service":"swarmmemo.com","command":{"operation":"message.get","room":"private-a","message_id":"abc"}}`},
	} {
		if got := string(Canonical("swarmmemo.com", fixture.command)); got != fixture.want {
			t.Fatalf("canonical bytes changed: %s", got)
		}
		if err := validateCommandFields(fixture.command); err != nil {
			t.Fatalf("valid event read rejected: %v", err)
		}
	}
	if err := validateCommandFields(Command{Operation: "message.get", MessageID: "abc", Room: "private-a", Query: "not permitted"}); err == nil {
		t.Fatal("room extension widened unrelated message.get fields")
	}
}
