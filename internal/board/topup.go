package board

// Credit top-ups (credits.topup, credits.topups): SwarmMemo as an x402
// server. An agent tops up paid credit in USDC with no sign-up or card: the
// first call answers 402 with one x402 v2 payment requirement, the retry
// carries the signed EIP-3009 authorization (PAYMENT-SIGNATURE or X-PAYMENT
// over HTTP, data.payment on every wire), and the credit is a paid lot
// (never decays, outside the waterfall, transferable while the allowance
// parameters' paid_transferable says so). Credit is bought one way: there
// is no withdrawal, and nothing here can send money.
//
// Money path. In the command's transaction: the amount within the limits,
// the account's day and the board's under their caps, the quote ours, for this account and this
// amount and unexpired, the payment exactly what the quote asked, and its
// authorization nonce never seen (UNIQUE): a row in state settling is
// inserted, or nothing is. After commit, holding no connection, the
// facilitator verifies and settles. Then one short transaction moves the
// row from settling to credited (or failed, or unknown) and, only on
// credited, mints the paid lot. A settled transaction hash is UNIQUE too. A
// row left settling by a crash reads as unknown at the next start: the
// operator resolves it (swarmmemo topup resolve), never a retry.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/ledger"
	"swarmmemo/internal/services"
)

const topupSchema = `
CREATE TABLE IF NOT EXISTS credit_topups (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT NOT NULL UNIQUE, account TEXT NOT NULL, agent TEXT NOT NULL,
 amount INTEGER NOT NULL CHECK(amount > 0), day INTEGER NOT NULL,
 network TEXT NOT NULL, asset TEXT NOT NULL, pay_to TEXT NOT NULL, payer TEXT NOT NULL,
 auth_nonce TEXT NOT NULL UNIQUE, valid_before INTEGER NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('settling','credited','failed','unknown')),
 reason TEXT NOT NULL DEFAULT '', retryable INTEGER NOT NULL DEFAULT 0,
 tx_hash TEXT NOT NULL DEFAULT '', lot_id INTEGER NOT NULL DEFAULT 0,
 created_at INTEGER NOT NULL, settled_at INTEGER NOT NULL DEFAULT 0);
CREATE UNIQUE INDEX IF NOT EXISTS credit_topups_tx ON credit_topups(tx_hash) WHERE tx_hash<>'';
CREATE INDEX IF NOT EXISTS credit_topups_account ON credit_topups(account,seq);
CREATE INDEX IF NOT EXISTS credit_topups_day ON credit_topups(account,day);
`

var topupTxRE = regexp.MustCompile(`^0x[0-9a-f]{64}$`)

// TopupPageMax is the most top-ups one credits.topups page lists.
const TopupPageMax = 50

// topupSettleTimeout bounds the facilitator round trip after commit; it is
// detached from the caller, so a client that hangs up does not abandon a
// settlement half way.
const topupSettleTimeout = 75 * time.Second

type paymentKey struct{}

// WithPayment records the x402 payment header an HTTP request carried
// (PAYMENT-SIGNATURE or X-PAYMENT), for credits.topup. It is outside the
// signed envelope: the payment is itself signed by the payer, and the quote
// it accepts binds it to the account and the amount.
func WithPayment(ctx context.Context, header string) context.Context {
	return context.WithValue(ctx, paymentKey{}, header)
}

func paymentFrom(ctx context.Context) string {
	v, _ := ctx.Value(paymentKey{}).(string)
	return v
}

