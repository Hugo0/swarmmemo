package services_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/services"
	"swarmmemo/internal/services/servicestest"
)

// fakeBoard is a services.BoardView held in memory.
type fakeBoard struct {
	mu      sync.Mutex
	events  []services.BoardEvent
	private map[string]map[string]bool // private room -> member accounts
	rooms   map[string]bool            // every room that exists
}

func newFakeBoard() *fakeBoard {
	return &fakeBoard{private: map[string]map[string]bool{}, rooms: map[string]bool{"lobby": true, "ops": true}}
}

func (b *fakeBoard) post(ev services.BoardEvent) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	ev.Seq = int64(len(b.events) + 1)
	if ev.ID == "" {
		ev.ID = fmt.Sprintf("%032x", ev.Seq)
	}
	b.events = append(b.events, ev)
	return ev.Seq
}

func (b *fakeBoard) LatestSeq(context.Context, allowance.Querier) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return int64(len(b.events)), nil
}

func (b *fakeBoard) EventsAfter(_ context.Context, _ allowance.Querier, after int64, limit int) ([]services.BoardEvent, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []services.BoardEvent
	for _, ev := range b.events {
		if ev.Seq > after && len(out) < limit {
			out = append(out, ev)
		}
	}
	return out, nil
}

func (b *fakeBoard) AddInboxEntry(context.Context, *sql.Tx, services.InboxEntry) error { return nil }

func (b *fakeBoard) CanRead(_ context.Context, _ allowance.Querier, account, room string) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if members, ok := b.private[room]; ok {
		return members[account], nil
	}
	return b.rooms[room], nil
}

func (b *fakeBoard) Member(_ context.Context, _ allowance.Querier, account, room string) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.private[room][account], nil
}

type wakeRig struct {
	t     *testing.T
	db    *sql.DB
	e     *services.Engine
	meter *servicestest.Meter
	board *fakeBoard
	now   int64
	n     int
	open  func() *services.Engine
}

const wakeT0 = 1_790_000_000

func newWakeRig(t *testing.T) *wakeRig {
	db := openDB(t)
	r := &wakeRig{t: t, meter: servicestest.NewMeter(1 << 30), board: newFakeBoard(), now: wakeT0}
	r.open = func() *services.Engine {
		reg := services.NewBuiltinRegistry([]string{"notary", "wakeup"}, services.Deps{Board: r.board, ServiceID: "swarmmemo.com", NotaryKey: testNotaryKey})
		return services.NewEngine(services.Config{DB: db, Registry: reg, Meter: r.meter, Now: func() int64 { return r.now }})
	}
	r.e = r.open()
	r.db = db
	return r
}

func subjectOf(account string) allowance.Subject {
	return allowance.Subject{ID: account, KeyID: account, Signed: true}
}

func (r *wakeRig) call(account, service, method string, args any, maxCost int64) (map[string]any, error) {
	r.t.Helper()
	raw, _ := json.Marshal(map[string]any{"schema": 1, "method": method, "args": args, "max_cost": maxCost})
	r.n++
	tx, err := r.db.Begin()
	if err != nil {
		r.t.Fatal(err)
	}
	defer tx.Rollback()
	out, err := r.e.Call(context.Background(), tx, services.Request{Service: service, Data: string(raw), Subject: subjectOf(account), RequestKey: fmt.Sprintf("id:%d", r.n)}, r.now)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		r.t.Fatal(err)
	}
	return roundTrip(r.t, out.Data), nil
}

func (r *wakeRig) mustCall(account, service, method string, args any, maxCost int64) map[string]any {
	r.t.Helper()
	out, err := r.call(account, service, method, args, maxCost)
	if err != nil {
		r.t.Fatalf("%s.%s %v: %v", service, method, args, err)
	}
	return out
}

func (r *wakeRig) read(subject allowance.Subject, service, method string, args any) (map[string]any, error) {
	r.t.Helper()
	raw, _ := json.Marshal(map[string]any{"schema": 1, "method": method, "args": args})
	out, err := r.e.Read(context.Background(), r.db, services.Request{Service: service, Data: string(raw), Subject: subject}, r.now)
	if err != nil {
		return nil, err
	}
	return roundTrip(r.t, out), nil
}

