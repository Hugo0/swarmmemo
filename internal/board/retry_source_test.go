package board

import (
	"errors"
	"testing"
)

// An arrival's rotating egress: an exact retry finds its receipt from another
// network (anonymous: within AnonCrossNetworkRetrySeconds, anonretry.go; signed:
// always, scoped to the key).
func TestRetryAcrossSourceChanges(t *testing.T) {
	for _, sign := range []bool{false, true} {
		name := "anonymous"
		if sign {
			name = "signed"
		}
		t.Run(name, func(t *testing.T) {
			s := openTest(t, Config{})
			command := Command{Operation: "post", Room: "lobby", Page: "main", Text: "one intended message", RequestID: "stable-intent"}
			if sign {
				command = signed(keyFor(41), command)
			}
			execute := func(c Command, source string) Result {
				t.Helper()
				r, err := s.Execute(testContext, c, source)
				if err != nil || !r.OK || r.Receipt == nil {
					t.Fatalf("execute %s: result=%+v error=%v", source, r, err)
				}
				return r
			}
			first := execute(command, "egress-a")
			same := execute(command, "egress-a")
			if same.Receipt.ID != first.Receipt.ID || !same.Receipt.Duplicate {
				t.Fatal("same-source retry did not recover original receipt")
			}
			moved := execute(command, "egress-b")
			if moved.Receipt.ID != first.Receipt.ID || !moved.Receipt.Duplicate {
				t.Fatal("exact retry lost deduplication after egress change")
			}
			if got := len(run(t, s, Command{Operation: "messages.list"}).Messages); got != 1 {
				t.Fatalf("got %d stored events, want 1", got)
			}
			changed := command
			changed.Text = "different intended message"
			if sign {
				changed = signed(keyFor(41), changed)
			}
			// An anonymous ID belongs to the network that used it; a signed one
			// to the key, wherever it is sent from.
			source := "egress-a"
			if sign {
				source = "egress-b"
			}
			_, err := s.Execute(testContext, changed, source)
			var conflict *Error
			if !errors.As(err, &conflict) || conflict.Code != "idempotency_conflict" {
				t.Fatalf("changed payload reused intent: %v", err)
			}
		})
	}
}
