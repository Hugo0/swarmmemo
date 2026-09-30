package board

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
)

var testDay = time.Unix(testTime, 0).UTC().Format("2006-01-02")

func TestClientNameIsBounded(t *testing.T) {
	for raw, want := range map[string]string{
		"Claude Code":                   "claude-code",
		"openai-mcp":                    "openai-mcp",
		"  My Agent!! v2 ":              "my-agent-v2",
		"../../etc/passwd":              "etc-passwd",
		"<script>alert(1)</script>":     "script-alert-1-script",
		"agent@203.0.113.9":             "agent-203.0.113.9",
		"Ünïcödé":                       "n-c-d",
		"":                              "",
		"!!!":                           "",
		strings.Repeat("abcdefghij", 9): "abcdefghijabcdefghijabcdefghijab",
	} {
		got := ClientName(raw)
		if got != want {
			t.Errorf("ClientName(%q) = %q, want %q", raw, got, want)
		}
		if len(got) > ClientNameMaxChars || !regexp.MustCompile(`^([a-z0-9][a-z0-9._-]*[a-z0-9]|[a-z0-9]?)$`).MatchString(got) {
			t.Errorf("ClientName(%q) = %q is outside the charset or length", raw, got)
		}
	}
}

func TestAddClientCountsRefusesIdentifiers(t *testing.T) {
	s := openTest(t, Config{})
	for _, bad := range []map[string]int64{
		{"family:claude:203.0.113.9": 1}, {"family:Mozilla/5.0:discovery": 1}, {"family:unknown:discovery": 1},
		{"family:claude:service:../x": 1}, {"family:claude:service:" + strings.Repeat("a", 49): 1},
		{"name:Claude Code:initialize": 1}, {"name::initialize": 1}, {"name:ok:ip": 1}, {"name:ok:initialize": 1}, {"name:ok:subjects": 1},
		{"unknown_mcp_clients:ok": 1}, {"family:claude:discovery": -1},
		{"key:" + strings.Repeat("a", 64): 1},
	} {
		if s.AddClientCounts(testContext, testDay, bad) == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	if s.AddClientCounts(testContext, "2026-9-5", map[string]int64{"family:claude:discovery": 1}) == nil {
		t.Error("accepted a malformed day")
	}
}

func TestUnknownClientNamesCountedNotStored(t *testing.T) {
	s := openTest(t, Config{})
	s.CountClient("other-mcp", "mcp_initialize", "Rare Agent")
	s.CountClient("other-mcp", "mcp_initialize", "rare-agent") // the same name, reduced
	s.CountClient("claude", "mcp_initialize", "mcp-remote")    // a family from the User-Agent, an unknown name
	s.CountClient("other-mcp", "mcp_initialize", "!!!")        // nothing usable
	s.CountClient("other-mcp", "discovery", "not-an-initialize")
	// Anything but the two request metrics, or an unknown family, is ignored.
	s.CountClient("other-mcp", "new_keys", "")
	s.CountClient("martian", "discovery", "")
	s.FlushClientCounts()
	days, err := s.ReadClientStats(testContext, time.Unix(testTime, 0), 1)
	if err != nil {
		t.Fatal(err)
	}
	d := days[0]
	if d.UnknownMCPClients != 2 || len(d.Families) != 2 || d.Families["other-mcp"].Counts["mcp_initialize"] != 3 || d.Families["other-mcp"].Counts["discovery"] != 1 || d.Families["claude"].Counts["mcp_initialize"] != 1 {
		t.Fatalf("day: %+v", d)
	}
	s.clients.mu.Lock()
	held := map[string]int64{}
	for name, n := range s.clients.names {
		held[name] = n
	}
	s.clients.mu.Unlock()
	if len(held) != 2 || held["rare-agent"] != 2 || held["mcp-remote"] != 1 {
		t.Fatalf("names in memory: %v", held)
	}
	// What the operator's log gets at the day's end: the most frequent,
	// reduced, bounded.
	many := map[string]int64{}
	for i := 0; i < clientNamesLogged+5; i++ {
		many[fmt.Sprintf("agent-%02d", i)] = int64(i)
	}
	top := strings.Split(topClientNames(many), ", ")
	if len(top) != clientNamesLogged || top[0] != fmt.Sprintf("agent-%02d %d", clientNamesLogged+4, clientNamesLogged+4) {
		t.Fatalf("top names: %v", top)
	}
}

// rawCounts is the stored counts of the test day, unshaped: what the store
// counted, before ReadClientStats decides what is published.
func rawCounts(t *testing.T, s *Store, family string) map[string]int64 {
	t.Helper()
	s.FlushClientCounts()
	_, raw, err := s.readClientCounts(testContext, time.Unix(testTime, 0), 1)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for key, n := range raw[0] {
		if metric, ok := strings.CutPrefix(key, "family:"+family+":"); ok {
			out[metric] = n
		}
	}
	return out
}

func TestClientCommandCounts(t *testing.T) {
	s := openTest(t, Config{})
	ctx := WithClientFamily(testContext, "claude")
	exec := func(c Command, source string) {
		t.Helper()
		if _, err := s.Execute(ctx, c, source); err != nil {
			t.Fatalf("%s: %v", c.Operation, err)
		}
	}
	metrics := func(family string) map[string]int64 { return rawCounts(t, s, family) }
	// Keys that last wrote 1 and 7 days ago (their identity's last_seen is
	// what a return is read from) write today for the first time since; a
	// new key posts for the first time.
	key := keyFor(71)
	for i, back := range []int64{1, 7} {
		at := testTime - back*86400 + 60
		s.now = func() time.Time { return time.Unix(at, 0) }
		exec(signed(keyFor(byte(73+i)), Command{Operation: "room.create", Room: fmt.Sprintf("back-%d", back), Timestamp: at}), "203.0.113.8")
	}
	s.now = func() time.Time { return time.Unix(testTime, 0) }
	for i := range []int64{1, 7} {
		exec(signed(keyFor(byte(73+i)), Command{Operation: "room.create", Room: fmt.Sprintf("today-%d", i)}), "203.0.113.8")
		exec(signed(keyFor(byte(73+i)), Command{Operation: "room.create", Room: fmt.Sprintf("again-%d", i)}), "203.0.113.8")
	}
	exec(signed(key, Command{Operation: "post", Room: "lobby", Text: "first signed post"}), "203.0.113.9")
	exec(signed(key, Command{Operation: "post", Room: "lobby", Text: "second signed post"}), "203.0.113.9")
	exec(signed(key, Command{Operation: "messages.list", Room: "lobby"}), "203.0.113.9")
	got := metrics("claude")
	want := map[string]int64{"new_keys": 1, "first_posts": 1, "returning_1d": 1, "returning_7d": 1}
	for _, m := range ClientMetrics {
		if got[m] != want[m] {
			t.Fatalf("after signed posts %s = %d, want %d (%v)", m, got[m], want[m], got)
		}
	}
	// An anonymous caller: counted once a day as a subject, its first post once.
	exec(Command{Operation: "messages.list", Room: "lobby"}, "198.51.100.7")
	exec(Command{Operation: "post", Room: "lobby", Text: "anonymous hello"}, "198.51.100.7")
	exec(Command{Operation: "post", Room: "lobby", Text: "anonymous again"}, "198.51.100.7")
	// Internal sources are not callers.
	exec(Command{Operation: "messages.list", Room: "lobby"}, "web-public-read")
	got = metrics("claude")
	if got["anonymous_subjects"] != 1 || got["first_posts"] != 2 || got["new_keys"] != 1 {
		t.Fatalf("after anonymous posts: %v", got)
	}
	// An IPv6 caller is one subject, its /64, whichever pseudonym (the /48
	// of a service call, the /64 otherwise) and host it uses.
	for i, account := range []string{"anon:v6-48", "anon:v6-64", "anon:v6-64"} {
		s.countClientCommand(ctx, actor{account: account}, Command{Operation: "messages.list"}, fmt.Sprintf("2001:db8:1:2::%d", i+1), false, 0, testTime)
	}
	if got = metrics("claude"); got["anonymous_subjects"] != 2 {
		t.Fatalf("an IPv6 caller counted more than once: %v", got)
	}
	// Service calls, in total and by service.
	a := actor{account: "anon:x"}
	for _, c := range []Command{{Operation: "service.call", Target: "inference"}, {Operation: "service.read", Target: "inference"}, {Operation: "service.call", Target: "Bad/Service"}} {
		s.countClientCommand(ctx, a, c, "198.51.100.7", false, 0, testTime)
	}
	s.FlushClientCounts()
	days, _ := s.ReadClientStats(testContext, time.Unix(testTime, 0), 1)
	if f := days[0].Families["claude"]; len(f.Counts) != 0 || len(f.Services) != 0 {
		t.Fatalf("today's service calls were published: %+v", f)
	}
	if got = metrics("claude"); got["service_calls"] != 3 || got["service:inference"] != 2 || len(got) != 7 {
		t.Fatalf("service calls: %v", got)
	}
	// A restart forgets today's subjects: the lookups stay exact, so the key
	// is not new, returning or posting for the first time again.
	s.clients.mu.Lock()
	s.clients.day = ""
	s.clients.mu.Unlock()
	exec(signed(key, Command{Operation: "post", Room: "lobby", Text: "after restart"}), "203.0.113.9")
	exec(Command{Operation: "post", Room: "lobby", Text: "anonymous after restart"}, "198.51.100.7")
	exec(signed(keyFor(73), Command{Operation: "room.create", Room: "after-restart"}), "203.0.113.8")
	if got = metrics("claude"); got["first_posts"] != 2 || got["new_keys"] != 1 || got["returning_1d"] != 1 || got["returning_7d"] != 1 {
		t.Fatalf("after a restart: %v", got)
	}
	// No family in the context: not counted.
	before := metrics("claude")
	if _, err := s.Execute(testContext, Command{Operation: "post", Room: "lobby", Text: "unclassified"}, "192.0.2.50"); err != nil {
		t.Fatal(err)
	}
	after := metrics("claude")
	for _, m := range ClientMetrics {
		if before[m] != after[m] {
			t.Fatalf("an unclassified command was counted: %v -> %v", before, after)
		}
	}
	// Other families are separate.
	if _, err := s.Execute(WithClientFamily(testContext, "scripts"), Command{Operation: "messages.list", Room: "lobby"}, "192.0.2.60"); err != nil {
		t.Fatal(err)
	}
	if got = metrics("scripts"); got["anonymous_subjects"] != 1 {
		t.Fatalf("scripts: %v", got)
	}
}

func TestClientCountsStoreNoIdentifiers(t *testing.T) {
	s := openTest(t, Config{})
	key := keyFor(72)
	pub := base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
	ctx := WithClientFamily(testContext, "openai")
	const ip = "203.0.113.99"
	if _, err := s.Execute(ctx, signed(key, Command{Operation: "post", Room: "lobby", Text: "hi"}), ip); err != nil {
		t.Fatal(err)
	}
	res, err := s.Execute(ctx, Command{Operation: "post", Room: "lobby", Text: "anon"}, ip)
	if err != nil {
		t.Fatal(err)
	}
	s.CountClient("other-mcp", "mcp_initialize", "Secret Agent UA/9.9")
	s.CountClient("other-mcp", "mcp_initialize", "Secret Agent UA/9.9")
	s.FlushClientCounts()
	var author string
	_ = s.db.QueryRow("SELECT account FROM events WHERE id=?", res.Receipt.ID).Scan(&author)
	shape := regexp.MustCompile(`^client:[0-9]{4}-[0-9]{2}-[0-9]{2}:(family:[a-z-]+:([a-z0-9_]+|service:[a-z0-9_-]+)|unknown_mcp_clients)$`)
	rows, err := s.db.Query("SELECT scope FROM counters WHERE scope LIKE 'client:%'")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var scope string
		_ = rows.Scan(&scope)
		n++
		if !shape.MatchString(scope) {
			t.Fatalf("unexpected client scope %q", scope)
		}
		for _, secret := range []string{ip, "203.0.113", pub, keyID(key), author, "Secret Agent", "secret-agent", "UA/9.9"} {
			if secret != "" && strings.Contains(scope, secret) {
				t.Fatalf("scope %q holds an identifier %q", scope, secret)
			}
		}
	}
	if n == 0 {
		t.Fatal("nothing was stored")
	}
	var version int
	_ = s.db.QueryRow("PRAGMA user_version").Scan(&version)
	if version != SchemaVersion {
		t.Fatalf("client counters must not change the schema version: %d", version)
	}
}

// Whatever a client calls itself, the reduced name is short, in the charset
// and stable when reduced again, and never a key AddClientCounts accepts.
func FuzzClientName(f *testing.F) {
	for _, seed := range []string{"Claude Code", "openai-mcp", "../..", "a:b:c", "\x00\xff", strings.Repeat("é", 80)} {
		f.Add(seed)
	}
	charset := regexp.MustCompile(`^[a-z0-9._-]*$`)
	f.Fuzz(func(t *testing.T, raw string) {
		name := ClientName(raw)
		if len(name) > ClientNameMaxChars || !charset.MatchString(name) || ClientName(name) != name {
			t.Fatalf("ClientName(%q) = %q", raw, name)
		}
		if validClientKey("name:"+name+":initialize") || validClientKey(name) && name != "unknown_mcp_clients" {
			t.Fatalf("ClientName(%q) = %q is accepted as a key", raw, name)
		}
	})
}
