package board

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The watchdog warns after three stalled samples (the connection in use while
// callers queue), writes one goroutine dump, and stays quiet for ten minutes.
func TestDBWatchWarnsOnceAndDumps(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(testTime, 0)
	var st sql.DBStats
	w := &dbWatch{stats: func() sql.DBStats { return st }, dumpDir: dir, now: func() time.Time { return now }}
	step := func(inUse int, waits int64, waited time.Duration) {
		st.InUse, st.WaitCount, st.WaitDuration = inUse, st.WaitCount+waits, st.WaitDuration+waited
		w.sample(time.Second)
	}
	// Busy but healthy: in use, nobody waiting.
	for i := 0; i < 5; i++ {
		step(1, 0, 0)
	}
	// Two stalled samples, then a free one: no warning.
	step(1, 1, 0)
	step(1, 0, 2*time.Second)
	step(0, 0, 0)
	if w.warns != 0 {
		t.Fatalf("warned on a short wait")
	}
	// Three in a row: one warning, one dump.
	step(1, 2, 0)
	step(1, 0, time.Second)
	step(1, 1, 3*time.Second)
	step(1, 1, 3*time.Second)
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
	// Still stalled ten minutes later: the next warning.
	now = now.Add(dbStallRepeat)
	step(1, 1, time.Second)
	if w.warns != 2 {
		t.Fatalf("warns = %d, want 2", w.warns)
	}
}
