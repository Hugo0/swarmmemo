package httpapi

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

// C549 (kosoyy, round 7): a post to a room that does not exist still opens
// it, but on every write path the receipt says so (receipt.new_room and a
// line), naming the closest existing public room when one is near, so a
// typo is visible. Posts to an existing room say nothing.
func TestEveryWritePathSaysANewRoomWasOpened(t *testing.T) {
	const base = "https://swarmmemo.com"
	store, err := board.Open(filepath.Join(t.TempDir(), "board.sqlite"), board.Config{ServiceID: "swarmmemo.com"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if _, err = store.SetAllowanceParams(t.Context(), board.PostingParamsNamespace, []byte(`{"anonymous_top_level_per_hour":1000,"anonymous_top_level_per_room_day":1000}`), "test", 0); err != nil {
		t.Fatal(err)
	}
	s := New(store, web.Handler(store), Config{PublicURL: base, ServiceID: "swarmmemo.com", Version: "test"})
	send := func(method, path, body string, header map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, base+path, strings.NewReader(body))
		r.RemoteAddr = "198.51.100.9:4000"
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		for k, v := range header {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return w
	}
	receipt := func(w *httptest.ResponseRecorder) *board.Receipt {
		t.Helper()
		var res board.Result
		if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || res.Receipt == nil {
			t.Fatalf("not a receipt: %s %v", w.Body.String(), err)
		}
		return res.Receipt
	}
	opened := func(wire string, n *board.Receipt, room, near string) {
		t.Helper()
		want := "New room: no room was named #" + room + ", so this post opened it."
		if near != "" {
			want += " Did you mean #" + near + "? Post there to reach the agents who read it."
		}
		if !n.NewRoom || n.NearestRoom != near || n.NewRoomNote != want {
			t.Fatalf("%s: new_room %t nearest_room %q note %q, want %q", wire, n.NewRoom, n.NearestRoom, n.NewRoomNote, want)
		}
	}
	quiet := func(wire string, n *board.Receipt) {
		t.Helper()
		if n.NewRoom || n.NearestRoom != "" || n.NewRoomNote != "" {
			t.Fatalf("%s: a post to an existing room says it opened one: %+v", wire, n)
		}
	}

	if _, err = store.Execute(context.Background(), board.Command{Operation: "post", Room: "lobby", Text: "the lobby has a post"}, "fixture"); err != nil {
		t.Fatal(err)
	}
	// The anonymous GET write URL: a typo of lobby, then the same room again.
	opened("GET /w", receipt(send("GET", "/w/lobyy/main?text=hi&format=json", "", nil)), "lobyy", "lobby")
	quiet("GET /w again", receipt(send("GET", "/w/lobyy/main?text=hi+again&format=json", "", nil)))
	quiet("GET /w lobby", receipt(send("GET", "/w/lobby/main?text=hello&format=json", "", nil)))
	// base64 (c64): a name near nothing.
	c64 := base64.RawURLEncoding.EncodeToString([]byte(`{"operation":"post","room":"zebra-talk","text":"by c64"}`))
	opened("c64", receipt(send("GET", "/c64/"+c64, "", nil)), "zebra-talk", "")
	// The plain-text receipt prints the line; a prefix of a room is near.
	text := send("GET", "/w/zebra/main?text=as+text", "", nil).Body.String()
	if !strings.Contains(text, "\nNew room: no room was named #zebra, so this post opened it. Did you mean #zebra-talk?") {
		t.Fatalf("text receipt: %q", text)
	}
	// MCP post_message.
	w := send("POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"post_message","arguments":{"room":"lobb","text":"by mcp"}}}`, map[string]string{"Accept": "application/json, text/event-stream"})
	var rpc struct {
		Result struct {
			StructuredContent board.Result `json:"structuredContent"`
		} `json:"result"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &rpc); err != nil || rpc.Result.StructuredContent.Receipt == nil {
		t.Fatalf("MCP: %s %v", w.Body.String(), err)
	}
	opened("MCP post_message", rpc.Result.StructuredContent.Receipt, "lobb", "lobby")

	// A signed post.
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	nonce := 0
	sign := func(c board.Command) board.Command {
		nonce++
		c.PublicKey = base64.RawURLEncoding.EncodeToString(public)
		c.Timestamp = time.Now().Unix()
		c.Nonce = "c549-" + string(rune('a'+nonce))
		c.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, board.Canonical("swarmmemo.com", c)))
		return c
	}
	if _, err = store.Execute(context.Background(), sign(board.Command{Operation: "agent.register"}), "fixture"); err != nil {
		t.Fatal(err)
	}
	opened("signed POST /v1/command", receipt(send("POST", "/v1/command", mustJSON(t, sign(board.Command{Operation: "post", Room: "lobbby", Text: "signed"})), nil)), "lobbby", "lobby")
	quiet("signed post to lobby", receipt(send("POST", "/v1/command", mustJSON(t, sign(board.Command{Operation: "post", Room: "lobby", Text: "signed in lobby"})), nil)))
}
