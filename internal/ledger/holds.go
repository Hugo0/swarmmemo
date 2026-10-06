package ledger

// Metering for remote and async calls (RFC0012 §2.5): Reserve holds units in
// the command's transaction; Commit settles at used <= max and refunds the
// rest to the lots it came from; Refund releases everything. A hold counts in
// the day's spend, and its free part is drawn from the pools, from the moment
// it is reserved, so the budget and the spend ceiling hold while calls are
// open; what is refunded goes back to the pools of the day it was drawn. The
// sweeper settles an expired hold at its maximum.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"swarmmemo/internal/allowance"
)

// ledgerService marks the holds that back pending transfers and escrows
// (escrow.go); they are settled by the transfer or the escrow, never by
// Commit, Refund or hold expiry.
const ledgerService = "ledger"

// Reserve holds up to max units for a remote or async call; ttl is in seconds.
// Holds are unique on (account, requestKey): a retry while the hold is open is
// request_in_flight with retry_after; once settled it returns the hold as it
// ended with hold_not_held (the call's stored result answers such a retry).
// At most HoldsPerAccount open holds per account and HoldsTotal in all; a
// deposit (Config.Deposits) is its own class, at most DepositHoldsPerAccount
// per account, and never counts against the calls' holds: a day-long deposit
// must not crowd paid calls out.
func (l *Ledger) Reserve(ctx context.Context, q allowance.Querier, s allowance.Subject, r allowance.Resource, max int64, requestKey string, ref Ref, ttl int64, now int64) (Hold, error) {
	if max <= 0 || max > maxUnits {
		return Hold{}, refuse("invalid_amount")
	}
	if s.ID == "" || requestKey == "" || len(requestKey) > 256 || ttl < 1 || ttl > 86400 || ref.Service == ledgerService {
		return Hold{}, errors.New("ledger: reserve needs a subject, a request key of 1–256 bytes, a ttl of 1–86400 s and a service")
	}
	id := holdID(s.ID, requestKey)
	if h, ok, err := loadHold(ctx, q, id); err != nil || ok {
		if err == nil && h.State == "held" {
			err = &allowance.Err{Code: "request_in_flight", RetryAfter: int(clamp(h.ExpiresAt-now, 1, 86400))}
		} else if err == nil {
			err = refuse("hold_not_held")
		}
		return h, err
	}
	if slices.Contains(l.cfg.Deposits, ref.Service) {
		var mine int
		if err := q.QueryRowContext(ctx, "SELECT count(*) FROM ledger_holds WHERE state='held' AND account=? AND service=?", s.ID, ref.Service).Scan(&mine); err != nil {
			return Hold{}, err
		}
		if mine >= DepositHoldsPerAccount {
			return Hold{}, refuse("hold_limit")
		}
	} else {
		args := []any{s.ID, ledgerService}
		for _, d := range l.cfg.Deposits {
			args = append(args, d)
		}
		var open, mine int
		if err := q.QueryRowContext(ctx, "SELECT count(*),coalesce(sum(account=?),0) FROM ledger_holds WHERE state='held' AND service NOT IN (?"+strings.Repeat(",?", len(l.cfg.Deposits))+")", args...).Scan(&open, &mine); err != nil {
			return Hold{}, err
		}
		if open >= HoldsTotal || mine >= HoldsPerAccount {
			return Hold{}, refuse("hold_limit")
		}
	}
	o, err := l.open(ctx, q, r, now)
	if err != nil {
		return Hold{}, err
	}
	if err = l.sweepOwn(ctx, q, o, s.ID); err != nil {
		return Hold{}, err
	}
	c, err := l.claim(ctx, q, o, s)
	if err != nil {
		return Hold{}, err
	}
	h := Hold{ID: id, Account: s.ID, Resource: r, Max: max, ExpiresAt: now + ttl, State: "held"}
	refusal := l.hold(ctx, q, o, s, c, h, requestKey, ref)
	if refusal != nil && !isRefusal(refusal) {
		return Hold{}, refusal
	}
	if err = o.save(ctx, q); err != nil {
		return Hold{}, err
	}
	if refusal != nil {
		return Hold{}, refusal
	}
	return h, nil
}

