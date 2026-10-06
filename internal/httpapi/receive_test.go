package httpapi

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/board"
	"swarmmemo/internal/ledger"
	"swarmmemo/internal/services"
	"swarmmemo/internal/services/servicestest"
	"swarmmemo/internal/web"
)

// receiverServer is a real store with receivers and wake-ups on, hosted
// identities on, and the fake ledger.
func receiverServer(t *testing.T) (*board.Store, *Server) {
	t.Helper()
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	kek := filepath.Join(t.TempDir(), "hosted-kek")
	if err := os.WriteFile(kek, []byte(base64.RawURLEncoding.EncodeToString(key)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	features := board.Features{Services: []string{"receiver", "wakeup"}}
	store, err := board.Open(filepath.Join(t.TempDir(), "board.sqlite"), board.Config{ServiceID: "swarmmemo.com", Features: features, HostedKEKFile: kek})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	store.UseServiceMeter(servicestest.NewMeter(1<<30), &servicestest.Params{})
	return store, New(store, nil, Config{Features: features, PublicURL: "https://swarmmemo.com", ServiceID: "swarmmemo.com"})
}

func postTo(h *Server, url, body, contentType string) *httptest.ResponseRecorder {
	return makeRequest(h, "POST", url, body, contentType)
}

func TestReceiveRouteEndToEnd(t *testing.T) {
	_, h := receiverServer(t)
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	digest := sha256.Sum256(key.Public().(ed25519.PublicKey))
	agent := hex.EncodeToString(digest[:])
	command := func(c board.Command) map[string]any {
		t.Helper()
		body, _ := json.Marshal(signService(key, c))
		w := makeRequest(h, "POST", "https://swarmmemo.com/v1/command", string(body), "application/json")
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", c.Operation, w.Code, w.Body.String())
		}
		return decodeResult(t, w.Body.Bytes())
	}
	createCmd := signService(key, board.Command{Operation: "service.call", Target: "receiver", Data: `{"schema":1,"method":"create","args":{"label":"jobs","screen":false},"max_cost":5}`, RequestID: "recv-create"})
	send := func(c board.Command) map[string]any {
		t.Helper()
		body, _ := json.Marshal(c)
		w := makeRequest(h, "POST", "https://swarmmemo.com/v1/command", string(body), "application/json")
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", c.Operation, w.Code, w.Body.String())
		}
		return decodeResult(t, w.Body.Bytes())
	}
	created := send(createCmd)
	url, _ := dig(created, "data", "result", "url").(string)
	if !strings.HasPrefix(url, "https://swarmmemo.com/in/") {
		t.Fatalf("create: %+v", created)
	}
	// An exact retry returns the receipt without the URL: it was shown once.
	again := send(createCmd)
	if dig(again, "data", "result", "url") != nil || dig(again, "data", "result", "receiver", "id") != dig(created, "data", "result", "receiver", "id") {
		t.Fatalf("a retry never shows the URL again: %+v", again)
	}

	if w := makeRequest(h, "GET", url, "", ""); w.Code != 405 || !strings.Contains(w.Body.String(), "POST only") {
		t.Fatalf("GET a receive URL: %d %s", w.Code, w.Body.String())
	}
	w := postTo(h, url, `{"status":"done","note":"<script>alert(1)</script>"}`, "application/json")
	if w.Code != 202 || !strings.Contains(w.Body.String(), `"item":`) || strings.Contains(w.Body.String(), agent) {
		t.Fatalf("deliver: %d %s", w.Code, w.Body.String())
	}
	if w = postTo(h, url, "x="+strings.Repeat("y", services.ReceiverBodyBytes), "application/x-www-form-urlencoded"); w.Code != 413 || !strings.Contains(w.Body.String(), "receiver_too_large") {
		t.Fatalf("too large: %d %s", w.Code, w.Body.String())
	}
	if w = postTo(h, url, "\x89PNG", "image/png"); w.Code != 415 {
		t.Fatalf("binary: %d %s", w.Code, w.Body.String())
	}
	if w = postTo(h, url[:len(url)-1]+"A", `{}`, "application/json"); w.Code != 404 || !strings.Contains(w.Body.String(), "receiver_not_found") {
		t.Fatalf("a wrong secret: %d %s", w.Code, w.Body.String())
	}
	if w = postTo(h, "https://swarmmemo.com/in/nothing", `{}`, "application/json"); w.Code != 404 {
		t.Fatalf("a malformed URL: %d", w.Code)
	}

	// The owner's own signed updates.get carries data.received; anyone
	// else's read of the agent's updates does not.
	own := command(board.Command{Operation: "updates.get", Target: agent})
	received, _ := dig(own, "data", "received").([]any)
	if len(received) != 1 || dig(received[0], "content_type") != "application/json" || dig(received[0], "body") != nil {
		t.Fatalf("own updates: %+v", own["data"])
	}
	public := decodeResult(t, makeRequest(h, "GET", "https://swarmmemo.com/api/updates?agent="+agent, "", "").Body.Bytes())
	if _, ok := dig(public, "data").(map[string]any)["received"]; ok {
		t.Fatalf("public updates show received items: %+v", public["data"])
	}
	read := command(board.Command{Operation: "service.read", Target: "receiver", Data: `{"schema":1,"method":"items","args":{}}`})
	list, _ := dig(read, "data", "result", "items").([]any)
	if len(list) != 1 || dig(list[0], "body") != `{"status":"done","note":"<script>alert(1)</script>"}` || dig(list[0], "untrusted") != true {
		t.Fatalf("items: %+v", read)
	}
	// An unsigned read of the items is refused.
	if w = makeRequest(h, "GET", "https://swarmmemo.com/c64/"+base64.RawURLEncoding.EncodeToString([]byte(`{"operation":"service.read","target":"receiver","data":"{\"schema\":1,\"method\":\"items\"}"}`)), "", ""); w.Code != 401 {
		t.Fatalf("unsigned items: %d %s", w.Code, w.Body.String())
	}
}

