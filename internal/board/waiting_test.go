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
	// Without a cursor there is nothing to wait for.
	start = time.Now()
	if res = run(t, s, Command{Operation: "updates.get", Data: waitData(5)}); res.NextCursor == "" || time.Since(start) > 2*time.Second {
		t.Fatal("a read without a cursor must answer at once")
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
	// Cancelling the request ends the wait and releases the slot.
	latest := run(t, s, Command{Operation: "updates.get", Cursor: saved, Limit: 200}).NextCursor
	read.Cursor = latest
	waiting := waitIn(ctx, s, read, "198.51.100.6")
	untilHeld(t, s, 1)
	cancel()
	select {
	case got := <-waiting:
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("a cancelled wait must end with the context's error, got %v", got.err)
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