func (r *wakeRig) work() int {
	r.t.Helper()
	n, err := r.e.Work(context.Background())
	if err != nil {
		r.t.Fatal(err)
	}
	return n
}

func (r *wakeRig) notices(account, caller string, since int64) []any {
	r.t.Helper()
	out, err := r.e.Notices(context.Background(), r.db, services.NoticeQuery{Account: account, Caller: caller, Since: since, Received: -1, Wakeups: -1, Now: r.now})
	if err != nil {
		r.t.Fatal(err)
	}
	return roundTrip(r.t, out)["wakeups"].([]any)
}

func roundTrip(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err = json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func get(v any, path ...string) any {
	for _, p := range path {
		m, _ := v.(map[string]any)
		v = m[p]
	}
	return v
}

func spends(t *testing.T, r *wakeRig, service string) int {
	t.Helper()
	entries, err := r.meter.Entries(context.Background(), r.db)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if e.Kind == "spend" && e.Service == service {
			n++
		}
	}
	return n
}

func TestWakeupScheduleIdempotentAndBounded(t *testing.T) {
	r := newWakeRig(t)
	at := int64(wakeT0 + 3600)
	first := r.mustCall("alice", "wakeup", "schedule", map[string]any{"key": "standup", "at": at}, 1)
	id, _ := get(first, "result", "wakeup", "id").(string)
	if len(id) != 32 || get(first, "result", "duplicate") != false || get(first, "call", "cost") != float64(1) || get(first, "result", "wakeup", "on") != "time" {
		t.Fatalf("schedule: %+v", first)
	}
	again := r.mustCall("alice", "wakeup", "schedule", map[string]any{"key": "standup", "at": at}, 1)
	if get(again, "result", "wakeup", "id") != id || get(again, "result", "duplicate") != true || get(again, "call", "cost") != float64(1) {
		t.Fatalf("the same registration again is the same wake-up, for the least a write costs: %+v", again)
	}
	if n := spends(t, r, "wakeup"); n != 2 {
		t.Fatalf("one charge for the registration and one for the repeat, got %d", n)
	}
	if _, err := r.call("alice", "wakeup", "schedule", map[string]any{"key": "standup", "at": at + 1}, 1); code(err) != "wakeup_conflict" {
		t.Fatalf("same key, other wake-up: %v", err)
	}
	// An event wake-up retried without until is the same registration, even
	// a minute later, when the default horizon has moved.
	r.mustCall("alice", "wakeup", "schedule", map[string]any{"key": "replies", "on": "reply"}, 1)
	r.now += 60
	if again := r.mustCall("alice", "wakeup", "schedule", map[string]any{"key": "replies", "on": "reply"}, 1); get(again, "result", "duplicate") != true {
		t.Fatalf("event retry: %+v", again)
	}
	for i := 2; i < services.WakeupsPerAccount; i++ {
		r.mustCall("alice", "wakeup", "schedule", map[string]any{"key": fmt.Sprintf("k%d", i), "at": at}, 1)
	}
	if _, err := r.call("alice", "wakeup", "schedule", map[string]any{"key": "one-more", "at": at}, 1); code(err) != "wakeup_limit" {
		t.Fatalf("the per-account cap: %v", err)
	}
	r.mustCall("bob", "wakeup", "schedule", map[string]any{"key": "one-more", "at": at}, 1) // another account is unaffected
	for _, bad := range []map[string]any{
		{"key": "x", "at": r.now},
		{"key": "x", "at": r.now + services.WakeupHorizon + 1},
		{"key": "x", "at": -5},
		{"key": "x", "at": "3600"},
		{"key": "", "at": at},
		{"key": "a b", "at": at},
		{"key": "x"},
		{"key": "x", "on": "room"},
		{"key": "x", "on": "room", "room": "Lobby"},
		{"key": "x", "on": "reply", "room": "lobby"},
		{"key": "x", "on": "reply", "until": r.now},
		{"key": "x", "on": "sometimes"},
		{"key": "x", "at": at, "on": "reply"},
		{"key": "x", "at": at, "url": "https://example.com"},
	} {
		if _, err := r.call("carol", "wakeup", "schedule", bad, 1); code(err) != "invalid_service_data" {
			t.Errorf("%v must be invalid_service_data, got %v", bad, err)
		}
	}
	if _, err := r.call("carol", "wakeup", "schedule", map[string]any{"key": "x", "on": "room", "room": "nowhere"}, 1); code(err) != "wakeup_room_not_found" {
		t.Fatalf("an unknown room: %v", err)
	}
	r.board.private["secret"] = map[string]bool{"dave": true}
	if _, err := r.call("carol", "wakeup", "schedule", map[string]any{"key": "x", "on": "room", "room": "secret"}, 1); code(err) != "wakeup_room_not_found" {
		t.Fatalf("a private room the agent cannot read: %v", err)
	}
	if _, err := r.call("carol", "wakeup", "schedule", map[string]any{"key": "x", "at": at}, 0); code(err) != "price_exceeds_max" {
		t.Fatalf("priced through the catalogue: %v", err)
	}

	// Cancel is idempotent; list is the owner's own.
	c1 := r.mustCall("alice", "wakeup", "cancel", map[string]any{"key": "standup"}, 1)
	c2 := r.mustCall("alice", "wakeup", "cancel", map[string]any{"id": id}, 1)
	if get(c1, "result", "cancelled") != true || get(c2, "result", "cancelled") != false || get(c2, "result", "wakeup", "state") != "cancelled" || get(c1, "call", "cost") != float64(1) || get(c2, "call", "cost") != float64(1) {
		t.Fatalf("cancel: %+v %+v", c1, c2)
	}
	if _, err := r.call("bob", "wakeup", "cancel", map[string]any{"id": id}, 1); code(err) != "wakeup_not_found" {
		t.Fatalf("another agent's wake-up: %v", err)
	}
	if _, err := r.call("alice", "wakeup", "cancel", map[string]any{"key": "standup", "id": id}, 1); code(err) != "invalid_service_data" {
		t.Fatalf("key or id, not both: %v", err)
	}
	list, err := r.read(subjectOf("alice"), "wakeup", "list", map[string]any{})
	if err != nil || len(get(list, "result", "active").([]any)) != services.WakeupsPerAccount-1 || len(get(list, "result", "recent").([]any)) != 1 {
		t.Fatalf("list: %+v %v", list, err)
	}
	if _, err = r.read(allowance.Subject{ID: "anon:x"}, "wakeup", "list", map[string]any{}); code(err) != "signature_required" {
		t.Fatalf("list is signed: %v", err)
	}
	// A cancelled key can be registered again.
	r.mustCall("alice", "wakeup", "schedule", map[string]any{"key": "standup", "at": at + 5}, 1)
}

