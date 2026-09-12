package board

import (
	"context"
	"encoding/json"
	"time"
)

// PublicUpdates returns current public versions affected by moderation or attachment
// deletion. The integer position refers only to the public changes journal; it grants
// no access and is independent of the encrypted message and archive cursors.
// after=-1 captures a watermark without replaying history. Capture it before loading
// the initial public feed so a concurrent moderation action cannot fall through a gap.
func (s *Store) PublicUpdates(ctx context.Context, after int64) ([]Message, int64, error) {
	events, next, _, err := s.PublicUpdatesGeneration(ctx, after, "")
	return events, next, err
}

// PublicUpdatesGeneration binds correction positions to a recovery generation.
// The generation and journal page are read in the same SQLite transaction, so a
// client can reject an old watermark after restoration instead of skipping data.
// An empty expected generation bootstraps (or preserves the legacy read contract).
func (s *Store) PublicUpdatesGeneration(ctx context.Context, after int64, expected string) ([]Message, int64, string, error) {
	if after < -1 {
		return nil, after, "", problem(400, "invalid_revision", "Use -1 for the initial public revision or a nonnegative revision returned by the server.")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, after, "", err
	}
	defer tx.Rollback()
	var generation string
	if err = tx.QueryRowContext(ctx, "SELECT value FROM meta WHERE key='generation'").Scan(&generation); err != nil {
		return nil, after, "", err
	}
	if expected != "" && expected != generation {
		return nil, after, "", problem(409, "cursor_reset", "The server generation changed; reconcile retained records before resuming correction or event traversal.")
	}
	if after == -1 {
		var current int64
		err = tx.QueryRowContext(ctx, `SELECT coalesce(max(ch.seq),0)
 FROM changes ch JOIN events e ON e.id=ch.event_id JOIN rooms r ON r.name=e.room
 WHERE ch.urgent=1 AND r.visibility='public'`).Scan(&current)
		if err != nil {
			return nil, after, "", err
		}
		if err = tx.Commit(); err != nil {
			return nil, after, "", err
		}
		return []Message{}, current, generation, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+eventColumns+`,ch.seq
 FROM changes ch JOIN events e ON e.id=ch.event_id JOIN rooms r ON r.name=e.room
 WHERE ch.urgent=1 AND ch.seq>? AND r.visibility='public'
 ORDER BY ch.seq LIMIT 100`, after)
	if err != nil {
		return nil, after, "", err
	}
	events := []Message{}
	positions := []int64{}
	for rows.Next() {
		var position int64
		event, scanErr := scanEvent(liveChangeRow{row: rows, position: &position})
		if scanErr != nil {
			rows.Close()
			return nil, after, "", scanErr
		}
		events = append(events, event)
		positions = append(positions, position)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, after, "", err
	}
	if err = s.loadAttachments(ctx, tx, events, s.now().Unix()); err != nil {
		return nil, after, "", err
	}
	// Do not advance past an update excluded by the byte budget. Preserve the full
	// first event even if its signed payload alone exceeds the normal response cap.
	budget, count := 0, 0
	for _, event := range events {
		encoded, encodeErr := json.Marshal(event)
		if encodeErr != nil {
			return nil, after, "", encodeErr
		}
		if count > 0 && budget+len(encoded) > 64<<10 {
			break
		}
		budget += len(encoded)
		count++
	}
	next := after
	if count > 0 {
		next = positions[count-1]
	}
	if err = tx.Commit(); err != nil {
		return nil, after, "", err
	}
	return events[:count], next, generation, nil
}

// Extend the shared sanitized event scanner with the separate change-journal position.
type liveChangeRow struct {
	row      scanner
	position *int64
}

func (r liveChangeRow) Scan(destinations ...any) error {
	return r.row.Scan(append(destinations, r.position)...)
}
