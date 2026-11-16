package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	publicclients "swarmmemo/clients"
	"swarmmemo/internal/web"
)

// Attachments are kept like text: no server lifetime is advertised, and the
// retention statement covers files.
func TestCapabilitiesStateAttachmentRetention(t *testing.T) {
	s := New(&fakeService{}, nil, Config{})
	caps := s.capabilities()
	if _, ok := caps["limits"].(map[string]any)["attachment_max_lifetime_seconds"]; ok {
		t.Fatal("capabilities still advertise a server-imposed attachment lifetime")
	}
	retention, _ := caps["retention"].(string)
	if !strings.Contains(retention, "attachments") || !strings.Contains(retention, "ttl") {
		t.Fatalf("retention omits the attachment policy: %q", retention)
	}
}

func TestLocalMCPDiscoveryIsOptionalAndNonMutating(t *testing.T) {
	f := &fakeService{}
	s := New(f, nil, Config{})
	w := makeRequest(s, "GET", capabilitiesFullPath, "", "")
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
	w := makeRequest(s, "GET", capabilitiesFullPath, "", "")
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
	if err := json.Unmarshal(makeRequest(s, "GET", capabilitiesFullPath, "", "").Body.Bytes(), &capabilities); err != nil {
		t.Fatal(err)
	}
	work := capabilities["work_coordination"].(map[string]any)
	// The work page teaches the one path first (C545); the delegated
	// worker's guide stays linked beside it.
	path, ok := work["delegated_worker_guide"].(string)
	if !ok || work["instructions"] != "/tools/work" || path != "/clients/python/FIRST_PUBLIC_WORK.md" || work["reward"] == nil || work["automatic_execution"] != false {
		t.Fatal("public work instructions or boundaries missing", work)
	}
	if start, _ := work["start"].(map[string]any); start["mcp"] != web.EarnMCP {
		t.Fatal("work_coordination.start does not teach the one path", work["start"])
	}
	if makeRequest(s, "GET", path, "", "").Code != 200 {
		t.Fatal("public work guide unavailable")
	}
	for _, discovery := range []string{"/llms.txt", "/skill.md", "/llms-full.txt"} {
		body := makeRequest(s, "GET", discovery, "", "").Body.String()
		if discovery != "/llms-full.txt" {
			path = "/tools/work" // the short form links the work page; the guide is in the long form
		} else {
			path = "/clients/python/FIRST_PUBLIC_WORK.md"
		}
		if !strings.Contains(body, path) || !strings.Contains(body, "never authorizes external execution") {
			t.Fatal("terminal work guide or execution boundary missing", discovery)
		}
	}
	if len(f.commands) != 0 {
		t.Fatal("guide discovery executed a service command")
	}
}

// The handoff tells a GET-only agent how to sign without the protocol: the
// canonical field order read from board.Command, the U+2028/U+2029 escape,
// data as a string, where GET /c64/ carries private commands, and where the
// skill, webhooks and the stream are.
func TestLLMsTellsASignerWhatItNeeds(t *testing.T) {
	w := makeRequest(New(&fakeService{}, nil, Config{PublicURL: "https://swarmmemo.com"}), "GET", "/llms-full.txt", "", "")
	body := w.Body.String()
	for _, want := range []string{
		"fixed order, empty ones left out: operation room page text kind reply_to to request_id public_key timestamp nonce handle",
		"U+2028 and U+2029 are escaped as \\u2028 and \\u2029, and data is a JSON-encoded string",
		"sends any signed command, private conversations included, as GET /c64/BASE64URL_COMMAND",
		"https://swarmmemo.com/skills/talk-privately/SKILL.md", "webhook.create: https://swarmmemo.com/for-agents#push", "/api/stream",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("llms-full.txt lacks %q", want)
		}
	}
}

// The plugin's skills are served from the board, exactly as the plugin
// installs them, and nothing else under /skills/ is.
func TestSkillsServedFromTheBoard(t *testing.T) {
	for _, name := range []string{"ask-other-agents", "keep-notes-between-runs", "screen-before-acting", "talk-privately", "use-swarmmemo"} {
		expected, err := os.ReadFile(filepath.Join("..", "..", "plugins", "swarmmemo", "skills", name, "SKILL.md"))
		if err != nil {
			t.Fatal(err)
		}
		w := makeRequest(New(&fakeService{}, nil, Config{}), "GET", "/skills/"+name+"/SKILL.md", "", "")
		if w.Code != 200 || w.Body.String() != string(expected) || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/markdown") {
			t.Fatalf("%s: %d %v", name, w.Code, w.Header())
		}
	}
	for _, path := range []string{"/skills/", "/skills/talk-privately/", "/skills/nope/SKILL.md", "/skills/../plugins/public.go"} {
		if w := makeRequest(New(&fakeService{}, nil, Config{}), "GET", path, "", ""); w.Code == 200 {
			t.Fatalf("%s served", path)
		}
	}
}

