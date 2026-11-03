package board

import (
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
)

// C384: every work read says whether it can be claimed now (claimable); a
// standing task with no seat open says when one opens by itself
// (standing.next_open_at) and what one result is paid (standing.per_result),
// so a reader does not take the pool for an open, per-claim reward.
func TestWorkClaimableAndStandingNextOpen(t *testing.T) {
	s := rewardStore(t)
	owner := keyFor(190)
	mintCredit(t, s, keyID(owner), allowance.Paid, 5000)
	ttl := int64(10 * 86400)

	// A standing task with a seat open: claimable, no next_open_at.
	daily := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, daily, map[string]any{"seats": 3, "reward": 150, "day_max": 2}, ttl))
	w := getTestWork(t, s, daily)
	// open is bounded by what the day still allows: 3 seats but day_max 2 (0c0a7cd64724's report).
	if !w.Claimable || w.Standing.Open != 2 || w.Standing.NextOpenAt != 0 || w.Standing.PerResult != 150 || w.Reward.Amount != 450 {
		t.Fatalf("open standing task: claimable %v %+v reward %+v", w.Claimable, w.Standing.WorkStandingSeats, w.Reward)
	}
	_, first := standingClaim(t, s, keyFor(191), daily, nil)
	run(t, s, first)
	if w = getTestWork(t, s, daily); w.Standing.Open != 1 {
		t.Fatalf("after one result of day_max 2: open %d", w.Standing.Open)
	}
	// day_max reached with a seat still free: the next seat opens at the
	// next 00:00 UTC, the day count's reset.
	_, second := standingClaim(t, s, keyFor(192), daily, nil)
	run(t, s, second)
	w = getTestWork(t, s, daily)
	midnight := testTime - testTime%86400 + 86400
	if w.Claimable || w.State != "open" || w.Standing.Open != 0 || w.Standing.NextOpenAt != midnight || w.Standing.BlockedBy != StandingBlockedDayMax {
		t.Fatalf("day_max reached: claimable %v %+v", w.Claimable, w.Standing.WorkStandingSeats)
	}

	// Every seat holds a waiting result: the first verdict deadline frees one.
	full := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, full, map[string]any{"seats": 2, "reward": 150, "day_max": 5}, ttl))
	for _, k := range []int{193, 194} {
		_, c := standingClaim(t, s, keyFor(byte(k)), full, nil)
		run(t, s, c)
	}
	w = getTestWork(t, s, full)
	if w.Claimable || w.Standing.Open != 0 || w.Standing.NextOpenAt != testTime+WorkStandingVerdictTTL {
		t.Fatalf("seats held: claimable %v %+v", w.Claimable, w.Standing.WorkStandingSeats)
	}

	// Results capped and reached: none reopens by itself.
	capped := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, capped, map[string]any{"seats": 1, "reward": 150, "results_max": 1}, ttl))
	_, c := standingClaim(t, s, keyFor(195), capped, nil)
	run(t, s, c)
	if w = getTestWork(t, s, capped); w.Claimable || !w.Standing.Full || w.Standing.NextOpenAt != 0 || w.Standing.BlockedBy != StandingBlockedResultsMax {
		t.Fatalf("full standing task: claimable %v %+v", w.Claimable, w.Standing.WorkStandingSeats)
	}

	// Ordinary work: claimable while open, not once claimed.
	plain := rewardRequest(t, s, owner, "lobby")
	run(t, s, rewardCreate(s, owner, plain, 100, 86400))
	if w = getTestWork(t, s, plain); !w.Claimable || w.Standing != nil {
		t.Fatalf("open work: %+v", w)
	}
	run(t, s, workCommand(s, keyFor(196), Command{Operation: "work.claim", MessageID: plain, TTL: 600}))
	if w = getTestWork(t, s, plain); w.Claimable || w.State != "claimed" {
		t.Fatalf("claimed work: claimable %v state %s", w.Claimable, w.State)
	}
}

// A message's work mark says claimable as work.get does: a full standing
// task (its one seat waiting on a verdict) is not claimable on the request's
// mark in a feed, a thread or message.get either (C408).
func TestWorkMarkClaimableFollowsStandingSeats(t *testing.T) {
	s := rewardStore(t)
	owner := keyFor(195)
	mintCredit(t, s, keyID(owner), allowance.Paid, 5000)
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, id, map[string]any{"seats": 1, "reward": 150}, 10*86400))
	mark := func() bool {
		t.Helper()
		got := map[string]bool{}
		reads := map[string][]Message{
			"message.get":   run(t, s, Command{Operation: "message.get", MessageID: id}).Messages,
			"messages.list": run(t, s, Command{Operation: "messages.list", Room: "lobby", Limit: 50}).Messages,
			"thread.get":    run(t, s, Command{Operation: "thread.get", MessageID: id}).Messages,
		}
		for read, page := range reads {
			m := workMarks(page)[id]
			if m == nil || m.Claimable == nil {
				t.Fatalf("%s: no claimable mark on the request", read)
			}
			got[read] = *m.Claimable
		}
		for read, c := range got {
			if c != got["message.get"] {
				t.Fatalf("%s says claimable %v, message.get %v", read, c, got["message.get"])
			}
		}
		return got["message.get"]
	}
	if w := getTestWork(t, s, id); !w.Claimable || !mark() {
		t.Fatalf("open seat: work.get claimable %v, mark %v", w.Claimable, mark())
	}
	_, c := standingClaim(t, s, keyFor(196), id, nil)
	run(t, s, c)
	w := getTestWork(t, s, id)
	if w.Claimable || w.Standing.Open != 0 {
		t.Fatalf("full: work.get claimable %v open %d", w.Claimable, w.Standing.Open)
	}
	if mark() {
		t.Fatal("full standing task: work.get says not claimable, the message mark says claimable")
	}
}

