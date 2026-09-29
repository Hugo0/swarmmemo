package ledger

// Lots, the implicit claim, Spend, Balance and Mint (RFC0012 §2.2–2.3).

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"swarmmemo/internal/allowance"
)

// lot is one row of ledger_lots.
type lot struct {
	ID            int64
	Account       string
	Bucket        allowance.Bucket
	OriginTier    int64
	OriginAccount string
	Hops          int64
	IssuedDay     int64
	ExpiresAt     int64
	HalfLife      int64
	DecayedDay    int64
	Initial       int64
	Remaining     int64
	Held          int64
}

func (x lot) spendable() int64 { return x.Remaining - x.Held }

const lotCols = "id,account,bucket,origin_tier,origin_account,hops,issued_day,expires_at,half_life_days,decayed_day,initial,remaining,held"

// spendOrder is soonest to lose first: free (earliest expiry), granted, earned
// (shortest half-life, oldest issue), then paid.
const spendOrder = "CASE bucket WHEN 'free' THEN 0 WHEN 'granted' THEN 1 WHEN 'earned' THEN 2 ELSE 3 END, CASE WHEN expires_at=0 THEN 9223372036854775807 ELSE expires_at END, CASE WHEN half_life_days=0 THEN 9223372036854775807 ELSE half_life_days END, issued_day, id"

func scanLot(scan func(...any) error) (lot, error) {
	var x lot
	var bucket string
	err := scan(&x.ID, &x.Account, &bucket, &x.OriginTier, &x.OriginAccount, &x.Hops, &x.IssuedDay, &x.ExpiresAt, &x.HalfLife, &x.DecayedDay, &x.Initial, &x.Remaining, &x.Held)
	x.Bucket = allowance.Bucket(bucket)
	return x, err
}

// liveLotsMax bounds the lots a spend plans over. creditLot keeps an account
// at LotsPerAccount live lots, and the day's claim lot comes on top, so this
// never cuts a live lot off: a subject's own share is always in the plan,
// however many lots others sent it (security review 1.20, H3).
const liveLotsMax = 2 * LotsPerAccount

