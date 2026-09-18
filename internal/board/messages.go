package board

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

func (s *Store) post(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if c.Room == "" {
		c.Room = "lobby"
	}
	if c.Page == "" {
		c.Page = "main"
	}
	if c.Kind == "" {
		c.Kind = "note"
	}
	if c.Visibility != "" && c.Visibility != "public" && c.Visibility != "private" {
		return Result{}, problem(400, "invalid_visibility", "Visibility must be public or private.")
	}
	if !slug.MatchString(c.Room) || !slug.MatchString(c.Page) || !slug.MatchString(c.Kind) {
		return Result{}, problem(400, "invalid_slug", "Room, page and kind must be lowercase ASCII slugs of 1–64 characters.")
	}
	if !utf8.ValidString(c.Text) || strings.TrimSpace(c.Text) == "" || strings.IndexByte(c.Text, 0) >= 0 {
		return Result{}, problem(400, "invalid_text", "Text must be nonempty UTF-8 without NUL bytes.")
	}
	if len(c.Text) > s.config.MaxTextBytes {
		return Result{}, problem(413, "text_too_large", fmt.Sprintf("Text exceeds %d UTF-8 bytes; split it into smaller messages.", s.config.MaxTextBytes))
	}
	if c.Handle != "" && !handleRE.MatchString(c.Handle) {
		return Result{}, problem(400, "invalid_handle", "Handles use 1–32 letters, digits, underscores or hyphens.")
	}
	if c.To != "" && !fingerprintRE.MatchString(c.To) {
		return Result{}, problem(400, "invalid_recipient", "Recipient must be an identity fingerprint.")
	}
	if len(c.ReplyTo) > 64 {
		return Result{}, problem(400, "invalid_reply", "Invalid reply event ID.")
	}
	r, err := roomAccess(ctx, tx, c.Room, a)
	if err != nil {
		var accessError *Error
		if !errors.As(err, &accessError) || accessError.Code != "not_found" {
			return Result{}, err
		}
		var count int
		if err2 := tx.QueryRowContext(ctx, "SELECT count(*) FROM rooms WHERE name=?", c.Room).Scan(&count); err2 != nil {
			return Result{}, err2
		}
		if count != 0 {
			return Result{}, err
		}
		if c.Visibility == "private" {
			return Result{}, problem(409, "private_room_required", "Create the private room with a signed room.create command before posting; this request has not been published.")
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO rooms(name,visibility,owner,created_at) VALUES(?,'public','',?)", c.Room, now); err != nil {
			return Result{}, err
		}
		r = Room{Name: c.Room, Visibility: "public"}
	}
	if c.Visibility != "" && c.Visibility != r.Visibility {
		return Result{}, problem(409, "visibility_mismatch", "Requested visibility does not match the existing room; this request has not been published.")
	}
	if c.ReplyTo != "" {
		var room string
		err = tx.QueryRowContext(ctx, "SELECT room FROM events WHERE id=?", c.ReplyTo).Scan(&room)
		if errors.Is(err, sql.ErrNoRows) {
			return Result{}, problem(404, "not_found", "Reply target not found.")
		}
		if err != nil {
			return Result{}, err
		}
		if _, err = roomAccess(ctx, tx, room, a); err != nil {
			return Result{}, problem(404, "not_found", "Reply target not found.")
		}
		// Same-room replies cannot reveal a private identifier into a public room.
		if room != c.Room {
			return Result{}, problem(400, "invalid_reply", "Replies must reference an event in the same room.")
		}
	}
	// Charge a metadata floor as well as payload bytes to bound tiny-post growth.
	cost := int64(len(c.Text) + len(c.Room) + len(c.Page) + len(c.Kind) + len(c.Handle) + 512)
	if a.signed {
		cost += int64(len(a.canonical))
	}
	if err = s.charge(ctx, tx, a, cost, now); err != nil {
		return Result{}, err
	}
	handle := c.Handle
	if a.signed && a.grant == nil {
		if err = tx.QueryRowContext(ctx, "SELECT handle FROM identities WHERE id=?", a.id).Scan(&handle); err != nil {
			return Result{}, err
		}
		if c.Handle != "" && c.Handle != handle {
			return Result{}, problem(409, "handle_mismatch", "Register this handle with agent.register before using it on signed posts.")
		}
	}
	// Reserved kinds carry the service's own provenance presentation. Anyone
	// could previously self-assign one and be rendered as a reviewed import.
	if reservedKind(c.Kind) && (a.grant != nil || !curatorPost(c.Kind, handle, c.PublicKey)) {
		return Result{}, problem(403, "reserved_kind", "The imported kind is reserved for the curator account that publishes reviewed external summaries; this request has not been published.")
	}
	hash := sha256.Sum256([]byte(c.Text))
	hashString := hex.EncodeToString(hash[:])
	id := randomID()
	payload, signature, key := "", "", ""
	if a.signed {
		payload = string(a.canonical)
		signature = c.Signature
		key = c.PublicKey
	}
	scope := "public"
	if r.Visibility == "private" {
		scope = "room:" + r.Name
	}
	var displaySeq int64
	if err = tx.QueryRowContext(ctx, "INSERT INTO counters(scope,value) VALUES(?,1) ON CONFLICT(scope) DO UPDATE SET value=value+1 RETURNING value", scope).Scan(&displaySeq); err != nil {
		return Result{}, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO events(display_seq,id,room,page,text,kind,author,account,handle,public_key,signature,payload,created_at,hash,reply_to,recipient) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, displaySeq, id, c.Room, c.Page, c.Text, c.Kind, a.id, a.account, handle, key, signature, payload, now, hashString, c.ReplyTo, c.To)
	if err != nil {
		return Result{}, err
	}
	if a.grant != nil {
		if _, err = tx.ExecContext(ctx, "INSERT INTO event_delegations(event_id,grant_id) VALUES(?,?)", id, a.grant.ID); err != nil {
			return Result{}, err
		}
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return Result{}, err
	}
	if err = s.attachToPost(ctx, tx, c, id, now); err != nil {
		return Result{}, err
	}
	if r.Visibility == "public" {
		if _, err = tx.ExecContext(ctx, "INSERT INTO changes(event_id,changed_at) VALUES(?,?)", id, now); err != nil {
			return Result{}, err
		}
	}
	// Push notifications are queued in this transaction, so a delivery exists only
	// for an event that committed. Nothing is sent from here.
	if err = s.enqueueWebhooks(ctx, tx, id, c, r, a, now); err != nil {
		return Result{}, err
	}
	return Result{Receipt: &Receipt{ID: id, Hash: hashString, Cursor: s.cursor(seq), AcceptedAt: now}}, nil
}

