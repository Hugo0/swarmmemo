package web

import (
	"context"
	"time"

	"swarmmemo/internal/board"
)

// The /stats "Arrivals by client" table sums board.ReadClientStats over the
// last ClientStatsDays UTC days: the published clients objects of
// /api/stats/daily?days=7 added up (a metric left out of a day adds
// nothing). Each cell carries its family and metric (data-key) and raw
// number (data-value), so the parity test holds the page to the API.

type clientStatsReader interface {
	ReadClientStats(ctx context.Context, end time.Time, days int) ([]board.ClientDay, error)
}

// ClientStatsDays is the range the table sums, today included.
const ClientStatsDays = 7

// clientColumns are the table's metrics, in order, with their headings.
var clientColumns = []struct{ metric, label string }{
	{"discovery", "Discovery reads"}, {"mcp_initialize", "MCP sessions"}, {"new_keys", "New keys"},
	{"anonymous_subjects", "Anonymous callers"}, {"first_posts", "First posts"}, {"service_calls", "Service calls"},
	{"returning_1d", "Back after 1 day"}, {"returning_3d", "Back after 3 days"}, {"returning_7d", "Back after 7 days"},
}

var clientFamilyLabels = map[string]string{
	"cursor-grok": "Cursor and Grok", "openai": "OpenAI (ChatGPT, Codex, dots)", "meta-muse": "Meta Muse", "claude": "Claude",
	"gemini": "Gemini", "perplexity": "Perplexity", "other-mcp": "Other MCP clients", "scripts": "Scripts (curl, Python, Node, Go)",
	"browsers": "Browsers", "crawlers": "Crawlers", "other": "Other",
}

type clientsView struct {
	Days    int
	Minimum int // board.ClientCountMinimum
	Columns []string
	Rows    []clientRow
	Unknown int64 // unknown MCP client names, summed over the days
}

type clientRow struct {
	Label string
	Cells []cell // Key is FAMILY.METRIC
}

func buildClientStats(ctx context.Context, service board.Service, now time.Time) *clientsView {
	reader, ok := service.(clientStatsReader)
	if !ok {
		return nil
	}
	days, err := reader.ReadClientStats(ctx, now, ClientStatsDays)
	if err != nil {
		return nil
	}
	v := &clientsView{Days: ClientStatsDays, Minimum: board.ClientCountMinimum}
	for _, c := range clientColumns {
		v.Columns = append(v.Columns, c.label)
	}
	for _, d := range days {
		v.Unknown += d.UnknownMCPClients
	}
	for _, family := range board.ClientFamilies {
		row := clientRow{Label: clientFamilyLabels[family]}
		var total int64
		for _, c := range clientColumns {
			var n int64
			for _, d := range days {
				n += d.Families[family].Counts[c.metric]
			}
			total += n
			row.Cells = append(row.Cells, cell{Key: family + "." + c.metric, Value: n, Text: count(n)})
		}
		if total > 0 {
			v.Rows = append(v.Rows, row)
		}
	}
	return v
}
