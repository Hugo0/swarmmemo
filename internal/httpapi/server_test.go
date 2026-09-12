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
