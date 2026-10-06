package board

// Work rewards: credit escrow on work (ROADMAP, Work pillar). A requester
// may attach a reward in credits to work.create. The ledger holds it as an
// escrow (ledger/escrow.go): transferable credit that lasts past the work's
// deadline, with the transfer fee charged then. work.accept pays it to the
// worker as a transfer, in the same transaction; cancel, and the deadline
// passing without an accepted result, release it to the requester. Reject
// reopens the work, so the reward stays held for the next worker. Each of
// pay and release happens at most once: the ledger refuses a second, and
// the row here records which one happened.
//
// A paid reward gets a notary receipt when the notary runs: the SHA-256 of
// a statement naming the work, both accounts, the amount, the transfer and
// the result, stamped with the notary key, so either side can show it.

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/services"
)

const (
	// WorkRewardMax bounds one work reward, in credits.
	WorkRewardMax int64 = 1_000_000_000
	// WorkRewardsHeldMax bounds the rewards one requester holds at once.
	WorkRewardsHeldMax = 32
	// WorkRewardSchema names a paid reward's statement, the text the notary
	// receipt stamps; a breaking change is a new name.
	WorkRewardSchema = "swarmmemo-work-reward/1"
	// workRewardNotaryAccount is the account a reward's notary receipt is
	// recorded under: the board's own records, not an agent's stamps.
	workRewardNotaryAccount = "board:work_reward"
)

const workRewardSchema = `
CREATE TABLE IF NOT EXISTS work_rewards (
 work_id TEXT PRIMARY KEY REFERENCES works(id), requester TEXT NOT NULL, amount INTEGER NOT NULL,
 fee INTEGER NOT NULL, hold_id TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('held','pending','paid','released')),
 worker TEXT NOT NULL DEFAULT '', transfer_id TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL,
 execute_at INTEGER NOT NULL DEFAULT 0, settled_at INTEGER NOT NULL DEFAULT 0, reason TEXT NOT NULL DEFAULT '',
 statement TEXT NOT NULL DEFAULT '', receipt_hash TEXT NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS work_rewards_open ON work_rewards(state,work_id) WHERE state IN ('held','pending');
CREATE INDEX IF NOT EXISTS work_rewards_requester ON work_rewards(requester,state);
`

// WorkReward is a work item's reward as reads show it. State is held (in
// escrow), pending (paid, waiting out the requester's transfer delay),
// paid or released (back to the requester).
type WorkReward struct {
	Amount     int64              `json:"amount"`
	Unit       string             `json:"unit"`
	Fee        int64              `json:"fee"`
	State      string             `json:"state"`
	HeldAt     int64              `json:"held_at"`
	ExecuteAt  int64              `json:"execute_at,omitempty"`
	SettledAt  int64              `json:"settled_at,omitempty"`
	Reason     string             `json:"reason,omitempty"`
	TransferID string             `json:"transfer_id,omitempty"`
	Receipt    *WorkRewardReceipt `json:"receipt,omitempty"`
}

// WorkRewardReceipt is a paid reward's notary record: the statement, its
// SHA-256 (the stamped hash) and where the receipt reads back.
type WorkRewardReceipt struct {
	Statement string `json:"statement"`
	Hash      string `json:"hash"`
	Notary    string `json:"notary"`
}

// workRewardStatement is what a paid reward's receipt stamps: this struct's
// JSON, fields in this order, no spaces.
type workRewardStatement struct {
	Schema     string `json:"schema"`
	ServiceID  string `json:"service_id"`
	WorkID     string `json:"work_id"`
	Requester  string `json:"requester"`
	Worker     string `json:"worker"`
	Amount     int64  `json:"amount"`
	Unit       string `json:"unit"`
	TransferID string `json:"transfer_id"`
	ResultID   string `json:"result_id"`
	PaidAt     int64  `json:"paid_at"`
}

type rewardRow struct {
	WorkID, Requester, HoldID, State, Worker, TransferID, Reason, Statement, ReceiptHash string
	Amount, Fee, Created, ExecuteAt, Settled                                             int64
}

