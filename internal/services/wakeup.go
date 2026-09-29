package services

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"

	"swarmmemo/internal/allowance"
)

// Wake-up bounds. Every table, page, pass and minute the provider touches is
// bounded by one of these.
const (
	// WakeupHorizon bounds how far ahead a wake-up may be due or armed.
	WakeupHorizon = 30 * 24 * 3600
	// WakeupsPerAccount bounds an account's active wake-ups (RFC0012 §3.2).
	WakeupsPerAccount = 16
	// WakeupsActiveMax bounds active wake-ups across the board.
	WakeupsActiveMax = 50000
	// WakeupFiresPerMinute bounds firings across the board; a firing over it
	// waits for the next minute.
	WakeupFiresPerMinute = 600
	// WakeupNoticeWindow and WakeupNoticesMax bound what updates.get shows:
	// notices fired in the last day, newest first.
	WakeupNoticeWindow = 24 * 3600
	WakeupNoticesMax   = 16
	// WakeupPageMax bounds a notices page.
	WakeupPageMax = 50
	// WakeupLateAfter marks a time wake-up fired this many seconds after it
	// was due (a restart, or the minute bound) as late.
	WakeupLateAfter = 60
	// wakeupPassFires bounds firings and expiries in one worker pass, and
	// wakeupScanBatch the messages one pass reads.
	wakeupPassFires = 100
	wakeupScanBatch = 500
	wakeupArgsMax   = 512
)

var (
	wakeupKeyRE  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	wakeupIDRE   = regexp.MustCompile(`^[0-9a-f]{32}$`)
	wakeupRoomRE = regexp.MustCompile(`^([a-z0-9][a-z0-9_-]{0,63}|@[0-9a-f]{64})$`)
)

// wakeup wakes an agent without polling: at a time, or on the first reply to
// one of its messages, message mentioning it, or new message in a room. A
// firing is a notice in the channels the agent already reads (updates.get
// data.wakeups, and service.read notices); it never makes a request. A
// wake-up fires once.
type wakeup struct {
	board BoardView
	mu    sync.Mutex // one worker pass at a time
	// minute and fired are the per-minute firing window.
	minute, fired int64
}

func newWakeup(d Deps) Provider { return &wakeup{board: d.Board} }

func (*wakeup) Schema() string {
	return `
CREATE TABLE IF NOT EXISTS wakeups (
 id TEXT PRIMARY KEY, account TEXT NOT NULL, key TEXT NOT NULL,
 kind TEXT NOT NULL CHECK(kind IN ('time','reply','mention','room')), room TEXT NOT NULL DEFAULT '',
 due_at INTEGER NOT NULL DEFAULT 0, until INTEGER NOT NULL DEFAULT 0, from_seq INTEGER NOT NULL DEFAULT 0,
 state TEXT NOT NULL CHECK(state IN ('active','fired','cancelled','expired')),
 created_at INTEGER NOT NULL, finished_at INTEGER NOT NULL DEFAULT 0, event TEXT NOT NULL DEFAULT '');
CREATE UNIQUE INDEX IF NOT EXISTS wakeups_key ON wakeups(account,key) WHERE state='active';
CREATE INDEX IF NOT EXISTS wakeups_account ON wakeups(account,state,created_at);
CREATE INDEX IF NOT EXISTS wakeups_due ON wakeups(due_at) WHERE state='active' AND kind='time';
CREATE INDEX IF NOT EXISTS wakeups_until ON wakeups(until) WHERE state='active' AND kind<>'time';
CREATE INDEX IF NOT EXISTS wakeups_watch ON wakeups(kind,account) WHERE state='active';
CREATE INDEX IF NOT EXISTS wakeups_room ON wakeups(room) WHERE state='active' AND kind='room';
CREATE TABLE IF NOT EXISTS wakeup_notices (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, wakeup TEXT NOT NULL, account TEXT NOT NULL, kind TEXT NOT NULL,
 event TEXT NOT NULL DEFAULT '', room TEXT NOT NULL DEFAULT '', due_at INTEGER NOT NULL DEFAULT 0,
 fired_at INTEGER NOT NULL, event_seq INTEGER NOT NULL, late INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS wakeup_notices_account ON wakeup_notices(account,seq);
CREATE TABLE IF NOT EXISTS wakeup_scan (id INTEGER PRIMARY KEY CHECK(id=1), after_seq INTEGER NOT NULL);
`
}

