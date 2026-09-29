package ledger

import (
	"testing"

	"swarmmemo/internal/allowance"
)

// The waterfall pseudocode of RFC0012 §2.3 as table tests on the pure
// functions: OpenDay, Draw (with borrowing) and Spill at fixed boundaries.

const day0 = int64(20701) // 2026-09-06, 00:00 UTC = 1788566400

func noLevers() allowance.Levers { return allowance.Levers{Tier4SharePPM: -1} }

func testResource(budget int64) *ResourceParams {
	rp := *DefaultAllowanceParams().Resources[allowance.PostBytes]
	rp.Budget, rp.SpendCeiling, rp.InboundCap = budget, 2*budget, budget
	rp.Cap = []int64{budget, budget, budget, budget}
	rp.Floor = []int64{0, 0, 0, 0}
	rp.RootCap = []int64{budget, budget, budget, budget}
	return &rp
}

func sizes(d dayState) [5]int64 {
	var s [5]int64
	for t := range d.Pools {
		s[t] = d.Pools[t].Size
	}
	return s
}

func TestOpenDayTable(t *testing.T) {
	prev := func(claimed [5]int64, expected [5]int64) *dayState {
		d := &dayState{}
		for t := range d.Pools {
			d.Pools[t].Claimed, d.Pools[t].ExpectedUnits = claimed[t], expected[t]
		}
		return d
	}
	cases := []struct {
		name        string
		budget      int64
		grant       int64
		prev        *dayState
		levers      allowance.Levers
		want        [5]int64 // sizes
		wants       [4]int64 // want[1..3] (index 0 unused)
		unallocated int64
	}{
		// No history: reserves only (10%, 10%, 20%), tier 4 takes the rest.
		{"fresh", 1000, 0, nil, noLevers(), [5]int64{0, 100, 100, 200, 600}, [4]int64{0, 100, 100, 200}, 0},
		// A grant share comes off the top; reserves are still shares of B.
		{"grant share", 1000, 100_000, nil, noLevers(), [5]int64{100, 100, 100, 200, 500}, [4]int64{0, 100, 100, 200}, 0},
		// Yesterday tier 3 drew 300 (expected 100): E rises at once to 300,
		// want = ceil(300 × 1.5) + 200 = 650.
		{"demand rises", 1000, 0, prev([5]int64{0, 0, 0, 300, 0}, [5]int64{0, 0, 0, 100, 0}), noLevers(), [5]int64{0, 100, 100, 650, 150}, [4]int64{0, 100, 100, 650}, 0},
		// Yesterday tier 3 drew 0 but expected 400: E falls by half, to 200.
		{"demand falls by half", 1000, 0, prev([5]int64{0, 0, 0, 0, 0}, [5]int64{0, 0, 0, 400, 0}), noLevers(), [5]int64{0, 100, 100, 500, 300}, [4]int64{0, 100, 100, 500}, 0},
		// Tier 1 wants more than the budget: tiers below get what is left.
		{"top tier saturates", 1000, 0, prev([5]int64{0, 2000, 0, 0, 0}, [5]int64{}), noLevers(), [5]int64{0, 1000, 0, 0, 0}, [4]int64{0, 1000, 100, 200}, 0},
		// signed-only: tier 4 gets nothing, the rest stays unallocated.
		{"signed only", 1000, 0, nil, allowance.Levers{Tier4SharePPM: -1, SignedOnly: true}, [5]int64{0, 100, 100, 200, 0}, [4]int64{0, 100, 100, 200}, 600},
		// tier4-shrink to 25%.
		{"tier4 shrink", 1000, 0, nil, allowance.Levers{Tier4SharePPM: 250_000}, [5]int64{0, 100, 100, 200, 250}, [4]int64{0, 100, 100, 200}, 350},
		// cut-budget by half: every share is of the effective budget.
		{"cut budget", 1000, 0, nil, allowance.Levers{Tier4SharePPM: -1, BudgetCutPPM: map[allowance.Resource]int64{allowance.PostBytes: 500_000}}, [5]int64{0, 50, 50, 100, 300}, [4]int64{0, 50, 50, 100}, 500},
		// proven-only: tiers 3 and 4 get nothing.
		{"proven only", 1000, 0, nil, allowance.Levers{Tier4SharePPM: -1, ProvenOnly: true}, [5]int64{0, 100, 100, 0, 0}, [4]int64{0, 100, 100, 200}, 800},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rp := testResource(c.budget)
			rp.GrantSharePPM = c.grant
			d := computeOpen(allowance.PostBytes, day0, rp, c.levers, c.prev, 0, day0*86400)
			if got := sizes(d); got != c.want {
				t.Fatalf("sizes %v, want %v", got, c.want)
			}
			for tier := 1; tier <= 3; tier++ {
				if d.Pools[tier].Want != c.wants[tier] {
					t.Fatalf("want[%d] = %d, expected %d", tier, d.Pools[tier].Want, c.wants[tier])
				}
			}
			if d.Unallocated != c.unallocated || d.allocated()+d.Unallocated != d.Budget {
				t.Fatalf("unallocated %d (expected %d), allocated %d, budget %d", d.Unallocated, c.unallocated, d.allocated(), d.Budget)
			}
		})
	}
}

