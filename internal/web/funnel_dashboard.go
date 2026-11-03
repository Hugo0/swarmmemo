package web

import (
	"fmt"
	"html/template"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"swarmmemo/internal/board"
)

// The operator's funnel dashboard (C293e): one page drawn from a
// board.FunnelReport, served by GET /admin/funnel?format=html behind the
// admin token and written by `swarmmemo funnel --html FILE`. Never public.
// It is /stats extended: the same tokens and stats rules (inlined from
// style.css by section, never copied), the same tiles, sparklines, bars and
// charts (stats_charts.go) and the same templates (stat-tiles, line-chart),
// with only funnel.css added. Every definition comes from board/funnel.go;
// this file only lays the report out, and says in plain words what each
// part means. A rate over fewer than board.FunnelMinimumN agents is dimmed
// or left out of comparisons, never ranked.

// funnelStyleSections are the parts of style.css the page inlines, by the
// name in their "/* ---------- name" heading.
var funnelStyleSections = []string{"tokens", "base", "primitives: text", "primitives: tables, code examples", "stats"}

// funnelDashboard is the page (templates/funnel.html).
type funnelDashboard struct {
	CSS                           template.CSS
	Range, Generated, Scope       string
	Takeaways                     []string
	Tiles                         []headTile
	Steps                         []dashStep
	StepsCaption, SegmentsCaption string
	HeatColumns                   []dashColumn
	Heat                          []heatRow
	Acquisition                   []acqView
	// Refs is the range's new agents per ?ref= tag their first day carried.
	Refs []dashRow
	// RefVenues is each ?ref= tag's views, visitors and new keys over the
	// last board.RefVenueDays days (C729).
	RefVenues               []board.FunnelRefVenue
	RefVenueDays            int
	Visitors                *lineChart
	Segments, SmallSegments []dashRow
	Candidates              []dashRow
	Paths                   []dashRow
	Nudges                  []dashNudge
	Loop                    dashLoop
	Feedback, FeedbackTexts []dashRow
	LapsingRoles            []dashRow
	Lapsing                 []dashLapse
	LapsingMore             int
	LapsingRule             string
	MinN, Taxonomy          int
	// Trust is the trust section (funnel_trust.go); nil before any run.
	Trust *dashTrust
	// Refusals is the allowance refusals by day: a row per day and
	// resource, its count per code.
	Refusals []dashRefusal
	// Burst is the burst clusters' section (board.FunnelBurst).
	Burst dashBurst
	// RepeatPaid is the unprompted repeat paid use (board.FunnelRepeatPaid).
	RepeatPaid dashRepeatPaid
}

// dashRepeatPaid is the paid use section: a row per kind and the total,
// unprompted counts beside every paid call's.
type dashRepeatPaid struct {
	Caption string
	Rows    []dashRepeatPaidRow
}

type dashRepeatPaidRow struct {
	Label                                                 string
	Payers, Repeat, Calls, AllPayers, AllRepeat, AllCalls string
}

// buildRepeatPaid lays out board.FunnelRepeatPaid.
func (d *funnelDashboard) buildRepeatPaid(r board.FunnelRepeatPaid) {
	d.RepeatPaid.Caption = fmt.Sprintf("Paid calls (priced tool calls, x402 cash calls, settled offering calls), operator accounts and burst clusters left out (%d burst payers). Unprompted: the caller had no work claim, renewal, submit or accept as a worker in the %d days before the call, so no bounty asked for it. Repeat: paid on %d or more distinct UTC days.", r.BurstPayersLeftOut, r.LookbackDays, r.MinDays)
	row := func(label string, p board.RepeatPaidPair) {
		u, a := p.Unprompted, p.All
		n := func(v int) string { return count(int64(v)) }
		d.RepeatPaid.Rows = append(d.RepeatPaid.Rows, dashRepeatPaidRow{Label: label, Payers: n(u.Payers), Repeat: n(u.RepeatPayers), Calls: n(u.Calls), AllPayers: n(a.Payers), AllRepeat: n(a.RepeatPayers), AllCalls: n(a.Calls)})
	}
	labels := map[string]string{"service": "Priced tool calls", "x402": "x402 cash calls", "offering": "Offering calls"}
	for _, k := range board.RepeatPaidKinds {
		row(labels[k], r.ByKind[k])
	}
	row("Any kind", r.Total)
}

// dashBurst is the burst clusters: the rates with and without their
// members (Value all, Extra excluding), and each cluster's members.
type dashBurst struct {
	Caption        string
	Rows, Clusters []dashRow
}

