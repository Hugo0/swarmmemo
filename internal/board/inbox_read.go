package board

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"

	"swarmmemo/internal/services"
)

// Reads over the inbox entry log (C61 step 2, INBOX_ENTRIES=read). The
// personal half of updates.get (replies, addressed messages, @handle
// mentions, the reasons each message is listed under, data.received and
// data.wakeups) is answered from inbox_entries instead of the concern
// queries; room activity and the caller's active conversations stay
// query-time (they are not entries). The response adds data.entries, the
// unified list, and the cursor becomes v2: (message sequence, entry seq).
// Under off and shadow nothing here runs and every read answers as before.
//
// The cursor's entry part replaces the receiver and wake-up parts, so a
// wake-up notice or a receiver item is listed once, after which the cursor
// is past it, on a quiet board too (the C44/C75 repeat class). An older
// cursor still works: its entry position is the newest entry written before
// its message sequence, so at cut-over at most the entries written at that
// sequence repeat, which the "dedupe by id" contract absorbs.

const (
	// updatesCursorV2Size is a v2 cursor's plaintext: kind 0, the message
	// sequence, updatesCursorV2Tag, the entry seq.
	updatesCursorV2Size = 18
	updatesCursorV2Tag  = 2
)

// inboxRead reports whether updates.get reads the entry log: the
// INBOX_ENTRIES flag, or the mode the operator's parity check
// (InboxReadParity) runs one read under.
func (s *Store) inboxRead(ctx context.Context) bool {
	if m, ok := ctx.Value(inboxModeKey{}).(InboxMode); ok {
		return m == InboxRead
	}
	return s.config.Features.InboxEntries == InboxRead
}

type inboxModeKey struct{}

// updatesCursorV2 is updates.get's cursor under INBOX_ENTRIES=read: the
// message sequence (shared with messages.list) and the seq of the newest
// inbox entry the reads have passed.
func (s *Store) updatesCursorV2(seq, entry int64) string {
	plain := make([]byte, updatesCursorV2Size)
	binary.BigEndian.PutUint64(plain[1:9], uint64(seq))
	plain[9] = updatesCursorV2Tag
	binary.BigEndian.PutUint64(plain[10:], uint64(entry))
	return s.sealCursor(plain)
}

// cursorEntryPart is a v2 cursor's entry part, or -1 for every other
// cursor (none, "start", v1, a messages.list cursor). The cursor has
// already been checked by parseUpdatesCursor.
func (s *Store) cursorEntryPart(cursor string) int64 {
	if cursor == "" || cursor == "start" {
		return -1
	}
	plain, err := s.openCursor(cursor)
	if err != nil || len(plain) != updatesCursorV2Size {
		return -1
	}
	return int64(binary.BigEndian.Uint64(plain[10:]))
}

// inboxAccount is the account whose entries an agent's read lists: its
// continuity account, or the fingerprint itself for a key never seen (the
// resolver keys a message addressed to it the same way).
func inboxAccount(ctx context.Context, tx *sql.Tx, agent string) (string, error) {
	var account string
	err := tx.QueryRowContext(ctx, "SELECT account FROM identities WHERE id=?", agent).Scan(&account)
	if errors.Is(err, sql.ErrNoRows) {
		return agent, nil
	}
	return account, err
}

// inboxMessageClause is the personal part of updates.get's message query
// under INBOX_ENTRIES=read, in place of the reply, addressed and mention
// subqueries: a message the agent's entries list as a reply, addressed or
// (while no version is hidden) a mention. Its one argument is the account.
const inboxMessageClause = "EXISTS(SELECT 1 FROM inbox_entries ie WHERE ie.subject=e.id AND ie.account=? AND (" +
	"ie.kind IN ('reply','addressed')" +
	" OR ie.kind='conversation' AND (instr(','||ie.reasons||',',',reply,')>0 OR instr(','||ie.reasons||',',',addressed,')>0)" +
	" OR ie.kind='mention' AND e.hidden=0 AND NOT EXISTS(SELECT 1 FROM events h WHERE h.id=e.origin AND h.hidden=1)))"

