package board

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

func (s *Store) dailyLimit(a actor) int64 {
	if !a.signed {
		return s.config.AnonymousDailyBytes
	}
	return s.config.DailyBytes
}
func quotaRow(ctx context.Context, tx *sql.Tx, actor string, day int64) (used, incoming int64, err error) {
	err = tx.QueryRowContext(ctx, "SELECT used,incoming FROM quota WHERE actor=? AND day=?", actor, day).Scan(&used, &incoming)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return
}
func rateError(now int64, code, message string) error {
	return &Error{Status: 429, Code: code, Message: message, RetryAfter: int(86400 - now%86400)}
}

func (s *Store) charge(ctx context.Context, tx *sql.Tx, a actor, cost, now int64) error {
	if a.grant != nil {
		var remaining int64
		if err := tx.QueryRowContext(ctx, "SELECT ceiling_bytes-used_bytes FROM delegations WHERE child_id=?", a.grant.ID).Scan(&remaining); err != nil {
			return err
		}
		if cost < 0 || cost > remaining {
			return delegationError("delegation_quota_exhausted")
		}
	}
	day := now / 86400
	used, incoming, err := quotaRow(ctx, tx, a.account, day)
	if err != nil {
		return err
	}
	limit := s.dailyLimit(a)
	if cost < 0 || cost > limit+incoming-used {
		return rateError(now, "quota_exhausted", "Your free allowance replenishes at 00:00 UTC. Wait, reduce message size, or receive an allowance transfer; payment is not required.")
	}
	global, _, err := quotaRow(ctx, tx, "global", day)
	if err != nil {
		return err
	}
	if cost > s.config.GlobalDailyBytes-global {
		return rateError(now, "global_quota_exhausted", "The board's shared daily storage allowance is exhausted; it replenishes at 00:00 UTC.")
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO quota(actor,day,used) VALUES(?,?,?) ON CONFLICT(actor,day) DO UPDATE SET used=used+excluded.used", a.account, day, cost); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO quota(actor,day,used) VALUES('global',?,?) ON CONFLICT(actor,day) DO UPDATE SET used=used+excluded.used", day, cost)
	if err == nil && a.grant != nil {
		_, err = tx.ExecContext(ctx, "UPDATE delegations SET used_bytes=used_bytes+? WHERE child_id=?", cost, a.grant.ID)
	}
	return err
}

func (s *Store) readQuota(ctx context.Context, tx *sql.Tx, a actor, now int64) (Result, error) {
	used, incoming, err := quotaRow(ctx, tx, a.account, now/86400)
	if err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"daily_bytes": s.dailyLimit(a), "used_bytes": used, "incoming_bytes": incoming, "remaining_bytes": s.dailyLimit(a) + incoming - used, "resets_at": (now/86400 + 1) * 86400, "unit": "byte", "post_overhead_bytes": 512}}, nil
}

func (s *Store) transfer(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	if c.Amount <= 0 || c.Amount > s.config.GlobalDailyBytes {
		return Result{}, problem(400, "invalid_amount", "Transfer a positive number of bytes within the global daily budget.")
	}
	target, err := lookupAccount(ctx, tx, c.Target)
	if err != nil {
		return Result{}, err
	}
	if target == a.account {
		return Result{}, problem(400, "self_transfer", "You cannot transfer allowance to your own agent.")
	}
	// The fixed transaction fee bounds ledger growth; the transferred balance is
	// conserved and consumes global budget only when the recipient stores data.
	if err = s.charge(ctx, tx, a, 256, now); err != nil {
		return Result{}, err
	}
	day := now / 86400
	used, incoming, err := quotaRow(ctx, tx, a.account, day)
	if err != nil {
		return Result{}, err
	}
	if c.Amount > s.dailyLimit(a)+incoming-used {
		return Result{}, rateError(now, "quota_exhausted", "Insufficient remaining allowance for this transfer and its 256-byte transaction fee.")
	}
	_, targetIncoming, err := quotaRow(ctx, tx, target, day)
	if err != nil {
		return Result{}, err
	}
	if targetIncoming > s.config.GlobalDailyBytes-c.Amount {
		return Result{}, problem(409, "recipient_limit", "Recipient allowance cannot exceed the shared daily capacity.")
	}
	if _, err = tx.ExecContext(ctx, "UPDATE quota SET used=used+? WHERE actor=? AND day=?", c.Amount, a.account, day); err != nil {
		return Result{}, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO quota(actor,day,incoming) VALUES(?,?,?) ON CONFLICT(actor,day) DO UPDATE SET incoming=incoming+excluded.incoming", target, day, c.Amount); err != nil {
		return Result{}, err
	}
	if err = audit(ctx, tx, c.Operation, a.id, c.Target, fmt.Sprintf("%d bytes; expires %d", c.Amount, (day+1)*86400), now); err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"transferred_bytes": c.Amount, "transaction_fee_bytes": 256, "recipient": c.Target, "expires_at": (day + 1) * 86400}}, nil
}

func (s *Store) lease(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	if _, err := roomAccess(ctx, tx, c.Room, a); err != nil {
		return Result{}, err
	}
	if !slug.MatchString(c.Target) {
		return Result{}, problem(400, "invalid_lease", "Lease target must be a lowercase ASCII slug of 1–64 characters.")
	}
	var owner string
	var fence, expires int64
	err := tx.QueryRowContext(ctx, "SELECT account,fence,expires_at FROM leases WHERE room=? AND name=?", c.Room, c.Target).Scan(&owner, &fence, &expires)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Result{}, err
	}
	if c.Operation == "lease.release" {
		if owner != a.account || expires <= now {
			return Result{}, problem(409, "lease_not_owned", "You do not hold an active lease for this target.")
		}
		if c.Amount != fence {
			return Result{}, problem(409, "stale_fence", "Release requires amount equal to the active fencing token.")
		}
		if err = s.charge(ctx, tx, a, 256, now); err != nil {
			return Result{}, err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE leases SET expires_at=0 WHERE room=? AND name=?", c.Room, c.Target); err != nil {
			return Result{}, err
		}
		return Result{Data: map[string]any{"released": true, "fence": fence}}, nil
	}
	if c.TTL < 1 || c.TTL > 3600 {
		return Result{}, problem(400, "invalid_ttl", "Lease TTL must be 1–3600 seconds.")
	}
	if expires > now {
		return Result{}, problem(409, "lease_busy", "This target already has an active lease; wait until it expires.")
	}
	if err = s.charge(ctx, tx, a, 512, now); err != nil {
		return Result{}, err
	}
	fence++
	if _, err = tx.ExecContext(ctx, "INSERT INTO leases(room,name,account,fence,expires_at) VALUES(?,?,?,?,?) ON CONFLICT(room,name) DO UPDATE SET account=excluded.account,fence=excluded.fence,expires_at=excluded.expires_at", c.Room, c.Target, a.account, fence, now+c.TTL); err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"room": c.Room, "target": c.Target, "holder": a.id, "fence": fence, "expires_at": now + c.TTL, "notice": "External consumers must enforce fencing tokens; a lease cannot guarantee exactly-once external work."}}, nil
}

// RotateGeneration is an operator recovery operation. After restoring an older
// backup, invoke it before serving traffic so cursors cannot silently skip gaps.
func (s *Store) RotateGeneration(ctx context.Context) error {
	generation := randomID()
	if _, err := s.db.ExecContext(ctx, "UPDATE meta SET value=? WHERE key='generation'", generation); err != nil {
		return err
	}
	s.cursorMu.Lock()
	s.generation = generation
	s.cursorMu.Unlock()
	return nil
}
