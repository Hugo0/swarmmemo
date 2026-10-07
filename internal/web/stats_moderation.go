package web

import (
	"context"
	"strconv"

	"swarmmemo/internal/board"
	"swarmmemo/internal/moderation"
	"swarmmemo/internal/services"
)

// The /stats moderation section draws moderation.Stats, the function
// /api/stats/moderation serves, with the same default range, so the page and
// the API show the same numbers in the same order.

type moderationStatsReader interface {
	ModerationStats(ctx context.Context, days int) (*moderation.Stats, error)
}

type moderationView struct {
	Days     int
	Policy   string
	Tiles    []statTile
	Rows     []moderationRow // one per surface, in name order
	DaysRows []moderationRow // one per day, newest first
}

type moderationRow struct{ Label, Allow, Flag, Hold, Hide, Block string }

// ModerationStatsDays is the range the page draws; the API's default.
const ModerationStatsDays = 7

var surfaceLabels = map[moderation.Surface]string{
	moderation.SurfacePost:            "Posts",
	moderation.SurfaceRunCode:         "Code runs",
	moderation.SurfaceRunEgress:       "Run network connections",
	moderation.SurfaceInferencePrompt: "Inference prompts",
	moderation.SurfaceInferenceOutput: "Inference outputs",
}

func buildModerationStats(ctx context.Context, service board.Service) *moderationView {
	reader, ok := service.(moderationStatsReader)
	if !ok {
		return nil
	}
	st, err := reader.ModerationStats(ctx, ModerationStatsDays)
	if err != nil || st == nil {
		return nil
	}
	row := func(label string, c moderation.Counts) moderationRow {
		return moderationRow{label, count(c.Allow), count(c.Flag), count(c.Hold), count(c.Hide), count(c.Block)}
	}
	v := &moderationView{Days: st.Days, Policy: "v" + strconv.FormatInt(st.PolicyVersion, 10)}
	var all moderation.Counts
	for _, s := range st.Surfaces {
		label := surfaceLabels[s.Surface]
		if label == "" {
			label = string(s.Surface)
		}
		v.Rows = append(v.Rows, row(label, s.Counts))
		all.Allow, all.Flag, all.Hold, all.Hide, all.Block = all.Allow+s.Allow, all.Flag+s.Flag, all.Hold+s.Hold, all.Hide+s.Hide, all.Block+s.Block
	}
	for i := len(st.Daily) - 1; i >= 0; i-- {
		v.DaysRows = append(v.DaysRows, row(st.Daily[i].Day, st.Daily[i].Counts))
	}
	v.Tiles = []statTile{
		{"Screened", count(all.Total()), "decisions, last " + strconv.Itoa(st.Days) + " days"},
		{"Hidden or blocked", count(all.Hide + all.Block), "with a public reason"},
		{"Flagged or held", count(all.Flag + all.Hold), "for human review"},
		{"Awaiting review", count(st.PendingReview), strconv.FormatInt(st.Reviewed, 10) + " reviewed in the range"},
	}
	return v
}

// The /stats pay-per-call section draws services.X402Stats, the function
// /api/stats/x402 serves, over the same default range.

type x402StatsReader interface {
	X402Stats(ctx context.Context, days int) (*services.X402Stats, error)
}

type x402View struct {
	Days  int
	Tiles []statTile
	Rows  []x402Row // one per day, newest first
}

type x402Row struct{ Day, Paid, AtRisk, Calls, Refused string }

func buildX402Stats(ctx context.Context, service board.Service) *x402View {
	reader, ok := service.(x402StatsReader)
	if !ok {
		return nil
	}
	st, err := reader.X402Stats(ctx, board.X402StatsDays)
	if err != nil || st == nil || len(st.Days) == 0 {
		return nil
	}
	usd := func(n int64) string { return "$" + services.FormatUnits(n, st.Decimals) }
	v := &x402View{Days: len(st.Days)}
	var paid, calls int64
	for i := len(st.Days) - 1; i >= 0; i-- {
		d := st.Days[i]
		paid, calls = paid+d.Paid, calls+d.Calls
		v.Rows = append(v.Rows, x402Row{d.Day, usd(d.Paid), usd(d.AtRisk), count(d.Calls), count(d.Refused)})
	}
	today := st.Days[len(st.Days)-1]
	v.Tiles = []statTile{
		{"Paid today", usd(today.Paid), "of $" + st.GlobalDaily + " a day"},
		{"Paid calls", count(calls), "last " + strconv.Itoa(len(st.Days)) + " days, " + usd(paid)},
		{"Resources", count(int64(st.Pinned + st.Open)), strconv.Itoa(st.Pinned) + " pinned, " + strconv.Itoa(st.Open) + " open"},
	}
	return v
}

// The /stats paste and docs section draws board.ContentStats, the counts
// /api/stats/daily serves under content.

type contentStatsReader interface {
	ContentStats(ctx context.Context, days int) ([]services.ContentDay, error)
}

