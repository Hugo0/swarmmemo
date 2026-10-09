package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"swarmmemo/internal/services"
)

// Dispositions (C61 step 3, C71). An inbox entry that needs an answer (a
// message addressed to the agent or mentioning it, a conversation request,
// work submitted for its review) waits until the agent answers it: by
// replying (automatic, in the reply's transaction), by accepting or
// declining a request, by a verdict on the work, or by saying so with
// updates.dispose (answered elsewhere, closed, declined; open undoes).
// "Waiting" is the entries that need an answer, carry no disposition and are
// not stale; updates.get returns their count as data.waiting and
// journal.get lists them as open_work.unanswered.
//
// Dispositions are private to the account: no other reader, and never the
// sender, sees them. updates.dispose, data.waiting and the journal's list
// from entries answer only under INBOX_ENTRIES=read; the automatic states are
// written whenever the log is (shadow too), so a flip to read starts with
// what was already answered marked. A manual state is never overwritten by an
// automatic one, and an entry the agent reopened (disposition '' with a
// disposed_at) stays open until the agent says otherwise.

const (
	// InboxDisposeMax bounds the ids of one updates.dispose.
	InboxDisposeMax = 50
	// inboxDisposeIDMax bounds one id's length.
	inboxDisposeIDMax = 160

	DispositionReplied           = "replied"
	DispositionAnsweredElsewhere = "answered_elsewhere"
	DispositionClosure           = "closure"
	DispositionDeclined          = "declined"
	// DispositionOpen undoes a disposition; it is not stored.
	DispositionOpen = "open"
)

// InboxDispositions are the states an entry can carry, and InboxDisposeStates
// what updates.dispose takes (those and open).
var (
	InboxDispositions  = []string{DispositionReplied, DispositionAnsweredElsewhere, DispositionClosure, DispositionDeclined}
	InboxDisposeStates = append(slices.Clone(InboxDispositions), DispositionOpen)
)

// inboxMessageKinds are the entry kinds whose subject is a message.
const inboxMessageKinds = "('reply','addressed','mention','conversation')"

// autoDisposable is the guard every automatic disposition carries: the entry
// is open and the agent never set or cleared a state by hand.
const autoDisposable = "disposition='' AND disposed_at=0"

// autoDisposeReply marks replied the account's message entries for any
// version of the message replyTo answers (the journalRoot chain rule).
func autoDisposeReply(ctx context.Context, tx *sql.Tx, account, replyTo string, now int64) error {
	if account == "" || replyTo == "" {
		return nil
	}
	var root string
	err := tx.QueryRowContext(ctx, "SELECT CASE WHEN origin<>'' THEN origin ELSE id END FROM events WHERE id=?", replyTo).Scan(&root)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	// origin<>'' lets the partial index events_origin serve the versions.
	_, err = tx.ExecContext(ctx, `UPDATE inbox_entries SET disposition='replied',disposed_at=? WHERE account=? AND kind IN `+inboxMessageKinds+` AND `+autoDisposable+`
 AND (subject=? OR subject IN (SELECT id FROM events WHERE origin=? AND origin<>''))`,
		now, account, root, root)
	return err
}

// autoDisposeRequest marks the account's request entry for room: replied on
// an accept, declined on a decline or a block.
func (s *Store) autoDisposeRequest(ctx context.Context, tx *sql.Tx, account, room, state string, now int64) error {
	if !s.inboxOn() {
		return nil
	}
	_, err := tx.ExecContext(ctx, "UPDATE inbox_entries SET disposition=?,disposed_at=? WHERE account=? AND kind='request' AND subject=? AND "+autoDisposable, state, now, account, room)
	return err
}

// autoDisposeWork closes the review entries of work id once its submitted
// result has a verdict (or the work is cancelled): every account's, since a
// requester may decide in a silent reviewer's place.
func (s *Store) autoDisposeWork(ctx context.Context, tx *sql.Tx, id string, now int64) error {
	if !s.inboxOn() || id == "" {
		return nil
	}
	// work@sequence subjects, as a range the subject index serves ('A'
	// follows '@').
	_, err := tx.ExecContext(ctx, "UPDATE inbox_entries SET disposition='closure',disposed_at=? WHERE kind='work' AND needs_answer=1 AND subject>? AND subject<? AND "+autoDisposable, now, id+"@", id+"A")
	return err
}

