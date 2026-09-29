package web

import (
	"context"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// explainerService is a store with RFC0012 flags and fixed numbers. The
// page's parity with the API is tested in internal/httpapi; these cover the
// page itself and its assets.
type explainerService struct {
	testService
	features board.Features
}

func (s *explainerService) Features() board.Features { return s.features }

func (s *explainerService) AllowanceStats(context.Context, int) (map[string]any, error) {
	return map[string]any{"params_version": 0, "resources": []any{map[string]any{"resource": "post_bytes", "day": "2026-09-29", "budget": 64 << 20,
		"tiers": []any{map[string]any{"tier": 4, "size": 40 << 20, "claimed": 3336, "claimants": 1}, map[string]any{"tier": 1, "size": 6 << 20, "want": 6 << 20},
			map[string]any{"tier": 3, "size": 12 << 20, "want": 12 << 20}, map[string]any{"tier": 2, "size": 6 << 20, "want": 6 << 20}}}}}, nil
}

func (s *explainerService) TrustDistribution(context.Context) (map[string]any, error) {
	return map[string]any{"mode": "shadow", "run": 2, "collateral_log10_bins": []int64{1, 6, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, "tier_counts": map[string]int64{"3": 7}}, nil
}

func (s *explainerService) TrustRuns(context.Context, int64, int) ([]map[string]any, int64, error) {
	return []map[string]any{{"id": int64(2), "state": "done"}}, 0, nil
}

func (s *explainerService) TrustRun(context.Context, int64) (map[string]any, error) {
	return map[string]any{"id": int64(2), "as_of": int64(1790640000), "state": "done", "nodes": int64(7), "edges": int64(3), "output_sha256": "ab",
		"capture_bound": map[string]any{"unit_per_share": 20, "edge_cap_units": 20, "lambda_ppm": 500000, "max_transit_units": 3, "pool_units": 140, "statement": "The bound."}}, nil
}

func getTrustPage(t *testing.T, s board.Service, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	Handler(s).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w
}

func TestTrustExplainerOffIsNotFound(t *testing.T) {
	// Today's 404 for any unknown address: the same page as /trusted, but for its path.
	off := getTrustPage(t, &testService{}, "/trust")
	other := getTrustPage(t, &testService{}, "/trusted")
	if off.Code != 404 || off.Body.String() != strings.ReplaceAll(other.Body.String(), "/trusted", "/trust") {
		t.Fatalf("/trust with every flag off is not the ordinary 404: %d", off.Code)
	}
	for _, unwanted := range []string{"tx-theme", "trust.css", "trust.js", "light dark"} {
		if strings.Contains(off.Body.String(), unwanted) {
			t.Errorf("flags-off 404 carries %q", unwanted)
		}
	}
	if TrustExplainerOn(board.Features{}) || !TrustExplainerOn(board.Features{Ledger: board.LedgerShadow}) || !TrustExplainerOn(board.Features{Trust: board.TrustShadow}) {
		t.Fatal("TrustExplainerOn does not follow the ledger and trust flags")
	}
}

func TestTrustExplainerRenders(t *testing.T) {
	s := &explainerService{features: board.Features{Ledger: board.LedgerOn, Trust: board.TrustShadow}}
	w := getTrustPage(t, s, "/trust")
	body := w.Body.String()
	if w.Code != 200 {
		t.Fatalf("/trust: %d", w.Code)
	}
	for _, want := range []string{"How to give something away when anyone can be a million people", `data-view="trust"`, `class="tx-theme"`,
		`data-fig="arrive"`, `data-fig="million"`, `data-fig="pour"`, `data-fig="collateral"`, `data-fig="flows"`, `data-fig="vouch"`, `data-fig="rings"`, `data-fig="levers"`, `data-fig="live"`,
		`data-key="allowance.resources.0.tiers.1.water" data-value="6291456"`, `data-key="trust.collateral_log10.1.accounts" data-value="6"`,
		`data-key="capture_bound.max_transit_units" data-value="3"`, "The bound.", `href="/api/trust/runs/2"`, `for="tx-agent"`, `/api/trust/runs/2/snapshot`} {
		if !strings.Contains(body, want) {
			t.Errorf("/trust lacks %q", want)
		}
	}
	// Bins after the last with accounts are left out (at least four shown).
	if strings.Contains(body, "trust.collateral_log10.4.") || !strings.Contains(body, "trust.collateral_log10.3.") {
		t.Error("collateral bins not trimmed to the populated range")
	}
	// Tier 0 (the grant pool) is not a subject tier and is not drawn.
	if strings.Contains(body, `data-tier="0"`) {
		t.Error("the grant pool is drawn as a tier")
	}
	if regexp.MustCompile(`\sstyle=`).MatchString(body) {
		t.Error("inline style attribute")
	}
	// With the ledger alone the page renders without the trust blocks.
	ledgerOnly := getTrustPage(t, &explainerService{features: board.Features{Ledger: board.LedgerOn}}, "/trust")
	if ledgerOnly.Code != 200 || strings.Contains(ledgerOnly.Body.String(), `id="tx-agent"`) || strings.Contains(ledgerOnly.Body.String(), `id="run-live"`) ||
		!strings.Contains(ledgerOnly.Body.String(), "Trust estimates are not published on this server yet.") {
		t.Error("ledger-only /trust offers trust blocks")
	}
}

// The page's script: syntax checked by node when it is installed, and held
// to the rules the rest of the site's scripts follow.
func TestTrustExplainerAssets(t *testing.T) {
	script, err := files.ReadFile("assets/trust.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(script)
	for _, unwanted := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "eval(", "new Function", "document.write", "setAttribute('style'", "http://", "https://", "localStorage"} {
		if strings.Contains(strings.ReplaceAll(src, "http://www.w3.org/2000/svg", ""), unwanted) {
			t.Errorf("trust.js uses %q", unwanted)
		}
	}
	for _, want := range []string{"prefers-reduced-motion", "textContent", "credentials: 'omit'", "encodeURIComponent(id)"} {
		if !strings.Contains(src, want) {
			t.Errorf("trust.js lacks %q", want)
		}
	}
	style, err := files.ReadFile("assets/trust.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"prefers-color-scheme:dark", `html.tx-theme[data-theme="dark"]`, "forced-colors:active"} {
		if !strings.Contains(string(style), want) {
			t.Errorf("trust.css lacks %q", want)
		}
	}
	if strings.Contains(string(style), "@import") || strings.Contains(string(style), "url(") {
		t.Error("trust.css loads something from elsewhere")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; the syntax check runs where it is")
	}
	if out, err := exec.Command(node, "--check", filepath.Join("assets", "trust.js")).CombinedOutput(); err != nil {
		t.Fatalf("trust.js: %v\n%s", err, out)
	}
}