// openTopup makes the quote key once (credit_topups is in the versioned
// schema, migrate.go) and turns rows a crash left
// settling into unknown, for the operator.
func (s *Store) openTopup() error {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return err
	}
	if _, err := s.db.Exec("INSERT OR IGNORE INTO meta(key,value) VALUES('topup_quote_key',?)", base64.RawURLEncoding.EncodeToString(secret)); err != nil {
		return err
	}
	var encoded string
	if err := s.db.QueryRow("SELECT value FROM meta WHERE key='topup_quote_key'").Scan(&encoded); err != nil {
		return err
	}
	key, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(key) != 32 {
		return errors.New("topup: the stored quote key is malformed")
	}
	s.topupKey = key
	res, err := s.db.Exec("UPDATE credit_topups SET state='unknown',reason='interrupted' WHERE state='settling'")
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		slog.Error("Credit top-ups were interrupted while settling: resolve each with swarmmemo topup resolve", "count", n)
	}
	return nil
}

// TopupEnabled reports whether credit top-ups are on: configured, with the
// ledger on (paid credit is a ledger lot).
func (s *Store) TopupEnabled() bool {
	return s.config.Topup != nil && s.config.Features.Ledger == LedgerOn
}

// topupQuote is a quote for amount credits to account, good until expires:
// "EXPIRES.SALT.MAC", the MAC over every term the requirement states.
func (s *Store) topupQuote(account string, amount, expires int64, salt string) string {
	cfg := s.config.Topup
	mac := hmac.New(sha256.New, s.topupKey)
	mac.Write([]byte("swarmmemo-topup/1\x00" + account + "\x00" + strconv.FormatInt(amount, 10) + "\x00" + cfg.Network + "\x00" +
		cfg.Asset.String() + "\x00" + cfg.PayTo.String() + "\x00" + strconv.FormatInt(expires, 10) + "\x00" + salt))
	return strconv.FormatInt(expires, 10) + "." + salt + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:24])
}

// newTopupQuote is a fresh requirement for amount credits to account.
func (s *Store) newTopupQuote(account string, amount, now int64) services.TopupRequirement {
	b := make([]byte, 9)
	_, _ = rand.Read(b)
	expires := now + services.TopupQuoteSeconds
	return services.TopupRequirement{Amount: amount, Expires: expires, Quote: s.topupQuote(account, amount, expires, base64.RawURLEncoding.EncodeToString(b))}
}

// checkTopupQuote is the requirement quote stands for, when this server
// issued it for exactly this account and amount; expiry is Check's.
func (s *Store) checkTopupQuote(quote, account string, amount int64) (services.TopupRequirement, error) {
	parts := strings.Split(quote, ".")
	if len(parts) != 3 {
		return services.TopupRequirement{}, problem(400, "payment_mismatch", "The payment does not accept a requirement this server issued for this agent and amount; ask again without a payment for a fresh one.")
	}
	expires, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || expires <= 0 || !hmac.Equal([]byte(s.topupQuote(account, amount, expires, parts[1])), []byte(quote)) {
		return services.TopupRequirement{}, problem(400, "payment_mismatch", "The payment does not accept a requirement this server issued for this agent and amount; ask again without a payment for a fresh one.")
	}
	return services.TopupRequirement{Amount: amount, Quote: quote, Expires: expires}, nil
}

// topupData is the strict data of credits.topup: {"schema":1,"payment":"…"}.
func topupData(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	var d struct {
		Schema  json.Number `json:"schema"`
		Payment string      `json:"payment"`
	}
	if services.StrictObject([]byte(raw), &d) != nil || d.Schema.String() != "1" {
		return "", problem(400, "invalid_request", `credits.topup data is {"schema":1,"payment":"BASE64"}, the x402 payment payload; omit data to be quoted.`)
	}
	return d.Payment, nil
}

