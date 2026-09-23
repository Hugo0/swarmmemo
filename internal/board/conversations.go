package board

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
)

// Read limits bound traversal work. The 64 KiB response budget is soft: one
// complete event is always returned even when its signed envelope exceeds it.
// A thread beyond the traversal limit remains accessible through the room feed.
const (
	ThreadAncestorLimit       = 256
	ThreadTraversalLimit      = 10000
	ConversationReadTimeout   = 2 * time.Second
	conversationResponseBytes = 64 << 10
)

// PageSummary counts currently visible messages at the directory snapshot.
// Pages containing only moderated messages are absent even for room members.
type PageSummary struct {
	Name      string `json:"name"`
	Count     int64  `json:"count"`
	UpdatedAt int64  `json:"updated_at"`
}

// This cursor format is independent of Command canonicalization and the public
// feed/export cursor formats. Its encrypted scope never grants authorization.
type conversationCursor struct {
	Version  int    `json:"v"`
	Domain   string `json:"domain"`
	Scope    string `json:"scope"`
	After    int64  `json:"after,omitempty"`
	Page     string `json:"page,omitempty"`
	Snapshot int64  `json:"snapshot,omitempty"`
}

func (s *Store) encodeConversationCursor(c conversationCursor) string {
	s.cursorMu.RLock()
	defer s.cursorMu.RUnlock()
	c.Version = 1
	plain, _ := json.Marshal(c)
	sealed := s.cursorCipher.Seal(nil, nil, plain, []byte("conversation-v1:"+s.generation))
	return s.generation + ":" + base64.RawURLEncoding.EncodeToString(sealed)
}

func (s *Store) decodeConversationCursor(raw, domain, scope string) (conversationCursor, error) {
	c := conversationCursor{Version: 1, Domain: domain, Scope: scope}
	if raw == "" || raw == "start" {
		return c, nil
	}
	if len(raw) > 1024 {
		return c, problem(400, "invalid_cursor", "Invalid conversation cursor.")
	}
	s.cursorMu.RLock()
	defer s.cursorMu.RUnlock()
	parts := strings.Split(raw, ":")
	if len(parts) != 2 {
		return c, problem(400, "invalid_cursor", "Use a cursor returned by this endpoint.")
	}
	if parts[0] != s.generation {
		return c, problem(409, "cursor_reset", "The server generation changed; restart this traversal and deduplicate event IDs.")
	}
	sealed, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil || base64.RawURLEncoding.EncodeToString(sealed) != parts[1] {
		return c, problem(400, "invalid_cursor", "Invalid conversation cursor.")
	}
	plain, err := s.cursorCipher.Open(nil, nil, sealed, []byte("conversation-v1:"+s.generation))
	if err != nil {
		return c, problem(400, "invalid_cursor", "Cursor is invalid or belongs to another endpoint.")
	}
	if err = json.Unmarshal(plain, &c); err != nil || c.Version != 1 || c.Domain != domain || c.Scope != scope || c.After < 0 || c.Snapshot < 0 {
		return c, problem(400, "invalid_cursor", "Cursor belongs to another conversation or room.")
	}
	if domain == "room.pages" && c.Page != "" && !slug.MatchString(c.Page) {
		return c, problem(400, "invalid_cursor", "Invalid page-directory cursor.")
	}
	return c, nil
}

func conversationError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Status: 503, Code: "conversation_read_timeout", Message: "Conversation read exceeded its two-second work budget; retry or use the room feed.", RetryAfter: 2}
	}
	return err
}

type threadReference struct {
	id, room, parent string
	seq              int64
}

// threadParent is a message's place in its thread: the message it replies to,
// or, for a new version of a root post, the version it supersedes. A version
// keeps its original's reply_to, so either way every version and every reply to
// any version resolves to the same root.
const threadParent = `CASE WHEN reply_to<>'' THEN reply_to ELSE supersedes END`

