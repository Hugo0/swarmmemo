package ledger

// The daily waterfall (RFC0012 §2.3): OpenDay, the lever and budget
// adjustment, Spill, the implicit Claim and Draw. The pure parts work on an
// in-memory dayState so a read (Balance) can show a prospective share without
// writing; loadDay and saveDay move it to and from allowance_days and
// allowance_pools.
//
// Overbooking: a claim issues an entitlement and draws nothing. Water is
// drawn only when free units are actually spent (or held for a remote call),
// from the pools of the lot's origin tier, so the day's spend stays within
// each tier's water and the budget while the entitlements may add up to more.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"swarmmemo/internal/allowance"
)

// pool is one tier's water on one day; tier 0 funds grants and dividends.
// Claimed is what this tier's free lots spent from its own water (for tier 0,
// what grants and dividends issued), Lent what it gave to higher tiers'
// spends and Borrowed what its spends took from lower tiers.
type pool struct {
	Size, Want, SpillIn, SpillOut, Claimed, Lent, Borrowed  int64
	ExpectedUnits, ExpectedWeight, ClaimedWeight, Claimants int64
}

// avail(t) = size + spill_in − spill_out − claimed − lent.
func (p pool) avail() int64 { return p.Size + p.SpillIn - p.SpillOut - p.Claimed - p.Lent }

// spentBy is what this tier's free lots spent: own water plus borrowed.
func (p pool) spentBy() int64 { return p.Claimed + p.Borrowed }

// dayState is allowance_days plus the five pools of one (resource, day).
type dayState struct {
	Resource                             allowance.Resource
	Day                                  int64
	Budget, BudgetEffective, Unallocated int64
	SpentNonpaid, SpentPaid              int64
	SpillDoneAt, OpenedAt, ParamsVersion int64
	Pools                                [5]pool
}

func (d *dayState) allocated() int64 {
	var n int64
	for _, p := range d.Pools {
		n += p.Size
	}
	return n
}

// effectiveBudget is the budget less a pulled cut-budget lever.
func effectiveBudget(rp *ResourceParams, r allowance.Resource, lv allowance.Levers) int64 {
	cut, ok := lv.BudgetCutPPM[r]
	if !ok {
		return rp.Budget
	}
	return rp.Budget - mulDiv(rp.Budget, clamp(cut, 0, ppm), ppm)
}

// limits are the most each pool may hold under today's levers: tier 0 its
// grant share, tiers 1–3 their want (tier 3 nothing under proven-only), tier 4
// its share (nothing under signed-only or proven-only).
func limits(d *dayState, rp *ResourceParams, lv allowance.Levers) [5]int64 {
	var l [5]int64
	l[0] = mulDiv(d.BudgetEffective, rp.GrantSharePPM, ppm)
	for t := 1; t <= 3; t++ {
		l[t] = d.Pools[t].Want
	}
	if lv.ProvenOnly {
		l[3] = 0
	}
	share := rp.ShareMaxPPM[3]
	if lv.Tier4SharePPM >= 0 {
		share = clamp(lv.Tier4SharePPM, 0, ppm)
	}
	l[4] = mulDiv(d.BudgetEffective, share, ppm)
	if anonymousOff(d.Resource, lv) {
		l[4] = 0
	}
	return l
}

// anonymousOff reports whether the levers give the anonymous tier nothing
// of resource r: signed-only and proven-only for every resource,
// signed-services for credit.
func anonymousOff(r allowance.Resource, lv allowance.Levers) bool {
	return lv.SignedOnly || lv.ProvenOnly || lv.SignedServices && r == allowance.Credit
}

