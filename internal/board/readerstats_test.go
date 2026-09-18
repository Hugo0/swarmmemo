package board

import (
	"context"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

func TestReaderCountsStoreOnlyDayMetricAndInteger(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "reader.db"), Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err = store.AddReaderCounts(ctx, "2026-09-12", map[string]int64{"llms_txt:crawler": 2, "updates_with_agent:other": 1}); err != nil {
		t.Fatal(err)
	}
	if err = store.AddReaderCounts(ctx, "2026-09-12", map[string]int64{"llms_txt:crawler": 3}); err != nil {
		t.Fatal(err)
	}
	// Anything that is not a known metric and class is refused, so no caller can
	// place an address, user agent or fingerprint into a scope string.
	for _, bad := range []map[string]int64{{"198.51.100.8:other": 1}, {"llms_txt:Mozilla/5.0": 1}, {"llms_txt": 1}, {"llms_txt:other": -1}} {
		if store.AddReaderCounts(ctx, "2026-09-12", bad) == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
	if store.AddReaderCounts(ctx, "2026-9-12", map[string]int64{"llms_txt:other": 1}) == nil {
		t.Fatal("accepted a malformed day")
	}
	var columns []string
	rows, err := store.db.Query("SELECT name FROM pragma_table_info('counters') ORDER BY cid")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name string
		_ = rows.Scan(&name)
		columns = append(columns, name)
	}
	rows.Close()
	if len(columns) != 2 || columns[0] != "scope" || columns[1] != "value" {
		t.Fatalf("counters must hold only scope and value: %v", columns)
	}
	var version int
	_ = store.db.QueryRow("PRAGMA user_version").Scan(&version)
	if version != 9 {
		t.Fatalf("reader counters must not change the schema version: %d", version)
	}
	shape := regexp.MustCompile(`^reader:[0-9]{4}-[0-9]{2}-[0-9]{2}:[a-z_]+:(crawler|other)$`)
	rows, err = store.db.Query("SELECT scope FROM counters WHERE scope LIKE 'reader:%'")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for rows.Next() {
		var scope string
		_ = rows.Scan(&scope)
		if !shape.MatchString(scope) {
			t.Fatalf("unexpected reader scope %q", scope)
		}
		n++
	}
	rows.Close()
	if n != 2 {
		t.Fatalf("want 2 reader rows, got %d", n)
	}
	stats, err := store.ReadDailyStats(ctx, time.Date(2026, 9, 13, 5, 0, 0, 0, time.UTC), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 2 || stats[0].Day != "2026-09-12" || stats[1].Day != "2026-09-13" {
		t.Fatalf("days: %+v", stats)
	}
	if stats[0].Reads["llms_txt:crawler"] != 5 || stats[0].Reads["updates_with_agent:other"] != 1 || len(stats[1].Reads) != 0 {
		t.Fatalf("reads: %+v", stats)
	}
}

func TestDailyPostMetricsCountFirstAndReturningKeys(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "posts.db"), Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	day := func(d, hour int) int64 { return time.Date(2026, 9, d, hour, 0, 0, 0, time.UTC).Unix() }
	for _, room := range [][2]string{{"lobby", "public"}, {"secret", "private"}} {
		if _, err = store.db.Exec("INSERT OR IGNORE INTO rooms(name,visibility,owner,created_at) VALUES(?,?,?,0)", room[0], room[1], "owner"); err != nil {
			t.Fatal(err)
		}
	}
	seq := 0
	insert := func(room, kind, key string, at int64, hidden int) {
		t.Helper()
		seq++
		id := randomID()
		if _, err := store.db.Exec(`INSERT INTO events(display_seq,id,room,page,text,kind,author,account,handle,public_key,signature,payload,created_at,hash,reply_to,recipient,hidden) VALUES(?,?,?,'main','x',?,?,?,'',?,'','',?,'','','',?)`, seq, id, room, kind, key, key, key, at, hidden); err != nil {
			t.Fatal(err)
		}
	}
	// Day 10: A posts first (twice), S only posts simulations, I only imported.
	insert("lobby", "note", "A", day(10, 1), 0)
	insert("lobby", "note", "A", day(10, 9), 0)
	insert("lobby", "simulation", "S", day(10, 2), 0)
	insert("lobby", "imported", "I", day(10, 3), 0)
	// Day 11: A returns; B first posts; S's first ordinary post is its first post;
	// an anonymous post and a private-room post never count.
	insert("lobby", "note", "A", day(11, 4), 0)
	insert("lobby", "", "B", day(11, 5), 0)
	insert("lobby", "note", "S", day(11, 6), 0)
	insert("lobby", "note", "", day(11, 7), 0)
	insert("secret", "note", "P", day(11, 8), 0)
	// Day 12: B returns; I's imported post again does not count; C's only post is hidden.
	insert("lobby", "note", "B", day(12, 1), 0)
	insert("lobby", "imported", "I", day(12, 2), 0)
	insert("lobby", "note", "C", day(12, 3), 1)
	// Day 13: a simulation from A does not make A a returning key.
	insert("lobby", "simulation", "A", day(13, 3), 0)

	stats, err := store.ReadDailyStats(context.Background(), time.Unix(day(13, 23), 0), 5)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][2]int64{"2026-09-09": {0, 0}, "2026-09-10": {1, 0}, "2026-09-11": {2, 1}, "2026-09-12": {0, 1}, "2026-09-13": {0, 0}}
	for _, got := range stats {
		if w := want[got.Day]; got.FirstPostKeys != w[0] || got.ReturningKeys != w[1] {
			t.Fatalf("%s: first=%d returning=%d, want %v", got.Day, got.FirstPostKeys, got.ReturningKeys, w)
		}
	}
	// A window that starts after a key's first post still classifies it correctly.
	stats, err = store.ReadDailyStats(context.Background(), time.Unix(day(11, 0), 0), 1)
	if err != nil || len(stats) != 1 || stats[0].FirstPostKeys != 2 || stats[0].ReturningKeys != 1 {
		t.Fatalf("single day window: %+v %v", stats, err)
	}
}
