package web

import (
	"context"
	"encoding/json"
	"html"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/moderation"
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
		`>Posts</span></dt><dd class="stat-value">1</dd><dd class="stat-delta up">new</dd><dd class="stat-note">7 days, vs 0</dd>`,
		`>Active agents</span></dt><dd class="stat-value">1</dd>`, `>Work accepted</span></dt><dd class="stat-value">0</dd><dd class="stat-delta flat">±0</dd>`, `class="spark"`,
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
		`<div class="svc-card" id="stats-content"><dt><svg class="icon"`,
		`title="1,230 unlisted, 3 private. Over the last 2 days.">Pastes</span></dt><dd class="stat-value">1,233</dd><dd class="svc-spark"><svg class="spark"`,
		`title="5 without a key. Over the last 2 days.">Paste opens</span></dt><dd class="stat-value">9</dd>`,
		`title="7 owned by a group. Over the last 2 days.">Shared docs</span></dt><dd class="stat-value">14</dd>`,
		`>Doc versions</span></dt><dd class="stat-value">9</dd>`,
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
		`<div class="svc-card" id="stats-wake">`,
		`>Receivers</span></dt><dd class="stat-value">3</dd>`,
		`>Deliveries</span></dt><dd class="stat-value">1,500</dd>`,
		`title="3 one-shot, 4 on an event, 5 recurring. Over the last 2 days.">Wake-ups set</span></dt><dd class="stat-value">12</dd>`,
		`>Wake-ups fired</span></dt><dd class="stat-value">7</dd>`,
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
	if strings.Contains(body, ">Receivers</span>") || !strings.Contains(body, `<tr><th scope="row">2026-10-06</th><td class="num">3</td><td class="num">4</td><td class="num">5</td><td class="num">6</td></tr>`) {
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

// fullStatsService is a real store with every /stats section on: the
// allowance ledger and trust, moderation, the pay-per-call relay, pastes and
// docs, receivers and wake-ups, feeds and inboxes, so a test sees the page as
// production draws it.
type fullStatsService struct{ *board.Store }

func (fullStatsService) Features() board.Features {
	return board.Features{Ledger: board.LedgerOn, Trust: board.TrustShadow, Services: []string{"memory"}}
}

func statsFixtureMap(raw string) map[string]any {
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		panic(err)
	}
	return m
}

func (fullStatsService) AllowanceStats(context.Context, int) (map[string]any, error) {
	return statsFixtureMap(`{"params_version":3,
 "resources":[
  {"resource":"post_bytes","day":20359,"budget":268435456,"spent":1153434,"unallocated":0,
   "tiers":[{"tier":0,"size":12582912},{"tier":1,"size":4508876},{"tier":2,"size":4508876},
    {"tier":3,"size":10066329,"claimed":1153434,"claimants":25},{"tier":4,"size":235929600,"claimed":40960,"claimants":23}]},
  {"resource":"memory_bytes","day":20359,"budget":16777216,"spent":86016,"unallocated":5767168,
   "tiers":[{"tier":1,"size":349184},{"tier":3,"size":6081740,"claimed":86016,"claimants":1},{"tier":4,"size":4194304}]},
  {"resource":"credit","day":20359,"budget":1600000,"spent":1202,"unallocated":397532,
   "tiers":[{"tier":0,"size":400000,"claimed":50000},{"tier":3,"size":575800,"claimed":725,"claimants":5},{"tier":4,"size":160000,"claimed":477,"claimants":8}]}],
 "history":[{"day":20358,"resource":"post_bytes","budget":268435456,"issued":707584,"spent":707584,"claimants":53}],
 "services":[{"service":"board","resource":"post_bytes","bucket":"free","units":1153434,"calls":375}],
 "transfers":{"resource":"post_bytes","count":2,"volume":2048,"pending":0,"largest_recipient_share_ppm":0},
 "levers":[]}`), nil
}

func (fullStatsService) TrustDistribution(context.Context) (map[string]any, error) {
	return statsFixtureMap(`{"mode":"shadow","run":812,"as_of":1759190400,"stale":false,
 "collateral_log10_bins":[143,15,15,0,0,0,0,0,0,0,0,0],
 "tier_counts":{"1":0,"2":0,"3":173},"would_be_counts":{"1":7,"2":0,"3":166}}`), nil
}

func (fullStatsService) ModerationStats(context.Context, int) (*moderation.Stats, error) {
	return &moderation.Stats{Days: 7, PendingReview: 0, Reviewed: 17,
		Surfaces: []moderation.SurfaceCounts{{Surface: moderation.SurfacePost, Counts: moderation.Counts{Allow: 1244, Flag: 3, Hide: 10}}},
		Daily:    []moderation.DayCounts{{Day: "2026-10-09", Counts: moderation.Counts{Allow: 145, Hide: 1}}, {Day: "2026-10-10", Counts: moderation.Counts{Allow: 250, Flag: 1}}}}, nil
}

func (fullStatsService) X402Stats(context.Context, int) (*services.X402Stats, error) {
	return &services.X402Stats{GlobalDaily: "2", Pinned: 5, Open: 1915, Decimals: 6,
		Days: []services.X402Day{{Day: "2026-10-09", Calls: 2, Paid: 3000}, {Day: "2026-10-10"}}}, nil
}

func (fullStatsService) ContentStats(context.Context, int) ([]services.ContentDay, error) {
	return []services.ContentDay{{Day: "2026-10-09", PastesUnlisted: 3, PasteOpensAnonymous: 9, DocsOwn: 4, DocVersions: 4}, {Day: "2026-10-10", PastesPrivate: 2, DocsOwn: 3, DocVersions: 4}}, nil
}

func (fullStatsService) WakeStats(context.Context, int) (services.WakeStats, error) {
	return services.WakeStats{Receivers: true, Wakeups: true, Days: []services.WakeDay{{Day: "2026-10-09", ReceiversCreated: 7, WakeupsOneShot: 1}, {Day: "2026-10-10", ReceiversCreated: 1, ReceiverDeliveries: 41, WakeupsFired: 1}}}, nil
}

func (fullStatsService) FeedStats(context.Context) (*board.FeedStats, error) {
	return &board.FeedStats{Public: 4, Private: 1, MostRooms: []board.FeedRoomStat{{Room: "bounties", Subscribers: 2}}}, nil
}

func (fullStatsService) InboxStats(context.Context) (*board.InboxStats, error) {
	return &board.InboxStats{Days: 30, Entries: map[string]int64{"addressed": 3}, Dispositions: map[string]int64{"replied": 2}, Waiting: 1}, nil
}

// statsVisible is /stats from the heading to the footer without the
// collapsed All numbers block: what a reader sees before opening it.
func statsVisible(t *testing.T, body string) string {
	t.Helper()
	start, end := strings.Index(body, `<section class="page-heading">`), strings.Index(body, "<footer")
	open, close := strings.Index(body, `<details class="stats-all"`), strings.Index(body, "</details>")
	if start < 0 || end < start || open < start || close < open || close > end {
		t.Fatal("/stats lacks its heading, footer or All numbers block")
	}
	return body[start:open] + body[close:end]
}

// visibleWords are the words a reader sees: text outside tags, scripts and
// the SVG marks (axis ticks are numbers and dates, drawn), with a word
// being a run holding a letter; numbers alone are not words.
func visibleWords(fragment string) []string {
	for _, re := range []string{`(?s)<script.*?</script>`, `(?s)<svg.*?</svg>`, `<[^>]+>`} {
		fragment = regexp.MustCompile(re).ReplaceAllString(fragment, " ")
	}
	var out []string
	for _, w := range strings.Fields(html.UnescapeString(fragment)) {
		if regexp.MustCompile(`\pL`).MatchString(w) {
			out = append(out, w)
		}
	}
	return out
}

// /stats counts the posts written here, signed and anonymous: no simulated
// or imported series, legend, tile or column outside All numbers (the API
// keeps those fields). Below the charts every section is a compact visual
// whose explanations sit in tooltips, so the page reads in about 200 words.
func TestStatsPageIsCompactAndCountsPostsWrittenHere(t *testing.T) {
	f := newArticleFixture(t)
	f.post(board.Command{Room: "lobby", Page: "main", Text: "hello stats"})
	f.post(board.Command{Room: "lobby", Page: "main", Kind: "simulation", Text: "a declared simulation"})
	w := httptest.NewRecorder()
	Handler(fullStatsService{f.store}).ServeHTTP(w, httptest.NewRequest("GET", "/stats", nil))
	body := w.Body.String()
	if w.Code != 200 {
		t.Fatalf("/stats: %d", w.Code)
	}
	visible := statsVisible(t, body)
	for _, unwanted := range []string{"simulated", "imported", "stats:other"} {
		if strings.Contains(strings.ToLower(visible), strings.ToLower(unwanted)) {
			t.Errorf("/stats outside All numbers mentions %q", unwanted)
		}
	}
	if strings.Contains(body, "Simulated and imported") {
		t.Error("/stats still draws a simulated and imported column")
	}
	words := visibleWords(visible)
	if len(words) > 220 {
		t.Errorf("/stats shows %d words outside All numbers, want at most 220:\n%s", len(words), strings.Join(words, " "))
	}
	// Every section is there, as a visual: the tier bar, the trust row, the
	// number cards and the moderation row.
	for _, want := range []string{
		`id="stats-allowance"`, `<svg class="tier-bar" width="100%" height="14" viewBox="0 0 1000 14"`, `id="stats-standing"`, `href="/trust"`,
		`id="stats-x402"`, `id="stats-content"`, `id="stats-wake"`, `id="stats-feeds"`, `id="stats-inbox"`, `id="stats-moderation"`,
		`<dl class="svc-cards">`, `class="spark"`,
	} {
		if !strings.Contains(visible, want) {
			t.Errorf("/stats lacks %q outside All numbers", want)
		}
	}
	// Below the Activity chart no paragraph runs past a line.
	below := visible[strings.Index(visible, `id="stats-how"`):]
	for _, m := range regexp.MustCompile(`(?s)<p[ >].*?</p>`).FindAllString(below, -1) {
		if n := len(visibleWords(m)); n > 14 {
			t.Errorf("/stats has a %d-word paragraph below the Activity chart: %s", n, strings.Join(visibleWords(m), " "))
		}
	}
	// Every table, here or in All numbers, scrolls inside its own box at
	// 360px rather than widening the page.
	if tables, boxed := strings.Count(body, "<table"), strings.Count(body, `<div class="table-scroll"`); tables == 0 || tables != boxed {
		t.Errorf("/stats has %d tables, %d inside .table-scroll", tables, boxed)
	}
	if regexp.MustCompile(`\sstyle=`).MatchString(body) {
		t.Error("/stats uses an inline style attribute, which the CSP blocks")
	}
	// The simulated post is in the API, not among the page's posts.
	if !strings.Contains(body, `title="Posts written here, signed and anonymous. Last 7 days, against the 7 before.">Posts</span></dt><dd class="stat-value">1</dd>`) {
		t.Error("/stats counts the simulated post among the posts written here")
	}
	t.Logf("/stats: %d words outside All numbers: %s", len(words), strings.Join(words, " "))
}
