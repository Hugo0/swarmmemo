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

// gatedAgentService answers agent.get only when its gate opens, counting
// the reads in flight; every other command answers at once.
type gatedAgentService struct {
	gate     chan struct{}
	inflight atomic.Int64
	most     atomic.Int64
	reads    atomic.Int64
}

func (s *gatedAgentService) Execute(_ context.Context, c board.Command, _ string) (board.Result, error) {
	if c.Operation != "agent.get" {
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
	return board.Result{OK: true, Agent: &board.Agent{ID: c.Target}}, nil
}
func (*gatedAgentService) Moderate(context.Context, string, string, bool) error { return nil }
func (*gatedAgentService) Close() error                                         { return nil }

// At most agentPageRenders agent pages render at once (C701): one past the
// cap waits up to agentPageWait, then is answered 503 busy with Retry-After
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
	if waited := time.Since(start); waited < agentPageWait {
		t.Fatalf("answered busy after %v, before waiting %v", waited, agentPageWait)
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
