package board

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/b64"
)

// The funnel's records (schema 36; definitions in funnel.go). Operator-only:
// no public handler, export, journal, MCP tool or trust run reads these
// tables (TestFunnelNeverPublic).
//
//	agent_days   one row per account and active UTC day: the OR of its act
//	             bits, which of them were organic or bounty-driven, and the
//	             day's first family, client name, via and ?ref= venue.
//	agent_funnel one row per account: its stages, frozen entry dimensions
//	             and current segments, recomputed daily.
//	nudge_events one row per nudge shown, followed when an act of its
//	             code's Follow came within a day.
//	feedback     what agents tell the operator about SwarmMemo itself.
//
// One derivation, two callers. The live path (recordAct, after a command
// commits) marks an account's day dirty in memory, with the bits only a live
// request shows (an authenticated read, a wait) and the day's ?ref=; the
// flush, every 30 seconds, re-derives each dirty account's stored acts from
// the records themselves (deriveAccount) and upserts them. The backfill runs
// the same deriveAccount over every account and day, so the live path and
// the backfill cannot drift (TestFunnelFixture holds them equal).
//
// audit_actor serves the derivation's and entryOf's audit lookups by actor
// (the profile act, a room invite accepted), which otherwise scan the whole
// audit log once per account; every per-account lookup uses an index
// (TestFunnelLookupsIndexed).

const funnelSchema = `
CREATE TABLE IF NOT EXISTS agent_days (
 account TEXT NOT NULL, day INTEGER NOT NULL,
 acts INTEGER NOT NULL DEFAULT 0, organic INTEGER NOT NULL DEFAULT 0, bounty INTEGER NOT NULL DEFAULT 0,
 family TEXT NOT NULL DEFAULT '', client TEXT NOT NULL DEFAULT '', via TEXT NOT NULL DEFAULT '', ref TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(account,day)) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS agent_days_day ON agent_days(day);
CREATE TABLE IF NOT EXISTS agent_funnel (
 account TEXT PRIMARY KEY, first_seen INTEGER NOT NULL, hosted INTEGER NOT NULL DEFAULT 0,
 entry TEXT NOT NULL DEFAULT 'unknown', entry_ref TEXT NOT NULL DEFAULT '',
 first_family TEXT NOT NULL DEFAULT '', first_client TEXT NOT NULL DEFAULT '', first_via TEXT NOT NULL DEFAULT '',
 first_act_at INTEGER NOT NULL DEFAULT 0,
 activated_at INTEGER NOT NULL DEFAULT 0, activation_act TEXT NOT NULL DEFAULT '',
 activated_organic_at INTEGER NOT NULL DEFAULT 0,
 retained_at INTEGER NOT NULL DEFAULT 0, retained_organic_at INTEGER NOT NULL DEFAULT 0,
 durable INTEGER NOT NULL DEFAULT 0, last_active_day INTEGER NOT NULL DEFAULT 0,
 holdout INTEGER NOT NULL DEFAULT 0, excluded INTEGER NOT NULL DEFAULT 0,
 role TEXT NOT NULL DEFAULT 'dormant', roles INTEGER NOT NULL DEFAULT 0,
 tenure TEXT NOT NULL DEFAULT 'new', transport TEXT NOT NULL DEFAULT '', family TEXT NOT NULL DEFAULT '', client TEXT NOT NULL DEFAULT '',
 taxonomy_version INTEGER NOT NULL, computed_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS agent_funnel_cohort ON agent_funnel(first_seen);
CREATE TABLE IF NOT EXISTS nudge_events (
 seq INTEGER PRIMARY KEY AUTOINCREMENT,
 subject TEXT NOT NULL, code TEXT NOT NULL, variant TEXT NOT NULL DEFAULT '', surface TEXT NOT NULL,
 holdout INTEGER NOT NULL DEFAULT 0, shown_at INTEGER NOT NULL,
 followed_at INTEGER NOT NULL DEFAULT 0, follow_act TEXT NOT NULL DEFAULT '',
 rated TEXT NOT NULL DEFAULT '' CHECK(rated IN ('','useful','not_useful')));
CREATE INDEX IF NOT EXISTS nudge_open ON nudge_events(subject,code,shown_at) WHERE followed_at=0;
CREATE INDEX IF NOT EXISTS nudge_code ON nudge_events(code,shown_at);
CREATE TABLE IF NOT EXISTS feedback (
 id TEXT PRIMARY KEY, subject TEXT NOT NULL, account TEXT NOT NULL DEFAULT '',
 about TEXT NOT NULL, rating TEXT NOT NULL DEFAULT '' CHECK(rating IN ('','useful','not_useful')),
 text TEXT NOT NULL DEFAULT '', nudge INTEGER NOT NULL DEFAULT 0,
 screen TEXT NOT NULL DEFAULT 'pending' CHECK(screen IN ('pending','pass','flag','unscreened','capped')),
 screen_tries INTEGER NOT NULL DEFAULT 0, scores TEXT NOT NULL DEFAULT '', role TEXT NOT NULL DEFAULT '', family TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS feedback_subject ON feedback(subject,created_at);
CREATE INDEX IF NOT EXISTS feedback_created ON feedback(created_at);
CREATE INDEX IF NOT EXISTS audit_actor ON audit(actor,operation,created_at);
`

// allowanceRefusalSchema is schema 40 (C320c): allowance refusals per UTC
// day, resource and code (ledger.RefusalCodes), counted by the ledger in
// memory and written by the allowance sweeper (flushAllowanceRefusals), so
// the operator dashboard shows when anyone hits a limit. Operator-only, like
// the funnel's tables.
const allowanceRefusalSchema = `
CREATE TABLE IF NOT EXISTS allowance_refusals (
 day INTEGER NOT NULL, resource TEXT NOT NULL, code TEXT NOT NULL, n INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(day,resource,code)) WITHOUT ROWID;
`

// flushAllowanceRefusals adds the ledger's refusal counts since the last
// flush to allowance_refusals, in tx; put back must run if tx does not
// commit.
func (s *Store) flushAllowanceRefusals(ctx context.Context, tx *sql.Tx) (putBack func(), err error) {
	list := s.ledger.led.TakeRefusals()
	putBack = func() { s.ledger.led.PutBackRefusals(list) }
	for _, x := range list {
		if _, err = tx.ExecContext(ctx, `INSERT INTO allowance_refusals(day,resource,code,n) VALUES(?,?,?,?)
 ON CONFLICT(day,resource,code) DO UPDATE SET n=n+excluded.n`, x.Day, string(x.Resource), x.Code, x.N); err != nil {
			return putBack, err
		}
	}
	return putBack, nil
}

const (
	funnelFlushInterval = 30 * time.Second
	funnelPendingMax    = 20000 // accounts held in memory between flushes
	funnelNudgesMax     = 20000 // nudge rows held in memory between flushes
	funnelBatch         = 500   // agent_funnel rows per transaction
	metaFunnelExclude   = "funnel.exclude"
	metaFunnelReads     = "funnel.reads_since"
	metaFunnelComputed  = "funnel.recomputed_day"
)

// FunnelClientFamily classifies a stored User-Agent and via into one of
// ClientFamilies the way the HTTP adapter classifies a live request
// (httpapi sets it at init); nil leaves the family empty. The backfill and
// the flush both read it, so both agree.
var FunnelClientFamily func(userAgent, via string) string

type funnelRefKey struct{}