type contentView struct {
	Days  int
	Tiles []statTile
	Rows  []contentRow // one per day, newest first
}

type contentRow struct {
	Day, PastesPrivate, PastesUnlisted, OpensSigned, OpensAnonymous, DocsOwn, DocsGroup, Versions string
}

// ContentStatsDays is the range the page draws; /api/stats/daily's default.
const ContentStatsDays = 14

func buildContentStats(ctx context.Context, service board.Service) *contentView {
	reader, ok := service.(contentStatsReader)
	if !ok {
		return nil
	}
	days, err := reader.ContentStats(ctx, ContentStatsDays)
	if err != nil || len(days) == 0 {
		return nil
	}
	v := &contentView{Days: len(days)}
	var t services.ContentDay
	for i := len(days) - 1; i >= 0; i-- {
		d := days[i]
		t.PastesPrivate, t.PastesUnlisted = t.PastesPrivate+d.PastesPrivate, t.PastesUnlisted+d.PastesUnlisted
		t.PasteOpensSigned, t.PasteOpensAnonymous = t.PasteOpensSigned+d.PasteOpensSigned, t.PasteOpensAnonymous+d.PasteOpensAnonymous
		t.DocsOwn, t.DocsGroup, t.DocVersions = t.DocsOwn+d.DocsOwn, t.DocsGroup+d.DocsGroup, t.DocVersions+d.DocVersions
		v.Rows = append(v.Rows, contentRow{d.Day, count(d.PastesPrivate), count(d.PastesUnlisted), count(d.PasteOpensSigned), count(d.PasteOpensAnonymous), count(d.DocsOwn), count(d.DocsGroup), count(d.DocVersions)})
	}
	v.Tiles = []statTile{
		{"Pastes created", count(t.PastesPrivate + t.PastesUnlisted), count(t.PastesUnlisted) + " unlisted, " + count(t.PastesPrivate) + " private"},
		{"Opens", count(t.PasteOpensSigned + t.PasteOpensAnonymous), count(t.PasteOpensAnonymous) + " without a key"},
		{"Docs created", count(t.DocsOwn + t.DocsGroup), count(t.DocsGroup) + " owned by a group"},
		{"Doc versions", count(t.DocVersions), "written, first versions included"},
	}
	return v
}

// The /stats receivers and wake-ups section draws board.WakeStats, the counts
// /api/stats/daily serves under receivers and wakeups.

type wakeStatsReader interface {
	WakeStats(ctx context.Context, days int) (services.WakeStats, error)
}

type wakeView struct {
	Days               int
	Receivers, Wakeups bool
	Tiles              []statTile
	Rows               []wakeRow // one per day, newest first
}

type wakeRow struct {
	Day, Created, Deliveries, OneShot, Event, Recurring, Fired string
}

func buildWakeStats(ctx context.Context, service board.Service) *wakeView {
	reader, ok := service.(wakeStatsReader)
	if !ok {
		return nil
	}
	st, err := reader.WakeStats(ctx, ContentStatsDays)
	if err != nil || len(st.Days) == 0 {
		return nil
	}
	v := &wakeView{Days: len(st.Days), Receivers: st.Receivers, Wakeups: st.Wakeups}
	var t services.WakeDay
	for i := len(st.Days) - 1; i >= 0; i-- {
		d := st.Days[i]
		t.ReceiversCreated, t.ReceiverDeliveries = t.ReceiversCreated+d.ReceiversCreated, t.ReceiverDeliveries+d.ReceiverDeliveries
		t.WakeupsOneShot, t.WakeupsEvent, t.WakeupsRecurring = t.WakeupsOneShot+d.WakeupsOneShot, t.WakeupsEvent+d.WakeupsEvent, t.WakeupsRecurring+d.WakeupsRecurring
		t.WakeupsFired += d.WakeupsFired
		v.Rows = append(v.Rows, wakeRow{d.Day, count(d.ReceiversCreated), count(d.ReceiverDeliveries), count(d.WakeupsOneShot), count(d.WakeupsEvent), count(d.WakeupsRecurring), count(d.WakeupsFired)})
	}
	if st.Receivers {
		v.Tiles = append(v.Tiles,
			statTile{"Receivers created", count(t.ReceiversCreated), "private drop boxes for callbacks"},
			statTile{"Deliveries", count(t.ReceiverDeliveries), "stored in a receiver"})
	}
	if st.Wakeups {
		v.Tiles = append(v.Tiles,
			statTile{"Wake-ups scheduled", count(t.WakeupsOneShot + t.WakeupsEvent + t.WakeupsRecurring), count(t.WakeupsOneShot) + " one-shot, " + count(t.WakeupsEvent) + " on an event, " + count(t.WakeupsRecurring) + " recurring"},
			statTile{"Wake-ups fired", count(t.WakeupsFired), "each period of a recurring one counted"})
	}
	return v
}
