package web

import (
	"encoding/json"
	"net/url"
	"strconv"

	"swarmmemo/internal/board"
)

// feedSort is a feed's sorted view: hot (score over age, weighted by a recency
// bias; the home and room default), top (all-time score, bias 0) or new (the
// live, cursor-paged stream). Hot and top nest each post's replies under it;
// new is flat and quotes each reply's parent (threadtree.go).
type feedSort struct {
	Sort       string
	Default    string // the view's sort when the URL names none
	Scope      string // "all" on the home feed: every public room (board/frontpage.go), set by the handler
	Bias       float64
	Offset     int
	More       bool
	NextOffset int
}

// sortTab is one of a feed's sort tabs, in the order the page shows them,
// with what it means for its tooltip. Customizing the ranking (weights,
// recency, rooms) is a setting in Me, not another tab.
type sortTab struct{ Sort, Label, Tip string }

var sortTabs = []sortTab{
	{"new", "New", "Newest first, as posts arrive."},
	{"hot", "Hot", "Score over age: well-voted recent posts first. The default."},
	{"top", "Top", "Highest score of all time."},
}

// newFeedParams are the new feed's own parameters: a search, a cursor or a
// filter reads the chronological stream whatever the view's default.
var newFeedParams = []string{"q", "cursor", "older", "target", "kind", "to"}

// parseFeedSort reads a feed's sort; def is the view's default, "hot" on the
// home and room pages and "new" elsewhere.
func parseFeedSort(q url.Values, def string) feedSort {
	f := feedSort{Sort: def, Default: def, Bias: board.BiasDefault}
	for _, key := range newFeedParams {
		if q.Get(key) != "" {
			f.Sort = "new"
		}
	}
	switch q.Get("sort") {
	case "new":
		f.Sort = "new"
	case "hot":
		f.Sort = "hot"
	case "top":
		f.Sort = "top"
	}
	if f.Sort == "top" {
		f.Bias = 0
	}
	if f.Sort == "hot" {
		if b, err := strconv.ParseFloat(q.Get("bias"), 64); err == nil && b >= 0 && b <= board.BiasMaximum {
			f.Bias = b
		}
		if f.Bias == 0 {
			f.Sort = "top"
		}
	}
	if n, err := strconv.Atoi(q.Get("offset")); err == nil && n > 0 && n <= board.HotCandidates {
		f.Offset = n
	}
	return f
}

func (f feedSort) Ranked() bool { return f.Sort == "hot" || f.Sort == "top" }

// IsDefault reports whether s is this view's default sort, which keeps the
// view's plain address; another sort is linked with ?sort=.
func (f feedSort) IsDefault(s string) bool { return s == f.Default }

func (f feedSort) data() string {
	b, _ := json.Marshal(board.ListOptions{Sort: f.Sort, Bias: &f.Bias, Offset: f.Offset, Scope: f.Scope})
	return string(b)
}

// BiasQuery is a hot view's non-default recency (?bias=, still accepted in
// the URL), kept on its pagination links.
func (f feedSort) BiasQuery() string {
	if f.Sort != "hot" || f.Bias == board.BiasDefault {
		return ""
	}
	return strconv.FormatFloat(f.Bias, 'f', -1, 64)
}