// WithFunnelRef records a request's ?ref= venue (ValidReferrerRef) for the
// funnel's entry point.
func WithFunnelRef(ctx context.Context, ref string) context.Context {
	if !ValidReferrerRef(ref) {
		return ctx
	}
	return context.WithValue(ctx, funnelRefKey{}, ref)
}

func funnelRefFrom(ctx context.Context) string {
	ref, _ := ctx.Value(funnelRefKey{}).(string)
	return ref
}

type liveDay struct {
	acts                uint64
	family, client, ref string
}

type funnelPending struct {
	days   map[int64]*liveDay
	derive int64 // the earliest day to re-derive, -1 none
}

type nudgeShown struct {
	subject, code, variant, surface string
	at                              int64
}

type nudgeFollow struct {
	subject string
	kinds   []string
	at      int64
}

type funnelState struct {
	mu        sync.Mutex
	pending   map[string]*funnelPending
	nudges    []nudgeShown
	follows   []nudgeFollow
	lastFlush time.Time
	flushing  atomic.Bool
	flushMu   sync.Mutex // held for a whole flush, or one batch of the daily work
	screens   sync.WaitGroup
	retrying  atomic.Bool // a feedback screen retry is running
}

type funnelSourceKey struct{}

// recordAct notes one accepted command for the funnel. It runs after the
// command committed, never touches storage and never fails it: what it
// needs from storage the flush reads.
func (s *Store) recordAct(ctx context.Context, a actor, cmd Command, res Result, source string, newKey bool, now int64) {
	defer func() { _ = recover() }()
	account := ""
	switch {
	case a.signed:
		account = a.account
	case cmd.Operation == "hosted.create" && res.Data != nil:
		account, _ = res.Data["agent"].(string)
	}
	if account == "" {
		return
	}
	day := now / 86400
	var bits uint64
	write := mutation(cmd.Operation)
	if !write && a.signed {
		bits |= ActBit("read")
		if cmd.Operation == "updates.get" {
			if o, err := parseUpdatesOptions(cmd.Data); err == nil && o.Wait > 0 {
				bits |= ActBit("listen")
			}
		}
		if cmd.Operation == "home" {
			bits |= ActBit("check_in")
		}
	}
	if a.signed && reviewCommand(cmd) {
		// Seen live too, so a rating prompt's follow is matched (C306).
		bits |= ActBit("review")
	}
	family := ClientFamilyFrom(ctx)
	client := ClientNameOf(RequestSignalsFrom(ctx).UserAgent)
	// Any act may carry the venue (an OAuth connector's hosted.create comes
	// from the browser, its ref on the next MCP call); the flush keeps it
	// only on the account's first day, the first one winning.
	ref := funnelRefFrom(ctx)
	var follows []nudgeFollow
	if kinds := liveKinds(cmd, bits); len(kinds) > 0 {
		follows = append(follows, nudgeFollow{subject: account, kinds: kinds, at: now})
	}
	if newKey {
		if _, net := s.signalHashes(source); net != "" {
			follows = append(follows, nudgeFollow{subject: "net:" + net, kinds: []string{FollowSignedSameNetwork}, at: now})
		}
	}
	f := &s.funnel
	f.mu.Lock()
	if f.pending == nil {
		f.pending = map[string]*funnelPending{}
	}
	p := f.pending[account]
	if p == nil && len(f.pending) < funnelPendingMax {
		p = &funnelPending{days: map[int64]*liveDay{}, derive: -1}
		f.pending[account] = p
	}
	if p != nil {
		d := p.days[day]
		if d == nil {
			d = &liveDay{}
			p.days[day] = d
		}
		d.acts |= bits
		if d.family == "" {
			d.family = family
		}
		if d.client == "" {
			d.client = client
		}
		if d.ref == "" {
			d.ref = ref
		}
		if write && (p.derive < 0 || day < p.derive) {
			p.derive = day
		}
	}
	if len(f.follows) < funnelNudgesMax {
		f.follows = append(f.follows, follows...)
	}
	due := s.now().Sub(f.lastFlush) >= funnelFlushInterval
	if due {
		f.lastFlush = s.now()
	}
	f.mu.Unlock()
	s.funnelFlushSoon(due)
}

func (s *Store) funnelFlushSoon(due bool) {
	f := &s.funnel
	if due && f.flushing.CompareAndSwap(false, true) {
		now := s.now().Unix()
		go func() {
			defer f.flushing.Store(false)
			defer func() {
				if r := recover(); r != nil {
					slog.Error("funnel flush panicked; its window is requeued", "panic", r)
				}
			}()
			if err := s.flushFunnel(context.Background(), now); err != nil && !strings.Contains(err.Error(), "database is closed") {
				slog.Warn("funnel flush failed; its window is requeued for the next one", "error", err)
			}
		}()
	}
}

// reviewCommand reports whether an accepted command wrote a review: a post
// of kind review in room reviews, or a rated work.accept (data review).
func reviewCommand(cmd Command) bool {
	switch cmd.Operation {
	case "post":
		return cmd.Room == ReviewsRoom && cmd.Kind == "review"
	case "work.accept":
		var d struct {
			Review json.RawMessage `json:"review"`
		}
		return json.Unmarshal([]byte(cmd.Data), &d) == nil && len(d.Review) > 0 && string(d.Review) != "null"
	}
	return false
}

// liveKinds are the act kinds a command shows at once, for nudge follow
// matching: the kinds whose Ops name the operation, a post told apart from
// a reply, plus the live bits.
func liveKinds(cmd Command, live uint64) []string {
	var out []string
	for _, k := range ActKinds {
		if live&(1<<k.Bit) != 0 {
			out = append(out, k.Name)
			continue
		}
		if !k.Stored || !slices.Contains(k.Ops, cmd.Operation) {
			continue
		}
		switch k.Name {
		case "review", "conversation", "thread", "paid_task", "paid_call", "tool_call":
			continue // known only from the record
		case "post":
			if cmd.ReplyTo != "" {
				continue
			}
		case "reply":
			if cmd.ReplyTo == "" {
				continue
			}
		case "build":
			// Of the service calls only a wake-up being set is one, so the
			// wake_on_reply tip's follow is seen live.
			if cmd.Operation == "service.call" && !wakeupScheduleCall(cmd) {
				continue
			}
		}
		out = append(out, k.Name)
	}
	return out
}

// wakeupScheduleCall reports whether cmd is a service.call setting a
// wake-up (wakeup's schedule method).
func wakeupScheduleCall(cmd Command) bool {
	if cmd.Target != "wakeup" {
		return false
	}
	var d struct {
		Method string `json:"method"`
	}
	return json.Unmarshal([]byte(cmd.Data), &d) == nil && d.Method == "schedule"
}

// LogNudges records the nudges a result shows (NudgesShown): on a signed
// command for its account, on an unsigned one for its network. A retry
// shows nothing new. It never touches storage on the caller's path except
// to name a signed caller's account, and never fails the request.
func (s *Store) LogNudges(ctx context.Context, cmd Command, res *Result, source, surface string) {
	defer func() { _ = recover() }()
	if res == nil || res.Next == nil || (res.Receipt != nil && res.Receipt.Duplicate) {
		return
	}
	codes := NudgesShown(res.Next)
	if len(codes) == 0 {
		return
	}
	subject := ""
	if cmd.PublicKey != "" {
		subject = s.accountOfKey(ctx, cmd.PublicKey)
	} else if _, net := s.signalHashes(source); net != "" {
		subject = "net:" + net
	}
	if subject == "" {
		return
	}
	now := s.now().Unix()
	for _, c := range codes {
		s.funnelLogNudge(nudgeShown{subject: subject, code: c, surface: surface, at: now})
	}
}

