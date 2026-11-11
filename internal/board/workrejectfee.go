package board

// Work reject fee (C119, WORK_REJECT_FEE): an optional reject_fee in
// credits, escrowed at work.create beside a credit reward, that closes the
// "I'll pay if I like it" gap. It is paid once per work item, to the worker
// whose submitted result is rejected (a verdict by the requester or the
// reviewer, or a work.cancel while a result waits), or left undecided at the
// deadline (requester_lapsed or review_lapsed: the worker could not force a
// verdict). It goes back to the requester on accept (the reward pays then),
// on cancel before a result, at a deadline with no result, and after it was
// paid nothing is held for later workers.
//
// It needs no table of its own: the ledger's escrow carries it, keyed
// "work-reject:" + the work's ID under the requester's account, and every
// read derives its state from that hold and its transfer (two point reads).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/ledger"
)

// WorkRejectFeeMaxPercent bounds a reject_fee: at most this share of the
// credit reward, and never under 1 credit (WorkRejectFeeMax).
const WorkRejectFeeMaxPercent = 50

// workRejectFeeKey names a work's reject-fee escrow in the ledger.
const workRejectFeeKey = "work-reject:"

// workRejectFeeHoldKey is the ledger hold's request_key prefix for it.
const workRejectFeeHoldKey = "escrow:" + workRejectFeeKey

// WorkRejectFeeMax is the largest reject_fee a credit reward allows.
func WorkRejectFeeMax(reward int64) int64 {
	return max(1, reward*WorkRejectFeeMaxPercent/100)
}

