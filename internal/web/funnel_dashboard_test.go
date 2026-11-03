package web

import (
	"bytes"
	"regexp"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// funnelFixture is a report with a whole-week history, a partial current
// week, a cohort too young for D30, a small cohort, segments on both sides
// of the minimum and empty nudges and feedback.
func funnelFixture() *board.FunnelReport {
	r := board.RateOf
	return &board.FunnelReport{
		Generated: 1791763200, From: 1789257600, To: 1791763200, ExcludedFound: 3, TaxonomyVersion: 1,
		Steps: []board.FunnelStep{
			{Stage: "visitor", Label: "Visitors", Count: 900},
			{Stage: "identity", Label: "New agents", Count: 60},
			{Stage: "first_act", Label: "First act", Count: 48, FromPrevious: ptr(r(48, 60))},
			{Stage: "activated", Label: "Activated", Count: 12, FromPrevious: ptr(r(12, 48))},
			{Stage: "retained", Label: "Retained", Count: 9, FromPrevious: ptr(r(9, 12))},
			{Stage: "durable", Label: "Durable", Count: 6, FromPrevious: ptr(r(6, 9))},
		},
		Weekly: []board.FunnelWeek{
			{Week: "2026-09-07", Days: 7, New: 20, Active: 30, Durable: 4, Activated: r(4, 20)},
			{Week: "2026-09-14", Days: 7, New: 24, Active: 33, Durable: 5, Activated: r(6, 24)},
			{Week: "2026-09-21", Days: 7, New: 12, Active: 35, Durable: 6, Activated: r(2, 10)},
			{Week: "2026-09-28", Days: 2, Partial: true, New: 4, Active: 9, Durable: 6, Activated: r(0, 0)},
		},
		Cohorts: []board.FunnelCohort{
			{Week: "2026-09-07", N: 20, Retention: map[string]board.Rate{"d1": r(0, 20), "d7": r(5, 20), "d14": r(3, 20), "d30": r(0, 0)}},
			{Week: "2026-09-14", N: 24, Retention: map[string]board.Rate{"d1": r(12, 24), "d7": r(9, 24), "d14": r(2, 10), "d30": r(0, 0)}},
			{Week: "2026-09-28", N: 4, Retention: map[string]board.Rate{"d1": r(1, 4), "d7": r(0, 0), "d14": r(0, 0), "d30": r(0, 0)}},
		},
		Acquisition: board.FunnelAcquisition{Weeks: []board.FunnelNewWeek{
			{Week: "2026-09-07", New: 20, Entry: board.Count{"api": 12, "ref:glama": 8}, Transport: board.Count{"http": 20}, Family: board.Count{"claude": 20}},
			{Week: "2026-09-21", New: 12, Entry: board.Count{"mcp_direct": 12}, Transport: board.Count{"mcp": 12}, Family: board.Count{"openai": 12}},
			{Week: "2026-09-28", New: 4, Entry: board.Count{"ui": 4}, Transport: board.Count{"http": 4}, Family: board.Count{"none": 4}},
		}},
		Segments: []board.FunnelSegment{
			{Dim: "entry", Value: "api", N: 12, Activated: r(6, 12), D7: r(3, 12)},
			{Dim: "entry", Value: "mcp_direct", N: 14, Activated: r(1, 14), D7: r(1, 8)},
			{Dim: "transport", Value: "get", N: 3, Activated: r(3, 3), D7: r(0, 0)},
		},
		Convinces: board.FunnelConvinces{Candidates: []board.FunnelCandidate{
			{Act: "post", Label: "Post", Doers: r(8, 16), NonDoers: r(2, 20), Lift: "5.0x"},
			{Act: "sale", Label: "Sale", Doers: r(0, 0), NonDoers: r(4, 36), Lift: "0.0x"},
		}},
		Expansion: board.FunnelExpansion{AdoptNext: board.Count{"post -> reply": 5, "reply -> work": 2}},
		Lapsing: []board.FunnelLapsing{
			{Account: "0123456789abcdef0123", LastActiveDay: "2026-09-20", Role: "poster"},
			{Account: "fedcba9876543210fedc", Handle: "weaver", LastActiveDay: "2026-09-18", Role: "worker"},
			{Account: "aaaaaaaaaaaaaaaaaaaa", LastActiveDay: "2026-09-17", Role: "poster"},
		},
		Nudges:   []board.FunnelNudge{{Code: "sign_for_replies", Label: "Sign to get replies"}},
		Feedback: board.FunnelFeedback{ByAbout: map[string]board.Count{}},
	}
}

func ptr[T any](v T) *T { return &v }

func renderFunnel(t *testing.T, rep *board.FunnelReport) string {
	t.Helper()
	var b bytes.Buffer
	if err := WriteFunnelDashboard(&b, rep); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// Every section is drawn, with its caption, and nothing loads a script.
func TestFunnelDashboardSections(t *testing.T) {
	page := renderFunnel(t, funnelFixture())
	for _, want := range []string{`id="dash-takeaways"`, `class="stat-tiles"`, `id="funnel"`, `id="retention"`, `id="acquisition"`, `id="converts"`, `id="nudges"`, `id="loop"`, `id="feedback"`, `id="lapsing"`, `class="heat"`, `class="chart-svg"`, `class="spark"`} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %s", want)
		}
	}
	for _, label := range []string{"New agents", "Activation", "D1 retention", "D7 retention", "Weekly active", "Durable agents"} {
		if !strings.Contains(page, ">"+label+"</span></dt>") {
			t.Errorf("no %q tile", label)
		}
	}
	if strings.Contains(page, "<script") || strings.Contains(page, "src=") {
		t.Fatal("the dashboard loads something")
	}
	if n := strings.Count(page, `class="dash-caption"`); n < 7 {
		t.Fatalf("%d captions, want one per section", n)
	}
	// The acquisition setting: one view per split, each with a funnel.css rule.
	css, _ := files.ReadFile("assets/funnel.css")
	for _, id := range []string{"entry", "transport", "family"} {
		if !strings.Contains(page, `id="acq-`+id+`"`) || !strings.Contains(string(css), "#acq-"+id+":checked) .acq-"+id) {
			t.Errorf("acquisition view %s: no input or no rule", id)
		}
	}
	// The biggest drop is first act to activated (25%), marked once.
	if strings.Count(page, "biggest drop</span>") != 1 || !regexp.MustCompile(`<tr class="worst">.*Activated`).MatchString(page) {
		t.Fatal("the biggest drop is not marked on Activated")
	}
}