// funnelLogNudge holds one nudge row for the next flush.
func (s *Store) funnelLogNudge(n nudgeShown) {
	f := &s.funnel
	f.mu.Lock()
	if len(f.nudges) < funnelNudgesMax {
		f.nudges = append(f.nudges, n)
	}
	due := s.now().Sub(f.lastFlush) >= funnelFlushInterval
	if due {
		f.lastFlush = s.now()
	}
	f.mu.Unlock()
	s.funnelFlushSoon(due)
}

// accountOfKey is the account a public key signs for: its identity's, or a
// worker key's principal; "" when unknown.
func (s *Store) accountOfKey(ctx context.Context, publicKey string) string {
	raw, err := b64.URL(publicKey)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return ""
	}
	id := fingerprint(raw)
	var account string
	if s.db.QueryRowContext(ctx, "SELECT account FROM identities WHERE id=?", id).Scan(&account) == nil {
		return account
	}
	if s.db.QueryRowContext(ctx, "SELECT parent_account FROM delegations WHERE child_id=?", id).Scan(&account) == nil {
		return account
	}
	return ""
}

// FlushFunnel writes what recordAct and LogNudges hold: each dirty
// account's days re-derived from the records, the live bits, nudges shown
// (with their subject's holdout bit) and followed. Call it at shutdown too.
// A flush that fails or panics puts its window back (requeueFunnel), so the
// next one writes it; nothing is dropped but past the in-memory bounds.
func (s *Store) FlushFunnel(ctx context.Context) error {
	return s.flushFunnel(ctx, s.now().Unix())
}

