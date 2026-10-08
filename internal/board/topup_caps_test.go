package board

import (
	"crypto/ed25519"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The conservative top-up caps on the real engine (SQLite, ledger on) with
// only the facilitator faked: 0.10 to 5 USDC a top-up, 10 USDC per account
// and 100 USDC per board per UTC day, each refusing exactly at its limit.

// topUp pays amount for key with a fresh quote and authorization nonce.
func topUp(t *testing.T, s *Store, key ed25519.PrivateKey, amount int64, nonce byte) {
	t.Helper()
	res := run(t, s, paidCommand(key, amount, pay(t, quote(t, s, key, amount), nonce, nil)))
	if res.Data["topup"].(map[string]any)["state"] != "credited" {
		t.Fatalf("top-up %d: %v", amount, res.Data)
	}
}

func TestTopupDefaultCapsRefuseAtTheLimit(t *testing.T) {
	f := &fakeFacilitator{}
	s := topupStore(t, filepath.Join(t.TempDir(), "b.sqlite"), topupConfig(t, f, ""))
	limits := s.TopupCapabilities()["limits"].(map[string]int64)
	if limits["min"] != 100_000 || limits["max"] != 5_000_000 || limits["account_daily"] != 10_000_000 || limits["board_daily"] != 100_000_000 {
		t.Fatalf("limits %v", limits)
	}
	key := keyFor(110)
	register(t, s, key)

	// Per purchase: 0.10 to 5 USDC, to the unit.
	for _, amount := range []int64{99_999, 5_000_001} {
		_, e := topupExec(s, testContext, signed(key, Command{Operation: "credits.topup", Amount: amount}))
		if e == nil || e.Code != "topup_amount" || e.Status != 400 || !strings.Contains(e.Message, "0.1 USDC") || !strings.Contains(e.Message, "5 USDC") {
			t.Fatalf("amount %d: %+v", amount, e)
		}
	}
	quote(t, s, key, 100_000)
	quote(t, s, key, 5_000_000)

	// Per account per day: 10 USDC. 5 + 4.9 leaves 0.1; 0.1000001 more is
	// refused, 0.1 is taken, then one credit more is refused.
	topUp(t, s, key, 5_000_000, 1)
	topUp(t, s, key, 4_900_000, 2)
	_, e := topupExec(s, testContext, signed(key, Command{Operation: "credits.topup", Amount: 100_001}))
	if e == nil || e.Code != "topup_daily_limit" || e.Status != 429 || e.RetryAfter <= 0 || !strings.Contains(e.Message, "10 USDC") || !strings.Contains(e.Message, "00:00 UTC") {
		t.Fatalf("account cap: %+v", e)
	}
	topUp(t, s, key, 100_000, 3)
	if _, e := topupExec(s, testContext, signed(key, Command{Operation: "credits.topup", Amount: 100_000})); e == nil || e.Code != "topup_daily_limit" {
		t.Fatalf("account cap reached: %+v", e)
	}
	if got := paidBalance(t, s, key); got != 10_000_000 {
		t.Fatalf("paid balance %d", got)
	}

	// Board-wide per day: 100 USDC from every account together. Nine more
	// accounts at their own 10 USDC fill it; the board refuses the next
	// account's first top-up while that account is far under its own cap.
	for i := range 9 {
		other := keyFor(byte(111 + i))
		register(t, s, other)
		topUp(t, s, other, 5_000_000, byte(10+2*i))
		topUp(t, s, other, 5_000_000, byte(11+2*i))
	}
	late := keyFor(125)
	register(t, s, late)
	_, e = topupExec(s, testContext, signed(late, Command{Operation: "credits.topup", Amount: 100_000}))
	if e == nil || e.Code != "topup_board_daily_limit" || e.Status != 429 || e.RetryAfter <= 0 || !strings.Contains(e.Message, "100 USDC") || !strings.Contains(e.Message, "00:00 UTC") {
		t.Fatalf("board cap: %+v", e)
	}
	if paidBalance(t, s, late) != 0 || sqlCount(t, s, "SELECT coalesce(sum(amount),0) FROM credit_topups WHERE state='credited'") != 100_000_000 {
		t.Fatal("the board took more than its daily cap")
	}

	// Both reset at 00:00 UTC.
	next := testTime + 86400
	s.now = func() time.Time { return time.Unix(next, 0) }
	for _, k := range []ed25519.PrivateKey{key, late} {
		if _, e := topupExec(s, testContext, signed(k, Command{Operation: "credits.topup", Amount: 100_000, Timestamp: next})); e == nil || e.Code != "payment_required" {
			t.Fatalf("next day: %+v", e)
		}
	}
}

// A quote taken while the board had room is refused at payment once other
// agents filled it, and nothing settles.
func TestTopupBoardCapHoldsAtPayment(t *testing.T) {
	f := &fakeFacilitator{}
	s := topupStore(t, filepath.Join(t.TempDir(), "b.sqlite"), topupConfig(t, f, `{"min":"0.1","max":"1","account_daily":"1","board_daily":"1.5"}`))
	a, b := keyFor(126), keyFor(127)
	register(t, s, a)
	register(t, s, b)
	accepted := quote(t, s, b, 1_000_000)
	topUp(t, s, a, 1_000_000, 50)
	if _, e := topupExec(s, testContext, paidCommand(b, 1_000_000, pay(t, accepted, 51, nil))); e == nil || e.Code != "topup_board_daily_limit" {
		t.Fatalf("board cap at payment: %+v", e)
	}
	if _, settles := f.counts(); settles != 1 || paidBalance(t, s, b) != 0 {
		t.Fatalf("%d settlements, balance %d", settles, paidBalance(t, s, b))
	}
	// Exactly the remainder still fits.
	topUp(t, s, b, 500_000, 52)
}

// An exact retry of a credited top-up (same request_id) answers the receipt
// and credits nothing more; a payment the facilitator would not verify
// credits nothing and charges nothing.
func TestTopupRetryAndFailedVerificationUnderCaps(t *testing.T) {
	f := &fakeFacilitator{}
	s := topupStore(t, filepath.Join(t.TempDir(), "b.sqlite"), topupConfig(t, f, ""))
	key := keyFor(128)
	register(t, s, key)
	cmd := signed(key, Command{Operation: "credits.topup", Amount: 5_000_000, RequestID: "caps-retry-1",
		Data: `{"schema":1,"payment":"` + pay(t, quote(t, s, key, 5_000_000), 60, nil) + `"}`})
	first := run(t, s, cmd)
	for range 3 {
		again := run(t, s, cmd)
		if again.Data["topup"].(map[string]any)["id"] != first.Data["topup"].(map[string]any)["id"] {
			t.Fatalf("retry answered another top-up: %v", again.Data)
		}
	}
	if _, settles := f.counts(); settles != 1 || paidBalance(t, s, key) != 5_000_000 {
		t.Fatalf("%d settlements, balance %d", settles, paidBalance(t, s, key))
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM ledger_entries WHERE kind='topup'"); n != 1 {
		t.Fatalf("%d topup journal entries", n)
	}

	f.set(func(f *fakeFacilitator) {
		f.verifyStatus, f.verifyBody = 200, `{"isValid":false,"invalidReason":"invalid_exact_evm_payload_signature"}`
	})
	if _, e := topupExec(s, testContext, paidCommand(key, 1_000_000, pay(t, quote(t, s, key, 1_000_000), 61, nil))); e == nil || e.Code != "payment_rejected" {
		t.Fatalf("failed verification: %+v", e)
	}
	if _, settles := f.counts(); settles != 1 || paidBalance(t, s, key) != 5_000_000 {
		t.Fatalf("a failed verification settled or credited: %d settlements, balance %d", settles, paidBalance(t, s, key))
	}
	// A failed top-up does not count toward the caps: the rest of the
	// account's 10 USDC is still there.
	f.set(func(f *fakeFacilitator) { f.verifyStatus, f.verifyBody = 0, "" })
	topUp(t, s, key, 5_000_000, 62)
	if paidBalance(t, s, key) != 10_000_000 {
		t.Fatalf("paid balance %d", paidBalance(t, s, key))
	}
}
