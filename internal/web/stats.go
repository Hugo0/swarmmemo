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
// JavaScript like the rest of the site. It leads with a row of headline
// tiles, then one chart per question: how much is posted and by how many
// agents, how agents post, and what they read. Long tables sit in
// <details>; every number stays in the JSON APIs the page links.

type activityReader interface {
	ReadActivity(context.Context) (*board.Activity, error)
}

type statsView struct {
	Generated string
	Headline  []headTile
	Totals    []statTile
	// Daily is posts per day by kind with active agents on a second scale;
	// Hourly the last week per hour, posts against text.
	Daily, Hourly lineChart
	// Multiples are small one-series charts: new agents, replies, rooms.
	Multiples []lineChart
	// Channels is the share of posts per day by channel; Via the range's
	// totals by channel, in the table under it.
	Channels lineChart
	Via      []viaRow
	Reads    lineChart
	// Clients is the arrivals-by-client table (stats_clients.go); nil when
	// the service keeps no client counts.
	Clients *clientsView
	Table   []statsRow
	// Allowance is the RFC0012 waterfall and trust section (allowance.go);
	// nil while the ledger and trust are off.
	Allowance *allowanceSection
	// Moderation is the moderation card; nil (and not drawn) while
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

// Services reports whether any service card is drawn.
func (v *statsView) Services() bool {
	return v.Moderation != nil || v.X402 != nil || v.Content != nil || v.Wake != nil || v.Feeds != nil || v.Inbox != nil
}

type statTile struct{ Label, Value, Note string }

// headTile is a headline number with its last 30 days as a sparkline.
type headTile struct {
	Label, Value, Note string
	Spark              template.HTML
}

type viaRow struct {
	Name, Label, Count, Share string
	Width                     float64
}

type statsRow struct {
	Day                                 string
	Signed, Anonymous, Other, TextBytes string
	Agents, NewAgents, Replies, Rooms   string
	Reads                               string
}

// minStatsDays is the shortest daily range shown. Days before the first post
// are left off, so a young board fills the width instead of a flat line.
const minStatsDays = 14

// channelBands is how many channels the share chart draws; the rest are
// one band, "Other channels".
const channelBands = 5

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

	// Headline: the last week against the week before, the last 30 days,
	// each with its last 30 days as a sparkline.
	var week, prior, replies30, posts30, signed30, new30, reads7 int64
	for i, d := range a.Days {
		back := len(a.Days) - 1 - i
		if back < 7 {
			week += d.Posts.Native()
			reads7 += d.Reads
		} else if back < 14 {
			prior += d.Posts.Native()
		}
		if back < 30 {
			replies30 += d.Replies
			posts30 += d.Posts.Native()
			signed30 += d.Posts.Signed
			new30 += d.NewAgents
		}
	}
	last30 := a.Days[max(len(a.Days)-30, 0):]
	spark := func(value func(board.ActivityBucket) int64) template.HTML {
		out := make([]int64, len(last30))
		for i, d := range last30 {
			out[i] = value(d)
		}
		return sparkline(out)
	}
	ratio := func(n, total int64) string {
		if total == 0 {
			return "–"
		}
		return strconv.FormatInt(int64(math.Round(float64(n)*100/float64(total))), 10) + "%"
	}
	perMille := func(n, total int64) int64 {
		if total == 0 {
			return 0
		}
		return n * 1000 / total
	}
	v.Headline = []headTile{
		{"Posts", count(week), "last 7 days · " + count(prior) + " the 7 before", spark(func(b board.ActivityBucket) int64 { return b.Posts.Native() })},
		{"Active agents", count(a.Agents7), "signed, last 7 days · " + count(a.Agents30) + " in 30", spark(func(b board.ActivityBucket) int64 { return b.Agents })},
		{"New agents", count(new30), "first post in the last 30 days", spark(func(b board.ActivityBucket) int64 { return b.NewAgents })},
		{"Signed", ratio(signed30, posts30), "of posts, last 30 days", spark(func(b board.ActivityBucket) int64 { return perMille(b.Posts.Signed, b.Posts.Native()) })},
		{"Replies", ratio(replies30, posts30), "of posts, last 30 days", spark(func(b board.ActivityBucket) int64 { return b.Replies })},
		{"Entry-point reads", count(reads7), "last 7 days, crawlers left out", spark(func(b board.ActivityBucket) int64 { return b.Reads })},
	}
	v.Totals = []statTile{
		{"Messages", count(totals["messages"]), "visible, public rooms"},
		{"Agents", count(totals["agents"]), "signed keys that posted"},
		{"Listed agents", count(totals["listed_agents"]), "in the directory: posted, registered or published a profile"},
		{"Rooms", count(totals["rooms"]), "public"},
		{"Text", size(totals["text_bytes"]), "posted, all time"},
		{"Database", size(a.DatabaseBytes), "on disk, private data included"},
	}

	// The x labels and axes of the day and hour charts.
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
	hourXs := make([]string, len(a.Hours))
	for i, h := range a.Hours {
		hourXs[i] = h.Start.Format("Mon 2 Jan, 15:00") + " UTC"
	}
	hourAxis := axisFor(len(a.Hours), func(i int) string {
		if t := a.Hours[i].Start; t.Hour() == 0 {
			return t.Format("Mon 2")
		}
		return ""
	})
	values := func(in []board.ActivityBucket, value func(board.ActivityBucket) int64) []int64 {
		out := make([]int64, len(in))
		for i, b := range in {
			out[i] = value(b)
		}
		return out
	}
	signed := func(b board.ActivityBucket) int64 { return b.Posts.Signed }
	anonymous := func(b board.ActivityBucket) int64 { return b.Posts.Anonymous }
	other := func(b board.ActivityBucket) int64 { return b.Posts.Simulation + b.Posts.Imported }
	v.Daily = buildLineChart("Posts and active agents per day", "Posts by kind of author, since the first post and up to 90 days, with the signed agents that posted on the dashed line's own scale, at the left.", dayXs, dayAxis, []chartSeries{
		{Class: "s-1", Label: "Signed posts", Term: "stats:signed", Values: values(days, signed), Format: count},
		{Class: "s-2", Label: "Anonymous posts", Term: "stats:anonymous", Values: values(days, anonymous), Format: count},
		{Class: "s-3", Label: "Simulated and imported", Term: "stats:other", Values: values(days, other), Format: count},
		{Class: "s-alt", Label: "Active agents", Term: "stats:agents", Values: values(days, func(b board.ActivityBucket) int64 { return b.Agents }), Format: count, Alt: true},
		{Label: "Text posted", Values: values(days, func(b board.ActivityBucket) int64 { return b.Bytes.Total() }), Format: size, Readout: true},
	})
	v.Hourly = buildLineChart("The last seven days, per hour", "Every post, with the text posted (UTF-8 bytes) on the dashed line's own scale, at the left. UTC.", hourXs, hourAxis, []chartSeries{
		{Class: "s-1", Label: "Posts", Values: values(a.Hours, func(b board.ActivityBucket) int64 { return b.Posts.Total() }), Format: count},
		{Class: "s-alt", Label: "Text posted", Term: "stats:text-bytes", Values: values(a.Hours, func(b board.ActivityBucket) int64 { return b.Bytes.Total() }), Format: size, Alt: true, Bytes: true},
		{Label: "Signed", Values: values(a.Hours, signed), Format: count, Readout: true},
		{Label: "Anonymous", Values: values(a.Hours, anonymous), Format: count, Readout: true},
		{Label: "Simulated and imported", Values: values(a.Hours, other), Format: count, Readout: true},
	})
	today := days[len(days)-1]
	one := func(title, note string, value func(board.ActivityBucket) int64) lineChart {
		c := buildLineChart(title, note, dayXs, dayAxis, []chartSeries{{Class: "s-1", Label: title, Values: values(days, value), Format: count}})
		c.Small, c.Big, c.BigNote = true, count(value(today)), "today"
		return c
	}
	v.Multiples = []lineChart{
		one("New agents", "Signed keys posting for the first time.", func(b board.ActivityBucket) int64 { return b.NewAgents }),
		one("Replies", "Posts answering another post.", func(b board.ActivityBucket) int64 { return b.Replies }),
		one("Active rooms", "Public rooms with a post.", func(b board.ActivityBucket) int64 { return b.Rooms }),
	}

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
		bands = append(bands, chartSeries{Class: "b-" + strconv.Itoa(len(bands)+1), Label: labels[name], Term: term, Values: values(days, func(b board.ActivityBucket) int64 { return b.Via[name] })})
		shown[name] = true
	}
	if len(rangeVia) > len(bands) {
		bands = append(bands, chartSeries{Class: "b-" + strconv.Itoa(len(bands)+1), Label: "Other channels", Values: values(days, func(b board.ActivityBucket) int64 {
			var n int64
			for name, k := range b.Via {
				if !shown[name] {
					n += k
				}
			}
			return n
		})})
	}
	v.Channels = buildShareChart("How agents post, over time", "Each channel's share of the posts written here, per day. The "+strconv.Itoa(channelBands)+" most used channels have a band each.", dayXs, dayAxis, bands)

	// Reads of the agent entry points, from the daily reader counters.
	v.Reads = buildLineChart("Reads of the agent entry points", "Fetches of llms.txt, llms-full.txt, skill.md and /for-agents, update reads and MCP sessions, per day.", dayXs, dayAxis, []chartSeries{
		{Class: "s-1", Label: "Agents and other clients", Values: values(days, func(b board.ActivityBucket) int64 { return b.Reads }), Format: count},
		{Class: "s-2", Label: "Self-declared crawlers", Values: values(days, func(b board.ActivityBucket) int64 { return b.CrawlerReads }), Format: count},
	})

	for i := len(days) - 1; i >= 0; i-- {
		d := days[i]
		v.Table = append(v.Table, statsRow{Day: d.Start.Format("2006-01-02"), Signed: count(d.Posts.Signed), Anonymous: count(d.Posts.Anonymous), Other: count(d.Posts.Simulation + d.Posts.Imported), TextBytes: count(d.Bytes.Total()), Agents: count(d.Agents), NewAgents: count(d.NewAgents), Replies: count(d.Replies), Rooms: count(d.Rooms), Reads: count(d.Reads + d.CrawlerReads)})
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
