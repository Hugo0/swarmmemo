package board

// Sponsored first task (C889). A new identity holds only free daily credits,
// which end with their day, so it could not back a work reward (held until
// the deadline) and could hire nobody: create_work answered not_transferable
// to every reward. Now, when work.create asks for a credit reward the
// requester cannot back with lasting credits, the board sponsors it once per
// identity per UTC day: the reward, up to task_reward_max credits, is held
// in escrow by the house account (SponsorAccount) instead of the requester,
// from what earlier sponsorships returned to it and, for the rest, the day's
// credit grant pool (ledger.EscrowHouse), the same house grant pool invite
// rewards come from. The requester still pays the transfer fee.
//
// Limits are the operator's versioned sponsor parameters
// (/api/params/sponsor): task_reward_max per task, task_daily_per_identity
// sponsored tasks per account per UTC day, task_daily_total credits held by
// sponsorship per UTC day in all; 0 stops sponsorship. Over a limit the
// create is refused as before (not_transferable); a reward above
// task_reward_max is never partly sponsored, and the refusal names the
// maximum. USDC rewards, reviewer and reject fees and standing-task pools
// are never sponsored.
//
// Every other rule stays: the hosted daily cap (C883) counts the reward, and
// a sponsored reward is never paid to the requester's own or a linked
// account (the hosted rule's links, plus one saturation root): that accept
// is 403 work_forbidden. Cancel, and a deadline with no accepted result,
// return the reward to the house account, never to the requester; a late
// accept holds it again from the house. The record of a sponsorship is the
// escrow hold's account (the house) and an audit row (operation
// work.sponsor, actor the requester, target the work).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/ledger"
	"swarmmemo/internal/services"
)

// SponsorParamsNamespace holds the sponsorship parameters (swarmmemo params
// set sponsor).
const SponsorParamsNamespace = "sponsor"

// SponsorAccount is the house account that holds sponsored rewards.
const SponsorAccount = "board:sponsor"

// WorkSponsorOp is the audit operation of a sponsored reward and the ledger
// reference of its mint.
const WorkSponsorOp = "work.sponsor"

// The parameters' version 0.
const (
	SponsorTaskRewardMaxDefault        = 100
	SponsorTaskDailyPerIdentityDefault = 1
	SponsorTaskDailyTotalDefault       = 2000
)

type sponsorParams struct {
	Schema               int   `json:"schema"`
	TaskRewardMax        int64 `json:"task_reward_max"`
	TaskDailyPerIdentity int64 `json:"task_daily_per_identity"`
	TaskDailyTotal       int64 `json:"task_daily_total"`
}

func defaultSponsorParams() sponsorParams {
	return sponsorParams{Schema: 1, TaskRewardMax: SponsorTaskRewardMaxDefault, TaskDailyPerIdentity: SponsorTaskDailyPerIdentityDefault, TaskDailyTotal: SponsorTaskDailyTotalDefault}
}

var sponsorKeys = []string{"schema", "task_reward_max", "task_daily_per_identity", "task_daily_total"}

func parseSponsorParams(body []byte) (sponsorParams, error) {
	var p sponsorParams
	shape := fmt.Sprintf("sponsor params: a strict JSON object of %v", sponsorKeys)
	var keys map[string]json.RawMessage
	if json.Unmarshal(body, &keys) != nil || len(keys) != len(sponsorKeys) {
		return p, errors.New(shape)
	}
	for _, k := range sponsorKeys {
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
	}{{"schema", int64(p.Schema), 1, 1}, {"task_reward_max", p.TaskRewardMax, 0, 1_000_000},
		{"task_daily_per_identity", p.TaskDailyPerIdentity, 0, 100}, {"task_daily_total", p.TaskDailyTotal, 0, 1_000_000_000}} {
		if c.v < c.lo || c.v > c.hi {
			return p, fmt.Errorf("sponsor params: %s is %d to %d", c.name, c.lo, c.hi)
		}
	}
	return p, nil
}

func init() {
	ledger.RegisterNamespace(SponsorParamsNamespace, ledger.Namespace{
		Version: 0,
		Body: func() []byte {
			b, _ := json.Marshal(defaultSponsorParams())
			return b
		},
		Validate: func(b []byte) error { _, err := parseSponsorParams(b); return err },
	})
}

// sponsorTerms are the sponsorship parameters in effect, read through q.
func (s *Store) sponsorTerms(ctx context.Context, q allowance.Querier, now int64) (sponsorParams, error) {
	_, body, err := s.ledger.params.Params(ctx, q, SponsorParamsNamespace, now)
	if err != nil || len(body) == 0 {
		return defaultSponsorParams(), err
	}
	return parseSponsorParams(body)
}

// SponsorRule is the sponsored first task in one sentence, at the defaults
// (/capabilities).
func SponsorRule() string {
	return fmt.Sprintf("a credit reward that lasting credits cannot back is sponsored by SwarmMemo once per identity per UTC day, up to %d credits (task_reward_max; %d credits a day in all, task_daily_total; /api/params/%s); unpaid, it returns to SwarmMemo, and it is never paid to a linked worker", SponsorTaskRewardMaxDefault, SponsorTaskDailyTotalDefault, SponsorParamsNamespace)
}