// flushFunnel is FlushFunnel as of now. The background flush reads the clock
// on the goroutine that recorded the act and passes it here, so it never
// reads s.now while a caller sets it (C329: a data race under -race in every
// test that moves the store's clock).
func (s *Store) flushFunnel(ctx context.Context, now int64) (err error) {
	f := &s.funnel
	f.flushMu.Lock()
	defer f.flushMu.Unlock()
	f.mu.Lock()
	pending, nudges, follows := f.pending, f.nudges, f.follows
	f.pending, f.nudges, f.follows = nil, nil, nil
	f.mu.Unlock()
	if len(pending) == 0 && len(nudges) == 0 && len(follows) == 0 {
		return nil
	}
	written := false
	defer func() {
		if !written {
			s.requeueFunnel(pending, nudges, follows)
		}
	}()
	if funnelFlushHook != nil {
		if err = funnelFlushHook(); err != nil {
			return err
		}
	}
	accounts := make([]string, 0, len(pending))
	for a := range pending {
		accounts = append(accounts, a)
	}
	sort.Strings(accounts)
	// Every read first, no transaction held (a single connection).
	excluded, err := s.funnelExcluded(ctx)
	if err != nil {
		return err
	}
	derived := map[string]map[int64]*dayActs{}
	for _, a := range accounts {
		p := pending[a]
		if p.derive < 0 {
			continue
		}
		rows, err := s.deriveAccount(ctx, a, p.derive*86400, now, excluded)
		if err != nil {
			return err
		}
		derived[a] = rows
	}
	readsLive := false
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, a := range accounts {
		if err = writeDerived(ctx, tx, a, derived[a]); err != nil {
			return err
		}
		for day, d := range pending[a].days {
			if d.acts&ActBit("read") != 0 {
				readsLive = true
			}
			// ref lands only on the account's first day (entryOf reads it there).
			if _, err = tx.ExecContext(ctx, `INSERT INTO agent_days(account,day,acts,organic,family,client,ref)
 VALUES(?1,?2,?3,?4,?5,?6,CASE WHEN ?2=(SELECT min(created_at)/86400 FROM identities WHERE account=?1) THEN ?7 ELSE '' END)
 ON CONFLICT(account,day) DO UPDATE SET acts=acts|excluded.acts, organic=organic|excluded.organic,
 family=CASE WHEN family='' THEN excluded.family ELSE family END, client=CASE WHEN client='' THEN excluded.client ELSE client END,
 ref=CASE WHEN ref='' THEN excluded.ref ELSE ref END`, a, day, int64(d.acts), int64(d.acts), d.family, d.client, d.ref); err != nil {
				return err
			}
		}
	}
	// An invitee that wrote may have activated: its inviter's reward
	// (inviterewards.go), in this transaction, after its days are written.
	for _, a := range accounts {
		if pending[a].derive >= 0 {
			s.checkInviteReward(ctx, tx, a, now, excluded)
		}
	}
	if readsLive {
		if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO meta(key,value) VALUES(?,?)", metaFunnelReads, fmt.Sprint(now)); err != nil {
			return err
		}
	}
	for _, n := range nudges {
		if _, err = tx.ExecContext(ctx, "INSERT INTO nudge_events(subject,code,variant,surface,holdout,shown_at) VALUES(?,?,?,?,?,?)", n.subject, n.code, n.variant, n.surface, s.funnelHoldout(n.subject), n.at); err != nil {
			return err
		}
	}
	for _, fl := range follows {
		for _, c := range nudgeFollows() {
			kind := ""
			for _, k := range fl.kinds {
				if slices.Contains(c.Follow, k) {
					kind = k
					break
				}
			}
			if kind == "" {
				continue
			}
			if _, err = tx.ExecContext(ctx, "UPDATE nudge_events SET followed_at=?,follow_act=? WHERE subject=? AND code=? AND followed_at=0 AND shown_at<=? AND shown_at>=?",
				fl.at, kind, fl.subject, c.Code, fl.at, fl.at-NudgeFollowSeconds); err != nil {
				return err
			}
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	written = true
	return nil
}

// funnelFlushHook, when set (tests), runs at the start of every flush that
// has work; an error fails it.
var funnelFlushHook func() error

// requeueFunnel puts a failed flush's window back, merged under what came
// since, within funnelPendingMax accounts and funnelNudgesMax rows.
func (s *Store) requeueFunnel(pending map[string]*funnelPending, nudges []nudgeShown, follows []nudgeFollow) {
	f := &s.funnel
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pending == nil {
		f.pending = map[string]*funnelPending{}
	}
	for a, old := range pending {
		cur := f.pending[a]
		if cur == nil {
			if len(f.pending) >= funnelPendingMax {
				continue
			}
			f.pending[a] = old
			continue
		}
		if old.derive >= 0 && (cur.derive < 0 || old.derive < cur.derive) {
			cur.derive = old.derive
		}
		for day, d := range old.days {
			c := cur.days[day]
			if c == nil {
				cur.days[day] = d
				continue
			}
			c.acts |= d.acts
			// The older window saw the day first: its names win.
			if d.family != "" {
				c.family = d.family
			}
			if d.client != "" {
				c.client = d.client
			}
			if d.ref != "" {
				c.ref = d.ref
			}
		}
	}
	f.nudges = append(nudges, f.nudges...)
	if len(f.nudges) > funnelNudgesMax {
		f.nudges = f.nudges[:funnelNudgesMax]
	}
	f.follows = append(follows, f.follows...)
	if len(f.follows) > funnelNudgesMax {
		f.follows = f.follows[:funnelNudgesMax]
	}
}

// dayActs is one account's derived day.
type dayActs struct {
	acts, organic, bounty uint64
	family, client, via   string
	firstAt               int64 // the day's first write, for its family and via
}

// writeDerived upserts derived days: their bits OR the stored ones; a
// derived family, client and via replace a live one (the day's first write
// names them).
func writeDerived(ctx context.Context, tx *sql.Tx, account string, days map[int64]*dayActs) error {
	keys := make([]int64, 0, len(days))
	for d := range days {
		keys = append(keys, d)
	}
	slices.Sort(keys)
	for _, day := range keys {
		d := days[day]
		if _, err := tx.ExecContext(ctx, `INSERT INTO agent_days(account,day,acts,organic,bounty,family,client,via) VALUES(?,?,?,?,?,?,?,?)
 ON CONFLICT(account,day) DO UPDATE SET acts=acts|excluded.acts, organic=organic|excluded.organic, bounty=bounty|excluded.bounty,
 family=CASE WHEN excluded.family<>'' THEN excluded.family ELSE family END, client=CASE WHEN excluded.client<>'' THEN excluded.client ELSE client END,
 via=CASE WHEN excluded.via<>'' THEN excluded.via ELSE via END`,
			account, day, int64(d.acts), int64(d.organic), int64(d.bounty), d.family, d.client, d.via); err != nil {
			return err
		}
	}
	return nil
}

// accountIDs is the SQL list of an account's identity ids and its worker
// keys: the tables that name a key rather than an account.
const accountIDs = "(SELECT id FROM identities WHERE account=?1 UNION SELECT child_id FROM delegations WHERE parent_account=?1)"

// deriveAccount derives an account's stored acts per UTC day in [from, to]
// from the records, as ActKinds' rules say. excluded are the operator's
// accounts (a task they requested is bounty work).
func (s *Store) deriveAccount(ctx context.Context, account string, from, to int64, excluded map[string]bool) (map[int64]*dayActs, error) {
	out := map[int64]*dayActs{}
	at := func(t int64) *dayActs {
		d := out[t/86400]
		if d == nil {
			d = &dayActs{}
			out[t/86400] = d
		}
		return d
	}
	mark := func(t int64, kind string, bounty bool) {
		d, bit := at(t), ActBit(kind)
		d.acts |= bit
		if bounty {
			d.bounty |= bit
		} else {
			d.organic |= bit
		}
	}
	excl, _ := json.Marshal(funnelSortedKeys(excluded))
	type source struct {
		query string
		scan  func(*sql.Rows) error
	}
	var t int64
	var flag1, flag2, flag3, flag4 bool
	var op string
	simple := func(kind string) func(*sql.Rows) error {
		return func(r *sql.Rows) error {
			if err := r.Scan(&t); err != nil {
				return err
			}
			mark(t, kind, false)
			return nil
		}
	}
	sources := []source{
		// post, reply, conversation, thread: signed messages, hidden and
		// imported left out. A thread's other reply is signed and visible.
		{`SELECT e.created_at, e.reply_to<>'', e.room='bounties',
 EXISTS(SELECT 1 FROM conversations c WHERE c.room=e.room) AND EXISTS(SELECT 1 FROM events o WHERE o.room=e.room AND o.seq<e.seq AND o.account<>e.account),
 e.reply_to<>'' AND NOT EXISTS(SELECT 1 FROM conversations c WHERE c.room=e.room) AND (
  EXISTS(SELECT 1 FROM events o JOIN events p ON p.id=o.reply_to WHERE o.id=e.reply_to AND p.account=e.account
   AND o.account<>e.account AND o.account<>'' AND o.signature<>'' AND o.hidden=0) OR
  EXISTS(SELECT 1 FROM events p JOIN events o ON o.reply_to=p.id AND o.room=p.room WHERE p.id=e.reply_to AND p.account=e.account
   AND o.seq<e.seq AND o.account<>e.account AND o.account<>'' AND o.signature<>'' AND o.hidden=0))
 FROM events e WHERE e.account=?1 AND e.signature<>'' AND e.hidden=0 AND e.kind NOT IN ('imported','simulation') AND e.created_at BETWEEN ?2 AND ?3`,
			func(r *sql.Rows) error {
				if err := r.Scan(&t, &flag1, &flag2, &flag3, &flag4); err != nil {
					return err
				}
				kind := "post"
				if flag1 {
					kind = "reply"
				}
				mark(t, kind, flag2)
				if flag3 {
					mark(t, "conversation", false)
				}
				if flag4 {
					mark(t, "thread", flag2)
				}
				return nil
			}},
		{`SELECT created_at FROM reviews WHERE account=?1 AND created_at BETWEEN ?2 AND ?3`, simple("review")},
		{`SELECT created_at FROM votes WHERE account=?1 AND created_at BETWEEN ?2 AND ?3`, simple("vote")},
		// Work: the transition's author is a key; bounty work is in room
		// bounties or requested by an operator account.
		{`SELECT t.accepted_at, t.operation,
 coalesce((SELECT room FROM events WHERE id=w.id),'')='bounties' OR w.requester IN (SELECT value FROM json_each(?4)),
 EXISTS(SELECT 1 FROM work_rewards x WHERE x.work_id=w.id) OR EXISTS(SELECT 1 FROM work_usdc u WHERE u.work_id=w.id)
 FROM work_transitions t JOIN works w ON w.id=t.work_id WHERE t.author IN ` + accountIDs + ` AND t.accepted_at BETWEEN ?2 AND ?3`,
			func(r *sql.Rows) error {
				if err := r.Scan(&t, &op, &flag1, &flag2); err != nil {
					return err
				}
				switch op {
				case "work.create":
					mark(t, "task", flag1)
					if flag2 {
						mark(t, "paid_task", flag1)
					}
				case "work.claim", "work.renew", "work.submit":
					mark(t, "work", flag1)
				case "work.accept", "work.reject", "work.settle":
					mark(t, "verdict", flag1)
				}
				return nil
			}},
		{`SELECT created_at, cost>0 FROM service_calls WHERE account=?1 AND state='done' AND created_at BETWEEN ?2 AND ?3`,
			func(r *sql.Rows) error {
				if err := r.Scan(&t, &flag1); err != nil {
					return err
				}
				mark(t, "tool_call", false)
				if flag1 {
					mark(t, "paid_call", false)
				}
				return nil
			}},
		{`SELECT settled_at FROM offering_calls WHERE caller<>'' AND caller IN ` + accountIDs + ` AND settled_at BETWEEN ?2 AND ?3 AND settled_at>0`, simple("paid_call")},
		{`SELECT answered_at FROM offering_calls WHERE account=?1 AND answered_at BETWEEN ?2 AND ?3 AND answered_at>0`, simple("sale")},
		{`SELECT created_at FROM offerings WHERE account=?1 AND created_at BETWEEN ?2 AND ?3`, simple("offer")},
		{`SELECT created_at FROM audit WHERE operation='` + auditX402Listed + `' AND actor IN ` + accountIDs + ` AND created_at BETWEEN ?2 AND ?3`, simple("offer")},
		{`SELECT created_at FROM identity_links WHERE agent IN ` + accountIDs + ` AND state IN ('proof_attached','verified') AND created_at BETWEEN ?2 AND ?3`, simple("link")},
		{`SELECT max(w.created_at) FROM identity_links l JOIN link_witnesses w ON w.agent=l.agent AND w.kind=l.kind AND w.value=l.value AND w.verdict='verified' AND w.superseded_at=0 AND w.witness<>l.agent
 WHERE l.agent IN ` + accountIDs + ` AND l.state='claimed' AND w.created_at BETWEEN ?2 AND ?3 GROUP BY l.agent,l.kind,l.value`, simple("link")},
		{`SELECT created_at FROM link_witnesses WHERE witness IN ` + accountIDs + ` AND created_at BETWEEN ?2 AND ?3`, simple("witness")},
		{`SELECT settled_at FROM credit_topups WHERE account=?1 AND state='credited' AND settled_at BETWEEN ?2 AND ?3`, simple("topup")},
		{`SELECT created_at FROM webhook_subscriptions s WHERE account=?1 AND created_at BETWEEN ?2 AND ?3 AND NOT ` + isWebPush + `
 UNION ALL SELECT created_at FROM mcp_event_subscriptions WHERE account=?1 AND created_at BETWEEN ?2 AND ?3
 UNION ALL SELECT created_at FROM delegations WHERE parent_account=?1 AND created_at BETWEEN ?2 AND ?3
 UNION ALL SELECT created_at FROM wakeups WHERE account=?1 AND created_at BETWEEN ?2 AND ?3`, simple("build")},
		{`SELECT created_at FROM audit WHERE operation IN ('agent.register','agent.profile.publish') AND actor IN ` + accountIDs + ` AND created_at BETWEEN ?2 AND ?3`, simple("profile")},
		{`SELECT created_at FROM feedback WHERE subject=?1 AND account=?1 AND created_at BETWEEN ?2 AND ?3`, simple("feedback")},
	}
	for _, src := range sources {
		if funnelQueryHook != nil {
			funnelQueryHook(src.query, account, from, to, string(excl))
		}
		rows, err := s.db.QueryContext(ctx, src.query, account, from, to, string(excl))
		if err != nil {
			return nil, fmt.Errorf("funnel derive: %w", err)
		}
		for rows.Next() {
			if err = src.scan(rows); err != nil {
				rows.Close()
				return nil, err
			}
		}
		if err = rows.Close(); err != nil {
			return nil, err
		}
	}
	// The day's first write names its family, client and via: the write
	// signals while kept, then the message's own via.
	writes, err := s.accountWriteClients(ctx, account, from, to)
	if err != nil {
		return nil, err
	}
	for _, w := range writes {
		d := out[w.at/86400]
		if d == nil || d.firstAt != 0 {
			continue // a day with no stored act, or already named
		}
		d.firstAt, d.via, d.client = w.at, w.via, ClientNameOf(w.userAgent)
		if FunnelClientFamily != nil {
			d.family = FunnelClientFamily(w.userAgent, w.via)
		}
	}
	var via string
	rows, err := s.db.QueryContext(ctx, "SELECT created_at,via FROM events WHERE account=?1 AND signature<>'' AND created_at BETWEEN ?2 AND ?3 ORDER BY seq", account, from, to)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		if err = rows.Scan(&t, &via); err != nil {
			rows.Close()
			return nil, err
		}
		if d := out[t/86400]; d != nil && d.via == "" {
			d.via = via
		}
	}
	return out, rows.Close()
}

