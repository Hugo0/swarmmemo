package web

import (
	"net/http/httptest"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// C97: a verifier's signed per-property checks render as a small table where
// its verdict shows: under a link witness on the agent page, and under a
// verdict in the work page's history. Participant text stays escaped.
var webChecks = []board.VerdictCheck{
	{Property: "integrity", State: "pass", SubjectSHA256: strings.Repeat("ab", 32), Tool: "<b>sha</b>@1", Evidence: strings.Repeat("c", 32)},
	{Property: "issuer_auth", State: "not_checkable", Evidence: "https://example.org/proof"},
	{Property: "anchor", State: "fail", Evidence: "javascript:alert(1)"},
	{Property: "conformance", State: "not_checked"},
}

func checkTable(t *testing.T, body string) {
	t.Helper()
	i := strings.Index(body, `<div class="verdict-checks">`)
	if i < 0 {
		t.Fatal("no checks table")
	}
	table := body[i:]
	table = table[:strings.Index(table, "</div>")]
	for _, want := range []string{
		`<th scope="col">Property</th><th scope="col">State</th><th scope="col">Subject</th><th scope="col">Tool</th><th scope="col">Evidence</th>`,
		`<td><code>integrity</code></td><td class="check-state-pass">pass</td><td><code title="` + strings.Repeat("ab", 32) + `">` + strings.Repeat("ab", 6) + `</code></td><td>&lt;b&gt;sha&lt;/b&gt;@1</td><td><a href="/e/` + strings.Repeat("c", 32) + `" rel="nofollow noopener ugc">`,
		`<a href="https://example.org/proof" rel="nofollow noopener ugc">https://example.org/proof</a>`,
		`<td class="check-state-fail">fail</td><td><span class="muted">—</span></td><td><span class="muted">—</span></td><td><code>javascript:alert(1)</code></td>`,
		`<td class="check-state-not_checked">not_checked</td>`,
	} {
		if !strings.Contains(table, want) {
			t.Errorf("checks table lacks %q in %s", want, table)
		}
	}
	if strings.Contains(table, `href="javascript`) || strings.Contains(table, "<b>sha") {
		t.Fatal("participant text reached the page unescaped")
	}
}

func TestAgentPageShowsWitnessChecks(t *testing.T) {
	agent := &board.Agent{ID: strings.Repeat("a", 64), Links: []board.IdentityLink{
		{Kind: "ed25519", Value: "KEYVALUE", State: "proof_attached", Method: "ed25519-signature", Proof: "sig", LinkedAt: 1, Witnessed: 1, Witnesses: []board.LinkWitness{
			{Fingerprint: strings.Repeat("b", 64), PublicKey: "WITNESSKEY", Verdict: "verified", Nonce: "n", At: 1790000000, Checks: webChecks}}},
	}}
	s := &testService{execute: func(c board.Command) (board.Result, error) {
		if c.Operation == "agent.get" {
			return board.Result{OK: true, Agent: agent}, nil
		}
		return board.Result{OK: true}, nil
	}}
	w := httptest.NewRecorder()
	Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/agent/"+agent.ID, nil))
	if w.Code != 200 {
		t.Fatalf("agent page %d", w.Code)
	}
	checkTable(t, w.Body.String())
}

func TestWorkPageShowsVerdictChecks(t *testing.T) {
	item := board.Work{ID: webWorkID, Room: "lobby", Title: "Checked work", State: "accepted", StoredState: "accepted", Requester: board.AgentRef{ID: strings.Repeat("2", 64)}, Capabilities: []string{}, Deadline: 1789171200}
	transitions := []board.WorkTransition{
		{Sequence: 1, Operation: "work.create", Author: strings.Repeat("2", 64), State: "open"},
		{Sequence: 2, Operation: "work.accept", Author: strings.Repeat("2", 64), State: "accepted", Checks: webChecks},
	}
	s := &testService{execute: func(c board.Command) (board.Result, error) {
		switch c.Operation {
		case "room.get":
			return board.Result{OK: true, Room: &board.Room{Name: c.Room, Visibility: "public"}}, nil
		case "work.get":
			return board.Result{OK: true, Data: map[string]any{"work": item}}, nil
		case "work.history":
			return board.Result{OK: true, Data: map[string]any{"work_id": webWorkID, "transitions": transitions}}, nil
		}
		return board.Result{OK: true}, nil
	}}
	w := httptest.NewRecorder()
	Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/work/"+webWorkID, nil))
	body := w.Body.String()
	if w.Code != 200 || strings.Count(body, `class="verdict-checks"`) != 1 {
		t.Fatalf("work page %d: %d tables", w.Code, strings.Count(body, `class="verdict-checks"`))
	}
	checkTable(t, body)
}
