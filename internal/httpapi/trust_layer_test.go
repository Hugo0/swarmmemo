package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// TrustLayer is a fixed two-run layer, as the store would build it
// (board.TestTrustLayerShapingCapsPrivacy covers the store's selection,
// public-only filter and caps; board.TestTrustLayerMatchesTrustGet its
// agreement with trust.get).
func (s *rfc0012Service) TrustLayer(ctx context.Context, runs, nodes int) (*board.TrustLayer, error) {
	other := "9eb0e9479b2e18fc9eb0e9479b2e18fc9eb0e9479b2e18fc9eb0e9479b2e18fc"
	return &board.TrustLayer{Mode: "on", Unit: "usd_cent", Scope: "public accounts only",
		Limits:  board.TrustLayerLimits{Runs: runs, Nodes: nodes, Edges: board.TrustLayerEdgesMax},
		Runs:    []board.TrustLayerRun{{Run: 811, AsOf: 1759104000, Agents: 2}, {Run: 812, AsOf: 1759190400, Agents: 2}},
		Sources: []board.TrustLayerSource{{ID: "world:money", Kind: "money", Label: "Money paid", Cents: []int64{400, 512}}},
		Nodes: []board.TrustLayerNode{
			{ID: parityAgent, Handle: "khepri", Standing: []int64{400, 512}, Band: 2, BandName: "proven"},
			{ID: other, Standing: []int64{0, 9}, Band: 1, BandName: "signed"},
		},
		Edges: []board.TrustLayerEdge{
			{From: parityAgent, To: other, Kind: "backing", Cents: []int64{0, 9}},
			{From: "world:money", To: parityAgent, Kind: "world", Cents: []int64{400, 512}},
		}}, nil
}

// /api/trust/layer: the shape, runs and limit with their bounds, discovery
// (capabilities, OpenAPI, llms.txt), and nothing while trust is off.
func TestTrustLayerRoute(t *testing.T) {
	s, _ := rfc0012Server(allOn)
	w := makeRequest(s, "GET", "/api/trust/layer?runs=2&limit=9", "", "")
	if w.Code != 200 || !strings.Contains(w.Header().Get("Cache-Control"), "max-age=300") {
		t.Fatalf("layer: %d %s", w.Code, w.Body)
	}
	var body struct {
		OK   bool `json:"ok"`
		Data struct {
			Limits  map[string]int   `json:"limits"`
			Runs    []map[string]any `json:"runs"`
			Sources []map[string]any `json:"sources"`
			Nodes   []map[string]any `json:"nodes"`
			Edges   []map[string]any `json:"edges"`
			Trunc   map[string]int   `json:"truncated"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || !body.OK || body.Data.Limits["runs"] != 2 || body.Data.Limits["nodes"] != 9 || len(body.Data.Runs) != 2 || len(body.Data.Nodes) != 2 || body.Data.Trunc == nil {
		t.Fatalf("layer body: %+v %v", body, err)
	}
	for _, key := range []string{"id", "handle", "standing_cents", "band", "band_name", "penalised"} {
		if _, ok := body.Data.Nodes[0][key]; !ok {
			t.Errorf("a node lacks %s", key)
		}
	}
	for _, key := range []string{"from", "to", "kind", "cents"} {
		if _, ok := body.Data.Edges[0][key]; !ok {
			t.Errorf("an edge lacks %s", key)
		}
	}
	for _, key := range []string{"id", "kind", "label", "cents"} {
		if _, ok := body.Data.Sources[0][key]; !ok {
			t.Errorf("a source lacks %s", key)
		}
	}
	if w := makeRequest(s, "GET", "/api/trust/layer", "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"runs":12,"nodes":80`) {
		t.Errorf("defaults: %d %s", w.Code, w.Body)
	}
	for _, q := range []string{"runs=0", "runs=31", "limit=0", "limit=201", "limit=x", "runs=2&runs=3", "since=1"} {
		if w := makeRequest(s, "GET", "/api/trust/layer?"+q, "", ""); w.Code != 400 {
			t.Errorf("?%s: %d", q, w.Code)
		}
	}
	if w := makeRequest(s, "POST", "/api/trust/layer", "{}", "application/json"); w.Code < 400 {
		t.Errorf("POST: %d", w.Code)
	}
	caps := getJSON(t, s, "GET", capabilitiesFullPath, "")
	if g, ok := lookup(caps, "trust.layer.url"); !ok || g != "/api/trust/layer" {
		t.Errorf("capabilities trust.layer: %v", g)
	}
	if g, ok := lookup(caps, "trust.layer.page"); !ok || g != TrustLayerPage {
		t.Errorf("capabilities trust.layer.page: %v", g)
	}
	if !strings.Contains(makeRequest(s, "GET", "/openapi.json", "", "").Body.String(), `"/api/trust/layer"`) {
		t.Error("openapi lacks /api/trust/layer")
	}
	if !strings.Contains(makeRequest(s, "GET", "/llms-full.txt", "", "").Body.String(), "/api/trust/layer") {
		t.Error("llms-full.txt lacks /api/trust/layer")
	}
	off, _ := rfc0012Server(board.Features{Ledger: board.LedgerShadow})
	if w := makeRequest(off, "GET", "/api/trust/layer", "", ""); w.Code != 404 {
		t.Errorf("trust off: %d", w.Code)
	}
	if strings.Contains(makeRequest(off, "GET", "/openapi.json", "", "").Body.String(), `"/api/trust/layer"`) {
		t.Error("openapi lists /api/trust/layer while trust is off")
	}
}
