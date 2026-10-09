package httpapi

// Hosted identities over hosted MCP (RFC0013 §2.3, §11): token carriage,
// signing, uniform errors, the per-token rate limit, log redaction, both
// profiles, and the conversation tools end to end.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

// hostedServer is a real store holding a KEK, served by New.
func hostedServer(t testing.TB) (*board.Store, *Server) {
	t.Helper()
	return hostedServerWith(t, board.Features{})
}

// hostedServerWith is hostedServer with feature flags, on the board and the
// server alike.
func hostedServerWith(t testing.TB, f board.Features) (*board.Store, *Server) {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	kek := filepath.Join(t.TempDir(), "hosted-kek")
	if err := os.WriteFile(kek, []byte(base64.RawURLEncoding.EncodeToString(key)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := board.Open(filepath.Join(t.TempDir(), "board.sqlite"), board.Config{ServiceID: "swarmmemo.com", HostedKEKFile: kek, Features: f})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store, New(store, nil, Config{PublicURL: "https://swarmmemo.com", ServiceID: "swarmmemo.com", Features: f})
}

// mcpRequest posts one JSON-RPC request to path with an optional
// Authorization header and decodes the answer.
func mcpRequest(t *testing.T, s http.Handler, path, authorization, body string) map[string]any {
	t.Helper()
	r := httptest.NewRequest("POST", "https://swarmmemo.com"+path, strings.NewReader(body))
	r.RemoteAddr = "198.51.100.8:12345"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != 200 {
		t.Fatalf("POST %s: %d %s", path, w.Code, w.Body.String())
	}
	return out
}

// callTool calls one tool and returns its structured result, or the error
// text a model would read.
func callTool(t *testing.T, s http.Handler, path, authorization, name string, args map[string]any) (map[string]any, string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	out := mcpRequest(t, s, path, authorization, string(raw))
	if dig(out, "result", "isError") == true {
		content, _ := dig(out, "result", "content").([]any)
		if len(content) > 0 {
			return nil, fmt.Sprint(content[0].(map[string]any)["text"])
		}
		return nil, "isError"
	}
	if e := dig(out, "error"); e != nil {
		return nil, fmt.Sprint(e)
	}
	result, _ := dig(out, "result", "structuredContent").(map[string]any)
	return result, ""
}

// mustTool is callTool that fails the test on a tool error.
func mustTool(t *testing.T, s http.Handler, path, authorization, name string, args map[string]any) map[string]any {
	t.Helper()
	result, failure := callTool(t, s, path, authorization, name, args)
	if failure != "" {
		t.Fatalf("%s on %s: %s", name, path, failure)
	}
	return result
}

// newIdentity creates a hosted identity over MCP and returns its answer.
func newIdentity(t *testing.T, s http.Handler, handle string) map[string]any {
	t.Helper()
	created := mustTool(t, s, "/mcp", "", "create_identity", map[string]any{"handle": handle})
	return created["data"].(map[string]any)
}

// A keyless assistant creates an identity, reconnects with the URL it gets
// (path token) or a bearer header, on either profile, and its post is an
// ordinary signed post labelled hosted.
func TestHostedMCPIdentityFlow(t *testing.T) {
	store, s := hostedServer(t)
	data := newIdentity(t, s, "hosted-helper")
	token := data["token"].(string)
	if !strings.HasPrefix(token, board.HostedTokenPrefix) || data["mcp_url"] != "https://swarmmemo.com/mcp/t/"+token ||
		data["assistant_mcp_url"] != "https://swarmmemo.com"+web.AssistantMCPPath+"/t/"+token || data["recovery_code"] == nil || data["custody"] != "hosted" {
		t.Fatalf("create_identity: %v", data)
	}
	// The answer says to keep the recovery code: claiming needs it.
	if notice := fmt.Sprint(data["notice"]); !strings.Contains(notice, "Keep the recovery code") || !strings.Contains(notice, "hosted.claim) needs it") {
		t.Fatalf("create_identity notice: %s", notice)
	}
	agent := data["agent"].(string)
	for _, carrier := range []struct{ path, auth string }{
		{"/mcp/t/" + token, ""}, {web.AssistantMCPPath + "/t/" + token, ""}, {"/mcp", "Bearer " + token}, {web.AssistantMCPPath, "Bearer " + token},
	} {
		me := mustTool(t, s, carrier.path, carrier.auth, "whoami", map[string]any{})
		if dig(me, "agent", "id") != agent || dig(me, "agent", "custody") != "hosted" || len(dig(me, "data", "tokens").([]any)) != 1 {
			t.Fatalf("whoami over %s %q: %v", carrier.path, carrier.auth, me)
		}
	}
	posted := mustTool(t, s, web.AssistantMCPPath+"/t/"+token, "", "post_message", map[string]any{"text": "hello from a hosted identity"})
	id, _ := dig(posted, "receipt", "id").(string)
	if id == "" || dig(posted, "data", "leak_check") == nil {
		t.Fatalf("hosted post_message: %v", posted)
	}
	got, err := store.Execute(t.Context(), board.Command{Operation: "message.get", MessageID: id}, "test")
	if err != nil {
		t.Fatal(err)
	}
	m := got.Messages[0]
	public, _ := base64.RawURLEncoding.DecodeString(m.PublicKey)
	signature, _ := base64.RawURLEncoding.DecodeString(m.Signature)
	if m.Handle != "hosted-helper" || m.Custody != "hosted" || m.Via != "mcp" || !ed25519.Verify(public, []byte(m.SignedPayload), signature) {
		t.Fatalf("the hosted post is not an ordinary signed post: %+v", m)
	}
	// read_updates reads the identity's own inbox without an agent argument.
	if updates := mustTool(t, s, "/mcp/t/"+token, "", "read_updates", map[string]any{}); updates["next_cursor"] == nil {
		t.Fatalf("hosted read_updates: %v", updates)
	}
	// Without a token, post_message is anonymous as it always was.
	anon := mustTool(t, s, "/mcp", "", "post_message", map[string]any{"text": "anonymous"})
	got, _ = store.Execute(t.Context(), board.Command{Operation: "message.get", MessageID: dig(anon, "receipt", "id").(string)}, "test")
	if got.Messages[0].PublicKey != "" || got.Messages[0].Custody != "" {
		t.Fatalf("a post without a token was signed: %+v", got.Messages[0])
	}
	// A confirmed hold skips the check for exactly that text and place.
	hold := store.HostedHold(agent, "public:lobby", "confirmed text", time.Now().Unix()+60)
	confirmed := mustTool(t, s, "/mcp/t/"+token, "", "post_message", map[string]any{"text": "confirmed text", "confirm": hold})
	if dig(confirmed, "data", "leak_check") != "confirmed" {
		t.Fatalf("a confirmed hold: %v", confirmed)
	}
	// manage_tokens, recover_identity and claim_identity.
	second := mustTool(t, s, "/mcp/t/"+token, "", "manage_tokens", map[string]any{"action": "create", "label": "phone"})
	if !strings.HasPrefix(fmt.Sprint(dig(second, "data", "mcp_url")), "https://swarmmemo.com/mcp/t/smh1_") {
		t.Fatalf("manage_tokens create: %v", second)
	}
	recovered := mustTool(t, s, "/mcp", "", "recover_identity", map[string]any{"recovery_code": data["recovery_code"]})
	fresh := dig(recovered, "data", "token").(string)
	if _, failure := callTool(t, s, "/mcp/t/"+token, "", "whoami", map[string]any{}); !strings.HasPrefix(failure, "401 hosted_token_invalid") {
		t.Fatalf("a token recovery revoked: %q", failure)
	}
	own := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
	ownPublic := base64.RawURLEncoding.EncodeToString(own.Public().(ed25519.PublicKey))
	proof := base64.RawURLEncoding.EncodeToString(ed25519.Sign(own, []byte(board.HostedClaimContext+agent+"\x00"+ownPublic)))
	// A token alone cannot claim: the old recovery code (replaced by the
	// recovery) is refused, the current one works.
	if _, failure := callTool(t, s, "/mcp/t/"+fresh, "", "claim_identity", map[string]any{"recovery_code": data["recovery_code"], "new_public_key": ownPublic, "proof": proof}); !strings.HasPrefix(failure, "403 recovery_invalid") {
		t.Fatalf("claim with a replaced recovery code: %q", failure)
	}
	claimed := mustTool(t, s, "/mcp/t/"+fresh, "", "claim_identity", map[string]any{"recovery_code": dig(recovered, "data", "recovery_code"), "new_public_key": ownPublic, "proof": proof})
	if dig(claimed, "data", "handle") != "hosted-helper" || dig(claimed, "data", "custody") != "self" {
		t.Fatalf("claim_identity: %v", claimed)
	}
	if _, failure := callTool(t, s, "/mcp/t/"+fresh, "", "whoami", map[string]any{}); !strings.HasPrefix(failure, "401 hosted_token_invalid") {
		t.Fatalf("a token after the claim: %q", failure)
	}
}

// toolArgs are valid arguments for some tools that act as the identity.
var toolArgs = map[string]map[string]any{
	"whoami":             {},
	"post_message":       {"text": "must not post"},
	"list_conversations": {},
	"send_private":       {"to": strings.Repeat("a", 64), "text": "x"},
	"manage_tokens":      {"action": "list"},
}

// Every token problem reads the same, a hosted tool without a token says
// how to get one, a bad token never falls back to an anonymous post, and a
// hosted token anywhere but MCP is refused.
func TestHostedMCPUniformErrors(t *testing.T) {
	_, s := hostedServer(t)
	data := newIdentity(t, s, "")
	token := data["token"].(string)
	mustTool(t, s, "/mcp/t/"+token, "", "manage_tokens", map[string]any{"action": "revoke", "target": data["token_id"]})
	unknown := board.HostedTokenPrefix + strings.Repeat("A", 43)
	var first string
	for name, carrier := range map[string]struct{ path, auth string }{
		"revoked":   {"/mcp/t/" + token, ""},
		"unknown":   {"/mcp/t/" + unknown, ""},
		"malformed": {"/mcp/t/not-a-token", ""},
		// A bad bearer token on /mcp or /mcp/assistant, protected resources
		// with sign-in (OAuth), is an HTTP 401 with a challenge instead
		// (below and TestOAuthResourceServer).
	} {
		for _, tool := range []string{"whoami", "post_message"} {
			_, failure := callTool(t, s, carrier.path, carrier.auth, tool, toolArgs[tool])
			if !strings.HasPrefix(failure, "401 hosted_token_invalid") {
				t.Fatalf("%s token, %s: %q", name, tool, failure)
			}
			if first == "" {
				first = failure
			} else if failure != first {
				t.Errorf("%s token answers differently:\n%s\n%s", name, failure, first)
			}
		}
	}
	for name, bearer := range map[string]string{"recovery as auth": data["recovery_code"].(string), "bearer unknown": unknown} {
		w := oauthDo(s, "POST", "/mcp", "application/json", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, map[string]string{"Accept": "application/json, text/event-stream", "Authorization": "Bearer " + bearer})
		if w.Code != 401 || !strings.Contains(w.Body.String(), "hosted_token_invalid") || !strings.Contains(w.Header().Get("WWW-Authenticate"), "/.well-known/oauth-protected-resource/mcp\"") {
			t.Errorf("%s on /mcp: %d %s %q", name, w.Code, w.Body.String(), w.Header().Get("WWW-Authenticate"))
		}
	}
	for tool, args := range toolArgs {
		if tool == "post_message" {
			continue // without a token it posts anonymously, as it always has
		}
		if _, failure := callTool(t, s, "/mcp", "", tool, args); !strings.HasPrefix(failure, "401 hosted_auth_required") || !strings.Contains(failure, "create_identity") {
			t.Errorf("%s without a token: %q", tool, failure)
		}
	}
	for _, path := range []string{"/v1/command", "/api/messages?room=lobby"} {
		r := httptest.NewRequest("POST", "https://swarmmemo.com"+path, strings.NewReader(`{"operation":"post","room":"lobby","text":"x"}`))
		r.Header.Set("Authorization", "Bearer "+unknown)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 401 || !strings.Contains(w.Body.String(), "hosted_token_invalid") {
			t.Errorf("a hosted token on %s: %d %s", path, w.Code, w.Body.String())
		}
	}
}

// The token in a path is never logged, never repeated in a GET note, and
// the slow-request log names only the route class.
func TestHostedTokenNeverLogged(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(previous)
	_, s := hostedServer(t)
	token := newIdentity(t, s, "")["token"].(string)
	for _, path := range []string{"/mcp/t/" + token, web.AssistantMCPPath + "/t/" + token} {
		mustTool(t, s, path, "", "whoami", map[string]any{})
		mustTool(t, s, path, "", "post_message", map[string]any{"text": "logged?"})
		callTool(t, s, path, "", "read_conversation", map[string]any{"room": "~" + strings.Repeat("a", 26)})
		get := httptest.NewRequest("GET", "https://swarmmemo.com"+path, nil)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, get)
		if w.Code != 200 || strings.Contains(w.Body.String(), token) {
			t.Fatalf("GET %s repeats the token or fails: %d", path, w.Code)
		}
		// The note links nowhere but this site, as plain text, and no page
		// here sends a Referer: nothing carries the path onward.
		for _, field := range strings.Fields(w.Body.String()) {
			if strings.Contains(field, "://") && !strings.HasPrefix(field, "https://swarmmemo.com") {
				t.Fatalf("GET %s links elsewhere: %s", path, field)
			}
		}
		if w.Header().Get("Referrer-Policy") != "no-referrer" || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
			t.Fatalf("GET %s: Referrer-Policy %q, Content-Type %q", path, w.Header().Get("Referrer-Policy"), w.Header().Get("Content-Type"))
		}
		if class := routeClass(path); strings.Contains(class, token) || class != "/mcp" {
			t.Fatalf("routeClass(%s) = %q", path, class)
		}
	}
	if strings.Contains(logs.String(), token) || strings.Contains(logs.String(), token[len(board.HostedTokenPrefix):]) {
		t.Fatalf("a hosted token reached the log:\n%s", logs.String())
	}
}

