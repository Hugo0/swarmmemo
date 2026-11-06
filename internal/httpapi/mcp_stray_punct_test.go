package httpapi

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

// A connector URL copied out of a sentence ("add …/mcp/assistant: no sign-in
// is needed") keeps its punctuation; the board sends it to the profile path
// with a 308, so the client's POST and body survive. Other paths are left alone.
func TestMCPPathWithStrayPunctRedirects(t *testing.T) {
	store, err := board.Open(filepath.Join(t.TempDir(), "punct.db"), board.Config{ServiceID: "swarmmemo.com"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s := New(store, web.Handler(store), Config{ServiceID: "swarmmemo.com", PublicURL: "https://swarmmemo.com"})
	for path, want := range map[string]string{
		web.AssistantMCPPath + ":":    web.AssistantMCPPath,
		web.AssistantMCPPath + ").":   web.AssistantMCPPath,
		"/mcp,":                       "/mcp",
		mcpProfileCore + ";?client=x": mcpProfileCore + "?client=x",
		"/mcp/":                       "/mcp", // C768: a trailing slash is the same slip
		mcpProfileCore + "/":          mcpProfileCore,
		web.AssistantMCPPath + "/":    web.AssistantMCPPath,
		"/mcp/.":                      "/mcp",
	} {
		r := httptest.NewRequest("POST", "https://swarmmemo.com"+path, strings.NewReader("{}"))
		r.RemoteAddr = "198.51.100.8:12345"
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 308 || w.Header().Get("Location") != want {
			t.Errorf("POST %s = %d Location %q, want 308 %q", path, w.Code, w.Header().Get("Location"), want)
		}
	}
	for _, path := range []string{"/mcpx:", "/mcp/t/tok:", "/mcp/t/tok/", "/r/lobby:", "/r/lobby/", "/"} {
		if _, ok := mcpPathWithStrayPunct(path); ok {
			t.Errorf("%s was treated as an MCP path with stray punctuation", path)
		}
	}
}

// C768: each route's 405 lists the methods that route serves (RFC 9110
// 15.5.6), from the list it gates on, never one fixed Allow for every route.
func TestMethodNotAllowedListsTheRoutesMethods(t *testing.T) {
	store, err := board.Open(filepath.Join(t.TempDir(), "allow.db"), board.Config{ServiceID: "swarmmemo.com"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s := New(store, web.Handler(store), Config{ServiceID: "swarmmemo.com", PublicURL: "https://swarmmemo.com"})
	for _, c := range []struct{ method, path, allow string }{
		{"POST", "/health", "GET, HEAD"},
		{"PUT", "/health", "GET, HEAD"},
		{"POST", "/capabilities", "GET, HEAD"},
		{"DELETE", "/time", "GET, HEAD"},
		{"POST", "/llms.txt", "GET, HEAD"},
		{"PUT", "/.well-known/agent-card.json", "GET, HEAD, POST"},
		{"GET", "/v1/command", "POST"},
		{"DELETE", "/w/lobby/main", "GET, POST, PUT, MKCOL"},
		{"POST", "/v1/events/req-1", "PUT"},
		{"TRACE", "/health", "GET, POST, PUT, MKCOL, OPTIONS"},
	} {
		r := httptest.NewRequest(c.method, "https://swarmmemo.com"+c.path, strings.NewReader("{}"))
		r.RemoteAddr = "198.51.100.9:12345"
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 405 || w.Header().Get("Allow") != c.allow {
			t.Errorf("%s %s = %d Allow %q, want 405 %q", c.method, c.path, w.Code, w.Header().Get("Allow"), c.allow)
		}
	}
}
