package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

// /trust, the illustrated allowance and trust guide. Invariant 9: every live
// number on it is a cell held to the JSON API that serves the same store
// function: the waterfall and the trust distribution to /api/stats/allowance,
// the capture bound to /api/trust/runs/ID.

// The trust runs of the fixture store: the newest did not finish, so the page
// shows run 812, the latest one with a capture bound.
var explainerRuns = []map[string]any{
	{"id": int64(813), "as_of": int64(1759276800), "state": "aborted", "nodes": int64(0), "edges": int64(0)},
	{"id": int64(812), "as_of": int64(1759190400), "state": "done", "nodes": int64(64), "edges": int64(211)},
}

const explainerRunFixture = `{"id":812,"as_of":1759190400,"params_version":1,"state":"done","nodes":64,"edges":211,"work":90210,
 "output_sha256":"20dc04783af41f8fb948d7b2a9f83093c5ca0cb94b29a0bdc8ae77a819719b10","error":"","started_at":1759192000,"finished_at":1759192210,
 "capture_bound":{"unit_per_share":20,"edge_cap_units":20,"lambda_ppm":500000,"max_transit_units":7,"pool_units":1280,
  "statement":"Whatever the number of sybils, a region behind k attack edges receives at most k x edge_cap_units flow units, and at most max_transit_units through any one non-seed endorser."},
 "evidence":0}`

func (s *rfc0012Service) TrustRuns(ctx context.Context, before int64, limit int) ([]map[string]any, int64, error) {
	return explainerRuns[:min(limit, len(explainerRuns))], 0, nil
}

func (s *rfc0012Service) TrustRun(ctx context.Context, id int64) (map[string]any, error) {
	if id != 812 {
		return nil, &board.Error{Status: 404, Code: "not_found", Message: "No trust run with that ID."}
	}
	return fixtureMap(explainerRunFixture), nil
}

func (s *rfc0012Service) TrustEvidence(ctx context.Context, before int64, limit int) ([]map[string]any, int64, error) {
	return []map[string]any{}, 0, nil
}

// section returns the page from the element with this id to the next live
// block or scene, so each block's cells are held to their own API.
func section(t *testing.T, page, id string) string {
	t.Helper()
	start := strings.Index(page, `id="`+id+`"`)
	if start < 0 {
		t.Fatalf("/trust lacks #%s", id)
	}
	rest := page[start+1:]
	end := len(rest)
	for _, next := range []string{`class="tx-live"`, `class="tx-ch`, `class="tx-lookup"`, "</article>"} {
		if i := strings.Index(rest, next); i >= 0 && i < end {
			end = i
		}
	}
	return rest[:end]
}

func TestTrustExplainerMatchesAPI(t *testing.T) {
	s, _ := rfc0012Server(allOn)
	page := getHTML(t, s, "/trust")
	stats := getJSON(t, s, "GET", "/api/stats/allowance", "")["data"]
	holdToAPI(t, "/trust waterfall", pageCells(t, section(t, page, "waterfall-live")), stats)
	holdToAPI(t, "/trust distribution", pageCells(t, section(t, page, "distribution-live")), stats)
	run := getJSON(t, s, "GET", "/api/trust/runs/812", "")["data"]
	runCells := pageCells(t, section(t, page, "run-live"))
	holdToAPI(t, "/trust latest run", runCells, run)
	keys := map[string]bool{}
	for _, c := range runCells {
		keys[c[0]] = true
	}
	for _, want := range []string{"id", "capture_bound.edge_cap_units", "capture_bound.max_transit_units", "capture_bound.pool_units", "capture_bound.lambda_ppm"} {
		if !keys[want] {
			t.Errorf("/trust does not show the latest run's %s", want)
		}
	}
	hash, _ := lookup(run, "output_sha256")
	statement, _ := lookup(run, "capture_bound.statement")
	for _, want := range []string{hash.(string), statement.(string), `href="/api/trust/runs/812"`, web.WaterfallSentence,
		"recompute.py verify endorsements.jsonl", "recompute.py run snapshot.jsonl | sha256sum", "/api/agent/AGENT/trust",
		`id="arrive"`, `id="million"`, `id="pour"`, `id="collateral"`, `id="flows"`, `id="vouch"`, `id="rings"`, `id="levers"`, `id="check"`, `data-fig="live"`,
		`<script defer src="/assets/trust.js"></script>`, `<link rel="stylesheet" href="/assets/trust.css">`, `content="light dark"`, `class="tx-theme"`} {
		if !strings.Contains(page, want) {
			t.Errorf("/trust lacks %q", want)
		}
	}
	// The waterfall rows are posting's tiers 1 to 4, in order: the drawing reads them.
	if got := regexp.MustCompile(`<tr data-tier="(\d)">`).FindAllStringSubmatch(page, -1); len(got) != 4 || got[0][1] != "1" || got[3][1] != "4" {
		t.Fatalf("waterfall rows: %v", got)
	}
	if regexp.MustCompile(`\sstyle=`).MatchString(page) || strings.Contains(page, "<script>") {
		t.Error("/trust uses inline style or script, which the CSP blocks")
	}
	// Public copy: no strategy, no money, no verdicts on people. ("Sponsor" is
	// the protocol's word for a vouch that introduces a newcomer, RFC0012 §4.5.)
	lower := strings.ToLower(page[strings.Index(page, "<article"):strings.Index(page, "</article>")])
	for _, unwanted := range []string{"$", "usd", "dollar", "sponsors", "sponsorship", "is human", "trustworthy", "verified human", "unlimited", " cents"} {
		if strings.Contains(lower, unwanted) {
			t.Errorf("/trust copy says %q", unwanted)
		}
	}
	// Every other scene's illustration names no real agent.
	for _, seed := range []string{"9eb0e9479b2e18fc", "5e4dd880110baa73", parityAgent[:16]} {
		if strings.Contains(page, seed) {
			t.Errorf("/trust shows the real account %s", seed)
		}
	}
}

