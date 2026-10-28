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

	"swarmmemo/internal/b64"
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
		return c, errConversationCursor()
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
	sealed, err := b64.URL(parts[1])
	if err != nil {
		return c, errConversationCursor()
	}
	plain, err := s.cursorCipher.Open(nil, nil, sealed, []byte("conversation-v1:"+s.generation))
	if err != nil {
		return c, errCursorOtherEndpoint()
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
	ctx, cancel := context.WithTimeout(ctx, ConversationReadTimeout)
	defer cancel()
	res, errs := s.readThreads(ctx, tx, []Command{c}, a, now)
	return res[0], errs[0]
}

// threadRead is one thread.get of a readThreads batch.
type threadRead struct {
	root                 threadReference
	cursor               conversationCursor
	replies, lastReplyAt int64
	ids                  []string // the page's events, oldest first, at most limit+1
	limit                int
	events               []Message
	hasMore              bool
	err                  error
}

// readThreads answers several thread.get commands in one transaction, each
// result and error exactly as thread.get alone gives it: each thread is
// resolved and bounded on its own, and the page's events are then read, with
// their attachments, votes, quality and work marks, once for all of them
// (C554: the Hot and Top feeds read a thread per post). A failure of that
// shared read fails every command.
func (s *Store) readThreads(ctx context.Context, tx *sql.Tx, cs []Command, a actor, now int64) ([]Result, []error) {
	reads := make([]threadRead, len(cs))
	var args []any
	queued := map[string]bool{}
	for i, c := range cs {
		r := &reads[i]
		r.err = s.threadPage(ctx, tx, c, a, r)
		if r.err != nil {
			continue
		}
		for _, id := range r.ids {
			if !queued[id] {
				queued[id] = true
				args = append(args, id)
			}
		}
	}
	results, errs := make([]Result, len(cs)), make([]error, len(cs))
	fail := func(err error) ([]Result, []error) {
		for i := range errs {
			if reads[i].err == nil {
				errs[i] = err
			} else {
				errs[i] = reads[i].err
			}
		}
		return results, errs
	}
	// At most 201 event bodies are loaded per thread, even for the largest
	// accepted tree.
	byID := make(map[string]Message, len(args))
	for start := 0; start < len(args); start += threadReadChunk {
		chunk := args[start:min(start+threadReadChunk, len(args))]
		rows, err := tx.QueryContext(ctx, "SELECT "+eventColumns+" FROM events e JOIN rooms r ON r.name=e.room WHERE e.id IN ("+strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")+")", chunk...)
		if err != nil {
			return fail(conversationError(err))
		}
		for rows.Next() {
			event, err := scanEvent(rows)
			if err != nil {
				rows.Close()
				return fail(conversationError(err))
			}
			byID[event.ID] = event
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return fail(conversationError(err))
		}
	}
	// Each thread's page, oldest first (ids are in seq order), cut to its limit.
	var all []Message
	for i := range reads {
		r := &reads[i]
		if r.err != nil {
			continue
		}
		r.events = []Message{}
		for _, id := range r.ids {
			if e, ok := byID[id]; ok {
				r.events = append(r.events, e)
			}
		}
		r.hasMore = len(r.events) > r.limit
		if r.hasMore {
			r.events = r.events[:r.limit]
		}
		all = append(all, r.events...)
	}
	// distribute copies all's enriched events back into each thread's page.
	distribute := func() {
		at := 0
		for i := range reads {
			if reads[i].err == nil {
				at += copy(reads[i].events, all[at:at+len(reads[i].events)])
			}
		}
	}
	if err := s.loadAttachments(ctx, tx, all, now); err != nil {
		return fail(conversationError(err))
	}
	distribute()
	all = all[:0]
	for i := range reads {
		r := &reads[i]
		if r.err != nil {
			continue
		}
		bytes, end := 0, 0
		for j, event := range r.events {
			encoded, _ := json.Marshal(event)
			if j > 0 && bytes+len(encoded) > conversationResponseBytes {
				r.hasMore = true
				break
			}
			bytes += len(encoded)
			end = j + 1
		}
		r.events = r.events[:end]
		if len(r.events) > 0 {
			r.cursor.After = r.events[len(r.events)-1].internalSequence
		}
		all = append(all, r.events...)
	}
	if err := s.attachScores(ctx, tx, all, now); err != nil {
		return fail(err)
	}
	if err := s.screenConversationMessages(ctx, tx, a, all); err != nil {
		return fail(err)
	}
	distribute()
	for i := range reads {
		r := &reads[i]
		if errs[i] = r.err; r.err != nil {
			continue
		}
		results[i] = Result{Messages: r.events, NextCursor: s.encodeConversationCursor(r.cursor), Data: map[string]any{"root_id": r.root.id, "requested_message_id": cs[i].MessageID, "room": r.root.room, "has_more": r.hasMore, "replies": r.replies, "last_reply_at": r.lastReplyAt}}
	}
	return results, errs
}

