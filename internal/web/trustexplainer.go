package web

import (
	"context"
	"math"
	"sort"
	"strconv"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/trust"
)

// /trust is the short guide to the allowance and trust model
// (docs/TRUST_MODEL.md is the paper): the problem, the waterfall with today's
// posting tiers, what standing is with one small interactive (assets/trust.js),
// the public trust network (assets/trust-network.js, on the /swarmchasing
// renderer graph-gl.js), and how to check every number. /trust/network is the
// network alone. Every live number is a cell (data-key, data-value) read
// through the function its JSON API serves (invariant 9): the waterfall is
// ReadAllowanceStats (/api/stats/allowance), the network's list is the
// store's TrustGraph (/api/trust/graph) and the latest run is TrustRun
// (/api/trust/runs/ID). The page exists only while the ledger or trust is on;
// otherwise /trust stays today's 404, byte for byte. /trust/network needs
// trust on.

// TrustExplainerOn reports whether /trust is served: while the allowance
// ledger or the trust module is on, the flags that gate the surfaces it
// explains. Links to it follow the same rule.
func TrustExplainerOn(f board.Features) bool {
	return f.Ledger != board.LedgerOff || f.Trust != board.TrustOff
}

// trustRunSource is the part of the store behind /api/trust/runs and
// /api/trust/runs/ID.
type trustRunSource interface {
	TrustRuns(ctx context.Context, before int64, limit int) ([]map[string]any, int64, error)
	TrustRun(ctx context.Context, id int64) (map[string]any, error)
}

// trustGraphSource is the part of the store behind /api/trust/graph.
type trustGraphSource interface {
	TrustGraph(ctx context.Context, core int) (*board.TrustGraph, error)
}

// trustRunsScanned is how many recent runs the page looks through for the
// latest finished one.
const trustRunsScanned = 10

// trustNetworkListed is how many identities the network's text list shows;
// the JSON has the rest.
const trustNetworkListed = 8

// TrustRunView is the part of /api/trust/runs/ID the page shows.
type TrustRunView struct {
	ID           int64  `json:"id"`
	AsOf         int64  `json:"as_of"`
	State        string `json:"state"`
	Nodes        int64  `json:"nodes"`
	Edges        int64  `json:"edges"`
	OutputSHA256 string `json:"output_sha256"`
}

// ReadLatestTrustRun is the latest finished trust run as /api/trust/runs/ID
// serves it (the store's TrustRun), or nil when there is none yet.
func ReadLatestTrustRun(ctx context.Context, service board.Service) (*TrustRunView, error) {
	src, ok := service.(trustRunSource)
	if !ok {
		return nil, errStatsUnavailable
	}
	runs, _, err := src.TrustRuns(ctx, 0, trustRunsScanned)
	if err != nil {
		return nil, err
	}
	for _, listed := range runs {
		var head struct {
			ID    int64  `json:"id"`
			State string `json:"state"`
		}
		if remarshal(listed, &head) != nil || head.State != "done" {
			continue
		}
		raw, err := src.TrustRun(ctx, head.ID)
		if err != nil {
			return nil, err
		}
		run := &TrustRunView{}
		if err := remarshal(raw, run); err != nil {
			return nil, err
		}
		return run, nil
	}
	return nil, nil
}

// trustExplainerView is what /trust and /trust/network render from live state.
type trustExplainerView struct {
	LedgerOn, TrustOn, ExportOn bool
	NetworkOnly                 bool // /trust/network: the network alone
	Unavailable                 bool
	Waterfall                   *explainerWaterfall
	Run                         *explainerRun
	RunUnavailable              bool
	Network                     *explainerNetwork
	NetworkUnavailable          bool
	Stake                       stakeDefaults
	Generated                   string
	Sentence                    string
}

// explainerWaterfall is today's posting waterfall: one bar per tier.
type explainerWaterfall struct {
	Day, Mode string
	Budget    cell
	Spent     cell
	Tiers     []explainerTier
}

type explainerTier struct {
	Tier       int
	Name       string
	Water      cell
	Claimed    cell
	Claimants  cell
	WaterPct   float64 // of the day's budget: the bar's length
	ClaimedPct float64 // of the tier's water: the bar's dark part
}

type explainerRun struct {
	ID, AsOf            int64
	Hash                string
	RunID, Nodes, Edges cell
}

// explainerNetwork is the trust network's counts and its text list: the
// identities with the most standing, in /api/trust/graph's order.
type explainerNetwork struct {
	Run          int64
	Nodes, Edges int
	WithStanding int
	Top          []networkRow
}

type networkRow struct {
	ID, Handle, Band string
	Roots            []string
	Cents            cell
	Score            string
}

// stakeDefaults are the published parameters (trust parameter version 6)
// the standing interactive uses, as data attributes, so the script and the
// text state the same numbers.
type stakeDefaults struct {
	StakePPM, BudgetPPM, VouchWeight, VouchMax, V0PPM, CRef, VFloor, Theta1, Theta2 int64
}

func defaultStake() stakeDefaults {
	st := trust.DefaultStanding()
	return stakeDefaults{StakePPM: st.StakePPM, BudgetPPM: st.StakeBudgetPPM, VouchWeight: st.EdgeWeights["vouch"], VouchMax: st.VouchWeightMax,
		V0PPM: st.V0PPM, CRef: st.CRefCents, VFloor: st.VFloorCents, Theta1: st.Theta1Cents, Theta2: st.Theta2Cents}
}