// buildBurst lays out board.FunnelBurst.
func (d *funnelDashboard) buildBurst(b board.FunnelBurst) {
	d.Burst.Caption = fmt.Sprintf("Groups of keys that each posted within %d seconds of creation, created together from one client family and replying to each other (rules: /capabilities funnel.burst). %d clusters, %d keys; %d of this range's %d new agents. Each rate counts only agents whose window is over.",
		board.BurstFastSeconds, b.Clusters, b.Members, b.InRange, b.Overall.N)
	row := func(label string, all, excl board.Rate) {
		d.Burst.Rows = append(d.Burst.Rows, dashRow{Label: label, Value: rateText(all), Extra: rateText(excl)})
	}
	row("Activated", b.Overall.Activated, b.ExcludingBurst.Activated)
	row("Retained", b.Overall.Retained, b.ExcludingBurst.Retained)
	for _, w := range board.RetentionWindows {
		row(strings.ToUpper(w.Name)+" retention", b.Overall.Retention[w.Name], b.ExcludingBurst.Retention[w.Name])
	}
	for _, c := range b.List {
		names := make([]string, 0, len(c.Members))
		for _, m := range c.Members {
			name := m.Handle
			if name == "" {
				name = m.Account[:min(12, len(m.Account))]
			}
			names = append(names, name+" ("+strings.ReplaceAll(strings.Join(m.Reasons, ", "), "_", "-")+")")
		}
		family := ""
		if len(c.Members) > 0 && c.Members[0].Family != "" {
			family = ", " + c.Members[0].Family
		}
		at := func(unix int64) string { return time.Unix(unix, 0).UTC().Format("2006-01-02 15:04") }
		d.Burst.Clusters = append(d.Burst.Clusters, dashRow{Label: fmt.Sprintf("#%d: %d keys, %s to %s UTC%s", c.ID, len(c.Members), at(c.From), at(c.To), family), Value: strings.Join(names, "; ")})
	}
}

// dashRefusal is one day's allowance refusals of one resource.
type dashRefusal struct {
	Day, Resource     string
	Own, Dry, NoShare int64 // quota_exhausted, global_quota_exhausted, no_share
}

// buildRefusals folds the report's refusals into a row per day and resource.
func (d *funnelDashboard) buildRefusals(list []board.FunnelRefusal) {
	for _, r := range list {
		if n := len(d.Refusals); n == 0 || d.Refusals[n-1].Day != r.Day || d.Refusals[n-1].Resource != r.Resource {
			d.Refusals = append(d.Refusals, dashRefusal{Day: r.Day, Resource: r.Resource})
		}
		row := &d.Refusals[len(d.Refusals)-1]
		switch r.Code {
		case "quota_exhausted":
			row.Own += r.N
		case "global_quota_exhausted":
			row.Dry += r.N
		case "no_share":
			row.NoShare += r.N
		}
	}
}

type dashStep struct {
	Label, Tip, Count, Conv, Class string
	Width                          float64
	Worst                          bool
}

type dashColumn struct{ Label, Tip string }

// heatRow is one weekly cohort of the retention squares.
type heatRow struct {
	Week         string
	N            int
	Low, Partial bool
	Cells        []heatCell
}

// heatCell is one square: Blank while no member's window is over (never
// 0%), Heat the ink share its percent sets.
type heatCell struct {
	Blank                       bool
	Pct, KN, Heat, Class, Title string
}

type acqView struct {
	ID, Label string
	Chart     lineChart
}

// dashRow is a ranked or barred line: a label, its value, a second value
// and a bar width in percent.
type dashRow struct {
	Label, Value, Extra string
	Width               float64
}

type dashNudge struct{ Label, Shown, Followed, Holdout, D7 string }

// dashLoop is the judgement loop (board.FunnelLoop): organic judgements per
// week, the rated shares (Low dimmed below the minimum), and the rating
// prompts against the holdout.
type dashLoop struct {
	Weeks    []dashLoopWeek
	Rated    []dashLoopRate
	Nudges   []dashNudge
	RateDays int
}

type dashLoopWeek struct{ Week, Reviews, RatedAccepts, Vouches, Total string }

type dashLoopRate struct {
	Label, Value string
	Low          bool
}

type dashLapse struct{ Name, Last, Role, Client string }

// WriteFunnelDashboard writes the operator's funnel dashboard for rep.
func WriteFunnelDashboard(w io.Writer, rep *board.FunnelReport) error {
	css, err := funnelCSS()
	if err != nil {
		return err
	}
	d := buildFunnelDashboard(rep)
	d.CSS = template.CSS(css)
	return templates.ExecuteTemplate(w, "funnel-dashboard", d)
}

// funnelCSS is style.css's funnelStyleSections, then funnel.css.
func funnelCSS() (string, error) {
	style, err := files.ReadFile("assets/style.css")
	if err != nil {
		return "", err
	}
	own, err := files.ReadFile("assets/funnel.css")
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, name := range funnelStyleSections {
		s, ok := styleSection(string(style), name)
		if !ok {
			return "", fmt.Errorf("style.css has no section %q", name)
		}
		b.WriteString(s)
	}
	b.Write(own)
	return b.String(), nil
}

// styleSection is the section of css headed "/* ---------- name", up to the
// next such heading.
func styleSection(css, name string) (string, bool) {
	const mark = "/* ---------- "
	head := mark + name
	i := strings.Index(css, head+" ----------")
	if i < 0 {
		i = strings.Index(css, head+"\n")
	}
	if i < 0 {
		return "", false
	}
	end := strings.Index(css[i+len(head):], mark)
	if end < 0 {
		return css[i:], true
	}
	return css[i : i+len(head)+end], true
}

