package board

import (
	"testing"
	"time"
)

// C75: a wake-up notice is returned once per cursor. On a quiet board (no
// message after the firing) the next read from the returned cursor must not
// list it again, while a read from the older cursor still replays it.
func TestWakeupNoticeOncePerCursorOnAQuietBoard(t *testing.T) {
	s := openWakeTest(t, "wakeup")
	alice := keyFor(1)
	run(t, s, signed(alice, Command{Operation: "agent.register", Handle: "alice"}))
	me := keyID(alice)
	run(t, s, signed(alice, Command{Operation: "post", Text: "hello"}))
	run(t, s, svcCall(alice, "wakeup", "schedule", map[string]any{"key": "t1", "at": testTime + 60}, 1, "c75-t1"))
	run(t, s, svcCall(alice, "wakeup", "schedule", map[string]any{"key": "t2", "at": testTime + 120}, 1, "c75-t2"))
	saved := run(t, s, Command{Operation: "updates.get", Target: me})
	if list := wakeNotices(t, saved); len(list) != 0 {
		t.Fatalf("nothing has fired: %+v", list)
	}

	svcSetNow(s, testTime+60)
	if n := wakeWork(t, s); n != 1 {
		t.Fatalf("t1 fires: %d", n)
	}
	first := run(t, s, Command{Operation: "updates.get", Target: me, Cursor: saved.NextCursor})
	if list := wakeNotices(t, first); len(list) != 1 {
		t.Fatalf("the firing is listed once: %+v", first.Data)
	}
	if first.NextCursor == saved.NextCursor {
		t.Fatal("listing a notice must move the cursor")
	}
	// The same board, no new message: the notice is not news any more, and a
	// caught-up read hands back its own cursor.
	for i := 0; i < 3; i++ {
		again := run(t, s, Command{Operation: "updates.get", Target: me, Cursor: first.NextCursor})
		if list := wakeNotices(t, again); len(list) != 0 {
			t.Fatalf("read %d repeated the notice: %+v", i, again.Data)
		}
		if again.NextCursor != first.NextCursor {
			t.Fatalf("a caught-up read must keep its cursor")
		}
	}
	// The older cursor still replays it.
	if list := wakeNotices(t, run(t, s, Command{Operation: "updates.get", Target: me, Cursor: saved.NextCursor})); len(list) != 1 {
		t.Fatalf("an older cursor replays the notice: %+v", list)
	}

	// The next firing is listed from the newer cursor, alone.
	svcSetNow(s, testTime+120)
	if n := wakeWork(t, s); n != 1 {
		t.Fatalf("t2 fires: %d", n)
	}
	second := run(t, s, Command{Operation: "updates.get", Target: me, Cursor: first.NextCursor})
	if list := wakeNotices(t, second); len(list) != 1 || svcField(t, list[0], "id") == svcField(t, wakeNotices(t, first)[0], "id") {
		t.Fatalf("only the new firing: %+v", second.Data)
	}
	if list := wakeNotices(t, run(t, s, Command{Operation: "updates.get", Target: me, Cursor: second.NextCursor})); len(list) != 0 {
		t.Fatalf("no repeat after the second firing: %+v", list)
	}
	if list := wakeNotices(t, run(t, s, Command{Operation: "updates.get", Target: me, Cursor: saved.NextCursor})); len(list) != 2 {
		t.Fatalf("the oldest cursor replays both: %+v", list)
	}
	// A messages.list cursor (no notice part) resumes updates.get as before:
	// notices since its message sequence.
	plain := run(t, s, Command{Operation: "messages.list", Room: "lobby"}).NextCursor
	if list := wakeNotices(t, run(t, s, Command{Operation: "updates.get", Target: me, Cursor: plain})); len(list) != 2 {
		t.Fatalf("a plain cursor lists the notices since its sequence: %+v", list)
	}
}

// C75: a wait= read returns promptly when a wake-up fires for the agent,
// although the firing posts no message.
func TestWaitingUpdatesWakeOnAWakeupFiring(t *testing.T) {
	s := openWakeTest(t, "wakeup")
	alice := keyFor(1)
	run(t, s, signed(alice, Command{Operation: "agent.register", Handle: "alice"}))
	me := keyID(alice)
	run(t, s, signed(alice, Command{Operation: "post", Text: "hello"}))
	run(t, s, svcCall(alice, "wakeup", "schedule", map[string]any{"key": "t1", "at": testTime + 60}, 1, "c75-wait"))
	saved := run(t, s, Command{Operation: "updates.get", Target: me}).NextCursor
	// Due, but fired only by the worker pass below.
	svcSetNow(s, testTime+60)
	start := time.Now()
	done := waitIn(testContext, s, Command{Operation: "updates.get", Target: me, Cursor: saved, Data: waitData(UpdatesWaitMax)}, "198.51.100.75")
	untilHeld(t, s, 1)
	if n := wakeWork(t, s); n != 1 {
		t.Fatalf("t1 fires: %d", n)
	}
	select {
	case got := <-done:
		if got.err != nil || len(wakeNotices(t, got.res)) != 1 || got.res.NextCursor == saved {
			t.Fatalf("the waiting read must return the firing: %+v %v", got.res, got.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the waiting read did not wake on a wake-up firing")
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("the waiting read ran to its timeout")
	}
	untilHeld(t, s, 0)
}
