package board

// Split bounty payout (C307, ROADMAP 4.9/4.10, RFC 0015 §5.6 "split bounty
// payouts; home shows settling payments"). This is the anti-farming payout
// that RFC 0020 points at: money for the board comes from accepted work, paid
// so a ring cannot grab it and run.
//
// When SPLIT_PAYOUT is on and the versioned split_payout params are enabled, an
// accept of a NON-standing work item with a credit reward paid to a
// NON-newcomer worker splits the payout: half now (upfront) and half held for
// split_payout.deferral_days (the deferred half), settled to the worker unless
// a contradiction or affiliation discovered meanwhile claws it back to the
// requester. It mirrors §5.6's 72-hour review-reward hold, extended to 14 days
// for the deferred half of a cash payout.
//
// No schema change: the deferred half lives in the EXISTING work_rewards row.
// A split accept leaves the row in state 'held' (now meaning "deferred half
// settling"), with worker set, execute_at = the settle-on time, and hold_id the
// reward escrow (now holding exactly the deferred half, the upfront half having
// been carved out and paid). The marker that a held row is settling rather than
// awaiting an accept is worker<>'' AND execute_at>0 (an ordinary pre-accept
// held reward has worker='' and execute_at=0).
//
// A newcomer's FIRST accepted bounty pays in full upfront, to lower
// first-contact friction. "First" is per account (the payee, not a handle or a
// key): no prior work_rewards row ever recorded this account as worker.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/ledger"
	"swarmmemo/internal/services"
)

// SplitPayoutParamsNamespace holds the split-payout numbers, versioned like
// every other operator parameter (swarmmemo params set split_payout).
const SplitPayoutParamsNamespace = "split_payout"

// The ledger request keys of a split's two escrows: the deferred half stays in
// the reward escrow (work:<id>), the upfront half is carved into work-up:<id>.
const splitUpfrontKey = "work-up:"

// The reason prefixes a settled or clawed-back deferred half carries, so a
// terminal split is told apart from an ordinary paid or released reward.
const (
	splitSettledReason = "settled_split"
	splitClawedPrefix  = "clawed_back:"
)

// SplitPayoutEnabled reports whether SPLIT_PAYOUT is on.
func (s *Store) SplitPayoutEnabled() bool { return s.config.Features.SplitPayout }

// workSplitNote is a split accept's acknowledgement: what was paid now and
// what settles when.
func workSplitNote(upfront, deferred, settlesAt int64) string {
	return fmt.Sprintf("%d credits go to the worker now; %d credits settle on %s unless a contradiction or affiliation claws them back. Earned credits fund a paid task: %s",
		upfront, deferred, time.Unix(settlesAt, 0).UTC().Format("2006-01-02"), WorkSpendHow)
}

// splitPayoutParams are the split-payout numbers (the namespace's body).
type splitPayoutParams struct {
	Schema  int  `json:"schema"`
	Enabled bool `json:"enabled"`
	// DeferralDays is how long the deferred half is held before it settles.
	DeferralDays int64 `json:"deferral_days"`
	// DeferredPPM is the share of the reward deferred, in ppm (500000 = half).
	DeferredPPM int64 `json:"deferred_ppm"`
	// MinSplitCredits is the smallest reward that splits; below it an accept
	// pays in full (so both halves are at least 1 credit).
	MinSplitCredits int64 `json:"min_split_credits"`
}

func defaultSplitPayoutParams() splitPayoutParams {
	return splitPayoutParams{Schema: 1, Enabled: true, DeferralDays: 14, DeferredPPM: 500_000, MinSplitCredits: 2}
}

var splitPayoutKeys = []string{"schema", "enabled", "deferral_days", "deferred_ppm", "min_split_credits"}

func parseSplitPayoutParams(body []byte) (splitPayoutParams, error) {
	var p splitPayoutParams
	shape := fmt.Sprintf("split_payout params: a strict JSON object of %v", splitPayoutKeys)
	var keys map[string]json.RawMessage
	if json.Unmarshal(body, &keys) != nil || len(keys) != len(splitPayoutKeys) {
		return p, errors.New(shape)
	}
	for _, k := range splitPayoutKeys {
		if _, ok := keys[k]; !ok {
			return p, errors.New(shape)
		}
	}
	if err := services.StrictObject(body, &p); err != nil {
		return p, errors.New(shape)
	}
	for _, c := range []struct {
		name      string
		v, lo, hi int64
	}{{"schema", int64(p.Schema), 1, 1}, {"deferral_days", p.DeferralDays, 1, 365},
		{"deferred_ppm", p.DeferredPPM, 1, 999_999}, {"min_split_credits", p.MinSplitCredits, 2, 1_000_000}} {
		if c.v < c.lo || c.v > c.hi {
			return p, fmt.Errorf("split_payout params: %s is %d to %d", c.name, c.lo, c.hi)
		}
	}
	return p, nil
}