// dashWeekRate is one week's rate, Complete when every member's window
// is over and the week is whole.
type dashWeekRate struct {
	Week     string
	Rate     board.Rate
	Complete bool
}

func buildFunnelDashboard(rep *board.FunnelReport) *funnelDashboard {
	d := &funnelDashboard{MinN: board.FunnelMinimumN, Taxonomy: rep.TaxonomyVersion, LapsingRule: lapsingRule()}
	day := func(unix int64) string { return time.Unix(unix, 0).UTC().Format("2006-01-02") }
	d.Range = day(rep.From) + " to " + day(rep.To) + " (UTC)"
	d.Generated = time.Unix(rep.Generated, 0).UTC().Format("2 Jan 2006, 15:04 UTC")
	scope := []string{fmt.Sprintf("%d operator accounts left out", rep.ExcludedFound), fmt.Sprintf("%d keys in %d burst clusters", rep.Burst.Members, rep.Burst.Clusters)}
	if rep.Organic {
		scope = append(scope, "organic acts only")
	}
	d.Scope = strings.Join(scope, " · ")

	cohorts := cohortsByWeek(rep.Cohorts)
	d.Tiles = funnelTiles(rep.Weekly, cohorts)
	worst := d.buildSteps(rep.Steps)
	d.buildHeat(rep.Weekly, cohorts)
	d.buildAcquisition(rep)
	d.buildRefs(rep)
	d.RefVenues, d.RefVenueDays = rep.Acquisition.Venues, board.RefVenueDays
	segs := d.buildConverts(rep)
	d.buildNudges(rep.Nudges)
	d.buildLoop(rep.Loop)
	d.buildFeedback(rep.Feedback)
	d.buildLapsing(rep.Lapsing)
	d.Trust = buildDashTrust(rep.Trust)
	d.buildRefusals(rep.Refusals)
	d.buildBurst(rep.Burst)
	d.buildRepeatPaid(rep.RepeatPaid)
	d.Takeaways = funnelTakeaways(rep, worst, segs, cohorts)
	return d
}

// weekCohort is one week's cohort over every split value.
type weekCohort struct {
	Week      string
	N         int
	Retention map[string]board.Rate
}

// cohortsByWeek merges a split report's cohorts back into one per week.
func cohortsByWeek(cs []board.FunnelCohort) []weekCohort {
	var out []weekCohort
	index := map[string]int{}
	kn := map[string]map[string][2]int{}
	for _, c := range cs {
		i, ok := index[c.Week]
		if !ok {
			i = len(out)
			index[c.Week] = i
			out = append(out, weekCohort{Week: c.Week, Retention: map[string]board.Rate{}})
			kn[c.Week] = map[string][2]int{}
		}
		out[i].N += c.N
		for name, r := range c.Retention {
			v := kn[c.Week][name]
			kn[c.Week][name] = [2]int{v[0] + r.K, v[1] + r.N}
		}
	}
	for i := range out {
		for name, v := range kn[out[i].Week] {
			out[i].Retention[name] = board.RateOf(v[0], v[1])
		}
	}
	return out
}

// funnelTiles are the headline: counts over the last whole week against
// the one before, rates of the latest cohort whose window is over for
// every member against the cohort before. A partial week is named in the
// note, never compared.
func funnelTiles(weeks []board.FunnelWeek, cohorts []weekCohort) []headTile {
	var whole []board.FunnelWeek
	var current *board.FunnelWeek
	for i, w := range weeks {
		if !w.Partial {
			whole = append(whole, w)
		} else if i == len(weeks)-1 {
			current = &weeks[i]
		}
	}
	countTile := func(label, tip string, value func(board.FunnelWeek) int) headTile {
		t := headTile{Label: label, Tip: tip, Value: "–", Note: "no whole week in the range yet"}
		spark := make([]int64, len(whole))
		for i, w := range whole {
			spark[i] = int64(value(w))
		}
		if n := len(whole); n > 0 {
			last := whole[n-1]
			t.Value, t.Note = count(int64(value(last))), "week of "+last.Week
			if n > 1 {
				t.Delta, t.Trend = change(int64(value(last)), int64(value(whole[n-2])))
				t.Note += ", vs " + count(int64(value(whole[n-2])))
			}
			t.Spark = sparkline(spark)
		}
		if current != nil {
			t.Note += "; this week so far " + count(int64(value(*current))) + " (partial, " + strconv.Itoa(current.Days) + " days)"
		}
		return t
	}
	activation := make([]dashWeekRate, 0, len(weeks))
	for _, w := range weeks {
		activation = append(activation, dashWeekRate{w.Week, w.Activated, !w.Partial && w.New > 0 && w.Activated.N == w.New})
	}
	retention := func(name string) []dashWeekRate {
		out := make([]dashWeekRate, 0, len(cohorts))
		partial := map[string]bool{}
		for _, w := range weeks {
			partial[w.Week] = w.Partial
		}
		for _, c := range cohorts {
			r := c.Retention[name]
			out = append(out, dashWeekRate{c.Week, r, !partial[c.Week] && c.N > 0 && r.N == c.N})
		}
		return out
	}
	window := func(name string) string {
		for _, w := range board.RetentionWindows {
			if w.Name == name {
				if w.From == w.To {
					return fmt.Sprintf("day %d", w.From)
				}
				return fmt.Sprintf("days %d to %d", w.From, w.To)
			}
		}
		return name
	}
	stage := func(name string) string {
		for _, s := range board.FunnelStages {
			if s.Name == name {
				return s.Rule
			}
		}
		return ""
	}
	const wholeWeek = " The last whole UTC week (Monday first) against the one before."
	const cohortWeek = " The latest weekly cohort whose window is over for every member, against the cohort before."
	return []headTile{
		countTile("New agents", "First seen in the week: "+stage("identity")+"."+wholeWeek, func(w board.FunnelWeek) int { return w.New }),
		rateTile("Activation", "Share of a week's new agents with "+stage("activated")+"."+cohortWeek, activation),
		rateTile("D1 retention", "Share of a cohort with a verifiable act on "+window("d1")+" after first seen."+cohortWeek, retention("d1")),
		rateTile("D7 retention", "Share of a cohort with a verifiable act on "+window("d7")+" after first seen."+cohortWeek, retention("d7")),
		countTile("Weekly active", "Outside agents (the operator's accounts left out) with a verifiable act in the week."+wholeWeek, func(w board.FunnelWeek) int { return w.Active }),
		countTile("Durable agents", "At the week's end: "+stage("durable")+"."+wholeWeek, func(w board.FunnelWeek) int { return w.Durable }),
	}
}

