package httpapi

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/services"
	"swarmmemo/internal/web"
)

// Credit top-ups over HTTP and hosted MCP: the board as an x402 server, on
// the real engine with a fake facilitator.

const topupTestPayTo = "0x1111111111111111111111111111111111111111"

// topupServer is a board with the ledger on, hosted identities and, when
// enabled, credit top-ups settled by a fake facilitator that counts
// settlements.
func topupServer(t *testing.T, enabled bool) (*Server, *atomic.Int64) {
	t.Helper()
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	kek := filepath.Join(t.TempDir(), "hosted-kek")
	if err := os.WriteFile(kek, []byte(base64.RawURLEncoding.EncodeToString(key)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	settled := &atomic.Int64{}
	var cfg *services.TopupConfig
	if enabled {
		var err error
		cfg, err = services.ParseTopupConfig([]byte(`{"schema":1,"enabled":true,"network":"eip155:8453","asset":"0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913","asset_name":"USD Coin","asset_version":"2","pay_to":"`+topupTestPayTo+`","facilitator_url":"https://facilitator.example.com"}`), nil)
		if err != nil {
			t.Fatal(err)
		}
		facilitator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/verify" {
				_, _ = w.Write([]byte(`{"isValid":true}`))
				return
			}
			n := settled.Add(1)
			fmt.Fprintf(w, `{"success":true,"transaction":"0x%064x","network":"eip155:8453"}`, n)
		}))
		t.Cleanup(facilitator.Close)
		cfg.UseTestFacilitator(facilitator.Client(), facilitator.URL)
	}
	features := board.Features{Ledger: board.LedgerOn}
	store, err := board.Open(filepath.Join(t.TempDir(), "board.sqlite"), board.Config{ServiceID: "swarmmemo.com", HostedKEKFile: kek, Features: features, Topup: cfg})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	features.Topup = cfg != nil
	return New(store, web.Handler(store), Config{PublicURL: "https://swarmmemo.com", ServiceID: "swarmmemo.com", Features: features}), settled
}

// topupPayment signs nothing: the fake facilitator accepts any signature.
func topupPayment(t *testing.T, accepted map[string]any, nonce byte) string {
	t.Helper()
	now := time.Now().Unix()
	raw, _ := json.Marshal(map[string]any{"x402Version": 2, "accepted": accepted, "payload": map[string]any{
		"signature": "0x" + strings.Repeat("ab", 65),
		"authorization": map[string]any{"from": "0x2222222222222222222222222222222222222222", "to": accepted["payTo"], "value": accepted["amount"],
			"validAfter": fmt.Sprint(now - 60), "validBefore": fmt.Sprint(now + 600), "nonce": "0x" + strings.Repeat(fmt.Sprintf("%02x", nonce), 32)}}})
	return base64.StdEncoding.EncodeToString(raw)
}

func topupRequest(s http.Handler, c board.Command, header string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(c)
	r := httptest.NewRequest("POST", "https://swarmmemo.com/v1/command", strings.NewReader(string(body)))
	r.RemoteAddr = "198.51.100.8:12345"
	r.Header.Set("Content-Type", "application/json")
	if header != "" {
		r.Header.Set("PAYMENT-SIGNATURE", header)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

// An x402 client's flow over HTTP: a 402 with PAYMENT-REQUIRED, the retry
// with PAYMENT-SIGNATURE, a 200 with PAYMENT-RESPONSE; the same payment
// again is refused.
func TestTopupOverHTTP(t *testing.T) {
	s, settled := topupServer(t, true)
	key := ed25519.NewKeyFromSeed(append(make([]byte, 31), 77))
	w := topupRequest(s, signService(key, board.Command{Operation: "credits.topup", Amount: 2_000_000}), "")
	header := w.Header().Get("PAYMENT-REQUIRED")
	if w.Code != 402 || header == "" || !strings.Contains(w.Header().Get("Access-Control-Expose-Headers"), "PAYMENT-REQUIRED") {
		t.Fatalf("quote: %d %q %s", w.Code, header, w.Body.String())
	}
	raw, _ := base64.StdEncoding.DecodeString(header)
	var required struct {
		Accepts []map[string]any `json:"accepts"`
	}
	if err := json.Unmarshal(raw, &required); err != nil || len(required.Accepts) != 1 || required.Accepts[0]["payTo"] != topupTestPayTo {
		t.Fatalf("PAYMENT-REQUIRED %s", raw)
	}
	payment := topupPayment(t, required.Accepts[0], 7)
	w = topupRequest(s, signService(key, board.Command{Operation: "credits.topup", Amount: 2_000_000}), payment)
	if w.Code != 200 || w.Header().Get("PAYMENT-RESPONSE") == "" {
		t.Fatalf("pay: %d %s", w.Code, w.Body.String())
	}
	if res := decodeResult(t, w.Body.Bytes()); dig(res, "data", "topup", "state") != "credited" {
		t.Fatalf("receipt %v", res)
	}
	w = topupRequest(s, signService(key, board.Command{Operation: "credits.topup", Amount: 2_000_000}), payment)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "payment_replayed") {
		t.Fatalf("replay: %d %s", w.Code, w.Body.String())
	}
	// Two payments in one request are refused before anything runs.
	body, _ := json.Marshal(signService(key, board.Command{Operation: "credits.topup", Amount: 2_000_000}))
	r := httptest.NewRequest("POST", "https://swarmmemo.com/v1/command", strings.NewReader(string(body)))
	r.RemoteAddr = "198.51.100.8:12345"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("PAYMENT-SIGNATURE", payment)
	r.Header.Set("X-PAYMENT", payment)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "payment_invalid") {
		t.Fatalf("two payments: %d %s", w.Code, w.Body.String())
	}
	if settled.Load() != 1 {
		t.Fatalf("%d settlements", settled.Load())
	}
	caps := decodeResult(t, get(s, "/capabilities", "application/json").Body.Bytes())
	if dig(caps, "topup", "operation") != "credits.topup" || dig(caps, "topup", "pay_to") != topupTestPayTo {
		t.Fatalf("capabilities topup %v", caps["topup"])
	}
	if w := get(s, "/tools/topup", "text/html"); w.Code != 200 {
		t.Fatalf("/tools/topup: %d", w.Code)
	}
	if body := get(s, "/llms.txt", "").Body.String(); !strings.Contains(body, "/tools/topup") {
		t.Fatal("llms.txt does not link /tools/topup while top-ups are on")
	}
}

