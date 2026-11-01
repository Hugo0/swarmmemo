package httpapi

import (
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// C675: GET first_post=mark|only|unanswered on message reads and MCP
// read_messages first_post. A read that does not ask carries no
// first_post key (installed inbox clients refuse unknown message keys).
func TestFirstPostOverHTTPAndMCP(t *testing.T) {
	store, s := hostedServer(t)
	ctx := t.Context()
	newcomer := ed25519.NewKeyFromSeed(append(make([]byte, 31), 7))
	res, err := store.Execute(ctx, signedNow(newcomer, board.Command{Operation: "post", Room: "lobby", Text: "Hello, first time here: where do I start?"}), "198.51.100.7")
	if err != nil {
		t.Fatal(err)
	}
	first := res.Receipt.ID

	read := func(path string) (int, []map[string]any) {
		t.Helper()
		w := makeRequestWithAccept(s, path, "application/json")
		var body struct {
			Messages []map[string]any `json:"messages"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		return w.Code, body.Messages
	}
	if code, ms := read("/api/messages?room=lobby&sort=new"); code != 200 || len(ms) == 0 || ms[0]["first_post"] != nil {
		t.Fatalf("unasked read: %d %v", code, ms)
	}
	for _, v := range []string{"mark", "only", "unanswered"} {
		code, ms := read("/api/messages?room=lobby&first_post=" + v)
		if code != 200 || len(ms) != 1 || ms[0]["id"] != first || ms[0]["first_post"] != true {
			t.Fatalf("first_post=%s: %d %v", v, code, ms)
		}
	}
	if code, _ := read("/api/messages?first_post=yes"); code != 400 {
		t.Fatalf("first_post=yes: %d", code)
	}
	if code, _ := read("/api/messages?first_post=only&sort=hot"); code != 400 {
		t.Fatalf("first_post=only&sort=hot: %d", code)
	}

	got := mustTool(t, s, "/mcp", "", "read_messages", map[string]any{"first_post": "unanswered", "scope": "all"})
	raw, _ := json.Marshal(got)
	if !strings.Contains(string(raw), first) || !strings.Contains(string(raw), `"first_post":true`) {
		t.Fatalf("read_messages first_post: %s", raw)
	}
}
