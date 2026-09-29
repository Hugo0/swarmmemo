package web

import (
	"context"
	"strconv"
	"time"

	"swarmmemo/internal/board"
)

// /trust is the illustrated guide to the allowance and trust model: five
// scenes, each with a drawing that runs in the browser (assets/trust.js).
// The drawings are illustrations; the live numbers beside them are not. Every
// live number is a cell (data-key, data-value) read through the function its
// JSON API serves (invariant 9): the waterfall and the trust distribution are
// ReadAllowanceStats (/api/stats/allowance), and the capture bound is the
// store's TrustRun (/api/trust/runs/ID). The page exists only while the
// ledger or trust is on; otherwise /trust stays today's 404, byte for byte.

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

// trustRunsScanned is how many recent runs the page looks through for the
// latest finished one, which is the one with a capture bound.
const trustRunsScanned = 10

// TrustRunView is the part of /api/trust/runs/ID the page shows.
type TrustRunView struct {
	ID           int64  `json:"id"`
	AsOf         int64  `json:"as_of"`
	State        string `json:"state"`
	Nodes        int64  `json:"nodes"`
	Edges        int64  `json:"edges"`
	OutputSHA256 string `json:"output_sha256"`
	CaptureBound *struct {
		UnitPerShare    int64  `json:"unit_per_share"`
		EdgeCapUnits    int64  `json:"edge_cap_units"`
		LambdaPPM       int64  `json:"lambda_ppm"`
		MaxTransitUnits int64  `json:"max_transit_units"`
		PoolUnits       int64  `json:"pool_units"`
		Statement       string `json:"statement"`
	} `json:"capture_bound"`
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

// trustExplainerView is what /trust renders from live state.
type trustExplainerView struct {
	LedgerOn, TrustOn, ExportOn bool
	Unavailable                 bool
	Waterfall                   *explainerWaterfall
	Distribution                *trustView
	Accounts                    cell
	Run                         *explainerRun
	RunUnavailable              bool
	Generated                   string
	Sentence                    string
}

// explainerWaterfall is today's posting waterfall, the initial state of the
// scene 2 drawing.
type explainerWaterfall struct {
	Day, Mode string
	Budget    cell
	Spent     cell
	Tiers     []explainerTier
}

type explainerTier struct {
	Tier  int
	Name  string
	Cells []cell // water, want, claimed, lent, borrowed, claimants, in that order
}

type explainerRun struct {
	ID, AsOf            int64
	Hash, Statement     string
	RunID, Nodes, Edges cell
	EdgeCap, MaxTransit cell
	Pool, Lambda, Unit  cell
}

func buildTrustExplainer(ctx context.Context, service board.Service, now time.Time) *trustExplainerView {
	f := ServiceFeatures(service)
	v := &trustExplainerView{LedgerOn: f.Ledger != board.LedgerOff, TrustOn: f.Trust != board.TrustOff, ExportOn: f.ExportEndorsements,
		Generated: now.UTC().Format("2006-01-02 15:04 UTC"), Sentence: WaterfallSentence}
	stats, err := ReadAllowanceStats(ctx, service, f, AllowanceStatsDaysDefault, now)
	if err != nil || stats == nil {
		v.Unavailable = true
	} else {
		v.Waterfall = explainerWaterfallFrom(stats.Allowance)
		if stats.Trust != nil {
			if s := allowanceSectionFrom(&AllowanceStats{Trust: stats.Trust}); s.Trust != nil {
				// The page lists bins up to the last one with accounts (at least
				// four), in the API's order; /stats and the API keep all twelve.
				last := 3
				for i, b := range stats.Trust.CollateralLog10 {
					if b.Accounts > 0 {
						last = max(last, i)
					}
				}
				s.Trust.Bins = s.Trust.Bins[:min(len(s.Trust.Bins), last+1)]
				v.Distribution = s.Trust
				v.Accounts = cell{Key: "trust.accounts", Value: stats.Trust.Accounts, Text: count(stats.Trust.Accounts)}
			}
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
	}
	return v
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
			out.Tiers = append(out.Tiers, explainerTier{Tier: t.Tier, Name: capitalize(t.Name), Cells: []cell{
				{Label: "water", Key: q + "water", Value: t.Water, Text: units(r.Resource, t.Water)},
				{Label: "want", Key: q + "want", Value: t.Want, Text: units(r.Resource, t.Want)},
				{Label: "claimed", Key: q + "claimed", Value: t.Claimed, Text: units(r.Resource, t.Claimed)},
				{Label: "lent", Key: q + "lent", Value: t.Lent, Text: units(r.Resource, t.Lent)},
				{Label: "borrowed", Key: q + "borrowed", Value: t.Borrowed, Text: units(r.Resource, t.Borrowed)},
				{Label: "claimants", Key: q + "claimants", Value: t.Claimants, Text: count(t.Claimants)},
			}})
		}
		return out
	}
	return nil
}

func explainerRunFrom(r *TrustRunView) *explainerRun {
	v := &explainerRun{ID: r.ID, AsOf: r.AsOf, Hash: r.OutputSHA256,
		RunID: cell{Key: "id", Value: r.ID, Text: count(r.ID)},
		Nodes: cell{Key: "nodes", Value: r.Nodes, Text: count(r.Nodes)},
		Edges: cell{Key: "edges", Value: r.Edges, Text: count(r.Edges)}}
	if b := r.CaptureBound; b != nil {
		v.Statement = b.Statement
		v.EdgeCap = cell{Key: "capture_bound.edge_cap_units", Value: b.EdgeCapUnits, Text: count(b.EdgeCapUnits)}
		v.MaxTransit = cell{Key: "capture_bound.max_transit_units", Value: b.MaxTransitUnits, Text: count(b.MaxTransitUnits)}
		v.Pool = cell{Key: "capture_bound.pool_units", Value: b.PoolUnits, Text: count(b.PoolUnits)}
		v.Lambda = cell{Key: "capture_bound.lambda_ppm", Value: b.LambdaPPM, Text: strconv.FormatFloat(float64(b.LambdaPPM)/1e6, 'f', -1, 64)}
		v.Unit = cell{Key: "capture_bound.unit_per_share", Value: b.UnitPerShare, Text: count(b.UnitPerShare)}
	}
	return v
}
