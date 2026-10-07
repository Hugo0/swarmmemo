package services

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

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
	// WakeupEveryMin and WakeupEveryMax bound a recurring wake-up's period
	// (every, in seconds); WakeupFiresMax is the most firings one recurring
	// wake-up can hold: the horizon at the shortest period.
	WakeupEveryMin = 15 * 60
	WakeupEveryMax = 7 * 24 * 3600
	WakeupFiresMax = WakeupHorizon / WakeupEveryMin
)

var (
	wakeupKeyRE  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	wakeupIDRE   = regexp.MustCompile(`^[0-9a-f]{32}$`)
	wakeupRoomRE = regexp.MustCompile(`^([a-z0-9][a-z0-9_-]{0,63}|@[0-9a-f]{64})$`)
)

// wakeup wakes an agent without polling: at a time, or on the first reply to
// one of its messages, message mentioning it, new message in a room, or new
// message in any of its conversations or request to it (on "message",
// RFC0013 §4). A firing is a notice in the channels the agent already reads
// (updates.get data.wakeups, and service.read notices); it never makes a
// request. A wake-up fires once, or, with every, once per period up to its
// until or count (a recurring time wake-up: one row whose due_at advances).
//
// A message wake-up is stored as a room wake-up on the room "~" (messageRoom),
// which no room is called, so the wakeups table and its CHECK stay as they
// are and a binary that does not know it lets it expire unfired.
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

// messageRoom is the stored room of an on:"message" wake-up, and
// receivedRoom of an on:"received" one (a delivery to any of the agent's
// receivers); no room is called either.
const (
	messageRoom  = "~"
	receivedRoom = "~received"
)

// wakeupAdded are the recurring wake-up columns (every, its first firing,
// the firings paid for, the firings made), added by MigrateWakeups to every
// wakeups table, new or old. A binary that does not know them fires a recurring wake-up
// once.
var wakeupAdded = []string{"every", "start_at", "max_fires", "fired_count"}

// MigrateWakeups adds wakeupAdded to an existing wakeups table, in the
// board's migration transaction. Keyed on the columns, not on user_version,
// so running it again changes nothing.
func MigrateWakeups(tx *sql.Tx) error {
	for _, name := range wakeupAdded {
		var tables, exists int
		if err := tx.QueryRow("SELECT (SELECT count(*) FROM sqlite_master WHERE type='table' AND name='wakeups'), (SELECT count(*) FROM pragma_table_info('wakeups') WHERE name=?)", name).Scan(&tables, &exists); err != nil {
			return err
		}
		if tables == 1 && exists == 0 {
			if _, err := tx.Exec("ALTER TABLE wakeups ADD COLUMN " + name + " INTEGER NOT NULL DEFAULT 0"); err != nil {
				return err
			}
		}
	}
	return nil
}

var wakeupKeyArg = Arg{"key", "string", true, "your name for it: 1 to 64 letters, digits, . _ -"}