func TestWakeupFiresAtTimeAndCatchesUpAfterRestart(t *testing.T) {
	r := newWakeRig(t)
	r.mustCall("alice", "wakeup", "schedule", map[string]any{"key": "soon", "at": wakeT0 + 10}, 1)
	r.mustCall("alice", "wakeup", "schedule", map[string]any{"key": "later", "at": wakeT0 + 7200}, 1)
	r.now = wakeT0 + 5
	if n := r.work(); n != 0 {
		t.Fatalf("nothing is due: %d", n)
	}
	r.now = wakeT0 + 10
	if n := r.work(); n != 1 {
		t.Fatalf("one firing: %d", n)
	}
	if n := r.work(); n != 0 {
		t.Fatalf("a wake-up fires once: %d", n)
	}
	got := r.notices("alice", "alice", 0)
	if len(got) != 1 || get(got[0], "on") != "time" || get(got[0], "at") != float64(wakeT0+10) || get(got[0], "late") != nil || get(got[0], "fired_at") != float64(wakeT0+10) {
		t.Fatalf("updates notice: %+v", got)
	}
	// Anyone else sees that it fired, not when it was due (security review
	// 1.20, L14).
	got = r.notices("alice", "anon:x", 0)
	if len(got) != 1 || get(got[0], "at") != nil || get(got[0], "late") != nil || get(got[0], "fired_at") != float64(wakeT0+10) {
		t.Fatalf("updates notice to another reader: %+v", got)
	}
	// The process stops; the second wake-up's time passes; a new engine on
	// the same database fires it, marked late.
	r.now = wakeT0 + 9000
	r.e = r.open()
	if n := r.work(); n != 1 {
		t.Fatalf("the missed wake-up catches up: %d", n)
	}
	page, err := r.read(subjectOf("alice"), "wakeup", "notices", map[string]any{})
	notices, _ := get(page, "result", "notices").([]any)
	if err != nil || len(notices) != 2 || get(notices[1], "late") != true || get(page, "result", "next_after") != get(notices[1], "seq") {
		t.Fatalf("notices: %+v %v", page, err)
	}
	after := get(page, "result", "next_after")
	if page, _ = r.read(subjectOf("alice"), "wakeup", "notices", map[string]any{"after": after}); len(get(page, "result", "notices").([]any)) != 0 {
		t.Fatalf("the notices cursor is exact: %+v", page)
	}
	if page, _ = r.read(subjectOf("bob"), "wakeup", "notices", map[string]any{}); len(get(page, "result", "notices").([]any)) != 0 {
		t.Fatalf("notices are the owner's own: %+v", page)
	}
	// updates.get shows notices of the last day only.
	r.now += services.WakeupNoticeWindow + 1
	if got = r.notices("alice", "anon:x", 0); len(got) != 0 {
		t.Fatalf("notices older than a day: %+v", got)
	}
}