// liveLots are an account's unexpired lots in spend order.
func liveLots(ctx context.Context, q allowance.Querier, account string, r allowance.Resource, now int64) ([]lot, error) {
	rows, err := q.QueryContext(ctx, "SELECT "+lotCols+" FROM ledger_lots WHERE account=? AND resource=? AND state='live' AND (expires_at=0 OR expires_at>?) ORDER BY "+spendOrder+" LIMIT ?", account, string(r), now, liveLotsMax)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []lot
	for rows.Next() {
		x, err := scanLot(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// part is units taken from one lot.
type part struct {
	Lot  lot
	Take int64
}

// plan takes up to units from lots in order, from those ok accepts.
func plan(lots []lot, units int64, ok func(lot) bool) ([]part, int64) {
	var parts []part
	var got int64
	for _, x := range lots {
		if got >= units {
			break
		}
		if ok != nil && !ok(x) {
			continue
		}
		if take := min(units-got, x.spendable()); take > 0 {
			parts = append(parts, part{Lot: x, Take: take})
			got += take
		}
	}
	return parts, got
}

func nonpaid(parts []part) int64 {
	var n int64
	for _, p := range parts {
		if p.Lot.Bucket != allowance.Paid {
			n += p.Take
		}
	}
	return n
}

// insertLot creates a lot; creditLot merges into a live lot of the same
// account, bucket, origin tier, issue day, expiry and decay clock when one
// exists (so an account holds at most LotsPerAccount live lots per resource).
// Lots from other accounts merge whoever sent them: the sender stays in the
// journal, so many small transfers from many senders fill one lot, not the
// recipient's lot slots. The account's own lots (its claim, its mints) merge
// only with lots of its own origin, so a received lot never joins the claim
// lot that capOwn limits.
func insertLot(ctx context.Context, q allowance.Querier, r allowance.Resource, x lot, now int64) (int64, error) {
	res, err := q.ExecContext(ctx, "INSERT INTO ledger_lots(account,resource,bucket,origin_tier,origin_account,hops,issued_day,expires_at,half_life_days,decayed_day,initial,remaining,held,state,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,0,'live',?)",
		x.Account, string(r), string(x.Bucket), x.OriginTier, x.OriginAccount, x.Hops, x.IssuedDay, x.ExpiresAt, x.HalfLife, x.DecayedDay, x.Initial, x.Remaining, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func creditLot(ctx context.Context, q allowance.Querier, r allowance.Resource, x lot, now int64) (int64, error) {
	var id int64
	origin := "origin_account=?"
	if x.OriginAccount != x.Account {
		origin = "origin_account<>?"
	}
	err := q.QueryRowContext(ctx, "SELECT id FROM ledger_lots WHERE account=? AND resource=? AND state='live' AND bucket=? AND origin_tier=? AND "+origin+" AND issued_day=? AND expires_at=? AND half_life_days=? AND decayed_day=? ORDER BY id LIMIT 1",
		x.Account, string(r), string(x.Bucket), x.OriginTier, x.Account, x.IssuedDay, x.ExpiresAt, x.HalfLife, x.DecayedDay).Scan(&id)
	if err == nil {
		_, err = q.ExecContext(ctx, "UPDATE ledger_lots SET remaining=remaining+?,initial=initial+?,hops=max(hops,?) WHERE id=?", x.Remaining, x.Remaining, x.Hops, id)
		return id, err
	}
	if !isNoRows(err) {
		return 0, err
	}
	var live int
	if err = q.QueryRowContext(ctx, "SELECT count(*) FROM ledger_lots WHERE account=? AND resource=? AND state='live'", x.Account, string(r)).Scan(&live); err != nil {
		return 0, err
	}
	if live >= LotsPerAccount {
		return 0, refuse("recipient_limit")
	}
	return insertLot(ctx, q, r, x, now)
}

// takeFrom spends take units of a lot (and releases held units first when
// the take settles a hold).
func takeFrom(ctx context.Context, q allowance.Querier, id, take, released, lost int64) error {
	gone := take + lost
	_, err := q.ExecContext(ctx, "UPDATE ledger_lots SET held=held-?,remaining=remaining-?,state=CASE WHEN remaining-?=0 THEN (CASE WHEN ?>0 THEN 'expired' ELSE 'spent' END) ELSE state END WHERE id=?", released, gone, gone, lost, id)
	return err
}

// entry is one journal line to write.
type entry struct {
	Day, Now         int64
	Kind             string
	Account, Counter string
	Resource         allowance.Resource
	Bucket           allowance.Bucket
	LotID, Amount    int64
	HoldID           string
	Ref              Ref
	Version          int64
	Detail           string
}

func journal(ctx context.Context, q allowance.Querier, e entry) error {
	_, err := q.ExecContext(ctx, "INSERT INTO ledger_entries(day,created_at,kind,account,counterparty,resource,bucket,lot_id,amount,hold_id,service,op,public_ref,params_version,detail) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		e.Day, e.Now, e.Kind, e.Account, e.Counter, string(e.Resource), string(e.Bucket), e.LotID, e.Amount, e.HoldID, e.Ref.Service, opOf(e.Ref), e.Ref.PublicRef, e.Version, e.Detail)
	return err
}

func opOf(r Ref) string {
	if r.Method != "" {
		return r.Op + ":" + r.Method
	}
	return r.Op
}

// addUsage adds to a subject's daily totals.
func addUsage(ctx context.Context, q allowance.Querier, r allowance.Resource, day int64, subject string, spent, incoming, outgoing int64) error {
	if spent == 0 && incoming == 0 && outgoing == 0 {
		return nil
	}
	_, err := q.ExecContext(ctx, "INSERT INTO allowance_usage(resource,day,subject,spent,incoming,outgoing) VALUES(?,?,?,?,?,?) ON CONFLICT(resource,day,subject) DO UPDATE SET spent=spent+excluded.spent,incoming=incoming+excluded.incoming,outgoing=outgoing+excluded.outgoing",
		string(r), day, subject, spent, incoming, outgoing)
	return err
}

// Usage is a subject's totals on one day.
type Usage struct {
	Day                       int64
	Spent, Incoming, Outgoing int64
}

func loadUsage(ctx context.Context, q allowance.Querier, r allowance.Resource, day int64, subject string) (Usage, error) {
	u := Usage{Day: day}
	err := q.QueryRowContext(ctx, "SELECT spent,incoming,outgoing FROM allowance_usage WHERE resource=? AND day=? AND subject=?", string(r), day, subject).Scan(&u.Spent, &u.Incoming, &u.Outgoing)
	if isNoRows(err) {
		err = nil
	}
	return u, err
}

// Usage lists a subject's daily totals for the last days UTC days (at most
// 31), newest first. Anonymous subjects appear publicly only this way.
func (l *Ledger) Usage(ctx context.Context, q allowance.Querier, subject string, r allowance.Resource, days int, now int64) ([]Usage, error) {
	days = int(clamp(int64(days), 1, 31))
	today := now / 86400
	rows, err := q.QueryContext(ctx, "SELECT day,spent,incoming,outgoing FROM allowance_usage WHERE resource=? AND subject=? AND day>? AND day<=? ORDER BY day DESC LIMIT 31", string(r), subject, today-int64(days), today)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Usage
	for rows.Next() {
		var u Usage
		if err = rows.Scan(&u.Day, &u.Spent, &u.Incoming, &u.Outgoing); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// op is one ledger operation on one (resource, day): the parameters and
// levers read once, the day opened, adjusted and spilled.
type op struct {
	p       *AllowanceParams
	version int64
	rp      *ResourceParams
	lv      allowance.Levers
	r       allowance.Resource
	day     int64
	now     int64
	d, orig dayState
	// hourBound is set by planSpend when the anonymous tier's hourly
	// release (tier4Released) is what left a spend short.
	hourBound bool
}

// refusal is the refusal of a spend planSpend left short: the day's, or,
// when the anonymous tier's hourly release bound it, global_quota_exhausted
// with retry_after at the next hour, when more is released.
func (o *op) refusal(short string, c claimRow) error {
	code := o.shortCode(short, c)
	if code == "global_quota_exhausted" && o.hourBound {
		return &allowance.Err{Code: code, RetryAfter: int(3600 - mod(o.now, 3600))}
	}
	return refuseDay(code, o.now)
}

func (o *op) entry(kind, account string, amount int64) entry {
	return entry{Day: o.day, Now: o.now, Kind: kind, Account: account, Resource: o.r, Amount: amount, Version: o.version}
}

// open is the prelude of every write: OpenDay (lazily, inside the first spend
// of the day), adjust for levers and budget changes, then Spill.
func (l *Ledger) open(ctx context.Context, q allowance.Querier, r allowance.Resource, now int64) (*op, error) {
	p, version, err := l.params(ctx, q, now)
	if err != nil {
		return nil, err
	}
	rp, err := l.resource(p, r)
	if err != nil {
		return nil, err
	}
	lv, err := l.levers(ctx, q, now)
	if err != nil {
		return nil, err
	}
	o := &op{p: p, version: version, rp: rp, lv: lv, r: r, day: now / 86400, now: now}
	d, ok, err := loadDay(ctx, q, r, o.day)
	if err != nil {
		return nil, err
	}
	if !ok {
		prev, err := previousDay(ctx, q, r, o.day)
		if err != nil {
			return nil, err
		}
		d = computeOpen(r, o.day, rp, lv, prev, version, now)
		if err = insertDay(ctx, q, &d); err != nil {
			return nil, err
		}
	}
	o.d, o.orig = d, d
	if adjust(&o.d, rp, lv) {
		e := o.entry("lever", "", o.d.BudgetEffective)
		e.Detail = fmt.Sprintf("pools adjusted: budget %d, effective %d, unallocated %d", o.d.Budget, o.d.BudgetEffective, o.d.Unallocated)
		if err = journal(ctx, q, e); err != nil {
			return nil, err
		}
	}
	for _, ev := range spill(&o.d, rp, now) {
		e := o.entry("spill", "", ev.Amount)
		e.Detail = fmt.Sprintf("tier %d to tier %d at %d", ev.Tier, ev.Tier+1, ev.At)
		if err = journal(ctx, q, e); err != nil {
			return nil, err
		}
	}
	return o, nil
}

func (o *op) save(ctx context.Context, q allowance.Querier) error {
	return saveDay(ctx, q, &o.d, &o.orig)
}

// claim is the implicit claim: the first spend by s on this day classifies
// it, issues its share as a free lot and journals it. The share is an
// entitlement: nothing is drawn from the pools until it is spent. A subject
// has at most one claim per day and resource; the tier is fixed here.
func (l *Ledger) claim(ctx context.Context, q allowance.Querier, o *op, s allowance.Subject) (claimRow, error) {
	c, ok, err := loadClaim(ctx, q, o.r, o.day, s.ID)
	if err != nil || ok {
		return c, err
	}
	st, err := l.classify(ctx, q, s, o.now)
	if err != nil {
		return c, err
	}
	rg, err := rootGranted(ctx, q, o.r, o.day, st.Root)
	if err != nil {
		return c, err
	}
	t := int(st.Tier)
	ent := entitlement(&o.d, o.rp, o.lv, st, rg)
	o.d.Pools[t].ClaimedWeight += st.WeightPPM
	o.d.Pools[t].Claimants++
	c = claimRow{Tier: t, Weight: st.WeightPPM, Root: st.Root, Entitlement: ent, Granted: ent, Source: st.Source}
	if ent > 0 {
		exp, hl := freeExpiry(o.p, t, o.day)
		c.LotID, err = insertLot(ctx, q, o.r, lot{Account: s.ID, Bucket: allowance.Free, OriginTier: int64(t), OriginAccount: s.ID, IssuedDay: o.day, ExpiresAt: exp, HalfLife: hl, DecayedDay: o.day, Initial: ent, Remaining: ent}, o.now)
		if err != nil {
			return c, err
		}
	}
	if _, err = q.ExecContext(ctx, "INSERT INTO allowance_claims(resource,day,subject,tier,weight,root,entitlement,granted,source,lot_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)",
		string(o.r), o.day, s.ID, t, c.Weight, c.Root, c.Entitlement, c.Granted, c.Source, c.LotID, o.now); err != nil {
		return c, err
	}
	e := o.entry("claim", s.ID, ent)
	e.Bucket, e.LotID = allowance.Free, c.LotID
	e.Detail = fmt.Sprintf("tier %d, entitlement %d", t, ent)
	if err = journal(ctx, q, e); err != nil {
		return c, err
	}
	if s.Signed {
		err = l.dormantCheck(ctx, q, o, s.ID)
	}
	return c, err
}

// capOwn applies a cap lowered after today's claim to the unspent part of the
// claim too: the subject's own claim lot yields at most min(entitlement, cap)
// less what it already gave (spent or transferred). Only the in-memory copy
// changes (its spendable part shrinks); nothing is written.
func (o *op) capOwn(lots []lot, c claimRow) {
	if c.LotID == 0 || c.Tier < 1 || c.Tier > 4 {
		return
	}
	limit := min(c.Entitlement, o.rp.Cap[c.Tier-1])
	for i := range lots {
		if x := &lots[i]; x.ID == c.LotID {
			if allowed := max(0, limit-(x.Initial-x.Remaining)); x.spendable() > allowed {
				x.Held = x.Remaining - allowed
			}
		}
	}
}

// shortCode refines a refusal for a subject whose share today is zero only
// because its tier had no water when it claimed: that is the shared budget
// running out (global_quota_exhausted), not its own share being used up. A
// share a lever set to zero stays quota_exhausted.
func (o *op) shortCode(short string, c claimRow) string {
	if short != "quota_exhausted" || c.Granted != 0 || c.Tier < 1 || c.Tier > 4 || o.rp.Cap[c.Tier-1] == 0 {
		return short
	}
	if c.Tier == 4 && anonymousOff(o.r, o.lv) || c.Tier == 3 && o.lv.ProvenOnly {
		return short
	}
	return "global_quota_exhausted"
}

// planSpend takes units from lots in spend order (from those ok accepts) on a
// copy of the day. Free units are drawn from the pools of the lot's origin
// tier as they are taken (overbooking: only actual spend counts against the
// day's budget); a lot whose tier has run dry yields what the pools still
// hold, and the spend moves on to the next bucket. short is the refusal when
// the lots cannot cover units: global_quota_exhausted when dry pools stood in
// the way, quota_exhausted when the subject's own units are used up.
func (o *op) planSpend(lots []lot, units int64, ok func(lot) bool) (parts []part, d dayState, short string) {
	d = o.d
	var got int64
	dry := false
	o.hourBound = false
	for _, x := range lots {
		if got >= units {
			break
		}
		if ok != nil && !ok(x) {
			continue
		}
		take := min(units-got, x.spendable())
		if take <= 0 {
			continue
		}
		if x.Bucket == allowance.Free {
			t := int(x.OriginTier)
			can := drawable(&d, o.rp, t)
			hourly := false
			if t == 4 {
				room, byHour := tier4Room(&d, o.rp, o.lv, o.now)
				if room < can {
					can, hourly = room, byHour
				}
			}
			if can < take {
				take, dry = can, true
				o.hourBound = o.hourBound || hourly
			}
			if take <= 0 {
				continue
			}
			draw(&d, o.rp, t, take)
		}
		parts = append(parts, part{Lot: x, Take: take})
		got += take
	}
	if got < units {
		if dry {
			return nil, o.d, "global_quota_exhausted"
		}
		return nil, o.d, "quota_exhausted"
	}
	return parts, d, ""
}

// dormantCheck trips the breaker on a signed account's first claim after
// dormant_days without one.
func (l *Ledger) dormantCheck(ctx context.Context, q allowance.Querier, o *op, account string) error {
	var last *int64
	if err := q.QueryRowContext(ctx, "SELECT max(day) FROM allowance_claims WHERE resource=? AND subject=? AND day<?", string(o.r), account, o.day).Scan(&last); err != nil {
		return err
	}
	if last == nil || o.day-*last <= o.p.DormantDays {
		return nil
	}
	return tripBreaker(ctx, q, o.p, account, "dormant", "", o.now)
}

// sweepOwn expires the subject's own due lots (at most LotsPerAccount).
func (l *Ledger) sweepOwn(ctx context.Context, q allowance.Querier, o *op, account string) error {
	rows, err := q.QueryContext(ctx, "SELECT id,bucket,remaining-held FROM ledger_lots WHERE account=? AND resource=? AND state='live' AND expires_at>0 AND expires_at<=? AND remaining>held LIMIT ?", account, string(o.r), o.now, LotsPerAccount)
	if err != nil {
		return err
	}
	type due struct {
		id, amount int64
		bucket     string
	}
	var list []due
	for rows.Next() {
		var d due
		if err = rows.Scan(&d.id, &d.bucket, &d.amount); err != nil {
			rows.Close()
			return err
		}
		list = append(list, d)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, d := range list {
		if err = expireLot(ctx, q, o.r, account, d.id, allowance.Bucket(d.bucket), d.amount, o.version, o.now); err != nil {
			return err
		}
	}
	return nil
}

// expireLot expires a lot's unheld remainder; held units stay until their
// hold settles.
func expireLot(ctx context.Context, q allowance.Querier, r allowance.Resource, account string, id int64, b allowance.Bucket, amount, version, now int64) error {
	if _, err := q.ExecContext(ctx, "UPDATE ledger_lots SET remaining=held,state=CASE WHEN held=0 THEN 'expired' ELSE state END WHERE id=?", id); err != nil {
		return err
	}
	return journal(ctx, q, entry{Day: now / 86400, Now: now, Kind: "expire", Account: account, Resource: r, Bucket: b, LotID: id, Amount: amount, Version: version})
}

// debit takes units from the subject's lots for a spend or fee, drawing the
// free part from the day's pools: refusals are quota_exhausted (its own share
// is used up) or global_quota_exhausted (its tier's water, borrowing included,
// has run dry, or the day's spend ceiling).
func (l *Ledger) debit(ctx context.Context, q allowance.Querier, o *op, s allowance.Subject, c claimRow, units int64, kind string, ref Ref) error {
	if units == 0 {
		return nil
	}
	lots, err := liveLots(ctx, q, s.ID, o.r, o.now)
	if err != nil {
		return err
	}
	o.capOwn(lots, c)
	parts, d, short := o.planSpend(lots, units, nil)
	if short != "" {
		return o.refusal(short, c)
	}
	np := nonpaid(parts)
	if o.d.SpentNonpaid+np > o.rp.SpendCeiling {
		return refuseDay("global_quota_exhausted", o.now)
	}
	if err = l.clientSpend(ctx, q, o, s, c, units); err != nil {
		return err
	}
	for _, p := range parts {
		if err = takeFrom(ctx, q, p.Lot.ID, p.Take, 0, 0); err != nil {
			return err
		}
		e := o.entry(kind, s.ID, p.Take)
		e.Bucket, e.LotID, e.Ref = p.Lot.Bucket, p.Lot.ID, ref
		if err = journal(ctx, q, e); err != nil {
			return err
		}
	}
	o.d = d
	o.d.SpentNonpaid += np
	o.d.SpentPaid += units - np
	return addUsage(ctx, q, o.r, o.day, s.ID, units, 0, 0)
}

// clientSpend narrows an anonymous prefix's allowance per client signature:
// one client spends at most client_share_ppm of the subject's entitlement.
func (l *Ledger) clientSpend(ctx context.Context, q allowance.Querier, o *op, s allowance.Subject, c claimRow, units int64) error {
	if s.Client == "" || o.rp.ClientSharePPM >= ppm {
		return nil
	}
	var spent int64
	err := q.QueryRowContext(ctx, "SELECT spent FROM allowance_client_spend WHERE resource=? AND day=? AND subject=? AND client=?", string(o.r), o.day, s.ID, s.Client).Scan(&spent)
	if err != nil && !isNoRows(err) {
		return err
	}
	if spent+units > mulDiv(c.Entitlement, o.rp.ClientSharePPM, ppm) {
		return refuseDay("quota_exhausted", o.now)
	}
	_, err = q.ExecContext(ctx, "INSERT INTO allowance_client_spend(resource,day,subject,client,spent) VALUES(?,?,?,?,?) ON CONFLICT(resource,day,subject,client) DO UPDATE SET spent=spent+excluded.spent", string(o.r), o.day, s.ID, s.Client, units)
	return err
}

// Spend is Reserve and Commit in the caller's transaction (§2.5): open the
// day, spill, sweep the subject's expired lots, claim implicitly, then take
// units in spend order. A refusal leaves the day's state consistent, and the
// caller's rollback undoes the claim with the rest of the command.
func (l *Ledger) Spend(ctx context.Context, q allowance.Querier, s allowance.Subject, r allowance.Resource, units int64, ref Ref, now int64) (Receipt, error) {
	if units < 0 || units > maxUnits {
		return Receipt{}, refuse("invalid_amount")
	}
	if s.ID == "" {
		return Receipt{}, errors.New("ledger: spend without a subject")
	}
	o, err := l.open(ctx, q, r, now)
	if err != nil {
		return Receipt{}, err
	}
	if err = l.sweepOwn(ctx, q, o, s.ID); err != nil {
		return Receipt{}, err
	}
	c, err := l.claim(ctx, q, o, s)
	if err != nil {
		return Receipt{}, err
	}
	refusal := l.debit(ctx, q, o, s, c, units, "spend", ref)
	if !isRefusal(refusal) && refusal != nil {
		return Receipt{}, refusal
	}
	if err = o.save(ctx, q); err != nil {
		return Receipt{}, err
	}
	if refusal != nil {
		return Receipt{}, refusal
	}
	return Receipt{Resource: r, Used: units, ParamsVersion: o.version}, nil
}

func isRefusal(err error) bool {
	var e *allowance.Err
	return errors.As(err, &e)
}

// Balance reads a subject's allowance without writing anything. Before the
// day's first spend it shows the prospective share: what a claim now would
// issue.
func (l *Ledger) Balance(ctx context.Context, q allowance.Querier, s allowance.Subject, r allowance.Resource, now int64) (Balance, error) {
	p, version, err := l.params(ctx, q, now)
	if err != nil {
		return Balance{}, err
	}
	rp, err := l.resource(p, r)
	if err != nil {
		return Balance{}, err
	}
	day := now / 86400
	b := Balance{Resource: r, ResetsAt: (day + 1) * 86400, ParamsVersion: version, ByBucket: map[allowance.Bucket]int64{}}
	c, ok, err := loadClaim(ctx, q, r, day, s.ID)
	if err != nil {
		return Balance{}, err
	}
	if ok {
		// The tier is fixed at the claim; the classifier only adds its public
		// reason (a read).
		b.Standing = allowance.Standing{Tier: allowance.Tier(c.Tier), WeightPPM: c.Weight, Root: c.Root, Source: c.Source}
		if st, err := l.classify(ctx, q, s, now); err == nil && int(st.Tier) == c.Tier {
			b.Standing.Reason = st.Reason
		}
		b.Entitlement = min(c.Granted, rp.Cap[c.Tier-1])
	} else {
		lv, err := l.levers(ctx, q, now)
		if err != nil {
			return Balance{}, err
		}
		st, err := l.classify(ctx, q, s, now)
		if err != nil {
			return Balance{}, err
		}
		d, opened, err := loadDay(ctx, q, r, day)
		if err != nil {
			return Balance{}, err
		}
		if !opened {
			prev, err := previousDay(ctx, q, r, day)
			if err != nil {
				return Balance{}, err
			}
			d = computeOpen(r, day, rp, lv, prev, version, now)
		}
		adjust(&d, rp, lv)
		spill(&d, rp, now)
		rg, err := rootGranted(ctx, q, r, day, st.Root)
		if err != nil {
			return Balance{}, err
		}
		b.Standing = st
		b.Entitlement = entitlement(&d, rp, lv, st, rg)
		b.Prospective = true
	}
	lots, err := liveLots(ctx, q, s.ID, r, now)
	if err != nil {
		return Balance{}, err
	}
	if !b.Prospective {
		(&op{rp: rp}).capOwn(lots, c)
	}
	for _, x := range lots {
		b.Remaining += x.spendable()
		b.ByBucket[x.Bucket] += x.spendable()
	}
	if b.Prospective && b.Entitlement > 0 {
		b.Remaining += b.Entitlement
		b.ByBucket[allowance.Free] += b.Entitlement
	}
	u, err := loadUsage(ctx, q, r, day, s.ID)
	if err != nil {
		return Balance{}, err
	}
	b.Used, b.Incoming, b.Outgoing = u.Spent, u.Incoming, u.Outgoing
	return b, nil
}

// Mint issues a granted, earned or paid lot. Granted and earned units are
// drawn from the day's tier-0 pool, so issuance stays within the budget;
// paid units come from a funding adapter and never decay. Reason is public.
func (l *Ledger) Mint(ctx context.Context, q allowance.Querier, account string, r allowance.Resource, b allowance.Bucket, units int64, reason string, now int64) error {
	if units <= 0 || units > maxUnits || !slices.Contains([]allowance.Bucket{allowance.Granted, allowance.Earned, allowance.Paid}, b) {
		return refuse("invalid_amount")
	}
	if account == "" || reason == "" || len(reason) > 512 {
		return errors.New("ledger: mint needs an account and a public reason of 1–512 bytes")
	}
	o, err := l.open(ctx, q, r, now)
	if err != nil {
		return err
	}
	x := lot{Account: account, Bucket: b, OriginTier: 0, OriginAccount: account, IssuedDay: o.day, DecayedDay: o.day, Initial: units, Remaining: units}
	kind := "topup"
	switch b {
	case allowance.Granted, allowance.Earned:
		if o.d.Pools[0].avail() < units {
			if err = o.save(ctx, q); err != nil {
				return err
			}
			return refuse("global_quota_exhausted")
		}
		o.d.Pools[0].Claimed += units
		x.HalfLife, kind = o.p.GrantedHalfLifeDays, "grant"
		if b == allowance.Earned {
			x.HalfLife, kind = o.p.EarnedHalfLifeDays, "earn"
		}
	}
	id, err := creditLot(ctx, q, r, x, now)
	if err != nil {
		return err
	}
	e := o.entry(kind, account, units)
	e.Bucket, e.LotID, e.Detail = b, id, reason
	if err = journal(ctx, q, e); err != nil {
		return err
	}
	return o.save(ctx, q)
}
