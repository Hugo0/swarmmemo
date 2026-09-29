package ledger

import (
	"database/sql"
	"fmt"
	"testing"

	"swarmmemo/internal/allowance"
)

// Credit for callers without a key: tier 4 gets a small share of the credit
// budget. The share of the budget is the hard bound on what every anonymous
// subject together can spend in a day, whatever spills down to tier 4 and
// however many networks call; the signed-services lever takes it to zero at
// once. Signed tiers and other resources are untouched.

// anonCreditParams: credit budget 10,000, anonymous cap and floor 20 a
// network, anonymous tier 10% of the budget (1,000).
func anonCreditParams() AllowanceParams {
	p := DefaultAllowanceParams()
	rp := p.Resources[allowance.Credit]
	rp.Budget, rp.SpendCeiling, rp.InboundCap = 10000, 10000, 10000
	rp.Cap = []int64{4000, 2000, 1000, 20}
	rp.Floor = []int64{16, 16, 16, 20}
	rp.RootCap = []int64{10000, 8000, 4000, 20}
	rp.ShareMaxPPM = []int64{ppm, ppm, ppm, 100_000}
	return p
}

func (h *harness) spendCredit(s allowance.Subject, units int64) error {
	return h.do(func(q *sql.Tx) error {
		_, err := h.l.Spend(ctx, q, s, allowance.Credit, units, Ref{Service: "notary", Op: "service.call", Method: "stamp"}, h.now)
		return err
	})
}

func (h *harness) creditDay() dayState {
	h.t.Helper()
	d, ok, err := loadDay(ctx, h.db, allowance.Credit, h.now/86400)
	if err != nil || !ok {
		h.t.Fatalf("credit day: %v %v", ok, err)
	}
	return d
}

func refusalCode(err error) string {
	if e, ok := err.(*allowance.Err); ok {
		return e.Code
	}
	return fmt.Sprint(err)
}

func TestAnonymousCreditPerNetworkAndTierCap(t *testing.T) {
	h := newHarness(t, anonCreditParams())
	// 05:00 UTC: 9/24 of the tier's day is released (tier4Released), room
	// for the first eleven networks; the rest is released by 20:00.
	h.now += 5 * 3600
	// One network gets its cap, and no more.
	a := anonSubject("net-a")
	if err := h.spendCredit(a, 20); err != nil {
		t.Fatal(err)
	}
	if err := h.spendCredit(a, 1); refusalCode(err) != "quota_exhausted" {
		t.Fatalf("a network past its share: %v", err)
	}
	// Networks together stop at the tier's 10% (1,000), even after hours of
	// spill have handed tier 4 far more water than that.
	spent := int64(20)
	var last error
	for i := 0; i < 200; i++ {
		if i == 10 {
			h.now += 18 * 3600 // spill boundaries pass: tier 3's unused water flows down
		}
		if last = h.spendCredit(anonSubject(fmt.Sprintf("net-%d", i)), 20); last != nil {
			break
		}
		spent += 20
	}
	if refusalCode(last) != "global_quota_exhausted" || spent != 1000 {
		t.Fatalf("anonymous tier spent %d, last %v; want 1000 then global_quota_exhausted", spent, last)
	}
	d := h.creditDay()
	if d.Pools[3].SpillIn == 0 || d.Pools[4].Size+d.Pools[4].SpillIn-d.Pools[4].SpillOut > 1000 {
		t.Fatalf("spill ran, and never past tier 4's share: tier 3 %+v, tier 4 %+v", d.Pools[3], d.Pools[4])
	}
	if d.Pools[4].Claimed != 1000 || d.Pools[4].Borrowed != 0 {
		t.Fatalf("tier 4 drew %d (borrowed %d), want exactly its share", d.Pools[4].Claimed, d.Pools[4].Borrowed)
	}
	// Signed callers still draw their own water, and may borrow tier 4's
	// leftover (a lower tier lends upward, never the reverse).
	if err := h.spendCredit(signedSubject("signed-1"), 16); err != nil {
		t.Fatalf("signed spend after the anonymous tier ran dry: %v", err)
	}
	// Posting bytes for the same networks are untouched by the credit cap.
	if err := h.spend(anonSubject("net-a"), 50); err != nil {
		t.Fatalf("anonymous posting: %v", err)
	}
}

