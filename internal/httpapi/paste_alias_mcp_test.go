package httpapi

// The deprecated paste.* aliases (C54): while docs runs, their MCP tools are
// left out of tools/list, the server card and /capabilities, yet a client
// that knows one still calls it, /call/paste/ still answers and /tools/paste
// is still a page. With docs off they are the only way to paste, so they
// are listed.

import (
	"encoding/json"
	"strings"
	"testing"

	"swarmmemo/internal/web"
)

func listedToolNames(t *testing.T, s *Server, path string) []string {
	t.Helper()
	out := mcpRequest(t, s, path, "", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	tools, _ := dig(out, "result", "tools").([]any)
	if len(tools) == 0 {
		t.Fatalf("tools/list on %s: %v", path, out)
	}
	var names []string
	for _, tool := range tools {
		name, _ := dig(tool, "name").(string)
		names = append(names, name)
	}
	return names
}

func pasteTools(names []string) []string {
	var out []string
	for _, n := range names {
		if strings.HasPrefix(n, "paste_") {
			out = append(out, n)
		}
	}
	return out
}

func TestPasteAliasesHiddenFromToolListings(t *testing.T) {
	s, _ := hostedServicesServer(t, "paste", "docs")
	me := newIdentity(t, s, "")
	hosted := "/mcp/t/" + me["token"].(string)
	for _, path := range []string{"/mcp", hosted, "/mcp/assistant", "/mcp/assistant/t/" + me["token"].(string)} {
		names := listedToolNames(t, s, path)
		if p := pasteTools(names); len(p) > 0 {
			t.Errorf("tools/list on %s lists the deprecated aliases %v", path, p)
		}
		if path == hosted && !strings.Contains(strings.Join(names, " "), "docs_create") {
			t.Errorf("tools/list on %s lacks docs_create: %v", path, names)
		}
	}
	for _, t2 := range s.mcpToolList() {
		if strings.HasPrefix(t2.Name, "paste_") {
			t.Errorf("mcpToolList lists %s", t2.Name)
		}
	}
	for path, lists := range map[string][][]string{
		"/.well-known/mcp/server-card.json": {{"tools"}},
		"/capabilities":                     {{"personal_assistants", "tools"}, {"conversations", "hosted", "tools"}},
	} {
		w := makeRequest(s, "GET", "https://swarmmemo.com"+path, "", "")
		var body map[string]any
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil {
			t.Fatalf("%s: %d", path, w.Code)
		}
		for _, at := range lists {
			listed, _ := json.Marshal(dig(body, at...))
			if len(listed) < 10 || strings.Contains(string(listed), `"paste_`) {
				t.Errorf("%s %v lists a paste_ tool, or none: %s", path, at, listed)
			}
		}
	}

	// The aliases still work: the hidden tools are called, and the wire
	// aliases and the landing page answer.
	created := mustTool(t, s, hosted, "", "paste_create", map[string]any{"text": "still pasting", "visibility": "unlisted"})
	id, _ := dig(created, "data", "result", "paste", "id").(string)
	if id == "" {
		t.Fatalf("paste_create: %v", created)
	}
	if got := mustTool(t, s, hosted, "", "paste_get", map[string]any{"id": id}); dig(got, "data", "result", "text") != "still pasting" {
		t.Fatalf("paste_get: %v", got)
	}
	w := makeRequest(s, "GET", "https://swarmmemo.com/call/paste/open?id="+id, "", "")
	var opened map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &opened)
	if w.Code != 200 || dig(opened, "data", "result", "paste", "id") != id {
		t.Fatalf("/call/paste/open: %d %s", w.Code, w.Body.String())
	}
	if !web.ToolServed(s.cfg.Features, "/tools/paste") {
		t.Fatal("/tools/paste is not served")
	}

	// Without docs the paste tools are the only ones, and are listed.
	alone, _ := hostedServicesServer(t, "paste")
	solo := newIdentity(t, alone, "")
	if p := pasteTools(listedToolNames(t, alone, "/mcp/t/"+solo["token"].(string))); len(p) == 0 {
		t.Fatal("with docs off, the paste tools are not listed")
	}
}
