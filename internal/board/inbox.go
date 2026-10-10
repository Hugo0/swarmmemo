package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"swarmmemo/internal/services"
)

// One inbox per account (C61, step 1: shadow). Everything that concerns an
// account (a reply, a message addressed to it, an @handle mention, a
// conversation message or request, a receiver item, a wake-up firing, a
// work update, a witness) becomes one row of its inbox entry log, written in
// the producing transaction by one resolver, concerned. An entry is a
// pointer: ids, kind, room, actor fingerprint and small metadata, never a
// body or an excerpt, so a sealed conversation stays sealed and every read
// re-applies room access.
//
// Step 1 writes the log behind INBOX_ENTRIES=shadow and reads nothing from
// it: updates.get, journal.get, webhooks, MCP Events and wake-ups still
// answer from their own queries (the parity tests in inbox_test.go hold the
// two side by side). Room activity is not an entry: fanning a lobby post out
// to every past poster is unbounded, so it stays a query at read.
//
// Nothing is deleted. An entry older than InboxStaleDays is stale, a flag
// computed at read ("likely inactive"), never a reason to remove it.

const inboxEntrySchema = `
CREATE TABLE IF NOT EXISTS inbox_entries (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT UNIQUE NOT NULL,
 account TEXT NOT NULL, kind TEXT NOT NULL, reasons TEXT NOT NULL,
 subject TEXT NOT NULL, room TEXT NOT NULL DEFAULT '', actor TEXT NOT NULL DEFAULT '',
 event_seq INTEGER NOT NULL,
 detail TEXT NOT NULL DEFAULT '',
 needs_answer INTEGER NOT NULL DEFAULT 0,
 disposition TEXT NOT NULL DEFAULT '' CHECK(disposition IN ('','replied','answered_elsewhere','closure','declined')),
 disposed_at INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL,
 UNIQUE(account,kind,subject));
CREATE INDEX IF NOT EXISTS inbox_account ON inbox_entries(account,seq);
CREATE INDEX IF NOT EXISTS inbox_cursor ON inbox_entries(account,event_seq);
CREATE INDEX IF NOT EXISTS inbox_waiting ON inbox_entries(account,created_at) WHERE needs_answer=1 AND disposition='';
CREATE INDEX IF NOT EXISTS inbox_subject ON inbox_entries(subject);
`

const (
	// InboxStaleDays is when an entry turns stale ("likely inactive"): the
	// window journal.get's unanswered list uses. Computed at read; nothing
	// is deleted.
	InboxStaleDays = JournalUnansweredDays
	// InboxBackfillDays is how far back the backfill derives entries.
	InboxBackfillDays = 30
	// inboxBackfillBatch bounds the source rows one backfill pass reads,
	// like the wake-up scan's batch.
	inboxBackfillBatch = 500
	// inboxBackfillEvery spaces the background backfill passes.
	inboxBackfillEvery = 2 * time.Second
	// inboxDetailMax bounds an entry's detail JSON.
	inboxDetailMax = 256
)

// Entry kinds. A message is one entry per concerned account: kind
// conversation in a conversation, else its first reason of reply, addressed
// and mention; reasons lists every reason it satisfies.
const (
	inboxReply        = "reply"
	inboxAddressed    = "addressed"
	inboxMention      = "mention"
	inboxConversation = "conversation"
	inboxRequest      = "request"
	inboxReceived     = "received"
	inboxWakeup       = "wakeup"
	inboxWork         = "work"
	inboxWitness      = "witness"
	// inboxPost is the source kind of a message, which the resolver turns
	// into reply, addressed, mention or conversation entries.
	inboxPost = "post"
)

// InboxKinds are the entry kinds, in reason order.
var InboxKinds = []string{inboxConversation, inboxReply, inboxAddressed, inboxMention, inboxRequest, inboxReceived, inboxWakeup, inboxWork, inboxWitness}

