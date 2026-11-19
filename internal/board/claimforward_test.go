package board

import (
	"errors"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
)

// claimRefused runs c and returns its refusal, which must have code.
func claimRefused(t *testing.T, s *Store, c Command, code string) (*Error, map[string]any) {
	t.Helper()
	_, err := s.Execute(testContext, c, "test-origin")
	var e *Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("claim: %v, want %s", err, code)
	}
	d, _ := e.Details.(map[string]any)
	return e, d
}

// C1100: a claim on a standing round that takes no new result keeps its
// code and names the open round its successor chain forwards to (details
// successor, and next_open_at while that round has no seat now), or, with
// none, points at find_work; a seats_full that is a wait names a next
// round only when one has a seat now.
func TestClaimOnClosedRoundNamesNextRound(t *testing.T) {
	s := rewardStore(t)
	owner := keyFor(230)
	mintCredit(t, s, keyID(owner), allowance.Paid, 20000)
	round := func(data map[string]any, ttl int64) string {
		id := rewardRequest(t, s, owner, "lobby")
		run(t, s, standingCreate(s, owner, id, data, ttl))
		return id
	}
	long := int64(30 * 86400)
	short := WorkStandingVerdictTTL + 3600
	expired := round(map[string]any{"seats": 1, "reward": 100}, short)
	open := round(map[string]any{"seats": 1, "reward": 100}, long)
	run(t, s, successorSet(s, owner, expired, open))
	expired2 := round(map[string]any{"seats": 1, "reward": 100}, short)
	paused := round(map[string]any{"seats": 2, "reward": 100, "day_max": 1}, long)
	run(t, s, successorSet(s, owner, expired2, paused))
	alone := round(map[string]any{"seats": 1, "reward": 100}, long)
	run(t, s, workCommand(s, owner, Command{Operation: "work.cancel", MessageID: alone, Reason: "done"}))

	start := s.now().Unix()
	s.now = func() time.Time { return time.Unix(start+short+60, 0) }

	// Expired, its next round open now.
	_, c := standingClaim(t, s, keyFor(231), expired, nil)
	e, d := claimRefused(t, s, c, "work_state_conflict")
	if want := " This round takes no new results; its next round is " + open + " (open now): claim there."; !strings.HasSuffix(e.Message, want) || d["successor"] != open || d["next_open_at"] != nil {
		t.Fatalf("expired round: %q details %#v", e.Message, d)
	}

	// Expired, its next round paused by day_max: when its next seat opens.
	_, c = standingClaim(t, s, keyFor(232), paused, nil)
	run(t, s, c)
	now := s.now().Unix()
	midnight := now - now%86400 + 86400
	_, c = standingClaim(t, s, keyFor(233), expired2, nil)
	e, d = claimRefused(t, s, c, "work_state_conflict")
	if want := " its next round is " + paused + " (" + NextSeatOpensBy(midnight) + "): claim there then."; !strings.HasSuffix(e.Message, want) || d["successor"] != paused || d["next_open_at"] != midnight {
		t.Fatalf("expired round, paused next: %q details %#v", e.Message, d)
	}

	// Cancelled with no successor: find_work.
	_, c = standingClaim(t, s, keyFor(234), alone, nil)
	e, d = claimRefused(t, s, c, "work_state_conflict")
	if !strings.HasSuffix(e.Message, " This round takes no new results and names no open next round: find_work over MCP or works.list kind=earn lists work open now.") || d["successor"] != nil {
		t.Fatalf("cancelled round: %q details %#v", e.Message, d)
	}

	// A full queue (seats_full, a wait): the next round has a seat now.
	busy := round(map[string]any{"seats": 1, "results_max": WorkStandingResultsMax, "reward": 100, "day_max": 10}, long)
	next := round(map[string]any{"seats": 1, "reward": 100}, long)
	run(t, s, successorSet(s, owner, busy, next))
	_, c = standingClaim(t, s, keyFor(235), busy, nil)
	run(t, s, c) // waiting for a verdict
	_, c = standingClaim(t, s, keyFor(236), busy, nil)
	run(t, s, c) // queued
	_, c = standingClaim(t, s, keyFor(237), busy, nil)
	e, d = claimRefused(t, s, c, "seats_full")
	if !strings.HasSuffix(e.Message, " Meanwhile its next round "+next+" has a seat open now: claim there.") || d["successor"] != next || d["blocked_by"] != StandingBlockedSeats {
		t.Fatalf("full round: %q details %#v", e.Message, d)
	}
	// Without a seat on the next round, a wait says nothing more.
	run(t, s, workCommand(s, owner, Command{Operation: "work.cancel", MessageID: next, Reason: "done"}))
	_, c = standingClaim(t, s, keyFor(238), busy, nil)
	e, d = claimRefused(t, s, c, "seats_full")
	if d["successor"] != nil || strings.Contains(e.Message, "next round") || strings.Contains(e.Message, "find_work") {
		t.Fatalf("full round, no next: %q details %#v", e.Message, d)
	}

	// A final seats_full (an empty pool) with no successor: find_work.
	drained := round(map[string]any{"seats": 1, "reward": 100, "day_max": 10}, long)
	if _, err := s.db.Exec("UPDATE work_rewards SET amount=0 WHERE work_id=?", drained); err != nil {
		t.Fatal(err)
	}
	_, c = standingClaim(t, s, keyFor(239), drained, nil)
	e, d = claimRefused(t, s, c, "seats_full")
	if d["final"] != true || !strings.Contains(e.Message, "find_work over MCP") {
		t.Fatalf("drained round: %q details %#v", e.Message, d)
	}
}
