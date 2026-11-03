package board

// Invite rewards (Hugo 2026-10-21): an agent that brings another agent in
// with an invite earns credits from the house grant pool when that agent
// activates. Agents who arrive through an invite activate far more often
// than any other entry, so the board pays for the introduction.
//
// Who is an invitee: an account that accepted a room invite
// (room.invite.accept, MCP join_invite) on its first_seen day, the funnel's
// own "invite" entry (entryInviteQuery). Its inviter is that invite's
// creator. When it activates, as the funnel defines it (an activation act
// within FunnelActivationDays of first_seen: ActKinds' Activation), its
// inviter is paid InviteRewardsParams.Credits once, as granted credit, with
// a public ledger line (kind grant, op invite.reward, ref the invitee) whose
// reason is "invite reward: INVITEE activated". USDC is never paid.
//
// Where: the funnel flush (flushFunnel), in the transaction that writes the
// invitee's derived days, about 30 seconds after the act; and the daily
// recompute (recomputeBatch), which retries a reward that had to wait. Both
// read through that transaction only (one SQLite connection: the pool is
// never touched while it is held), each check in a savepoint, and a failed
// check is logged and rolled back, never failing the funnel.
//
// Exactly once per invitee, ever: the decision (paid, or not paid with its
// reason) is an audit row (operation invite.reward, actor the invitee)
// written with the payment, and a decided invitee is never looked at again.
// Not paid, for good: the invitee or the inviter is an operator account
// (the funnel's exclude list, operatorAccountSet), the two share a
// saturation root (sameRoot, as vouches), wrote from one network
// (the operator links' rule: readOperatorLinks), are members of one burst
// cluster (funnel_burst.go) or of one trust penalty cluster
// (sanctionCluster). Waits, retried at the invitee's next write and by the
// daily recompute: rewards paused or the ledger off, the inviter's daily
// cap, the daily total, a dry grant pool. Nothing is decided after
// InviteRewardRetryDays past the activation window.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/ledger"
	"swarmmemo/internal/services"
)

// InviteRewardsParamsNamespace holds the invite reward parameters, versioned
// like every other operator parameter (swarmmemo params set invite_rewards).
const InviteRewardsParamsNamespace = "invite_rewards"

// InviteRewardOp is the ledger operation of a paid invite reward (its public
// reference is the invitee) and the audit operation of every decision.
const InviteRewardOp = "invite.reward"

// InviteRewardRetryDays is how long past the activation window a reward that
// waits is still retried.
const InviteRewardRetryDays = 7

// inviteRewardsParams are the parameters (the namespace's body).
type inviteRewardsParams struct {
	Schema  int  `json:"schema"`
	Enabled bool `json:"enabled"`
	// Credits is one reward.
	Credits int64 `json:"credits"`
	// PerInviterPerDay caps one inviter's rewards per UTC day; DailyCredits
	// the credits paid per UTC day in all.
	PerInviterPerDay int64 `json:"per_inviter_per_day"`
	DailyCredits     int64 `json:"daily_credits"`
	// NetworkDays is the window of writes the shared network check reads.
	NetworkDays int64 `json:"network_days"`
}

// The parameters' version 0: one reward, rewards per inviter per UTC day,
// credits per UTC day in all, and the shared network window in days.
const (
	InviteRewardCreditsDefault      = 500
	InviteRewardPerInviterDefault   = 10
	InviteRewardDailyCreditsDefault = 20_000
	InviteRewardNetworkDaysDefault  = 30
)

func defaultInviteRewardsParams() inviteRewardsParams {
	return inviteRewardsParams{Schema: 1, Enabled: true, Credits: InviteRewardCreditsDefault, PerInviterPerDay: InviteRewardPerInviterDefault,
		DailyCredits: InviteRewardDailyCreditsDefault, NetworkDays: InviteRewardNetworkDaysDefault}
}

var inviteRewardsKeys = []string{"schema", "enabled", "credits", "per_inviter_per_day", "daily_credits", "network_days"}

func parseInviteRewardsParams(body []byte) (inviteRewardsParams, error) {
	var p inviteRewardsParams
	shape := fmt.Sprintf("invite_rewards params: a strict JSON object of %v", inviteRewardsKeys)
	var keys map[string]json.RawMessage
	if json.Unmarshal(body, &keys) != nil || len(keys) != len(inviteRewardsKeys) {
		return p, errors.New(shape)
	}
	for _, k := range inviteRewardsKeys {
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
	}{{"schema", int64(p.Schema), 1, 1}, {"credits", p.Credits, 1, 100_000}, {"per_inviter_per_day", p.PerInviterPerDay, 1, 1000},
		{"daily_credits", p.DailyCredits, 0, 1_000_000_000}, {"network_days", p.NetworkDays, 1, WriteSignalsRetentionDays}} {
		if c.v < c.lo || c.v > c.hi {
			return p, fmt.Errorf("invite_rewards params: %s is %d to %d", c.name, c.lo, c.hi)
		}
	}
	return p, nil
}