func TestWakeupOnEvents(t *testing.T) {
	r := newWakeRig(t)
	r.work()
	b := r.board
	old := b.post(services.BoardEvent{Room: "lobby", Author: "bob", ReplyToAuthor: "alice"})
	r.mustCall("alice", "wakeup", "schedule", map[string]any{"key": "r", "on": "reply"}, 1)
	r.mustCall("alice", "wakeup", "schedule", map[string]any{"key": "m", "on": "mention"}, 1)
	r.mustCall("alice", "wakeup", "schedule", map[string]any{"key": "room", "on": "room", "room": "ops"}, 1)
	r.mustCall("carol", "wakeup", "schedule", map[string]any{"key": "m", "on": "mention"}, 1)
	if n := r.work(); n != 0 || old == 0 {
		t.Fatalf("a message before the registration never fires it: %d", n)
	}
	// The agent's own messages never wake it.
	b.post(services.BoardEvent{Room: "ops", Author: "alice", Mentions: []string{"alice"}})
	if n := r.work(); n != 0 {
		t.Fatalf("own message: %d", n)
	}
	reply := b.post(services.BoardEvent{ID: strings.Repeat("a", 32), Room: "lobby", Author: "bob", ReplyToAuthor: "alice"})
	if n := r.work(); n != 1 {
		t.Fatalf("a reply fires the reply wake-up: %d", n)
	}
	// An addressed message and an @handle mention each fire one mention
	// wake-up; a message in the watched room fires the room wake-up.
	b.post(services.BoardEvent{ID: strings.Repeat("b", 32), Room: "ops", Author: "", Addressed: "alice", Mentions: []string{"carol"}})
	if n := r.work(); n != 3 {
		t.Fatalf("mention (addressed), mention (@handle) and room: %d", n)
	}
	got := r.notices("alice", "anon:x", 0)
	if len(got) != 3 {
		t.Fatalf("three notices: %+v", got)
	}
	kinds := map[string]string{}
	for _, n := range got {
		kinds[get(n, "on").(string)] = fmt.Sprint(get(n, "event"))
		if get(n, "room") != nil {
			t.Fatalf("updates notices never name the room: %+v", n)
		}
	}
	if kinds["reply"] != strings.Repeat("a", 32) || kinds["mention"] != strings.Repeat("b", 32) || kinds["room"] != strings.Repeat("b", 32) {
		t.Fatalf("each notice names the message that fired it: %+v", kinds)
	}
	// A cursor past the firing hides it; one at or before it shows it.
	if got = r.notices("alice", "anon:x", reply+10); len(got) != 0 {
		t.Fatalf("a cursor after the firing: %+v", got)
	}
	if got = r.notices("alice", "anon:x", reply); len(got) != 3 {
		t.Fatalf("a cursor at the firing: %+v", got)
	}
	if n := r.work(); n != 0 {
		t.Fatalf("event wake-ups fire once: %d", n)
	}
}