func resolveThreadRoot(ctx context.Context, tx *sql.Tx, current threadReference) (threadReference, error) {
	// Older, same-room parents make cycles impossible and avoid inspecting any
	// cross-room content, including malformed legacy parent relationships.
	for hops := 0; current.parent != ""; hops++ {
		if hops >= ThreadAncestorLimit {
			return threadReference{}, problem(400, "thread_depth_limit", "Thread ancestry exceeds the 256-parent read limit; use the room feed.")
		}
		var parent threadReference
		err := tx.QueryRowContext(ctx, "SELECT id,room,"+threadParent+",seq FROM events WHERE id=? AND room=? AND seq<?", current.parent, current.room, current.seq).Scan(&parent.id, &parent.room, &parent.parent, &parent.seq)
		if errors.Is(err, sql.ErrNoRows) {
			return threadReference{}, problem(400, "invalid_thread", "Thread contains an invalid parent relationship.")
		}
		if err != nil {
			return threadReference{}, conversationError(err)
		}
		current = parent
	}
	return current, nil
}

// threadReferences bounds the pending frontier as well as the visited rows.
// A recursive SQL LIMIT alone would still allow an arbitrarily large child
// queue. Each indexed expansion instead reads at most the remaining budget plus
// one sentinel row; neither a broad root nor a deep tree can allocate beyond it.
func threadReferences(ctx context.Context, tx *sql.Tx, root threadReference) ([]threadReference, error) {
	refs := []threadReference{root}
	seen := map[string]bool{root.id: true}
	for start := 0; start < len(refs); {
		end := min(start+128, len(refs))
		ids := make([]any, 0, end-start)
		for _, ref := range refs[start:end] {
			ids = append(ids, ref.id)
		}
		marks := strings.TrimSuffix(strings.Repeat("?,", end-start), ",")
		args := append(append(append(append([]any{}, ids...), root.room), ids...), root.room, ThreadTraversalLimit-len(refs)+1)
		// Replies, and newer versions of a message (children through supersedes).
		// A version of a reply is found both ways, so rows are deduplicated below.
		query := `SELECT id,seq FROM (SELECT e.id,e.seq FROM events e INDEXED BY events_reply JOIN events p ON p.id=e.reply_to
 WHERE e.reply_to IN (` + marks + `) AND e.room=? AND e.seq>p.seq
 UNION SELECT e.id,e.seq FROM events e INDEXED BY events_supersedes JOIN events p ON p.id=e.supersedes
 WHERE e.supersedes IN (` + marks + `) AND e.supersedes<>'' AND e.room=? AND e.seq>p.seq) LIMIT ?`
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, conversationError(err)
		}
		for rows.Next() {
			var ref threadReference
			if err = rows.Scan(&ref.id, &ref.seq); err != nil {
				rows.Close()
				return nil, conversationError(err)
			}
			if seen[ref.id] {
				continue
			}
			seen[ref.id] = true
			if len(refs) == ThreadTraversalLimit {
				rows.Close()
				return nil, problem(400, "thread_too_large", "Thread exceeds the 10000-event traversal limit; use the room's cursor feed.")
			}
			refs = append(refs, ref)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, conversationError(err)
		}
		start = end
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].seq < refs[j].seq })
	return refs, nil
}

