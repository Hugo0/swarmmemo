package ledger

// Spend limits per credential (ROADMAP §3.11): an account's owner may cap
// what one delegated credential (a worker key, a hosted token) spends of
// the account's credit, per UTC day and per call, and may give it an end.
// The ledger enforces the limit where every credit spend is reserved:
// debit (an immediate spend), hold (a call's reserve, a deposit) and a
// transfer, in the command's own transaction, next to clientSpend. The day's
// count lives in spend_limit_usage; a hold's units return to it when the
// hold settles below its maximum or is refunded, exactly as allowance_usage
// gets them back, so a refunded call never consumes the limit.

import (
	"context"
	"errors"

	"swarmmemo/internal/allowance"
)

// SpendLimitSchema is created with Schema: tables only, so SchemaVersion
// does not change. Rows are never deleted: a limit lifted keeps its row with
// both limits null, and the daily counts stay as history.
const SpendLimitSchema = `
CREATE TABLE IF NOT EXISTS spend_limits (
 credential TEXT PRIMARY KEY, account TEXT NOT NULL, credit_per_day INTEGER, credit_per_call INTEGER,
 expires_at INTEGER NOT NULL DEFAULT 0, set_at INTEGER NOT NULL, set_by TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS spend_limits_account ON spend_limits(account,credential);
CREATE TABLE IF NOT EXISTS spend_limit_usage (
 credential TEXT NOT NULL, day INTEGER NOT NULL, spent INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(credential,day));
CREATE TABLE IF NOT EXISTS spend_limit_holds (
 hold_id TEXT PRIMARY KEY, credential TEXT NOT NULL, day INTEGER NOT NULL);
`

// SpendLimitMax bounds credit_per_day and credit_per_call.
const SpendLimitMax = maxUnits

// SpendLimit is one credential's limit. A nil PerDay or PerCall is no limit
// of that kind; ExpiresAt 0 is no end.
type SpendLimit struct {
	Credential string
	Account    string
	PerDay     *int64
	PerCall    *int64
	ExpiresAt  int64
	SetAt      int64
	SetBy      string
}

// Limited reports whether the limit restricts anything.
func (s SpendLimit) Limited() bool { return s.PerDay != nil || s.PerCall != nil || s.ExpiresAt != 0 }

// SpendLimitStatus is a credential's limit with today's spend through it.
type SpendLimitStatus struct {
	SpendLimit
	Set        bool   // a limit row exists
	SpentToday int64  // credits spent or held through the credential today
	Remaining  *int64 // PerDay − SpentToday (never below 0); nil without a daily limit
	ResetsAt   int64  // next 00:00 UTC
}

// SetSpendLimit writes a credential's limit (replacing the earlier one).
// The caller has checked that the account owns the credential and that the
// command did not come through a limited credential.
func (l *Ledger) SetSpendLimit(ctx context.Context, q allowance.Querier, lim SpendLimit, now int64) error {
	if lim.Credential == "" || lim.Account == "" {
		return errors.New("ledger: a spend limit needs a credential and an account")
	}
	for _, v := range []*int64{lim.PerDay, lim.PerCall} {
		if v != nil && (*v < 0 || *v > SpendLimitMax) {
			return refuse("invalid_amount")
		}
	}
	if lim.ExpiresAt < 0 {
		return refuse("invalid_amount")
	}
	_, err := q.ExecContext(ctx, `INSERT INTO spend_limits(credential,account,credit_per_day,credit_per_call,expires_at,set_at,set_by) VALUES(?,?,?,?,?,?,?)
 ON CONFLICT(credential) DO UPDATE SET credit_per_day=excluded.credit_per_day,credit_per_call=excluded.credit_per_call,expires_at=excluded.expires_at,set_at=excluded.set_at,set_by=excluded.set_by WHERE spend_limits.account=excluded.account`,
		lim.Credential, lim.Account, lim.PerDay, lim.PerCall, lim.ExpiresAt, now, lim.SetBy)
	return err
}

func loadSpendLimit(ctx context.Context, q allowance.Querier, credential string) (SpendLimit, bool, error) {
	lim := SpendLimit{Credential: credential}
	var perDay, perCall *int64
	err := q.QueryRowContext(ctx, "SELECT account,credit_per_day,credit_per_call,expires_at,set_at,set_by FROM spend_limits WHERE credential=?", credential).
		Scan(&lim.Account, &perDay, &perCall, &lim.ExpiresAt, &lim.SetAt, &lim.SetBy)
	if isNoRows(err) {
		return lim, false, nil
	}
	lim.PerDay, lim.PerCall = perDay, perCall
	return lim, err == nil, err
}

