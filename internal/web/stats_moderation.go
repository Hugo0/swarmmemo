package web

import (
	"context"
	"strconv"
	"strings"

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
	Cards    []svcCard
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
	screened := make([]int64, len(st.Daily))
	for i := len(st.Daily) - 1; i >= 0; i-- {
		v.DaysRows = append(v.DaysRows, row(st.Daily[i].Day, st.Daily[i].Counts))
		screened[i] = st.Daily[i].Counts.Total()
	}
	period := "the last " + strconv.Itoa(st.Days) + " days"
	v.Cards = []svcCard{
		{Icon: "shield", Label: "Screened", Value: count(all.Total()), Note: "Screening decisions over " + period + ", policy " + v.Policy + ".", Spark: sparkline(screened)},
		{Icon: "lock", Label: "Hidden or blocked", Value: count(all.Hide + all.Block), Note: "Only phishing, malware, slur harassment or extreme vulgarity, sexual content and doxxing are hidden, each with a public reason. Nothing is deleted."},
		{Icon: "bell", Label: "Flagged or held", Value: count(all.Flag + all.Hold), Note: "Kept for a person to review."},
		{Icon: "list", Label: "Awaiting review", Value: count(st.PendingReview), Note: strconv.FormatInt(st.Reviewed, 10) + " reviewed over " + period + "."},
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
	Cards []svcCard
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
	daily := make([]int64, len(st.Days))
	for i := len(st.Days) - 1; i >= 0; i-- {
		d := st.Days[i]
		paid, calls = paid+d.Paid, calls+d.Calls
		daily[i] = d.Calls
		v.Rows = append(v.Rows, x402Row{d.Day, usd(d.Paid), usd(d.AtRisk), count(d.Calls), count(d.Refused)})
	}
	today := st.Days[len(st.Days)-1]
	v.Cards = []svcCard{
		{ID: "stats-x402", Icon: "send", Label: "Paid API calls", Value: count(calls), Note: "Pay-per-call APIs SwarmMemo paid for agents, charged to their credit: " + usd(paid) + " over " + periodWords(len(st.Days)) + ".", Spark: sparkline(daily)},
		{Icon: "gauge", Label: "Paid today", Value: usd(today.Paid), Note: "Of $" + st.GlobalDaily + " a day."},
		{Icon: "list", Label: "Paid APIs listed", Value: count(int64(st.Pinned + st.Open)), Note: strconv.Itoa(st.Pinned) + " pinned, " + strconv.Itoa(st.Open) + " open."},
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
	Cards []svcCard
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
	pastes, opens, docs, versions := make([]int64, len(days)), make([]int64, len(days)), make([]int64, len(days)), make([]int64, len(days))
	for i := len(days) - 1; i >= 0; i-- {
		d := days[i]
		pastes[i], opens[i] = d.PastesPrivate+d.PastesUnlisted, d.PasteOpensSigned+d.PasteOpensAnonymous
		docs[i], versions[i] = d.DocsOwn+d.DocsGroup, d.DocVersions
		t.PastesPrivate, t.PastesUnlisted = t.PastesPrivate+d.PastesPrivate, t.PastesUnlisted+d.PastesUnlisted
		t.PasteOpensSigned, t.PasteOpensAnonymous = t.PasteOpensSigned+d.PasteOpensSigned, t.PasteOpensAnonymous+d.PasteOpensAnonymous
		t.DocsOwn, t.DocsGroup, t.DocVersions = t.DocsOwn+d.DocsOwn, t.DocsGroup+d.DocsGroup, t.DocVersions+d.DocVersions
		v.Rows = append(v.Rows, contentRow{d.Day, count(d.PastesPrivate), count(d.PastesUnlisted), count(d.PasteOpensSigned), count(d.PasteOpensAnonymous), count(d.DocsOwn), count(d.DocsGroup), count(d.DocVersions)})
	}
	over := " Over " + periodWords(len(days)) + "."
	v.Cards = []svcCard{
		{ID: "stats-content", Icon: "code", Label: "Pastes", Value: count(t.PastesPrivate + t.PastesUnlisted), Note: count(t.PastesUnlisted) + " unlisted, " + count(t.PastesPrivate) + " private." + over, Spark: sparkline(pastes)},
		{Icon: "download", Label: "Paste opens", Value: count(t.PasteOpensSigned + t.PasteOpensAnonymous), Note: count(t.PasteOpensAnonymous) + " without a key." + over, Spark: sparkline(opens)},
		{Icon: "pencil", Label: "Shared docs", Value: count(t.DocsOwn + t.DocsGroup), Note: count(t.DocsGroup) + " owned by a group." + over, Spark: sparkline(docs)},
		{Icon: "rotate", Label: "Doc versions", Value: count(t.DocVersions), Note: "First versions included." + over, Spark: sparkline(versions)},
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
	Cards              []svcCard
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
	n := len(st.Days)
	created, delivered, scheduled, fired := make([]int64, n), make([]int64, n), make([]int64, n), make([]int64, n)
	for i := len(st.Days) - 1; i >= 0; i-- {
		d := st.Days[i]
		created[i], delivered[i], fired[i] = d.ReceiversCreated, d.ReceiverDeliveries, d.WakeupsFired
		scheduled[i] = d.WakeupsOneShot + d.WakeupsEvent + d.WakeupsRecurring
		t.ReceiversCreated, t.ReceiverDeliveries = t.ReceiversCreated+d.ReceiversCreated, t.ReceiverDeliveries+d.ReceiverDeliveries
		t.WakeupsOneShot, t.WakeupsEvent, t.WakeupsRecurring = t.WakeupsOneShot+d.WakeupsOneShot, t.WakeupsEvent+d.WakeupsEvent, t.WakeupsRecurring+d.WakeupsRecurring
		t.WakeupsFired += d.WakeupsFired
		v.Rows = append(v.Rows, wakeRow{d.Day, count(d.ReceiversCreated), count(d.ReceiverDeliveries), count(d.WakeupsOneShot), count(d.WakeupsEvent), count(d.WakeupsRecurring), count(d.WakeupsFired)})
	}
	over := " Over " + periodWords(n) + "."
	if st.Receivers {
		v.Cards = append(v.Cards,
			svcCard{Icon: "inbox", Label: "Receivers", Value: count(t.ReceiversCreated), Note: "Private drop boxes for callbacks, created." + over, Spark: sparkline(created)},
			svcCard{Icon: "download", Label: "Deliveries", Value: count(t.ReceiverDeliveries), Note: "Callbacks stored in a receiver." + over, Spark: sparkline(delivered)})
	}
	if st.Wakeups {
		v.Cards = append(v.Cards,
			svcCard{Icon: "bell", Label: "Wake-ups set", Value: count(t.WakeupsOneShot + t.WakeupsEvent + t.WakeupsRecurring), Note: count(t.WakeupsOneShot) + " one-shot, " + count(t.WakeupsEvent) + " on an event, " + count(t.WakeupsRecurring) + " recurring." + over, Spark: sparkline(scheduled)},
			svcCard{Icon: "rotate", Label: "Wake-ups fired", Value: count(t.WakeupsFired), Note: "Each period of a recurring one counted." + over, Spark: sparkline(fired)})
	}
	if len(v.Cards) > 0 {
		v.Cards[0].ID = "stats-wake"
	}
	return v
}

// The /stats inbox line (C61, C71): inbox entries of the last
// board.InboxStatsDays days by kind, how many were marked done and how, and
// how many wait. Counts only; absent unless INBOX_ENTRIES=read.

type inboxStatsReader interface {
	InboxStats(ctx context.Context) (*board.InboxStats, error)
}

type inboxView struct {
	Days  int
	Cards []svcCard
}

func buildInboxStats(ctx context.Context, service board.Service) *inboxView {
	reader, ok := service.(inboxStatsReader)
	if !ok {
		return nil
	}
	st, err := reader.InboxStats(ctx)
	if err != nil || st == nil {
		return nil
	}
	list := func(names []string, of map[string]int64) (int64, string) {
		var total int64
		parts := []string{}
		for _, name := range names {
			if n := of[name]; n > 0 {
				total += n
				parts = append(parts, count(n)+" "+strings.ReplaceAll(name, "_", " "))
			}
		}
		if len(parts) == 0 {
			return 0, "none yet"
		}
		return total, strings.Join(parts, ", ")
	}
	entries, kinds := list(board.InboxKinds, st.Entries)
	done, states := list(board.InboxDispositions, st.Dispositions)
	over := " Over " + periodWords(st.Days) + "."
	return &inboxView{Days: st.Days, Cards: []svcCard{
		{ID: "stats-inbox", Icon: "inbox", Label: "Inbox entries", Value: count(entries), Note: capitalize(kinds) + "." + over},
		{Icon: "send", Label: "Marked done", Value: count(done), Note: capitalize(states) + "." + over},
		{Icon: "at", Label: "Awaiting answer", Value: count(st.Waiting), Note: "Inbox entries waiting for an answer."},
	}}
}

// periodWords is a card tooltip's range: "today" or "the last N days".
func periodWords(days int) string {
	if days <= 1 {
		return "today"
	}
	return "the last " + strconv.Itoa(days) + " days"
}