func init() {
	ledger.RegisterNamespace(SplitPayoutParamsNamespace, ledger.Namespace{
		Version: 0,
		Body: func() []byte {
			b, _ := json.Marshal(defaultSplitPayoutParams())
			return b
		},
		Validate: func(b []byte) error { _, err := parseSplitPayoutParams(b); return err },
	})
}

// splitTerms are the split-payout params in effect and whether splitting runs.
type splitTerms struct {
	splitPayoutParams
	version int64
	on      bool
}

// splitPayoutTerms reads the terms through q. Splitting runs only with the flag
// on, the ledger on and the params enabled.
func (s *Store) splitPayoutTerms(ctx context.Context, q allowance.Querier, now int64) (splitTerms, error) {
	t := splitTerms{splitPayoutParams: defaultSplitPayoutParams()}
	if !s.SplitPayoutEnabled() || s.config.Features.Ledger != LedgerOn {
		return t, nil
	}
	version, body, err := s.ledger.params.Params(ctx, q, SplitPayoutParamsNamespace, now)
	if err != nil {
		return t, err
	}
	if len(body) > 0 {
		if t.splitPayoutParams, err = parseSplitPayoutParams(body); err != nil {
			return t, err
		}
	}
	t.version = version
	t.on = t.Enabled
	return t, nil
}

// deferredAmount is the share of reward to defer, floored, with the odd credit
// paid upfront; 0 when the reward is too small to split into two whole halves.
func (t splitTerms) deferredAmount(reward int64) int64 {
	if reward < t.MinSplitCredits {
		return 0
	}
	d := reward * t.DeferredPPM / 1_000_000
	if d < 1 || reward-d < 1 {
		return 0
	}
	return d
}

// payWorkRewardSplit pays an accepted work item's reward: the whole reward at
// once (the pre-C307 path) for a newcomer, a standing task, a USDC-bearing or
// too-small reward, or while SPLIT_PAYOUT is off; otherwise half now and half
// held to settle in split_payout.deferral_days. It replaces the direct
// payWorkReward call on work.accept for the reward (not the reviewer fee).
func (s *Store) payWorkRewardSplit(ctx context.Context, tx *sql.Tx, r *rewardRow, w *workRow, now int64) error {
	if r == nil || r.State != "held" {
		return nil
	}
	deferred, err := s.splitDeferred(ctx, tx, r, w, now)
	if err != nil {
		return err
	}
	if deferred <= 0 {
		return s.payWorkReward(ctx, tx, r, w.Worker, w.Result, now)
	}
	return s.splitPayout(ctx, tx, r, w, deferred, now)
}

// splitDeferred is the credits to defer on this accept, or 0 to pay in full.
func (s *Store) splitDeferred(ctx context.Context, tx *sql.Tx, r *rewardRow, w *workRow, now int64) (int64, error) {
	if w.StandingID != "" { // standing-task pools pay in full (scope: C307 v1)
		return 0, nil
	}
	t, err := s.splitPayoutTerms(ctx, tx, now)
	if err != nil || !t.on {
		return 0, err
	}
	deferred := t.deferredAmount(r.Amount)
	if deferred <= 0 {
		return 0, nil
	}
	// A USDC-bearing reward pays its credit half in full: its receipt rides
	// with the USDC assets, which settle on their own rail.
	withUSDC, err := workHasUSDC(ctx, tx, r.WorkID)
	if err != nil || withUSDC {
		return 0, err
	}
	newcomer, err := s.workNewcomer(ctx, tx, w.Worker)
	if err != nil || newcomer {
		return 0, err
	}
	return deferred, nil
}

// workNewcomer reports whether account has never had a work reward accepted:
// no prior work_rewards row records it as worker (worker is set only when an
// accept pays or defers a reward). It counts the payee ACCOUNT, so it cannot be
// reset by a new handle or a new key under the same account.
func (s *Store) workNewcomer(ctx context.Context, tx *sql.Tx, account string) (bool, error) {
	var exists bool
	err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM work_rewards WHERE worker=?)", account).Scan(&exists)
	return !exists, err
}

