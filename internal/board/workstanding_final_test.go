package board

import (
	"errors"
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
)

// C836: a standing task whose credit pool holds no reward and has no
// result waiting for a verdict opens no seat by itself: the claim is
// seats_full (a wait code) with details.final true and no next_open_at or
// retry_after, and nothing is queued. Polling it was a 16/h loop on prod.
func TestStandingPoolEmptyIsFinal(t *testing.T) {
	s := rewardStore(t)
	owner, w1 := keyFor(240), keyFor(241)
	mintCredit(t, s, keyID(owner), allowance.Paid, 5000)
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, id, map[string]any{"seats": 2, "reward": 200, "day_max": 10}, 10*86400))
	if _, err := s.db.Exec("UPDATE work_rewards SET amount=0 WHERE work_id=?", id); err != nil {
		t.Fatal(err)
	}
	_, c := standingClaim(t, s, w1, id, nil)
	_, err := s.Execute(testContext, c, "test-origin")
	var e *Error
	if !errors.As(err, &e) || e.Status != 409 || e.Code != "seats_full" {
		t.Fatalf("claim on an empty pool: %v", err)
	}
	d, _ := e.Details.(map[string]any)
	if d["final"] != true || d["blocked_by"] != StandingBlockedPool || d["next_open_at"] != nil || e.NextOpenAt != 0 || e.RetryAfter != 0 {
		t.Fatalf("empty pool refusal: %+v details %#v", e, e.Details)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM works WHERE standing_id=?", id); n != 0 {
		t.Fatalf("a final refusal queued or opened a result: %d", n)
	}
}

// C836: a refusal that names when the next seat opens is a wait, never
// final; one with no opening before results_until is final.
func TestStandingNoSeatFinalOnlyWithoutOpening(t *testing.T) {
	const now, until = int64(1792886400 + 3600), int64(1793145600)
	var e *Error
	if !errors.As(errNoSeat(StandingBlockedSeats, now+600, until, now, "all 2 seats hold a result waiting for a verdict", "x"), &e) || e.Details.(map[string]any)["final"] != nil || e.NextOpenAt != now+600 {
		t.Fatalf("a wait marked final: %+v", e)
	}
	if !errors.As(errNoSeat(StandingBlockedSeats, 0, until, now, "all 2 seats hold a result waiting for a verdict", ""), &e) || e.Details.(map[string]any)["final"] != true || e.NextOpenAt != 0 || e.RetryAfter != 0 {
		t.Fatalf("no opening, not final: %+v", e)
	}
}

// C836: a standing task whose accepted results reach results_max refuses
// a claim with results_max_reached, even when its pool is empty too (the
// cap is checked before the pool), never seats_full.
func TestStandingResultsMaxAllAcceptedBeatsEmptyPool(t *testing.T) {
	s := rewardStore(t)
	owner, w1, w2, w3 := keyFor(242), keyFor(243), keyFor(244), keyFor(245)
	mintCredit(t, s, keyID(owner), allowance.Paid, 5000)
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, id, map[string]any{"seats": 2, "reward": 200, "day_max": 10, "results_max": 2}, 10*86400))
	r1, c1 := standingClaim(t, s, w1, id, nil)
	run(t, s, c1)
	r2, c2 := standingClaim(t, s, w2, id, nil)
	run(t, s, c2)
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: r1, Amount: 1}))
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: r2, Amount: 1}))
	if _, err := s.db.Exec("UPDATE work_rewards SET amount=0 WHERE work_id=?", id); err != nil {
		t.Fatal(err)
	}
	if st := standingAt(t, s, id); !st.Full || st.Open != 0 || st.Paid != 2 || st.Submitted != 0 {
		t.Fatalf("all accepted: %+v", st)
	}
	_, c3 := standingClaim(t, s, w3, id, nil)
	_, err := s.Execute(testContext, c3, "test-origin")
	var e *Error
	if !errors.As(err, &e) || e.Code != "results_max_reached" {
		t.Fatalf("claim past results_max with an empty pool: %v", err)
	}
	if d, _ := e.Details.(map[string]any); d["full"] != true || d["blocked_by"] != StandingBlockedResultsMax || e.NextOpenAt != 0 {
		t.Fatalf("results_max details: %#v", e.Details)
	}
}

// C860: a drained round whose requester set a successor names it in the
// final refusal, message and details, so a worker claims the next round
// instead of retrying this one.
func TestStandingPoolEmptyNamesSuccessor(t *testing.T) {
	s := rewardStore(t)
	owner, w1 := keyFor(246), keyFor(247)
	mintCredit(t, s, keyID(owner), allowance.Paid, 10000)
	round1 := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, round1, map[string]any{"seats": 2, "reward": 200, "day_max": 10}, 10*86400))
	round2 := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, round2, map[string]any{"seats": 2, "reward": 200, "day_max": 10}, 10*86400))
	run(t, s, successorSet(s, owner, round1, round2))
	if _, err := s.db.Exec("UPDATE work_rewards SET amount=0 WHERE work_id=?", round1); err != nil {
		t.Fatal(err)
	}
	_, c := standingClaim(t, s, w1, round1, nil)
	_, err := s.Execute(testContext, c, "test-origin")
	var e *Error
	if !errors.As(err, &e) || e.Code != "seats_full" {
		t.Fatalf("claim on a drained round: %v", err)
	}
	d, _ := e.Details.(map[string]any)
	if d["final"] != true || d["successor"] != round2 || !strings.Contains(e.Message, "opened the next round "+round2) {
		t.Fatalf("drained round refusal: %q details %#v", e.Message, e.Details)
	}
}