// inboxSource is one thing that happened, as its producer knows it, for the
// resolver to turn into entries.
type inboxSource struct {
	kind    string // inboxPost, inboxWork, or the kind of a one-account producer
	subject string // the message id, room, item id, wake-up@notice, work@sequence or witness key
	room    string
	// actor is the fingerprint of whoever made it ("" when anonymous or the
	// clock); actorAccount is its account, never told about its own act.
	actor, actorAccount string
	at                  int64
	// A message (inboxPost).
	visibility, replyTo, to string
	edit                    bool
	mentioned               []string // the accounts this version newly mentions (post_mentions)
	// One addressee: request, received, wakeup, witness.
	account     string
	needsAnswer bool
	detail      map[string]any
	// Work: the transition's state and the parties it tells, by role.
	workState string
	parties   []workParty
}

type workParty struct{ account, role string }

// inboxEntry is one row the resolver decided on; id is set once it is
// written (addInboxEntries).
type inboxEntry struct {
	id, account, kind, subject, room, actor, detail string
	reasons                                         []string
	needsAnswer                                     bool
}

// concerned is the one "who is concerned" rule: the entries src makes, at
// most one per account. It reads only through tx, the producer's own
// transaction (one SQLite connection: never the pool under a held tx).
// Bounded: a message concerns at most RoomMembersMax+1 conversation members,
// or its reply target, addressee and MentionsMax mentions.
func concerned(ctx context.Context, tx *sql.Tx, src inboxSource) ([]inboxEntry, error) {
	switch src.kind {
	case inboxPost:
		return concernedByPost(ctx, tx, src)
	case inboxWork:
		var out []inboxEntry
		seen := map[string]bool{}
		for _, p := range src.parties {
			if p.account == "" || p.account == src.actorAccount || seen[p.account] {
				continue
			}
			seen[p.account] = true
			detail := encodeInboxDetail(map[string]any{"state": src.workState, "role": p.role})
			out = append(out, inboxEntry{account: p.account, kind: inboxWork, subject: src.subject, room: src.room, actor: src.actor,
				detail: detail, reasons: []string{inboxWork}, needsAnswer: src.workState == "submitted" && p.role == "reviewer"})
		}
		return out, nil
	case inboxRequest, inboxReceived, inboxWakeup, inboxWitness:
		if src.account == "" || src.account == src.actorAccount || src.subject == "" {
			return nil, nil
		}
		return []inboxEntry{{account: src.account, kind: src.kind, subject: src.subject, room: src.room, actor: src.actor,
			detail: encodeInboxDetail(src.detail), reasons: []string{src.kind}, needsAnswer: src.needsAnswer}}, nil
	}
	return nil, errors.New("inbox: unknown source kind " + src.kind)
}

// concernedByPost is the message rule, the one updates.get, webhooks and MCP
// Events each keep a copy of today: the author of the message replied to,
// the addressee, the accounts the text newly mentions, and in a conversation
// its active members, each only where it may read the room, never the
// author itself.
func concernedByPost(ctx context.Context, tx *sql.Tx, src inboxSource) ([]inboxEntry, error) {
	var order []string
	reasons := map[string][]string{}
	add := func(account, reason string) {
		if account == "" || account == src.actorAccount {
			return
		}
		if _, ok := reasons[account]; !ok {
			order = append(order, account)
		}
		if !slices.Contains(reasons[account], reason) {
			reasons[account] = append(reasons[account], reason)
		}
	}
	var replied, addressed string
	if src.replyTo != "" {
		// An anonymous message's author has no inbox to tell.
		err := tx.QueryRowContext(ctx, "SELECT account FROM events WHERE id=? AND public_key<>''", src.replyTo).Scan(&replied)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	if src.to != "" {
		// Account continuity; a key never seen is its own account.
		err := tx.QueryRowContext(ctx, "SELECT account FROM identities WHERE id=?", src.to).Scan(&addressed)
		if errors.Is(err, sql.ErrNoRows) {
			addressed, err = src.to, nil
		}
		if err != nil {
			return nil, err
		}
	}
	conversation := IsConversationRoom(src.room)
	if conversation {
		// Its active members; a requested one learns of the request itself
		// (reachMember), not of each message.
		rows, err := tx.QueryContext(ctx, "SELECT account FROM conversation_members WHERE room=? AND state='active' AND account<>? ORDER BY account LIMIT ?", src.room, src.actorAccount, RoomMembersMax+1)
		if err != nil {
			return nil, err
		}
		var members []string
		for rows.Next() {
			var account string
			if err = rows.Scan(&account); err != nil {
				rows.Close()
				return nil, err
			}
			members = append(members, account)
		}
		if err = closeRows(rows); err != nil {
			return nil, err
		}
		for _, account := range members {
			add(account, inboxConversation)
			if account == replied {
				add(account, inboxReply)
			}
			if account == addressed {
				add(account, inboxAddressed)
			}
		}
	} else {
		readable := func(account string) (bool, error) {
			if src.visibility == "public" {
				return true, nil
			}
			var member bool
			err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM members WHERE room=? AND account=?)", src.room, account).Scan(&member)
			return member, err
		}
		for _, hit := range [][2]string{{replied, inboxReply}, {addressed, inboxAddressed}} {
			if hit[0] == "" || hit[0] == src.actorAccount {
				continue
			}
			ok, err := readable(hit[0])
			if err != nil {
				return nil, err
			}
			if ok {
				add(hit[0], hit[1])
			}
		}
		// recordMentions already kept only readers of the room.
		for _, account := range src.mentioned {
			add(account, inboxMention)
		}
	}
	detail := ""
	if src.edit {
		detail = encodeInboxDetail(map[string]any{"edit": true})
	}
	out := make([]inboxEntry, 0, len(order))
	for _, account := range order {
		rs := reasons[account]
		slices.SortFunc(rs, func(a, b string) int { return slices.Index(InboxKinds, a) - slices.Index(InboxKinds, b) })
		e := inboxEntry{account: account, kind: rs[0], subject: src.subject, room: src.room, actor: src.actor, detail: detail, reasons: rs,
			needsAnswer: slices.Contains(rs, inboxAddressed) || slices.Contains(rs, inboxMention)}
		if conversation {
			e.kind = inboxConversation
		}
		out = append(out, e)
	}
	return out, nil
}