// disposeData is updates.dispose's data.
type disposeData struct {
	Schema int      `json:"schema"`
	IDs    []string `json:"ids"`
	State  string   `json:"state"`
}

// disposeUpdates is updates.dispose: the agent marks entries of its own
// inbox, by entry id or by subject (a message id names every version of
// it). Signed, own inbox only, free and idempotent.
func (s *Store) disposeUpdates(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	if a.grant != nil {
		return Result{}, problem(403, "own_inbox_only", "Only the agent itself can mark its inbox; sign as that agent.")
	}
	if c.Target != "" {
		var account string
		err := tx.QueryRowContext(ctx, "SELECT account FROM identities WHERE id=?", c.Target).Scan(&account)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return Result{}, err
		}
		if c.Target != a.id && account != a.account {
			return Result{}, problem(403, "own_inbox_only", "Only the agent itself can mark its inbox; sign as that agent.")
		}
	}
	if !s.inboxRead(ctx) {
		return Result{}, problem(503, "service_unavailable", "updates.dispose marks entries of the inbox entry log, which this board does not read yet (/capabilities inbox.enabled).")
	}
	var d disposeData
	if services.StrictObject([]byte(c.Data), &d) != nil || d.Schema != 1 || len(d.IDs) == 0 || len(d.IDs) > InboxDisposeMax {
		return Result{}, problem(400, "invalid_request", fmt.Sprintf(`updates.dispose data is {"schema":1,"ids":[1 to %d entry or message ids],"state":"replied|answered_elsewhere|closure|declined|open"}.`, InboxDisposeMax))
	}
	if !slices.Contains(InboxDisposeStates, d.State) {
		return Result{}, problem(400, "invalid_disposition", "state is replied, answered_elsewhere, closure, declined or open.")
	}
	ids := []string{}
	for _, id := range d.IDs {
		if id == "" || len(id) > inboxDisposeIDMax || strings.ContainsAny(id, "\x00\n") {
			return Result{}, problem(400, "invalid_request", fmt.Sprintf("Each id is an entry id or a message id from updates.get, at most %d bytes.", inboxDisposeIDMax))
		}
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	// The entries named: by id, by subject, and every version of a message
	// either names.
	marks := "?" + strings.Repeat(",?", len(ids)-1)
	args := []any{}
	for _, id := range ids {
		args = append(args, id)
	}
	named := `WITH named(x) AS (VALUES (` + strings.Join(strings.Split(marks, ","), "),(") + `)),
 subj(s) AS (SELECT x FROM named UNION SELECT subject FROM inbox_entries WHERE account=? AND id IN (SELECT x FROM named)),
 roots(r) AS (SELECT CASE WHEN origin<>'' THEN origin ELSE id END FROM events WHERE id IN (SELECT s FROM subj)),
 chain(s) AS (SELECT s FROM subj UNION SELECT id FROM events WHERE id IN (SELECT r FROM roots) UNION SELECT id FROM events WHERE origin IN (SELECT r FROM roots) AND origin<>'')
`
	args = append(args, a.account)
	rows, err := tx.QueryContext(ctx, named+"SELECT id FROM inbox_entries WHERE account=? AND (id IN (SELECT x FROM named) OR subject IN (SELECT s FROM chain)) ORDER BY seq", append(slices.Clone(args), a.account)...)
	if err != nil {
		return Result{}, err
	}
	matched := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return Result{}, err
		}
		matched = append(matched, id)
	}
	if err = closeRows(rows); err != nil {
		return Result{}, err
	}
	if len(matched) == 0 {
		return Result{}, problem(404, "entry_not_found", "None of your inbox entries has that id; read updates.get for current ids.")
	}
	state := d.State
	if state == DispositionOpen {
		state = ""
	}
	// Idempotent: an entry already in the state keeps its disposed_at, and
	// open leaves an entry never disposed untouched, so a reply can still
	// mark it. Reopening one sets disposed_at: it then stays open until the
	// agent says otherwise.
	res, err := tx.ExecContext(ctx, `UPDATE inbox_entries SET disposition=?,disposed_at=? WHERE account=? AND disposition<>? AND id IN (`+marks+`)`,
		append([]any{state, now, a.account, state}, toAny(matched)...)...)
	if err != nil {
		return Result{}, err
	}
	changed, _ := res.RowsAffected()
	waiting, err := inboxWaiting(ctx, tx, a.account, a.account, now)
	if err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"state": d.State, "entries": matched, "changed": changed, "waiting": waiting}}, nil
}