func TestDrawAndBorrowTable(t *testing.T) {
	cases := []struct {
		name     string
		borrow   bool
		tier     int
		want     int64
		got      int64
		claimed  [5]int64
		lent     [5]int64
		borrowed [5]int64
	}{
		// Own water first.
		{"own water", true, 3, 150, 150, [5]int64{0, 0, 0, 150, 0}, [5]int64{}, [5]int64{}},
		// Tier 3 has 200: the rest is borrowed from tier 4.
		{"borrow from 4", true, 3, 500, 500, [5]int64{0, 0, 0, 200, 0}, [5]int64{0, 0, 0, 0, 300}, [5]int64{0, 0, 0, 300, 0}},
		// Tier 1 borrows lowest priority first: tier 4, then 3, then 2.
		{"tier 1 borrows bottom up", true, 1, 1000, 1000, [5]int64{0, 100, 0, 0, 0}, [5]int64{0, 0, 100, 200, 600}, [5]int64{0, 900, 0, 0, 0}},
		// Borrowing off: own water only.
		{"no borrow", false, 2, 500, 100, [5]int64{0, 0, 100, 0, 0}, [5]int64{}, [5]int64{}},
		// Tier 4 never borrows (nothing is below it) and never from above.
		{"tier 4 own only", true, 4, 900, 600, [5]int64{0, 0, 0, 0, 600}, [5]int64{}, [5]int64{}},
		// The whole day: 1000 units, a tier-2 subject wanting 2000 gets 900
		// (it cannot borrow tier 1's water).
		{"never upward", true, 2, 2000, 900, [5]int64{0, 0, 100, 0, 0}, [5]int64{0, 0, 0, 200, 600}, [5]int64{0, 0, 800, 0, 0}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rp := testResource(1000)
			rp.Borrow = []bool{c.borrow, c.borrow, c.borrow}
			d := computeOpen(allowance.PostBytes, day0, rp, noLevers(), nil, 0, day0*86400)
			if got := draw(&d, rp, c.tier, c.want); got != c.got {
				t.Fatalf("got %d, want %d", got, c.got)
			}
			for u := range d.Pools {
				p := d.Pools[u]
				if p.Claimed != c.claimed[u] || p.Lent != c.lent[u] || p.Borrowed != c.borrowed[u] {
					t.Fatalf("tier %d: claimed %d lent %d borrowed %d; want %d %d %d", u, p.Claimed, p.Lent, p.Borrowed, c.claimed[u], c.lent[u], c.borrowed[u])
				}
				if p.avail() < 0 {
					t.Fatalf("tier %d overdrawn: %d", u, p.avail())
				}
			}
		})
	}
}

func TestSpillAtFixedBoundaries(t *testing.T) {
	rp := testResource(86400) // one unit per second of the day keeps the arithmetic exact
	start := day0 * 86400
	fresh := func() dayState { return computeOpen(allowance.PostBytes, day0, rp, noLevers(), nil, 0, start) }
	// Sizes: tier 1 8640, tier 2 8640, tier 3 17280, tier 4 51840.
	cases := []struct {
		name   string
		at     int64
		out    [4]int64 // spill_out of tiers 1–3
		doneAt int64
	}{
		{"before spill_start", start + 3599, [4]int64{}, 0},
		// At 01:00, f = floor(3600e6/86400) = 41666 ppm: each tier keeps
		// ceil(unmet want × (1e6 − f) / 1e6) = 8281 of 8640 (17280: 16561)
		// and hands on the rest; tier 1's spill reaches tier 2 before tier 2
		// computes (8999 − 8281 = 718; 17998 − 16561 = 1437).
		{"first boundary", start + 3600, [4]int64{0, 359, 718, 1437}, start + 3600},
		{"between boundaries uses the latest", start + 3600 + 1799, [4]int64{0, 359, 718, 1437}, start + 3600},
		// At 12:00 in one step (skipped boundaries are not replayed): f = 1/2,
		// each keeps half its want: 4320; 12960 − 4320; 25920 − 8640.
		{"noon", start + 12*3600, [4]int64{0, 4320, 8640, 17280}, start + 12*3600},
		// After midnight the day's last boundary (23:00) is used: f = 958333,
		// keeps 361, 361 and 721.
		{"end of day", start + 86400 + 10, [4]int64{0, 8279, 16558, 33117}, start + 23*3600},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := fresh()
			spill(&d, rp, c.at)
			for tier := 1; tier <= 3; tier++ {
				if d.Pools[tier].SpillOut != c.out[tier] || d.Pools[tier+1].SpillIn != c.out[tier] {
					t.Fatalf("tier %d spill_out %d, tier %d spill_in %d; want %d", tier, d.Pools[tier].SpillOut, tier+1, d.Pools[tier+1].SpillIn, c.out[tier])
				}
			}
			if d.SpillDoneAt != c.doneAt {
				t.Fatalf("spill_done_at %d, want %d", d.SpillDoneAt, c.doneAt)
			}
			// Processing the same boundary again is a no-op.
			before := d
			if spill(&d, rp, c.at) != nil || d != before {
				t.Fatal("spill processed a boundary twice")
			}
		})
	}
	// Claims count against what a tier keeps: a tier that drew its whole want
	// keeps nothing and passes on what spilled into it.
	d := fresh()
	draw(&d, rp, 3, 17280)
	spill(&d, rp, start+3600)
	if d.Pools[3].SpillOut != d.Pools[3].SpillIn || d.Pools[3].avail() != 0 {
		t.Fatalf("a tier that drew its want kept water: %+v", d.Pools[3])
	}
}

