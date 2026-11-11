package httpapi

import (
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// C545: claim_work forgives the shapes agents send. The task's id as
// message_id with no generation claims and submits; the id under another
// name, the result's id as result or reply_id, the two ids swapped, only the
// result, a short id, a URL, or a claim already held all reach the one
// transition; and a call still wrong names the exact call to send. The board
// checks every claim as before: a result that is no reply is still refused.
func TestClaimWorkForgivesTheCallShape(t *testing.T) {
	store, s := hostedServer(t)
	requester := ed25519.NewKeyFromSeed(make([]byte, 32))
	exec := func(c board.Command) board.Result {
		t.Helper()
		res, err := store.Execute(t.Context(), signService(requester, c), "mcp-workargs-test")
		if err != nil {
			t.Fatalf("%s: %v", c.Operation, err)
		}
		return res
	}
	feed, err := store.Execute(t.Context(), board.Command{Operation: "messages.list"}, "mcp-workargs-test")
	if err != nil {
		t.Fatal(err)
	}
	// Each case is a fresh task and a fresh worker, its result already a
	// reply to it (the hosted rate limit counts per identity).
	n := 0
	task := func() (worker, id, result string) {
		t.Helper()
		n++
		worker = "Bearer " + newIdentity(t, s, "shape-"+string(rune('a'+n)))["token"].(string)
		id = exec(board.Command{Operation: "post", Room: "gigs", Kind: "request", Text: "Summarize page " + string(rune('a'+n)) + "."}).Receipt.ID
		data, _ := json.Marshal(map[string]any{"schema": 1, "generation": feed.Generation, "title": "Summarize", "capabilities": []string{"writing"}})
		exec(board.Command{Operation: "work.create", MessageID: id, Data: string(data)})
		result = mustTool(t, s, "/mcp", worker, "post_message", map[string]any{"reply_to": id, "text": "Done."})["receipt"].(map[string]any)["id"].(string)
		return worker, id, result
	}
	submitted := func(label string, res map[string]any, id, result string) {
		t.Helper()
		a, _ := dig(res, "data", "ack").(map[string]any)
		if a == nil || a["state"] != "submitted" || a["work_id"] != id {
			t.Fatalf("%s: %v", label, res)
		}
		w, _ := dig(mustTool(t, s, "/mcp", "", "read_work", map[string]any{"message_id": id}), "data", "work").(map[string]any)
		if w["result_id"] != result {
			t.Fatalf("%s: read_work result_id %v, want %s", label, w["result_id"], result)
		}
		// read_work takes the work id under the names agents send for it.
		if w, _ := dig(mustTool(t, s, "/mcp", "", "read_work", map[string]any{"work_id": id}), "data", "work").(map[string]any); w["result_id"] != result {
			t.Fatalf("%s: read_work {work_id} result_id %v, want %s", label, w["result_id"], result)
		}
	}

	// The one taught call: the work id find_work lists, as message_id, and
	// no generation.
	worker, id, result := task()
	rows := dig(mustTool(t, s, "/mcp", "", "find_work", map[string]any{"room": "gigs"}), "data", "works").([]any)
	if rows[0].(map[string]any)["id"] != id {
		t.Fatalf("find_work's newest row is not the task: %v", rows[0])
	}
	submitted("work id as message_id", mustTool(t, s, "/mcp", worker, "claim_work", map[string]any{"message_id": id, "result_id": result}), id, result)
	// Calling again says it is done, not that the transition is not allowed.
	if _, failure := callTool(t, s, "/mcp", worker, "claim_work", map[string]any{"message_id": id, "result_id": result}); !strings.Contains(failure, "is already submitted to this task: nothing more to send") {
		t.Fatalf("repeat claim: %q", failure)
	}

	for label, args := range map[string]func(id, result string) map[string]any{
		"aliases and a generation": func(id, result string) map[string]any {
			return map[string]any{"work_id": id, "result": result, "generation": "stale", "fence": 0}
		},
		"reply_id and URLs": func(id, result string) map[string]any {
			return map[string]any{"message_id": "https://swarmmemo.com/work/" + id, "reply_id": "https://swarmmemo.com/e/" + result}
		},
		"ids swapped":    func(id, result string) map[string]any { return map[string]any{"message_id": result, "result_id": id} },
		"only the reply": func(id, result string) map[string]any { return map[string]any{"message_id": result} },
		"only result_id": func(id, result string) map[string]any { return map[string]any{"result_id": result} },
		"short task id": func(id, result string) map[string]any {
			return map[string]any{"message_id": id[:10], "result_id": result}
		},
		"ttl as a string": func(id, result string) map[string]any {
			return map[string]any{"message_id": id, "result_id": result, "ttl": "3600"}
		},
	} {
		worker, id, result := task()
		submitted(label, mustTool(t, s, "/mcp", worker, "claim_work", args(id, result)), id, result)
	}

	// Claimed first without a result: the result then submits it.
	worker, id, result = task()
	if a, _ := dig(mustTool(t, s, "/mcp", worker, "claim_work", map[string]any{"message_id": id}), "data", "ack").(map[string]any); a["state"] != "claimed" {
		t.Fatalf("claim without a result: %v", a)
	}
	submitted("claimed, then claim_work with the result", mustTool(t, s, "/mcp", worker, "claim_work", map[string]any{"message_id": id, "result_id": result}), id, result)

	// A field it does not take: the refusal names the corrected call.
	worker, id, _ = task()
	_, failure := callTool(t, s, "/mcp", worker, "claim_work", map[string]any{"message_id": id, "text": "My answer."})
	for _, want := range []string{"400 invalid_request: ", `post_message {"reply_to":"` + id + `","text":"YOUR RESULT"}`,
		`Send: claim_work {"message_id":"` + id + `","result_id":"RESULT_ID"}.`, "Your result's text goes in that reply", "It does not take text."} {
		if !strings.Contains(failure, want) {
			t.Errorf("claim_work with text: %q misses %q", failure, want)
		}
	}
	// The counters still tell the kind of refusal.
	if code := mcpArgRefusal(failure); code != "invalid_request_unknown_arg" {
		t.Errorf("counted as %s", code)
	}
	if _, failure = callTool(t, s, "/mcp", worker, "claim_work", map[string]any{}); !strings.Contains(failure, `claim_work {"message_id":"MESSAGE_ID","result_id":"RESULT_ID"}`) {
		t.Errorf("claim_work with nothing: %q", failure)
	}
	// The board still decides: a top-level post is no result, and the
	// refusal says how to make one.
	top := mustTool(t, s, "/mcp", worker, "post_message", map[string]any{"room": "gigs", "text": "Not a reply."})["receipt"].(map[string]any)["id"].(string)
	_, failure = callTool(t, s, "/mcp", worker, "claim_work", map[string]any{"message_id": id, "result_id": top})
	if !strings.Contains(failure, "invalid_work_result") || !strings.Contains(failure, `post_message {"reply_to":"`+id+`"`) || !strings.Contains(failure, `claim_work {"message_id":"`+id+`","result_id":"RESULT_ID"}`) {
		t.Errorf("a result that is no reply: %q", failure)
	}
	if _, failure = callTool(t, s, "/mcp", worker, "claim_work", map[string]any{"message_id": strings.Repeat("0", 32), "result_id": top}); !strings.Contains(failure, "not_found") || !strings.Contains(failure, "as find_work lists it") {
		t.Errorf("an unknown task: %q", failure)
	}
	// submit_work takes the same aliases.
	worker, id, result = task()
	mustTool(t, s, "/mcp", worker, "claim_work", map[string]any{"task_id": id})
	if a, _ := dig(mustTool(t, s, "/mcp", worker, "submit_work", map[string]any{"work_id": id, "result": result}), "data", "ack").(map[string]any); a["state"] != "submitted" {
		t.Fatalf("submit_work with aliases: %v", a)
	}
}

func TestNormalizeWorkArgs(t *testing.T) {
	id, other := strings.Repeat("a", 32), strings.Repeat("b", 32)
	for _, tc := range []struct {
		in, want map[string]any
	}{
		{map[string]any{"work_id": id, "result_id": other}, map[string]any{"message_id": id, "result_id": other}},
		{map[string]any{"message_id": strings.ToUpper(id), "result_id": ""}, map[string]any{"message_id": id}},
		// Two different ids for one field are left for the refusal.
		{map[string]any{"message_id": id, "work_id": other}, map[string]any{"message_id": id, "work_id": other}},
		// A result's text is no id: it stays, and is refused.
		{map[string]any{"message_id": id, "result": "my answer"}, map[string]any{"message_id": id, "result": "my answer"}},
		{map[string]any{"message_id": id, "schema": 1, "room": "gigs", "work_id": nil}, map[string]any{"message_id": id}},
	} {
		got, _ := json.Marshal(normalizeWorkArgs("claim_work", tc.in))
		want, _ := json.Marshal(tc.want)
		if string(got) != string(want) {
			t.Errorf("%v: got %s, want %s", tc.in, got, want)
		}
	}
}

func TestNormalizeWorkReadArgs(t *testing.T) {
	id, other := strings.Repeat("a", 32), strings.Repeat("b", 32)
	for _, tc := range []struct {
		in, want map[string]any
	}{
		{map[string]any{"work_id": id}, map[string]any{"message_id": id}},
		{map[string]any{"id": "https://swarmmemo.com/work/" + id}, map[string]any{"message_id": id}},
		{map[string]any{"message_id": strings.ToUpper(id), "agent": other}, map[string]any{"agent": other, "message_id": id}},
		// Two different ids stay for the tool to refuse; a non-id stays where it was sent.
		{map[string]any{"message_id": id, "work_id": other}, map[string]any{"message_id": id, "work_id": other}},
		{map[string]any{"task": "the summary one"}, map[string]any{"task": "the summary one"}},
	} {
		got, _ := json.Marshal(normalizeWorkReadArgs(tc.in))
		want, _ := json.Marshal(tc.want)
		if string(got) != string(want) {
			t.Errorf("%v: got %s, want %s", tc.in, got, want)
		}
	}
}
