package ledger

// Transfers, the account-change breaker and taint (RFC0012 §2.4). Every
// bucket moves by default; a moved lot keeps its bucket, expiry, decay clock,
// origin tier and origin account, with hops + 1, so a transfer never makes
// value newer, longer-lived or more liquid. A sender with an active breaker
// waits transfer_delay, and the transfer can be cancelled by the account's
// current key or by the key its rotation replaced.

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"swarmmemo/internal/allowance"
)

// Breaker is one row of account_breakers.
type Breaker struct {
	Seq           int64
	Account       string
	Reason        string // "agent.rotate", "identity.link", "identity.unlink", "dormant", "spike"
	CancelKey     string
	StartedAt     int64
	TransferUntil int64
	TrustUntil    int64
}

// TripBreaker records a breaker trigger (§2.4): transfers from the account
// wait transfer_delay, and the trust module treats it as a newcomer until
// trust_until. cancelKey is the key rotated away, which may cancel a pending
// transfer in the window. Tripping twice at the same instant is one breaker.
func (l *Ledger) TripBreaker(ctx context.Context, q allowance.Querier, account, reason, cancelKey string, now int64) error {
	p, _, err := l.params(ctx, q, now)
	if err != nil {
		return err
	}
	return tripBreaker(ctx, q, p, account, reason, cancelKey, now)
}

func tripBreaker(ctx context.Context, q allowance.Querier, p *AllowanceParams, account, reason, cancelKey string, now int64) error {
	var n int
	if err := q.QueryRowContext(ctx, "SELECT count(*) FROM account_breakers WHERE account=? AND trust_until>=? AND reason=? AND cancel_key=? AND started_at=?", account, now, reason, cancelKey, now).Scan(&n); err != nil || n > 0 {
		return err
	}
	_, err := q.ExecContext(ctx, "INSERT INTO account_breakers(account,reason,cancel_key,started_at,transfer_until,trust_until) VALUES(?,?,?,?,?,?)",
		account, reason, cancelKey, now, now+p.TransferDelay, now+p.TrustResetDays*86400)
	return err
}

// ActiveBreaker is the newest breaker still in effect for transfers or trust.
func (l *Ledger) ActiveBreaker(ctx context.Context, q allowance.Querier, account string, now int64) (Breaker, bool, error) {
	var b Breaker
	err := q.QueryRowContext(ctx, "SELECT seq,account,reason,cancel_key,started_at,transfer_until,trust_until FROM account_breakers WHERE account=? AND (trust_until>? OR transfer_until>?) ORDER BY seq DESC LIMIT 1", account, now, now).
		Scan(&b.Seq, &b.Account, &b.Reason, &b.CancelKey, &b.StartedAt, &b.TransferUntil, &b.TrustUntil)
	if isNoRows(err) {
		return b, false, nil
	}
	return b, err == nil, err
}

func transferBreaker(ctx context.Context, q allowance.Querier, account string, now int64) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, "SELECT count(*) FROM account_breakers WHERE account=? AND transfer_until>?", account, now).Scan(&n)
	return n > 0, err
}

