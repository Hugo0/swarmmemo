package board

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
)

// The read pool (C936). The store's one read-write connection serializes
// every command; on 2026-11-12 crawler surges of uncached anonymous reads
// (agent.get at ~250 ms, conversation, room and agent pages) filled it and
// every route stalled 5-9 s while the box sat 90% idle. Anonymous reads of
// public data now run on a few read-only connections of their own
// (readOnlyPragmas: mode=ro, query_only), beside the writer: WAL lets a
// reader keep its snapshot while the writer commits.
//
// Only an unsigned command with no delegation, private read or app token,
// of an operation in readPoolOperations, takes the pool; everything signed
// stays on the writer, in one transaction with its own writes. A committed
// write is in every snapshot begun after it, so an anonymous reader still
// reads what it wrote a request earlier. query_only makes a write on the
// pool fail loudly rather than happen.
//
// The no-pool-in-a-tx rule (1.22.0) holds: a pool transaction is begun
// only at the top of executeCommand and in publicRead, never under another
// transaction, and the reads it runs were written for the one-connection
// store, where any nested s.db use under a held transaction would already
// have deadlocked.

const (
	// DefaultReadConnections is the read pool's size when the configuration
	// leaves it zero (READ_CONNECTIONS unset).
	DefaultReadConnections = 2
	// MaxReadConnections bounds READ_CONNECTIONS.
	MaxReadConnections = 8
)

// readPoolOperations are the anonymous public reads the pool serves. Each
// was audited to write nothing for an unsigned caller (export, which
// assigns export sequences, is not one). Their in-memory caches that a
// write empties (hot agents, rankings, vote weights) store a pool read's
// answer only while cacheFresh holds.
var readPoolOperations = map[string]bool{
	"agent.get": true, "agents.list": true, "agent.posts": true,
	"messages.list": true, "message.get": true, "thread.get": true,
	"room.get": true, "rooms.list": true, "room.pages": true,
	"feed.get": true,
}

// readConnections is the pool size config asks for: zero means the default,
// a negative count none.
func readConnections(n int) int {
	switch {
	case n == 0:
		return DefaultReadConnections
	case n < 0:
		return 0
	case n > MaxReadConnections:
		return MaxReadConnections
	}
	return n
}

// openReadPool opens n read-only connections to the database at path, or
// none for n == 0 or an in-memory database (a second connection to
// :memory: is another database).
func openReadPool(path string, n int) (*sql.DB, error) {
	if n <= 0 || path == ":memory:" {
		return nil, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dsn := (&url.URL{Scheme: "file", Path: abs, RawQuery: readOnlyPragmas}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(n)
	db.SetMaxIdleConns(n)
	var only int
	if err = db.QueryRow("PRAGMA query_only").Scan(&only); err == nil && only != 1 {
		err = fmt.Errorf("read pool connection is not query_only")
	}
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// usesReadPool reports whether executeCommand runs cmd, from authenticated
// actor a, on the read pool.
func (s *Store) usesReadPool(ctx context.Context, cmd Command, a actor) bool {
	return s.readDB != nil && !a.signed && readPoolOperations[cmd.Operation] && !mutation(cmd.Operation) &&
		cmd.PrivateRead == nil && cmd.Delegation == nil && bearerFrom(ctx) == ""
}

// readSnapshotKey carries, in a pool transaction's context, cacheGen as it
// was before the transaction's snapshot began.
type readSnapshotKey struct{}

// beginPoolRead begins a read-only transaction on the read pool, its
// snapshot taken at once, and returns the context its reads run under.
func (s *Store) beginPoolRead(ctx context.Context) (context.Context, *sql.Tx, error) {
	// Read before the snapshot: a write that commits after the snapshot
	// bumps cacheGen after this read, so cacheFresh then says so.
	ctx = context.WithValue(ctx, readSnapshotKey{}, s.cacheGen.Load())
	tx, err := s.readDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ctx, nil, err
	}
	// A deferred transaction's snapshot begins at its first read.
	var n int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM meta WHERE key='generation'").Scan(&n); err != nil {
		_ = tx.Rollback()
		return ctx, nil, err
	}
	return ctx, tx, nil
}

// cacheFresh reports whether a read under ctx may store what it read in a
// cache that writes empty: always on the writer (no write runs beside it),
// and on the pool only while no write has committed since its snapshot
// began. A stale snapshot's answer is still right for its reader; it is
// only never kept for the next one.
func (s *Store) cacheFresh(ctx context.Context) bool {
	gen, ok := ctx.Value(readSnapshotKey{}).(uint64)
	return !ok || gen == s.cacheGen.Load()
}

// bumpCacheGen marks a committed change: pool reads whose snapshot began
// before it no longer store into the caches.
func (s *Store) bumpCacheGen() { s.cacheGen.Add(1) }
