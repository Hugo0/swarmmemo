package ledger

import (
	"database/sql"
	"fmt"
	"sync"
	"testing"

	"swarmmemo/internal/allowance"
)

// Spend limits per credential, on the real ledger and SQLite: the daily
// limit, its refund, the per-call ceiling, the UTC day, the expiry,
// transfers and two racing reserves.

func limited(owner, credential string) allowance.Subject {
	s := signedSubject(owner)
	s.Credential = credential
	return s
}

func (h *harness) setLimit(account, credential string, perDay, perCall *int64, expires int64) {
	h.t.Helper()
	if err := h.do(func(q *sql.Tx) error {
		return h.l.SetSpendLimit(ctx, q, SpendLimit{Credential: credential, Account: account, PerDay: perDay, PerCall: perCall, ExpiresAt: expires, SetBy: account}, h.now)
	}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) reserveCredit(s allowance.Subject, max int64, key string) (Hold, error) {
	var hold Hold
	err := h.do(func(q *sql.Tx) error {
		var err error
		hold, err = h.l.Reserve(ctx, q, s, allowance.Credit, max, key, Ref{Service: "fetch", Op: "service.call", Method: "page"}, 60, h.now)
		return err
	})
	return hold, err
}

func (h *harness) limitStatus(account, credential string) SpendLimitStatus {
	h.t.Helper()
	st, err := h.l.SpendLimitOf(ctx, h.db, account, credential, h.now)
	if err != nil {
		h.t.Fatal(err)
	}
	return st
}

func ptr(v int64) *int64 { return &v }

func spendLimitOf(err error) (string, int) {
	if e, ok := err.(*allowance.Err); ok && e.Code == "spend_limit" {
		return e.SpendLimit, e.RetryAfter
	}
	return fmt.Sprint(err), 0
}

func TestSpendLimitDaily(t *testing.T) {
	h := newHarness(t, anonCreditParams())
	key := limited("owner", "key:worker")
	h.setLimit("owner", "key:worker", ptr(100), nil, 0)
	hold, err := h.reserveCredit(key, 60, "a")
	if err != nil {
		t.Fatal(err)
	}
	if err = h.do(func(q *sql.Tx) error { _, err := h.l.Commit(ctx, q, hold.ID, 60, h.now); return err }); err != nil {
		t.Fatal(err)
	}
	if err := h.spendCredit(key, 30); err != nil {
		t.Fatal(err)
	}
	h.now += 3600
	_, err = h.reserveCredit(key, 20, "b")
	which, retry := spendLimitOf(err)
	if which != "per_day" || retry != 86400-3600 {
		t.Fatalf("over the daily limit: %v (%s, retry %d)", err, which, retry)
	}
	if err = h.spendCredit(key, 11); err == nil {
		t.Fatal("an immediate spend over the daily limit went through")
	}
	if err = h.spendCredit(key, 10); err != nil {
		t.Fatalf("exactly to the limit: %v", err)
	}
	// The owner's own key, and another credential, are not limited by it.
	if _, err = h.reserveCredit(signedSubject("owner"), 200, "owner"); err != nil {
		t.Fatalf("owner key: %v", err)
	}
	if err = h.spendCredit(limited("owner", "key:other"), 50); err != nil {
		t.Fatalf("an unlimited credential: %v", err)
	}
	st := h.limitStatus("owner", "key:worker")
	if !st.Set || st.SpentToday != 100 || st.Remaining == nil || *st.Remaining != 0 || st.ResetsAt != (h.now/86400+1)*86400 {
		t.Fatalf("status %+v", st)
	}
	if st := h.limitStatus("owner", "key:other"); st.Set || st.SpentToday != 50 || st.Remaining != nil {
		t.Fatalf("an unlimited credential's status %+v", st)
	}
	// Another account cannot read the limit as its own.
	if st := h.limitStatus("someone", "key:worker"); st.Set {
		t.Fatalf("another account read the limit: %+v", st)
	}
	// The next UTC day starts afresh.
	h.now = (h.now/86400 + 1) * 86400
	if _, err = h.reserveCredit(key, 100, "c"); err != nil {
		t.Fatalf("next day: %v", err)
	}
}

func TestSpendLimitRefundFreesTheLimit(t *testing.T) {
	h := newHarness(t, anonCreditParams())
	key := limited("owner", "key:worker")
	h.setLimit("owner", "key:worker", ptr(100), nil, 0)
	hold, err := h.reserveCredit(key, 100, "a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.reserveCredit(key, 1, "b"); err == nil {
		t.Fatal("the open hold did not count")
	}
	if err = h.do(func(q *sql.Tx) error { return h.l.Refund(ctx, q, hold.ID, "upstream_failed", h.now) }); err != nil {
		t.Fatal(err)
	}
	if st := h.limitStatus("owner", "key:worker"); st.SpentToday != 0 {
		t.Fatalf("after the refund: %+v", st)
	}
	hold, err = h.reserveCredit(key, 80, "c")
	if err != nil {
		t.Fatalf("after the refund: %v", err)
	}
	if err = h.do(func(q *sql.Tx) error { _, err := h.l.Commit(ctx, q, hold.ID, 30, h.now); return err }); err != nil {
		t.Fatal(err)
	}
	if st := h.limitStatus("owner", "key:worker"); st.SpentToday != 30 || *st.Remaining != 70 {
		t.Fatalf("after settling at 30 of 80: %+v", st)
	}
	// A hold made yesterday and refunded today gives back to yesterday.
	hold, err = h.reserveCredit(key, 70, "d")
	if err != nil {
		t.Fatal(err)
	}
	h.now = (h.now/86400 + 1) * 86400
	if _, err = h.reserveCredit(key, 100, "e"); err != nil {
		t.Fatalf("the next day: %v", err)
	}
	if err = h.do(func(q *sql.Tx) error { return h.l.Refund(ctx, q, hold.ID, "upstream_failed", h.now) }); err != nil {
		t.Fatal(err)
	}
	if st := h.limitStatus("owner", "key:worker"); st.SpentToday != 100 {
		t.Fatalf("yesterday's refund changed today: %+v", st)
	}
	if n := h.count("SELECT spent FROM spend_limit_usage WHERE credential='key:worker' AND day=?", h.now/86400-1); n != 30 {
		t.Fatalf("yesterday's count after its refund: %d", n)
	}
}

func TestSpendLimitPerCallAndExpiry(t *testing.T) {
	h := newHarness(t, anonCreditParams())
	key := limited("owner", "token:abc")
	h.setLimit("owner", "token:abc", nil, ptr(40), h.now+600)
	_, err := h.reserveCredit(key, 41, "a")
	if which, retry := spendLimitOf(err); which != "per_call" || retry != 0 {
		t.Fatalf("over the per-call ceiling: %v", err)
	}
	if err = h.spendCredit(key, 41); err == nil {
		t.Fatal("an immediate spend over the per-call ceiling went through")
	}
	for range 3 {
		if err = h.spendCredit(key, 40); err != nil {
			t.Fatalf("within the ceiling, no daily limit: %v", err)
		}
	}
	// Post bytes are not credit: the limit does not apply.
	if err = h.spend(key, 500); err != nil {
		t.Fatal(err)
	}
	h.now += 600
	_, err = h.reserveCredit(key, 1, "late")
	if which, _ := spendLimitOf(err); which != "expired" {
		t.Fatalf("after expires_at: %v", err)
	}
	if limited, err := h.l.CredentialLimited(ctx, h.db, "token:abc"); err != nil || !limited {
		t.Fatalf("CredentialLimited: %v %v", limited, err)
	}
	// Lifting the limit keeps the row, with nothing limited.
	h.setLimit("owner", "token:abc", nil, nil, 0)
	if limited, _ := h.l.CredentialLimited(ctx, h.db, "token:abc"); limited || h.count("SELECT count(*) FROM spend_limits") != 1 {
		t.Fatal("lifting the limit")
	}
	if err = h.spendCredit(key, 500); err != nil {
		t.Fatal(err)
	}
}

func TestSpendLimitTransfers(t *testing.T) {
	p := anonCreditParams()
	p.Resources[allowance.Credit].TransferFee = 5
	h := newHarness(t, p)
	key := limited("owner", "key:worker")
	h.setLimit("owner", "key:worker", ptr(100), nil, 0)
	transfer := func(amount int64, k string) (Transfer, error) {
		var t Transfer
		err := h.do(func(q *sql.Tx) error {
			var err error
			t, err = h.l.Transfer(ctx, q, key, "friend", allowance.Credit, amount, k, h.now)
			return err
		})
		return t, err
	}
	if _, err := transfer(96, "a"); err == nil {
		t.Fatal("a transfer whose amount and fee pass the limit went through")
	}
	if _, err := transfer(45, "b"); err != nil {
		t.Fatal(err)
	}
	if st := h.limitStatus("owner", "key:worker"); st.SpentToday != 50 {
		t.Fatalf("a transfer counts its amount and fee: %+v", st)
	}
	// A pending transfer (breaker on) that is cancelled gives its amount back.
	if err := h.do(func(q *sql.Tx) error { return h.l.TripBreaker(ctx, q, "owner", "agent.rotate", "", h.now) }); err != nil {
		t.Fatal(err)
	}
	tr, err := transfer(20, "c")
	if err != nil || tr.State != "pending" {
		t.Fatalf("pending transfer: %+v %v", tr, err)
	}
	if st := h.limitStatus("owner", "key:worker"); st.SpentToday != 75 {
		t.Fatalf("pending: %+v", st)
	}
	if err = h.do(func(q *sql.Tx) error { _, err := h.l.CancelTransfer(ctx, q, tr.ID, "key-owner", h.now); return err }); err != nil {
		t.Fatal(err)
	}
	if st := h.limitStatus("owner", "key:worker"); st.SpentToday != 55 {
		t.Fatalf("cancelled: the amount comes back, the fee stays: %+v", st)
	}
}

// Two reserves through one credential race; the database has one
// connection, so each runs in its own transaction one after the other, and
// the second reads the first's count: exactly one fits the limit.
func TestSpendLimitConcurrentReserves(t *testing.T) {
	h := newHarness(t, anonCreditParams())
	key := limited("owner", "key:worker")
	h.setLimit("owner", "key:worker", ptr(100), nil, 0)
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tx, err := h.db.BeginTx(ctx, nil)
			if err != nil {
				errs[i] = err
				return
			}
			defer tx.Rollback()
			if _, err = h.l.Reserve(ctx, tx, key, allowance.Credit, 60, fmt.Sprint("race-", i), Ref{Service: "fetch"}, 60, h.now); err != nil {
				errs[i] = err
				return
			}
			errs[i] = tx.Commit()
		}()
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
		} else if which, _ := spendLimitOf(err); which != "per_day" {
			t.Fatalf("unexpected: %v", err)
		}
	}
	if st := h.limitStatus("owner", "key:worker"); ok != 1 || st.SpentToday != 60 {
		t.Fatalf("%d reserves went through, %d counted; want 1 and 60", ok, st.SpentToday)
	}
}
