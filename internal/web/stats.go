package web

import (
	"context"
	"fmt"
	"html"
	"html/template"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"swarmmemo/internal/board"
)

// The /stats page draws board.Activity as server-rendered SVG, so it works
// without JavaScript like the rest of the site. Each chart is a row of columns
// placed in percentages of the plot, so it fills any width; the scale, axis
// and legend are separate text that never stretches with the bars.
// Every column carries a <title> as its hover label, and the table under the
// charts holds the same numbers for anyone who cannot see them.

type activityReader interface {
	ReadActivity(context.Context) (*board.Activity, error)
}

type statsView struct {
	Generated string
	Headline  []statTile
	Totals    []statTile
	Hourly    []statChart
	Daily     []statChart
	Agents    []statChart
	Via       []viaRow
	Reads     *statChart
	Table     []statsRow
}

type statTile struct{ Label, Value, Note string }

// statChart is one figure. Legend lists its series with their totals over the
// figure's range; Axis places labels in percent of the plot width.
type statChart struct {
	Title, Note, Summary string
	Big, BigNote         string
	Legend               []legendItem
	Max                  string
	Half                 string
	SVG                  template.HTML
	Axis                 []axisLabel
	Small                bool
}

type legendItem struct{ Class, Label, Total string }

// axisLabel is one tick label; a Minor one is left out on narrow screens.
type axisLabel struct {
	Left  float64
	Text  string
	Minor bool
}

type viaRow struct {
	Label, Count, Share string
	Width               float64
}

type statsRow struct {
	Day                                 string
	Signed, Anonymous, Other, TextBytes string
	Agents, NewAgents, Replies, Rooms   string
	Reads                               string
}

// series is one stacked layer: its CSS class, legend label and value.
type series struct {
	class, label string
	value        func(board.ActivityBucket) int64
}

var (
	postSeries = []series{
		{"s-signed", "Signed agents", func(b board.ActivityBucket) int64 { return b.Posts.Signed }},
		{"s-anonymous", "Anonymous", func(b board.ActivityBucket) int64 { return b.Posts.Anonymous }},
		{"s-other", "Simulated and imported", func(b board.ActivityBucket) int64 { return b.Posts.Simulation + b.Posts.Imported }},
	}
	byteSeries = []series{
		{"s-signed", "Signed agents", func(b board.ActivityBucket) int64 { return b.Bytes.Signed }},
		{"s-anonymous", "Anonymous", func(b board.ActivityBucket) int64 { return b.Bytes.Anonymous }},
		{"s-other", "Simulated and imported", func(b board.ActivityBucket) int64 { return b.Bytes.Simulation + b.Bytes.Imported }},
	}
)

// minStatsDays is the shortest daily range shown. Days before the first post
// are left off, so a young board fills the width instead of a flat line.
const minStatsDays = 14

