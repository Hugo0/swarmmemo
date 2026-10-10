package board

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"swarmmemo/internal/testrace"
)

// The 1.22.0 stall: paths inside a command's transaction (services.list, an
// unsigned service.call's no-key check, screen.text's quote) asked the real
// moderation engine whether screening runs; with its policy cache older than
// 30s it reread the parameter store on the pool while the transaction held
// the only connection, and waited on itself until the request's deadline.
// With the policy served from memory each answers at once, and a concurrent
// health ping gets the connection.
func TestNoSelfDeadlockOnStalePolicy(t *testing.T) {
	c := updatesConfig()
	c.Features = Features{Services: []string{"screen", "notary"}, Ledger: LedgerOn, AnonPrefix: true, Moderation: true}
	s := openTest(t, c)
	t.Cleanup(s.stopServices)
	setAnonCredit(t, s, 1000, 1_000_000)
	base := time.Unix(testTime+23*3600, 0) // the anonymous tier's whole day is released
	var mu sync.Mutex
	offset := time.Duration(0)
	s.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return base.Add(offset) }
	stale := func() { mu.Lock(); offset += 31 * time.Second; mu.Unlock() }
	key := keyFor(7)
	within := func(label string, cmd Command, source string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		start := time.Now()
		_, err := s.Execute(ctx, cmd, source)
		took := time.Since(start)
		t.Logf("%s: %v in %v", label, err, took)
		// Waiting on itself runs to the 2s deadline; an answer from memory
		// is well under 100ms (a race build: under a second).
		if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil || took >= testrace.Slowdown*100*time.Millisecond {
			t.Fatalf("%s took %v (err %v): the command waited on its own connection", label, took, err)
		}
	}
	within("services.list, policy fresh", Command{Operation: "services.list"}, "198.51.100.9")

	stale()
	health := make(chan error, 1)
	go func() {
		time.Sleep(20 * time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		health <- s.Health(ctx)
	}()
	within("services.list, policy stale", Command{Operation: "services.list"}, "198.51.100.9")
	if err := <-health; err != nil {
		t.Fatalf("concurrent health: %v", err)
	}

	stale()
	within("unsigned service.call (no-key check), policy stale", Command{Operation: "service.call", Target: "notary",
		Data: svcData("stamp", map[string]any{"text": "stall"}, 10), RequestID: anonID("stall1")}, "198.51.100.10")

	stale()
	within("unsigned screen.text, policy stale", Command{Operation: "service.call", Target: "screen",
		Data: svcData("text", map[string]any{"text": "hello", "source": "web"}, 1000), RequestID: anonID("stall2")}, "198.51.100.11")

	stale()
	signedScreen := func() Command {
		c := Command{Operation: "service.call", Target: "screen", Data: svcData("text", map[string]any{"text": "hello", "source": "web"}, 1000), RequestID: anonID("stall3")}
		c.Timestamp = s.now().Unix()
		return signed(key, c)
	}
	within("signed screen.text quote, policy stale", signedScreen(), "198.51.100.12")
}

// The per-connection PRAGMAs survive a connection replacement: a query
// interrupted by its context may cost the pool its connection, and the one
// opened in its place still has a busy timeout and foreign keys.
func TestPragmasSurviveConnectionReplacement(t *testing.T) {
	s := openTest(t, updatesConfig())
	check := func(label string) {
		t.Helper()
		var busy, fk int
		if err := s.db.QueryRow("PRAGMA busy_timeout").Scan(&busy); err != nil {
			t.Fatal(err)
		}
		if err := s.db.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
			t.Fatal(err)
		}
		if busy != 5000 || fk != 1 {
			t.Fatalf("%s: busy_timeout=%d foreign_keys=%d, want 5000 and 1", label, busy, fk)
		}
	}
	check("at open")
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		var n int64
		err := s.db.QueryRowContext(ctx, "WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x<1000000000) SELECT count(*) FROM c").Scan(&n)
		cancel()
		if err == nil {
			t.Fatal("the long query was not interrupted")
		}
		check("after an interrupted query")
	}
	// A connection closed outright is replaced from the DSN too.
	s.db.SetMaxIdleConns(0) // closes the idle connection
	s.db.SetMaxIdleConns(1)
	check("after a replaced connection")
}