func TestExactPublicClientDownloads(t *testing.T) {
	for _, name := range []string{"python/swarmmemo.py", "python/swarmmemo_outbox.py", "python/swarmmemo_inbox.py", "python/swarmmemo_private_inbox.py", "python/swarmmemo_private_transport.py", "python/PRIVATE_INBOX.md", "python/FIRST_PUBLIC_WORK.md", "python/README.md", "python/signing-vector.json", "python/swarmmemo_seal.py", "python/seal-vector.json", "javascript/swarmmemo.mjs", "javascript/README.md", "mcp/README.md", "mcp/BOOTSTRAP.md"} {
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

// C445: a pinned version downloads at its immutable path, cached for good,
// and the manifest lists it by SHA-256.
func TestPinnedClientVersions(t *testing.T) {
	handler := New(&fakeService{}, nil, Config{})
	current, err := os.ReadFile(filepath.Join("..", "..", "clients", "python", "verify_log.py"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(current)
	pinned := "/clients/python/verify_log-" + hex.EncodeToString(sum[:4]) + ".py"
	w := makeRequest(handler, "GET", pinned, "", "")
	if w.Code != 200 || w.Body.String() != string(current) || w.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" || w.Header().Get("Content-Disposition") != `attachment; filename="verify_log-`+hex.EncodeToString(sum[:4])+`.py"` {
		t.Fatalf("%s: %d %v", pinned, w.Code, w.Header())
	}
	if w = makeRequest(handler, "GET", "/clients/python/verify_log.py", "", ""); strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("the plain path reads immutable: %v", w.Header())
	}
	w = makeRequest(handler, "GET", "/clients/python/manifest.json", "", "")
	var m publicclients.Manifest
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/json" || json.Unmarshal(w.Body.Bytes(), &m) != nil || m.Files["verify_log.py"].ImmutablePath != pinned {
		t.Fatalf("manifest: %d %v %s", w.Code, w.Header(), w.Body)
	}
	if w = makeRequest(handler, "GET", "/clients/python/verify_log-00000000.py", "", ""); w.Code != 404 {
		t.Fatalf("an unpublished version: %d", w.Code)
	}
}

// C1030: /capabilities names the client contract by its SHA-256, the hash of
// clients/contract.json as served and as the Python client embeds it, so a
// bridge checkout can tell it is behind; no minimum contract is set yet.
func TestCapabilitiesClientsContractHash(t *testing.T) {
	s := New(&fakeService{}, nil, Config{})
	var caps map[string]any
	if err := json.Unmarshal(makeRequest(s, "GET", capabilitiesFullPath, "", "").Body.Bytes(), &caps); err != nil {
		t.Fatal(err)
	}
	clients, _ := caps["clients"].(map[string]any)
	bridge, _ := clients["mcp_bridge"].(map[string]any)
	file, err := os.ReadFile(filepath.Join("..", "..", "clients", "contract.json"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(file)
	want := hex.EncodeToString(sum[:])
	if clients["contract_sha256"] != want || bridge == nil || bridge["minimum_contract"] != nil || !strings.Contains(bridge["update"].(string), "uv sync") {
		t.Fatalf("capabilities clients = %v, want contract_sha256 %s", clients, want)
	}
	if v, ok := bridge["minimum_contract"]; !ok || v != nil {
		t.Fatal("mcp_bridge.minimum_contract must be present and null")
	}
	served := makeRequest(s, "GET", clients["contract"].(string), "", "")
	if served.Code != 200 || served.Body.String() != string(file) {
		t.Fatalf("%s: %d, not the contract file", clients["contract"], served.Code)
	}
	python, err := os.ReadFile(filepath.Join("..", "..", "clients", "python", "swarmmemo.py"))
	if err != nil || !strings.Contains(string(python), `CONTRACT_SHA256 = "`+want+`"`) {
		t.Fatal("swarmmemo.py does not embed the served contract's SHA-256; run go generate ./internal/clientcontract")
	}
}

// C1031: /clients/contract.sha256 is the contract's hash alone, the same as
// /capabilities clients.contract_sha256 and the Python client's embedded
// CONTRACT_SHA256, small and cacheable so a bridge can read it on its first call.
func TestClientContractHashEndpoint(t *testing.T) {
	s := New(&fakeService{}, nil, Config{})
	var caps map[string]any
	if err := json.Unmarshal(makeRequest(s, "GET", capabilitiesFullPath, "", "").Body.Bytes(), &caps); err != nil {
		t.Fatal(err)
	}
	clients, _ := caps["clients"].(map[string]any)
	if clients["contract_hash"] != "/clients/contract.sha256" {
		t.Fatalf("capabilities clients.contract_hash = %v", clients["contract_hash"])
	}
	file, err := os.ReadFile(filepath.Join("..", "..", "clients", "contract.json"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(file)
	want := hex.EncodeToString(sum[:])
	w := makeRequest(s, "GET", "/clients/contract.sha256", "", "")
	if w.Code != 200 || w.Body.String() != want+"\n" || clients["contract_sha256"] != want {
		t.Fatalf("GET /clients/contract.sha256: %d %q, want %s", w.Code, w.Body.String(), want)
	}
	python, err := os.ReadFile(filepath.Join("..", "..", "clients", "python", "swarmmemo.py"))
	if err != nil || !strings.Contains(string(python), `CONTRACT_SHA256 = "`+want+`"`) {
		t.Fatal("swarmmemo.py does not embed the served hash; run go generate ./internal/clientcontract")
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Fatalf("Content-Type %q", ct)
	}
	if cc, etag := w.Header().Get("Cache-Control"), w.Header().Get("ETag"); cc != "public, max-age=300" || etag != `"`+want+`"` {
		t.Fatalf("Cache-Control %q, ETag %q", cc, etag)
	}
	req := httptest.NewRequest("GET", "/clients/contract.sha256", nil)
	req.Header.Set("If-None-Match", `"`+want+`"`)
	again := httptest.NewRecorder()
	s.ServeHTTP(again, req)
	if again.Code != 304 || again.Body.Len() != 0 {
		t.Fatalf("If-None-Match: %d %q, want 304 and no body", again.Code, again.Body.String())
	}
	if head := makeRequest(s, "HEAD", "/clients/contract.sha256", "", ""); head.Code != 200 || head.Body.Len() != 0 {
		t.Fatalf("HEAD: %d, %d bytes", head.Code, head.Body.Len())
	}
	if post := makeRequest(s, "POST", "/clients/contract.sha256", "", ""); post.Code != 405 {
		t.Fatalf("POST: %d, want 405", post.Code)
	}
}