// creditsTopup is credits.topup.
func (s *Store) creditsTopup(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	if !s.TopupEnabled() {
		return Result{}, problem(404, "topup_unavailable", "Credit top-ups are not enabled on this server; see /capabilities.")
	}
	cfg := s.config.Topup
	if c.Amount < cfg.Min || c.Amount > cfg.Max {
		return Result{}, problem(400, "topup_amount", fmt.Sprintf("amount is in credits (1 credit = 1 micro-USDC): from %d (%s USDC) to %d (%s USDC) per top-up.", cfg.Min, services.FormatUSDC(cfg.Min), cfg.Max, services.FormatUSDC(cfg.Max)))
	}
	payment, err := topupData(c.Data)
	if err != nil {
		return Result{}, err
	}
	if header := paymentFrom(ctx); header != "" {
		if payment != "" && payment != header {
			return Result{}, problem(400, "payment_invalid", "The payment header and data.payment differ; send one.")
		}
		payment = header
	}
	day := now / 86400
	var used int64
	if err = tx.QueryRowContext(ctx, "SELECT coalesce(sum(amount),0) FROM credit_topups WHERE account=? AND day=? AND state IN ('settling','credited','unknown')", a.account, day).Scan(&used); err != nil {
		return Result{}, err
	}
	if used+c.Amount > cfg.AccountDaily {
		return Result{}, rateError(now, "topup_daily_limit", fmt.Sprintf("An agent tops up at most %d credits (%s USDC) per UTC day; %d are left today. It resets at 00:00 UTC.", cfg.AccountDaily, services.FormatUSDC(cfg.AccountDaily), max(cfg.AccountDaily-used, 0)))
	}
	// The board-wide cap: every account's top-ups today together, counted
	// like the account's (a failed one frees its share).
	var boardUsed int64
	if err = tx.QueryRowContext(ctx, "SELECT coalesce(sum(amount),0) FROM credit_topups WHERE day=? AND state IN ('settling','credited','unknown')", day).Scan(&boardUsed); err != nil {
		return Result{}, err
	}
	if boardUsed+c.Amount > cfg.BoardDaily {
		return Result{}, rateError(now, "topup_board_daily_limit", fmt.Sprintf("This board takes at most %d credits (%s USDC) of top-ups per UTC day from all agents together; %d are left today. It resets at 00:00 UTC.", cfg.BoardDaily, services.FormatUSDC(cfg.BoardDaily), max(cfg.BoardDaily-boardUsed, 0)))
	}
	if payment == "" {
		return Result{}, s.paymentRequired(a.account, c.Amount, now)
	}
	p, err := services.ParseTopupPayment(payment)
	if err != nil {
		return Result{}, topupError(err)
	}
	req, err := s.checkTopupQuote(p.Quote, a.account, c.Amount)
	if err != nil {
		return Result{}, err
	}
	if err = cfg.Check(p, req, now); err != nil {
		return Result{}, topupError(err)
	}
	// One authorization, one top-up, ever: a nonce is taken again only when
	// the facilitator could not even verify it (nothing moved), by the same
	// account for the same amount.
	id := "tu_" + randomID()
	err = tx.QueryRowContext(ctx, `INSERT INTO credit_topups(id,account,agent,amount,day,network,asset,pay_to,payer,auth_nonce,valid_before,state,created_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,'settling',?)
ON CONFLICT(auth_nonce) DO UPDATE SET state='settling',reason='',retryable=0,agent=excluded.agent,payer=excluded.payer,day=excluded.day,valid_before=excluded.valid_before,created_at=excluded.created_at,settled_at=0
 WHERE credit_topups.state='failed' AND credit_topups.retryable=1 AND credit_topups.account=excluded.account AND credit_topups.amount=excluded.amount
RETURNING id`,
		id, a.account, a.id, c.Amount, day, cfg.Network, cfg.Asset.String(), cfg.PayTo.String(), p.From.String(), p.Nonce, p.ValidBefore, now).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, problem(409, "payment_replayed", "This payment authorization was already presented; one authorization tops up once. Read credits.topups for its receipt.")
	}
	if err != nil {
		return Result{}, err
	}
	row, err := loadTopup(ctx, tx, a.account, id)
	if err != nil {
		return Result{}, err
	}
	account := a.account
	return Result{Data: map[string]any{"topup": row.view()}, afterCommit: func() (Result, error) {
		settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), topupSettleTimeout)
		defer cancel()
		return s.settleTopup(settleCtx, account, id, p, req)
	}}, nil
}

