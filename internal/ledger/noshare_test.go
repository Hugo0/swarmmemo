package ledger

import (
	"database/sql"
	"errors"
	"testing"

	"swarmmemo/internal/allowance"
)

// A tier the parameters give no share of a resource (its cap 0, or its
// share_max_ppm 0) is refused tier_has_no_share, naming the resource and the
// tier, with no retry_after: waiting for 00:00 UTC never helps. A real dry
// pool stays global_quota_exhausted, and a lever's zero stays as it was.

func (h *harness) spendMemory(s allowance.Subject, units int64) error {
	return h.do(func(q *sql.Tx) error {
		_, err := h.l.Spend(ctx, q, s, allowance.MemoryBytes, units, Ref{Service: "memory", Op: "service.call", Method: "put"}, h.now)
		return err
	})
}

func (h *harness) reserveMemory(s allowance.Subject, units int64, key string) error {
	return h.do(func(q *sql.Tx) error {
		_, err := h.l.Reserve(ctx, q, s, allowance.MemoryBytes, units, key, Ref{Service: "memory", Op: "service.call", Method: "put"}, 60, h.now)
		return err
	})
}

func hostedSubject(id string) allowance.Subject {
	return allowance.Subject{ID: id, KeyID: "key-" + id, Signed: true, Hosted: true}
}

func wantNoShare(t *testing.T, err error, r allowance.Resource, tier int) {
	t.Helper()
	var e *allowance.Err
	if !errors.As(err, &e) || e.Code != "tier_has_no_share" || e.Resource != r || e.Tier != tier || e.RetryAfter != 0 {
		t.Fatalf("want tier_has_no_share for %s, tier %d, no retry_after; got %#v", r, tier, err)
	}
}

// As on prod: memory_bytes gives tier 4 a cap and a floor but share_max_ppm
// 0, so a hosted identity's issued share can never be drawn.
func TestNoShareByShareMaxPPM(t *testing.T) {
	p := DefaultAllowanceParams()
	mp := p.Resources[allowance.MemoryBytes]
	mp.Cap[3], mp.Floor[3], mp.RootCap[3], mp.ShareMaxPPM[3] = 1<<20, 16<<10, 1<<20, 0
	h := newHarness(t, p)
	wantNoShare(t, h.spendMemory(hostedSubject("hosted-1"), 300), allowance.MemoryBytes, 4)
	wantNoShare(t, h.reserveMemory(hostedSubject("hosted-1"), 300, "put-1"), allowance.MemoryBytes, 4)
	wantNoShare(t, h.spendMemory(anonSubject("net-a"), 300), allowance.MemoryBytes, 4)
	// The signed tier has its share; posting is untouched for tier 4.
	if err := h.spendMemory(signedSubject("alice"), 300); err != nil {
		t.Fatalf("signed memory: %v", err)
	}
	if err := h.spend(hostedSubject("hosted-1"), 300); err != nil {
		t.Fatalf("hosted posting: %v", err)
	}
	// A lever that sets the tier-4 share overrides the parameters: then the
	// share exists and is drawn.
	h.levers.lv.Tier4SharePPM = ppm
	if err := h.spendMemory(hostedSubject("hosted-2"), 300); err != nil {
		t.Fatalf("tier-4 share lever at 100%%: %v", err)
	}
	checkInvariants(t, h.db, h.p)
}

// The defaults give tier 4 a memory cap of 0.
func TestNoShareByCap(t *testing.T) {
	h := newHarness(t, DefaultAllowanceParams())
	wantNoShare(t, h.spendMemory(hostedSubject("hosted-1"), 1), allowance.MemoryBytes, 4)
	wantNoShare(t, h.spendMemory(anonSubject("net-a"), 1), allowance.MemoryBytes, 4)
	// A tier-4 lever at 0 is a lever, not the parameters: posting stays as
	// before (a share issued at claim, then the dry tier).
	h.levers.lv.Tier4SharePPM = 0
	if got := code(h.spend(anonSubject("net-b"), 1)); got == "tier_has_no_share" {
		t.Fatalf("a lever's zero share: %s", got)
	}
}

// A signed tier with share_max_ppm 0 that does not borrow has no share; one
// that borrows draws lower tiers' water, and its refusal when they are dry
// too is the real global_quota_exhausted.
func TestNoShareSignedTier(t *testing.T) {
	p := smallParams()
	rp := p.Resources[allowance.PostBytes]
	rp.ShareMaxPPM = []int64{ppm, ppm, 0, ppm}
	rp.ReserveMinPPM = []int64{0, 0, 0}
	rp.Floor = []int64{0, 0, 50, 0}
	rp.Borrow = []bool{true, true, false}
	h := newHarness(t, p)
	wantNoShare(t, h.spend(signedSubject("alice"), 10), allowance.PostBytes, 3)

	rp.Borrow[2] = true
	h2 := newHarness(t, p)
	if err := h2.spend(signedSubject("bob"), 10); err != nil {
		t.Fatalf("a borrowing tier with share 0: %v", err)
	}
}
