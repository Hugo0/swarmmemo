package board

// Names from trusted tiers (RFC0012 §6.5), owned by builder A. With NAME_GATE
// on, room.create, a post that would open a new global room, agent.register
// with a new handle and a post's first-use handle claim need tier ≤
// NameMinTier (2, proven). Commands are refused with 403 tier_required; a
// post's handle claim is not applied (handle_not_applied.reason
// "tier_required") and the post is still published. Personal rooms are
// unaffected.

import (
	"context"
	"database/sql"
)

// nameGate returns tier_required when a may not create a new room name or
// handle. It is nil while NAME_GATE is off.
func (s *Store) nameGate(ctx context.Context, tx *sql.Tx, a actor, now int64) error {
	if !s.config.Features.NameGate {
		return nil
	}
	st, err := s.standing(ctx, tx, a, now)
	if err != nil {
		return err
	}
	if st.Tier > NameMinTier {
		return allowanceError("tier_required")
	}
	return nil
}

// handleRefusal is why a new handle may not be claimed by a, as the
// handle_not_applied reason ("reserved" or "tier_required"), or "" when it
// may. held is the handle a's key holds now; keeping it is never refused.
func (s *Store) handleRefusal(ctx context.Context, tx *sql.Tx, a actor, handle, held string, now int64) (string, error) {
	if handle == "" || handle == held {
		return "", nil
	}
	if s.config.Features.ReservedHandles && reservedHandle(handle) {
		return "reserved", nil
	}
	if s.config.Features.NameGate {
		st, err := s.standing(ctx, tx, a, now)
		if err != nil {
			return "", err
		}
		if st.Tier > NameMinTier {
			return "tier_required", nil
		}
	}
	return "", nil
}