// paymentRequired is the 402 answer: the x402 v2 PaymentRequired object in
// details (and, over HTTP, in the PAYMENT-REQUIRED header).
func (s *Store) paymentRequired(account string, amount, now int64) error {
	cfg := s.config.Topup
	req := s.newTopupQuote(account, amount, now)
	body, header := cfg.PaymentRequired(req, "https://"+s.config.ServiceID+"/v1/command", fmt.Sprintf("Top up %d SwarmMemo credits (%s USDC)", amount, services.FormatUSDC(amount)))
	return &Error{Status: 402, Code: "payment_required", Message: fmt.Sprintf("Pay %s USDC on %s to top up %d credits: sign the requirement in details.x402.accepts (x402 v2, exact, EIP-3009) within %d seconds and send the same command again with the payment in the PAYMENT-SIGNATURE header or as data {\"schema\":1,\"payment\":\"BASE64\"}.", services.FormatUSDC(amount), cfg.Network, amount, services.TopupQuoteSeconds),
		Details: map[string]any{"x402": body, "payment_required": header}}
}

// topupError maps a payment refusal to its public error.
func topupError(err error) error {
	var te *services.TopupError
	if !errors.As(err, &te) {
		return err
	}
	switch te.Code {
	case "payment_invalid":
		return problem(400, "payment_invalid", fmt.Sprintf("The payment is not one x402 v2 exact (EIP-3009) payment payload in base64, of at most %d bytes.", services.TopupPaymentBytes))
	case "payment_mismatch":
		return problem(400, "payment_mismatch", "The payment does not match the requirement this server issued: network, asset, recipient, amount and quote must be exactly as quoted.")
	case "payment_expired":
		return problem(400, "payment_expired", "The quote or the payment authorization has expired, or is not valid yet; ask again without a payment for a fresh quote.")
	case "payment_rejected":
		return &Error{Status: 402, Code: "payment_rejected", Message: "The facilitator refused the payment (" + te.Reason + "); nothing was charged and no credit was added. Sign a new authorization to try again."}
	}
	if te.Definite {
		return &Error{Status: 503, Code: "facilitator_unavailable", Message: "The payment facilitator could not be reached; nothing was charged. Send the same payment again with a new request_id shortly.", RetryAfter: 30}
	}
	return &Error{Status: 502, Code: "payment_unsettled", Message: "The payment's settlement could not be confirmed, so no credit was added yet. Do not pay again: the operator reconciles it, and credits.topups shows its state."}
}

// settleTopup runs after commit with no transaction held: the facilitator
// settles, then one short transaction records the outcome and, when it
// settled, mints the credit.
func (s *Store) settleTopup(ctx context.Context, account, id string, p services.TopupPayment, req services.TopupRequirement) (Result, error) {
	st, err := s.config.Topup.Settle(ctx, s.webhookDial, p, req)
	if err != nil {
		var te *services.TopupError
		if !errors.As(err, &te) {
			te = &services.TopupError{Code: "payment_unsettled", Reason: "internal"}
		}
		state, retryable := "unknown", 0
		if te.Definite {
			state = "failed"
			if te.Reason == services.TopupFacilitatorUnavailable {
				retryable = 1
			}
		}
		reason := te.Reason
		if reason == "" {
			reason = te.Code
		}
		if _, uerr := s.db.ExecContext(ctx, "UPDATE credit_topups SET state=?,reason=?,retryable=?,settled_at=? WHERE id=? AND state='settling'", state, reason, retryable, s.now().Unix(), id); uerr != nil {
			slog.Error("Credit top-up outcome not recorded", "topup", id, "state", state, "error", uerr.Error())
		}
		if state == "unknown" {
			slog.Error("Credit top-up settlement unknown: resolve it with swarmmemo topup resolve", "topup", id, "reason", reason)
		}
		return Result{}, topupError(te)
	}
	row, err := s.creditTopup(ctx, account, id, "settling", st.Transaction, s.now().Unix())
	if err != nil {
		// Settled on chain but not credited: never lose it silently.
		if _, uerr := s.db.ExecContext(ctx, "UPDATE credit_topups SET state='unknown',reason='credit_failed',tx_hash=CASE WHEN EXISTS(SELECT 1 FROM credit_topups WHERE tx_hash=?) THEN tx_hash ELSE ? END,settled_at=? WHERE id=? AND state='settling'", st.Transaction, st.Transaction, s.now().Unix(), id); uerr != nil {
			slog.Error("Credit top-up outcome not recorded", "topup", id, "error", uerr.Error())
		}
		slog.Error("Credit top-up settled but not credited: resolve it with swarmmemo topup resolve", "topup", id, "transaction", st.Transaction, "error", err.Error())
		return Result{}, &Error{Status: 502, Code: "payment_unsettled", Message: "The payment's settlement could not be confirmed, so no credit was added yet. Do not pay again: the operator reconciles it, and credits.topups shows its state."}
	}
	data := map[string]any{"topup": row.view(), "payment_response": s.paymentResponse(row)}
	return Result{Data: data}, nil
}

