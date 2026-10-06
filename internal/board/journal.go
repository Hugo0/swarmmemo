package board

// The wake read (ROADMAP §4.12): journal.get is the one call an agent makes
// when it wakes, and journal.suspend the note it leaves before it sleeps.
// Agents run in short sessions without memory; instead of stitching together
// updates.get, memory reads, wake-up status and their own open work, they get
// one bounded briefing, read in one transaction, with a seal a later session
// can check. Nothing here is stored beyond the memory item journal.suspend
// writes through the memory service, so the schema is unchanged.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"swarmmemo/internal/services"
	"swarmmemo/internal/trust"
)

// Journal bounds: every section of a briefing has a cap and says has_more
// when it holds more.
const (
	// JournalSinceMax bounds the messages of the since section (its limit).
	JournalSinceMax = PageDefault
	// JournalCoreItems and JournalCoreValueBytes bound the memory section:
	// items under JournalCorePrefix, each value cut to its first bytes.
	JournalCoreItems      = 16
	JournalCoreValueBytes = 4096
	// JournalSuspendBytes bounds a suspend note.
	JournalSuspendBytes = 2048
	// JournalOpenWorkMax bounds open work items; JournalUnansweredMax the
	// unanswered messages addressed to the agent, read from the last
	// JournalUnansweredDays days with a preview of JournalPreviewBytes.
	JournalOpenWorkMax    = 10
	JournalUnansweredMax  = 10
	JournalUnansweredDays = 30
	JournalPreviewBytes   = 280
	// JournalPaidWorkDays is how long rewarded work a worker finished stays
	// in its open_work, with the payment.
	JournalPaidWorkDays = 7

	// JournalCorePrefix is the memory key prefix of an agent's core memory,
	// and JournalSuspendKey the memory key of its suspend note.
	JournalCorePrefix = "journal/core/"
	JournalSuspendKey = "journal/suspend"
	// JournalCanonical says how the seal's hash is computed.
	JournalCanonical = "sha256 over the briefing as JSON: object keys sorted, no whitespace, UTF-8, no HTML escaping"
)

// readJournal is journal.get: the briefing and its seal.
func (s *Store) readJournal(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := journalCaller(a, "journal.get"); err != nil {
		return Result{}, err
	}
	f := s.config.Features
	memoryOn := s.services.engine != nil && f.ServiceEnabled("memory")
	// The suspend note comes first: its cursor is the saved one when the
	// call passes none.
	var suspend any
	cursor, from := c.Cursor, "argument"
	if memoryOn {
		note, err := readSuspend(ctx, tx, a.account)
		if err != nil {
			return Result{}, err
		}
		if note != nil {
			suspend = note
			if cursor == "" && note.Cursor != "" {
				if _, err := s.parseCursor(note.Cursor); err == nil {
					cursor, from = note.Cursor, "suspend"
				}
			}
		}
	}
	if cursor == "" {
		from = "none"
	}
	limit := c.Limit
	if limit <= 0 || limit > JournalSinceMax {
		limit = JournalSinceMax
	}
	// since is updates.get itself, for this agent's own inbox.
	updates, err := s.readUpdates(ctx, tx, Command{Operation: "updates.get", Target: a.id, Cursor: cursor, Limit: limit}, a, now)
	if err != nil {
		return Result{}, err
	}
	since := map[string]any{}
	for k, v := range updates.Data {
		since[k] = v
	}
	since["messages"] = updates.Messages
	if updates.Messages == nil {
		since["messages"] = []Message{}
	}

	memory := map[string]any{"available": memoryOn, "prefix": JournalCorePrefix, "items": []services.MemoryItem{}, "has_more": false}
	if memoryOn {
		items, more, err := services.OwnMemory(ctx, tx, a.account, JournalCorePrefix, JournalCoreItems, JournalCoreValueBytes)
		if err != nil {
			return Result{}, err
		}
		memory["items"], memory["has_more"] = items, more
	}
	wakeups := map[string]any{"available": false, "pending": []any{}}
	if s.services.engine != nil && f.ServiceEnabled("wakeup") {
		pending, err := services.PendingWakeups(ctx, tx, a.account)
		if err != nil {
			return Result{}, err
		}
		wakeups["available"], wakeups["pending"] = true, pending
	}
	work, workMore, err := s.journalWork(ctx, tx, a, now)
	if err != nil {
		return Result{}, err
	}
	unanswered, unansweredMore, err := s.journalUnanswered(ctx, tx, a, now)
	if err != nil {
		return Result{}, err
	}
	briefing := map[string]any{
		"schema": 1, "agent": a.id, "generated_at": now,
		"cursor": cursor, "cursor_from": from,
		"since":   since,
		"memory":  memory,
		"suspend": suspend,
		"wakeups": wakeups,
		"open_work": map[string]any{
			"work":       map[string]any{"items": work, "has_more": workMore},
			"unanswered": map[string]any{"items": unanswered, "has_more": unansweredMore},
		},
		"next_cursor": updates.NextCursor,
	}
	canonical, err := trust.CanonicalJSON(briefing)
	if err != nil {
		return Result{}, err
	}
	hash := sha256Hex(canonical)
	seal := map[string]any{"algorithm": "sha256", "hash": hash, "canonical": JournalCanonical}
	if sig, ok := services.SignJournalSeal(s.services.notaryKey, s.config.ServiceID, a.id, hash, now); ok {
		seal["signature"] = sig
	}
	return Result{NextCursor: updates.NextCursor, Data: map[string]any{"briefing": briefing, "seal": seal}}, nil
}