// spike trips the breaker when today's outbound transfers, this one included,
// exceed max(spike_x × the 30-day median, spike_floor).
func spike(ctx context.Context, q allowance.Querier, p *AllowanceParams, account string, day, now int64) error {
	rows, err := q.QueryContext(ctx, "SELECT created_at/86400,count(*) FROM ledger_transfers WHERE from_account=? AND created_at>=? GROUP BY created_at/86400 LIMIT 31", account, (day-30)*86400)
	if err != nil {
		return err
	}
	counts := make([]int64, 30)
	var today int64
	for rows.Next() {
		var d, n int64
		if err = rows.Scan(&d, &n); err != nil {
			rows.Close()
			return err
		}
		if d == day {
			today = n
		} else if i := day - 1 - d; i >= 0 && i < 30 {
			counts[i] = n
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	slices.Sort(counts)
	median := (counts[14] + counts[15]) / 2
	if today+1 > max(p.SpikeX*median, p.SpikeFloor) {
		return tripBreaker(ctx, q, p, account, "spike", "", now)
	}
	return nil
}

func (p *AllowanceParams) transferable(x lot) bool {
	switch x.Bucket {
	case allowance.Free:
		return slices.Contains(p.TransferableFreeTiers, x.OriginTier)
	case allowance.Paid:
		return p.PaidTransferable
	}
	return true
}

// Transfer moves units between two signed accounts (§2.4). The fixed fee is
// spent, not moved; the recipient's inbound per day is capped; with an
// active breaker the transfer is pending, its lots held, until execute_at.
// requestKey makes it idempotent: the same key returns the same transfer.
func (l *Ledger) Transfer(ctx context.Context, q allowance.Querier, from allowance.Subject, to string, r allowance.Resource, amount int64, requestKey string, now int64) (Transfer, error) {
	if !from.Signed || from.ID == "" || to == "" || requestKey == "" {
		return Transfer{}, errors.New("ledger: a transfer needs a signed sender, a recipient and a request key")
	}
	if to == from.ID {
		return Transfer{}, refuse("self_transfer")
	}
	id := transferID(from.ID, requestKey)
	if t, ok, err := l.loadTransfer(ctx, q, id); err != nil || ok {
		return t, err
	}
	o, err := l.open(ctx, q, r, now)
	if err != nil {
		return Transfer{}, err
	}
	if amount <= 0 || amount > maxUnits || o.rp.Budget > 0 && amount > o.rp.Budget {
		return Transfer{}, refuse("invalid_amount")
	}
	if o.lv.FreezeTransfers {
		return Transfer{}, refuse("transfers_frozen")
	}
	t, refusal := l.transfer(ctx, q, o, from, to, amount, id, requestKey)
	if refusal != nil && !isRefusal(refusal) {
		return Transfer{}, refusal
	}
	if err = o.save(ctx, q); err != nil {
		return Transfer{}, err
	}
	return t, refusal
}

func (l *Ledger) transfer(ctx context.Context, q allowance.Querier, o *op, from allowance.Subject, to string, amount int64, id, requestKey string) (Transfer, error) {
	if err := l.sweepOwn(ctx, q, o, from.ID); err != nil {
		return Transfer{}, err
	}
	c, err := l.claim(ctx, q, o, from)
	if err != nil {
		return Transfer{}, err
	}
	ref := Ref{Service: ledgerService, Op: "allowance.transfer", PublicRef: id}
	if err = l.debit(ctx, q, o, from, c, o.rp.TransferFee, "fee", ref); err != nil {
		return Transfer{}, err
	}
	lots, err := liveLots(ctx, q, from.ID, o.r, o.now)
	if err != nil {
		return Transfer{}, err
	}
	o.capOwn(lots, c)
	// Moving units draws nothing: the recipient's spend of a moved free lot
	// draws from the pools of the lot's origin tier.
	parts, got := plan(lots, amount, o.p.transferable)
	if got < amount {
		if _, all := plan(lots, amount, nil); all >= amount {
			return Transfer{}, refuse("not_transferable")
		}
		return Transfer{}, refuseDay("quota_exhausted", o.now)
	}
	in, err := loadUsage(ctx, q, o.r, o.day, to)
	if err != nil {
		return Transfer{}, err
	}
	if in.Incoming+amount > o.rp.InboundCap {
		return Transfer{}, refuse("recipient_limit")
	}
	if err = spike(ctx, q, o.p, from.ID, o.day, o.now); err != nil {
		return Transfer{}, err
	}
	pending, err := transferBreaker(ctx, q, from.ID, o.now)
	if err != nil {
		return Transfer{}, err
	}
	t := Transfer{ID: id, From: from.ID, To: to, Resource: o.r, Amount: amount, Fee: o.rp.TransferFee, State: "done", CreatedAt: o.now, ExecuteAt: o.now, ByBucket: map[allowance.Bucket]int64{}}
	for _, p := range parts {
		t.ByBucket[p.Lot.Bucket] += p.Take
		if p.Lot.ExpiresAt != 0 && (t.ExpiresAt == 0 || p.Lot.ExpiresAt < t.ExpiresAt) {
			t.ExpiresAt = p.Lot.ExpiresAt
		}
	}
	var hold string
	if pending {
		var n int
		if err = q.QueryRowContext(ctx, "SELECT count(*) FROM ledger_transfers WHERE from_account=? AND state='pending' AND created_at>=?", from.ID, o.now-o.p.TransferDelay-86400).Scan(&n); err != nil {
			return Transfer{}, err
		}
		if n >= TransfersPendingMax {
			return Transfer{}, refuse("hold_limit")
		}
		t.State, t.ExecuteAt = "pending", o.now+o.p.TransferDelay
		hold = holdID(from.ID, "transfer:"+requestKey)
		if _, err = q.ExecContext(ctx, "INSERT INTO ledger_holds(id,account,resource,max_units,used_units,state,request_key,service,method,created_at,expires_at) VALUES(?,?,?,?,0,'held',?,?,?,?,?)",
			hold, from.ID, string(o.r), amount, "transfer:"+requestKey, ledgerService, "transfer", o.now, t.ExecuteAt); err != nil {
			return Transfer{}, err
		}
		if err = holdParts(ctx, q, o, from.ID, hold, parts, ref); err != nil {
			return Transfer{}, err
		}
	} else if _, _, err = moveParts(ctx, q, o.r, o.version, from.ID, to, id, parts, false, o.now); err != nil {
		return Transfer{}, err
	}
	doneAt := int64(0)
	if !pending {
		doneAt = o.now
	}
	if _, err = q.ExecContext(ctx, "INSERT INTO ledger_transfers(id,from_account,to_account,resource,amount,fee,hold_id,state,created_at,execute_at,done_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)",
		id, from.ID, to, string(o.r), amount, t.Fee, hold, t.State, o.now, t.ExecuteAt, doneAt); err != nil {
		return Transfer{}, err
	}
	if err = addUsage(ctx, q, o.r, o.day, from.ID, 0, 0, amount); err != nil {
		return Transfer{}, err
	}
	return t, addUsage(ctx, q, o.r, o.day, to, 0, amount, 0)
}

// moveParts moves units from the sender's lots to recipient lots with the
// same bucket, expiry, decay clock and origin, hops + 1. Units of a lot that
// expired meanwhile (a pending transfer's held free units) expire instead.
func moveParts(ctx context.Context, q allowance.Querier, r allowance.Resource, version int64, from, to, id string, parts []part, held bool, now int64) (moved int64, byBucket map[allowance.Bucket]int64, err error) {
	byBucket = map[allowance.Bucket]int64{}
	day := now / 86400
	ref := Ref{Service: ledgerService, Op: "allowance.transfer", PublicRef: id}
	for _, p := range parts {
		released := int64(0)
		if held {
			released = p.Take
		}
		if p.Lot.ExpiresAt != 0 && p.Lot.ExpiresAt <= now {
			if err = takeFrom(ctx, q, p.Lot.ID, 0, released, p.Take); err != nil {
				return
			}
			if err = journal(ctx, q, entry{Day: day, Now: now, Kind: "expire", Account: from, Resource: r, Bucket: p.Lot.Bucket, LotID: p.Lot.ID, Amount: p.Take, Ref: ref, Version: version}); err != nil {
				return
			}
			continue
		}
		if err = takeFrom(ctx, q, p.Lot.ID, p.Take, released, 0); err != nil {
			return
		}
		dst := p.Lot
		dst.Account, dst.Hops, dst.Initial, dst.Remaining, dst.Held = to, p.Lot.Hops+1, p.Take, p.Take, 0
		var lotID int64
		if lotID, err = creditLot(ctx, q, r, dst, now); err != nil {
			return
		}
		if err = journal(ctx, q, entry{Day: day, Now: now, Kind: "transfer_out", Account: from, Counter: to, Resource: r, Bucket: p.Lot.Bucket, LotID: p.Lot.ID, Amount: p.Take, Ref: ref, Version: version}); err != nil {
			return
		}
		if err = journal(ctx, q, entry{Day: day, Now: now, Kind: "transfer_in", Account: to, Counter: from, Resource: r, Bucket: p.Lot.Bucket, LotID: lotID, Amount: p.Take, Ref: ref, Version: version}); err != nil {
			return
		}
		moved += p.Take
		byBucket[p.Lot.Bucket] += p.Take
	}
	return
}

func (l *Ledger) loadTransfer(ctx context.Context, q allowance.Querier, id string) (Transfer, bool, error) {
	var t Transfer
	var r, hold string
	var doneAt int64
	err := q.QueryRowContext(ctx, "SELECT id,from_account,to_account,resource,amount,fee,hold_id,state,created_at,execute_at,done_at FROM ledger_transfers WHERE id=?", id).
		Scan(&t.ID, &t.From, &t.To, &r, &t.Amount, &t.Fee, &hold, &t.State, &t.CreatedAt, &t.ExecuteAt, &doneAt)
	if isNoRows(err) {
		return t, false, nil
	}
	if err != nil {
		return t, false, err
	}
	t.Resource = allowance.Resource(r)
	t.ByBucket = map[allowance.Bucket]int64{}
	// The bucket mix: what moved (done) or what was held (pending, cancelled),
	// read through the day index, so the scan is bounded by that day's lines.
	day, kind := t.CreatedAt/86400, "reserve"
	if t.State == "done" {
		day, kind = doneAt/86400, "transfer_out"
	}
	rows, err := q.QueryContext(ctx, "SELECT bucket,sum(amount) FROM ledger_entries WHERE day=? AND kind=? AND public_ref=? AND account=? GROUP BY bucket LIMIT 4", day, kind, id, t.From)
	if err != nil {
		return t, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var b string
		var n int64
		if err = rows.Scan(&b, &n); err != nil {
			return t, false, err
		}
		t.ByBucket[allowance.Bucket(b)] = n
	}
	return t, true, rows.Err()
}

// GetTransfer reads one transfer.
func (l *Ledger) GetTransfer(ctx context.Context, q allowance.Querier, id string) (Transfer, error) {
	t, ok, err := l.loadTransfer(ctx, q, id)
	if err == nil && !ok {
		err = refuse("transfer_not_found")
	}
	return t, err
}

// CancelTransfer cancels a pending transfer; by is the signing key's
// fingerprint, recorded publicly. The caller has checked that by is the
// sender's current key or the key rotated away by the breaker's rotation
// (RotatedKeyMayCancel). The held units return to the sender's lots; units
// whose lot expired meanwhile expire at the next sweep.
func (l *Ledger) CancelTransfer(ctx context.Context, q allowance.Querier, id string, by string, now int64) (Transfer, error) {
	t, ok, err := l.loadTransfer(ctx, q, id)
	if err != nil {
		return Transfer{}, err
	}
	if !ok {
		return Transfer{}, refuse("transfer_not_found")
	}
	if t.State != "pending" {
		return t, refuse("transfer_not_pending")
	}
	if err = l.release(ctx, q, t, "cancelled", by, now); err != nil {
		return Transfer{}, err
	}
	t.State = "cancelled"
	return t, nil
}

// release returns a pending transfer's held units and ends it.
func (l *Ledger) release(ctx context.Context, q allowance.Querier, t Transfer, state, by string, now int64) error {
	_, version, err := l.params(ctx, q, now)
	if err != nil {
		return err
	}
	var hold string
	if err = q.QueryRowContext(ctx, "SELECT hold_id FROM ledger_transfers WHERE id=?", t.ID).Scan(&hold); err != nil {
		return err
	}
	parts, err := loadHoldParts(ctx, q, hold)
	if err != nil {
		return err
	}
	for _, p := range parts {
		if _, err = q.ExecContext(ctx, "UPDATE ledger_lots SET held=held-? WHERE id=?", p.Units, p.LotID); err != nil {
			return err
		}
		if err = journal(ctx, q, entry{Day: now / 86400, Now: now, Kind: "refund", Account: t.From, Counter: t.To, Resource: t.Resource, Bucket: p.Bucket, LotID: p.LotID, Amount: p.Units, HoldID: hold, Ref: Ref{Service: ledgerService, Op: "allowance.transfer.cancel", PublicRef: t.ID}, Version: version}); err != nil {
			return err
		}
	}
	if _, err = q.ExecContext(ctx, "UPDATE ledger_holds SET state='refunded' WHERE id=?", hold); err != nil {
		return err
	}
	if _, err = q.ExecContext(ctx, "UPDATE ledger_transfers SET state=?,cancelled_by=?,done_at=? WHERE id=?", state, by, now, t.ID); err != nil {
		return err
	}
	made := t.CreatedAt / 86400
	if err = addUsage(ctx, q, t.Resource, made, t.From, 0, 0, -t.Amount); err != nil {
		return err
	}
	return addUsage(ctx, q, t.Resource, made, t.To, 0, -t.Amount, 0)
}

// execute runs a due pending transfer; the sweeper calls it inside a
// savepoint, so a recipient over its lot bound cancels the transfer instead.
func (l *Ledger) execute(ctx context.Context, q allowance.Querier, t Transfer, now int64) error {
	_, version, err := l.params(ctx, q, now)
	if err != nil {
		return err
	}
	var hold string
	if err = q.QueryRowContext(ctx, "SELECT hold_id FROM ledger_transfers WHERE id=?", t.ID).Scan(&hold); err != nil {
		return err
	}
	hp, err := loadHoldParts(ctx, q, hold)
	if err != nil {
		return err
	}
	var parts []part
	for _, p := range hp {
		rows, err := q.QueryContext(ctx, "SELECT "+lotCols+" FROM ledger_lots WHERE id=?", p.LotID)
		if err != nil {
			return err
		}
		var x lot
		found := false
		for rows.Next() {
			x, err = scanLot(rows.Scan)
			found = err == nil
		}
		rows.Close()
		if err != nil || !found {
			return fmt.Errorf("ledger: lot %d of transfer %s: %v", p.LotID, t.ID, err)
		}
		parts = append(parts, part{Lot: x, Take: p.Units})
	}
	moved, _, err := moveParts(ctx, q, t.Resource, version, t.From, t.To, t.ID, parts, true, now)
	if err != nil {
		return err
	}
	if _, err = q.ExecContext(ctx, "UPDATE ledger_holds SET state='committed',used_units=? WHERE id=?", moved, hold); err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, "UPDATE ledger_transfers SET state='done',done_at=? WHERE id=?", now, t.ID)
	return err
}

// RotatedKeyMayCancel reports whether key, rotated away from the sender's
// account, may cancel transfer id: a pending transfer made while the breaker
// its own rotation tripped was active, at any time until the transfer
// executes (§2.4). The window follows the transfer, not the breaker: a thief
// who transfers just before the breaker ends cannot leave the owner seconds
// to object (security review 1.20, M7).
func (l *Ledger) RotatedKeyMayCancel(ctx context.Context, q allowance.Querier, id, key string, now int64) (bool, error) {
	t, ok, err := l.loadTransfer(ctx, q, id)
	if err != nil || !ok || t.State != "pending" || key == "" || now >= t.ExecuteAt {
		return false, err
	}
	var n int
	err = q.QueryRowContext(ctx, "SELECT count(*) FROM account_breakers WHERE account=? AND reason='agent.rotate' AND cancel_key=? AND started_at<=? AND transfer_until>?", t.From, key, t.CreatedAt, t.CreatedAt).Scan(&n)
	return n > 0, err
}
