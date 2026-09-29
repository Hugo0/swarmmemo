package httpapi

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"swarmmemo/internal/board"
	"swarmmemo/internal/services"
	"swarmmemo/internal/services/servicestest"
)

func openWakeNotaryStore(t *testing.T) (*board.Store, http.Handler) {
	t.Helper()
	features := board.Features{Services: []string{"notary", "wakeup"}}
	store, err := board.Open(filepath.Join(t.TempDir(), "board.sqlite"), board.Config{ServiceID: "swarmmemo.com", Features: features})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	store.UseServiceMeter(servicestest.NewMeter(1<<30), &servicestest.Params{})
	return store, New(store, nil, Config{Features: features, PublicURL: "https://swarmmemo.com", ServiceID: "swarmmemo.com"})
}

func wireKey(seed byte) (ed25519.PrivateKey, string) {
	key := ed25519.NewKeyFromSeed(append(make([]byte, 31), seed))
	sum := sha256.Sum256(key.Public().(ed25519.PublicKey))
	return key, hex.EncodeToString(sum[:])
}

func postCommand(t *testing.T, h http.Handler, c board.Command) map[string]any {
	t.Helper()
	body, _ := json.Marshal(c)
	w := makeRequest(h, "POST", "https://swarmmemo.com/v1/command", string(body), "application/json")
	if w.Code != 200 {
		t.Fatalf("/v1/command %s: %d %s", c.Operation, w.Code, w.Body.String())
	}
	return decodeResult(t, w.Body.Bytes())
}

func c64Command(t *testing.T, h http.Handler, c board.Command) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(c)
	w := makeRequest(h, "GET", "https://swarmmemo.com/c64/"+base64.RawURLEncoding.EncodeToString(raw), "", "")
	if w.Code != 200 {
		t.Fatalf("/c64 %s: %d %s", c.Operation, w.Code, w.Body.String())
	}
	return decodeResult(t, w.Body.Bytes())
}

func serviceArgs(method string, args any, maxCost int64) string {
	d := map[string]any{"schema": 1, "method": method, "args": args}
	if maxCost >= 0 {
		d["max_cost"] = maxCost
	}
	raw, _ := json.Marshal(d)
	return string(raw)
}

