package ledger

import (
	"database/sql"
	"testing"

	"swarmmemo/internal/allowance"
)

// Escrow on the real ledger and SQLite: held from lasting transferable lots
// with the transfer fee, paid exactly once, released exactly once, the
// inbound cap, the breaker, spend limits and no decay while held.

func (h *harness) escrow(s allowance.Subject, amount int64, key string, until int64) (Hold, error) {
	var hold Hold
	err := h.do(func(q *sql.Tx) error {
		var err error
		hold, _, err = h.l.Escrow(ctx, q, s, allowance.Credit, amount, key, until, h.now)
		return err
	})
	return hold, err
}

func (h *harness) escrowPay(id, to string) (Transfer, error) {
	var t Transfer
	err := h.do(func(q *sql.Tx) error {
		var err error
		t, err = h.l.EscrowPay(ctx, q, id, to, h.now)
		return err
	})
	return t, err
}

func (h *harness) escrowRelease(id string) error {
	return h.do(func(q *sql.Tx) error { return h.l.EscrowRelease(ctx, q, id, "test", h.now) })
}

func (h *harness) creditOf(account, bucket string) int64 {
	h.t.Helper()
	return h.count("SELECT coalesce(sum(remaining-held),0) FROM ledger_lots WHERE account=? AND resource='credit' AND state='live' AND bucket=?", account, bucket)
}

func (h *harness) mintCredit(account string, b allowance.Bucket, units int64) {
	h.t.Helper()
	if err := h.do(func(q *sql.Tx) error { return h.l.Mint(ctx, q, account, allowance.Credit, b, units, "test", h.now) }); err != nil {
		h.t.Fatal(err)
	}
}

func escrowParams() AllowanceParams {
	p := anonCreditParams()
	rp := p.Resources[allowance.Credit]
	rp.TransferFee, rp.GrantSharePPM = 3, ppm
	return p
}

func TestEscrowPaysOnceAndReleasesOnce(t *testing.T) {
	h := newHarness(t, escrowParams())
	alice := signedSubject("alice")
	h.mintCredit("alice", allowance.Paid, 500)
	until := h.now + 7*86400
	// Today's free share expires at midnight, so it is never held: 505 is
	// more than the 500 paid units that last.
	if _, err := h.escrow(alice, 505, "w1", until); code(err) != "not_transferable" {
		t.Fatalf("free units that expire before the end: %v", err)
	}
	if _, err := h.escrow(alice, 5000, "w1", until); code(err) != "quota_exhausted" {
		t.Fatalf("more than the balance: %v", err)
	}
	hold, err := h.escrow(alice, 400, "w1", until)
	if err != nil || hold.State != "held" || hold.Max != 400 {
		t.Fatalf("%+v %v", hold, err)
	}
	if again, err := h.escrow(alice, 400, "w1", until); err != nil || again.ID != hold.ID {
		t.Fatalf("the same key returns the same hold: %+v %v", again, err)
	}
	if held := h.count("SELECT coalesce(sum(held),0) FROM ledger_lots WHERE account='alice' AND bucket='paid'"); held != 400 {
		t.Fatalf("400 lasting paid units held: %d", held)
	}
	if _, err = h.escrowPay(hold.ID, "alice"); code(err) != "self_transfer" {
		t.Fatalf("paying the holder: %v", err)
	}
	tr, err := h.escrowPay(hold.ID, "bob")
	if err != nil || tr.State != "done" || tr.Amount != 400 || tr.To != "bob" {
		t.Fatalf("%+v %v", tr, err)
	}
	again, err := h.escrowPay(hold.ID, "bob")
	if err != nil || again.ID != tr.ID {
		t.Fatalf("a second payment returns the first: %+v %v", again, err)
	}
	if got := h.creditOf("bob", "paid"); got != 400 {
		t.Fatalf("bob received %d", got)
	}
	if n := h.count("SELECT count(*) FROM ledger_entries WHERE kind='transfer_in' AND account='bob' AND op='work_reward' AND public_ref=?", tr.ID); n != 1 {
		t.Fatalf("bob's journal has %d payment lines", n)
	}
	if err = h.escrowRelease(hold.ID); code(err) != "hold_not_held" {
		t.Fatalf("release after payment: %v", err)
	}

	// A released escrow returns its units; the fee stays spent.
	total := "SELECT coalesce(sum(remaining),0) FROM ledger_lots WHERE account='alice' AND state='live'"
	before := h.count(total)
	second, err := h.escrow(alice, 50, "w2", until)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.escrowRelease(second.ID); err != nil {
		t.Fatal(err)
	}
	if err = h.escrowRelease(second.ID); code(err) != "hold_not_held" {
		t.Fatalf("second release: %v", err)
	}
	if _, err = h.escrowPay(second.ID, "bob"); code(err) != "hold_not_held" {
		t.Fatalf("payment after release: %v", err)
	}
	if after := h.count(total); after != before-3 || h.count("SELECT coalesce(sum(held),0) FROM ledger_lots WHERE account='alice'") != 0 {
		t.Fatalf("after the release alice has %d of %d", after, before)
	}
	if fees := h.count("SELECT coalesce(sum(amount),0) FROM ledger_entries WHERE account='alice' AND kind='fee' AND op='work_reward'"); fees != 6 {
		t.Fatalf("two escrows charged %d in fees", fees)
	}
	checkInvariants(t, h.db, h.p)
}