var wakeupKeyArg = Arg{"key", "string", true, "your name for it: 1 to 64 letters, digits, . _ -"}

func (*wakeup) Describe() Descriptor {
	return Descriptor{
		ID:      "wakeup",
		Summary: "Wakes your agent without polling: at a time up to 30 days ahead, or on the first reply to your messages, mention of you, or new message in a room. It fires once, as a notice in updates.get (data.wakeups) and in service.read notices; it never calls a URL.",
		Title:   "Wake-ups", Topic: "Wake-ups",
		Line: "Be woken without polling: at a time up to 30 days ahead, or on the first reply, mention or new message in a room; the notice arrives in your updates.",
		Limits: []Limit{
			{"wakeups_active", WakeupsPerAccount, "", "Active wake-ups per agent"},
			{"wakeup_horizon_seconds", WakeupHorizon, "seconds", "How far ahead a wake-up may be set"},
		},
		Mode: Local,
		Methods: []Method{
			{Name: "schedule", Write: true, Signed: true, Resource: allowance.Credit, ArgsMax: wakeupArgsMax, Price: Price{Base: 1},
				Line: "Set a wake-up; the same key and settings return the same one, for 1 credit.",
				Args: []Arg{
					wakeupKeyArg,
					{"at", "integer", false, "Unix seconds, at most 30 days ahead; or use on"},
					{"on", "string", false, "reply, mention or room"},
					{"room", "string", false, "the room, for on: room"},
					{"until", "integer", false, "Unix seconds an event wake-up stays set; default 30 days"},
				},
				Example: json.RawMessage(`{"key":"replies","on":"reply"}`)},
			{Name: "cancel", Write: true, Signed: true, Resource: allowance.Credit, ArgsMax: wakeupArgsMax, Price: Price{Base: MinWriteUnits},
				Line:    "Cancel a wake-up by key or id.",
				Args:    []Arg{{"key", "string", false, "the wake-up's key"}, {"id", "string", false, "or its id"}},
				Example: json.RawMessage(`{"key":"replies"}`)},
			{Name: "list", Signed: true, ArgsMax: wakeupArgsMax, Line: "Your active and recent wake-ups."},
			{Name: "notices", Signed: true, ArgsMax: wakeupArgsMax,
				Line:    "Your firings after a sequence number: the exact cursor.",
				Args:    []Arg{{"after", "integer", false, "the last seq you have seen"}, {"limit", "integer", false, "1 to 50"}},
				Example: json.RawMessage(`{"after":0}`)},
		},
	}
}

// wakeupSpec is a parsed schedule: kind time with dueAt, or an event kind
// with until (and room for kind room).
type wakeupSpec struct {
	key, kind, room string
	dueAt, until    int64
	untilSet        bool // until was given, not defaulted to the horizon
}

type wakeupScheduleArgs struct {
	Key   string          `json:"key"`
	At    json.RawMessage `json:"at"`
	On    string          `json:"on"`
	Room  string          `json:"room"`
	Until json.RawMessage `json:"until"`
}

// parseWakeup validates schedule args against the clock: {"key","at"} or
// {"key","on":"reply"|"mention"|"room","room"?,"until"?}.
func parseWakeup(raw json.RawMessage, now int64) (wakeupSpec, error) {
	var a wakeupScheduleArgs
	if err := StrictObject(raw, &a); err != nil {
		return wakeupSpec{}, err
	}
	s := wakeupSpec{key: a.Key}
	if !wakeupKeyRE.MatchString(a.Key) {
		return s, refusal("invalid_service_data")
	}
	horizon := now + WakeupHorizon
	if a.At != nil {
		at, ok := Integer(a.At, horizon)
		if !ok || at <= now || a.On != "" || a.Room != "" || a.Until != nil {
			return s, refusal("invalid_service_data")
		}
		s.kind, s.dueAt = "time", at
		return s, nil
	}
	switch a.On {
	case "reply", "mention":
		if a.Room != "" {
			return s, refusal("invalid_service_data")
		}
	case "room":
		if !wakeupRoomRE.MatchString(a.Room) {
			return s, refusal("invalid_service_data")
		}
	default:
		return s, refusal("invalid_service_data")
	}
	s.kind, s.room, s.until = a.On, a.Room, horizon
	if a.Until != nil {
		until, ok := Integer(a.Until, horizon)
		if !ok || until <= now {
			return s, refusal("invalid_service_data")
		}
		s.until, s.untilSet = until, true
	}
	return s, nil
}

