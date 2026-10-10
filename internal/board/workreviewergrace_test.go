package board

import (
	"crypto/ed25519"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
)

// C80 on the real store and ledger with a fixed clock: the reviewer grace is
// a server parameter, the requester's fallback verdict is flagged in the
// history, a race between the reviewer and the requester has one winner, and
// the escrows move once each with nothing minted.

// totalCredit is every live credit lot's remaining units (held included).
func totalCredit(t *testing.T, s *Store) int64 {
	t.Helper()
	return sqlCount(t, s, "SELECT coalesce(sum(remaining),0) FROM ledger_lots WHERE resource='credit' AND state='live'")
}

func TestWorkReviewerGraceIsAServerParam(t *testing.T) {
	for _, bad := range []int64{60, ReviewerGraceMin - 1, ReviewerGraceMax + 1, -1} {
		c := rewardConfig()
		c.ReviewerGraceSeconds = bad
		if _, err := Open(t.TempDir()+"/board.sqlite", c); err == nil {
			t.Fatalf("grace %d opened", bad)
		}
	}
	if _, _, ok := ledgerTestOverride(); ok {
		t.Skip("the ledger mode is overridden for the whole suite")
	}
	c := rewardConfig()
	c.ReviewerGraceSeconds = 3600
	s := openTest(t, c)
	t.Cleanup(s.stopServices)
	setRewardParams(t, s)
	if s.ReviewerGrace() != 3600 || openTest(t, Config{}).ReviewerGrace() != ReviewerGraceDefault {
		t.Fatal("the grace in force")
	}
	owner, worker, judge := keyFor(230), keyFor(231), keyFor(232)
	requester, reviewer := keyID(owner), keyID(judge)
	register(t, s, judge)
	mintCredit(t, s, requester, allowance.Paid, 1000)
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, reviewedCreate(s, owner, id, map[string]any{"reward": 300, "reviewer": reviewer, "reviewer_fee": 20}, 0))
	fence := claimAndSubmit(t, s, worker, id, "lobby")
	at := s.now().Unix() + 3600
	if w := getTestWork(t, s, id); w.RequesterMayDecideAt != at {
		t.Fatalf("requester_may_decide_at %d, want %d", w.RequesterMayDecideAt, at)
	}
	atTime(s, at-1)
	msg := failsWith(t, s, workCommand(s, owner, Command{Operation: "work.reject", MessageID: id, Amount: fence, Reason: "late"}), "not_the_reviewer")
	if !strings.Contains(msg, time.Unix(at, 0).UTC().Format(time.RFC3339)) || !strings.Contains(msg, "(1 hour after the submit)") {
		t.Fatalf("refusal: %s", msg)
	}
	atTime(s, at)
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: id, Amount: fence}))
	transitions := run(t, s, Command{Operation: "work.history", MessageID: id}).Data["transitions"].([]WorkTransition)
	if last := transitions[len(transitions)-1]; last.Fallback != WorkFallbackReviewerSilent || last.Note != WorkReviewerSilentNote {
		t.Fatalf("the fallback accept: %+v", last)
	}
}

