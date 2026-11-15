package web

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"swarmmemo/internal/board"
)

// gatedAgentService answers agent.get (or op, when set) only when its gate
// opens, counting the reads in flight; every other command answers at once.
type gatedAgentService struct {
	gate     chan struct{}
	op       string
	inflight atomic.Int64
	most     atomic.Int64
	reads    atomic.Int64
}

func (s *gatedAgentService) Execute(_ context.Context, c board.Command, _ string) (board.Result, error) {
	op := s.op
	if op == "" {
		op = "agent.get"
	}
	if c.Operation != op {
		return board.Result{OK: true}, nil
	}
	s.reads.Add(1)
	n := s.inflight.Add(1)
	defer s.inflight.Add(-1)
	for {
		m := s.most.Load()
		if n <= m || s.most.CompareAndSwap(m, n) {
			break
		}
	}
	<-s.gate
	if op == "thread.get" {
		m := board.Message{ID: c.MessageID, Room: board.DefaultRoom, Page: board.DefaultPage, Text: "hello", Visibility: "public", Type: "message"}
		return board.Result{OK: true, Messages: []board.Message{m}, Data: map[string]any{"root_id": c.MessageID}}, nil
	}
	return board.Result{OK: true, Agent: &board.Agent{ID: c.Target}}, nil
}
func (*gatedAgentService) Moderate(context.Context, string, string, bool) error { return nil }
func (*gatedAgentService) Close() error                                         { return nil }

// At most agentPageRenders agent pages render at once (C701): one past the
// cap waits up to renderWait, then is answered 503 busy with Retry-After
// and never reads the store; one that finds a turn within the wait renders.
// Other pages are never held by it.
func TestAgentPageRendersAreCapped(t *testing.T) {
	s := &gatedAgentService{gate: make(chan struct{})}
	h := Handler(s)
	agent := strings.Repeat("ab", 32)
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w
	}
	var wg sync.WaitGroup
	codes := make(chan int, agentPageRenders+1)
	for range agentPageRenders {
		wg.Add(1)
		go func() { defer wg.Done(); codes <- get("/agent/" + agent).Code }()
	}
	deadline := time.Now().Add(5 * time.Second)
	for s.inflight.Load() < agentPageRenders {
		if time.Now().After(deadline) {
			t.Fatalf("only %d renders started", s.inflight.Load())
		}
		time.Sleep(time.Millisecond)
	}
	for _, other := range []string{"/", "/r/lobby", "/@atlas/notary"} {
		if w := get(other); w.Code == 503 {
			t.Fatalf("%s was held by the agent page cap", other)
		}
	}
	start := time.Now()
	busy := get("/@" + agent[:12])
	if busy.Code != 503 || busy.Header().Get("Retry-After") != "5" || !strings.Contains(busy.Body.String(), "This page is busy.") || !strings.Contains(busy.Body.String(), "noindex") {
		t.Fatalf("past the cap: %d %v %q", busy.Code, busy.Header(), busy.Body.String())
	}
	if waited := time.Since(start); waited < renderWait {
		t.Fatalf("answered busy after %v, before waiting %v", waited, renderWait)
	}
	if s.reads.Load() != agentPageRenders {
		t.Fatalf("store reads %d, want %d", s.reads.Load(), agentPageRenders)
	}
	// A page that arrives while the cap is full waits, and renders once a
	// turn frees.
	wg.Add(1)
	go func() { defer wg.Done(); codes <- get("/agent/" + agent).Code }()
	time.Sleep(50 * time.Millisecond)
	close(s.gate)
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != 200 {
			t.Fatalf("a capped render answered %d", code)
		}
	}
	if s.most.Load() != agentPageRenders || s.reads.Load() != agentPageRenders+1 {
		t.Fatalf("most in flight %d, reads %d", s.most.Load(), s.reads.Load())
	}
}

func TestAgentPagePath(t *testing.T) {
	for p, want := range map[string]bool{
		"/@atlas": true, "/@0123456789ab": true, "/agent/" + strings.Repeat("a", 64): true,
		"/@atlas/notary": false, "/@": false, "/agent/": false, "/agents": false, "/r/@atlas": false, "/agent/x/badge.svg": false,
	} {
		if agentPagePath(p) != want {
			t.Errorf("agentPagePath(%q) = %v", p, !want)
		}
	}
}

