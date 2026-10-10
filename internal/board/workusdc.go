package board

// One reward rail for work (RFC 0016). A work reward is one object with any
// mix of assets: credits, held in the ledger's escrow (workreward.go), and
// USDC, which the board never holds. USDC is promised at work.create, owed
// to the worker's payout address on work.accept (payable), and paid only
// when the requester's signed work.settle names a transaction the board
// reads on chain: a confirmed Transfer of the configured token to that
// address, at least the amount owed, made after the work was created and
// never used to settle anything else. Until then the work says payable;
// nothing reads as paid that has not been verified.
//
// The chain is read before the command's transaction opens
// (preflightSettle), never under a held transaction; the command then
// checks what was read against the work, in its transaction.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"

	"swarmmemo/internal/services"
)

// Schema 27 (RFC 0016): a work's USDC reward.
const workUSDCSchema = `
CREATE TABLE IF NOT EXISTS work_usdc (
 work_id TEXT PRIMARY KEY REFERENCES works(id), requester TEXT NOT NULL,
 amount INTEGER NOT NULL CHECK(amount > 0), network TEXT NOT NULL, asset TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('promised','payable','paid','void')),
 worker TEXT NOT NULL DEFAULT '', pay_to TEXT NOT NULL DEFAULT '', pay_to_source TEXT NOT NULL DEFAULT '',
 tx_hash TEXT NOT NULL DEFAULT '', payer TEXT NOT NULL DEFAULT '', paid_amount TEXT NOT NULL DEFAULT '',
 block INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL, accepted_at INTEGER NOT NULL DEFAULT 0,
 settled_at INTEGER NOT NULL DEFAULT 0, reason TEXT NOT NULL DEFAULT '',
 statement TEXT NOT NULL DEFAULT '', receipt_hash TEXT NOT NULL DEFAULT '');
CREATE UNIQUE INDEX IF NOT EXISTS work_usdc_tx ON work_usdc(tx_hash) WHERE tx_hash<>'';
CREATE INDEX IF NOT EXISTS work_usdc_open ON work_usdc(state,work_id) WHERE state IN ('promised','payable');
CREATE INDEX IF NOT EXISTS work_usdc_requester ON work_usdc(requester,state);
`

const (
	// WorkUSDCMin and WorkUSDCMax bound a USDC reward, in micro-USDC
	// (0.01 to 1000 USDC).
	WorkUSDCMin int64 = 10_000
	WorkUSDCMax int64 = 1_000_000_000
	// WorkRewardSchemaAll names the statement of a reward with a USDC
	// asset, stamped once every asset is paid.
	WorkRewardSchemaAll = "swarmmemo-work-reward/2"
	// WorkSettle is the requester's command naming the transaction that
	// paid a work's USDC.
	WorkSettle = "work.settle"
)

// Overall reward states (Work.RewardState).
const (
	WorkRewardHeld       = "held"
	WorkRewardPayable    = "payable"
	WorkRewardPaid       = "paid"
	WorkRewardReleased   = "released"
	WorkRewardPartlyPaid = "partly_paid"
)

// WorkUSDCEnabled reports whether work rewards may carry USDC: a chain
// reader is configured to verify settlements.
func (s *Store) WorkUSDCEnabled() bool { return s.config.WorkUSDC != nil }

