package board

import (
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
)

// successorSet is the requester's signed work.successor.set on id naming next.
func successorSet(s *Store, key ed25519.PrivateKey, id, next string) Command {
	b, _ := json.Marshal(map[string]any{"schema": 1, "generation": s.generation, "successor": next})
	return signed(key, Command{Operation: WorkSuccessorSet, MessageID: id, Data: string(b), Timestamp: s.now().Unix()})
}

// C391: the requester of a standing task names the next round. Refused for
// another requester's task (either end), a task that is not standing, the
// task itself, one not open and a cycle; work.get shows standing.successor,
// and while the round is not claimable standing.next_round, the first
// claimable round along the chain.
func TestWorkStandingSuccessor(t *testing.T) {
	s := rewardStore(t)
	owner, other := keyFor(200), keyFor(201)
	mintCredit(t, s, keyID(owner), allowance.Paid, 10000)
	mintCredit(t, s, keyID(other), allowance.Paid, 5000)
	ttl := int64(10 * 86400)
	standing := func(key ed25519.PrivateKey, extra map[string]any) string {
		id := rewardRequest(t, s, key, "lobby")
		data := map[string]any{"seats": 1, "reward": 100}
		for k, v := range extra {
			data[k] = v
		}
		run(t, s, standingCreate(s, key, id, data, ttl))
		return id
	}
	round1 := standing(owner, map[string]any{"results_max": 1})
	round2 := standing(owner, map[string]any{"results_max": 1})
	round3 := standing(owner, nil)
	foreign := standing(other, nil)
	plain := rewardRequest(t, s, owner, "lobby")
	run(t, s, rewardCreate(s, owner, plain, 100, 86400))

	fails(t, s, successorSet(s, other, round1, round2), "work_forbidden")
	fails(t, s, successorSet(s, owner, round1, foreign), "invalid_work_data")
	fails(t, s, successorSet(s, owner, round1, plain), "invalid_work_data")
	fails(t, s, successorSet(s, owner, plain, round1), "work_state_conflict")
	fails(t, s, successorSet(s, owner, round1, round1), "invalid_work_data")
	fails(t, s, successorSet(s, owner, round1, strings.Repeat("9", 32)), "invalid_work_data")
	b, _ := json.Marshal(map[string]any{"schema": 1, "generation": s.generation})
	fails(t, s, signed(owner, Command{Operation: WorkSuccessorSet, MessageID: round1, Data: string(b), Timestamp: s.now().Unix()}), "invalid_work_data")

	// round1 → round2 → round3; round3 → round1 would be a cycle.
	run(t, s, successorSet(s, owner, round1, round2))
	run(t, s, successorSet(s, owner, round2, round3))
	fails(t, s, successorSet(s, owner, round3, round1), "invalid_work_data")

	// round1 is claimable: it names its successor, no next_round.
	w := getTestWork(t, s, round1)
	if !w.Claimable || w.Standing.Successor != round2 || w.Standing.NextRound != "" {
		t.Fatalf("open round: claimable %v successor %q next %q", w.Claimable, w.Standing.Successor, w.Standing.NextRound)
	}
	// round1 fills: next_round is round2.
	_, c := standingClaim(t, s, keyFor(202), round1, nil)
	run(t, s, c)
	if w = getTestWork(t, s, round1); w.Claimable || w.Standing.NextRound != round2 {
		t.Fatalf("full round1: claimable %v next %q", w.Claimable, w.Standing.NextRound)
	}
	// round2 fills too: round1's next_round resolves through it to round3.
	_, c = standingClaim(t, s, keyFor(203), round2, nil)
	run(t, s, c)
	if w = getTestWork(t, s, round1); w.Standing.NextRound != round3 {
		t.Fatalf("round1 next %q, want round3 %s", w.Standing.NextRound, round3)
	}
	// A shorter cycle is refused too; a successor can be named on a closed
	// round (not named as one), and the history holds each signed change.
	fails(t, s, successorSet(s, owner, round3, round2), "invalid_work_data")
	run(t, s, workCommand(s, owner, Command{Operation: "work.cancel", MessageID: round2, Reason: "next round"}))
	run(t, s, successorSet(s, owner, round2, round3))
	fails(t, s, successorSet(s, owner, round3, round2), "invalid_work_data")
	if w = getTestWork(t, s, round2); w.State != "cancelled" || w.Standing.NextRound != round3 {
		t.Fatalf("cancelled round2: %s next %q", w.State, w.Standing.NextRound)
	}
	found := 0
	for _, tr := range run(t, s, Command{Operation: "work.history", MessageID: round2}).Data["transitions"].([]WorkTransition) {
		if tr.Operation == WorkSuccessorSet {
			found++
		}
	}
	if found != 2 {
		t.Fatalf("round2 history holds %d successor changes", found)
	}
	// round3 is cancelled too: nothing claimable along the chain.
	run(t, s, workCommand(s, owner, Command{Operation: "work.cancel", MessageID: round3, Reason: "done"}))
	if w = getTestWork(t, s, round1); w.Standing.Successor != round2 || w.Standing.NextRound != "" {
		t.Fatalf("no open round: successor %q next %q", w.Standing.Successor, w.Standing.NextRound)
	}
}

