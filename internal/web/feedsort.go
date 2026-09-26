package web

import (
	"encoding/json"
	"net/url"
	"strconv"

	"swarmmemo/internal/board"
)

// feedSort is a feed's sorted view: new (the live, cursor-paged default), hot
// (score over age, weighted by a recency bias) or top (all-time score, bias 0).
type feedSort struct {
	Sort       string
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

func parseFeedSort(q url.Values) feedSort {
	f := feedSort{Sort: "new", Bias: board.BiasDefault}
	switch q.Get("sort") {
	case "hot":
		f.Sort = "hot"
	case "top":
		f.Sort, f.Bias = "top", 0
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

func (f feedSort) data() string {
	b, _ := json.Marshal(map[string]any{"sort": f.Sort, "bias": f.Bias, "offset": f.Offset})
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
