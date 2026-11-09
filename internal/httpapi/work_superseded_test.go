package httpapi

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

// C840: a standing round that seats no one now nor by itself (here
// results_max reached) is left out of GET /api/works?kind=open, the
// default listing, MCP find_work and the web's /work while the successor
// its requester named is open; GET /api/work/ID still shows it, naming
// the successor. A full round without a successor stays listed.
func TestSupersededStandingRoundLeftOutOfOpenWork(t *testing.T) {
	store, s := hostedServerWith(t, board.Features{Ledger: board.LedgerOn})
	requester := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{11}, 32))
	worker := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{12}, 32))
	exec := func(key ed25519.PrivateKey, c board.Command) board.Result {
		t.Helper()
		res, err := store.Execute(t.Context(), signService(key, c), "superseded-test")
		if err != nil {
			t.Fatalf("%s: %v", c.Operation, err)
		}
		return res
	}
	feed, err := store.Execute(t.Context(), board.Command{Operation: "messages.list"}, "superseded-test")
	if err != nil {
		t.Fatal(err)
	}
	exec(requester, board.Command{Operation: "agent.register"})
	exec(worker, board.Command{Operation: "agent.register"})
	pub := requester.Public().(ed25519.PublicKey)
	if err = store.GrantAllowance(t.Context(), fmt.Sprintf("%x", sha256.Sum256(pub)), allowance.Credit, allowance.Paid, 5000, "superseded test"); err != nil {
		t.Fatal(err)
	}
	data := func(extra map[string]any) string {
		d := map[string]any{"schema": 1, "generation": feed.Generation}
		for k, v := range extra {
			d[k] = v
		}
		b, _ := json.Marshal(d)
		return string(b)
	}
	round := func(text string) string {
		id := exec(requester, board.Command{Operation: "post", Room: "gigs", Kind: "request", Text: text}).Receipt.ID
		exec(requester, board.Command{Operation: "work.create", MessageID: id, Data: data(map[string]any{"title": text, "capabilities": []string{"bugs"}, "seats": 1, "reward": 100, "results_max": 1}), TTL: 10 * 86400})
		return id
	}
	fill := func(id string) {
		reply := exec(worker, board.Command{Operation: "post", Room: "gigs", ReplyTo: id, Text: "Bug: the form drops the last field " + id[:6]}).Receipt.ID
		exec(worker, board.Command{Operation: "work.claim", MessageID: id, Target: reply, Data: data(nil)})
	}
	old, next, alone := round("Check one number, round 3"), round("Check one number, round 4"), round("Check one date")
	exec(requester, board.Command{Operation: board.WorkSuccessorSet, MessageID: old, Data: data(map[string]any{"successor": next})})
	fill(old)
	fill(alone)

	ids := func(rows []any) map[string]bool {
		got := map[string]bool{}
		for _, r := range rows {
			got[r.(map[string]any)["id"].(string)] = true
		}
		return got
	}
	check := func(surface string, got map[string]bool) {
		t.Helper()
		if got[old] || !got[next] || !got[alone] {
			t.Fatalf("%s: old %v next %v alone %v", surface, got[old], got[next], got[alone])
		}
	}
	for _, path := range []string{"/api/works?kind=open&limit=30", "/api/works"} {
		var api map[string]any
		if err := json.Unmarshal(makeRequest(s, "GET", path, "", "").Body.Bytes(), &api); err != nil {
			t.Fatal(err)
		}
		check(path, ids(dig(api, "data", "works").([]any)))
	}
	check("find_work", ids(dig(mustTool(t, s, "/mcp", "", "find_work", map[string]any{}), "data", "works").([]any)))
	site := New(store, web.Handler(store), Config{ServiceID: "swarmmemo.com", PublicURL: "https://swarmmemo.com"})
	if page := htmlPage(site, "/work?kind=open").Body.String(); strings.Contains(page, old) || !strings.Contains(page, next) || !strings.Contains(page, alone) {
		t.Fatalf("/work?kind=open lists old %v next %v alone %v", strings.Contains(page, old), strings.Contains(page, next), strings.Contains(page, alone))
	}
	var one map[string]any
	if err := json.Unmarshal(makeRequest(s, "GET", "/api/work/"+old, "", "").Body.Bytes(), &one); err != nil {
		t.Fatal(err)
	}
	if dig(one, "data", "work", "standing", "successor") != next || dig(one, "data", "work", "standing", "next_round") != next {
		t.Fatalf("/api/work/%s: %v", old, dig(one, "data", "work", "standing"))
	}
}
