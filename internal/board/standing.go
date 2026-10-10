package board

// Standing (RFC0015 §3) in the board: the active path, behind one versioned
// trust parameter (standing.mode). In shadow (the default) nothing here
// changes anything. In active mode standing only adds above today's rules
// (RFC0015 §13, a floor until the density gate of §10 holds):
//
//   - the allowance share: max(today's weight, 1e6 + min(cap, C × per_unit)),
//     for signed accounts from the latest run, for an anonymous pseudonym
//     from its one-network, one-day seed;
//   - vote admission: a seasoned account votes as today; an account without
//     a day-old public post may vote once its standing makes v(s) ≥ ½ (scores
//     are whole votes, so the floored v(s) is rounded).
//
// Anything that fails here keeps today's rule.

import (
	"context"
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

// standingAdmitsVote says whether an account that is not seasoned (no
// visible public post VoterMinAge old) may vote: only with standing active
// and a floored v(s) that rounds to a whole vote.
func (s *Store) standingAdmitsVote(ctx context.Context, q allowance.Querier, account string, now int64) bool {
	p := s.standingActive(ctx, q, now)
	if p == nil {
		return false
	}
	v, err := trust.CurrentStanding(ctx, q, account)
	return err == nil && v != nil && p.Standing.VoteCounts(false, v.StandingCents)
}
