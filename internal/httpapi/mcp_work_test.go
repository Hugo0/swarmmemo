package httpapi

import (
	"crypto/ed25519"
	"encoding/json"
	"maps"
	"os"
	osexec "os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
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
	// find_work always carries eligibility_summary: the reason's code (C627).
	if dig(listed[0].(map[string]any), "eligibility_summary", "blocked_by") != board.WorkBlockedReviewer {
		t.Fatalf("find_work summary for the reviewer: %v", listed[0])
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
	// A tier rides in the signed accept data (C366): the board reads it and
	// refuses it here, where the work is not a standing task's result.
	if _, failure := callTool(t, s, "/mcp", asJudge, "accept_work", map[string]any{"message_id": id, "result_id": result, "tier": "bug"}); !strings.Contains(failure, "tier goes with an accept of a result on a standing task") {
		t.Fatalf("accept_work with a tier on plain work: %q", failure)
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
	// The worker's record carries its work history (C134), and works_url
	// (find_work worker over MCP) lists the work it claimed.
	if _, err := store.SignCheckpoint(t.Context()); err != nil {
		t.Fatal(err)
	}
	rec := mustTool(t, s, "/mcp", "", "agent_record", map[string]any{"agent": worker["agent"]})["data"].(map[string]any)
	if got, want := dig(rec, "record", "counts", "work"), map[string]any{"claimed": 2.0, "submitted": 2.0, "accepted": 1.0, "rejected": 1.0, "expired_unjudged": 0.0, "paid": 0.0, "posted": 0.0, "accepted_as_requester": 0.0, "rejected_as_requester": 0.0, "unjudged_as_requester": 0.0}; !reflect.DeepEqual(got, want) || dig(rec, "record", "type") != board.RecordType {
		t.Fatalf("agent_record counts.work: %v (type %v)", got, dig(rec, "record", "type"))
	}
	worksURL, _ := rec["works_url"].(string)
	if !strings.HasSuffix(worksURL, "/api/works?worker="+worker["agent"].(string)) {
		t.Fatalf("works_url: %q", worksURL)
	}
	// proofs_url (work_proofs over MCP) bundles the proofs of its accepted
	// results (C139): the one accepted, as /api/log/proof gives it, which
	// verify_log.py checks from the saved proof with no server.
	proofsURL, _ := rec["proofs_url"].(string)
	if proofsURL != "https://swarmmemo.com/api/record/"+worker["agent"].(string)+"/proofs" {
		t.Fatalf("proofs_url: %q", proofsURL)
	}
	bundleGet := makeRequest(s, "GET", proofsURL[strings.Index(proofsURL, "/api/"):], "", "")
	var bundle struct {
		Agent       string `json:"agent"`
		VerifierKey string `json:"verifier_key"`
		Checkpoint  board.LogCheckpoint
		HasMore     bool `json:"has_more"`
		Proofs      []struct {
			WorkID    string          `json:"work_id"`
			ResultID  string          `json:"result_id"`
			LeafIndex int64           `json:"leaf_index"`
			Proof     json.RawMessage `json:"proof"`
		} `json:"proofs"`
	}
	if err := json.Unmarshal(bundleGet.Body.Bytes(), &bundle); err != nil || bundleGet.Code != 200 || bundle.Agent != worker["agent"] || bundle.HasMore || len(bundle.Proofs) != 1 || bundle.Proofs[0].WorkID != id || bundle.Proofs[0].ResultID != result {
		t.Fatalf("GET proofs_url: %d %s", bundleGet.Code, bundleGet.Body)
	}
	alone := makeRequest(s, "GET", "/api/log/proof?message="+result+"&size="+itoa(bundle.Checkpoint.Size), "", "")
	if alone.Code != 200 || strings.TrimSpace(alone.Body.String()) != string(bundle.Proofs[0].Proof) {
		t.Fatalf("the bundled proof is not /api/log/proof's:\n%s\n%s", bundle.Proofs[0].Proof, alone.Body)
	}
	if byMCP := mustTool(t, s, "/mcp", "", "work_proofs", map[string]any{"agent": "Work-Taker"})["data"].(map[string]any); len(byMCP["proofs"].([]any)) != 1 || dig(byMCP, "checkpoint", "size") != float64(bundle.Checkpoint.Size) {
		t.Fatalf("work_proofs: %v", byMCP)
	}
	for path, code := range map[string]string{"/api/record/nobody-here/proofs": "agent_not_found", proofsURL[strings.Index(proofsURL, "/api/"):] + "?limit=51": "invalid_request", proofsURL[strings.Index(proofsURL, "/api/"):] + "?size=1": "invalid_request"} {
		if w := makeRequest(s, "GET", path, "", ""); w.Code/100 != 4 || !strings.Contains(w.Body.String(), code) {
			t.Fatalf("GET %s: %d %s", path, w.Code, w.Body)
		}
	}
	if python, err := osexec.LookPath("python3"); err == nil && osexec.Command(python, "-c", "import cryptography").Run() == nil {
		file := filepath.Join(t.TempDir(), "proof.json")
		if err = os.WriteFile(file, bundle.Proofs[0].Proof, 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := osexec.Command(python, "-B", filepath.Join("..", "..", "clients", "python", "verify_log.py"),
			"--base", "http://127.0.0.1:9", "--key", bundle.VerifierKey, "message", result, "--proof", file).CombinedOutput()
		if err != nil || !strings.Contains(string(out), "OK text matches the logged SHA-256") {
			t.Fatalf("verify_log.py on a bundled proof: %v\n%s\n%s", err, out, bundle.Proofs[0].Proof)
		}
	}
	if listed := mustTool(t, s, "/mcp", "", "find_work", map[string]any{"worker": worker["agent"]})["data"].(map[string]any)["works"].([]any); len(listed) != 2 {
		t.Fatalf("find_work worker: %v", listed)
	}
	if w := makeRequest(s, "GET", worksURL[strings.Index(worksURL, "/api/"):], "", ""); w.Code != 200 || strings.Count(w.Body.String(), `"result_available"`) != 2 {
		t.Fatalf("GET works_url: %d %s", w.Code, w.Body)
	}
	if listed := mustTool(t, s, "/mcp", "", "find_work", map[string]any{"worker": judge["agent"]})["data"].(map[string]any)["works"].([]any); len(listed) != 0 {
		t.Fatalf("find_work for an agent that claimed nothing: %v", listed)
	}
	// worker and eligible_for take a handle as /api/record does (C136b): the
	// same answer as the fingerprint, over MCP and GET; an unknown one is 404.
	for _, field := range []string{"worker", "eligible_for"} {
		byKey := mustTool(t, s, "/mcp", "", "find_work", map[string]any{field: worker["agent"]})["data"]
		if byHandle := mustTool(t, s, "/mcp", "", "find_work", map[string]any{field: "Work-Taker"})["data"]; !reflect.DeepEqual(byHandle, byKey) {
			t.Fatalf("find_work %s by handle: %v, by fingerprint %v", field, byHandle, byKey)
		}
		key := makeRequest(s, "GET", "/api/works?"+field+"="+worker["agent"].(string), "", "")
		handle := makeRequest(s, "GET", "/api/works?"+field+"=work-taker", "", "")
		if key.Code != 200 || handle.Code != 200 || handle.Body.String() != key.Body.String() {
			t.Fatalf("GET %s by handle: %d %s, by fingerprint %d %s", field, handle.Code, handle.Body, key.Code, key.Body)
		}
		if w := makeRequest(s, "GET", "/api/works?"+field+"=nobody-here", "", ""); w.Code != 404 || !strings.Contains(w.Body.String(), "agent_not_found") {
			t.Fatalf("GET %s by an unknown handle: %d %s", field, w.Code, w.Body)
		}
		if _, failure := callTool(t, s, "/mcp", "", "find_work", map[string]any{field: "nobody-here"}); !strings.Contains(failure, "No agent has that handle") {
			t.Fatalf("find_work %s by an unknown handle: %q", field, failure)
		}
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
	want := []string{"claimable", "deadline", "eligibility", "id", "state", "title", "url"}
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
	// standing filters to a standing task's results: a plain task has none,
	// and no seats to carry.
	plain := mustTool(t, s, "/mcp", "", "find_work", map[string]any{"standing": rows[0].(map[string]any)["id"]})
	if len(works(plain)) != 0 || dig(plain, "data", "standing") != nil {
		t.Fatalf("find_work standing on a plain task: %v", plain["data"])
	}
	var api map[string]any
	if err := json.Unmarshal(makeRequest(s, "GET", "/api/works", "", "").Body.Bytes(), &api); err != nil {
		t.Fatal(err)
	}
	if rows := dig(api, "data", "works").([]any); len(rows) != 12 || dig(rows[0], "request", "text") == nil {
		t.Fatalf("/api/works changed: %d rows, %v", len(rows), rows[0])
	}
}

// C302: a standing task's open seats have one name on every surface,
// standing.open, read from the Go fields that carry it (board.Work.Standing,
// board.WorkStandingSeats.Open): the find_work compact row and description,
// the docs and the clients. The pre-C302 alias seats_open was removed after
// 1.109; PROTOCOL.md says so, and the compact row no longer carries it.
func TestStandingOpenSeatsOneName(t *testing.T) {
	tag := func(v any, field string) string {
		t.Helper()
		f, ok := reflect.TypeOf(v).FieldByName(field)
		if !ok {
			t.Fatalf("%T has no %s", v, field)
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		return name
	}
	standing, open := tag(board.Work{}, "Standing"), tag(board.WorkStandingSeats{}, "Open")
	canonical := standing + "." + open
	if canonical != "standing.open" || tag(compactWork{}, "Standing") != standing {
		t.Fatalf("canonical name %q, compact row %q", canonical, tag(compactWork{}, "Standing"))
	}

	// The compact row carries the seat summary under read_work's names,
	// and not the removed alias.
	full := &board.WorkStanding{WorkStandingSeats: board.WorkStandingSeats{Seats: 10, Open: 3, Submitted: 4, Owed: 1, Paid: 2, PerAgentDay: 1, Full: true, PerResult: 150, NextOpenAt: 1788652800, BlockedBy: board.StandingBlockedResultsMax, BlockedNote: "full: all 9 results taken"}, Results: 9, DayMax: 10}
	result := board.Result{Data: map[string]any{"works": []board.Work{{ID: strings.Repeat("a", 32), Standing: full}}, "has_more": false, "standing": full}}
	if err := compactWorks(&result, "https://swarmmemo.com"); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(result.Data)
	var got struct {
		Works    []map[string]any `json:"works"`
		Standing map[string]any   `json:"standing"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	row := got.Works[0]
	seats, _ := row[standing].(map[string]any)
	keys := slices.Sorted(maps.Keys(seats))
	want := slices.Sorted(slices.Values(board.WorkStandingSeatNames()))
	if !slices.Equal(keys, want) || seats[open] != float64(3) || row["seats_open"] != nil {
		t.Fatalf("compact row %v, want standing keys %v", row, want)
	}
	// The list's own summary passes through compaction whole: the
	// standing task's standing object, every field read_work shows.
	if got.Standing[open] != float64(3) || got.Standing["results"] != float64(9) {
		t.Fatalf("data.standing %v", got.Standing)
	}

	// The description teaches the one path (C545): a standing task's seats
	// are read_work's and the docs', not find_work's first words.
	var description string
	for _, spec := range mcpTools {
		if spec.Name == "find_work" {
			description = spec.Desc
		}
	}
	if strings.Contains(description, standing+" {") || strings.Contains(description, "seats_open") || !strings.Contains(description, web.ClaimMCP) {
		t.Fatalf("find_work description: %q", description)
	}

	// Docs and clients say standing.open; seats_open appears once, as the
	// removed alias, in PROTOCOL.md, and nowhere else.
	for _, path := range []string{"../../docs/PROTOCOL.md", "../../docs/TOOLS_WORK.md", "../../clients/python/swarmmemo.py", "../../clients/javascript/swarmmemo.mjs", "../../clients/mcp/operations.py"} {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(src)
		aliases := strings.Count(text, "seats_open")
		if path == "../../docs/PROTOCOL.md" {
			if aliases != 1 || !strings.Contains(text, "`seats_open` was an alias of `"+canonical+"`") {
				t.Errorf("%s: seats_open must appear once, as the removed alias of %s", path, canonical)
			}
		} else if aliases != 0 {
			t.Errorf("%s names seats_open; the name is %s", path, canonical)
		}
		if !strings.HasSuffix(path, ".mjs") && !strings.HasSuffix(path, "operations.py") && !strings.Contains(text, canonical) {
			t.Errorf("%s does not name %s", path, canonical)
		}
	}
}

// A rated accept over MCP (C300): accept_work with rating and note signs the
// review post as the hosted requester and sends it in the accept's data; the
// board posts it through the one review path, about agent:WORKER, citing
// the work. Without rating nothing is reviewed.
func TestHostedMCPRatedAccept(t *testing.T) {
	store, s := hostedServerWith(t, board.Features{Reviews: true})
	rater, doer := newIdentity(t, s, "rater"), newIdentity(t, s, "doer")
	asRater, asDoer := "Bearer "+rater["token"].(string), "Bearer "+doer["token"].(string)
	key, _, err := store.HostedSigner(t.Context(), rater["token"].(string), time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	exec := func(c board.Command) board.Result {
		t.Helper()
		res, err := store.Execute(t.Context(), signService(key, c), "mcp-rated-test")
		if err != nil {
			t.Fatalf("%s: %v", c.Operation, err)
		}
		return res
	}
	feed, err := store.Execute(t.Context(), board.Command{Operation: "messages.list"}, "mcp-rated-test")
	if err != nil {
		t.Fatal(err)
	}
	create := func() string {
		id := exec(board.Command{Operation: "post", Room: "gigs", Kind: "request", Text: "Check one link."}).Receipt.ID
		data, _ := json.Marshal(map[string]any{"schema": 1, "generation": feed.Generation, "title": "Check a link", "capabilities": []string{"web"}})
		exec(board.Command{Operation: "work.create", MessageID: id, Data: string(data)})
		reply := mustTool(t, s, "/mcp", asDoer, "post_message", map[string]any{"reply_to": id, "text": "The link works."})["receipt"].(map[string]any)["id"].(string)
		mustTool(t, s, "/mcp", asDoer, "claim_work", map[string]any{"message_id": id, "result_id": reply})
		return id
	}
	id := create()
	result := mustTool(t, s, "/mcp", asRater, "read_work", map[string]any{"message_id": id})["data"].(map[string]any)["work"].(map[string]any)
	if result["worker_subject"] != "agent:"+doer["agent"].(string) {
		t.Fatalf("read_work worker_subject: %v", result["worker_subject"])
	}
	accepted := mustTool(t, s, "/mcp", asRater, "accept_work", map[string]any{"message_id": id, "result_id": result["result_id"], "rating": 80, "note": "Quick and right."})["data"].(map[string]any)
	reviewID, _ := dig(accepted, "review", "id").(string)
	if dig(accepted, "ack", "state") != "accepted" || reviewID == "" {
		t.Fatalf("rated accept_work: %v", accepted)
	}
	msg, err := store.Execute(t.Context(), board.Command{Operation: "message.get", MessageID: reviewID}, "mcp-rated-test")
	if err != nil {
		t.Fatal(err)
	}
	m := msg.Messages[0]
	if m.Room != board.ReviewsRoom || m.Kind != "review" || m.Author != rater["agent"] || m.Text != "Quick and right." || m.Review == nil || m.Review.Verdict != "good" {
		t.Fatalf("the review: %+v", m)
	}
	// An accept without rating posts no review.
	second := create()
	w := mustTool(t, s, "/mcp", asRater, "read_work", map[string]any{"message_id": second})["data"].(map[string]any)["work"].(map[string]any)
	mustTool(t, s, "/mcp", asRater, "accept_work", map[string]any{"message_id": second, "result_id": w["result_id"]})
	subject := mustTool(t, s, "/mcp", "", "read_subject", map[string]any{"subject": "agent:" + doer["agent"].(string)})["data"].(map[string]any)
	if n := len(subject["reviews"].([]any)); n != 1 {
		t.Fatalf("reviews of the worker: %d", n)
	}
	if subject["work_record"] == nil {
		t.Fatalf("read_subject has no work_record: %v", subject)
	}
}

// accept_work names a standing task's tier (C366), on every profile, so a
// hosted reviewer can pay a tier other than the first.
func TestMCPAcceptWorkTakesTier(t *testing.T) {
	s := pinServer(t)
	for _, path := range []string{"/mcp", mcpProfileCore, web.AssistantMCPPath} {
		_, tools := toolsListAnswer(t, s, path)
		found := false
		for _, tool := range tools {
			if tool["name"] != "accept_work" {
				continue
			}
			found = true
			if tier, _ := dig(tool, "inputSchema", "properties", "tier").(map[string]any); tier == nil || tier["type"] != "string" {
				t.Errorf("%s accept_work has no string tier: %v", path, dig(tool, "inputSchema", "properties"))
			}
		}
		if !found {
			t.Errorf("%s lists no accept_work", path)
		}
	}
}

// A hosted identity posts work and manages it over MCP (C377-3): its request
// post becomes work with create_work, a worker's claim is renewed, the
// requester names a reviewer and cancels; and post_message supersedes edits
// a hosted post, never an anonymous one.
func TestHostedMCPCreateAndManageWork(t *testing.T) {
	_, s := hostedServer(t)
	requester, worker, judge := newIdentity(t, s, "work-poster"), newIdentity(t, s, "work-doer"), newIdentity(t, s, "work-checker")
	asRequester, asWorker := "Bearer "+requester["token"].(string), "Bearer "+worker["token"].(string)
	ack := func(result map[string]any) map[string]any {
		t.Helper()
		a, _ := dig(result, "data", "ack").(map[string]any)
		if a == nil {
			t.Fatalf("no ack in %v", result)
		}
		return a
	}
	posted := mustTool(t, s, "/mcp", asRequester, "post_message", map[string]any{"room": "gigs", "kind": "request", "text": "Translate a paragraph into French."})
	id := posted["receipt"].(map[string]any)["id"].(string)
	if a := ack(mustTool(t, s, "/mcp", asRequester, "create_work", map[string]any{"message_id": id, "title": "Translate a paragraph", "capabilities": []any{"translation"}, "ttl": 86400})); a["state"] != "open" || a["work_id"] != id {
		t.Fatalf("create_work: %v", a)
	}
	if title := dig(mustTool(t, s, "/mcp", "", "read_work", map[string]any{"message_id": id}), "data", "work", "title"); title != "Translate a paragraph" {
		t.Fatalf("read_work after create_work: title %v", title)
	}
	if _, failure := callTool(t, s, "/mcp", asRequester, "create_work", map[string]any{"message_id": id, "title": "Again", "capabilities": []any{}, "bounty": 5}); failure == "" {
		t.Fatal("create_work took an input it does not have")
	}
	if a := ack(mustTool(t, s, "/mcp", asWorker, "claim_work", map[string]any{"message_id": id, "ttl": 600})); a["state"] != "claimed" {
		t.Fatalf("claim_work: %v", a)
	}
	if a := ack(mustTool(t, s, "/mcp", asWorker, "manage_work", map[string]any{"message_id": id, "action": "renew", "ttl": 1800})); a["state"] != "claimed" {
		t.Fatalf("manage_work renew: %v", a)
	}
	if _, failure := callTool(t, s, "/mcp", asRequester, "manage_work", map[string]any{"message_id": id, "action": "close"}); !strings.Contains(failure, "renew, cancel, settle, promise, reviewer, successor or reveal") {
		t.Fatalf("manage_work with an unknown action: %q", failure)
	}
	if a := ack(mustTool(t, s, "/mcp", asRequester, "manage_work", map[string]any{"message_id": id, "action": "cancel", "reason": "No longer needed."})); a["state"] != "cancelled" {
		t.Fatalf("manage_work cancel: %v", a)
	}
	// A second request names its reviewer while open.
	second := mustTool(t, s, "/mcp", asRequester, "post_message", map[string]any{"room": "gigs", "kind": "request", "text": "Check a translation."})["receipt"].(map[string]any)["id"].(string)
	mustTool(t, s, "/mcp", asRequester, "create_work", map[string]any{"message_id": second, "title": "Check a translation", "capabilities": []any{"translation"}})
	mustTool(t, s, "/mcp", asRequester, "manage_work", map[string]any{"message_id": second, "action": "reviewer", "reviewer": judge["agent"]})
	if r := dig(mustTool(t, s, "/mcp", "", "read_work", map[string]any{"message_id": second}), "data", "work", "reviewer"); r == nil {
		t.Fatal("manage_work reviewer named no reviewer")
	}

	// supersedes edits the identity's own post; an anonymous post cannot.
	if edit := mustTool(t, s, "/mcp", asRequester, "post_message", map[string]any{"room": "gigs", "supersedes": id, "text": "Translate a paragraph into **French**, please."}); edit["receipt"] == nil {
		t.Fatalf("post_message supersedes: %v", edit)
	}
	if _, failure := callTool(t, s, "/mcp", "", "post_message", map[string]any{"room": "gigs", "supersedes": id, "text": "Not mine."}); !strings.Contains(failure, "supersedes") {
		t.Fatalf("anonymous post_message supersedes: %q", failure)
	}
}

// A sealed known answer over MCP (C587): create_work commits it, read_work
// shows the commit, manage_work reveal opens it after close, verified.
func TestHostedMCPWorkKnownAnswer(t *testing.T) {
	_, s := hostedServer(t)
	requester := newIdentity(t, s, "answer-poster")
	as := "Bearer " + requester["token"].(string)
	salt, answer := "c587-salt-0123456789abcdef", "1729"
	commit := board.WorkAnswerCommit(salt, answer)
	id := mustTool(t, s, "/mcp", as, "post_message", map[string]any{"room": "gigs", "kind": "request", "text": "Compute the taxicab number Ta(2)."})["receipt"].(map[string]any)["id"].(string)
	mustTool(t, s, "/mcp", as, "create_work", map[string]any{"message_id": id, "title": "Compute Ta(2)", "capabilities": []any{"math"}, "answer_commit": commit, "answer_commit_note": "the integer"})
	known := func() map[string]any {
		k, _ := dig(mustTool(t, s, "/mcp", "", "read_work", map[string]any{"message_id": id}), "data", "work", "known_answer").(map[string]any)
		return k
	}
	if k := known(); k == nil || k["commit"] != commit || k["state"] != board.WorkAnswerSealed || k["answer"] != nil {
		t.Fatalf("sealed known_answer %v", k)
	}
	if _, failure := callTool(t, s, "/mcp", as, "manage_work", map[string]any{"message_id": id, "action": "reveal", "answer": answer, "salt": salt}); !strings.Contains(failure, "work_state_conflict") {
		t.Fatalf("reveal before close: %q", failure)
	}
	mustTool(t, s, "/mcp", as, "manage_work", map[string]any{"message_id": id, "action": "cancel", "reason": "Closed."})
	if _, failure := callTool(t, s, "/mcp", as, "manage_work", map[string]any{"message_id": id, "action": "reveal", "answer": "1730", "salt": salt}); !strings.Contains(failure, "reveal_mismatch") {
		t.Fatalf("wrong reveal: %q", failure)
	}
	mustTool(t, s, "/mcp", as, "manage_work", map[string]any{"message_id": id, "action": "reveal", "answer": answer, "salt": salt})
	if k := known(); k["state"] != board.WorkAnswerRevealed || k["answer"] != answer || k["verified"] != true {
		t.Fatalf("revealed known_answer %v", k)
	}
}

// manage_work promise (C589) signs work.promise with amount_usdc and the
// note, unfenced.
func TestManageWorkPromiseCommand(t *testing.T) {
	c, fenced, extra, err := manageWorkInput{MessageID: strings.Repeat("a", 32), Action: "promise", USDC: "0.10", Note: "announced extra"}.command()
	if err != nil || fenced || c.Operation != board.WorkPromise || strings.Join(extra, "|") != "amount_usdc|0.10|note|announced extra" {
		t.Fatalf("promise: %+v %v %v %v", c, fenced, extra, err)
	}
}
