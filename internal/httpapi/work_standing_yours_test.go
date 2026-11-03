package httpapi

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/board"
)

// C481 over MCP and HTTPS on the real store: a hosted worker's claim on a
// standing task says where its result went (result_work_id, result_state,
// seat of seats), and read_work on the standing task, signed as that worker,
// lists its own results under your_results; another worker sees only its
// own, and the anonymous /api/work none.
func TestStandingResultWhereItWent(t *testing.T) {
	store, s := hostedServerWith(t, board.Features{Ledger: board.LedgerOn})
	requester := ed25519.NewKeyFromSeed(make([]byte, 32))
	exec := func(c board.Command) board.Result {
		t.Helper()
		res, err := store.Execute(t.Context(), signService(requester, c), "standing-yours-test")
		if err != nil {
			t.Fatalf("%s: %v", c.Operation, err)
		}
		return res
	}
	feed, err := store.Execute(t.Context(), board.Command{Operation: "messages.list"}, "standing-yours-test")
	if err != nil {
		t.Fatal(err)
	}
	id := exec(board.Command{Operation: "post", Room: "gigs", Kind: "request", Text: "Report bugs on the board."}).Receipt.ID
	pub := requester.Public().(ed25519.PublicKey)
	if err = store.GrantAllowance(t.Context(), fmt.Sprintf("%x", sha256.Sum256(pub)), allowance.Credit, allowance.Paid, 5000, "standing test"); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{"schema": 1, "generation": feed.Generation, "title": "Bug reports", "capabilities": []string{"bugs"}, "seats": 3, "reward": 100,
		// Both hosted workers write from the test server's one address, so they are linked keys and
		// share per_agent_day (C707); room for both keeps this test about where results went.
		"per_agent_day": 3})
	exec(board.Command{Operation: "work.create", MessageID: id, Data: string(data), TTL: 10 * 86400})

	claim := func(name string) (string, string, map[string]any) {
		t.Helper()
		who := newIdentity(t, s, name)
		auth := "Bearer " + who["token"].(string)
		reply := mustTool(t, s, "/mcp", auth, "post_message", map[string]any{"reply_to": id, "text": "Bug: " + name})["receipt"].(map[string]any)["id"].(string)
		ack := dig(mustTool(t, s, "/mcp", auth, "claim_work", map[string]any{"message_id": id, "result_id": reply}), "data", "ack").(map[string]any)
		return auth, reply, ack
	}
	authA, replyA, ack := claim("standing-a")
	if ack["work_id"] != replyA || ack["result_work_id"] != replyA || ack["result_state"] != "submitted" || ack["standing_id"] != id || ack["seat"] != 1.0 || ack["seats"] != 3.0 {
		t.Fatalf("claim ack: %v", ack)
	}
	authB, replyB, ack := claim("standing-b")
	if ack["result_work_id"] != replyB || ack["seat"] != 2.0 {
		t.Fatalf("second claim ack: %v", ack)
	}

	yours := func(auth string) []any {
		t.Helper()
		w := dig(mustTool(t, s, "/mcp", auth, "read_work", map[string]any{"message_id": id}), "data", "work").(map[string]any)
		if w["state"] != "open" {
			t.Fatalf("the standing task reads %v", w["state"])
		}
		list, _ := w["your_results"].([]any)
		return list
	}
	if y := yours(authA); len(y) != 1 || dig(y[0], "work_id") != replyA || dig(y[0], "state") != "submitted" || dig(y[0], "verdict") != nil {
		t.Fatalf("worker A's your_results: %v", y)
	}
	if y := yours(authB); len(y) != 1 || dig(y[0], "work_id") != replyB {
		t.Fatalf("worker B's your_results: %v", y)
	}
	if y := yours(""); y != nil {
		t.Fatalf("an anonymous read_work lists %v", y)
	}
	var api map[string]any
	if err = json.Unmarshal(makeRequest(s, "GET", "/api/work/"+id, "", "").Body.Bytes(), &api); err != nil {
		t.Fatal(err)
	}
	if w := dig(api, "data", "work").(map[string]any); w["your_results"] != nil || w["standing"] == nil {
		t.Fatalf("/api/work: %v", w)
	}
}