// A retention square not yet eligible is blank, never 0%; a 0% one is drawn.
func TestFunnelDashboardBlankVersusZero(t *testing.T) {
	d := buildFunnelDashboard(funnelFixture())
	first := d.Heat[0] // d1 0 of 20, d30 not eligible
	if first.Cells[0].Blank || first.Cells[0].Pct != "0%" || first.Cells[0].KN != "0/20" {
		t.Fatalf("a real 0%% is drawn: %+v", first.Cells[0])
	}
	if !first.Cells[3].Blank || first.Cells[3].Pct != "" {
		t.Fatalf("D30 not yet eligible is blank: %+v", first.Cells[3])
	}
	second := d.Heat[1] // d14 over for 10 of 24 only
	if !strings.Contains(second.Cells[2].Class, "part") {
		t.Fatalf("a window over for some only is marked: %+v", second.Cells[2])
	}
	page := renderFunnel(t, funnelFixture())
	if n := strings.Count(page, `class="cell blank"`); n != 5 {
		t.Fatalf("%d blank squares, want 5", n)
	}
	if !strings.Contains(page, `style="--heat:0%"`) {
		t.Fatal("the 0% square has no heat")
	}
}

// A cohort under the minimum is dimmed, and so is a square under it.
func TestFunnelDashboardLowN(t *testing.T) {
	d := buildFunnelDashboard(funnelFixture())
	if d.Heat[0].Low || !d.Heat[2].Low {
		t.Fatalf("low rows: %v %v", d.Heat[0].Low, d.Heat[2].Low)
	}
	if !d.Heat[2].Partial {
		t.Fatal("the week still filling is not marked partial")
	}
	if !strings.Contains(d.Heat[2].Cells[0].Class, "low") || strings.Contains(d.Heat[0].Cells[0].Class, "low") {
		t.Fatalf("low squares: %+v %+v", d.Heat[2].Cells[0], d.Heat[0].Cells[0])
	}
	// Segments under the minimum leave the comparison; no 0.0x lift on no data.
	if len(d.Segments) != 2 || len(d.SmallSegments) != 1 || !strings.Contains(d.SmallSegments[0].Label, "GET link") {
		t.Fatalf("segments %+v small %+v", d.Segments, d.SmallSegments)
	}
	if d.Segments[0].Label != "Entry point: API" {
		t.Fatalf("segments not best first: %+v", d.Segments)
	}
	if len(d.Candidates) != 1 || strings.Contains(renderFunnel(t, funnelFixture()), "0.0x") {
		t.Fatalf("candidates %+v", d.Candidates)
	}
}