// threadReadChunk bounds the event IDs one statement of readThreads names.
const threadReadChunk = 500

// messageRefID is the bare message ID from what an agent may paste in place
// of a 32-hex id: a Copy link's "…#sm-ID" (the embed and moderation flows
// hand that form out) or a bare "#ID". A reference with no "#sm-" marker or
// leading "#" is returned trimmed but otherwise unchanged, so a wrong id
// still reads as not_found rather than being rewritten into a different post.
func messageRefID(ref string) string {
	ref = strings.TrimSpace(ref)
	if i := strings.LastIndex(ref, "#sm-"); i >= 0 {
		return ref[i+len("#sm-"):]
	}
	return strings.TrimPrefix(ref, "#")
}

// threadPage resolves one thread.get: the message, the reader's access to
// its room, the thread's root, the cursor, the thread's references and
// reply counts, and the page of event IDs after the cursor (one past the
// limit, to tell whether more follow).
func (s *Store) threadPage(ctx context.Context, tx *sql.Tx, c Command, a actor, r *threadRead) error {
	id := messageRefID(c.MessageID)
	if id == "" || len(id) > 64 {
		return problem(400, "invalid_message_id", "thread.get requires a message_id of at most 64 characters.")
	}
	var current threadReference
	err := tx.QueryRowContext(ctx, "SELECT id,room,"+threadParent+",seq FROM events WHERE id=?", id).Scan(&current.id, &current.room, &current.parent, &current.seq)
	if errors.Is(err, sql.ErrNoRows) {
		// An inbox entry's id (read_updates data.entries[].id) is 32 hex
		// like a message id, and agents pass it here; read the message it
		// is about (its subject). Access is checked below as for any id.
		err = tx.QueryRowContext(ctx, "SELECT e.id,e.room,"+threadParent+",e.seq FROM inbox_entries ie JOIN events e ON e.id=ie.subject WHERE ie.id=?", id).Scan(&current.id, &current.room, &current.parent, &current.seq)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return errThreadNotFound()
	}
	if err != nil {
		return conversationError(err)
	}
	if _, err = roomAccess(ctx, tx, current.room, a); err != nil {
		var access *Error
		if errors.As(err, &access) && access.Code == "not_found" {
			return errThreadNotFound()
		}
		return conversationError(err)
	}
	if r.root, err = resolveThreadRoot(ctx, tx, current); err != nil {
		return err
	}
	if r.cursor, err = s.decodeConversationCursor(c.Cursor, "thread.get", r.root.id); err != nil {
		return err
	}
	refs, err := threadReferences(ctx, tx, r.root)
	if err != nil {
		return err
	}
	if r.replies, r.lastReplyAt, err = threadReplyStats(ctx, tx, refs); err != nil {
		return err
	}
	r.limit = limitValue(c.Limit)
	start := sort.Search(len(refs), func(i int) bool { return refs[i].seq > r.cursor.After })
	for _, ref := range refs[start:min(start+r.limit+1, len(refs))] {
		r.ids = append(r.ids, ref.id)
	}
	return nil
}

// PublicThreads reads, for an anonymous reader in one read-only
// transaction, thread.get of each of ids with limit: what Execute gives each
// thread.get from the public web pages (web-public-read), without a
// transaction per thread. A thread that cannot be read has its error in
// errs; it is never a reason to fail the others.
func (s *Store) PublicThreads(ctx context.Context, ids []string, limit int) (results []Result, errs []error, err error) {
	cs := make([]Command, len(ids))
	for i, id := range ids {
		cs[i] = Command{Operation: "thread.get", MessageID: id, Limit: limit}
		if err = readLimitRange(cs[i]); err != nil {
			return nil, nil, err
		}
	}
	err = s.publicRead(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var generation string
		if err := tx.QueryRowContext(ctx, "SELECT value FROM meta WHERE key='generation'").Scan(&generation); err != nil {
			return err
		}
		results, errs = s.readThreads(ctx, tx, cs, actor{id: "anonymous"}, s.now().Unix())
		for i := range results {
			if errs[i] == nil {
				results[i].OK, results[i].Generation = true, generation
			}
		}
		return nil
	})
	return results, errs, err
}

