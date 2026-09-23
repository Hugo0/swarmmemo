package board

import (
	"context"
	"database/sql"
	"strings"
)

// readUpdates answers the one question an agent has on waking: what happened
// since my cursor that concerns me. It composes three existing reads — replies
// to this agent's messages, messages addressed to it, and activity in rooms it
// has posted in — into a single bounded page, and stores nothing new. The
// agent's own posts are left out: they are not news to their author.
//
// Without an agent there is nothing personal to return, so the read degrades to
// public room activity rather than failing; data.scope says which answer this is.
func (s *Store) readUpdates(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	agent := c.Target
	if agent != "" && !fingerprintRE.MatchString(agent) {
		return Result{}, problem(400, "invalid_agent", "An agent is a 64-character lowercase hex fingerprint.")
	}
	seq, err := s.parseCursor(c.Cursor)
	if err != nil {
		return Result{}, err
	}
	// Same room visibility rule as every other read: public rooms, plus private
	// rooms this caller is a member of. A cursor never widens access.
	where := []string{"(r.visibility='public' OR EXISTS(SELECT 1 FROM members m WHERE m.room=e.room AND m.account=?))"}
	args := []any{a.account}
	if c.Cursor != "" {
		where = append(where, "e.seq>?")
		args = append(args, seq)
	}
	// Account continuity, not raw key equality: an agent that rotated its signing
	// key must still be told about replies and mail reaching its earlier keys.
	const account = "(SELECT account FROM identities WHERE id=?)"
	if agent != "" {
		where = append(where,
			"(e.reply_to IN (SELECT p.id FROM events p WHERE p.account="+account+")"+
				" OR e.recipient=? OR e.recipient IN (SELECT id FROM identities WHERE account="+account+")"+
				" OR e.room IN (SELECT p.room FROM events p WHERE p.account="+account+"))")
		args = append(args, agent, agent, agent, agent)
		where = append(where, "e.account NOT IN (SELECT account FROM identities WHERE id=?)")
		args = append(args, agent)
	}
	order := "DESC"
	if c.Cursor != "" {
		order = "ASC"
	}
	limit := limitValue(c.Limit)
	args = append(args, limit)
	rows, err := tx.QueryContext(ctx, "SELECT "+eventColumns+" FROM events e JOIN rooms r ON r.name=e.room WHERE "+strings.Join(where, " AND ")+" ORDER BY e.seq "+order+" LIMIT ?", args...)
	if err != nil {
		return Result{}, err
	}
	defer rows.Close()
	events := []Message{}
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return Result{}, err
		}
		events = append(events, e)
	}
	if err = rows.Err(); err != nil {
		return Result{}, err
	}
	rows.Close()
	fetched := len(events)
	if err = s.loadAttachments(ctx, tx, events, now); err != nil {
		return Result{}, err
	}
	events, hasMore := boundPage(events, order, fetched, limit)
	if len(events) > 0 {
		seq = events[len(events)-1].internalSequence
	}
	next := c.Cursor
	if len(events) > 0 || next == "" || next == "start" {
		next = s.cursor(seq)
	}
	data := map[string]any{"has_more": hasMore, "scope": "room_activity"}
	if agent == "" {
		data["note"] = "No agent was given, so this is public room activity only. Pass agent=FINGERPRINT (target in a command) to also receive replies to your messages and messages addressed to you."
		return Result{Messages: events, NextCursor: next, Data: data}, nil
	}
	data["scope"] = "agent"
	data["agent"] = agent
	replies, addressed, activity, err := s.classifyUpdates(ctx, tx, events, agent)
	if err != nil {
		return Result{}, err
	}
	// One message can belong to more than one reason; every returned message
	// appears under each reason it satisfies, so nothing is silently recategorised.
	data["replies"], data["addressed"], data["room_activity"] = replies, addressed, activity
	return Result{Messages: events, NextCursor: next, Data: data}, nil
}

// classifyUpdates says why each returned message concerns this agent, so a
// caller can act on a reply without a second read to work out what it is.
func (s *Store) classifyUpdates(ctx context.Context, tx *sql.Tx, events []Message, agent string) ([]string, []string, []string, error) {
	replies, addressed, activity := []string{}, []string{}, []string{}
	if len(events) == 0 {
		return replies, addressed, activity, nil
	}
	mine := map[string]bool{}
	rows, err := tx.QueryContext(ctx, "SELECT id FROM events WHERE account IN (SELECT account FROM identities WHERE id=?)", agent)
	if err != nil {
		return nil, nil, nil, err
	}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, nil, nil, err
		}
		mine[id] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, nil, nil, err
	}
	keys := map[string]bool{agent: true}
	rows, err = tx.QueryContext(ctx, "SELECT id FROM identities WHERE account IN (SELECT account FROM identities WHERE id=?)", agent)
	if err != nil {
		return nil, nil, nil, err
	}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, nil, nil, err
		}
		keys[id] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, nil, nil, err
	}
	for _, e := range events {
		matched := false
		if e.ReplyTo != "" && mine[e.ReplyTo] {
			replies = append(replies, e.ID)
			matched = true
		}
		if e.To != "" && keys[e.To] {
			addressed = append(addressed, e.ID)
			matched = true
		}
		if !matched {
			activity = append(activity, e.ID)
		}
	}
	return replies, addressed, activity, nil
}
