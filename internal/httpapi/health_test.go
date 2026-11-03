package httpapi

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// healthService is a service whose storage check counts its calls and
// fails while broken is set.
type healthService struct {
	fakeService
	checks atomic.Int64
	broken atomic.Bool
}

func (h *healthService) Health(context.Context) error {
	h.checks.Add(1)
	if h.broken.Load() {
		return errors.New("storage gone")
	}
	return nil
}

// /health is answered before the request pool and from the last good
// storage check while it is recent; with none recent a probe checks for
// itself, and a failed check is a 503 (C726).
func TestHealthAnswersFromMemoryBeforeThePool(t *testing.T) {
	svc := &healthService{}
	s := New(svc, nil, Config{Version: "test"})
	if w := makeRequest(s, "GET", "/health", "", ""); w.Code != 200 || svc.checks.Load() != 1 {
		t.Fatalf("first probe: %d after %d checks", w.Code, svc.checks.Load())
	}
	// A full pool sheds other reads, never /health.
	for range cap(s.inflight) {
		s.inflight <- struct{}{}
	}
	for range 5 {
		if w := makeRequest(s, "GET", "/health", "", ""); w.Code != 200 {
			t.Fatalf("with the pool full: %d %s", w.Code, w.Body)
		}
	}
	if w := makeRequest(s, "GET", "/api/messages?room=lobby", "", ""); w.Code != 503 {
		t.Fatalf("a read with the pool full: %d", w.Code)
	}
	for range cap(s.inflight) {
		<-s.inflight
	}
	if svc.checks.Load() != 1 {
		t.Fatalf("recent probes checked storage %d times", svc.checks.Load())
	}
	// A memory past healthStale is not trusted: the probe checks, and fails.
	svc.broken.Store(true)
	s.healthMemo.mu.Lock()
	s.healthMemo.okAt = time.Now().Add(-healthStale)
	s.healthMemo.mu.Unlock()
	if w := makeRequest(s, "GET", "/health", "", ""); w.Code != 503 || svc.checks.Load() != 2 {
		t.Fatalf("stale memory, broken storage: %d after %d checks", w.Code, svc.checks.Load())
	}
	// Between healthFresh and healthStale it answers and refreshes behind.
	svc.broken.Store(false)
	s.healthMemo.mu.Lock()
	s.healthMemo.okAt = time.Now().Add(-healthFresh)
	s.healthMemo.mu.Unlock()
	if w := makeRequest(s, "GET", "/health", "", ""); w.Code != 200 {
		t.Fatalf("aging memory: %d", w.Code)
	}
	deadline := time.Now().Add(5 * time.Second)
	for svc.checks.Load() != 3 || time.Since(s.healthOK()) > time.Second {
		if time.Now().After(deadline) {
			t.Fatalf("no background refresh: %d checks", svc.checks.Load())
		}
		time.Sleep(time.Millisecond)
	}
	if w := makeRequest(s, "POST", "/health", "", ""); w.Code != 405 {
		t.Fatalf("POST /health: %d", w.Code)
	}
}

func (s *Server) healthOK() time.Time {
	s.healthMemo.mu.Lock()
	defer s.healthMemo.mu.Unlock()
	return s.healthMemo.okAt
}
