package ledger

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
)

// The pending-transfer bound (TransfersPendingMax) on the real ledger and
// SQLite: work-reward payments (an escrow's) wait out the breaker's delay
// like any transfer but are not counted against it, so a requester can pay
// more than eight accepted works while its breaker is on; ordinary
// transfers stay bounded at eight, and the refusal names that bound.

func (h *harness) creditTransfer(from allowance.Subject, to string, amount int64, key string) (Transfer, error) {
	var t Transfer
	err := h.do(func(q *sql.Tx) error {
		var err error
		t, err = h.l.Transfer(ctx, q, from, to, allowance.Credit, amount, key, h.now)
		return err
	})
	return t, err
}

func TestEscrowPaymentsSkipThePendingTransferBound(t *testing.T) {
	p := escrowParams()
	h := newHarness(t, p)
	alice := signedSubject("alice")
	h.mintCredit("alice", allowance.Paid, 5000)
	until := h.now + 7*86400
	const works = 12
	holds := make([]Hold, works)
	for i := range holds {
		var err error
		if holds[i], err = h.escrow(alice, 100, fmt.Sprint("w", i), until); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.do(func(q *sql.Tx) error { return h.l.TripBreaker(ctx, q, "alice", "agent.rotate", "old-key", h.now) }); err != nil {
		t.Fatal(err)
	}
	var paid []Transfer
	for i, hold := range holds {
		tr, err := h.escrowPay(hold.ID, fmt.Sprint("worker", i))
		if err != nil || tr.State != "pending" || tr.ExecuteAt != h.now+p.TransferDelay {
			t.Fatalf("payment %d with the breaker on: %+v %v", i+1, tr, err)
		}
		paid = append(paid, tr)
	}

	// Ordinary transfers: eight pending at once, whatever escrow payments
	// wait beside them; the ninth is refused with the bound and the wait.
	for i := 0; i < TransfersPendingMax; i++ {
		tr, err := h.creditTransfer(alice, fmt.Sprint("friend", i), 10, fmt.Sprint("t", i))
		if err != nil || tr.State != "pending" {
			t.Fatalf("transfer %d: %+v %v", i+1, tr, err)
		}
		paid = append(paid, tr)
	}
	h.now += 3600
	_, err := h.creditTransfer(alice, "friend9", 10, "t9")
	var e *allowance.Err
	if !errors.As(err, &e) || e.Code != "hold_limit" {
		t.Fatalf("the ninth pending transfer: %v", err)
	}
	if want := int(p.TransferDelay - 3600); e.RetryAfter != want {
		t.Fatalf("retry_after %d, want %d (until the first executes)", e.RetryAfter, want)
	}
	for _, part := range []string{"8 transfers pending", "transfers_pending", "each waits 48 h", "about 47 h", "allowance.transfer.cancel"} {
		if !strings.Contains(e.Message, part) {
			t.Fatalf("the refusal %q lacks %q", e.Message, part)
		}
	}
	if strings.Contains(e.Message, "calls") {
		t.Fatalf("the refusal speaks of calls: %q", e.Message)
	}

	// After the delay the sweeper executes every one of them.
	h.now += p.TransferDelay
	h.sweep()
	for _, tr := range paid {
		if got, err := h.l.GetTransfer(ctx, h.db, tr.ID); err != nil || got.State != "done" {
			t.Fatalf("after the delay %s: %+v %v", tr.ID, got, err)
		}
	}
	for i := 0; i < works; i++ {
		if got := h.creditOf(fmt.Sprint("worker", i), "paid"); got != 100 {
			t.Fatalf("worker%d received %d", i, got)
		}
	}
	if n := h.count("SELECT count(*) FROM ledger_entries WHERE kind='transfer_in' AND op='work_reward'"); n != works {
		t.Fatalf("%d work_reward payment lines", n)
	}
	checkInvariants(t, h.db, h.p)
}

func TestApproxDuration(t *testing.T) {
	for in, want := range map[int64]string{1: "1 min", 60: "1 min", 61: "2 min", 3599: "60 min", 3600: "1 h", 5400: "2 h", 172800: "48 h"} {
		if got := approxDuration(in); got != want {
			t.Errorf("approxDuration(%d) = %q, want %q", in, got, want)
		}
	}
}
