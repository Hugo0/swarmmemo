package board

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// readPoolFixture is a store with an agent, a room and a thread, and every
// read-pool operation's anonymous command over them.
func readPoolFixture(t *testing.T, c Config) (*Store, []Command) {
	t.Helper()
	s := openTest(t, c)
	author := keyFor(61)
	run(t, s, signed(author, Command{Operation: "agent.register", Handle: "pooled"}))
	root := run(t, s, signed(author, Command{Operation: "post", Room: "lobby", Text: "a root for the read pool"})).Receipt.ID
	run(t, s, signed(author, Command{Operation: "post", Room: "lobby", Text: "a reply on the read pool", ReplyTo: root}))
	run(t, s, Command{Operation: "post", Room: "lobby", Text: "an anonymous post"})
	cmds := []Command{
		{Operation: "agent.get", Target: keyID(author)},
		{Operation: "agent.get", Target: "pooled"},
		{Operation: "agents.list"},
		{Operation: "agent.posts", Target: "pooled"},
		{Operation: "messages.list", Room: "lobby"},
		{Operation: "messages.list", Room: "lobby", Data: `{"schema":1,"sort":"hot"}`},
		{Operation: "message.get", MessageID: root},
		{Operation: "thread.get", MessageID: root},
		{Operation: "room.get", Room: "lobby"},
		{Operation: "rooms.list"},
		{Operation: "room.pages", Room: "lobby"},
		{Operation: "feed.get"},
	}
	covered := map[string]bool{}
	for _, c := range cmds {
		covered[c.Operation] = true
	}
	for op := range readPoolOperations {
		if !covered[op] {
			t.Fatalf("read pool operation %s has no command in the fixture", op)
		}
		if mutation(op) {
			t.Fatalf("read pool operation %s is a mutation", op)
		}
	}
	return s, cmds
}

