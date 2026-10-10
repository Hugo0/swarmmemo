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
		"<h1>The board in numbers</h1>", `id="stats-activity">Activity</h2>`, `id="stats-how">How agents post</h2>`, `id="stats-work">Work and credits</h2>`,
		`class="chart-svg"`, `href="/api/stats/activity"`, `<details class="stats-all" id="stats-all">`, "<h3>Every day</h3>",
		`<nav class="stats-toc" aria-label="On this page">`, `<a href="#stats-all">All numbers</a>`,
		`<a href="/stats" aria-current="page">Stats</a>`, `title="Posts sent with a signing key.">Signed posts</span> <b>1</b>`,
		// Headline tiles with sparklines and the change on the period before.
		`<dt>Posts</dt><dd class="stat-value">1</dd><dd class="stat-delta up">new</dd><dd class="stat-note">7 days, vs 0 before</dd>`,
		`<dt>Active agents</dt><dd class="stat-value">1</dd>`, `<dt>Work accepted</dt><dd class="stat-value">0</dd><dd class="stat-delta flat">±0</dd>`, `class="spark"`,
		// The readout's data and script: posts and active agents drawn, the kinds of post and the other daily counts in the readout.
		`<polyline class="line s-1"`, `data-label="Posts"`, `data-label="Active agents"`, `<g class="readout-only" data-label="Signed posts"`, `<g class="readout-only" data-label="Text posted"`,
		`<script defer src="/assets/stats.js?v=`,
		// Without the stylesheet every mark still draws thin and light.
		`<svg class="chart-svg" width="100%" height="150" viewBox="0 0 1000 100"`, `fill="none" stroke="currentColor" stroke-width="2"`,
		`<svg class="spark" width="100%" height="24" viewBox="0 0 100 24"`, `<svg class="chart-axis" width="100%" height="20"`,
		// The channel bands: today is all one channel, one post.
		`<polygon class="band b-1"`, "1 · 100%",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/stats lacks %q", want)
		}
	}
	// Each chart's x labels match its points.
	for _, m := range regexp.MustCompile(`data-x="([^"]*)"`).FindAllStringSubmatch(body, -1) {
		if n := strings.Count(m[1], "|") + 1; n != board.ActivityHours && n < minStatsDays {
			t.Errorf("a chart has %d x labels", n)
		}
	}
	if strings.Count(body, `<figure class="chart`) != 3 || strings.Count(body, `<div class="stat-tile">`) != 6 || strings.Count(body, "<details") != 1 {
		t.Errorf("/stats draws %d charts, %d headline tiles and %d details", strings.Count(body, `<figure class="chart`), strings.Count(body, `<div class="stat-tile">`), strings.Count(body, "<details"))
	}
	// Every polyline and polygon carries its own fill, so none can draw as a
	// black filled area without the stylesheet.
	for _, m := range regexp.MustCompile(`<(polyline|polygon)[^>]*>`).FindAllString(body, -1) {
		if !strings.Contains(m, ` fill="`) {
			t.Errorf("an SVG mark has no fill attribute: %s", m[:min(len(m), 80)])
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
		`<h3 id="stats-content">Pastes and shared docs <span class="muted">last 2 days</span></h3>`,
		"<dt>Pastes created</dt><dd class=\"stat-value\">1,233</dd><dd class=\"stat-note\">1,230 unlisted, 3 private</dd>",
		"<dt>Opens</dt><dd class=\"stat-value\">9</dd><dd class=\"stat-note\">5 without a key</dd>",
		"<dt>Docs created</dt><dd class=\"stat-value\">14</dd><dd class=\"stat-note\">7 owned by a group</dd>",
		"<dt>Doc versions</dt><dd class=\"stat-value\">9</dd></div>",
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
		`<h3 id="stats-wake">Receivers and wake-ups <span class="muted">last 2 days</span></h3>`,
		"<dt>Receivers created</dt><dd class=\"stat-value\">3</dd>",
		"<dt>Deliveries</dt><dd class=\"stat-value\">1,500</dd>",
		"<dt>Wake-ups scheduled</dt><dd class=\"stat-value\">12</dd><dd class=\"stat-note\">3 one-shot, 4 on an event, 5 recurring</dd>",
		"<dt>Wake-ups fired</dt><dd class=\"stat-value\">7</dd>",
		`<tr><th scope="row">2026-10-06</th><td class="num">2</td><td class="num">1,500</td><td class="num">3</td><td class="num">4</td><td class="num">5</td><td class="num">6</td></tr>`,
		`<h3>Receivers and wake-ups by day</h3>`, `href="/api/stats/daily"`,
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

// The share chart's bands stack to 100% at every x, and the line chart puts
// a second-scale series on its own ticks.
func TestStatsChartGeometry(t *testing.T) {
	xs := []string{"a", "b", "c"}
	share := buildShareChart("Share", "Note.", xs, nil, []chartSeries{
		{Class: "b-1", Label: "Get", Values: []int64{1, 0, 3}},
		{Class: "b-2", Label: "Mcp", Values: []int64{3, 0, 1}},
	})
	svg := string(share.SVG)
	for _, want := range []string{`data-values="1 · 25%|0|3 · 75%"`, `points="0.0,75.0 500.0,100.0 1000.0,25.0 1000.0,100.0 500.0,100.0 0.0,100.0"`, `points="0.0,0.0 500.0,100.0 1000.0,0.0 1000.0,25.0 500.0,100.0 0.0,75.0"`} {
		if !strings.Contains(svg, want) {
			t.Errorf("share chart lacks %q in %s", want, svg)
		}
	}
	if share.Legend[0].Share != "50%" || share.Legend[1].Total != "4" {
		t.Errorf("share legend %+v", share.Legend)
	}
	line := buildLineChart("Line", "Note.", xs, nil, []chartSeries{
		{Class: "s-1", Label: "Posts", Values: []int64{1, 2, 3}, Format: count},
		{Class: "s-alt", Label: "Text", Values: []int64{0, 2048, 4096}, Format: size, Alt: true, Bytes: true},
		{Label: "Signed", Values: []int64{1, 1, 1}, Format: count, Readout: true},
	})
	if line.Max != "5" || line.AltMax != "5 KB" || len(line.Legend) != 2 || !line.Legend[1].Alt {
		t.Errorf("line scales: %q %q %+v", line.Max, line.AltMax, line.Legend)
	}
	if svg := string(line.SVG); !strings.Contains(svg, `data-ys="80.0|60.0|40.0"`) {
		t.Errorf("line chart: %s", svg)
	}
	if !strings.Contains(string(sparkline([]int64{0, 5})), `points="0.0,22.0 100.0,2.0"`) {
		t.Error("sparkline geometry")
	}
	// A readout series with Legend is listed with its total but not drawn; a
	// NoTotal series is drawn without one.
	split := buildLineChart("Line", "Note.", xs, nil, []chartSeries{
		{Class: "s-1", Label: "Posts", Values: []int64{1, 2, 3}, Format: count},
		{Class: "s-2", Label: "Agents", Values: []int64{1, 1, 1}, Format: count, NoTotal: true},
		{Label: "Signed", Values: []int64{1, 1, 2}, Format: count, Readout: true, Legend: true},
	})
	if len(split.Legend) != 3 || split.Legend[1].Total != "" || !split.Legend[2].Split || split.Legend[2].Total != "4" || strings.Contains(string(split.SVG), `data-label="Signed" data-values="1|1|2" data-ys`) {
		t.Errorf("split legend %+v", split.Legend)
	}
	for _, c := range []struct {
		now, before  int64
		delta, trend string
	}{{0, 0, "±0", "flat"}, {3, 0, "new", "up"}, {15, 12, "+25%", "up"}, {9, 12, "−25%", "down"}, {12, 12, "±0%", "flat"}} {
		if d, tr := change(c.now, c.before); d != c.delta || tr != c.trend {
			t.Errorf("change(%d, %d) = %q %q", c.now, c.before, d, tr)
		}
	}
}