// classifyByEntries is classifyUpdates read from the entries: each message
// under every reason its entry lists, room activity when it has none. A
// hidden message mentions no one, as in mentionedEvents.
func classifyByEntries(ctx context.Context, tx *sql.Tx, events []Message, account string) ([]string, []string, []string, []string, error) {
	replies, addressed, mentions, activity := []string{}, []string{}, []string{}, []string{}
	if len(events) == 0 {
		return replies, addressed, mentions, activity, nil
	}
	args := []any{account}
	for _, e := range events {
		args = append(args, e.ID)
	}
	rows, err := tx.QueryContext(ctx, "SELECT subject,reasons FROM inbox_entries WHERE account=? AND kind IN ('reply','addressed','mention','conversation') AND subject IN (?"+strings.Repeat(",?", len(events)-1)+")", args...)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	reasons := map[string][]string{}
	for rows.Next() {
		var subject, list string
		if err = rows.Scan(&subject, &list); err != nil {
			rows.Close()
			return nil, nil, nil, nil, err
		}
		reasons[subject] = strings.Split(list, ",")
	}
	if err = closeRows(rows); err != nil {
		return nil, nil, nil, nil, err
	}
	for _, e := range events {
		rs := reasons[e.ID]
		matched := false
		if slices.Contains(rs, inboxReply) {
			replies = append(replies, e.ID)
			matched = true
		}
		if slices.Contains(rs, inboxAddressed) {
			addressed = append(addressed, e.ID)
			matched = true
		}
		if slices.Contains(rs, inboxMention) && !e.Hidden {
			mentions = append(mentions, e.ID)
			matched = true
		}
		if !matched {
			activity = append(activity, e.ID)
		}
	}
	return replies, addressed, mentions, activity, nil
}

// UpdateEntry is one item of updates.get data.entries: a pointer into the
// account's inbox, never a body. Detail is small metadata (a work item's
// state and the reader's role, a witness's kind and verdict, a wake-up's
// trigger); Disposition is absent while the entry is open.
type UpdateEntry struct {
	ID          string          `json:"id"`
	Seq         int64           `json:"seq"`
	Kind        string          `json:"kind"`
	Reasons     []string        `json:"reasons"`
	Subject     string          `json:"subject"`
	Room        string          `json:"room,omitempty"`
	Actor       string          `json:"actor,omitempty"`
	Detail      json.RawMessage `json:"detail,omitempty"`
	NeedsAnswer bool            `json:"needs_answer"`
	Disposition string          `json:"disposition,omitempty"`
	CreatedAt   int64           `json:"created_at"`
	Stale       bool            `json:"stale,omitempty"`
}

// inboxPageOf is one read's entry page: what data.entries lists, the
// receiver items and wake-up notices it points at (for data.received and
// data.wakeups), the next cursor's entry part and whether more wait.
type inboxPageOf struct {
	entries []UpdateEntry
	notices services.NoticeEntries
	next    int64
	more    bool
}

// inboxOwnKinds are the kinds only the agent's own read lists.
var inboxOwnKinds = []string{inboxConversation, inboxRequest, inboxReceived, inboxWakeup, inboxWork, inboxWitness}

