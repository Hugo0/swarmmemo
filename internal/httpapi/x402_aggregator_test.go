package httpapi

import (
	"context"
	"strings"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/services"
	"swarmmemo/internal/web"
)

// x402StatsService is a store with the pay-per-call relay's spend.
type x402StatsService struct {
	rfc0012Service
	stats *services.X402Stats
}

func (s *x402StatsService) X402Stats(_ context.Context, days int) (*services.X402Stats, error) {
	if s.stats == nil {
		return nil, nil
	}
	out := *s.stats
	out.Days = out.Days[max(0, len(out.Days)-days):]
	return &out, nil
}

// The aggregator on every surface: one line in the catalogue, /llms.txt and
// /capabilities, a search example on each, the /for-agents section, and the
// same spend on /stats and /api/stats/x402.
func TestX402AggregatorSurfaces(t *testing.T) {
	f := board.Features{Ledger: board.LedgerOn, Services: []string{"memory", "x402", "public_data"}}
	svc := &x402StatsService{rfc0012Service: rfc0012Service{features: f}, stats: &services.X402Stats{
		Days:        []services.X402Day{{Day: "2026-09-28"}, {Day: "2026-09-29", Paid: 1500, Calls: 1}},
		GlobalDaily: "2", Pinned: 5, Open: 1200, Bundlers: []string{"x402"}, Decimals: 6,
	}}
	s := New(svc, web.Handler(svc), Config{Features: f})

	llms := makeRequest(s, "GET", "/llms.txt", "", "").Body.String()
	if !strings.Contains(llms, "Tools across the internet: "+services.X402Line) || !strings.Contains(llms, "x402_resources") || !strings.Contains(llms, `web search`) {
		t.Errorf("/llms.txt lacks the aggregator line or its search example")
	}
	caps := getJSON(t, s, "GET", "/capabilities", "")
	x402, _ := caps["services"].(map[string]any)["x402"].(map[string]any)
	if x402["line"] != services.X402Line || !strings.Contains(x402["search"].(string), `"method":"resources"`) || x402["without_key"] != false {
		t.Errorf("/capabilities services.x402: %v", x402)
	}
	page := makeRequest(s, "GET", "/for-agents", "", "").Body.String()
	if !strings.Contains(page, `<section id="tools">`) || !strings.Contains(page, "Tools across the internet") || !strings.Contains(page, "x402_resources") {
		t.Errorf("/for-agents lacks the tools section")
	}
	// Vetted versus candidate resources, in plain words, on every surface.
	if !strings.Contains(llms, "Only operator-vetted resources can be called") || x402["vetting"] != services.X402VettingNote ||
		!strings.Contains(page, "x402_unvetted") || !strings.Contains(page, "<code>callable: true</code>") {
		t.Errorf("a surface does not explain vetted and candidate resources")
	}
	var mcpDesc string
	for _, tool := range s.mcpToolListWith(s.fullProfile(nil)) {
		if tool.Name == "x402_resources" {
			mcpDesc = tool.Desc
		}
	}
	if !strings.Contains(mcpDesc, "vetted and callable") {
		t.Errorf("the MCP resources tool does not say what is callable: %q", mcpDesc)
	}

	api := getJSON(t, s, "GET", "/api/stats/x402?days=2", "")
	st, _ := api["stats"].(map[string]any)
	days, _ := st["days"].([]any)
	if api["ok"] != true || len(days) != 2 || days[1].(map[string]any)["paid"] != 1500.0 || st["open_resources"] != 1200.0 {
		t.Fatalf("/api/stats/x402: %v", api)
	}
	if rec := makeRequest(s, "GET", "/api/stats/x402?days=0", "", ""); rec.Code != 400 {
		t.Errorf("days=0: %d", rec.Code)
	}
	stats := makeRequest(s, "GET", "/stats", "", "").Body.String()
	if !strings.Contains(stats, `id="stats-x402"`) || !strings.Contains(stats, "$0.0015") || !strings.Contains(stats, "5 pinned, 1200 open") {
		t.Errorf("/stats lacks the relay's spend")
	}

	// Off: the route declines and the page draws nothing.
	off := board.Features{Ledger: board.LedgerOn, Services: []string{"memory"}}
	quiet := &x402StatsService{rfc0012Service: rfc0012Service{features: off}}
	q := New(quiet, web.Handler(quiet), Config{Features: off})
	if rec := makeRequest(q, "GET", "/api/stats/x402", "", ""); rec.Code == 200 {
		t.Errorf("x402 off: /api/stats/x402 answered 200")
	}
	if strings.Contains(makeRequest(q, "GET", "/stats", "", "").Body.String(), "stats-x402") || strings.Contains(makeRequest(q, "GET", "/for-agents", "", "").Body.String(), `id="tools"`) {
		t.Errorf("x402 off: the relay is still drawn")
	}
}
