package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"swarmmemo/internal/board"
)

type fakeService struct {
	mu       sync.Mutex
	commands []board.Command
	peers    []string
	err      error
}

func (f *fakeService) Execute(_ context.Context, c board.Command, peer string) (board.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, c)
	f.peers = append(f.peers, peer)
	if f.err != nil {
		return board.Result{}, f.err
	}
	if c.Operation == "post" {
		return board.Result{OK: true, Receipt: &board.Receipt{ID: "memo123", Hash: "hash123"}}, nil
	}
	return board.Result{OK: true, Messages: []board.Message{}, Rooms: []board.Room{}}, nil
}
func (f *fakeService) Moderate(context.Context, string, string, bool) error { return nil }
func (f *fakeService) Close() error                                         { return nil }
func makeRequest(s http.Handler, method, path, body, ct string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = "198.51.100.8:12345"
	if ct != "" {
		r.Header.Set("Content-Type", ct)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
func TestWriteAdapters(t *testing.T) {
	for _, tc := range []struct{ name, method, path, body, ct string }{
		{"query", "GET", "/w/lobby/main?text=hello%20world", "", ""},
		{"base64", "GET", "/w64/lobby/main/" + base64.RawURLEncoding.EncodeToString([]byte("hello world")), "", ""},
		{"raw", "POST", "/w/lobby/main", "hello world", "text/plain"},
		{"form", "POST", "/w/lobby/main", "text=hello+world&room=lobby&page=main", "application/x-www-form-urlencoded"},
		{"json", "POST", "/w/lobby/main", `{"text":"hello world"}`, "application/json"},
		{"put", "PUT", "/v1/events/id123", `{"room":"lobby","page":"main","text":"hello world"}`, "application/json"},
		{"mkcol", "MKCOL", "/w/lobby/main", "hello world", "text/plain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeService{}
			s := New(f, nil, Config{})
			w := makeRequest(s, tc.method, tc.path, tc.body, tc.ct)
			if w.Code != 200 {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			if len(f.commands) != 1 || f.commands[0].Text != "hello world" || f.commands[0].Operation != "post" {
				t.Fatalf("commands: %+v", f.commands)
			}
			if tc.name == "put" && f.commands[0].RequestID != "id123" {
				t.Fatal("lost idempotency key")
			}
		})
	}
}
func TestInvalidWritesHaveNoSideEffects(t *testing.T) {
	for _, tc := range []struct{ method, path, body, ct string }{
		{"HEAD", "/w/lobby/main?text=hello", "", ""},
		{"OPTIONS", "/w/lobby/main?text=hello", "", ""},
		{"DELETE", "/w/lobby/main?text=hello", "", ""},
		{"GET", "/w/lobby/main?text=a&text=b", "", ""},
		{"GET", "/w/lobby/main?text=a&room=other", "", ""},
		{"GET", "/w/lobby/main?text=a&unknown=b", "", ""},
		{"GET", "/w64/lobby/main/aGVsbG8=", "", ""},
		{"GET", "/w64/lobby/main/_w", "", ""},
		{"POST", "/w/lobby/main?text=hello", "other", "text/plain"},
		{"POST", "/w/lobby/main", `{"text":"hello","room":"other"}`, "application/json"},
		{"POST", "/w/lobby/main", `{"text":"hello","unknown":true}`, "application/json"},
		{"POST", "/w/lobby/main", `{"text":"hello"}{}`, "application/json"},
		{"PUT", "/v1/events/id123?request_id=other", "hello", "text/plain"},
		{"GET", "/w/lobby/ma%5Cin?text=hi", "", ""},
		{"POST", "/v1/command", `{"operation":"admin.grant"}`, "application/json"},
	} {
		t.Run(tc.method+tc.path+tc.body, func(t *testing.T) {
			f := &fakeService{}
			s := New(f, nil, Config{})
			w := makeRequest(s, tc.method, tc.path, tc.body, tc.ct)
			if len(f.commands) != 0 {
				t.Fatalf("side effect: %+v", f.commands)
			}
			if tc.method != "OPTIONS" && w.Code < 400 {
				t.Fatalf("unexpected success %d", w.Code)
			}
		})
	}
}
func TestHeaderAdapter(t *testing.T) {
	f := &fakeService{}
	s := New(f, nil, Config{})
	r := httptest.NewRequest("GET", "/w/lobby/main", nil)
	r.Header.Set("X-Text", "hello")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 || len(f.commands) != 1 || f.commands[0].Text != "hello" {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestProxyHeadersOnlyTrustedFromLoopback(t *testing.T) {
	s := New(&fakeService{}, nil, Config{TrustLoopbackProxy: true})
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "198.51.100.5:44"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	r.Header.Set("X-Forwarded-Proto", "https")
	if s.peer(r) != "198.51.100.5" || s.secure(r) {
		t.Fatal("trusted external spoof")
	}
	r.RemoteAddr = "127.0.0.1:44"
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 198.51.100.6")
	if s.peer(r) != "198.51.100.6" || !s.secure(r) {
		t.Fatal("proxy not resolved correctly")
	}
}
func TestSignedPrivateOperationsRequireTLS(t *testing.T) {
	f := &fakeService{}
	s := New(f, nil, Config{})
	w := makeRequest(s, "POST", "http://swarmmemo.com/v1/command", `{"operation":"messages.list","room":"private","public_key":"key"}`, "application/json")
	if w.Code != 400 || len(f.commands) != 0 {
		t.Fatal(w.Code, w.Body.String(), f.commands)
	}
}
func TestDiscoveryAndMCP(t *testing.T) {
	f := &fakeService{}
	s := New(f, nil, Config{})
	for _, path := range []string{"/capabilities", "/llms.txt", "/skill.md", "/robots.txt", "/sitemap.xml", "/openapi.json", "/feed.atom", "/feed.json", "/exports"} {
		w := makeRequest(s, "GET", path, "", "")
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	r := httptest.NewRequest("POST", "https://swarmmemo.com/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "post_message") {
		t.Fatal(w.Code, w.Body.String())
	}
	r = httptest.NewRequest("POST", "https://swarmmemo.com/mcp", strings.NewReader(`{}`))
	r.Header.Set("Origin", "https://evil.example")
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("MCP accepted invalid Origin", w.Code)
	}
}
func TestCommandJSONResult(t *testing.T) {
	f := &fakeService{}
	w := makeRequest(New(f, nil, Config{}), "POST", "/v1/command", `{"operation":"post","text":"hello"}`, "application/json")
	var result board.Result
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || !result.OK || result.Receipt == nil {
		t.Fatal(w.Body.String(), err)
	}
}
func TestMultipartPostIsRefusedNotStoredRaw(t *testing.T) {
	f := &fakeService{}
	s := New(f, nil, Config{})
	body := "--b\r\nContent-Disposition: form-data; name=\"text\"\r\n\r\nhello\r\n--b--\r\n"
	w := makeRequest(s, "POST", "/w/lobby/main", body, "multipart/form-data; boundary=b")
	if w.Code != 415 || !strings.Contains(w.Body.String(), "unsupported_media_type") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if len(f.commands) != 0 {
		t.Fatalf("multipart body reached the board: %+v", f.commands)
	}
}

// An anonymous receipt carries one piece of advice beside it (C113): where
// its replies are read, and that signing brings them to /api/updates. The
// same line on every transport; nothing that existed before moves.
func TestAnonymousReceiptAdvisesSigning(t *testing.T) {
	const how = "https://swarmmemo.com/for-agents#scheduled"
	const line = "Replies to this post reach no inbox: read them at https://swarmmemo.com/e/memo123, or sign your posts (python3 swarmmemo.py keygen; signed GET /c64/... for GET-only tools) to get replies in /api/updates."
	s := New(&fakeService{}, nil, Config{})
	for _, tc := range []struct{ name, method, path, body, ct string }{
		{"command", "POST", "/v1/command", `{"operation":"post","text":"hello"}`, "application/json"},
		{"get json", "GET", "/w/lobby/main?text=hello&format=json", "", ""},
	} {
		w := makeRequest(s, tc.method, tc.path, tc.body, tc.ct)
		var result board.Result
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 || result.Receipt == nil || result.Receipt.ID != "memo123" {
			t.Fatalf("%s: %d %s %v", tc.name, w.Code, w.Body.String(), err)
		}
		if result.Next == nil || result.Next.How != how || result.Next.SignToGetReplies != line {
			t.Fatalf("%s: no signing advice: %s", tc.name, w.Body.String())
		}
	}
	w := makeRequest(s, "GET", "/w/lobby/main?text=hello", "", "")
	lines := strings.Split(strings.TrimSuffix(w.Body.String(), "\n"), "\n")
	if w.Code != 200 || len(lines) != 2 || lines[0] != "ok memo123 sha256=hash123 url=/e/memo123 duplicate=false" {
		t.Fatalf("text receipt: %d %q", w.Code, w.Body.String())
	}
	if lines[1] != line {
		t.Fatalf("text advice: %q", lines[1])
	}

	r := httptest.NewRequest("POST", "https://swarmmemo.com/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"post_message","arguments":{"text":"hello"}}}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	mw := httptest.NewRecorder()
	s.ServeHTTP(mw, r)
	var rpc struct {
		Result struct {
			StructuredContent board.Result `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(mw.Body.Bytes(), &rpc); err != nil || rpc.Result.StructuredContent.Next == nil || rpc.Result.StructuredContent.Next.How != how || rpc.Result.StructuredContent.Next.SignToGetReplies != line || rpc.Result.StructuredContent.SharedReceipt == nil || rpc.Result.StructuredContent.SharedReceipt.Acceptance.ID != "memo123" {
		t.Fatalf("MCP post: %d %s %v", mw.Code, mw.Body.String(), err)
	}
}

func TestSignedReceiptHasNoSigningAdvice(t *testing.T) {
	s := New(&fakeService{}, nil, Config{})
	w := makeRequest(s, "POST", "/v1/command", `{"operation":"post","text":"hello","public_key":"key","signature":"sig"}`, "application/json")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"receipt"`) || strings.Contains(w.Body.String(), `"next"`) {
		t.Fatalf("signed JSON: %d %s", w.Code, w.Body.String())
	}
	w = makeRequest(s, "GET", "/w/lobby/main?text=hello&public_key=key&signature=sig", "", "")
	if w.Code != 200 || w.Body.String() != "ok memo123 sha256=hash123 url=/e/memo123 duplicate=false\n" {
		t.Fatalf("signed text: %d %q", w.Code, w.Body.String())
	}
	// Reads never carry it either.
	w = makeRequest(s, "GET", "/api/messages?limit=1", "", "")
	if strings.Contains(w.Body.String(), `"next"`) {
		t.Fatalf("read carried advice: %s", w.Body.String())
	}
}

func TestRobotsPublicAPIAndWriteExclusions(t *testing.T) {
	s := New(&fakeService{}, nil, Config{PublicURL: "https://swarmmemo.com"})
	w := makeRequest(s, "GET", "/robots.txt", "", "")
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	// User-agent groups replace the wildcard group; within a group the
	// longest matching path wins, with Allow winning ties.
	type rule struct {
		path  string
		allow bool
	}
	groups := map[string][]rule{}
	for _, block := range strings.Split(w.Body.String(), "\n\n") {
		var agents []string
		var rules []rule
		for _, line := range strings.Split(block, "\n") {
			key, value, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			value = strings.TrimSpace(value)
			switch key {
			case "User-agent":
				agents = append(agents, strings.ToLower(value))
			case "Allow", "Disallow":
				rules = append(rules, rule{value, key == "Allow"})
			}
		}
		for _, agent := range agents {
			groups[agent] = rules
		}
	}
	if len(groups["*"]) == 0 {
		t.Fatal("missing wildcard rules")
	}
	allowed := func(agent, path string) bool {
		rules, ok := groups[strings.ToLower(agent)]
		if !ok {
			rules = groups["*"]
		}
		longest, allow := -1, true
		for _, rule := range rules {
			if strings.HasPrefix(path, rule.path) && (len(rule.path) > longest || len(rule.path) == longest && rule.allow) {
				longest, allow = len(rule.path), rule.allow
			}
		}
		return allow
	}
	agents := []string{"Claude-User", "ChatGPT-User", "Perplexity-User", "meta-externalfetcher", "GenericAgent", "Googlebot"}
	for agent := range groups {
		agents = append(agents, agent)
	}
	for _, agent := range agents {
		t.Run(agent, func(t *testing.T) {
			for _, path := range []string{"/api/messages?limit=10", "/api/agents", "/api/updates", "/api/stats", "/api/trust/runs/1", "/api/params/allowance", "/llms.txt", "/docs"} {
				if !allowed(agent, path) {
					t.Errorf("public read disallowed: %s", path)
				}
			}
			for _, path := range []string{"/call/public_data/fetch", "/v1/command", "/w/lobby/main", "/w64/payload", "/c64/payload", "/admin/status", "/mcp", "/mcp/assistant", "/metrics", "/a/attachment", "/me"} {
				if allowed(agent, path) {
					t.Errorf("private/write path allowed: %s", path)
				}
			}
			_, crawler := groups[strings.ToLower(agent)]
			crawler = crawler && agent != "*"
			for _, path := range []string{"/references", "/api/references", "/api/references/example"} {
				if allowed(agent, path) == crawler {
					t.Errorf("reference crawler policy incorrect: %s", path)
				}
			}
		})
	}
	if !strings.Contains(w.Body.String(), "Sitemap: https://swarmmemo.com/sitemap.xml") {
		t.Fatal("missing sitemap")
	}
}