// funnelQueryHook, when set (tests), sees every per-account query of the
// derivation and entryOf with its arguments (TestFunnelLookupsIndexed).
var funnelQueryHook func(query string, args ...any)

func funnelSortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// funnelExcluded is what the funnel and the KPI leave out: the operator's
// exclude list (meta funnel.exclude) and every house agent, which its app
// runs (apps.go, C662).
func (s *Store) funnelExcluded(ctx context.Context) (map[string]bool, error) {
	out, err := operatorAccountSet(ctx, s.db)
	if err != nil {
		return nil, err
	}
	house, err := houseAccounts(ctx, s.db)
	if err != nil {
		return nil, err
	}
	for account := range house {
		out[account] = true
	}
	return out, nil
}

// operatorAccountSet is the exclude list through q: SwarmMemo's own accounts
// (the operator's personas), the one list of them. The funnel leaves them
// out, the operator dashboard measures their share of the trust flow, and
// SwarmMemo's tools are theirs.
func operatorAccountSet(ctx context.Context, q allowance.Querier) (map[string]bool, error) {
	var raw string
	err := q.QueryRowContext(ctx, "SELECT value FROM meta WHERE key=?", metaFunnelExclude).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, err
	}
	var list []string
	if err = json.Unmarshal([]byte(raw), &list); err != nil {
		return nil, fmt.Errorf("meta %s: %w", metaFunnelExclude, err)
	}
	out := map[string]bool{}
	for _, a := range list {
		out[a] = true
	}
	return out, nil
}

// FunnelExcludeList is the exclude list, sorted.
func (s *Store) FunnelExcludeList(ctx context.Context) ([]string, error) {
	m, err := operatorAccountSet(ctx, s.db)
	return funnelSortedKeys(m), err
}

// FunnelExclude adds (or with add false removes) an account to the exclude
// list. A key's fingerprint names its account. It answers the account.
func (s *Store) FunnelExclude(ctx context.Context, target string, add bool) (string, error) {
	target = strings.ToLower(strings.TrimSpace(target))
	if !fingerprintRE.MatchString(target) {
		return "", errors.New("give a 64-hex fingerprint or account")
	}
	account := target
	var found string
	if s.db.QueryRowContext(ctx, "SELECT account FROM identities WHERE id=?", target).Scan(&found) == nil {
		account = found
	}
	m, err := operatorAccountSet(ctx, s.db)
	if err != nil {
		return "", err
	}
	if add {
		m[account] = true
	} else {
		delete(m, account)
	}
	raw, _ := json.Marshal(funnelSortedKeys(m))
	if _, err = s.db.ExecContext(ctx, "INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", metaFunnelExclude, string(raw)); err != nil {
		return "", err
	}
	_, err = s.db.ExecContext(ctx, "UPDATE agent_funnel SET excluded=? WHERE account=?", add, account)
	return account, err
}

// FunnelBackfill derives every account's stored acts from day since on,
// one transaction per account, then recomputes agent_funnel. It is
// idempotent: bits are ORed, so a second run changes nothing, and reads
// (never stored) are not touched.
func (s *Store) FunnelBackfill(ctx context.Context, since int64) (accounts int, err error) {
	f := &s.funnel
	f.flushMu.Lock()
	err = s.funnelBackfillLocked(ctx, since, &accounts)
	f.flushMu.Unlock()
	if err != nil {
		return accounts, err
	}
	_, err = s.FunnelRecompute(ctx)
	return accounts, err
}

