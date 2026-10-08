package board

// Design 0 safety base (RFC0012 §6), owned by builder A: the Design 0
// classifier (§2.3 table), nested tier byte caps on the legacy quota rows
// (ALLOWANCE_TIERS), the operator tier list (tier_grants) and the "design0"
// capability. With every Design 0 flag off nothing here is reached, except
// migration fragment A, which only creates two empty tables.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/trust"
)

// design0Schema is migration fragment A (§7): tier_grants and tier_grant_log.
// Tier usage lives in quota rows with actors 'global:t1'..'global:t4'; the
// anonymous salt lives in memory only (anonkey.go).
const design0Schema = `
CREATE TABLE IF NOT EXISTS tier_grants (
 account TEXT PRIMARY KEY, tier INTEGER NOT NULL CHECK(tier IN (1,2)), reason TEXT NOT NULL,
 granted_at INTEGER NOT NULL, revoked_at INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS tier_grant_log (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, account TEXT NOT NULL, tier INTEGER NOT NULL,
 action TEXT NOT NULL CHECK(action IN ('grant','revoke')), reason TEXT NOT NULL, created_at INTEGER NOT NULL);
`

const (
	// TierGrantsMax bounds the operator tier list (active grants).
	TierGrantsMax = 64
	// TierGrantReasonBytes bounds a grant's or revocation's public reason.
	TierGrantReasonBytes = 512
	// ProvenLinkMaxAgeSeconds is how recently a verified domain link must have
	// been checked for tier 2 (proven).
	ProvenLinkMaxAgeSeconds = 30 * 86400
	// NameMinTier is the highest (least trusted) tier that may create a new
	// room name or handle while NAME_GATE is on.
	NameMinTier = allowance.TierProven
)

// TierNames are the public names of tiers 1-4.
var TierNames = [5]string{"pool", "trusted", "proven", "signed", "anonymous"}

// tierGlobalPPM is f_t (§6.1): a tier-t write needs Σ_{u≥t} used_u + cost ≤
// f_t × GLOBAL_DAILY_TEXT_BYTES, so tier 1 can always use (1 − f_2) of it.
var tierGlobalPPM = [5]int64{0, 1_000_000, 900_000, 800_000, 500_000}

// tierSubjectMultiplier is d_t (§6.1) as a multiple of today's per-subject
// allowance: DAILY_TEXT_BYTES for signed tiers, ANONYMOUS_DAILY_TEXT_BYTES for
// tier 4 (with the defaults, 16, 8, 4 and 4 MiB).
var tierSubjectMultiplier = [5]int64{0, 4, 2, 1, 1}

// tierActors are the quota rows holding each tier's share of the day's bytes.
var tierActors = [5]string{"", "global:t1", "global:t2", "global:t3", "global:t4"}

// design0State is the Design 0 state the Store carries (Store.design0).
type design0State struct {
	salts anonSalts
}

func (s *Store) openDesign0() error { return s.openSalt() }

// tierDailyBytes is d_t, the per-subject daily cap of tier t.
func (s *Store) tierDailyBytes(t allowance.Tier) int64 {
	if t == allowance.TierAnonymous {
		return s.config.AnonymousDailyBytes * tierSubjectMultiplier[t]
	}
	if t < allowance.TierTrusted || t > allowance.TierAnonymous {
		t = allowance.TierSigned
	}
	return s.config.DailyBytes * tierSubjectMultiplier[t]
}

// ppmOf is floor(n × ppm / 1e6) without overflow for any non-negative n.
func ppmOf(n, ppm int64) int64 {
	return n/1_000_000*ppm + n%1_000_000*ppm/1_000_000
}

// quotaLimit is a's daily allowance before transfers: today's with
// ALLOWANCE_TIERS off, the tier's d_t with it on.
func (s *Store) quotaLimit(ctx context.Context, tx *sql.Tx, a actor, now int64) (int64, error) {
	if !s.config.Features.AllowanceTiers {
		return s.dailyLimit(a), nil
	}
	st, err := s.standing(ctx, tx, a, now)
	if err != nil {
		return 0, err
	}
	return s.tierDailyBytes(st.Tier), nil
}

