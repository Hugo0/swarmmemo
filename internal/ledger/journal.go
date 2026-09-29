package ledger

// The public journal and the waterfall statistics (RFC0012 §8.2, §11).
// Public ledger rules: anonymous subjects appear only as daily totals per
// pseudonym (Usage), never as journal lines; references appear only when the
// caller passed a public one; hold ids and request keys are never shown.

import (
	"context"
	"math"
	"strings"

	"swarmmemo/internal/allowance"
)

const entryCols = "seq,day,created_at,kind,account,counterparty,resource,bucket,amount,service,op,public_ref,params_version,detail"

// Journal reads the public journal, newest first. next is the cursor for the
// following page (0 when there is none). Without an account the scan covers
// at most journalScanMax lines per page, skipping anonymous subjects' lines.
func (l *Ledger) Journal(ctx context.Context, q allowance.Querier, query JournalQuery) ([]Entry, int64, error) {
	limit := query.Limit
	if limit <= 0 || limit > JournalPageMax {
		limit = JournalPageMax
	}
	before := query.Before
	if before <= 0 {
		before = math.MaxInt64
	}
	if strings.HasPrefix(query.Account, "anon:") {
		return nil, 0, nil
	}
	var sqlText string
	var args []any
	if query.Account != "" {
		sqlText = "SELECT " + entryCols + " FROM ledger_entries WHERE account=? AND seq<? ORDER BY seq DESC LIMIT ?"
		args = []any{query.Account, before, limit}
	} else {
		sqlText = "SELECT " + entryCols + " FROM (SELECT " + entryCols + " FROM ledger_entries WHERE seq<? ORDER BY seq DESC LIMIT ?) WHERE account NOT LIKE 'anon:%' ORDER BY seq DESC LIMIT ?"
		args = []any{before, journalScanMax, limit}
	}
	rows, err := q.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		var r, b string
		if err = rows.Scan(&e.Seq, &e.Day, &e.CreatedAt, &e.Kind, &e.Account, &e.Counterparty, &r, &b, &e.Amount, &e.Service, &e.Op, &e.PublicRef, &e.ParamsVersion, &e.Detail); err != nil {
			return nil, 0, err
		}
		e.Resource, e.Bucket = allowance.Resource(r), allowance.Bucket(b)
		out = append(out, e)
	}
	if err = rows.Err(); err != nil {
		return nil, 0, err
	}
	var next int64
	if len(out) == limit {
		next = out[len(out)-1].Seq
	} else if query.Account == "" {
		// The scan window may have been all anonymous lines: continue below it.
		var lowest, scanned int64
		if err = q.QueryRowContext(ctx, "SELECT coalesce(min(seq),0),count(*) FROM (SELECT seq FROM ledger_entries WHERE seq<? ORDER BY seq DESC LIMIT ?)", before, journalScanMax).Scan(&lowest, &scanned); err != nil {
			return nil, 0, err
		}
		if scanned == journalScanMax {
			next = lowest
		}
	}
	return out, next, nil
}

// RecordShadow journals a shadow-mode disagreement (§3.3): the legacy path
// and the ledger decided one spend differently. detail names both decisions.
func (l *Ledger) RecordShadow(ctx context.Context, q allowance.Querier, account string, r allowance.Resource, amount int64, op, detail string, now int64) error {
	_, version, err := l.params(ctx, q, now)
	if err != nil {
		return err
	}
	return journal(ctx, q, entry{Day: now / 86400, Now: now, Kind: "shadow", Account: account, Resource: r, Amount: amount, Ref: Ref{Service: "board", Op: op}, Version: version, Detail: detail})
}

// ReservedAbove is the unspent water today in the pools of the tiers above
// the subject's: reserves the waterfall keeps for higher tiers, which the
// legacy rows would hand to anyone.
func (l *Ledger) ReservedAbove(ctx context.Context, q allowance.Querier, s allowance.Subject, r allowance.Resource, now int64) (int64, error) {
	st, err := l.classify(ctx, q, s, now)
	if err != nil {
		return 0, err
	}
	d, ok, err := loadDay(ctx, q, r, now/86400)
	if err != nil || !ok {
		return 0, err
	}
	var reserved int64
	for t := 1; t < int(st.Tier); t++ {
		reserved += max(0, d.Pools[t].avail())
	}
	return reserved, nil
}