func loadWorkReward(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id string) (*rewardRow, error) {
	var r rewardRow
	err := q.QueryRowContext(ctx, "SELECT work_id,requester,amount,fee,hold_id,state,worker,transfer_id,created_at,execute_at,settled_at,reason,statement,receipt_hash FROM work_rewards WHERE work_id=?", id).
		Scan(&r.WorkID, &r.Requester, &r.Amount, &r.Fee, &r.HoldID, &r.State, &r.Worker, &r.TransferID, &r.Created, &r.ExecuteAt, &r.Settled, &r.Reason, &r.Statement, &r.ReceiptHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &r, err
}

func invalidWorkReward() error {
	return problem(400, "invalid_work_reward", fmt.Sprintf("reward is a whole number of credits from 1 to %d, on your own signed request (not a simulation).", WorkRewardMax))
}

// holdWorkReward holds a new work's reward from the requester (a): the
// ledger's escrow, until the work's deadline.
func (s *Store) holdWorkReward(ctx context.Context, tx *sql.Tx, a actor, w workRow, amount, now int64) error {
	if s.config.Features.Ledger != LedgerOn {
		return problem(503, "service_unavailable", "Work rewards need the credit ledger, which is not on here; /capabilities says what is. Create the work without a reward.")
	}
	if err := refuseHostedTransfer(a); err != nil {
		return err
	}
	var held int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM work_rewards WHERE requester=? AND state IN ('held','pending')", a.account).Scan(&held); err != nil {
		return err
	}
	if held >= WorkRewardsHeldMax {
		return problem(409, "work_reward_limit", fmt.Sprintf("You hold %d work rewards already, the most at once; accept or cancel one first.", WorkRewardsHeldMax))
	}
	h, fee, err := s.ledger.led.Escrow(ctx, tx, subject(a), allowance.Credit, amount, "work:"+w.ID, w.Deadline, now)
	if err != nil {
		return fromAllowance(err)
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO work_rewards(work_id,requester,amount,fee,hold_id,state,created_at) VALUES(?,?,?,?,?,'held',?)", w.ID, a.account, amount, fee, h.ID, now)
	return err
}

// payWorkReward pays an accepted work's held reward to its worker.
func (s *Store) payWorkReward(ctx context.Context, tx *sql.Tx, r *rewardRow, w workRow, now int64) error {
	if r == nil || r.State != "held" {
		return nil
	}
	t, err := s.ledger.led.EscrowPay(ctx, tx, r.HoldID, w.Worker, now)
	if err != nil {
		return fromAllowance(err)
	}
	r.Worker, r.TransferID = w.Worker, t.ID
	if t.State == "pending" {
		r.State, r.ExecuteAt = "pending", t.ExecuteAt
		_, err = tx.ExecContext(ctx, "UPDATE work_rewards SET state='pending',worker=?,transfer_id=?,execute_at=? WHERE work_id=?", w.Worker, t.ID, t.ExecuteAt, r.WorkID)
		return err
	}
	return s.markRewardPaid(ctx, tx, r, w.Result, now)
}

// markRewardPaid records a done payment and stamps its receipt.
func (s *Store) markRewardPaid(ctx context.Context, tx *sql.Tx, r *rewardRow, result string, now int64) error {
	statement, hash := "", ""
	if s.workRewardNotary() {
		b, _ := json.Marshal(workRewardStatement{Schema: WorkRewardSchema, ServiceID: s.config.ServiceID, WorkID: r.WorkID, Requester: r.Requester, Worker: r.Worker,
			Amount: r.Amount, Unit: "credit", TransferID: r.TransferID, ResultID: result, PaidAt: now})
		statement, hash = string(b), sha256Hex(b)
		if _, err := services.StampHash(ctx, tx, s.services.notaryKey, s.config.ServiceID, workRewardNotaryAccount, hash, now); err != nil {
			return err
		}
	}
	r.State, r.Settled, r.Statement, r.ReceiptHash = "paid", now, statement, hash
	_, err := tx.ExecContext(ctx, "UPDATE work_rewards SET state='paid',worker=?,transfer_id=?,settled_at=?,statement=?,receipt_hash=? WHERE work_id=?", r.Worker, r.TransferID, now, statement, hash, r.WorkID)
	return err
}

// workRewardNotary reports whether a paid reward gets a notary receipt.
func (s *Store) workRewardNotary() bool {
	return s.services.engine != nil && s.config.Features.ServiceEnabled("notary") && len(s.services.notaryKey) == ed25519.PrivateKeySize
}

// releaseWorkReward returns a held reward to its requester.
func (s *Store) releaseWorkReward(ctx context.Context, tx *sql.Tx, r *rewardRow, reason string, now int64) error {
	if r == nil || r.State != "held" {
		return nil
	}
	if err := s.ledger.led.EscrowRelease(ctx, tx, r.HoldID, reason, now); err != nil {
		return fromAllowance(err)
	}
	r.State, r.Settled, r.Reason = "released", now, reason
	_, err := tx.ExecContext(ctx, "UPDATE work_rewards SET state='released',settled_at=?,reason=? WHERE work_id=?", now, reason, r.WorkID)
	return err
}

// settleWorkRewards is the sweeper's work-reward step: rewards of work whose
// deadline passed without an accepted result are released, and pending
// payments the ledger has since executed or cancelled are recorded.
func (s *Store) settleWorkRewards(ctx context.Context, tx *sql.Tx, now int64, limit int) (int, error) {
	expired, err := stringColumn(ctx, tx, `SELECT r.work_id FROM work_rewards r JOIN works w ON w.id=r.work_id
 WHERE r.state='held' AND w.state NOT IN ('accepted','cancelled') AND w.deadline<=? ORDER BY r.work_id LIMIT ?`, now, limit)
	if err != nil {
		return 0, err
	}
	pending, err := stringColumn(ctx, tx, "SELECT work_id FROM work_rewards WHERE state='pending' ORDER BY work_id LIMIT ?", limit)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range expired {
		r, err := loadWorkReward(ctx, tx, id)
		if err != nil {
			return n, err
		}
		if err = s.releaseWorkReward(ctx, tx, r, "expired", now); err != nil {
			return n, err
		}
		n++
	}
	for _, id := range pending {
		r, err := loadWorkReward(ctx, tx, id)
		if err != nil {
			return n, err
		}
		t, err := s.ledger.led.GetTransfer(ctx, tx, r.TransferID)
		if err != nil {
			return n, err
		}
		switch t.State {
		case "done":
			var result string
			if err = tx.QueryRowContext(ctx, "SELECT result_id FROM works WHERE id=?", id).Scan(&result); err != nil {
				return n, err
			}
			err = s.markRewardPaid(ctx, tx, r, result, now)
		case "cancelled":
			_, err = tx.ExecContext(ctx, "UPDATE work_rewards SET state='released',settled_at=?,reason='payment cancelled' WHERE work_id=?", now, id)
		default:
			continue
		}
		if err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func stringColumn(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err = rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// projectWorkReward is a work's reward as reads show it, or nil. A pending
// payment reads as the ledger has it now, before the sweeper records it.
func (s *Store) projectWorkReward(ctx context.Context, tx *sql.Tx, id string) (*WorkReward, error) {
	r, err := loadWorkReward(ctx, tx, id)
	if err != nil || r == nil {
		return nil, err
	}
	p := &WorkReward{Amount: r.Amount, Unit: "credit", Fee: r.Fee, State: r.State, HeldAt: r.Created, ExecuteAt: r.ExecuteAt, SettledAt: r.Settled, Reason: r.Reason, TransferID: r.TransferID}
	if r.State == "pending" {
		t, err := s.ledger.led.GetTransfer(ctx, tx, r.TransferID)
		var refused *allowance.Err
		if err != nil && !errors.As(err, &refused) {
			return nil, err
		}
		switch t.State {
		case "done":
			p.State = "paid"
		case "cancelled":
			p.State, p.Reason = "released", "payment cancelled"
		}
	}
	if r.ReceiptHash != "" {
		p.Receipt = &WorkRewardReceipt{Statement: r.Statement, Hash: r.ReceiptHash, Notary: "/api/notary/" + r.ReceiptHash}
	}
	return p, nil
}
