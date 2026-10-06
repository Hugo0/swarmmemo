package ledger

// Escrow: a transfer whose recipient is named later (work rewards). Escrow
// holds transferable units now, exactly as a pending transfer holds them;
// EscrowPay moves them to the recipient with the transfer's rules (inbound
// cap, spike and account-change breaker: with an active breaker, or with
// transfers frozen, the payment is a pending transfer that the sweeper
// executes and that can be cancelled like any other); EscrowRelease returns
// them. The transfer fee is charged when the units are held, as a transfer
// charges it when it is made, and stays spent when they are released, as a
// cancelled pending transfer's does.
//
// An escrow hold is a ledger hold (service "ledger", method "escrow"): the
// sweeper never settles it on expiry, it does not count against the calls'
// holds, and held units neither decay nor expire while held, because only
// lots that outlive the hold's end are held at all.

import (
	"context"
	"errors"
	"fmt"

	"swarmmemo/internal/allowance"
)

// EscrowMax bounds what one escrow holds.
const EscrowMax = maxUnits

// escrowMethod marks an escrow's hold among the ledger's own holds.
const escrowMethod = "escrow"

// escrowRef is the journal reference of an escrow's lines: the id of the
// transfer that pays it, so the hold, the payment and a release read as one.
func escrowRef(op, transfer string) Ref {
	return Ref{Service: ledgerService, Op: op, PublicRef: transfer}
}

// escrowKeys are an escrow's hold and transfer ids for a request key.
func escrowKeys(account, requestKey string) (hold, transfer string) {
	return holdID(account, "escrow:"+requestKey), transferID(account, "escrow:"+requestKey)
}

// Escrow holds amount transferable units of from's until at most until (a
// unix time, the latest the payment can be made) and charges the transfer
// fee. Only lots that do not expire before until are held, so the units
// can still be paid at until. requestKey names the escrow: the same key
// returns the same hold. The returned fee is what was charged.
func (l *Ledger) Escrow(ctx context.Context, q allowance.Querier, from allowance.Subject, r allowance.Resource, amount int64, requestKey string, until, now int64) (Hold, int64, error) {
	if !from.Signed || from.ID == "" || requestKey == "" || len(requestKey) > 256 || until <= now {
		return Hold{}, 0, errors.New("ledger: an escrow needs a signed account, a request key of 1–256 bytes and an end after now")
	}
	id, transfer := escrowKeys(from.ID, requestKey)
	if h, ok, err := loadHold(ctx, q, id); err != nil || ok {
		return h, 0, err
	}
	o, err := l.open(ctx, q, r, now)
	if err != nil {
		return Hold{}, 0, err
	}
	if amount <= 0 || amount > EscrowMax || o.rp.Budget > 0 && amount > o.rp.Budget {
		return Hold{}, 0, refuse("invalid_amount")
	}
	if o.lv.FreezeTransfers {
		return Hold{}, 0, refuse("transfers_frozen")
	}
	h := Hold{ID: id, Account: from.ID, Resource: r, Max: amount, ExpiresAt: until, State: "held"}
	refusal := l.escrow(ctx, q, o, from, h, requestKey, transfer)
	if refusal != nil && !isRefusal(refusal) {
		return Hold{}, 0, refusal
	}
	if err = o.save(ctx, q); err != nil {
		return Hold{}, 0, err
	}
	if refusal != nil {
		return Hold{}, 0, refusal
	}
	return h, o.rp.TransferFee, nil
}