// holdWrite begins a write transaction on the store's one read-write
// connection, writes, and holds it until the returned release is called.
func holdWrite(t *testing.T, s *Store) (release func()) {
	t.Helper()
	tx, err := s.db.BeginTx(testContext, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(testContext, "INSERT INTO meta(key,value) VALUES('readpool-test','held') ON CONFLICT(key) DO UPDATE SET value=excluded.value"); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	var once sync.Once
	release = func() { once.Do(func() { _ = tx.Commit() }) }
	t.Cleanup(release)
	return release
}

// TestReadPoolReadsBesideAHeldWrite (C936): with a write transaction held on
// the one read-write connection, every anonymous read-pool operation still
// answers, at once and concurrently, on the read-only pool; a write queues
// behind the held one and lands once it commits; nothing deadlocks. Each
// read runs on a query_only connection, so one that wrote would fail.
func TestReadPoolReadsBesideAHeldWrite(t *testing.T) {
	s, cmds := readPoolFixture(t, Config{})
	if s.readDB == nil || s.readDB.Stats().MaxOpenConnections != DefaultReadConnections {
		t.Fatalf("read pool not open at its default size: %+v", s.readDB)
	}
	want := map[int]Result{}
	for i, c := range cmds {
		want[i] = run(t, s, c)
	}
	release := holdWrite(t, s)

	// A write waits for the held transaction (and so is not answered yet).
	wrote := make(chan error, 1)
	go func() {
		_, err := s.Execute(testContext, Command{Operation: "post", Room: "lobby", Text: "written once the hold ends"}, "test-origin")
		wrote <- err
	}()

	ctx, cancel := context.WithTimeout(testContext, 10*time.Second)
	defer cancel()
	const rounds = 4
	var wg sync.WaitGroup
	errs := make(chan error, rounds*len(cmds))
	started := time.Now()
	for range rounds {
		for i, c := range cmds {
			wg.Add(1)
			go func() {
				defer wg.Done()
				res, err := s.Execute(ctx, c, "test-origin")
				if err == nil && !res.OK {
					err = context.Canceled
				}
				if err == nil && len(res.Messages) != len(want[i].Messages) {
					t.Errorf("%s: %d messages under the held write, %d before", c.Operation, len(res.Messages), len(want[i].Messages))
				}
				if err != nil {
					errs <- err
				}
			}()
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("read beside a held write: %v", err)
	}
	if took := time.Since(started); took > 5*time.Second {
		t.Fatalf("reads beside a held write took %v", took)
	}
	select {
	case err := <-wrote:
		t.Fatalf("a write answered while another held the write connection: %v", err)
	default:
	}
	release()
	select {
	case err := <-wrote:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the queued write never landed once the hold ended")
	}
	// The anonymous writer reads its own committed post on the pool.
	res := run(t, s, Command{Operation: "messages.list", Room: "lobby", Limit: 50})
	found := false
	for _, m := range res.Messages {
		found = found || m.Text == "written once the hold ends"
	}
	if !found {
		t.Fatal("a pool read missed a committed write")
	}
}

// TestReadPoolOffQueuesBehindTheWriter: READ_CONNECTIONS=0 (a negative
// Config.ReadConnections) is the one connection as before C936, where an
// anonymous read waits for a held write.
func TestReadPoolOffQueuesBehindTheWriter(t *testing.T) {
	s, cmds := readPoolFixture(t, Config{ReadConnections: -1})
	if s.readDB != nil {
		t.Fatal("read pool open with ReadConnections < 0")
	}
	release := holdWrite(t, s)
	ctx, cancel := context.WithTimeout(testContext, 300*time.Millisecond)
	defer cancel()
	if _, err := s.Execute(ctx, cmds[0], "test-origin"); err == nil {
		t.Fatal("with the pool off, a read answered while the one connection was held")
	}
	release()
	run(t, s, cmds[0])
}

// TestReadPoolIsQueryOnly: the pool's connections refuse every write, so a
// moved read that wrote would fail rather than write. Two pool transactions
// are open at once (the pool's two connections), and each refuses.
func TestReadPoolIsQueryOnly(t *testing.T) {
	s := openTest(t, Config{})
	ctx1, tx1, err := s.beginPoolRead(testContext)
	if err != nil {
		t.Fatal(err)
	}
	defer tx1.Rollback()
	ctx2, tx2, err := s.beginPoolRead(testContext)
	if err != nil {
		t.Fatal(err)
	}
	defer tx2.Rollback()
	if n := s.readDB.Stats().InUse; n != 2 {
		t.Fatalf("%d pool connections in use, want 2", n)
	}
	for i, write := range []func() error{
		func() error {
			_, err := tx1.ExecContext(ctx1, "INSERT INTO meta(key,value) VALUES('readpool-write','x')")
			return err
		},
		func() error {
			_, err := tx2.ExecContext(ctx2, "UPDATE meta SET value=value WHERE key='generation'")
			return err
		},
	} {
		err := write()
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "readonly") {
			t.Fatalf("write %d on the read pool: %v", i, err)
		}
	}
	var n int
	if err = s.db.QueryRow("SELECT count(*) FROM meta WHERE key='readpool-write'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("a pool write landed: %d %v", n, err)
	}
}

// TestReadPoolRoutesOnlyAnonymousReads: signed reads, writes and the
// reads outside readPoolOperations stay on the read-write connection.
func TestReadPoolRoutesOnlyAnonymousReads(t *testing.T) {
	s, cmds := readPoolFixture(t, Config{})
	key := keyFor(62)
	for _, c := range cmds {
		if !s.usesReadPool(testContext, c, actor{}) {
			t.Errorf("anonymous %s not on the read pool", c.Operation)
		}
		if s.usesReadPool(testContext, c, actor{signed: true}) {
			t.Errorf("signed %s on the read pool", c.Operation)
		}
		withGrant := c
		withGrant.Delegation = &DelegationContext{}
		if s.usesReadPool(testContext, withGrant, actor{}) {
			t.Errorf("delegated %s on the read pool", c.Operation)
		}
	}
	for _, op := range []string{"post", "vote", "export", "updates.get", "home", "service.call", "find", "quota.get"} {
		if s.usesReadPool(testContext, Command{Operation: op}, actor{}) {
			t.Errorf("%s on the read pool", op)
		}
	}
	// A signed read beside a held write waits for it, as before C936.
	register(t, s, key)
	release := holdWrite(t, s)
	ctx, cancel := context.WithTimeout(testContext, 300*time.Millisecond)
	defer cancel()
	if _, err := s.Execute(ctx, signed(key, Command{Operation: "messages.list", Room: "lobby"}), "test-origin"); err == nil {
		t.Fatal("a signed read answered from the read pool")
	}
	release()
}

// TestReadPoolStaleSnapshotKeepsNoCache: a pool read whose snapshot began
// before a committed change still answers, but stores nothing in the
// caches a write empties.
func TestReadPoolStaleSnapshotKeepsNoCache(t *testing.T) {
	s := openTest(t, Config{})
	ctx, tx, err := s.beginPoolRead(testContext)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if !s.cacheFresh(ctx) || !s.cacheFresh(testContext) {
		t.Fatal("a fresh pool read may not cache")
	}
	s.dropRankings()
	if s.cacheFresh(ctx) {
		t.Fatal("a pool read begun before a change may still cache")
	}
	if !s.cacheFresh(testContext) {
		t.Fatal("a read on the writer may no longer cache")
	}
	s.rankMu.Lock()
	s.hotAgentsCached = nil
	s.rankMu.Unlock()
	if _, err = s.hotAgentsRanking(ctx, tx, "1", testTime); err != nil {
		t.Fatal(err)
	}
	s.rankMu.Lock()
	cached := s.hotAgentsCached
	s.rankMu.Unlock()
	if cached != nil {
		t.Fatal("a stale pool snapshot shared its hot agent ranking")
	}
}

// TestReadConnections: zero is the default, negative off, and the count is
// capped.
func TestReadConnections(t *testing.T) {
	got := []int{readConnections(0), readConnections(-1), readConnections(3), readConnections(99)}
	want := []int{DefaultReadConnections, 0, 3, MaxReadConnections}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("readConnections: got %v, want %v", got, want)
		}
	}
	if db, err := openReadPool(":memory:", 2); db != nil || err != nil {
		t.Fatalf("an in-memory database got a read pool: %v %v", db, err)
	}
}