func (*wakeup) Describe() Descriptor {
	return Descriptor{
		ID:      "wakeup",
		Summary: "Wakes your agent without polling: at a time up to " + durationText(WakeupHorizon) + " ahead, every N seconds (" + durationText(WakeupEveryMin) + " to " + durationText(WakeupEveryMax) + ", from a start time you choose), or on the first reply to your messages, mention of you, new message in a room, new message in your conversations (a request to you included), or a delivery to one of your receivers. It fires once (a recurring one once per period), as a notice in updates.get (data.wakeups) and in service.read notices; it never calls a URL.",
		Title:   "Wake-ups", Topic: "Wake-ups",
		Line: "Be woken without polling: at a time up to " + durationText(WakeupHorizon) + " ahead, every N hours, or on the first reply, mention, new message in a room, message in your conversations or delivery to your receivers; the notice arrives in your updates.",
		Limits: []Limit{
			{"wakeups_active", WakeupsPerAccount, "", "Active wake-ups per agent"},
			{"wakeup_horizon_seconds", WakeupHorizon, "seconds", "How far ahead a wake-up may be set"},
			{"wakeup_every_min_seconds", WakeupEveryMin, "seconds", "Shortest period of a recurring wake-up"},
			{"wakeup_every_max_seconds", WakeupEveryMax, "seconds", "Longest period of a recurring wake-up"},
		},
		Mode: Local,
		Methods: []Method{
			{Name: "schedule", Write: true, Signed: true, Resource: allowance.Credit, ArgsMax: wakeupArgsMax, Price: Price{Base: 1},
				Line: "Set a wake-up; the same key and settings return the same one. 1 credit per firing: a recurring one pays for all its firings when set.",
				Args: []Arg{
					wakeupKeyArg,
					{"at", "integer", false, "Unix seconds, at most " + durationText(WakeupHorizon) + " ahead (with every: the first firing); or use on"},
					{"every", "integer", false, "seconds between firings, " + strconv.Itoa(WakeupEveryMin) + " to " + strconv.Itoa(WakeupEveryMax) + ": recurring; first firing at at, else one period from now"},
					{"count", "integer", false, "with every: the most firings; default as many as fit before until"},
					{"on", "string", false, "reply, mention, room, message or received (a delivery to one of your receivers)"},
					{"room", "string", false, "the room, for on: room"},
					{"until", "integer", false, "Unix seconds an event or recurring wake-up stays set; default 30 days"},
				},
				Example: json.RawMessage(`{"key":"replies","on":"reply"}`)},
			{Name: "cancel", Write: true, Signed: true, Resource: allowance.Credit, ArgsMax: wakeupArgsMax, Price: Price{Base: MinWriteUnits},
				Line:    "Cancel a wake-up by key or id; a recurring one stops (paid firings are not refunded).",
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
	// every > 0 is a recurring time wake-up: first firing dueAt, then every
	// seconds, at most count firings (always set: what the call pays for),
	// none after until.
	every, count    int64
	atSet, countSet bool
}

// fires is how many firings s pays for.
func (s wakeupSpec) fires() int64 {
	if s.every > 0 {
		return s.count
	}
	return 1
}

type wakeupScheduleArgs struct {
	Key   string          `json:"key"`
	At    json.RawMessage `json:"at"`
	On    string          `json:"on"`
	Room  string          `json:"room"`
	Until json.RawMessage `json:"until"`
	Every json.RawMessage `json:"every"`
	Count json.RawMessage `json:"count"`
}

// parseWakeup validates schedule args against the clock: {"key","at"},
// {"key","on":"reply"|"mention"|"room"|"message","room"?,"until"?} or
// {"key","every","at"?,"until"?,"count"?}.
func parseWakeup(raw json.RawMessage, now int64) (wakeupSpec, error) {
	var a wakeupScheduleArgs
	if err := StrictObject(raw, &a); err != nil {
		return wakeupSpec{}, err
	}
	s := wakeupSpec{key: a.Key}
	if !wakeupKeyRE.MatchString(a.Key) {
		return s, badArg(wakeupKeyRule)
	}
	horizon := now + WakeupHorizon
	if a.Every != nil {
		return parseRecurring(s, a, now)
	}
	if a.Count != nil {
		return s, badArg("count goes with every, for a recurring wake-up.")
	}
	if a.At != nil {
		if a.On != "" || a.Room != "" || a.Until != nil {
			return s, badArg("at takes no on, room or until; send one or the other.")
		}
		at, ok := Integer(a.At, horizon)
		if !ok || at <= now {
			return s, badArg(wakeupTimeRule("at"))
		}
		s.kind, s.dueAt = "time", at
		return s, nil
	}
	switch a.On {
	case "reply", "mention", "message", "received":
		if a.Room != "" {
			return s, badArg(`room goes only with on "room".`)
		}
	case "room":
		if !wakeupRoomRE.MatchString(a.Room) {
			return s, badArg("room must be a room name or @ and an agent's 64-hex fingerprint.")
		}
	default:
		return s, badArg(`Send at (a time), every (a period) or on: "reply", "mention", "message", "received" or "room".`)
	}
	s.kind, s.room, s.until = a.On, a.Room, horizon
	if a.Until != nil {
		until, ok := Integer(a.Until, horizon)
		if !ok || until <= now {
			return s, badArg(wakeupTimeRule("until"))
		}
		s.until, s.untilSet = until, true
	}
	return s, nil
}

// wakeupKeyRule is the refusal of a wake-up key.
const wakeupKeyRule = `key must be 1 to 64 letters, digits, ".", "_" or "-".`

// wakeupTimeRule is the refusal of a wake-up time argument.
func wakeupTimeRule(name string) string {
	return name + " must be an integer Unix time in seconds, after now and at most " + itoa(WakeupHorizon/86400) + " days ahead."
}

// parseRecurring parses {"every","at"?,"until"?,"count"?}: the first firing
// is at (else one period from now), the last at or before until (default
// the horizon), at most count of them.
func parseRecurring(s wakeupSpec, a wakeupScheduleArgs, now int64) (wakeupSpec, error) {
	horizon := now + WakeupHorizon
	if a.On != "" || a.Room != "" {
		return s, badArg("every takes no on or room; send one or the other.")
	}
	every, err := intArg(a.Every, "every", "seconds", WakeupEveryMin, WakeupEveryMax)
	if err != nil {
		return s, err
	}
	s.kind, s.every, s.dueAt, s.until = "time", every, now+every, horizon
	if a.At != nil {
		at, ok := Integer(a.At, horizon)
		if !ok || at <= now {
			return s, badArg(wakeupTimeRule("at"))
		}
		s.dueAt, s.atSet = at, true
	}
	if a.Until != nil {
		until, ok := Integer(a.Until, horizon)
		if !ok {
			return s, badArg(wakeupTimeRule("until"))
		}
		s.until, s.untilSet = until, true
	}
	if s.dueAt > s.until {
		return s, badArg("until must not be before the first firing (at, else now plus every).")
	}
	s.count = (s.until-s.dueAt)/every + 1
	if a.Count != nil {
		count, err := intArg(a.Count, "count", "", 1, WakeupFiresMax)
		if err != nil {
			return s, err
		}
		s.count, s.countSet = min(s.count, count), true
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
		s, err := parseWakeup(c.Args, c.Now)
		if err != nil {
			return Quote{}, err
		}
		// A recurring wake-up pays for every firing it can make, when set.
		return Quote{Resource: allowance.Credit, Max: c.Price.For(0) * s.fires()}, nil
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
	On         string `json:"on"` // time, reply, mention, room or message
	At         int64  `json:"at,omitempty"`
	Room       string `json:"room,omitempty"`
	Until      int64  `json:"until,omitempty"`
	State      string `json:"state"` // active, fired, cancelled or expired
	CreatedAt  int64  `json:"created_at"`
	FinishedAt int64  `json:"finished_at,omitempty"`
	Event      string `json:"event,omitempty"` // the message that fired it
	// A recurring wake-up: every seconds from at, count firings paid for,
	// fired_count made, next_due the next one while active.
	Every      int64  `json:"every,omitempty"`
	Count      int64  `json:"count,omitempty"`
	FiredCount *int64 `json:"fired_count,omitempty"`
	NextDue    int64  `json:"next_due,omitempty"`
}

const wakeupColumns = "id,key,kind,room,due_at,until,state,created_at,finished_at,event,every,start_at,max_fires,fired_count"

func scanWakeup(row interface{ Scan(...any) error }) (wakeupView, error) {
	var v wakeupView
	var start, fired int64
	err := row.Scan(&v.ID, &v.Key, &v.On, &v.Room, &v.At, &v.Until, &v.State, &v.CreatedAt, &v.FinishedAt, &v.Event, &v.Every, &start, &v.Count, &fired)
	if v.Every > 0 {
		if v.State == "active" {
			v.NextDue = v.At
		}
		v.At, v.FiredCount = start, &fired
	}
	if v.On == "room" && v.Room == messageRoom {
		v.On, v.Room = "message", ""
	}
	if v.On == "room" && v.Room == receivedRoom {
		v.On, v.Room = "received", ""
	}
	return v, err
}

// storedKind is the kind and room a wake-up is stored under.
func storedKind(kind, room string) (string, string) {
	switch kind {
	case "message":
		return "room", messageRoom
	case "received":
		return "room", receivedRoom
	}
	return kind, room
}

// same reports whether an active wake-up is the one s asks for; a retry that
// omits until matches whatever until the registration defaulted to.
// A recurring retry that omits at or count likewise matches the defaults.
func (v wakeupView) same(s wakeupSpec) bool {
	if v.Key != s.key || v.On != s.kind || v.Room != s.room || v.Every != s.every || (v.Until != s.until && s.untilSet) {
		return false
	}
	if s.every == 0 {
		return v.At == s.dueAt
	}
	return (v.At == s.dueAt || !s.atSet) && (v.Count == s.count || !s.countSet)
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
		var start int64
		if s.every > 0 {
			var none int64
			start = s.dueAt
			v.Every, v.Count, v.FiredCount, v.NextDue = s.every, s.count, &none, s.dueAt
		}
		kind, room := storedKind(v.On, v.Room)
		if _, err = tx.ExecContext(ctx, "INSERT INTO wakeups(id,account,key,kind,room,due_at,until,from_seq,state,created_at,every,start_at,max_fires) VALUES(?,?,?,?,?,?,?,?,'active',?,?,?,?)",
			v.ID, account, v.Key, kind, room, s.dueAt, v.Until, fromSeq, v.CreatedAt, s.every, start, v.Count); err != nil {
			return Result{}, err
		}
		// 1 credit per firing: a recurring wake-up pays for all of them now.
		return wakeupResult(ctx, tx, account, v, false, c.Price.For(0)*s.fires())
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
			v.State, v.FinishedAt, v.NextDue, cancelled = "cancelled", c.Now, 0, true
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
			n, err := intArg(a.After, "after", "", 0, 1<<53)
			if err != nil {
				return nil, err
			}
			after = n
		}
		if a.Limit != nil {
			n, err := intArg(a.Limit, "limit", "", 1, WakeupPageMax)
			if err != nil {
				return nil, err
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

// PendingWakeups is account's active wake-ups, newest first: at most
// WakeupsPerAccount, so the list is always whole. journal.get reads it in the
// board's transaction; it needs the wakeup service's table (the service
// enabled).
func PendingWakeups(ctx context.Context, q allowance.Querier, account string) (any, error) {
	return wakeupList(ctx, q, "SELECT "+wakeupColumns+" FROM wakeups WHERE account=? AND state='active' ORDER BY created_at DESC, id DESC LIMIT ?", account, WakeupsPerAccount)
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
	kept := out[:0]
	for _, v := range out {
		// A delivery to a receiver is the owner's private business.
		if v.On == "received" && !n.Own {
			continue
		}
		kept = append(kept, v)
	}
	out = kept
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

// addWakeNotice stores the notice a firing leaves in the account's inbox,
// in the firing's transaction: every kind of firing (time, recurring, event,
// received) writes it here. seq is the newest message's sequence when it
// fired: an updates.get cursor at or before it has not yet shown the notice.
func addWakeNotice(ctx context.Context, tx *sql.Tx, id, account, kind, event, room string, dueAt, firedAt, seq int64, late bool) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO wakeup_notices(wakeup,account,kind,event,room,due_at,fired_at,event_seq,late) VALUES(?,?,?,?,?,?,?,?,?)", id, account, kind, event, room, dueAt, firedAt, seq, late)
	return err
}

// fireReceived fires the account's active on:"received" wake-ups (at most
// WakeupsPerAccount), in a delivery's transaction: latest is the newest
// message's sequence, so an updates.get cursor at or before it has not yet
// shown the notice.
func fireReceived(ctx context.Context, tx *sql.Tx, account string, latest, now int64) error {
	rows, err := tx.QueryContext(ctx, "SELECT id FROM wakeups WHERE account=? AND state='active' AND kind='room' AND room=? LIMIT ?", account, receivedRoom, WakeupsPerAccount)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		res, err := tx.ExecContext(ctx, "UPDATE wakeups SET state='fired', finished_at=? WHERE id=? AND state='active'", now, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			continue
		}
		if err = addWakeNotice(ctx, tx, id, account, "received", "", "", 0, now, latest, false); err != nil {
			return err
		}
	}
	return nil
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
	if err = addWakeNotice(ctx, p.tx, id, account, kind, event, room, dueAt, p.now, p.latest, late); err != nil {
		return err
	}
	p.fired++
	return nil
}

// dueWakeup is a due time wake-up as the clock reads it; every > 0 is a
// recurring one.
type dueWakeup struct {
	id, account                            string
	at, every, until, maxFires, firedCount int64
}

// fireRecurring fires a recurring wake-up and advances it to its first
// period after now: periods missed while the clock was down (or held by the
// minute bound) are skipped, never fired in a burst, and the one firing is
// marked late. After its last firing (count reached, or the next period past
// until) the wake-up is done: state fired. Rows are never deleted.
func (p *wakePass) fireRecurring(ctx context.Context, d dueWakeup) error {
	p.budget--
	next := d.at + d.every
	if next <= p.now {
		next = d.at + ((p.now-d.at)/d.every+1)*d.every
	}
	fired := d.firedCount + 1
	query, args := "UPDATE wakeups SET due_at=?, fired_count=? WHERE id=? AND state='active' AND due_at=?", []any{next, fired, d.id, d.at}
	if fired >= d.maxFires || next > d.until {
		query, args = "UPDATE wakeups SET state='fired', finished_at=?, fired_count=? WHERE id=? AND state='active' AND due_at=?", []any{p.now, fired, d.id, d.at}
	}
	res, err := p.tx.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil
	}
	late := p.now-d.at > WakeupLateAfter
	if err = addWakeNotice(ctx, p.tx, d.id, d.account, "time", "", "", d.at, p.now, p.latest, late); err != nil {
		return err
	}
	p.fired++
	return nil
}

// Work is one bounded pass of the wake-up clock, in one transaction: expire
// event wake-ups past their until, fire due time wake-ups (the oldest first,
// so after a restart missed ones catch up in order, marked late; a recurring
// one fires once and skips the periods it missed), then read
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
	var dues []dueWakeup
	if p.budget > 0 {
		rows, err := tx.QueryContext(ctx, "SELECT id,account,due_at,every,until,max_fires,fired_count FROM wakeups WHERE state='active' AND kind='time' AND due_at<=? ORDER BY due_at, id LIMIT ?", now, p.budget)
		if err != nil {
			return 0, err
		}
		for rows.Next() {
			var d dueWakeup
			if err = rows.Scan(&d.id, &d.account, &d.at, &d.every, &d.until, &d.maxFires, &d.firedCount); err != nil {
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
		if d.every > 0 {
			err = p.fireRecurring(ctx, d)
		} else {
			err = p.fire(ctx, d.id, d.account, "time", "", "", d.at)
		}
		if err != nil {
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
		// Message wake-ups (RFC0013 §4): the conversation's active members
		// and those it is a request to, at most 2*(RoomMembersMax+1)
		// accounts with WakeupsPerAccount each; membership is the check.
		if ev.Conversation {
			var watchers []any
			for _, a := range append(append([]string{}, ev.Members...), ev.RequestTo...) {
				if a != "" && a != ev.Author {
					watchers = append(watchers, a)
				}
			}
			if len(watchers) > 0 {
				matches, err := wakeMatches(ctx, p.tx, "SELECT id,account,'message' FROM wakeups WHERE state='active' AND kind='room' AND room=? AND from_seq<? AND account IN (?"+strings.Repeat(",?", len(watchers)-1)+") ORDER BY created_at, id",
					append([]any{messageRoom, ev.Seq}, watchers...)...)
				if err != nil {
					return after, err
				}
				for _, m := range matches {
					if p.budget <= 0 {
						return after, nil
					}
					if err = p.fire(ctx, m.id, m.account, m.kind, ev.ID, ev.Room, 0); err != nil {
						return after, err
					}
				}
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

// WakeDay is one UTC day of receiver and wake-up use for /stats: counts
// only, split by kind, never an id, a key, a label or a body.
type WakeDay struct {
	Day string // YYYY-MM-DD
	// ReceiversCreated counts receivers created, ReceiverDeliveries the
	// deliveries stored.
	ReceiversCreated, ReceiverDeliveries int64
	// WakeupsOneShot, WakeupsEvent and WakeupsRecurring count wake-ups
	// scheduled: once at a time, on an event, or every period.
	WakeupsOneShot, WakeupsEvent, WakeupsRecurring int64
	// WakeupsFired counts firings, each period of a recurring one included.
	WakeupsFired int64
}

// WakeStats is WakeDay over a range of days, with which of the two
// services is enabled; Days is nil while neither is.
type WakeStats struct {
	Receivers, Wakeups bool
	Days               []WakeDay
}

// ReadWakeStats counts receiver and wake-up use over the days UTC days
// ending with the one holding now, oldest first. Counts come from the tables
// at read time; nothing is stored.
func (r *Registry) ReadWakeStats(ctx context.Context, q allowance.Querier, now int64, days int) (WakeStats, error) {
	_, recvErr := r.Lookup(ReceiverID)
	_, wakeErr := r.Lookup("wakeup")
	st := WakeStats{Receivers: recvErr == nil, Wakeups: wakeErr == nil}
	if !st.Receivers && !st.Wakeups || days < 1 {
		return WakeStats{}, nil
	}
	first := now/86400 - int64(days) + 1
	out := make([]WakeDay, days)
	for i := range out {
		out[i].Day = time.Unix((first+int64(i))*86400, 0).UTC().Format("2006-01-02")
	}
	type count struct {
		query string
		arg   int64
		add   func(d *WakeDay, kind, n int64)
	}
	var queries []count
	if st.Receivers {
		queries = append(queries,
			count{"SELECT created_at/86400, 0, count(*) FROM receivers WHERE created_at>=? GROUP BY 1", first * 86400, func(d *WakeDay, _, n int64) { d.ReceiversCreated += n }},
			// receiver_days counts each stored delivery in its transaction.
			count{"SELECT day, 0, sum(count) FROM receiver_days WHERE day>=? GROUP BY 1", first, func(d *WakeDay, _, n int64) { d.ReceiverDeliveries += n }})
	}
	if st.Wakeups {
		queries = append(queries,
			count{"SELECT created_at/86400, CASE WHEN kind<>'time' THEN 1 WHEN every>0 THEN 2 ELSE 0 END, count(*) FROM wakeups WHERE created_at>=? GROUP BY 1,2", first * 86400, func(d *WakeDay, kind, n int64) {
				switch kind {
				case 1:
					d.WakeupsEvent += n
				case 2:
					d.WakeupsRecurring += n
				default:
					d.WakeupsOneShot += n
				}
			}},
			count{"SELECT fired_at/86400, 0, count(*) FROM wakeup_notices WHERE fired_at>=? GROUP BY 1", first * 86400, func(d *WakeDay, _, n int64) { d.WakeupsFired += n }})
	}
	for _, c := range queries {
		if err := countDays(ctx, q, c.query, c.arg, first, days, func(i int, kind, n int64) { c.add(&out[i], kind, n) }); err != nil {
			return WakeStats{}, err
		}
	}
	st.Days = out
	return st, nil
}