func (l *Ledger) escrow(ctx context.Context, q allowance.Querier, o *op, from allowance.Subject, h Hold, requestKey, transfer string) error {
	if err := l.sweepOwn(ctx, q, o, from.ID); err != nil {
		return err
	}
	c, err := l.claim(ctx, q, o, from)
	if err != nil {
		return err
	}
	ref := escrowRef("work_reward", transfer)
	// As in transfer: the amount and its fee are one spend for a
	// credential's limit, checked before anything is written.
	if err = l.credentialCheck(ctx, q, o, from, h.Max+o.rp.TransferFee); err != nil {
		return err
	}
	if err = l.debit(ctx, q, o, from, c, o.rp.TransferFee, "fee", ref); err != nil {
		return err
	}
	lots, err := liveLots(ctx, q, from.ID, o.r, o.now)
	if err != nil {
		return err
	}
	o.capOwn(lots, c)
	lasting := func(x lot) bool { return o.p.transferable(x) && (x.ExpiresAt == 0 || x.ExpiresAt > h.ExpiresAt) }
	parts, got := plan(lots, h.Max, lasting)
	if got < h.Max {
		if _, all := plan(lots, h.Max, nil); all >= h.Max {
			return refuse("not_transferable")
		}
		return refuseDay("quota_exhausted", o.now)
	}
	if _, err = q.ExecContext(ctx, "INSERT INTO ledger_holds(id,account,resource,max_units,used_units,state,request_key,service,method,created_at,expires_at) VALUES(?,?,?,?,0,'held',?,?,?,?,?)",
		h.ID, from.ID, string(o.r), h.Max, "escrow:"+requestKey, ledgerService, escrowMethod, o.now, h.ExpiresAt); err != nil {
		return err
	}
	if err = holdParts(ctx, q, o, from.ID, h.ID, parts, ref); err != nil {
		return err
	}
	if err = credentialRecord(ctx, q, o, from, o.rp.TransferFee, ""); err != nil {
		return err
	}
	return credentialRecord(ctx, q, o, from, h.Max, h.ID)
}

// loadEscrow is an escrow's hold row and its transfer id; it refuses a hold
// that is not an escrow.
func loadEscrow(ctx context.Context, q allowance.Querier, id string) (holdRow, string, error) {
	h, ok, err := loadHoldRow(ctx, q, id)
	if err != nil {
		return h, "", err
	}
	if !ok || h.Service != ledgerService || h.Method != escrowMethod {
		return h, "", fmt.Errorf("%w: escrow %s", ErrNotFound, id)
	}
	var key string
	if err = q.QueryRowContext(ctx, "SELECT request_key FROM ledger_holds WHERE id=?", id).Scan(&key); err != nil {
		return h, "", err
	}
	return h, transferID(h.Account, key), nil
}

// EscrowPay pays an escrow's units to to, as a transfer from the account
// that holds them: done at once, or pending (with an active account-change
// breaker, or with transfers frozen) until the sweeper executes it. Paying
// it again returns the same transfer; paying a released escrow is
// hold_not_held. The inbound cap is checked as for any transfer
// (recipient_limit), and nothing changes when it refuses.
func (l *Ledger) EscrowPay(ctx context.Context, q allowance.Querier, holdID, to string, now int64) (Transfer, error) {
	h, id, err := loadEscrow(ctx, q, holdID)
	if err != nil {
		return Transfer{}, err
	}
	if t, ok, err := l.loadTransfer(ctx, q, id); err != nil || ok {
		return t, err
	}
	if h.State != "held" {
		return Transfer{}, refuse("hold_not_held")
	}
	if to == "" || to == h.Account {
		return Transfer{}, refuse("self_transfer")
	}
	o, err := l.open(ctx, q, h.Resource, now)
	if err != nil {
		return Transfer{}, err
	}
	t, refusal := l.escrowPay(ctx, q, o, h.Hold, to, id)
	if refusal != nil && !isRefusal(refusal) {
		return Transfer{}, refusal
	}
	if err = o.save(ctx, q); err != nil {
		return Transfer{}, err
	}
	return t, refusal
}