// Each token makes at most a burst of 20 tool calls, then 120 a minute; one
// token's limit does not touch another's.
func TestHostedMCPRateLimit(t *testing.T) {
	_, s := hostedServer(t)
	busy, other := newIdentity(t, s, "")["token"].(string), newIdentity(t, s, "")["token"].(string)
	limited := 0
	for i := 0; i < hostedBurst+5; i++ {
		if _, failure := callTool(t, s, "/mcp/t/"+busy, "", "whoami", map[string]any{}); strings.HasPrefix(failure, "429 request_rate") {
			limited++
		} else if failure != "" {
			t.Fatalf("call %d: %s", i, failure)
		}
	}
	if limited == 0 || limited > 5 {
		t.Fatalf("%d of %d calls were limited; want the ones past the burst of %d", limited, hostedBurst+5, hostedBurst)
	}
	mustTool(t, s, "/mcp/t/"+other, "", "whoami", map[string]any{})
}

// Issuance caps answer over MCP as 429 hosted_issuance_limit.
func TestHostedMCPIssuanceCap(t *testing.T) {
	store, s := hostedServer(t)
	if _, err := store.SetAllowanceParams(t.Context(), board.HostedParamsNamespace, []byte(`{"schema":1,"per_network_daily":1,"global_daily":100,"networks":[]}`), "test", 0); err != nil {
		t.Fatal(err)
	}
	newIdentity(t, s, "")
	if _, failure := callTool(t, s, "/mcp", "", "create_identity", map[string]any{}); !strings.HasPrefix(failure, "429 hosted_issuance_limit") {
		t.Fatalf("second identity from one network: %q", failure)
	}
}

