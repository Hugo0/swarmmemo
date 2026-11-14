package board

import (
	"errors"
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
)

// C959 (an agent's friction report, 11-14): a standing create its credits
// cannot back says the reward is per result and seats multiply it, with the
// numbers the hold compared and what the requester has; standing fields sent
// without seats name themselves and every field that needs seats.
func TestWorkStandingCreateRefusalsExplainSeats(t *testing.T) {
	s := rewardStore(t)
	owner := keyFor(240)
	mintCredit(t, s, keyID(owner), allowance.Paid, 400)
	id := rewardRequest(t, s, owner, "lobby")
	ttl := int64(10 * 86400)

	_, err := s.Execute(testContext, standingCreate(s, owner, id, map[string]any{"seats": 3, "reward": 225}, ttl), "test-origin")
	var e *Error
	if !errors.As(err, &e) || e.Code != "not_transferable" && e.Code != "quota_exhausted" {
		t.Fatalf("a 675-credit pool on 400 lasting credits: %v", err)
	}
	for _, want := range []string{"A standing task's reward is per result, and seats multiply it: work.create holds 225 x 3 = 675 credits", "and you have ", "credits now"} {
		if !strings.Contains(e.Message, want) {
			t.Fatalf("pool refusal %q lacks %q", e.Message, want)
		}
	}
	if e.Code == "not_transferable" && !strings.HasSuffix(e.Message, errWorkRewardNotTransferable().(*Error).Message) {
		t.Fatalf("pool refusal %q lost the reward's not_transferable text", e.Message)
	}

	tiers := []any{map[string]any{"name": "bug", "credits": 300}}
	msg := failsWith(t, s, standingCreate(s, owner, id, map[string]any{"tiers": tiers, "per_agent_day": 1}, ttl), "invalid_work_data")
	want := "You sent per_agent_day and tiers without seats. These standing-task fields each need seats (1 to 50, the results paid at once) in the same work data: per_agent_day, day_max, results_max, per_agent_max, per_agent_cooldown_days, tiers, usdc_per_agent_day and usdc_day_max."
	if !strings.HasPrefix(msg, want) {
		t.Fatalf("seats refusal %q, want it to start %q", msg, want)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM works"); n != 0 {
		t.Fatalf("refused creates left %d works", n)
	}
}