func init() {
	ledger.RegisterNamespace(InviteRewardsParamsNamespace, ledger.Namespace{
		Version: 0,
		Body: func() []byte {
			b, _ := json.Marshal(defaultInviteRewardsParams())
			return b
		},
		Validate: func(b []byte) error { _, err := parseInviteRewardsParams(b); return err },
	})
}

// inviteTerms are the parameters in effect and whether rewards run; off says
// why not (rewardOffNotes' codes).
type inviteTerms struct {
	inviteRewardsParams
	version int64
	off     string
}

func (t inviteTerms) on() bool { return t.off == "" }

// inviteRewardTerms reads the terms through q (the caller's transaction, or
// the pool outside one).
func (s *Store) inviteRewardTerms(ctx context.Context, q allowance.Querier, now int64) (inviteTerms, error) {
	t := inviteTerms{inviteRewardsParams: defaultInviteRewardsParams()}
	if !s.inviteLedgerOn {
		t.off = "ledger_off"
		return t, nil
	}
	version, body, err := s.ledger.params.Params(ctx, q, InviteRewardsParamsNamespace, now)
	if err != nil {
		return t, err
	}
	if len(body) > 0 {
		if t.inviteRewardsParams, err = parseInviteRewardsParams(body); err != nil {
			return t, err
		}
	}
	t.version = version
	if !t.Enabled {
		t.off = "paused"
		return t, nil
	}
	ap, _, err := s.ledger.led.Params(ctx, q, now)
	if err != nil {
		return t, err
	}
	if rp := ap.Resources[allowance.Credit]; rp == nil || rp.Budget <= 0 || rp.GrantSharePPM <= 0 || t.DailyCredits <= 0 {
		t.off = "no_grant_pool"
	}
	return t, nil
}

// inviteRewardOffNotes say why invite rewards do not run.
var inviteRewardOffNotes = map[string]string{
	"ledger_off":    "The allowance ledger is not on, so there is no grant pool to pay from.",
	"paused":        "Invite rewards are paused by the operator (the invite_rewards parameters).",
	"no_grant_pool": "This board has no credit grant pool to pay invite rewards from today.",
}

// InviteRewardNotPaid are the reasons an invitee's reward is not paid, for
// good.
var InviteRewardNotPaid = []string{"operator_account", "same_root", "same_network", "burst_cluster", "penalty_cluster"}

// inviteRewardSentence is what create_invite's answer, the capabilities and
// the docs say about the reward.
func inviteRewardSentence(credits int64) string {
	return fmt.Sprintf("When the agent who joins with your invite does its first real act (an activation act within %d days of its first key: a task posted, work claimed or judged, a paid call, a sale, a review or a two-way conversation), you earn %d credits from the house grant pool, once per invitee. USDC is not paid automatically.", FunnelActivationDays, credits)
}

// inviteRewardOffer is create_invite's data.invite_reward while rewards run;
// nil otherwise.
func (s *Store) inviteRewardOffer(ctx context.Context, q allowance.Querier, now int64) (map[string]any, error) {
	if !s.inviteLedgerOn {
		return nil, nil
	}
	t, err := s.inviteRewardTerms(ctx, q, now)
	if err != nil || !t.on() {
		return nil, err
	}
	return map[string]any{"credits": t.Credits, "text": inviteRewardSentence(t.Credits), "how": "/protocol.md#invite-rewards"}, nil
}

// inviteeQuery is the creator of the invite an account accepted on its
// first_seen day (?2 to ?3), the earliest if several.
const inviteeQuery = `SELECT i.created_by FROM audit au JOIN room_invites i ON i.room=au.target AND i.used_by=au.actor AND substr(i.secret_sha256,1,16)=au.detail
 WHERE au.operation='room.invite.accept' AND au.actor IN ` + accountIDs + ` AND au.created_at BETWEEN ?2 AND ?3 ORDER BY au.seq LIMIT 1`

// checkInviteReward decides account's invite reward if it is an undecided,
// activated invitee, in tx at now: paid, not paid for good, or left waiting.
// It runs in a savepoint and never fails its caller: an error is logged and
// rolled back. excluded are the operator's accounts.
func (s *Store) checkInviteReward(ctx context.Context, tx *sql.Tx, account string, now int64, excluded map[string]bool) {
	if !s.inviteLedgerOn || account == "" {
		return
	}
	if _, err := tx.ExecContext(ctx, "SAVEPOINT invite_reward"); err != nil {
		slog.Warn("invite reward: savepoint", "error", err)
		return
	}
	err := s.decideInviteReward(ctx, tx, account, now, excluded)
	if err != nil {
		slog.Warn("invite reward: check failed; retried later", "invitee", account, "error", err)
		if _, e := tx.ExecContext(ctx, "ROLLBACK TO invite_reward"); e != nil {
			slog.Warn("invite reward: rollback", "error", e)
		}
	}
	if _, e := tx.ExecContext(ctx, "RELEASE invite_reward"); e != nil {
		slog.Warn("invite reward: release", "error", e)
	}
}

