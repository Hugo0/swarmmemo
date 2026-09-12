package board

import (
	"context"
	"database/sql"
	"time"
)

type privateReadBucket struct {
	tokens        float64
	updated, seen time.Time
}

func privateBucket(now time.Time, previous privateReadBucket, rate, burst float64) privateReadBucket {
	if previous.updated.IsZero() {
		return privateReadBucket{tokens: burst, updated: now, seen: now}
	}
	elapsed := now.Sub(previous.updated).Seconds()
	if elapsed > 0 {
		previous.tokens += elapsed * rate
		if previous.tokens > burst {
			previous.tokens = burst
		}
		previous.updated = now
	}
	previous.seen = now
	return previous
}

// Admission commits all four buckets together. Rejection cannot partially
// charge a child when its aggregate owner/service bucket is exhausted.
func (s *Store) admitPrivateRead(g privateReadRow) error {
	s.privateRateMu.Lock()
	defer s.privateRateMu.Unlock()
	now := s.now()
	childKey, ownerKey, roomKey := "child:"+g.ID, "owner:"+g.Owner, "room:"+g.Room
	// An aggregate-denied authorized attempt is still activity. Touch existing
	// entries without creating state or partially debiting/refilling tokens.
	for _, key := range []string{childKey, ownerKey, roomKey} {
		if bucket, ok := s.privateRates[key]; ok {
			bucket.seen = now
			s.privateRates[key] = bucket
		}
	}
	child := privateBucket(now, s.privateRates[childKey], 1, 10)
	owner := privateBucket(now, s.privateRates[ownerKey], 2, 20)
	room := privateBucket(now, s.privateRates[roomKey], 2, 20)
	service := privateBucket(now, s.privateServiceRate, 10, 60)
	if child.tokens < 1 || owner.tokens < 1 || room.tokens < 1 || service.tokens < 1 {
		return privateReadError("private_read_rate_limited")
	}
	needed := 0
	if _, ok := s.privateRates[childKey]; !ok {
		needed++
	}
	if _, ok := s.privateRates[ownerKey]; !ok {
		needed++
	}
	if _, ok := s.privateRates[roomKey]; !ok {
		needed++
	}
	if len(s.privateRates)+needed > 1024 {
		for key, bucket := range s.privateRates {
			if key != childKey && key != ownerKey && key != roomKey && now.Sub(bucket.seen) >= 60*time.Second {
				delete(s.privateRates, key)
			}
		}
		if len(s.privateRates)+needed > 1024 {
			return privateReadError("private_read_rate_limited")
		}
	}
	child.tokens--
	owner.tokens--
	room.tokens--
	service.tokens--
	s.privateRates[childKey], s.privateRates[ownerKey], s.privateRates[roomKey], s.privateServiceRate = child, owner, room, service
	return nil
}
func (s *Store) readPrivateGrant(ctx context.Context, tx *sql.Tx, c Command, a actor, g privateReadRow, now int64) (Result, error) {
	if err := privateReadFresh(c, now); err != nil {
		return Result{}, err
	}
	if c.Room != g.Room || a.id != g.ID {
		return Result{}, privateReadError("not_found")
	}
	state, err := privateReadState(ctx, tx, g, now)
	if err != nil {
		return Result{}, err
	}
	if state != "active" {
		return Result{}, privateReadError("not_found")
	}
	limit := c.Limit
	if limit == 0 {
		limit = 10
	}
	if c.Operation == "messages.list" && (limit < 1 || limit > 100) {
		return Result{}, privateReadError("invalid_limit")
	}
	if c.Operation == "message.get" && !workIDRE.MatchString(c.MessageID) {
		return Result{}, privateReadError("not_found")
	}
	if err = s.admitPrivateRead(g); err != nil {
		return Result{}, err
	}
	if c.Operation == "room.get" {
		return Result{OK: true, Data: map[string]any{"private_room": map[string]string{"name": g.Room, "visibility": "private"}}}, nil
	}
	// This is intentionally separate from generic member/public read SQL. The
	// owner is not substituted into an unrestricted actor and no global event
	// lookup precedes the fixed-room private selection.
	where := "e.room=? AND r.visibility='private'"
	args := []any{g.Room}
	order := "DESC"
	if c.Operation == "message.get" {
		where += " AND e.id=?"
		args = append(args, c.MessageID)
		limit = 1
	} else if c.Cursor != "" {
		sequence, err := s.parseCursor(c.Cursor)
		if err != nil {
			return Result{}, err
		}
		where += " AND e.seq>?"
		args = append(args, sequence)
		order = "ASC"
	}
	args = append(args, limit)
	rows, err := tx.QueryContext(ctx, "SELECT "+eventColumns+" FROM events e JOIN rooms r ON r.name=e.room WHERE "+where+" ORDER BY e.seq "+order+" LIMIT ?", args...)
	if err != nil {
		return Result{}, err
	}
	events := []Message{}
	for rows.Next() {
		event, e := scanEvent(rows)
		if e != nil {
			rows.Close()
			return Result{}, e
		}
		events = append(events, event)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return Result{}, err
	}
	if c.Operation == "message.get" && len(events) == 0 {
		return Result{}, privateReadError("not_found")
	}
	if err = s.loadAttachments(ctx, tx, events, now); err != nil {
		return Result{}, err
	}
	if order == "DESC" {
		for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
			events[i], events[j] = events[j], events[i]
		}
	}
	for _, event := range events {
		if event.Visibility != "private" || event.Room != g.Room || event.ArchiveEligible || event.DelegationID != "" {
			return Result{}, privateReadError("not_found")
		}
		// The per-event cap measures encoded JSON, without the helper's
		// standalone newline; the full response below includes its final newline.
		if err = privateReadBound(event, (256<<10)+1); err != nil {
			return Result{}, err
		}
	}
	result := Result{OK: true, Messages: events, NextCursor: c.Cursor}
	if len(events) > 0 {
		result.NextCursor = s.cursor(events[len(events)-1].internalSequence)
	} else if result.NextCursor == "" || result.NextCursor == "start" {
		result.NextCursor = s.cursor(0)
	}
	if err = tx.QueryRowContext(ctx, "SELECT value FROM meta WHERE key='generation'").Scan(&result.Generation); err != nil {
		return Result{}, err
	}
	if err = privateReadBound(result, 1<<20); err != nil {
		return Result{}, err
	}
	return result, nil
}
