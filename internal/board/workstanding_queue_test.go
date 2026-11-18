package board

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
)

// C661: a result sent while no seat is open (standing.open 0) is held
// queued, never refused or lost: its own work item reads queued (not 404),
// counts in standing.submitted, and opens with the same id when a verdict
// frees a seat, to be judged and paid as any result.
func TestWorkStandingQueuedResultOpensOnVerdict(t *testing.T) {
	s := rewardStore(t)
	owner, w1, w2, w3, w4 := keyFor(250), keyFor(251), keyFor(252), keyFor(253), keyFor(254)
	requester := keyID(owner)
	mintCredit(t, s, requester, allowance.Paid, 5000)
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, id, map[string]any{"seats": 1, "results_max": WorkStandingResultsMax, "reward": 200, "day_max": 10}, 10*86400))

	r1, c1 := standingClaim(t, s, w1, id, nil)
	run(t, s, c1)
	firstDeadline := getTestWork(t, s, r1).Deadline

	// No seat open: the claim answers queued, with when one opens.
	r2, c2 := standingClaim(t, s, w2, id, nil)
	ack := run(t, s, c2).Data["ack"].(WorkAck)
	if ack.WorkID != r2 || ack.State != "queued" || ack.ResultState != "queued" || ack.ResultWorkID != r2 || ack.StandingID != id || ack.Seat != 0 || ack.Seats != 1 ||
		ack.NextOpenAt != firstDeadline || !strings.Contains(ack.Note, "queued") || !strings.Contains(ack.Note, "same id") {
		t.Fatalf("queued ack: %+v", ack)
	}
	item := getTestWork(t, s, r2) // was 404 before C661
	if item.State != "queued" || item.StoredState != "open" || item.StandingID != id || item.ResultID != r2 || item.Worker == nil || item.Worker.ID != keyID(w2) ||
		item.NextOpenAt != firstDeadline || item.Reward != nil || item.Claimable || item.Standing != nil {
		t.Fatalf("queued result: %+v", item)
	}
	if st := standingAt(t, s, id); st.Submitted != 2 || st.Queued != 1 || st.Open != 0 || st.BlockedBy != StandingBlockedSeats || st.NextOpenAt != firstDeadline {
		t.Fatalf("standing with a queued result: %+v", st.WorkStandingSeats)
	}
	if held := creditIn(t, s, requester, "held"); held != 200 {
		t.Fatalf("a queued result moved credits: %d held", held)
	}
	history := run(t, s, Command{Operation: "work.history", MessageID: r2}).Data["transitions"].([]WorkTransition)
	if len(history) != 1 || history[0].Operation != "work.claim" || history[0].State != "queued" {
		t.Fatalf("queued history: %+v", history)
	}
	page := run(t, s, Command{Operation: "message.get", MessageID: r2})
	if raw, _ := json.Marshal(page.Messages); !strings.Contains(string(raw), `"result_of":"`+r2+`"`) || !strings.Contains(string(raw), `"state":"queued"`) {
		t.Fatalf("queued mark: %s", raw)
	}
	yours := run(t, s, signed(w2, Command{Operation: "work.get", MessageID: id})).Data["work"].(Work).YourResults
	if len(yours) != 1 || yours[0].WorkID != r2 || yours[0].State != "queued" {
		t.Fatalf("your_results: %+v", yours)
	}
	// Not judgeable yet, and not sent twice; one queued result per agent,
	// and the queue holds seats results.
	if msg := failsWith(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: r2, Amount: 1}), "work_state_conflict"); !strings.Contains(msg, "queued") {
		t.Fatalf("accept of a queued result: %s", msg)
	}
	// C931: accept with target on the queued item itself names the state
	// every read shows (queued), not the stored one, as the root path does.
	if msg := failsWith(t, s, acceptReplyCmd(s, owner, r2, r2, nil), "invalid_work_result"); !strings.Contains(msg, r2+", queued)") {
		t.Fatalf("accept with target on a queued item: %s", msg)
	}
	fails(t, s, workWith(s, w2, Command{Operation: "work.claim", MessageID: id, Target: r2}, nil), "result_in_use")
	_, again := standingClaim(t, s, w2, id, nil)
	if msg := failsWith(t, s, again, "seat_limit"); !strings.Contains(msg, "queued") {
		t.Fatalf("second result while queued: %s", msg)
	}
	_, c3 := standingClaim(t, s, w3, id, nil)
	// C933: a full queue is not final; it names the queue, not an end.
	if msg := failsWith(t, s, c3, "seats_full"); !strings.Contains(msg, "queue is full") || strings.Contains(msg, "none opens again") {
		t.Fatalf("claim with the queue full: %s", msg)
	}

	// The verdict on the waiting result frees the seat: the queued result
	// takes it at once, with the same id, funded and due from now.
	atTime(s, testTime+600)
	run(t, s, workCommand(s, owner, Command{Operation: "work.reject", MessageID: r1, Amount: 1, Reason: "not reproducible"}))
	item = getTestWork(t, s, r2)
	if item.State != "submitted" || item.StoredState != "submitted" || item.NextOpenAt != 0 || item.Reward == nil || item.Reward.Amount != 200 || item.Reward.State != "held" ||
		item.Deadline != testTime+600+WorkStandingVerdictTTL || item.CreatedAt != testTime+600 {
		t.Fatalf("opened result: %+v %+v", item, item.Reward)
	}
	if st := standingAt(t, s, id); st.Submitted != 1 || st.Queued != 0 || st.Open != 0 {
		t.Fatalf("standing after the queue opened: %+v", st.WorkStandingSeats)
	}
	history = run(t, s, Command{Operation: "work.history", MessageID: r2}).Data["transitions"].([]WorkTransition)
	if len(history) != 1 || history[0].State != "submitted" {
		t.Fatalf("opened history: %+v", history)
	}
	if held := creditIn(t, s, requester, "held"); held != 200 {
		t.Fatalf("held %d after the queue opened, want 200", held)
	}
	// Judged and paid as any result.
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: r2, Amount: 1}))
	if got := creditIn(t, s, keyID(w2), "remaining"); got != 200 {
		t.Fatalf("the queued worker got %d credits", got)
	}
	if rec := getTestWork(t, s, id).RequesterRecord; rec == nil || rec.Results != 2 || rec.Paid != 1 || rec.Rejected != 1 {
		t.Fatalf("requester record: %+v", rec)
	}
	r4, c4 := standingClaim(t, s, w4, id, nil)
	if ack := run(t, s, c4).Data["ack"].(WorkAck); ack.State != "submitted" || ack.WorkID != r4 {
		t.Fatalf("a claim with a seat open: %+v", ack)
	}
	if err := s.Integrity(testContext); err != nil {
		t.Fatal(err)
	}
}