// C964 (release check 1.148.23): a full round whose successor is open but
// paused by its day_max (no seat now, one opens tomorrow) names that
// successor as standing.next_round, as the message mark and the reply
// receipt already did (C954); it went unnamed on work.get and the listing.
func TestWorkStandingNextRoundPausedByDayMax(t *testing.T) {
	s := rewardStore(t)
	owner := keyFor(210)
	mintCredit(t, s, keyID(owner), allowance.Paid, 10000)
	ttl := int64(10 * 86400)
	standing := func(data map[string]any) string {
		id := rewardRequest(t, s, owner, "lobby")
		run(t, s, standingCreate(s, owner, id, data, ttl))
		return id
	}
	round1 := standing(map[string]any{"seats": 1, "reward": 100, "results_max": 1})
	round2 := standing(map[string]any{"seats": 2, "reward": 100, "day_max": 1})
	run(t, s, successorSet(s, owner, round1, round2))

	// round2 takes its one claim for today: open, no seat now, not full.
	_, c := standingClaim(t, s, keyFor(211), round2, nil)
	run(t, s, c)
	if w := getTestWork(t, s, round2); w.State != "open" || w.Claimable || w.Standing.Open != 0 || w.Standing.Full {
		t.Fatalf("round2 paused: state %s claimable %v open %d full %v", w.State, w.Claimable, w.Standing.Open, w.Standing.Full)
	}
	// round1 fills: its next_round is the paused round2.
	_, c = standingClaim(t, s, keyFor(212), round1, nil)
	run(t, s, c)
	if w := getTestWork(t, s, round1); w.Claimable || w.Standing.NextRound != round2 {
		t.Fatalf("full round1: claimable %v next %q, want paused round2 %s", w.Claimable, w.Standing.NextRound, round2)
	}
}


// C967: a full round whose next round has no seat now says when one opens
// there by itself (standing.next_round_open_at, that round's next_open_at):
// paused by day_max, the next 00:00 UTC, on work.get, the mark's line and a
// reply's receipt; a next round with a seat open now carries no time, and a
// next round that is full too is no next round, so no time either.
func TestWorkStandingNextRoundOpenAt(t *testing.T) {
	s := rewardStore(t)
	owner := keyFor(213)
	mintCredit(t, s, keyID(owner), allowance.Paid, 10000)
	ttl := int64(10 * 86400)
	standing := func(data map[string]any) string {
		id := rewardRequest(t, s, owner, "lobby")
		run(t, s, standingCreate(s, owner, id, data, ttl))
		return id
	}
	now := s.now().Unix()
	midnight := now - now%86400 + 86400
	opens := NextSeatOpensBy(midnight)
	if want := "next seat opens by " + time.Unix(midnight, 0).UTC().Format("2006-01-02") + " 00:00 UTC"; opens != want || NextSeatOpensBy(0) != "" {
		t.Fatalf("NextSeatOpensBy: %q, want %q", opens, want)
	}

	// Paused by day_max: the next 00:00 UTC.
	round1 := standing(map[string]any{"seats": 1, "reward": 100, "results_max": 1})
	round2 := standing(map[string]any{"seats": 2, "reward": 100, "day_max": 1})
	run(t, s, successorSet(s, owner, round1, round2))
	_, c := standingClaim(t, s, keyFor(214), round2, nil)
	run(t, s, c)
	_, c = standingClaim(t, s, keyFor(215), round1, nil)
	run(t, s, c)
	if st := standingAt(t, s, round2); st.Open != 0 || st.BlockedBy != StandingBlockedDayMax || st.NextOpenAt != midnight || st.NextRoundOpenAt != 0 {
		t.Fatalf("round2 paused by day_max: %+v", st)
	}
	if st := standingAt(t, s, round1); !st.Full || st.NextRound != round2 || st.NextRoundOpenAt != midnight {
		t.Fatalf("full round1: next %q opens %d, want %s at %d", st.NextRound, st.NextRoundOpenAt, round2, midnight)
	}
	got := run(t, s, Command{Operation: "message.get", MessageID: round1}).Messages
	if want := "Full: all 1 result taken. The next round is " + round2[:8] + " (" + opens + ")."; len(got) != 1 || got[0].Work == nil || got[0].Work.NextNote != want {
		t.Fatalf("round1 mark: %+v, want note %q", got, want)
	}
	r := run(t, s, signed(keyFor(216), Command{Operation: "post", Room: "lobby", ReplyTo: round1, Text: "result " + randomID()[:8], Timestamp: s.now().Unix()})).Receipt
	if r.ReplyWorkNextRound != round2 || !r.ReplyWorkNextFull || r.ReplyWorkNextOpenAt != midnight || !strings.Contains(WorkHintLine("", r), "no seat is open there now ("+opens+"). Wait for its next seat") {
		t.Fatalf("reply to full round1: %+v %q", r, WorkHintLine("", r))
	}

	// A next round with a seat open now: named, with no time.
	round3 := standing(map[string]any{"seats": 1, "reward": 100, "results_max": 1})
	round4 := standing(map[string]any{"seats": 1, "reward": 100, "results_max": 1})
	run(t, s, successorSet(s, owner, round3, round4))
	_, c = standingClaim(t, s, keyFor(217), round3, nil)
	run(t, s, c)
	if st := standingAt(t, s, round3); !st.Full || st.NextRound != round4 || st.NextRoundOpenAt != 0 {
		t.Fatalf("full round3, open round4: next %q opens %d", st.NextRound, st.NextRoundOpenAt)
	}
	// round4 fills too (results_max): no seat opens there again, so no
	// next round and no time.
	_, c = standingClaim(t, s, keyFor(218), round4, nil)
	run(t, s, c)
	if st := standingAt(t, s, round4); !st.Full || st.NextOpenAt != 0 {
		t.Fatalf("round4 full: %+v", st)
	}
	if st := standingAt(t, s, round3); st.NextRound != "" || st.NextRoundOpenAt != 0 {
		t.Fatalf("full round3, full round4: next %q opens %d", st.NextRound, st.NextRoundOpenAt)
	}
	if raw, _ := json.Marshal(standingAt(t, s, round3)); strings.Contains(string(raw), "next_round_open_at") {
		t.Fatalf("no next round, still a time: %s", raw)
	}
}