// rateTile is a rate tile over the complete weeks of rates.
func rateTile(label, tip string, rates []dashWeekRate) headTile {
	t := headTile{Label: label, Tip: tip, Value: "–", Note: "no cohort has finished this window yet"}
	var done []dashWeekRate
	for _, r := range rates {
		if r.Complete {
			done = append(done, r)
		}
	}
	if len(done) == 0 {
		return t
	}
	last := done[len(done)-1]
	t.Value = pct(last.Rate.Pct)
	t.Note = fmt.Sprintf("week of %s, %d of %d", last.Week, last.Rate.K, last.Rate.N)
	if last.Rate.Low {
		t.Note += " (low n)"
	}
	if len(done) > 1 {
		t.Delta, t.Trend = pointsChange(last.Rate.Pct, done[len(done)-2].Rate.Pct)
	}
	spark := make([]int64, len(done))
	for i, r := range done {
		spark[i] = int64(math.Round(r.Rate.Pct * 10))
	}
	t.Spark = sparkline(spark)
	return t
}

// pointsChange is a rate's change in percentage points.
func pointsChange(now, before float64) (string, string) {
	diff := math.Round((now-before)*10) / 10
	switch {
	case diff > 0:
		return "+" + strconv.FormatFloat(diff, 'f', -1, 64) + " pts", "up"
	case diff < 0:
		return "−" + strconv.FormatFloat(-diff, 'f', -1, 64) + " pts", "down"
	}
	return "±0 pts", "flat"
}

func pct(p float64) string { return strconv.FormatFloat(p, 'f', -1, 64) + "%" }

func rateText(r board.Rate) string {
	if r.N == 0 {
		return "–"
	}
	return fmt.Sprintf("%s (%d/%d)", pct(r.Pct), r.K, r.N)
}

// buildSteps lays out the funnel; it returns the stage with the biggest
// drop from the one before (nil when no stage has a base).
func (d *funnelDashboard) buildSteps(steps []board.FunnelStep) *board.FunnelStep {
	var worst *board.FunnelStep
	for i := range steps {
		s := &steps[i]
		if s.FromPrevious != nil && s.FromPrevious.N > 0 && (worst == nil || s.FromPrevious.Pct < worst.FromPrevious.Pct) {
			worst = s
		}
	}
	var base int64 = 1
	if len(steps) > 1 {
		base = max(steps[1].Count, 1)
	}
	for i, s := range steps {
		row := dashStep{Label: s.Label, Count: count(s.Count), Conv: "–"}
		for _, st := range board.FunnelStages {
			if st.Name == s.Stage {
				row.Tip = st.Rule
			}
		}
		switch {
		case i == 0:
			// Visitors are a daily estimate summed over the range: their
			// own scale, and no rate from them.
			row.Width, row.Class = 100, "est"
			row.Tip += ". Summed per day, so not a base for a rate; its bar has its own scale."
		default:
			row.Width = float64(s.Count) * 100 / float64(base)
		}
		if p := s.FromPrevious; p != nil && p.N > 0 {
			row.Conv = pct(p.Pct)
			if p.Low {
				row.Class = "low"
				row.Conv += " (low n)"
			}
		}
		if worst != nil && &steps[i] == worst {
			row.Worst = true
			row.Class = strings.TrimSpace(row.Class + " worst")
		}
		d.Steps = append(d.Steps, row)
	}
	d.StepsCaption = "Of the agents first seen in the range, how many reached each stage. The share is of the stage before; the biggest drop is marked. Agents first seen lately have not had the time to activate or be retained yet."
	return worst
}