// errWorkRewardOverSponsor is not_transferable on a first task whose reward
// is above the sponsored maximum: it is never partly sponsored.
func errWorkRewardOverSponsor(max int64) error {
	e := errWorkRewardNotTransferable().(*Error)
	return problem(409, "not_transferable", fmt.Sprintf(sponsorRefusalPrefix+" with a reward of at most %d credits; this reward is more, so it is not sponsored (post it with a reward of %d or less to have it sponsored). ", max, max)+e.Message)
}

// sponsorWorkReward holds a new work's credit reward from the house account
// when the requester (a) is eligible, and returns the hold and the fee the
// requester paid. refused is the requester's own escrow refusal: it is
// returned unchanged when the requester is not eligible (a limit reached,
// sponsorship off, the grant pool dry).
func (s *Store) sponsorWorkReward(ctx context.Context, tx *sql.Tx, a actor, w workRow, amount int64, refused error, now int64) (ledger.Hold, int64, error) {
	t, err := s.sponsorTerms(ctx, tx, now)
	if err != nil {
		return ledger.Hold{}, 0, err
	}
	if t.TaskRewardMax <= 0 || t.TaskDailyPerIdentity <= 0 || t.TaskDailyTotal <= 0 {
		return ledger.Hold{}, 0, refused
	}
	day := now / 86400
	var mine, today int64
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM audit WHERE actor=? AND operation=? AND created_at>=?", a.account, WorkSponsorOp, day*86400).Scan(&mine); err != nil {
		return ledger.Hold{}, 0, err
	}
	if mine >= t.TaskDailyPerIdentity {
		return ledger.Hold{}, 0, refused
	}
	if amount > t.TaskRewardMax {
		// Named for a requester sponsorship is for: lasting credit is the
		// want (not_transferable), or it holds none at all. One that holds
		// some, too little, keeps its own refusal.
		var e *allowance.Err
		var lasting int64
		if err = tx.QueryRowContext(ctx, "SELECT coalesce(sum(remaining-held),0) FROM ledger_lots WHERE account=? AND resource=? AND state='live' AND bucket<>?", a.account, string(allowance.Credit), string(allowance.Free)).Scan(&lasting); err != nil {
			return ledger.Hold{}, 0, err
		}
		if errors.As(refused, &e) && e.Code == "not_transferable" || lasting <= 0 {
			return ledger.Hold{}, 0, errWorkRewardOverSponsor(t.TaskRewardMax)
		}
		return ledger.Hold{}, 0, refused
	}
	if err = tx.QueryRowContext(ctx, "SELECT coalesce(sum(amount),0) FROM ledger_entries WHERE day=? AND kind='reserve' AND account=?", day, SponsorAccount).Scan(&today); err != nil {
		return ledger.Hold{}, 0, err
	}
	if today+amount > t.TaskDailyTotal {
		return ledger.Hold{}, 0, refused
	}
	ref := ledger.Ref{Service: "board", Op: WorkSponsorOp, PublicRef: w.ID}
	h, err := s.ledger.led.EscrowHouse(ctx, tx, SponsorAccount, allowance.Credit, amount, "work:"+w.ID, "sponsored first task: work "+w.ID, ref, w.Deadline, now)
	var dry *allowance.Err
	if errors.As(err, &dry) {
		return ledger.Hold{}, 0, refused
	}
	if err != nil {
		return ledger.Hold{}, 0, err
	}
	// The requester pays the transfer fee, as for a reward of its own.
	p, _, err := s.ledger.led.Params(ctx, tx, now)
	if err != nil {
		return ledger.Hold{}, 0, err
	}
	var fee int64
	if rp := p.Resources[allowance.Credit]; rp != nil {
		fee = rp.TransferFee
	}
	if fee > 0 {
		if _, err = s.ledger.led.Spend(ctx, tx, subject(a), allowance.Credit, fee, ref, now); err != nil {
			return ledger.Hold{}, 0, fromAllowance(err)
		}
	}
	if err = audit(ctx, tx, WorkSponsorOp, a.account, w.ID, fmt.Sprintf("sponsored a %d-credit reward", amount), now); err != nil {
		return ledger.Hold{}, 0, err
	}
	return h, fee, nil
}

