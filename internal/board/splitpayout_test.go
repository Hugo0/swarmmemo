package board

import (
	"crypto/ed25519"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
)

// Split bounty payout (C307) on the real store, ledger and notary: a
// newcomer's first bounty pays in full, a non-newcomer's accept pays half now
// and settles half on day 14, a contradiction claws the deferred half back,
// and every path pays each half exactly once.

func splitConfig() Config {
	c := rewardConfig()
	c.Features.SplitPayout = true
	return c
}

func splitStore(t *testing.T) *Store {
	t.Helper()
	if _, _, ok := ledgerTestOverride(); ok {
		t.Skip("the ledger mode is overridden for the whole suite")
	}
	s := openTest(t, splitConfig())
	t.Cleanup(s.stopServices)
	setRewardParams(t, s)
	return s
}

// acceptTask runs one rewarded task end to end (create, claim, submit, accept)
// and returns its id and the accept acknowledgement.
func acceptTask(t *testing.T, s *Store, owner, worker ed25519.PrivateKey, reward int64) (string, WorkAck) {
	t.Helper()
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, rewardCreate(s, owner, id, reward, 7*86400))
	fence := claimAndSubmit(t, s, worker, id, "lobby")
	ack := run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: id, Amount: fence})).Data["ack"].(WorkAck)
	return id, ack
}

func TestSplitPayoutNewcomerThenSplitAndSettle(t *testing.T) {
	s := splitStore(t)
	owner, worker := keyFor(160), keyFor(161)
	requester, payee := keyID(owner), keyID(worker)
	mintCredit(t, s, requester, allowance.Paid, 10000)

	// First accepted bounty: a newcomer is paid in full upfront, no settlement.
	idA, ackA := acceptTask(t, s, owner, worker, 600)
	if got := creditIn(t, s, payee, "remaining"); got != 600 {
		t.Fatalf("newcomer full pay: worker has %d, want 600", got)
	}
	if r := getTestWork(t, s, idA).Reward; r == nil || r.State != "paid" || r.Settlement != nil {
		t.Fatalf("newcomer reward should be a full paid reward: %+v", r)
	}
	if strings.Contains(ackA.Note, "settle on") {
		t.Fatalf("newcomer note should not mention a settlement: %q", ackA.Note)
	}

	// Second accepted bounty: now a non-newcomer, so it splits half/half.
	idB, ackB := acceptTask(t, s, owner, worker, 600)
	if got := creditIn(t, s, payee, "remaining"); got != 900 {
		t.Fatalf("split upfront: worker should have 600+300=900, has %d", got)
	}
	if held := creditIn(t, s, requester, "held"); held != 300 {
		t.Fatalf("the deferred half (300) should stay held, held=%d", held)
	}
	r := getTestWork(t, s, idB).Reward
	if r == nil || r.State != "settling" || r.Settlement == nil {
		t.Fatalf("split reward should be settling: %+v", r)
	}
	set := r.Settlement
	if set.State != "settling" || set.Upfront != 300 || set.Deferred != 300 || set.SettlesAt != testTime+14*86400 {
		t.Fatalf("settlement: %+v", set)
	}
	if !strings.Contains(ackB.Note, "300 credits go to the worker now") || !strings.Contains(ackB.Note, "settle on") {
		t.Fatalf("split accept note: %q", ackB.Note)
	}

	// The worker's journal surfaces the settling payment.
	b, _, _ := journalOf(t, run(t, s, journalGet(worker, "", 0)))
	if p := path(t, b, "settling_payments", "pending"); p.(float64) != 1 {
		t.Fatalf("journal settling_payments.pending: %v", p)
	}
	if c := path(t, b, "settling_payments", "pending_credits"); c.(float64) != 300 {
		t.Fatalf("journal settling_payments.pending_credits: %v", c)
	}

	// Before day 14, a sweep settles nothing.
	s.now = func() time.Time { return time.Unix(testTime+13*86400, 0) }
	if _, err := s.SweepAllowance(testContext); err != nil {
		t.Fatal(err)
	}
	if r := getTestWork(t, s, idB).Reward; r.State != "settling" {
		t.Fatalf("settled before day 14: %+v", r)
	}
	if got := creditIn(t, s, payee, "remaining"); got != 900 {
		t.Fatalf("worker paid early: %d", got)
	}

	// On day 14, the deferred half settles to the worker.
	s.now = func() time.Time { return time.Unix(testTime+14*86400, 0) }
	if _, err := s.SweepAllowance(testContext); err != nil {
		t.Fatal(err)
	}
	if got := creditIn(t, s, payee, "remaining"); got != 1200 {
		t.Fatalf("deferred settlement: worker should have 1200, has %d", got)
	}
	if held := creditIn(t, s, requester, "held"); held != 0 {
		t.Fatalf("a settled split leaks a hold of %d", held)
	}
	r = getTestWork(t, s, idB).Reward
	if r.State != "settled" || r.Settlement == nil || r.Settlement.State != "settled" {
		t.Fatalf("settled reward: %+v", r)
	}

	// Idempotent: a second sweep pays nothing more.
	if _, err := s.SweepAllowance(testContext); err != nil {
		t.Fatal(err)
	}
	if got := creditIn(t, s, payee, "remaining"); got != 1200 {
		t.Fatalf("double-paid the deferred half: %d", got)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM ledger_transfers WHERE to_account=?", payee); n != 3 {
		t.Fatalf("worker should have exactly 3 transfers (A full, B upfront, B deferred), has %d", n)
	}
	if err := s.Integrity(testContext); err != nil {
		t.Fatal(err)
	}
}