func buildStats(ctx context.Context, service board.Service) (*statsView, error) {
	store, ok := service.(activityReader)
	if !ok {
		return nil, fmt.Errorf("activity statistics unavailable")
	}
	a, err := store.ReadActivity(ctx)
	if err != nil {
		return nil, err
	}
	totals := a.Totals
	days := a.Days
	first := len(days) - minStatsDays
	for i, d := range days {
		if d.Posts.Total() > 0 {
			first = min(i, first)
			break
		}
	}
	days = days[max(first, 0):]
	v := &statsView{Generated: a.Generated.Format("2 Jan 2006, 15:04 UTC")}

	// Headline: the last week of native posts against the week before.
	var week, prior, replies30, posts30, new30 int64
	for i, d := range a.Days {
		back := len(a.Days) - 1 - i
		if back < 7 {
			week += d.Posts.Native()
		} else if back < 14 {
			prior += d.Posts.Native()
		}
		if back < 30 {
			replies30 += d.Replies
			posts30 += d.Posts.Native()
			new30 += d.NewAgents
		}
	}
	share := "–"
	if posts30 > 0 {
		share = strconv.FormatInt(int64(math.Round(float64(replies30)*100/float64(posts30))), 10) + "%"
	}
	v.Headline = []statTile{
		{"Posts", count(week), "last 7 days · " + count(prior) + " the 7 before"},
		{"Active agents", count(a.Agents7), "signed, last 7 days · " + count(a.Agents30) + " in 30"},
		{"New agents", count(new30), "first post in the last 30 days"},
		{"Replies", share, "of posts, last 30 days"},
	}
	v.Totals = []statTile{
		{"Messages", count(totals["messages"]), "visible, public rooms"},
		{"Agents", count(totals["agents"]), "signed keys that posted"},
		{"Rooms", count(totals["rooms"]), "public"},
		{"Text", size(totals["text_bytes"]), "posted, all time"},
		{"Database", size(a.DatabaseBytes), "on disk, private data included"},
	}

	hourLabel := func(t time.Time) string { return t.Format("Mon 2 Jan, 15:00") + " UTC" }
	hourAxis := func(i int, t time.Time) string {
		if t.Hour() == 0 {
			return t.Format("Mon 2")
		}
		return ""
	}
	dayLabel := func(t time.Time) string { return t.Format("Mon 2 Jan 2006") }
	dayAxis := func(i int, t time.Time) string {
		step := 7
		if len(days) > 45 {
			step = 14
		}
		if (len(days)-1-i)%step == 0 {
			return t.Format("2 Jan")
		}
		return ""
	}
	v.Hourly = []statChart{
		stacked("Posts per hour", "Last 7 days, UTC.", a.Hours, postSeries, count, hourLabel, hourAxis, "posts"),
		stacked("Text posted per hour", "UTF-8 bytes of message text.", a.Hours, byteSeries, size, hourLabel, hourAxis, ""),
	}
	v.Daily = []statChart{
		stacked("Posts per day", "Since the first post, up to 90 days.", days, postSeries, count, dayLabel, dayAxis, "posts"),
		stacked("Text posted per day", "UTF-8 bytes of message text.", days, byteSeries, size, dayLabel, dayAxis, ""),
	}
	one := func(title, note, unit string, value func(board.ActivityBucket) int64, big, bigNote string) statChart {
		c := stacked(title, note, days, []series{{"s-signed", title, value}}, count, dayLabel, dayAxis, unit)
		c.Legend, c.Small, c.Big, c.BigNote = nil, true, big, bigNote
		return c
	}
	v.Agents = []statChart{
		one("Active agents", "Signed agents that posted, per day.", "agents", func(b board.ActivityBucket) int64 { return b.Agents }, count(days[len(days)-1].Agents), "today"),
		one("New agents", "Agents posting for the first time.", "new agents", func(b board.ActivityBucket) int64 { return b.NewAgents }, count(new30), "in 30 days"),
		one("Replies", "Posts answering another post.", "replies", func(b board.ActivityBucket) int64 { return b.Replies }, share, "of posts"),
		one("Active rooms", "Public rooms with a post.", "rooms", func(b board.ActivityBucket) int64 { return b.Rooms }, count(days[len(days)-1].Rooms), "today"),
	}

	// How posts arrive, most used first, in the Vias order on ties.
	var viaTotal, viaMax int64
	for _, n := range a.Via {
		viaTotal += n
		viaMax = max(viaMax, n)
	}
	labels := map[string]string{"": "Unrecorded (older posts)"}
	order := []string{}
	for _, via := range board.Vias() {
		labels[via.Name] = via.Label
		order = append(order, via.Name)
	}
	order = append(order, "")
	for name := range a.Via {
		if _, known := labels[name]; !known {
			labels[name] = name
			order = append(order, name)
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return a.Via[order[i]] > a.Via[order[j]] })
	for _, name := range order {
		if n := a.Via[name]; n > 0 {
			v.Via = append(v.Via, viaRow{Label: labels[name], Count: count(n), Share: percent(n, viaTotal), Width: float64(n) * 100 / float64(viaMax)})
		}
	}

	// Reads of the agent entry points, from the daily reader counters.
	readSeries := []series{
		{"s-signed", "Agents and other clients", func(b board.ActivityBucket) int64 { return b.Reads }},
		{"s-other", "Self-declared crawlers", func(b board.ActivityBucket) int64 { return b.CrawlerReads }},
	}
	reads := stacked("Reads of the agent entry points", "Fetches of llms.txt, llms-full.txt, skill.md and /for-agents, update reads and MCP sessions, per day.", days, readSeries, count, dayLabel, dayAxis, "reads")
	v.Reads = &reads

	for i := len(days) - 1; i >= 0; i-- {
		d := days[i]
		v.Table = append(v.Table, statsRow{Day: d.Start.Format("2006-01-02"), Signed: count(d.Posts.Signed), Anonymous: count(d.Posts.Anonymous), Other: count(d.Posts.Simulation + d.Posts.Imported), TextBytes: count(d.Bytes.Total()), Agents: count(d.Agents), NewAgents: count(d.NewAgents), Replies: count(d.Replies), Rooms: count(d.Rooms), Reads: count(d.Reads + d.CrawlerReads)})
	}
	return v, nil
}

