package board

import (
	"context"
	"errors"
	"log/slog"
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
		w.sample(nil, 0)
	}
	w.sample(stalled, dbProbeTimeout)
	w.sample(stalled, dbProbeTimeout)
	w.sample(nil, 0)
	if w.warns != 0 {
		t.Fatalf("warned on a short wait")
	}
	w.sample(stalled, dbProbeTimeout)
	w.sample(stalled, dbProbeTimeout)
	w.sample(stalled, dbProbeTimeout)
	w.sample(stalled, dbProbeTimeout)
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
	w.sample(errors.New("still stalled"), dbProbeTimeout)
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

// A single probe that waited dbSlowProbe or longer logs one "db slow probe"
// line with its wait, whether it then got the connection or timed out, and
// writes no dump; a quicker probe logs nothing. A probe slower than
// dbProbeTimeout counts toward the stall streak as a failed one (C701).
func TestDBWatchLogsASlowProbe(t *testing.T) {
	dir := t.TempDir()
	var lines []string
	w := &dbWatch{dumpDir: dir, now: func() time.Time { return time.Unix(testTime, 0) }, warn: func(msg string, args ...any) {
		var b strings.Builder
		slog.New(slog.NewTextHandler(&b, nil)).Warn(msg, args...)
		lines = append(lines, b.String())
	}}
	w.sample(nil, dbSlowProbe-time.Millisecond)
	if len(lines) != 0 || w.slow != 0 || w.streak != 1 {
		t.Fatalf("a probe under %v logged %q (streak %d)", dbSlowProbe, lines, w.streak)
	}
	w.sample(nil, dbProbeTimeout)
	if w.streak != 0 {
		t.Fatalf("a probe within %v kept the streak at %d", dbProbeTimeout, w.streak)
	}
	w.sample(nil, 3700*time.Millisecond)
	w.sample(context.DeadlineExceeded, dbProbeLimit)
	if w.slow != 2 || len(lines) != 2 {
		t.Fatalf("slow probes logged %d lines: %q", len(lines), lines)
	}
	if !strings.Contains(lines[0], `msg="db slow probe" wait=3.7s slow_after=3s`) || strings.Contains(lines[0], "error=") {
		t.Fatalf("first line %q", lines[0])
	}
	if !strings.Contains(lines[1], "wait=4.5s") || !strings.Contains(lines[1], `error="context deadline exceeded"`) {
		t.Fatalf("second line %q", lines[1])
	}
	if dumps, _ := filepath.Glob(filepath.Join(dir, "goroutines-*.txt")); len(dumps) != 0 || w.warns != 0 {
		t.Fatalf("slow probes dumped %v or warned a stall (%d)", dumps, w.warns)
	}
	// Two slow probes are two failed ones: a third makes a stall.
	w.sample(nil, dbProbeTimeout+time.Millisecond)
	if w.warns != 1 || !strings.Contains(lines[len(lines)-1], `msg="db stall" failed_probes=3`) {
		t.Fatalf("slow probes did not count toward the stall: warns %d, %q", w.warns, lines)
	}
}