// Moves is what one act of this weight moves from an author with this
// standing, in cents, as the text states it: "12.5".
func (s stakeDefaults) Moves(cents, weight int64) string {
	return strconv.FormatFloat(float64(cents)*float64(s.StakePPM*weight)/1e6, 'f', -1, 64)
}

// Percent is the share of its author's standing one act of this weight
// stakes: "2.5%".
func (s stakeDefaults) Percent(weight int64) string {
	return strconv.FormatFloat(float64(s.StakePPM*weight)/1e4, 'f', -1, 64) + "%"
}

// VoteWeight is v(s) for a voter with this standing, two decimals.
func (s stakeDefaults) VoteWeight(cents int64) string {
	if cents < s.VFloor || cents <= 0 || s.CRef <= 0 {
		return "0"
	}
	v0 := float64(s.V0PPM) / 1e6
	return strconv.FormatFloat(v0+(1-v0)*math.Sqrt(math.Min(1, float64(cents)/float64(s.CRef))), 'f', 2, 64)
}

func buildTrustExplainer(ctx context.Context, service board.Service, now time.Time) *trustExplainerView {
	f := ServiceFeatures(service)
	v := &trustExplainerView{LedgerOn: f.Ledger != board.LedgerOff, TrustOn: f.Trust != board.TrustOff, ExportOn: f.ExportEndorsements,
		Generated: now.UTC().Format("2006-01-02 15:04 UTC"), Sentence: WaterfallSentence, Stake: defaultStake()}
	if v.LedgerOn {
		stats, err := ReadAllowanceStats(ctx, service, f, AllowanceStatsDaysDefault, now)
		if err != nil || stats == nil {
			v.Unavailable = true
		} else {
			v.Waterfall = explainerWaterfallFrom(stats.Allowance)
		}
	}
	if v.TrustOn {
		run, err := ReadLatestTrustRun(ctx, service)
		switch {
		case err != nil:
			v.RunUnavailable = true
		case run != nil:
			v.Run = explainerRunFrom(run)
		}
		v.Network, v.NetworkUnavailable = buildTrustNetwork(ctx, service)
	}
	return v
}

// buildTrustNetwork reads the default graph (the one the page's script
// fetches) for the counts and the text list.
func buildTrustNetwork(ctx context.Context, service board.Service) (*explainerNetwork, bool) {
	src, ok := service.(trustGraphSource)
	if !ok {
		return nil, true
	}
	g, err := src.TrustGraph(ctx, board.TrustGraphCoreDefault)
	if err != nil || g == nil {
		return nil, true
	}
	n := &explainerNetwork{Run: g.Run, Nodes: len(g.Nodes), Edges: len(g.Edges)}
	for i, node := range g.Nodes {
		if !node.Core {
			continue
		}
		n.WithStanding++
		if len(n.Top) < trustNetworkListed {
			n.Top = append(n.Top, networkRow{ID: node.ID, Handle: node.Handle, Band: node.BandName, Roots: node.Roots,
				Cents: cell{Key: "nodes." + strconv.Itoa(i) + ".standing_cents", Value: node.StandingCents, Text: count(node.StandingCents)},
				Score: strconv.FormatFloat(node.Standing, 'f', 1, 64)})
		}
	}
	return n, false
}

func explainerWaterfallFrom(w *WaterfallStats) *explainerWaterfall {
	if w == nil {
		return nil
	}
	for i, r := range w.Resources {
		if r.Resource != "post_bytes" {
			continue
		}
		p := "allowance.resources." + strconv.Itoa(i) + "."
		out := &explainerWaterfall{Day: string(r.Day), Mode: w.Ledger,
			Budget: cell{Key: p + "budget", Value: r.Budget, Text: units(r.Resource, r.Budget)},
			Spent:  cell{Key: p + "spent", Value: r.Spent, Text: units(r.Resource, r.Spent)}}
		for j, t := range r.Tiers {
			if t.Tier < 1 || t.Tier > 4 {
				continue
			}
			q := p + "tiers." + strconv.Itoa(j) + "."
			row := explainerTier{Tier: t.Tier, Name: capitalize(t.Name),
				Water:     cell{Key: q + "water", Value: t.Water, Text: units(r.Resource, t.Water)},
				Claimed:   cell{Key: q + "claimed", Value: t.Claimed, Text: units(r.Resource, t.Claimed)},
				Claimants: cell{Key: q + "claimants", Value: t.Claimants, Text: count(t.Claimants)}}
			if r.Budget > 0 {
				row.WaterPct = min(100, float64(max(0, t.Water))*100/float64(r.Budget))
			}
			if t.Water > 0 {
				row.ClaimedPct = min(100, float64(max(0, t.Claimed))*100/float64(t.Water))
			}
			out.Tiers = append(out.Tiers, row)
		}
		// Trusted first, whatever order the API lists the tiers in.
		sort.SliceStable(out.Tiers, func(a, b int) bool { return out.Tiers[a].Tier < out.Tiers[b].Tier })
		return out
	}
	return nil
}

func explainerRunFrom(r *TrustRunView) *explainerRun {
	return &explainerRun{ID: r.ID, AsOf: r.AsOf, Hash: r.OutputSHA256,
		RunID: cell{Key: "id", Value: r.ID, Text: count(r.ID)},
		Nodes: cell{Key: "nodes", Value: r.Nodes, Text: count(r.Nodes)},
		Edges: cell{Key: "edges", Value: r.Edges, Text: count(r.Edges)}}
}