func (l *Ledger) hold(ctx context.Context, q allowance.Querier, o *op, s allowance.Subject, c claimRow, h Hold, requestKey string, ref Ref) error {
	lots, err := liveLots(ctx, q, s.ID, o.r, o.now)
	if err != nil {
		return err
	}
	o.capOwn(lots, c)
	parts, d, short := o.planSpend(lots, h.Max, nil)
	if short != "" {
		return o.refusal(short, c)
	}
	np := nonpaid(parts)
	if o.d.SpentNonpaid+np > o.rp.SpendCeiling {
		return refuseDay("global_quota_exhausted", o.now)
	}
	if err = l.credentialCheck(ctx, q, o, s, h.Max); err != nil {
		return err
	}
	if err = l.clientSpend(ctx, q, o, s, c, h.Max); err != nil {
		return err
	}
	if err = credentialRecord(ctx, q, o, s, h.Max, h.ID); err != nil {
		return err
	}
	if _, err = q.ExecContext(ctx, "INSERT INTO ledger_holds(id,account,resource,max_units,used_units,state,request_key,service,method,created_at,expires_at) VALUES(?,?,?,?,0,'held',?,?,?,?,?)",
		h.ID, s.ID, string(o.r), h.Max, requestKey, ref.Service, ref.Method, o.now, h.ExpiresAt); err != nil {
		return err
	}
	if err = holdParts(ctx, q, o, s.ID, h.ID, parts, ref); err != nil {
		return err
	}
	o.d = d
	o.d.SpentNonpaid += np
	o.d.SpentPaid += h.Max - np
	return addUsage(ctx, q, o.r, o.day, s.ID, h.Max, 0, 0)
}

// holdParts marks parts of lots as held by a hold and journals the reserve.
func holdParts(ctx context.Context, q allowance.Querier, o *op, account, id string, parts []part, ref Ref) error {
	for _, p := range parts {
		if _, err := q.ExecContext(ctx, "UPDATE ledger_lots SET held=held+? WHERE id=?", p.Take, p.Lot.ID); err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, "INSERT INTO ledger_hold_parts(hold_id,lot_id,units) VALUES(?,?,?)", id, p.Lot.ID, p.Take); err != nil {
			return err
		}
		e := o.entry("reserve", account, p.Take)
		e.Bucket, e.LotID, e.HoldID, e.Ref = p.Lot.Bucket, p.Lot.ID, id, ref
		if err := journal(ctx, q, e); err != nil {
			return err
		}
	}
	return nil
}

type holdRow struct {
	Hold
	Used      int64
	CreatedAt int64
	Service   string
	Method    string
}

func loadHold(ctx context.Context, q allowance.Querier, id string) (Hold, bool, error) {
	h, ok, err := loadHoldRow(ctx, q, id)
	return h.Hold, ok, err
}

func loadHoldRow(ctx context.Context, q allowance.Querier, id string) (holdRow, bool, error) {
	var h holdRow
	var r string
	err := q.QueryRowContext(ctx, "SELECT id,account,resource,max_units,used_units,state,expires_at,created_at,service,method FROM ledger_holds WHERE id=?", id).
		Scan(&h.ID, &h.Account, &r, &h.Max, &h.Used, &h.State, &h.ExpiresAt, &h.CreatedAt, &h.Service, &h.Method)
	if isNoRows(err) {
		return h, false, nil
	}
	h.Resource = allowance.Resource(r)
	return h, err == nil, err
}

// Commit settles a hold at used <= max and refunds the rest to its lots.
// Committing a settled hold again returns how it was settled.
func (l *Ledger) Commit(ctx context.Context, q allowance.Querier, holdID string, used int64, now int64) (Receipt, error) {
	return l.settle(ctx, q, holdID, used, "committed", "", now)
}

// Refund releases a hold in full.
func (l *Ledger) Refund(ctx context.Context, q allowance.Querier, holdID string, reason string, now int64) error {
	_, err := l.settle(ctx, q, holdID, 0, "refunded", reason, now)
	return err
}

// GetHold reads one hold as it stands; ok is false when there is none.
func (l *Ledger) GetHold(ctx context.Context, q allowance.Querier, id string) (Hold, bool, error) {
	return loadHold(ctx, q, id)
}

type holdPart struct {
	LotID, Units, ExpiresAt, OriginTier int64
	Bucket                              allowance.Bucket
}

func loadHoldParts(ctx context.Context, q allowance.Querier, id string) ([]holdPart, error) {
	rows, err := q.QueryContext(ctx, "SELECT hp.lot_id,hp.units,l.expires_at,l.origin_tier,l.bucket FROM ledger_hold_parts hp JOIN ledger_lots l ON l.id=hp.lot_id WHERE hp.hold_id=? ORDER BY "+
		"CASE l.bucket WHEN 'free' THEN 0 WHEN 'granted' THEN 1 WHEN 'earned' THEN 2 ELSE 3 END, CASE WHEN l.expires_at=0 THEN 9223372036854775807 ELSE l.expires_at END, CASE WHEN l.half_life_days=0 THEN 9223372036854775807 ELSE l.half_life_days END, l.issued_day, l.id LIMIT ?", id, liveLotsMax)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []holdPart
	for rows.Next() {
		var p holdPart
		var b string
		if err = rows.Scan(&p.LotID, &p.Units, &p.ExpiresAt, &p.OriginTier, &b); err != nil {
			return nil, err
		}
		p.Bucket = allowance.Bucket(b)
		out = append(out, p)
	}
	return out, rows.Err()
}

