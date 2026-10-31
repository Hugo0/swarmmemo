package board

// The live stream (C665, HexArena platform S4): stream.read is what an
// authenticated /api/stream carries for one credential, after a cursor:
// new messages in the caller's app rooms (app/…, as a member) and its inbox
// (messages addressed to it, replies to its posts, its @handle mentions).
// With an app's token it is only the rooms that token's scopes reach
// (app-rooms: the app's rooms the person is in; dm: the two-member ones),
// never another app's rooms and never the person's inbox elsewhere. It is
// one read, checked as every command is (signature, token, scope), so the
// stream needs no second permission system, and masked rooms read masked
// (maskResult). The HTTP adapter re-runs it on each committed write
// (Changes): the existing fan-out, no second pub/sub. Its position is a
// message cursor; data.positions has each message's own, the SSE event id a
// client resumes from with Last-Event-ID.

import (
	"context"
	"database/sql"
	"slices"
	"strconv"
)

// StreamPageMax bounds one stream.read page.
const StreamPageMax = 100

// streamScanWindow bounds the positions one stream.read scans: an idle
// stream's position moves with the board (a var for tests).
var streamScanWindow int64 = 5000

type streamCursorKey struct{}

// WithStreamCursor sets the position a stream.read resumes from, in place
// of its command's cursor: the HTTP stream advances it between reads of
// one signed command (the position is no authority: a read only ever shows
// what its credential may read).
func WithStreamCursor(ctx context.Context, cursor string) context.Context {
	return context.WithValue(ctx, streamCursorKey{}, cursor)
}

// readStream is stream.read.
func (s *Store) readStream(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	cursor := c.Cursor
	if v, ok := ctx.Value(streamCursorKey{}).(string); ok {
		cursor = v
	}
	var after int64
	if cursor == "" {
		// No position: the stream starts now.
		if err := tx.QueryRowContext(ctx, "SELECT coalesce(max(seq),0) FROM events").Scan(&after); err != nil {
			return Result{}, err
		}
		return Result{NextCursor: s.cursor(after), Data: map[string]any{"positions": []string{}, "has_more": false}}, nil
	}
	after, err := s.parseCursor(cursor)
	if err != nil {
		return Result{}, err
	}
	limit := c.Limit
	if limit <= 0 || limit > StreamPageMax {
		limit = StreamPageMax
	}
	// One read scans at most streamScanWindow positions from the cursor
	// and moves the cursor past those it scanned, so a stream re-read on
	// every commit scans only what is new, however old its cursor.
	var head int64
	if err = tx.QueryRowContext(ctx, "SELECT coalesce(max(seq),0) FROM events").Scan(&head); err != nil {
		return Result{}, err
	}
	upper := min(head, after+streamScanWindow)
	where, args := streamScope(a)
	args = append([]any{after, upper}, append(args, limit+1)...)
	rows, err := tx.QueryContext(ctx, "SELECT "+eventColumns+" FROM events e JOIN rooms r ON r.name=e.room WHERE e.seq>? AND e.seq<=? AND e.hidden=0 AND "+where+" ORDER BY e.seq LIMIT ?", args...)
	if err != nil {
		return Result{}, err
	}
	events := []Message{}
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			rows.Close()
			return Result{}, err
		}
		events = append(events, e)
	}
	if err = closeRows(rows); err != nil {
		return Result{}, err
	}
	more := len(events) > limit
	next := cursor
	switch {
	case more:
		events = events[:limit]
		next = s.cursor(events[len(events)-1].internalSequence)
	case upper > after:
		next, more = s.cursor(upper), upper < head
	}
	// A room this wire may not carry drops out; its position still moves.
	if events, err = withoutViaRestricted(ctx, tx, events); err != nil {
		return Result{}, err
	}
	if err = s.loadAttachments(ctx, tx, events, now); err != nil {
		return Result{}, err
	}
	if err = s.screenConversationMessages(ctx, tx, a, events); err != nil {
		return Result{}, err
	}
	positions := make([]string, len(events))
	for i, e := range events {
		positions[i] = s.cursor(e.internalSequence)
	}
	return Result{Messages: events, NextCursor: next, Data: map[string]any{"positions": positions, "has_more": more}}, nil
}

// streamScope is the SQL clause (and its arguments) of what a's stream
// carries.
func streamScope(a actor) (string, []any) {
	member := "EXISTS(SELECT 1 FROM members m WHERE m.room=e.room AND m.account=?)"
	if b := a.bearer; b != nil {
		// Only the rooms the token's scopes reach: the app's own, the
		// person a member; with dm alone, those of two people besides it.
		prefix := "app/" + b.app + "/"
		clause := "substr(e.room,1," + strconv.Itoa(len(prefix)) + ")=? AND r.owner=? AND " + member
		args := []any{prefix, b.app, a.account}
		switch {
		case slices.Contains(b.scopes, "app-rooms"):
		case slices.Contains(b.scopes, "dm"):
			clause += " AND (SELECT count(*) FROM members p WHERE p.room=e.room AND p.account<>r.owner)=2"
		default:
			return "0", nil
		}
		return "(" + clause + ")", args
	}
	const account = "?"
	inbox := "e.account<>" + account + " AND (r.visibility='public' OR " + member + ") AND (" +
		"e.recipient IN (SELECT id FROM identities WHERE account=" + account + ")" +
		" OR e.reply_to IN (SELECT p.id FROM events p WHERE p.account=" + account + ")" +
		" OR e.id IN (SELECT pm.event_id FROM post_mentions pm WHERE pm.account=" + account + "))"
	clause := "((substr(e.room,1,4)='app/' AND " + member + ") OR (" + inbox + "))"
	return clause, []any{a.account, a.account, a.account, a.account, a.account, a.account}
}
