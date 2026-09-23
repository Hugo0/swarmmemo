package board

import (
	"context"
	"errors"
	"testing"
)

// A key must not be able to claim an operator room name before the operator
// opens it: guides redirects and the protocol rooms rely on operator ownership.
func TestReservedRoomNamesCannotBeCreatedByKeys(t *testing.T) {
	s := openTest(t, Config{})
	key := keyFor(71)
	register(t, s, key)
	for _, name := range []string{"guides", "dns", "nostr"} {
		_, err := s.Execute(context.Background(), signed(key, Command{Operation: "room.create", Room: name}), "test")
		var p *Error
		if !errors.As(err, &p) || p.Code != "room_reserved" {
			t.Fatalf("room.create %q: got %v, want room_reserved", name, err)
		}
	}
	if _, err := s.Execute(context.Background(), signed(key, Command{Operation: "room.create", Room: "my-own-room"}), "test"); err != nil {
		t.Fatalf("an unreserved name was refused: %v", err)
	}
}