func credentialSpent(ctx context.Context, q allowance.Querier, credential string, day int64) (int64, error) {
	var spent int64
	err := q.QueryRowContext(ctx, "SELECT spent FROM spend_limit_usage WHERE credential=? AND day=?", credential, day).Scan(&spent)
	if isNoRows(err) {
		err = nil
	}
	return spent, err
}

// SpendLimitOf reads a credential's limit and today's spend through it. An
// account names the owner: a limit row of another account reads as unset.
func (l *Ledger) SpendLimitOf(ctx context.Context, q allowance.Querier, account, credential string, now int64) (SpendLimitStatus, error) {
	day := now / 86400
	st := SpendLimitStatus{ResetsAt: (day + 1) * 86400}
	lim, ok, err := loadSpendLimit(ctx, q, credential)
	if err != nil {
		return st, err
	}
	if ok && lim.Account == account {
		st.SpendLimit, st.Set = lim, true
	} else {
		st.SpendLimit = SpendLimit{Credential: credential, Account: account}
	}
	if st.SpentToday, err = credentialSpent(ctx, q, credential, day); err != nil {
		return st, err
	}
	if st.PerDay != nil {
		left := max(0, *st.PerDay-st.SpentToday)
		st.Remaining = &left
	}
	return st, nil
}

// CredentialLimited reports whether the credential carries any limit: such
// a credential may not set limits or mint credentials of its own.
func (l *Ledger) CredentialLimited(ctx context.Context, q allowance.Querier, credential string) (bool, error) {
	if credential == "" {
		return false, nil
	}
	lim, ok, err := loadSpendLimit(ctx, q, credential)
	return ok && lim.Limited(), err
}

// spendLimitRefusal is a spend_limit refusal naming the limit hit; the
// daily one clears at the next 00:00 UTC.
func spendLimitRefusal(which string, value, now int64) error {
	e := &allowance.Err{Code: "spend_limit", SpendLimit: which, SpendLimitValue: value}
	if which == "per_day" {
		e.RetryAfter = int(86400 - mod(now, 86400))
	}
	return e
}

// credentialCheck refuses a credit spend of units through the subject's
// credential that its limit does not allow. It only reads, so it runs
// before anything of the spend is written; credentialRecord counts the
// spend once it goes through. Both run in the command's transaction, which
// holds the database's one connection, so two reserves through the same
// credential are serialized: the second reads the first's count.
func (l *Ledger) credentialCheck(ctx context.Context, q allowance.Querier, o *op, s allowance.Subject, units int64) error {
	if s.Credential == "" || o.r != allowance.Credit || units <= 0 {
		return nil
	}
	lim, ok, err := loadSpendLimit(ctx, q, s.Credential)
	if err != nil || !ok || lim.Account != s.ID {
		return err
	}
	if lim.ExpiresAt != 0 && o.now >= lim.ExpiresAt {
		return spendLimitRefusal("expired", 0, o.now)
	}
	if lim.PerCall != nil && units > *lim.PerCall {
		return spendLimitRefusal("per_call", *lim.PerCall, o.now)
	}
	if lim.PerDay != nil {
		spent, err := credentialSpent(ctx, q, s.Credential, o.day)
		if err != nil {
			return err
		}
		if spent+units > *lim.PerDay {
			return spendLimitRefusal("per_day", *lim.PerDay, o.now)
		}
	}
	return nil
}

// credentialRecord counts units spent through the subject's credential
// today; with a hold, it remembers the hold so its settlement gives back
// what was not used.
func credentialRecord(ctx context.Context, q allowance.Querier, o *op, s allowance.Subject, units int64, holdID string) error {
	if s.Credential == "" || o.r != allowance.Credit || units <= 0 {
		return nil
	}
	if _, err := q.ExecContext(ctx, "INSERT INTO spend_limit_usage(credential,day,spent) VALUES(?,?,?) ON CONFLICT(credential,day) DO UPDATE SET spent=spent+excluded.spent", s.Credential, o.day, units); err != nil {
		return err
	}
	if holdID == "" {
		return nil
	}
	_, err := q.ExecContext(ctx, "INSERT INTO spend_limit_holds(hold_id,credential,day) VALUES(?,?,?) ON CONFLICT(hold_id) DO NOTHING", holdID, s.Credential, o.day)
	return err
}

// credentialRelease gives back units of a hold made through a credential to
// the day it was made, as settle and release give them back to
// allowance_usage.
func credentialRelease(ctx context.Context, q allowance.Querier, holdID string, units int64) error {
	if units <= 0 {
		return nil
	}
	var credential string
	var day int64
	err := q.QueryRowContext(ctx, "SELECT credential,day FROM spend_limit_holds WHERE hold_id=?", holdID).Scan(&credential, &day)
	if isNoRows(err) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, "UPDATE spend_limit_usage SET spent=max(0,spent-?) WHERE credential=? AND day=?", units, credential, day)
	return err
}
