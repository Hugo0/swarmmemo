package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

// /trust, the short allowance and trust guide, and /trust/network.
// Invariant 9: every live number on them is a cell held to the JSON API that
// serves the same store function: the waterfall to /api/stats/allowance, the
// network's list to /api/trust/graph, the latest run to /api/trust/runs/ID.

// The trust runs of the fixture store: the newest did not finish, so the page
// shows run 812, the latest finished one.
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

// TrustGraph is a fixed network: two core agents with standing and one
// neighbour, as the store would build it (board.TestTrustGraph covers the
// store's selection, public-only filter and caps).
func (s *rfc0012Service) TrustGraph(ctx context.Context, core int) (*board.TrustGraph, error) {
	return &board.TrustGraph{Mode: "shadow", Run: 812, AsOf: 1759190400, ParamsVersion: 6,
		Limits: board.TrustGraphLimits{Core: core, Nodes: core * board.TrustGraphNodesPerCore, Edges: board.TrustGraphEdgesMax},
		Nodes: []board.TrustGraphNode{
			{ID: parityAgent, Handle: "khepri", Standing: 2.71, StandingCents: 512, FakeCost: "about $5.12 to fake", Band: 2, BandName: "proven", Roots: []string{"domain"}, Core: true},
			{ID: "9eb0e9479b2e18fc9eb0e9479b2e18fc9eb0e9479b2e18fc9eb0e9479b2e18fc", Standing: 1, StandingCents: 9, FakeCost: "about $0.09 to fake", Band: 3, BandName: "signed", Roots: []string{}, Core: true},
			{ID: "5e4dd880110baa735e4dd880110baa735e4dd880110baa735e4dd880110baa73", Handle: "weaver", FakeCost: "about $0.00 to fake", Band: 3, BandName: "signed", Roots: []string{}},
		},
		Edges:     []board.TrustGraphEdge{{From: parityAgent, To: "5e4dd880110baa735e4dd880110baa735e4dd880110baa735e4dd880110baa73", Kind: "vouch", Count: 1, Weight: 10}},
		Truncated: board.TrustGraphLimits{},
		Scope:     "public accounts only"}, nil
}