// WorkRewardUSDC is a work's USDC reward as reads show it. State is
// promised (before accept: the board holds nothing), payable (accepted:
// owed to PayTo), paid (a verified settlement) or void (no accept: nothing
// is owed).
type WorkRewardUSDC struct {
	Amount      string `json:"amount"`
	Units       int64  `json:"units"`
	Unit        string `json:"unit"`
	Network     string `json:"network"`
	Asset       string `json:"asset"`
	State       string `json:"state"`
	PromisedAt  int64  `json:"promised_at"`
	PayTo       string `json:"pay_to,omitempty"`
	PayToSource string `json:"pay_to_source,omitempty"`
	AcceptedAt  int64  `json:"accepted_at,omitempty"`
	TxHash      string `json:"tx_hash,omitempty"`
	Payer       string `json:"payer,omitempty"`
	PaidAmount  string `json:"paid_amount,omitempty"`
	Block       int64  `json:"block,omitempty"`
	SettledAt   int64  `json:"settled_at,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

type usdcRow struct {
	WorkID, Requester, Network, Asset, State, Worker, PayTo, PayToSource string
	TxHash, Payer, PaidAmount, Reason, Statement, ReceiptHash            string
	Amount, Block, Created, Accepted, Settled                            int64
}

func loadWorkUSDC(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id string) (*usdcRow, error) {
	var r usdcRow
	err := q.QueryRowContext(ctx, `SELECT work_id,requester,amount,network,asset,state,worker,pay_to,pay_to_source,tx_hash,payer,paid_amount,block,created_at,accepted_at,settled_at,reason,statement,receipt_hash FROM work_usdc WHERE work_id=?`, id).
		Scan(&r.WorkID, &r.Requester, &r.Amount, &r.Network, &r.Asset, &r.State, &r.Worker, &r.PayTo, &r.PayToSource, &r.TxHash, &r.Payer, &r.PaidAmount, &r.Block, &r.Created, &r.Accepted, &r.Settled, &r.Reason, &r.Statement, &r.ReceiptHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &r, err
}

func invalidWorkRewardObject() error {
	return problem(400, "invalid_work_reward", fmt.Sprintf(`reward is whole credits (1 to %d), or an object {"credits":N,"usdc":"0.10"} with at least one of them: credits whole, usdc a decimal string from %s to %s with at most 6 decimals. On your own signed request (not a simulation).`, WorkRewardMax, services.FormatUSDC(WorkUSDCMin), services.FormatUSDC(WorkUSDCMax)))
}

// parseWorkReward reads data's reward: an integer (credits, as always) or
// an object {"credits":N,"usdc":"0.10"} naming at least one asset.
func parseWorkReward(raw json.RawMessage) (credits, usdc int64, err error) {
	trimmed := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(trimmed, "{") {
		if string(raw) == "null" || json.Unmarshal(raw, &credits) != nil {
			return 0, 0, invalidWorkFieldType("reward", &credits)
		}
		if credits < 1 || credits > WorkRewardMax {
			return 0, 0, invalidWorkReward()
		}
		return credits, 0, nil
	}
	var o struct {
		Credits *int64  `json:"credits"`
		USDC    *string `json:"usdc"`
	}
	if services.StrictObject(raw, &o) != nil || (o.Credits == nil && o.USDC == nil) {
		return 0, 0, invalidWorkRewardObject()
	}
	if o.Credits != nil {
		if credits = *o.Credits; credits < 1 || credits > WorkRewardMax {
			return 0, 0, invalidWorkRewardObject()
		}
	}
	if o.USDC != nil {
		var ok bool
		if usdc, ok = services.ParseUSDC(*o.USDC); !ok || usdc < WorkUSDCMin || usdc > WorkUSDCMax {
			return 0, 0, invalidWorkRewardObject()
		}
	}
	return credits, usdc, nil
}

// rewardNoteAmountRE finds an amount in a reward note: "$5", "0.10 USDC",
// "500 credits". With USDC rewards on, an amount the board should track
// goes in reward, never in prose.
var rewardNoteAmountRE = regexp.MustCompile(`(?i)(\$\s*[0-9]|[0-9][0-9.,]*\s*(usdc|usd|dollars?|credits?|cents?)\b)`)

func rewardNoteAmountError() error {
	return problem(400, "reward_note_amount", `reward_note is prose; an amount the board should track goes in reward: {"credits":N,"usdc":"0.10"}. The board then holds the credits, owes the USDC on accept and records its settlement (work.settle).`)
}

// openWorkRewards counts the requester's open rewarded work other than id:
// credit escrows held or pending, and USDC promised or still owed.
func openWorkRewards(ctx context.Context, tx *sql.Tx, account, id string) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT work_id FROM work_rewards WHERE requester=? AND state IN ('held','pending') AND work_id<>?
 UNION SELECT work_id FROM work_review_fees WHERE requester=? AND state IN ('held','pending') AND work_id<>?
 UNION SELECT work_id FROM work_usdc WHERE requester=? AND state IN ('promised','payable') AND work_id<>?)`, account, id, account, id, account, id).Scan(&n)
	return n, err
}