func TestEscrowInboundCapAndBreaker(t *testing.T) {
	p := escrowParams()
	p.Resources[allowance.Credit].InboundCap = 300
	h := newHarness(t, p)
	alice := signedSubject("alice")
	h.mintCredit("alice", allowance.Paid, 1000)
	hold, err := h.escrow(alice, 400, "w", h.now+86400*7)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.escrowPay(hold.ID, "bob"); code(err) != "recipient_limit" {
		t.Fatalf("over the inbound cap: %v", err)
	}
	if st, _, _ := h.l.GetHold(ctx, h.db, hold.ID); st.State != "held" {
		t.Fatalf("a refused payment changed the hold: %+v", st)
	}
	small, err := h.escrow(alice, 200, "w2", h.now+86400*7)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.do(func(q *sql.Tx) error { return h.l.TripBreaker(ctx, q, "alice", "agent.rotate", "old-key", h.now) }); err != nil {
		t.Fatal(err)
	}
	tr, err := h.escrowPay(small.ID, "bob")
	if err != nil || tr.State != "pending" || tr.ExecuteAt != h.now+p.TransferDelay {
		t.Fatalf("with the breaker on the payment waits: %+v %v", tr, err)
	}
	if err = h.escrowRelease(small.ID); code(err) != "hold_not_held" {
		t.Fatalf("a pending payment is not released as an escrow: %v", err)
	}
	h.now += p.TransferDelay
	h.sweep()
	if got, _ := h.l.GetTransfer(ctx, h.db, tr.ID); got.State != "done" {
		t.Fatalf("after the delay: %+v", got)
	}
	if n := h.count("SELECT coalesce(sum(amount),0) FROM ledger_entries WHERE kind='transfer_in' AND account='bob' AND op='work_reward'"); n != 200 {
		t.Fatalf("bob received %d", n)
	}
	checkInvariants(t, h.db, h.p)
}

func TestEscrowSpendLimitAndNoDecay(t *testing.T) {
	h := newHarness(t, escrowParams())
	key := limited("owner", "key:worker")
	h.setLimit("owner", "key:worker", ptr(100), nil, 0)
	h.mintCredit("owner", allowance.Granted, 1000)
	until := h.now + 30*86400
	if _, err := h.escrow(key, 98, "big", until); code(err) != "spend_limit" {
		t.Fatalf("amount and fee over the daily limit: %v", err)
	}
	hold, err := h.escrow(key, 90, "ok", until)
	if err != nil {
		t.Fatal(err)
	}
	if st := h.limitStatus("owner", "key:worker"); st.SpentToday != 93 {
		t.Fatalf("an escrow counts its amount and fee: %+v", st)
	}
	// Granted units decay daily, but not the held ones.
	for day := 1; day <= 20; day++ {
		h.now += 86400
		h.sweep()
	}
	if held := h.count("SELECT coalesce(sum(held),0) FROM ledger_lots WHERE account='owner'"); held != 90 {
		t.Fatalf("held units decayed to %d", held)
	}
	if err = h.escrowRelease(hold.ID); err != nil {
		t.Fatal(err)
	}
	h.now = day0 * 86400
	if st := h.limitStatus("owner", "key:worker"); st.SpentToday != 3 {
		t.Fatalf("a release gives the amount back to its day, the fee stays: %+v", st)
	}
	checkInvariants(t, h.db, h.p)
}