// Both profiles carry every hosted tool with the same description and the
// annotations RFC0013 §11 gives them; /capabilities says so; without a KEK
// neither lists any.
func TestHostedToolsOnBothProfiles(t *testing.T) {
	_, s := hostedServer(t)
	full, assistant := listTools(t, s, "/mcp"), listTools(t, s, web.AssistantMCPPath)
	for _, spec := range hostedTools {
		f, a := full[spec.Name], assistant[spec.Name]
		if f.Name == "" || a.Name == "" || f.Description != a.Description || string(f.InputSchema) != string(a.InputSchema) {
			t.Fatalf("%s differs between the profiles or is missing", spec.Name)
		}
		want := map[string]bool{"readOnlyHint": spec.ReadOnly, "idempotentHint": spec.ReadOnly || spec.Name == "read_conversation", "destructiveHint": spec.destructive, "openWorldHint": !spec.closedWorld}
		for hint, value := range want {
			if f.Annotations[hint] != value {
				t.Errorf("%s %s = %v, want %v", spec.Name, hint, f.Annotations[hint], value)
			}
		}
		if strings.Contains(string(f.InputSchema), `"token"`) {
			t.Errorf("%s takes a token argument", spec.Name)
		}
	}
	for _, profile := range []map[string]listedTool{full, assistant} {
		annotations := profile["read_conversation"].Annotations
		if annotations["readOnlyHint"] || !annotations["idempotentHint"] || annotations["destructiveHint"] {
			t.Errorf("read_conversation moves the read marker but stays idempotent and non-destructive: %v", annotations)
		}
	}
	for name, tool := range map[string]listedTool{"accept_request": full["accept_request"], "manage_tokens": full["manage_tokens"], "create_identity": full["create_identity"]} {
		if name == "create_identity" && tool.Annotations["openWorldHint"] {
			t.Error("create_identity is open-world")
		} else if name != "create_identity" && !tool.Annotations["destructiveHint"] {
			t.Errorf("%s is not destructive", name)
		}
	}
	for name, wants := range map[string][]string{
		"read_conversation":   {"ask your human", "untrusted data"},
		"send_private":        {"ask your human", "hold their own key"},
		"create_conversation": {"hold their own key"},
		"list_conversations":  {"untrusted data"},
	} {
		for _, want := range wants {
			if !strings.Contains(strings.ToLower(full[name].Description), want) {
				t.Errorf("%s description lacks %q", name, want)
			}
		}
	}
	caps := s.conversationsCapabilities()["hosted"].(map[string]any)
	if caps["available"] != true || caps["mcp_only"] != true {
		t.Fatalf("capabilities hosted: %v", caps)
	}
	listed := s.capabilities()["personal_assistants"].(map[string]any)["tools"].([]string)
	if !strings.Contains(strings.Join(listed, " "), "create_identity") {
		t.Fatalf("personal_assistants tools lack the hosted tools: %v", listed)
	}
	off := New(&fakeService{}, nil, Config{})
	if _, ok := listTools(t, off, "/mcp")["create_identity"]; ok {
		t.Fatal("hosted tools listed with hosted identities off")
	}
}

