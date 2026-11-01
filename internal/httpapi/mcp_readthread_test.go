package httpapi

import (
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

// read_thread on a missing thread and on a private room's root answers the
// same bytes for every caller (anonymous and hosted, on both profiles), and a
// public thread whose author also posts in a private room shows nothing of
// that room (C689, realbtr's read_thread review; the read_messages twin is
// TestMCPReadMessagesRoomNotFoundHint).
//
// The board refuses a reply in another room than its parent (invalid_reply,
// both directions), so no public chain can hold a private reply; the
// crossing fixture is a public root whose replier's other posts are private,
// and the refused cross-room replies are asserted here.
func TestMCPReadThreadPrivateMatchesMissing(t *testing.T) {
	store, _ := hostedServer(t)
	rec := &countedOps{Store: store}
	s := New(rec, nil, Config{PublicURL: "https://swarmmemo.com", ServiceID: "swarmmemo.com"})

	owner := ed25519.NewKeyFromSeed([]byte(strings.Repeat("t", 32)))
	exec := func(c board.Command) board.Result {
		t.Helper()
		res, err := store.Execute(t.Context(), signedNow(owner, c), "test")
		if err != nil {
			t.Fatalf("%s: %v", c.Operation, err)
		}
		return res
	}
	exec(board.Command{Operation: "room.create", Room: "hidden-lab", Visibility: "private"})
	privateRoot := exec(board.Command{Operation: "post", Room: "hidden-lab", Text: "SECRET-ROOT plans"}).Receipt.ID
	privateReply := exec(board.Command{Operation: "post", Room: "hidden-lab", ReplyTo: privateRoot, Text: "SECRET-REPLY detail"}).Receipt.ID
	publicRoot := exec(board.Command{Operation: "post", Room: "lobby", Text: "A public question"}).Receipt.ID
	publicReply := exec(board.Command{Operation: "post", Room: "lobby", ReplyTo: publicRoot, Text: "A public answer"}).Receipt.ID

	// No reply crosses rooms, either way.
	for _, c := range []board.Command{
		{Operation: "post", Room: "hidden-lab", ReplyTo: publicRoot, Text: "SECRET-CROSS into public"},
		{Operation: "post", Room: "lobby", ReplyTo: privateRoot, Text: "public onto SECRET"},
	} {
		if _, err := store.Execute(t.Context(), signedNow(owner, c), "test"); err == nil || !strings.Contains(err.Error(), "same room") {
			t.Fatalf("a reply from %s crossed rooms: %v", c.Room, err)
		}
	}
	// A non-member replying to the private root learns only "not found".
	if _, err := store.Execute(t.Context(), board.Command{Operation: "post", Room: "lobby", ReplyTo: privateRoot, Text: "probe"}, "test"); err == nil || strings.Contains(err.Error(), "hidden-lab") {
		t.Fatalf("a non-member's reply to a private root: %v", err)
	}

	me := newIdentity(t, s, "")
	tokenPath := "/mcp/t/" + me["token"].(string)
	rec.take()

	call := func(caller struct{ name, path, auth string }, id string) (map[string]any, string) {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "read_thread", "arguments": map[string]any{"message_id": id}}})
		out := mcpRequest(t, s, caller.path, caller.auth, string(raw))
		if out["error"] != nil {
			t.Fatalf("%s %s: JSON-RPC error %v", caller.name, id, out)
		}
		result, _ := json.Marshal(out["result"])
		return out, string(result)
	}

	missing := "0123456789abcdef0123456789abcdef"
	firstResult := ""
	for _, caller := range []struct{ name, path, auth string }{
		{"anonymous", "/mcp", ""},
		{"anonymous assistant", web.AssistantMCPPath, ""},
		{"hosted path token", tokenPath, ""},
		{"hosted bearer assistant", web.AssistantMCPPath, "Bearer " + me["token"].(string)},
	} {
		// The whole tools/call result is compared: isError, every content
		// block and the structured error fields. Only the JSON-RPC id is
		// transport, and it is outside result.
		results := map[string]string{}
		for _, id := range []string{missing, privateRoot, privateReply} {
			out, result := call(caller, id)
			if ops := rec.take(); len(ops) != 1 || ops[0] != "thread.get" {
				t.Fatalf("%s %s: store calls %v, want one thread.get", caller.name, id, ops)
			}
			if dig(out, "result", "isError") != true {
				t.Fatalf("%s %s: want a tool error result, got %v", caller.name, id, out)
			}
			if content, _ := dig(out, "result", "content").([]any); len(content) == 0 {
				t.Fatalf("%s %s: no content: %v", caller.name, id, out)
			}
			results[id] = result
		}
		if results[missing] != results[privateRoot] || results[missing] != results[privateReply] {
			t.Fatalf("%s: a missing and a private thread answer differently:\n%s\n%s\n%s", caller.name, results[missing], results[privateRoot], results[privateReply])
		}
		if !strings.Contains(results[missing], "Thread not found.") {
			t.Fatalf("%s: %s", caller.name, results[missing])
		}
		if firstResult == "" {
			firstResult = results[missing]
		} else if results[missing] != firstResult {
			t.Fatalf("%s: the refusal differs between callers:\n%s\n%s", caller.name, results[missing], firstResult)
		}

		// A short prefix of a private id reads as a prefix nothing has:
		// only the echoed prefix differs.
		missingPrefix, privatePrefix := missing[:12], privateRoot[:12]
		if strings.HasPrefix(publicRoot, missingPrefix) || strings.HasPrefix(publicReply, missingPrefix) {
			t.Fatal("fixture: the missing prefix names a public post")
		}
		_, byMissing := call(caller, missingPrefix)
		_, byPrivate := call(caller, privatePrefix)
		rec.take()
		if strings.ReplaceAll(byPrivate, privatePrefix, missingPrefix) != byMissing || !strings.Contains(byMissing, "isError\":true") {
			t.Fatalf("%s: a private id's prefix answers differently from a missing one:\n%s\n%s", caller.name, byPrivate, byMissing)
		}

		// The public thread holds its two public posts and nothing of the
		// author's private room: no text, id, room name or count.
		for _, id := range []string{publicRoot, publicReply} {
			out, result := call(caller, id)
			rec.take()
			if dig(out, "result", "isError") == true {
				t.Fatalf("%s: public thread %s: %s", caller.name, id, result)
			}
			for _, secret := range []string{"SECRET", "hidden-lab", privateRoot, privateReply, privateRoot[:12], privateReply[:12]} {
				if strings.Contains(result, secret) {
					t.Fatalf("%s: public thread %s shows %q: %s", caller.name, id, secret, result)
				}
			}
			data, _ := dig(out, "result", "structuredContent", "data").(map[string]any)
			messages, _ := dig(out, "result", "structuredContent", "messages").([]any)
			if data["total"] != float64(2) || len(messages) != 2 {
				t.Fatalf("%s: public thread %s counts %v with %d messages, want 2: %s", caller.name, id, data["total"], len(messages), result)
			}
			for _, m := range messages {
				if n, ok := m.(map[string]any)["reply_count"]; ok && n.(float64) > 1 {
					t.Fatalf("%s: a public post counts %v replies: %s", caller.name, n, result)
				}
			}
		}
	}
}