func (l *Ledger) escrowPay(ctx context.Context, q allowance.Querier, o *op, h Hold, to, id string) (Transfer, error) {
	in, err := loadUsage(ctx, q, o.r, o.day, to)
	if err != nil {
		return Transfer{}, err
	}
	if in.Incoming+h.Max > o.rp.InboundCap {
		return Transfer{}, refuse("recipient_limit")
	}
	if err = spike(ctx, q, o.p, h.Account, o.day, o.now); err != nil {
		return Transfer{}, err
	}
	pending, err := transferBreaker(ctx, q, h.Account, o.now)
	if err != nil {
		return Transfer{}, err
	}
	pending = pending || o.lv.FreezeTransfers
	t := Transfer{ID: id, From: h.Account, To: to, Resource: o.r, Amount: h.Max, State: "done", CreatedAt: o.now, ExecuteAt: o.now, ByBucket: map[allowance.Bucket]int64{}}
	hp, err := loadHoldParts(ctx, q, h.ID)
	if err != nil {
		return Transfer{}, err
	}
	for _, p := range hp {
		t.ByBucket[p.Bucket] += p.Units
		if p.ExpiresAt != 0 && (t.ExpiresAt == 0 || p.ExpiresAt < t.ExpiresAt) {
			t.ExpiresAt = p.ExpiresAt
		}
	}
	doneAt := int64(0)
	if pending {
		var n int
		if err = q.QueryRowContext(ctx, "SELECT count(*) FROM ledger_transfers WHERE from_account=? AND state='pending' AND created_at>=?", h.Account, o.now-o.p.TransferDelay-86400).Scan(&n); err != nil {
			return Transfer{}, err
		}
		if n >= TransfersPendingMax {
			return Transfer{}, refuse("hold_limit")
		}
		t.State = "pending"
		if !o.lv.FreezeTransfers {
			t.ExecuteAt = o.now + o.p.TransferDelay
		}
		// The hold now backs the pending transfer: its end is the transfer's.
		if _, err = q.ExecContext(ctx, "UPDATE ledger_holds SET expires_at=? WHERE id=?", t.ExecuteAt, h.ID); err != nil {
			return Transfer{}, err
		}
	} else {
		parts, err := heldParts(ctx, q, id, hp)
		if err != nil {
			return Transfer{}, err
		}
		moved, _, err := moveParts(ctx, q, o.r, o.version, h.Account, to, id, parts, true, "work_reward", o.now)
		if err != nil {
			return Transfer{}, err
		}
		if _, err = q.ExecContext(ctx, "UPDATE ledger_holds SET state='committed',used_units=? WHERE id=?", moved, h.ID); err != nil {
			return Transfer{}, err
		}
		doneAt = o.now
	}
	if _, err = q.ExecContext(ctx, "INSERT INTO ledger_transfers(id,from_account,to_account,resource,amount,fee,hold_id,state,created_at,execute_at,done_at) VALUES(?,?,?,?,?,0,?,?,?,?,?)",
		id, h.Account, to, string(o.r), h.Max, h.ID, t.State, o.now, t.ExecuteAt, doneAt); err != nil {
		return Transfer{}, err
	}
	if err = addUsage(ctx, q, o.r, o.day, h.Account, 0, 0, h.Max); err != nil {
		return Transfer{}, err
	}
	return t, addUsage(ctx, q, o.r, o.day, to, 0, h.Max, 0)
}

// heldParts reads the lots behind a hold's parts, for moveParts.
func heldParts(ctx context.Context, q allowance.Querier, transfer string, hp []holdPart) ([]part, error) {
	var parts []part
	for _, p := range hp {
		rows, err := q.QueryContext(ctx, "SELECT "+lotCols+" FROM ledger_lots WHERE id=?", p.LotID)
		if err != nil {
			return nil, err
		}
		var x lot
		found := false
		for rows.Next() {
			x, err = scanLot(rows.Scan)
			found = err == nil
		}
		rows.Close()
		if err != nil || !found {
			return nil, fmt.Errorf("ledger: lot %d of transfer %s: %v", p.LotID, transfer, err)
		}
		parts = append(parts, part{Lot: x, Take: p.Units})
	}
	return parts, nil
}

// EscrowRelease returns an escrow's held units to the account that holds
// them; the fee stays spent. Releasing it again, or after it was paid, is
// hold_not_held and changes nothing.
func (l *Ledger) EscrowRelease(ctx context.Context, q allowance.Querier, holdID, reason string, now int64) error {
	h, id, err := loadEscrow(ctx, q, holdID)
	if err != nil {
		return err
	}
	if h.State != "held" {
		return refuse("hold_not_held")
	}
	if _, ok, err := l.loadTransfer(ctx, q, id); err != nil || ok {
		if err == nil {
			err = refuse("hold_not_held") // a pending payment: cancelled as a transfer
		}
		return err
	}
	_, version, err := l.params(ctx, q, now)
	if err != nil {
		return err
	}
	parts, err := loadHoldParts(ctx, q, holdID)
	if err != nil {
		return err
	}
	for _, p := range parts {
		if _, err = q.ExecContext(ctx, "UPDATE ledger_lots SET held=held-? WHERE id=?", p.Units, p.LotID); err != nil {
			return err
		}
		if err = journal(ctx, q, entry{Day: now / 86400, Now: now, Kind: "refund", Account: h.Account, Resource: h.Resource, Bucket: p.Bucket, LotID: p.LotID, Amount: p.Units, HoldID: holdID, Ref: escrowRef("work_reward_release", id), Version: version, Detail: reason}); err != nil {
			return err
		}
	}
	if _, err = q.ExecContext(ctx, "UPDATE ledger_holds SET state='refunded' WHERE id=?", holdID); err != nil {
		return err
	}
	return credentialRelease(ctx, q, holdID, h.Max)
}
