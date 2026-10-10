package httpapi

import (
	"crypto/ed25519"
	"encoding/json"
	"reflect"
	"slices"
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
	// A bad checks list is refused, naming the field (C97).
	if _, failure := callTool(t, s, "/mcp", asJudge, "accept_work", map[string]any{"message_id": id, "result_id": result, "checks": []any{map[string]any{"property": "integrity", "state": "maybe"}}}); !strings.Contains(failure, "checks[0].state is not valid") {
		t.Fatalf("accept_work with a bad check: %q", failure)
	}
	checks := []any{map[string]any{"property": "integrity", "state": "pass", "subject_sha256": strings.Repeat("ab", 32), "tool": "sha256sum@9.4"}, map[string]any{"property": "conformance", "state": "not_checked"}}
	if a := ack(mustTool(t, s, "/mcp", asJudge, "accept_work", map[string]any{"message_id": id, "result_id": result, "checks": checks})); a["state"] != "accepted" {
		t.Fatalf("accept_work: %v", a)
	}
	if v, _ := work(mustTool(t, s, "/mcp", "", "read_work", map[string]any{"message_id": id}))["verdict_checks"].(map[string]any); v["operation"] != "work.accept" || !reflect.DeepEqual(v["checks"], checks) {
		t.Fatalf("read_work verdict_checks: %v", v)
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
	if a := ack(mustTool(t, s, "/mcp", asJudge, "reject_work", map[string]any{"message_id": second, "reason": "Not the right paragraph.", "checks": []any{map[string]any{"property": "conformance", "state": "fail", "evidence": reply}}})); a["state"] != "open" {
		t.Fatalf("reject_work: %v", a)
	}
	// Without a hosted identity the lifecycle tools say how to get one.
	if _, failure := callTool(t, s, "/mcp", "", "claim_work", map[string]any{"message_id": second}); !strings.Contains(failure, "hosted_auth_required") {
		t.Fatalf("anonymous claim_work: %q", failure)
	}
}

// Over MCP, an edited request's version ID reaches its work, and accept_work
// signs the hash of the submitted text it read, refusing any other.
func TestHostedMCPWorkEditedVersionAndResultHash(t *testing.T) {
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
	worker := newIdentity(t, s, "work-editor")
	judge := newIdentity(t, s, "work-hasher")
	asWorker, asJudge := "Bearer "+worker["token"].(string), "Bearer "+judge["token"].(string)
	feed, err := store.Execute(t.Context(), board.Command{Operation: "messages.list"}, "mcp-work-test")
	if err != nil {
		t.Fatal(err)
	}
	id := exec(board.Command{Operation: "post", Room: "gigs", Kind: "request", Text: "Summarize a page."}).Receipt.ID
	data, _ := json.Marshal(map[string]any{"schema": 1, "generation": feed.Generation, "title": "Summarize", "capabilities": []string{"writing"}, "reviewer": judge["agent"]})
	exec(board.Command{Operation: "work.create", MessageID: id, Data: string(data)})
	edit := exec(board.Command{Operation: "post", Room: "gigs", Kind: "request", Text: "Summarize a page, in three lines. Claim this message with work.claim.", Data: `{"schema":1,"supersedes":"` + id + `"}`}).Receipt.ID

	w := mustTool(t, s, "/mcp", asWorker, "read_work", map[string]any{"message_id": edit})["data"].(map[string]any)["work"].(map[string]any)
	if w["id"] != id || w["resolved_from"] != edit || dig(w, "request", "version_id") != edit {
		t.Fatalf("read_work by version: %v", w)
	}
	for _, path := range []string{"/api/work/" + edit, "/api/work/" + edit + "/history"} {
		if r := makeRequest(s, "GET", path, "", ""); r.Code != 200 || !strings.Contains(r.Body.String(), `"resolved_from":"`+edit+`"`) || !strings.Contains(r.Body.String(), id) {
			t.Fatalf("GET %s: %d %s", path, r.Code, r.Body.String())
		}
	}
	result := mustTool(t, s, "/mcp", asWorker, "post_message", map[string]any{"reply_to": id, "text": "Three lines."})["receipt"].(map[string]any)["id"].(string)
	claimed := mustTool(t, s, "/mcp", asWorker, "claim_work", map[string]any{"message_id": edit, "result_id": result})["data"].(map[string]any)["ack"].(map[string]any)
	if claimed["work_id"] != id || claimed["resolved_from"] != edit || claimed["state"] != "submitted" {
		t.Fatalf("claim_work by version: %v", claimed)
	}
	// The reviewer reads the submitted text's hash and accepts exactly that.
	w = mustTool(t, s, "/mcp", asJudge, "read_work", map[string]any{"message_id": id})["data"].(map[string]any)["work"].(map[string]any)
	submitted, _ := w["result_sha256"].(string)
	if w["result_id"] != result || w["result_changed_since_submit"] != false || len(submitted) != 64 {
		t.Fatalf("read_work of the submitted result: %v", w)
	}
	if _, failure := callTool(t, s, "/mcp", asJudge, "accept_work", map[string]any{"message_id": edit, "result_id": result, "result_sha256": strings.Repeat("0", 64)}); !strings.Contains(failure, "work_result_changed") {
		t.Fatalf("accept_work with another hash: %q", failure)
	}
	accepted := mustTool(t, s, "/mcp", asJudge, "accept_work", map[string]any{"message_id": edit, "result_id": result})["data"].(map[string]any)["ack"].(map[string]any)
	if accepted["state"] != "accepted" || accepted["result_sha256"] != submitted {
		t.Fatalf("accept_work: %v", accepted)
	}
}

// find_work pages 10 compact rows by default (C132b); detail=true and a
// limit restore full rows and bigger pages, and /api/works keeps its own
// default (25) and full rows.
func TestMCPFindWorkCompactByDefault(t *testing.T) {
	store, s := hostedServer(t)
	requester := ed25519.NewKeyFromSeed(make([]byte, 32))
	feed, err := store.Execute(t.Context(), board.Command{Operation: "messages.list"}, "mcp-work-test")
	if err != nil {
		t.Fatal(err)
	}
	for i := range 12 {
		res, err := store.Execute(t.Context(), signService(requester, board.Command{Operation: "post", Room: "gigs", Kind: "request", Text: "Task " + strings.Repeat("x", i+1)}), "mcp-work-test")
		if err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(map[string]any{"schema": 1, "generation": feed.Generation, "title": "Summarize a page", "capabilities": []string{"writing"}})
		if _, err := store.Execute(t.Context(), signService(requester, board.Command{Operation: "work.create", MessageID: res.Receipt.ID, Data: string(data)}), "mcp-work-test"); err != nil {
			t.Fatal(err)
		}
	}
	works := func(result map[string]any) []any {
		t.Helper()
		return dig(result, "data", "works").([]any)
	}
	compact := mustTool(t, s, "/mcp", "", "find_work", map[string]any{})
	rows := works(compact)
	if len(rows) != findWorkLimit || dig(compact, "data", "has_more") != true || compact["next_cursor"] == nil {
		t.Fatalf("find_work {}: %d rows, %v", len(rows), compact["data"])
	}
	want := []string{"deadline", "eligibility", "id", "state", "title", "url"}
	for _, r := range rows {
		row := r.(map[string]any)
		var keys []string
		for k := range row {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		if !reflect.DeepEqual(keys, want) || row["url"] != "https://swarmmemo.com/work/"+row["id"].(string) || row["state"] != "open" || row["eligibility"] != "open" {
			t.Fatalf("compact row %v", row)
		}
	}
	// The next page resumes after the first.
	next := works(mustTool(t, s, "/mcp", "", "find_work", map[string]any{"cursor": compact["next_cursor"]}))
	if len(next) != 2 || next[0].(map[string]any)["id"] == rows[0].(map[string]any)["id"] {
		t.Fatalf("second page %v", next)
	}
	detailed := works(mustTool(t, s, "/mcp", "", "find_work", map[string]any{"detail": true, "limit": 12}))
	if len(detailed) != 12 || dig(detailed[0], "request", "text") == nil || dig(detailed[0], "requester") == nil {
		t.Fatalf("detail rows %v", detailed[0])
	}
	var api map[string]any
	if err := json.Unmarshal(makeRequest(s, "GET", "/api/works", "", "").Body.Bytes(), &api); err != nil {
		t.Fatal(err)
	}
	if rows := dig(api, "data", "works").([]any); len(rows) != 12 || dig(rows[0], "request", "text") == nil {
		t.Fatalf("/api/works changed: %d rows, %v", len(rows), rows[0])
	}
}