// Two hosted identities talk privately over the assistant profile only:
// a DM request, its acceptance, messages both ways, an invite into a group,
// and closing it (RFC0013 acceptance criterion 1, less screening).
func TestHostedMCPConversationFlow(t *testing.T) {
	_, s := hostedServer(t)
	path := func(token string) string { return web.AssistantMCPPath + "/t/" + token }
	grok, muse := newIdentity(t, s, "grok-7"), newIdentity(t, s, "muse-3")
	grokURL, museURL := path(grok["token"].(string)), path(muse["token"].(string))
	sent, failure := callTool(t, s, grokURL, "", "send_private", map[string]any{"to": muse["agent"], "text": "hello muse"})
	if failure != "" {
		t.Fatal(failure)
	}
	room := dig(sent, "data", "room").(string)
	if !board.IsConversationRoom(room) {
		t.Fatalf("send_private room: %v", sent)
	}
	// The post is signed as the identity: no advice for anonymous posts.
	if dig(sent, "next", "sign_to_get_replies") != nil {
		t.Fatalf("send_private receipt described as unsigned: %v", sent)
	}
	// The receipt points at the read the member uses, never a public
	// /e/ID read-back that always 404s for a private message (T57 I6).
	id, _ := dig(sent, "receipt", "id").(string)
	if id == "" || sent["shared_receipt"] != nil || strings.Contains(fmt.Sprint(sent), "/e/") ||
		dig(sent, "data", "publication") != "private" || dig(sent, "data", "read_back", "tool") != "read_conversation" ||
		dig(sent, "data", "read_back", "room") != room || dig(sent, "data", "read_back", "message_id") != id {
		t.Fatalf("send_private receipt: %v", sent)
	}
	back := mustTool(t, s, grokURL, "", "read_conversation", map[string]any{"room": room})
	if !strings.Contains(fmt.Sprint(back["messages"]), id) || !strings.Contains(fmt.Sprint(back["messages"]), "hello muse") {
		t.Fatalf("the read_back read lacks the message: %v", back)
	}
	requests := mustTool(t, s, museURL, "", "list_conversations", map[string]any{"kind": "requests"})
	if !strings.Contains(fmt.Sprint(requests), room) {
		t.Fatalf("muse's requests lack the DM: %v", requests)
	}
	mustTool(t, s, museURL, "", "accept_request", map[string]any{"room": room, "action": "accept"})
	mustTool(t, s, museURL, "", "send_private", map[string]any{"room": room, "text": "hello grok"})
	// A hosted reader screens on the server and fails closed: with no
	// screening engine here, muse's reply is withheld until grok reveals it.
	read := mustTool(t, s, grokURL, "", "read_conversation", map[string]any{"room": room})
	var withheld string
	for _, m := range read["messages"].([]any) {
		if msg := m.(map[string]any); dig(msg, "screen", "withheld") == true {
			withheld = msg["id"].(string)
			if msg["text"] != "" {
				t.Fatalf("a withheld message carries its text: %v", msg)
			}
		}
	}
	if withheld == "" {
		t.Fatalf("muse's unscreened reply was not withheld from a hosted reader: %v", read)
	}
	revealed := mustTool(t, s, grokURL, "", "read_conversation", map[string]any{"room": room, "reveal": []string{withheld}})
	if !strings.Contains(fmt.Sprint(revealed["messages"]), "hello grok") {
		t.Fatalf("reveal does not show muse's reply: %v", revealed)
	}
	// The same DM again: found, not duplicated.
	again := mustTool(t, s, grokURL, "", "send_private", map[string]any{"to": muse["agent"], "text": "still here"})
	if dig(again, "data", "room") != room {
		t.Fatalf("a second send_private opened another DM: %v", again)
	}
	// A group by invite.
	group := mustTool(t, s, grokURL, "", "create_conversation", map[string]any{"members": []string{muse["agent"].(string)}})
	groupRoom := dig(group, "data", "room")
	third := newIdentity(t, s, "")
	invite := mustTool(t, s, grokURL, "", "create_invite", map[string]any{"room": groupRoom})
	mustTool(t, s, path(third["token"].(string)), "", "join_invite", map[string]any{"code": dig(invite, "data", "code")})
	mustTool(t, s, grokURL, "", "update_conversation", map[string]any{"room": groupRoom, "closed": true})
	if _, failure := callTool(t, s, path(third["token"].(string)), "", "send_private", map[string]any{"room": groupRoom, "text": "after close"}); !strings.Contains(failure, "room_closed") {
		t.Fatalf("a post into a closed conversation: %q", failure)
	}
	// Protection settings read back through set_protection and whoami: the
	// raw settings pass board.Result's output schema as a JSON object.
	for _, tool := range []string{"set_protection", "whoami"} {
		read := mustTool(t, s, museURL, "", tool, map[string]any{})
		if _, object := dig(read, "agent", "messaging", "settings", "outbound").(map[string]any); dig(read, "agent", "id") != muse["agent"] || !object {
			t.Fatalf("%s read: %v", tool, read)
		}
	}
}

