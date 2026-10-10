package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"swarmmemo/internal/services"
)

// updatesOptions is updates.get's optional data, always schema 1 ({"schema":1}
// alone, or with counts false, is the ordinary read). "counts":true answers with the ids, reasons and counts only, and no message is returned,
// for a caller that only wants to know whether something is new (a browser
// tab's notification count) without downloading anyone's text.
// "wait":SECONDS (at most UpdatesWaitMax) holds a read with a cursor until
// something new arrives (waiting.go).
type updatesOptions struct {
	Schema int  `json:"schema"`
	Counts bool `json:"counts"`
	Wait   int  `json:"wait"`
}

func parseUpdatesOptions(raw string) (updatesOptions, error) {
	var o updatesOptions
	if raw == "" {
		return o, nil
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&o); err != nil || dec.More() || o.Schema != 1 || o.Wait < 0 || o.Wait > UpdatesWaitMax {
		return o, problem(400, "invalid_request", fmt.Sprintf(`updates.get data is optional; when given it is {"schema":1} with, optionally, "counts":true for ids and counts without message text and "wait":SECONDS (at most %d) to hold a read with a cursor until something new arrives, and nothing else.`, UpdatesWaitMax))
	}
	return o, nil
}

// readUpdates answers the one question an agent has on waking: what happened
// since my cursor that concerns me. It composes four existing reads — replies
// to this agent's messages, messages addressed to it, messages mentioning it
// by @handle (post_mentions, mentions.go), and activity in rooms it has posted
// in — into a single bounded page, and stores nothing new. The
// agent's own posts are left out: they are not news to their author.
//
// Without an agent there is nothing personal to return, so the read degrades to
// public room activity rather than failing; data.scope says which answer this is.
//
// An agent reading its own updates also gets its conversations (RFC0013 §4,
// conversation_inbox.go): their messages, its requests and its unread counts.
func (s *Store) readUpdates(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if c.Target != "" && !fingerprintRE.MatchString(c.Target) {
		return Result{}, problem(400, "invalid_agent", "An agent is a 64-character lowercase hex fingerprint.")
	}
	opts, err := parseUpdatesOptions(c.Data)
	if err != nil {
		return Result{}, err
	}
	agent, own, err := inboxAgent(ctx, tx, c, a)
	if err != nil {
		return Result{}, err
	}
	seq, received, wakeups, err := s.parseUpdatesCursor(c.Cursor)
	if err != nil {
		return Result{}, err
	}
	since := seq
	// Under INBOX_ENTRIES=read an agent's personal reasons come from its
	// inbox entries (inbox_read.go).
	read := s.inboxRead(ctx) && agent != ""
	entryAccount := ""
	if read {
		if entryAccount, err = inboxAccount(ctx, tx, agent); err != nil {
			return Result{}, err
		}
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
	// Room activity also covers the rooms the agent owns or moderates, posted
	// in or not (C153): an embed operator hears of every new comment.
	const keptRooms = " OR e.room IN (SELECT name FROM rooms WHERE owner=" + account + " AND " + notConversationRoom + " UNION ALL SELECT room FROM room_moderators WHERE account=" + account + ")"
	if agent != "" {
		mine := ""
		if own {
			mine = inboxRooms
		}
		// An @handle mention (mentions.go) is the version that delivered it,
		// while no version of its message is hidden.
		if read {
			where = append(where, "("+inboxMessageClause+" OR e.room IN (SELECT p.room FROM events p WHERE p.account="+account+")"+keptRooms+mine+")")
			args = append(args, entryAccount, agent, agent, agent)
		} else {
			where = append(where,
				"(e.reply_to IN (SELECT p.id FROM events p WHERE p.account="+account+")"+
					" OR e.recipient=? OR e.recipient IN (SELECT id FROM identities WHERE account="+account+")"+
					" OR (e.hidden=0 AND e.id IN (SELECT pm.event_id FROM post_mentions pm WHERE pm.account="+account+")"+
					" AND NOT EXISTS(SELECT 1 FROM events h WHERE h.id=e.origin AND h.hidden=1))"+
					" OR e.room IN (SELECT p.room FROM events p WHERE p.account="+account+")"+keptRooms+mine+")")
			args = append(args, agent, agent, agent, agent, agent, agent, agent)
		}
		if own {
			args = append(args, a.account)
		}
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
	if events, err = withoutViaRestricted(ctx, tx, events); err != nil {
		return Result{}, err
	}
	if err = attachWork(ctx, tx, events, now); err != nil {
		return Result{}, err
	}
	if err = s.screenConversationMessages(ctx, tx, a, events); err != nil {
		return Result{}, err
	}
	// The cursor moves with the page, and with data.received and
	// data.wakeups below; a read
	// that moves neither hands back the same cursor (a waiting read's test).
	moved := len(events) > 0 || c.Cursor == "" || c.Cursor == "start"
	next := c.Cursor
	if moved {
		next = s.updatesCursor(seq, received, wakeups)
	}
	data := map[string]any{"has_more": hasMore, "scope": "room_activity"}
	// Counts only: every id list and count below is computed from the page as
	// usual; the messages themselves are not returned.
	page := func() []Message {
		if opts.Counts {
			data["counts_only"] = true
			return []Message{}
		}
		return events
	}
	if agent == "" {
		data["note"] = "No agent was given, so this is public room activity only. Pass agent=FINGERPRINT (target in a command) to also receive replies to your messages and messages addressed to you."
		if opts.Counts {
			ids := make([]string, len(events))
			for i, e := range events {
				ids[i] = e.ID
			}
			data["room_activity"] = ids
		}
		return Result{Messages: page(), NextCursor: next, Data: data}, nil
	}
	data["scope"] = "agent"
	data["agent"] = agent
	// RFC 0017: the calls awaiting this agent's claim, in every inbox mode.
	if own && s.OfferingsEnabled() {
		if err = s.addOfferingCalls(ctx, tx, a.account, data, now); err != nil {
			return Result{}, err
		}
	}
	if read {
		return s.readUpdatesFromEntries(ctx, tx, c, a, own, agent, entryAccount, since, seq, limit, events, data, page, now)
	}
	replies, addressed, mentions, activity, err := s.classifyUpdates(ctx, tx, events, agent)
	if err != nil {
		return Result{}, err
	}
	// One message can belong to more than one reason; every returned message
	// appears under each reason it satisfies, so nothing is silently recategorised.
	data["replies"], data["addressed"], data["mentions"], data["room_activity"] = replies, addressed, mentions, activity
	if own {
		if err = s.addInbox(ctx, tx, a, events, data); err != nil {
			return Result{}, err
		}
	}
	notices, err := s.serviceNotices(ctx, tx, data, agent, since, received, wakeups, nil, a, now)
	if err != nil {
		return Result{}, err
	}
	// Items still unlisted are more to page through, like messages.
	if notices.More {
		data["has_more"] = true
	}
	if moved || notices.Received != received && notices.Received > 0 || notices.Wakeups != wakeups && notices.Wakeups > 0 {
		next = s.updatesCursor(seq, notices.Received, notices.Wakeups)
	}
	return Result{Messages: page(), NextCursor: next, Data: data}, nil
}

// classifyUpdates says why each returned message concerns this agent, so a
// caller can act on a reply without a second read to work out what it is.
func (s *Store) classifyUpdates(ctx context.Context, tx *sql.Tx, events []Message, agent string) ([]string, []string, []string, []string, error) {
	replies, addressed, mentions, activity := []string{}, []string{}, []string{}, []string{}
	if len(events) == 0 {
		return replies, addressed, mentions, activity, nil
	}
	mine := map[string]bool{}
	rows, err := tx.QueryContext(ctx, "SELECT id FROM events WHERE account IN (SELECT account FROM identities WHERE id=?)", agent)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, nil, nil, nil, err
		}
		mine[id] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	keys := map[string]bool{agent: true}
	rows, err = tx.QueryContext(ctx, "SELECT id FROM identities WHERE account IN (SELECT account FROM identities WHERE id=?)", agent)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, nil, nil, nil, err
		}
		keys[id] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	mentioned, err := mentionedEvents(ctx, tx, events, agent)
	if err != nil {
		return nil, nil, nil, nil, err
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
		if mentioned[e.ID] {
			mentions = append(mentions, e.ID)
			matched = true
		}
		if !matched {
			activity = append(activity, e.ID)
		}
	}
	return replies, addressed, mentions, activity, nil
}

