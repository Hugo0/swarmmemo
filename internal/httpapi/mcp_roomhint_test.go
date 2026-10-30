package httpapi

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"sync"
	"testing"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

// countedOps is the real store, counting each operation it executes.
type countedOps struct {
	*board.Store
	mu  sync.Mutex
	ops []string
}

func (o *countedOps) Execute(ctx context.Context, c board.Command, peer string) (board.Result, error) {
	o.mu.Lock()
	o.ops = append(o.ops, c.Operation)
	o.mu.Unlock()
	return o.Store.Execute(ctx, c, peer)
}

func (o *countedOps) take() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	ops := o.ops
	o.ops = nil
	return ops
}

// read_messages on a missing room and on a private one answers the same
// bytes, anonymous and with a hosted token, on both profiles: one store
// call (messages.list), nothing written, and one next step for every
// caller that names the signed HTTPS read and never the DM tools (C642,
// realbtr's checklist on PR #45).
func TestMCPReadMessagesRoomNotFoundHint(t *testing.T) {
	store, _ := hostedServer(t)
	rec := &countedOps{Store: store}
	s := New(rec, nil, Config{PublicURL: "https://swarmmemo.com", ServiceID: "swarmmemo.com"})

	key := ed25519.NewKeyFromSeed([]byte(strings.Repeat("h", 32)))
	create := board.Command{Operation: "room.create", Room: "hidden-lab", Visibility: "private"}
	create.PublicKey = base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
	create.Timestamp, create.Nonce = time.Now().Unix(), "c642"
	create.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, board.Canonical("swarmmemo.com", create)))
	if _, err := store.Execute(t.Context(), create, "test"); err != nil {
		t.Fatal(err)
	}
	me := newIdentity(t, s, "")
	tokenPath := "/mcp/t/" + me["token"].(string)
	rec.take()

	first := ""
	for _, caller := range []struct{ name, path, auth string }{
		{"anonymous", "/mcp", ""},
		{"anonymous assistant", web.AssistantMCPPath, ""},
		{"hosted path token", tokenPath, ""},
		{"hosted bearer assistant", web.AssistantMCPPath, "Bearer " + me["token"].(string)},
	} {
		answers := map[string]string{}
		for _, room := range []string{"no-such-room", "hidden-lab"} {
			_, failure := callTool(t, s, caller.path, caller.auth, "read_messages", map[string]any{"room": room})
			ops := rec.take()
			if len(ops) != 1 || ops[0] != "messages.list" {
				t.Fatalf("%s %s: store calls %v, want one messages.list", caller.name, room, ops)
			}
			answers[room] = failure
		}
		got := answers["no-such-room"]
		if got != answers["hidden-lab"] {
			t.Fatalf("%s: a missing and a private room answer differently:\n%q\n%q", caller.name, got, answers["hidden-lab"])
		}
		if !strings.Contains(got, "Room not found.") || !strings.Contains(got, "list_rooms") {
			t.Fatalf("%s: %q", caller.name, got)
		}
		if strings.Contains(got, "read_conversation") || !strings.Contains(got, "/protocol.md#arrive-post-read") {
			t.Fatalf("%s: want the signed-read link and no DM tool: %q", caller.name, got)
		}
		if first == "" {
			first = got
		} else if got != first {
			t.Fatalf("%s: the hint differs between callers:\n%q\n%q", caller.name, got, first)
		}
	}

	// Nothing was written: no read opened the missing room.
	if _, err := store.Execute(t.Context(), board.Command{Operation: "messages.list", Room: "no-such-room"}, "test"); !board.IsRoomNotFound(err) {
		t.Fatalf("a read opened no-such-room: %v", err)
	}
	// Another refusal on a room that exists keeps its own words.
	if _, err := store.Execute(t.Context(), board.Command{Operation: "post", Room: "lobby", Text: "hello"}, "test"); err != nil {
		t.Fatal(err)
	}
	if _, failure := callTool(t, s, "/mcp", "", "read_messages", map[string]any{"room": "lobby", "sort": "sideways"}); failure == "" || strings.Contains(failure, "list_rooms") {
		t.Fatalf("a non-room refusal took the room hint: %q", failure)
	}
}