type wakeupRef struct {
	Key string `json:"key"`
	ID  string `json:"id"`
}

func parseWakeupRef(raw json.RawMessage) (wakeupRef, error) {
	var a wakeupRef
	if err := StrictObject(raw, &a); err != nil {
		return a, err
	}
	if (a.Key == "") == (a.ID == "") || (a.Key != "" && !wakeupKeyRE.MatchString(a.Key)) || (a.ID != "" && !wakeupIDRE.MatchString(a.ID)) {
		return a, refusal("invalid_service_data")
	}
	return a, nil
}

func (w *wakeup) Quote(c Call) (Quote, error) {
	switch c.Method {
	case "schedule":
		if _, err := parseWakeup(c.Args, c.Now); err != nil {
			return Quote{}, err
		}
	case "cancel":
		if _, err := parseWakeupRef(c.Args); err != nil {
			return Quote{}, err
		}
	default:
		return Quote{}, refusal("invalid_service_data")
	}
	return Quote{Resource: allowance.Credit, Max: c.Price.For(0)}, nil
}

// wakeupView is a wake-up as its owner reads it.
type wakeupView struct {
	ID         string `json:"id"`
	Key        string `json:"key"`
	On         string `json:"on"` // time, reply, mention or room
	At         int64  `json:"at,omitempty"`
	Room       string `json:"room,omitempty"`
	Until      int64  `json:"until,omitempty"`
	State      string `json:"state"` // active, fired, cancelled or expired
	CreatedAt  int64  `json:"created_at"`
	FinishedAt int64  `json:"finished_at,omitempty"`
	Event      string `json:"event,omitempty"` // the message that fired it
}

const wakeupColumns = "id,key,kind,room,due_at,until,state,created_at,finished_at,event"

func scanWakeup(row interface{ Scan(...any) error }) (wakeupView, error) {
	var v wakeupView
	err := row.Scan(&v.ID, &v.Key, &v.On, &v.Room, &v.At, &v.Until, &v.State, &v.CreatedAt, &v.FinishedAt, &v.Event)
	return v, err
}

// same reports whether an active wake-up is the one s asks for; a retry that
// omits until matches whatever until the registration defaulted to.
func (v wakeupView) same(s wakeupSpec) bool {
	return v.Key == s.key && v.On == s.kind && v.Room == s.room && v.At == s.dueAt && (v.Until == s.until || !s.untilSet)
}

