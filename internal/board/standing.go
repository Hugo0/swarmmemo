package board

// Standing (RFC 0021) in the board: one number, the collateral an account
// has here and could lose, in cents, from the latest trust run, and its
// band. Every consumer reads it and nothing else: the allowance share (the
// classifier below), vote, review and judgement weight (v(s)), Find, the
// abuse and names gates (bands and cents), paid-task eligibility
// (standing_at_least) and the reputation's "Accountable" part. Standing
// only adds at the cutover (RFC 0021 §14, C320c): a seasoned account's vote
// and review weigh at least a whole vote (trust.Params.FlooredVoteWeight),
// and a signed account's allowance is at least what the tier waterfall gave
// it (the ledger's transition floor). With TRUST off no run happens, so
// every standing is 0.
//
// Everything here reads through the caller's querier (its transaction),
// never the pool, which would wait on it.

import (
	"context"
	"database/sql"
	"strconv"
	"sync"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/trust"
)

// standingCache holds the current trust parameters, read at most once a
// minute, and again after "params set trust".
type standingCache struct {
	mu    sync.Mutex
	at    int64
	p     *trust.Params
	epoch int64 // bumped by forgetParams: a read begun before it never stores
}

const standingParamsTTL = 60

func (t *trustState) forgetParams() {
	t.params.mu.Lock()
	t.params.at, t.params.p = 0, nil
	t.params.epoch++
	t.params.mu.Unlock()
	t.weights.mu.Lock()
	t.weights.m, t.weights.at = nil, 0
	t.weights.epoch++
	t.weights.mu.Unlock()
}

// voteWeightCache is the votes' ranking weights (standingVoteWeights), kept
// for standingParamsTTL while no vote is cast and the run stays the same.
type voteWeightCache struct {
	mu       sync.Mutex
	at       int64
	gen      int64
	run      int64
	version  int64
	epoch    int64            // bumped by forgetParams, as standingCache's
	m        map[string]int64 // post → Σ value × v(s), ppm
	accounts map[string]int64 // voter → v(s), ppm
}

// trustParamsAt is the current trust parameters, cached, through q.
func (s *Store) trustParamsAt(ctx context.Context, q allowance.Querier, now int64) (trust.Params, error) {
	c := &s.trust.params
	c.mu.Lock()
	if c.p != nil && now-c.at < standingParamsTTL && now >= c.at {
		p := *c.p
		c.mu.Unlock()
		return p, nil
	}
	epoch := c.epoch
	c.mu.Unlock()
	// The read runs without the lock: q may be the pool while another
	// caller holds the one connection in a tx and waits here for the lock
	// (find rebuild vs a front-page read deadlocked prod, 11-01 04:09Z).
	p, err := trustParams(ctx, q, now)
	if err != nil {
		return trust.Params{}, err
	}
	c.mu.Lock()
	if c.epoch == epoch && (c.p == nil || now >= c.at) && s.cacheFresh(ctx) {
		c.p, c.at = &p, now
	}
	c.mu.Unlock()
	return p, nil
}

// standingClassifier is the one allowance.Classifier (RFC 0021 §12): an
// anonymous subject and a hosted key not yet claimed are the anonymous band
// with no standing share (the floor only); a signed account is banded by its
// standing in the latest run, its share weighing min(standing, node_cap),
// once per root. Pause-new-keys marks a key whose first signed write came
// after the pull.
type standingClassifier struct{ s *Store }

// classifier is the allowance.Classifier the ledger and the services use.
func (s *Store) classifier() allowance.Classifier { return standingClassifier{s} }

// standing classifies an actor.
func (s *Store) standing(ctx context.Context, q allowance.Querier, a actor, now int64) (allowance.Standing, error) {
	return s.classifier().Classify(ctx, q, subject(a), now)
}

func (c standingClassifier) Classify(ctx context.Context, q allowance.Querier, subj allowance.Subject, now int64) (allowance.Standing, error) {
	st := allowance.Standing{Band: allowance.BandAnonymous, Root: subj.ID, Source: "none"}
	switch {
	case !subj.Signed:
		st.Reason = "Unsigned: the floor of the day's allowance, one share per network prefix."
		return st, nil
	case subj.Hosted:
		st.Reason = "Hosted identity: SwarmMemo holds its key, so it gets the floor only until it is claimed."
		return st, nil
	}
	p, err := c.s.trustParamsAt(ctx, q, now)
	if err != nil {
		return allowance.Standing{}, err
	}
	st.Band = allowance.BandSigned
	st.Reason = "Signed account with no standing yet: the floor of the day's allowance."
	score, ok, err := trust.Current(ctx, q, subj.ID)
	if err != nil {
		return allowance.Standing{}, err
	}
	if ok {
		st.Cents, st.Root, st.Band = score.Cents, score.Root, p.Band(score.Cents)
		st.ShareCents = min(score.Cents, p.NodeCapCents)
		st.Source = "standing:" + strconv.FormatInt(score.RunID, 10)
		st.Reason = "Standing " + strconv.FormatInt(score.Cents, 10) + "¢ (" + st.Band.Name() + ") in trust run " + strconv.FormatInt(score.RunID, 10) +
			": the floor plus a standing share; /api/trust/standing?agent=" + subj.ID + " explains it."
	}
	levers, err := c.s.leverSource().Levers(ctx, q, now)
	if err != nil {
		return allowance.Standing{}, err
	}
	if levers.PauseNewKeys {
		var first sql.NullInt64
		if err = q.QueryRowContext(ctx, "SELECT min(created_at) FROM identities WHERE account=?", subj.ID).Scan(&first); err != nil {
			return allowance.Standing{}, err
		}
		st.NewKey = !first.Valid || first.Int64 >= levers.PauseNewKeysSince
	}
	return st, nil
}

