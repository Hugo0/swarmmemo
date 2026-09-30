package board

// The database stall watchdog. The store has one database connection
// (MaxOpenConns 1): a request that holds it and waits on something else
// stalls every other request until a deadline ends it. The watchdog samples
// the pool's counters once a second and, when the connection stays in use
// while callers keep queueing for it, logs a warning and writes one goroutine
// dump, so the holder can be found. It reads no rows and logs no request data.

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/pprof"
	"sync"
	"time"
)

const (
	dbWatchTick   = time.Second
	dbStallTicks  = 3                // consecutive stalled samples before a warning
	dbStallRepeat = 10 * time.Minute // at most one warning and dump per this
)

// WatchDB runs the stall watchdog until ctx ends. Goroutine dumps go to
// dumpDir as goroutines-<unix>.txt (mode 0600); "" writes none.
func (s *Store) WatchDB(ctx context.Context, dumpDir string) {
	w := &dbWatch{stats: s.db.Stats, dumpDir: dumpDir, now: time.Now}
	go w.run(ctx, dbWatchTick)
}

type dbWatch struct {
	stats   func() sql.DBStats
	dumpDir string
	now     func() time.Time

	mu     sync.Mutex
	prev   sql.DBStats
	streak int
	warned time.Time
	warns  int // for tests
}

func (w *dbWatch) run(ctx context.Context, tick time.Duration) {
	t := time.NewTicker(tick)
	defer t.Stop()
	w.prev = w.stats()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.sample(tick)
		}
	}
}

// sample takes one reading. A stalled sample is the connection in use while
// the time callers spent waiting for it grew by at least a tick, or new
// callers started waiting (a caller still waiting adds no wait time yet).
func (w *dbWatch) sample(tick time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	cur := w.stats()
	waited := cur.WaitDuration - w.prev.WaitDuration
	queued := cur.WaitCount - w.prev.WaitCount
	w.prev = cur
	if cur.InUse < 1 || (waited < tick && queued == 0) {
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
	attrs := []any{"in_use", cur.InUse, "wait_count", cur.WaitCount, "wait_duration", cur.WaitDuration.Round(time.Millisecond).String(), "stalled_seconds", w.streak}
	if path, err := w.dump(now); err != nil {
		attrs = append(attrs, "dump_error", err.Error())
	} else if path != "" {
		attrs = append(attrs, "goroutines", path)
	}
	slog.Warn("db stall", attrs...)
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
