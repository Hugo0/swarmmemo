package httpapi

import (
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// C691: a hosted reviewer accepts a result posted as a plain reply, never
// claimed: accept_work with the reply as result_id signs work.accept with
// target, and the board claims, submits and accepts it in one step.
func TestHostedMCPAcceptReply(t *testing.T) {
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
	worker := newIdentity(t, s, "reply-worker")
	judge := newIdentity(t, s, "reply-judge")
	asWorker, asJudge := "Bearer "+worker["token"].(string), "Bearer "+judge["token"].(string)
	feed, err := store.Execute(t.Context(), board.Command{Operation: "messages.list"}, "mcp-work-test")
	if err != nil {
		t.Fatal(err)
	}
	id := exec(board.Command{Operation: "post", Room: "gigs", Kind: "request", Text: "Summarize a page."}).Receipt.ID
	data, _ := json.Marshal(map[string]any{"schema": 1, "generation": feed.Generation, "title": "Summarize a page", "capabilities": []string{"writing"}, "reviewer": judge["agent"]})
	exec(board.Command{Operation: "work.create", MessageID: id, Data: string(data)})
	reply := mustTool(t, s, "/mcp", asWorker, "post_message", map[string]any{"reply_to": id, "text": "Summary: it is about agents."})["receipt"].(map[string]any)["id"].(string)
	got := mustTool(t, s, "/mcp", asJudge, "accept_work", map[string]any{"message_id": id, "result_id": reply})
	a := got["data"].(map[string]any)["ack"].(map[string]any)
	if a["state"] != "accepted" || !strings.Contains(a["note"].(string), "Accepted reply "+reply) {
		t.Fatalf("accept_work of a reply: %v", a)
	}
	w := mustTool(t, s, "/mcp", "", "read_work", map[string]any{"message_id": id})["data"].(map[string]any)["work"].(map[string]any)
	if w["state"] != "accepted" || w["result_id"] != reply {
		t.Fatalf("read_work after: %v", w)
	}
}
