package httpapi

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
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
	board.OpenStandingDayForTest(t) // claims several seats at once, at any hour (C1046)
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

	// C1144: the requester accepts A's first result and rejects B's, then A
	// sends a second. Each worker's read_work lists its own results with
	// their states, and its next step reads the newest judged one by its
	// work_id.
	verdict := fmt.Sprintf(`{"schema":1,"generation":"%s"}`, feed.Generation)
	exec(board.Command{Operation: "work.accept", MessageID: replyA, Amount: 1, Data: verdict})
	exec(board.Command{Operation: "work.reject", MessageID: replyB, Amount: 1, Reason: "not reproducible", Data: verdict})
	replyA2 := mustTool(t, s, "/mcp", authA, "post_message", map[string]any{"reply_to": id, "text": "Bug: standing-a, a second one"})["receipt"].(map[string]any)["id"].(string)
	if ack := dig(mustTool(t, s, "/mcp", authA, "claim_work", map[string]any{"message_id": id, "result_id": replyA2}), "data", "ack").(map[string]any); ack["result_work_id"] != replyA2 {
		t.Fatalf("A's second claim ack: %v", ack)
	}
	readWork := func(auth string) map[string]any {
		t.Helper()
		return dig(mustTool(t, s, "/mcp", auth, "read_work", map[string]any{"message_id": id}), "data").(map[string]any)
	}
	nextStep := func(data map[string]any) (string, any) {
		t.Helper()
		steps, _ := data["next_steps"].([]any)
		if len(steps) != 1 || dig(steps[0], "tool", "name") != "read_work" {
			t.Fatalf("next_steps: %v", data["next_steps"])
		}
		return dig(steps[0], "text").(string), dig(steps[0], "tool", "arguments", "message_id")
	}
	dataA := readWork(authA)
	states := map[any]any{}
	for _, r := range dig(dataA, "work", "your_results").([]any) {
		states[dig(r, "work_id")] = fmt.Sprint(dig(r, "state"), "/", dig(r, "verdict"))
	}
	if len(states) != 2 || states[replyA] != "accepted/accepted" || states[replyA2] != "submitted/<nil>" {
		t.Fatalf("worker A's two results: %v", states)
	}
	if text, target := nextStep(dataA); target != replyA || !strings.Contains(text, replyA+" on this standing task was accepted") {
		t.Fatalf("worker A's next step: %q -> %v", text, target)
	}
	dataB := readWork(authB)
	if y := dig(dataB, "work", "your_results").([]any); len(y) != 1 || dig(y[0], "work_id") != replyB || dig(y[0], "verdict") != "rejected" {
		t.Fatalf("worker B's your_results after the verdict: %v", y)
	}
	if text, target := nextStep(dataB); target != replyB || !strings.Contains(text, "was rejected") {
		t.Fatalf("worker B's next step: %q -> %v", text, target)
	}
	if data := readWork(""); data["next_steps"] != nil || dig(data, "work", "your_results") != nil {
		t.Fatalf("an anonymous read_work after the verdicts: %v", data)
	}
	var api map[string]any
	if err = json.Unmarshal(makeRequest(s, "GET", "/api/work/"+id, "", "").Body.Bytes(), &api); err != nil {
		t.Fatal(err)
	}
	if w := dig(api, "data", "work").(map[string]any); w["your_results"] != nil || w["standing"] == nil {
		t.Fatalf("/api/work: %v", w)
	}
}
