package ledger

import (
	"fmt"
	"testing"

	"swarmmemo/internal/allowance"
)

// C654: escrow payouts (accepted work) neither count toward the spike
// breaker nor trip it; plain transfers keep the spike; an active breaker
// still makes a payout pending.

// payEscrows escrows and pays n rewards of 10 from s, each to a new worker.
func (h *harness) payEscrows(s allowance.Subject, n int, tag string) []Transfer {
	h.t.Helper()
	var out []Transfer
	for i := 0; i < n; i++ {
		hold, err := h.escrow(s, 10, fmt.Sprint(tag, i), h.now+7*86400)
		if err != nil {
			h.t.Fatal(err)
		}
		tr, err := h.escrowPay(hold.ID, fmt.Sprint("worker-", tag, i))
		if err != nil {
			h.t.Fatal(err)
		}
		out = append(out, tr)
	}
	return out
}

func spikeParams() AllowanceParams {
	p := escrowParams()
	p.SpikeX, p.SpikeFloor = 4, 2
	return p
}

func TestEscrowPayoutsNeverTripTheSpike(t *testing.T) {
	h := newHarness(t, spikeParams())
	alice := signedSubject("alice")
	h.mintCredit("alice", allowance.Paid, 5000)
	for _, tr := range h.payEscrows(alice, 12, "w") {
		if tr.State != "done" {
			t.Fatalf("an accepted reward waits: %+v", tr)
		}
	}
	if n := h.count("SELECT count(*) FROM account_breakers WHERE account='alice'"); n != 0 {
		t.Fatalf("12 payouts in a day tripped %d breakers", n)
	}
	// Plain transfers still trip it, counted without the payouts: the
	// floor of 2 lets two through and holds the third.
	for i := 0; i < 2; i++ {
		if tr, err := h.creditTransfer(alice, fmt.Sprint("r", i), 5, fmt.Sprint("t", i)); err != nil || tr.State != "done" {
			t.Fatalf("plain transfer %d: %+v %v", i, tr, err)
		}
	}
	if tr, err := h.creditTransfer(alice, "r9", 5, "t9"); err != nil || tr.State != "pending" {
		t.Fatalf("third plain transfer of the day: %+v %v", tr, err)
	}
	if n := h.count("SELECT count(*) FROM account_breakers WHERE account='alice' AND reason='spike'"); n != 1 {
		t.Fatalf("spike breakers: %d", n)
	}
	// With the breaker active a payout waits, and the sweeper pays it.
	tr := h.payEscrows(alice, 1, "late")[0]
	if tr.State != "pending" || tr.ExecuteAt != h.now+h.p.TransferDelay {
		t.Fatalf("a payout under an active breaker: %+v", tr)
	}
	h.now += h.p.TransferDelay
	h.sweep()
	if got, _ := h.l.GetTransfer(ctx, h.db, tr.ID); got.State != "done" {
		t.Fatalf("after the delay: %+v", got)
	}
	checkInvariants(t, h.db, h.p)
}

func TestSpikeMedianCountsPlainTransfersOnly(t *testing.T) {
	h := newHarness(t, spikeParams())
	alice, carol := signedSubject("alice"), signedSubject("carol")
	h.mintCredit("alice", allowance.Paid, 20000)
	h.mintCredit("carol", allowance.Paid, 20000)
	// 20 days: alice pays 3 rewards a day, carol makes 1 plain transfer.
	for d := 0; d < 20; d++ {
		h.payEscrows(alice, 3, fmt.Sprint("d", d, "-"))
		if tr, err := h.creditTransfer(carol, "bob", 5, fmt.Sprint("c", d)); err != nil || tr.State != "done" {
			t.Fatalf("carol day %d: %+v %v", d, tr, err)
		}
		h.now += 86400
	}
	// Alice's median of plain transfers is 0: the floor of 2 applies, not
	// 4 × 3 from her payouts.
	for i := 0; i < 2; i++ {
		if tr, err := h.creditTransfer(alice, "bob", 5, fmt.Sprint("a", i)); err != nil || tr.State != "done" {
			t.Fatalf("alice plain %d: %+v %v", i, tr, err)
		}
	}
	if tr, err := h.creditTransfer(alice, "bob", 5, "a2"); err != nil || tr.State != "pending" {
		t.Fatalf("payouts raised alice's threshold: %+v %v", tr, err)
	}
	// Carol's median of 1 still raises hers to 4.
	for i := 0; i < 4; i++ {
		if tr, err := h.creditTransfer(carol, "bob", 5, fmt.Sprint("x", i)); err != nil || tr.State != "done" {
			t.Fatalf("carol plain %d: %+v %v", i, tr, err)
		}
	}
	if tr, err := h.creditTransfer(carol, "bob", 5, "x4"); err != nil || tr.State != "pending" {
		t.Fatalf("carol's fifth: %+v %v", tr, err)
	}
	checkInvariants(t, h.db, h.p)
}
