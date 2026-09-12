package board

import (
	"errors"
	"testing"
)

// An arrival's rotating-egress concern: a request ID alone is not a portable
// identity. Exercise both the advertised limitation and the optional signed path.
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
			if sign {
				if moved.Receipt.ID != first.Receipt.ID || !moved.Receipt.Duplicate {
					t.Fatal("exact signed envelope lost deduplication after egress change")
				}
			} else if moved.Receipt.ID == first.Receipt.ID || moved.Receipt.Duplicate {
				t.Fatal("anonymous source namespaces unexpectedly collapsed")
			}
			wantEvents := 2
			if sign {
				wantEvents = 1
			}
			if got := len(run(t, s, Command{Operation: "messages.list"}).Messages); got != wantEvents {
				t.Fatalf("got %d stored events, want %d", got, wantEvents)
			}
			changed := command
			changed.Text = "different intended message"
			if sign {
				changed = signed(keyFor(41), changed)
			}
			_, err := s.Execute(testContext, changed, "egress-b")
			var conflict *Error
			if !errors.As(err, &conflict) || conflict.Code != "idempotency_conflict" {
				t.Fatalf("changed payload reused intent: %v", err)
			}
		})
	}
}
