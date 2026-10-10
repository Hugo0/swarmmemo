package httpapi

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/services"
)

// A USDC work reward (RFC 0016) through HTTPS and MCP on the real store: the
// capabilities say how, a hosted worker names its payout address with
// claim_work, /api/work and find_work show the asset owed after accept, and
// a settle verified on a fake chain makes the whole reward paid.
func TestWorkUSDCRewardOnAPIAndMCP(t *testing.T) {
	var block int64 = 1000
	paidAt := time.Now().Unix() + 60
	payee := "0x00000000000000000000000000000000000000aa"
	var asset string
	rpc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var result any
		switch req.Method {
		case "eth_getTransactionReceipt":
			pad := "0x" + strings.Repeat("0", 24)
			result = map[string]any{"status": "0x1", "blockNumber": fmt.Sprintf("0x%x", block), "logs": []any{map[string]any{"address": asset,
				"topics": []string{"0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef", pad + strings.Repeat("b", 40), pad + payee[2:]},
				"data":   fmt.Sprintf("0x%064x", 250_000)}}}
		case "eth_blockNumber":
			result = fmt.Sprintf("0x%x", block+5)
		case "eth_getBlockByNumber":
			result = map[string]any{"timestamp": fmt.Sprintf("0x%x", paidAt)}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
	}))
	t.Cleanup(rpc.Close)
	chain, err := services.NewUSDCChain("https://rpc.example.org/base")
	if err != nil {
		t.Fatal(err)
	}
	chain.UseTestRPC(rpc.Client(), rpc.URL)
	asset = strings.ToLower(chain.Asset.String())
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	kek := filepath.Join(t.TempDir(), "hosted-kek")
	if err := os.WriteFile(kek, []byte(base64.RawURLEncoding.EncodeToString(key)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := board.Open(filepath.Join(t.TempDir(), "board.sqlite"), board.Config{ServiceID: "swarmmemo.com", HostedKEKFile: kek, WorkUSDC: chain})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	s := New(store, nil, Config{PublicURL: "https://swarmmemo.com", ServiceID: "swarmmemo.com"})

	caps := makeRequest(s, "GET", "/capabilities", "", "").Body.String()
	for _, want := range []string{`"usdc":{`, `"enabled":true`, `"command":"work.settle"`, `"network":"eip155:8453"`} {
		if !strings.Contains(caps, want) {
			t.Errorf("/capabilities misses %s", want)
		}
	}

	requester := ed25519.NewKeyFromSeed(make([]byte, 32))
	exec := func(c board.Command) board.Result {
		t.Helper()
		res, err := store.Execute(t.Context(), signService(requester, c), "usdc-test")
		if err != nil {
			t.Fatalf("%s: %v", c.Operation, err)
		}
		return res
	}
	feed, err := store.Execute(t.Context(), board.Command{Operation: "messages.list"}, "usdc-test")
	if err != nil {
		t.Fatal(err)
	}
	id := exec(board.Command{Operation: "post", Room: "gigs", Kind: "request", Text: "Translate a short doc."}).Receipt.ID
	data, _ := json.Marshal(map[string]any{"schema": 1, "generation": feed.Generation, "title": "Translate a doc", "capabilities": []string{"translation"}, "reward": map[string]any{"usdc": "0.25"}})
	exec(board.Command{Operation: "work.create", MessageID: id, Data: string(data)})

	worker := newIdentity(t, s, "usdc-taker")
	asWorker := "Bearer " + worker["token"].(string)
	posted := mustTool(t, s, "/mcp", asWorker, "post_message", map[string]any{"reply_to": id, "text": "Translated."})
	result := posted["receipt"].(map[string]any)["id"].(string)
	claimed := mustTool(t, s, "/mcp", asWorker, "claim_work", map[string]any{"message_id": id, "result_id": result, "payout_address": payee})
	fence := int64(dig(claimed, "data", "ack", "fence").(float64))
	exec(board.Command{Operation: "work.accept", MessageID: id, Amount: fence, Data: fmt.Sprintf(`{"schema":1,"generation":"%s"}`, feed.Generation)})

	getWork := func() map[string]any {
		t.Helper()
		w := makeRequest(s, "GET", "/api/work/"+id, "", "")
		var body map[string]any
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil {
			t.Fatalf("/api/work: %d %s", w.Code, w.Body.String())
		}
		return dig(body, "data", "work").(map[string]any)
	}
	w := getWork()
	if dig(w, "reward_usdc", "state") != "payable" || dig(w, "reward_usdc", "amount") != "0.25" || !strings.EqualFold(dig(w, "reward_usdc", "pay_to").(string), payee) || w["reward_state"] != "payable" {
		t.Fatalf("/api/work after accept: %v %v", w["reward_usdc"], w["reward_state"])
	}
	listed := mustTool(t, s, "/mcp", "", "find_work", map[string]any{"room": "gigs", "kind": "accepted"})["data"].(map[string]any)["works"].([]any)
	if len(listed) != 1 || dig(listed[0], "reward_usdc", "state") != "payable" || dig(listed[0], "reward_state") != "payable" {
		t.Fatalf("find_work: %v", listed)
	}

	settle, _ := json.Marshal(map[string]any{"schema": 1, "generation": feed.Generation, "tx_hash": "0x" + strings.Repeat("c", 64)})
	exec(board.Command{Operation: board.WorkSettle, MessageID: id, Data: string(settle)})
	if w = getWork(); dig(w, "reward_usdc", "state") != "paid" || w["reward_state"] != "paid" || dig(w, "reward_usdc", "paid_amount") != "0.25" {
		t.Fatalf("/api/work after settle: %v %v", w["reward_usdc"], w["reward_state"])
	}
}
