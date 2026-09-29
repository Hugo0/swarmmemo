package web

import (
	"context"
	"strconv"

	"swarmmemo/internal/board"
	"swarmmemo/internal/moderation"
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