// A hosted identity has the receiver tools; they sign as it.
func TestReceiverHostedTools(t *testing.T) {
	_, h := receiverServer(t)
	tools := listTools(t, h, "/mcp")
	for _, name := range []string{"receiver_create", "receiver_rotate", "receiver_delete", "receiver_list", "receiver_items"} {
		if tools[name].Name == "" {
			t.Fatalf("%s is not listed", name)
		}
	}
	if tools["receiver_items"].Annotations["readOnlyHint"] != true || tools["receiver_create"].Annotations["readOnlyHint"] == true {
		t.Fatalf("annotations: %+v %+v", tools["receiver_items"].Annotations, tools["receiver_create"].Annotations)
	}
	if _, failure := callTool(t, h, "/mcp", "", "receiver_create", map[string]any{}); !strings.Contains(failure, "hosted") {
		t.Fatalf("without an identity: %q", failure)
	}
	data := newIdentity(t, h, "")
	token := data["token"].(string)
	// The assistant profile is an allowlist a directory reviewed: receivers
	// are not on it.
	if assistant := listTools(t, h, web.AssistantMCPPath); assistant["receiver_create"].Name != "" {
		t.Fatal("the assistant profile must not grow receiver tools by itself")
	}
	created := mustTool(t, h, "/mcp/t/"+token, "", "receiver_create", map[string]any{"label": "callbacks"})
	url, _ := dig(created, "data", "result", "url").(string)
	if !strings.HasPrefix(url, "https://swarmmemo.com/in/") {
		t.Fatalf("receiver_create: %+v", created)
	}
	if w := postTo(h, url, "result: 42", "text/plain"); w.Code != 202 {
		t.Fatalf("deliver: %d %s", w.Code, w.Body.String())
	}
	got := mustTool(t, h, "/mcp/t/"+token, "", "receiver_items", map[string]any{})
	if list, _ := dig(got, "data", "result", "items").([]any); len(list) != 1 || dig(list[0], "body") != "result: 42" {
		t.Fatalf("receiver_items: %+v", got)
	}
}

// Fetch over /call/ without a key, as /tools/fetch shows it: the board's own
// hosts are refused before anything is reserved or requested, in words.
func TestFetchCallRoute(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "fetch.json")
	if err := os.WriteFile(cfg, []byte(`{"schema":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	features := board.Features{Services: []string{"fetch"}, FetchConfig: cfg, Ledger: board.LedgerOn, AnonPrefix: true}
	store, err := board.Open(filepath.Join(t.TempDir(), "board.sqlite"), board.Config{ServiceID: "swarmmemo.com", Features: features})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	p := ledger.DefaultAllowanceParams()
	rp := p.Resources[allowance.Credit]
	rp.Budget, rp.SpendCeiling, rp.InboundCap = 1_600_000, 1_600_000, 1_600_000
	rp.Cap = []int64{400_000, 200_000, 100_000, 2000}
	rp.Floor = []int64{1600, 1600, 1600, 2000}
	rp.RootCap = []int64{1_600_000, 800_000, 400_000, 2000}
	rp.ShareMaxPPM = []int64{1_000_000, 1_000_000, 1_000_000, 100_000}
	if _, err = store.SetAllowanceParams(t.Context(), ledger.AllowanceNamespace, p.Marshal(), "test", 0); err != nil {
		t.Fatal(err)
	}
	h := New(store, nil, Config{Features: features, PublicURL: "https://swarmmemo.com", ServiceID: "swarmmemo.com", AllowInsecureLocal: true})
	w := makeRequest(h, "GET", "https://swarmmemo.com/call/fetch/page?url=https://swarmmemo.com/w/lobby?text=hi", "", "")
	if w.Code != 403 || !strings.Contains(w.Body.String(), `"fetch_denied"`) || !strings.Contains(w.Body.String(), "Nothing was charged") {
		t.Fatalf("own host: %d %s", w.Code, w.Body.String())
	}
	w = makeRequest(h, "POST", "https://swarmmemo.com/call/fetch/page", "url=http%3A%2F%2F10.0.0.1%2F", "application/x-www-form-urlencoded")
	if w.Code != 400 || !strings.Contains(w.Body.String(), `"fetch_address_blocked"`) {
		t.Fatalf("private address: %d %s", w.Code, w.Body.String())
	}
	if w = makeRequest(h, "GET", "https://swarmmemo.com/call/fetch/page?url=https://example.com/&max_bytes=65536", "", ""); w.Code != 400 {
		t.Fatalf("more than 8 KiB without a key: %d %s", w.Code, w.Body.String())
	}
	tools := listTools(t, h, "/mcp")
	if tools["fetch_page"].Name == "" {
		t.Fatal("fetch_page is not an MCP tool")
	}
}
