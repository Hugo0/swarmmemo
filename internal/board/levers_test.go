package board

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
)

func leversNow(t *testing.T, s *Store) allowance.Levers {
	t.Helper()
	l, err := s.leverSource().Levers(testContext, s.db, s.now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func pull(t *testing.T, s *Store, name string, args ...string) LeverChange {
	t.Helper()
	c, err := s.PullLever(testContext, LeverPull{Name: name, Args: args, Reason: "test: " + name, Actor: "steward"})
	if err != nil {
		t.Fatalf("pull %s: %v", name, err)
	}
	return c
}

func release(t *testing.T, s *Store, name string, args ...string) {
	t.Helper()
	if _, err := s.ReleaseLever(testContext, name, args, "steward", "test over"); err != nil {
		t.Fatalf("release %s: %v", name, err)
	}
}

func leverLog(t *testing.T, s *Store) []LeverLogEntry {
	t.Helper()
	r, err := s.LeverReport(testContext)
	if err != nil {
		t.Fatal(err)
	}
	return r.Log
}

// With nothing pulled, nothing acts: the zero lever set, no refusals.
func TestLeversInertUntilPulled(t *testing.T) {
	s := openTest(t, Config{})
	l := leversNow(t, s)
	if l.SignedOnly || l.ProvenOnly || l.FreezeTransfers || l.PauseNewKeys || l.Tier4SharePPM != -1 || l.BudgetCutPPM != nil || l.Version != 0 {
		t.Fatalf("levers: %+v", l)
	}
	run(t, s, Command{Operation: "post", Room: "lobby", Text: "anonymous", RequestID: "anon"})
	r, err := s.LeverReport(testContext)
	if err != nil || len(r.Pulled) != 0 || len(r.Levers) != 0 || len(r.Log) != 0 || len(r.Names) != 7 {
		t.Fatalf("report: %+v %v", r, err)
	}
}

// Every lever: pull sets its effect, release restores, each is logged.
func TestEveryLeverPullAndRelease(t *testing.T) {
	s := openTest(t, Config{})
	cases := []struct {
		name  string
		args  []string
		check func(allowance.Levers) bool
	}{
		{LeverTier4Shrink, []string{"250000"}, func(l allowance.Levers) bool { return l.Tier4SharePPM == 250000 }},
		{LeverSignedOnly, nil, func(l allowance.Levers) bool { return l.SignedOnly }},
		{LeverPauseNewKeys, nil, func(l allowance.Levers) bool { return l.PauseNewKeys && l.PauseNewKeysSince == testTime }},
		{LeverCutBudget, []string{"post_bytes", "500000"}, func(l allowance.Levers) bool { return l.BudgetCutPPM[allowance.PostBytes] == 500000 }},
		{LeverProvenOnly, nil, func(l allowance.Levers) bool { return l.ProvenOnly }},
		{LeverFreezeTransfers, nil, func(l allowance.Levers) bool { return l.FreezeTransfers }},
	}
	zero := leversNow(t, s)
	for _, c := range cases {
		change := pull(t, s, c.name, c.args...)
		l := leversNow(t, s)
		if !c.check(l) || l.Version != change.Seq {
			t.Fatalf("%s pulled: %+v", c.name, l)
		}
		report, err := s.LeverReport(testContext)
		if err != nil || len(report.Pulled) != 1 || report.Pulled[0] != c.name {
			t.Fatalf("%s report: %+v %v", c.name, report.Pulled, err)
		}
		var which []string
		if c.name == LeverCutBudget {
			which = c.args[:1]
		}
		release(t, s, c.name, which...)
		after := leversNow(t, s)
		after.Version = zero.Version
		if c.check(after) || after.SignedOnly || after.ProvenOnly || after.FreezeTransfers || after.PauseNewKeys || after.Tier4SharePPM != -1 || len(after.BudgetCutPPM) != 0 {
			t.Fatalf("%s released: %+v", c.name, after)
		}
		if _, err := s.ReleaseLever(testContext, c.name, which, "", ""); err == nil || !strings.Contains(err.Error(), "not pulled") {
			t.Fatalf("%s double release: %v", c.name, err)
		}
	}
	// block-prefix acts through PrefixBlocked.
	change := pull(t, s, LeverBlockPrefix, "203.0.113.0/24")
	src := s.leverSource()
	if b, _ := src.PrefixBlocked(testContext, s.db, "203.0.113.9", testTime); !b {
		t.Fatal("prefix not blocked")
	}
	release(t, s, LeverBlockPrefix, "203.0.113.0/24")
	if b, _ := src.PrefixBlocked(testContext, s.db, "203.0.113.9", testTime); b {
		t.Fatal("prefix still blocked")
	}
	log := leverLog(t, s)
	if len(log) != 14 {
		t.Fatalf("log has %d entries", len(log))
	}
	for i, e := range log {
		want := "release"
		if i%2 == 1 {
			want = "pull"
		}
		if e.Action != want || e.Actor != "steward" || e.Reason == "" || e.CreatedAt != testTime {
			t.Fatalf("log %d: %+v", i, e)
		}
	}
	if log[1].Args["id"] != float64(change.Args["id"].(int64)) || log[1].Args["bits"] != float64(24) || log[1].Args["keyed_hash"] == "" {
		t.Fatalf("block-prefix log: %+v", log[1])
	}
	// Levers never delete data: every row stays, released.
	var levers, prefixes int
	if err := s.db.QueryRow("SELECT (SELECT count(*) FROM levers WHERE state='released'),(SELECT count(*) FROM blocked_prefixes WHERE released_at>0)").Scan(&levers, &prefixes); err != nil || levers != 6 || prefixes != 1 {
		t.Fatalf("rows %d %d %v", levers, prefixes, err)
	}
}

// A lever pulled with --until stops acting at until and is logged as
// expired on the next write, by lever list, or by lever expire.
func TestLeverExpiry(t *testing.T) {
	s := openTest(t, Config{})
	until := testTime + 3600
	if _, err := s.PullLever(testContext, LeverPull{Name: LeverSignedOnly, Reason: "flood", Until: until}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PullLever(testContext, LeverPull{Name: LeverBlockPrefix, Args: []string{"2001:db8::/48"}, Reason: "flood", Until: until}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PullLever(testContext, LeverPull{Name: LeverFreezeTransfers, Reason: "hold"}); err != nil {
		t.Fatal(err)
	}
	fails(t, s, Command{Operation: "post", Room: "lobby", Text: "anonymous", RequestID: "a1"}, "signed_only")
	if _, err := s.Execute(testContext, Command{Operation: "post", Room: "lobby", Text: "anonymous", RequestID: "a2"}, "2001:db8::77"); errCode(err) != "signed_only" {
		t.Fatalf("v6: %v", err)
	}
	s.now = func() time.Time { return time.Unix(until, 0) }
	l := leversNow(t, s)
	if l.SignedOnly || !l.FreezeTransfers {
		t.Fatalf("at until: %+v", l)
	}
	if b, _ := s.leverSource().PrefixBlocked(testContext, s.db, "2001:db8::77", until); b {
		t.Fatal("prefix blocked at until")
	}
	run(t, s, Command{Operation: "post", Room: "lobby", Text: "anonymous", RequestID: "a3"})
	log := leverLog(t, s)
	if len(log) != 5 || log[0].Action != "expire" || log[1].Action != "expire" || log[0].CreatedAt != until {
		t.Fatalf("log: %+v", log)
	}
	if n, err := s.ExpireLevers(testContext); err != nil || n != 0 {
		t.Fatalf("expire again: %d %v", n, err)
	}
	report, err := s.LeverReport(testContext)
	if err != nil || len(report.Pulled) != 1 || report.Pulled[0] != LeverFreezeTransfers {
		t.Fatalf("report: %+v %v", report, err)
	}
	if pulled, version, err := s.ActiveLevers(testContext); err != nil || len(pulled) != 1 || pulled[0] != LeverFreezeTransfers || version != report.Version {
		t.Fatalf("active: %v %d %v", pulled, version, err)
	}
	for _, v := range report.Levers {
		if v.Name == LeverSignedOnly && (v.State != "expired" || v.Active || v.ReleasedAt != until) {
			t.Fatalf("expired lever: %+v", v)
		}
	}
	// Pulled again after expiry, it counts from the new pull.
	s.now = func() time.Time { return time.Unix(until+10, 0) }
	if _, err := s.PullLever(testContext, LeverPull{Name: LeverPauseNewKeys, Reason: "fresh keys"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PullLever(testContext, LeverPull{Name: LeverPauseNewKeys, Reason: "still fresh keys"}); err != nil {
		t.Fatal(err)
	}
	if l := leversNow(t, s); !l.PauseNewKeys || l.PauseNewKeysSince != until+10 {
		t.Fatalf("re-pull kept the first pull time? %+v", l)
	}
}

// signed-only refuses unsigned writes at admission; signed writes and every
// read go on. block-prefix refuses every write from the prefix.
func TestAdmitSignedOnlyAndBlockPrefix(t *testing.T) {
	s := openTest(t, Config{})
	alice := keyFor(31)
	pull(t, s, LeverSignedOnly)
	fails(t, s, Command{Operation: "post", Room: "lobby", Text: "anonymous", RequestID: "a1"}, "signed_only")
	run(t, s, signed(alice, Command{Operation: "post", Room: "lobby", Text: "signed", RequestID: "s1"}))
	run(t, s, Command{Operation: "messages.list", Room: "lobby"})
	run(t, s, Command{Operation: "quota.get"})
	release(t, s, LeverSignedOnly)
	run(t, s, Command{Operation: "post", Room: "lobby", Text: "anonymous", RequestID: "a2"})

	pull(t, s, LeverBlockPrefix, "198.51.100.0/24")
	for _, source := range []string{"198.51.100.7", "::ffff:198.51.100.7", "198.51.100.7:4431"} {
		if _, err := s.Execute(testContext, signed(alice, Command{Operation: "post", Room: "lobby", Text: "from " + source, RequestID: "b-" + source}), source); errCode(err) != "prefix_blocked" {
			t.Fatalf("%s: %v", source, err)
		}
		if _, err := s.Execute(testContext, Command{Operation: "messages.list", Room: "lobby"}, source); err != nil {
			t.Fatalf("read from %s: %v", source, err)
		}
	}
	for _, source := range []string{"198.51.101.7", "test-origin", "nostr-bridge", "", "2001:db8::1"} {
		if _, err := s.Execute(testContext, Command{Operation: "post", Room: "lobby", Text: "from " + source, RequestID: "ok-" + source}, source); err != nil {
			t.Fatalf("%q: %v", source, err)
		}
	}
	// The public view never shows the prefix.
	report, err := s.LeverReport(testContext)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(report)
	if strings.Contains(string(raw), "198.51") || len(report.BlockedPrefixes) != 1 || report.BlockedPrefixes[0].Bits != 24 || report.BlockedPrefixes[0].Family != "ipv4" || len(report.BlockedPrefixes[0].KeyedHash) != 32 {
		t.Fatalf("public report: %s", raw)
	}
}

func TestLeverPullValidation(t *testing.T) {
	s := openTest(t, Config{})
	for _, p := range []LeverPull{
		{Name: "nope", Reason: "x"},
		{Name: LeverSignedOnly},
		{Name: LeverSignedOnly, Reason: "   "},
		{Name: LeverSignedOnly, Reason: "bad\x00reason"},
		{Name: LeverSignedOnly, Reason: strings.Repeat("r", 501)},
		{Name: LeverSignedOnly, Reason: "x", Args: []string{"extra"}},
		{Name: LeverSignedOnly, Reason: "x", Until: testTime},
		{Name: LeverSignedOnly, Reason: "x", Until: testTime * 1000},
		{Name: LeverTier4Shrink, Reason: "x"},
		{Name: LeverTier4Shrink, Reason: "x", Args: []string{"1000001"}},
		{Name: LeverTier4Shrink, Reason: "x", Args: []string{"-1"}},
		{Name: LeverTier4Shrink, Reason: "x", Args: []string{"0.5"}},
		{Name: LeverCutBudget, Reason: "x", Args: []string{"post_bytes"}},
		{Name: LeverCutBudget, Reason: "x", Args: []string{"gold", "10"}},
		{Name: LeverCutBudget, Reason: "x", Args: []string{"post_bytes", "0"}},
		{Name: LeverBlockPrefix, Reason: "x", Args: []string{"198.51.100.7"}},
		{Name: LeverBlockPrefix, Reason: "x", Args: []string{"10.0.0.0/7"}},
		{Name: LeverBlockPrefix, Reason: "x", Args: []string{"2001::/15"}},
		{Name: LeverBlockPrefix, Reason: "blocking 198.51.100.0 today", Args: []string{"198.51.100.0/24"}},
		{Name: LeverSignedOnly, Reason: "x", Actor: strings.Repeat("a", 65)},
	} {
		if _, err := s.PullLever(testContext, p); err == nil {
			t.Fatalf("accepted %+v", p)
		}
	}
	if log := leverLog(t, s); len(log) != 0 {
		t.Fatalf("refused pulls logged: %+v", log)
	}
	// tier4-shrink 0 is a valid pull (tier 4 gets nothing), and so is a
	// mapped IPv4 prefix, stored as IPv4.
	pull(t, s, LeverTier4Shrink, "0")
	if l := leversNow(t, s); l.Tier4SharePPM != 0 {
		t.Fatalf("tier4 0: %+v", l)
	}
	pull(t, s, LeverBlockPrefix, "::ffff:192.0.2.0/120")
	if b, _ := s.leverSource().PrefixBlocked(testContext, s.db, "192.0.2.200", testTime); !b {
		t.Fatal("mapped prefix not blocked")
	}
	// Two budget cuts on two resources are two rows; releasing one keeps the other.
	pull(t, s, LeverCutBudget, "post_bytes", "100000")
	pull(t, s, LeverCutBudget, "memory_bytes", "200000")
	release(t, s, LeverCutBudget, "post_bytes")
	if l := leversNow(t, s); len(l.BudgetCutPPM) != 1 || l.BudgetCutPPM[allowance.MemoryBytes] != 200000 {
		t.Fatalf("cuts: %+v", l.BudgetCutPPM)
	}
	// The same prefix pulled twice is one blocked prefix, renewed.
	pull(t, s, LeverBlockPrefix, "192.0.2.0/24")
	report, _ := s.LeverReport(testContext)
	if len(report.BlockedPrefixes) != 1 {
		t.Fatalf("prefixes: %+v", report.BlockedPrefixes)
	}
	release(t, s, LeverBlockPrefix, "1")
}

// The lever cache follows changes made by another process (the CLI opens its
// own store on the same database).
func TestLeverCacheSeesOtherWriters(t *testing.T) {
	s := openTest(t, Config{})
	var seq int
	var name, path string
	if err := s.db.QueryRow("PRAGMA database_list").Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	other, err := Open(path, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	other.now = s.now
	if leversNow(t, s).SignedOnly {
		t.Fatal("pulled before")
	}
	if _, err := other.PullLever(testContext, LeverPull{Name: LeverSignedOnly, Reason: "other process"}); err != nil {
		t.Fatal(err)
	}
	if !leversNow(t, s).SignedOnly {
		t.Fatal("cache missed another writer's pull")
	}
	fails(t, s, Command{Operation: "post", Room: "lobby", Text: "anonymous", RequestID: "a1"}, "signed_only")
}