// tier4Room is what tier-4 lots may still draw at now: the tier's limit
// (its share of the effective budget, or the lever's) as released so far
// today (tier4Released), less what they drew. Spill from tier 3 can leave
// tier 4 more water than that; it is never spent by tier 4 (it may still be
// lent to higher tiers), so the anonymous tier's spend stays within its
// share whatever spills down. hourly reports that the hourly release, not
// the day's limit, is what binds.
func tier4Room(d *dayState, rp *ResourceParams, lv allowance.Levers, now int64) (room int64, hourly bool) {
	day := limits(d, rp, lv)[4]
	released := tier4Released(d, rp, day, now)
	return max(0, released-d.Pools[4].Claimed), released < day
}

// tier4Released is how much of the anonymous tier's day limit lim tier-4
// lots may have drawn by now. Where the parameters cap the tier below the
// whole budget (share_max_ppm[3] < 100%, as for credit), the day is released
// over the day: a sixth at 00:00 UTC, then a 24th each hour, cumulatively,
// all of it from 20:00 (tier4ReleaseStart). One burst takes at most what has
// been released so far, never the whole day at once (security review 1.21,
// M3); what an hour leaves unused carries forward. At a full share (posting's
// default) nothing is held back.
func tier4Released(d *dayState, rp *ResourceParams, lim, now int64) int64 {
	if rp.ShareMaxPPM[3] >= ppm {
		return lim
	}
	hours := clamp((now-d.Day*86400)/3600, 0, 24)
	return min(lim, mulDiv(lim, tier4ReleaseStart+hours, 24))
}

// tier4ReleaseStart is the 24ths of a capped anonymous tier's day released
// at 00:00 UTC (tier4Released).
const tier4ReleaseStart = 4

// computeOpen is OpenDay without writing: sizes from the budget, reserves and
// the smoothed demand of the previous opened day (prev may be nil).
func computeOpen(r allowance.Resource, day int64, rp *ResourceParams, lv allowance.Levers, prev *dayState, version, now int64) dayState {
	d := dayState{Resource: r, Day: day, Budget: rp.Budget, OpenedAt: now, ParamsVersion: version}
	d.BudgetEffective = effectiveBudget(rp, r, lv)
	for t := 1; t <= 4; t++ {
		var claimed, expected, claimedW, expectedW int64
		if prev != nil {
			claimed, expected = prev.Pools[t].spentBy(), prev.Pools[t].ExpectedUnits
			claimedW, expectedW = prev.Pools[t].ClaimedWeight, prev.Pools[t].ExpectedWeight
		}
		// Smoothing: rises at once, falls by half a day.
		d.Pools[t].ExpectedUnits = max(claimed, (expected+claimed)/2)
		d.Pools[t].ExpectedWeight = max(claimedW, (expectedW+claimedW)/2)
	}
	B := d.BudgetEffective
	for t := 1; t <= 3; t++ {
		want := mulDivCeil(d.Pools[t].ExpectedUnits, 100+rp.HeadroomPct[t-1], 100) + mulDiv(B, rp.ReserveMinPPM[t-1], ppm)
		d.Pools[t].Want = min(want, mulDiv(B, rp.ShareMaxPPM[t-1], ppm))
	}
	lim := limits(&d, rp, lv)
	d.Pools[0].Size = min(B, lim[0])
	rem := B - d.Pools[0].Size
	for t := 1; t <= 4; t++ {
		d.Pools[t].Size = min(rem, lim[t])
		rem -= d.Pools[t].Size
	}
	d.Unallocated = d.Budget - d.allocated()
	return d
}

