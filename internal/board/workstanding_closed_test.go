package board

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
)

// C994: a fully paid standing round no longer re-opens its seats. Created
// without results_max it takes seats results in all, and an accept does not
// refill a pool its cap no longer needs; an older round created with no cap
// takes seats results in all once its requester names a successor, and its
// reads, claim refusal and the open-work list say so. work.cancel's
// acknowledgement names the new state.
func TestStandingRoundClosesWhenPaid(t *testing.T) {
	s := rewardStore(t)
	owner := keyFor(160)
	mintCredit(t, s, keyID(owner), allowance.Paid, 50000)
	standing := func() string {
		id := rewardRequest(t, s, owner, "lobby")
		run(t, s, standingCreate(s, owner, id, map[string]any{"seats": 2, "reward": 100, "day_max": 10}, 10*86400))
		return id
	}
	pool := func(id string) int64 {
		return sqlCount(t, s, "SELECT amount FROM work_rewards WHERE work_id=?", id)
	}
	payBoth := func(id string, first byte) {
		t.Helper()
		for i := range byte(2) {
			r, c := standingClaim(t, s, keyFor(first+i), id, nil)
			run(t, s, c)
			run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: r, Amount: 1}))
		}
	}

	// (1) The default cap: seats.
	round := standing()
	if st := standingAt(t, s, round); st.ResultsMax != 2 || st.Open != 2 {
		t.Fatalf("new round: %+v", st)
	}
	payBoth(round, 161)
	if st := standingAt(t, s, round); !st.Full || st.Open != 0 || st.Paid != 2 || st.BlockedBy != StandingBlockedResultsMax {
		t.Fatalf("paid round: %+v", st)
	}
	if got := pool(round); got != 0 {
		t.Fatalf("a full round's pool was refilled to %d", got)
	}
	_, c := standingClaim(t, s, keyFor(163), round, nil)
	fails(t, s, c, "results_max_reached")

	// (2) An older round (no cap) re-opens paid seats until a successor is
	// named; then it takes seats results in all.
	old, next := standing(), standing()
	if _, err := s.db.Exec("UPDATE work_standing SET results_max=0 WHERE work_id=?", old); err != nil {
		t.Fatal(err)
	}
	payBoth(old, 164)
	if st := standingAt(t, s, old); st.Full || st.Open != 2 || st.ResultsMax != 0 {
		t.Fatalf("older round before a successor: %+v", st)
	}
	run(t, s, successorSet(s, owner, old, next))
	w := getTestWork(t, s, old)
	if w.State != "open" || w.Claimable || !w.Standing.Full || w.Standing.Open != 0 || w.Standing.Successor != next || w.Standing.NextRound != next ||
		!strings.Contains(w.Standing.BlockedNote, "opened the next round "+next) {
		t.Fatalf("older round after its successor: claimable=%v %+v", w.Claimable, w.Standing)
	}
	_, c = standingClaim(t, s, keyFor(166), old, nil)
	_, err := s.Execute(testContext, c, "test-origin")
	var e *Error
	if !errors.As(err, &e) || e.Code != "results_max_reached" || !strings.Contains(e.Message, "next round "+next) {
		t.Fatalf("claim on a closed round: %v", err)
	}
	if d, _ := e.Details.(map[string]any); d["successor"] != next {
		t.Fatalf("closed round's refusal details: %#v", e.Details)
	}
	// Discovery lists the successor, not the closed round (C840).
	for _, kind := range []string{"open", ""} {
		ids := listedWorkIDs(t, s, kind, 25)
		if slices.Contains(ids, old) || !slices.Contains(ids, next) {
			t.Fatalf("kind=%q lists %v", kind, ids)
		}
	}

	// (3) work.cancel's acknowledgement names the new state.
	ack := run(t, s, workCommand(s, owner, Command{Operation: "work.cancel", MessageID: next, Reason: "done"})).Data["ack"].(WorkAck)
	if ack.State != "cancelled" || ack.WorkID != next {
		t.Fatalf("cancel ack: %+v", ack)
	}
	if err := s.Integrity(testContext); err != nil {
		t.Fatal(err)
	}
}
