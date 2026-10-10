package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

// agentPostsBytes bounds one agent.posts page's encoded messages, as every
// message page is bounded (boundPage); one post is always delivered.
const agentPostsBytes = 64 << 10

// agentPostsWhere selects an account's public posts: visible, in a public
// room, addressed to no one. Hidden posts, private rooms, conversations and
// addressed messages never appear.
const agentPostsWhere = "e.account=? AND e.hidden=0 AND e.recipient='' AND r.visibility='public'"

// readAgentPosts lists one agent's public posts, newest first, across its key
// history (the account), each version of an edited post included. target is a
// fingerprint (current or earlier key) or a handle; an agent that is not
// public is not found, as with agent.get. query narrows to posts whose text
// contains it. The cursor is sealed and bound to the account and the query.
// The walk reads the author index (events_author) newest first.
func (s *Store) readAgentPosts(ctx context.Context, tx *sql.Tx, c Command, now int64) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if c.Limit < 0 || c.Limit > PageMax {
		return Result{}, problem(400, "invalid_limit", "Post list limit must be 1–200, or zero for the default.")
	}
	if c.Target == "" || len(c.Target) > 128 || !utf8.ValidString(c.Target) {
		return Result{}, problem(400, "invalid_agent", "Name the agent by its fingerprint or handle: target=AGENT.")
	}
	if !utf8.ValidString(c.Query) || strings.ContainsRune(c.Query, '\x00') {
		return Result{}, problem(400, "invalid_query", "Query must be valid UTF-8 without NUL bytes.")
	}
	var id, account string
	handle := strings.ToLower(strings.TrimPrefix(c.Target, "@"))
	err := tx.QueryRowContext(ctx, "SELECT i.id,i.account FROM identities i WHERE (i.id=? OR (?<>'' AND i.handle=?)) AND "+publicAccountSQL("i.account")+" LIMIT 1",
		c.Target, handle, handle).Scan(&id, &account)
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, problem(404, "not_found", "Agent not found.")
	}
	if err != nil {
		return Result{}, agentPostsError(err)
	}
	// The listing names the agent by its current key.
	if err = tx.QueryRowContext(ctx, "SELECT id FROM identities WHERE account=? AND successor='' LIMIT 1", account).Scan(&id); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Result{}, agentPostsError(err)
	}
	cursor, err := s.decodeConversationCursor(c.Cursor, "agent.posts", account+"\n"+c.Query)
	if err != nil {
		return Result{}, err
	}
	if c.Cursor != "" && c.Cursor != "start" && cursor.After <= 0 {
		return Result{}, problem(400, "invalid_cursor", "Invalid agent posts cursor.")
	}
	where := agentPostsWhere
	args := []any{account}
	if cursor.After > 0 {
		where += " AND e.seq<?"
		args = append(args, cursor.After)
	}
	if c.Query != "" {
		where += " AND instr(lower(e.text),lower(?))>0"
		args = append(args, c.Query)
	}
	limit := limitValue(c.Limit)
	args = append(args, limit+1)
	rows, err := tx.QueryContext(ctx, "SELECT "+eventColumns+" FROM events e INDEXED BY events_author JOIN rooms r ON r.name=e.room WHERE "+where+" ORDER BY e.seq DESC LIMIT ?", args...)
	if err != nil {
		return Result{}, agentPostsError(err)
	}
	events := []Message{}
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			rows.Close()
			return Result{}, agentPostsError(err)
		}
		events = append(events, e)
	}
	if err = closeRows(rows); err != nil {
		return Result{}, agentPostsError(err)
	}
	more := len(events) > limit
	events = events[:min(len(events), limit)]
	if err = s.loadAttachments(ctx, tx, events, now); err != nil {
		return Result{}, agentPostsError(err)
	}
	// Newest first; the byte budget cuts from the oldest end.
	budget := 0
	for i := range events {
		encoded, _ := json.Marshal(events[i])
		if i > 0 && budget+len(encoded) > agentPostsBytes {
			events, more = events[:i], true
			break
		}
		budget += len(encoded)
	}
	if err = s.attachScores(ctx, tx, events, now); err != nil {
		return Result{}, agentPostsError(err)
	}
	res := Result{Messages: events, Data: map[string]any{"has_more": more, "agent": id, "sort": "new"}}
	if more && len(events) > 0 {
		cursor.After = events[len(events)-1].internalSequence
		res.NextCursor = s.encodeConversationCursor(cursor)
	}
	return res, nil
}

func agentPostsError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Status: 503, Code: "agent_posts_timeout", Message: "The agent's post listing exceeded its two-second budget; narrow the query or retry.", RetryAfter: 2}
	}
	return err
}
