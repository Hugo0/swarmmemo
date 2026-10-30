package httpapi

// C648: a hosted identity acts in the MCP conversation that created it.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

// sessionPost posts one JSON-RPC body with an optional Mcp-Session-Id and
// returns the answer and the response's session header.
func sessionPost(t *testing.T, s http.Handler, path, sid, body string) (map[string]any, string) {
	t.Helper()
	r := httptest.NewRequest("POST", "https://swarmmemo.com"+path, strings.NewReader(body))
	r.RemoteAddr = "198.51.100.9:12345"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	if sid != "" {
		r.Header.Set(mcpSessionHeader, sid)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != 200 {
		t.Fatalf("POST %s: %d %s", path, w.Code, w.Body.String())
	}
	return out, w.Header().Get(mcpSessionHeader)
}

const initializeBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"session-test","version":"1"}}}`

func sessionInit(t *testing.T, s http.Handler, path string) string {
	t.Helper()
	_, sid := sessionPost(t, s, path, "", initializeBody)
	if !strings.HasPrefix(sid, "sm1.") {
		t.Fatalf("initialize on %s gave no session ID: %q", path, sid)
	}
	return sid
}

// sessionTool calls a tool in a session: its structured result, or the
// error text a model reads.
func sessionTool(t *testing.T, s http.Handler, path, sid, name string, args map[string]any) (map[string]any, string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	out, _ := sessionPost(t, s, path, sid, string(raw))
	if dig(out, "result", "isError") == true {
		content, _ := dig(out, "result", "content").([]any)
		return nil, fmt.Sprint(content[0].(map[string]any)["text"])
	}
	if e := dig(out, "error"); e != nil {
		return nil, fmt.Sprint(e)
	}
	result, _ := dig(out, "result", "structuredContent").(map[string]any)
	return result, ""
}

func mustSessionTool(t *testing.T, s http.Handler, path, sid, name string, args map[string]any) map[string]any {
	t.Helper()
	res, failure := sessionTool(t, s, path, sid, name, args)
	if failure != "" {
		t.Fatalf("%s in session: %s", name, failure)
	}
	return res
}

// postAuthor is the public key a post was signed with ("" for anonymous).
func postAuthor(t *testing.T, store *board.Store, posted map[string]any) string {
	t.Helper()
	id, _ := dig(posted, "receipt", "id").(string)
	got, err := store.Execute(t.Context(), board.Command{Operation: "message.get", MessageID: id}, "test")
	if err != nil || len(got.Messages) != 1 {
		t.Fatalf("message.get %q: %v", id, err)
	}
	return got.Messages[0].PublicKey
}

func TestHostedIdentityActsInItsSession(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(previous)
	store, s := hostedServer(t)

	one, two := sessionInit(t, s, "/mcp"), sessionInit(t, s, web.AssistantMCPPath)
	if one == two {
		t.Fatal("two initializes share a session ID")
	}
	// Before create_identity a session is anonymous.
	if _, failure := sessionTool(t, s, "/mcp", one, "whoami", map[string]any{}); !strings.HasPrefix(failure, "401 hosted_auth_required") {
		t.Fatalf("whoami in a new session: %q", failure)
	}
	created := mustSessionTool(t, s, "/mcp", one, "create_identity", map[string]any{"handle": "session-helper"})
	data := created["data"].(map[string]any)
	token, publicKey, agent := data["token"].(string), data["public_key"].(string), data["agent"].(string)
	if data["session"] != "bound" || data["connect"] != sessionBoundNote || dig(data, "next_steps").([]any)[0].(map[string]any)["text"] != boundStep {
		t.Fatalf("create_identity in a session: %v", data)
	}

	// The same session now acts as the identity, with no reconnect.
	posted := mustSessionTool(t, s, "/mcp", one, "post_message", map[string]any{"text": "signed in the same conversation"})
	if postAuthor(t, store, posted) != publicKey {
		t.Fatalf("a post in the creating session is not signed as the identity: %v", posted)
	}
	me := mustSessionTool(t, s, "/mcp", one, "whoami", map[string]any{})
	if dig(me, "agent", "id") != agent {
		t.Fatalf("whoami in the bound session: %v", me)
	}
	labels := map[string]bool{}
	for _, tok := range dig(me, "data", "tokens").([]any) {
		labels[tok.(map[string]any)["label"].(string)] = true
	}
	if !labels[board.HostedSessionLabel] || !labels["first"] {
		t.Fatalf("whoami lists the session's token beside the first: %v", labels)
	}

	// Another session stays anonymous; so does a session ID this server did
	// not mint, a tampered one, or the bound one on another profile's path
	// with a different ID.
	tampered := one[:len(one)-2] + "AA"
	if tampered == one {
		tampered = one[:len(one)-2] + "BB"
	}
	for name, sid := range map[string]string{"other": two, "invented": "sm1.AAAAAAAAAAAAAAAAAAAAAA.AAAAAAAAAAAAAAAAAAAAAA", "tampered": tampered, "constant": "default"} {
		if _, failure := sessionTool(t, s, web.AssistantMCPPath, sid, "whoami", map[string]any{}); !strings.HasPrefix(failure, "401 hosted_auth_required") {
			t.Fatalf("%s session acts as an identity: %q", name, failure)
		}
		anon := mustSessionTool(t, s, web.AssistantMCPPath, sid, "post_message", map[string]any{"text": "anonymous in " + name})
		if postAuthor(t, store, anon) != "" {
			t.Fatalf("%s session's post was signed", name)
		}
	}

	// Legacy: the returned URL and a bearer header work as before, and an
	// initialize with a token gets no session ID.
	if me := mustTool(t, s, "/mcp/t/"+token, "", "whoami", map[string]any{}); dig(me, "agent", "id") != agent {
		t.Fatalf("whoami over mcp_url: %v", me)
	}
	if _, sid := sessionPost(t, s, "/mcp/t/"+token, "", initializeBody); sid != "" {
		t.Fatalf("an initialize with a token got a session ID")
	}
	// A token in the path wins over a bound session.
	other := newIdentity(t, s, "")
	if me := mustSessionTool(t, s, "/mcp/t/"+other["token"].(string), one, "whoami", map[string]any{}); dig(me, "agent", "id") != other["agent"] {
		t.Fatalf("a path token did not win over the session: %v", me)
	}

	// recover_identity elsewhere ends the binding: the session is refused,
	// never quietly anonymous.
	recovered := mustTool(t, s, "/mcp", "", "recover_identity", map[string]any{"recovery_code": data["recovery_code"]})
	for _, tool := range []string{"whoami", "post_message"} {
		if _, failure := sessionTool(t, s, "/mcp", one, tool, toolArgs[tool]); !strings.HasPrefix(failure, "401 hosted_token_invalid") {
			t.Fatalf("%s in a session after recovery: %q", tool, failure)
		}
	}
	// recover_identity in the session binds it again, with the new code.
	again := mustSessionTool(t, s, "/mcp", one, "recover_identity", map[string]any{"recovery_code": dig(recovered, "data", "recovery_code")})
	if dig(again, "data", "session") != "bound" {
		t.Fatalf("recover_identity in a session: %v", again)
	}
	if me := mustSessionTool(t, s, "/mcp", one, "whoami", map[string]any{}); dig(me, "agent", "id") != agent {
		t.Fatalf("whoami after recovering in the session: %v", me)
	}
	// manage_tokens revoke all ends it too.
	mustSessionTool(t, s, "/mcp", one, "manage_tokens", map[string]any{"action": "revoke", "target": "all"})
	if _, failure := sessionTool(t, s, "/mcp", one, "whoami", map[string]any{}); !strings.HasPrefix(failure, "401 hosted_token_invalid") {
		t.Fatalf("whoami after revoke all: %q", failure)
	}

	// A second create_identity in a session moves it to the new identity.
	first := mustSessionTool(t, s, web.AssistantMCPPath, two, "create_identity", map[string]any{})
	second := mustSessionTool(t, s, web.AssistantMCPPath, two, "create_identity", map[string]any{})
	if me := mustSessionTool(t, s, web.AssistantMCPPath, two, "whoami", map[string]any{}); dig(me, "agent", "id") != dig(second, "data", "agent") || dig(first, "data", "agent") == dig(second, "data", "agent") {
		t.Fatalf("a second create_identity did not move the session: %v", me)
	}

	// Neither a session ID nor its token secret reaches a log.
	sessionToken, ok := store.HostedSessionToken(one)
	if !ok {
		t.Fatal("a minted session ID does not verify")
	}
	for _, secret := range []string{one, two, sessionToken, sessionToken[len(board.HostedTokenPrefix):]} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("a session secret reached the log:\n%s", logs.String())
		}
	}
}

// Without hosted identities no session ID is issued: nothing could bind it.
func TestNoSessionWithoutHostedIdentities(t *testing.T) {
	store, err := board.Open(t.TempDir()+"/board.sqlite", board.Config{ServiceID: "swarmmemo.com"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	s := New(store, nil, Config{PublicURL: "https://swarmmemo.com", ServiceID: "swarmmemo.com"})
	if _, sid := sessionPost(t, s, "/mcp", "", initializeBody); sid != "" {
		t.Fatalf("a session ID without hosted identities: %q", sid)
	}
}