// Stats reads the waterfall state for the last days UTC days (at most 31),
// newest first, with today's transfers and today's spending by service.
func (l *Ledger) Stats(ctx context.Context, q allowance.Querier, days int, now int64) (Stats, error) {
	days = int(clamp(int64(days), 1, 31))
	today := now / 86400
	from := today - int64(days) + 1
	rows, err := q.QueryContext(ctx, "SELECT resource,day FROM allowance_days WHERE day>=? AND day<=? ORDER BY day DESC,resource LIMIT 128", from, today)
	if err != nil {
		return Stats{}, err
	}
	type key struct {
		r   string
		day int64
	}
	var keys []key
	for rows.Next() {
		var k key
		if err = rows.Scan(&k.r, &k.day); err != nil {
			rows.Close()
			return Stats{}, err
		}
		keys = append(keys, k)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return Stats{}, err
	}
	var st Stats
	var todayBudget int64
	for _, k := range keys {
		d, ok, err := loadDay(ctx, q, allowance.Resource(k.r), k.day)
		if err != nil || !ok {
			return Stats{}, err
		}
		ds := DayStats{Resource: d.Resource, Day: d.Day, Budget: d.Budget, BudgetEffective: d.BudgetEffective, Unallocated: d.Unallocated,
			Spent: d.SpentNonpaid + d.SpentPaid, SpentPaid: d.SpentPaid, ParamsVersion: d.ParamsVersion, Buckets: map[allowance.Bucket]int64{}}
		for t, p := range d.Pools {
			ds.Tiers = append(ds.Tiers, PoolStats{Tier: t, Size: p.Size, Want: p.Want, SpillIn: p.SpillIn, SpillOut: p.SpillOut, Claimed: p.Claimed, Lent: p.Lent, Borrowed: p.Borrowed, Claimants: p.Claimants})
		}
		if err = bucketsInUse(ctx, q, &ds); err != nil {
			return Stats{}, err
		}
		if d.Resource == allowance.PostBytes && d.Day == today {
			todayBudget = d.Budget
		}
		st.Days = append(st.Days, ds)
	}
	// Today's transfers of post_bytes, and the largest recipient's share of
	// the day's budget.
	st.Transfers.Resource = allowance.PostBytes
	if err = q.QueryRowContext(ctx, "SELECT count(*),coalesce(sum(amount),0),coalesce(sum(state='pending'),0) FROM ledger_transfers WHERE created_at>=? AND created_at<? AND resource=?", today*86400, (today+1)*86400, string(allowance.PostBytes)).
		Scan(&st.Transfers.Count, &st.Transfers.Volume, &st.Transfers.Pending); err != nil {
		return Stats{}, err
	}
	var top int64
	if err = q.QueryRowContext(ctx, "SELECT coalesce(max(total),0) FROM (SELECT sum(amount) AS total FROM ledger_transfers WHERE created_at>=? AND created_at<? AND resource=? AND state<>'cancelled' GROUP BY to_account)", today*86400, (today+1)*86400, string(allowance.PostBytes)).Scan(&top); err != nil {
		return Stats{}, err
	}
	if todayBudget > 0 {
		st.Transfers.LargestRecipientSharePPM = mulDiv(top, ppm, todayBudget)
	}
	// Today's spending by service and bucket (spend, commit and fee lines).
	rows, err = q.QueryContext(ctx, "SELECT service,resource,bucket,coalesce(sum(amount),0),count(*) FROM ledger_entries WHERE day=? AND kind IN ('spend','commit','fee') GROUP BY service,resource,bucket ORDER BY 4 DESC LIMIT 64", today)
	if err != nil {
		return Stats{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var sv ServiceStats
		var r, b string
		if err = rows.Scan(&sv.Service, &r, &b, &sv.Units, &sv.Calls); err != nil {
			return Stats{}, err
		}
		sv.Resource, sv.Bucket = allowance.Resource(r), allowance.Bucket(b)
		st.Services = append(st.Services, sv)
	}
	return st, rows.Err()
}

// bucketsInUse sums the day's spends by bucket (spend, commit and fee lines).
func bucketsInUse(ctx context.Context, q allowance.Querier, ds *DayStats) error {
	rows, err := q.QueryContext(ctx, "SELECT bucket,coalesce(sum(amount),0) FROM ledger_entries WHERE day=? AND kind IN ('spend','commit','fee') AND resource=? GROUP BY bucket LIMIT 4", ds.Day, string(ds.Resource))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var b string
		var n int64
		if err = rows.Scan(&b, &n); err != nil {
			return err
		}
		ds.Buckets[allowance.Bucket(b)] = n
	}
	return rows.Err()
}