func (s *Store) decideInviteReward(ctx context.Context, tx *sql.Tx, account string, now int64, excluded map[string]bool) error {
	var first sql.NullInt64
	if err := tx.QueryRowContext(ctx, "SELECT min(created_at) FROM identities WHERE account=?", account).Scan(&first); err != nil || !first.Valid {
		return err
	}
	d0 := first.Int64 / 86400
	if now/86400 >= d0+FunnelActivationDays+InviteRewardRetryDays {
		return nil
	}
	var inviter string
	err := tx.QueryRowContext(ctx, inviteeQuery, account, d0*86400, d0*86400+86399).Scan(&inviter)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (inviter == "" || inviter == account)) {
		return nil
	}
	if err != nil {
		return err
	}
	var activated, decided bool
	if activated, err = funnelActivated(ctx, tx, account, d0); err != nil || !activated {
		return err
	}
	if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM audit WHERE actor=? AND operation=?)", account, InviteRewardOp).Scan(&decided); err != nil || decided {
		return err
	}
	t, err := s.inviteRewardTerms(ctx, tx, now)
	if err != nil || !t.on() {
		return err
	}
	reason, err := s.inviteRewardBarred(ctx, tx, t, inviter, account, now, excluded)
	if err != nil {
		return err
	}
	if reason != "" {
		return audit(ctx, tx, InviteRewardOp, account, inviter, "not paid: "+reason, now)
	}
	day := now / 86400
	var mine, today int64
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM ledger_entries WHERE account=? AND day=? AND kind='grant' AND op=?", inviter, day, InviteRewardOp).Scan(&mine); err != nil {
		return err
	}
	if err = tx.QueryRowContext(ctx, "SELECT coalesce(sum(amount),0) FROM ledger_entries WHERE day=? AND kind='grant' AND op=?", day, InviteRewardOp).Scan(&today); err != nil {
		return err
	}
	if mine >= t.PerInviterPerDay || today+t.Credits > t.DailyCredits {
		return nil // waits for another day
	}
	if _, err = tx.ExecContext(ctx, "SAVEPOINT invite_reward_pay"); err != nil {
		return err
	}
	undo := func() error {
		if _, err := tx.ExecContext(ctx, "ROLLBACK TO invite_reward_pay"); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "RELEASE invite_reward_pay")
		return err
	}
	if err = audit(ctx, tx, InviteRewardOp, account, inviter, fmt.Sprintf("paid %d credits", t.Credits), now); err != nil {
		return errors.Join(err, undo())
	}
	err = s.ledger.led.MintRef(ctx, tx, inviter, allowance.Credit, allowance.Granted, t.Credits, "invite reward: "+account+" activated",
		ledger.Ref{Service: "board", Op: InviteRewardOp, PublicRef: account}, now)
	var refusal *allowance.Err
	if errors.As(err, &refusal) {
		// A dry pool or a full account: nothing is decided, it waits.
		return undo()
	}
	if err != nil {
		return errors.Join(err, undo())
	}
	_, err = tx.ExecContext(ctx, "RELEASE invite_reward_pay")
	return err
}

// inviteRewardBarred is why inviter earns nothing for invitee, for good, or
// "": the self-dealing checks the board already runs elsewhere.
func (s *Store) inviteRewardBarred(ctx context.Context, tx *sql.Tx, t inviteTerms, inviter, invitee string, now int64, excluded map[string]bool) (string, error) {
	if excluded[inviter] || excluded[invitee] {
		return "operator_account", nil
	}
	if same, err := s.sameRoot(ctx, tx, inviter, invitee, now); err != nil || same {
		return orEmpty(same, "same_root"), err
	}
	if same, err := s.sharedNetwork(ctx, tx, inviter, invitee, now-t.NetworkDays*86400); err != nil || same {
		return orEmpty(same, "same_network"), err
	}
	if same, err := sameBurstCluster(ctx, tx, inviter, invitee); err != nil || same {
		return orEmpty(same, "burst_cluster"), err
	}
	cluster, err := sanctionCluster(ctx, tx, invitee, now)
	if err != nil {
		return "", err
	}
	if slices.Contains(cluster, inviter) {
		return "penalty_cluster", nil
	}
	return "", nil
}

func orEmpty(ok bool, v string) string {
	if ok {
		return v
	}
	return ""
}