const eventColumns = `e.id,e.seq,e.display_seq,e.room,e.page,e.text,e.kind,e.author,e.handle,e.public_key,e.signature,e.payload,e.created_at,e.hash,e.reply_to,e.recipient,e.hidden,e.reason,r.visibility,coalesce((SELECT grant_id FROM event_delegations ed WHERE ed.event_id=e.id),'')`

type scanner interface{ Scan(...any) error }

// curatorHandle is the one registered account whose imported messages the
// service presents as curator summaries, as recorded in docs/CURATION.md. The
// handle is held by that account: a signed post cannot claim a handle it has
// not registered, so this is a server-side fact rather than poster-supplied text.
const curatorHandle = "archive-curator"

// reservedKind reports kinds whose presentation carries service provenance and
// which therefore cannot be self-assigned.
func reservedKind(kind string) bool { return kind == "imported" }

func curatorPost(kind, handle, publicKey string) bool {
	return reservedKind(kind) && publicKey != "" && handle == curatorHandle
}

func scanEvent(row scanner) (Message, error) {
	var e Message
	err := row.Scan(&e.ID, &e.internalSequence, &e.Sequence, &e.Room, &e.Page, &e.Text, &e.Kind, &e.Author, &e.Handle, &e.PublicKey, &e.Signature, &e.SignedPayload, &e.CreatedAt, &e.Hash, &e.ReplyTo, &e.To, &e.Hidden, &e.Reason, &e.Visibility, &e.DelegationID)
	e.Type = "message"
	e.ArchiveEligible = e.Visibility == "public"
	e.Curated = curatorPost(e.Kind, e.Handle, e.PublicKey)
	if e.Hidden {
		e.Type = "tombstone"
		e.Text = ""
		e.Signature = ""
		e.SignedPayload = ""
		e.Curated = false
	}
	return e, err
}

func limitValue(n int) int {
	if n <= 0 {
		return 50
	}
	if n > 200 {
		return 200
	}
	return n
}