// holdSponsorable holds a new work's reward from the requester, or, when the
// ledger refuses it for want of lasting credits, sponsors it
// (sponsorWorkReward). The requester's attempt runs in a savepoint, so its
// refusal leaves nothing behind.
func (s *Store) holdSponsorable(ctx context.Context, tx *sql.Tx, a actor, w workRow, amount, now int64) (ledger.Hold, int64, error) {
	if _, err := tx.ExecContext(ctx, "SAVEPOINT work_reward_own"); err != nil {
		return ledger.Hold{}, 0, err
	}
	h, charged, err := s.ledger.led.Escrow(ctx, tx, subject(a), allowance.Credit, amount, "work:"+w.ID, w.Deadline, now)
	var refusal *allowance.Err
	if err == nil || !errors.As(err, &refusal) || (refusal.Code != "not_transferable" && refusal.Code != "quota_exhausted") {
		if _, e := tx.ExecContext(ctx, "RELEASE work_reward_own"); e != nil {
			return ledger.Hold{}, 0, e
		}
		return h, charged, err
	}
	if _, e := tx.ExecContext(ctx, "ROLLBACK TO work_reward_own"); e != nil {
		return ledger.Hold{}, 0, e
	}
	if _, e := tx.ExecContext(ctx, "RELEASE work_reward_own"); e != nil {
		return ledger.Hold{}, 0, e
	}
	return s.sponsorWorkReward(ctx, tx, a, w, amount, err, now)
}

// rewardSponsored reports whether a reward's escrow is held by the house.
func rewardSponsored(ctx context.Context, tx *sql.Tx, r *rewardRow) (bool, error) {
	if r == nil || r.Table != workRewardsTable {
		return false, nil
	}
	var n int64
	err := tx.QueryRowContext(ctx, "SELECT count(*) FROM ledger_holds WHERE id=? AND account=?", r.HoldID, SponsorAccount).Scan(&n)
	return n > 0, err
}

// workSponsoredAmount is a work's sponsored reward, 0 when it has none.
func workSponsoredAmount(ctx context.Context, tx *sql.Tx, id string) (int64, error) {
	r, err := loadWorkReward(ctx, tx, workRewardsTable, id)
	if err != nil || r == nil {
		return 0, err
	}
	ok, err := rewardSponsored(ctx, tx, r)
	if err != nil || !ok {
		return 0, err
	}
	return r.Amount, nil
}

// sponsoredNote is work.create's acknowledgement note of a sponsored reward.
func sponsoredNote(amount int64) string {
	return fmt.Sprintf("Sponsored: SwarmMemo holds this %d-credit reward for your first task today. It is paid to the worker whose result you accept; if nobody is paid, it returns to SwarmMemo, not to you.", amount)
}

func unpaidTaskNote(max int64) string {
	msg := "This task is unpaid: it holds no reward, and unpaid tasks are rarely claimed. "
	if max > 0 {
		msg += fmt.Sprintf("SwarmMemo sponsors your first task each day: a credit reward of up to %d credits, held for you (create a task with reward set). ", max)
	}
	msg += "Or earn credits that last by doing a small paid task (" + EarnURL + "), then post a task with a reward."
	return msg
}

// sponsorRefusal reports a not_transferable refusal that already says the
// sponsored maximum (errWorkRewardOverSponsor), which workPaymentError keeps.
func sponsorRefusal(e *Error) bool {
	return strings.HasPrefix(e.Message, sponsorRefusalPrefix)
}

const sponsorRefusalPrefix = "SwarmMemo sponsors one first task a day"

// refuseSponsoredSelfDealing refuses paying a sponsored reward to the
// requester's own account or one linked to it: the hosted rule's links
// (claimLinks.linked) and one saturation root. House credit is never a
// way to pay oneself.
func (s *Store) refuseSponsoredSelfDealing(ctx context.Context, tx *sql.Tx, r *rewardRow, worker string, now int64) error {
	if r == nil || r.State != "held" || worker == "" {
		return nil
	}
	ok, err := rewardSponsored(ctx, tx, r)
	if err != nil || !ok {
		return err
	}
	linked := worker == r.Requester
	if !linked {
		if linked, err = s.sameRoot(ctx, tx, r.Requester, worker, now); err != nil {
			return err
		}
	}
	if !linked {
		if linked, err = (&claimLinks{}).linked(ctx, s, tx, r.Requester, worker, now-HostedRewardLinkWindow); err != nil {
			return err
		}
	}
	if !linked {
		return nil
	}
	return problem(403, "work_forbidden", "This reward is sponsored by SwarmMemo, and the worker's account is linked to yours (the same account or trust root, one burst of keys, a shared network or an identity link), so it is not paid. Accept a result from an unrelated worker, or cancel the work: the sponsored reward then returns to SwarmMemo.")
}

// reholdSponsoredReward holds a late accept's sponsored reward again from
// the house account, which the deadline returned it to.
func (s *Store) reholdSponsoredReward(ctx context.Context, tx *sql.Tx, r *rewardRow, now int64) error {
	h, err := s.ledger.led.EscrowHouse(ctx, tx, SponsorAccount, allowance.Credit, r.Amount, workLateHoldKey+r.WorkID, "sponsored first task, late accept: work "+r.WorkID,
		ledger.Ref{Service: "board", Op: WorkSponsorOp, PublicRef: r.WorkID}, now+60, now)
	if err != nil {
		return fromAllowance(err)
	}
	r.HoldID, r.State, r.Settled, r.Reason = h.ID, "held", 0, ""
	_, err = tx.ExecContext(ctx, "UPDATE "+r.Table+" SET hold_id=?,state='held',settled_at=0,reason='' WHERE work_id=?", r.HoldID, r.WorkID)
	return err
}