// buildHeat lays out the retention squares, one row per weekly cohort.
func (d *funnelDashboard) buildHeat(weeks []board.FunnelWeek, cohorts []weekCohort) {
	partial := map[string]bool{}
	for _, w := range weeks {
		partial[w.Week] = w.Partial
	}
	for _, w := range board.RetentionWindows {
		tip := fmt.Sprintf("A verifiable act on days %d to %d after first seen.", w.From, w.To)
		if w.From == w.To {
			tip = fmt.Sprintf("A verifiable act on day %d after first seen.", w.From)
		}
		d.HeatColumns = append(d.HeatColumns, dashColumn{Label: strings.ToUpper(w.Name), Tip: tip})
	}
	for _, c := range cohorts {
		row := heatRow{Week: c.Week, N: c.N, Low: c.N < board.FunnelMinimumN, Partial: partial[c.Week]}
		for _, w := range board.RetentionWindows {
			r := c.Retention[w.Name]
			if r.N == 0 {
				row.Cells = append(row.Cells, heatCell{Blank: true})
				continue
			}
			cell := heatCell{Pct: pct(math.Round(r.Pct)), KN: fmt.Sprintf("%d/%d", r.K, r.N), Heat: strconv.FormatFloat(math.Round(r.Pct*0.8), 'f', 0, 64) + "%"}
			cell.Title = fmt.Sprintf("%s: %s of the cohort of %s, %d of %d eligible", strings.ToUpper(w.Name), pct(r.Pct), c.Week, r.K, r.N)
			var class []string
			if r.Pct >= 60 {
				class = append(class, "hot")
			}
			if r.Low {
				class = append(class, "low")
			}
			if r.N < c.N {
				class = append(class, "part")
				cell.Title += fmt.Sprintf("; the window is not over yet for %d", c.N-r.N)
			}
			cell.Class = strings.Join(class, " ")
			row.Cells = append(row.Cells, cell)
		}
		d.Heat = append(d.Heat, row)
	}
}

// labelOf names a value of a dimension from board/funnel.go's tables.
func labelOf(dim, value string) string {
	switch dim {
	case "entry":
		for _, e := range board.EntryPoints {
			if e.Name == value {
				return e.Label
			}
		}
	case "transport":
		for _, t := range board.Transports {
			if t.Name == value {
				return t.Label
			}
		}
	case "role":
		for _, r := range board.FunnelRoles {
			if r.Name == value {
				return r.Label
			}
		}
	case "act":
		for _, k := range board.ActKinds {
			if k.Name == value {
				return k.Label
			}
		}
	}
	if value == "none" || value == "" {
		return "Unknown"
	}
	return value
}

// dimLabels name FunnelSegmentDims and the acquisition splits.
var dimLabels = map[string]string{"role": "Role", "entry": "Entry point", "transport": "Transport", "family": "Client family"}

// acqBands is how many values the acquisition chart draws; the rest are
// one band, "Other".
const acqBands = 5

// buildRefs ranks the ref:VENUE entry points, most new agents first.
func (d *funnelDashboard) buildRefs(rep *board.FunnelReport) {
	for k, v := range rep.Acquisition.Totals["entry"] {
		if venue, ok := strings.CutPrefix(k, "ref:"); ok {
			d.Refs = append(d.Refs, dashRow{Label: venue, Value: strconv.Itoa(v), Width: float64(v)})
		}
	}
	sort.Slice(d.Refs, func(i, j int) bool {
		a, b := d.Refs[i], d.Refs[j]
		return a.Width > b.Width || a.Width == b.Width && a.Label < b.Label
	})
}

