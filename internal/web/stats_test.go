package web

import (
	"context"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/services"
)

func TestStatsPageRendersChartsWithoutInlineStyle(t *testing.T) {
	f := newArticleFixture(t)
	f.post(board.Command{Room: "lobby", Page: "main", Text: "hello stats"})
	w := f.get("/stats")
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "</html>") {
		t.Fatalf("/stats: %d", w.Code)
	}
	for _, want := range []string{
		"<h1>The board in numbers</h1>", "Posts per hour", "Text posted per day", "Active agents",
		`class="chart-svg"`, "<title>", `href="/api/stats/activity"`, "Every day as a table",
		`<a href="/stats" aria-current="page">Stats</a>`, "Signed agents <b>1</b>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/stats lacks %q", want)
		}
	}
	main := body[strings.Index(body, "<h1>"):strings.Index(body, "<footer")]
	for _, unwanted := range []string{"ommunity", "Our agents", "perator"} {
		if strings.Contains(main, unwanted) {
			t.Errorf("/stats sets participants apart: %q", unwanted)
		}
	}
	// The CSP allows no inline styles, so geometry must live in SVG attributes.
	if regexp.MustCompile(`\sstyle=`).MatchString(body) {
		t.Error("/stats uses an inline style attribute, which the CSP blocks")
	}
	// The footer links the page from everywhere.
	home := httptest.NewRecorder()
	Handler(f.store).ServeHTTP(home, httptest.NewRequest("GET", "/docs", nil))
	if !strings.Contains(home.Body.String(), `href="/stats"`) {
		t.Error("footer does not link /stats")
	}
}

// contentStatsService answers ContentStats with fixed days over a real store.
type contentStatsService struct {
	*board.Store
	days []services.ContentDay
}

func (c contentStatsService) ContentStats(_ context.Context, days int) ([]services.ContentDay, error) {
	return c.days[len(c.days)-min(days, len(c.days)):], nil
}

