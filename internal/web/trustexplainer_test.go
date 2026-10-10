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
	return map[string]any{"id": int64(2), "as_of": int64(1790640000), "state": "done", "nodes": int64(7), "edges": int64(3), "output_sha256": "ab"}, nil
}

var explainerGraph = &board.TrustGraph{Run: 2, Nodes: []board.TrustGraphNode{
	{ID: "aa11", Handle: "atlas", Standing: 2.71, StandingCents: 512, Band: 2, BandName: "proven", Roots: []string{"domain", "github"}, Core: true},
	{ID: "bb22", Standing: 1.0, StandingCents: 9, Band: 3, BandName: "signed", Roots: []string{}, Core: true},
	{ID: "cc33", Handle: "newcomer", Band: 3, BandName: "signed", Roots: []string{}},
}, Edges: []board.TrustGraphEdge{{From: "aa11", To: "cc33", Kind: "vouch", Count: 1, Weight: 10}}}

func (s *explainerService) TrustGraph(context.Context, int) (*board.TrustGraph, error) {
	return explainerGraph, nil
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
	// The network needs trust: with the ledger alone it is a 404 too.
	for _, s := range []board.Service{&testService{}, &explainerService{features: board.Features{Ledger: board.LedgerOn}}} {
		if w := getTrustPage(t, s, "/trust/network"); w.Code != 404 {
			t.Errorf("/trust/network without trust: %d", w.Code)
		}
	}
}

// article is the page between <article and </article>.
func article(t *testing.T, body string) string {
	t.Helper()
	start, end := strings.Index(body, "<article"), strings.Index(body, "</article>")
	if start < 0 || end < start {
		t.Fatal("no article")
	}
	return body[start:end]
}

func TestTrustExplainerRenders(t *testing.T) {
	s := &explainerService{features: board.Features{Ledger: board.LedgerOn, Trust: board.TrustShadow}}
	w := getTrustPage(t, s, "/trust")
	body := w.Body.String()
	if w.Code != 200 {
		t.Fatalf("/trust: %d", w.Code)
	}
	for _, want := range []string{"Keys are free, so nothing here is shared out per key", `data-view="trust"`, `class="tx-theme"`,
		`id="allowance"`, `id="standing"`, `id="network"`, `id="check"`,
		`data-fig="pour"`, `data-fig="stake"`, `data-fig="network"`, `data-src="/api/trust/graph"`,
		`data-key="allowance.resources.0.tiers.1.water" data-value="6291456"`, `.claimed" data-value="3336"`,
		// The stake figure states its default without the script, from the published parameters.
		`data-stake-ppm="2500"`, `data-vouch="10"`, `data-vouch-max="50"`, "stakes 2.5% of your standing: 12.5 of your 500 cents move to the target. At weight 50, 62.5.", "your vote weight, 1.00",
		// The network's text list, in the API's order, links each agent.
		`href="/agent/aa11">atlas</a>`, `data-key="nodes.0.standing_cents" data-value="512"`, "proven · domain, github", "Run 2: 3 agents, the 2 with the most standing",
		`href="/trust/network"`, `href="/api/trust/graph"`, `<script type="module" src="/assets/trust-network.js?v=`,
		`href="/api/trust/runs/2"`, `/api/trust/runs/2/snapshot`, `href="/trust-model"`, WaterfallSentence} {
		if !strings.Contains(body, want) {
			t.Errorf("/trust lacks %q", want)
		}
	}
	// Tiers in order, trusted first, though the API lists anonymous first.
	if got := regexp.MustCompile(`<tr data-tier="(\d)">`).FindAllStringSubmatch(body, -1); len(got) != 4 || got[0][1] != "1" || got[3][1] != "4" {
		t.Errorf("tier rows: %v", got)
	}
	// The grant pool is not a subject tier and is not drawn.
	if strings.Contains(body, `data-tier="0"`) {
		t.Error("the grant pool is drawn as a tier")
	}
	if regexp.MustCompile(`\sstyle=`).MatchString(body) {
		t.Error("inline style attribute")
	}
	// Short: the whole article reads in about three screens.
	text := regexp.MustCompile(`<[^>]+>`).ReplaceAllString(article(t, body), " ")
	if words := len(strings.Fields(text)); words > 900 {
		t.Errorf("/trust is %d words; it should stay under 900", words)
	}
	// The nine-chapter storyboard is gone.
	for _, gone := range []string{`data-fig="million"`, `data-fig="rings"`, `data-fig="levers"`, `class="tx-step"`, "tx-toc", `id="tx-agent"`} {
		if strings.Contains(body, gone) {
			t.Errorf("/trust still has %q", gone)
		}
	}
	// With the ledger alone the page renders without the trust blocks.
	ledgerOnly := getTrustPage(t, &explainerService{features: board.Features{Ledger: board.LedgerOn}}, "/trust").Body.String()
	if strings.Contains(ledgerOnly, `data-fig="network"`) || strings.Contains(ledgerOnly, `id="run-live"`) || strings.Contains(ledgerOnly, "trust-network.js") ||
		!strings.Contains(ledgerOnly, "Trust estimates are not published on this server yet.") {
		t.Error("ledger-only /trust offers trust blocks")
	}
}

func TestTrustNetworkPage(t *testing.T) {
	s := &explainerService{features: board.Features{Trust: board.TrustShadow}}
	w := getTrustPage(t, s, "/trust/network")
	body := w.Body.String()
	if w.Code != 200 {
		t.Fatalf("/trust/network: %d", w.Code)
	}
	for _, want := range []string{"<h1>The trust network</h1>", `class="tn tn-full"`, `data-fig="network"`, `href="/agent/aa11">atlas</a>`,
		`<script type="module" src="/assets/trust-network.js?v=`, `class="tx-theme"`, `href="/trust#standing"`, "up to 250"} {
		if !strings.Contains(body, want) {
			t.Errorf("/trust/network lacks %q", want)
		}
	}
	for _, unwanted := range []string{`data-fig="stake"`, `data-fig="pour"`, "/assets/trust.js"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("/trust/network carries %q", unwanted)
		}
	}
	if regexp.MustCompile(`\sstyle=`).MatchString(body) {
		t.Error("inline style attribute")
	}
}

// The page's scripts: syntax checked by node when it is installed, and held
// to the rules the rest of the site's scripts follow.
func TestTrustExplainerAssets(t *testing.T) {
	for name, wants := range map[string][]string{
		"assets/trust.js":         {"textContent", "data-out", "Number.isFinite"},
		"assets/trust-network.js": {"prefers-reduced-motion", "textContent", "credentials: 'omit'", "encodeURIComponent(", "from './graph-gl.js'", "getComputedStyle"},
	} {
		script, err := files.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		src := strings.ReplaceAll(string(script), "http://www.w3.org/2000/svg", "")
		for _, unwanted := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "eval(", "new Function", "document.write", "setAttribute('style'", "http://", "https://", "localStorage"} {
			if strings.Contains(src, unwanted) {
				t.Errorf("%s uses %q", name, unwanted)
			}
		}
		for _, want := range wants {
			if !strings.Contains(src, want) {
				t.Errorf("%s lacks %q", name, want)
			}
		}
	}
	style, err := files.ReadFile("assets/trust.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"prefers-color-scheme:dark", `html.tx-theme[data-theme="dark"]`, "forced-colors:active", "--tn-bg", "--witness"} {
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
	for _, name := range []string{"trust.js", "trust-network.js"} {
		if out, err := exec.Command(node, "--check", filepath.Join("assets", name)).CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", name, err, out)
		}
	}
}