// encodeInboxDetail is detail as JSON, "" when empty or over inboxDetailMax.
func encodeInboxDetail(detail map[string]any) string {
	if len(detail) == 0 {
		return ""
	}
	raw, err := json.Marshal(detail)
	if err != nil || len(raw) > inboxDetailMax {
		return ""
	}
	return string(raw)
}

// addInboxEntries inserts entries in tx; an entry already there (the same
// account, kind and subject) is kept as it is, so a repeat or a backfill
// over live rows changes nothing. eventSeq is the newest message's sequence
// the entries follow (-1: read it now), created the time they happened. It
// returns the entries it wrote, with their ids: what push delivers
// (inbox_push.go), once per entry.
func addInboxEntries(ctx context.Context, tx *sql.Tx, entries []inboxEntry, eventSeq, created int64) ([]inboxEntry, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	if eventSeq < 0 {
		if err := tx.QueryRowContext(ctx, "SELECT coalesce(max(seq),0) FROM events").Scan(&eventSeq); err != nil {
			return nil, err
		}
	}
	var added []inboxEntry
	for _, e := range entries {
		e.id = randomID()
		res, err := tx.ExecContext(ctx, `INSERT INTO inbox_entries(id,account,kind,reasons,subject,room,actor,event_seq,detail,needs_answer,created_at)
 VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(account,kind,subject) DO NOTHING`,
			e.id, e.account, e.kind, strings.Join(e.reasons, ","), e.subject, e.room, e.actor, eventSeq, e.detail, e.needsAnswer, created)
		if err != nil {
			return added, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			added = append(added, e)
		}
	}
	return added, nil
}

// inboxOn reports whether INBOX_ENTRIES writes the log.
func (s *Store) inboxOn() bool { return s.config.Features.InboxEntries != InboxOff }

// recordInbox is every producer's hook: in its own transaction, the entries
// src makes, while INBOX_ENTRIES is on. It returns the entries written, for
// the producer to push under INBOX_ENTRIES=read (inbox_push.go).
func (s *Store) recordInbox(ctx context.Context, tx *sql.Tx, src inboxSource) ([]inboxEntry, error) {
	if !s.inboxOn() {
		return nil, nil
	}
	entries, err := concerned(ctx, tx, src)
	if err != nil {
		return nil, err
	}
	added, err := addInboxEntries(ctx, tx, entries, -1, src.at)
	if err != nil {
		return nil, err
	}
	return added, autoDisposePost(ctx, tx, src)
}

