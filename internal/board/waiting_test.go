package board

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func waitData(seconds int) string { return fmt.Sprintf(`{"schema":1,"wait":%d}`, seconds) }

// untilHeld waits (briefly, on the real clock) for n waiting reads to hold a slot.
func untilHeld(t *testing.T, s *Store, n int) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); s.updateWaiters.Held() != n; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("waiting reads holding slots: %d, want %d", s.updateWaiters.Held(), n)
		}
	}
}

type waitAnswer struct {
	res Result
	err error
}

func waitIn(ctx context.Context, s *Store, c Command, source string) <-chan waitAnswer {
	done := make(chan waitAnswer, 1)
	go func() {
		res, err := s.Execute(ctx, c, source)
		done <- waitAnswer{res, err}
	}()
	return done
}

// A waiting read answers as soon as a write lands, not when its wait runs out.
func TestWaitingUpdatesAnswerWhenAPostLands(t *testing.T) {
	s := openTest(t, updatesConfig())
	run(t, s, Command{Operation: "post", Room: "lobby", Text: "Before the wait."})
	saved := run(t, s, Command{Operation: "updates.get"}).NextCursor
	start := time.Now()
	done := waitIn(testContext, s, Command{Operation: "updates.get", Cursor: saved, Data: waitData(UpdatesWaitMax)}, "198.51.100.1")
	untilHeld(t, s, 1)
	// A write that concerns nobody wakes the waiter, which reads again and waits on.
	run(t, s, signed(keyFor(9), Command{Operation: "agent.register", Handle: "bystander"}))
	id := run(t, s, Command{Operation: "post", Room: "lobby", Text: "Wake up."}).Receipt.ID
	select {
	case got := <-done:
		if got.err != nil || len(got.res.Messages) != 1 || got.res.Messages[0].ID != id || got.res.NextCursor == saved {
			t.Fatalf("the waiting read must return the new post: %+v %v", got.res, got.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the waiting read did not wake on a post")
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("the waiting read ran to its timeout")
	}
	untilHeld(t, s, 0)
}

// A wait that runs out answers as a caught-up read: no messages, the same cursor.
func TestWaitingUpdatesTimeOutEmpty(t *testing.T) {
	s := openTest(t, updatesConfig())
	run(t, s, Command{Operation: "post", Room: "lobby", Text: "Old news."})
	saved := run(t, s, Command{Operation: "updates.get"}).NextCursor
	start := time.Now()
	res := run(t, s, Command{Operation: "updates.get", Cursor: saved, Data: `{"schema":1,"wait":1,"counts":true}`})
	if len(res.Messages) != 0 || res.NextCursor != saved || res.Data["counts_only"] != true {
		t.Fatalf("a timed-out wait must be an empty caught-up read: %+v", res)
	}
	if res.Next != nil && res.Next.WaitNeedsCursor != "" {
		t.Fatalf("a wait held with a cursor must not carry wait_needs_cursor: %+v", res.Next)
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Fatalf("the read returned after %v, before its wait", elapsed)
	}
	if s.updateWaiters.Held() != 0 {
		t.Fatal("a finished wait kept its slot")
	}
	// A shorter command budget (the text wires' 10 seconds) ends the wait early
	// with the same caught-up answer, not a timeout error.
	short, cancel := context.WithTimeout(testContext, 1500*time.Millisecond)
	defer cancel()
	start = time.Now()
	res, err := s.Execute(short, Command{Operation: "updates.get", Cursor: saved, Data: waitData(UpdatesWaitMax)}, "test-origin")
	if err != nil || len(res.Messages) != 0 || res.NextCursor != saved || time.Since(start) > time.Second {
		t.Fatalf("a wait under a short budget must end early and empty: %v %v", res, err)
	}
	// Without a cursor there is nothing to wait for: the first answer stands
	// at once, with a notice (next.wait_needs_cursor, C317) telling a client
	// that sends wait unconditionally why it was not held.
	start = time.Now()
	if res = run(t, s, Command{Operation: "updates.get", Data: waitData(5)}); res.NextCursor == "" || time.Since(start) > 2*time.Second {
		t.Fatal("a read without a cursor must answer at once")
	}
	if res.Next == nil || res.Next.WaitNeedsCursor == "" {
		t.Fatalf("a wait with no cursor must carry next.wait_needs_cursor: %+v", res.Next)
	}
	// A plain read (no wait) never carries the notice: only wait triggers it.
	if res = run(t, s, Command{Operation: "updates.get"}); res.Next != nil && res.Next.WaitNeedsCursor != "" {
		t.Fatalf("a read without wait must not carry wait_needs_cursor: %+v", res.Next)
	}
	for _, data := range []string{waitData(UpdatesWaitMax + 1), waitData(-1), `{"schema":1,"wait":"5"}`} {
		if _, err := s.Execute(testContext, Command{Operation: "updates.get", Cursor: saved, Data: data}, "test-origin"); !isCode(err, "invalid_request") {
			t.Fatalf("%s: want invalid_request, got %v", data, err)
		}
	}
}

// Waiting reads are capped per address and per key; a cancelled one frees its slot.
func TestWaitingUpdatesCapsAndCancellation(t *testing.T) {
	s := openTest(t, updatesConfig())
	key := keyFor(7)
	run(t, s, signed(key, Command{Operation: "agent.register", Handle: "waiter"}))
	run(t, s, signed(key, Command{Operation: "post", Room: "lobby", Text: "Seed."}))
	saved := run(t, s, Command{Operation: "updates.get"}).NextCursor
	read := Command{Operation: "updates.get", Cursor: saved, Data: waitData(UpdatesWaitMax)}
	ctx, cancel := context.WithCancel(testContext)
	defer cancel()
	first := waitIn(ctx, s, read, "198.51.100.2")
	second := waitIn(ctx, s, read, "198.51.100.2")
	untilHeld(t, s, 2)
	_, err := s.Execute(testContext, read, "198.51.100.2")
	var e *Error
	if !errors.As(err, &e) || e.Status != 429 || e.Code != "request_rate" || e.RetryAfter == 0 {
		t.Fatalf("a third waiting read from one address must be 429 request_rate, got %v", err)
	}
	// An IPv6 /64 is one address.
	v6 := waitIn(ctx, s, read, "2001:db8::1")
	untilHeld(t, s, 3)
	v6b := waitIn(ctx, s, read, "2001:db8::2")
	untilHeld(t, s, 4)
	if _, err = s.Execute(testContext, read, "2001:db8::3"); !isCode(err, "request_rate") {
		t.Fatalf("a third waiting read from one /64 must be refused, got %v", err)
	}
	// One key from two addresses: the key cap applies.
	signedRead := func() Command {
		c := read
		c.Target = keyID(key)
		return signed(key, c)
	}
	k1 := waitIn(ctx, s, signedRead(), "198.51.100.3")
	k2 := waitIn(ctx, s, signedRead(), "198.51.100.4")
	untilHeld(t, s, 6)
	if _, err = s.Execute(testContext, signedRead(), "198.51.100.5"); !isCode(err, "request_rate") {
		t.Fatalf("a third waiting read by one key must be refused, got %v", err)
	}
	// A read with something new never waits, so it is never refused.
	run(t, s, Command{Operation: "post", Room: "lobby", Text: "News for everyone."})
	for _, done := range []<-chan waitAnswer{first, second, v6, v6b, k1, k2} {
		select {
		case got := <-done:
			if got.err != nil || len(got.res.Messages) == 0 {
				t.Fatalf("a waiter missed the post: %v", got.err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a waiter did not wake")
		}
	}
	untilHeld(t, s, 0)
	// Cancelling the request (a deploy's SIGTERM, or the caller leaving) ends
	// the wait, releases the slot and answers as a timeout: no messages, the
	// same cursor (C561).
	latest := run(t, s, Command{Operation: "updates.get", Cursor: saved, Limit: 200}).NextCursor
	read.Cursor = latest
	waiting := waitIn(ctx, s, read, "198.51.100.6")
	untilHeld(t, s, 1)
	cancel()
	select {
	case got := <-waiting:
		if got.err != nil || len(got.res.Messages) != 0 || got.res.NextCursor != latest {
			t.Fatalf("a cancelled wait must answer empty with the same cursor, got %+v %v", got.res.Messages, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled wait did not return")
	}
	untilHeld(t, s, 0)
}

// Past the server-wide total every address is refused.
func TestWaitSlotsServerCap(t *testing.T) {
	w := NewWaitSlots(2, 2)
	a, full := w.Acquire("198.51.100.1", "")
	b, full2 := w.Acquire("198.51.100.2", "")
	if full != "" || full2 != "" {
		t.Fatal("slots under the caps must be granted")
	}
	if _, full = w.Acquire("198.51.100.3", ""); full != "server" {
		t.Fatalf("want the server cap, got %q", full)
	}
	a()
	a() // a second release is harmless
	b()
	if w.Held() != 0 || len(w.held) != 0 {
		t.Fatal("released slots must leave nothing behind")
	}
}

// thread.get with a wait (C880): held until a newer visible post lands in
// the thread, empty at the timeout, never ended by a hidden reply or by
// activity elsewhere, and capped and refused as updates.get's waits are.
func TestWaitingThreadRead(t *testing.T) {
	s := openTest(t, updatesConfig())
	root := run(t, s, Command{Operation: "post", Room: "lobby", Text: "A question."}).Receipt.ID
	saved := run(t, s, Command{Operation: "thread.get", MessageID: root}).NextCursor
	// wait=0 is today's read: at once, nothing new.
	start := time.Now()
	if res := run(t, s, Command{Operation: "thread.get", MessageID: root, Cursor: saved, Data: waitData(0)}); len(res.Messages) != 0 || time.Since(start) > 2*time.Second {
		t.Fatalf("wait 0 must answer at once: %+v", res.Messages)
	}
	// A timed-out wait is an empty answer, as a caught-up read is.
	start = time.Now()
	res := run(t, s, Command{Operation: "thread.get", MessageID: root, Cursor: saved, Data: waitData(1)})
	if len(res.Messages) != 0 || res.NextCursor == "" || time.Since(start) < time.Second {
		t.Fatalf("a timed-out thread wait must be empty, after its wait: %+v", res)
	}
	if res.Next != nil && res.Next.WaitNeedsCursor != "" {
		t.Fatalf("a wait held with a cursor must not carry wait_needs_cursor: %+v", res.Next)
	}
	if s.updateWaiters.Held() != 0 {
		t.Fatal("a finished wait kept its slot")
	}
	// A hidden reply (a payload-free tombstone in the read) and activity
	// elsewhere do not end the wait; a visible reply does.
	hidden := run(t, s, signed(keyFor(5), Command{Operation: "post", Room: "lobby", Text: "Spam reply.", ReplyTo: root})).Receipt.ID
	if err := s.Moderate(testContext, hidden, "test removal", true); err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	done := waitIn(testContext, s, Command{Operation: "thread.get", MessageID: root, Cursor: saved, Data: waitData(UpdatesWaitMax)}, "198.51.100.9")
	untilHeld(t, s, 1)
	run(t, s, Command{Operation: "post", Room: "lobby", Text: "Another topic."})
	select {
	case got := <-done:
		t.Fatalf("a hidden reply or activity elsewhere ended the wait: %+v %v", got.res.Messages, got.err)
	case <-time.After(300 * time.Millisecond):
	}
	answer := run(t, s, Command{Operation: "post", Room: "lobby", Text: "The answer.", ReplyTo: root}).Receipt.ID
	select {
	case got := <-done:
		if got.err != nil || len(got.res.Messages) == 0 || got.res.Messages[len(got.res.Messages)-1].ID != answer {
			t.Fatalf("the waiting thread read must return the new reply: %+v %v", got.res.Messages, got.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the waiting thread read did not wake on a reply")
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("the waiting thread read ran to its timeout")
	}
	untilHeld(t, s, 0)
	// Without a cursor the first answer stands at once, with the notice.
	start = time.Now()
	if res = run(t, s, Command{Operation: "thread.get", MessageID: root, Data: waitData(5)}); len(res.Messages) == 0 || time.Since(start) > 2*time.Second || res.Next == nil || res.Next.WaitNeedsCursor == "" {
		t.Fatalf("a thread wait with no cursor must answer at once with wait_needs_cursor: %+v", res.Next)
	}
	latest := res.NextCursor
	// The per-address cap is the one updates.get's waits use, shared with them.
	read := Command{Operation: "thread.get", MessageID: root, Cursor: latest, Data: waitData(UpdatesWaitMax)}
	updates := Command{Operation: "updates.get", Cursor: run(t, s, Command{Operation: "updates.get", Limit: 200}).NextCursor, Data: waitData(UpdatesWaitMax)}
	ctx, cancel := context.WithCancel(testContext)
	defer cancel()
	a := waitIn(ctx, s, read, "198.51.100.10")
	b := waitIn(ctx, s, updates, "198.51.100.10")
	untilHeld(t, s, 2)
	if _, err := s.Execute(testContext, read, "198.51.100.10"); !isCode(err, "request_rate") {
		t.Fatalf("a third waiting read from one address must be refused, got %v", err)
	}
	cancel()
	for _, ch := range []<-chan waitAnswer{a, b} {
		if got := <-ch; got.err != nil || len(got.res.Messages) != 0 {
			t.Fatalf("a cancelled wait must answer empty: %+v %v", got.res.Messages, got.err)
		}
	}
	untilHeld(t, s, 0)
	for _, data := range []string{waitData(UpdatesWaitMax + 1), waitData(-1), `{"schema":1,"wait":"5"}`, `{"schema":1,"counts":true}`} {
		if _, err := s.Execute(testContext, Command{Operation: "thread.get", MessageID: root, Cursor: latest, Data: data}, "test-origin"); !isCode(err, "invalid_request") {
			t.Fatalf("%s: want invalid_request, got %v", data, err)
		}
	}
}