// splitPayout carves the upfront half out of the reward escrow and pays it now,
// then leaves the row settling the deferred half (held in the reward escrow)
// until now + deferral_days. One transaction; every move is keyed by the work
// id, so a retry repeats nothing.
func (s *Store) splitPayout(ctx context.Context, tx *sql.Tx, r *rewardRow, w *workRow, deferred, now int64) error {
	t, err := s.splitPayoutTerms(ctx, tx, now)
	if err != nil {
		return err
	}
	upfront := r.Amount - deferred
	// Carve the upfront half into its own escrow (no fee) and pay it now; the
	// reward escrow (r.HoldID, work:<id>) keeps exactly the deferred half.
	up, err := s.ledger.led.EscrowSplit(ctx, tx, r.HoldID, upfront, splitUpfrontKey+r.WorkID, w.Deadline, now)
	if err != nil {
		return fromAllowance(err)
	}
	paid, err := s.ledger.led.EscrowPay(ctx, tx, up.ID, w.Worker, now)
	if err != nil {
		return fromAllowance(err)
	}
	if paid.State == "done" {
		if err = s.stampSplitUpfront(ctx, tx, r, w.Result, paid, now); err != nil {
			return err
		}
	}
	// The row stays 'held' but now settles the deferred half: worker set,
	// execute_at = the settle-on time, reviewer preserved for the receipt.
	settleAt := now + t.DeferralDays*86400
	res, err := tx.ExecContext(ctx, "UPDATE work_rewards SET worker=?,execute_at=?,reviewer=? WHERE work_id=? AND state='held'",
		w.Worker, settleAt, r.Reviewer, r.WorkID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return errors.Join(err, fmt.Errorf("split payout: reward %s is no longer held", r.WorkID))
	}
	r.State, r.Worker, r.ExecuteAt = "held", w.Worker, settleAt
	return nil
}

// stampSplitUpfront stamps the notary receipt of a split's upfront payment (the
// amount actually transferred), writing it to the row WITHOUT changing the
// row's state; the deferred settlement later overwrites the pointer with its
// own stamp, and both stamps persist in the notary store.
func (s *Store) stampSplitUpfront(ctx context.Context, tx *sql.Tx, r *rewardRow, result string, paid ledger.Transfer, now int64) error {
	if !s.workRewardNotary() {
		return nil
	}
	b, _ := json.Marshal(workRewardStatement{Schema: WorkRewardSchema, ServiceID: s.config.ServiceID, WorkID: r.WorkID, Requester: r.Requester,
		Worker: paid.To, Amount: paid.Amount, Unit: "credit", TransferID: paid.ID, ResultID: result, PaidAt: now, Reviewer: r.Reviewer, DecidedBy: r.DecidedBy})
	statement, hash := string(b), sha256Hex(b)
	if _, err := services.StampHash(ctx, tx, s.services.notaryKey, s.config.ServiceID, workRewardNotaryAccount, hash, now); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "UPDATE work_rewards SET statement=?,receipt_hash=? WHERE work_id=?", statement, hash, r.WorkID)
	return err
}

// settleSplitPayouts settles the deferred halves whose settle-on time has
// passed, oldest first: it pays the worker when the accept still stands, or
// claws the half back to the requester when a contradiction or affiliation is
// found (splitClawback). Each half settles once (the state='held' guard on the
// escrow and the row). Nothing here mints; it only moves the held escrow.
func (s *Store) settleSplitPayouts(ctx context.Context, tx *sql.Tx, now int64, limit int) (int, error) {
	if !s.SplitPayoutEnabled() || s.config.Features.Ledger != LedgerOn {
		return 0, nil
	}
	ids, err := stringColumn(ctx, tx, "SELECT work_id FROM work_rewards WHERE state='held' AND worker<>'' AND execute_at>0 AND execute_at<=? ORDER BY execute_at,work_id LIMIT ?", now, limit)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		r, err := loadWorkReward(ctx, tx, workRewardsTable, id)
		if err != nil {
			return n, err
		}
		if r == nil || r.State != "held" {
			continue
		}
		reason, err := s.splitClawback(ctx, tx, r, now)
		if err != nil {
			return n, err
		}
		if reason != "" {
			if err = s.releaseWorkReward(ctx, tx, r, splitClawedPrefix+reason, now); err != nil {
				return n, err
			}
			n++
			continue
		}
		var result string
		if err = tx.QueryRowContext(ctx, "SELECT result_id FROM works WHERE id=?", id).Scan(&result); err != nil {
			return n, err
		}
		// An accept a silent reviewer's requester decided names who decided, as
		// settleWorkEscrows does, so the deferred receipt reads like the upfront.
		if err = tx.QueryRowContext(ctx, `SELECT coalesce((SELECT t.author FROM work_transitions t JOIN works w ON w.id=t.work_id
 WHERE t.work_id=? AND t.operation='work.accept' AND w.reviewer<>'' AND coalesce((SELECT i.account FROM identities i WHERE i.id=t.author),'')<>w.reviewer
 ORDER BY t.sequence DESC LIMIT 1),'')`, id).Scan(&r.DecidedBy); err != nil {
			return n, err
		}
		if err = s.payWorkReward(ctx, tx, r, r.Worker, result, now); err != nil {
			return n, err
		}
		// Mark a done settlement so a settled split is told from an ordinary
		// paid reward; a rare pending one completes as an ordinary paid reward.
		if r.State == "paid" {
			if _, err = tx.ExecContext(ctx, "UPDATE work_rewards SET reason=? WHERE work_id=? AND state='paid'", splitSettledReason, id); err != nil {
				return n, err
			}
		}
		n++
	}
	return n, nil
}