// Tiles compare whole weeks; the week still filling is named, never a drop.
func TestFunnelDashboardTiles(t *testing.T) {
	d := buildFunnelDashboard(funnelFixture())
	tiles := map[string]headTile{}
	for _, tile := range d.Tiles {
		tiles[tile.Label] = tile
	}
	if n := tiles["New agents"]; n.Value != "12" || n.Delta != "−50%" || !strings.Contains(n.Note, "this week so far 4 (partial, 2 days)") {
		t.Fatalf("new agents tile %+v", n)
	}
	if a := tiles["Weekly active"]; a.Value != "35" || a.Trend != "up" {
		t.Fatalf("weekly active compares the partial week: %+v", a)
	}
	// Activation: the week of 09-21 is still open for 2 of 12, so 09-14 is the latest complete.
	if a := tiles["Activation"]; a.Value != "25%" || a.Delta != "+5 pts" || !strings.Contains(a.Note, "2026-09-14") {
		t.Fatalf("activation tile %+v", a)
	}
	if d7 := tiles["D7 retention"]; d7.Value != "37.5%" || d7.Delta != "+12.5 pts" {
		t.Fatalf("D7 tile %+v", d7)
	}
}

// The takeaways come from rules over the data, and say when n is small.
func TestFunnelDashboardTakeaways(t *testing.T) {
	got := buildFunnelDashboard(funnelFixture()).Takeaways
	if len(got) < 3 || len(got) > 5 {
		t.Fatalf("%d takeaways: %q", len(got), got)
	}
	all := strings.Join(got, "\n")
	for _, want := range []string{
		"The biggest drop is from first act to activated: 25% (12 of 48)",
		"Best converting segment: Entry point: API, 50% activated (n=12); weakest: Entry point: MCP, 7.1% (n=14)",
		"D7 retention rose to 37.5% for the cohort of 2026-09-14 (n=24), from 25% the week before (n=20)",
		"12 new agents in the week of 2026-09-21 (24 the week before); most arrived by mcp (12)",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("takeaways lack %q:\n%s", want, all)
		}
	}
	// No data: honest lines, no invented trend or segment.
	empty := buildFunnelDashboard(&board.FunnelReport{Steps: []board.FunnelStep{{Stage: "visitor"}, {Stage: "identity"}, {Stage: "first_act", FromPrevious: ptr(board.RateOf(0, 0))}}}).Takeaways
	joined := strings.Join(empty, "\n")
	for _, want := range []string{"no drop to show", "No segment has 10 or more", "No weekly cohort of 10 or more"} {
		if !strings.Contains(joined, want) {
			t.Errorf("empty takeaways lack %q:\n%s", want, joined)
		}
	}
	// A low-n drop is called a hint.
	small := funnelFixture()
	small.Steps[3].FromPrevious = ptr(board.RateOf(1, 6))
	if s := strings.Join(buildFunnelDashboard(small).Takeaways, " "); !strings.Contains(s, "a hint, not a finding") {
		t.Fatalf("low-n drop: %s", s)
	}
}