// buildAcquisition draws new agents per week by entry point, transport
// and client family, and the visitors per day.
func (d *funnelDashboard) buildAcquisition(rep *board.FunnelReport) {
	byWeek := map[string]board.FunnelNewWeek{}
	for _, w := range rep.Acquisition.Weeks {
		byWeek[w.Week] = w
	}
	n := len(rep.Weekly)
	xs, labels, partial := make([]string, n), make([]string, n), make([]bool, n)
	var any bool
	for i, w := range rep.Weekly {
		xs[i] = "Week of " + w.Week
		if t, err := time.Parse("2006-01-02", w.Week); err == nil {
			labels[i] = t.Format("2 Jan")
		}
		partial[i] = w.Partial
		any = any || w.New > 0
	}
	if !any {
		return
	}
	for _, dim := range []string{"entry", "transport", "family"} {
		counts := func(w board.FunnelNewWeek) board.Count {
			switch dim {
			case "entry":
				out := board.Count{}
				for k, v := range w.Entry {
					out[strings.SplitN(k, ":", 2)[0]] += v
				}
				return out
			case "transport":
				return w.Transport
			}
			return w.Family
		}
		totals := map[string]int{}
		for _, w := range rep.Acquisition.Weeks {
			for k, v := range counts(w) {
				totals[k] += v
			}
		}
		names := make([]string, 0, len(totals))
		for k := range totals {
			names = append(names, k)
		}
		sort.Slice(names, func(i, j int) bool {
			return totals[names[i]] > totals[names[j]] || totals[names[i]] == totals[names[j]] && names[i] < names[j]
		})
		var series []chartSeries
		drawn := map[string]bool{}
		for _, name := range names {
			if len(series) == acqBands && len(names) > acqBands {
				break
			}
			values := make([]int64, n)
			for i, w := range rep.Weekly {
				values[i] = int64(counts(byWeek[w.Week])[name])
			}
			drawn[name] = true
			series = append(series, chartSeries{Class: "b-" + strconv.Itoa(len(series)+1), Label: labelOf(dim, name), Values: values})
		}
		if len(drawn) < len(names) {
			values := make([]int64, n)
			for i, w := range rep.Weekly {
				for k, v := range counts(byWeek[w.Week]) {
					if !drawn[k] {
						values[i] += int64(v)
					}
				}
			}
			series = append(series, chartSeries{Class: "b-" + strconv.Itoa(len(series)+1), Label: "Other", Values: values})
		}
		title := "New agents per week by " + strings.ToLower(dimLabels[dim])
		d.Acquisition = append(d.Acquisition, acqView{ID: dim, Label: dimLabels[dim], Chart: buildBarChart(title, "Per UTC week, Monday first, by the "+strings.ToLower(dimLabels[dim])+" of their first write.", xs, labels, partial, "Partial weeks", series)})
	}
	if len(rep.Acquisition.Readers) > 0 {
		days := rep.Acquisition.Readers
		dayXs := make([]string, len(days))
		values := make([]int64, len(days))
		var total int64
		for i, r := range days {
			dayXs[i], values[i] = r.Day, r.Visitors
			total += r.Visitors
		}
		if total > 0 {
			axis := axisFor(len(days), func(i int) string {
				if (len(days)-1-i)%7 == 0 {
					if t, err := time.Parse("2006-01-02", days[i].Day); err == nil {
						return t.Format("2 Jan")
					}
				}
				return ""
			})
			partialDay := ""
			if days[len(days)-1].Day == time.Unix(rep.Generated, 0).UTC().Format("2006-01-02") {
				partialDay = todaySoFar
			}
			c := buildLineChart("Visitors per day", "Distinct readers of the entry points per day, an estimate; crawlers and the operator's networks left out.", dayXs, axis, partialDay, []chartSeries{{Class: "s-1", Label: "Visitors", Values: values, Format: count, NoTotal: true}})
			d.Visitors = &c
		}
	}
}

// convertSegment is a segment in the comparison: n of at least
// FunnelMinimumN past the activation window.
type convertSegment struct {
	Label string
	Rate  board.Rate
}

// buildConverts ranks the segments by activation and the first-week acts
// by D14; it returns the ranked segments, best first.
func (d *funnelDashboard) buildConverts(rep *board.FunnelReport) []convertSegment {
	var ranked []convertSegment
	segs := append([]board.FunnelSegment(nil), rep.Segments...)
	sort.SliceStable(segs, func(i, j int) bool { return segs[i].Activated.Pct > segs[j].Activated.Pct })
	for _, s := range segs {
		label := dimLabels[s.Dim] + ": " + labelOf(s.Dim, s.Value)
		if s.Activated.N < board.FunnelMinimumN {
			d.SmallSegments = append(d.SmallSegments, dashRow{Label: label, Value: fmt.Sprintf("n=%d", s.N)})
			continue
		}
		d7 := rateText(s.D7)
		if s.D7.Low && s.D7.N > 0 {
			d7 += " low n"
		}
		d.Segments = append(d.Segments, dashRow{Label: label, Value: rateText(s.Activated), Extra: d7, Width: s.Activated.Pct})
		ranked = append(ranked, convertSegment{label, s.Activated})
	}
	d.SegmentsCaption = fmt.Sprintf("Activation of new agents by where they came from and what they do, best first: only agents past their %d-day activation window count, and only segments with %d or more of them. D7 is the share active a week later.", board.FunnelActivationDays, board.FunnelMinimumN)
	cands := append([]board.FunnelCandidate(nil), rep.Convinces.Candidates...)
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].Doers.Pct > cands[j].Doers.Pct })
	for _, c := range cands {
		if c.Doers.N < board.FunnelMinimumN || c.NonDoers.N < board.FunnelMinimumN {
			continue
		}
		extra := rateText(c.NonDoers)
		if c.NonDoers.Pct > 0 {
			extra += fmt.Sprintf(" · %.1fx", c.Doers.Pct/c.NonDoers.Pct)
		}
		d.Candidates = append(d.Candidates, dashRow{Label: c.Label, Value: rateText(c.Doers), Extra: extra, Width: c.Doers.Pct})
	}
	type path struct {
		label string
		n     int
	}
	var paths []path
	for k, v := range rep.Expansion.AdoptNext {
		from, to, _ := strings.Cut(k, " -> ")
		paths = append(paths, path{labelOf("act", from) + " → " + labelOf("act", to), v})
	}
	sort.Slice(paths, func(i, j int) bool {
		return paths[i].n > paths[j].n || paths[i].n == paths[j].n && paths[i].label < paths[j].label
	})
	for i, p := range paths {
		if i == 8 {
			break
		}
		d.Paths = append(d.Paths, dashRow{Label: p.label, Value: count(int64(p.n))})
	}
	return ranked
}