// Off: no capability, no page, no llms.txt line, no MCP tool, and the
// operation is unavailable.
func TestTopupOffHasNoSurface(t *testing.T) {
	s, _ := topupServer(t, false)
	caps := decodeResult(t, get(s, "/capabilities", "application/json").Body.Bytes())
	if caps["topup"] != nil {
		t.Fatalf("capabilities topup %v", caps["topup"])
	}
	if w := get(s, "/tools/topup", "text/html"); w.Code != 404 {
		t.Fatalf("/tools/topup while off: %d", w.Code)
	}
	if body := get(s, "/llms.txt", "").Body.String(); strings.Contains(body, "/tools/topup") {
		t.Fatal("llms.txt links /tools/topup while top-ups are off")
	}
	token := newIdentity(t, s, "no-topup")["token"].(string)
	if _, ok := listTools(t, s, "/mcp/t/"+token)["credits_topup"]; ok {
		t.Fatal("credits_topup listed while top-ups are off")
	}
	key := ed25519.NewKeyFromSeed(append(make([]byte, 31), 78))
	w := topupRequest(s, signService(key, board.Command{Operation: "credits.topup", Amount: 2_000_000}), "")
	if w.Code != 404 || !strings.Contains(w.Body.String(), "topup_unavailable") {
		t.Fatalf("credits.topup while off: %d %s", w.Code, w.Body.String())
	}
}

// The assistant profile has no payment tools, as its instructions say: with
// top-ups on and a hosted identity signed in, its tools/list has no
// top-up or other payment tool, and a call to credits_topup is an unknown
// tool that never reaches the ledger; /mcp keeps it (C52).
func TestTopupNotOnAssistantProfile(t *testing.T) {
	s, settled := topupServer(t, true)
	token := newIdentity(t, s, "assistant-no-pay")["token"].(string)
	if _, ok := listTools(t, s, "/mcp/t/"+token)["credits_topup"]; !ok {
		t.Fatal("control: credits_topup not listed on /mcp")
	}
	path := web.AssistantMCPPath + "/t/" + token
	payment := regexp.MustCompile(`topup|pay|x402|credit`)
	tools := listTools(t, s, path)
	if _, ok := tools["whoami"]; !ok {
		t.Fatalf("the hosted identity is not signed in on the assistant profile, so this test proves nothing: %v", slices.Collect(maps.Keys(tools)))
	}
	for name := range tools {
		if payment.MatchString(name) {
			t.Errorf("assistant profile lists payment tool %s", name)
		}
	}
	var init struct{ Instructions string }
	mcpPost(t, s, path, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`, &init)
	if !strings.Contains(init.Instructions, "This profile has no payment tools") {
		t.Errorf("assistant instructions no longer state the rule this test enforces: %q", init.Instructions)
	}
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "credits_topup", "arguments": map[string]any{"amount": 500_000}}})
	r := httptest.NewRequest("POST", "https://swarmmemo.com"+path, strings.NewReader(string(raw)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), "unknown tool") || strings.Contains(w.Body.String(), "payment_required") || settled.Load() != 0 {
		t.Fatalf("assistant credits_topup call: %d %s", w.Code, w.Body.String())
	}
}

// A hosted identity tops up over MCP: quoted, then paid.
func TestTopupOverHostedMCP(t *testing.T) {
	s, settled := topupServer(t, true)
	token := newIdentity(t, s, "topup-helper")["token"].(string)
	path := "/mcp/t/" + token
	if _, ok := listTools(t, s, path)["credits_topup"]; !ok {
		t.Fatal("credits_topup not listed")
	}
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "credits_topup", "arguments": map[string]any{"amount": 500_000}}})
	out := mcpRequest(t, s, path, "", string(raw))
	accepts, _ := dig(out, "result", "structuredContent", "error", "details", "x402", "accepts").([]any)
	if dig(out, "result", "structuredContent", "error", "code") != "payment_required" || len(accepts) != 1 {
		t.Fatalf("quote over MCP: %v", out)
	}
	payment := topupPayment(t, accepts[0].(map[string]any), 9)
	res := mustTool(t, s, path, "", "credits_topup", map[string]any{"amount": 500_000, "payment": payment})
	if dig(res, "data", "topup", "state") != "credited" || settled.Load() != 1 {
		t.Fatalf("paid over MCP: %v", res)
	}
}
