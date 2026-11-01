package board

// USDC owed after the fact (C589). A requester sometimes owes USDC on a
// result it already accepted: a standing task's extra announced in the
// thread, a bonus for a good result. work.promise records it: the
// requester's signed line on an accepted result, {amount_usdc, note},
// owed to that result's worker at its payout address (its earlier one on
// this result, else its verified wallet link). It is the same work_usdc
// row a reward's USDC is (state payable), so work.settle pays it and the
// reads show it, with no schema change: the note stays in the signed
// transition (work.history), read back from there.
//
// Reads show each accepted result's money (work.get results[].money,
// /work/ID): the credits paid on accept, and the USDC owed (amount,
// since) or paid (amount, tx_hash with its explorer link, paid_at).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"swarmmemo/internal/services"
)

// WorkPromise is the requester's command owing USDC on an accepted result.
const WorkPromise = "work.promise"

func invalidWorkPromise() error {
	return problem(400, "invalid_work_reward", fmt.Sprintf(`%s data names amount_usdc, a decimal string from %s to %s with at most 6 decimals, and may add note, one line of at most %d characters.`,
		WorkPromise, services.FormatUSDC(WorkUSDCMin), services.FormatUSDC(WorkUSDCMax), WorkRewardNoteMax))
}

func errPromiseNotAccepted() error {
	return problem(409, "work_state_conflict", WorkPromise+" owes USDC on an accepted result: accept it first. On a standing task, name the result's own work item (work.get results[].work_id), not the task.")
}

// checkPromise refuses a promise before anything changes: the board has no
// chain reader, the work is not a request, or the result already owes or
// was paid USDC.
func (s *Store) checkPromise(ctx context.Context, tx *sql.Tx, w workRow, root workRoot) error {
	if err := refuseMoney(ctx, "Owing USDC on a result"); err != nil {
		return err
	}
	if !s.WorkUSDCEnabled() {
		return errUSDCRewardsUnavailable()
	}
	if root.Kind == "simulation" {
		return invalidWorkReward()
	}
	r, err := loadWorkUSDC(ctx, tx, w.ID)
	if err != nil || r == nil {
		return err
	}
	switch r.State {
	case "paid":
		return problem(409, "work_already_paid", "This result's USDC is already paid, by transaction "+r.TxHash+"; post a new task for more work.")
	case "payable", "promised":
		return problem(409, "work_state_conflict", "This result already owes "+services.FormatUSDC(r.Amount)+" USDC; pay it and send work.settle first.")
	}
	return nil
}

// promiseWorkResult records the USDC an accepted result is owed: a new
// payable line, or the result's void one (a standing task's result paid in
// credits only) made payable, bound to its worker and payout address.
func (s *Store) promiseWorkResult(ctx context.Context, tx *sql.Tx, a actor, w workRow, amount, now int64) error {
	r, err := loadWorkUSDC(ctx, tx, w.ID)
	if err != nil {
		return err
	}
	payTo, source := "", ""
	if r != nil && r.Worker == w.Worker && r.PayTo != "" {
		payTo, source = r.PayTo, r.PayToSource
	} else if payTo, err = workPayoutAddress(ctx, tx, w.Worker); err != nil {
		return err
	} else if payTo != "" {
		source = "wallet_link"
	}
	if r != nil {
		_, err = tx.ExecContext(ctx, "UPDATE work_usdc SET amount=?,state='payable',worker=?,pay_to=?,pay_to_source=?,accepted_at=?,settled_at=0,reason='' WHERE work_id=? AND state='void'", amount, w.Worker, payTo, source, now, w.ID)
		return err
	}
	// created_at is the work's: a payment made since the work opened, even
	// before this promise, can settle it.
	c := s.config.WorkUSDC
	_, err = tx.ExecContext(ctx, "INSERT INTO work_usdc(work_id,requester,amount,network,asset,state,worker,pay_to,pay_to_source,created_at,accepted_at) VALUES(?,?,?,?,?,'payable',?,?,?,?,?)",
		w.ID, a.account, amount, c.Network, c.Asset.String(), w.Worker, payTo, source, w.Created, now)
	return err
}

// fillPayTo gives an owed line without a payout address the worker's
// verified wallet link at settle (a promise made before the worker linked
// one); without any the settle is refused, nothing changed.
func fillPayTo(ctx context.Context, tx *sql.Tx, r *usdcRow) error {
	if r.PayTo != "" {
		return nil
	}
	address, err := workPayoutAddress(ctx, tx, r.Worker)
	if err != nil {
		return err
	}
	if address == "" {
		return problem(409, "payout_address_required", "The worker has no payout address for this USDC yet: it links a wallet (standing.challenge kind wallet, then identity.link), then you pay that address and settle.")
	}
	r.PayTo, r.PayToSource = address, "wallet_link"
	_, err = tx.ExecContext(ctx, "UPDATE work_usdc SET pay_to=?,pay_to_source=? WHERE work_id=?", r.PayTo, r.PayToSource, r.WorkID)
	return err
}

