package httpapi

import (
	"context"
	"net/http"
	"sync"
	"time"

	"swarmmemo/internal/board"
)

// /health (C726). It used to read one meta row on every probe, queued on the
// store's one connection behind whatever else was reading, and to wait for a
// slot of the request pool: during a crawl of heavy pages a load balancer's
// probe found the pool full (503 busy) or waited past its 2 s bound
// (503 storage_unavailable), while signed traffic was still answered.
//
// Now a GET or HEAD of /health is answered before the request pool, and the
// storage check is remembered: within healthFresh of the last good check it
// answers from memory; up to healthStale it still does, while one check
// runs in the background (bounded by healthCheckLimit, so a queue on the
// connection is not mistaken for a failure). With no good check within
// healthStale (startup, or storage that stopped answering) a probe checks
// for itself within healthProbeLimit, as before, and a failed check is a
// 503 storage_unavailable.
const (
	healthFresh      = 5 * time.Second
	healthStale      = 30 * time.Second
	healthCheckLimit = 10 * time.Second
	healthProbeLimit = 2 * time.Second
)

// healthMemo is the last good storage check.
type healthMemo struct {
	mu       sync.Mutex
	okAt     time.Time
	checking bool
}

type healthChecker interface{ Health(context.Context) error }

// storageHealthy reports whether storage answered within healthStale,
// checking for itself when it has not.
func (s *Server) storageHealthy(ctx context.Context) error {
	checker, ok := s.service.(healthChecker)
	if !ok {
		return nil
	}
	m := &s.healthMemo
	m.mu.Lock()
	age := time.Since(m.okAt)
	if !m.okAt.IsZero() && age < healthStale {
		if age >= healthFresh && !m.checking {
			m.checking = true
			go s.refreshHealth(checker)
		}
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()
	probe, cancel := context.WithTimeout(ctx, healthProbeLimit)
	err := checker.Health(probe)
	cancel()
	if err == nil {
		m.mu.Lock()
		m.okAt = time.Now()
		m.mu.Unlock()
	}
	return err
}

func (s *Server) refreshHealth(checker healthChecker) {
	ctx, cancel := context.WithTimeout(context.Background(), healthCheckLimit)
	err := checker.Health(ctx)
	cancel()
	m := &s.healthMemo
	m.mu.Lock()
	if err == nil {
		m.okAt = time.Now()
	}
	m.checking = false
	m.mu.Unlock()
}

// health answers a GET or HEAD of /health.
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if err := s.storageHealthy(r.Context()); err != nil {
		writeError(w, &board.Error{Status: 503, Code: "storage_unavailable", Message: "Storage is unavailable."})
		return
	}
	jsonResponse(w, 200, map[string]any{"ok": true, "version": s.cfg.Version})
}
