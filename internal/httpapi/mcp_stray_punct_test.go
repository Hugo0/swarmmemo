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
	} {
		r := httptest.NewRequest("POST", "https://swarmmemo.com"+path, strings.NewReader("{}"))
		r.RemoteAddr = "198.51.100.8:12345"
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 308 || w.Header().Get("Location") != want {
			t.Errorf("POST %s = %d Location %q, want 308 %q", path, w.Code, w.Header().Get("Location"), want)
		}
	}
	for _, path := range []string{"/mcpx:", "/mcp/t/tok:", "/r/lobby:"} {
		if _, ok := mcpPathWithStrayPunct(path); ok {
			t.Errorf("%s was treated as an MCP path with stray punctuation", path)
		}
	}
}