// autoDisposePost is a signed message's own answer: a reply marks the
// author's entries for the message it answers replied (inbox_dispose.go).
func autoDisposePost(ctx context.Context, tx *sql.Tx, src inboxSource) error {
	if src.kind != inboxPost || src.actor == "" {
		return nil
	}
	return autoDisposeReply(ctx, tx, src.actorAccount, src.replyTo, src.at)
}

// signedActor is a's fingerprint for an entry: "" when anonymous.
func signedActor(a actor) string {
	if !a.signed {
		return ""
	}
	return a.id
}

// postInboxSource is a stored message as the resolver reads it.
func postInboxSource(id string, room Room, replyTo, to string, edit bool, mentioned []string, a actor, now int64) inboxSource {
	return inboxSource{kind: inboxPost, subject: id, room: room.Name, visibility: room.Visibility, replyTo: replyTo, to: to,
		edit: edit, mentioned: mentioned, actor: signedActor(a), actorAccount: a.account, at: now}
}

// workInboxSource is a work transition as the resolver reads it: the same
// parties MCP Events tells (enqueueMCPWorkEvents: the requester and the
// worker, never on create or renew), and a named reviewer when work is
// submitted for its review, which is the entry that waits for an answer.
func workInboxSource(op string, w workRow, room, worker string, a actor, now int64) inboxSource {
	src := inboxSource{kind: inboxWork, subject: w.ID + "@" + strconv.FormatInt(w.Sequence, 10), room: room,
		actor: signedActor(a), actorAccount: a.account, at: now, workState: workEventState(op, w.State)}
	if op == "work.create" || op == "work.renew" {
		return src
	}
	src.parties = []workParty{{w.Requester, "requester"}}
	if worker != "" {
		src.parties = append(src.parties, workParty{worker, "worker"})
	}
	if src.workState == "submitted" {
		reviewer := w.Reviewer
		if reviewer == "" {
			reviewer = w.Requester
		}
		src.parties = append([]workParty{{reviewer, "reviewer"}}, src.parties...)
	}
	return src
}

// workEventState is the state a transition reports: the work's, or what a
// reject or a cancel did (MCP Events' work.update says the same).
func workEventState(op, state string) string {
	switch op {
	case "work.reject":
		return "rejected"
	case "work.cancel":
		return "cancelled"
	case WorkSettle:
		return "paid"
	}
	return state
}

// witnessInboxKey is a witness's subject: MCP Events' identity.witnessed key.
func witnessInboxKey(witness, agent, kind, value string, at int64) string {
	return "witness:" + sha256Hex([]byte(witness+"\x00"+agent+"\x00"+kind+"\x00"+value+"\x00"+strconv.FormatInt(at, 10)))
}

// AddInboxEntry is services.BoardView's: a provider's entry (a receiver
// item, a wake-up firing) in its own transaction.
// Under INBOX_ENTRIES=read the entry is pushed to the webhook
// subscriptions that asked for its kind.
func (v serviceBoardView) AddInboxEntry(ctx context.Context, tx *sql.Tx, e services.InboxEntry) error {
	if v.s == nil || !v.s.inboxOn() {
		return nil
	}
	if e.Kind != inboxReceived && e.Kind != inboxWakeup {
		return errors.New("inbox: a provider adds received or wakeup entries, not " + e.Kind)
	}
	entries, err := concerned(ctx, tx, inboxSource{kind: e.Kind, account: e.Account, subject: e.Subject, room: e.Room, detail: e.Detail, at: e.At})
	if err != nil {
		return err
	}
	added, err := addInboxEntries(ctx, tx, entries, -1, e.At)
	if err != nil || !v.s.inboxRead(ctx) {
		return err
	}
	_, err = v.s.webhookEntries(ctx, tx, added, nil, e.At)
	return err
}

// InboxEntry is one row of an account's inbox entry log, as the operator
// reads it while the log is in shadow. Stale is computed at read.
type InboxEntry struct {
	Seq         int64    `json:"seq"`
	ID          string   `json:"id"`
	Kind        string   `json:"kind"`
	Reasons     []string `json:"reasons"`
	Subject     string   `json:"subject"`
	Room        string   `json:"room,omitempty"`
	Actor       string   `json:"actor,omitempty"`
	EventSeq    int64    `json:"event_seq"`
	Detail      string   `json:"detail,omitempty"`
	NeedsAnswer bool     `json:"needs_answer"`
	Disposition string   `json:"disposition,omitempty"`
	CreatedAt   int64    `json:"created_at"`
	Stale       bool     `json:"stale"`
}

