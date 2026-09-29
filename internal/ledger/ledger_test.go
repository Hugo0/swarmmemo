package ledger

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"swarmmemo/internal/allowance"
)

func TestFirstSpendMaterialisesTheShare(t *testing.T) {
	h := newHarness(t, smallParams())
	alice := signedSubject("alice")
	before := h.balance(alice)
	if !before.Prospective || before.Entitlement != 200 || before.Remaining != 200 {
		t.Fatalf("prospective balance %+v", before)
	}
	if n := h.count("SELECT count(*) FROM allowance_days") + h.count("SELECT count(*) FROM allowance_claims") + h.count("SELECT count(*) FROM ledger_entries"); n != 0 {
		t.Fatalf("a read wrote %d rows", n)
	}
	if err := h.spend(alice, 50); err != nil {
		t.Fatal(err)
	}
	after := h.balance(alice)
	if after.Prospective || after.Entitlement != 200 || after.Remaining != 150 || after.Used != 50 || after.Standing.Tier != allowance.TierSigned {
		t.Fatalf("balance after first spend %+v", after)
	}
	if err := h.spend(alice, 150); err != nil {
		t.Fatal(err)
	}
	if n := h.count("SELECT count(*) FROM allowance_claims WHERE subject='alice'"); n != 1 {
		t.Fatalf("%d claims for one subject", n)
	}
	if got := code(h.spend(alice, 1)); got != "quota_exhausted" {
		t.Fatalf("over the share: %s", got)
	}
	// A refused spend claims nothing new and spends nothing.
	if b := h.balance(alice); b.Used != 200 || b.Remaining != 0 {
		t.Fatalf("refusal changed the balance: %+v", b)
	}
	checkInvariants(t, h.db, h.p)
}

func TestFailedCommandNeverClaimsTwice(t *testing.T) {
	h := newHarness(t, smallParams())
	bob := signedSubject("bob")
	// The command fails after the spend: its rollback undoes the claim.
	err := h.do(func(q *sql.Tx) error {
		if _, err := h.l.Spend(ctx, q, bob, allowance.PostBytes, 10, Ref{Service: "board", Op: "post"}, h.now); err != nil {
			return err
		}
		return errors.New("later failure")
	})
	if err == nil || h.count("SELECT count(*) FROM allowance_claims") != 0 || h.count("SELECT count(*) FROM allowance_days") != 0 {
		t.Fatal("a failed command left a claim or an opened day")
	}
	for i := 0; i < 3; i++ {
		if err := h.spend(bob, 10); err != nil {
			t.Fatal(err)
		}
	}
	if n := h.count("SELECT count(*) FROM allowance_claims"); n != 1 {
		t.Fatalf("%d claims", n)
	}
	checkInvariants(t, h.db, h.p)
}

func TestDayTotalNeverExceedsTheBudget(t *testing.T) {
	h := newHarness(t, smallParams())
	for i := 0; i < 40; i++ {
		s := signedSubject(fmt.Sprintf("s%d", i))
		if i%3 == 0 {
			s = anonSubject(fmt.Sprintf("a%d", i))
		}
		err := h.spend(s, 1)
		if err != nil && code(err) != "global_quota_exhausted" {
			t.Fatalf("subject %d: %v", i, err)
		}
	}
	// Overbooking: 40 entitlements add up to far more than the budget, and
	// only the 40 bytes spent were drawn from the pools.
	var entitled int64
	if err := h.db.QueryRow("SELECT sum(granted) FROM allowance_claims").Scan(&entitled); err != nil {
		t.Fatal(err)
	}
	d, _, _ := loadDay(ctx, h.db, allowance.PostBytes, day0)
	if entitled <= 1000 || d.SpentNonpaid != 40 {
		t.Fatalf("entitled %d, spent %d", entitled, d.SpentNonpaid)
	}
	// Now everyone spends what they can: the day's spend stops at the water
	// below tiers 1–2, whose reserves (100 + 100) stay untouched.
	for i := 0; i < 40; i++ {
		s := signedSubject(fmt.Sprintf("s%d", i))
		if i%3 == 0 {
			s = anonSubject(fmt.Sprintf("a%d", i))
		}
		for _, n := range []int64{100, 10, 1} {
			for h.spend(s, n) == nil {
			}
		}
	}
	d, _, _ = loadDay(ctx, h.db, allowance.PostBytes, day0)
	if d.SpentNonpaid != 800 || d.Pools[1].avail() != 100 || d.Pools[2].avail() != 100 {
		t.Fatalf("spent %d of a 1000 budget with 200 reserved above: %+v", d.SpentNonpaid, d.Pools)
	}
	checkInvariants(t, h.db, h.p)
}

