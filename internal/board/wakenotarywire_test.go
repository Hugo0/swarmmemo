package board

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"swarmmemo/internal/services"
	"swarmmemo/internal/services/servicestest"
)

// openWakeTest opens a store with wakeup and notary enabled, spending
// through the fake ledger.
func openWakeTest(t *testing.T, enabled ...string) *Store {
	t.Helper()
	c := updatesConfig()
	c.Features = Features{Services: enabled}
	s := openTest(t, c)
	s.UseServiceMeter(servicestest.NewMeter(1<<30), &servicestest.Params{})
	t.Cleanup(s.stopServices)
	return s
}

func wakeWork(t *testing.T, s *Store) int {
	t.Helper()
	n, err := s.WorkServices(testContext)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func wakeNotices(t *testing.T, r Result) []any {
	t.Helper()
	list, ok := svcField(t, r.Data, "wakeups").([]any)
	if !ok {
		t.Fatalf("updates.get has no data.wakeups: %+v", r.Data)
	}
	return list
}

func TestWakeupNotaryOffMeansToday(t *testing.T) {
	agent := keyFor(1)
	for _, enabled := range [][]string{nil, {"echo", "memory"}} {
		c := updatesConfig()
		c.Features = Features{Services: enabled}
		s := openTest(t, c)
		run(t, s, signed(agent, Command{Operation: "post", Text: "hello"}))
		got := run(t, s, Command{Operation: "updates.get", Target: keyID(agent)})
		if _, ok := got.Data["wakeups"]; ok {
			t.Fatalf("updates.get must not change while wakeup is off (%v): %+v", enabled, got.Data)
		}
		for _, service := range []string{"wakeup", "notary"} {
			code := "service_unavailable"
			if enabled != nil {
				code = "invalid_service"
			}
			fails(t, s, svcCall(agent, service, "schedule", map[string]any{"key": "k", "at": testTime + 60}, 5, ""), code)
			fails(t, s, svcRead(nil, service, "key", map[string]any{}), code)
		}
		if n, err := s.WorkServices(testContext); err != nil || n != 0 {
			t.Fatalf("no wake-up work while off: %d %v", n, err)
		}
		s.stopServices()
	}
	if !reflect.DeepEqual(KnownServices, services.Known()) || !strings.Contains(strings.Join(KnownServices, ","), "wakeup") || !strings.Contains(strings.Join(KnownServices, ","), "notary") {
		t.Fatalf("SERVICES may name the new providers: %v", KnownServices)
	}
}

func TestWakeupEndToEnd(t *testing.T) {
	s := openWakeTest(t, "wakeup")
	alice, bob := keyFor(1), keyFor(2)
	run(t, s, signed(alice, Command{Operation: "agent.register", Handle: "alice"}))
	run(t, s, signed(bob, Command{Operation: "agent.register", Handle: "bob"}))
	me := keyID(alice)
	root := run(t, s, signed(alice, Command{Operation: "post", Room: "workshop", Text: "An open question."})).Receipt.ID
	run(t, s, signed(bob, Command{Operation: "post", Room: "ops", Text: "Ops room opens."}))
	wakeWork(t, s)

	schedule := func(key string, args map[string]any) Result {
		args["key"] = key
		return run(t, s, svcCall(alice, "wakeup", "schedule", args, 1, "w-"+key))
	}
	schedule("reply", map[string]any{"on": "reply"})
	schedule("mention", map[string]any{"on": "mention"})
	schedule("ops", map[string]any{"on": "room", "room": "ops"})
	timed := schedule("timed", map[string]any{"at": testTime + 120})
	if svcField(t, timed.Data, "result", "wakeup", "at") != float64(testTime+120) || svcField(t, timed.Data, "call", "cost") != float64(1) {
		t.Fatalf("schedule: %+v", timed.Data)
	}
	// An exact retry returns the stored receipt; the same key with the same
	// wake-up under a new request ID is the same registration.
	again := run(t, s, svcCall(alice, "wakeup", "schedule", map[string]any{"key": "timed", "at": testTime + 120}, 1, "w-timed-2"))
	if svcField(t, again.Data, "result", "duplicate") != true {
		t.Fatalf("idempotent registration: %+v", again.Data)
	}
	fails(t, s, svcCall(alice, "wakeup", "schedule", map[string]any{"key": "timed", "at": testTime + 121}, 1, ""), "wakeup_conflict")
	fails(t, s, svcCall(alice, "wakeup", "schedule", map[string]any{"key": "x", "on": "room", "room": "nowhere"}, 1, ""), "not_found")
	fails(t, s, svcCall(alice, "wakeup", "cancel", map[string]any{"key": "never"}, 1, ""), "wakeup_not_found")

	saved := run(t, s, Command{Operation: "updates.get", Target: me})
	if list := wakeNotices(t, saved); len(list) != 0 {
		t.Fatalf("nothing has fired: %+v", list)
	}

	reply := run(t, s, signed(bob, Command{Operation: "post", Room: "workshop", Text: "An answer.", ReplyTo: root})).Receipt.ID
	mention := run(t, s, signed(bob, Command{Operation: "post", Room: "lobby", Text: "cc @Alice, thoughts?"})).Receipt.ID
	anonymous := run(t, s, Command{Operation: "post", Room: "ops", Text: "deploy at noon"}).Receipt.ID
	if n := wakeWork(t, s); n != 3 {
		t.Fatalf("reply, @mention and room fire: %d", n)
	}
	// They arrive in the read the agent already makes, unsigned, with the
	// cursor it saved, over the ordinary replies.
	back := run(t, s, Command{Operation: "updates.get", Target: me, Cursor: saved.NextCursor})
	fired := map[string]string{}
	for _, n := range wakeNotices(t, back) {
		fired[svcField(t, n, "on").(string)] = svcField(t, n, "event").(string)
	}
	if fired["reply"] != reply || fired["mention"] != mention || fired["room"] != anonymous || len(fired) != 3 {
		t.Fatalf("notices: %+v", fired)
	}
	if !slicesContain(ids(back, "replies"), reply) {
		t.Fatalf("the ordinary updates are unchanged: %+v", back.Data)
	}
	// Notices repeat (the same ids) until the saved cursor passes the newest
	// message at the firing; then they stop.
	bob2 := run(t, s, signed(bob, Command{Operation: "post", Room: "workshop", Text: "Another answer.", ReplyTo: root})).Receipt.ID
	later := run(t, s, Command{Operation: "updates.get", Target: me, Cursor: back.NextCursor})
	if list := wakeNotices(t, later); len(list) != 3 || !slicesContain(ids(later, "replies"), bob2) {
		t.Fatalf("a cursor before the firing: %+v", later.Data)
	}
	past := run(t, s, Command{Operation: "updates.get", Target: me, Cursor: later.NextCursor})
	if list := wakeNotices(t, past); len(list) != 0 {
		t.Fatalf("a cursor past the firing: %+v", past.Data)
	}

	// The time wake-up fires when due, and the owner's notices page lists all.
	svcSetNow(s, testTime+120)
	if n := wakeWork(t, s); n != 1 {
		t.Fatalf("time: %d", n)
	}
	page := run(t, s, svcRead(alice, "wakeup", "notices", map[string]any{}))
	if notices := svcField(t, page.Data, "result", "notices").([]any); len(notices) != 4 || svcField(t, notices[3], "on") != "time" {
		t.Fatalf("notices page: %+v", page.Data)
	}
	fails(t, s, svcRead(nil, "wakeup", "notices", map[string]any{}), "signature_required")
	list := run(t, s, svcRead(alice, "wakeup", "list", map[string]any{}))
	if active := svcField(t, list.Data, "result", "active").([]any); len(active) != 0 {
		t.Fatalf("every wake-up fired once: %+v", list.Data)
	}
	// Cancel stays idempotent after firing.
	c := run(t, s, svcCall(alice, "wakeup", "cancel", map[string]any{"key": "timed"}, 1, ""))
	if svcField(t, c.Data, "result", "cancelled") != false || svcField(t, c.Data, "result", "wakeup", "state") != "fired" {
		t.Fatalf("cancel after firing: %+v", c.Data)
	}
}

func slicesContain(list []string, id string) bool {
	for _, v := range list {
		if v == id {
			return true
		}
	}
	return false
}

func TestWakeupBoardViewAndPrivateRooms(t *testing.T) {
	s := openWakeTest(t, "wakeup")
	owner, member, outsider := keyFor(1), keyFor(2), keyFor(3)
	for i, k := range []Command{{Handle: "owner"}, {Handle: "member"}, {Handle: "outsider"}} {
		k.Operation = "agent.register"
		run(t, s, signed([]ed25519.PrivateKey{owner, member, outsider}[i], k))
	}
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "den", Visibility: "private"}))
	run(t, s, signed(owner, Command{Operation: "room.member.add", Room: "den", Target: keyID(member)}))
	wakeWork(t, s)
	// Registering a room wake-up needs read access to the room.
	fails(t, s, svcCall(outsider, "wakeup", "schedule", map[string]any{"key": "den", "on": "room", "room": "den"}, 1, ""), "not_found")
	run(t, s, svcCall(member, "wakeup", "schedule", map[string]any{"key": "den", "on": "room", "room": "den"}, 1, ""))
	run(t, s, svcCall(outsider, "wakeup", "schedule", map[string]any{"key": "m", "on": "mention"}, 1, ""))
	secret := run(t, s, signed(owner, Command{Operation: "post", Room: "den", Text: "@outsider will never see this"})).Receipt.ID
	if n := wakeWork(t, s); n != 1 {
		t.Fatalf("the member wakes; the outsider named in a room it cannot read does not: %d", n)
	}
	// The member's own signed read names the message; an anonymous reader of
	// the member's updates sees the notice without it.
	mine := run(t, s, signed(member, Command{Operation: "updates.get", Target: keyID(member)}))
	if list := wakeNotices(t, mine); len(list) != 1 || svcField(t, list[0], "event") != secret {
		t.Fatalf("member view: %+v", list)
	}
	theirs := run(t, s, Command{Operation: "updates.get", Target: keyID(member)})
	if list := wakeNotices(t, theirs); len(list) != 1 || svcField(t, list[0], "event") != nil {
		t.Fatalf("outside view: %+v", list)
	}

	// The board view: edits and hidden messages are not new messages.
	view := serviceBoardView{}
	latest, err := view.LatestSeq(testContext, s.db)
	if err != nil {
		t.Fatal(err)
	}
	orig := run(t, s, signed(owner, Command{Operation: "post", Room: "lobby", Text: "v1 @member @member @nobody x@owner", To: keyID(outsider)}))
	run(t, s, signed(owner, Command{Operation: "post", Room: "lobby", Text: "v2", Data: `{"schema":1,"supersedes":"` + orig.Receipt.ID + `"}`}))
	hidden := run(t, s, Command{Operation: "post", Room: "lobby", Text: "hide me"}).Receipt.ID
	if err = s.Moderate(testContext, hidden, "test", true); err != nil {
		t.Fatal(err)
	}
	events, err := view.EventsAfter(testContext, s.db, latest, 10)
	if err != nil || len(events) != 1 || events[0].ID != orig.Receipt.ID {
		t.Fatalf("only the original: %+v %v", events, err)
	}
	ev := events[0]
	if ev.Author != keyID(owner) || ev.Addressed != keyID(outsider) || !reflect.DeepEqual(ev.Mentions, []string{keyID(member)}) || ev.Room != "lobby" {
		t.Fatalf("event view: %+v", ev)
	}
	anon := run(t, s, Command{Operation: "post", Room: "lobby", Text: "anonymous"}).Receipt.ID
	reply := run(t, s, signed(member, Command{Operation: "post", Room: "lobby", Text: "re", ReplyTo: anon})).Receipt.ID
	events, _ = view.EventsAfter(testContext, s.db, latest, 10)
	for _, e := range events {
		if (e.ID == anon && e.Author != "") || (e.ID == reply && e.ReplyToAuthor != "") {
			t.Fatalf("an anonymous message has no author account: %+v", e)
		}
	}
}

