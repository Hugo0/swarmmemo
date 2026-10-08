package board

// Spend limits per credential (ROADMAP §3.11): the owner of an account caps
// what a worker key (delegation) or a hosted token may spend of the
// account's credit: credit_per_day per UTC day, credit_per_call on one
// call's reserved maximum, and, for a hosted token, expires_at. The limit is
// set when the credential is made (delegation.create, hosted.token create)
// or later with spend_limit.set, signed by the account's own key; a limited
// credential never sets one. The ledger enforces it where every credit spend
// is reserved (ledger/limits.go), so services, x402 tools, fetch, inference,
// runs, postage and transfers are all covered; the refusal is 429
// spend_limit.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/ledger"
	"swarmmemo/internal/services"
)

const (
	// credentialKeyPrefix and credentialTokenPrefix name a credential to the
	// ledger: a worker key by its fingerprint, a hosted token by token_id.
	credentialKeyPrefix   = "key:"
	credentialTokenPrefix = "token:"
	// SpendLimitMaxCredits bounds credit_per_day and credit_per_call.
	SpendLimitMaxCredits = ledger.SpendLimitMax
)

type hostedTokenCtxKey struct{}

// WithHostedToken records the hosted token a hosted MCP tool call carries,
// so the command it signs is counted against that token's spend limit. Only
// the hosted MCP handler calls it.
func WithHostedToken(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, hostedTokenCtxKey{}, token)
}

// hostedTokenCredential sets a.credential for a command a hosted identity's
// key signed through a hosted token: the token must be a live one of that
// identity, or the command is refused as a bad token would be.
func hostedTokenCredential(ctx context.Context, tx *sql.Tx, a *actor) error {
	token, _ := ctx.Value(hostedTokenCtxKey{}).(string)
	if !a.hosted || token == "" {
		return nil
	}
	var id string
	err := tx.QueryRowContext(ctx, "SELECT token_id FROM hosted_tokens WHERE token_sha256=? AND account=? AND revoked_at=0", hostedHash(token), a.account).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return hostedTokenInvalid()
	}
	if err != nil {
		return err
	}
	a.credential = credentialTokenPrefix + id
	return nil
}

// spendLimitData is a limit as commands carry it; an omitted field is no
// limit of that kind.
type spendLimitData struct {
	CreditPerDay  *int64 `json:"credit_per_day,omitempty"`
	CreditPerCall *int64 `json:"credit_per_call,omitempty"`
	ExpiresAt     *int64 `json:"expires_at,omitempty"`
}

// SpendLimitView is a credential's limit and today's credit spent through
// it, as listings show it: null limits are no limit.
type SpendLimitView struct {
	CreditPerDay         *int64 `json:"credit_per_day"`
	CreditPerCall        *int64 `json:"credit_per_call"`
	ExpiresAt            int64  `json:"expires_at,omitempty"`
	CreditSpentToday     int64  `json:"credit_spent_today"`
	CreditRemainingToday *int64 `json:"credit_remaining_today"`
	ResetsAt             int64  `json:"resets_at"`
}

func invalidSpendLimit(why string) error {
	return problem(400, "invalid_spend_limit", fmt.Sprintf("A spend limit is credit_per_day and credit_per_call, whole numbers of credits from 0 to %d (omitted: no limit of that kind), and expires_at, a future Unix time, for a hosted token only; %s. See /protocol.md#spend-limits-per-credential.", SpendLimitMaxCredits, why))
}

func credentialLimitedError() error {
	return problem(403, "credential_limited", "This credential has a spend limit, so it cannot set spend limits or make, revoke or change credentials; the account's own key (or an unlimited token) can.")
}

// validSpendLimit checks d for a credential of the given kind.
func validSpendLimit(d spendLimitData, token bool, now int64) error {
	for _, v := range []*int64{d.CreditPerDay, d.CreditPerCall} {
		if v != nil && (*v < 0 || *v > SpendLimitMaxCredits) {
			return invalidSpendLimit("a value is out of range")
		}
	}
	if d.ExpiresAt != nil {
		if !token {
			return invalidSpendLimit("a worker key ends at its grant's own expires_at")
		}
		if *d.ExpiresAt <= now {
			return invalidSpendLimit("expires_at is not in the future")
		}
	}
	return nil
}

func (d spendLimitData) limit(credential, account, setBy string) ledger.SpendLimit {
	lim := ledger.SpendLimit{Credential: credential, Account: account, PerDay: d.CreditPerDay, PerCall: d.CreditPerCall, SetBy: setBy}
	if d.ExpiresAt != nil {
		lim.ExpiresAt = *d.ExpiresAt
	}
	return lim
}

// spendLimitsOn reports whether the ledger, which keeps and enforces the
// limits, is on.
func (s *Store) spendLimitsOn() bool {
	return s.config.Features.Ledger != LedgerOff && s.ledger.led != nil
}

// credentialLimited reports whether the command came through a credential
// that carries a limit.
func (s *Store) credentialLimited(ctx context.Context, tx *sql.Tx, a actor) (bool, error) {
	if a.credential == "" || !s.spendLimitsOn() {
		return false, nil
	}
	return s.ledger.led.CredentialLimited(ctx, tx, a.credential)
}

