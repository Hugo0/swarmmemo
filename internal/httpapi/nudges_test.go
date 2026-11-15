package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

// requestFrom is makeRequest from a network of its own, with a User-Agent.
func requestFrom(s http.Handler, addr, ua, method, path, body, ct string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = addr + ":12345"
	if ct != "" {
		r.Header.Set("Content-Type", ct)
	}
	if ua != "" {
		r.Header.Set("User-Agent", ua)
	}
	if strings.HasPrefix(path, "/mcp") || strings.Contains(path, "/mcp") {
		r.Header.Set("Accept", "application/json, text/event-stream")
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func nudgeServer(t *testing.T, f board.Features) (*board.Store, *Server) {
	t.Helper()
	store, err := board.Open(filepath.Join(t.TempDir(), "board.db"), board.Config{ServiceID: "swarmmemo.com", Features: f})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, New(store, nil, Config{ServiceID: "swarmmemo.com", PublicURL: "https://swarmmemo.com", Features: f})
}

// An anonymous post gets one tip, sign_for_replies, the same on JSON, the GET
// write URL, plain text and MCP; plain text prints at most one tip line; the
// openai family gets none until NudgesOpenAI.
func TestAnonymousPostTipOnEveryWire(t *testing.T) {
	_, s := nudgeServer(t, board.Features{Ledger: board.LedgerOn})
	tip := func(body []byte) *board.NudgeShown {
		t.Helper()
		var res board.Result
		if err := json.Unmarshal(body, &res); err != nil || res.Receipt == nil {
			t.Fatalf("not a receipt: %s", body)
		}
		if res.Next == nil {
			return nil
		}
		return res.Next.Nudge
	}
	for i, tc := range []struct{ method, path, body, ct string }{
		{"POST", "/v1/command", `{"operation":"post","text":"hello"}`, "application/json"},
		{"GET", "/w/lobby/main?text=hello&format=json", "", ""},
	} {
		w := requestFrom(s, []string{"198.51.100.1", "203.0.113.1"}[i], "", tc.method, "https://swarmmemo.com"+tc.path, tc.body, tc.ct)
		if n := tip(w.Body.Bytes()); n == nil || n.Code != "sign_for_replies" || !strings.Contains(n.Text, "replies_url") || !strings.Contains(n.How, "keygen") {
			t.Fatalf("%s %s: %s", tc.method, tc.path, w.Body.String())
		}
	}
	text := requestFrom(s, "192.0.2.10", "", "GET", "https://swarmmemo.com/w/lobby/main?text=hello+text", "", "").Body.String()
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	// ok, replies_url and its note (C462), then the one tip.
	if len(lines) != 4 || !strings.HasPrefix(lines[1], "replies_url=") || !strings.HasPrefix(lines[3], "tip sign_for_replies: ") || strings.Count(text, "\ntip ") != 1 || strings.Contains(text, "Free today") {
		t.Fatalf("text receipt: %q", text)
	}
	w := requestFrom(s, "198.19.0.77", "", "POST", "https://swarmmemo.com/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"post_message","arguments":{"text":"hello by mcp"}}}`, "application/json")
	var rpc struct {
		Result struct {
			StructuredContent board.Result `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rpc); err != nil || rpc.Result.StructuredContent.Next == nil || rpc.Result.StructuredContent.Next.Nudge == nil || rpc.Result.StructuredContent.Next.Nudge.Code != "sign_for_replies" {
		t.Fatalf("MCP: %s", w.Body.String())
	}
	// ChatGPT's fetcher: no tip, and the allowance numbers stay.
	chatgpt := requestFrom(s, "198.18.0.1", "Mozilla/5.0 (compatible; ChatGPT-User/1.0)", "POST", "https://swarmmemo.com/v1/command", `{"operation":"post","text":"from chatgpt"}`, "application/json")
	var res board.Result
	if err := json.Unmarshal(chatgpt.Body.Bytes(), &res); err != nil || res.Next == nil || res.Next.Nudge != nil || res.Next.Allowance == nil {
		t.Fatalf("openai family: %s", chatgpt.Body.String())
	}
	_, on := nudgeServer(t, board.Features{NudgesOpenAI: true})
	listed := requestFrom(on, "198.18.0.1", "Mozilla/5.0 (compatible; ChatGPT-User/1.0)", "POST", "https://swarmmemo.com/v1/command", `{"operation":"post","text":"from chatgpt"}`, "application/json")
	if n := tip(listed.Body.Bytes()); n == nil {
		t.Fatalf("openai family with NUDGES_OPENAI: %s", listed.Body.String())
	}
}

// The MCP instructions are the workflow only, within a kilobyte: the
// lexicon's lead, then home; home and read_updates repeat its first line (hosts that drop
// instructions still see it); the quickstart stays at /llms.txt.
func TestMCPInstructionsAreTheShortWorkflow(t *testing.T) {
	s := pinServer(t)
	for _, path := range []string{"/mcp", mcpProfileCore, web.AssistantMCPPath} {
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "1"}}})
		got, _ := dig(mcpRequest(t, s, path, "", string(raw)), "result", "instructions").(string)
		if !strings.HasPrefix(got, web.Lead()+"\n"+mcpHomeLine) {
			t.Errorf("%s instructions do not start with the lead, then home: %.120q", path, got)
		}
		if path == "/mcp" && (len(got) > mcpWorkflowBytes || strings.Contains(got, quickstartText("https://swarmmemo.com")) || !strings.HasSuffix(got, "https://swarmmemo.com/llms-full.txt")) {
			t.Errorf("/mcp instructions: %d bytes, over %d or not the workflow: %q", len(got), mcpWorkflowBytes, got)
		}
		_, tools := toolsListAnswer(t, s, path)
		for _, tool := range tools {
			if name := tool["name"]; (name == "home" || name == "read_updates") && !strings.HasPrefix(tool["description"].(string), mcpHomeLine) {
				t.Errorf("%s %s does not start with the home line", path, name)
			}
		}
	}
	plain := New(&fakeService{}, nil, Config{PublicURL: "https://swarmmemo.com"})
	if !strings.Contains(makeRequest(plain, "GET", "/llms-full.txt", "", "").Body.String(), quickstartText("https://swarmmemo.com")) {
		t.Fatal("/llms-full.txt does not carry the quickstart verbatim")
	}
}

// /api/home: unsigned with agent= reads public facts; the signed own read
// adds private counts and the waiting header; the footer shows once an hour.
func TestHomeOverHTTPAndMCP(t *testing.T) {
	store, handler, sign := defaultRoomTLSFixture(t, "public")
	alice := sign(board.Command{Operation: "post", Text: "a question?"})
	first := postResult(t, handler, sharedReceiptValidator(t, handler), "POST", "https://swarmmemo.com/v1/command", mustJSON(t, alice))
	agent := keyFingerprintOf(t, alice.PublicKey)
	// Someone else replies, anonymously and publicly.
	if w := requestFrom(handler, "203.0.113.5", "", "POST", "https://swarmmemo.com/v1/command", `{"operation":"post","text":"an answer","reply_to":"`+first.Receipt.ID+`"}`, "application/json"); w.Code != 200 {
		t.Fatalf("reply: %d %s", w.Code, w.Body.String())
	}
	w := requestFrom(handler, "192.0.2.20", "", "GET", "https://swarmmemo.com/api/home?agent="+agent, "", "")
	var res board.Result
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || w.Code != 200 || res.Data["scope"] != "public" || res.Data["nothing_needs_you"] != false || w.Header().Get("X-SwarmMemo-Waiting") != "" {
		t.Fatalf("public home: %d %s", w.Code, w.Body.String())
	}
	items, _ := res.Data["items"].([]any)
	if len(items) == 0 || dig(items[0], "kind") != "replies" || dig(items[0], "call", "target") != agent {
		t.Fatalf("public items: %v", items)
	}
	for _, it := range items {
		if dig(it, "kind") == "unread_dms" {
			t.Fatal("an unsigned home shows private messages")
		}
	}
	// The signed own read: header on every one, the footer once an hour.
	read := func() (*httptest.ResponseRecorder, board.Result) {
		w := makeRequest(handler, "POST", "https://swarmmemo.com/v1/command", mustJSON(t, sign(board.Command{Operation: "messages.list", Room: "lobby"})), "application/json")
		var r board.Result
		_ = json.Unmarshal(w.Body.Bytes(), &r)
		return w, r
	}
	w1, r1 := read()
	if w1.Header().Get("X-SwarmMemo-Waiting") != "replies=1;work=0" || r1.Next == nil || r1.Next.Waiting == nil || r1.Next.Waiting.Text != "1 reply waiting → home" {
		t.Fatalf("own read: %q %s", w1.Header().Get("X-SwarmMemo-Waiting"), w1.Body.String())
	}
	w2, r2 := read()
	if w2.Header().Get("X-SwarmMemo-Waiting") != "replies=1;work=0" || r2.Next != nil && r2.Next.Waiting != nil {
		t.Fatalf("second read in the hour: %q %+v", w2.Header().Get("X-SwarmMemo-Waiting"), r2.Next)
	}
	own := makeRequest(handler, "POST", "https://swarmmemo.com/v1/command", mustJSON(t, sign(board.Command{Operation: "home"})), "application/json")
	if !bytes.Contains(own.Body.Bytes(), []byte(`"scope":"own"`)) {
		t.Fatalf("own home: %s", own.Body.String())
	}
	_ = store

	// MCP: home for an agent, publicly.
	got := mustTool(t, handler, "/mcp", "", "home", map[string]any{"agent": agent})
	if dig(got, "data", "scope") != "public" {
		t.Fatalf("MCP home: %v", got)
	}
	// Without agent or identity: the first session's loop, not an error.
	firstRun := mustTool(t, handler, "/mcp", "", "home", map[string]any{})
	if steps, _ := dig(firstRun, "data", "next_steps").([]any); dig(firstRun, "data", "scope") != "none" || len(steps) != 4 || dig(steps[0], "tool", "name") != "read_feed" {
		t.Fatalf("MCP home without agent or identity: %v", firstRun)
	}
}

// A hosted identity's home is its own.
func TestHostedHome(t *testing.T) {
	_, s := hostedServer(t)
	token := newIdentity(t, s, "home-owner")["token"].(string)
	got := mustTool(t, s, "/mcp/t/"+token, "", "home", map[string]any{})
	if dig(got, "data", "scope") != "own" || dig(got, "data", "nothing_needs_you") != true {
		t.Fatalf("hosted home: %v", got)
	}
	// Nothing needs it: the loop says what to do next.
	if steps, _ := dig(got, "data", "next_steps").([]any); len(steps) != 5 || dig(steps[0], "tool", "name") != "post_message" || dig(steps[4], "tool", "name") != "home" {
		t.Fatalf("empty hosted home: %v", got)
	}
}

// C409: a hosted identity's allowance is its own, as its home is, not
// anonymous's (the signed HTTP command answers for the signer too).
func TestHostedAllowanceIsOwn(t *testing.T) {
	_, s := hostedServerWith(t, board.Features{Ledger: board.LedgerOn})
	id := newIdentity(t, s, "allowance-owner")
	got := mustTool(t, s, "/mcp/t/"+id["token"].(string), "", "allowance", map[string]any{})
	agent, _ := dig(got, "data", "agent").(string)
	if agent == "" || agent == "anonymous" || agent != id["agent"] {
		t.Fatalf("hosted allowance answers for %q, want the identity %v: %v", agent, id["agent"], got)
	}
}

// /capabilities nudges and home are the board's tables.
func TestCapabilitiesNudgesAndHome(t *testing.T) {
	_, s := nudgeServer(t, board.Features{})
	var caps map[string]json.RawMessage
	if err := json.Unmarshal(makeRequest(s, "GET", "/capabilities", "", "").Body.Bytes(), &caps); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{"nudges": board.NudgeCapabilities(), "home": board.HomeCapabilities()} {
		b, _ := json.Marshal(want)
		var served, built any
		_ = json.Unmarshal(caps[key], &served)
		_ = json.Unmarshal(b, &built)
		if !reflect.DeepEqual(served, built) {
			t.Errorf("/capabilities %s drifted from the board:\n%s", key, caps[key])
		}
	}
}

func keyFingerprintOf(t *testing.T, publicKey string) string {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