// promiseNote is the ack's line on an owed promise.
func promiseNote(u *usdcRow) string {
	to := "the worker's payout address " + u.PayTo
	if u.PayTo == "" {
		to = "the worker, who has no payout address yet (it links a wallet; work.get shows it once linked)"
	}
	return services.FormatUSDC(u.Amount) + " USDC is now owed to " + to + " on " + u.Network + ": pay it, then send work.settle with the transaction's tx_hash. Until then it reads owed, not paid."
}

// payToNote says where a claimed or submitted task's USDC reward will be
// paid, so the worker checks the address before anything is owed to it.
func payToNote(u *usdcRow) string {
	if u == nil || u.State != "promised" {
		return ""
	}
	if u.PayTo == "" {
		return "The " + services.FormatUSDC(u.Amount) + " USDC reward has no payout address yet: submit names payout_address, or uses your linked wallet."
	}
	from := "your payout_address"
	if u.PayToSource == "wallet_link" {
		from = "your linked wallet"
	}
	return "The " + services.FormatUSDC(u.Amount) + " USDC reward will be paid to " + u.PayTo + " on " + u.Network + " (" + from + "); check it now, it is fixed once accepted."
}

// WorkResultMoney is what one accepted result was paid or is owed:
// Credits paid on accept, and its USDC, owed or paid.
type WorkResultMoney struct {
	Credits int64           `json:"credits_paid,omitempty"`
	USDC    *WorkResultUSDC `json:"usdc,omitempty"`
}

// WorkResultUSDC is an accepted result's USDC: State owed (Since, when it
// became owed) or paid (TxHash, TxURL on the chain's explorer, PaidAt).
// Note is the requester's line when a work.promise owed it.
type WorkResultUSDC struct {
	State  string `json:"state"`
	Amount string `json:"amount"`
	Since  int64  `json:"since,omitempty"`
	PayTo  string `json:"pay_to,omitempty"`
	TxHash string `json:"tx_hash,omitempty"`
	TxURL  string `json:"tx_url,omitempty"`
	PaidAt int64  `json:"paid_at,omitempty"`
	Note   string `json:"note,omitempty"`
}

// The states of an accepted result's USDC.
const (
	WorkUSDCOwed = "owed"
	WorkUSDCPaid = "paid"
)

// USDCTxURL is a settling transaction's page on the network's explorer, or
// "" for a network without one known here.
func USDCTxURL(network, hash string) string {
	if network != services.USDCBaseNetwork || !services.ValidTxHash(hash) {
		return ""
	}
	return "https://basescan.org/tx/" + hash
}

// workResultMoney is an accepted result's money, nil when it moved none.
func workResultMoney(ctx context.Context, tx *sql.Tx, id string) (*WorkResultMoney, error) {
	m := &WorkResultMoney{}
	c, err := loadWorkReward(ctx, tx, workRewardsTable, id)
	if err != nil {
		return nil, err
	}
	if c != nil && c.State == "paid" {
		m.Credits = c.Amount
	}
	u, err := loadWorkUSDC(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if u != nil && (u.State == "payable" || u.State == "paid") {
		v := &WorkResultUSDC{State: WorkUSDCOwed, Amount: services.FormatUSDC(u.Amount), Since: u.Accepted, PayTo: u.PayTo}
		if u.State == "paid" {
			v = &WorkResultUSDC{State: WorkUSDCPaid, Amount: services.FormatUSDC(u.Amount), PayTo: u.PayTo, TxHash: u.TxHash, TxURL: USDCTxURL(u.Network, u.TxHash), PaidAt: u.Settled}
		}
		if v.Note, err = workPromiseNote(ctx, tx, id); err != nil {
			return nil, err
		}
		m.USDC = v
	}
	if m.Credits == 0 && m.USDC == nil {
		return nil, nil
	}
	return m, nil
}

// workPromiseNote is the note of a result's newest work.promise, "" when
// none or none named one.
func workPromiseNote(ctx context.Context, tx rowQuerier, id string) (string, error) {
	var payload string
	err := tx.QueryRowContext(ctx, `SELECT payload FROM work_transitions WHERE work_id=? AND operation=? ORDER BY sequence DESC LIMIT 1`, id, WorkPromise).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	data, err := workCommandData(payload)
	var d struct {
		Note string `json:"note"`
	}
	if err != nil || json.Unmarshal([]byte(data), &d) != nil {
		return "", fmt.Errorf("board: work %s has an unreadable promise", id)
	}
	return d.Note, nil
}