// adjust applies a budget change or a lever pulled or released since the day
// opened: every pool is brought within its limit and the total within the
// effective budget, shrinking bottom-up and only from unspent water;
// growth refills pools top-down to their limits. Issued lots are untouched,
// but a tier whose water shrank dries up for them too.
// It reports whether anything changed.
func adjust(d *dayState, rp *ResourceParams, lv allowance.Levers) bool {
	before := *d
	if d.Budget != rp.Budget {
		d.Budget = rp.Budget
	}
	d.BudgetEffective = effectiveBudget(rp, d.Resource, lv)
	lim := limits(d, rp, lv)
	for t := 4; t >= 0; t-- {
		if over := d.Pools[t].Size - lim[t]; over > 0 {
			d.Pools[t].Size -= min(over, max(0, d.Pools[t].avail()))
		}
	}
	if need := d.allocated() - d.BudgetEffective; need > 0 {
		for t := 4; t >= 0 && need > 0; t-- {
			x := min(need, max(0, d.Pools[t].avail()))
			d.Pools[t].Size -= x
			need -= x
		}
	}
	if room := d.BudgetEffective - d.allocated(); room > 0 {
		for _, t := range []int{0, 1, 2, 3, 4} {
			x := min(room, max(0, lim[t]-d.Pools[t].Size))
			d.Pools[t].Size += x
			room -= x
		}
	}
	d.Unallocated = d.Budget - d.allocated()
	return *d != before
}

// spillEvent is one tier handing water down at a boundary.
type spillEvent struct {
	Tier   int
	Amount int64
	At     int64
}

// spill processes the latest spill boundary at or before now not yet
// processed: tiers 1–3 keep a shrinking share of their unmet want and hand
// the rest to the next tier. Tier 4's leftover is never spent.
func spill(d *dayState, rp *ResourceParams, now int64) []spillEvent {
	if end := (d.Day + 1) * 86400; now >= end {
		now = end - 1
	}
	start := d.Day*86400 + rp.SpillStart
	if now < start {
		return nil
	}
	k := start + (now-start)/rp.SpillInterval*rp.SpillInterval
	if k <= d.SpillDoneAt {
		return nil
	}
	f := (k - d.Day*86400) * ppm / 86400 // evaluated at the boundary, not at now
	var events []spillEvent
	for t := 1; t <= 3; t++ {
		keep := mulDivCeil(max(0, d.Pools[t].Want-d.Pools[t].Claimed), ppm-f, ppm)
		x := max(0, d.Pools[t].avail()-keep)
		if t == 3 {
			// Tier 4 never holds more than its share of the budget: what
			// would spill past it stays with tier 3 (tier4Room bounds the
			// draws too, for levers pulled after a spill).
			x = min(x, max(0, mulDiv(d.BudgetEffective, rp.ShareMaxPPM[3], ppm)-d.Pools[4].Size-d.Pools[4].SpillIn))
		}
		if x > 0 {
			d.Pools[t].SpillOut += x
			d.Pools[t+1].SpillIn += x
			events = append(events, spillEvent{Tier: t, Amount: x, At: k})
		}
	}
	d.SpillDoneAt = k
	return events
}

// draw takes want units for a spend from a tier-t lot: own water first,
// then, if the tier borrows, from lower tiers, lowest priority first. A lower
// tier never borrows from a higher one.
func draw(d *dayState, rp *ResourceParams, t int, want int64) int64 {
	if want <= 0 {
		return 0
	}
	got := min(want, max(0, d.Pools[t].avail()))
	d.Pools[t].Claimed += got
	if t >= 1 && t <= 3 && rp.Borrow[t-1] {
		for u := 4; u > t && got < want; u-- {
			x := min(want-got, max(0, d.Pools[u].avail()))
			d.Pools[u].Lent += x
			d.Pools[t].Borrowed += x
			got += x
		}
	}
	return got
}

// drawable is what a spend from a tier-t lot could draw now: the tier's own
// water plus, if it borrows, what the lower tiers hold.
func drawable(d *dayState, rp *ResourceParams, t int) int64 {
	if t < 1 || t > 4 {
		return 0
	}
	n := max(0, d.Pools[t].avail())
	if t <= 3 && rp.Borrow[t-1] {
		for u := t + 1; u <= 4; u++ {
			n += max(0, d.Pools[u].avail())
		}
	}
	return n
}