func TestFloodAtMidnightLeavesHigherTiersTheirReserves(t *testing.T) {
	p := smallParams()
	p.Resources[allowance.PostBytes].Floor = []int64{0, 0, 10, 10}
	h := newHarness(t, p)
	for i := 0; i < 300; i++ {
		s := signedSubject(fmt.Sprintf("fresh%d", i))
		if i%2 == 0 {
			s = anonSubject(fmt.Sprintf("flood%d", i))
		}
		_ = h.spend(s, 10)
	}
	// Tiers 3–4 are exhausted: a late tier-3 subject is still entitled to
	// its floor, but its spend finds no water.
	if got := code(h.spend(signedSubject("late"), 1)); got != "global_quota_exhausted" {
		t.Fatalf("a late tier-3 subject: %s", got)
	}
	// A trusted and a proven subject draw their reserves in full.
	for _, s := range []allowance.Subject{signedSubject("t1-trusted"), signedSubject("t2-proven")} {
		if err := h.spend(s, 100); err != nil {
			t.Fatalf("%s: %v", s.ID, err)
		}
	}
	checkInvariants(t, h.db, h.p)
}

func TestTrustedBorrowsFromLowerTiersNeverTheReverse(t *testing.T) {
	p := smallParams()
	p.Resources[allowance.PostBytes].Floor = []int64{80, 0, 0, 0}
	h := newHarness(t, p)
	// The first trusted subject's share is tier 1's whole pool (100).
	if err := h.spend(signedSubject("t1-a"), 100); err != nil {
		t.Fatal(err)
	}
	// The second's share is half the pool, raised to the floor (80); tier 1
	// has no water left, so all 80 are borrowed from tier 4, the lowest.
	if err := h.spend(signedSubject("t1-b"), 80); err != nil {
		t.Fatal(err)
	}
	d, _, _ := loadDay(ctx, h.db, allowance.PostBytes, day0)
	if d.Pools[1].Claimed != 100 || d.Pools[1].Borrowed != 80 || d.Pools[4].Lent != 80 {
		t.Fatalf("pools %+v", d.Pools)
	}
	// An anonymous subject cannot reach tiers 1–3's water.
	anon := anonSubject("x")
	if b := h.balance(anon); b.Entitlement != 100 {
		t.Fatalf("anon share %d", b.Entitlement)
	}
	checkInvariants(t, h.db, h.p)
}

func TestSpillHandsReservesDown(t *testing.T) {
	h := newHarness(t, smallParams())
	late := signedSubject("t3-late")
	h.now = day0*86400 + 12*3600
	// At noon tiers 1–2 kept half their reserve; tier 3 holds 200 + 100.
	if err := h.spend(late, 1); err != nil {
		t.Fatal(err)
	}
	d, _, _ := loadDay(ctx, h.db, allowance.PostBytes, day0)
	if d.Pools[1].SpillOut != 50 || d.Pools[2].SpillOut != 100 || d.Pools[3].SpillIn != 100 {
		t.Fatalf("spill %+v", d.Pools)
	}
	if n := h.count("SELECT count(*) FROM ledger_entries WHERE kind='spill'"); n != 3 {
		t.Fatalf("%d spill lines", n)
	}
	checkInvariants(t, h.db, h.p)
}