// Room pages and /find have turns of their own (C929): past roomPageRenders
// (findPageRenders) a render waits up to renderWait, then is answered 503
// busy without reading the store, while the other kinds are still served.
func TestRoomAndFindRendersAreCapped(t *testing.T) {
	for _, tc := range []struct {
		op, path, other string
		turns           int
	}{
		{"room.get", "/r/lobby", "/find?q=weather", roomPageRenders},
		{"find", "/find?q=weather", "/r/lobby", findPageRenders},
	} {
		t.Run(tc.op, func(t *testing.T) {
			s := &gatedAgentService{gate: make(chan struct{}), op: tc.op}
			h := Handler(s)
			get := func(path string) *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
				return w
			}
			var wg sync.WaitGroup
			codes := make(chan int, tc.turns)
			for range tc.turns {
				wg.Add(1)
				go func() { defer wg.Done(); codes <- get(tc.path).Code }()
			}
			deadline := time.Now().Add(5 * time.Second)
			for s.inflight.Load() < int64(tc.turns) {
				if time.Now().After(deadline) {
					t.Fatalf("only %d renders started", s.inflight.Load())
				}
				time.Sleep(time.Millisecond)
			}
			for _, other := range []string{tc.other, "/", "/agent/" + strings.Repeat("ab", 32)} {
				if w := get(other); w.Code == 503 {
					t.Fatalf("%s was held by the %s cap", other, tc.path)
				}
			}
			start := time.Now()
			busy := get(tc.path)
			if busy.Code != 503 || busy.Header().Get("Retry-After") != "5" || !strings.Contains(busy.Body.String(), "This page is busy.") {
				t.Fatalf("past the cap: %d %v", busy.Code, busy.Header())
			}
			if waited := time.Since(start); waited < renderWait {
				t.Fatalf("answered busy after %v, before waiting %v", waited, renderWait)
			}
			if s.reads.Load() != int64(tc.turns) || s.most.Load() != int64(tc.turns) {
				t.Fatalf("store reads %d, most in flight %d, want %d", s.reads.Load(), s.most.Load(), tc.turns)
			}
			close(s.gate)
			wg.Wait()
			close(codes)
			for code := range codes {
				if code == 503 {
					t.Fatal("a render inside the cap answered 503")
				}
			}
		})
	}
}

func TestRoomPagePath(t *testing.T) {
	for p, want := range map[string]bool{
		"/r/lobby": true, "/r/lobby/notes": true,
		"/r/": false, "/r/lobby/a/b": false, "/rooms": false, "/find": false,
	} {
		if roomPagePath(p) != want {
			t.Errorf("roomPagePath(%q) = %v", p, !want)
		}
	}
}

// A kind's turns keep at most turns*WaitersPerTurn readers waiting (C991):
// the next one is refused at once instead of holding a slot of the request
// pool for renderWait, and a place to wait frees when a waiter leaves.
func TestTurnsBoundTheirWaiters(t *testing.T) {
	turns := NewTurns(2)
	r := httptest.NewRequest("GET", "/", nil)
	for range 2 {
		if !turns.Take(r, time.Millisecond) {
			t.Fatal("a free turn was refused")
		}
	}
	waiters := 2 * WaitersPerTurn
	got := make(chan bool, waiters)
	for range waiters {
		go func() { got <- turns.Take(r, 5*time.Second) }()
	}
	deadline := time.Now().Add(5 * time.Second)
	for turns.waiting.Load() < int64(waiters) {
		if time.Now().After(deadline) {
			t.Fatalf("only %d waiting", turns.waiting.Load())
		}
		time.Sleep(time.Millisecond)
	}
	start := time.Now()
	if turns.Take(r, 5*time.Second) {
		t.Fatal("a reader past every place to wait got a turn")
	}
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("refused after %v, want at once", waited)
	}
	if turns.waiting.Load() != int64(waiters) {
		t.Fatalf("a refused reader left %d waiting", turns.waiting.Load())
	}
	// Every waiter gets a turn as turns free.
	for range waiters {
		turns.Release()
		if !<-got {
			t.Fatal("a waiter was refused while turns freed")
		}
	}
	if turns.waiting.Load() != 0 {
		t.Fatalf("%d still waiting", turns.waiting.Load())
	}
	// A reader that leaves frees its place.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if turns.Take(r.WithContext(ctx), 5*time.Second) || turns.waiting.Load() != 0 {
		t.Fatalf("a reader that left: waiting %d", turns.waiting.Load())
	}
}

// Through the handler (C991): with every agent page turn taken and every
// place to wait for one held, the next agent page is answered busy at once
// without reading the store, and other pages are still served.
func TestAgentPageWaitersAreBounded(t *testing.T) {
	s := &gatedAgentService{gate: make(chan struct{})}
	h := Handler(s)
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w
	}
	id := func(i int) string {
		return "/agent/" + strings.Repeat("0", 60) + strings.Repeat(string("0123456789abcdef"[i%16]), 4)
	}
	held := agentPageRenders * (1 + WaitersPerTurn)
	var wg sync.WaitGroup
	codes := make(chan int, held)
	for i := range held {
		wg.Add(1)
		go func() { defer wg.Done(); codes <- get(id(i)).Code }()
		if i == agentPageRenders-1 {
			deadline := time.Now().Add(5 * time.Second)
			for s.inflight.Load() < agentPageRenders {
				if time.Now().After(deadline) {
					t.Fatalf("only %d renders started", s.inflight.Load())
				}
				time.Sleep(time.Millisecond)
			}
		}
	}
	// The waiters hold their places for renderWait; well inside it the next
	// page is refused at once.
	time.Sleep(renderWait / 5)
	start := time.Now()
	busy := get(id(held))
	if waited := time.Since(start); busy.Code != 503 || waited > renderWait/2 {
		t.Fatalf("past every place to wait: %d after %v", busy.Code, waited)
	}
	if busy.Header().Get("Retry-After") != "5" || s.reads.Load() != agentPageRenders {
		t.Fatalf("refused page: %v, store reads %d", busy.Header(), s.reads.Load())
	}
	if w := get("/r/lobby"); w.Code == 503 {
		t.Fatal("a room page was held by the agent page waiters")
	}
	close(s.gate)
	wg.Wait()
	close(codes)
	ok := 0
	for code := range codes {
		if code == 200 {
			ok++
		}
	}
	if ok < agentPageRenders {
		t.Fatalf("only %d of the held pages rendered", ok)
	}
}