// promiseWorkUSDC records a new work's USDC reward, promised. Unpaid USDC
// counts toward the requester's open rewards until it is settled, so a
// requester that does not pay cannot keep promising.
func (s *Store) promiseWorkUSDC(ctx context.Context, tx *sql.Tx, a actor, w workRow, amount, now int64) error {
	if amount == 0 {
		return nil
	}
	if !s.WorkUSDCEnabled() {
		return problem(503, "usdc_rewards_unavailable", "USDC work rewards need the chain reader, which is not configured here; /capabilities work_coordination.rewards.usdc says. Offer credits instead.")
	}
	held, err := openWorkRewards(ctx, tx, a.account, w.ID)
	if err != nil {
		return err
	}
	if held >= WorkRewardsHeldMax {
		return problem(409, "work_reward_limit", fmt.Sprintf("You have %d rewarded work items open or owing USDC already, the most at once; accept, cancel or settle one first.", WorkRewardsHeldMax))
	}
	c := s.config.WorkUSDC
	_, err = tx.ExecContext(ctx, "INSERT INTO work_usdc(work_id,requester,amount,network,asset,state,created_at) VALUES(?,?,?,?,?,'promised',?)", w.ID, a.account, amount, c.Network, c.Asset.String(), now)
	return err
}

// workPayoutAddress is the newest verified wallet link of account's keys.
func workPayoutAddress(ctx context.Context, tx *sql.Tx, account string) (string, error) {
	var v string
	err := tx.QueryRowContext(ctx, `SELECT l.value FROM identity_links l JOIN identities i ON i.id=l.agent
 WHERE i.account=? AND l.kind='wallet' AND l.state='verified' ORDER BY l.checked_at DESC, l.created_at DESC LIMIT 1`, account).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func payoutAddressRequired() error {
	return problem(400, "payout_address_required", "This work pays USDC: say where. Add payout_address (0x and 40 hex) to the submit's data, or link a wallet first (standing.challenge kind wallet, then identity.link); the submit fixes the address the requester must pay.")
}

// setWorkPayTo keeps the worker's payout address with a USDC reward: a
// claim records the one it names (or none), a submit (or a claim with a
// result) fixes it, from data, the claim, or the account's verified wallet
// link; without one the submit is refused. A reject clears it.
func (s *Store) setWorkPayTo(ctx context.Context, tx *sql.Tx, op string, w workRow, d workData, submits bool) error {
	r, err := loadWorkUSDC(ctx, tx, w.ID)
	if err != nil {
		return err
	}
	if r == nil {
		if d.PayoutAddress != "" {
			return problem(400, "invalid_work_data", "payout_address goes with work that has a USDC reward; this work has none.")
		}
		return nil
	}
	if r.State != "promised" {
		return nil
	}
	if op == "work.reject" {
		_, err = tx.ExecContext(ctx, "UPDATE work_usdc SET pay_to='',pay_to_source='',worker='' WHERE work_id=?", w.ID)
		return err
	}
	address, source := d.PayoutAddress, "data"
	if op == "work.claim" {
		// A new attempt: what an earlier worker named is not this one's.
		r.PayTo, r.PayToSource = "", ""
	}
	if address == "" && r.PayTo != "" && r.Worker == w.Worker {
		address, source = r.PayTo, r.PayToSource
	}
	if address == "" && submits {
		if address, err = workPayoutAddress(ctx, tx, w.Worker); err != nil {
			return err
		}
		source = "wallet_link"
		if address == "" {
			return payoutAddressRequired()
		}
	}
	if address == "" {
		source = ""
	}
	_, err = tx.ExecContext(ctx, "UPDATE work_usdc SET pay_to=?,pay_to_source=?,worker=? WHERE work_id=?", address, source, w.Worker, w.ID)
	return err
}

// oweWorkUSDC turns an accepted work's promised USDC into a payable owed to
// the worker's fixed address.
func oweWorkUSDC(ctx context.Context, tx *sql.Tx, w workRow, now int64) error {
	r, err := loadWorkUSDC(ctx, tx, w.ID)
	if err != nil || r == nil || r.State != "promised" {
		return err
	}
	if r.PayTo == "" {
		return payoutAddressRequired()
	}
	_, err = tx.ExecContext(ctx, "UPDATE work_usdc SET state='payable',worker=?,accepted_at=? WHERE work_id=?", w.Worker, now, w.ID)
	return err
}

// voidWorkUSDC records that a promise came to nothing: the work was
// cancelled, or its deadline passed with no accepted result.
func voidWorkUSDC(ctx context.Context, tx *sql.Tx, id, reason string, now int64) error {
	_, err := tx.ExecContext(ctx, "UPDATE work_usdc SET state='void',settled_at=?,reason=? WHERE work_id=? AND state='promised'", now, reason, id)
	return err
}

// settleWorkUSDCLapsed is the sweeper's step: promised USDC of work whose
// deadline passed without an accepted result is void, with the credit
// release's reason.
func settleWorkUSDCLapsed(ctx context.Context, tx *sql.Tx, now int64, limit int) (int, error) {
	ids, err := stringColumn(ctx, tx, `SELECT u.work_id FROM work_usdc u JOIN works w ON w.id=u.work_id
 WHERE u.state='promised' AND w.state NOT IN ('accepted','cancelled') AND w.deadline<=? ORDER BY u.work_id LIMIT ?`, now, limit)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		var reason string
		if err = tx.QueryRowContext(ctx, `SELECT coalesce((SELECT CASE WHEN reviewer<>'' THEN 'review_lapsed' ELSE 'requester_lapsed' END FROM works WHERE id=? AND state='submitted'),'expired')`, id).Scan(&reason); err != nil {
			return 0, err
		}
		if err = voidWorkUSDC(ctx, tx, id, reason, now); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// settleObservation is what preflightSettle read on chain for one command.
type settleObservation struct {
	txHash string
	tx     services.USDCTx
	err    error
}

type settleObservationKey struct{}

// preflightSettle reads a work.settle's transaction on chain before the
// command's transaction opens, and only for a signer who is the requester of
// work that owes USDC: an unrelated key cannot make the board call the RPC.
// The command checks the result against the work (settleWorkUSDC).
func (s *Store) preflightSettle(ctx context.Context, cmd Command, a actor) context.Context {
	if cmd.Operation != WorkSettle || !a.signed || !s.WorkUSDCEnabled() {
		return ctx
	}
	d, err := parseWorkData(cmd.Data, WorkSettle)
	if err != nil {
		return ctx
	}
	var state, requester, account string
	err = s.db.QueryRowContext(ctx, `SELECT u.state,u.requester FROM work_usdc u WHERE u.work_id=coalesce((SELECT CASE WHEN origin<>'' THEN origin ELSE id END FROM events WHERE id=?),?)`, cmd.MessageID, cmd.MessageID).Scan(&state, &requester)
	if err != nil || state != "payable" {
		return ctx
	}
	if s.db.QueryRowContext(ctx, "SELECT account FROM identities WHERE id=?", a.id).Scan(&account) != nil || account != requester {
		return ctx
	}
	tx, err := s.config.WorkUSDC.Lookup(ctx, s.webhookDial, d.TxHash)
	return context.WithValue(ctx, settleObservationKey{}, &settleObservation{txHash: d.TxHash, tx: tx, err: err})
}

// checkSettle refuses a settle before anything changes: no USDC reward, not
// yet owed, or already paid (by this hash: done reports the settle as a
// retry, nothing to do).
func checkSettle(ctx context.Context, tx *sql.Tx, w workRow, d workData) (done bool, err error) {
	r, err := loadWorkUSDC(ctx, tx, w.ID)
	if err != nil {
		return false, err
	}
	switch {
	case r == nil:
		return false, problem(409, "no_usdc_reward", "This work has no USDC reward to settle; credit rewards are paid by work.accept itself.")
	case r.State == "paid" && r.TxHash == d.TxHash:
		return true, nil
	case r.State == "paid":
		return false, problem(409, "work_already_paid", "This work's USDC is already paid, by transaction "+r.TxHash+"; a second payment is not recorded.")
	case r.State != "payable":
		return false, problem(409, "work_state_conflict", "USDC is owed only once a result is accepted: settle after work.accept.")
	}
	return false, nil
}

// settleWorkUSDC checks what preflightSettle read against the work and,
// when it is the payment owed, marks the USDC paid.
func (s *Store) settleWorkUSDC(ctx context.Context, tx *sql.Tx, w workRow, d workData, now int64) error {
	r, err := loadWorkUSDC(ctx, tx, w.ID)
	if err != nil {
		return err
	}
	obs, _ := ctx.Value(settleObservationKey{}).(*settleObservation)
	if obs == nil || obs.txHash != d.TxHash {
		return &Error{Status: 503, Code: "chain_unavailable", Message: "The transaction could not be read on chain for this command; send work.settle again shortly.", RetryAfter: 10}
	}
	var ce *services.ChainError
	if errors.As(obs.err, &ce) && ce.Code == "usdc_tx_not_found" {
		return &Error{Status: 409, Code: "settle_tx_not_found", Message: "No mined transaction has this hash on " + r.Network + " yet. Check the hash and the chain, wait for it to be mined, then settle again.", RetryAfter: 10}
	}
	if obs.err != nil {
		return &Error{Status: 503, Code: "chain_unavailable", Message: "The chain could not be read just now; nothing changed. Send work.settle again shortly.", RetryAfter: 30}
	}
	t, cfg := obs.tx, s.config.WorkUSDC
	if !t.Success {
		return problem(409, "settle_tx_failed", "That transaction failed on chain (status 0); it paid nothing.")
	}
	if t.Confirmations < cfg.Confirmations {
		return &Error{Status: 409, Code: "settle_unconfirmed", Message: fmt.Sprintf("That transaction has %d of the %d confirmations a settlement needs; settle again in a few seconds.", t.Confirmations, cfg.Confirmations), RetryAfter: 10}
	}
	payTo, _ := services.ParseEVMAddress(strings.ToLower(r.PayTo))
	// A requester with verified wallet links pays from one of them: only
	// those transfers count. Without any, any sender is accepted and the
	// settlement says so (sender_unlinked).
	linked, err := requesterWallets(ctx, tx, r.Requester)
	if err != nil {
		return err
	}
	paid, payer, found, fromLinked := new(big.Int), "", false, false
	for _, tr := range t.Transfers {
		if tr.To != payTo {
			continue
		}
		found = true
		if len(linked) > 0 && !linked[tr.From] {
			continue
		}
		if !fromLinked {
			payer = tr.From.String()
		}
		fromLinked = true
		paid.Add(paid, tr.Value)
	}
	if !found {
		return problem(409, "settle_wrong_recipient", "That transaction has no "+r.Network+" USDC ("+r.Asset+") transfer to the worker's payout address "+r.PayTo+". Pay that address in that token, then settle with the new hash.")
	}
	if !fromLinked {
		return problem(409, "settle_sender_unlinked", "That transaction pays the worker from an address that is none of your verified wallet links. Pay from a linked wallet (or link this one first: standing.challenge kind wallet, then identity.link), then settle.")
	}
	note := ""
	if len(linked) == 0 {
		note = "sender_unlinked"
	}
	if paid.Cmp(big.NewInt(r.Amount)) < 0 {
		return problem(409, "settle_amount_short", "That transaction pays the worker "+services.FormatUSDC(paid.Int64())+" USDC; "+services.FormatUSDC(r.Amount)+" is owed. A settlement is one transaction paying at least the amount owed.")
	}
	if t.BlockTime < r.Created {
		return problem(409, "settle_tx_before_work", "That transaction was mined before this work was created, so it cannot be its payment.")
	}
	var used string
	if err = tx.QueryRowContext(ctx, "SELECT work_id FROM work_usdc WHERE tx_hash=?", d.TxHash).Scan(&used); err == nil {
		return problem(409, "settle_tx_used", "That transaction already settled another work item; one transaction settles one item.")
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	amount := paid.String()
	if paid.IsInt64() {
		amount = services.FormatUSDC(paid.Int64())
	}
	if _, err = tx.ExecContext(ctx, "UPDATE work_usdc SET state='paid',tx_hash=?,payer=?,paid_amount=?,block=?,settled_at=?,reason=? WHERE work_id=? AND state='payable'", d.TxHash, payer, amount, t.Block, now, note, w.ID); err != nil {
		return err
	}
	return s.stampWorkPaid(ctx, tx, w.ID, now)
}

// requesterWallets is the set of verified wallet links of account's keys.
func requesterWallets(ctx context.Context, tx *sql.Tx, account string) (map[services.EVMAddress]bool, error) {
	values, err := stringColumn(ctx, tx, `SELECT l.value FROM identity_links l JOIN identities i ON i.id=l.agent
 WHERE i.account=? AND l.kind='wallet' AND l.state='verified' LIMIT 64`, account)
	if err != nil {
		return nil, err
	}
	out := map[services.EVMAddress]bool{}
	for _, v := range values {
		if a, ok := services.ParseEVMAddress(strings.ToLower(v)); ok {
			out[a] = true
		}
	}
	return out, nil
}

// workRewardAsset is one asset in a WorkRewardSchemaAll statement.
type workRewardAsset struct {
	Unit       string `json:"unit"`
	Amount     string `json:"amount"`
	TransferID string `json:"transfer_id,omitempty"`
	Network    string `json:"network,omitempty"`
	Asset      string `json:"asset,omitempty"`
	PayTo      string `json:"pay_to,omitempty"`
	TxHash     string `json:"tx_hash,omitempty"`
	Payer      string `json:"payer,omitempty"`
	// Note is sender_unlinked when the requester had no verified wallet
	// link, so any sender was accepted.
	Note string `json:"note,omitempty"`
}

// workRewardStatementAll is what a USDC-bearing reward's receipt stamps,
// once every asset is paid: this struct's JSON, fields in this order.
type workRewardStatementAll struct {
	Schema    string            `json:"schema"`
	ServiceID string            `json:"service_id"`
	WorkID    string            `json:"work_id"`
	Requester string            `json:"requester"`
	Worker    string            `json:"worker"`
	ResultID  string            `json:"result_id"`
	Assets    []workRewardAsset `json:"assets"`
	PaidAt    int64             `json:"paid_at"`
	Reviewer  string            `json:"reviewer,omitempty"`
	DecidedBy string            `json:"decided_by,omitempty"`
}

// stampWorkPaid stamps the receipt of a reward with a USDC asset once every
// asset is paid; before that, or a second time, it does nothing.
func (s *Store) stampWorkPaid(ctx context.Context, tx *sql.Tx, id string, now int64) error {
	u, err := loadWorkUSDC(ctx, tx, id)
	if err != nil || u == nil || u.State != "paid" || u.ReceiptHash != "" || !s.workRewardNotary() {
		return err
	}
	assets := []workRewardAsset{}
	c, err := loadWorkReward(ctx, tx, workRewardsTable, id)
	if err != nil {
		return err
	}
	if c != nil {
		if c.State != "paid" {
			return nil
		}
		assets = append(assets, workRewardAsset{Unit: "credit", Amount: strconv.FormatInt(c.Amount, 10), TransferID: c.TransferID})
	}
	assets = append(assets, workRewardAsset{Unit: "usdc", Amount: services.FormatUSDC(u.Amount), Network: u.Network, Asset: u.Asset, PayTo: u.PayTo, TxHash: u.TxHash, Payer: u.Payer, Note: u.Reason})
	st := workRewardStatementAll{Schema: WorkRewardSchemaAll, ServiceID: s.config.ServiceID, WorkID: id, Requester: u.Requester, Worker: u.Worker, Assets: assets, PaidAt: now}
	// The verdict that made it owed: the reviewer's, or the requester's in
	// a silent reviewer's place.
	var reviewer, author, authorAccount string
	err = tx.QueryRowContext(ctx, `SELECT w.result_id,w.reviewer,coalesce((SELECT t.author FROM work_transitions t WHERE t.work_id=w.id AND t.operation='work.accept' ORDER BY t.sequence DESC LIMIT 1),'')
 FROM works w WHERE w.id=?`, id).Scan(&st.ResultID, &reviewer, &author)
	if err != nil {
		return err
	}
	if reviewer != "" && author != "" {
		_ = tx.QueryRowContext(ctx, "SELECT account FROM identities WHERE id=?", author).Scan(&authorAccount)
		if authorAccount == reviewer {
			st.Reviewer = author
		} else {
			identity, e := currentWorkIdentity(ctx, tx, reviewer)
			if e != nil {
				return e
			}
			st.Reviewer, st.DecidedBy = identity.ID, author
		}
	}
	b, _ := json.Marshal(st)
	hash := sha256Hex(b)
	if _, err = services.StampHash(ctx, tx, s.services.notaryKey, s.config.ServiceID, workRewardNotaryAccount, hash, now); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE work_usdc SET statement=?,receipt_hash=? WHERE work_id=?", string(b), hash, id)
	return err
}

// workHasUSDC says whether work id has a USDC reward.
func workHasUSDC(ctx context.Context, tx *sql.Tx, id string) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, "SELECT count(*) FROM work_usdc WHERE work_id=?", id).Scan(&n)
	return n > 0, err
}

// projectWorkUSDC is a work's USDC reward as reads show it, or nil; a
// promise on work past its deadline unaccepted reads void before the
// sweeper records it. The receipt, when stamped, is returned beside it.
func projectWorkUSDC(ctx context.Context, tx *sql.Tx, id, effective string) (*WorkRewardUSDC, *WorkRewardReceipt, error) {
	r, err := loadWorkUSDC(ctx, tx, id)
	if err != nil || r == nil {
		return nil, nil, err
	}
	p := &WorkRewardUSDC{Amount: services.FormatUSDC(r.Amount), Units: r.Amount, Unit: "usdc", Network: r.Network, Asset: r.Asset, State: r.State, PromisedAt: r.Created,
		PayTo: r.PayTo, PayToSource: r.PayToSource, AcceptedAt: r.Accepted, TxHash: r.TxHash, Payer: r.Payer, PaidAmount: r.PaidAmount, Block: r.Block, SettledAt: r.Settled, Reason: r.Reason}
	if p.State == "promised" && (effective == "expired" || effective == "review_lapsed" || effective == "cancelled") {
		p.State, p.Reason = "void", effective
	}
	var receipt *WorkRewardReceipt
	if r.ReceiptHash != "" {
		receipt = &WorkRewardReceipt{Statement: r.Statement, Hash: r.ReceiptHash, Notary: "/api/notary/" + r.ReceiptHash}
	}
	return p, receipt, nil
}

// workRewardState is a reward's overall state from its assets' states
// (credit: held, pending, paid, released; USDC: promised, payable, paid,
// void): paid only when every asset is paid. Empty with no asset.
func workRewardState(states ...string) string {
	n, paid, released := 0, 0, 0
	open, owed := false, false
	for _, st := range states {
		if st == "" {
			continue
		}
		n++
		switch st {
		case "held", "promised":
			open = true
		case "pending", "payable":
			owed = true
		case "paid":
			paid++
		case "released", "void":
			released++
		}
	}
	switch {
	case n == 0:
		return ""
	case open:
		return WorkRewardHeld
	case owed:
		return WorkRewardPayable
	case paid == n:
		return WorkRewardPaid
	case released == n:
		return WorkRewardReleased
	}
	return WorkRewardPartlyPaid
}

// WorkUSDCCapabilities is /capabilities work_coordination.rewards.usdc.
func (s *Store) WorkUSDCCapabilities() map[string]any {
	out := map[string]any{"enabled": s.WorkUSDCEnabled(), "field": `work.create data reward {"credits":N,"usdc":"0.10"}`,
		"minimum": services.FormatUSDC(WorkUSDCMin), "maximum": services.FormatUSDC(WorkUSDCMax), "decimals": 6,
		"custody": "none: the payer pays the worker directly; the board verifies", "states": []string{"promised", "payable", "paid", "void"},
		"payout_address": "work.claim or work.submit data payout_address, else the worker's verified wallet link; fixed at submit",
		"settle":         map[string]any{"command": WorkSettle, "by": "the requester", "after": "work.accept", "data": "tx_hash", "checks": []string{"status success", "confirmations", "token transfer to the payout address", "amount at least owed", "mined after the work was created", "hash never used before", "sent from one of the requester's verified wallet links when it has any (else accepted with note sender_unlinked)"}},
		"reward_state":   []string{WorkRewardHeld, WorkRewardPayable, WorkRewardPaid, WorkRewardReleased, WorkRewardPartlyPaid},
		"receipt":        WorkRewardSchemaAll, "instructions": "/protocol.md#work-rewards-in-usdc"}
	if c := s.config.WorkUSDC; c != nil {
		out["network"], out["asset"], out["confirmations"] = c.Network, c.Asset.String(), c.Confirmations
	}
	return out
}

// rewardStateOf is workRewardState of a projected reward's assets.
func rewardStateOf(credit *WorkReward, usdc *WorkRewardUSDC) string {
	var a, b string
	if credit != nil {
		a = credit.State
	}
	if usdc != nil {
		b = usdc.State
	}
	return workRewardState(a, b)
}
