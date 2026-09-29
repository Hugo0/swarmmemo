package moderation

import (
	"context"
)

// rulesScanBytes bounds how much of a text the rules read. RE2 is linear in
// it, so the bound is on time, not on correctness of what fits.
const rulesScanBytes = 1 << 20

type classResult struct {
	scores map[string]float64
	model  string
	cost   int64 // Jev: what its calls cost, in microUSD
}

// classifyRules scores each rule's category at 1 when its regex matches.
func classifyRules(sp *SurfacePolicy, c Content) classResult {
	r := classResult{scores: map[string]float64{}, model: "rules"}
	text := c.Text
	if len(text) > rulesScanBytes {
		text = text[:rulesScanBytes]
	}
	for _, rule := range sp.Rules {
		if rule.re != nil && rule.re.MatchString(text) {
			r.scores[rule.Category] = 1
		}
	}
	return r
}

// classifySize scores "size" at 1 when the content exceeds max_bytes.
func classifySize(sp *SurfacePolicy, c Content) classResult {
	r := classResult{scores: map[string]float64{}}
	if sp.MaxBytes > 0 && int64(len(c.Text)) > sp.MaxBytes {
		r.scores["size"] = 1
	}
	return r
}

// classifyRate scores "rate" at 1 when the agent has already been screened
// Max times on this surface within the window. Anonymous callers without a
// pseudonym are not counted (the board's own allowance bounds them).
func (e *Engine) classifyRate(ctx context.Context, sp *SurfacePolicy, s Surface, subj Subject, now int64) (classResult, error) {
	r := classResult{scores: map[string]float64{}}
	if sp.Rate == nil || subj.Agent == "" {
		return r, nil
	}
	var n int
	err := e.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM moderation_decisions WHERE surface=? AND agent=? AND created_at>?", string(s), bound(subj.Agent, 128), now-sp.Rate.WindowSeconds).Scan(&n)
	if err != nil {
		return r, err
	}
	if n >= sp.Rate.Max {
		r.scores["rate"] = 1
	}
	return r, nil
}
