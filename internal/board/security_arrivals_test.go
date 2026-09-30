package board

// Security review of the arrivals branch (f20205c). Each test began as a
// proof of concept that passed while its weakness was present (1adefe2) and
// now asserts the fixed behaviour.

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// rawClientDay is the stored counts of the day containing at, unshaped.
func rawClientDay(t *testing.T, s *Store, at int64) map[string]int64 {
	t.Helper()
	_, raw, err := s.readClientCounts(testContext, time.Unix(at, 0), 1)
	if err != nil {
		t.Fatal(err)
	}
	return raw[0]
}

// noClientName fails when text appears in any stored counter or in the
// published day.
func noClientName(t *testing.T, s *Store, text string) {
	t.Helper()
	var n int
	_ = s.db.QueryRow("SELECT count(*) FROM counters WHERE instr(scope,?)>0", text).Scan(&n)
	if n != 0 {
		t.Fatalf("a counter holds the client name %q", text)
	}
	days, err := s.ReadClientStats(testContext, time.Unix(testTime, 0), 1)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fmt.Sprintf("%+v", days), text) {
		t.Fatalf("the client name %q is published: %+v", text, days)
	}
}

// H1 (fixed): an unknown MCP client name is never stored or published, so
// no threshold can be bypassed by a restart: the day only counts it.
func TestSecArrivalsRestartNeverPublishesName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "board.sqlite")
	open := func() *Store {
		s, err := Open(path, Config{})
		if err != nil {
			t.Fatal(err)
		}
		s.now = func() time.Time { return time.Unix(testTime+3600, 0) }
		return s
	}
	const text = "free-usdc.claim-now.xyz"
	s := open()
	s.CountClient("other-mcp", "mcp_initialize", text)
	s.FlushClientCounts() // what shutdown does
	noClientName(t, s, text)
	_ = s.Close()
	s = open() // restart: same caller
	defer s.Close()
	s.CountClient("other-mcp", "mcp_initialize", text)
	s.FlushClientCounts()
	noClientName(t, s, text)
	// Counted once per process; a restart mid-day can count it twice.
	days, _ := s.ReadClientStats(testContext, time.Unix(testTime, 0), 1)
	if days[0].UnknownMCPClients != 2 || days[0].Families["other-mcp"].Counts["mcp_initialize"] != 2 {
		t.Fatalf("day: %+v", days[0])
	}
}

// H1b (fixed): a command stamped before midnight that completes after it
// does not move the in-memory day back, so today's names and subjects are
// kept and the same name is not counted again.
func TestSecArrivalsMidnightFlapKeepsDay(t *testing.T) {
	s := openTest(t, Config{})
	const text = "one-caller-name"
	s.CountClient("other-mcp", "mcp_initialize", text)
	// A command that started one second before midnight completes now.
	s.countClientCommand(WithClientFamily(testContext, "claude"), actor{account: "anon:x"}, Command{Operation: "messages.list"}, "198.51.100.1", false, 0, testTime-1)
	s.clients.mu.Lock()
	day, held := s.clients.day, s.clients.names[text]
	s.clients.mu.Unlock()
	if day != testDay || held != 1 {
		t.Fatalf("the day moved back: day %q, name held %d", day, held)
	}
	s.CountClient("other-mcp", "mcp_initialize", text)
	s.FlushClientCounts()
	if n := rawClientDay(t, s, testTime)["unknown_mcp_clients"]; n != 1 {
		t.Fatalf("unknown_mcp_clients = %d, want 1", n)
	}
	// The stale command is counted on its own day, without once-a-day facts.
	if raw := rawClientDay(t, s, testTime-1); raw["family:claude:anonymous_subjects"] != 0 {
		t.Fatalf("a stale command claimed a subject: %v", raw)
	}
}

// M1 (fixed): with no names published there is nothing to evict. A flood of
// junk names from one address only raises the day's count, up to its bound,
// and nothing named is stored.
func TestSecArrivalsNameFloodOnlyCounts(t *testing.T) {
	s := openTest(t, Config{})
	s.CountClient("other-mcp", "mcp_initialize", "real-client")
	for i := 0; i < clientNamesPerDay+50; i++ {
		for j := 0; j < 3; j++ {
			s.CountClient("other-mcp", "mcp_initialize", fmt.Sprintf("junk-%03d", i))
		}
	}
	s.FlushClientCounts()
	raw := rawClientDay(t, s, testTime)
	if raw["unknown_mcp_clients"] != clientNamesPerDay || raw["family:other-mcp:mcp_initialize"] != 1+3*(clientNamesPerDay+50) {
		t.Fatalf("counts: %v", raw)
	}
	for key := range raw {
		if strings.Contains(key, "junk") || strings.Contains(key, "real-client") {
			t.Fatalf("a name was stored: %q", key)
		}
	}
	s.clients.mu.Lock()
	held := len(s.clients.names)
	s.clients.mu.Unlock()
	if held != clientNamesPerDay {
		t.Fatalf("held %d names in memory, want %d", held, clientNamesPerDay)
	}
}