// setSpendLimit writes a limit for a credential the caller has resolved as
// its own, and returns its view.
func (s *Store) setSpendLimit(ctx context.Context, tx *sql.Tx, a actor, credential string, d spendLimitData, now int64) (*SpendLimitView, error) {
	if !s.spendLimitsOn() {
		return nil, allowanceError("service_unavailable")
	}
	if err := s.ledger.led.SetSpendLimit(ctx, tx, d.limit(credential, a.account, a.id), now); err != nil {
		return nil, fromAllowance(err)
	}
	if err := audit(ctx, tx, "spend_limit.set", a.id, a.account, credential, now); err != nil {
		return nil, err
	}
	return s.spendLimitView(ctx, tx, a.account, credential, now)
}

// spendLimitView reads a credential's limit and today's spend; nil while the
// ledger is off.
func (s *Store) spendLimitView(ctx context.Context, q allowance.Querier, account, credential string, now int64) (*SpendLimitView, error) {
	if !s.spendLimitsOn() {
		return nil, nil
	}
	st, err := s.ledger.led.SpendLimitOf(ctx, q, account, credential, now)
	if err != nil {
		return nil, err
	}
	return &SpendLimitView{CreditPerDay: st.PerDay, CreditPerCall: st.PerCall, ExpiresAt: st.ExpiresAt, CreditSpentToday: st.SpentToday, CreditRemainingToday: st.Remaining, ResetsAt: st.ResetsAt}, nil
}

// changeSpendLimit is spend_limit.set: target is one of the account's worker
// keys (a grant_id) or hosted tokens (a token_id); data replaces its limit.
func (s *Store) changeSpendLimit(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	if a.grant != nil {
		return Result{}, delegationError("delegation_forbidden")
	}
	if limited, err := s.credentialLimited(ctx, tx, a); err != nil || limited {
		if err == nil {
			err = credentialLimitedError()
		}
		return Result{}, err
	}
	if !s.spendLimitsOn() {
		return Result{}, allowanceError("service_unavailable")
	}
	var d struct {
		Schema int `json:"schema"`
		spendLimitData
	}
	if services.StrictObject([]byte(c.Data), &d) != nil || d.Schema != 1 {
		return Result{}, invalidSpendLimit(`send data {"schema":1,"credit_per_day"?,"credit_per_call"?,"expires_at"?}`)
	}
	credential, kind := "", ""
	switch {
	case fingerprintRE.MatchString(c.Target):
		var n int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM delegations WHERE child_id=? AND parent_account=?", c.Target, a.account).Scan(&n); err != nil {
			return Result{}, err
		}
		if n == 1 {
			credential, kind = credentialKeyPrefix+c.Target, "worker_key"
		}
	case len(c.Target) == 16 && strings.Trim(c.Target, "0123456789abcdef") == "":
		var oauth int
		// An expired token has ended: a new limit would revive it past the
		// HostedTokensMax it no longer counts toward.
		err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM oauth_families f WHERE f.access_sha256=t.token_sha256) FROM hosted_tokens t WHERE t.token_id=? AND t.account=? AND t.revoked_at=0"+hostedUnexpiredFilter, c.Target, a.account, now).Scan(&oauth)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return Result{}, err
		}
		if err == nil && oauth == 1 {
			return Result{}, invalidSpendLimit("a sign-in (OAuth) connection's token changes as it refreshes, so it takes no limit; revoke it, or give the app a token of its own")
		}
		if err == nil {
			credential, kind = credentialTokenPrefix+c.Target, "hosted_token"
		}
	}
	if credential == "" {
		return Result{}, problem(404, "not_found", "target names none of your worker keys (a grant_id; delegations.list) or live hosted tokens (a token_id; hosted.token list).")
	}
	if err := validSpendLimit(d.spendLimitData, kind == "hosted_token", now); err != nil {
		return Result{}, err
	}
	view, err := s.setSpendLimit(ctx, tx, a, credential, d.spendLimitData, now)
	if err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"target": c.Target, "kind": kind, "spend_limit": view}}, nil
}

// spendLimitError is 429 spend_limit, naming the limit that was hit.
func spendLimitError(e *allowance.Err) error {
	out := &Error{Status: 429, Code: "spend_limit", RetryAfter: e.RetryAfter}
	switch e.SpendLimit {
	case "per_day":
		out.Message = fmt.Sprintf("This credential's daily spend limit (credit_per_day %d) is used up; nothing was charged. It resets at 00:00 UTC (retry_after), or the account's owner can raise it with spend_limit.set.", e.SpendLimitValue)
	case "per_call":
		out.Message = fmt.Sprintf("This call's maximum cost is above this credential's per-call limit (credit_per_call %d); nothing was charged. Lower max_cost, or ask the account's owner to raise the limit.", e.SpendLimitValue)
	default:
		out.Message = "This credential's spend limit has expired (expires_at), so it can spend no credit; nothing was charged. The account's owner can make a new token (hosted.token create)."
	}
	return out
}