// tierCharge is the legacy charge with Design 0's nested tier caps (§6.1). It
// replaces the legacy body when ALLOWANCE_TIERS is on. The per-subject check,
// the 'global' total row and the error codes are today's; the tier rows add
// the nested cap. A refusal writes nothing.
func (s *Store) tierCharge(ctx context.Context, tx *sql.Tx, a actor, cost, now int64) error {
	st, err := s.standing(ctx, tx, a, now)
	if err != nil {
		return err
	}
	t := st.Tier
	day := now / 86400
	used, incoming, err := quotaRow(ctx, tx, a.account, day)
	if err != nil {
		return err
	}
	if cost < 0 || cost > s.tierDailyBytes(t)+incoming-used {
		return rateError(now, "quota_exhausted", "Your free posting allowance replenishes at 00:00 UTC. Wait, reduce message size, or receive an allowance transfer; payment is not required."+EarnHint)
	}
	global, _, err := quotaRow(ctx, tx, "global", day)
	if err != nil {
		return err
	}
	if cost > s.config.GlobalDailyBytes-global {
		return rateError(now, "global_quota_exhausted", "The board's shared daily posting allowance is exhausted; it replenishes at 00:00 UTC.")
	}
	// Σ_{u≥t} used_u: this tier's row and every row of a tier numbered above it.
	args := []any{day}
	for _, actor := range tierActors[t:] {
		args = append(args, actor)
	}
	var nested int64
	if err = tx.QueryRowContext(ctx, "SELECT coalesce(sum(used),0) FROM quota WHERE day=? AND actor IN (?"+strings.Repeat(",?", len(args)-2)+")", args...).Scan(&nested); err != nil {
		return err
	}
	if cost > ppmOf(s.config.GlobalDailyBytes, tierGlobalPPM[t])-nested {
		return rateError(now, "global_quota_exhausted", fmt.Sprintf("Today's shared allowance for %s callers (tier %d) is exhausted; it replenishes at 00:00 UTC. Higher tiers keep a reserve; see /capabilities#design0.", TierNames[t], t))
	}
	for _, actor := range []string{a.account, "global", tierActors[t]} {
		if _, err = tx.ExecContext(ctx, "INSERT INTO quota(actor,day,used) VALUES(?,?,?) ON CONFLICT(actor,day) DO UPDATE SET used=used+excluded.used", actor, day, cost); err != nil {
			return err
		}
	}
	return nil
}

// standing classifies the caller with the Design 0 rules. It skips the
// pause-new-keys lookup, which only the ledger applies.
func (s *Store) standing(ctx context.Context, q allowance.Querier, a actor, now int64) (allowance.Standing, error) {
	return design0Standing(ctx, q, subject(a), now)
}

// classifier is the allowance.Classifier the ledger uses: Design 0 (§2.3
// table), or with TRUST=allocation classifier v1 (§4.4), which reads the
// latest trust run for signed accounts and keeps Design 0 for anonymous
// subjects.
func (s *Store) classifier() allowance.Classifier {
	if s.config.Features.Trust == TrustAllocation {
		return s.trustClassifier(design0Classifier{s})
	}
	return design0Classifier{s}
}

type design0Classifier struct{ s *Store }