func TestPaidNeverDecaysGrantedDoes(t *testing.T) {
	p := smallParams()
	p.Resources[allowance.PostBytes].GrantSharePPM = 500_000
	h := newHarness(t, p)
	for _, b := range []allowance.Bucket{allowance.Paid, allowance.Granted} {
		if err := h.do(func(q *sql.Tx) error {
			return h.l.Mint(ctx, q, "carol", allowance.PostBytes, b, 400, "test "+string(b), h.now)
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Grants come from the tier-0 pool: 500 of it, 400 used.
	if err := h.do(func(q *sql.Tx) error {
		return h.l.Mint(ctx, q, "carol", allowance.PostBytes, allowance.Granted, 200, "over the pool", h.now)
	}); code(err) != "global_quota_exhausted" {
		t.Fatalf("grant beyond the pool: %v", err)
	}
	for day := 1; day <= 120; day += 7 {
		h.now = (day0 + int64(day)) * 86400
		h.sweep()
	}
	var paid, granted int64
	h.db.QueryRow("SELECT coalesce(sum(remaining),0) FROM ledger_lots WHERE bucket='paid'").Scan(&paid)
	h.db.QueryRow("SELECT coalesce(sum(remaining),0) FROM ledger_lots WHERE bucket='granted'").Scan(&granted)
	if paid != 400 {
		t.Fatalf("paid credit decayed to %d", paid)
	}
	if granted != 0 {
		t.Fatalf("granted credit still %d after 8 half-lives", granted)
	}
	checkInvariants(t, h.db, h.p)
}

func TestGrantedDemurrageIsIntegerAndDaily(t *testing.T) {
	p := smallParams()
	p.Resources[allowance.PostBytes].GrantSharePPM = ppm
	h := newHarness(t, p)
	if err := h.do(func(q *sql.Tx) error {
		return h.l.Mint(ctx, q, "dan", allowance.PostBytes, allowance.Granted, 1000, "grant", h.now)
	}); err != nil {
		t.Fatal(err)
	}
	h.now += 14 * 86400
	h.sweep()
	var rem int64
	h.db.QueryRow("SELECT remaining FROM ledger_lots WHERE account='dan'").Scan(&rem)
	// Fourteen daily steps of floor(r × 48323 / 1e6) from 1000: 504.
	want := int64(1000)
	for i := 0; i < 14; i++ {
		want -= want * RatePPM(14) / ppm
	}
	if rem != want || rem < 490 || rem > 510 {
		t.Fatalf("after one half-life %d, want %d", rem, want)
	}
	checkInvariants(t, h.db, h.p)
}

func TestTransferKeepsExpiryNoBanking(t *testing.T) {
	h := newHarness(t, smallParams())
	alice, bob := signedSubject("alice"), signedSubject("bob")
	tr, err := h.transfer(alice, "bob", 100, "k1")
	if err != nil {
		t.Fatal(err)
	}
	if tr.State != "done" || tr.ExpiresAt != (day0+1)*86400 || tr.ByBucket[allowance.Free] != 100 {
		t.Fatalf("transfer %+v", tr)
	}
	// The same request key is the same transfer.
	again, err := h.transfer(alice, "bob", 100, "k1")
	if err != nil || again.ID != tr.ID || h.count("SELECT count(*) FROM ledger_transfers") != 1 {
		t.Fatalf("retry made another transfer: %v", err)
	}
	var hops, origin int64
	var originAccount string
	h.db.QueryRow("SELECT hops,origin_tier,origin_account FROM ledger_lots WHERE account='bob'").Scan(&hops, &origin, &originAccount)
	if hops != 1 || origin != 3 || originAccount != "alice" {
		t.Fatalf("taint lost: hops %d origin tier %d account %s", hops, origin, originAccount)
	}
	if b := h.balance(bob); b.Incoming != 100 || b.Remaining != b.Entitlement+100 || !b.Prospective {
		t.Fatalf("recipient %+v", b)
	}
	// Bob forwards the units to carol: still today's water.
	if err = h.spend(bob, 1); err != nil {
		t.Fatal(err)
	}
	if _, err = h.transfer(bob, "carol", 150, "k2"); err != nil {
		t.Fatal(err)
	}
	h.now = (day0 + 1) * 86400
	if b := h.balance(signedSubject("carol")); b.Remaining != b.Entitlement {
		t.Fatalf("forwarded free units survived midnight: %+v", b)
	}
	h.sweep()
	if n := h.count("SELECT count(*) FROM ledger_lots WHERE state='live' AND bucket='free' AND expires_at<=?", h.now); n != 0 {
		t.Fatalf("%d free lots live past midnight", n)
	}
	checkInvariants(t, h.db, h.p)
}

func TestTransferRefusals(t *testing.T) {
	p := smallParams()
	p.Resources[allowance.PostBytes].InboundCap = 150
	h := newHarness(t, p)
	alice := signedSubject("alice")
	cases := []struct {
		to     string
		amount int64
		want   string
	}{
		{"alice", 10, "self_transfer"},
		{"bob", 0, "invalid_amount"},
		{"bob", 1001, "invalid_amount"},
		{"bob", 191, "quota_exhausted"}, // 200 less the 10 fee
		{"bob", 151, "recipient_limit"},
	}
	for i, c := range cases {
		if _, err := h.transfer(alice, c.to, c.amount, fmt.Sprint(i)); code(err) != c.want {
			t.Fatalf("%+v: %v", c, err)
		}
	}
	// Refusals spend nothing: the fee went with the rollback.
	if b := h.balance(alice); b.Used != 0 {
		t.Fatalf("a refused transfer spent %d", b.Used)
	}
	if _, err := h.transfer(alice, "bob", 150, "ok"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.transfer(signedSubject("carol"), "bob", 1, "over"); code(err) != "recipient_limit" {
		t.Fatalf("inbound cap: %v", err)
	}
	h.levers.lv.FreezeTransfers = true
	if _, err := h.transfer(alice, "dan", 1, "frozen"); code(err) != "transfers_frozen" {
		t.Fatalf("frozen: %v", err)
	}
	h.levers.lv.FreezeTransfers = false
	// A narrowed parameter version: free units of tier 3 no longer move.
	h.p.TransferableFreeTiers = []int64{1, 2}
	if _, err := h.transfer(alice, "dan", 10, "narrow"); code(err) != "not_transferable" {
		t.Fatalf("narrowed: %v", err)
	}
	checkInvariants(t, h.db, h.p)
}

func TestBreakerHoldsTransfersAndRotatedKeyCancels(t *testing.T) {
	h := newHarness(t, smallParams())
	alice := signedSubject("alice")
	if err := h.do(func(q *sql.Tx) error { return h.l.TripBreaker(ctx, q, "alice", "agent.rotate", "old-key", h.now) }); err != nil {
		t.Fatal(err)
	}
	tr, err := h.transfer(alice, "thief", 100, "steal")
	if err != nil {
		t.Fatal(err)
	}
	if tr.State != "pending" || tr.ExecuteAt != h.now+48*3600 {
		t.Fatalf("transfer under an active breaker %+v", tr)
	}
	// The units are held, not moved.
	if b := h.balance(alice); b.Remaining != 90 {
		t.Fatalf("sender balance while pending %+v", b)
	}
	if b := h.balance(signedSubject("thief")); b.Incoming != 100 || b.Remaining != b.Entitlement {
		t.Fatalf("recipient got units early: %+v", b)
	}
	may := func(key string, now int64) bool {
		ok, err := h.l.RotatedKeyMayCancel(ctx, h.db, tr.ID, key, now)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !may("old-key", h.now+3600) {
		t.Fatal("the rotated-away key cannot cancel")
	}
	if may("other-key", h.now) || may("old-key", h.now+48*3600) {
		t.Fatal("another key, or the old key after the window, may cancel")
	}
	var cancelled Transfer
	if err = h.do(func(q *sql.Tx) error {
		cancelled, err = h.l.CancelTransfer(ctx, q, tr.ID, "old-key", h.now+3600)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if cancelled.State != "cancelled" {
		t.Fatalf("cancel %+v", cancelled)
	}
	if b := h.balance(alice); b.Remaining != 190 || b.Outgoing != 0 {
		t.Fatalf("cancel did not return the units: %+v", b)
	}
	if err = h.do(func(q *sql.Tx) error { _, err := h.l.CancelTransfer(ctx, q, tr.ID, "old-key", h.now); return err }); code(err) != "transfer_not_pending" {
		t.Fatalf("second cancel: %v", err)
	}
	if err = h.do(func(q *sql.Tx) error { _, err := h.l.CancelTransfer(ctx, q, "nope", "k", h.now); return err }); code(err) != "transfer_not_found" {
		t.Fatalf("unknown transfer: %v", err)
	}
	checkInvariants(t, h.db, h.p)
}

func TestPendingTransferExecutesAfterTheDelay(t *testing.T) {
	p := smallParams()
	p.Resources[allowance.PostBytes].GrantSharePPM = 500_000
	h := newHarness(t, p)
	alice := signedSubject("alice")
	if err := h.do(func(q *sql.Tx) error {
		return h.l.Mint(ctx, q, "alice", allowance.PostBytes, allowance.Paid, 300, "funding test", h.now)
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.do(func(q *sql.Tx) error { return h.l.TripBreaker(ctx, q, "alice", "identity.link", "", h.now) }); err != nil {
		t.Fatal(err)
	}
	// 190 free (after the fee) then 110 paid are held.
	tr, err := h.transfer(alice, "bob", 300, "big")
	if err != nil || tr.State != "pending" || tr.ByBucket[allowance.Free] != 190 || tr.ByBucket[allowance.Paid] != 110 {
		t.Fatalf("%+v %v", tr, err)
	}
	h.now += 48 * 3600
	h.sweep()
	var state string
	h.db.QueryRow("SELECT state FROM ledger_transfers").Scan(&state)
	if state != "done" {
		t.Fatalf("transfer %s after the delay", state)
	}
	// The held free units expired with their day; the paid units moved.
	var bobPaid, bobFree int64
	h.db.QueryRow("SELECT coalesce(sum(remaining),0) FROM ledger_lots WHERE account='bob' AND bucket='paid'").Scan(&bobPaid)
	h.db.QueryRow("SELECT coalesce(sum(remaining),0) FROM ledger_lots WHERE account='bob' AND bucket='free'").Scan(&bobFree)
	if bobPaid != 110 || bobFree != 0 {
		t.Fatalf("bob got paid %d free %d", bobPaid, bobFree)
	}
	// After the breaker window transfers are immediate again.
	if tr, err = h.transfer(alice, "bob", 10, "later"); err != nil || tr.State != "done" {
		t.Fatalf("%+v %v", tr, err)
	}
	checkInvariants(t, h.db, h.p)
}

func TestSpikeAndDormancyTripTheBreaker(t *testing.T) {
	p := smallParams()
	p.SpikeFloor = 2
	h := newHarness(t, p)
	alice := signedSubject("alice")
	for i := 0; i < 2; i++ {
		if tr, err := h.transfer(alice, fmt.Sprint("r", i), 5, fmt.Sprint(i)); err != nil || tr.State != "done" {
			t.Fatalf("%+v %v", tr, err)
		}
	}
	if tr, err := h.transfer(alice, "r9", 5, "third"); err != nil || tr.State != "pending" {
		t.Fatalf("third transfer of the day: %+v %v", tr, err)
	}
	// Dormancy: a claim after more than dormant_days without one.
	dave := signedSubject("dave")
	if err := h.spend(dave, 1); err != nil {
		t.Fatal(err)
	}
	h.now += 31 * 86400
	if err := h.spend(dave, 1); err != nil {
		t.Fatal(err)
	}
	if n := h.count("SELECT count(*) FROM account_breakers WHERE account='dave' AND reason='dormant'"); n != 1 {
		t.Fatalf("dormant breakers: %d", n)
	}
	checkInvariants(t, h.db, h.p)
}

func TestHoldsReserveCommitRefund(t *testing.T) {
	h := newHarness(t, smallParams())
	alice := signedSubject("alice")
	ref := Ref{Service: "echo", Op: "service.call", Method: "echo"}
	reserve := func(key string, max int64) (Hold, error) {
		var hold Hold
		err := h.do(func(q *sql.Tx) error {
			var err error
			hold, err = h.l.Reserve(ctx, q, alice, allowance.PostBytes, max, key, ref, 60, h.now)
			return err
		})
		return hold, err
	}
	h1, err := reserve("a", 50)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reserve("a", 50); code(err) != "request_in_flight" {
		t.Fatalf("retry while held: %v", err)
	}
	if b := h.balance(alice); b.Remaining != 150 || b.Used != 50 {
		t.Fatalf("while held %+v", b)
	}
	var rc Receipt
	if err = h.do(func(q *sql.Tx) error { rc, err = h.l.Commit(ctx, q, h1.ID, 20, h.now); return err }); err != nil || rc.Used != 20 || rc.Refunded != 30 {
		t.Fatalf("commit %+v %v", rc, err)
	}
	if b := h.balance(alice); b.Remaining != 180 || b.Used != 20 {
		t.Fatalf("after commit %+v", b)
	}
	// Settled: a retry returns the hold as it ended, and a second commit the
	// first receipt, each with hold_not_held: they settle nothing.
	var again Hold
	var commitErr error
	h.do(func(q *sql.Tx) error {
		again, err = h.l.Reserve(ctx, q, alice, allowance.PostBytes, 50, "a", ref, 60, h.now)
		if code(err) != "hold_not_held" {
			t.Fatalf("retry after commit: %v", err)
		}
		rc, commitErr = h.l.Commit(ctx, q, h1.ID, 50, h.now)
		return nil
	})
	if again.State != "committed" || code(commitErr) != "hold_not_held" || rc.Used != 20 {
		t.Fatalf("settled hold %+v %+v %v", again, rc, commitErr)
	}
	h2, _ := reserve("b", 30)
	if err = h.do(func(q *sql.Tx) error { return h.l.Refund(ctx, q, h2.ID, "upstream failed", h.now) }); err != nil {
		t.Fatal(err)
	}
	if b := h.balance(alice); b.Remaining != 180 {
		t.Fatalf("after refund %+v", b)
	}
	// Two open holds per account.
	reserve("c", 1)
	reserve("d", 1)
	if _, err = reserve("e", 1); code(err) != "hold_limit" {
		t.Fatalf("third open hold: %v", err)
	}
	// A crash: the hold expires and settles at its maximum.
	h.now += 61
	h.sweep()
	if n := h.count("SELECT count(*) FROM ledger_holds WHERE state='expired' AND used_units=max_units"); n != 2 {
		t.Fatalf("%d expired holds settled at max", n)
	}
	if n := h.count("SELECT count(*) FROM ledger_entries WHERE kind='commit' AND detail='unknown'"); n != 2 {
		t.Fatalf("%d unknown-outcome lines", n)
	}
	if err = h.do(func(q *sql.Tx) error { _, err = h.l.Commit(ctx, q, h1.ID, -1, h.now); return err }); code(err) != "hold_not_held" {
		t.Fatalf("commit of a settled hold: %v", err)
	}
	if err = h.do(func(q *sql.Tx) error { return h.l.Refund(ctx, q, h1.ID, "late", h.now) }); code(err) != "hold_not_held" {
		t.Fatalf("refund of a settled hold: %v", err)
	}
	checkInvariants(t, h.db, h.p)
}

func TestHoldRefundToAnExpiredLotExpires(t *testing.T) {
	p := smallParams()
	p.Resources[allowance.PostBytes].SpillStart = 86399 // keep tier 3's water until the end
	h := newHarness(t, p)
	alice := signedSubject("alice")
	var hold Hold
	if err := h.do(func(q *sql.Tx) error {
		var err error
		hold, err = h.l.Reserve(ctx, q, alice, allowance.PostBytes, 100, "late", Ref{Service: "echo"}, 86400, h.now+86000)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	h.now = (day0+1)*86400 + 10
	h.sweep() // the lot's unheld 100 expire; the held 100 wait for the hold
	if err := h.do(func(q *sql.Tx) error { _, err := h.l.Commit(ctx, q, hold.ID, 40, h.now); return err }); err != nil {
		t.Fatal(err)
	}
	if n := h.count("SELECT coalesce(sum(amount),0) FROM ledger_entries WHERE kind='expire'"); n != 160 {
		t.Fatalf("expired %d, want 100 swept and 60 refunded into an expired lot", n)
	}
	if n := h.count("SELECT count(*) FROM ledger_lots WHERE state='live'"); n != 0 {
		t.Fatalf("%d lots live", n)
	}
	checkInvariants(t, h.db, h.p)
}

func TestLeversSignedOnlyAndCutBudget(t *testing.T) {
	h := newHarness(t, smallParams())
	anon := anonSubject("a")
	h.levers.lv.SignedOnly = true
	if got := code(h.spend(anon, 1)); got != "quota_exhausted" {
		t.Fatalf("anon under signed-only: %s", got)
	}
	if err := h.spend(signedSubject("s0"), 1); err != nil {
		t.Fatal(err)
	}
	d, _, _ := loadDay(ctx, h.db, allowance.PostBytes, day0)
	if d.Pools[4].Size != 0 || d.Unallocated != 600 {
		t.Fatalf("tier 4 under signed-only %+v", d)
	}
	h.levers.lv.SignedOnly = false
	h.levers.lv.BudgetCutPPM = map[allowance.Resource]int64{allowance.PostBytes: 900_000}
	// A newcomer finds no water left under the cut, and so does s0: its
	// entitlement stands, but tier 3's unspent water is gone.
	if got := code(h.spend(signedSubject("s"), 1)); got != "global_quota_exhausted" {
		t.Fatalf("newcomer under the cut: %s", got)
	}
	if got := code(h.spend(signedSubject("s0"), 1)); got != "global_quota_exhausted" {
		t.Fatalf("s0 under the cut: %s", got)
	}
	// A trusted subject still draws what the cut left its tier (refusals roll
	// back, so this spend is what stores the adjusted day).
	if err := h.spend(signedSubject("t1-x"), 1); err != nil {
		t.Fatal(err)
	}
	d, _, _ = loadDay(ctx, h.db, allowance.PostBytes, day0)
	// Spent water is untouched (s0's 1 stays); the pools shrink bottom-up to
	// the cut budget.
	if d.BudgetEffective != 100 || d.allocated() != 100 || d.Pools[3].Size != 1 || d.Pools[3].avail() != 0 || d.Pools[4].Size != 0 {
		t.Fatalf("cut budget %+v", d)
	}
	h.levers.lv.BudgetCutPPM = nil
	if err := h.spend(signedSubject("s"), 1); err != nil {
		t.Fatal(err)
	}
	d, _, _ = loadDay(ctx, h.db, allowance.PostBytes, day0)
	if d.BudgetEffective != 1000 || d.allocated() != 1000 {
		t.Fatalf("released %+v", d)
	}
	if n := h.count("SELECT count(*) FROM ledger_entries WHERE kind='lever'"); n < 2 {
		t.Fatalf("%d lever lines", n)
	}
	checkInvariants(t, h.db, h.p)
}

func TestClientSignatureOnlyNarrows(t *testing.T) {
	p := smallParams()
	p.Resources[allowance.PostBytes].ClientSharePPM = 500_000
	h := newHarness(t, p)
	curl := allowance.Subject{ID: "anon:net", Client: "c1"}
	wget := allowance.Subject{ID: "anon:net", Client: "c2"}
	if err := h.spend(curl, 50); err != nil {
		t.Fatal(err)
	}
	if got := code(h.spend(curl, 1)); got != "quota_exhausted" {
		t.Fatalf("one client past its share: %s", got)
	}
	if err := h.spend(wget, 50); err != nil {
		t.Fatal(err)
	}
	// Both clients together never exceed the prefix's one entitlement.
	if got := code(h.spend(allowance.Subject{ID: "anon:net", Client: "c3"}, 1)); got != "quota_exhausted" {
		t.Fatalf("third client: %s", got)
	}
	checkInvariants(t, h.db, h.p)
}

func TestJournalPublicRules(t *testing.T) {
	h := newHarness(t, smallParams())
	h.spend(anonSubject("x"), 5)
	h.spend(signedSubject("alice"), 5)
	entries, next, err := h.l.Journal(ctx, h.db, JournalQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Account == "anon:x" {
			t.Fatal("an anonymous subject's line is public")
		}
	}
	if len(entries) != 2 || next != 0 || entries[0].Seq < entries[1].Seq {
		t.Fatalf("journal %+v next %d", entries, next)
	}
	if anon, _, _ := h.l.Journal(ctx, h.db, JournalQuery{Account: "anon:x"}); len(anon) != 0 {
		t.Fatal("anonymous journal by account")
	}
	if u, err := h.l.Usage(ctx, h.db, "anon:x", allowance.PostBytes, 7, h.now); err != nil || len(u) != 1 || u[0].Spent != 5 {
		t.Fatalf("anonymous daily totals %+v %v", u, err)
	}
	page, next, _ := h.l.Journal(ctx, h.db, JournalQuery{Account: "alice", Limit: 1})
	if len(page) != 1 || next != page[0].Seq {
		t.Fatalf("page %+v next %d", page, next)
	}
}

func TestParamsVersions(t *testing.T) {
	h := newHarness(t, smallParams())
	store := ParamsStore{}
	v, body, err := store.Params(ctx, h.db, AllowanceNamespace, h.now)
	if err != nil || v != 0 || string(body) != string(DefaultAllowanceParams().Marshal()) {
		t.Fatalf("compiled-in version %d %v", v, err)
	}
	next := DefaultAllowanceParams()
	next.Resources[allowance.PostBytes].TransferFee = 512
	if _, err = store.Set(ctx, h.db, AllowanceNamespace, next.Marshal(), "steward", "", h.now, h.now); err == nil {
		t.Fatal("a version without a reason")
	}
	if _, err = store.Set(ctx, h.db, AllowanceNamespace, []byte(`{"schema":1}`), "steward", "partial", h.now, h.now); err == nil {
		t.Fatal("a partial body")
	}
	if v, err = store.Set(ctx, h.db, AllowanceNamespace, next.Marshal(), "steward", "raise the fee", h.now+100, h.now); err != nil || v != 1 {
		t.Fatalf("set: %d %v", v, err)
	}
	if v, _, _ = store.Params(ctx, h.db, AllowanceNamespace, h.now); v != 0 {
		t.Fatal("a version before its effective time")
	}
	if v, _, _ = store.Params(ctx, h.db, AllowanceNamespace, h.now+100); v != 1 {
		t.Fatal("the new version is not in effect")
	}
	list, err := store.List(ctx, h.db, AllowanceNamespace)
	if err != nil || len(list) != 2 || !list[1].CompiledIn {
		t.Fatalf("list %+v %v", list, err)
	}
	if _, err = store.Get(ctx, h.db, AllowanceNamespace, 7, h.now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing version: %v", err)
	}
}

// Overbooking (Hugo, 2026-09-29): a share is an entitlement and only actual
// spend counts against the day's budget, so early claimants cannot reserve
// the whole day before anything is used.
func TestOverbookingEntitlementsDoNotReserveTheDay(t *testing.T) {
	p := smallParams()
	p.Resources[allowance.PostBytes].Floor = []int64{0, 0, 200, 100} // floor = cap, as the legacy rows
	h := newHarness(t, p)
	for i := 0; i < 16; i++ {
		if err := h.spend(signedSubject(fmt.Sprintf("k%d", i)), 1); err != nil {
			t.Fatalf("key %d: %v", i, err)
		}
	}
	var entitled int64
	if err := h.db.QueryRow("SELECT sum(entitlement) FROM allowance_claims").Scan(&entitled); err != nil {
		t.Fatal(err)
	}
	if entitled != 16*200 {
		t.Fatalf("entitled %d", entitled)
	}
	// A seventeenth key still finds water: 16 bytes were drawn, not 3,200.
	if err := h.spend(signedSubject("late"), 150); err != nil {
		t.Fatal(err)
	}
	// Spend stops at the water tier 3 can reach (its own 200 and tier 4's
	// 600); tiers 1–2 keep their reserves.
	for i := 0; i < 16; i++ {
		for h.spend(signedSubject(fmt.Sprintf("k%d", i)), 50) == nil {
		}
	}
	d, _, _ := loadDay(ctx, h.db, allowance.PostBytes, day0)
	if d.SpentNonpaid > 800 || d.SpentNonpaid < 750 || d.Pools[1].avail() != 100 || d.Pools[2].avail() != 100 {
		t.Fatalf("spent %d: %+v", d.SpentNonpaid, d.Pools)
	}
	if got := code(h.spend(signedSubject("t2-p"), 100)); got != "" {
		t.Fatalf("proven reserve: %s", got)
	}
	checkInvariants(t, h.db, h.p)
}

func TestHoldDrawsAndRefundReturnsWater(t *testing.T) {
	h := newHarness(t, smallParams())
	alice := signedSubject("alice")
	var hold Hold
	if err := h.do(func(q *sql.Tx) error {
		var err error
		hold, err = h.l.Reserve(ctx, q, alice, allowance.PostBytes, 100, "k", Ref{Service: "echo"}, 60, h.now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if d, _, _ := loadDay(ctx, h.db, allowance.PostBytes, day0); d.Pools[3].Claimed != 100 {
		t.Fatalf("a hold drew %+v", d.Pools[3])
	}
	checkInvariants(t, h.db, h.p)
	if err := h.do(func(q *sql.Tx) error { _, err := h.l.Commit(ctx, q, hold.ID, 40, h.now); return err }); err != nil {
		t.Fatal(err)
	}
	if d, _, _ := loadDay(ctx, h.db, allowance.PostBytes, day0); d.Pools[3].Claimed != 40 || d.SpentNonpaid != 40 {
		t.Fatalf("after commit %d: %+v", d.SpentNonpaid, d.Pools[3])
	}
	checkInvariants(t, h.db, h.p)
}

func TestLoweredCapAppliesToUnspentEntitlement(t *testing.T) {
	h := newHarness(t, smallParams())
	alice := signedSubject("alice")
	if err := h.spend(alice, 50); err != nil {
		t.Fatal(err)
	}
	h.p.Resources[allowance.PostBytes].Cap[2] = 60
	if got := code(h.spend(alice, 11)); got != "quota_exhausted" {
		t.Fatalf("over the lowered cap: %s", got)
	}
	if err := h.spend(alice, 10); err != nil {
		t.Fatal(err)
	}
	if b := h.balance(alice); b.Entitlement != 60 || b.Remaining != 0 {
		t.Fatalf("balance under the lowered cap %+v", b)
	}
	if _, err := h.transfer(alice, "bob", 1, "t"); code(err) != "quota_exhausted" {
		t.Fatalf("transfer past the lowered cap: %v", err)
	}
	checkInvariants(t, h.db, h.p)
}
