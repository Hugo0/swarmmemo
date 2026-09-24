package board

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestActivitySplitsPostsByKind(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "activity.db"), Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 9, 24, 15, 30, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	for _, room := range [][2]string{{"lobby", "public"}, {"help", "public"}, {"secret", "private"}} {
		if _, err = store.db.Exec("INSERT INTO rooms(name,visibility,owner,created_at) VALUES(?,?,?,0) ON CONFLICT(name) DO UPDATE SET visibility=excluded.visibility", room[0], room[1], "owner"); err != nil {
			t.Fatal(err)
		}
	}
	seq := 0
	insert := func(room, kind, account, text, replyTo, supersedes, via string, at time.Time, hidden int) {
		t.Helper()
		seq++
		key := ""
		if account != "" {
			key = "key-" + account
		}
		if _, err := store.db.Exec(`INSERT INTO events(display_seq,id,room,page,text,kind,author,account,handle,public_key,signature,payload,created_at,hash,reply_to,recipient,hidden,supersedes,via) VALUES(?,?,?,'main',?,?,?,?,'',?,'','',?,'',?,'',?,?,?)`, seq, randomID(), room, text, kind, account, account, key, at.Unix(), replyTo, hidden, supersedes, via); err != nil {
			t.Fatal(err)
		}
	}
	hour := func(back int) time.Time {
		return now.Truncate(time.Hour).Add(-time.Duration(back) * time.Hour).Add(5 * time.Minute)
	}
	insert("lobby", "note", "alice", "hello", "", "", "post", hour(0), 0)
	insert("help", "note", "alice", "héllo", "x", "", "get", hour(0), 0)    // a reply, 6 bytes
	insert("lobby", "note", "alice", "hello!", "", "x", "post", hour(0), 0) // an edit: bytes, not a post
	insert("lobby", "note", "", "anon", "", "", "get", hour(1), 0)          // anonymous counts as community
	insert("lobby", "note", "bob", "signed", "", "", "post", hour(1), 0)
	insert("lobby", "simulation", "sim", "demo", "", "", "post", hour(1), 0)
	insert("lobby", "imported", "curator", "copy", "", "", "", hour(2), 0)
	insert("lobby", "note", "bob", "hidden", "", "", "post", hour(2), 1)   // hidden: never counted
	insert("secret", "note", "bob", "private", "", "", "post", hour(2), 0) // private: never counted
	insert("lobby", "note", "carol", "old", "", "", "post", now.AddDate(0, 0, -20), 0)
	// An edit is not activity, and an edit of a hidden original is not counted at all.
	insert("lobby", "note", "frank", "edited", "", "y", "post", hour(3), 0)
	if _, err = store.db.Exec("UPDATE events SET origin=(SELECT id FROM events WHERE text='hidden') WHERE text='edited'"); err != nil {
		t.Fatal(err)
	}
	insert("lobby", "note", "dave", "ancient", "", "", "post", now.AddDate(0, 0, -200), 0) // before the window

	if err = store.AddReaderCounts(context.Background(), now.Format("2006-01-02"), map[string]int64{"llms_txt:other": 3, "skill_md:crawler": 2, "for_agents:other": 1}); err != nil {
		t.Fatal(err)
	}
	a, err := store.ReadActivity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if d := a.Days[len(a.Days)-1]; d.Reads != 4 || d.CrawlerReads != 2 {
		t.Fatalf("reads today: %d other, %d crawler", d.Reads, d.CrawlerReads)
	}
	if len(a.Hours) != ActivityHours || len(a.Days) != ActivityDays {
		t.Fatalf("buckets: %d hours, %d days", len(a.Hours), len(a.Days))
	}
	last := a.Hours[len(a.Hours)-1]
	if !last.Start.Equal(now.Truncate(time.Hour)) || !a.Days[len(a.Days)-1].Start.Equal(now.Truncate(24*time.Hour)) {
		t.Fatalf("last buckets start %v and %v", last.Start, a.Days[len(a.Days)-1].Start)
	}
	if last.Posts != (ActivitySeries{Signed: 2}) || last.Bytes.Signed != 5+6+6 || last.Replies != 1 || last.Agents != 1 || last.Rooms != 2 {
		t.Fatalf("current hour: %+v", last)
	}
	prev := a.Hours[len(a.Hours)-2]
	if prev.Posts != (ActivitySeries{Signed: 1, Anonymous: 1, Simulation: 1}) || prev.Agents != 1 || prev.Rooms != 1 {
		t.Fatalf("previous hour: %+v", prev)
	}
	if got := a.Hours[len(a.Hours)-3].Posts; got != (ActivitySeries{Imported: 1}) {
		t.Fatalf("imported hour: %+v", got)
	}
	today := a.Days[len(a.Days)-1]
	if today.Posts != (ActivitySeries{Signed: 3, Anonymous: 1, Simulation: 1, Imported: 1}) || today.Agents != 2 || today.NewAgents != 2 {
		t.Fatalf("today: %+v", today)
	}
	if a.Days[len(a.Days)-21].NewAgents != 1 || a.Agents7 != 2 || a.Agents30 != 3 {
		t.Fatalf("new/active agents: day-20 %+v, 7d %d, 30d %d", a.Days[len(a.Days)-21], a.Agents7, a.Agents30)
	}
	// Native posts by channel; the edit, simulated and imported posts are left out.
	if len(a.Via) != 2 || a.Via["post"] != 3 || a.Via["get"] != 2 {
		t.Fatalf("via: %v", a.Via)
	}
	if h := a.Hours[len(a.Hours)-4]; h.Posts.Total() != 0 || h.Bytes.Total() != 0 {
		t.Fatalf("edit of a hidden original counted: %+v", h)
	}
	// Totals are the stats operation's own counts, cached with the rest.
	if a.DatabaseBytes <= 0 || a.Totals["messages"] != 10 {
		t.Fatalf("database %d, totals %v", a.DatabaseBytes, a.Totals)
	}

	// The summary is shared for a minute, then recomputed.
	insert("lobby", "note", "erin", "late", "", "", "post", now, 0)
	if again, _ := store.ReadActivity(context.Background()); again != a {
		t.Fatal("recomputed within the minute")
	}
	now = now.Add(time.Minute)
	again, err := store.ReadActivity(context.Background())
	if err != nil || again == a || again.Days[len(again.Days)-1].Posts.Signed != 4 {
		t.Fatalf("not recomputed after a minute: %+v %v", again, err)
	}
}

// A caller queued behind a running computation gives up with its own context.
func TestActivityWaitersHonourTheirContext(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "gate.db"), Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.activityGate <- struct{}{} // a computation in progress
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := store.ReadActivity(ctx); err != context.DeadlineExceeded {
		t.Fatalf("queued caller: %v", err)
	}
	<-store.activityGate
	if a, err := store.ReadActivity(context.Background()); err != nil || a == nil {
		t.Fatalf("after: %v", err)
	}
}