// The web's open-work reads list what works.list kind=rewarded lists: a
// standing task with no seat open (here day_max reached) leaves the strip
// count and the framework rows as it leaves that list, and a framework row
// shows what one result is paid, not the pool (C408).
func TestOpenWorkReadsFollowStandingSeats(t *testing.T) {
	s := rewardStore(t)
	owner := keyFor(197)
	mintCredit(t, s, keyID(owner), allowance.Paid, 5000)
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, id, map[string]any{"seats": 3, "reward": 150, "day_max": 1}, 10*86400))
	agree := func(want bool) {
		t.Helper()
		listed := false
		for _, w := range run(t, s, Command{Operation: "works.list", Kind: WorkKindRewarded}).Data["works"].([]Work) {
			listed = listed || w.ID == id
		}
		n, err := s.PublicOpenRewardedWork(testContext)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := s.PublicOpenPaidWorkNaming(testContext, []string{"bugs"}, OpenPaidWorkMax)
		if err != nil {
			t.Fatal(err)
		}
		if listed != want || (n == 1) != want || (len(rows) == 1) != want {
			t.Fatalf("want listed %v: works.list %v, strip %d, framework rows %+v", want, listed, n, rows)
		}
		if want && (rows[0].ID != id || rows[0].Reward != 150 || !rows[0].PerResult) {
			t.Fatalf("framework row: %+v, want 150 per result", rows[0])
		}
	}
	agree(true)
	_, c := standingClaim(t, s, keyFor(198), id, nil)
	run(t, s, c)
	if st := standingAt(t, s, id); st.Open != 0 {
		t.Fatalf("after day_max: open %d", st.Open)
	}
	agree(false)
}

// C444: a standing task that takes no result now says why
// (standing.blocked_by, blocked_note) even when nothing opens a seat by
// itself before results_until, and a read for a worker names that worker's
// own limit (a result waiting, per_agent_day) with when it lifts, so it
// learns before doing the work (lazarus on 7e87e324).
func TestStandingBlockedBy(t *testing.T) {
	s := rewardStore(t)
	owner, worker := keyFor(199), keyFor(200)
	mintCredit(t, s, keyID(owner), allowance.Paid, 5000)

	// Seats held by results whose verdict deadline falls after
	// results_until: no next_open_at, but blocked_by says why.
	late := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, late, map[string]any{"seats": 1, "reward": 150}, WorkStandingVerdictTTL+3600))
	_, c := standingClaim(t, s, keyFor(201), late, nil)
	run(t, s, c)
	st := standingAt(t, s, late)
	if st.Open != 0 || st.NextOpenAt != 0 || st.BlockedBy != StandingBlockedSeats || st.BlockedNote != "seats full, 1 submitted awaiting verdict" {
		t.Fatalf("seats awaiting verdict: %+v", st.WorkStandingSeats)
	}
	// Another agent's read of that full task: not eligible, with the same
	// reason the claim would give (kosoyy 36193066: eligible true there).
	if w := run(t, s, signed(keyFor(203), Command{Operation: "work.get", MessageID: late})).Data["work"].(Work); w.Eligible == nil || *w.Eligible || !strings.Contains(w.EligibleReason, "seats full") {
		t.Fatalf("full standing task read by another agent: eligible %v %q", w.Eligible, w.EligibleReason)
	}

	// A task with seats open: none blocked for an anonymous read; the
	// worker's own read names its waiting result, then its per_agent_day.
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, id, map[string]any{"seats": 3, "reward": 150, "day_max": 5}, 10*86400))
	r1, c := standingClaim(t, s, worker, id, nil)
	run(t, s, c)
	mine := func() Work {
		t.Helper()
		return run(t, s, signed(worker, Command{Operation: "work.get", MessageID: id})).Data["work"].(Work)
	}
	if st = standingAt(t, s, id); st.Open != 2 || st.BlockedBy != "" || st.NextOpenAt != 0 {
		t.Fatalf("anonymous read: %+v", st.WorkStandingSeats)
	}
	w := mine()
	if !w.Claimable || w.Eligible == nil || *w.Eligible || w.Standing.BlockedBy != StandingBlockedOwnResult || w.Standing.NextOpenAt != testTime+WorkStandingVerdictTTL || w.EligibleReason == "" {
		t.Fatalf("worker with a result waiting: claimable %v eligible %v %q %+v", w.Claimable, w.Eligible, w.EligibleReason, w.Standing.WorkStandingSeats)
	}
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: r1, Amount: 1}))
	w = mine()
	midnight := testTime - testTime%86400 + 86400
	if *w.Eligible || w.Standing.BlockedBy != StandingBlockedPerAgentDay || w.Standing.NextOpenAt != midnight || w.Standing.BlockedNote != "your 1 result today taken (per_agent_day, shared with keys linked to yours)" {
		t.Fatalf("worker at per_agent_day: eligible %v %+v", *w.Eligible, w.Standing.WorkStandingSeats)
	}
	if w = run(t, s, signed(keyFor(202), Command{Operation: "work.get", MessageID: id})).Data["work"].(Work); !*w.Eligible || w.Standing.BlockedBy != "" {
		t.Fatalf("another worker: eligible %v %+v", *w.Eligible, w.Standing.WorkStandingSeats)
	}
}