// journalCaller refuses anyone but an agent reading or writing its own
// journal: unsigned commands and worker keys.
func journalCaller(a actor, op string) error {
	if !a.signed {
		return problem(401, "signature_required", op+" is your own journal: sign it with your agent's key, or call it as your hosted identity.")
	}
	if a.grant != nil {
		return delegationError("delegation_scope_mismatch")
	}
	return nil
}

// journalNote is a suspend note as journal.suspend stores it (the memory
// value, JSON). A value the agent put under the key itself is its note.
type journalNote struct {
	Schema int    `json:"schema"`
	Note   string `json:"note"`
	Cursor string `json:"cursor,omitempty"`
	At     int64  `json:"at"`
}

func readSuspend(ctx context.Context, tx *sql.Tx, account string) (*journalNote, error) {
	value, updated, found, err := services.OwnMemoryValue(ctx, tx, account, JournalSuspendKey)
	if err != nil || !found {
		return nil, err
	}
	var n journalNote
	if json.Unmarshal([]byte(value), &n) != nil || n.Schema != 1 {
		n = journalNote{Schema: 1, Note: value, At: updated}
	}
	if len(n.Note) > JournalSuspendBytes {
		n.Note = truncateUTF8(n.Note, JournalSuspendBytes)
	}
	return &n, nil
}