// splitClawback returns the reason the deferred half is clawed back to the
// requester, or "" to pay the worker. It mirrors §5.6's reward cancel logic
// adapted to work: the accepted result was hidden (the contradiction), or the
// worker is found affiliated with the requester (self-dealing).
func (s *Store) splitClawback(ctx context.Context, tx *sql.Tx, r *rewardRow, now int64) (string, error) {
	var hidden bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM works w JOIN events h ON (h.id=w.result_id OR (h.origin=w.result_id AND h.origin<>'')) WHERE w.id=? AND h.hidden<>0)`, r.WorkID).Scan(&hidden); err != nil {
		return "", err
	}
	if hidden {
		return "result_hidden", nil
	}
	var linked bool
	// workLinkedSQL places the worker first in its SQL, then the requester.
	if err := tx.QueryRowContext(ctx, "SELECT "+workLinkedSQL("?", "?"), r.Worker, r.Requester).Scan(&linked); err != nil {
		return "", err
	}
	if linked {
		return "affiliated", nil
	}
	rw, err := s.accountRoot(ctx, tx, r.Worker, now)
	if err != nil {
		return "", err
	}
	rr, err := s.accountRoot(ctx, tx, r.Requester, now)
	if err != nil {
		return "", err
	}
	if rw != "" && rw == rr {
		return "affiliated", nil
	}
	return "", nil
}

// WorkSettlement is a split payout's deferred half as reads show it: how much
// was paid upfront, how much is deferred, when it settles and its state
// (settling, settled or clawed, with the clawback reason).
type WorkSettlement struct {
	State     string `json:"state"`
	Upfront   int64  `json:"upfront"`
	Deferred  int64  `json:"deferred"`
	Unit      string `json:"unit"`
	SettlesAt int64  `json:"settles_at"`
	Reason    string `json:"reason,omitempty"`
}

// splitState is a work_rewards row's settlement state, or "" when the row is
// not a split payout (worker<>'' and execute_at>0 mark a split).
func splitState(r *rewardRow) string {
	if r.Table != workRewardsTable || r.Worker == "" || r.ExecuteAt == 0 {
		return ""
	}
	switch {
	case r.State == "held":
		return "settling"
	case r.State == "paid" && r.Reason == splitSettledReason:
		return "settled"
	case r.State == "released" && strings.HasPrefix(r.Reason, splitClawedPrefix):
		return "clawed"
	}
	return ""
}

// workSettlement reads a row's settlement, or nil when it is not a split. The
// deferred amount is the reward escrow's held units (unchanged by a pay or a
// release), so upfront = reward − deferred is exact in every state.
func (s *Store) workSettlement(ctx context.Context, q allowance.Querier, r *rewardRow) (*WorkSettlement, error) {
	state := splitState(r)
	if state == "" {
		return nil, nil
	}
	e, ok, err := s.ledger.led.GetEscrow(ctx, q, r.Requester, "work:"+r.WorkID)
	if err != nil {
		return nil, err
	}
	deferred := r.Amount
	if ok {
		deferred = e.Amount
	}
	out := &WorkSettlement{State: state, Deferred: deferred, Upfront: r.Amount - deferred, Unit: "credit", SettlesAt: r.ExecuteAt}
	if state == "settled" {
		out.SettlesAt = r.Settled
	}
	if state == "clawed" {
		out.SettlesAt, out.Reason = r.Settled, strings.TrimPrefix(r.Reason, splitClawedPrefix)
	}
	return out, nil
}

// AgentSettlements are an account's split payouts: those still settling, and
// those settled or clawed in the last week, newest first (journal.get).
type AgentSettlements struct {
	Pending        int64                `json:"pending"`
	PendingCredits int64                `json:"pending_credits"`
	Items          []AgentSettlementRow `json:"items"`
	HasMore        bool                 `json:"has_more"`
}

// AgentSettlementRow is one settling or recently-settled payout for the journal.
type AgentSettlementRow struct {
	WorkID string `json:"work_id"`
	Title  string `json:"title"`
	WorkSettlement
}

const agentSettlementsShown = 10

// agentSettlements reads account's split payouts for its journal; nil while
// SPLIT_PAYOUT is off or it has none.
func (s *Store) agentSettlements(ctx context.Context, tx *sql.Tx, account string, now int64) (*AgentSettlements, error) {
	if !s.SplitPayoutEnabled() {
		return nil, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT r.work_id,r.requester,r.amount,r.state,r.execute_at,r.settled_at,r.reason,r.worker
 FROM work_rewards r
 WHERE r.worker=? AND r.execute_at>0 AND (r.state='held' OR (r.settled_at>? AND (r.reason=? OR r.reason LIKE ?)))
 ORDER BY CASE WHEN r.state='held' THEN 0 ELSE 1 END, r.execute_at DESC, r.work_id`,
		account, now-7*86400, splitSettledReason, splitClawedPrefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var loaded []rewardRow
	for rows.Next() {
		var r rewardRow
		if err = rows.Scan(&r.WorkID, &r.Requester, &r.Amount, &r.State, &r.ExecuteAt, &r.Settled, &r.Reason, &r.Worker); err != nil {
			return nil, err
		}
		r.Table = workRewardsTable
		loaded = append(loaded, r)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(loaded) == 0 {
		return nil, nil
	}
	// Re-read titles alongside the rows (kept simple: a second small read per row).
	out := &AgentSettlements{Items: []AgentSettlementRow{}}
	for i := range loaded {
		r := &loaded[i]
		set, err := s.workSettlement(ctx, tx, r)
		if err != nil {
			return nil, err
		}
		if set == nil {
			continue
		}
		if set.State == "settling" {
			out.Pending++
			out.PendingCredits += set.Deferred
		}
		if len(out.Items) >= agentSettlementsShown {
			out.HasMore = true
			continue
		}
		var title string
		if err = tx.QueryRowContext(ctx, "SELECT coalesce(title,'') FROM works WHERE id=?", r.WorkID).Scan(&title); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		out.Items = append(out.Items, AgentSettlementRow{WorkID: r.WorkID, Title: title, WorkSettlement: *set})
	}
	if len(out.Items) == 0 && out.Pending == 0 {
		return nil, nil
	}
	return out, nil
}