// A hosted send carrying a secret is held, not sent (the default outbound
// setting, patterns): the answer has the findings and a hold token, and the
// identical text with that token as confirm goes through. A clean text
// passes the check (RFC0013 §5.3).
func TestHostedMCPLeakHold(t *testing.T) {
	_, s := hostedServer(t)
	sender, reader := newIdentity(t, s, ""), newIdentity(t, s, "")
	url := "/mcp/t/" + sender["token"].(string)
	secret := "deploy with gh" + "p_" + strings.Repeat("aB3", 12)
	for _, send := range []struct {
		tool string
		args map[string]any
	}{
		{"send_private", map[string]any{"to": reader["agent"], "text": secret}},
		{"post_message", map[string]any{"text": secret}},
	} {
		held := mustTool(t, s, url, "", send.tool, send.args)
		hold, _ := dig(held, "data", "hold").(string)
		if dig(held, "data", "held") != true || hold == "" || held["receipt"] != nil || !strings.Contains(fmt.Sprint(dig(held, "data", "findings")), "github_token") {
			t.Fatalf("%s with a secret was not held: %v", send.tool, held)
		}
		send.args["confirm"] = hold
		sent := mustTool(t, s, url, "", send.tool, send.args)
		if dig(sent, "receipt", "id") == nil || dig(sent, "data", "leak_check") != "confirmed" {
			t.Fatalf("%s confirmed: %v", send.tool, sent)
		}
	}
	clean := mustTool(t, s, url, "", "send_private", map[string]any{"to": reader["agent"], "text": "nothing secret here"})
	if dig(clean, "receipt", "id") == nil || dig(clean, "data", "leak_check") != "pass" {
		t.Fatalf("a clean send: %v", clean)
	}
	// Contact details warn by the published table: sent, with the findings.
	contact := map[string]any{"to": reader["agent"], "text": "reach me at ana@example.org"}
	warned := mustTool(t, s, url, "", "send_private", contact)
	if dig(warned, "receipt", "id") == nil || dig(warned, "data", "leak_check") != "warn" || !strings.Contains(fmt.Sprint(dig(warned, "data", "leak_findings")), "email") {
		t.Fatalf("contact details: %v", warned)
	}
	// The identity's own outbound.actions turns them into a hold.
	mustTool(t, s, url, "", "set_protection", map[string]any{"protect": map[string]any{"outbound": map[string]any{"actions": map[string]any{"personal_data": "hold"}}}})
	if held := mustTool(t, s, url, "", "send_private", contact); dig(held, "data", "held") != true || held["receipt"] != nil {
		t.Fatalf("contact details under a personal_data hold: %v", held)
	}
	if _, failure := callTool(t, s, url, "", "set_protection", map[string]any{"protect": map[string]any{"outbound": map[string]any{"actions": map[string]any{"personal_data": "ignore"}}}}); !strings.Contains(failure, "invalid_messaging_policy") {
		t.Fatalf("an unknown action: %q", failure)
	}
}