// Empty panels say so in one line instead of an empty table.
func TestFunnelDashboardNoData(t *testing.T) {
	page := renderFunnel(t, funnelFixture())
	for _, want := range []string{"No data yet: no nudge was shown", "No data yet: no feedback", "No data yet: no organic judgement", "No data yet: no rating prompt"} {
		if strings.Count(page, want) != 1 {
			t.Errorf("want one %q", want)
		}
	}
	empty := renderFunnel(t, &board.FunnelReport{})
	if strings.Contains(empty, "<tbody></tbody>") || strings.Count(empty, "No data yet") < 6 {
		t.Fatalf("an empty report draws empty tables:\n%s", empty)
	}
	// Lapsing: a handle where there is one, else a short fingerprint.
	if !strings.Contains(page, "<code>weaver</code>") || !strings.Contains(page, "<code>0123456789ab</code>") {
		t.Fatal("lapsing names")
	}
}

// The page's stylesheet is style.css's sections, derived not copied, and
// funnel.css adds no colour and redefines no token.
func TestFunnelDashboardStyleFromStats(t *testing.T) {
	css, err := funnelCSS()
	if err != nil {
		t.Fatal(err)
	}
	style, _ := files.ReadFile("assets/style.css")
	for _, name := range funnelStyleSections {
		s, ok := styleSection(string(style), name)
		if !ok || !strings.Contains(css, s) {
			t.Fatalf("section %q not inlined", name)
		}
	}
	tokens, _ := styleSection(string(style), "tokens")
	if !strings.Contains(tokens, "prefers-color-scheme:dark") || !strings.Contains(css, ".stat-tile") {
		t.Fatal("the dark tokens or the stats rules are missing")
	}
	if _, ok := styleSection(string(style), "no such section"); ok {
		t.Fatal("styleSection found a missing section")
	}
	own, _ := files.ReadFile("assets/funnel.css")
	rules := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(string(own), "")
	if m := regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b|rgb\(|hsl\(|oklch\(|--[a-z0-9-]+\s*:`).FindString(rules); m != "" {
		t.Fatalf("funnel.css has a literal colour or defines a property: %q", m)
	}
	for _, m := range regexp.MustCompile(`var\((--[a-z0-9-]+)`).FindAllStringSubmatch(rules, -1) {
		if m[1] != "--heat" && !strings.Contains(tokens, m[1]+":") {
			t.Errorf("funnel.css uses %s, not a style.css token", m[1])
		}
	}
}

// Stacked bars: one rect per non-zero value, partial bars faint and named.
func TestBarChartPartial(t *testing.T) {
	c := buildBarChart("t", "n", []string{"a", "b"}, []string{"a", "b"}, []bool{false, true}, "Partial weeks", []chartSeries{
		{Class: "b-1", Label: "x", Values: []int64{3, 1}},
		{Class: "b-2", Label: "y", Values: []int64{0, 2}},
	})
	svg := string(c.SVG)
	if strings.Count(svg, "<rect") != 3 || strings.Count(svg, `fill-opacity=".35"`) != 2 {
		t.Fatalf("bars %s", svg)
	}
	if c.Max != "5" || c.Legend[len(c.Legend)-1].Label != "Partial weeks" {
		t.Fatalf("scale %s legend %+v", c.Max, c.Legend)
	}
}

// The judgement loop panel: weeks with judgements, the rated shares (a
// low-n one dimmed) and the rating prompts against the holdout.
func TestFunnelDashboardLoop(t *testing.T) {
	rep := funnelFixture()
	rep.Loop = board.FunnelLoop{RateDays: board.FunnelLoopRateDays,
		Weeks:     []board.FunnelLoopWeek{{Week: "2026-09-21"}, {Week: "2026-09-28", Reviews: 3, RatedAccepts: 2, Vouches: 1, Total: 6}},
		PaidCalls: board.RateOf(1, 4), Works: board.RateOf(6, 12),
		Nudges: []board.FunnelNudge{{Code: "rate_paid_call", Label: "Rate a paid call", Shown: 5, Followed: board.RateOf(2, 5), HoldoutRows: 1}}}
	page := renderFunnel(t, rep)
	for _, want := range []string{`<a href="#loop">`, `<th scope="row">2026-09-28</th><td class="num">3</td><td class="num">2</td><td class="num">1</td><td class="num">6</td>`,
		`<li class="low"><span>Paid calls rated by their caller</span><b>25% (1/4)</b>`, `<li><span>Accepted works rated by their requester</span><b>50% (6/12)</b>`,
		`<th scope="row">Rate a paid call</th><td class="num">5</td><td class="num">40% (2/5)</td>`} {
		if !strings.Contains(page, want) {
			t.Errorf("loop panel lacks %s", want)
		}
	}
}

// C469: the acquisition section ranks the ?ref= tags new agents arrived by,
// most first, and only those.
func TestFunnelDashboardRefs(t *testing.T) {
	rep := funnelFixture()
	if page := renderFunnel(t, rep); strings.Contains(page, "By ref tag") {
		t.Fatal("ref list drawn with no totals")
	}
	rep.Acquisition.Totals = map[string]board.Count{"entry": {"api": 12, "ref:skitter-colony": 3, "ref:glama": 8, "gate": 2}}
	page := renderFunnel(t, rep)
	i, j := strings.Index(page, "<code>glama</code>"), strings.Index(page, "<code>skitter-colony</code>")
	if !strings.Contains(page, "By ref tag") || i < 0 || j < i || strings.Contains(page, "<code>api</code>") || strings.Contains(page, "<code>gate</code>") {
		t.Fatalf("ref list: glama at %d, skitter-colony at %d", i, j)
	}
}

// The burst section shows each rate over all new agents and excluding burst
// clusters, the cluster count, and each cluster's members with reasons.
func TestFunnelDashboardBurst(t *testing.T) {
	r := board.RateOf
	rep := funnelFixture()
	rep.Burst = board.FunnelBurst{Clusters: 1, Members: 30, InRange: 30, Overall: board.FunnelRates{N: 58, Activated: r(2, 58), Retention: map[string]board.Rate{"d7": r(3, 58)}},
		ExcludingBurst: board.FunnelRates{N: 28, Activated: r(2, 28), Retention: map[string]board.Rate{"d7": r(3, 28)}},
		List:           []board.BurstCluster{{ID: 1, From: 1791763200, To: 1791763500, Members: []board.BurstMember{{Account: "0123456789abcdef0123", Handle: "basalt", Family: "browsers", Reasons: []string{"creation_linked", "reply_linked"}}}}}}
	page := renderFunnel(t, rep)
	for _, want := range []string{`id="burst"`, "30 keys in 1 burst clusters", "1 clusters, 30 keys; 30 of this range", "3.4% (2/58)", "7.1% (2/28)", "basalt (creation-linked, reply-linked)", "#1: 1 keys, 2026-10-12 00:00 to 2026-10-12 00:05 UTC, browsers", "30 of 58 new agents were keys created in bursts"} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

// A retired nudge's rows read under its label, marked retired.
func TestFunnelDashboardRetiredNudge(t *testing.T) {
	rep := funnelFixture()
	rep.Nudges = []board.FunnelNudge{{Code: "first_work", Label: "First work", Shown: 4, Followed: board.RateOf(1, 4)},
		{Code: "standing_bounty", Label: "Standing bounty", Retired: true, Shown: 9, Followed: board.RateOf(0, 9)}}
	page := renderFunnel(t, rep)
	for _, want := range []string{`<th scope="row">First work</th>`, `<th scope="row">Standing bounty (retired)</th><td class="num">9</td>`} {
		if !strings.Contains(page, want) {
			t.Errorf("nudges table lacks %s", want)
		}
	}
}

// C729: the acquisition section tables each ?ref= tag's views, visitors and
// new keys of the last 7 days.
func TestFunnelDashboardRefVenues(t *testing.T) {
	rep := funnelFixture()
	if page := renderFunnel(t, rep); strings.Contains(page, "Ref tag arrivals") {
		t.Fatal("venue table drawn with no venues")
	}
	rep.Acquisition.Venues = []board.FunnelRefVenue{{Ref: "skitter-rstation", Views: 41, Visitors: 17, Keys: 3, Acted: 2, Activated: 1}}
	page := renderFunnel(t, rep)
	if !strings.Contains(page, "Ref tag arrivals") || !strings.Contains(page, `<th scope="row"><code>skitter-rstation</code></th><td class="num">41</td><td class="num">17</td><td class="num">3</td><td class="num">2</td><td class="num">1</td>`) {
		t.Fatal("venue row missing")
	}
}