// threadReplyStats counts the thread's visible replies (every post but the
// root, one per message however often it was edited) and the newest one's
// time, 0 with none (C462). A returning poster reads them from one
// thread.get with limit=1 to learn whether anything was answered, so a sweep
// over its saved post ids is one small read each. The ids are the ones
// threadReferences already found, read by primary key in bounded chunks.
func threadReplyStats(ctx context.Context, tx *sql.Tx, refs []threadReference) (int64, int64, error) {
	var replies, last int64
	const chunk = 500
	for start := 1; start < len(refs); start += chunk {
		end := min(start+chunk, len(refs))
		ids := make([]any, 0, end-start)
		for _, ref := range refs[start:end] {
			ids = append(ids, ref.id)
		}
		var n, at int64
		err := tx.QueryRowContext(ctx, "SELECT count(*),coalesce(max(created_at),0) FROM events WHERE id IN ("+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+") AND hidden=0 AND supersedes=''", ids...).Scan(&n, &at)
		if err != nil {
			return 0, 0, conversationError(err)
		}
		replies += n
		last = max(last, at)
	}
	return replies, last, nil
}

// replyCountWalkMax bounds the whole batched reply-count walk — every root on
// the page together — as ThreadTraversalLimit bounds one thread.get. A page of
// ordinary threads never approaches it; a pathological thread can only make the
// list's count an underestimate, never unbounded work.
const replyCountWalkMax = 50000

// replyCountsFor counts, for each id in rootIDs, the visible posts in its
// thread after the root: the same number threadReplyStats gives read_thread
// (data.replies), for a whole page of roots in one statement instead of a
// traversal each. A root with no replies is absent from the map (read as 0).
// The walk mirrors threadReferences' edges (replies by reply_to, newer versions
// by supersedes, older same-room rows only) and carries each descendant's root
// and room through the recursion, so a page spanning rooms stays correct; the
// count filters exactly as threadReplyStats does (hidden excluded, one row per
// message however often edited: supersedes='').
func replyCountsFor(ctx context.Context, tx *sql.Tx, rootIDs []string) (map[string]int64, error) {
	out := map[string]int64{}
	if len(rootIDs) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(rootIDs)+1)
	for _, id := range rootIDs {
		args = append(args, id)
	}
	args = append(args, replyCountWalkMax)
	marks := strings.TrimSuffix(strings.Repeat("?,", len(rootIDs)), ",")
	query := `WITH RECURSIVE down(root, room, id, seq, hidden, supersedes) AS (
 SELECT e.id, e.room, e.id, e.seq, e.hidden, e.supersedes FROM events e WHERE e.id IN (` + marks + `)
 UNION
 SELECT d.root, d.room, c.id, c.seq, c.hidden, c.supersedes FROM down d JOIN events c ON c.reply_to=d.id AND c.room=d.room AND c.seq>d.seq
 UNION
 SELECT d.root, d.room, c.id, c.seq, c.hidden, c.supersedes FROM down d JOIN events c ON c.supersedes=d.id AND c.supersedes<>'' AND c.room=d.room AND c.seq>d.seq
 LIMIT ?)
SELECT root, count(*) FROM down WHERE id<>root AND hidden=0 AND supersedes='' GROUP BY root`
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, conversationError(err)
	}
	defer rows.Close()
	for rows.Next() {
		var root string
		var n int64
		if err = rows.Scan(&root, &n); err != nil {
			return nil, conversationError(err)
		}
		out[root] = n
	}
	if err = rows.Err(); err != nil {
		return nil, conversationError(err)
	}
	return out, nil
}

// attachReplyCounts sets Replies on each top-level ORIGIN post (reply_to=='' and
// supersedes=='') in events to its thread's visible reply count, the number
// read_thread reports (threadReplyStats). A reply carries none, and so does a
// newer version shown as a root (supersedes!=''): the walk is seeded from the
// listed id, and only an origin's subtree matches read_thread, which resolves to
// the origin first. Withholding the count there (nil) sends the reader to
// read_thread rather than risk an undercount read as "unanswered". One batched
// walk serves the whole page (replyCountsFor), never a query per post.
func attachReplyCounts(ctx context.Context, tx *sql.Tx, events []Message) error {
	idx := map[string][]int{}
	ids := make([]string, 0, len(events))
	for i, e := range events {
		if e.Type != "message" || e.ReplyTo != "" || e.Supersedes != "" {
			continue
		}
		if _, seen := idx[e.ID]; !seen {
			ids = append(ids, e.ID)
		}
		idx[e.ID] = append(idx[e.ID], i)
	}
	if len(ids) == 0 {
		return nil
	}
	counts, err := replyCountsFor(ctx, tx, ids)
	if err != nil {
		return err
	}
	for id, at := range idx {
		n := int(counts[id])
		for _, i := range at {
			c := n
			events[i].Replies = &c
		}
	}
	return nil
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