// M2 (fixed): unwritten counts are never served, today's command metrics are
// not published at all, and a closed day's count below ClientCountMinimum
// is left out, so polling cannot tie one key's first post to its client.
func TestSecArrivalsNoRealTimeFamilyAttribution(t *testing.T) {
	s := openTest(t, Config{})
	families := func(at int64) map[string]ClientFamilyDay {
		t.Helper()
		days, err := s.ReadClientStats(testContext, time.Unix(at, 0), 1)
		if err != nil {
			t.Fatal(err)
		}
		return days[0].Families
	}
	post := func(seed byte) {
		t.Helper()
		if _, err := s.Execute(WithClientFamily(testContext, "gemini"), signed(keyFor(seed), Command{Operation: "post", Room: "lobby", Text: fmt.Sprintf("hello from agent %d", seed)}), "203.0.113.9"); err != nil {
			t.Fatal(err)
		}
	}
	// The first count of a process is written at once, the next ones at the
	// next write (the clock stands still here): only written counts are served.
	s.CountClient("gemini", "discovery", "")
	for s.clients.flushing.Load() {
		time.Sleep(time.Millisecond)
	}
	s.CountClient("gemini", "discovery", "")
	if n := families(testTime)["gemini"].Counts["discovery"]; n != 1 {
		t.Fatalf("unwritten counts were served: discovery = %d", n)
	}
	post(91)
	s.FlushClientCounts()
	if f := families(testTime)["gemini"]; len(f.Counts) != 1 || len(f.Services) != 0 {
		t.Fatalf("today's command metrics were published: %+v", f)
	}
	if raw := rawClientDay(t, s, testTime); raw["family:gemini:new_keys"] != 1 || raw["family:gemini:first_posts"] != 1 {
		t.Fatalf("not counted: %v", raw)
	}
	// The next day, one key is below the minimum: still left out.
	s.now = func() time.Time { return time.Unix(testTime+86400, 0) }
	if f := families(testTime)["gemini"]; len(f.Counts) != 1 || f.Counts["discovery"] != 2 {
		t.Fatalf("a count below %d was published: %+v", ClientCountMinimum, f)
	}
	// Three keys that day: published once the day is closed.
	s.now = func() time.Time { return time.Unix(testTime, 0) }
	post(93)
	post(94)
	s.FlushClientCounts()
	if f := families(testTime)["gemini"]; len(f.Counts) != 1 {
		t.Fatalf("today: %+v", f)
	}
	s.now = func() time.Time { return time.Unix(testTime+86400, 0) }
	if f := families(testTime)["gemini"]; f.Counts["new_keys"] != 3 || f.Counts["first_posts"] != 3 || f.Counts["discovery"] != 2 {
		t.Fatalf("closed day: %+v", f)
	}
}

// L1 (fixed): a key's return is read from its previous write
// (identities.last_seen, inside the command's transaction), not from its
// request records, which are never scanned.
func TestSecArrivalsReturningFromLastSeen(t *testing.T) {
	s := openTest(t, Config{})
	ctx := WithClientFamily(testContext, "claude")
	key := keyFor(92)
	account := keyID(key)
	// Request records alone (as many as a busy key leaves) count nothing.
	for i := int64(0); i < 200; i++ {
		if _, err := s.db.Exec("INSERT INTO requests(actor,request_key,digest,result,created_at) VALUES(?,?,?,?,?)", account, fmt.Sprintf("nonce:%d", i), fmt.Sprintf("%064d", i), "{}", testTime-86400*(1+i%7)); err != nil {
			t.Fatal(err)
		}
	}
	s.now = func() time.Time { return time.Unix(testTime-3*86400+60, 0) }
	if _, err := s.Execute(ctx, signed(key, Command{Operation: "post", Room: "lobby", Text: "three days ago", Timestamp: testTime - 3*86400 + 60}), "203.0.113.9"); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Unix(testTime+60, 0) }
	for i := 0; i < 3; i++ {
		if _, err := s.Execute(ctx, signed(key, Command{Operation: "post", Room: "lobby", Text: fmt.Sprintf("today %d", i)}), "203.0.113.9"); err != nil {
			t.Fatal(err)
		}
	}
	s.FlushClientCounts()
	raw := rawClientDay(t, s, testTime)
	if raw["family:claude:returning_3d"] != 1 || raw["family:claude:returning_1d"] != 0 || raw["family:claude:returning_7d"] != 0 {
		t.Fatalf("returning: %v", raw)
	}
}

// Clean check: concurrent counting, background flushes and reads neither
// race nor lose or double a count.
func TestArrivalsConcurrentCountsExact(t *testing.T) {
	s := openTest(t, Config{})
	var clock int64 = testTime + 60
	s.now = func() time.Time { return time.Unix(clock, 0) }
	done := make(chan struct{})
	const workers, each = 8, 500
	for w := 0; w < workers; w++ {
		go func() {
			for i := 0; i < each; i++ {
				s.CountClient("claude", "discovery", "")
				if i%50 == 0 {
					s.clients.mu.Lock()
					s.clients.lastFlush = time.Time{} // force a background flush now
					s.clients.mu.Unlock()
				}
			}
			done <- struct{}{}
		}()
	}
	go func() {
		for i := 0; i < 50; i++ {
			_, _ = s.ReadClientStats(testContext, time.Unix(testTime, 0), 1)
		}
		done <- struct{}{}
	}()
	for i := 0; i < workers+1; i++ {
		<-done
	}
	for s.clients.flushing.Load() {
		time.Sleep(time.Millisecond)
	}
	s.FlushClientCounts()
	days, err := s.ReadClientStats(testContext, time.Unix(testTime, 0), 1)
	if err != nil {
		t.Fatal(err)
	}
	if n := days[0].Families["claude"].Counts["discovery"]; n != workers*each {
		t.Fatalf("discovery = %d, want %d", n, workers*each)
	}
}