func (s *Store) readThread(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if c.MessageID == "" || len(c.MessageID) > 64 {
		return Result{}, problem(400, "invalid_message_id", "thread.get requires a message_id of at most 64 characters.")
	}
	ctx, cancel := context.WithTimeout(ctx, ConversationReadTimeout)
	defer cancel()
	var current threadReference
	err := tx.QueryRowContext(ctx, "SELECT id,room,"+threadParent+",seq FROM events WHERE id=?", c.MessageID).Scan(&current.id, &current.room, &current.parent, &current.seq)
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, problem(404, "not_found", "Thread not found.")
	}
	if err != nil {
		return Result{}, conversationError(err)
	}
	if _, err = roomAccess(ctx, tx, current.room, a); err != nil {
		var access *Error
		if errors.As(err, &access) && access.Code == "not_found" {
			return Result{}, problem(404, "not_found", "Thread not found.")
		}
		return Result{}, conversationError(err)
	}
	root, err := resolveThreadRoot(ctx, tx, current)
	if err != nil {
		return Result{}, err
	}
	cursor, err := s.decodeConversationCursor(c.Cursor, "thread.get", root.id)
	if err != nil {
		return Result{}, err
	}
	refs, err := threadReferences(ctx, tx, root)
	if err != nil {
		return Result{}, err
	}
	limit := limitValue(c.Limit)
	start := sort.Search(len(refs), func(i int) bool { return refs[i].seq > cursor.After })
	refs = refs[start:min(start+limit+1, len(refs))]
	args := make([]any, 0, len(refs))
	for _, ref := range refs {
		args = append(args, ref.id)
	}
	// At most 201 event bodies are loaded, even for the largest accepted tree.
	rows, err := tx.QueryContext(ctx, "SELECT "+eventColumns+" FROM events e JOIN rooms r ON r.name=e.room WHERE e.id IN ("+strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")+") ORDER BY e.seq", args...)
	if err != nil {
		return Result{}, conversationError(err)
	}
	events := []Message{}
	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			rows.Close()
			return Result{}, conversationError(err)
		}
		events = append(events, event)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return Result{}, conversationError(err)
	}
	hasMore := len(events) > limit
	if hasMore {
		events = events[:limit]
	}
	if err = s.loadAttachments(ctx, tx, events, now); err != nil {
		return Result{}, conversationError(err)
	}
	bytes, end := 0, 0
	for i, event := range events {
		encoded, _ := json.Marshal(event)
		if i > 0 && bytes+len(encoded) > conversationResponseBytes {
			hasMore = true
			break
		}
		bytes += len(encoded)
		end = i + 1
	}
	events = events[:end]
	if len(events) > 0 {
		cursor.After = events[len(events)-1].internalSequence
	}
	return Result{Messages: events, NextCursor: s.encodeConversationCursor(cursor), Data: map[string]any{"root_id": root.id, "requested_message_id": c.MessageID, "room": root.room, "has_more": hasMore}}, nil
}

func (s *Store) readRoomPages(ctx context.Context, tx *sql.Tx, c Command, a actor) (Result, error) {
	if !ValidRoomName(c.Room) {
		return Result{}, problem(400, "invalid_slug", "room.pages requires an explicit lowercase room slug or personal room.")
	}
	ctx, cancel := context.WithTimeout(ctx, ConversationReadTimeout)
	defer cancel()
	if _, err := roomAccess(ctx, tx, c.Room, a); err != nil {
		return Result{}, conversationError(err)
	}
	cursor, err := s.decodeConversationCursor(c.Cursor, "room.pages", c.Room)
	if err != nil {
		return Result{}, err
	}
	if c.Cursor == "" || c.Cursor == "start" {
		if err = tx.QueryRowContext(ctx, "SELECT coalesce(max(seq),0) FROM events WHERE room=?", c.Room).Scan(&cursor.Snapshot); err != nil {
			return Result{}, conversationError(err)
		}
	}
	limit := limitValue(c.Limit)
	rows, err := tx.QueryContext(ctx, `SELECT page,count(*),max(created_at) FROM events INDEXED BY events_page_directory
 WHERE room=? AND hidden=0 AND seq<=? AND page>? GROUP BY page ORDER BY page COLLATE BINARY LIMIT ?`, c.Room, cursor.Snapshot, cursor.Page, limit+1)
	if err != nil {
		return Result{}, conversationError(err)
	}
	pages := []PageSummary{}
	for rows.Next() {
		var page PageSummary
		if err = rows.Scan(&page.Name, &page.Count, &page.UpdatedAt); err != nil {
			rows.Close()
			return Result{}, conversationError(err)
		}
		pages = append(pages, page)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return Result{}, conversationError(err)
	}
	hasMore := len(pages) > limit
	if hasMore {
		pages = pages[:limit]
	}
	next := ""
	if hasMore {
		cursor.Page = pages[len(pages)-1].Name
		next = s.encodeConversationCursor(cursor)
	}
	return Result{Data: map[string]any{"pages": pages, "has_more": hasMore}, NextCursor: next}, nil
}