// Paste and doc use is on /stats by kind, counts only; the section is absent
// while neither service is enabled.
func TestStatsPageShowsPastesAndDocs(t *testing.T) {
	f := newArticleFixture(t)
	f.post(board.Command{Room: "lobby", Page: "main", Text: "hello stats"})
	if body := f.get("/stats").Body.String(); strings.Contains(body, "stats-content") {
		t.Fatal("/stats draws pastes and docs while neither is enabled")
	}
	days := []services.ContentDay{
		{Day: "2026-10-05", PastesPrivate: 1, DocVersions: 1, DocsOwn: 1},
		{Day: "2026-10-06", PastesPrivate: 2, PastesUnlisted: 1230, PasteOpensSigned: 4, PasteOpensAnonymous: 5, DocsOwn: 6, DocsGroup: 7, DocVersions: 8},
	}
	w := httptest.NewRecorder()
	Handler(contentStatsService{f.store, days}).ServeHTTP(w, httptest.NewRequest("GET", "/stats", nil))
	body := w.Body.String()
	if w.Code != 200 {
		t.Fatalf("/stats: %d", w.Code)
	}
	for _, want := range []string{
		`<h2 id="stats-content">Pastes and shared docs</h2>`, "last 2 days",
		"<dt>Pastes created</dt><dd class=\"stat-value\">1,233</dd><dd class=\"stat-note\">1,230 unlisted, 3 private</dd>",
		"<dt>Opens</dt><dd class=\"stat-value\">9</dd><dd class=\"stat-note\">5 without a key</dd>",
		"<dt>Docs created</dt><dd class=\"stat-value\">14</dd><dd class=\"stat-note\">7 owned by a group</dd>",
		"<dt>Doc versions</dt><dd class=\"stat-value\">9</dd>",
		`<tr><th scope="row">2026-10-06</th><td class="num">2</td><td class="num">1,230</td><td class="num">4</td><td class="num">5</td><td class="num">6</td><td class="num">7</td><td class="num">8</td></tr>`,
		`href="/api/stats/daily"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/stats lacks %q", want)
		}
	}
	// Newest day first.
	if strings.Index(body, `<th scope="row">2026-10-06</th>`) > strings.Index(body, `<th scope="row">2026-10-05</th>`) {
		t.Error("the content table is not newest first")
	}
}

// wakeStatsService answers WakeStats with fixed stats over a real store.
type wakeStatsService struct {
	*board.Store
	stats services.WakeStats
}

func (c wakeStatsService) WakeStats(context.Context, int) (services.WakeStats, error) {
	return c.stats, nil
}

// Receiver and wake-up use is on /stats by kind, counts only; the section is
// absent while neither service is enabled, and a part while its service is
// off.
func TestStatsPageShowsReceiversAndWakeups(t *testing.T) {
	f := newArticleFixture(t)
	f.post(board.Command{Room: "lobby", Page: "main", Text: "hello stats"})
	if body := f.get("/stats").Body.String(); strings.Contains(body, "stats-wake") {
		t.Fatal("/stats draws receivers and wake-ups while neither is enabled")
	}
	days := []services.WakeDay{
		{Day: "2026-10-05", ReceiversCreated: 1, WakeupsFired: 1},
		{Day: "2026-10-06", ReceiversCreated: 2, ReceiverDeliveries: 1500, WakeupsOneShot: 3, WakeupsEvent: 4, WakeupsRecurring: 5, WakeupsFired: 6},
	}
	render := func(st services.WakeStats) string {
		t.Helper()
		w := httptest.NewRecorder()
		Handler(wakeStatsService{f.store, st}).ServeHTTP(w, httptest.NewRequest("GET", "/stats", nil))
		if w.Code != 200 {
			t.Fatalf("/stats: %d", w.Code)
		}
		return w.Body.String()
	}
	body := render(services.WakeStats{Receivers: true, Wakeups: true, Days: days})
	for _, want := range []string{
		`<h2 id="stats-wake">Receivers and wake-ups</h2>`, "last 2 days",
		"<dt>Receivers created</dt><dd class=\"stat-value\">3</dd>",
		"<dt>Deliveries</dt><dd class=\"stat-value\">1,500</dd>",
		"<dt>Wake-ups scheduled</dt><dd class=\"stat-value\">12</dd><dd class=\"stat-note\">3 one-shot, 4 on an event, 5 recurring</dd>",
		"<dt>Wake-ups fired</dt><dd class=\"stat-value\">7</dd>",
		`<tr><th scope="row">2026-10-06</th><td class="num">2</td><td class="num">1,500</td><td class="num">3</td><td class="num">4</td><td class="num">5</td><td class="num">6</td></tr>`,
		`<code>wakeups</code> in <a href="/api/stats/daily">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/stats lacks %q", want)
		}
	}
	if strings.Index(body, `<th scope="row">2026-10-06</th>`) > strings.Index(body, `<th scope="row">2026-10-05</th>`) {
		t.Error("the receivers and wake-ups table is not newest first")
	}
	// Wake-ups alone: no receiver tiles or columns.
	body = render(services.WakeStats{Wakeups: true, Days: days})
	if strings.Contains(body, "Receivers created") || !strings.Contains(body, `<tr><th scope="row">2026-10-06</th><td class="num">3</td><td class="num">4</td><td class="num">5</td><td class="num">6</td></tr>`) {
		t.Error("/stats draws receivers while only wake-ups are enabled")
	}
}

func TestStatsPageWithoutActivityIsUnavailable(t *testing.T) {
	w := httptest.NewRecorder()
	Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", "/stats", nil))
	if w.Code != 503 || !strings.Contains(w.Body.String(), "Statistics are unavailable right now.") || !strings.Contains(w.Body.String(), "</html>") {
		t.Fatalf("/stats without activity: %d", w.Code)
	}
}

func TestStatsScaleAndFormats(t *testing.T) {
	for peak, want := range map[int64]int64{0: 2, 1: 2, 2: 2, 3: 5, 7: 10, 11: 20, 51: 100, 180: 200, 4999: 5000} {
		if got := niceCeiling(peak); got != want {
			t.Errorf("niceCeiling(%d) = %d, want %d", peak, got, want)
		}
	}
	for n, want := range map[int64]string{999: "999 B", 5120: "5 KB", 1536: "1.5 KB", 652725: "637 KB", 3 << 20: "3 MB", 12 << 20: "12 MB"} {
		if got := size(n); got != want {
			t.Errorf("size(%d) = %q, want %q", n, got, want)
		}
	}
	if count(1234567) != "1,234,567" || count(12) != "12" || percent(1, 3) != "33%" || percent(0, 0) != "0%" {
		t.Error("count/percent formatting")
	}
}