// A queued result opens at next_open_at by itself: the sweep after the
// waiting result's verdict deadline lapses it and opens the queue; a
// verdict sent with a queued result's id opens it first when a seat is
// open for it.
func TestWorkStandingQueuedResultOpensAtNextOpen(t *testing.T) {
	s := rewardStore(t)
	owner, w1, w2, w3 := keyFor(255), keyFor(1), keyFor(2), keyFor(3)
	mintCredit(t, s, keyID(owner), allowance.Paid, 5000)
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, id, map[string]any{"seats": 1, "results_max": WorkStandingResultsMax, "reward": 100, "day_max": 10}, 20*86400))

	r1, c1 := standingClaim(t, s, w1, id, nil)
	run(t, s, c1)
	r2, c2 := standingClaim(t, s, w2, id, nil)
	next := run(t, s, c2).Data["ack"].(WorkAck).NextOpenAt
	if next != testTime+WorkStandingVerdictTTL {
		t.Fatalf("next_open_at %d", next)
	}
	atTime(s, next+60)
	if _, err := s.SweepAllowance(testContext); err != nil {
		t.Fatal(err)
	}
	if w := getTestWork(t, s, r1); w.State != "expired" {
		t.Fatalf("lapsed result reads %q", w.State)
	}
	if w := getTestWork(t, s, r2); w.State != "submitted" || w.Deadline != next+60+WorkStandingVerdictTTL || w.Reward == nil || w.Reward.State != "held" {
		t.Fatalf("after the sweep: %+v", w)
	}

	// A second queue, judged by its own id once the seat is free.
	r3, c3 := standingClaim(t, s, w3, id, nil)
	if ack := run(t, s, c3).Data["ack"].(WorkAck); ack.State != "queued" {
		t.Fatalf("queued: %+v", ack)
	}
	atTime(s, next+60+WorkStandingVerdictTTL+60) // r2 lapses; no sweep yet
	if w := getTestWork(t, s, r3); w.State != "queued" {
		t.Fatalf("before any write: %q", w.State)
	}
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: r3, Amount: 1}))
	if w := getTestWork(t, s, r3); w.State != "accepted" {
		t.Fatalf("accepted by its own id: %+v", w)
	}
	if err := s.Integrity(testContext); err != nil {
		t.Fatal(err)
	}
}

