package httpapi

import (
	"context"
	"encoding/json"
	"net/url"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// C620: a message read's default answer is what installed inbox clients
// (clients/python swarmmemo_inbox.py event_page, swarmmemo_private_transport.py
// _events) accept key by key: top-level ok, messages, next_cursor, generation
// and data, and data exactly {has_more}. The list shape is opt-in (shape=1).
func TestMessageReadsDefaultDataIsHasMoreOnly(t *testing.T) {
	store, err := board.Open(filepath.Join(t.TempDir(), "shape.sqlite"), board.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	post := func(c board.Command) string {
		t.Helper()
		res, err := store.Execute(context.Background(), c, "test")
		if err != nil {
			t.Fatal(err)
		}
		return res.Receipt.ID
	}
	inbox := strings.Repeat("a", 64)
	root := post(board.Command{Operation: "post", Room: "lobby", Text: "a post"})
	post(board.Command{Operation: "post", Room: "lobby", ReplyTo: root, Text: "a reply"})
	post(board.Command{Operation: "post", Room: "lobby", To: inbox, Text: "for the inbox"})
	s := New(store, nil, Config{PublicURL: "https://example.test", ServiceID: "swarmmemo.com"})

	read := func(path string) (keys []string, data map[string]any, next string) {
		t.Helper()
		w := makeRequestWithAccept(s, path, "application/json")
		var body map[string]json.RawMessage
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil {
			t.Fatalf("%s: %d %.300s", path, w.Code, w.Body.String())
		}
		for k := range body {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		_ = json.Unmarshal(body["data"], &data)
		_ = json.Unmarshal(body["next_cursor"], &next)
		return keys, data, next
	}
	strict := func(path string) string {
		t.Helper()
		keys, data, next := read(path)
		for _, k := range keys {
			switch k {
			case "ok", "messages", "next_cursor", "generation", "data":
			default:
				t.Fatalf("%s: top-level %s; installed clients refuse it (%v)", path, k, keys)
			}
		}
		if _, ok := data["has_more"].(bool); data != nil && (!ok || len(data) != 1) {
			t.Fatalf("%s: data %v, want {has_more} alone", path, data)
		}
		return next
	}
	// The inbox client's reads: its first page and polls, and /e/ID.
	q := url.Values{"to": {inbox}, "limit": {"100"}}
	next := strict("/api/messages?" + q.Encode())
	q.Set("cursor", next)
	strict("/api/messages?" + q.Encode())
	q.Set("cursor", "start")
	strict("/api/messages?" + q.Encode())
	strict("/api/messages?room=lobby&cursor=start")
	strict("/e/" + root)
	// Every other read keeps its own data (sort and the like) and no shape.
	for _, path := range []string{"/api/messages?limit=20", "/api/messages?sort=new&limit=1", "/api/messages?sort=hot&room=lobby", "/r/lobby"} {
		if _, data, _ := read(path); data["returned"] != nil || data["list_depth"] != nil || data["total"] != nil || data["total_exact"] != nil {
			t.Fatalf("%s carries the list shape unasked: %v", path, data)
		}
	}

	// shape=1 adds it: the reply is in this list, and the list is whole.
	_, data, _ := read("/api/messages?room=lobby&sort=new&shape=1")
	want := map[string]any{"has_more": false, "returned": float64(3), "list_depth": board.ListDepthNested, "total": float64(3), "total_exact": true}
	if !reflect.DeepEqual(data, want) {
		t.Fatalf("shape=1: %v, want %v", data, want)
	}
	if _, data, _ = read("/api/messages?shape=1"); data["list_depth"] != board.ListDepthNested && data["list_depth"] != board.ListDepthTop {
		t.Fatalf("first contact with shape=1: %v", data)
	}
	if w := makeRequestWithAccept(s, "/api/messages?shape=2", "application/json"); w.Code != 400 {
		t.Fatalf("shape=2: %d", w.Code)
	}
}

// MCP read_messages always carries the list shape: its output is ours to
// format, and no installed client reads it key by key.
func TestMCPReadMessagesCarriesListShape(t *testing.T) {
	_, s := hostedServer(t)
	mustTool(t, s, "/mcp", "", "post_message", map[string]any{"room": "lobby", "text": "a post for the shape"})
	for _, args := range []map[string]any{{}, {"room": "lobby", "sort": "new"}, {"room": "lobby", "kind": "no-such-kind"}} {
		got := mustTool(t, s, "/mcp", "", "read_messages", args)
		if dig(got, "data", "list_depth") == nil || dig(got, "data", "returned") == nil {
			t.Fatalf("read_messages %v: %v", args, got["data"])
		}
	}
}