func TestEntitlementTable(t *testing.T) {
	rp := testResource(1000)
	rp.Floor = []int64{50, 50, 50, 20}
	rp.Cap = []int64{400, 300, 150, 100}
	rp.RootCap = []int64{800, 600, 300, 100}
	rp.NewKeyFloor = 5
	d := computeOpen(allowance.PostBytes, day0, rp, noLevers(), nil, 0, day0*86400) // tier 3 size 200
	st := func(tier allowance.Tier, w int64) allowance.Standing {
		return allowance.Standing{Tier: tier, WeightPPM: w, Root: "r"}
	}
	cases := []struct {
		name   string
		claim  int64 // claimed weight so far in tier 3
		expW   int64
		st     allowance.Standing
		root   int64
		levers allowance.Levers
		want   int64
	}{
		{"alone gets the pool, capped", 0, 0, st(3, ppm), 0, noLevers(), 150},
		{"shares by weight", 3 * ppm, 0, st(3, ppm), 0, noLevers(), 50},
		{"expected weight divides before anyone arrives", 0, 8 * ppm, st(3, ppm), 0, noLevers(), 50},
		{"floor when the share is tiny", 0, 100 * ppm, st(3, ppm), 0, noLevers(), 50},
		{"double weight", ppm, 0, st(3, 2*ppm), 0, noLevers(), 133},
		{"root cap", 0, 0, st(3, ppm), 250, noLevers(), 50},
		{"root cap exhausted", 0, 0, st(3, ppm), 300, noLevers(), 0},
		{"new key paused", 0, 0, allowance.Standing{Tier: 3, WeightPPM: ppm, Root: "r", NewKey: true}, 0, allowance.Levers{Tier4SharePPM: -1, PauseNewKeys: true}, 5},
		{"signed only zeroes tier 4", 0, 0, st(4, ppm), 0, allowance.Levers{Tier4SharePPM: -1, SignedOnly: true}, 0},
		{"proven only zeroes tier 3", 0, 0, st(3, ppm), 0, allowance.Levers{Tier4SharePPM: -1, ProvenOnly: true}, 0},
		{"weight zero keeps the floor", 0, 0, st(3, 0), 0, noLevers(), 50},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dd := d
			dd.Pools[3].ClaimedWeight, dd.Pools[3].ExpectedWeight = c.claim, c.expW
			if got := entitlement(&dd, rp, c.levers, c.st, c.root); got != c.want {
				t.Fatalf("entitlement %d, want %d", got, c.want)
			}
		})
	}
}

func TestAdjustShrinksBottomUpAndRestores(t *testing.T) {
	rp := testResource(1000)
	d := computeOpen(allowance.PostBytes, day0, rp, noLevers(), nil, 0, day0*86400)
	draw(&d, rp, 4, 500) // tier 4 issued 500 of its 600
	cut := allowance.Levers{Tier4SharePPM: -1, BudgetCutPPM: map[allowance.Resource]int64{allowance.PostBytes: 500_000}}
	if !adjust(&d, rp, cut) {
		t.Fatal("cut-budget changed nothing")
	}
	// Target 500: tier 4 is first brought within its limit as far as its
	// unissued water allows (600 → 500, all issued), then the pools shrink
	// bottom-up: tier 3, 2 and 1 give up their unissued water. Issued units
	// are untouched.
	if got := sizes(d); got != ([5]int64{0, 0, 0, 0, 500}) || d.Unallocated != 500 {
		t.Fatalf("after cut: %v, unallocated %d", got, d.Unallocated)
	}
	if !adjust(&d, rp, noLevers()) {
		t.Fatal("release changed nothing")
	}
	if got := sizes(d); got != ([5]int64{0, 100, 100, 200, 600}) || d.Unallocated != 0 {
		t.Fatalf("after release: %v, unallocated %d", got, d.Unallocated)
	}
	if adjust(&d, rp, noLevers()) {
		t.Fatal("adjust is not idempotent")
	}
}