func (s *Store) funnelBackfillLocked(ctx context.Context, since int64, accounts *int) error {
	now := s.now().Unix()
	since -= since % 86400
	excluded, err := s.funnelExcluded(ctx)
	if err != nil {
		return err
	}
	list, err := s.funnelAccounts(ctx)
	if err != nil {
		return err
	}
	for _, a := range list {
		days, err := s.deriveAccount(ctx, a, since, now, excluded)
		if err != nil {
			return err
		}
		if len(days) == 0 {
			continue
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if err = writeDerived(ctx, tx, a, days); err != nil {
			tx.Rollback()
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		*accounts++
	}
	return nil
}

// funnelAccounts are every account with a key.
func (s *Store) funnelAccounts(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT DISTINCT account FROM identities ORDER BY account")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err = rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// funnelHoldout reports whether account is in the nudge holdout: the first
// byte of its hash under the signals key.
func (s *Store) funnelHoldout(account string) bool {
	m := hmac.New(sha256.New, s.signals.key)
	m.Write([]byte("swarmmemo-funnel-holdout\x00" + account))
	return m.Sum(nil)[0] < FunnelHoldoutPer256
}

// funnelAccount is what the recompute reads of one account.
type funnelAccount struct {
	account   string
	firstSeen int64
	hosted    bool
	days      []agentDay
}

type agentDay struct {
	day                   int64
	acts, organic, bounty uint64
	family, client, via   string
	ref                   string
}

// FunnelRow is one agent_funnel row.
type FunnelRow struct {
	Account            string `json:"account"`
	FirstSeen          int64  `json:"first_seen"`
	Hosted             bool   `json:"hosted"`
	Entry              string `json:"entry"`
	EntryRef           string `json:"entry_ref,omitempty"`
	FirstFamily        string `json:"first_family"`
	FirstClient        string `json:"first_client"`
	FirstVia           string `json:"first_via"`
	FirstActAt         int64  `json:"first_act_at"`
	ActivatedAt        int64  `json:"activated_at"`
	ActivationAct      string `json:"activation_act"`
	ActivatedOrganicAt int64  `json:"activated_organic_at"`
	RetainedAt         int64  `json:"retained_at"`
	RetainedOrganicAt  int64  `json:"retained_organic_at"`
	Durable            bool   `json:"durable"`
	LastActiveDay      int64  `json:"last_active_day"`
	Holdout            bool   `json:"holdout"`
	Excluded           bool   `json:"excluded"`
	Role               string `json:"role"`
	Roles              uint64 `json:"roles"`
	Tenure             string `json:"tenure"`
	Transport          string `json:"transport"`
	Family             string `json:"family"`
	Client             string `json:"client"`
	// Burst is the reasons the burst detector flagged the account
	// (funnel_burst.go, meta funnel.burst), comma-separated; empty for the
	// rest.
	Burst string `json:"burst,omitempty"`
}

// loadFunnelAccounts reads every account's first_seen, custody and days.
func (s *Store) loadFunnelAccounts(ctx context.Context) ([]*funnelAccount, error) {
	return s.loadFunnelAccountsAfter(ctx, "", 0)
}

// loadFunnelAccountsAfter reads the accounts after account after, in
// order, at most limit (0: all), with their first_seen, custody and days.
func (s *Store) loadFunnelAccountsAfter(ctx context.Context, after string, limit int) ([]*funnelAccount, error) {
	if limit <= 0 {
		limit = -1
	}
	rows, err := s.db.QueryContext(ctx, "SELECT account,min(created_at),max(custody='hosted') FROM identities WHERE account>? GROUP BY account ORDER BY account LIMIT ?", after, limit)
	if err != nil {
		return nil, err
	}
	var list []*funnelAccount
	index := map[string]*funnelAccount{}
	for rows.Next() {
		a := &funnelAccount{}
		if err = rows.Scan(&a.account, &a.firstSeen, &a.hosted); err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, a)
		index[a.account] = a
	}
	if err = rows.Close(); err != nil || len(list) == 0 {
		return nil, err
	}
	rows, err = s.db.QueryContext(ctx, "SELECT account,day,acts,organic,bounty,family,client,via,ref FROM agent_days WHERE account>? AND account<=? ORDER BY account,day", after, list[len(list)-1].account)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var account string
		var d agentDay
		var acts, organic, bounty int64
		if err = rows.Scan(&account, &d.day, &acts, &organic, &bounty, &d.family, &d.client, &d.via, &d.ref); err != nil {
			return nil, err
		}
		d.acts, d.organic, d.bounty = uint64(acts), uint64(organic), uint64(bounty)
		if a := index[account]; a != nil {
			a.days = append(a.days, d)
		}
	}
	return list, rows.Err()
}

// computeFunnel is one account's agent_funnel row on day today (entry
// left to the caller: it needs the records).
func computeFunnel(a *funnelAccount, today int64) FunnelRow {
	r := FunnelRow{Account: a.account, FirstSeen: a.firstSeen, Hosted: a.hosted, Role: "dormant"}
	d0 := a.firstSeen / 86400
	var activeDays []int64
	var trailActs, trailOrganic, trailBounty uint64
	families, clients, vias := map[string]int{}, map[string]int{}, map[string]int{}
	verDays, verOrganic := 0, 0
	firstNamed := false
	for _, d := range a.days {
		if d.acts == 0 {
			continue
		}
		activeDays = append(activeDays, d.day)
		r.LastActiveDay = d.day
		if !firstNamed && (d.via != "" || d.family != "") {
			firstNamed = true
			r.FirstVia, r.FirstFamily, r.FirstClient = d.via, d.family, d.client
		}
		if d.acts&VerifiableActs != 0 && r.FirstActAt == 0 {
			r.FirstActAt = d.day * 86400
		}
		if d.day >= d0 && d.day < d0+FunnelActivationDays {
			if m := d.acts & ActivationActs; m != 0 && r.ActivatedAt == 0 {
				r.ActivatedAt, r.ActivationAct = d.day*86400, ActNames(m)[0]
			}
			if d.organic&ActivationActs != 0 && r.ActivatedOrganicAt == 0 {
				r.ActivatedOrganicAt = d.day * 86400
			}
		}
		if d.day >= d0+1 && d.day <= d0+FunnelRetainedDays {
			if d.acts&VerifiableActs != 0 {
				if verDays++; verDays == FunnelRetainedActiveDays {
					r.RetainedAt = d.day * 86400
				}
			}
			if d.organic&VerifiableActs != 0 {
				if verOrganic++; verOrganic == FunnelRetainedActiveDays {
					r.RetainedOrganicAt = d.day * 86400
				}
			}
		}
		if d.day <= today && d.day > today-FunnelSegmentDays {
			trailActs |= d.acts
			trailOrganic |= d.organic
			trailBounty |= d.bounty
			if d.family != "" {
				families[d.family]++
			}
			if d.client != "" {
				clients[d.client]++
			}
			if d.via != "" {
				vias[d.via]++
			}
		}
	}
	r.Durable = durableOn(a.days, today)
	r.Role, r.Roles = RolesOf(trailActs, trailOrganic, trailBounty)
	r.Tenure = tenureOf(today, d0, activeDays)
	r.Family, r.Client = mostFrequent(families), mostFrequent(clients)
	r.Transport = TransportOf(mostFrequent(vias), a.hosted)
	return r
}

// durableOn reports whether the sorted days make an account durable on day
// today: a verifiable act in FunnelDurableActive of the last
// FunnelDurableBuckets buckets of 7 days ending today.
func durableOn(days []agentDay, today int64) bool {
	buckets := map[int64]bool{}
	for _, d := range days {
		if d.acts&VerifiableActs != 0 && d.day <= today && d.day > today-7*FunnelDurableBuckets {
			buckets[(today-d.day)/7] = true
		}
	}
	return len(buckets) >= FunnelDurableActive
}

// mostFrequent is the key with the highest count, the smallest on a tie.
func mostFrequent(m map[string]int) string {
	best, n := "", 0
	for k, v := range m {
		if v > n || v == n && k < best {
			best, n = k, v
		}
	}
	return best
}

// entryOf's lookups: its first signed post answers a message addressed to
// it; it accepted a room invite on its first day.
const (
	entryDMQuery = `SELECT EXISTS(SELECT 1 FROM events e JOIN events p ON p.id=e.reply_to
 WHERE e.seq=(SELECT min(seq) FROM events WHERE account=?1 AND signature<>'') AND p.recipient<>'' AND (p.recipient=?1 OR p.recipient IN ` + accountIDs + `))`
	entryInviteQuery = `SELECT EXISTS(SELECT 1 FROM audit WHERE operation='room.invite.accept' AND actor IN ` + accountIDs + ` AND created_at BETWEEN ?2 AND ?3)`
)

// entryOf is an account's entry point (EntryPoints, first that applies).
func (s *Store) entryOf(ctx context.Context, a *funnelAccount, r FunnelRow) (entry, ref string, err error) {
	d0 := a.firstSeen / 86400
	for _, d := range a.days {
		if d.day == d0 && d.ref == GateRef {
			return "gate", d.ref, nil
		}
		if d.day == d0 && d.ref != "" {
			return "ref:" + d.ref, d.ref, nil
		}
	}
	var dm bool
	if funnelQueryHook != nil {
		funnelQueryHook(entryDMQuery, a.account)
		funnelQueryHook(entryInviteQuery, a.account, d0*86400, d0*86400+86399)
	}
	if err = s.db.QueryRowContext(ctx, entryDMQuery, a.account).Scan(&dm); err != nil {
		return "", "", err
	}
	if dm {
		return "reply_to_dm", "", nil
	}
	var invite bool
	if err = s.db.QueryRowContext(ctx, entryInviteQuery, a.account, d0*86400, d0*86400+86399).Scan(&invite); err != nil {
		return "", "", err
	}
	switch {
	case invite:
		return "invite", "", nil
	case a.hosted || r.FirstVia == "mcp":
		return "mcp_direct", "", nil
	case r.FirstVia == "ui":
		return "ui", "", nil
	case TransportOf(r.FirstVia, false) == "get":
		return "get_link", "", nil
	case r.FirstVia != "":
		return "api", "", nil
	}
	return "unknown", "", nil
}

// FunnelRecompute rebuilds agent_funnel for every account from agent_days,
// in batches of funnelBatch accounts (one transaction each), and records
// the day. It answers the rows written.
func (s *Store) FunnelRecompute(ctx context.Context) (int, error) {
	today := s.now().Unix() / 86400
	excluded, err := s.funnelExcluded(ctx)
	if err != nil {
		return 0, err
	}
	total, after := 0, ""
	for {
		n, last, err := s.recomputeBatch(ctx, today, after, funnelBatch, excluded, nil)
		if err != nil {
			return total, err
		}
		if n == 0 {
			break
		}
		total, after = total+n, last
	}
	if err = s.funnelDetectBursts(ctx); err != nil {
		return total, err
	}
	_, err = s.db.ExecContext(ctx, "INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", metaFunnelComputed, fmt.Sprint(today))
	return total, err
}

// recomputeBatch rebuilds the agent_funnel rows of at most limit accounts
// after account after, in one transaction that also runs save (the daily
// work's cursor) when set, holding flushMu for the batch only. It answers
// the rows written and the last account.
func (s *Store) recomputeBatch(ctx context.Context, today int64, after string, limit int, excluded map[string]bool, save func(*sql.Tx, string) error) (int, string, error) {
	f := &s.funnel
	f.flushMu.Lock()
	defer f.flushMu.Unlock()
	now := s.now().Unix()
	accounts, err := s.loadFunnelAccountsAfter(ctx, after, limit)
	if err != nil || len(accounts) == 0 {
		return 0, "", err
	}
	rows := make([]FunnelRow, 0, len(accounts))
	for _, a := range accounts {
		r := computeFunnel(a, today)
		if r.Entry, r.EntryRef, err = s.entryOf(ctx, a, r); err != nil {
			return 0, "", err
		}
		r.Holdout, r.Excluded = s.funnelHoldout(a.account), excluded[a.account]
		rows = append(rows, r)
	}
	last := accounts[len(accounts)-1].account
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, "", err
	}
	defer tx.Rollback()
	for _, r := range rows {
		if _, err = tx.ExecContext(ctx, `INSERT INTO agent_funnel(account,first_seen,hosted,entry,entry_ref,first_family,first_client,first_via,first_act_at,activated_at,activation_act,activated_organic_at,retained_at,retained_organic_at,durable,last_active_day,holdout,excluded,role,roles,tenure,transport,family,client,taxonomy_version,computed_at)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
 ON CONFLICT(account) DO UPDATE SET first_seen=excluded.first_seen,hosted=excluded.hosted,entry=excluded.entry,entry_ref=excluded.entry_ref,first_family=excluded.first_family,first_client=excluded.first_client,first_via=excluded.first_via,
 first_act_at=excluded.first_act_at,activated_at=excluded.activated_at,activation_act=excluded.activation_act,activated_organic_at=excluded.activated_organic_at,retained_at=excluded.retained_at,retained_organic_at=excluded.retained_organic_at,
 durable=excluded.durable,last_active_day=excluded.last_active_day,holdout=excluded.holdout,excluded=excluded.excluded,role=excluded.role,roles=excluded.roles,tenure=excluded.tenure,transport=excluded.transport,family=excluded.family,client=excluded.client,
 taxonomy_version=excluded.taxonomy_version,computed_at=excluded.computed_at`,
			r.Account, r.FirstSeen, r.Hosted, r.Entry, r.EntryRef, r.FirstFamily, r.FirstClient, r.FirstVia, r.FirstActAt, r.ActivatedAt, r.ActivationAct, r.ActivatedOrganicAt, r.RetainedAt, r.RetainedOrganicAt,
			r.Durable, r.LastActiveDay, r.Holdout, r.Excluded, r.Role, int64(r.Roles), r.Tenure, r.Transport, r.Family, r.Client, FunnelTaxonomyVersion, now); err != nil {
			return 0, "", err
		}
		// A reward that waited (a cap, a dry pool) is retried daily.
		if r.ActivatedAt > 0 && today < r.FirstSeen/86400+FunnelActivationDays+InviteRewardRetryDays {
			s.checkInviteReward(ctx, tx, r.Account, now, excluded)
		}
	}
	if save != nil {
		if err = save(tx, last); err != nil {
			return 0, "", err
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, "", err
	}
	return len(rows), last, nil
}

// The daily work (FunnelMaintenance, once a UTC day after 00:05) runs in
// batches of funnelDailyBatch accounts, each its own short transaction and
// flushMu hold, with a pause between them, so the one connection is never
// held for long. meta funnel.daily keeps its cursor in each batch's
// transaction: a run cut short (the hourly deadline, a restart) resumes
// where it stopped, never from the start.
const (
	metaFunnelDaily  = "funnel.daily"
	funnelDailyYield = 25 * time.Millisecond
)

// funnelDailyBatch is the accounts per batch of the daily work (a var:
// tests shrink it).
var funnelDailyBatch = 200

// funnelDailyHook, when set (tests), runs after each committed batch of the
// daily work with its stage and last account.
var funnelDailyHook func(stage, last string)

type funnelDailyState struct {
	Day   int64  `json:"day"`
	Stage string `json:"stage"` // derive, then recompute
	After string `json:"after"` // the last account done in the stage
	From  int64  `json:"from"`  // the first day re-derived
}

func (s *Store) saveFunnelDaily(ctx context.Context, tx *sql.Tx, st funnelDailyState) error {
	raw, _ := json.Marshal(st)
	_, err := tx.ExecContext(ctx, "INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", metaFunnelDaily, string(raw))
	return err
}

// funnelDailyAccounts are the accounts the daily re-derive visits, after
// account after: those with a day stored since day from-1, a key seen since
// then, or a link another agent witnessed since then (a late link act).
// Every other account's records did not change since the last run.
func (s *Store) funnelDailyAccounts(ctx context.Context, from int64, after string) ([]string, error) {
	since := (from - 1) * 86400
	rows, err := s.db.QueryContext(ctx, `SELECT account FROM agent_days WHERE day>=?1 AND account>?3
 UNION SELECT account FROM identities WHERE last_seen>=?2 AND account>?3
 UNION SELECT i.account FROM link_witnesses w JOIN identities i ON i.id=w.agent WHERE w.created_at>=?2 AND i.account>?3
 ORDER BY 1`, from-1, since, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err = rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// funnelYield pauses between batches, so requests take the connection.
func funnelYield(ctx context.Context) error {
	t := time.NewTimer(funnelDailyYield)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// funnelDaily re-derives the days since the last run for the accounts with
// activity (late records: an async call that finished, a link verified),
// then recomputes agent_funnel, in resumable batches, and records the day.
func (s *Store) funnelDaily(ctx context.Context, today, done int64) error {
	var st funnelDailyState
	var raw string
	if s.db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key=?", metaFunnelDaily).Scan(&raw) != nil || json.Unmarshal([]byte(raw), &st) != nil || st.Day != today {
		from := today - 1
		if done > 0 && done < from {
			from = max(done, today-FunnelBackfillDays)
		}
		st = funnelDailyState{Day: today, Stage: "derive", From: from}
	}
	excluded, err := s.funnelExcluded(ctx)
	if err != nil {
		return err
	}
	f := &s.funnel
	if st.Stage == "derive" {
		list, err := s.funnelDailyAccounts(ctx, st.From, st.After)
		if err != nil {
			return err
		}
		for start := 0; start < len(list); start += funnelDailyBatch {
			batch := list[start:min(start+funnelDailyBatch, len(list))]
			if err = s.deriveBatch(ctx, batch, st, excluded); err != nil {
				return err
			}
			st.After = batch[len(batch)-1]
			if funnelDailyHook != nil {
				funnelDailyHook(st.Stage, st.After)
			}
			if err = funnelYield(ctx); err != nil {
				return err
			}
		}
		st.Stage, st.After = "recompute", ""
		f.flushMu.Lock()
		err = s.saveFunnelDailyNow(ctx, st)
		f.flushMu.Unlock()
		if err != nil {
			return err
		}
	}
	for {
		cur := st
		n, last, err := s.recomputeBatch(ctx, today, st.After, funnelDailyBatch, excluded, func(tx *sql.Tx, last string) error {
			cur.After = last
			return s.saveFunnelDaily(ctx, tx, cur)
		})
		if err != nil {
			return err
		}
		if n == 0 {
			break
		}
		st.After = last
		if funnelDailyHook != nil {
			funnelDailyHook(st.Stage, st.After)
		}
		if err = funnelYield(ctx); err != nil {
			return err
		}
	}
	if err = s.funnelDetectBursts(ctx); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, "INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", metaFunnelComputed, fmt.Sprint(today))
	return err
}

func (s *Store) saveFunnelDailyNow(ctx context.Context, st funnelDailyState) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = s.saveFunnelDaily(ctx, tx, st); err != nil {
		return err
	}
	return tx.Commit()
}

// deriveBatch re-derives a batch of accounts from day st.From on and writes
// them with the cursor at the batch's last account, under flushMu.
func (s *Store) deriveBatch(ctx context.Context, batch []string, st funnelDailyState, excluded map[string]bool) error {
	f := &s.funnel
	f.flushMu.Lock()
	defer f.flushMu.Unlock()
	now := s.now().Unix()
	derived := make([]map[int64]*dayActs, len(batch))
	for i, a := range batch {
		days, err := s.deriveAccount(ctx, a, st.From*86400, now, excluded)
		if err != nil {
			return err
		}
		derived[i] = days
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i, a := range batch {
		if err = writeDerived(ctx, tx, a, derived[i]); err != nil {
			return err
		}
	}
	st.After = batch[len(batch)-1]
	if err = s.saveFunnelDaily(ctx, tx, st); err != nil {
		return err
	}
	return tx.Commit()
}

// FunnelMaintenance is the hourly loop's funnel step: a flush; once a UTC
// day (after 00:05) the daily work (funnelDaily: the recent days of the
// accounts with activity re-derived, agent_funnel recomputed, resumable);
// feedback screens that failed retried; and the nudge rows past retention
// pruned.
func (s *Store) FunnelMaintenance(ctx context.Context) error {
	if err := s.FlushFunnel(ctx); err != nil {
		return err
	}
	now := s.now().Unix()
	today := now / 86400
	var done int64
	var raw string
	if s.db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key=?", metaFunnelComputed).Scan(&raw) == nil {
		fmt.Sscan(raw, &done)
	}
	if done < today && now%86400 >= 300 {
		if err := s.funnelDaily(ctx, today, done); err != nil {
			return err
		}
	}
	if _, err := s.RetryFeedbackScreens(ctx); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, "DELETE FROM nudge_events WHERE (subject LIKE 'net:%' AND shown_at<?) OR shown_at<?", now-NudgeAnonymousDays*86400, now-NudgeAccountDays*86400)
	return err
}