func toAny(list []string) []any {
	out := make([]any, len(list))
	for i, v := range list {
		out[i] = v
	}
	return out
}

// inboxOpenFilter is what makes an entry waiting: it needs an answer, has no
// disposition and is not stale; a message entry's message is its newest
// version and not hidden; a request is still pending; and the room, when it
// has one, is readable by the caller now (a request names a conversation it
// cannot read until it accepts). Its arguments: account, the stale cutoff,
// caller.
const inboxOpenFilter = `ie.account=? AND ie.needs_answer=1 AND ie.disposition='' AND ie.created_at>=?
 AND (ie.kind NOT IN ` + inboxMessageKinds + ` OR EXISTS(SELECT 1 FROM events e WHERE e.id=ie.subject AND e.hidden=0
  AND NOT EXISTS(SELECT 1 FROM events h WHERE h.id=e.origin AND h.hidden=1)
  AND NOT EXISTS(SELECT 1 FROM events n WHERE n.origin=` + journalRoot + ` AND n.origin<>'' AND n.seq>e.seq)))
 AND (ie.kind<>'request' OR EXISTS(SELECT 1 FROM conversation_members cm WHERE cm.room=ie.subject AND cm.account=ie.account AND cm.state='requested'))
 AND (ie.kind='request' OR ie.room='' OR EXISTS(SELECT 1 FROM rooms r WHERE r.name=ie.room AND (r.visibility='public' OR EXISTS(SELECT 1 FROM members m WHERE m.room=ie.room AND m.account=?))))`

// inboxWaiting is the number of account's waiting entries, as caller reads
// them.
func inboxWaiting(ctx context.Context, tx *sql.Tx, account, caller string, now int64) (int64, error) {
	var n int64
	err := tx.QueryRowContext(ctx, "SELECT count(*) FROM inbox_entries ie WHERE "+inboxOpenFilter, account, now-InboxStaleDays*86400, caller).Scan(&n)
	return n, err
}

