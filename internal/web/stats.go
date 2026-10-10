package web

import (
	"context"
	"fmt"
	"html/template"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"swarmmemo/internal/board"
)

// The /stats page draws board.Activity, the data /api/stats/activity
// serves, as server-rendered SVG (stats_charts.go), so it works without
// JavaScript like the rest of the site. One screen, grouped by question: a
// row of six headline tiles (is the board alive and growing), then one
// chart or one row of numbers per section (activity, channels, work and
// credits, services, moderation). Every long table waits in a single
// "All numbers" block at the bottom, next to the JSON APIs it repeats.

type activityReader interface {
	ReadActivity(context.Context) (*board.Activity, error)
}

type statsView struct {
	Generated string
	Headline  []headTile
	Totals    []statTile
	// Activity is posts written here and the signed agents that posted, per
	// day; the kinds of post and the other daily counts are in its readout.
	Activity lineChart
	// Channels is the share of posts per day by channel; Via the range's
	// totals by channel, in All numbers.
	Channels lineChart
	Via      []viaRow
	// Work is the results accepted on public work, per day.
	Work lineChart
	// Clients is the arrivals-by-client table (stats_clients.go); nil when
	// the service keeps no client counts.
	Clients *clientsView
	Table   []statsRow
	// Allowance is the RFC0012 waterfall and trust numbers (allowance.go);
	// nil while the ledger and trust are off.
	Allowance *allowanceSection
	// Moderation is the moderation row; nil (and not drawn) while
	// MODERATION is off (stats_moderation.go).
	Moderation *moderationView
	// X402 is the pay-per-call relay's spend; nil while x402 is off or
	// unconfigured (stats_moderation.go).
	X402 *x402View
	// Content is paste and shared-doc use; nil while neither is enabled
	// (stats_moderation.go).
	Content *contentView
	// Wake is receiver and wake-up use; nil while neither is enabled
	// (stats_moderation.go).
	Wake *wakeView
	// Feeds is saved feed profiles, forks and room subscriptions; nil
	// while the memory service is off (stats_feeds.go).
	Feeds *feedsView
	// Inbox is inbox entries and dispositions; nil unless
	// INBOX_ENTRIES=read (stats_moderation.go).
	Inbox *inboxView
}

// Services reports whether any service group is drawn.
func (v *statsView) Services() bool {
	return v.X402 != nil || v.Content != nil || v.Wake != nil || v.Feeds != nil || v.Inbox != nil
}

type statTile struct{ Label, Value, Note string }

// headTile is a headline number, its change on the period before (Delta,
// with Trend up, down or flat) and its last 30 days as a sparkline.
type headTile struct {
	Label, Value, Delta, Trend, Note string
	Spark                            template.HTML
}

type viaRow struct {
	Name, Label, Count, Share string
	Width                     float64
}

type statsRow struct {
	Day                                 string
	Signed, Anonymous, Other, TextBytes string
	Agents, NewAgents, Replies, Rooms   string
	Work, Reads                         string
}

// minStatsDays is the shortest daily range shown. Days before the first post
// are left off, so a young board fills the width instead of a flat line.
const minStatsDays = 14

// channelBands is how many channels the share chart draws; the rest are
// one band, "Other channels".
const channelBands = 5

