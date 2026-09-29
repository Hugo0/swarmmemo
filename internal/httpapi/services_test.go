package httpapi

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"swarmmemo/internal/board"
	"swarmmemo/internal/services/servicestest"
)

func TestServiceRoutesDeclineWhileOff(t *testing.T) {
	f := &fakeService{}
	h := New(f, nil, Config{})
	for _, path := range []string{"/api/services", "/api/memory/" + strings.Repeat("a", 64) + "/k"} {
		makeRequest(h, "GET", "https://swarmmemo.com"+path, "", "")
	}
	for _, c := range f.commands {
		if c.Operation == "services.list" || c.Operation == "service.read" {
			t.Fatalf("a service route answered while SERVICES is empty: %+v", c)
		}
	}
	w := makeRequest(h, "GET", "https://swarmmemo.com/capabilities", "", "")
	var caps map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &caps); err != nil {
		t.Fatal(err)
	}
	if _, ok := caps["services"]; ok {
		t.Fatal("/capabilities must omit services while SERVICES is empty")
	}
}

func TestServiceRoutesTranslateToReads(t *testing.T) {
	agent := strings.Repeat("a", 64)
	f := &fakeService{}
	h := New(f, nil, Config{Features: board.Features{Services: []string{"echo", "memory"}}})
	if w := makeRequest(h, "GET", "https://swarmmemo.com/api/services", "", ""); w.Code != 200 || f.commands[0].Operation != "services.list" {
		t.Fatalf("catalogue route: %d %+v", w.Code, f.commands)
	}
	if w := makeRequest(h, "GET", "https://swarmmemo.com/api/memory/"+agent+"/notes/today", "", ""); w.Code != 200 {
		t.Fatalf("memory route: %d", w.Code)
	}
	c := f.commands[1]
	if c.Operation != "service.read" || c.Target != "memory" || c.PublicKey != "" || c.Data != `{"args":{"agent":"`+agent+`","key":"notes/today"},"method":"get","schema":1}` {
		t.Fatalf("memory route command: %+v", c)
	}
	for _, bad := range []struct{ method, path string }{
		{"POST", "/api/services"}, {"GET", "/api/services?x=1"}, {"GET", "/api/memory/" + agent}, {"GET", "/api/memory/" + agent + "/"}, {"PUT", "/api/memory/" + agent + "/k"},
	} {
		if w := makeRequest(h, bad.method, "https://swarmmemo.com"+bad.path, "", ""); w.Code < 400 {
			t.Errorf("%s %s: %d", bad.method, bad.path, w.Code)
		}
	}
	if len(f.commands) != 2 {
		t.Fatalf("refused requests reached the store: %+v", f.commands[2:])
	}
	caps := New(f, nil, Config{Features: board.Features{Services: []string{"echo"}}})
	before := len(f.commands)
	makeRequest(caps, "GET", "https://swarmmemo.com/api/memory/"+agent+"/k", "", "")
	for _, c := range f.commands[before:] {
		if c.Operation == "service.read" {
			t.Fatal("the memory route must decline while memory is not enabled")
		}
	}
}

func signService(key ed25519.PrivateKey, c board.Command) board.Command {
	c.PublicKey = base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
	c.Timestamp = time.Now().Unix()
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	c.Nonce = hex.EncodeToString(nonce)
	c.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, board.Canonical("swarmmemo.com", c)))
	return c
}

func memoryData(method string, args map[string]string, maxCost int64) string {
	d := map[string]any{"schema": 1, "method": method, "args": args}
	if maxCost >= 0 {
		d["max_cost"] = maxCost
	}
	raw, _ := json.Marshal(d)
	return string(raw)
}

func decodeResult(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return out
}

func dig(v any, path ...string) any {
	for _, p := range path {
		m, _ := v.(map[string]any)
		v = m[p]
	}
	return v
}