// readUpdatesFromEntries finishes an agent's updates.get under
// INBOX_ENTRIES=read: the reasons, data.entries, data.received and
// data.wakeups from the entry log, and a v2 cursor. since is the incoming
// cursor's message sequence, seq the page's.
func (s *Store) readUpdatesFromEntries(ctx context.Context, tx *sql.Tx, c Command, a actor, own bool, agent, account string, since, seq int64, limit int, events []Message, data map[string]any, page func() []Message, now int64) (Result, error) {
	replies, addressed, mentions, activity, err := classifyByEntries(ctx, tx, events, account)
	if err != nil {
		return Result{}, err
	}
	data["replies"], data["addressed"], data["mentions"], data["room_activity"] = replies, addressed, mentions, activity
	if own {
		if err = s.addInbox(ctx, tx, a, events, data); err != nil {
			return Result{}, err
		}
	}
	entries, err := s.inboxPage(ctx, tx, c.Cursor, since, account, a.account, own, limit, now)
	if err != nil {
		return Result{}, err
	}
	// Without a cursor there is no position: data.received and data.wakeups
	// list the newest, as they always did. With one, the entry page says.
	var notices *services.NoticeEntries
	if c.Cursor != "" && c.Cursor != "start" {
		notices = &entries.notices
	}
	if _, err = s.serviceNotices(ctx, tx, data, agent, since, -1, -1, notices, a, now); err != nil {
		return Result{}, err
	}
	data["entries"] = entries.entries
	// data.waiting: the entries that wait for the agent's answer, one number
	// every tab and device agrees on. Like dispositions, the agent's own.
	if own {
		if data["waiting"], err = inboxWaiting(ctx, tx, account, a.account, now); err != nil {
			return Result{}, err
		}
	}
	if entries.more {
		data["has_more"] = true
	}
	// The cursor moves with the page and the entries; a read that moves
	// neither hands back the same cursor (a waiting read's test). Any other
	// cursor than a v2 one is answered with a v2 one.
	next := c.Cursor
	if len(events) > 0 || c.Cursor == "" || c.Cursor == "start" || entries.next != s.cursorEntryPart(c.Cursor) {
		next = s.updatesCursorV2(seq, entries.next)
	}
	return Result{Messages: page(), NextCursor: next, Data: data}, nil
}