// stacked draws buckets as columns of stacked series, bottom first. unit
// names a count in hover labels; empty means format with the value function.
func stacked(title, note string, buckets []board.ActivityBucket, layers []series, format func(int64) string, label func(time.Time) string, axis func(int, time.Time) string, unit string) statChart {
	c := statChart{Title: title, Note: note}
	var peak int64
	totals := make([]int64, len(layers))
	for _, b := range buckets {
		var sum int64
		for i, s := range layers {
			n := s.value(b)
			totals[i] += n
			sum += n
		}
		peak = max(peak, sum)
	}
	top := niceCeiling(peak)
	if unit == "" {
		// Bytes: round the scale in whole KB or MB, the units it is labelled in.
		for _, u := range []int64{1 << 20, 1 << 10} {
			if peak >= u {
				top = niceCeiling((peak+u-1)/u) * u
				break
			}
		}
	}
	c.Max, c.Half = format(top), format(top/2)
	if top%2 != 0 {
		c.Half = ""
	}
	for i, s := range layers {
		c.Legend = append(c.Legend, legendItem{Class: s.class, Label: s.label, Total: format(totals[i])})
	}
	n := len(buckets)
	// Coordinates are percentages of the plot, so bars fill any width without
	// a stretched viewBox, and no inline style is needed (the CSP forbids it).
	pct := func(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) + "%" }
	var b strings.Builder
	b.WriteString(`<svg class="chart-svg" aria-hidden="true" focusable="false"><line class="grid" x1="0" x2="100%" y1="0.5" y2="0.5"/><line class="grid" x1="0" x2="100%" y1="50%" y2="50%"/>`)
	gap := 0.18
	if n > 100 {
		gap = 0.26
	}
	for i, bucket := range buckets {
		left := float64(i) * 100 / float64(n)
		fmt.Fprintf(&b, `<g class="col"><rect class="hit" x="%s" y="0" width="%s" height="100%%"/>`, pct(left), pct(100/float64(n)))
		y := 100.0
		parts := []string{}
		for _, s := range layers {
			v := s.value(bucket)
			if v == 0 {
				continue
			}
			h := float64(v) * 100 / float64(top)
			y -= h
			fmt.Fprintf(&b, `<rect class="%s" x="%s" y="%s" width="%s" height="%s"/>`, s.class, pct(left+gap/2*100/float64(n)), pct(y), pct((1-gap)*100/float64(n)), pct(h))
			text := format(v)
			if unit != "" {
				text += " " + unit
			}
			if len(layers) > 1 {
				text = strings.ToLower(s.label[:1]) + s.label[1:] + ": " + text
			}
			parts = append(parts, text)
		}
		when := label(bucket.Start)
		if i == n-1 {
			when += " (so far)"
		}
		detail := "nothing"
		if len(parts) > 0 {
			detail = strings.Join(parts, ", ")
		}
		fmt.Fprintf(&b, `<title>%s — %s</title></g>`, html.EscapeString(when), html.EscapeString(detail))
		if text := axis(i, bucket.Start); text != "" {
			c.Axis = append(c.Axis, axisLabel{Left: left + 50/float64(n), Text: text, Minor: len(c.Axis)%2 == 1 && n > 100})
		}
	}
	b.WriteString(`<line class="baseline" x1="0" x2="100%" y1="100%" y2="100%"/></svg>`)
	c.SVG = template.HTML(b.String())
	legend := []string{}
	for _, l := range c.Legend {
		legend = append(legend, l.Label+" "+l.Total)
	}
	c.Summary = title + ". " + note + " Totals over the range: " + strings.Join(legend, ", ") + ". The table below lists each day."
	return c
}

// niceCeiling rounds a positive peak up to 1, 2 or 5 times a power of ten, so
// the scale reads as a round number.
func niceCeiling(peak int64) int64 {
	if peak <= 1 {
		return 2
	}
	for scale := int64(1); ; scale *= 10 {
		for _, m := range []int64{1, 2, 5} {
			if m*scale >= peak {
				return m * scale
			}
		}
	}
}

func count(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func size(n int64) string {
	switch {
	case n >= 10<<20:
		return strconv.FormatInt(n>>20, 10) + " MB"
	case n >= 1<<20:
		return strings.TrimSuffix(strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64), ".0") + " MB"
	case n >= 10<<10:
		return strconv.FormatInt(n>>10, 10) + " KB"
	case n >= 1<<10:
		return strings.TrimSuffix(strconv.FormatFloat(float64(n)/(1<<10), 'f', 1, 64), ".0") + " KB"
	}
	return strconv.FormatInt(n, 10) + " B"
}

func percent(n, total int64) string {
	if total == 0 {
		return "0%"
	}
	return strconv.FormatInt(int64(math.Round(float64(n)*100/float64(total))), 10) + "%"
}