// inboxPage lists account's entries after the cursor: with no cursor the
// newest limit, oldest first; with "start" from the first; with a v2 cursor
// after its entry part; with any other cursor after the newest entry
// written before its message sequence (since). Every row re-applies access:
// a message entry needs its message to exist and its room to be readable by
// caller now, and the private kinds (inboxOwnKinds) are the own read's only.
// Another reader's page also carries the agent's wake-up notices, for
// data.wakeups as it always showed them, but does not list them.
func (s *Store) inboxPage(ctx context.Context, tx *sql.Tx, cursor string, since int64, account, caller string, own bool, limit int, now int64) (inboxPageOf, error) {
	var page inboxPageOf
	after := int64(-1)
	switch {
	case cursor == "start":
		after = 0
	case cursor != "":
		if after = s.cursorEntryPart(cursor); after < 0 {
			if err := tx.QueryRowContext(ctx, "SELECT coalesce(max(seq),0) FROM inbox_entries WHERE account=? AND event_seq<?", account, since).Scan(&after); err != nil {
				return page, err
			}
		}
	}
	// readable: the room is public, or caller is a member of it.
	query := `SELECT ie.seq,ie.id,ie.kind,ie.reasons,ie.subject,ie.room,ie.actor,ie.detail,ie.needs_answer,ie.disposition,ie.created_at,
 ie.room='' OR EXISTS(SELECT 1 FROM rooms r WHERE r.name=ie.room AND (r.visibility='public' OR EXISTS(SELECT 1 FROM members m WHERE m.room=ie.room AND m.account=?))) AS readable
 FROM inbox_entries ie WHERE ie.account=?`
	args := []any{caller, account}
	if !own {
		query += " AND ie.kind IN ('reply','addressed','mention','wakeup')"
	}
	// A message entry: the message is there, and a mention-only one is not
	// hidden (no version of it).
	query += ` AND (ie.kind NOT IN ('reply','addressed','mention','conversation') OR EXISTS(SELECT 1 FROM events e WHERE e.id=ie.subject
 AND (ie.kind<>'mention' OR e.hidden=0 AND NOT EXISTS(SELECT 1 FROM events h WHERE h.id=e.origin AND h.hidden=1))))`
	order := "DESC"
	if after >= 0 {
		query += " AND ie.seq>?"
		args = append(args, after)
		order = "ASC"
	}
	// A wake-up entry is listed without its room and trigger when the room
	// is not readable; a request names the conversation the reader is asked
	// into, readable only once it accepts. Every other entry with a room
	// needs it readable.
	query = "SELECT * FROM (" + query + ") WHERE readable OR kind IN ('request','wakeup') ORDER BY seq " + order + " LIMIT ?"
	args = append(args, limit+1)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return page, err
	}
	var list []UpdateEntry
	for rows.Next() {
		var e UpdateEntry
		var reasons, detail string
		var readable bool
		if err = rows.Scan(&e.Seq, &e.ID, &e.Kind, &reasons, &e.Subject, &e.Room, &e.Actor, &detail, &e.NeedsAnswer, &e.Disposition, &e.CreatedAt, &readable); err != nil {
			rows.Close()
			return page, err
		}
		e.Reasons = strings.Split(reasons, ",")
		if !own {
			// How the agent triaged an entry is its own business.
			e.Disposition = ""
		}
		if !readable && e.Kind == inboxWakeup {
			e.Room, detail = "", withoutDetail(detail, "event")
		}
		if detail != "" {
			e.Detail = json.RawMessage(detail)
		}
		e.Stale = inboxStale(e.CreatedAt, now)
		list = append(list, e)
	}
	if err = closeRows(rows); err != nil {
		return page, err
	}
	if page.more = len(list) > limit; page.more {
		list = list[:limit]
	}
	if order == "DESC" {
		slices.Reverse(list)
		if err = tx.QueryRowContext(ctx, "SELECT coalesce(max(seq),0) FROM inbox_entries WHERE account=?", account).Scan(&page.next); err != nil {
			return page, err
		}
	} else {
		page.next = after
		if len(list) > 0 {
			page.next = list[len(list)-1].Seq
		}
	}
	page.entries = []UpdateEntry{}
	for _, e := range list {
		switch e.Kind {
		case inboxReceived:
			page.notices.Items = append(page.notices.Items, e.Subject)
		case inboxWakeup:
			if _, n, ok := strings.Cut(e.Subject, "@"); ok {
				if seq, err := strconv.ParseInt(n, 10, 64); err == nil {
					page.notices.Notices = append(page.notices.Notices, seq)
				}
			}
			if !own {
				continue
			}
		}
		page.entries = append(page.entries, e)
	}
	return page, nil
}

// withoutDetail is detail without key.
func withoutDetail(detail, key string) string {
	if detail == "" {
		return ""
	}
	var m map[string]any
	if json.Unmarshal([]byte(detail), &m) != nil {
		return ""
	}
	delete(m, key)
	return encodeInboxDetail(m)
}

// inboxMark is what a waiting read under INBOX_ENTRIES=read compares
// between commits: the newest message and the agent's newest entry. While
// neither moves, re-running a caught-up read would answer the same, so the
// wait goes on without it. ok is false when the read cannot be checked this
// way (another mode, no agent, no v2 cursor): it then re-runs on every
// commit, as before. Reads through the pool; the waiter holds no
// transaction.
type inboxMark struct {
	events, entries int64
	ok              bool
}

func (s *Store) inboxMarkFor(ctx context.Context, cmd Command) inboxMark {
	if !s.inboxRead(ctx) || cmd.Target == "" || s.cursorEntryPart(cmd.Cursor) < 0 {
		return inboxMark{}
	}
	var m inboxMark
	err := s.db.QueryRowContext(ctx, `SELECT (SELECT coalesce(max(seq),0) FROM events),
 (SELECT coalesce(max(seq),0) FROM inbox_entries WHERE account=coalesce((SELECT account FROM identities WHERE id=?),?))`, cmd.Target, cmd.Target).Scan(&m.events, &m.entries)
	m.ok = err == nil
	return m
}