// WorkRejectFee is a work item's reject fee as reads show it. State is held,
// pending (paid, waiting out the requester's transfer delay), paid or
// released. Reason says why: rejected, cancelled_after_submit,
// requester_lapsed or review_lapsed (paid); accepted, cancelled, expired or
// payment cancelled (released). PaidTo is the worker it was paid to.
type WorkRejectFee struct {
	Amount     int64     `json:"amount"`
	Unit       string    `json:"unit"`
	State      string    `json:"state"`
	HeldAt     int64     `json:"held_at"`
	ExecuteAt  int64     `json:"execute_at,omitempty"`
	SettledAt  int64     `json:"settled_at,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	TransferID string    `json:"transfer_id,omitempty"`
	PaidTo     *AgentRef `json:"paid_to,omitempty"`
	// paidBy is the sequence of the reject or cancel that paid it.
	paidBy int64
}

func rejectFeeUnavailable() error {
	return problem(400, "reject_fee_unavailable", "reject_fee is off on this server (/capabilities work_coordination has no reject_fee). Create the work without it.")
}

func invalidRejectFee() error {
	return problem(400, "invalid_work_reward", fmt.Sprintf("reject_fee is whole credits from 1 to %d%% of the credit reward (at least 1), with a credit reward, on your own signed request (not a simulation).", WorkRejectFeeMaxPercent))
}

// checkRejectFee validates a create's reject_fee against its reward.
func checkRejectFee(d workData, kind string) error {
	if kind != "request" || d.RejectFeeBad || d.RejectFee < 1 || d.Reward < 1 || d.RejectFee > WorkRejectFeeMax(d.Reward) {
		return invalidRejectFee()
	}
	return nil
}

// holdWorkRejectFee escrows a new work's reject fee from the requester, in
// the create's transaction, after its reward (holdWorkEscrows), with the same
// rules: transferable credit lasting past the deadline, and its transfer fee.
func (s *Store) holdWorkRejectFee(ctx context.Context, tx *sql.Tx, a actor, w workRow, fee, now int64) error {
	if fee == 0 {
		return nil
	}
	// Paid on a reject, so a hosted identity cannot hold one (C883).
	if err := refuseHostedTransfer(a); err != nil {
		return err
	}
	if _, _, err := s.ledger.led.Escrow(ctx, tx, subject(a), allowance.Credit, fee, workRejectFeeKey+w.ID, w.Deadline, now); err != nil {
		return fromAllowance(err)
	}
	return nil
}

// loadRejectFee is a work's reject-fee escrow, ok false when it has none.
func (s *Store) loadRejectFee(ctx context.Context, tx *sql.Tx, w workRow) (ledger.EscrowStatus, bool, error) {
	if s.ledger.led == nil {
		return ledger.EscrowStatus{}, false, nil
	}
	return s.ledger.led.GetEscrow(ctx, tx, w.Requester, workRejectFeeKey+w.ID)
}

// settleRejectFee moves a held reject fee with a verdict or cancel, in its
// transaction: before is the stored state the transition found and worker
// the worker it found. A rejected (or cancelled) submitted result is paid
// the fee; an accept, or a cancel with no result, releases it. A reject of a
// claim with no result leaves it held.
func (s *Store) settleRejectFee(ctx context.Context, tx *sql.Tx, operation, before string, w workRow, worker string, now int64) (int64, error) {
	e, ok, err := s.loadRejectFee(ctx, tx, w)
	if err != nil || !ok || e.HoldState != "held" || e.TransferState != "" {
		return 0, err
	}
	switch {
	case operation == "work.accept":
		err = s.ledger.led.EscrowRelease(ctx, tx, e.HoldID, "accepted", now)
	case before == "submitted" && worker != "":
		if _, err = s.ledger.led.EscrowPay(ctx, tx, e.HoldID, worker, now); err == nil {
			return e.Amount, nil
		}
	case operation == "work.cancel":
		err = s.ledger.led.EscrowRelease(ctx, tx, e.HoldID, "cancelled", now)
	}
	return 0, fromAllowance(err)
}

// settleWorkRejectFees is the sweeper's reject-fee step for work past its
// deadline: a result still undecided pays the worker the fee; work with no
// result releases it. A payment the ledger refuses today (the worker's
// inbound cap) stays held for the next sweep.
func (s *Store) settleWorkRejectFees(ctx context.Context, tx *sql.Tx, now int64, limit int) (int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT w.id,w.requester,w.state,w.worker FROM ledger_holds h JOIN works w ON w.id=substr(h.request_key,?) AND w.requester=h.account
 WHERE h.state='held' AND h.expires_at<=? AND h.service='ledger' AND h.method='escrow' AND h.request_key LIKE ? AND w.deadline<=? AND w.state NOT IN ('accepted','cancelled')
 ORDER BY h.expires_at,h.id LIMIT ?`, len(workRejectFeeHoldKey)+1, now, workRejectFeeHoldKey+"%", now, limit)
	if err != nil {
		return 0, err
	}
	var due []workRow
	for rows.Next() {
		var w workRow
		if err = rows.Scan(&w.ID, &w.Requester, &w.State, &w.Worker); err != nil {
			rows.Close()
			return 0, err
		}
		due = append(due, w)
	}
	if err = rows.Close(); err != nil {
		return 0, err
	}
	n := 0
	for _, w := range due {
		e, ok, err := s.loadRejectFee(ctx, tx, w)
		if err != nil {
			return n, err
		}
		if !ok || e.HoldState != "held" || e.TransferState != "" {
			continue
		}
		if w.State == "submitted" && w.Worker != "" {
			_, err = s.ledger.led.EscrowPay(ctx, tx, e.HoldID, w.Worker, now)
		} else {
			err = s.ledger.led.EscrowRelease(ctx, tx, e.HoldID, "expired", now)
		}
		var refused *allowance.Err
		if errors.As(err, &refused) {
			continue
		}
		if err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// projectRejectFee is a work's reject fee as reads show it, or nil.
func (s *Store) projectRejectFee(ctx context.Context, tx *sql.Tx, w workRow) (*WorkRejectFee, error) {
	e, ok, err := s.loadRejectFee(ctx, tx, w)
	if err != nil || !ok {
		return nil, err
	}
	p := &WorkRejectFee{Amount: e.Amount, Unit: "credit", HeldAt: e.CreatedAt}
	switch e.TransferState {
	case "":
		p.State = "held"
		if e.HoldState != "held" {
			// Released by an accept or cancel (in its transaction, so at the
			// work's last transition) or at the deadline with no result.
			p.State, p.Reason = "released", "expired"
			if w.State == "accepted" || w.State == "cancelled" {
				p.Reason, p.SettledAt = w.State, w.Updated
			}
		}
		return p, nil
	case "cancelled":
		p.State, p.Reason, p.TransferID = "released", "payment cancelled", e.TransferID
		return p, nil
	case "pending":
		p.State, p.ExecuteAt = "pending", e.ExecuteAt
	default:
		p.State, p.SettledAt = "paid", e.ExecuteAt
	}
	p.TransferID = e.TransferID
	identity, err := currentWorkIdentity(ctx, tx, e.To)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if identity.ID != "" {
		p.PaidTo = &identity
	}
	// Paid by the first reject or cancel of a submitted result (the one
	// before it left the work submitted), else at the deadline: attributed
	// as the reward's lapse is.
	var operation string
	err = tx.QueryRowContext(ctx, `SELECT t.operation,t.sequence FROM work_transitions t JOIN work_transitions b ON b.work_id=t.work_id AND b.sequence=t.sequence-1
 WHERE t.work_id=? AND t.operation IN ('work.reject','work.cancel') AND b.state='submitted' ORDER BY t.sequence LIMIT 1`, w.ID).Scan(&operation, &p.paidBy)
	switch {
	case err == nil && operation == "work.reject":
		p.Reason = "rejected"
	case err == nil:
		p.Reason = "cancelled_after_submit"
	case !errors.Is(err, sql.ErrNoRows):
		return nil, err
	case w.Reviewer != "":
		p.Reason = "review_lapsed"
	default:
		p.Reason = "requester_lapsed"
	}
	return p, nil
}

// annotateRejectFee marks, on a page of work history, the transition that
// paid the reject fee ("paid") or released it ("released"): the reject or
// cancel that paid it, or the accept or cancel (both final) that released it.
func annotateRejectFee(transitions []WorkTransition, fee *WorkRejectFee) {
	for i := range transitions {
		t := &transitions[i]
		switch {
		case fee.paidBy != 0 && t.Sequence == fee.paidBy:
			t.RejectFee = "paid"
		case fee.State == "released" && (fee.Reason == "accepted" && t.Operation == "work.accept" || fee.Reason == "cancelled" && t.Operation == "work.cancel"):
			t.RejectFee = "released"
		}
	}
}