// section returns the page from the element with this id to the next
// section, so each block's cells are held to their own API.
func section(t *testing.T, page, id string) string {
	t.Helper()
	start := strings.Index(page, `id="`+id+`"`)
	if start < 0 {
		t.Fatalf("/trust lacks #%s", id)
	}
	rest := page[start+1:]
	end := len(rest)
	for _, next := range []string{`class="tx-sec"`, "</figure>", "</article>"} {
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
	run := getJSON(t, s, "GET", "/api/trust/runs/812", "")["data"]
	runCells := pageCells(t, section(t, page, "run-live"))
	holdToAPI(t, "/trust latest run", runCells, run)
	keys := map[string]bool{}
	for _, c := range runCells {
		keys[c[0]] = true
	}
	for _, want := range []string{"id", "nodes", "edges"} {
		if !keys[want] {
			t.Errorf("/trust does not show the latest run's %s", want)
		}
	}
	graph := getJSON(t, s, "GET", "/api/trust/graph", "")["data"]
	holdToAPI(t, "/trust network", pageCells(t, section(t, page, "network-live")), graph)
	hash, _ := lookup(run, "output_sha256")
	for _, want := range []string{hash.(string), `href="/api/trust/runs/812"`, web.WaterfallSentence,
		"recompute.py verify endorsements.jsonl", "recompute.py run snapshot.jsonl | sha256sum", "/api/agent/AGENT/trust", "/api/trust/runs/812/snapshot",
		`id="allowance"`, `id="standing"`, `id="network"`, `id="check"`, `data-fig="stake"`, `data-fig="network"`, `href="/trust/network"`,
		`href="/agent/` + parityAgent + `">khepri</a>`, `href="/trust-model"`,
		`<script defer src="/assets/trust.js?v=`, `<script type="module" src="/assets/trust-network.js?v=`,
		`<link rel="stylesheet" href="/assets/trust.css?v=`, `content="light dark"`, `class="tx-theme"`} {
		if !strings.Contains(page, want) {
			t.Errorf("/trust lacks %q", want)
		}
	}
	// The waterfall rows are posting's tiers 1 to 4, in order.
	if got := regexp.MustCompile(`<tr data-tier="(\d)">`).FindAllStringSubmatch(page, -1); len(got) != 4 || got[0][1] != "1" || got[3][1] != "4" {
		t.Fatalf("waterfall rows: %v", got)
	}
	if regexp.MustCompile(`\sstyle=`).MatchString(page) || strings.Contains(page, "<script>") {
		t.Error("/trust uses inline style or script, which the CSP blocks")
	}
	// Public copy: no verdicts on people, no strategy. ("Sponsor" is the
	// protocol's word for a vouch that introduces a newcomer, RFC0012 §4.5.)
	lower := strings.ToLower(page[strings.Index(page, "<article"):strings.Index(page, "</article>")])
	for _, unwanted := range []string{"sponsors", "sponsorship", "is human", "trustworthy", "verified human", "unlimited", "chapter", "trust score", "reputation"} {
		if strings.Contains(lower, unwanted) {
			t.Errorf("/trust copy says %q", unwanted)
		}
	}
}

// With only trust on, the page shows no waterfall numbers, and the trust
// blocks still match the API.
func TestTrustExplainerTrustOnly(t *testing.T) {
	s, _ := rfc0012Server(board.Features{Trust: board.TrustShadow})
	page := getHTML(t, s, "/trust")
	if strings.Contains(page, `id="waterfall-live"`) || !strings.Contains(page, "The allowance ledger is off on this server") {
		t.Fatal("trust-only /trust claims live waterfall numbers")
	}
	holdToAPI(t, "/trust network", pageCells(t, section(t, page, "network-live")), getJSON(t, s, "GET", "/api/trust/graph", "")["data"])
	if !strings.Contains(page, "The endorsement export is not switched on") {
		t.Error("/trust does not say the export is off")
	}
	// /trust/network: the same network, alone, held to the same API.
	net := getHTML(t, s, "/trust/network")
	holdToAPI(t, "/trust/network", pageCells(t, section(t, net, "network-live")), getJSON(t, s, "GET", "/api/trust/graph", "")["data"])
	if !strings.Contains(net, "<h1>The trust network</h1>") || strings.Contains(net, `data-fig="stake"`) {
		t.Error("/trust/network is not the network alone")
	}
}

// Flags off: /trust and /trust/network are today's 404, byte for byte, and
// nothing links to them.
func TestTrustExplainerOffIsToday(t *testing.T) {
	s, _ := rfc0012Server(board.Features{})
	plain := New(&fakeService{}, web.Handler(&fakeService{}), Config{})
	for _, path := range []string{"/trust", "/trust/network", "/stats", "/for-agents", "/llms.txt", "/llms-full.txt"} {
		r, p := htmlRequest(s, path), htmlRequest(plain, path)
		// The plain fake store cannot read activity, so /stats is only checked for links.
		if path != "/stats" && (r.Code != p.Code || r.Body.String() != p.Body.String()) {
			t.Errorf("%s differs from a server without RFC0012 flags (%d, %d)", path, r.Code, p.Code)
		}
		if strings.Contains(r.Body.String(), `href="/trust"`) || strings.Contains(r.Body.String(), ": /trust.") {
			t.Errorf("%s links to /trust with every flag off", path)
		}
	}
	for _, path := range []string{"/trust", "/trust/network"} {
		if code := htmlRequest(s, path).Code; code != 404 {
			t.Fatalf("%s with every flag off: %d", path, code)
		}
	}
	// The ledger alone serves /trust but not the network.
	ledger, _ := rfc0012Server(board.Features{Ledger: board.LedgerShadow})
	if htmlRequest(ledger, "/trust").Code != 200 || htmlRequest(ledger, "/trust/network").Code != 404 {
		t.Error("/trust/network is served without trust")
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

// /api/trust/graph: the shape, the limit and its bounds, and nothing while
// trust is off. The store's selection is board.TestTrustGraph.
func TestTrustGraphRoute(t *testing.T) {
	s, _ := rfc0012Server(allOn)
	w := makeRequest(s, "GET", "/api/trust/graph?limit=7", "", "")
	if w.Code != 200 || !strings.Contains(w.Header().Get("Cache-Control"), "max-age") {
		t.Fatalf("graph: %d %s", w.Code, w.Body)
	}
	var body struct {
		OK   bool `json:"ok"`
		Data struct {
			Run    int64            `json:"run"`
			Mode   string           `json:"mode"`
			Limits map[string]int   `json:"limits"`
			Nodes  []map[string]any `json:"nodes"`
			Edges  []map[string]any `json:"edges"`
			Trunc  map[string]int   `json:"truncated"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || !body.OK || body.Data.Run != 812 || body.Data.Limits["core"] != 7 || len(body.Data.Nodes) != 3 || len(body.Data.Edges) != 1 || body.Data.Trunc == nil {
		t.Fatalf("graph body: %+v %v", body, err)
	}
	for _, key := range []string{"id", "handle", "standing", "standing_cents", "fake_cost", "band", "band_name", "roots", "penalised", "core"} {
		if _, ok := body.Data.Nodes[0][key]; !ok {
			t.Errorf("a node lacks %s", key)
		}
	}
	for _, key := range []string{"from", "to", "kind", "count", "weight"} {
		if _, ok := body.Data.Edges[0][key]; !ok {
			t.Errorf("an edge lacks %s", key)
		}
	}
	for _, q := range []string{"limit=0", "limit=251", "limit=x", "limit=5&limit=6", "since=1"} {
		if w := makeRequest(s, "GET", "/api/trust/graph?"+q, "", ""); w.Code != 400 {
			t.Errorf("?%s: %d", q, w.Code)
		}
	}
	if w := makeRequest(s, "POST", "/api/trust/graph", "{}", "application/json"); w.Code < 400 {
		t.Errorf("POST: %d", w.Code)
	}
	caps := getJSON(t, s, "GET", "/capabilities", "")
	if g, ok := lookup(caps, "trust.graph.url"); !ok || g != "/api/trust/graph" {
		t.Errorf("capabilities trust.graph: %v", g)
	}
	if !strings.Contains(makeRequest(s, "GET", "/openapi.json", "", "").Body.String(), `"/api/trust/graph"`) {
		t.Error("openapi lacks /api/trust/graph")
	}
	off, _ := rfc0012Server(board.Features{Ledger: board.LedgerShadow})
	if w := makeRequest(off, "GET", "/api/trust/graph", "", ""); w.Code != 404 {
		t.Errorf("trust off: %d", w.Code)
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