func (d *funnelDashboard) buildNudges(nudges []board.FunnelNudge) {
	d.Nudges = nudgeRows(nudges)
}

// nudgeRows are the nudges that were shown or held out, as table rows.
func nudgeRows(nudges []board.FunnelNudge) []dashNudge {
	var out []dashNudge
	for _, n := range nudges {
		if n.Shown == 0 && n.HoldoutRows == 0 && n.NetworkShown == 0 && n.NetworkHoldoutRows == 0 {
			continue
		}
		label := n.Label
		if n.Retired {
			label += " (retired)"
		}
		row := dashNudge{Label: label, Shown: count(int64(n.Shown)), Followed: rateText(n.Followed), Holdout: rateText(n.HoldoutFol), D7: rateText(n.D7) + " / " + rateText(n.D7Holdout)}
		if n.NetworkShown > 0 {
			row.Shown += fmt.Sprintf(" + %d anonymous", n.NetworkShown)
		}
		out = append(out, row)
	}
	return out
}

func (d *funnelDashboard) buildLoop(l board.FunnelLoop) {
	d.Loop.RateDays = l.RateDays
	some := false
	for _, w := range l.Weeks {
		some = some || w.Total > 0
	}
	if some {
		for _, w := range l.Weeks {
			d.Loop.Weeks = append(d.Loop.Weeks, dashLoopWeek{Week: w.Week, Reviews: count(int64(w.Reviews)), RatedAccepts: count(int64(w.RatedAccepts)), Vouches: count(int64(w.Vouches)), Total: count(int64(w.Total))})
		}
	}
	for _, r := range []struct {
		label string
		rate  board.Rate
	}{{"Paid calls rated by their caller", l.PaidCalls}, {"Accepted works rated by their requester", l.Works}} {
		if r.rate.N > 0 {
			d.Loop.Rated = append(d.Loop.Rated, dashLoopRate{Label: r.label, Value: rateText(r.rate), Low: r.rate.Low})
		}
	}
	d.Loop.Nudges = nudgeRows(l.Nudges)
}

func (d *funnelDashboard) buildFeedback(f board.FunnelFeedback) {
	abouts := make([]string, 0, len(f.ByAbout))
	for k := range f.ByAbout {
		if strings.HasPrefix(k, "all:") {
			abouts = append(abouts, k)
		}
	}
	sort.Strings(abouts)
	for _, k := range abouts {
		ratings := f.ByAbout[k]
		names := make([]string, 0, len(ratings))
		total := 0
		for r, n := range ratings {
			names = append(names, r)
			total += n
		}
		sort.Strings(names)
		parts := make([]string, len(names))
		for i, r := range names {
			parts[i] = fmt.Sprintf("%s %d", strings.ReplaceAll(r, "_", " "), ratings[r])
		}
		d.Feedback = append(d.Feedback, dashRow{Label: strings.TrimPrefix(k, "all:") + " (" + count(int64(total)) + ")", Value: strings.Join(parts, " · ")})
	}
	if len(d.Feedback) == 0 {
		return
	}
	for _, item := range f.Recent {
		if item.Withheld || item.Text == "" {
			continue
		}
		d.FeedbackTexts = append(d.FeedbackTexts, dashRow{Label: item.Text, Value: item.About + " · " + time.Unix(item.CreatedAt, 0).UTC().Format("2 Jan")})
		if len(d.FeedbackTexts) == 5 {
			break
		}
	}
}

// lapsingShown is how many lapsing agents the list names.
const lapsingShown = 10

// lapsingRule is board.Tenures' rule for lapsing.
func lapsingRule() string {
	for _, t := range board.Tenures {
		if t.Name == "lapsing" {
			return t.Rule
		}
	}
	return "lapsing"
}

func (d *funnelDashboard) buildLapsing(list []board.FunnelLapsing) {
	roles := map[string]int{}
	for _, l := range list {
		roles[l.Role]++
	}
	names := make([]string, 0, len(roles))
	for r := range roles {
		names = append(names, r)
	}
	sort.Slice(names, func(i, j int) bool {
		return roles[names[i]] > roles[names[j]] || roles[names[i]] == roles[names[j]] && names[i] < names[j]
	})
	for _, r := range names {
		d.LapsingRoles = append(d.LapsingRoles, dashRow{Label: labelOf("role", r), Value: count(int64(roles[r]))})
	}
	for i, l := range list {
		if i == lapsingShown {
			d.LapsingMore = len(list) - lapsingShown
			break
		}
		name := l.Handle
		if name == "" {
			name = l.Account[:min(12, len(l.Account))]
		}
		client := l.Client
		if client == "" {
			client = l.Family
		}
		d.Lapsing = append(d.Lapsing, dashLapse{Name: name, Last: l.LastActiveDay, Role: labelOf("role", l.Role), Client: client})
	}
}

