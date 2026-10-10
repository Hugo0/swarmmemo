package board

// Standing (RFC0015 §3) in the board: the active path, behind one versioned
// trust parameter (standing.mode). In shadow (the default) nothing here
// changes anything. In active mode standing only adds above today's rules
// (RFC0015 §13, a floor until the density gate of §10 holds):
//
//   - the allowance share: max(today's weight, 1e6 + min(cap, C × per_unit)),
//     for signed accounts from the latest run, for an anonymous pseudonym
//     from its one-network, one-day seed;
//   - the votes' ranking weight (votes.go): a vote cast by a seasoned account
//     weighs one whole vote as today; any other vote weighs v(s), which is 0
//     in shadow and below the standing floor (standingVoteWeights). Every
//     signed key may vote either way; the shown vote count never reads
//     standing.
//
// Anything that fails here keeps today's rule.

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"sync"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/trust"
)

// standingCache holds the current trust parameters for the active path,
// read at most once a minute, and again after "params set trust".
type standingCache struct {
	mu sync.Mutex
	at int64
	p  *trust.Params
}

const standingParamsTTL = 60

func (t *trustState) forgetParams() {
	t.params.mu.Lock()
	t.params.at, t.params.p = 0, nil
	t.params.mu.Unlock()
	t.weights.mu.Lock()
	t.weights.m, t.weights.at = nil, 0
	t.weights.mu.Unlock()
}

// voteWeightCache is the standing part of the votes' ranking weight
// (standingVoteWeights), kept for standingParamsTTL while no vote is cast
// and the trust run and parameters stay the same.
type voteWeightCache struct {
	mu      sync.Mutex
	at      int64
	gen     int64
	run     int64
	version int64
	m       map[string]int64
}

// standingParams is the current trust parameters, through q (the caller's
// transaction: never the pool, which would wait on it).
func (s *Store) standingParams(ctx context.Context, q allowance.Querier, now int64) (*trust.Params, error) {
	c := &s.trust.params
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.p != nil && now-c.at < standingParamsTTL && now >= c.at {
		return c.p, nil
	}
	p, err := trustParams(ctx, q, now)
	if err != nil {
		return nil, err
	}
	c.p, c.at = &p, now
	return c.p, nil
}

// standingActive is the standing section when standing is active, else nil.
func (s *Store) standingActive(ctx context.Context, q allowance.Querier, now int64) *trust.Params {
	if s.config.Features.Trust == TrustOff {
		return nil
	}
	p, err := s.standingParams(ctx, q, now)
	if err != nil || p == nil || !p.Standing.Active() {
		return nil
	}
	return p
}

// standingClassifier raises the share the inner classifier gives, never
// lowers it, when standing is active.
type standingClassifier struct {
	inner allowance.Classifier
	s     *Store
}

func (c standingClassifier) Classify(ctx context.Context, q allowance.Querier, subject allowance.Subject, now int64) (allowance.Standing, error) {
	st, err := c.inner.Classify(ctx, q, subject, now)
	if err != nil {
		return st, err
	}
	p := c.s.standingActive(ctx, q, now)
	if p == nil {
		return st, nil
	}
	cents := p.Standing.AnonSeedCents
	if subject.Signed {
		v, err := trust.CurrentStanding(ctx, q, subject.ID)
		if err != nil || v == nil {
			return st, nil
		}
		cents = v.StandingCents
	}
	if w := trust.FlooredShareWeightPPM(st.WeightPPM, cents, *p); w > st.WeightPPM {
		st.WeightPPM = w
		st.Reason += " Standing (" + strconv.FormatInt(cents, 10) + " cents) raises the share to " + strconv.FormatInt(w, 10) + " ppm."
	}
	return st, nil
}

// standingVoteWeights is what the votes cast by accounts that were not
// seasoned add to each post's ranking weight, in ppm: Σ value × v(s) over
// those votes, v(s) under the current parameters and the latest trust run's
// standing (RFC0015 §13). nil while standing is in shadow: those votes then
// weigh nothing. Only accounts whose v(s) is above zero are read, each
// through votes_account, so fresh keys (no standing, v = 0) cost nothing
// here however many there are. Read through q (the caller's transaction).
func (s *Store) standingVoteWeights(ctx context.Context, q allowance.Querier, now int64) (map[string]int64, error) {
	p := s.standingActive(ctx, q, now)
	if p == nil {
		return nil, nil
	}
	var run int64
	var mode string
	err := q.QueryRowContext(ctx, `SELECT id,coalesce(json_extract(inputs,'$.params.standing.mode'),'') FROM trust_runs WHERE state='done' ORDER BY id DESC LIMIT 1`).Scan(&run, &mode)
	if errors.Is(err, sql.ErrNoRows) || err == nil && mode == "" {
		return nil, nil // no run has computed standing
	}
	if err != nil {
		return nil, err
	}
	c := &s.trust.weights
	gen := s.trust.voteGen.Load()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m != nil && c.run == run && c.version == p.Version && c.gen == gen && now >= c.at && now-c.at < standingParamsTTL {
		return c.m, nil
	}
	rows, err := q.QueryContext(ctx, `SELECT account,CAST(json_extract(parts,'$.standing.cents') AS INTEGER) FROM trust_scores
 WHERE run_id=? AND json_extract(parts,'$.standing.cents')>=?`, run, max(1, p.Standing.VFloorCents))
	if err != nil {
		return nil, err
	}
	weights := map[string]int64{}
	for rows.Next() {
		var account string
		var cents int64
		if err = rows.Scan(&account, &cents); err != nil {
			rows.Close()
			return nil, err
		}
		if w := p.Standing.FlooredVoteWeightPPM(false, cents); w > 0 {
			weights[account] = w
		}
	}
	if err = closeRows(rows); err != nil {
		return nil, err
	}
	m := map[string]int64{}
	for account, w := range weights {
		rows, err := q.QueryContext(ctx, "SELECT event_id,sum(value) FROM votes WHERE account=? AND seasoned=0 GROUP BY event_id", account)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			var sum int64
			if err = rows.Scan(&id, &sum); err != nil {
				rows.Close()
				return nil, err
			}
			m[id] += sum * w
		}
		if err = closeRows(rows); err != nil {
			return nil, err
		}
	}
	c.m, c.at, c.gen, c.run, c.version = m, now, gen, run, p.Version
	return m, nil
}
