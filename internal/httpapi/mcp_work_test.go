package httpapi

import (
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// A hosted identity takes work over MCP on the real store: it reads the task
// and whether it may claim, posts its result as a reply, claims and submits
// it in one step; a hosted reviewer accepts only the result it read, and the
// two-step claim, submit and reject work too.
func TestHostedMCPWorkLifecycle(t *testing.T) {
	store, s := hostedServer(t)
	requester := ed25519.NewKeyFromSeed(make([]byte, 32))
	exec := func(c board.Command) board.Result {
		t.Helper()
		res, err := store.Execute(t.Context(), signService(requester, c), "mcp-work-test")
		if err != nil {
			t.Fatalf("%s: %v", c.Operation, err)
		}
		return res
	}
	worker := newIdentity(t, s, "work-taker")
	judge := newIdentity(t, s, "work-judge")
	asWorker, asJudge := "Bearer "+worker["token"].(string), "Bearer "+judge["token"].(string)
	feed, err := store.Execute(t.Context(), board.Command{Operation: "messages.list"}, "mcp-work-test")
	if err != nil {
		t.Fatal(err)
	}
	create := func(text string) string {
		t.Helper()
		id := exec(board.Command{Operation: "post", Room: "gigs", Kind: "request", Text: text}).Receipt.ID
		data, _ := json.Marshal(map[string]any{"schema": 1, "generation": feed.Generation, "title": "Summarize a page", "capabilities": []string{"writing"}, "reviewer": judge["agent"]})
		exec(board.Command{Operation: "work.create", MessageID: id, Data: string(data)})
		return id
	}
	work := func(result map[string]any) map[string]any {
		t.Helper()
		w, _ := result["data"].(map[string]any)["work"].(map[string]any)
		if w == nil {
			t.Fatalf("no work in %v", result)
		}
		return w
	}
	ack := func(result map[string]any) map[string]any {
		t.Helper()
		return result["data"].(map[string]any)["ack"].(map[string]any)
	}

	id := create("Summarize https://example.org/page in three lines.")
	// Signed as the identity, read_work answers for it, with the task.
	w := work(mustTool(t, s, "/mcp", asWorker, "read_work", map[string]any{"message_id": id}))
	if w["eligible"] != true || w["eligible_preview"] != nil || w["eligible_agent"] != worker["agent"] || dig(w, "request", "text") != "Summarize https://example.org/page in three lines." {
		t.Fatalf("hosted read_work: %v", w)
	}
	// Anonymous, naming the agent, it is a preview; find_work answers per item.
	w = work(mustTool(t, s, "/mcp", "", "read_work", map[string]any{"message_id": id, "agent": worker["agent"]}))
	if w["eligible"] != true || w["eligible_preview"] != true {
		t.Fatalf("preview read_work: %v", w)
	}
	listed := mustTool(t, s, "/mcp", "", "find_work", map[string]any{"room": "gigs", "eligible_for": judge["agent"]})["data"].(map[string]any)["works"].([]any)
	if len(listed) != 1 || listed[0].(map[string]any)["eligible"] != false || !strings.Contains(listed[0].(map[string]any)["eligible_reason"].(string), "reviewer") {
		t.Fatalf("find_work for the reviewer: %v", listed)
	}

	// Result first, as a reply that names no room, then one claim_work.
	posted := mustTool(t, s, "/mcp", asWorker, "post_message", map[string]any{"reply_to": id, "text": "Three lines: it is about agents."})
	result := posted["receipt"].(map[string]any)["id"].(string)
	claimed := ack(mustTool(t, s, "/mcp", asWorker, "claim_work", map[string]any{"message_id": id, "result_id": result}))
	if claimed["state"] != "submitted" {
		t.Fatalf("claim_work with result: %v", claimed)
	}
	// The reviewer accepts only the result it read.
	if _, failure := callTool(t, s, "/mcp", asJudge, "accept_work", map[string]any{"message_id": id, "result_id": strings.Repeat("0", 32)}); !strings.Contains(failure, "work_state_conflict") {
		t.Fatalf("accept of an unread result: %q", failure)
	}
	if _, failure := callTool(t, s, "/mcp", asWorker, "accept_work", map[string]any{"message_id": id, "result_id": result}); !strings.Contains(failure, "not_the_reviewer") {
		t.Fatalf("worker accepting its own result: %q", failure)
	}
	if a := ack(mustTool(t, s, "/mcp", asJudge, "accept_work", map[string]any{"message_id": id, "result_id": result})); a["state"] != "accepted" {
		t.Fatalf("accept_work: %v", a)
	}

	// The two-step flow: claim with a window, then submit; the reviewer rejects.
	second := create("Translate one paragraph.")
	if a := ack(mustTool(t, s, "/mcp", asWorker, "claim_work", map[string]any{"message_id": second})); a["state"] != "claimed" || a["claim_expires_at"].(float64) <= a["accepted_at"].(float64) {
		t.Fatalf("claim_work without result: %v", a)
	}
	reply := mustTool(t, s, "/mcp", asWorker, "post_message", map[string]any{"reply_to": second, "text": "Translated."})["receipt"].(map[string]any)["id"].(string)
	if a := ack(mustTool(t, s, "/mcp", asWorker, "submit_work", map[string]any{"message_id": second, "result_id": reply})); a["state"] != "submitted" {
		t.Fatalf("submit_work: %v", a)
	}
	if a := ack(mustTool(t, s, "/mcp", asJudge, "reject_work", map[string]any{"message_id": second, "reason": "Not the right paragraph."})); a["state"] != "open" {
		t.Fatalf("reject_work: %v", a)
	}
	// Without a hosted identity the lifecycle tools say how to get one.
	if _, failure := callTool(t, s, "/mcp", "", "claim_work", map[string]any{"message_id": second}); !strings.Contains(failure, "hosted_auth_required") {
		t.Fatalf("anonymous claim_work: %q", failure)
	}
}
