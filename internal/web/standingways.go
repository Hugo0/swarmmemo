package web

import (
	"strconv"

	"swarmmemo/internal/board"
)

// Raise your standing (RFC0015 §13, C144) on the agent page: standing.ways
// rendered read-only, one settings row per way. /me imports this list from
// the agent page (app.js) and adds the actions, so the list is rendered one
// way everywhere.

// standingWayView is one row. Every amount is in cents from standing.ways.
type standingWayView struct {
	Kind, Label, Icon      string
	Proves, Reveals        string
	Adds                   string // the summary note: what it can add
	State, Value, Root     string
	Counted, Assessed      string // "" when not known
	Action                 string
	Priced, HasState, Earn bool
}

var standingWayIcons = map[string]string{"domain": "globe", "wallet": "key", "github": "code", "pow": "gauge", "earned": "person"}

// centsText is an amount of US cents as a reader says it: "50¢", "$4.00".
func centsText(c int64) string {
	if c < 100 {
		return strconv.FormatInt(c, 10) + "¢"
	}
	d := strconv.FormatInt(c%100, 10)
	if c%100 < 10 {
		d = "0" + d
	}
	return "$" + strconv.FormatInt(c/100, 10) + "." + d
}

// standingWaysFrom reads standing.ways' answer into rows.
func standingWaysFrom(data map[string]any) []standingWayView {
	var ways []board.StandingWay
	if remarshal(data["ways"], &ways) != nil {
		return nil
	}
	out := make([]standingWayView, 0, len(ways))
	for _, w := range ways {
		v := standingWayView{Kind: w.Kind, Label: w.Label, Icon: standingWayIcons[w.Kind], Proves: w.Proves, Reveals: w.Reveals,
			State: w.State, Value: w.Value, Root: w.Root, Action: w.Action, Priced: w.Priced, HasState: w.State != "none", Earn: w.Kind == "earned"}
		switch {
		case w.AddsCents != nil && w.Priced:
			v.Adds = "up to " + centsText(w.AddsCents.Max)
		case w.Kind == "earned":
			v.Adds = "grows as agents with standing endorse you"
		default:
			v.Adds = "priced from the next parameter version"
		}
		if w.CountedCents != nil {
			v.Counted = centsText(*w.CountedCents)
		}
		if w.AssessedCents != nil {
			v.Assessed = centsText(*w.AssessedCents)
		}
		out = append(out, v)
	}
	return out
}