// The requester's fallback accept pays the reward once, returns the fee once,
// pays the reviewer nothing and mints nothing; the ledger refuses a second
// release of the same hold.
func TestWorkReviewerFallbackLedgerInvariants(t *testing.T) {
	s := rewardStore(t)
	owner, worker, judge := keyFor(233), keyFor(234), keyFor(235)
	requester, payee, reviewer := keyID(owner), keyID(worker), keyID(judge)
	register(t, s, judge)
	mintCredit(t, s, requester, allowance.Paid, 1000)
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, reviewedCreate(s, owner, id, map[string]any{"reward": 300, "reviewer": reviewer, "reviewer_fee": 20}, 0))
	fence := claimAndSubmit(t, s, worker, id, "lobby")
	supply, entries := totalCredit(t, s), sqlCount(t, s, "SELECT count(*) FROM ledger_entries")
	requesterBefore := creditIn(t, s, requester, "remaining")
	reviewerEntries := sqlCount(t, s, "SELECT count(*) FROM ledger_entries WHERE account=?", reviewer)
	reviewerBefore := creditIn(t, s, reviewer, "remaining")
	atTime(s, s.now().Unix()+ReviewerGraceDefault)
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: id, Amount: fence}))

	// The verdict only moves credit: the supply is unchanged, the requester
	// gives up exactly the reward (its held fee comes back), the worker gets it.
	if got := totalCredit(t, s); got != supply {
		t.Fatalf("the fallback changed the supply: %d -> %d", supply, got)
	}
	if got := creditIn(t, s, requester, "remaining"); got != requesterBefore-300 {
		t.Fatalf("the requester has %d, want %d", got, requesterBefore-300)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM ledger_transfers WHERE to_account=?", payee); n != 1 || creditIn(t, s, payee, "remaining") != 300 {
		t.Fatalf("the worker got %d transfers, %d credits", n, creditIn(t, s, payee, "remaining"))
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM ledger_entries WHERE account=?", reviewer); n != reviewerEntries || creditIn(t, s, reviewer, "remaining") != reviewerBefore {
		t.Fatalf("the silent reviewer gained ledger entries: %d -> %d", reviewerEntries, n)
	}
	if held := creditIn(t, s, requester, "held"); held != 0 {
		t.Fatalf("held after settling: %d", held)
	}
	if sqlCount(t, s, "SELECT count(*) FROM ledger_entries") <= entries {
		t.Fatal("the verdict wrote no ledger entries")
	}
	// The released fee cannot be released or paid again.
	f, err := func() (*rewardRow, error) {
		tx, err := s.db.BeginTx(testContext, nil)
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		return loadWorkReward(testContext, tx, workReviewFeesTable, id)
	}()
	if err != nil || f == nil || f.State != "released" || f.Reason != WorkFallbackReviewerSilent {
		t.Fatalf("fee row %+v %v", f, err)
	}
	tx, err := s.db.BeginTx(testContext, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ledger.led.EscrowRelease(testContext, tx, f.HoldID, "again", s.now().Unix()); err == nil {
		t.Fatal("the ledger released the fee twice")
	}
	_ = tx.Rollback()
	if err := s.Integrity(testContext); err != nil {
		t.Fatal(err)
	}
}

// After the grace, the reviewer and the requester judge at once: exactly one
// verdict lands, the other is a state conflict, and the money moves once.
func TestWorkReviewerFallbackRace(t *testing.T) {
	s := rewardStore(t)
	owner, judge := keyFor(236), keyFor(237)
	requester, reviewer := keyID(owner), keyID(judge)
	register(t, s, judge)
	mintCredit(t, s, requester, allowance.Paid, 5000)
	for i := range 6 {
		worker := keyFor(byte(240 + i))
		id := rewardRequest(t, s, owner, "lobby")
		run(t, s, reviewedCreate(s, owner, id, map[string]any{"reward": 100, "reviewer": reviewer, "reviewer_fee": 10}, 0))
		fence := claimAndSubmit(t, s, worker, id, "lobby")
		atTime(s, s.now().Unix()+ReviewerGraceDefault)
		supply := totalCredit(t, s)
		reviewerBefore := creditIn(t, s, reviewer, "remaining")
		verdict := func(key ed25519.PrivateKey, op string) Command {
			c := Command{Operation: op, MessageID: id, Amount: fence}
			if op == "work.reject" {
				c.Reason = "not good enough"
			}
			return workCommand(s, key, c)
		}
		ops := []string{"work.accept", "work.reject"}
		cmds := []Command{verdict(judge, ops[i%2]), verdict(owner, ops[(i/2)%2])}
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for k := range cmds {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[k] = s.Execute(testContext, cmds[k], "test-origin")
			}()
		}
		wg.Wait()
		won := -1
		for k, err := range errs {
			if err == nil {
				if won != -1 {
					t.Fatalf("round %d: both verdicts landed", i)
				}
				won = k
				continue
			}
			var e *Error
			if !errors.As(err, &e) || e.Code != "work_state_conflict" {
				t.Fatalf("round %d: the losing verdict: %v", i, err)
			}
		}
		if won == -1 {
			t.Fatalf("round %d: no verdict landed", i)
		}
		if got := totalCredit(t, s); got != supply {
			t.Fatalf("round %d changed the supply: %d -> %d", i, supply, got)
		}
		w := getTestWork(t, s, id)
		paidFee := creditIn(t, s, reviewer, "remaining") - reviewerBefore
		switch won {
		case 0: // the reviewer's verdict: its fee is paid
			if w.ReviewerFee.State != "paid" || paidFee != 10 {
				t.Fatalf("round %d reviewer won: fee %+v paid %d", i, w.ReviewerFee, paidFee)
			}
		case 1: // the requester's fallback: the fee is returned
			if w.ReviewerFee.State != "released" || w.ReviewerFee.Reason != WorkFallbackReviewerSilent || paidFee != 0 {
				t.Fatalf("round %d requester won: fee %+v paid %d", i, w.ReviewerFee, paidFee)
			}
		}
		paid := sqlCount(t, s, "SELECT count(*) FROM ledger_transfers WHERE to_account=?", keyID(worker))
		if want := map[string]int64{"work.accept": 1, "work.reject": 0}[cmds[won].Operation]; paid != want {
			t.Fatalf("round %d: %s paid the worker %d times", i, cmds[won].Operation, paid)
		}
	}
	if err := s.Integrity(testContext); err != nil {
		t.Fatal(err)
	}
}

