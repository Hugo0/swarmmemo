package ledger

// The sweeper (RFC0012 §2.2, §2.5): expire due lots, settle expired holds at
// their maximum (outcome unknown), execute due pending transfers and apply
// daily demurrage. Each step handles at most limit rows, so one call is one
// short transaction; the board runs it every minute while the ledger is on.

import (
	"context"
	"errors"
	"fmt"

	"swarmmemo/internal/allowance"
)

// Sweep expires lots and holds that are due, executes due transfers and
// decays lots, at most limit rows per step (limit ≤ SweepMax). q must be a
// transaction: a due transfer runs in a savepoint.
func (l *Ledger) Sweep(ctx context.Context, q allowance.Querier, now int64, limit int) (int, error) {
	limit = int(clamp(int64(limit), 1, SweepMax))
	p, version, err := l.params(ctx, q, now)
	if err != nil {
		return 0, err
	}
	n := 0
	// 1. Lots past their expiry: the unheld remainder expires.
	type due struct {
		id, amount int64
		account    string
		resource   string
		bucket     string
	}
	var lots []due
	rows, err := q.QueryContext(ctx, "SELECT id,account,resource,bucket,remaining-held FROM ledger_lots WHERE state='live' AND expires_at>0 AND expires_at<=? AND remaining>held LIMIT ?", now, limit)
	if err != nil {
		return n, err
	}
	for rows.Next() {
		var d due
		if err = rows.Scan(&d.id, &d.account, &d.resource, &d.bucket, &d.amount); err != nil {
			rows.Close()
			return n, err
		}
		lots = append(lots, d)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return n, err
	}
	for _, d := range lots {
		if err = expireLot(ctx, q, allowance.Resource(d.resource), d.account, d.id, allowance.Bucket(d.bucket), d.amount, version, now); err != nil {
			return n, err
		}
		n++
	}
	// 2. Service holds past their TTL settle at their maximum.
	holds, err := ids(ctx, q, "SELECT id FROM ledger_holds WHERE state='held' AND expires_at<=? AND service<>? LIMIT ?", now, ledgerService, limit)
	if err != nil {
		return n, err
	}
	for _, id := range holds {
		h, ok, err := loadHold(ctx, q, id)
		if err != nil || !ok {
			return n, err
		}
		if _, err = l.settle(ctx, q, id, h.Max, "expired", "unknown", now); err != nil {
			return n, err
		}
		n++
	}
	// 3. Due pending transfers execute, unless transfers are frozen.
	lv, err := l.levers(ctx, q, now)
	if err != nil {
		return n, err
	}
	if !lv.FreezeTransfers {
		transfers, err := ids(ctx, q, "SELECT id FROM ledger_transfers WHERE state='pending' AND execute_at<=? ORDER BY execute_at LIMIT ?", now, limit)
		if err != nil {
			return n, err
		}
		for _, id := range transfers {
			if err = l.executeDue(ctx, q, id, now); err != nil {
				return n, err
			}
			n++
		}
	}
	// 4. Daily demurrage on decaying lots.
	today := now / 86400
	var decaying []lot
	rows, err = q.QueryContext(ctx, "SELECT "+lotCols+",resource FROM ledger_lots WHERE state='live' AND half_life_days>0 AND decayed_day<? LIMIT ?", today, limit)
	if err != nil {
		return n, err
	}
	resources := map[int64]string{}
	for rows.Next() {
		var x lot
		var bucket, r string
		if err = rows.Scan(&x.ID, &x.Account, &bucket, &x.OriginTier, &x.OriginAccount, &x.Hops, &x.IssuedDay, &x.ExpiresAt, &x.HalfLife, &x.DecayedDay, &x.Initial, &x.Remaining, &x.Held, &r); err != nil {
			rows.Close()
			return n, err
		}
		x.Bucket = allowance.Bucket(bucket)
		resources[x.ID] = r
		decaying = append(decaying, x)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return n, err
	}
	for _, x := range decaying {
		if err = decay(ctx, q, p, allowance.Resource(resources[x.ID]), x, version, today, now); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func ids(ctx context.Context, q allowance.Querier, query string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// executeDue runs one due transfer in a savepoint; a refusal (the recipient
// cannot hold another lot) cancels it and returns the held units.
func (l *Ledger) executeDue(ctx context.Context, q allowance.Querier, id string, now int64) error {
	t, ok, err := l.loadTransfer(ctx, q, id)
	if err != nil || !ok || t.State != "pending" {
		return err
	}
	if _, err = q.ExecContext(ctx, "SAVEPOINT ledger_transfer"); err != nil {
		return err
	}
	if err = l.execute(ctx, q, t, now); err == nil {
		_, err = q.ExecContext(ctx, "RELEASE ledger_transfer")
		return err
	}
	if _, rerr := q.ExecContext(ctx, "ROLLBACK TO ledger_transfer"); rerr != nil {
		return rerr
	}
	if _, rerr := q.ExecContext(ctx, "RELEASE ledger_transfer"); rerr != nil {
		return rerr
	}
	var refusal *allowance.Err
	if !errors.As(err, &refusal) {
		return err
	}
	return l.release(ctx, q, t, "cancelled", "ledger:"+refusal.Code, now)
}

// decay applies whole days of demurrage since the lot's decay clock:
// floor(remaining × rate_ppm / 1e6) per day, on the unheld units. A lot below
// dust, or older than 8 half-lives, expires.
func decay(ctx context.Context, q allowance.Querier, p *AllowanceParams, r allowance.Resource, x lot, version, today, now int64) error {
	rate := RatePPM(x.HalfLife)
	free := x.spendable()
	var lost int64
	for d := x.DecayedDay; d < today && d-x.DecayedDay <= 8*x.HalfLife; d++ {
		loss := mulDiv(free, rate, ppm)
		free -= loss
		lost += loss
	}
	var expired int64
	if free < p.Dust || today-x.IssuedDay >= 8*x.HalfLife {
		expired, free = free, 0
	}
	gone := lost + expired
	if _, err := q.ExecContext(ctx, "UPDATE ledger_lots SET remaining=remaining-?,decayed_day=?,state=CASE WHEN remaining-?=0 THEN 'expired' ELSE state END WHERE id=?", gone, today, gone, x.ID); err != nil {
		return err
	}
	day := now / 86400
	for _, j := range []struct {
		kind   string
		amount int64
	}{{"decay", lost}, {"expire", expired}} {
		if j.amount == 0 {
			continue
		}
		if err := journal(ctx, q, entry{Day: day, Now: now, Kind: j.kind, Account: x.Account, Resource: r, Bucket: x.Bucket, LotID: x.ID, Amount: j.amount, Version: version, Detail: fmt.Sprintf("half-life %d days", x.HalfLife)}); err != nil {
			return err
		}
	}
	return nil
}
