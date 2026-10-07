package httpapi

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

// eventsServer is hostedServer with the outbound sender on, which MCP Events
// need; push says whether it is on.
func eventsServer(t *testing.T, push bool) *Server {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	kek := filepath.Join(t.TempDir(), "hosted-kek")
	if err := os.WriteFile(kek, []byte(base64.RawURLEncoding.EncodeToString(key)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := board.Open(filepath.Join(t.TempDir(), "board.sqlite"), board.Config{ServiceID: "swarmmemo.com", HostedKEKFile: kek})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return New(store, nil, Config{PublicURL: "https://swarmmemo.com", ServiceID: "swarmmemo.com", PushDelivery: push, Version: "test"})
}

var modernMeta = map[string]any{
	"io.modelcontextprotocol/protocolVersion":    "2026-07-28",
	"io.modelcontextprotocol/clientCapabilities": map[string]any{},
	"io.modelcontextprotocol/clientInfo":         map[string]any{"name": "test", "version": "1"},
}

// rpc posts one JSON-RPC request; headers are the modern ones unless
// overridden (an empty value removes a header).
func eventsRPC(t *testing.T, s http.Handler, path, method string, params map[string]any, headers map[string]string) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": method, "params": params})
	r := httptest.NewRequest("POST", "https://swarmmemo.com"+path, strings.NewReader(string(body)))
	r.RemoteAddr = "198.51.100.9:12345"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("MCP-Protocol-Version", "2026-07-28")
	r.Header.Set("Mcp-Method", method)
	for k, v := range headers {
		if v == "" {
			r.Header.Del(k)
		} else {
			r.Header.Set(k, v)
		}
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func withMeta(params map[string]any) map[string]any {
	out := map[string]any{"_meta": modernMeta}
	for k, v := range params {
		out[k] = v
	}
	return out
}

func rpcErrorCode(out map[string]any) float64 {
	code, _ := dig(out, "error", "code").(float64)
	return code
}

// server/discover advertises the events capability and the protocol
// versions the SDK serves, on both hosted profiles.
func TestMCPEventsDiscover(t *testing.T) {
	s := eventsServer(t, true)
	for _, path := range []string{"/mcp", web.AssistantMCPPath} {
		code, out := eventsRPC(t, s, path, "server/discover", withMeta(nil), nil)
		caps, _ := dig(out, "result", "capabilities").(map[string]any)
		if code != 200 || caps["events"] == nil || caps["tools"] == nil || dig(out, "result", "resultType") != "complete" {
			t.Fatalf("%s discover: %d %+v", path, code, out)
		}
		if ext, _ := caps["extensions"].(map[string]any); ext["ai.smithery/events"] == nil {
			t.Fatalf("%s: no smithery extension: %+v", path, caps)
		}
		versions, _ := dig(out, "result", "supportedVersions").([]any)
		var got []string
		for _, v := range versions {
			got = append(got, v.(string))
		}
		if !reflect.DeepEqual(got, mcpVersions) {
			t.Fatalf("supportedVersions %v, events errors name %v", got, mcpVersions)
		}
	}
	// With the sender off there are no events, and discover says so.
	off := eventsServer(t, false)
	_, out := eventsRPC(t, off, "/mcp", "server/discover", withMeta(nil), nil)
	if caps, _ := dig(out, "result", "capabilities").(map[string]any); caps["events"] != nil {
		t.Fatalf("events advertised with the sender off: %+v", caps)
	}
	if code, out := eventsRPC(t, off, "/mcp", "events/list", withMeta(nil), nil); rpcErrorCode(out) != -32601 {
		t.Fatalf("events/list with the sender off: %d %+v", code, out)
	}
}

// A 2025-06-18 client's handshake is byte for byte what it was: no events
// capability, no extension, the version it asked for.
func TestMCPEventsLegacyHandshakeUnchanged(t *testing.T) {
	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`
	answer := func(s *Server) string {
		r := httptest.NewRequest("POST", "https://swarmmemo.com/mcp", strings.NewReader(init))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("initialize: %d %s", w.Code, w.Body)
		}
		return w.Body.String()
	}
	on, off := answer(eventsServer(t, true)), answer(eventsServer(t, false))
	if on != off || strings.Contains(on, "events") || !strings.Contains(on, `"protocolVersion":"2025-06-18"`) {
		t.Fatalf("legacy initialize changed:\n%s\n%s", on, off)
	}
	// A 2025-11-25 client learns of Smithery's extension, nothing else.
	newer := strings.Replace(init, "2025-06-18", "2025-11-25", 1)
	r := httptest.NewRequest("POST", "https://swarmmemo.com/mcp", strings.NewReader(newer))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	w := httptest.NewRecorder()
	eventsServer(t, true).ServeHTTP(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if ext, _ := dig(out, "result", "capabilities", "extensions").(map[string]any); ext["ai.smithery/events"] == nil || dig(out, "result", "capabilities", "events") != nil {
		t.Fatalf("2025-11-25 initialize: %s", w.Body)
	}
}

// events/list: every event with a name, description, webhook delivery and
// both schemas; Smithery's alias is the same list, with or without _meta.
func TestMCPEventsList(t *testing.T) {
	s := eventsServer(t, true)
	code, out := eventsRPC(t, s, "/mcp", "events/list", withMeta(nil), nil)
	events, _ := dig(out, "result", "events").([]any)
	if code != 200 || len(events) != 8 || dig(out, "result", "resultType") != "complete" || dig(out, "result", "_meta", "io.modelcontextprotocol/serverInfo", "name") != "swarmmemo" {
		t.Fatalf("events/list: %d %+v", code, out)
	}
	names := []string{}
	for _, e := range events {
		m := e.(map[string]any)
		names = append(names, m["name"].(string))
		if m["description"] == "" || !reflect.DeepEqual(m["delivery"], []any{"webhook"}) || dig(m, "inputSchema", "type") != "object" || dig(m, "payloadSchema", "properties", "untrusted") == nil {
			t.Fatalf("event %+v", m)
		}
	}
	if strings.Join(names, " ") != "reply mention conversation.message conversation.request room.post work.open work.update identity.witnessed" {
		t.Fatalf("names %v", names)
	}
	_, alias := eventsRPC(t, s, web.AssistantMCPPath, "ai.smithery/events/list", map[string]any{}, map[string]string{"MCP-Protocol-Version": "", "Mcp-Method": ""})
	if aliased, _ := dig(alias, "result", "events").([]any); len(aliased) != 8 || dig(alias, "result", "resultType") != nil {
		t.Fatalf("smithery alias: %+v", alias)
	}
}

// The modern transport's rules: headers must match the body, and a version
// the extension is not defined at is refused with the supported list.
func TestMCPEventsHeaderRules(t *testing.T) {
	s := eventsServer(t, true)
	if code, out := eventsRPC(t, s, "/mcp", "events/list", withMeta(nil), map[string]string{"Mcp-Method": "tools/list"}); code != 400 || rpcErrorCode(out) != -32020 {
		t.Fatalf("method mismatch: %d %+v", code, out)
	}
	if code, out := eventsRPC(t, s, "/mcp", "events/list", withMeta(nil), map[string]string{"MCP-Protocol-Version": "2025-06-18"}); code != 400 || rpcErrorCode(out) != -32020 {
		t.Fatalf("version mismatch: %d %+v", code, out)
	}
	future := map[string]any{"_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": "2027-01-01", "io.modelcontextprotocol/clientCapabilities": map[string]any{}}}
	code, out := eventsRPC(t, s, "/mcp", "events/list", future, map[string]string{"MCP-Protocol-Version": "2027-01-01"})
	if code != 400 || rpcErrorCode(out) != -32022 || dig(out, "error", "data", "requested") != "2027-01-01" {
		t.Fatalf("unsupported version: %d %+v", code, out)
	}
}

// Subscribing needs an identity, for public events too; with one, the
// spec's refusals come back as JSON-RPC errors.
func TestMCPEventsSubscribeNeedsAnIdentity(t *testing.T) {
	s := eventsServer(t, true)
	secret := "whsec_" + base64.StdEncoding.EncodeToString(make([]byte, 32))
	sub := func(name string, args map[string]any, url string) map[string]any {
		return withMeta(map[string]any{"name": name, "arguments": args, "delivery": map[string]any{"mode": "webhook", "url": url, "secret": secret}})
	}
	for _, name := range []string{"reply", "conversation.message", "room.post", "work.open"} {
		if _, out := eventsRPC(t, s, "/mcp", "events/subscribe", sub(name, map[string]any{"room": "lobby"}, "https://example.com/h"), nil); rpcErrorCode(out) != -32012 {
			t.Fatalf("anonymous %s: %+v", name, out)
		}
	}
	if _, out := eventsRPC(t, s, "/mcp", "events/unsubscribe", withMeta(map[string]any{"name": "reply", "delivery": map[string]any{"url": "https://example.com/h"}}), nil); rpcErrorCode(out) != -32012 {
		t.Fatalf("anonymous unsubscribe: %+v", out)
	}
	// A made-up bearer token meets the sign-in gate (401 and its challenge);
	// a made-up path token, the events refusal.
	if code, out := eventsRPC(t, s, "/mcp", "events/subscribe", sub("reply", nil, "https://example.com/h"), map[string]string{"Authorization": "Bearer smh_not-a-real-token"}); code != 401 {
		t.Fatalf("a made-up bearer token: %d %+v", code, out)
	}
	if _, out := eventsRPC(t, s, "/mcp/t/smh_not-a-real-token", "events/subscribe", sub("reply", nil, "https://example.com/h"), nil); rpcErrorCode(out) != -32012 {
		t.Fatalf("a made-up path token: %+v", out)
	}
	token := newIdentity(t, s, "events-helper")["token"].(string)
	path := "/mcp/t/" + token
	for _, c := range []struct {
		params map[string]any
		code   float64
	}{
		{sub("reply", nil, "http://example.com/h"), -32602},
		{sub("reply", nil, "https://127.0.0.1/h"), -32602},
		{sub("nope", nil, "https://example.com/h"), -32011},
		{withMeta(map[string]any{"name": "reply", "delivery": map[string]any{"mode": "poll"}}), -32014},
	} {
		if _, out := eventsRPC(t, s, path, "events/subscribe", c.params, nil); rpcErrorCode(out) != c.code {
			t.Errorf("%+v: %+v, want %v", c.params, out, c.code)
		}
	}
	if _, out := eventsRPC(t, s, path, "events/unsubscribe", withMeta(map[string]any{"name": "reply", "delivery": map[string]any{"url": "https://example.com/h"}}), nil); rpcErrorCode(out) != -32011 {
		t.Fatalf("unsubscribe of nothing: %+v", out)
	}
	// The owner's list tool works with nothing subscribed.
	listed := mustTool(t, s, path, "", "list_event_subscriptions", map[string]any{})
	if subs, _ := dig(listed, "data", "mcp_event_subscriptions").([]any); subs == nil || len(subs) != 0 {
		t.Fatalf("list_event_subscriptions: %+v", listed)
	}
}

// Discovery states the block at parity with the MCP methods.
func TestMCPEventsDiscovery(t *testing.T) {
	s := eventsServer(t, true)
	caps := discoveryJSON(t, s, "/capabilities")
	block, _ := caps["mcp_events"].(map[string]any)
	if block == nil || block["protocol_version"] != "2026-07-28" || block["enabled"] != true || len(block["events"].([]any)) != 8 {
		t.Fatalf("capabilities mcp_events: %+v", block)
	}
	card := discoveryJSON(t, s, "/.well-known/mcp/server-card.json")
	if events, _ := card["events"].(map[string]any); events == nil || events["protocolVersion"] != "2026-07-28" {
		t.Fatalf("server card events: %+v", card["events"])
	}
}

func discoveryJSON(t *testing.T, s http.Handler, path string) map[string]any {
	t.Helper()
	r := httptest.NewRequest("GET", "https://swarmmemo.com"+path, nil)
	r.Header.Set("Accept", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != 200 {
		t.Fatalf("GET %s: %d %s", path, w.Code, w.Body)
	}
	return out
}