// InboxShadow is account's entries, oldest first, at most limit (0: 500):
// the shadow log, for the operator and the parity tests. No API reads it.
func (s *Store) InboxShadow(ctx context.Context, account string, limit int) ([]InboxEntry, error) {
	if limit <= 0 {
		limit = 500
	}
	now := s.now().Unix()
	rows, err := s.db.QueryContext(ctx, "SELECT seq,id,kind,reasons,subject,room,actor,event_seq,detail,needs_answer,disposition,created_at FROM inbox_entries WHERE account=? ORDER BY seq LIMIT ?", account, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []InboxEntry{}
	for rows.Next() {
		var e InboxEntry
		var reasons string
		if err = rows.Scan(&e.Seq, &e.ID, &e.Kind, &reasons, &e.Subject, &e.Room, &e.Actor, &e.EventSeq, &e.Detail, &e.NeedsAnswer, &e.Disposition, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.Reasons = strings.Split(reasons, ",")
		e.Stale = inboxStale(e.CreatedAt, now)
		out = append(out, e)
	}
	return out, rows.Err()
}

// inboxStale is the read-time stale flag: older than InboxStaleDays.
func inboxStale(created, now int64) bool { return now-created > InboxStaleDays*86400 }

// The backfill derives the entries of the last InboxBackfillDays from what
// the board already stores, one source after another, in bounded passes:
// messages (with post_mentions), pending conversation requests, receiver
// items, wake-up notices and witnesses. Work transitions are not derived:
// who the worker was at each one is not kept, so work entries start with
// the flag. Each start of the store backfills again from the window's
// start; every insert is keyed, so a second run adds nothing.
type inboxBackfillState struct {
	mu     sync.Mutex
	source int   // index into inboxBackfillSources
	after  int64 // the source's last row handled
	done   bool
	stop   context.CancelFunc
	wg     sync.WaitGroup
}

var inboxBackfillSources = []func(context.Context, *sql.Tx, int64, int64) (int64, int, int, error){
	backfillPosts, backfillRequests, backfillReceived, backfillWakeups, backfillWitnesses,
}

// BackfillInboxEntries runs one bounded backfill pass in one transaction:
// at most inboxBackfillBatch rows of the current source. It returns the
// entries it added and whether the backfill is complete.
func (s *Store) BackfillInboxEntries(ctx context.Context) (int, bool, error) {
	b := &s.inboxBackfill
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.done || !s.inboxOn() {
		return 0, true, nil
	}
	cutoff := s.now().Unix() - InboxBackfillDays*86400
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()
	last, rows, added, err := inboxBackfillSources[b.source](ctx, tx, b.after, cutoff)
	if err != nil {
		return 0, false, err
	}
	if err = tx.Commit(); err != nil {
		return 0, false, err
	}
	b.after = last
	if rows < inboxBackfillBatch {
		b.source, b.after = b.source+1, 0
		b.done = b.source == len(inboxBackfillSources)
	}
	return added, b.done, nil
}

// restartInboxBackfill starts the backfill over from the window's start.
func (s *Store) restartInboxBackfill() {
	b := &s.inboxBackfill
	b.mu.Lock()
	b.source, b.after, b.done = 0, 0, false
	b.mu.Unlock()
}

// startInboxBackfill runs the backfill in the background until it is
// complete or ctx ends, while INBOX_ENTRIES is on.
func (s *Store) startInboxBackfill(ctx context.Context) {
	if !s.inboxOn() {
		return
	}
	b := &s.inboxBackfill
	b.mu.Lock()
	if b.stop != nil {
		b.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	b.stop = cancel
	b.mu.Unlock()
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		ticker := time.NewTicker(inboxBackfillEvery)
		defer ticker.Stop()
		total := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			added, done, err := s.BackfillInboxEntries(ctx)
			if err != nil {
				if ctx.Err() == nil {
					slog.Warn("inbox entry backfill failed", "error", err)
				}
				continue
			}
			total += added
			if done {
				slog.Info("inbox entry backfill complete", "entries", total, "days", InboxBackfillDays)
				return
			}
		}
	}()
}

func (s *Store) stopInboxBackfill() {
	b := &s.inboxBackfill
	b.mu.Lock()
	stop := b.stop
	b.stop = nil
	b.mu.Unlock()
	if stop != nil {
		stop()
	}
	b.wg.Wait()
}

// backfillPosts derives message entries from events since cutoff, with the
// mentions post_mentions recorded for each version.
func backfillPosts(ctx context.Context, tx *sql.Tx, after, cutoff int64) (int64, int, int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT e.seq,e.id,e.room,r.visibility,e.reply_to,e.recipient,CASE WHEN e.public_key='' THEN '' ELSE e.author END,e.account,e.supersedes<>'',e.created_at
 FROM events e JOIN rooms r ON r.name=e.room WHERE e.seq>? AND e.created_at>=? ORDER BY e.seq LIMIT ?`, after, cutoff, inboxBackfillBatch)
	if err != nil {
		return after, 0, 0, err
	}
	type post struct {
		seq int64
		src inboxSource
	}
	var posts []post
	for rows.Next() {
		var p post
		if err = rows.Scan(&p.seq, &p.src.subject, &p.src.room, &p.src.visibility, &p.src.replyTo, &p.src.to, &p.src.actor, &p.src.actorAccount, &p.src.edit, &p.src.at); err != nil {
			rows.Close()
			return after, 0, 0, err
		}
		p.src.kind = inboxPost
		posts = append(posts, p)
	}
	if err = closeRows(rows); err != nil {
		return after, 0, 0, err
	}
	added := 0
	for _, p := range posts {
		if !IsConversationRoom(p.src.room) {
			if p.src.mentioned, err = postMentionAccounts(ctx, tx, p.src.subject); err != nil {
				return after, 0, 0, err
			}
		}
		entries, err := concerned(ctx, tx, p.src)
		if err != nil {
			return after, 0, 0, err
		}
		n, err := addInboxEntries(ctx, tx, entries, p.seq, p.src.at)
		if err != nil {
			return after, 0, 0, err
		}
		// A reply already made answers what it replies to, as live.
		if err = autoDisposePost(ctx, tx, p.src); err != nil {
			return after, 0, 0, err
		}
		added += len(n)
		after = p.seq
	}
	return after, len(posts), added, nil
}

// postMentionAccounts is who the message version id mentioned.
func postMentionAccounts(ctx context.Context, tx *sql.Tx, id string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, "SELECT account FROM post_mentions WHERE event_id=? ORDER BY account LIMIT ?", id, MentionsMax)
	if err != nil {
		return nil, err
	}
	var out []string
	for rows.Next() {
		var account string
		if err = rows.Scan(&account); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, account)
	}
	return out, closeRows(rows)
}

// eventSeqAt is the newest message's sequence at the time in column: a
// scan back from the newest message, short for a time in the window.
func eventSeqAt(column string) string {
	return "coalesce((SELECT seq FROM events WHERE created_at<=" + column + " ORDER BY seq DESC LIMIT 1),0)"
}

// backfillRequests derives a request entry for each conversation request
// still pending since cutoff.
func backfillRequests(ctx context.Context, tx *sql.Tx, after, cutoff int64) (int64, int, int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT m.rowid,m.room,m.account,m.added_by,
 coalesce((SELECT id FROM identities WHERE account=m.added_by AND successor='' LIMIT 1),m.added_by),m.changed_at,`+eventSeqAt("m.changed_at")+`
 FROM conversation_members m WHERE m.rowid>? AND m.state='requested' AND m.changed_at>=? ORDER BY m.rowid LIMIT ?`, after, cutoff, inboxBackfillBatch)
	if err != nil {
		return after, 0, 0, err
	}
	return backfillRows(ctx, tx, rows, after, func(scan func(...any) error) (inboxSource, int64, int64, error) {
		var row, eventSeq int64
		src := inboxSource{kind: inboxRequest, needsAnswer: true}
		err := scan(&row, &src.room, &src.account, &src.actorAccount, &src.actor, &src.at, &eventSeq)
		src.subject = src.room
		return src, row, eventSeq, err
	})
}

