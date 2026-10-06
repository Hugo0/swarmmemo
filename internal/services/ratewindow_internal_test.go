package services

import (
	"strconv"
	"testing"
)

// windowIn is the one bounded table of rate windows (anonymous calls,
// receiver deliveries): it reuses a key's window, drops stale windows only
// when full, and fails closed when every window is live.
func TestWindowInBoundsTheTable(t *testing.T) {
	const now = int64(86400*100 + 30)
	table := map[string]*anonWindow{}
	stale := func(w *anonWindow) bool { return w.minute != now/60 }
	first := windowIn(table, "a", stale)
	if first == nil || windowIn(table, "a", stale) != first {
		t.Fatal("a key's window is not reused")
	}
	first.roll(now)
	for i := 1; i < rateEntriesMax; i++ {
		w := windowIn(table, strconv.Itoa(i), stale)
		if w == nil {
			t.Fatalf("refused entry %d of %d", i, rateEntriesMax)
		}
		w.roll(now - 120) // live in another minute: stale
	}
	if len(table) != rateEntriesMax {
		t.Fatalf("table holds %d windows", len(table))
	}
	if w := windowIn(table, "new", stale); w == nil || len(table) != 2 || table["a"] != first {
		t.Fatalf("a full table did not drop exactly its stale windows: %d left", len(table))
	}
	for i := len(table); i < rateEntriesMax; i++ {
		windowIn(table, "live"+strconv.Itoa(i), stale).roll(now)
	}
	table["new"].roll(now)
	if windowIn(table, "one-more", stale) != nil {
		t.Fatal("a table full of live windows did not fail closed")
	}
	if windowIn(table, "a", stale) != first {
		t.Fatal("a full table refused a key it already holds")
	}
}
