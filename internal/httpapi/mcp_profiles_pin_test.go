package httpapi

// The /mcp and /mcp/assistant tool lists (names, titles, descriptions,
// annotations, security schemes) pinned against a golden file, so a change
// meant for another profile (the core profile, C99) cannot reach them
// unnoticed. A deliberate change to these profiles regenerates it:
//
//	UPDATE_MCP_GOLDEN=1 go test ./internal/httpapi/ -run TestMCPProfilesPinned

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/services"
	"swarmmemo/internal/web"
)

const mcpProfilesGolden = "testdata/mcp_profiles.golden.json"

// pinServer is a real store with hosted identities and sign-in on, serving
// every known service with the ledger and trust on: the widest tool lists.
func pinServer(t *testing.T) *Server {
	t.Helper()
	return pinServerWith(t, false)
}

// pinServerWith is pinServer, serving the web pages too when pages is set.
func pinServerWith(t *testing.T, pages bool) *Server {
	t.Helper()
	dir := t.TempDir()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	kek := filepath.Join(dir, "hosted-kek")
	if err := os.WriteFile(kek, []byte(base64.RawURLEncoding.EncodeToString(key)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := board.Open(filepath.Join(dir, "board.sqlite"), board.Config{ServiceID: "swarmmemo.com", HostedKEKFile: kek})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	f := board.Features{Services: services.Known(), Ledger: board.LedgerOn, Trust: board.TrustAllocation}
	var handler http.Handler
	if pages {
		handler = web.Handler(store)
	}
	return New(store, handler, Config{Features: f, PublicURL: "https://swarmmemo.com", ServiceID: "swarmmemo.com"})
}

// listedTools is a profile's tools/list answer, each tool less its schemas.
func profileToolList(t *testing.T, s *Server, path string) []map[string]any {
	t.Helper()
	out := mcpRequest(t, s, path, "", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	raw, _ := dig(out, "result", "tools").([]any)
	if len(raw) == 0 {
		t.Fatalf("%s lists no tools: %v", path, out)
	}
	var tools []map[string]any
	for _, r := range raw {
		tool := r.(map[string]any)
		delete(tool, "inputSchema")
		delete(tool, "outputSchema")
		tools = append(tools, tool)
	}
	return tools
}

func TestMCPProfilesPinned(t *testing.T) {
	s := pinServer(t)
	got := map[string][]map[string]any{}
	for _, path := range []string{"/mcp", web.AssistantMCPPath} {
		got[path] = profileToolList(t, s, path)
	}
	raw, err := json.MarshalIndent(got, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	if os.Getenv("UPDATE_MCP_GOLDEN") != "" {
		if err := os.MkdirAll(filepath.Dir(mcpProfilesGolden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(mcpProfilesGolden, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(mcpProfilesGolden)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != string(raw) {
		var w map[string][]map[string]any
		_ = json.Unmarshal(want, &w)
		for path, tools := range got {
			index := map[string]string{}
			for _, tool := range w[path] {
				b, _ := json.Marshal(tool)
				index[tool["name"].(string)] = string(b)
			}
			if len(tools) != len(w[path]) {
				t.Errorf("%s lists %d tools, the golden %d", path, len(tools), len(w[path]))
			}
			for _, tool := range tools {
				b, _ := json.Marshal(tool)
				if index[tool["name"].(string)] != string(b) {
					t.Errorf("%s %s changed:\n got  %s\n want %s", path, tool["name"], b, index[tool["name"].(string)])
				}
			}
		}
		t.Fatalf("the /mcp or %s tool list differs from %s (UPDATE_MCP_GOLDEN=1 regenerates it, for a deliberate change only)", web.AssistantMCPPath, mcpProfilesGolden)
	}
}