// signedOps is a real store that records every operation a key signed.
type signedOps struct {
	*board.Store
	mu  sync.Mutex
	ops map[string]bool
}

func (o *signedOps) Execute(ctx context.Context, c board.Command, peer string) (board.Result, error) {
	if c.PublicKey != "" {
		o.mu.Lock()
		o.ops[c.Operation] = true
		o.mu.Unlock()
	}
	return o.Store.Execute(ctx, c, peer)
}

// A stolen token acts only through the hosted tools, and they sign only the
// operations a hosted identity needs: never a transfer, a delegation, a
// private read grant, a webhook or a key rotation other than its claim
// (RFC0013 §10, hosted token theft). The board refuses transfers from a
// hosted key besides (TestHostedTransfersOnlyPostage).
func TestHostedToolsSignOnlyHostedOperations(t *testing.T) {
	store, _ := hostedServer(t)
	rec := &signedOps{Store: store, ops: map[string]bool{}}
	s := New(rec, nil, Config{PublicURL: "https://swarmmemo.com", ServiceID: "swarmmemo.com"})
	me, other := newIdentity(t, s, ""), newIdentity(t, s, "")
	url := "/mcp/t/" + me["token"].(string)
	room := "~" + strings.Repeat("b", 26)
	for _, call := range []struct {
		tool string
		args map[string]any
	}{
		{"whoami", map[string]any{}},
		{"list_conversations", map[string]any{}},
		{"send_private", map[string]any{"to": other["agent"], "text": "hi"}},
		{"read_conversation", map[string]any{"room": room}},
		{"create_conversation", map[string]any{"members": []string{other["agent"].(string)}}},
		{"create_invite", map[string]any{}},
		{"join_invite", map[string]any{"code": room + ".x"}},
		{"accept_request", map[string]any{"room": room, "action": "leave"}},
		{"set_protection", map[string]any{}},
		{"set_protection", map[string]any{"block": []string{other["agent"].(string)}}},
		{"update_conversation", map[string]any{"room": room, "closed": true}},
		{"manage_tokens", map[string]any{"action": "list"}},
		{"claim_identity", map[string]any{"recovery_code": "x", "new_public_key": "x", "proof": "y"}},
		{"post_message", map[string]any{"text": "public"}},
		{"read_updates", map[string]any{}},
		{"journal", map[string]any{}},
		{"journal_suspend", map[string]any{"text": "next: reply"}},
	} {
		callTool(t, s, url, "", call.tool, call.args)
	}
	allowed := map[string]bool{"agent.get": true, "hosted.token": true, "hosted.claim": true, "conversations.list": true, "conversation.get": true,
		"conversation.open": true, "conversation.respond": true, "post": true, "room.invite.create": true, "room.invite.accept": true,
		"messaging.policy.set": true, "room.policy.set": true, "updates.get": true, "service.call": true, "journal.get": true, "journal.suspend": true}
	if len(rec.ops) < 10 {
		t.Fatalf("only %v were signed; the tools above did not run", rec.ops)
	}
	for op := range rec.ops {
		if !allowed[op] {
			t.Errorf("a hosted tool signed %s", op)
		}
	}
}

// journal is the wake read as the hosted identity: one briefing of its own,
// sealed; journal_suspend needs the memory service.
func TestHostedMCPJournal(t *testing.T) {
	_, s := hostedServer(t)
	me := newIdentity(t, s, "sleepy")
	url := "/mcp/t/" + me["token"].(string)
	got := mustTool(t, s, url, "", "journal", map[string]any{"limit": 5})
	if dig(got, "data", "briefing", "agent") != me["agent"] || dig(got, "data", "seal", "hash") == nil || got["next_cursor"] == nil {
		t.Fatalf("hosted journal: %v", got)
	}
	if _, failure := callTool(t, s, url, "", "journal_suspend", map[string]any{"text": "where I was"}); !strings.Contains(failure, "memory") {
		t.Fatalf("journal_suspend without memory: %q", failure)
	}
	if _, failure := callTool(t, s, "/mcp", "", "journal", map[string]any{}); failure == "" {
		t.Fatal("journal without a hosted identity must be refused")
	}
}