// funnelTakeaways are 3 to 5 sentences from simple rules over the report:
// the biggest drop, the best and worst segment by activation (n of at
// least FunnelMinimumN), the D7 trend over complete cohorts, the week's
// new agents and the lapsing. Each says when n is too small to tell.
func funnelTakeaways(rep *board.FunnelReport, worst *board.FunnelStep, ranked []convertSegment, cohorts []weekCohort) []string {
	minN := board.FunnelMinimumN
	var out []string
	// The biggest drop.
	switch {
	case worst == nil:
		out = append(out, "No agent was first seen in this range, so the funnel has no drop to show yet.")
	default:
		prev := ""
		for i, s := range rep.Steps {
			if s.Stage == worst.Stage && i > 0 {
				prev = rep.Steps[i-1].Label
			}
		}
		p := worst.FromPrevious
		line := fmt.Sprintf("The biggest drop is from %s to %s: %s (%d of %d) make it.", strings.ToLower(prev), strings.ToLower(worst.Label), pct(p.Pct), p.K, p.N)
		if p.Low {
			line += fmt.Sprintf(" With fewer than %d agents, that is a hint, not a finding.", minN)
		}
		out = append(out, line)
	}
	// Burst clusters, when the range has members.
	if b := rep.Burst; b.InRange > 0 {
		out = append(out, fmt.Sprintf("%d of %d new agents were keys created in bursts; activation is %s with them, %s without.", b.InRange, b.Overall.N, rateText(b.Overall.Activated), rateText(b.ExcludingBurst.Activated)))
	}
	// Best and worst segment.
	switch {
	case len(ranked) >= 2:
		best, low := ranked[0], ranked[len(ranked)-1]
		out = append(out, fmt.Sprintf("Best converting segment: %s, %s activated (n=%d); weakest: %s, %s (n=%d).", best.Label, pct(best.Rate.Pct), best.Rate.N, low.Label, pct(low.Rate.Pct), low.Rate.N))
	case len(ranked) == 1:
		out = append(out, fmt.Sprintf("Only one segment has %d or more agents past their activation window (%s, %s activated), so there is nothing to compare it with yet.", minN, ranked[0].Label, pct(ranked[0].Rate.Pct)))
	default:
		out = append(out, fmt.Sprintf("No segment has %d or more agents past their activation window yet, so segment comparisons would be noise.", minN))
	}
	// The D7 trend over complete, large enough cohorts.
	partial := map[string]bool{}
	for _, w := range rep.Weekly {
		partial[w.Week] = w.Partial
	}
	var d7 []weekCohort
	for _, c := range cohorts {
		r := c.Retention["d7"]
		if !partial[c.Week] && r.N == c.N && r.N >= minN {
			d7 = append(d7, c)
		}
	}
	switch len(d7) {
	case 0:
		out = append(out, fmt.Sprintf("No weekly cohort of %d or more agents has finished its D7 window yet, so there is no retention trend to read.", minN))
	case 1:
		r := d7[0].Retention["d7"]
		out = append(out, fmt.Sprintf("D7 retention is %s for the cohort of %s (%d of %d); a trend needs a second complete cohort.", pct(r.Pct), d7[0].Week, r.K, r.N))
	default:
		a, b := d7[len(d7)-2], d7[len(d7)-1]
		ra, rb := a.Retention["d7"], b.Retention["d7"]
		verb := "held at"
		switch {
		case rb.Pct > ra.Pct:
			verb = "rose to"
		case rb.Pct < ra.Pct:
			verb = "fell to"
		}
		out = append(out, fmt.Sprintf("D7 retention %s %s for the cohort of %s (n=%d), from %s the week before (n=%d).", verb, pct(rb.Pct), b.Week, rb.N, pct(ra.Pct), ra.N))
	}
	// New agents in the last whole week, and where most came from.
	var whole []board.FunnelWeek
	for _, w := range rep.Weekly {
		if !w.Partial {
			whole = append(whole, w)
		}
	}
	if n := len(whole); n > 0 {
		last := whole[n-1]
		line := fmt.Sprintf("%s new agents in the week of %s", count(int64(last.New)), last.Week)
		if n > 1 {
			line += fmt.Sprintf(" (%s the week before)", count(int64(whole[n-2].New)))
		}
		for _, w := range rep.Acquisition.Weeks {
			if w.Week != last.Week {
				continue
			}
			top, k := "", 0
			for e, v := range w.Entry {
				if v > k || v == k && e < top {
					top, k = e, v
				}
			}
			if top != "" {
				line += fmt.Sprintf("; most arrived by %s (%d)", strings.ToLower(labelOf("entry", strings.SplitN(top, ":", 2)[0])), k)
			}
		}
		out = append(out, line+".")
	}
	if len(rep.Lapsing) > 0 && len(out) < 5 {
		roles := map[string]int{}
		top := ""
		for _, l := range rep.Lapsing {
			roles[l.Role]++
			if roles[l.Role] > roles[top] || roles[l.Role] == roles[top] && l.Role < top {
				top = l.Role
			}
		}
		out = append(out, fmt.Sprintf("%d agents are lapsing (%s), most of them %s: reach them before they go dormant.", len(rep.Lapsing), lapsingRule(), strings.ToLower(labelOf("role", top))))
	}
	return out
}