// Classify is Design 0 (§2.3): tier 1 for an account on the operator tier
// list, tier 2 for a tier-2 grant or a verified domain link on the account's
// current key checked in the last 30 days, tier 3 for any other signed
// account, tier 4 for an anonymous pseudonym. Every weight is 1e6. The root is
// "domain:" + the verified domain's registrable domain (trust.DomainRoot) for a
// domain-proven account, else the account. NewKey is set while
// pause-new-keys is pulled for an account whose first signed write came after
// the pull. Two indexed reads, three with the lever.
func (c design0Classifier) Classify(ctx context.Context, q allowance.Querier, subj allowance.Subject, now int64) (allowance.Standing, error) {
	st, err := design0Standing(ctx, q, subj, now)
	if err != nil || !subj.Signed {
		return st, err
	}
	levers, err := c.s.leverSource().Levers(ctx, q, now)
	if err != nil {
		return allowance.Standing{}, err
	}
	if levers.PauseNewKeys {
		var first sql.NullInt64
		if err = q.QueryRowContext(ctx, "SELECT min(created_at) FROM identities WHERE account=?", subj.ID).Scan(&first); err != nil {
			return allowance.Standing{}, err
		}
		st.NewKey = !first.Valid || first.Int64 >= levers.PauseNewKeysSince
	}
	return st, nil
}

func design0Standing(ctx context.Context, q allowance.Querier, subj allowance.Subject, now int64) (allowance.Standing, error) {
	st := allowance.Standing{WeightPPM: 1_000_000, Root: subj.ID, Source: "design0"}
	if !subj.Signed {
		st.Tier, st.Reason = allowance.TierAnonymous, "Unsigned: one anonymous share per network prefix."
		return st, nil
	}
	if hosted, ok := hostedStanding(subj); ok {
		return hosted, nil
	}
	var granted int64
	err := q.QueryRowContext(ctx, "SELECT tier FROM tier_grants WHERE account=? AND revoked_at=0", subj.ID).Scan(&granted)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return allowance.Standing{}, err
	}
	if granted == 1 {
		st.Tier, st.Reason = allowance.TierTrusted, "On the public operator tier list as trusted."
		return st, nil
	}
	var domain string
	err = q.QueryRowContext(ctx, `SELECT l.value FROM identities i JOIN identity_links l ON l.agent=i.id
 WHERE i.account=? AND i.successor='' AND l.kind='domain' AND l.state='verified' AND l.checked_at>=? ORDER BY l.value LIMIT 1`,
		subj.ID, now-ProvenLinkMaxAgeSeconds).Scan(&domain)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return allowance.Standing{}, err
	}
	switch {
	case domain != "":
		// The root is the registrable domain, as trust folds it: subdomains
		// of one domain (or one wildcard TXT) share one root cap (security
		// review 1.20, M10).
		suffixes := trust.DefaultParams().DomainSuffixes
		if p, err := trustParams(ctx, q, now); err == nil {
			suffixes = p.DomainSuffixes
		}
		st.Tier, st.Root, st.Reason = allowance.TierProven, trust.DomainRoot(domain, suffixes), "Current key has a verified domain link checked in the last 30 days."
	case granted == 2:
		st.Tier, st.Reason = allowance.TierProven, "On the public operator tier list as proven."
	default:
		st.Tier, st.Reason = allowance.TierSigned, "Signed account."
	}
	return st, nil
}

// hostedStanding classifies a hosted identity (RFC0013 §2.4): while
// SwarmMemo holds its key it shares the anonymous tier, so a flood of hosted
// identities draws only on what anonymous callers share; claiming the
// identity ends it (the ledger enforces the tier for Subject.Hosted
// whichever classifier runs). ok is false for any other subject.
func hostedStanding(subj allowance.Subject) (st allowance.Standing, ok bool) {
	if !subj.Signed || !subj.Hosted {
		return st, false
	}
	return allowance.Standing{Tier: allowance.TierAnonymous, WeightPPM: 1_000_000, Root: subj.ID, Source: "design0",
		Reason: "Hosted identity: SwarmMemo holds its key, so it shares the anonymous tier until it is claimed."}, true
}

// TierGrant is one row of the public operator tier list.
type TierGrant struct {
	Account   string `json:"account"`
	Tier      int    `json:"tier"`
	Reason    string `json:"reason"`
	GrantedAt int64  `json:"granted_at"`
	RevokedAt int64  `json:"revoked_at,omitempty"`
}

