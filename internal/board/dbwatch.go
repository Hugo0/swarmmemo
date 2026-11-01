package board

// The database stall watchdog. The store has one database connection
// (MaxOpenConns 1): a request that holds it and waits on something else
// stalls every other request until a deadline ends it. Counting waiters
// cannot tell that from ordinary queueing (the first version warned on every
// busy minute), so the watchdog probes instead: every few seconds it asks the
// pool for the connection with a short deadline, and when that fails several
// times in a row it logs a warning and writes one goroutine dump, so the
// holder can be found. It reads no rows and logs no request data.
//
// A stall shorter than the streak (about 15 s) is still worth seeing: on
// 2026-11-01 a burst of agent pages held every route for 5-7 s and nothing
// was logged (C701). So a probe waits up to dbProbeLimit, counts as failed
// past dbProbeTimeout as before, and any one probe that waited dbSlowProbe
// or longer logs its own "db slow probe" warning (no dump, no page).

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/pprof"
	"sync"
	"time"
)

const (
	dbWatchTick    = 5 * time.Second
	dbProbeTimeout = 2 * time.Second // a probe slower than this is a failed one
	dbProbeLimit   = dbWatchTick - 500*time.Millisecond
	dbSlowProbe    = 3 * time.Second  // a probe this slow logs a warning of its own
	dbStallTicks   = 3                // consecutive failed probes before a warning
	dbStallRepeat  = 10 * time.Minute // at most one warning and dump per this
)

// WatchDB runs the stall watchdog until ctx ends. Goroutine dumps go to
// dumpDir as goroutines-<unix>.txt (mode 0600); "" writes none.
func (s *Store) WatchDB(ctx context.Context, dumpDir string) {
	probe := func(ctx context.Context) error {
		conn, err := s.db.Conn(ctx)
		if err != nil {
			return err
		}
		return conn.Close()
	}
	w := &dbWatch{probe: probe, dumpDir: dumpDir, now: time.Now}
	go w.run(ctx, dbWatchTick)
}

type dbWatch struct {
	probe   func(context.Context) error
	dumpDir string
	now     func() time.Time

	mu     sync.Mutex
	streak int
	warned time.Time
	warns  int                  // for tests
	slow   int                  // for tests: slow-probe warnings
	warn   func(string, ...any) // slog.Warn; tests replace it
}

func (w *dbWatch) run(ctx context.Context, tick time.Duration) {
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, dbProbeLimit)
			start := time.Now()
			err := w.probe(pctx)
			wait := time.Since(start)
			cancel()
			if ctx.Err() != nil {
				return
			}
			w.sample(err, wait)
		}
	}
}

// sample records one probe: probeErr nil when the connection was free
// within the probe's deadline, wait how long the probe waited for it.
func (w *dbWatch) sample(probeErr error, wait time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if wait >= dbSlowProbe {
		w.slow++
		attrs := []any{"wait", wait.Round(time.Millisecond).String(), "slow_after", dbSlowProbe.String()}
		if probeErr != nil {
			attrs = append(attrs, "error", probeErr.Error())
		}
		w.warnf("db slow probe", attrs...)
	}
	if probeErr == nil && wait > dbProbeTimeout {
		probeErr = context.DeadlineExceeded // as slow as a failed probe
	}
	if probeErr == nil {
		w.streak = 0
		return
	}
	w.streak++
	if w.streak < dbStallTicks {
		return
	}
	now := w.now()
	if !w.warned.IsZero() && now.Sub(w.warned) < dbStallRepeat {
		return
	}
	w.warned = now
	w.warns++
	attrs := []any{"failed_probes", w.streak, "probe_timeout", dbProbeTimeout.String(), "error", probeErr.Error()}
	if path, err := w.dump(now); err != nil {
		attrs = append(attrs, "dump_error", err.Error())
	} else if path != "" {
		attrs = append(attrs, "goroutines", path)
	}
	w.warnf("db stall", attrs...)
}

func (w *dbWatch) warnf(msg string, args ...any) {
	if w.warn != nil {
		w.warn(msg, args...)
		return
	}
	slog.Warn(msg, args...)
}

// dump writes every goroutine's stack to dumpDir.
func (w *dbWatch) dump(now time.Time) (string, error) {
	if w.dumpDir == "" {
		return "", nil
	}
	path := filepath.Join(w.dumpDir, fmt.Sprintf("goroutines-%d.txt", now.Unix()))
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	err = pprof.Lookup("goroutine").WriteTo(f, 2)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return path, err
}
