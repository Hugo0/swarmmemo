package board

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These opt-in benchmarks use disposable LOCAL databases, not production traffic
// or adoption evidence. Setup uses the real command/permission/commit pathway.
// Run with -run '^$' -bench '^BenchmarkBoard' -benchtime=200x -benchmem.
const benchmarkRetainedMessages = 1000

func benchmarkStore(b *testing.B) *Store {
	b.Helper()
	s, err := Open(filepath.Join(b.TempDir(), "benchmark.sqlite"), Config{
		DailyBytes: 1 << 30, AnonymousDailyBytes: 1 << 30, GlobalDailyBytes: 1 << 30,
	})
	if err != nil {
		b.Fatal(err)
	}
	s.now = func() time.Time { return time.Unix(testTime, 0) }
	b.Cleanup(func() {
		if err := s.Close(); err != nil {
			b.Error(err)
		}
	})
	return s
}

func benchmarkExecute(b *testing.B, s *Store, c Command) Result {
	b.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := s.Execute(ctx, c, "disposable-benchmark-origin")
	if err != nil || !r.OK {
		b.Fatalf("%s: ok=%t error=%v", c.Operation, r.OK, err)
	}
	return r
}

func benchmarkCorpus(b *testing.B, s *Store, key ed25519.PrivateKey, works int) []string {
	b.Helper()
	ids := make([]string, 0, works)
	text := strings.Repeat("Disposable local benchmark text. ", 32) // 1056 bytes.
	for i := 0; i < benchmarkRetainedMessages; i++ {
		kind := "note"
		if i < works {
			kind = "request"
		}
		r := benchmarkExecute(b, s, signed(key, Command{Operation: "post", Room: "benchmark", Page: "main", Kind: kind, Text: text, RequestID: fmt.Sprintf("setup-%d", i)}))
		if i < works {
			ids = append(ids, r.Receipt.ID)
			benchmarkExecute(b, s, workCommand(s, key, Command{Operation: "work.create", MessageID: r.Receipt.ID}))
		}
	}
	return ids
}

func BenchmarkBoardPublicEvents(b *testing.B) {
	s := benchmarkStore(b)
	benchmarkCorpus(b, s, keyFor(211), 0)
	for _, tc := range []struct {
		name string
		cmd  Command
	}{
		{"latest25", Command{Operation: "messages.list", Room: "benchmark", Limit: 25}},
		{"history25", Command{Operation: "messages.list", Room: "benchmark", Cursor: "start", Limit: 25}},
		{"search_miss", Command{Operation: "messages.list", Room: "benchmark", Query: "no-such-benchmark-content", Limit: 25}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r := benchmarkExecute(b, s, tc.cmd)
				if tc.name == "search_miss" && len(r.Messages) != 0 || tc.name != "search_miss" && len(r.Messages) == 0 {
					b.Fatal("unexpected retained-event fixture result")
				}
			}
			b.StopTimer()
			b.ReportMetric(benchmarkRetainedMessages, "retained_msgs")
			b.ReportMetric(float64(len(benchmarkExecute(b, s, tc.cmd).Messages)), "events/op")
		})
	}
}

func BenchmarkBoardPublicWork(b *testing.B) {
	s := benchmarkStore(b)
	owner, worker := keyFor(212), keyFor(213)
	ids := benchmarkCorpus(b, s, owner, 100)
	// 101 real transitions: create plus 50 claim/reject cycles. No computation,
	// assets, arbitrary instructions, public endpoint, or synthetic adoption.
	for fence := int64(1); fence <= 50; fence++ {
		benchmarkExecute(b, s, workCommand(s, worker, Command{Operation: "work.claim", MessageID: ids[0], TTL: 60}))
		benchmarkExecute(b, s, workCommand(s, owner, Command{Operation: "work.reject", MessageID: ids[0], Amount: fence, Reason: "Local benchmark fixture reset"}))
	}
	for _, tc := range []struct {
		name string
		cmd  Command
		key  string
	}{
		{"list25", Command{Operation: "works.list", Limit: 25}, "works"},
		{"filtered25", Command{Operation: "works.list", Kind: "open", Query: "review", Limit: 25}, "works"},
		{"history25", Command{Operation: "work.history", MessageID: ids[0], Limit: 25}, "transitions"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r := benchmarkExecute(b, s, tc.cmd)
				if r.Data[tc.key] == nil || r.NextCursor == "" {
					b.Fatal("expected nonempty paginated public work result")
				}
			}
			b.StopTimer()
			b.ReportMetric(100, "retained_works")
		})
	}
}

func BenchmarkBoardSignedWrite(b *testing.B) {
	for _, delegated := range []bool{false, true} {
		name := "ordinary"
		if delegated {
			name = "delegated"
		}
		b.Run(name, func(b *testing.B) {
			// Explicitly bound retained growth even if invoked with an overly long
			// benchmark duration. Read benchmarks have no iteration storage growth.
			if b.N > 10000 {
				b.Fatal("write benchmark limited to 10000 new records; use -benchtime=200x")
			}
			s := benchmarkStore(b)
			parent, child := keyFor(214), keyFor(215)
			benchmarkCorpus(b, s, parent, 0)
			key := parent
			var grant *DelegationContext
			if delegated {
				benchmarkExecute(b, s, grantCommand(s, parent, child, "benchmark", 3600, 1<<29, []string{"post"}))
				key = child
				grant = &DelegationContext{Schema: 1, GrantID: keyID(child), Generation: s.generation}
			}
			before := benchmarkDatabaseBytes(b, s)
			text := strings.Repeat("Disposable local benchmark text. ", 32)
			b.ReportAllocs()
			b.SetBytes(int64(len(text)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				// Includes client-side signing, verification, parent/grant charging,
				// SQLite WAL/FULL commit and receipt caching, but no HTTP/JSON wire.
				c := signed(key, Command{Operation: "post", Room: "benchmark", Page: "main", Kind: "note", Visibility: "public", Text: text, RequestID: fmt.Sprintf("measured-%d", i), Delegation: grant})
				r := benchmarkExecute(b, s, c)
				if r.Receipt == nil || r.Receipt.Duplicate {
					b.Fatal("benchmark must commit a fresh mutation")
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(benchmarkDatabaseBytes(b, s)-before)/float64(b.N), "db_bytes/post")
			b.ReportMetric(benchmarkRetainedMessages, "initial_msgs")
		})
	}
}

// Logical SQLite pages include rows and indexes; they exclude WAL files,
// filesystem allocation, backups and temporary memory. This is not total disk.
func benchmarkDatabaseBytes(b *testing.B, s *Store) int64 {
	b.Helper()
	var pages, size int64
	if err := s.db.QueryRow("PRAGMA page_count").Scan(&pages); err != nil {
		b.Fatal(err)
	}
	if err := s.db.QueryRow("PRAGMA page_size").Scan(&size); err != nil {
		b.Fatal(err)
	}
	return pages * size
}