// undraw returns x units a tier-t spend drew but did not use (a hold's
// refunded part): borrowed water first, to the lenders nearest the tier,
// then the tier's own. Each step is bounded, so the pools stay consistent.
func undraw(d *dayState, t int, x int64) {
	if t < 1 || t > 4 {
		return
	}
	for u := t + 1; u <= 4 && x > 0 && d.Pools[t].Borrowed > 0; u++ {
		y := min(x, d.Pools[t].Borrowed, d.Pools[u].Lent)
		d.Pools[u].Lent -= y
		d.Pools[t].Borrowed -= y
		x -= y
	}
	y := min(x, d.Pools[t].Claimed)
	d.Pools[t].Claimed -= y
}

// entitlement is a subject's share: its weight's share of its tier's water,
// clamped to the tier's floor and cap, the root's remaining cap and, for a new
// key under pause-new-keys, new_key_floor. It draws nothing: the tier's water
// is drawn only as the share is spent, so entitlements may add up to more than
// the tier holds (overbooking).
func entitlement(d *dayState, rp *ResourceParams, lv allowance.Levers, st allowance.Standing, rootGranted int64) int64 {
	t := int(st.Tier)
	if t == 4 && anonymousOff(d.Resource, lv) || t == 3 && lv.ProvenOnly {
		return 0
	}
	p := d.Pools[t]
	wt := max(p.ExpectedWeight, p.ClaimedWeight+st.WeightPPM)
	var share int64
	if wt > 0 {
		share = mulDiv(max(0, p.Size+p.SpillIn-p.SpillOut), st.WeightPPM, wt)
	}
	ent := min(max(share, rp.Floor[t-1]), rp.Cap[t-1])
	ent = min(ent, max(0, rp.RootCap[t-1]-rootGranted))
	if st.NewKey && lv.PauseNewKeys {
		ent = min(ent, rp.NewKeyFloor)
	}
	return max(0, ent)
}

// loadDay reads a day; ok is false when it has not opened.
func loadDay(ctx context.Context, q allowance.Querier, r allowance.Resource, day int64) (dayState, bool, error) {
	d := dayState{Resource: r, Day: day}
	err := q.QueryRowContext(ctx, "SELECT budget,budget_effective,unallocated,spent_nonpaid,spent_paid,spill_done_at,opened_at,params_version FROM allowance_days WHERE resource=? AND day=?", string(r), day).
		Scan(&d.Budget, &d.BudgetEffective, &d.Unallocated, &d.SpentNonpaid, &d.SpentPaid, &d.SpillDoneAt, &d.OpenedAt, &d.ParamsVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return d, false, nil
	}
	if err != nil {
		return d, false, err
	}
	rows, err := q.QueryContext(ctx, "SELECT tier,size,want,spill_in,spill_out,claimed,lent,borrowed,expected_units,expected_weight,claimed_weight,claimants FROM allowance_pools WHERE resource=? AND day=? ORDER BY tier LIMIT 5", string(r), day)
	if err != nil {
		return d, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var t int
		var p pool
		if err = rows.Scan(&t, &p.Size, &p.Want, &p.SpillIn, &p.SpillOut, &p.Claimed, &p.Lent, &p.Borrowed, &p.ExpectedUnits, &p.ExpectedWeight, &p.ClaimedWeight, &p.Claimants); err != nil {
			return d, false, err
		}
		if t < 0 || t > 4 {
			return d, false, fmt.Errorf("ledger: pool tier %d", t)
		}
		d.Pools[t] = p
	}
	return d, true, rows.Err()
}

// previousDay is the most recent opened day before day, or nil.
func previousDay(ctx context.Context, q allowance.Querier, r allowance.Resource, day int64) (*dayState, error) {
	var prev int64
	err := q.QueryRowContext(ctx, "SELECT day FROM allowance_days WHERE resource=? AND day<? ORDER BY day DESC LIMIT 1", string(r), day).Scan(&prev)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	d, ok, err := loadDay(ctx, q, r, prev)
	if err != nil || !ok {
		return nil, err
	}
	return &d, nil
}