// TestWakeupOverEveryWire schedules over /v1/command and /c64, and finds the
// firing in the reads an agent already makes: GET /api/updates and the
// hosted MCP read_updates tool.
func TestWakeupOverEveryWire(t *testing.T) {
	store, h := openWakeNotaryStore(t)
	alice, me := wireKey(1)
	bob, _ := wireKey(2)
	root := postCommand(t, h, signService(alice, board.Command{Operation: "post", Room: "lobby", Text: "Anyone?"}))
	rootID := dig(root, "receipt", "id").(string)
	if _, err := store.WorkServices(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := postCommand(t, h, signService(alice, board.Command{Operation: "service.call", Target: "wakeup", Data: serviceArgs("schedule", map[string]any{"key": "replies", "on": "reply"}, 1), RequestID: "wake-1"}))
	if dig(out, "data", "call", "state") != "done" || dig(out, "data", "result", "wakeup", "on") != "reply" {
		t.Fatalf("schedule over /v1/command: %+v", out)
	}
	out = c64Command(t, h, signService(alice, board.Command{Operation: "service.call", Target: "wakeup", Data: serviceArgs("schedule", map[string]any{"key": "later", "at": time.Now().Unix() + 3600}, 1)}))
	if dig(out, "data", "result", "wakeup", "on") != "time" {
		t.Fatalf("schedule over /c64: %+v", out)
	}
	reply := postCommand(t, h, signService(bob, board.Command{Operation: "post", Room: "lobby", Text: "Me.", ReplyTo: rootID}))
	replyID := dig(reply, "receipt", "id").(string)
	if n, err := store.WorkServices(context.Background()); err != nil || n != 1 {
		t.Fatalf("one firing: %d %v", n, err)
	}

	w := makeRequest(h, "GET", "https://swarmmemo.com/api/updates?agent="+me, "", "")
	updates := decodeResult(t, w.Body.Bytes())
	list, _ := dig(updates, "data", "wakeups").([]any)
	if w.Code != 200 || len(list) != 1 || dig(list[0], "on") != "reply" || dig(list[0], "event") != replyID {
		t.Fatalf("GET /api/updates: %d %s", w.Code, w.Body.String())
	}

	call := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read_updates","arguments":{"agent":"` + me + `"}}}`
	r := httptest.NewRequest("POST", "https://swarmmemo.com/mcp", strings.NewReader(call))
	r.RemoteAddr = "198.51.100.8:12345"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"wakeups"`) || !strings.Contains(rec.Body.String(), replyID) || strings.Contains(rec.Body.String(), `"isError":true`) {
		t.Fatalf("MCP read_updates: %d %s", rec.Code, rec.Body.String())
	}

	caps := decodeResult(t, makeRequest(h, "GET", "https://swarmmemo.com/capabilities", "", "").Body.Bytes())
	if dig(caps, "services", "wakeup", "active_per_agent") != float64(board.WakeupsPerAccount) || dig(caps, "services", "notary", "public_key") != "/api/notary/key" {
		t.Fatalf("capabilities: %+v", caps["services"])
	}
}

// TestNotaryOverEveryWire stamps over /v1/command and /c64, reads receipts
// and the key over GET, /v1/command and MCP, and verifies them offline.
func TestNotaryOverEveryWire(t *testing.T) {
	store, h := openWakeNotaryStore(t)
	key, _ := wireKey(3)
	w := makeRequest(h, "GET", "https://swarmmemo.com/api/notary/key", "", "")
	if w.Code != 200 {
		t.Fatalf("key: %d %s", w.Code, w.Body.String())
	}
	public := dig(decodeResult(t, w.Body.Bytes()), "data", "result", "public_key").(string)

	receipt := func(v any) services.NotaryReceipt {
		raw, _ := json.Marshal(v)
		var r services.NotaryReceipt
		if err := json.Unmarshal(raw, &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	sum := sha256.Sum256([]byte("contract v1"))
	hash := hex.EncodeToString(sum[:])
	out := postCommand(t, h, signService(key, board.Command{Operation: "service.call", Target: "notary", Data: serviceArgs("stamp", map[string]any{"text": "contract v1"}, 1), RequestID: "stamp-1"}))
	first := receipt(dig(out, "data", "result", "receipt"))
	if first.Hash != hash || !services.VerifyNotaryReceipt(public, first) {
		t.Fatalf("/v1/command stamp: %+v", out)
	}
	out = c64Command(t, h, signService(key, board.Command{Operation: "service.call", Target: "notary", Data: serviceArgs("stamp", map[string]any{"hash": strings.Repeat("0f", 32)}, 1)}))
	second := receipt(dig(out, "data", "result", "receipt"))
	if second.Seq != first.Seq+1 || !services.VerifyNotaryReceipt(public, second) {
		t.Fatalf("/c64 stamp: %+v", out)
	}
	// Anyone reads a receipt back by hash, unsigned.
	w = makeRequest(h, "GET", "https://swarmmemo.com/api/notary/"+hash, "", "")
	if w.Code != 200 || receipt(dig(decodeResult(t, w.Body.Bytes()), "data", "result", "receipt")) != first {
		t.Fatalf("GET receipt: %d %s", w.Code, w.Body.String())
	}
	for _, bad := range []string{"/api/notary/" + strings.ToUpper(hash), "/api/notary/", "/api/notary/" + hash + "?x=1"} {
		if w = makeRequest(h, "GET", "https://swarmmemo.com"+bad, "", ""); w.Code != 400 {
			t.Errorf("%s: %d %s", bad, w.Code, w.Body.String())
		}
	}
	if w = makeRequest(h, "GET", "https://swarmmemo.com/api/notary/"+strings.Repeat("1", 64), "", ""); w.Code != 404 || !strings.Contains(w.Body.String(), "notary_not_found") {
		t.Fatalf("unknown hash: %d %s", w.Code, w.Body.String())
	}
	got := postCommand(t, h, board.Command{Operation: "service.read", Target: "notary", Data: serviceArgs("get", map[string]any{"hash": second.Hash}, -1)})
	if receipt(dig(got, "data", "result", "receipt")) != second {
		t.Fatalf("/v1/command read: %+v", got)
	}

	// MCP: a notary_get tool of the route's shape, over a real protocol
	// session to the same store.
	type notaryGetInput struct {
		Hash string `json:"hash"`
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "test"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "notary_get"}, func(ctx context.Context, _ *mcp.CallToolRequest, in notaryGetInput) (*mcp.CallToolResult, board.Result, error) {
		result, err := store.Execute(mcpVia(ctx), notaryCommand(in.Hash), "")
		return nil, result, err
	})
	serverSide, clientSide := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := server.Connect(ctx, serverSide, nil); err != nil {
		t.Fatal(err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "client"}, nil).Connect(ctx, clientSide, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "notary_get", Arguments: map[string]any{"hash": hash}})
	if err != nil || res.IsError {
		t.Fatalf("MCP notary_get: %v %+v", err, res)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	if r := receipt(dig(decodeResult(t, raw), "data", "result", "receipt")); r != first || !services.VerifyNotaryReceipt(public, r) {
		t.Fatalf("MCP receipt: %s", raw)
	}
}

func TestNotaryRouteDeclinesWhileOff(t *testing.T) {
	f := &fakeService{}
	h := New(f, nil, Config{Features: board.Features{Services: []string{"memory"}}})
	makeRequest(h, "GET", "https://swarmmemo.com/api/notary/key", "", "")
	for _, c := range f.commands {
		if c.Operation == "service.read" {
			t.Fatalf("the notary route answered while notary is off: %+v", c)
		}
	}
	caps := decodeResult(t, makeRequest(h, "GET", "https://swarmmemo.com/capabilities", "", "").Body.Bytes())
	if dig(caps, "services", "notary") != nil || dig(caps, "services", "wakeup") != nil {
		t.Fatalf("capabilities list only enabled services: %+v", caps["services"])
	}
}