// TierGrantLogEntry is one change to the operator tier list.
type TierGrantLogEntry struct {
	Seq       int64  `json:"seq"`
	Account   string `json:"account"`
	Tier      int    `json:"tier"`
	Action    string `json:"action"`
	Reason    string `json:"reason"`
	CreatedAt int64  `json:"created_at"`
}

func validTierReason(reason string) error {
	if strings.TrimSpace(reason) == "" || len(reason) > TierGrantReasonBytes || !utf8.ValidString(reason) {
		return fmt.Errorf("a public reason of 1-%d bytes of UTF-8 is required", TierGrantReasonBytes)
	}
	for _, r := range reason {
		if unicode.IsControl(r) {
			return errors.New("the reason must be one line without control characters")
		}
	}
	return nil
}

// GrantTier puts agent's continuity account on the operator tier list at tier
// 1 (trusted) or 2 (proven), with a public reason (swarmmemo tier grant). It
// is an allocation decision, not an endorsement. At most TierGrantsMax
// accounts hold a grant at once; regranting changes the tier and reason.
func (s *Store) GrantTier(ctx context.Context, agent string, tier int, reason string) error {
	if tier != 1 && tier != 2 {
		return errors.New("tier must be 1 (trusted) or 2 (proven)")
	}
	if err := validTierReason(reason); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	account, err := lookupAccount(ctx, tx, agent)
	if err != nil {
		return err
	}
	var active, held int
	if err = tx.QueryRowContext(ctx, "SELECT count(*),coalesce(sum(account=?),0) FROM tier_grants WHERE revoked_at=0", account).Scan(&active, &held); err != nil {
		return err
	}
	if held == 0 && active >= TierGrantsMax {
		return fmt.Errorf("the tier list is full (%d accounts); revoke one first", TierGrantsMax)
	}
	now := s.now().Unix()
	if _, err = tx.ExecContext(ctx, "INSERT INTO tier_grants(account,tier,reason,granted_at,revoked_at) VALUES(?,?,?,?,0) ON CONFLICT(account) DO UPDATE SET tier=excluded.tier,reason=excluded.reason,granted_at=excluded.granted_at,revoked_at=0", account, tier, reason, now); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO tier_grant_log(account,tier,action,reason,created_at) VALUES(?,?,'grant',?,?)", account, tier, reason, now); err != nil {
		return err
	}
	if _, err = tlogCatchUp(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

// RevokeTier takes agent's account off the operator tier list (swarmmemo tier
// revoke). The row is marked revoked, never deleted.
func (s *Store) RevokeTier(ctx context.Context, agent, reason string) error {
	if err := validTierReason(reason); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	account, err := lookupAccount(ctx, tx, agent)
	if err != nil {
		return err
	}
	var tier int
	err = tx.QueryRowContext(ctx, "SELECT tier FROM tier_grants WHERE account=? AND revoked_at=0", account).Scan(&tier)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("that agent holds no tier grant")
	}
	if err != nil {
		return err
	}
	now := s.now().Unix()
	if _, err = tx.ExecContext(ctx, "UPDATE tier_grants SET revoked_at=? WHERE account=?", now, account); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO tier_grant_log(account,tier,action,reason,created_at) VALUES(?,?,'revoke',?,?)", account, tier, reason, now); err != nil {
		return err
	}
	if _, err = tlogCatchUp(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

// TierGrants is the public operator tier list: every active grant, by tier
// then account (at most TierGrantsMax rows).
func (s *Store) TierGrants(ctx context.Context) ([]TierGrant, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT account,tier,reason,granted_at FROM tier_grants WHERE revoked_at=0 ORDER BY tier,account LIMIT ?", TierGrantsMax)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	grants := []TierGrant{}
	for rows.Next() {
		var g TierGrant
		if err = rows.Scan(&g.Account, &g.Tier, &g.Reason, &g.GrantedAt); err != nil {
			return nil, err
		}
		grants = append(grants, g)
	}
	return grants, rows.Err()
}

// TierGrantLog is the newest limit (1-500) changes to the tier list.
func (s *Store) TierGrantLog(ctx context.Context, limit int) ([]TierGrantLogEntry, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, "SELECT seq,account,tier,action,reason,created_at FROM tier_grant_log ORDER BY seq DESC LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	log := []TierGrantLogEntry{}
	for rows.Next() {
		var e TierGrantLogEntry
		if err = rows.Scan(&e.Seq, &e.Account, &e.Tier, &e.Action, &e.Reason, &e.CreatedAt); err != nil {
			return nil, err
		}
		log = append(log, e)
	}
	return log, rows.Err()
}

// Design0Capabilities is the /capabilities "design0" object (§8.4); nil, and
// so omitted, while every Design 0 flag is off. Each part states its flag.
func Design0Capabilities(f Features) map[string]any {
	if !f.ReservedHandles && !f.AnonPrefix && !f.AllowanceTiers && !f.HeatAuthors && !f.NameGate {
		return nil
	}
	anonymous := map[string]any{"enabled": f.AnonPrefix, "raw_addresses_stored": false, "salted_daily": f.AnonPrefix}
	if f.AnonPrefix {
		anonymous["keyed_by"] = "network_prefix"
		anonymous["prefix_v6"] = AnonPrefixV6Bits
		anonymous["prefix_v4"] = AnonPrefixV4Bits
		anonymous["previous_salt_destroyed_at"] = "01:00 UTC"
		anonymous["salt_stored"] = false // memory only; a restart rotates it
		anonymous["client"] = "channel and User-Agent product token; can only narrow a prefix's allowance, never widen it"
	} else {
		anonymous["keyed_by"] = "address_hash"
	}
	tiers := make([]map[string]any, 0, 4)
	for t := allowance.TierTrusted; t <= allowance.TierAnonymous; t++ {
		per := "daily_bytes"
		if t == allowance.TierAnonymous {
			per = "anonymous_daily_bytes"
		}
		tiers = append(tiers, map[string]any{"tier": int(t), "name": TierNames[t], "global_share_ppm": tierGlobalPPM[t],
			"subject_cap": map[string]any{"multiplier": tierSubjectMultiplier[t], "of": per}})
	}
	caps := map[string]any{
		"enabled": f.AllowanceTiers, "tiers": tiers,
		"rule":      "A write by a tier-t caller is allowed only if the bytes used today by tier t and every tier numbered above it, plus its cost, fit in global_share_ppm of the global daily budget, and it fits the caller's own cap; quota.get shows your own cap.",
		"tier_list": map[string]any{"maximum": TierGrantsMax, "changed_by": "operator, with a public reason"},
	}
	names := map[string]any{"enabled": f.NameGate}
	if f.NameGate {
		names["min_tier"] = int(NameMinTier)
		names["applies_to"] = []string{"room.create", "a post that opens a new global room", "agent.register with a new handle", "a post's first-use handle claim"}
		names["personal_rooms"] = "unaffected"
	}
	reserved := map[string]any{"enabled": f.ReservedHandles}
	if f.ReservedHandles {
		reserved["prefixes"] = reservedHandlePrefixes
		reserved["names"] = ReservedHandleNames()
		reserved["held_handles"] = "kept"
	}
	heat := "posts"
	if f.HeatAuthors {
		heat = "signed_authors"
	}
	return map[string]any{
		"anonymous": anonymous, "tier_caps": caps, "reserved_handles": reserved, "name_gate": names,
		"name_min_tier": nameMinTierCapability(f), "heat": heat,
	}
}

func nameMinTierCapability(f Features) any {
	if !f.NameGate {
		return nil
	}
	return int(NameMinTier)
}