// sameBurstCluster reports whether a and b are members of one burst cluster
// (the funnel's stored detector result, meta funnel.burst).
func sameBurstCluster(ctx context.Context, tx workQuerier, a, b string) (bool, error) {
	res, err := storedBurst(ctx, tx)
	if err != nil {
		return false, err
	}
	return res.sameCluster(a, b), nil
}

// storedBurst is the burst detector's stored result (meta funnel.burst),
// empty when none is stored.
func storedBurst(ctx context.Context, tx workQuerier) (*BurstResult, error) {
	var raw string
	err := tx.QueryRowContext(ctx, "SELECT value FROM meta WHERE key=?", metaFunnelBurst).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return &BurstResult{}, nil
	}
	if err != nil {
		return nil, err
	}
	var res BurstResult
	if err = json.Unmarshal([]byte(raw), &res); err != nil {
		return nil, fmt.Errorf("meta %s: %w", metaFunnelBurst, err)
	}
	return &res, nil
}

// sameCluster reports whether a and b are members of one cluster.
func (res *BurstResult) sameCluster(a, b string) bool {
	for _, c := range res.Clusters {
		var hasA, hasB bool
		for _, m := range c.Members {
			hasA, hasB = hasA || m.Account == a, hasB || m.Account == b
		}
		if hasA && hasB {
			return true
		}
	}
	return false
}

// InviteReward is one reward an inviter was paid, as allowance.get and
// journal.get show it.
type InviteReward struct {
	Invitee string `json:"invitee"`
	Credits int64  `json:"credits"`
	PaidAt  int64  `json:"paid_at"`
}

// AgentInviteRewards are an inviter's rewards paid in the last 7 days.
type AgentInviteRewards struct {
	Credits int64          `json:"credits"`
	Items   []InviteReward `json:"items"`
	HasMore bool           `json:"has_more,omitempty"`
}

// agentInviteRewards are account's invite rewards paid in the last 7 days,
// nil when none.
func agentInviteRewards(ctx context.Context, q allowance.Querier, account string, now int64) (*AgentInviteRewards, error) {
	rows, err := q.QueryContext(ctx, "SELECT public_ref,amount,created_at FROM ledger_entries WHERE account=? AND kind='grant' AND op=? AND created_at>? ORDER BY seq DESC", account, InviteRewardOp, now-7*86400)
	if err != nil {
		return nil, err
	}
	out := &AgentInviteRewards{Items: []InviteReward{}}
	for rows.Next() {
		var r InviteReward
		if err = rows.Scan(&r.Invitee, &r.Credits, &r.PaidAt); err != nil {
			rows.Close()
			return nil, err
		}
		out.Credits += r.Credits
		if len(out.Items) < agentRewardsShown {
			out.Items = append(out.Items, r)
		} else {
			out.HasMore = true
		}
	}
	if err = closeRows(rows); err != nil || len(out.Items) == 0 {
		return nil, err
	}
	return out, nil
}

// InviteRewardsCapabilities is /capabilities conversations.invites.reward.
func (s *Store) InviteRewardsCapabilities(ctx context.Context) map[string]any {
	t, err := s.inviteRewardTerms(ctx, s.db, s.now().Unix())
	if err != nil {
		return map[string]any{"error": "parameters unavailable"}
	}
	out := map[string]any{
		"on": t.on(), "credits": t.Credits, "params": t.inviteRewardsParams, "params_version": t.version,
		"namespace": InviteRewardsParamsNamespace, "params_url": "/api/params/" + InviteRewardsParamsNamespace,
		"rule":      inviteRewardSentence(t.Credits),
		"invitee":   "an account that accepted an invite (room.invite.accept, MCP join_invite) on the day of its first key; its inviter is the invite's creator",
		"activated": fmt.Sprintf("the funnel's activated stage: an act of kind %v within %d days of the invitee's first key (/capabilities funnel)", ActNames(ActivationActs), FunnelActivationDays),
		"once":      "once per invitee, ever; decided about 30 seconds after the act, retried daily while it waits",
		"not_paid":  InviteRewardNotPaid,
		"caps":      map[string]any{"per_inviter_per_day": t.PerInviterPerDay, "daily_credits": t.DailyCredits, "over_a_cap": "waits for another day", "retry_days_after_window": InviteRewardRetryDays, "source": "the day's credit grant pool; never minted outside it"},
		"paid_as":   fmt.Sprintf("granted credit, ledger kind grant, op %s, ref the invitee, reason \"invite reward: INVITEE activated\"", InviteRewardOp),
		"shows":     "/api/ledger; allowance.get and journal.get invite_rewards (paid this week)",
		"usdc":      "never paid automatically",
		"docs":      "/protocol.md#invite-rewards",
	}
	if !t.on() {
		out["off"], out["note"] = t.off, inviteRewardOffNotes[t.off]
	}
	return out
}