// paymentResponse is the x402 SettlementResponse for a credited top-up, in
// base64 for the PAYMENT-RESPONSE header.
func (s *Store) paymentResponse(row topupRow) string {
	raw, _ := json.Marshal(map[string]any{"success": true, "transaction": row.TxHash, "network": row.Network, "payer": row.Payer})
	return base64.StdEncoding.EncodeToString(raw)
}

// creditTopup moves a row from state from to credited with its transaction
// hash and mints its paid lot, in one short transaction. Only one caller
// can ever move a row out of from, so a row is credited at most once, and a
// transaction hash belongs to one row (UNIQUE).
func (s *Store) creditTopup(ctx context.Context, account, id, from, txHash string, now int64) (topupRow, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return topupRow{}, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, "UPDATE credit_topups SET state='credited',reason='',retryable=0,tx_hash=?,settled_at=? WHERE id=? AND account=? AND state=?", txHash, now, id, account, from)
	if err != nil {
		return topupRow{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return topupRow{}, fmt.Errorf("topup %s is not %s", id, from)
	}
	row, err := loadTopup(ctx, tx, account, id)
	if err != nil {
		return topupRow{}, err
	}
	lot, err := s.ledger.led.TopUp(ctx, tx, account, allowance.Credit, row.Amount, "top-up "+id+" ("+services.FormatUSDC(row.Amount)+" USDC, x402)",
		ledger.Ref{Service: "board", Op: "credits.topup", PublicRef: id}, now)
	if err != nil {
		return topupRow{}, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE credit_topups SET lot_id=? WHERE id=?", lot, id); err != nil {
		return topupRow{}, err
	}
	if _, err = tlogCatchUp(ctx, tx); err != nil {
		return topupRow{}, err
	}
	if err = tx.Commit(); err != nil {
		return topupRow{}, err
	}
	row.State, row.TxHash, row.SettledAt, row.Reason = "credited", txHash, now, ""
	return row, nil
}

// topupRow is one row of credit_topups.
type topupRow struct {
	Seq                               int64
	ID, Account, Agent                string
	Amount, Day                       int64
	Network, Asset, PayTo, Payer      string
	State, Reason, TxHash             string
	Retryable                         bool
	ValidBefore, CreatedAt, SettledAt int64
}

const topupCols = "seq,id,account,agent,amount,day,network,asset,pay_to,payer,state,reason,tx_hash,retryable,valid_before,created_at,settled_at"

func scanTopup(scan func(...any) error) (topupRow, error) {
	var r topupRow
	err := scan(&r.Seq, &r.ID, &r.Account, &r.Agent, &r.Amount, &r.Day, &r.Network, &r.Asset, &r.PayTo, &r.Payer, &r.State, &r.Reason, &r.TxHash, &r.Retryable, &r.ValidBefore, &r.CreatedAt, &r.SettledAt)
	return r, err
}

