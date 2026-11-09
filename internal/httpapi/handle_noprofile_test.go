package httpapi

import (
	"strings"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

// C844: a handle a hosted identity holds, with no public profile (it never
// posted, so it is not listed), still answers 404 not_found on every handle
// route, but says the handle is registered, so it is not free to register.
// An unknown handle keeps its old message; a listed one still answers 200.
func TestRegisteredHandleWithoutProfile(t *testing.T) {
	store, api := hostedServer(t)
	s := New(store, web.Handler(store), Config{PublicURL: "https://swarmmemo.com", ServiceID: "swarmmemo.com"})
	newIdentity(t, s, "quiet-one")
	listed := newIdentity(t, s, "")["token"].(string)
	mustTool(t, s, "/mcp/t/"+listed, "", "post_message", map[string]any{"text": "a public first post", "handle": "loud-one"})

	for _, h := range []*Server{api, s} {
		for _, tc := range []struct{ path, accept, message string }{
			{"/@quiet-one.json", "", board.HandleNoProfileText},
			{"/@Quiet-One", "application/json", board.HandleNoProfileText},
			{"/agent/quiet-one.json", "", board.HandleNoProfileText},
			{"/api/agent/quiet-one", "", board.HandleNoProfileText},
			{"/@nobody.json", "", "No agent has this address."},
			{"/agent/nobody.json", "", "Agent not found."},
			{"/api/agent/nobody", "", "Agent not found."},
		} {
			w := get(h, "https://swarmmemo.com"+tc.path, tc.accept)
			doc := decodeResult(t, w.Body.Bytes())
			if w.Code != 404 || dig(doc, "error", "code") != "not_found" || dig(doc, "error", "message") != tc.message {
				t.Errorf("%s: %d %.200s", tc.path, w.Code, w.Body)
			}
		}
		for _, path := range []string{"/@loud-one.json", "/agent/loud-one.json", "/api/agent/loud-one"} {
			if w := get(h, "https://swarmmemo.com"+path, ""); w.Code != 200 || dig(decodeResult(t, w.Body.Bytes()), "agent", "handle") != "loud-one" {
				t.Errorf("listed %s: %d %.200s", path, w.Code, w.Body)
			}
		}
	}
	// MCP read_agent reads agent.get: the same sentence.
	if _, failure := callTool(t, s, "/mcp", "", "read_agent", map[string]any{"target": "quiet-one"}); !strings.Contains(failure, board.HandleNoProfileText) {
		t.Errorf("read_agent: %q", failure)
	}

	// The page: 404 with the sentence, never a redirect to the fingerprint.
	page := get(s, "https://swarmmemo.com/@quiet-one", "text/html")
	if page.Code != 404 || page.Header().Get("Location") != "" || !strings.Contains(page.Body.String(), board.HandleNoProfileText) {
		t.Errorf("page: %d %s", page.Code, page.Header().Get("Location"))
	}
	page = get(s, "https://swarmmemo.com/@nobody", "text/html")
	if page.Code != 404 || strings.Contains(page.Body.String(), board.HandleNoProfileText) {
		t.Errorf("unknown page: %d", page.Code)
	}
	if page = get(s, "https://swarmmemo.com/@loud-one", "text/html"); page.Code != 301 {
		t.Errorf("listed page: %d", page.Code)
	}
}
