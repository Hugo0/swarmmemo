package services_test

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"swarmmemo/internal/services"
	"swarmmemo/internal/services/servicestest"
)

// wakeupByKey is the owner's view of one wake-up from list (active or
// recent).
func (r *wakeRig) wakeupByKey(account, key string) map[string]any {
	r.t.Helper()
	list, err := r.read(subjectOf(account), "wakeup", "list", map[string]any{})
	if err != nil {
		r.t.Fatal(err)
	}
	for _, part := range []string{"active", "recent"} {
		for _, v := range get(list, "result", part).([]any) {
			if get(v, "key") == key {
				return v.(map[string]any)
			}
		}
	}
	r.t.Fatalf("no wake-up %q in %+v", key, list)
	return nil
}

func TestWakeupRecurring(t *testing.T) {
	r := newWakeRig(t)
	start := int64(wakeT0 + 600)
	args := map[string]any{"key": "hourly", "every": 3600, "at": start, "count": 5}
	// 1 credit per firing, paid when set: five firings cost 5.
	if _, err := r.call("alice", "wakeup", "schedule", args, 4); code(err) != "price_exceeds_max" {
		t.Fatalf("a recurring wake-up quotes all its firings: %v", err)
	}
	first := r.mustCall("alice", "wakeup", "schedule", args, 5)
	w := get(first, "result", "wakeup")
	id, _ := get(w, "id").(string)
	if get(first, "call", "cost") != float64(5) || get(w, "on") != "time" || get(w, "every") != float64(3600) || get(w, "count") != float64(5) ||
		get(w, "fired_count") != float64(0) || get(w, "next_due") != float64(start) || get(w, "at") != float64(start) || get(w, "until") != float64(wakeT0+services.WakeupHorizon) {
		t.Fatalf("schedule: %+v", first)
	}
	// A retry is the same registration, for the least a write costs, even
	// without count (the default matches; it is still quoted at its default
	// firings, and the rest of the hold is refunded); another period is a
	// conflict.
	for _, retry := range []map[string]any{args, {"key": "hourly", "every": 3600, "at": start}} {
		again := r.mustCall("alice", "wakeup", "schedule", retry, 1000)
		if get(again, "result", "duplicate") != true || get(again, "call", "cost") != float64(1) || get(again, "result", "wakeup", "id") != id {
			t.Fatalf("retry: %+v", again)
		}
	}
	if _, err := r.call("alice", "wakeup", "schedule", map[string]any{"key": "hourly", "every": 7200, "at": start}, 1000); code(err) != "wakeup_conflict" {
		t.Fatalf("another period under the same key: %v", err)
	}
	if _, err := r.call("alice", "wakeup", "schedule", map[string]any{"key": "hourly", "at": start}, 1); code(err) != "wakeup_conflict" {
		t.Fatalf("a one-shot under a recurring key: %v", err)
	}

	// It fires when due and advances one period; the same pass never fires
	// it twice.
	r.now = start
	if n := r.work(); n != 1 {
		t.Fatalf("first firing: %d", n)
	}
	if n := r.work(); n != 0 {
		t.Fatalf("fired once per period: %d", n)
	}
	v := r.wakeupByKey("alice", "hourly")
	if get(v, "state") != "active" || get(v, "fired_count") != float64(1) || get(v, "next_due") != float64(start+3600) || get(v, "at") != float64(start) {
		t.Fatalf("advanced: %+v", v)
	}
	// The clock is down for three periods: one late firing, then it skips
	// ahead to the next period after now, never a burst.
	r.now = start + 3*3600 + 100
	r.e = r.open()
	if n := r.work(); n != 1 {
		t.Fatalf("missed periods fire once: %d", n)
	}
	if n := r.work(); n != 0 {
		t.Fatalf("no catch-up burst: %d", n)
	}
	v = r.wakeupByKey("alice", "hourly")
	if get(v, "fired_count") != float64(2) || get(v, "next_due") != float64(start+4*3600) {
		t.Fatalf("skipped ahead: %+v", v)
	}
	page, err := r.read(subjectOf("alice"), "wakeup", "notices", map[string]any{})
	notices, _ := get(page, "result", "notices").([]any)
	if err != nil || len(notices) != 2 || get(notices[0], "late") != nil || get(notices[1], "late") != true || get(notices[1], "at") != float64(start+3600) ||
		get(notices[0], "id") != id || get(notices[1], "id") != id {
		t.Fatalf("one notice per firing, the late one marked: %+v %v", page, err)
	}
	// updates.get (data.wakeups) shows both firings, newest first: the same
	// id, told apart by fired_at.
	got := r.notices("alice", "alice", 0)
	if len(got) != 2 || get(got[0], "fired_at") != float64(r.now) || get(got[1], "fired_at") != float64(start) || get(got[0], "on") != "time" {
		t.Fatalf("updates notices: %+v", got)
	}
	// The rest fire; after the fifth (its count) it is done, never deleted.
	for i := int64(4); i <= 6; i++ {
		r.now = start + i*3600
		if n := r.work(); n != 1 {
			t.Fatalf("firing at period %d: %d", i, n)
		}
	}
	v = r.wakeupByKey("alice", "hourly")
	if get(v, "state") != "fired" || get(v, "fired_count") != float64(5) || get(v, "next_due") != nil || get(v, "finished_at") != float64(r.now) {
		t.Fatalf("done after its count: %+v", v)
	}
	r.now += 3600
	if n := r.work(); n != 0 {
		t.Fatalf("a finished wake-up never fires: %d", n)
	}
	// The key is free again.
	r.mustCall("alice", "wakeup", "schedule", map[string]any{"key": "hourly", "every": 3600, "count": 1}, 1)
}