// Private and simulated work keep their scope under the fallback: members
// see the flagged verdict, outsiders still see nothing, and neither moves
// credit.
func TestWorkReviewerFallbackPrivateAndSimulated(t *testing.T) {
	s := openTest(t, Config{})
	owner, worker, judge := keyFor(246), keyFor(247), keyFor(248)
	reviewer := keyID(judge)
	register(t, s, worker)
	register(t, s, judge)
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "private-review", Visibility: "private", Members: []string{keyID(worker), reviewer}}))
	entries := sqlCount(t, s, "SELECT count(*) FROM ledger_entries")
	for _, item := range []struct{ room, kind string }{{"private-review", "request"}, {"lobby", "simulation"}} {
		id := run(t, s, signed(owner, Command{Operation: "post", Room: item.room, Kind: item.kind, Text: "Judged brief", Timestamp: s.now().Unix()})).Receipt.ID
		run(t, s, reviewedCreate(s, owner, id, map[string]any{"reviewer": reviewer}, 0))
		fence := claimAndSubmit(t, s, worker, id, item.room)
		at := s.now().Unix() + ReviewerGraceDefault
		w := run(t, s, signed(owner, Command{Operation: "work.get", MessageID: id, Timestamp: s.now().Unix()})).Data["work"].(Work)
		if w.RequesterMayDecideAt != at {
			t.Fatalf("%s: requester_may_decide_at %d, want %d", item.kind, w.RequesterMayDecideAt, at)
		}
		atTime(s, at-1)
		failsWith(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: id, Amount: fence}), "not_the_reviewer")
		atTime(s, at)
		run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: id, Amount: fence}))
		transitions := run(t, s, signed(worker, Command{Operation: "work.history", MessageID: id, Timestamp: s.now().Unix()})).Data["transitions"].([]WorkTransition)
		if last := transitions[len(transitions)-1]; last.Fallback != WorkFallbackReviewerSilent {
			t.Fatalf("%s %s: the fallback accept: %+v", item.room, item.kind, last)
		}
		if item.room == "private-review" {
			for _, op := range []string{"work.get", "work.history"} {
				fails(t, s, Command{Operation: op, MessageID: id}, "not_found")
			}
		}
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM ledger_entries"); n != entries {
		t.Fatalf("unrewarded fallback verdicts wrote %d ledger entries", n-entries)
	}
}