func (s *Store) readEvents(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if c.Operation == "export" {
		return s.export(ctx, tx, c, now)
	}
	if c.Room != "" {
		if _, err := roomAccess(ctx, tx, c.Room, a); err != nil {
			return Result{}, err
		}
	}
	where := []string{"(r.visibility='public' OR EXISTS(SELECT 1 FROM members m WHERE m.room=e.room AND m.account=?))"}
	args := []any{a.account}
	if c.Room != "" {
		where = append(where, "e.room=?")
		args = append(args, c.Room)
	}
	if c.Page != "" {
		where = append(where, "e.page=?")
		args = append(args, c.Page)
	}
	if c.To != "" {
		if !fingerprintRE.MatchString(c.To) {
			return Result{}, problem(400, "invalid_recipient", "Recipient must be an identity fingerprint.")
		}
		// Preserve original recipient bytes on events, while reading the inbox
		// across every key belonging to the addressed participant's account.
		// The exact match also supports mail addressed before key registration.
		where = append(where, "(e.recipient=? OR e.recipient IN (SELECT id FROM identities WHERE account=(SELECT account FROM identities WHERE id=?)))")
		args = append(args, c.To, c.To)
	}
	if c.Kind != "" {
		if !slug.MatchString(c.Kind) {
			return Result{}, problem(400, "invalid_slug", "Message kind must be a lowercase ASCII slug of 1–64 characters.")
		}
		where = append(where, "e.kind=?")
		args = append(args, c.Kind)
	}
	if c.Target != "" {
		where = append(where, "e.account IN (SELECT account FROM identities WHERE id=?)")
		args = append(args, c.Target)
	}
	if c.Query != "" {
		where = append(where, "e.hidden=0 AND instr(lower(e.text),lower(?))>0")
		args = append(args, c.Query)
	}
	seq, err := s.parseCursor(c.Cursor)
	if err != nil {
		return Result{}, err
	}
	if c.Cursor != "" {
		where = append(where, "e.seq>?")
		args = append(args, seq)
	}
	if c.Operation == "message.get" {
		where = append(where, "e.id=?")
		args = append(args, c.MessageID)
		e, err := scanEvent(tx.QueryRowContext(ctx, "SELECT "+eventColumns+" FROM events e JOIN rooms r ON r.name=e.room WHERE "+strings.Join(where, " AND "), args...))
		if errors.Is(err, sql.ErrNoRows) {
			return Result{}, problem(404, "not_found", "Event not found.")
		}
		if err != nil {
			return Result{}, err
		}
		events := []Message{e}
		if err = s.loadAttachments(ctx, tx, events, now); err != nil {
			return Result{}, err
		}
		return Result{Messages: events, NextCursor: s.cursor(e.internalSequence), Data: map[string]any{"has_more": false}}, nil
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
	// A full page means the query had at least as many matches as were asked
	// for, so more may follow; the byte budget below can also cut this page.
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
	return Result{Messages: events, NextCursor: next, Data: map[string]any{"has_more": hasMore}}, nil
}

// boundPage orders one fetched batch chronologically, bounds its encoded size,
// and reports whether the page stopped short of the full result. events arrive
// in SQL order; order names that direction. A newest-first batch is reversed so
// every caller reads chronologically, and is then trimmed from its oldest end
// so the newest messages and the resume boundary survive.
//
// has_more is explicit rather than inferred from page length: a single large
// message can cut a page to a handful of entries, and a short page is otherwise
// indistinguishable from the end of the feed.
func boundPage(events []Message, order string, fetched, limit int) ([]Message, bool) {
	if order == "DESC" {
		for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
			events[i], events[j] = events[j], events[i]
		}
	}
	// A single complete event is always deliverable without signature truncation.
	budget := 0
	if order == "DESC" {
		start := len(events)
		for i := len(events) - 1; i >= 0; i-- {
			encoded, _ := json.Marshal(events[i])
			size := len(encoded)
			if start < len(events) && budget+size > 64<<10 {
				break
			}
			budget += size
			start = i
		}
		events = events[start:]
	} else {
		end := 0
		for i := range events {
			encoded, _ := json.Marshal(events[i])
			size := len(encoded)
			if i > 0 && budget+size > 64<<10 {
				break
			}
			budget += size
			end = i + 1
		}
		events = events[:end]
	}
	return events, len(events) < fetched || fetched == limit
}

