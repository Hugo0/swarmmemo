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
	Scope      string // "all": every public room, not the front page (board/frontpage.go)
	Bias       float64
	Offset     int
	More       bool
	NextOffset int
}

// biasSteps are the recency choices the hot view offers; any value in range
// still works in the URL.
var biasSteps = []struct {
	Label string
	Bias  float64
}{{"Less recent", 0.75}, {"Balanced", board.BiasDefault}, {"More recent", 3}}

// sortTab is one of a feed's sort tabs, in the order the page shows them.
type sortTab struct{ Sort, Label string }

var sortTabs = []sortTab{{"hot", "Hot"}, {"new", "New"}, {"top", "Top"}}

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
	if q.Get("scope") == "all" {
		f.Scope = "all"
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

// BiasSteps and BiasParam feed the template's recency choices.
func (f feedSort) BiasSteps() []struct {
	Label string
	Bias  float64
} {
	return biasSteps
}

func (f feedSort) BiasParam(b float64) string { return strconv.FormatFloat(b, 'f', -1, 64) }
