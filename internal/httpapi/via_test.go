package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

const testBridgeToken = "bridge-secret-bridge-secret-bridge-secret"

func viaFixture(t *testing.T) (*board.Store, *Server) {
	t.Helper()
	store, err := board.Open(filepath.Join(t.TempDir(), "board.db"), board.Config{ServiceID: "swarmmemo.com"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, New(store, nil, Config{BridgeTokens: map[string]string{"email": testBridgeToken}})
}

// viaRequest sends one write and returns the stored message's via, or the
// error code when it was refused.
func viaRequest(t *testing.T, store *board.Store, s *Server, method, target, body, ct string, headers map[string]string) (via, code string) {
	t.Helper()
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.RemoteAddr = "198.51.100.8:12345"
	if ct != "" {
		r.Header.Set("Content-Type", ct)
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	var out struct {
		Receipt *board.Receipt `json:"receipt"`
		Error   struct {
			Code string `json:"code"`
		} `json:"error"`
		Result struct {
			StructuredContent struct {
				Receipt *board.Receipt `json:"receipt"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	receipt := out.Receipt
	if receipt == nil {
		receipt = out.Result.StructuredContent.Receipt
	}
	if receipt == nil {
		if out.Error.Code == "" {
			return "", "status " + w.Result().Status + ": " + w.Body.String()
		}
		return "", out.Error.Code
	}
	res, err := store.Execute(t.Context(), board.Command{Operation: "message.get", MessageID: receipt.ID}, "x")
	if err != nil || len(res.Messages) != 1 {
		t.Fatalf("read back %s: %v", receipt.ID, err)
	}
	return res.Messages[0].Via, ""
}

func c64(c string) string { return "/c64/" + base64.RawURLEncoding.EncodeToString([]byte(c)) }

// Every HTTP write route records the channel it served, set by the server:
// the method, the header, the command path, the site's own pages, MCP, or a
// verified bridge. Nothing in the command changes it.
func TestEveryHTTPRouteRecordsItsVia(t *testing.T) {
	store, s := viaFixture(t)
	same := map[string]string{"Sec-Fetch-Site": "same-origin"}
	cross := map[string]string{"Sec-Fetch-Site": "cross-site"}
	bridge := map[string]string{"X-SwarmMemo-Bridge": "email", "X-SwarmMemo-Bridge-Token": testBridgeToken}
	command := `{"operation":"post","room":"lobby","text":"hello"}`
	mcpCall := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"post_message","arguments":{"room":"lobby","text":"hello over mcp"}}}`
	for _, tc := range []struct {
		name, method, target, body, ct string
		headers                        map[string]string
		want                           string
	}{
		{"get query", "GET", "/w/lobby/main?text=hi&format=json", "", "", nil, "get"},
		{"get base64", "GET", "/w64/lobby/main/aGk?format=json", "", "", nil, "get"},
		{"get from a site page is still get", "GET", "/w/lobby/main?text=hi&format=json", "", "", same, "get"},
		{"post text", "POST", "/w/lobby/main?format=json", "hi", "text/plain", nil, "post"},
		{"post form", "POST", "/w/lobby/main?format=json", "text=hi", "application/x-www-form-urlencoded", nil, "post"},
		{"post form from the site", "POST", "/w/lobby/main?format=json", "text=hi", "application/x-www-form-urlencoded", same, "ui"},
		{"post form cross-site", "POST", "/w/lobby/main?format=json", "text=hi", "application/x-www-form-urlencoded", cross, "post"},
		{"post json", "POST", "/w/lobby/main?format=json", `{"text":"hi"}`, "application/json", nil, "post"},
		{"post json from the site is not the composer", "POST", "/w/lobby/main?format=json", `{"text":"hi"}`, "application/json", same, "post"},
		{"put path", "PUT", "/w/lobby/main?format=json", "hi", "text/plain", nil, "put"},
		{"put event", "PUT", "/v1/events/via-put?format=json", `{"room":"lobby","page":"main","text":"hi"}`, "application/json", nil, "put"},
		{"mkcol", "MKCOL", "/w64/lobby/main/aGk?format=json", "", "", nil, "mkcol"},
		{"x-text post", "POST", "/w/lobby/main?format=json", "", "", map[string]string{"X-Text": "hi"}, "x-text"},
		{"x-text get", "GET", "/w/lobby/main?format=json", "", "", map[string]string{"X-Text": "hi"}, "x-text"},
		{"c64 get", "GET", c64(command), "", "", nil, "c64"},
		{"c64 post", "POST", c64(command), "", "", nil, "c64"},
		{"c64 from the site is still c64", "GET", c64(command), "", "", same, "c64"},
		{"command", "POST", "/v1/command", command, "application/json", nil, "command"},
		{"command cross-site", "POST", "/v1/command", command, "application/json", cross, "command"},
		{"command from the site", "POST", "/v1/command", command, "application/json", same, "ui"},
		{"mcp", "POST", "https://swarmmemo.com/mcp", mcpCall, "application/json", map[string]string{"Accept": "application/json, text/event-stream"}, "mcp"},
		{"email bridge on c64", "POST", "https://swarmmemo.com" + c64(command), "", "", bridge, "email"},
		{"email bridge on command", "POST", "https://swarmmemo.com/v1/command", command, "application/json", bridge, "email"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			via, code := viaRequest(t, store, s, tc.method, tc.target, tc.body, tc.ct, tc.headers)
			if code != "" || via != tc.want {
				t.Fatalf("via %q (refused: %s), want %q", via, code, tc.want)
			}
		})
	}
}

// A client cannot choose via: there is no field for it on any route, and a
// bridge claim needs the configured secret, over HTTPS, for a bridge channel.
func TestViaCannotBeSpoofed(t *testing.T) {
	store, s := viaFixture(t)
	refused := []struct {
		name, method, target, body, ct string
		headers                        map[string]string
		code                           string
	}{
		{"query field", "GET", "/w/lobby/main?text=hi&via=dns&format=json", "", "", nil, "invalid_request"},
		{"json field", "POST", "/v1/command", `{"operation":"post","room":"lobby","text":"hi","via":"dns"}`, "application/json", nil, "invalid_request"},
		{"json body field", "POST", "/w/lobby/main?format=json", `{"text":"hi","via":"dns"}`, "application/json", nil, "invalid_request"},
		{"c64 field", "GET", c64(`{"operation":"post","room":"lobby","text":"hi","via":"dns"}`), "", "", nil, "invalid_request"},
		// Nor can a client claim to have been bridged: forwarded has no field on any route.
		{"forwarded json field", "POST", "/v1/command", `{"operation":"post","room":"lobby","text":"hi","forwarded":{"mode":"reissued","origin_service":"nostr","origin_id":"a","origin_author":"b","origin_ref":"c"}}`, "application/json", nil, "invalid_request"},
		{"forwarded body field", "POST", "/w/lobby/main?format=json", `{"text":"hi","forwarded":{"mode":"reissued"}}`, "application/json", nil, "invalid_request"},
		{"forwarded c64 field", "GET", c64(`{"operation":"post","room":"lobby","text":"hi","forwarded":{"mode":"reissued"}}`), "", "", nil, "invalid_request"},
		{"forwarded query field", "GET", "/w/lobby/main?text=hi&forwarded=nostr&format=json", "", "", nil, "invalid_request"},
		{"bridge claim repeated", "POST", "https://swarmmemo.com/v1/command", `{"operation":"post","room":"lobby","text":"hi"}`, "application/json", map[string]string{"X-SwarmMemo-Bridge": "email, email", "X-SwarmMemo-Bridge-Token": testBridgeToken}, "bridge_unverified"},
		{"bridge without token", "POST", "https://swarmmemo.com/v1/command", `{"operation":"post","room":"lobby","text":"hi"}`, "application/json", map[string]string{"X-SwarmMemo-Bridge": "email"}, "bridge_unverified"},
		{"bridge wrong token", "POST", "https://swarmmemo.com/v1/command", `{"operation":"post","room":"lobby","text":"hi"}`, "application/json", map[string]string{"X-SwarmMemo-Bridge": "email", "X-SwarmMemo-Bridge-Token": testBridgeToken + "x"}, "bridge_unverified"},
		{"bridge unconfigured channel", "POST", "https://swarmmemo.com/v1/command", `{"operation":"post","room":"lobby","text":"hi"}`, "application/json", map[string]string{"X-SwarmMemo-Bridge": "nostr", "X-SwarmMemo-Bridge-Token": testBridgeToken}, "bridge_unverified"},
		{"bridge claims a wire", "POST", "https://swarmmemo.com/v1/command", `{"operation":"post","room":"lobby","text":"hi"}`, "application/json", map[string]string{"X-SwarmMemo-Bridge": "dns", "X-SwarmMemo-Bridge-Token": testBridgeToken}, "bridge_unverified"},
		{"bridge over plain http", "POST", "/v1/command", `{"operation":"post","room":"lobby","text":"hi"}`, "application/json", map[string]string{"X-SwarmMemo-Bridge": "email", "X-SwarmMemo-Bridge-Token": testBridgeToken}, "bridge_unverified"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			if via, code := viaRequest(t, store, s, tc.method, tc.target, tc.body, tc.ct, tc.headers); code != tc.code {
				t.Fatalf("got via %q, code %q; want %s", via, code, tc.code)
			}
		})
	}
	// MCP arguments cannot carry provenance either: whatever the tool does with an
	// extra argument, nothing on the board comes out bridged.
	if _, err := store.Execute(t.Context(), board.Command{Operation: "post", Room: "lobby", Text: "opened"}, "x"); err != nil {
		t.Fatal(err)
	}
	viaRequest(t, store, s, "POST", "https://swarmmemo.com/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"post_message","arguments":{"room":"lobby","text":"hi","forwarded":{"mode":"reissued","origin_service":"nostr","origin_id":"a","origin_author":"b","origin_ref":"c"},"via":"nostr"}}}`, "application/json", map[string]string{"Accept": "application/json, text/event-stream"})
	listed, err := store.Execute(t.Context(), board.Command{Operation: "messages.list", Room: "lobby", Limit: 100}, "x")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range listed.Messages {
		if m.Forwarded != nil || m.Via == "nostr" {
			t.Fatalf("a client set provenance: %+v", m)
		}
	}
	// The bridge headers are not ones a browser may send cross-origin.
	w := makeRequest(s, "OPTIONS", "/v1/command", "", "")
	if strings.Contains(strings.ToLower(w.Header().Get("Access-Control-Allow-Headers")), "swarmmemo") {
		t.Fatal("CORS allows the bridge headers")
	}
	// A server with no bridge secrets accepts no claim at all.
	bare := New(store, nil, Config{})
	if _, code := viaRequest(t, store, bare, "POST", "https://swarmmemo.com/v1/command", `{"operation":"post","room":"lobby","text":"hi"}`, "application/json", map[string]string{"X-SwarmMemo-Bridge": "email", "X-SwarmMemo-Bridge-Token": ""}); code != "bridge_unverified" {
		t.Fatalf("unconfigured bridge: %s", code)
	}
}

// write_via is enforced on every HTTP route and MCP, and the room reads the
// same everywhere, with its policy saying how to post.
func TestWriteViaOnHTTPRoutes(t *testing.T) {
	store, s := viaFixture(t)
	if _, err := store.Execute(t.Context(), board.Command{Operation: "post", Room: "netcat", Text: "opened"}, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.OperatorRoom(t.Context(), board.Command{Operation: "room.policy.set", Room: "netcat", Data: `{"write_via":["tcp","email"]}`}); err != nil {
		t.Fatal(err)
	}
	command := `{"operation":"post","room":"netcat","text":"hello"}`
	for _, tc := range []struct{ method, target, body, ct string }{
		{"GET", "/w/netcat/main?text=hi&format=json", "", ""},
		{"POST", "/w/netcat/main?format=json", "hi", "text/plain"},
		{"PUT", "/v1/events/via-netcat?format=json", `{"room":"netcat","page":"main","text":"hi"}`, "application/json"},
		{"MKCOL", "/w64/netcat/main/aGk?format=json", "", ""},
		{"GET", c64(command), "", ""},
		{"POST", "/v1/command", command, "application/json"},
		{"POST", "https://swarmmemo.com/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"post_message","arguments":{"room":"netcat","text":"hello"}}}`, "application/json"},
	} {
		headers := map[string]string{"Accept": "application/json, text/event-stream"}
		via, code := viaRequest(t, store, s, tc.method, tc.target, tc.body, tc.ct, headers)
		if strings.HasSuffix(tc.target, "/mcp") {
			if via != "" || code == "" {
				t.Fatalf("mcp posted into a tcp room: %q %q", via, code)
			}
			continue
		}
		if code != "room_via_restricted" {
			t.Fatalf("%s %s: via %q code %q", tc.method, tc.target, via, code)
		}
	}
	// The mail bridge is one of the allowed channels.
	if via, code := viaRequest(t, store, s, "POST", "https://swarmmemo.com"+c64(command), "", "", map[string]string{"X-SwarmMemo-Bridge": "email", "X-SwarmMemo-Bridge-Token": testBridgeToken}); via != "email" {
		t.Fatalf("bridge refused in its own room: %q", code)
	}
	for _, path := range []string{"/api/messages?room=netcat", "/api/room/netcat"} {
		w := makeRequest(s, "GET", path, "", "")
		if w.Code != 200 {
			t.Fatalf("%s: %d", path, w.Code)
		}
		if path == "/api/room/netcat" && !strings.Contains(w.Body.String(), `"write_via":["tcp","email"]`) {
			t.Fatalf("room.get hides write_via: %s", w.Body.String())
		}
	}
	w := makeRequest(s, "GET", "/api/room/netcat?format=txt", "", "text/plain")
	if !strings.Contains(w.Body.String(), "write_via=tcp,email") && !strings.Contains(w.Body.String(), `"write_via"`) {
		t.Fatalf("room text: %s", w.Body.String())
	}
	caps := makeRequest(s, "GET", "/capabilities", "", "").Body.String()
	for _, want := range []string{`"vias":[`, `"write_via":{`, `"name":"dns"`} {
		if !strings.Contains(caps, want) {
			t.Fatalf("/capabilities lacks %s", want)
		}
	}
}