// settle ends a service hold: used units are spent in spend order, the rest
// returns to its lots, except that units whose lot expired meanwhile expire
// too. The refunded units leave the day's spend (the day the hold was made).
func (l *Ledger) settle(ctx context.Context, q allowance.Querier, id string, used int64, final, detail string, now int64) (Receipt, error) {
	h, ok, err := loadHoldRow(ctx, q, id)
	if err != nil {
		return Receipt{}, err
	}
	if !ok {
		return Receipt{}, fmt.Errorf("%w: hold %s", ErrNotFound, id)
	}
	if h.State != "held" {
		// Settled already: the receipt says how, and the refusal tells the
		// caller this call settled nothing (the services engine treats the
		// call's outcome as unknown).
		return Receipt{HoldID: id, Resource: h.Resource, Used: h.Used, Refunded: h.Max - h.Used}, refuse("hold_not_held")
	}
	if h.Service == ledgerService {
		return Receipt{}, errors.New("ledger: a transfer's hold is settled by the transfer")
	}
	if used < 0 || used > h.Max {
		return Receipt{}, refuse("invalid_amount")
	}
	_, version, err := l.params(ctx, q, now)
	if err != nil {
		return Receipt{}, err
	}
	parts, err := loadHoldParts(ctx, q, id)
	if err != nil {
		return Receipt{}, err
	}
	day := now / 86400
	ref := Ref{Service: h.Service, Method: h.Method}
	left := used
	var backNonpaid, backPaid int64
	var backFree [5]int64 // free units to return to each origin tier's pools
	for _, p := range parts {
		take := min(p.Units, left)
		left -= take
		back := p.Units - take
		var lost int64
		if p.ExpiresAt != 0 && p.ExpiresAt <= now {
			lost = back
		}
		if err = takeFrom(ctx, q, p.LotID, take, p.Units, lost); err != nil {
			return Receipt{}, err
		}
		for _, j := range []struct {
			kind   string
			amount int64
		}{{"commit", take}, {"refund", back - lost}, {"expire", lost}} {
			if j.amount == 0 {
				continue
			}
			if err = journal(ctx, q, entry{Day: day, Now: now, Kind: j.kind, Account: h.Account, Resource: h.Resource, Bucket: p.Bucket, LotID: p.LotID, Amount: j.amount, HoldID: id, Ref: ref, Version: version, Detail: detail}); err != nil {
				return Receipt{}, err
			}
		}
		if p.Bucket == allowance.Paid {
			backPaid += back
		} else {
			backNonpaid += back
		}
		if p.Bucket == allowance.Free && p.OriginTier >= 1 && p.OriginTier <= 4 {
			backFree[p.OriginTier] += back
		}
	}
	made := h.CreatedAt / 86400
	if err = undrawDay(ctx, q, h.Resource, made, backFree); err != nil {
		return Receipt{}, err
	}
	if backNonpaid+backPaid > 0 {
		if _, err = q.ExecContext(ctx, "UPDATE allowance_days SET spent_nonpaid=spent_nonpaid-?,spent_paid=spent_paid-? WHERE resource=? AND day=?", backNonpaid, backPaid, string(h.Resource), made); err != nil {
			return Receipt{}, err
		}
		if err = addUsage(ctx, q, h.Resource, made, h.Account, -(backNonpaid + backPaid), 0, 0); err != nil {
			return Receipt{}, err
		}
		if err = credentialRelease(ctx, q, id, backNonpaid+backPaid); err != nil {
			return Receipt{}, err
		}
	}
	if _, err = q.ExecContext(ctx, "UPDATE ledger_holds SET state=?,used_units=? WHERE id=?", final, used, id); err != nil {
		return Receipt{}, err
	}
	return Receipt{HoldID: id, Resource: h.Resource, Used: used, Refunded: h.Max - used, ParamsVersion: version}, nil
}

// undrawDay returns a settled hold's unused free units to the pools of the
// day the hold drew them from (overbooking counts only actual spend).
func undrawDay(ctx context.Context, q allowance.Querier, r allowance.Resource, day int64, back [5]int64) error {
	if back == [5]int64{} {
		return nil
	}
	d, ok, err := loadDay(ctx, q, r, day)
	if err != nil || !ok {
		return err
	}
	orig := d
	for t := 1; t <= 4; t++ {
		undraw(&d, t, back[t])
	}
	return saveDay(ctx, q, &d, &orig)
}