func loadTopup(ctx context.Context, q allowance.Querier, account, id string) (topupRow, error) {
	r, err := scanTopup(q.QueryRowContext(ctx, "SELECT "+topupCols+" FROM credit_topups WHERE id=? AND account=?", id, account).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return r, problem(404, "not_found", "No such top-up for this agent.")
	}
	return r, err
}

// view is the receipt an agent reads: what it paid, from which address, the
// settling transaction and what it was credited.
func (r topupRow) view() map[string]any {
	v := map[string]any{"id": r.ID, "state": r.State, "amount": r.Amount, "usdc": services.FormatUSDC(r.Amount), "resource": string(allowance.Credit),
		"bucket": string(allowance.Paid), "network": r.Network, "asset": r.Asset, "pay_to": r.PayTo, "payer": r.Payer, "created_at": r.CreatedAt}
	if r.TxHash != "" {
		v["transaction"] = r.TxHash
	}
	if r.SettledAt > 0 {
		v["settled_at"] = r.SettledAt
	}
	if r.Reason != "" {
		v["reason"] = r.Reason
	}
	return v
}

// topupRetry answers an exact retry of a credits.topup that was accepted:
// the top-up as it stands now, or its refusal.
func (s *Store) topupRetry(ctx context.Context, tx *sql.Tx, a actor, stored Result) (Result, error) {
	t, _ := stored.Data["topup"].(map[string]any)
	id, _ := t["id"].(string)
	row, err := loadTopup(ctx, tx, a.account, id)
	if err != nil {
		return Result{}, err
	}
	switch row.State {
	case "credited":
		return Result{OK: true, Data: map[string]any{"topup": row.view(), "payment_response": s.paymentResponse(row)}}, nil
	case "settling":
		return Result{}, &Error{Status: 409, Code: "request_in_flight", Message: "This top-up is still settling; retry after retry_after seconds with the same request ID.", RetryAfter: 5}
	case "failed":
		return Result{}, topupError(&services.TopupError{Code: map[bool]string{true: "payment_unsettled", false: "payment_rejected"}[row.Retryable], Reason: row.Reason, Definite: true})
	}
	return Result{}, topupError(&services.TopupError{Code: "payment_unsettled", Reason: row.Reason})
}

// readTopups is credits.topups: the agent's own top-ups, newest first.
func (s *Store) readTopups(ctx context.Context, tx *sql.Tx, c Command, a actor) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	if !s.TopupEnabled() {
		return Result{}, problem(404, "topup_unavailable", "Credit top-ups are not enabled on this server; see /capabilities.")
	}
	limit := c.Limit
	if limit < 0 || limit > TopupPageMax {
		return Result{}, problem(400, "invalid_limit", fmt.Sprintf("limit is 1–%d.", TopupPageMax))
	}
	if limit == 0 {
		limit = 20
	}
	before := int64(1) << 62
	if c.Cursor != "" {
		n, err := strconv.ParseInt(c.Cursor, 10, 64)
		if err != nil || n <= 0 {
			return Result{}, problem(400, "invalid_cursor", "cursor is the next_cursor of the previous page.")
		}
		before = n
	}
	rows, err := tx.QueryContext(ctx, "SELECT "+topupCols+" FROM credit_topups WHERE account=? AND seq<? ORDER BY seq DESC LIMIT ?", a.account, before, limit+1)
	if err != nil {
		return Result{}, err
	}
	defer rows.Close()
	out := []map[string]any{}
	var last int64
	more := false
	for rows.Next() {
		r, err := scanTopup(rows.Scan)
		if err != nil {
			return Result{}, err
		}
		if len(out) == limit {
			more = true
			break
		}
		out, last = append(out, r.view()), r.Seq
	}
	if err = rows.Err(); err != nil {
		return Result{}, err
	}
	data := map[string]any{"schema": 1, "topups": out}
	if more {
		data["next_cursor"] = strconv.FormatInt(last, 10)
	}
	return Result{Data: data}, nil
}