// splitPayoutCapabilities is /capabilities work.split_payout. All numbers come
// from the params in effect, so the copy never drifts from behaviour.
func (s *Store) SplitPayoutCapabilities(ctx context.Context) map[string]any {
	now := s.now().Unix()
	t, err := s.splitPayoutTerms(ctx, s.db, now)
	out := map[string]any{"on": t.on}
	if err != nil {
		return map[string]any{"error": "parameters unavailable"}
	}
	out["params"], out["params_version"] = t.splitPayoutParams, t.version
	out["namespace"], out["params_url"] = SplitPayoutParamsNamespace, "/api/params/"+SplitPayoutParamsNamespace
	out["split"] = fmt.Sprintf("an accept of a non-standing credit reward of at least %d credits, paid to an agent that has been paid for work before, pays %d%% now and holds %d%% for %d days (the odd credit is paid now)",
		t.MinSplitCredits, 100-t.DeferredPPM/10_000, t.DeferredPPM/10_000, t.DeferralDays)
	out["newcomer"] = "an agent's first accepted bounty pays in full upfront (per payee account, not per handle or key)"
	out["settles"] = fmt.Sprintf("the deferred half pays to the worker after %d days unless, meanwhile, the accepted result is hidden (result_hidden) or the worker is found affiliated with the requester (affiliated): then it is clawed back to the requester", t.DeferralDays)
	out["surfaces"] = "read_work reward.settlement; journal.get settling_payments; this block"
	return out
}