func TestWakeupRespectsRoomAccess(t *testing.T) {
	r := newWakeRig(t)
	r.work()
	b := r.board
	b.private["den"] = map[string]bool{"alice": true, "bob": true}
	b.rooms["den"] = true
	r.mustCall("alice", "wakeup", "schedule", map[string]any{"key": "den", "on": "room", "room": "den"}, 1)
	r.mustCall("carol", "wakeup", "schedule", map[string]any{"key": "m", "on": "mention"}, 1)
	// A mention of carol in a room she cannot read does not wake her.
	b.post(services.BoardEvent{ID: strings.Repeat("c", 32), Room: "den", Author: "bob", Mentions: []string{"carol"}})
	if n := r.work(); n != 1 {
		t.Fatalf("only alice, a member, wakes: %d", n)
	}
	// The event is private: an outside reader of alice's updates sees the
	// notice without the message it names; alice sees it.
	if got := r.notices("alice", "anon:x", 0); len(got) != 1 || get(got[0], "event") != nil {
		t.Fatalf("outsider view: %+v", got)
	}
	if got := r.notices("alice", "alice", 0); len(got) != 1 || get(got[0], "event") != strings.Repeat("c", 32) {
		t.Fatalf("member view: %+v", got)
	}
	// Alice loses access: her next room wake-up there ends instead of firing.
	r.mustCall("alice", "wakeup", "schedule", map[string]any{"key": "den2", "on": "room", "room": "den"}, 1)
	delete(b.private["den"], "alice")
	b.post(services.BoardEvent{Room: "den", Author: "bob"})
	r.work()
	list, _ := r.read(subjectOf("alice"), "wakeup", "list", map[string]any{})
	if active := get(list, "result", "active").([]any); len(active) != 0 {
		t.Fatalf("the unreadable room wake-up ended: %+v", list)
	}
	if got := r.notices("alice", "alice", 0); len(got) != 1 {
		t.Fatalf("no notice for a room the agent cannot read: %+v", got)
	}
}

func TestWakeupExpiresAndScanSurvivesRestart(t *testing.T) {
	r := newWakeRig(t)
	r.work()
	r.mustCall("alice", "wakeup", "schedule", map[string]any{"key": "short", "on": "reply", "until": wakeT0 + 60}, 1)
	r.mustCall("alice", "wakeup", "schedule", map[string]any{"key": "long", "on": "room", "room": "lobby"}, 1)
	r.now = wakeT0 + 60
	if n := r.work(); n != 1 {
		t.Fatalf("the short wake-up expires: %d", n)
	}
	// Down for an hour: messages arrive with no worker running. The new
	// engine resumes from the stored position and fires on them.
	b := r.board
	b.post(services.BoardEvent{Room: "lobby", Author: "bob", ReplyToAuthor: "alice"})
	r.now += 3600
	r.e = r.open()
	if n := r.work(); n != 1 {
		t.Fatalf("the room wake-up catches up after a restart: %d", n)
	}
	list, _ := r.read(subjectOf("alice"), "wakeup", "list", map[string]any{})
	recent := get(list, "result", "recent").([]any)
	states := map[string]string{}
	for _, w := range recent {
		states[get(w, "key").(string)] = get(w, "state").(string)
	}
	if states["short"] != "expired" || states["long"] != "fired" {
		t.Fatalf("states: %+v", states)
	}
}

func TestWakeupIdleAndFirstScan(t *testing.T) {
	r := newWakeRig(t)
	b := r.board
	for i := 0; i < 5; i++ {
		b.post(services.BoardEvent{Room: "lobby", Author: "bob"})
	}
	if n := r.work(); n != 0 {
		t.Fatalf("an idle clock does nothing: %d", n)
	}
	var scans int
	if err := r.db.QueryRow("SELECT count(*) FROM wakeup_scan").Scan(&scans); err != nil || scans != 0 {
		t.Fatalf("an idle clock writes nothing: %d %v", scans, err)
	}
	// Registered before any pass ran: the messages after it still fire it,
	// and the ones before it never do.
	r.mustCall("alice", "wakeup", "schedule", map[string]any{"key": "lobby", "on": "room", "room": "lobby"}, 1)
	b.post(services.BoardEvent{ID: strings.Repeat("d", 32), Room: "lobby", Author: "bob"})
	if n := r.work(); n != 1 {
		t.Fatalf("the first scan: %d", n)
	}
	if got := r.notices("alice", "alice", 0); len(got) != 1 || get(got[0], "event") != strings.Repeat("d", 32) {
		t.Fatalf("fired by the message after registration: %+v", got)
	}
}