func TestWakeupRecurringUntilCancelAndBounds(t *testing.T) {
	r := newWakeRig(t)
	// Without at, the first firing is one period from now; without until or
	// count, the firings that fit in the 30-day horizon: 30 daily ones.
	daily := r.mustCall("alice", "wakeup", "schedule", map[string]any{"key": "daily", "every": 86400}, 100)
	if get(daily, "call", "cost") != float64(30) || get(daily, "result", "wakeup", "next_due") != float64(wakeT0+86400) || get(daily, "result", "wakeup", "count") != float64(30) {
		t.Fatalf("defaults: %+v", daily)
	}
	// until ends it: two firings fit before it, so it costs 2; a count above
	// what fits is capped by until.
	until := int64(wakeT0 + 2*86400 + 43200)
	two := r.mustCall("alice", "wakeup", "schedule", map[string]any{"key": "two", "every": 86400, "until": until, "count": 10}, 100)
	if get(two, "call", "cost") != float64(2) || get(two, "result", "wakeup", "count") != float64(2) || get(two, "result", "wakeup", "until") != float64(until) {
		t.Fatalf("until: %+v", two)
	}
	r.now = wakeT0 + 86400
	if n := r.work(); n != 2 {
		t.Fatalf("both daily wake-ups fire: %d", n)
	}
	r.now = wakeT0 + 2*86400
	r.work()
	if v := r.wakeupByKey("alice", "two"); get(v, "state") != "fired" || get(v, "fired_count") != float64(2) {
		t.Fatalf("done at until: %+v", v)
	}
	// Periods missed past until: one late firing, and it is done.
	late := r.mustCall("bob", "wakeup", "schedule", map[string]any{"key": "late", "every": 900, "at": r.now + 900, "until": r.now + 900*3}, 100)
	if get(late, "call", "cost") != float64(3) {
		t.Fatalf("three firings fit: %+v", late)
	}
	r.now += 900 * 10
	if n := r.work(); n != 1 {
		t.Fatalf("one late firing: %d", n)
	}
	if v := r.wakeupByKey("bob", "late"); get(v, "state") != "fired" || get(v, "fired_count") != float64(1) {
		t.Fatalf("past until after its late firing: %+v", v)
	}
	// Cancel stops a recurring wake-up and shows it as it stood; nothing is
	// refunded.
	r.mustCall("carol", "wakeup", "schedule", map[string]any{"key": "q", "every": 900, "count": 4}, 4)
	r.now += 900
	r.work()
	c := r.mustCall("carol", "wakeup", "cancel", map[string]any{"key": "q"}, 1)
	if get(c, "result", "cancelled") != true || get(c, "result", "wakeup", "state") != "cancelled" || get(c, "result", "wakeup", "every") != float64(900) || get(c, "result", "wakeup", "fired_count") != float64(1) || get(c, "result", "wakeup", "next_due") != nil {
		t.Fatalf("cancel: %+v", c)
	}
	r.now += 900
	r.work()
	if v := r.wakeupByKey("carol", "q"); get(v, "state") != "cancelled" || get(v, "next_due") != nil || get(v, "fired_count") != float64(1) {
		t.Fatalf("a cancelled wake-up never fires: %+v", v)
	}
	// A recurring wake-up counts once toward the active limit.
	for i := 0; i < services.WakeupsPerAccount; i++ {
		r.mustCall("dave", "wakeup", "schedule", map[string]any{"key": fmt.Sprintf("r%d", i), "every": 3600, "count": 2}, 2)
	}
	if _, err := r.call("dave", "wakeup", "schedule", map[string]any{"key": "more", "every": 3600, "count": 2}, 2); code(err) != "wakeup_limit" {
		t.Fatalf("the per-agent limit: %v", err)
	}
	for _, bad := range []map[string]any{
		{"key": "x", "every": services.WakeupEveryMin - 1},
		{"key": "x", "every": services.WakeupEveryMax + 1},
		{"key": "x", "every": "3600"},
		{"key": "x", "every": 3600, "on": "reply"},
		{"key": "x", "every": 3600, "room": "lobby"},
		{"key": "x", "every": 3600, "count": 0},
		{"key": "x", "every": 3600, "count": services.WakeupFiresMax + 1},
		{"key": "x", "every": 3600, "at": r.now},
		{"key": "x", "every": 3600, "at": r.now + 7200, "until": r.now + 3600},
		{"key": "x", "every": 3600, "until": r.now + services.WakeupHorizon + 1},
		{"key": "x", "at": r.now + 60, "count": 2},
		{"key": "x", "on": "reply", "count": 2},
	} {
		if _, err := r.call("erin", "wakeup", "schedule", bad, 10000); code(err) != "invalid_service_data" {
			t.Errorf("%v must be invalid_service_data, got %v", bad, err)
		}
	}
}