// Queued results count where a sent result counts: results_max, the
// duplicate check, and the agent's own one-at-a-time rule; a hidden one
// never opens.
func TestWorkStandingQueuedResultCounts(t *testing.T) {
	s := rewardStore(t)
	owner, w1, w2, w3 := keyFor(4), keyFor(5), keyFor(6), keyFor(7)
	mintCredit(t, s, keyID(owner), allowance.Paid, 5000)
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, id, map[string]any{"seats": 2, "reward": 100, "results_max": 3, "day_max": 10}, 10*86400))

	r1, c1 := standingClaim(t, s, w1, id, nil)
	run(t, s, c1)
	_, c2 := standingClaim(t, s, w2, id, nil)
	run(t, s, c2)
	// The same text as a queued result is a duplicate.
	text := "Bug: queued copy check " + randomID()[:8]
	q := run(t, s, signed(w3, Command{Operation: "post", Room: "lobby", ReplyTo: id, Text: text, Timestamp: s.now().Unix()})).Receipt.ID
	if ack := run(t, s, workWith(s, w3, Command{Operation: "work.claim", MessageID: id, Target: q}, nil)).Data["ack"].(WorkAck); ack.State != "queued" {
		t.Fatalf("queued: %+v", ack)
	}
	if st := standingAt(t, s, id); st.Submitted != 3 || st.Queued != 1 || !st.Full || st.BlockedBy != StandingBlockedResultsMax {
		t.Fatalf("results_max counts the queued result: %+v", st.WorkStandingSeats)
	}
	copyID := run(t, s, signed(keyFor(8), Command{Operation: "post", Room: "lobby", ReplyTo: id, Text: text, Timestamp: s.now().Unix()})).Receipt.ID
	fails(t, s, workWith(s, keyFor(8), Command{Operation: "work.claim", MessageID: id, Target: copyID}, nil), "duplicate_result")
	_, c4 := standingClaim(t, s, keyFor(9), id, nil)
	fails(t, s, c4, "results_max_reached")

	// Hidden while queued: it never opens, and the place it held is free.
	if _, err := s.db.Exec("UPDATE events SET hidden=1,hidden_by='room' WHERE id=?", q); err != nil {
		t.Fatal(err)
	}
	if st := standingAt(t, s, id); st.Queued != 0 || st.Full {
		t.Fatalf("hidden queued result still counts: %+v", st.WorkStandingSeats)
	}
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: r1, Amount: 1}))
	if n := sqlCount(t, s, "SELECT count(*) FROM works WHERE id=? AND state='cancelled'", q); n != 1 {
		t.Fatalf("hidden queued result not cancelled")
	}
	if err := s.Integrity(testContext); err != nil {
		t.Fatal(err)
	}
}

