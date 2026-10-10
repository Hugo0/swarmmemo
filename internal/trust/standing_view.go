package trust

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"strconv"

	"swarmmemo/internal/allowance"
)

// StandingView is an account's standing as reads show it (the record, the
// trust.get answer, MCP and the agent page): the score log₁₀(1 + C), the
// cents C, the dollar figure, the breakdown by root, and the run it comes
// from. Never a boolean and never a rank.
type StandingView struct {
	Standing       float64        `json:"standing"`
	StandingCents  int64          `json:"standing_cents"`
	FakeCost       string         `json:"fake_cost"`
	Band           int64          `json:"band"`
	VoteWeightPPM  int64          `json:"vote_weight_ppm"`
	ShareWeightPPM int64          `json:"share_weight_ppm"`
	SeedCents      int64          `json:"seed_cents"`
	ReceivedCents  int64          `json:"received_cents"`
	OpposedCents   int64          `json:"opposed_cents"`
	PenaltyPPM     int64          `json:"penalty_ppm"`
	Breakdown      []StandingRoot `json:"breakdown"`
	Mode           string         `json:"mode"`
	Run            int64          `json:"run"`
	AsOf           int64          `json:"as_of"`
	ParamsVersion  int64          `json:"params_version"`
}

// NewStandingView makes the view of a run's standing part (nil: the account
// has no row in that run, so standing 0).
func NewStandingView(part *StandingPart, mode string, run, asOf, version int64) *StandingView {
	if part == nil {
		part = &StandingPart{Band: 3, ShareWeightPPM: 1e6}
	}
	breakdown := part.Breakdown
	if breakdown == nil {
		breakdown = []StandingRoot{}
	}
	return &StandingView{Standing: StandingScore(part.Cents), StandingCents: part.Cents, FakeCost: FakeCostText(part.Cents), Band: part.Band,
		VoteWeightPPM: part.VoteWeightPPM, ShareWeightPPM: part.ShareWeightPPM, SeedCents: part.SeedCents, ReceivedCents: part.ReceivedCents,
		OpposedCents: part.OpposedCents, PenaltyPPM: part.PenaltyPPM, Breakdown: breakdown, Mode: mode, Run: run, AsOf: asOf, ParamsVersion: version}
}

// Text is the quiet line the agent page shows: "Standing 2.4 (about $2.50
// to fake)".
func (v *StandingView) Text() string {
	return "Standing " + strconv.FormatFloat(math.Round(v.Standing*10)/10, 'f', 1, 64) + " (" + FakeCostText(v.StandingCents) + ")"
}

// CurrentStanding reads an account's standing in the latest done run; nil
// when there is no run or that run has no standing (parameter version 1).
func CurrentStanding(ctx context.Context, q allowance.Querier, account string) (*StandingView, error) {
	var id, asOf, version int64
	var mode string
	err := q.QueryRowContext(ctx, `SELECT id,as_of,params_version,coalesce(json_extract(inputs,'$.params.standing.mode'),'') FROM trust_runs
 WHERE state='done' ORDER BY id DESC LIMIT 1`).Scan(&id, &asOf, &version, &mode)
	if errors.Is(err, sql.ErrNoRows) || err == nil && mode == "" {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var parts string
	err = q.QueryRowContext(ctx, "SELECT parts FROM trust_scores WHERE run_id=? AND account=?", id, account).Scan(&parts)
	if errors.Is(err, sql.ErrNoRows) {
		return NewStandingView(nil, mode, id, asOf, version), nil
	}
	if err != nil {
		return nil, err
	}
	var p Parts
	if err = json.Unmarshal([]byte(parts), &p); err != nil {
		return nil, err
	}
	return NewStandingView(p.Standing, mode, id, asOf, version), nil
}