func TestSplitPayoutClawbackOnHiddenResult(t *testing.T) {
	s := splitStore(t)
	owner, worker := keyFor(162), keyFor(163)
	requester, payee := keyID(owner), keyID(worker)
	mintCredit(t, s, requester, allowance.Paid, 10000)

	// Make the worker a non-newcomer, then run a splitting task.
	acceptTask(t, s, owner, worker, 600)
	id, _ := acceptTask(t, s, owner, worker, 600)
	w := getTestWork(t, s, id)
	if r := w.Reward; r.State != "settling" {
		t.Fatalf("expected settling: %+v", r)
	}
	beforeHeld := creditIn(t, s, requester, "held")
	if beforeHeld != 300 {
		t.Fatalf("deferred held should be 300, is %d", beforeHeld)
	}

	// A contradiction: the accepted result is hidden during the hold.
	hideEvent(t, s, w.ResultID)

	// On day 14 the deferred half is clawed back to the requester, not paid.
	s.now = func() time.Time { return time.Unix(testTime+14*86400, 0) }
	if _, err := s.SweepAllowance(testContext); err != nil {
		t.Fatal(err)
	}
	if got := creditIn(t, s, payee, "remaining"); got != 900 {
		t.Fatalf("worker should keep only the upfront halves (600+300=900), has %d", got)
	}
	if held := creditIn(t, s, requester, "held"); held != 0 {
		t.Fatalf("clawback should release the hold, held=%d", held)
	}
	r := getTestWork(t, s, id).Reward
	if r.State != "clawed" || r.Settlement == nil || r.Settlement.State != "clawed" || r.Settlement.Reason != "result_hidden" {
		t.Fatalf("clawed reward: %+v (settlement %+v)", r, r.Settlement)
	}
	// Idempotent: a second sweep does not pay the clawed half out.
	if _, err := s.SweepAllowance(testContext); err != nil {
		t.Fatal(err)
	}
	if got := creditIn(t, s, payee, "remaining"); got != 900 {
		t.Fatalf("paid a clawed half: %d", got)
	}
	if err := s.Integrity(testContext); err != nil {
		t.Fatal(err)
	}
}

func TestSplitPayoutOffPaysFull(t *testing.T) {
	// With SPLIT_PAYOUT off, every accept pays the whole reward at once, even
	// for a non-newcomer: nothing on the wire changes.
	s := rewardStore(t)
	owner, worker := keyFor(164), keyFor(165)
	requester, payee := keyID(owner), keyID(worker)
	mintCredit(t, s, requester, allowance.Paid, 10000)
	acceptTask(t, s, owner, worker, 600)
	id, _ := acceptTask(t, s, owner, worker, 600)
	if got := creditIn(t, s, payee, "remaining"); got != 1200 {
		t.Fatalf("flag off should pay full both times (1200), worker has %d", got)
	}
	if r := getTestWork(t, s, id).Reward; r.State != "paid" || r.Settlement != nil {
		t.Fatalf("flag off reward: %+v", r)
	}
	if held := creditIn(t, s, requester, "held"); held != 0 {
		t.Fatalf("flag off leaves a hold of %d", held)
	}
}

func TestSplitDeferredAmountRounding(t *testing.T) {
	// The odd credit is paid upfront; too-small rewards do not split.
	tm := splitTerms{splitPayoutParams: defaultSplitPayoutParams(), on: true}
	for _, c := range []struct{ reward, deferred int64 }{{1, 0}, {2, 1}, {3, 1}, {4, 2}, {600, 300}, {601, 300}} {
		if got := tm.deferredAmount(c.reward); got != c.deferred {
			t.Fatalf("deferredAmount(%d)=%d, want %d", c.reward, got, c.deferred)
		}
	}
}

// hideEvent marks an event hidden, as moderation would during the hold.
func hideEvent(t *testing.T, s *Store, id string) {
	t.Helper()
	tx, err := s.db.BeginTx(testContext, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(testContext, "UPDATE events SET hidden=1 WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
