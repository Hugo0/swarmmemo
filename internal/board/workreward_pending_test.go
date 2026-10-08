package board

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/ledger"
)

// C84 on the real store and ledger: a requester whose account-change
// breaker is on accepts more rewarded works than the pending-transfer bound
// (eight); every accept succeeds, every payment waits out transfer_delay
// and then executes. Ordinary credit transfers stay bounded at eight
// pending, and that refusal names pending transfers, not calls.
func TestWorkRewardAcceptsPastThePendingTransferBound(t *testing.T) {
	s := rewardStore(t)
	owner := keyFor(160)
	requester := keyID(owner)
	mintCredit(t, s, requester, allowance.Paid, 10_000)

	const works = 12
	ids := make([]string, works)
	for i := range ids {
		ids[i] = rewardRequest(t, s, owner, "lobby")
		run(t, s, rewardCreate(s, owner, ids[i], 100, WorkMaxTTL))
	}
	tx, err := s.db.BeginTx(testContext, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ledger.led.TripBreaker(testContext, tx, requester, "identity.link", "", s.now().Unix()); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for i, id := range ids {
		worker := keyFor(byte(170 + i))
		fence := claimAndSubmit(t, s, worker, id, "lobby")
		run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: id, Amount: fence}))
		if r := getTestWork(t, s, id).Reward; r.State != "pending" || r.ExecuteAt <= s.now().Unix() {
			t.Fatalf("accept %d: %+v", i+1, r)
		}
	}

	// Ordinary transfers: eight pending beside the twelve payments, then
	// the bound, with its own copy and the wait until the next executes.
	transfer := func(n int) Command {
		friend := keyFor(byte(200 + n))
		register(t, s, friend)
		return signed(owner, Command{Operation: "allowance.transfer", Target: keyID(friend), Amount: 10, Data: `{"schema":1,"resource":"credit"}`, Timestamp: s.now().Unix()})
	}
	for i := 0; i < TransfersPendingMax; i++ {
		run(t, s, transfer(i))
	}
	msg := failsWith(t, s, transfer(TransfersPendingMax), "hold_limit")
	for _, part := range []string{"8 transfers pending", "transfers_pending", "each waits 48 h", "about 48 h", "allowance.transfer.cancel"} {
		if !strings.Contains(msg, part) {
			t.Fatalf("hold_limit copy %q lacks %q", msg, part)
		}
	}
	if strings.Contains(msg, "calls") {
		t.Fatalf("hold_limit copy speaks of calls: %q", msg)
	}

	// After the delay the sweeper pays every worker and records it.
	at := s.now().Unix() + s.allowanceDefaults().TransferDelay
	s.now = func() time.Time { return time.Unix(at, 0) }
	if _, err = s.SweepAllowance(testContext); err != nil {
		t.Fatal(err)
	}
	for i, id := range ids {
		if r := getTestWork(t, s, id).Reward; r.State != "paid" {
			t.Fatalf("work %d after the delay: %+v", i+1, r)
		}
		if got := creditIn(t, s, keyID(keyFor(byte(170+i))), "remaining"); got != 100 {
			t.Fatalf("worker %d received %d", i+1, got)
		}
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM ledger_transfers WHERE from_account=? AND state='done'", requester); n != works+TransfersPendingMax {
		t.Fatalf("%d of the requester's transfers executed", n)
	}
	if err = s.Integrity(testContext); err != nil {
		t.Fatal(err)
	}
}

// The metered calls' hold_limit keeps its own copy; only the ledger's
// pending-transfer bound carries a sentence of its own.
func TestHoldLimitCopy(t *testing.T) {
	calls := fromAllowance(&allowance.Err{Code: "hold_limit", RetryAfter: 5})
	if e, ok := calls.(*Error); !ok || !strings.Contains(e.Message, "calls") || e.RetryAfter != 5 {
		t.Fatalf("calls: %+v", calls)
	}
	own := fromAllowance(&allowance.Err{Code: "hold_limit", RetryAfter: 7200, Message: fmt.Sprintf("You have %d transfers pending.", ledger.TransfersPendingMax)})
	if e, ok := own.(*Error); !ok || e.Status != 409 || e.Code != "hold_limit" || e.Message != "You have 8 transfers pending." || e.RetryAfter != 7200 {
		t.Fatalf("transfers: %+v", own)
	}
}
