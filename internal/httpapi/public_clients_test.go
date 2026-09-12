package httpapi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalMCPDiscoveryIsOptionalAndNonMutating(t *testing.T) {
	f := &fakeService{}
	s := New(f, nil, Config{})
	w := makeRequest(s, "GET", "/capabilities", "", "")
	var capabilities map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &capabilities); err != nil {
		t.Fatal(err)
	}
	local, ok := capabilities["local_mcp"].(map[string]any)
	if !ok || local["optional"] != true || local["transport"] != "stdio" || local["platform"] != "Linux" || local["default_mode"] != "draft" || local["public_room_only"] != true || local["automatic_execution"] != false || local["hosted_key_custody"] != false {
		t.Fatal("local MCP discovery lost its authority boundaries", local)
	}
	for _, field := range []string{"instructions", "operator_setup"} {
		path, ok := local[field].(string)
		if !ok || makeRequest(s, "GET", path, "", "").Code != 200 {
			t.Fatal("local MCP instructions unavailable", field)
		}
	}
	// The adapter's authority boundary is stated where the adapter is documented;
	// first-contact instructions only have to route an agent to that document.
	for _, path := range []string{"/protocol.md", "/clients/mcp/README.md"} {
		body := makeRequest(s, "GET", path, "", "").Body.String()
		for _, want := range []string{"scoped-send", "draft", "MCP"} {
			if !strings.Contains(body, want) {
				t.Fatal("missing local MCP boundary", path, want)
			}
		}
	}
	for _, path := range []string{"/llms.txt", "/skill.md"} {
		if !strings.Contains(makeRequest(s, "GET", path, "", "").Body.String(), local["instructions"].(string)) {
			t.Fatal("instructions must route to the local MCP boundary", path)
		}
	}
	if len(f.commands) != 0 {
		t.Fatal("local MCP discovery executed a service operation")
	}
}

func TestPrivateInboxDiscoveryKeepsClientAuthorityExplicit(t *testing.T) {
	service := &fakeService{}
	s := New(service, nil, Config{})
	w := makeRequest(s, "GET", "/capabilities", "", "")
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	inbox, ok := result["private_inbox"].(map[string]any)
	if !ok || inbox["optional_client"] != true || inbox["storage"] != "metadata-only" || inbox["offline_bodies"] != false || inbox["mcp"] != false || inbox["e2ee"] != false {
		t.Fatal("private client scope became misleading", inbox)
	}
	for _, boundary := range []string{"schema1 ordinary member", "schema2 room-specific read-only grant", "no automatic migration"} {
		if !strings.Contains(inbox["reader_key"].(string), boundary) {
			t.Fatal("explicit separate reader authority omitted", boundary)
		}
	}
	guide := makeRequest(s, "GET", inbox["instructions"].(string), "", "")
	if guide.Code != 200 || !strings.Contains(guide.Body.String(), "not a read-only grant") || !strings.Contains(guide.Body.String(), "No stale/offline body override") {
		t.Fatal("private operator guide unavailable or weakened")
	}
	if len(service.commands) != 0 {
		t.Fatal("private inbox discovery executed service operations")
	}
}

func TestPublicWorkGuideDiscoveryIsReadOnly(t *testing.T) {
	f := &fakeService{}
	s := New(f, nil, Config{})
	var capabilities map[string]any
	if err := json.Unmarshal(makeRequest(s, "GET", "/capabilities", "", "").Body.Bytes(), &capabilities); err != nil {
		t.Fatal(err)
	}
	work := capabilities["work_coordination"].(map[string]any)
	path, ok := work["instructions"].(string)
	if !ok || path != "/clients/python/FIRST_PUBLIC_WORK.md" || work["paid"] != false || work["automatic_execution"] != false {
		t.Fatal("public work instructions or boundaries missing", work)
	}
	if makeRequest(s, "GET", path, "", "").Code != 200 {
		t.Fatal("public work guide unavailable")
	}
	for _, discovery := range []string{"/llms.txt", "/skill.md"} {
		body := makeRequest(s, "GET", discovery, "", "").Body.String()
		if !strings.Contains(body, path) || !strings.Contains(body, "never authorizes external execution") {
			t.Fatal("terminal work guide or execution boundary missing", discovery)
		}
	}
	if len(f.commands) != 0 {
		t.Fatal("guide discovery executed a service command")
	}
}

func TestExactPublicClientDownloads(t *testing.T) {
	for _, name := range []string{"python/swarmmemo.py", "python/swarmmemo_outbox.py", "python/swarmmemo_inbox.py", "python/swarmmemo_private_inbox.py", "python/swarmmemo_private_transport.py", "python/PRIVATE_INBOX.md", "python/FIRST_PUBLIC_WORK.md", "python/README.md", "python/signing-vector.json", "javascript/swarmmemo.mjs", "javascript/README.md", "mcp/README.md", "mcp/BOOTSTRAP.md"} {
		expected, err := os.ReadFile(filepath.Join("..", "..", "clients", name))
		if err != nil {
			t.Fatal(err)
		}
		for _, method := range []string{"GET", "HEAD", "POST", "PUT"} {
			f := &fakeService{}
			w := makeRequest(New(f, nil, Config{}), method, "/clients/"+name, "", "")
			if len(f.commands) != 0 {
				t.Fatal("source download executed a command")
			}
			if method == "POST" || method == "PUT" {
				if w.Code != 405 {
					t.Fatal("source download accepted a write")
				}
				continue
			}
			if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") || w.Header().Get("Content-Disposition") != `attachment; filename="`+filepath.Base(name)+`"` {
				t.Fatalf("bad download headers: %v", w.Header())
			}
			if method == "GET" && w.Body.String() != string(expected) {
				t.Fatal("download differs from reviewed embedded client")
			}
			if method == "HEAD" && w.Body.Len() != 0 {
				t.Fatal("HEAD returned source bytes")
			}
		}
	}
	for _, path := range []string{"/clients/", "/clients/python/", "/clients/python/key.json", "/clients/python/.env", "/clients/mcp/worker.py", "/clients/mcp/profile.json", "/clients/mcp/.venv/pyvenv.cfg", "/clients/public.go", "/ops/coordination_lab.py", "/clients/python/../../ACTIONS.md"} {
		w := makeRequest(New(&fakeService{}, nil, Config{}), "GET", path, "", "")
		if w.Code != 404 && w.Code != 400 {
			t.Fatalf("unreviewed source path exposed: %s (%d)", path, w.Code)
		}
	}
}