// With only trust on, the page shows no waterfall numbers (the drawing falls
// back to the published defaults), and the trust blocks still match the API.
func TestTrustExplainerTrustOnly(t *testing.T) {
	s, _ := rfc0012Server(board.Features{Trust: board.TrustShadow})
	page := getHTML(t, s, "/trust")
	if strings.Contains(page, `id="waterfall-live"`) || !strings.Contains(page, "The allowance ledger is off on this server") {
		t.Fatal("trust-only /trust claims live waterfall numbers")
	}
	holdToAPI(t, "/trust", pageCells(t, section(t, page, "distribution-live")), getJSON(t, s, "GET", "/api/stats/allowance", "")["data"])
	if !strings.Contains(page, "The endorsement export is not switched on") {
		t.Error("/trust does not say the export is off")
	}
}

// Flags off: /trust is today's 404, byte for byte, and nothing links to it.
func TestTrustExplainerOffIsToday(t *testing.T) {
	s, _ := rfc0012Server(board.Features{})
	plain := New(&fakeService{}, web.Handler(&fakeService{}), Config{})
	for _, path := range []string{"/trust", "/stats", "/for-agents", "/llms.txt", "/llms-full.txt"} {
		r, p := htmlRequest(s, path), htmlRequest(plain, path)
		// The plain fake store cannot read activity, so /stats is only checked for links.
		if path != "/stats" && (r.Code != p.Code || r.Body.String() != p.Body.String()) {
			t.Errorf("%s differs from a server without RFC0012 flags (%d, %d)", path, r.Code, p.Code)
		}
		if strings.Contains(r.Body.String(), `href="/trust"`) || strings.Contains(r.Body.String(), ": /trust.") {
			t.Errorf("%s links to /trust with every flag off", path)
		}
	}
	if code := htmlRequest(s, "/trust").Code; code != 404 {
		t.Fatalf("/trust with every flag off: %d", code)
	}
}

// Flags on: /stats, /for-agents and llms.txt link to the page.
func TestTrustExplainerLinkedWhenOn(t *testing.T) {
	for _, f := range []board.Features{allOn, {Ledger: board.LedgerShadow}, {Trust: board.TrustShadow}} {
		s, _ := rfc0012Server(f)
		for _, path := range []string{"/stats", "/for-agents"} {
			if !strings.Contains(getHTML(t, s, path), `href="/trust"`) {
				t.Errorf("%s does not link to /trust with %+v", path, f)
			}
		}
		for _, path := range []string{"/llms.txt", "/llms-full.txt"} {
			if !strings.Contains(makeRequest(s, "GET", path, "", "").Body.String(), "with the live numbers: /trust.") {
				t.Errorf("%s does not link to /trust", path)
			}
		}
	}
}

func htmlRequest(s http.Handler, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	r.Header.Set("Accept", "text/html")
	r.RemoteAddr = "198.51.100.8:12345"
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