func TestAnonymousCreditLeverOff(t *testing.T) {
	h := newHarness(t, anonCreditParams())
	a := anonSubject("net-a")
	if err := h.spendCredit(a, 5); err != nil {
		t.Fatal(err)
	}
	// Pulled mid-day: the network's unspent share stops at once (the tier
	// has no water left for it), a new network gets nothing, and signed
	// credit and anonymous posting go on.
	h.levers.lv.SignedServices = true
	if err := h.spendCredit(a, 1); refusalCode(err) != "global_quota_exhausted" {
		t.Fatalf("an issued share after signed-services: %v", err)
	}
	if err := h.spendCredit(anonSubject("net-b"), 1); refusalCode(err) != "quota_exhausted" {
		t.Fatalf("a new network under signed-services: %v", err)
	}
	b, err := h.l.Balance(ctx, h.db, anonSubject("net-c"), allowance.Credit, h.now)
	if err != nil || b.Entitlement != 0 {
		t.Fatalf("prospective anonymous credit under signed-services: %+v %v", b, err)
	}
	if err := h.spendCredit(signedSubject("signed-1"), 10); err != nil {
		t.Fatalf("signed credit under signed-services: %v", err)
	}
	if err := h.spend(anonSubject("net-b"), 10); err != nil {
		t.Fatalf("anonymous posting under signed-services: %v", err)
	}
	// Released: the anonymous tier draws again, within the same caps.
	h.levers.lv.SignedServices = false
	if err := h.spendCredit(anonSubject("net-d"), 20); err != nil {
		t.Fatalf("after release: %v", err)
	}
}

// With the anonymous tier's share at 100% (post_bytes' default) the cap on
// tier-4 draws never binds: posting is unchanged by it.
func TestAnonymousTierCapInertAtFullShare(t *testing.T) {
	h := newHarness(t, smallParams())
	for i := 0; i < 5; i++ {
		if err := h.spend(anonSubject(fmt.Sprintf("n%d", i)), 100); err != nil {
			t.Fatalf("anonymous post %d: %v", i, err)
		}
	}
	d, _, _ := loadDay(ctx, h.db, allowance.PostBytes, h.now/86400)
	if room, hourly := tier4Room(&d, h.p.Resources[allowance.PostBytes], noLevers(), h.now); room < max(0, d.Pools[4].avail()) || hourly {
		t.Fatalf("at a full share the cap must not bind: room %d, avail %d", room, d.Pools[4].avail())
	}
}

// Security review 1.21, M3: where the anonymous tier is capped below the
// whole budget (credit), its day is released over the day (a sixth at
// midnight, a 24th each hour), so one burst early in the day cannot take the
// whole day's share; what an hour
// leaves unused carries forward, and the refusal says when more comes.
func TestSecReview121AnonymousTierReleasedHourly(t *testing.T) {
	h := newHarness(t, anonCreditParams()) // tier 4: 1,000 a day, 20 a network
	h.now += 30 * 60                       // 00:30 UTC: a sixth released (166)
	burst := func() (int64, error) {
		var spent int64
		for i := 0; i < 60; i++ {
			if err := h.spendCredit(anonSubject(fmt.Sprintf("burst-%d-%d", h.now, i)), 20); err != nil {
				return spent, err
			}
			spent += 20
		}
		return spent, nil
	}
	spent, err := burst()
	if spent != 160 || refusalCode(err) != "global_quota_exhausted" {
		t.Fatalf("first hour: spent %d, %v; want 160 then global_quota_exhausted", spent, err)
	}
	if e, ok := err.(*allowance.Err); !ok || e.RetryAfter <= 0 || e.RetryAfter > 1800 {
		t.Fatalf("the refusal waits for the next hour, not midnight: %+v", err)
	}
	// 10:30: 14/24 released (583) less the 160 drawn: the unused hours carry
	// forward.
	h.now += 10 * 3600
	if spent, err = burst(); spent != 420 || refusalCode(err) != "global_quota_exhausted" {
		t.Fatalf("after ten hours: spent %d, %v; want 420", spent, err)
	}
	// 23:30: the whole day is released; the tier still stops at its share.
	h.now += 13 * 3600
	if spent, err = burst(); spent != 420 || refusalCode(err) != "global_quota_exhausted" {
		t.Fatalf("last hour: spent %d, %v; want the rest of 1,000", spent, err)
	}
	if e, ok := err.(*allowance.Err); !ok || int64(e.RetryAfter) != 86400-h.now%86400 {
		t.Fatalf("with the whole day released the refusal waits for midnight: %+v", err)
	}
	// Posting (tier 4 at a full share) is not held back.
	if err := h.spend(anonSubject("poster"), 50); err != nil {
		t.Fatalf("anonymous posting: %v", err)
	}
}