// FunnelRows reads agent_funnel, for tests and the operator.
func (s *Store) FunnelRows(ctx context.Context) ([]FunnelRow, error) {
	bursts, err := s.FunnelBursts(ctx)
	if err != nil {
		return nil, err
	}
	burst := bursts.Members()
	rows, err := s.db.QueryContext(ctx, `SELECT account,first_seen,hosted,entry,entry_ref,first_family,first_client,first_via,first_act_at,activated_at,activation_act,activated_organic_at,retained_at,retained_organic_at,durable,last_active_day,holdout,excluded,role,roles,tenure,transport,family,client FROM agent_funnel ORDER BY account`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FunnelRow
	for rows.Next() {
		var r FunnelRow
		var roles int64
		if err = rows.Scan(&r.Account, &r.FirstSeen, &r.Hosted, &r.Entry, &r.EntryRef, &r.FirstFamily, &r.FirstClient, &r.FirstVia, &r.FirstActAt, &r.ActivatedAt, &r.ActivationAct, &r.ActivatedOrganicAt, &r.RetainedAt, &r.RetainedOrganicAt,
			&r.Durable, &r.LastActiveDay, &r.Holdout, &r.Excluded, &r.Role, &roles, &r.Tenure, &r.Transport, &r.Family, &r.Client); err != nil {
			return nil, err
		}
		r.Roles = uint64(roles)
		if reasons, ok := burst[r.Account]; ok {
			r.Burst = burstLabel(reasons)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// funnelActivated reports, through tx, whether account has an activation act
// within the activation window from its first_seen day d0, as the stored
// days say: the activated stage computeFunnel derives (invite rewards read
// it, inviterewards.go).
func funnelActivated(ctx context.Context, tx *sql.Tx, account string, d0 int64) (bool, error) {
	var ok bool
	err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM agent_days WHERE account=? AND day>=? AND day<? AND (acts & ?)<>0)", account, d0, d0+FunnelActivationDays, int64(ActivationActs)).Scan(&ok)
	return ok, err
}