// C1067: a queued result shows its own opening, the time its claim's ACK
// gave, on work.get (what its page and the public GET render), not the task's next new seat, which counts it and every result
// queued after it (08:00:01 on the ACK read 16:00:01 on the item).
func TestWorkStandingQueuedResultOwnOpening(t *testing.T) {
	s := rewardStore(t)
	standingDayWhole = false
	owner, a, b, c := keyFor(233), keyFor(234), keyFor(235), keyFor(236)
	mintCredit(t, s, keyID(owner), allowance.Paid, 5000)
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, id, map[string]any{"seats": 3, "results_max": WorkStandingResultsMax, "reward": 300}, 10*86400))
	_, ca := standingClaim(t, s, a, id, nil)
	run(t, s, ca)
	day := testTime - testTime%86400
	rb, cb := standingClaim(t, s, b, id, nil)
	ackB := run(t, s, cb).Data["ack"].(WorkAck)
	// Queued results open in claim order (created_at, then id).
	s.now = func() time.Time { return time.Unix(testTime+1, 0) }
	rc, cc := standingClaim(t, s, c, id, nil)
	ackC := run(t, s, cc).Data["ack"].(WorkAck)
	if ackB.State != "queued" || ackB.NextOpenAt != day+8*3600+1 || ackC.State != "queued" || ackC.NextOpenAt != day+16*3600+1 {
		t.Fatalf("acks: %+v / %+v", ackB, ackC)
	}
	at := func(ack WorkAck) string { return time.Unix(ack.NextOpenAt, 0).UTC().Format(time.RFC3339) }
	if !strings.Contains(ackB.Note, at(ackB)) || !strings.Contains(ackC.Note, at(ackC)) {
		t.Fatalf("ack notes: %q / %q", ackB.Note, ackC.Note)
	}
	for _, q := range []struct {
		id  string
		ack WorkAck
	}{{rb, ackB}, {rc, ackC}} {
		if got := getTestWork(t, s, q.id); got.State != "queued" || got.NextOpenAt != q.ack.NextOpenAt {
			t.Fatalf("work.get %s: next_open_at %d, ack %d", q.id, got.NextOpenAt, q.ack.NextOpenAt)
		}
		signedGet := run(t, s, signed(owner, Command{Operation: "work.get", MessageID: q.id})).Data["work"].(Work)
		if signedGet.NextOpenAt != q.ack.NextOpenAt {
			t.Fatalf("signed work.get %s: %d, ack %d", q.id, signedGet.NextOpenAt, q.ack.NextOpenAt)
		}
	}
	// The task itself still says when its next new seat opens: after both.
	if st := standingAt(t, s, id); st.Queued != 2 || st.NextOpenAt < day+86400 {
		t.Fatalf("standing: %+v", st.WorkStandingSeats)
	}
	// At b's opening it opens; c's reads keep their own time.
	s.now = func() time.Time { return time.Unix(ackB.NextOpenAt, 0) }
	if _, err := s.SweepAllowance(testContext); err != nil {
		t.Fatal(err)
	}
	if got := getTestWork(t, s, rb); got.State != "submitted" {
		t.Fatalf("b at its opening: %q", got.State)
	}
	if got := getTestWork(t, s, rc); got.State != "queued" || got.NextOpenAt != ackC.NextOpenAt {
		t.Fatalf("c after b opened: %+v", got)
	}
}

// C1088 (makeprofitcodex): results queued past the day's day_max carry into
// the next day by its even release, not all onto its 00:00. At 23:00 with
// day_max 3 taken, the first queued opens at 00:00 and the second at
// 08:00:01, and each ACK says so (both read 00:00 before).
func TestWorkStandingQueuedETAAcrossMidnight(t *testing.T) {
	s := rewardStore(t)
	standingDayWhole = false
	day := testTime - testTime%86400
	atTime(s, day+23*3600)
	owner := keyFor(40)
	mintCredit(t, s, keyID(owner), allowance.Paid, 5000)
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, id, map[string]any{
		"seats": 3, "day_max": 3, "results_max": 12, "reward": 100,
	}, 10*86400))
	for i := 0; i < 3; i++ {
		result, claim := standingClaim(t, s, keyFor(byte(41+i)), id, nil)
		if ack := run(t, s, claim).Data["ack"].(WorkAck); ack.State != "submitted" {
			t.Fatalf("initial claim: %+v", ack)
		}
		run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: result, Amount: 1}))
	}
	first, claimFirst := standingClaim(t, s, keyFor(44), id, nil)
	ackFirst := run(t, s, claimFirst).Data["ack"].(WorkAck)
	atTime(s, day+23*3600+1)
	second, claimSecond := standingClaim(t, s, keyFor(45), id, nil)
	ackSecond := run(t, s, claimSecond).Data["ack"].(WorkAck)
	midnight := day + 86400
	wantSecond := midnight + 8*3600 + 1
	if ackFirst.State != "queued" || ackFirst.NextOpenAt != midnight || ackSecond.State != "queued" || ackSecond.NextOpenAt != wantSecond {
		t.Fatalf("ACKs: first %s %s, second %s %s (want 00:00 and 08:00:01)", ackFirst.State, time.Unix(ackFirst.NextOpenAt, 0).UTC(), ackSecond.State, time.Unix(ackSecond.NextOpenAt, 0).UTC())
	}
	atTime(s, midnight)
	if _, err := s.SweepAllowance(testContext); err != nil {
		t.Fatal(err)
	}
	if f, g := getTestWork(t, s, first), getTestWork(t, s, second); f.State != "submitted" || g.State != "queued" || g.NextOpenAt != wantSecond {
		t.Fatalf("at 00:00: first %s, second %s next %d", f.State, g.State, g.NextOpenAt)
	}
	atTime(s, wantSecond)
	if _, err := s.SweepAllowance(testContext); err != nil {
		t.Fatal(err)
	}
	if got := getTestWork(t, s, second).State; got != "submitted" {
		t.Fatalf("second at its ETA: %s", got)
	}
}