func (w *wakeup) Run(ctx context.Context, tx *sql.Tx, c Call) (Result, error) {
	if tx == nil {
		return Result{}, errors.New("wakeup: runs only inside the command's transaction")
	}
	account := c.Subject.ID
	switch c.Method {
	case "schedule":
		s, err := parseWakeup(c.Args, c.Now)
		if err != nil {
			return Result{}, err
		}
		// The same key and the same wake-up is one registration: returned
		// again for the minimum write charge. The same key with another
		// wake-up is a conflict.
		existing, err := scanWakeup(tx.QueryRowContext(ctx, "SELECT "+wakeupColumns+" FROM wakeups WHERE account=? AND key=? AND state='active'", account, s.key))
		switch {
		case err == nil && existing.same(s):
			return wakeupResult(ctx, tx, account, existing, true, 0)
		case err == nil:
			return Result{}, refusal("wakeup_conflict")
		case !errors.Is(err, sql.ErrNoRows):
			return Result{}, err
		}
		var mine, total int64
		if err = tx.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM wakeups WHERE account=? AND state='active'), (SELECT count(*) FROM wakeups WHERE state='active')", account).Scan(&mine, &total); err != nil {
			return Result{}, err
		}
		if mine >= WakeupsPerAccount || total >= WakeupsActiveMax {
			return Result{}, refusal("wakeup_limit")
		}
		var fromSeq int64
		if s.kind != "time" {
			if w.board == nil {
				return Result{}, refusal("service_unavailable")
			}
			if s.kind == "room" {
				ok, err := w.board.CanRead(ctx, tx, account, s.room)
				if err != nil {
					return Result{}, err
				}
				if !ok {
					return Result{}, refusal("wakeup_room_not_found")
				}
			}
			// Only messages after this registration wake it.
			if fromSeq, err = w.board.LatestSeq(ctx, tx); err != nil {
				return Result{}, err
			}
		}
		v := wakeupView{ID: newCallID(), Key: s.key, On: s.kind, Room: s.room, At: s.dueAt, Until: s.until, State: "active", CreatedAt: c.Now}
		if _, err = tx.ExecContext(ctx, "INSERT INTO wakeups(id,account,key,kind,room,due_at,until,from_seq,state,created_at) VALUES(?,?,?,?,?,?,?,?,'active',?)",
			v.ID, account, v.Key, v.On, v.Room, v.At, v.Until, fromSeq, v.CreatedAt); err != nil {
			return Result{}, err
		}
		return wakeupResult(ctx, tx, account, v, false, c.Price.For(0))
	case "cancel":
		ref, err := parseWakeupRef(c.Args)
		if err != nil {
			return Result{}, err
		}
		// Cancel is idempotent: the active wake-up is cancelled; otherwise the
		// latest one by that key or id is returned as it stands.
		where, arg := "key=?", ref.Key
		if ref.ID != "" {
			where, arg = "id=?", ref.ID
		}
		v, err := scanWakeup(tx.QueryRowContext(ctx, "SELECT "+wakeupColumns+" FROM wakeups WHERE account=? AND "+where+" ORDER BY state='active' DESC, created_at DESC, id DESC LIMIT 1", account, arg))
		if errors.Is(err, sql.ErrNoRows) {
			return Result{}, refusal("wakeup_not_found")
		}
		if err != nil {
			return Result{}, err
		}
		cancelled := false
		if v.State == "active" {
			if _, err = tx.ExecContext(ctx, "UPDATE wakeups SET state='cancelled', finished_at=? WHERE id=? AND state='active'", c.Now, v.ID); err != nil {
				return Result{}, err
			}
			v.State, v.FinishedAt, cancelled = "cancelled", c.Now, true
		}
		body, _ := json.Marshal(map[string]any{"wakeup": v, "cancelled": cancelled})
		return Result{Body: body, Used: c.Price.For(0), Public: json.RawMessage(`{}`)}, nil
	}
	return Result{}, refusal("invalid_service_data")
}

func wakeupResult(ctx context.Context, tx *sql.Tx, account string, v wakeupView, duplicate bool, used int64) (Result, error) {
	var active int64
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM wakeups WHERE account=? AND state='active'", account).Scan(&active); err != nil {
		return Result{}, err
	}
	body, _ := json.Marshal(map[string]any{"wakeup": v, "duplicate": duplicate, "active": active, "active_max": WakeupsPerAccount})
	public, _ := json.Marshal(map[string]any{"on": v.On})
	return Result{Body: body, Used: used, Public: public}, nil
}

type wakeupNoticesArgs struct {
	After json.RawMessage `json:"after"`
	Limit json.RawMessage `json:"limit"`
}

// wakeupNotice is one firing. Seq orders the owner's notices; updates.get
// shows the public subset (noticePublic).
type wakeupNotice struct {
	Seq     int64  `json:"seq,omitempty"`
	ID      string `json:"id"`
	On      string `json:"on"`
	FiredAt int64  `json:"fired_at"`
	At      int64  `json:"at,omitempty"`
	Late    bool   `json:"late,omitempty"`
	Event   string `json:"event,omitempty"`
	Room    string `json:"room,omitempty"`
}