// TestWakeupMigrationAddsRecurringColumns: a wakeups table from before
// recurring wake-ups gains the columns, its rows still fire once, and the
// migration runs twice without change.
func TestWakeupMigrationAddsRecurringColumns(t *testing.T) {
	db, err := sql.Open("sqlite", t.TempDir()+"/old.db")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	if _, err = db.Exec(`CREATE TABLE wakeups (
 id TEXT PRIMARY KEY, account TEXT NOT NULL, key TEXT NOT NULL,
 kind TEXT NOT NULL CHECK(kind IN ('time','reply','mention','room')), room TEXT NOT NULL DEFAULT '',
 due_at INTEGER NOT NULL DEFAULT 0, until INTEGER NOT NULL DEFAULT 0, from_seq INTEGER NOT NULL DEFAULT 0,
 state TEXT NOT NULL CHECK(state IN ('active','fired','cancelled','expired')),
 created_at INTEGER NOT NULL, finished_at INTEGER NOT NULL DEFAULT 0, event TEXT NOT NULL DEFAULT '');
INSERT INTO wakeups(id,account,key,kind,due_at,state,created_at) VALUES('` + strings.Repeat("a", 32) + `','alice','old','time',` + fmt.Sprint(wakeT0+60) + `,'active',` + fmt.Sprint(wakeT0) + `);`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(services.Schema); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err = services.MigrateWakeups(tx); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	var columns int
	if err = db.QueryRow("SELECT count(*) FROM pragma_table_info('wakeups') WHERE name IN ('every','start_at','max_fires','fired_count')").Scan(&columns); err != nil || columns != 4 {
		t.Fatalf("columns: %d %v", columns, err)
	}
	r := &wakeRig{t: t, db: db, meter: servicestest.NewMeter(1 << 30), board: newFakeBoard(), now: wakeT0 + 60}
	r.open = func() *services.Engine {
		reg := services.NewBuiltinRegistry([]string{"wakeup"}, services.Deps{Board: r.board})
		return services.NewEngine(services.Config{DB: db, Registry: reg, Meter: r.meter, Now: func() int64 { return r.now }})
	}
	r.e = r.open()
	if n := r.work(); n != 1 {
		t.Fatalf("the old one-shot fires: %d", n)
	}
	if v := r.wakeupByKey("alice", "old"); get(v, "state") != "fired" || get(v, "every") != nil || get(v, "fired_count") != nil {
		t.Fatalf("a one-shot shows no recurring fields: %+v", v)
	}
}