func TestWakeupFiringBoundedPerMinute(t *testing.T) {
	r := newWakeRig(t)
	total := services.WakeupFiresPerMinute + 150
	for i := 0; i < total; i++ {
		r.mustCall(fmt.Sprintf("acct%03d", i/10), "wakeup", "schedule", map[string]any{"key": fmt.Sprintf("k%d", i%10), "at": wakeT0 + 30}, 1)
	}
	r.now = wakeT0 + 60 // the start of a minute
	fired := 0
	for pass := 0; pass < 20; pass++ {
		n := r.work()
		if n > 100 {
			t.Fatalf("one pass fires at most 100: %d", n)
		}
		fired += n
	}
	if fired != services.WakeupFiresPerMinute {
		t.Fatalf("one minute fires at most %d, fired %d", services.WakeupFiresPerMinute, fired)
	}
	r.now += 60
	for pass := 0; pass < 5; pass++ {
		fired += r.work()
	}
	if fired != total {
		t.Fatalf("the rest fire the next minute: %d of %d", fired, total)
	}
}

func FuzzWakeupArgs(f *testing.F) {
	for _, s := range []string{
		`{"key":"a","at":1790003600}`, `{"key":"a","on":"reply"}`, `{"key":"a","on":"room","room":"lobby","until":1790003600}`,
		`{"key":"a","on":"mention","until":1}`, `{"key":"a","at":1790003600,"at":1}`, `{"key":"` + strings.Repeat("k", 65) + `","at":5}`,
		`{"key":"a","on":"room","room":"@` + strings.Repeat("0", 64) + `"}`, `{"id":"` + strings.Repeat("0", 32) + `"}`, `{}`, `[]`,
		`{"key":"a","every":3600}`, `{"key":"a","every":900,"at":1790000600,"until":1790090000,"count":7}`, `{"key":"a","every":899}`,
		`{"key":"a","every":604800,"count":0}`, `{"key":"a","every":3600,"on":"reply"}`, `{"key":"a","count":3,"at":1790003600}`,
	} {
		f.Add(s)
	}
	keyRE := regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	f.Fuzz(func(t *testing.T, raw string) {
		now := int64(wakeT0)
		s, err := services.ParseWakeupForTest(json.RawMessage(raw), now)
		if err == nil {
			if !utf8.ValidString(raw) || !json.Valid([]byte(raw)) || !keyRE.MatchString(s.Key) {
				t.Fatalf("accepted %q: %+v", raw, s)
			}
			switch s.Kind {
			case "time":
				if s.DueAt <= now || s.DueAt > now+services.WakeupHorizon || s.Room != "" {
					t.Fatalf("time out of bounds %q: %+v", raw, s)
				}
				if s.Every == 0 && (s.Until != 0 || s.Count != 0) {
					t.Fatalf("a one-shot time wake-up has no until or count %q: %+v", raw, s)
				}
				// A recurring one: a period in bounds, at least one firing, all
				// of them at or before until, until within the horizon.
				if s.Every != 0 && (s.Every < services.WakeupEveryMin || s.Every > services.WakeupEveryMax || s.Count < 1 ||
					s.Count > services.WakeupFiresMax || s.Until > now+services.WakeupHorizon || s.DueAt+(s.Count-1)*s.Every > s.Until) {
					t.Fatalf("recurring out of bounds %q: %+v", raw, s)
				}
			case "reply", "mention", "room":
				if s.Until <= now || s.Until > now+services.WakeupHorizon || s.DueAt != 0 || (s.Kind == "room") != (s.Room != "") {
					t.Fatalf("event out of bounds %q: %+v", raw, s)
				}
			default:
				t.Fatalf("unknown kind %q: %+v", raw, s)
			}
		} else if code(err) != "invalid_service_data" {
			t.Fatalf("refusal must be invalid_service_data: %v", err)
		}
		key, id, err := services.ParseWakeupRefForTest(json.RawMessage(raw))
		if err == nil && (key == "") == (id == "") {
			t.Fatalf("cancel accepted %q: %q %q", raw, key, id)
		}
		if err != nil && code(err) != "invalid_service_data" {
			t.Fatalf("cancel refusal: %v", err)
		}
	})
}