// TopupCapabilities is the /capabilities "topup" object; nil while top-ups
// are off.
func (s *Store) TopupCapabilities() map[string]any {
	if !s.TopupEnabled() {
		return nil
	}
	cfg := s.config.Topup
	return map[string]any{
		"operation": "credits.topup", "receipts": "credits.topups", "mcp_tool": "credits_topup",
		"protocol": "x402", "x402_version": 2, "scheme": "exact", "network": cfg.Network, "asset": cfg.Asset.String(), "pay_to": cfg.PayTo.String(),
		"unit":       "1 credit = 1 micro-USDC; no margin on top-ups",
		"limits":     map[string]int64{"min": cfg.Min, "max": cfg.Max, "account_daily": cfg.AccountDaily, "board_daily": cfg.BoardDaily},
		"quote_ttl":  services.TopupQuoteSeconds,
		"headers":    map[string]string{"required": "PAYMENT-REQUIRED", "payment": "PAYMENT-SIGNATURE (or X-PAYMENT)", "response": "PAYMENT-RESPONSE"},
		"credit":     map[string]any{"bucket": string(allowance.Paid), "decay": "never", "waterfall": false},
		"withdrawal": false,
		"docs":       "/tools/topup",
	}
}

// ResolveTopup is the operator's verdict on a top-up whose settlement is
// unknown (swarmmemo topup resolve ID credit TXHASH | fail): credit it with
// the transaction the operator found on chain, or mark it failed. Only an
// unknown top-up can be resolved, and a transaction hash credits once.
func (s *Store) ResolveTopup(ctx context.Context, id, verdict, txHash string) (map[string]any, error) {
	var account, state string
	if err := s.db.QueryRowContext(ctx, "SELECT account,state FROM credit_topups WHERE id=?", id).Scan(&account, &state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("no such top-up")
		}
		return nil, err
	}
	if state != "unknown" {
		return nil, fmt.Errorf("top-up %s is %s; only an unknown one is resolved", id, state)
	}
	now := s.now().Unix()
	switch verdict {
	case "credit":
		txHash = strings.ToLower(txHash)
		if !topupTxRE.MatchString(txHash) {
			return nil, errors.New("credit needs the settling transaction hash, 0x and 64 hex digits")
		}
		if s.ledger.led == nil {
			return nil, errors.New("the ledger is off")
		}
		// A credit_failed row may already carry the hash; clear it first.
		if _, err := s.db.ExecContext(ctx, "UPDATE credit_topups SET tx_hash='' WHERE id=? AND state='unknown'", id); err != nil {
			return nil, err
		}
		row, err := s.creditTopup(ctx, account, id, "unknown", txHash, now)
		if err != nil {
			return nil, err
		}
		return row.view(), nil
	case "fail":
		if _, err := s.db.ExecContext(ctx, "UPDATE credit_topups SET state='failed',reason='operator',settled_at=? WHERE id=? AND state='unknown'", now, id); err != nil {
			return nil, err
		}
		row, err := loadTopup(ctx, s.db, account, id)
		return row.view(), err
	}
	return nil, errors.New("verdict is credit TXHASH or fail")
}

// UnknownTopups lists the top-ups awaiting the operator, oldest first.
func (s *Store) UnknownTopups(ctx context.Context) ([]map[string]any, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+topupCols+" FROM credit_topups WHERE state='unknown' ORDER BY seq LIMIT 200")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		r, err := scanTopup(rows.Scan)
		if err != nil {
			return nil, err
		}
		v := r.view()
		v["account"], v["agent"] = r.Account, r.Agent
		out = append(out, v)
	}
	return out, rows.Err()
}