// TestMemoryOverEveryWire stores memory over /v1/command and /c64, reads it
// over GET, /v1/command and MCP, and finds one ledger receipt per write.
func TestMemoryOverEveryWire(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "board.sqlite")
	store, err := board.Open(dbPath, board.Config{ServiceID: "swarmmemo.com", Features: board.Features{Services: []string{"echo", "memory"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	meter := servicestest.NewMeter(1 << 30)
	store.UseServiceMeter(meter, &servicestest.Params{})
	cfg := Config{Features: board.Features{Services: []string{"echo", "memory"}}, PublicURL: "https://swarmmemo.com", ServiceID: "swarmmemo.com"}
	h := New(store, nil, cfg)
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	sum := key.Public().(ed25519.PublicKey)
	digest := sha256.Sum256(sum)
	agent := hex.EncodeToString(digest[:])

	// /v1/command, JSON.
	put := signService(key, board.Command{Operation: "service.call", Target: "memory", Data: memoryData("put", map[string]string{"key": "run/state", "value": "step 3"}, 300), RequestID: "wire-json"})
	body, _ := json.Marshal(put)
	w := makeRequest(h, "POST", "https://swarmmemo.com/v1/command", string(body), "application/json")
	res := decodeResult(t, w.Body.Bytes())
	if w.Code != 200 || dig(res, "data", "call", "state") != "done" || dig(res, "data", "receipt", "used") != float64(256+9+6) {
		t.Fatalf("/v1/command put: %d %s", w.Code, w.Body.String())
	}
	// /c64, GET.
	pub := signService(key, board.Command{Operation: "service.call", Target: "memory", Data: memoryData("put", map[string]string{"key": "profile", "value": "hello", "visibility": "public"}, 300)})
	raw, _ := json.Marshal(pub)
	w = makeRequest(h, "GET", "https://swarmmemo.com/c64/"+base64.RawURLEncoding.EncodeToString(raw), "", "")
	if w.Code != 200 || dig(decodeResult(t, w.Body.Bytes()), "data", "call", "state") != "done" {
		t.Fatalf("/c64 put: %d %s", w.Code, w.Body.String())
	}
	// Public GET read; the private item does not exist to it.
	w = makeRequest(h, "GET", "https://swarmmemo.com/api/memory/"+agent+"/profile", "", "")
	if w.Code != 200 || dig(decodeResult(t, w.Body.Bytes()), "data", "result", "value") != "hello" {
		t.Fatalf("public GET: %d %s", w.Code, w.Body.String())
	}
	w = makeRequest(h, "GET", "https://swarmmemo.com/api/memory/"+agent+"/run/state", "", "")
	if w.Code != 404 || !strings.Contains(w.Body.String(), "memory_not_found") {
		t.Fatalf("a private item over GET: %d %s", w.Code, w.Body.String())
	}
	// The owner's signed read over /v1/command.
	get := signService(key, board.Command{Operation: "service.read", Target: "memory", Data: memoryData("get", map[string]string{"key": "run/state"}, -1)})
	body, _ = json.Marshal(get)
	w = makeRequest(h, "POST", "https://swarmmemo.com/v1/command", string(body), "application/json")
	if w.Code != 200 || dig(decodeResult(t, w.Body.Bytes()), "data", "result", "value") != "step 3" {
		t.Fatalf("owner read: %d %s", w.Code, w.Body.String())
	}
	// The catalogue and the capability.
	w = makeRequest(h, "GET", "https://swarmmemo.com/api/services", "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"id":"memory"`) || !strings.Contains(w.Body.String(), `"id":"echo"`) {
		t.Fatalf("catalogue: %d %s", w.Code, w.Body.String())
	}
	caps := decodeResult(t, makeRequest(h, "GET", "https://swarmmemo.com/capabilities", "", "").Body.Bytes())
	if dig(caps, "services", "memory", "e2ee") != false || dig(caps, "services", "memory", "value_bytes") != float64(board.MemoryValueBytes) {
		t.Fatalf("capabilities: %+v", caps["services"])
	}

	// MCP: a memory_get tool of the hosted shape (unsigned service.read with
	// args {agent, key}), over a real protocol session to the same store.
	type memoryGetInput struct {
		Agent string `json:"agent"`
		Key   string `json:"key"`
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "test"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "memory_get"}, func(ctx context.Context, _ *mcp.CallToolRequest, in memoryGetInput) (*mcp.CallToolResult, board.Result, error) {
		result, err := store.Execute(mcpVia(ctx), memoryGetCommand(in.Agent, in.Key), "")
		return nil, result, err
	})
	serverSide, clientSide := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err = server.Connect(ctx, serverSide, nil); err != nil {
		t.Fatal(err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "client"}, nil).Connect(ctx, clientSide, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	tool := func(name string, args map[string]any) map[string]any {
		t.Helper()
		out, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil || out.IsError {
			t.Fatalf("%s: %v %+v", name, err, out)
		}
		raw, _ := json.Marshal(out.StructuredContent)
		return decodeResult(t, raw)
	}
	if got := tool("memory_get", map[string]any{"agent": agent, "key": "profile"}); dig(got, "data", "result", "value") != "hello" {
		t.Fatalf("memory_get public: %+v", got)
	}
	// A private item does not exist to an unsigned MCP read.
	if out, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "memory_get", Arguments: map[string]any{"agent": agent, "key": "run/state"}}); err == nil && !out.IsError {
		t.Fatal("memory_get must not read a private item")
	}

	db, err := sql.Open("sqlite", dbPath) // a second connection, to read the fake journal
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	entries, err := meter.Entries(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if e.Kind == "spend" && e.Service == "memory" && e.Method == "put" && e.Resource == "memory_bytes" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("one receipt per write on each wire, got %d: %+v", n, entries)
	}
}