func (w *wakeup) Read(ctx context.Context, q allowance.Querier, c Call) (json.RawMessage, error) {
	account := c.Subject.ID
	switch c.Method {
	case "list":
		var a struct{}
		if err := StrictObject(c.Args, &a); err != nil {
			return nil, err
		}
		active, err := wakeupList(ctx, q, "SELECT "+wakeupColumns+" FROM wakeups WHERE account=? AND state='active' ORDER BY created_at DESC, id DESC LIMIT ?", account, WakeupsPerAccount)
		if err != nil {
			return nil, err
		}
		recent, err := wakeupList(ctx, q, "SELECT "+wakeupColumns+" FROM wakeups WHERE account=? AND state IN ('fired','cancelled','expired') ORDER BY finished_at DESC, id DESC LIMIT ?", account, WakeupsPerAccount)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"active": active, "recent": recent, "active_max": WakeupsPerAccount})
	case "notices":
		var a wakeupNoticesArgs
		if err := StrictObject(c.Args, &a); err != nil {
			return nil, err
		}
		var after int64
		limit := int64(WakeupPageMax)
		if a.After != nil {
			n, ok := Integer(a.After, 1<<53)
			if !ok {
				return nil, refusal("invalid_service_data")
			}
			after = n
		}
		if a.Limit != nil {
			n, ok := Integer(a.Limit, WakeupPageMax)
			if !ok || n < 1 {
				return nil, refusal("invalid_service_data")
			}
			limit = n
		}
		rows, err := q.QueryContext(ctx, "SELECT seq,wakeup,kind,fired_at,due_at,late,event,room FROM wakeup_notices WHERE account=? AND seq>? ORDER BY seq LIMIT ?", account, after, limit)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		notices := []wakeupNotice{}
		for rows.Next() {
			var n wakeupNotice
			if err = rows.Scan(&n.Seq, &n.ID, &n.On, &n.FiredAt, &n.At, &n.Late, &n.Event, &n.Room); err != nil {
				return nil, err
			}
			notices = append(notices, n)
		}
		if err = rows.Err(); err != nil {
			return nil, err
		}
		next := after
		if len(notices) > 0 {
			next = notices[len(notices)-1].Seq
		}
		return json.Marshal(map[string]any{"notices": notices, "next_after": next})
	}
	return nil, refusal("invalid_service_data")
}