func insertDay(ctx context.Context, q allowance.Querier, d *dayState) error {
	if _, err := q.ExecContext(ctx, "INSERT INTO allowance_days(resource,day,budget,budget_effective,unallocated,spent_nonpaid,spent_paid,spill_done_at,opened_at,params_version) VALUES(?,?,?,?,?,?,?,?,?,?)",
		string(d.Resource), d.Day, d.Budget, d.BudgetEffective, d.Unallocated, d.SpentNonpaid, d.SpentPaid, d.SpillDoneAt, d.OpenedAt, d.ParamsVersion); err != nil {
		return err
	}
	for t, p := range d.Pools {
		if _, err := q.ExecContext(ctx, "INSERT INTO allowance_pools(resource,day,tier,size,want,spill_in,spill_out,claimed,lent,borrowed,expected_units,expected_weight,claimed_weight,claimants) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
			string(d.Resource), d.Day, t, p.Size, p.Want, p.SpillIn, p.SpillOut, p.Claimed, p.Lent, p.Borrowed, p.ExpectedUnits, p.ExpectedWeight, p.ClaimedWeight, p.Claimants); err != nil {
			return err
		}
	}
	return nil
}

// saveDay writes back what changed since orig.
func saveDay(ctx context.Context, q allowance.Querier, d, orig *dayState) error {
	if *d == *orig {
		return nil
	}
	if _, err := q.ExecContext(ctx, "UPDATE allowance_days SET budget=?,budget_effective=?,unallocated=?,spent_nonpaid=?,spent_paid=?,spill_done_at=? WHERE resource=? AND day=?",
		d.Budget, d.BudgetEffective, d.Unallocated, d.SpentNonpaid, d.SpentPaid, d.SpillDoneAt, string(d.Resource), d.Day); err != nil {
		return err
	}
	for t, p := range d.Pools {
		if p == orig.Pools[t] {
			continue
		}
		if _, err := q.ExecContext(ctx, "UPDATE allowance_pools SET size=?,spill_in=?,spill_out=?,claimed=?,lent=?,borrowed=?,claimed_weight=?,claimants=? WHERE resource=? AND day=? AND tier=?",
			p.Size, p.SpillIn, p.SpillOut, p.Claimed, p.Lent, p.Borrowed, p.ClaimedWeight, p.Claimants, string(d.Resource), d.Day, t); err != nil {
			return err
		}
	}
	*orig = *d
	return nil
}

// claimRow is allowance_claims.
type claimRow struct {
	Tier        int
	Weight      int64
	Root        string
	Entitlement int64
	Granted     int64
	Source      string
	LotID       int64
}

func loadClaim(ctx context.Context, q allowance.Querier, r allowance.Resource, day int64, subject string) (claimRow, bool, error) {
	var c claimRow
	err := q.QueryRowContext(ctx, "SELECT tier,weight,root,entitlement,granted,source,lot_id FROM allowance_claims WHERE resource=? AND day=? AND subject=?", string(r), day, subject).
		Scan(&c.Tier, &c.Weight, &c.Root, &c.Entitlement, &c.Granted, &c.Source, &c.LotID)
	if errors.Is(err, sql.ErrNoRows) {
		return c, false, nil
	}
	return c, err == nil, err
}

func rootGranted(ctx context.Context, q allowance.Querier, r allowance.Resource, day int64, root string) (int64, error) {
	var n int64
	err := q.QueryRowContext(ctx, "SELECT coalesce(sum(granted),0) FROM allowance_claims WHERE resource=? AND day=? AND root=?", string(r), day, root).Scan(&n)
	return n, err
}

// freeExpiry is when a tier's claim lot expires: tiers 3–4 at the end of the
// UTC day, always; tiers 1–2 after claim_expiry_days more days.
func freeExpiry(p *AllowanceParams, tier int, day int64) (expiresAt, halfLife int64) {
	if tier >= 3 {
		return (day + 1) * 86400, 0
	}
	return (day + 1 + p.ClaimExpiryDays) * 86400, p.ClaimHalfLifeDays
}

func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }
