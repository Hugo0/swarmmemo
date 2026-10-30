package httpapi

import (
	"os"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// The trust pages (C320b): /agent/FP/trust, the public standing breakdown
// /me imports as "Your trust", and /trust/penalties, the public penalty log.
// Every number on them is a cell of the API they mirror, and each page is
// held to a byte budget, as the agent page is (C315).
const (
	agentTrustPageBudget = 30_000
	penaltiesPageBudget  = 24_000
)

// dumpPage writes a page for a look when SWARMMEMO_DUMP names a directory.
func dumpPage(t *testing.T, name, body string) {
	if dir := os.Getenv("SWARMMEMO_DUMP"); dir != "" {
		_ = os.WriteFile(dir+"/"+name, []byte(body), 0o644)
	}
}

func TestAgentTrustPageMatchesAPI(t *testing.T) {
	s, _ := rfc0012Server(allOn)
	page := getHTML(t, s, "/agent/"+parityAgent+"?part=standing")
	dumpPage(t, "agent-trust.html", page)
	t.Logf("/agent/FP/trust: %d bytes (budget %d)", len(page), agentTrustPageBudget)
	if len(page) > agentTrustPageBudget {
		t.Errorf("/agent/FP/trust is %d bytes, over its %d byte budget", len(page), agentTrustPageBudget)
	}
	api := getJSON(t, s, "GET", "/api/trust/standing?agent="+parityAgent, "")
	at := strings.Index(page, `id="trust-breakdown"`)
	if at < 0 {
		t.Fatal("no breakdown section")
	}
	holdToAPI(t, "standing breakdown", pageCells(t, page[at:]), api["data"])
	// The visuals, each with its words: the band meter, the own costs bar
	// with the cap, the flow, accuracy, grace, the burn, the ways.
	for _, want := range []string{`class="band-meter"`, `class="costs-bar"`, `class="cap"`, `class="flow-svg"`, `class="acc-scale"`, `class="meter grace"`,
		`Standing 1,200¢, trusted`, "@weaver", "example.org", "What a penalty would burn", "Link a wallet", `href="/me#standing-wallet"`,
		`href="/trust/penalties?agent=` + parityAgent + `"`, "Domain", "Money paid", `title="Accuracy: `} {
		if !strings.Contains(page, want) {
			t.Errorf("/agent/FP/trust lacks %q", want)
		}
	}
	if strings.Contains(page, `id="tb-viewer"`) {
		t.Error("the public page shows a viewer's view nobody asked for")
	}
	// ?viewer= adds that viewer's view, never indexed.
	viewer := strings.Repeat("ab", 32)
	w := makeRequest(s, "GET", "/agent/"+parityAgent+"?part=standing&viewer="+viewer, "", "")
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, `id="tb-viewer"`) || !strings.Contains(body, `data-key="viewer.standing_cents" data-value="1450"`) || !strings.Contains(w.Header().Get("X-Robots-Tag"), "noindex") {
		t.Fatalf("viewer view: %d %s", w.Code, w.Header().Get("X-Robots-Tag"))
	}
	for path, code := range map[string]int{"/agent/" + strings.Repeat("0", 64) + "?part=standing": 404} {
		if w := makeRequest(s, "GET", path, "", ""); w.Code != code {
			t.Errorf("%s: %d, want %d", path, w.Code, code)
		}
	}
	// Trust off: no page.
	off, _ := rfc0012Server(board.Features{Ledger: board.LedgerOn})
	if w := makeRequest(off, "GET", "/agent/"+parityAgent+"?part=standing", "", ""); w.Code != 404 {
		t.Errorf("trust off: /agent/FP/trust %d", w.Code)
	}
}

func TestPenaltyLogPage(t *testing.T) {
	s, _ := rfc0012Server(allOn)
	page := getHTML(t, s, "/trust/penalties")
	dumpPage(t, "penalties.html", page)
	t.Logf("/trust/penalties: %d bytes (budget %d)", len(page), penaltiesPageBudget)
	if len(page) > penaltiesPageBudget {
		t.Errorf("/trust/penalties is %d bytes, over its %d byte budget", len(page), penaltiesPageBudget)
	}
	for _, want := range []string{"Ring detector", "Lift", `href="#ring-bb"`, "lifted", `href="/e/0123456789abcdef0123456789abcdef"`, "github:4242",
		`href="/agent/` + parityAgent + `"`, "38%, 290¢", "30 days", "level 1", "detector v2", "By hugo", `href="/trust/penalties?before=5"`, `href="/api/trust/penalties"`} {
		if !strings.Contains(page, want) {
			t.Errorf("/trust/penalties lacks %q", want)
		}
	}
	api := getJSON(t, s, "GET", "/api/trust/penalties", "")
	entries, _ := api["data"].(map[string]any)["entries"].([]any)
	if strings.Count(page, `class="penalty-entry"`) != len(entries) {
		t.Errorf("the page lists %d entries, the API %d", strings.Count(page, `class="penalty-entry"`), len(entries))
	}
	one := getHTML(t, s, "/trust/penalties?agent="+parityAgent)
	if !strings.Contains(one, "Penalty log: @khepri") || !strings.Contains(one, `href="/trust/penalties"`) {
		t.Error("the per-agent log does not name its agent or link the whole log")
	}
	for path, code := range map[string]int{"/trust/penalties?agent=nope": 404, "/trust/penalties?before=x": 400} {
		if w := makeRequest(s, "GET", path, "", ""); w.Code != code {
			t.Errorf("%s: %d, want %d", path, w.Code, code)
		}
	}
	model := getHTML(t, s, "/trust-model")
	dumpPage(t, "trust-model.html", model)
	if !strings.Contains(model, `href="/trust/penalties"`) {
		t.Error("/trust-model does not link the penalty log")
	}
	// /trust-model: the one-graph model, short, with its one diagram.
	if strings.Count(model, `<figure class="trust-model-figure">`) != 1 || !strings.Contains(model, `<title id="tmf-title">The one trust graph</title>`) {
		t.Error("/trust-model lacks its diagram")
	}
	if n := len(strings.Fields(makeRequest(s, "GET", "/trust-model.md", "", "").Body.String())); n > 1300 {
		t.Errorf("/trust-model.md is %d words; the model is meant to read short", n)
	}
}

// /stats (C322) and /me (with "Your trust") are held to byte budgets too.
const (
	statsPageBudget = 56_000
	// C349: Me holds the feed settings form (once /feed/tune) and the
	// Standing and allowance panel. C576: Your devices (link a device by
	// code or QR, list, remove), about 1.5 KB. C630: the Feedback form
	// (appeals), about 0.7 KB.
	mePageBudget = 75_000
)

func TestStatsAndMePageByteBudgets(t *testing.T) {
	s, _ := rfc0012Server(allOn)
	for path, budget := range map[string]int{"/stats": statsPageBudget, "/me": mePageBudget} {
		page := getHTML(t, s, path)
		dumpPage(t, strings.Trim(path, "/")+".html", page)
		t.Logf("%s: %d bytes (budget %d)", path, len(page), budget)
		if len(page) > budget {
			t.Errorf("%s is %d bytes, over its %d byte budget", path, len(page), budget)
		}
	}
	me := getHTML(t, s, "/me")
	for _, want := range []string{`id="your-trust"`, `id="trust-host"`, `id="trust-mine" aria-pressed="false"`} {
		if !strings.Contains(me, want) {
			t.Errorf("/me lacks %q", want)
		}
	}
}