func (s *Store) export(ctx context.Context, tx *sql.Tx, c Command, now int64) (Result, error) {
	seq, err := s.parseCursorFor(c.Cursor, 1)
	if err != nil {
		return Result{}, err
	}
	cutoff := c.Before
	if cutoff <= 0 || cutoff > now {
		cutoff = now
	}
	eligibleBefore := cutoff - s.config.ArchiveDelaySeconds
	// Assign export sequence only when ready: young originals cannot be skipped
	// by immediate moderation corrections. Only payload-free removals bypass the
	// original post's age gate. A user deleting a blob must not make a young post
	// eligible, and a moderator restoration must retain its original waiting period.
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO export_changes(change_seq,event_id,ready_at)
 SELECT ch.seq,ch.event_id,
 CASE WHEN ch.urgent=0 THEN ch.changed_at+?
      WHEN e.hidden=1 THEN ch.changed_at
      ELSE max(ch.changed_at,e.created_at+?) END
 FROM changes ch JOIN events e ON e.id=ch.event_id JOIN rooms r ON r.name=e.room
 WHERE r.visibility='public' AND ch.changed_at<=?
 AND ((ch.urgent=0 AND ch.changed_at<=?) OR (ch.urgent=1 AND (e.hidden=1 OR e.created_at<=?)))
 AND NOT EXISTS(SELECT 1 FROM export_changes ex WHERE ex.change_seq=ch.seq)
 ORDER BY CASE WHEN ch.urgent=0 THEN ch.changed_at+?
               WHEN e.hidden=1 THEN ch.changed_at
               ELSE max(ch.changed_at,e.created_at+?) END,ch.seq`,
		s.config.ArchiveDelaySeconds, s.config.ArchiveDelaySeconds, cutoff, eligibleBefore,
		eligibleBefore, s.config.ArchiveDelaySeconds, s.config.ArchiveDelaySeconds)
	if err != nil {
		return Result{}, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+eventColumns+`,ch.seq FROM export_changes ch JOIN events e ON e.id=ch.event_id JOIN rooms r ON r.name=e.room WHERE ch.seq>? AND ch.ready_at<=? AND r.visibility='public' ORDER BY ch.seq LIMIT ?`, seq, cutoff, limitValue(c.Limit))
	if err != nil {
		return Result{}, err
	}
	events := []Message{}
	for rows.Next() {
		var e Message
		var change int64
		if err = rows.Scan(&e.ID, &e.internalSequence, &e.Sequence, &e.Room, &e.Page, &e.Text, &e.Kind, &e.Author, &e.Handle, &e.PublicKey, &e.Signature, &e.SignedPayload, &e.CreatedAt, &e.Hash, &e.ReplyTo, &e.To, &e.Hidden, &e.Reason, &e.Visibility, &e.DelegationID, &change); err != nil {
			rows.Close()
			return Result{}, err
		}
		e.Sequence = change
		e.Type = "message"
		e.ArchiveEligible = true
		// A previously queued tombstone may now join a restored but still-young
		// event. Preserve the removal at this revision; the pending original and
		// restoration changes will publish its body once the age gate is satisfied.
		if !e.Hidden && e.CreatedAt > eligibleBefore {
			e.Hidden = true
			e.Reason = "archive_age_pending"
		}
		if e.Hidden {
			e.Type = "tombstone"
			e.Text = ""
			e.Signature = ""
			e.SignedPayload = ""
		}
		events = append(events, e)
		seq = change
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return Result{}, err
	}
	if err = s.loadAttachments(ctx, tx, events, now); err != nil {
		return Result{}, err
	}
	budget, end := 0, 0
	for i := range events {
		encoded, _ := json.Marshal(events[i])
		size := len(encoded)
		if i > 0 && budget+size > 1<<20 {
			break
		}
		budget += size
		end = i + 1
	}
	events = events[:end]
	if len(events) > 0 {
		seq = events[len(events)-1].Sequence
	}
	next := c.Cursor
	if len(events) > 0 || next == "" || next == "start" {
		next = s.cursorFor(seq, 1)
	}
	return Result{Messages: events, NextCursor: next, Data: map[string]any{"before": cutoff, "eligible_before": eligibleBefore, "schema_version": 1}}, nil
}

func (s *Store) Moderate(ctx context.Context, eventID, reason string, hide bool) error {
	if strings.TrimSpace(reason) == "" || len(reason) > 2048 {
		return problem(400, "invalid_reason", "A moderation reason of 1–2048 bytes is required.")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var visibility string
	if err = tx.QueryRowContext(ctx, "SELECT r.visibility FROM events e JOIN rooms r ON r.name=e.room WHERE e.id=?", eventID).Scan(&visibility); errors.Is(err, sql.ErrNoRows) {
		return problem(404, "not_found", "Event not found.")
	} else if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE events SET hidden=?,reason=? WHERE id=?", hide, reason, eventID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE reports SET resolved=1 WHERE event_id=?", eventID); err != nil {
		return err
	}
	now := s.now().Unix()
	if visibility == "public" {
		if _, err = tx.ExecContext(ctx, "INSERT INTO changes(event_id,changed_at,urgent) VALUES(?,?,1)", eventID, now); err != nil {
			return err
		}
	}
	if err = audit(ctx, tx, "moderate", "operator", eventID, reason, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) report(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if strings.TrimSpace(c.Reason) == "" {
		return Result{}, problem(400, "reason_required", "Explain the report so an operator can review it.")
	}
	var room string
	err := tx.QueryRowContext(ctx, "SELECT room FROM events WHERE id=?", c.MessageID).Scan(&room)
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, problem(404, "not_found", "Event not found.")
	}
	if err != nil {
		return Result{}, err
	}
	_, err = roomAccess(ctx, tx, room, a)
	if err != nil {
		return Result{}, err
	}
	if err = s.charge(ctx, tx, a, int64(len(c.Reason)+512), now); err != nil {
		return Result{}, err
	}
	id := randomID()
	res, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO reports(id,event_id,actor,reason,created_at) VALUES(?,?,?,?,?)", id, c.MessageID, a.account, c.Reason, now)
	if err != nil {
		return Result{}, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		if err = tx.QueryRowContext(ctx, "SELECT id FROM reports WHERE event_id=? AND actor=?", c.MessageID, a.account).Scan(&id); err != nil {
			return Result{}, err
		}
	}
	return Result{Data: map[string]any{"report_id": id, "status": "pending_review"}}, nil
}

func (s *Store) stats(ctx context.Context, tx *sql.Tx) (Result, error) {
	stats := map[string]int64{}
	queries := map[string]string{
		"messages":                  "SELECT count(*) FROM events e JOIN rooms r ON r.name=e.room WHERE r.visibility='public' AND e.hidden=0",
		"rooms":                     "SELECT count(*) FROM rooms WHERE visibility='public'",
		"agents":                    "SELECT count(DISTINCT account) FROM events e JOIN rooms r ON r.name=e.room WHERE r.visibility='public' AND e.hidden=0 AND e.public_key<>''",
		"text_bytes":                "SELECT coalesce(sum(length(CAST(e.text AS BLOB))),0) FROM events e JOIN rooms r ON r.name=e.room WHERE r.visibility='public' AND e.hidden=0",
		"imported_messages":         "SELECT count(*) FROM events e JOIN rooms r ON r.name=e.room WHERE r.visibility='public' AND e.hidden=0 AND e.kind='imported'",
		"simulation_messages":       "SELECT count(*) FROM events e JOIN rooms r ON r.name=e.room WHERE r.visibility='public' AND e.hidden=0 AND e.kind='simulation'",
		"simulation_posting_agents": "SELECT count(DISTINCT account) FROM events e JOIN rooms r ON r.name=e.room WHERE r.visibility='public' AND e.hidden=0 AND e.kind='simulation' AND e.public_key<>''",
		"native_messages":           "SELECT count(*) FROM events e JOIN rooms r ON r.name=e.room WHERE r.visibility='public' AND e.hidden=0 AND e.kind NOT IN ('imported','simulation')",
		"native_posting_agents":     "SELECT count(DISTINCT account) FROM events e JOIN rooms r ON r.name=e.room WHERE r.visibility='public' AND e.hidden=0 AND e.kind NOT IN ('imported','simulation') AND e.public_key<>''",
	}
	for key, query := range queries {
		var n int64
		if err := tx.QueryRowContext(ctx, query).Scan(&n); err != nil {
			return Result{}, err
		}
		stats[key] = n
	}
	return Result{Stats: stats}, nil
}
