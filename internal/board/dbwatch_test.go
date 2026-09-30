package board

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The watchdog warns after three failed probes in a row (the connection not
// free within the probe's deadline), writes one goroutine dump, and stays
// quiet for ten minutes. Busy but healthy (probes succeed) never warns.
func TestDBWatchWarnsOnceAndDumps(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(testTime, 0)
	w := &dbWatch{dumpDir: dir, now: func() time.Time { return now }}
	stalled := context.DeadlineExceeded
	for i := 0; i < 5; i++ {
		w.sample(nil)
	}
	w.sample(stalled)
	w.sample(stalled)
	w.sample(nil)
	if w.warns != 0 {
		t.Fatalf("warned on a short wait")
	}
	w.sample(stalled)
	w.sample(stalled)
	w.sample(stalled)
	w.sample(stalled)
	if w.warns != 1 {
		t.Fatalf("warns = %d, want 1", w.warns)
	}
	dumps, _ := filepath.Glob(filepath.Join(dir, "goroutines-*.txt"))
	if len(dumps) != 1 {
		t.Fatalf("dumps %v", dumps)
	}
	body, err := os.ReadFile(dumps[0])
	info, _ := os.Stat(dumps[0])
	if err != nil || !strings.Contains(string(body), "goroutine") || info.Mode().Perm() != 0600 {
		t.Fatalf("dump: %v %v", err, info.Mode())
	}
	now = now.Add(dbStallRepeat)
	w.sample(errors.New("still stalled"))
	if w.warns != 2 {
		t.Fatalf("warns = %d, want 2", w.warns)
	}
}

// Against a real store: a free connection probes clean, a held one fails
// the probe within its deadline.
func TestDBWatchProbeSeesAHeldConnection(t *testing.T) {
	s := openTest(t, updatesConfig())
	probe := func(ctx context.Context) error {
		conn, err := s.db.Conn(ctx)
		if err != nil {
			return err
		}
		return conn.Close()
	}
	ctx, cancel := context.WithTimeout(testContext, time.Second)
	defer cancel()
	if err := probe(ctx); err != nil {
		t.Fatalf("free connection: %v", err)
	}
	tx, err := s.db.BeginTx(testContext, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	ctx2, cancel2 := context.WithTimeout(testContext, 100*time.Millisecond)
	defer cancel2()
	if err := probe(ctx2); err == nil {
		t.Fatal("probe got the connection while a transaction held it")
	}
}