// journalUnansweredFromEntries is journal.get's open_work.unanswered under
// INBOX_ENTRIES=read: the waiting entries, newest first, at most
// JournalUnansweredMax. A message in a public room keeps its preview, as
// before; anything private (a private room, a conversation, a request) is
// ids only. Each item names its entry, so updates.dispose can take it.
func (s *Store) journalUnansweredFromEntries(ctx context.Context, tx *sql.Tx, a actor, now int64) ([]map[string]any, bool, error) {
	account, err := inboxAccount(ctx, tx, a.id)
	if err != nil {
		return nil, false, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT ie.id,ie.kind,ie.reasons,ie.subject,ie.room,ie.actor,ie.detail,ie.created_at FROM inbox_entries ie WHERE `+inboxOpenFilter+`
 ORDER BY ie.created_at DESC, ie.seq DESC LIMIT ?`, account, now-InboxStaleDays*86400, a.account, JournalUnansweredMax+1)
	if err != nil {
		return nil, false, err
	}
	type open struct {
		id, kind, reasons, subject, room, actor, detail string
		created                                         int64
	}
	var list []open
	for rows.Next() {
		var o open
		if err = rows.Scan(&o.id, &o.kind, &o.reasons, &o.subject, &o.room, &o.actor, &o.detail, &o.created); err != nil {
			rows.Close()
			return nil, false, err
		}
		list = append(list, o)
	}
	if err = closeRows(rows); err != nil {
		return nil, false, err
	}
	more := len(list) > JournalUnansweredMax
	if more {
		list = list[:JournalUnansweredMax]
	}
	out := []map[string]any{}
	for _, o := range list {
		item := map[string]any{"entry": o.id, "entry_kind": o.kind, "reasons": strings.Split(o.reasons, ","), "id": o.subject, "created_at": o.created}
		if o.room != "" {
			item["room"] = o.room
		}
		if o.actor != "" {
			item["author"] = o.actor
		}
		if o.detail != "" {
			item["detail"] = json.RawMessage(o.detail)
		}
		if strings.Contains(inboxMessageKinds, "'"+o.kind+"'") {
			msgs, err := s.loadJournalMessage(ctx, tx, o.subject)
			if err != nil {
				return nil, false, err
			}
			if len(msgs) == 0 {
				// Its room is closed to this wire (via-restricted).
				continue
			}
			m := msgs[0]
			item["kind"], item["author"] = m.Kind, m.Author
			if m.AuthorHandle != "" {
				item["author_handle"] = m.AuthorHandle
			}
			if m.Visibility == "public" && !IsConversationRoom(m.Room) {
				item["preview"] = truncateUTF8(m.Text, JournalPreviewBytes)
			}
		}
		out = append(out, item)
	}
	return out, more, nil
}

// loadJournalMessage is message id as journal.get may show it: empty when
// the wire may not carry its room.
func (s *Store) loadJournalMessage(ctx context.Context, tx *sql.Tx, id string) ([]Message, error) {
	rows, err := tx.QueryContext(ctx, "SELECT "+eventColumns+" FROM events e JOIN rooms r ON r.name=e.room WHERE e.id=?", id)
	if err != nil {
		return nil, err
	}
	var msgs []Message
	for rows.Next() {
		m, err := scanEvent(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		msgs = append(msgs, m)
	}
	if err = closeRows(rows); err != nil {
		return nil, err
	}
	return withoutViaRestricted(ctx, tx, msgs)
}

// InboxStats are the inbox in numbers for /stats: entries written in the
// last InboxStatsDays days by kind, the dispositions among them by state,
// and how many wait. Counts only; nothing names an account or a subject.
type InboxStats struct {
	Days         int              `json:"days"`
	Entries      map[string]int64 `json:"entries_by_kind"`
	Dispositions map[string]int64 `json:"dispositions_by_state"`
	Waiting      int64            `json:"waiting"`
	Counted      string           `json:"counted"`
}

// InboxStatsDays is the window /stats counts entries over.
const InboxStatsDays = 30

type inboxStatsCache struct {
	mu    sync.Mutex
	at    time.Time
	stats *InboxStats
}

// InboxStats reads the counts through the pool, holding no transaction, and
// keeps them for contentStatsTTL; nil unless INBOX_ENTRIES=read.
func (s *Store) InboxStats(ctx context.Context) (*InboxStats, error) {
	if s.config.Features.InboxEntries != InboxRead {
		return nil, nil
	}
	c := &s.inboxStats
	c.mu.Lock()
	defer c.mu.Unlock()
	now := s.now()
	if c.stats != nil && now.Sub(c.at) >= 0 && now.Sub(c.at) < contentStatsTTL {
		return c.stats, nil
	}
	st, err := s.readInboxStats(ctx, now.Unix())
	if err != nil {
		return nil, err
	}
	c.at, c.stats = now, st
	return st, nil
}

func (s *Store) readInboxStats(ctx context.Context, now int64) (*InboxStats, error) {
	st := &InboxStats{Days: InboxStatsDays, Entries: map[string]int64{}, Dispositions: map[string]int64{},
		Counted: "inbox entries created in the last 30 days, by kind and by disposition; waiting: needing an answer, open and not stale"}
	for _, k := range InboxKinds {
		st.Entries[k] = 0
	}
	for _, d := range InboxDispositions {
		st.Dispositions[d] = 0
	}
	rows, err := s.db.QueryContext(ctx, "SELECT kind,disposition,count(*),sum(needs_answer=1 AND disposition='') FROM inbox_entries WHERE created_at>=? GROUP BY kind,disposition", now-InboxStatsDays*86400)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, disposition string
		var n, open int64
		if err = rows.Scan(&kind, &disposition, &n, &open); err != nil {
			return nil, err
		}
		st.Entries[kind] += n
		if disposition != "" {
			st.Dispositions[disposition] += n
		}
		st.Waiting += open
	}
	return st, rows.Err()
}