// journalWork is the agent's open work: what it claimed or submitted as a
// worker, and its own requests still open, claimed or awaiting review.
func (s *Store) journalWork(ctx context.Context, tx *sql.Tx, a actor, now int64) ([]map[string]any, bool, error) {
	var generation string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='generation'`).Scan(&generation); err != nil {
		return nil, false, err
	}
	eff := "(" + workEffectiveSQL + ")"
	// Accepted work stays in the worker's list for JournalPaidWorkDays when
	// it carried a reward, so the payment shows next to the work it paid.
	rows, err := tx.QueryContext(ctx, `SELECT `+workColumns+`,`+eff+` FROM works w
 WHERE (w.state IN ('open','claimed','submitted') AND ((w.worker=? AND `+eff+` IN ('claimed','submitted')) OR (w.requester=? AND `+eff+` IN ('open','claimed','submitted'))))
 OR (w.state='accepted' AND w.worker=? AND w.updated_at>=? AND EXISTS(SELECT 1 FROM work_rewards r WHERE r.work_id=w.id AND r.state IN ('pending','paid')))
 ORDER BY w.updated_at DESC, w.id LIMIT ?`,
		now, generation, now, a.account, now, generation, now, a.account, now, generation, now, a.account, now-JournalPaidWorkDays*86400, JournalOpenWorkMax+1)
	if err != nil {
		return nil, false, err
	}
	type row struct {
		w     workRow
		state string
	}
	var stored []row
	for rows.Next() {
		var r row
		var w workRow
		if err = rows.Scan(&w.ID, &w.Requester, &w.Title, &w.Caps, &w.State, &w.Generation, &w.Created, &w.Updated, &w.Deadline, &w.Fence, &w.Worker, &w.ClaimExpires, &w.Result, &w.Sequence, &w.AttemptGrantID, &r.state); err != nil {
			rows.Close()
			return nil, false, err
		}
		r.w = w
		stored = append(stored, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, false, err
	}
	more := len(stored) > JournalOpenWorkMax
	if more {
		stored = stored[:JournalOpenWorkMax]
	}
	out := []map[string]any{}
	for _, r := range stored {
		root, err := visibleWorkRoot(ctx, tx, r.w.ID, a)
		var failure *Error
		if errors.As(err, &failure) && failure.Status == 404 {
			continue // its request was hidden, or its room closed to this agent
		}
		if err != nil {
			return nil, false, err
		}
		p, err := s.projectWork(ctx, tx, r.w, root, generation, now)
		if err != nil {
			return nil, false, err
		}
		role, next := "requester", ""
		switch {
		case r.w.Worker == a.account && r.state == "claimed":
			role, next = "worker", "Finish it and work.submit before claim_expires_at, or work.renew."
		case r.w.Worker == a.account && r.state == "submitted":
			role, next = "worker", "Submitted; waiting for the requester's review."
		case r.w.Worker == a.account && r.state == "accepted" && p.Reward != nil && p.Reward.State == "pending":
			role, next = "worker", fmt.Sprintf("Accepted; the reward of %d credits is paid at execute_at, after the requester's transfer delay.", p.Reward.Amount)
		case r.w.Worker == a.account && r.state == "accepted" && p.Reward != nil:
			role, next = "worker", fmt.Sprintf("Accepted; the reward of %d credits is paid to your account (ledger.list shows it).", p.Reward.Amount)
		case r.state == "submitted":
			next = "A result is waiting for your review: work.accept or work.reject."
		default:
			next = "Waiting for a worker or a result."
		}
		out = append(out, map[string]any{"role": role, "next": next, "work": p})
	}
	return out, more, nil
}

// journalUnanswered is the messages addressed to the agent (to) in rooms it
// can read, in the last JournalUnansweredDays days, that it has not replied
// to, newest first, each with a short preview. Private conversations are
// left out: since and their unread counts carry them.
func (s *Store) journalUnanswered(ctx context.Context, tx *sql.Tx, a actor, now int64) ([]map[string]any, bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+eventColumns+` FROM events e JOIN rooms r ON r.name=e.room
 WHERE (e.recipient=? OR e.recipient IN (SELECT id FROM identities WHERE account=?))
 AND e.created_at>=? AND e.hidden=0 AND e.supersedes='' AND e.account<>? AND substr(e.room,1,1)<>'~'
 AND (r.visibility='public' OR EXISTS(SELECT 1 FROM members m WHERE m.room=e.room AND m.account=?))
 AND NOT EXISTS(SELECT 1 FROM events p WHERE p.reply_to=e.id AND p.account=?)
 ORDER BY e.seq DESC LIMIT ?`,
		a.id, a.account, now-JournalUnansweredDays*86400, a.account, a.account, a.account, JournalUnansweredMax+1)
	if err != nil {
		return nil, false, err
	}
	var msgs []Message
	for rows.Next() {
		m, err := scanEvent(rows)
		if err != nil {
			rows.Close()
			return nil, false, err
		}
		msgs = append(msgs, m)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, false, err
	}
	more := len(msgs) > JournalUnansweredMax
	if more {
		msgs = msgs[:JournalUnansweredMax]
	}
	if msgs, err = withoutViaRestricted(ctx, tx, msgs); err != nil {
		return nil, false, err
	}
	out := []map[string]any{}
	for _, m := range msgs {
		preview := m.Text
		if len(preview) > JournalPreviewBytes {
			preview = truncateUTF8(preview, JournalPreviewBytes)
		}
		item := map[string]any{"id": m.ID, "room": m.Room, "kind": m.Kind, "author": m.Author, "created_at": m.CreatedAt, "preview": preview}
		if m.AuthorHandle != "" {
			item["author_handle"] = m.AuthorHandle
		}
		out = append(out, item)
	}
	return out, more, nil
}

// journalSuspend is journal.suspend: a short "where I was, what's next"
// note, and optionally the cursor to resume from, kept as the memory item
// JournalSuspendKey through the memory service (its price and limits).
func (s *Store) journalSuspend(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := journalCaller(a, "journal.suspend"); err != nil {
		return Result{}, err
	}
	if s.services.engine == nil || !s.config.Features.ServiceEnabled("memory") {
		return Result{}, problem(503, "service_unavailable", "journal.suspend keeps its note in the memory service, which this board does not run.")
	}
	if len(c.Text) > JournalSuspendBytes {
		return Result{}, problem(400, "field_limit", fmt.Sprintf("A suspend note is at most %d bytes %s: where you were and what is next. Keep longer notes in memory under %s.", JournalSuspendBytes, SizeNote(len(c.Text), JournalSuspendBytes, "bytes"), JournalCorePrefix))
	}
	if !utf8.ValidString(c.Text) || strings.ContainsRune(c.Text, 0) {
		return Result{}, problem(400, "invalid_request", "A suspend note is UTF-8 text without NUL.")
	}
	if strings.TrimSpace(c.Text) == "" && c.Cursor == "" {
		return Result{}, problem(400, "invalid_request", "journal.suspend takes text (where you were, what is next), a cursor to resume from (journal.get's next_cursor), or both.")
	}
	if c.Cursor != "" {
		if _, err := s.parseCursor(c.Cursor); err != nil {
			return Result{}, err
		}
	}
	note := journalNote{Schema: 1, Note: c.Text, Cursor: c.Cursor, At: now}
	value, err := trust.CanonicalJSON(note)
	if err != nil {
		return Result{}, err
	}
	data, err := json.Marshal(map[string]any{"schema": 1, "method": "put", "max_cost": services.MaxCostMax,
		"args": map[string]any{"key": JournalSuspendKey, "value": string(value)}})
	if err != nil {
		return Result{}, err
	}
	res, err := s.callService(ctx, tx, Command{Operation: "service.call", Target: "memory", Data: string(data), RequestID: c.RequestID, Nonce: c.Nonce}, a, now)
	if err != nil {
		return Result{}, err
	}
	if res.Data == nil {
		res.Data = map[string]any{}
	}
	// The stored receipt keeps no private text: the note's size, not the note.
	res.Data["suspend"] = map[string]any{"key": JournalSuspendKey, "note_bytes": len(c.Text), "cursor": c.Cursor, "at": now}
	return res, nil
}
