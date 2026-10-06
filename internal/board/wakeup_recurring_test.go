package board

import "testing"

// TestWakeupRecurringEndToEnd: a recurring wake-up through the board: one
// charge per firing when set, a notice per firing in updates.get, pending in
// journal.get with its next firing, and done after its count.
func TestWakeupRecurringEndToEnd(t *testing.T) {
	s := openWakeTest(t, "wakeup")
	alice := keyFor(1)
	me := keyID(alice)
	run(t, s, signed(alice, Command{Operation: "post", Text: "hello"}))
	wakeWork(t, s)

	args := map[string]any{"key": "quarter", "every": 900, "at": testTime + 120, "count": 2}
	fails(t, s, svcCall(alice, "wakeup", "schedule", args, 1, ""), "price_exceeds_max")
	set := run(t, s, svcCall(alice, "wakeup", "schedule", args, 2, "w-quarter"))
	if svcField(t, set.Data, "call", "cost") != float64(2) || svcField(t, set.Data, "result", "wakeup", "every") != float64(900) || svcField(t, set.Data, "result", "wakeup", "next_due") != float64(testTime+120) {
		t.Fatalf("schedule: %+v", set.Data)
	}
	saved := run(t, s, Command{Operation: "updates.get", Target: me})

	svcSetNow(s, testTime+120)
	if n := wakeWork(t, s); n != 1 {
		t.Fatalf("first firing: %d", n)
	}
	b, _, _ := journalOf(t, run(t, s, journalGet(alice, "", 0)))
	pending := path(t, b, "wakeups", "pending").([]any)
	if len(pending) != 1 || path(t, pending[0], "next_due") != float64(testTime+120+900) || path(t, pending[0], "fired_count") != float64(1) {
		t.Fatalf("journal.get pending: %v", pending)
	}

	svcSetNow(s, testTime+120+900)
	if n := wakeWork(t, s); n != 1 {
		t.Fatalf("second firing: %d", n)
	}
	back := run(t, s, Command{Operation: "updates.get", Target: me, Cursor: saved.NextCursor})
	list := wakeNotices(t, back)
	if len(list) != 2 || svcField(t, list[0], "id") != svcField(t, list[1], "id") || svcField(t, list[0], "on") != "time" ||
		svcField(t, list[0], "fired_at") != float64(testTime+120+900) || svcField(t, list[1], "fired_at") != float64(testTime+120) {
		t.Fatalf("a notice per firing in updates.get: %+v", list)
	}
	// The test keys sign at testTime; read the list within their window.
	svcSetNow(s, testTime+120)
	l := run(t, s, svcRead(alice, "wakeup", "list", map[string]any{}))
	recent := svcField(t, l.Data, "result", "recent").([]any)
	if active := svcField(t, l.Data, "result", "active").([]any); len(active) != 0 || len(recent) != 1 ||
		svcField(t, recent[0], "state") != "fired" || svcField(t, recent[0], "fired_count") != float64(2) {
		t.Fatalf("done after its count: %+v", l.Data)
	}
}
