package ledger

// House escrow (C889): an escrow the board funds itself, for a sponsored
// work reward. The house account is an account of the ledger's own (it
// signs nothing and claims no daily share); it holds what earlier
// sponsorships released, and what it lacks is minted to it as a granted lot
// from the day's grant slice, the same house grant pool invite rewards are
// paid from, so a sponsorship never issues credit outside the day's budget.
// The escrow is an ordinary escrow hold of the house account: EscrowPay
// moves it to the worker, and EscrowRelease returns it to the house, never
// to the requester the board sponsored. No transfer fee is charged here.

import (
	"context"
	"errors"

	"swarmmemo/internal/allowance"
)

// EscrowHouse holds amount lasting units of house's until at most until,
// minting the shortfall to house as a granted lot (reason and ref are the
// mint's public journal line). requestKey names the escrow: the same key
// returns the same hold. A dry grant slice is global_quota_exhausted, and
// nothing is held or minted.
func (l *Ledger) EscrowHouse(ctx context.Context, q allowance.Querier, house string, r allowance.Resource, amount int64, requestKey, reason string, ref Ref, until, now int64) (Hold, error) {
	if house == "" || requestKey == "" || len(requestKey) > 256 || until <= now || reason == "" || len(reason) > 512 {
		return Hold{}, errors.New("ledger: a house escrow needs an account, a request key of 1–256 bytes, a reason and an end after now")
	}
	id, transfer := escrowKeys(house, requestKey)
	if h, ok, err := loadHold(ctx, q, id); err != nil || ok {
		return h, err
	}
	o, err := l.open(ctx, q, r, now)
	if err != nil {
		return Hold{}, err
	}
	if amount <= 0 || amount > EscrowMax || o.rp.Budget > 0 && amount > o.rp.Budget {
		return Hold{}, refuse("invalid_amount")
	}
	if o.lv.FreezeTransfers {
		return Hold{}, refuse("transfers_frozen")
	}
	if err = l.sweepOwn(ctx, q, o, house); err != nil {
		return Hold{}, err
	}
	lasting := o.p.outlasting(until)
	lots, err := liveLots(ctx, q, house, r, now)
	if err != nil {
		return Hold{}, err
	}
	if _, got := plan(lots, amount, lasting); got < amount {
		short := amount - got
		if o.d.Slices[sliceGrant].avail() < short {
			if err = o.save(ctx, q); err != nil {
				return Hold{}, err
			}
			return Hold{}, refuse("global_quota_exhausted")
		}
		o.d.Slices[sliceGrant].Claimed += short
		x := lot{Account: house, Bucket: allowance.Granted, Origin: originGrant, OriginAccount: house, IssuedDay: o.day, DecayedDay: o.day,
			HalfLife: o.p.GrantedHalfLifeDays, Initial: short, Remaining: short}
		lotID, err := creditLot(ctx, q, r, x, now)
		if e := (*allowance.Err)(nil); err != nil && errors.As(err, &e) && e.Code == "recipient_limit" {
			lotID, err = insertLot(ctx, q, r, x, now)
		}
		if err != nil {
			return Hold{}, err
		}
		e := o.entry("grant", house, short)
		e.Bucket, e.LotID, e.Detail, e.Ref = allowance.Granted, lotID, reason, ref
		if err = journal(ctx, q, e); err != nil {
			return Hold{}, err
		}
		if lots, err = liveLots(ctx, q, house, r, now); err != nil {
			return Hold{}, err
		}
	}
	parts, got := plan(lots, amount, lasting)
	if got < amount {
		return Hold{}, errors.New("ledger: a house escrow found its minted lot short")
	}
	h := Hold{ID: id, Account: house, Resource: r, Max: amount, ExpiresAt: until, State: "held"}
	if _, err = q.ExecContext(ctx, "INSERT INTO ledger_holds(id,account,resource,max_units,used_units,state,request_key,service,method,created_at,expires_at) VALUES(?,?,?,?,0,'held',?,?,?,?,?)",
		h.ID, house, string(o.r), h.Max, "escrow:"+requestKey, ledgerService, escrowMethod, o.now, h.ExpiresAt); err != nil {
		return Hold{}, err
	}
	if err = holdParts(ctx, q, o, house, h.ID, parts, escrowRef("work_reward", transfer)); err != nil {
		return Hold{}, err
	}
	return h, o.save(ctx, q)
}