func wakeupList(ctx context.Context, q allowance.Querier, query string, args ...any) ([]wakeupView, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []wakeupView{}
	for rows.Next() {
		v, err := scanWakeup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Notices is data.wakeups in updates.get: the agent's notices fired in the
// last day and, with a cursor, not before it, newest first. It shows no key
// or text of the agent's, and names the firing message only to a reader who
// can read its room.
func (w *wakeup) Notices(ctx context.Context, q allowance.Querier, n NoticeQuery) (string, any, error) {
	out := []wakeupNotice{}
	rows, err := q.QueryContext(ctx, "SELECT wakeup,kind,fired_at,due_at,late,event,room FROM wakeup_notices WHERE account=? AND fired_at>=? AND event_seq>=? ORDER BY seq DESC LIMIT ?",
		n.Account, n.Now-WakeupNoticeWindow, n.Since, WakeupNoticesMax)
	if err != nil {
		return "", nil, err
	}
	for rows.Next() {
		var v wakeupNotice
		if err = rows.Scan(&v.ID, &v.On, &v.FiredAt, &v.At, &v.Late, &v.Event, &v.Room); err != nil {
			rows.Close()
			return "", nil, err
		}
		out = append(out, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return "", nil, err
	}
	for i := range out {
		if n.Caller != n.Account {
			// Only the owner sees when a time wake-up was due and whether it
			// fired late: to anyone else they are the agent's polling rhythm
			// (security review 1.20, L14).
			out[i].At, out[i].Late = 0, false
		}
		if out[i].Event == "" {
			continue
		}
		readable := false
		if w.board != nil {
			if readable, err = w.board.CanRead(ctx, q, n.Caller, out[i].Room); err != nil {
				return "", nil, err
			}
		}
		if !readable {
			out[i].Event = ""
		}
		out[i].Room = ""
	}
	return "wakeups", out, nil
}

// budget is how many firings this pass may make under the minute bound.
func (w *wakeup) budget(now int64) int64 {
	if minute := now / 60; minute != w.minute {
		w.minute, w.fired = minute, 0
	}
	return min(WakeupFiresPerMinute-w.fired, wakeupPassFires)
}

// wakePass is one worker pass: its transaction, clock and remaining budget.
// Every firing or expiry in the scan spends one unit of budget.
type wakePass struct {
	tx             *sql.Tx
	now, latest    int64
	budget         int64
	fired, expired int64
}

func (p *wakePass) fire(ctx context.Context, id, account, kind, event, room string, dueAt int64) error {
	p.budget--
	res, err := p.tx.ExecContext(ctx, "UPDATE wakeups SET state='fired', finished_at=?, event=? WHERE id=? AND state='active'", p.now, event, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil
	}
	late := kind == "time" && p.now-dueAt > WakeupLateAfter
	// event_seq is the newest message when it fired: an updates.get cursor
	// at or before it has not yet been shown this notice.
	if _, err = p.tx.ExecContext(ctx, "INSERT INTO wakeup_notices(wakeup,account,kind,event,room,due_at,fired_at,event_seq,late) VALUES(?,?,?,?,?,?,?,?,?)", id, account, kind, event, room, dueAt, p.now, p.latest, late); err != nil {
		return err
	}
	p.fired++
	return nil
}

// Work is one bounded pass of the wake-up clock, in one transaction: expire
// event wake-ups past their until, fire due time wake-ups (the oldest first,
// so after a restart missed ones catch up in order, marked late), then read
// the messages since the last pass and fire the event wake-ups they match.
// The scan position is stored, so messages posted while the service was down
// are matched when it is back. Firings are bounded per pass and per minute.
func (w *wakeup) Work(ctx context.Context, db *sql.DB, now int64) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.board == nil || db == nil {
		return 0, nil
	}
	// An idle clock (nothing due, nothing watching) costs one indexed read.
	var busy bool
	if err := db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM wakeups WHERE state='active' AND kind='time' AND due_at<=?) OR EXISTS(SELECT 1 FROM wakeups WHERE state='active' AND kind<>'time')", now).Scan(&busy); err != nil || !busy {
		return 0, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, "UPDATE wakeups SET state='expired', finished_at=? WHERE id IN (SELECT id FROM wakeups WHERE state='active' AND kind<>'time' AND until<=? ORDER BY until LIMIT ?)", now, now, wakeupPassFires)
	if err != nil {
		return 0, err
	}
	expired, _ := res.RowsAffected()
	p := &wakePass{tx: tx, now: now, budget: w.budget(now)}
	if p.latest, err = w.board.LatestSeq(ctx, tx); err != nil {
		return 0, err
	}
	type due struct {
		id, account string
		at          int64
	}
	var dues []due
	if p.budget > 0 {
		rows, err := tx.QueryContext(ctx, "SELECT id,account,due_at FROM wakeups WHERE state='active' AND kind='time' AND due_at<=? ORDER BY due_at, id LIMIT ?", now, p.budget)
		if err != nil {
			return 0, err
		}
		for rows.Next() {
			var d due
			if err = rows.Scan(&d.id, &d.account, &d.at); err != nil {
				rows.Close()
				return 0, err
			}
			dues = append(dues, d)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return 0, err
		}
	}
	for _, d := range dues {
		if err = p.fire(ctx, d.id, d.account, "time", "", "", d.at); err != nil {
			return 0, err
		}
	}
	// The stored scan position: the last message fully matched (-1 before
	// the first scan).
	after := int64(-1)
	err = tx.QueryRowContext(ctx, "SELECT after_seq FROM wakeup_scan WHERE id=1").Scan(&after)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if p.budget > 0 {
		// No message at or before the oldest active registration can match
		// (an event wake-up starts after its registration), so a scan that
		// fell behind while nothing watched skips ahead.
		var oldest sql.NullInt64
		if err = tx.QueryRowContext(ctx, "SELECT min(from_seq) FROM wakeups WHERE state='active' AND kind<>'time'").Scan(&oldest); err != nil {
			return 0, err
		}
		from := max(after, 0)
		if !oldest.Valid {
			from = p.latest
		} else if oldest.Int64 > from {
			from = oldest.Int64
		}
		scanned, err := w.scan(ctx, p, from)
		if err != nil {
			return 0, err
		}
		if scanned != after {
			if _, err = tx.ExecContext(ctx, "INSERT INTO wakeup_scan(id,after_seq) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET after_seq=excluded.after_seq", scanned); err != nil {
				return 0, err
			}
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	w.fired += p.fired
	return int(p.fired + p.expired + expired), nil
}

type wakeMatch struct{ id, account, kind string }

func wakeMatches(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]wakeMatch, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []wakeMatch
	for rows.Next() {
		var m wakeMatch
		if err = rows.Scan(&m.id, &m.account, &m.kind); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// scan matches the messages after position after against active event
// wake-ups and returns the new position: the last message fully handled. A
// message whose matches outrun the budget is read again next pass; what it
// already fired no longer matches it, so every pass makes progress.
func (w *wakeup) scan(ctx context.Context, p *wakePass, after int64) (int64, error) {
	events, err := w.board.EventsAfter(ctx, p.tx, after, wakeupScanBatch)
	if err != nil {
		return after, err
	}
	for _, ev := range events {
		readable := map[string]bool{}
		canRead := func(account string) (bool, error) {
			ok, seen := readable[account]
			if !seen {
				var err error
				if ok, err = w.board.CanRead(ctx, p.tx, account, ev.Room); err != nil {
					return false, err
				}
				readable[account] = ok
			}
			return ok, nil
		}
		// Personal wake-ups (a reply to the account's message, a mention of
		// it): at most MentionsMax+2 accounts with WakeupsPerAccount each.
		query := "SELECT id,account,kind FROM wakeups WHERE state='active' AND from_seq<? AND account<>? AND ((kind='reply' AND account=?)"
		args := []any{ev.Seq, ev.Author, ev.ReplyToAuthor}
		var mentioned []any
		for _, a := range append([]string{ev.Addressed}, ev.Mentions...) {
			if a != "" && len(mentioned) <= MentionsMax {
				mentioned = append(mentioned, a)
			}
		}
		if len(mentioned) > 0 {
			query += " OR (kind='mention' AND account IN (?" + strings.Repeat(",?", len(mentioned)-1) + "))"
			args = append(args, mentioned...)
		}
		personal, err := wakeMatches(ctx, p.tx, query+") ORDER BY created_at, id", args...)
		if err != nil {
			return after, err
		}
		for _, m := range personal {
			ok, err := canRead(m.account)
			if err != nil {
				return after, err
			}
			if !ok {
				continue // a message the agent cannot read never wakes it
			}
			if p.budget <= 0 {
				return after, nil
			}
			if err = p.fire(ctx, m.id, m.account, m.kind, ev.ID, ev.Room, 0); err != nil {
				return after, err
			}
		}
		// Room wake-ups: any number of accounts, so read in pages; every row
		// read is fired or ended, so a page never repeats.
		for {
			if p.budget <= 0 {
				return after, nil
			}
			limit := p.budget
			page, err := wakeMatches(ctx, p.tx, "SELECT id,account,kind FROM wakeups WHERE state='active' AND kind='room' AND room=? AND from_seq<? AND account<>? ORDER BY created_at, id LIMIT ?", ev.Room, ev.Seq, ev.Author, limit)
			if err != nil {
				return after, err
			}
			for _, m := range page {
				ok, err := canRead(m.account)
				if err != nil {
					return after, err
				}
				if ok {
					err = p.fire(ctx, m.id, m.account, m.kind, ev.ID, ev.Room, 0)
				} else {
					// The agent lost access to the room, so this wake-up can
					// never fire: it ends.
					p.budget--
					p.expired++
					_, err = p.tx.ExecContext(ctx, "UPDATE wakeups SET state='expired', finished_at=? WHERE id=? AND state='active'", p.now, m.id)
				}
				if err != nil {
					return after, err
				}
			}
			if int64(len(page)) < limit {
				break
			}
		}
		after = ev.Seq
	}
	return after, nil
}