func TestMentionPattern(t *testing.T) {
	for text, want := range map[string][]string{
		"@alice hi":                   {"alice"},
		"hi @alice.":                  {"alice"},
		"(@Bob_1)":                    {"Bob_1"},
		"mail x@alice.com":            nil,
		"@@alice":                     nil,
		"@alice-and-more":             {"alice-and-more"},
		"@" + strings.Repeat("a", 33): nil,
		"@a @b":                       {"a", "b"},
	} {
		if got := mentionHandles(text); !reflect.DeepEqual(got, want) {
			t.Errorf("%q: %v, want %v", text, got, want)
		}
	}
}

func TestNotaryEndToEnd(t *testing.T) {
	s := openWakeTest(t, "notary")
	agent := keyFor(1)
	key := run(t, s, svcRead(nil, "notary", "key", map[string]any{}))
	public := svcField(t, key.Data, "result", "public_key").(string)
	// The key is the notary key file beside the database (mode 0600), never a
	// database row.
	fileKey, err := services.LoadOrCreateNotaryKey(s.config.NotaryKeyFile)
	if err != nil || filepath.Base(s.config.NotaryKeyFile) != services.NotaryKeyFileName ||
		base64.RawURLEncoding.EncodeToString(fileKey.Public().(ed25519.PublicKey)) != public {
		t.Fatalf("notary key file %q: %v", s.config.NotaryKeyFile, err)
	}
	var tables int
	if err = s.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='notary_keys'").Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("the notary key must not be kept in the database: %d %v", tables, err)
	}
	stamp := run(t, s, svcCall(agent, "notary", "stamp", map[string]any{"text": "the plan, v3"}, 1, "n1"))
	raw, _ := json.Marshal(svcField(t, stamp.Data, "result", "receipt"))
	var r services.NotaryReceipt
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("the plan, v3"))
	if r.Hash != hex.EncodeToString(sum[:]) || r.ServiceID != "swarmmemo.com" || r.Time != testTime || !services.VerifyNotaryReceipt(public, r) {
		t.Fatalf("receipt: %+v", r)
	}
	back := run(t, s, svcRead(nil, "notary", "get", map[string]any{"hash": r.Hash}))
	if svcField(t, back.Data, "result", "receipt", "signature") != r.Signature {
		t.Fatalf("read back: %+v", back.Data)
	}
	fails(t, s, svcRead(nil, "notary", "get", map[string]any{"hash": strings.Repeat("0", 64)}), "notary_not_found")
	fails(t, s, svcCall(agent, "notary", "stamp", map[string]any{"hash": "nothex"}, 1, ""), "invalid_service_data")
	var e *Error
	if err := providerError("notary_limit"); err == nil || !errors.As(err, &e) || e.Status != 429 {
		t.Fatalf("notary_limit: %v", err)
	}
}