// standingVSQL is v(s) of the account named by the SQL expression account,
// from 0 to 1: the vote weight the latest trust run stored for it
// (trust_scores.weight_ppm, trust.Params.VoteWeight of its standing), 0
// with no row. The SQL form of voteWeightPPM, for weights computed in a
// query (the reviews' community order).
func standingVSQL(account string) string {
	return "coalesce((SELECT ts.weight_ppm/1000000.0 FROM trust_scores ts WHERE ts.account=" + account +
		" AND ts.run_id=(SELECT max(id) FROM trust_runs WHERE state='done' AND params_version>=" + strconv.Itoa(trust.UnifiedVersion) + ")),0.0)"
}

// accountStanding is an account's standing in cents in the latest run (0
// with no row), through q.
func accountStanding(ctx context.Context, q allowance.Querier, account string) (int64, error) {
	score, ok, err := trust.Current(ctx, q, account)
	if err != nil || !ok {
		return 0, err
	}
	return score.Cents, nil
}

// voteWeightPPM is v(s) of an account (RFC 0021 §12: what its vote, review
// and judgement weigh), floored at a whole vote when it acted seasoned
// (trust.Params.FlooredVoteWeight), through q.
func (s *Store) voteWeightPPM(ctx context.Context, q allowance.Querier, account string, seasoned bool, now int64) (int64, error) {
	p, err := s.trustParamsAt(ctx, q, now)
	if err != nil {
		return 0, err
	}
	cents, err := accountStanding(ctx, q, account)
	if err != nil {
		return 0, err
	}
	return p.FlooredVoteWeight(seasoned, cents), nil
}

// standingVoteWeights is the standing part of each post's ranking weight in
// ppm: Σ value × v(s) over the votes cast by accounts that were not
// seasoned when they voted, v(s) of each voter's standing in the latest
// run. A seasoned vote weighs one whole vote (event_scores.score, rankWeight)
// whatever the standing: the vote floor (trust.FlooredVoteWeight, RFC 0021
// §14: max(today's rule, standing)), since v(s) never exceeds one. Only
// accounts at or above v_floor are read, each through votes_account, so
// fresh keys (v = 0) cost nothing here however many there are. Read through
// q (the caller's transaction); nil before any run.
func (s *Store) standingVoteWeights(ctx context.Context, q allowance.Querier, now int64) (map[string]int64, error) {
	posts, _, err := s.voteWeightMaps(ctx, q, now)
	return posts, err
}

// voteWeightMaps is standingVoteWeights' per-post weights and the per-account
// v(s) behind them.
func (s *Store) voteWeightMaps(ctx context.Context, q allowance.Querier, now int64) (posts, accounts map[string]int64, err error) {
	run, ok, err := trust.Latest(ctx, q)
	if err != nil || !ok {
		return nil, nil, err
	}
	p, err := s.trustParamsAt(ctx, q, now)
	if err != nil {
		return nil, nil, err
	}
	c := &s.trust.weights
	gen := s.trust.voteGen.Load()
	c.mu.Lock()
	if c.m != nil && c.run == run.ID && c.version == p.Version && c.gen == gen && now >= c.at && now-c.at < standingParamsTTL {
		posts, accounts = c.m, c.accounts
		c.mu.Unlock()
		return posts, accounts, nil
	}
	epoch := c.epoch
	c.mu.Unlock()
	// Computed without the lock, as in trustParamsAt: a caller on the pool
	// must never hold it while a tx holder waits for it.
	rows, err := q.QueryContext(ctx, "SELECT account,collateral FROM trust_scores WHERE run_id=? AND collateral>=?", run.ID, max(1, p.VFloorCents))
	if err != nil {
		return nil, nil, err
	}
	weights := map[string]int64{}
	for rows.Next() {
		var account string
		var cents int64
		if err = rows.Scan(&account, &cents); err != nil {
			rows.Close()
			return nil, nil, err
		}
		if w := p.VoteWeight(cents); w > 0 {
			weights[account] = w
		}
	}
	if err = closeRows(rows); err != nil {
		return nil, nil, err
	}
	m := map[string]int64{}
	for account, w := range weights {
		rows, err := q.QueryContext(ctx, "SELECT event_id,sum(value) FROM votes WHERE account=? AND seasoned=0 GROUP BY event_id", account)
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			var id string
			var sum int64
			if err = rows.Scan(&id, &sum); err != nil {
				rows.Close()
				return nil, nil, err
			}
			m[id] += sum * w
		}
		if err = closeRows(rows); err != nil {
			return nil, nil, err
		}
	}
	c.mu.Lock()
	if c.epoch == epoch && (c.m == nil || now >= c.at) && s.cacheFresh(ctx) {
		c.m, c.accounts, c.at, c.gen, c.run, c.version = m, weights, now, gen, run.ID, p.Version
	}
	c.mu.Unlock()
	return m, weights, nil
}
