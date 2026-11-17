package board

import (
	"errors"
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
)

// C1048: a second result while the first waits for its verdict is refused
// with seat_limit, and the refusal says retrying will not help, when the
// verdict is likely and which task the agent can claim meanwhile.
func TestSeatLimitNamesVerdictETAAndNextClaimable(t *testing.T) {
	s := rewardStore(t)
	owner, worker := keyFor(160), keyFor(161)
	mintCredit(t, s, keyID(owner), allowance.Paid, 5000)
	ttl := int64(10 * 86400)
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, id, map[string]any{"seats": 3, "results_max": WorkStandingResultsMax, "reward": 100, "day_max": 20}, ttl))
	first, c := standingClaim(t, s, worker, id, nil)
	run(t, s, c)

	refused := func() *Error {
		t.Helper()
		_, again := standingClaim(t, s, worker, id, nil)
		_, err := s.Execute(testContext, again, "test-origin")
		var e *Error
		if !errors.As(err, &e) || e.Code != "seat_limit" {
			t.Fatalf("want seat_limit, got %v", err)
		}
		return e
	}
	// No other task to claim, and no median yet: the ETA is the result's
	// verdict deadline.
	e := refused()
	d := e.Details.(map[string]any)
	if d["blocked_by"] != StandingBlockedOwnResult || d["result_id"] != first || d["verdict_eta"] != testTime+WorkStandingVerdictTTL || d["next_claimable"] != nil ||
		e.RetryAfter != int(WorkStandingVerdictTTL) || e.NextOpenAt != 0 || !strings.Contains(e.Message, "retrying will not help") || strings.Contains(e.Message, "Meanwhile") {
		t.Fatalf("seat_limit alone: %+v", e)
	}
	// An open paid task elsewhere: the refusal names it.
	plain := rewardRequest(t, s, owner, "lobby")
	run(t, s, rewardCreate(s, owner, plain, 40, 86400))
	e = refused()
	if d = e.Details.(map[string]any); d["next_claimable"] != plain || !strings.Contains(e.Message, "Meanwhile, a task you can claim now") || !strings.Contains(e.Message, plain) {
		t.Fatalf("seat_limit with a task to claim: %+v", e)
	}
}