// backfillRows reads one source's rows, then resolves and inserts each.
func backfillRows(ctx context.Context, tx *sql.Tx, rows *sql.Rows, after int64, read func(func(...any) error) (inboxSource, int64, int64, error)) (int64, int, int, error) {
	type item struct {
		src           inboxSource
		row, eventSeq int64
	}
	var items []item
	for rows.Next() {
		src, row, eventSeq, err := read(rows.Scan)
		if err != nil {
			rows.Close()
			return after, 0, 0, err
		}
		items = append(items, item{src, row, eventSeq})
	}
	if err := closeRows(rows); err != nil {
		return after, 0, 0, err
	}
	added := 0
	for _, it := range items {
		entries, err := concerned(ctx, tx, it.src)
		if err != nil {
			return after, 0, 0, err
		}
		n, err := addInboxEntries(ctx, tx, entries, it.eventSeq, it.src.at)
		if err != nil {
			return after, 0, 0, err
		}
		added += len(n)
		after = it.row
	}
	return after, len(items), added, nil
}

// backfillReceived derives the entries of receiver items since cutoff.
func backfillReceived(ctx context.Context, tx *sql.Tx, after, cutoff int64) (int64, int, int, error) {
	rows, err := tx.QueryContext(ctx, "SELECT seq,id,receiver,account,received_at,event_seq FROM receiver_items WHERE seq>? AND received_at>=? ORDER BY seq LIMIT ?", after, cutoff, inboxBackfillBatch)
	if err != nil {
		return after, 0, 0, err
	}
	return backfillRows(ctx, tx, rows, after, func(scan func(...any) error) (inboxSource, int64, int64, error) {
		var row, at, eventSeq int64
		var item, receiver, account string
		err := scan(&row, &item, &receiver, &account, &at, &eventSeq)
		return providerInboxSource(services.ReceivedInboxEntry(item, receiver, account, at)), row, eventSeq, err
	})
}