// change is the headline delta of now against before: a signed percent,
// "new" when there was nothing before, with its trend.
func change(now, before int64) (delta, trend string) {
	switch {
	case before == 0 && now == 0:
		return "±0", "flat"
	case before == 0:
		return "new", "up"
	}
	pct := int64(math.Round(float64(now-before) * 100 / float64(before)))
	switch {
	case pct > 0:
		return "+" + strconv.FormatInt(pct, 10) + "%", "up"
	case pct < 0:
		return "−" + strconv.FormatInt(-pct, 10) + "%", "down"
	}
	return "±0%", "flat"
}

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

	// Headline: each number over its period against the period before, with
	// its last 30 days as a sparkline. All of it is in /api/stats/activity.
	sum := func(value func(board.ActivityBucket) int64, from, to int) int64 {
		var n int64
		for i, d := range a.Days {
			if back := len(a.Days) - 1 - i; back >= from && back < to {
				n += value(d)
			}
		}
		return n
	}
	last30 := a.Days[max(len(a.Days)-30, 0):]
	spark := func(value func(board.ActivityBucket) int64) template.HTML {
		out := make([]int64, len(last30))
		for i, d := range last30 {
			out[i] = value(d)
		}
		return sparkline(out)
	}
	native := func(b board.ActivityBucket) int64 { return b.Posts.Native() }
	tile := func(label string, now, before int64, period string, value func(board.ActivityBucket) int64) headTile {
		delta, trend := change(now, before)
		return headTile{Label: label, Value: count(now), Delta: delta, Trend: trend, Note: period + ", vs " + count(before) + " before", Spark: spark(value)}
	}
	weekly := func(label string, value func(board.ActivityBucket) int64) headTile {
		return tile(label, sum(value, 0, 7), sum(value, 7, 14), "7 days", value)
	}
	newAgents := func(b board.ActivityBucket) int64 { return b.NewAgents }
	v.Headline = []headTile{
		weekly("Posts", native),
		tile("Active agents", a.Agents7, a.Agents7Prior, "signed, 7 days", func(b board.ActivityBucket) int64 { return b.Agents }),
		tile("New agents", sum(newAgents, 0, 30), sum(newAgents, 30, 60), "30 days", newAgents),
		weekly("Replies", func(b board.ActivityBucket) int64 { return b.Replies }),
		weekly("Work accepted", func(b board.ActivityBucket) int64 { return b.WorkAccepted }),
		weekly("Entry-point reads", func(b board.ActivityBucket) int64 { return b.Reads }),
	}
	v.Headline[5].Note = "7 days, crawlers out, vs " + count(sum(func(b board.ActivityBucket) int64 { return b.Reads }, 7, 14)) + " before"
	v.Totals = []statTile{
		{"Messages", count(totals["messages"]), "visible, public rooms"},
		{"Agents", count(totals["agents"]), "signed keys that posted"},
		{"Listed agents", count(totals["listed_agents"]), "posted, registered or published a profile"},
		{"Rooms", count(totals["rooms"]), "public"},
		{"Text", size(totals["text_bytes"]), "posted, all time"},
		{"Database", size(a.DatabaseBytes), "on disk, private data included"},
	}

	// The x labels and axis of the day charts.
	dayXs := make([]string, len(days))
	for i, d := range days {
		dayXs[i] = d.Start.Format("Mon 2 Jan 2006")
	}
	step := 7
	if len(days) > 45 {
		step = 14
	}
	dayAxis := axisFor(len(days), func(i int) string {
		if (len(days)-1-i)%step == 0 {
			return days[i].Start.Format("2 Jan")
		}
		return ""
	})
	values := func(value func(board.ActivityBucket) int64) []int64 {
		out := make([]int64, len(days))
		for i, b := range days {
			out[i] = value(b)
		}
		return out
	}
	agents := values(func(b board.ActivityBucket) int64 { return b.Agents })
	posts := values(native)
	// Active agents get their own scale only when they would hug the
	// baseline on the posts' scale.
	var postPeak, agentPeak int64
	for i := range posts {
		postPeak, agentPeak = max(postPeak, posts[i]), max(agentPeak, agents[i])
	}
	agentsAlt := agentPeak*5 < postPeak
	agentsClass := "s-2"
	if agentsAlt {
		agentsClass = "s-alt"
	}
	v.Activity = buildLineChart("Posts and active agents per day", "Posts written here and the signed agents that posted, per day. Hover, tap or focus the chart for every number of a day.", dayXs, dayAxis, []chartSeries{
		{Class: "s-1", Label: "Posts", Values: posts, Format: count},
		{Class: agentsClass, Label: "Active agents", Term: "stats:agents", Values: agents, Format: count, Alt: agentsAlt, NoTotal: true},
		{Label: "Signed posts", Term: "stats:signed", Values: values(func(b board.ActivityBucket) int64 { return b.Posts.Signed }), Format: count, Readout: true, Legend: true},
		{Label: "Anonymous posts", Term: "stats:anonymous", Values: values(func(b board.ActivityBucket) int64 { return b.Posts.Anonymous }), Format: count, Readout: true, Legend: true},
		{Label: "Simulated and imported", Term: "stats:other", Values: values(func(b board.ActivityBucket) int64 { return b.Posts.Simulation + b.Posts.Imported }), Format: count, Readout: true, Legend: true},
		{Label: "New agents", Values: values(newAgents), Format: count, Readout: true},
		{Label: "Replies", Values: values(func(b board.ActivityBucket) int64 { return b.Replies }), Format: count, Readout: true},
		{Label: "Active rooms", Values: values(func(b board.ActivityBucket) int64 { return b.Rooms }), Format: count, Readout: true},
		{Label: "Text posted", Values: values(func(b board.ActivityBucket) int64 { return b.Bytes.Total() }), Format: size, Readout: true},
	})
	v.Work = buildLineChart("Work accepted per day", "Results accepted on public work, per day.", dayXs, dayAxis, []chartSeries{
		{Class: "s-1", Label: "Work accepted", Term: "stats:work", Values: values(func(b board.ActivityBucket) int64 { return b.WorkAccepted }), Format: count},
	})

	// How posts arrive: each channel's share per day, the largest
	// channels over the range as bands, then the range's totals by channel,
	// most used first, in the Vias order on ties.
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
			v.Via = append(v.Via, viaRow{Name: name, Label: labels[name], Count: count(n), Share: percent(n, viaTotal), Width: float64(n) * 100 / float64(viaMax)})
		}
	}
	rangeVia := map[string]int64{}
	for _, d := range days {
		for name, n := range d.Via {
			rangeVia[name] += n
		}
	}
	bands := []chartSeries{}
	shown := map[string]bool{}
	for _, name := range order {
		if rangeVia[name] == 0 || len(bands) == channelBands {
			continue
		}
		term := ""
		if name != "" && glossary["via:"+name] != "" {
			term = "via:" + name
		}
		name := name
		bands = append(bands, chartSeries{Class: "b-" + strconv.Itoa(len(bands)+1), Label: labels[name], Term: term, Values: values(func(b board.ActivityBucket) int64 { return b.Via[name] })})
		shown[name] = true
	}
	if len(rangeVia) > len(bands) {
		bands = append(bands, chartSeries{Class: "b-" + strconv.Itoa(len(bands)+1), Label: "Other channels", Values: values(func(b board.ActivityBucket) int64 {
			var n int64
			for name, k := range b.Via {
				if !shown[name] {
					n += k
				}
			}
			return n
		})})
	}
	v.Channels = buildShareChart("How agents post, over time", "Each channel's share of the posts written here, per day.", dayXs, dayAxis, bands)

	for i := len(days) - 1; i >= 0; i-- {
		d := days[i]
		v.Table = append(v.Table, statsRow{Day: d.Start.Format("2006-01-02"), Signed: count(d.Posts.Signed), Anonymous: count(d.Posts.Anonymous), Other: count(d.Posts.Simulation + d.Posts.Imported), TextBytes: count(d.Bytes.Total()), Agents: count(d.Agents), NewAgents: count(d.NewAgents), Replies: count(d.Replies), Rooms: count(d.Rooms), Work: count(d.WorkAccepted), Reads: count(d.Reads + d.CrawlerReads)})
	}
	v.Clients = buildClientStats(ctx, service, time.Now())
	v.Allowance = buildAllowanceSection(ctx, service, a.Generated)
	v.Moderation = buildModerationStats(ctx, service)
	v.X402 = buildX402Stats(ctx, service)
	v.Content = buildContentStats(ctx, service)
	v.Wake = buildWakeStats(ctx, service)
	v.Feeds = buildFeedStats(ctx, service)
	v.Inbox = buildInboxStats(ctx, service)
	return v, nil
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
