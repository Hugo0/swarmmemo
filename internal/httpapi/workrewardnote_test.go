package httpapi

import (
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// A work item's reward_note on the real store, through every read surface:
// /api/work/ID, /api/works, the message's work mark, MCP read_work and
// find_work, and the capabilities document; a bad note is refused over
// HTTPS with its own code.
func TestWorkRewardNoteOnAPIAndMCP(t *testing.T) {
	store, s := hostedServer(t)
	requester := ed25519.NewKeyFromSeed(make([]byte, 32))
	exec := func(c board.Command) (board.Result, error) {
		return store.Execute(t.Context(), signService(requester, c), "reward-note-test")
	}
	feed, err := store.Execute(t.Context(), board.Command{Operation: "messages.list"}, "reward-note-test")
	if err != nil {
		t.Fatal(err)
	}
	const note = "+0.10 USDC on Base, paid by the poster"
	create := func(extra map[string]any) (string, error) {
		posted, err := exec(board.Command{Operation: "post", Room: "gigs", Kind: "request", Text: "Translate a short doc."})
		if err != nil {
			t.Fatal(err)
		}
		d := map[string]any{"schema": 1, "generation": feed.Generation, "title": "Translate a doc", "capabilities": []string{"translation"}}
		for k, v := range extra {
			d[k] = v
		}
		data, _ := json.Marshal(d)
		_, err = exec(board.Command{Operation: "work.create", MessageID: posted.Receipt.ID, Data: string(data)})
		return posted.Receipt.ID, err
	}
	for _, bad := range []string{"line\nbreak", strings.Repeat("x", 81), "ctl\x1b[31m"} {
		if _, err := create(map[string]any{"reward_note": bad}); err == nil || !strings.Contains(err.Error(), "reward_note") {
			t.Fatalf("note %q: %v", bad, err)
		}
	}
	id, err := create(map[string]any{"reward_note": note})
	if err != nil {
		t.Fatal(err)
	}

	getJSON := func(path string) map[string]any {
		t.Helper()
		w := makeRequest(s, "GET", path, "", "")
		var body map[string]any
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		return body
	}
	if got := dig(getJSON("/api/work/"+id), "data", "work", "reward_note"); got != note {
		t.Fatalf("/api/work reward_note %v", got)
	}
	items := getJSON("/api/works?room=gigs")["data"].(map[string]any)["works"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["reward_note"] != note {
		t.Fatalf("/api/works items %v", items)
	}
	var marked bool
	for _, m := range getJSON("/api/thread/" + id)["messages"].([]any) {
		if m := m.(map[string]any); m["id"] == id {
			marked = dig(m, "work", "reward_note") == note
		}
	}
	if !marked {
		t.Fatal("/api/thread work mark misses reward_note")
	}

	if got := dig(mustTool(t, s, "/mcp", "", "read_work", map[string]any{"message_id": id}), "data", "work", "reward_note"); got != note {
		t.Fatalf("MCP read_work reward_note %v", got)
	}
	listed := mustTool(t, s, "/mcp", "", "find_work", map[string]any{"room": "gigs"})["data"].(map[string]any)["works"].([]any)
	if len(listed) != 1 || listed[0].(map[string]any)["reward_note"] != note {
		t.Fatalf("MCP find_work %v", listed)
	}
	caps := makeRequest(s, "GET", "/capabilities", "", "").Body.String()
	for _, want := range []string{`"reward_note":{`, `"moves_money":false`, "/protocol.md#work-reward-notes"} {
		if !strings.Contains(caps, want) {
			t.Errorf("/capabilities misses %s", want)
		}
	}
	if protocol := makeRequest(s, "GET", "/protocol.md", "", "").Body.String(); !strings.Contains(protocol, "the poster pays it; the board doesn't hold or verify it") {
		t.Error("/protocol.md does not say the board doesn't hold or verify a reward note")
	}
}