// backfillWakeups derives the entries of wake-up firings since cutoff.
func backfillWakeups(ctx context.Context, tx *sql.Tx, after, cutoff int64) (int64, int, int, error) {
	rows, err := tx.QueryContext(ctx, "SELECT seq,wakeup,account,kind,event,room,fired_at,event_seq,late FROM wakeup_notices WHERE seq>? AND fired_at>=? ORDER BY seq LIMIT ?", after, cutoff, inboxBackfillBatch)
	if err != nil {
		return after, 0, 0, err
	}
	return backfillRows(ctx, tx, rows, after, func(scan func(...any) error) (inboxSource, int64, int64, error) {
		var row, at, eventSeq int64
		var id, account, kind, event, room string
		var late bool
		err := scan(&row, &id, &account, &kind, &event, &room, &at, &eventSeq, &late)
		return providerInboxSource(services.WakeInboxEntry(id, account, kind, event, room, row, at, late)), row, eventSeq, err
	})
}

// providerInboxSource is a provider's entry as the resolver reads it.
func providerInboxSource(e services.InboxEntry) inboxSource {
	return inboxSource{kind: e.Kind, account: e.Account, subject: e.Subject, room: e.Room, detail: e.Detail, at: e.At}
}

// backfillWitnesses derives the entries of witnesses since cutoff.
func backfillWitnesses(ctx context.Context, tx *sql.Tx, after, cutoff int64) (int64, int, int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT w.seq,w.witness,coalesce(wi.account,w.witness),w.agent,i.account,w.kind,w.value,w.verdict,w.created_at,`+eventSeqAt("w.created_at")+`
 FROM link_witnesses w JOIN identities i ON i.id=w.agent LEFT JOIN identities wi ON wi.id=w.witness
 WHERE w.seq>? AND w.created_at>=? ORDER BY w.seq LIMIT ?`, after, cutoff, inboxBackfillBatch)
	if err != nil {
		return after, 0, 0, err
	}
	return backfillRows(ctx, tx, rows, after, func(scan func(...any) error) (inboxSource, int64, int64, error) {
		var row, eventSeq int64
		var agent, kind, value, verdict string
		src := inboxSource{kind: inboxWitness}
		err := scan(&row, &src.actor, &src.actorAccount, &agent, &src.account, &kind, &value, &verdict, &src.at, &eventSeq)
		src.subject = witnessInboxKey(src.actor, agent, kind, value, src.at)
		src.detail = map[string]any{"kind": kind, "verdict": verdict}
		return src, row, eventSeq, err
	})
}
