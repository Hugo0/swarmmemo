package ledger

// Security review 1.20 regression tests. Each one inverts a proof of concept
// (TestSecPoC_*, branch security-review-1.20): it fails while the weakness is
// present.

import (
	"database/sql"
	"fmt"
	"testing"

	"swarmmemo/internal/allowance"
)

// H3: 64 dust transfers from 64 throwaway keys no longer hide the victim's
// own share from the spend planner or fill its lot slots: incoming lots from
// other accounts merge, and the planner sees every live lot.
func TestSec120_DustTransfersCannotBlockARecipient(t *testing.T) {
	p := smallParams()
	rp := p.Resources[allowance.PostBytes]
	rp.Budget, rp.SpendCeiling, rp.InboundCap = 100_000, 200_000, 100_000
	rp.Cap = []int64{40_000, 30_000, 20_000, 10_000}
	rp.RootCap = []int64{100_000, 100_000, 100_000, 100_000}
	rp.Floor = []int64{0, 0, 10_000, 0}
	rp.GrantSharePPM = 100_000
	h := newHarness(t, p)
	victim := signedSubject("victim")
	for i := 0; i < LotsPerAccount; i++ {
		att := signedSubject(fmt.Sprintf("att%02d", i))
		if _, err := h.transfer(att, "victim", 1, "dust"); err != nil {
			t.Fatalf("attacker %d: %v", i, err)
		}
	}
	var lots int
	if err := h.db.QueryRow("SELECT count(*) FROM ledger_lots WHERE account='victim' AND state='live'").Scan(&lots); err != nil || lots > 4 {
		t.Fatalf("the dust fills %d lot slots (%v)", lots, err)
	}
	// The journal still names every sender.
	var senders int
	if err := h.db.QueryRow("SELECT count(DISTINCT counterparty) FROM ledger_entries WHERE kind='transfer_in' AND account='victim'").Scan(&senders); err != nil || senders != LotsPerAccount {
		t.Fatalf("senders in the journal: %d %v", senders, err)
	}
	if err := h.spend(victim, 600); err != nil {
		t.Fatalf("the victim cannot post after the dust: %v", err)
	}
	// An operator grant still lands.
	if err := h.do(func(q *sql.Tx) error {
		return h.l.Mint(ctx, q, "victim", allowance.PostBytes, allowance.Granted, 500, "grant", h.now)
	}); err != nil {
		t.Fatalf("grant refused: %v", err)
	}
	h.now += 12 * 3600
	if err := h.spend(victim, 600); err != nil {
		t.Fatalf("later in the day: %v", err)
	}
	// Balance and spend agree: the whole share and the dust are spendable.
	b := h.balance(victim)
	if err := h.spend(victim, b.Remaining); err != nil {
		t.Fatalf("spending the balance shown (%d): %v", b.Remaining, err)
	}
}

// M7: the rotated-away key may cancel a pending transfer until the transfer
// executes, not only while the breaker lasts: a thief who rotates the stolen
// key and transfers just before the breaker ends no longer leaves the owner
// a window of seconds.
func TestSec120_RotatedKeyMayCancelUntilExecution(t *testing.T) {
	p := smallParams()
	p.Resources[allowance.PostBytes].GrantSharePPM = 500_000
	h := newHarness(t, p)
	rotation := h.now
	if err := h.do(func(q *sql.Tx) error {
		return h.l.TripBreaker(ctx, q, "victim", "agent.rotate", "victim-old-key", rotation)
	}); err != nil {
		t.Fatal(err)
	}
	h.now = rotation + 48*3600 - 60
	if err := h.do(func(q *sql.Tx) error {
		return h.l.Mint(ctx, q, "victim", allowance.PostBytes, allowance.Granted, 300, "grant", h.now)
	}); err != nil {
		t.Fatal(err)
	}
	tr, err := h.transfer(signedSubject("victim"), "thief", 250, "steal")
	if err != nil || tr.State != "pending" || tr.ExecuteAt != h.now+48*3600 {
		t.Fatalf("%+v %v", tr, err)
	}
	for _, at := range []int64{h.now + 120, h.now + 24*3600, tr.ExecuteAt - 1} {
		ok, err := h.l.RotatedKeyMayCancel(ctx, h.db, tr.ID, "victim-old-key", at)
		if err != nil || !ok {
			t.Fatalf("the rotated-away key may not cancel at +%d s: %v", at-tr.CreatedAt, err)
		}
	}
	if ok, _ := h.l.RotatedKeyMayCancel(ctx, h.db, tr.ID, "victim-old-key", tr.ExecuteAt); ok {
		t.Fatal("cancel allowed at execution time")
	}
	if ok, _ := h.l.RotatedKeyMayCancel(ctx, h.db, tr.ID, "someone-else", h.now+120); ok {
		t.Fatal("another key may cancel")
	}
	var cancelled Transfer
	if err = h.do(func(q *sql.Tx) error {
		cancelled, err = h.l.CancelTransfer(ctx, q, tr.ID, "victim-old-key", h.now+24*3600)
		return err
	}); err != nil || cancelled.State != "cancelled" {
		t.Fatalf("cancel a day in: %+v %v", cancelled, err)
	}
}

// H3: however the lots arrived, a spend plans over all of them: an account
// with LotsPerAccount lots ahead of its claim lot still spends its share.
func TestSec120_PlannerSeesTheClaimLotBehindAFullWindow(t *testing.T) {
	p := smallParams()
	p.Resources[allowance.PostBytes].GrantSharePPM = 500_000
	h := newHarness(t, p)
	// LotsPerAccount one-unit granted lots on distinct past issue days (they
	// never merge), all sorting ahead of today's claim.
	for i := 0; i < LotsPerAccount; i++ {
		if _, err := h.db.Exec("INSERT INTO ledger_lots(account,resource,bucket,origin_tier,origin_account,hops,issued_day,expires_at,half_life_days,decayed_day,initial,remaining,held,state,created_at) VALUES('alice',?,'free',1,'alice',0,?,?,0,?,1,1,0,'live',?)",
			string(allowance.PostBytes), h.now/86400-int64(i+1), h.now+60, h.now/86400, h.now); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.spend(signedSubject("alice"), 150); err != nil {
		t.Fatalf("the claim lot is out of the planner's sight: %v", err)
	}
}
