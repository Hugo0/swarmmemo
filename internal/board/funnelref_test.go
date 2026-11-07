package board

import (
	"context"
	"crypto/ed25519"
	"testing"
	"time"
)

// funnelEntry flushes and recomputes the funnel and answers an account's
// stored entry and entry_ref.
func funnelEntry(t *testing.T, s *Store, account string) (entry, ref string) {
	t.Helper()
	if err := s.FlushFunnel(testContext); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FunnelRecompute(testContext); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT entry,entry_ref FROM agent_funnel WHERE account=?", account).Scan(&entry, &ref); err != nil {
		t.Fatalf("agent_funnel %s: %v", account, err)
	}
	return entry, ref
}

func refPost(t *testing.T, s *Store, key ed25519.PrivateKey, ref string) {
	t.Helper()
	ctx := WithVia(context.Background(), "command")
	if ref != "" {
		ctx = WithFunnelRef(ctx, ref)
	}
	c := signed(key, Command{Operation: "post", Room: "lobby", Text: "hello from a referred agent" + testPostSuffix(), RequestID: randomID(), Timestamp: s.now().Unix()})
	if _, err := s.Execute(ctx, c, "198.51.100.41"); err != nil {
		t.Fatal(err)
	}
}

// C469: a ref the first act carries is the account's entry; a bad one is
// ignored; a later different one never replaces it; a later day's never
// counts.
func TestFunnelRefEntry(t *testing.T) {
	s := openTest(t, Config{})
	s.funnel.lastFlush = time.Unix(1<<40, 0) // the test flushes
	base := testTime - testTime%86400 + 3600
	s.now = func() time.Time { return time.Unix(base, 0) }

	good, bad, later, nextDay := keyFor(201), keyFor(202), keyFor(203), keyFor(204)
	refPost(t, s, good, "skitter-colony")
	refPost(t, s, good, "skitter-agentboard") // same day: the first wins
	refPost(t, s, bad, "Skitter_Colony!")
	refPost(t, s, later, "")
	refPost(t, s, later, "skitter-colony") // same first day, no ref yet: credited
	refPost(t, s, nextDay, "")
	s.now = func() time.Time { return time.Unix(base+86400, 0) }
	refPost(t, s, nextDay, "skitter-colony") // a later day: never an entry
	refPost(t, s, good, "skitter-other")

	if e, r := funnelEntry(t, s, keyID(good)); e != "ref:skitter-colony" || r != "skitter-colony" {
		t.Fatalf("good: entry %q ref %q", e, r)
	}
	if e, r := funnelEntry(t, s, keyID(bad)); r != "" || e == "ref:Skitter_Colony!" {
		t.Fatalf("bad ref stored: entry %q ref %q", e, r)
	}
	if e, r := funnelEntry(t, s, keyID(later)); e != "ref:skitter-colony" || r != "skitter-colony" {
		t.Fatalf("later same day: entry %q ref %q", e, r)
	}
	if e, r := funnelEntry(t, s, keyID(nextDay)); r != "" {
		t.Fatalf("next day credited: entry %q ref %q", e, r)
	}
	var stray int
	if err := s.db.QueryRow("SELECT count(*) FROM agent_days WHERE ref<>'' AND day<>?", base/86400).Scan(&stray); err != nil || stray != 0 {
		t.Fatalf("refs stored past the first day: %d %v", stray, err)
	}
}

// A hosted identity created with a ref (an MCP URL's) has it as its entry.
func TestFunnelRefHostedCreate(t *testing.T) {
	s := openHostedTest(t)
	s.funnel.lastFlush = time.Unix(1<<40, 0)
	res, err := s.Execute(WithFunnelRef(mcpViaForTest(), "skitter-colony"), Command{Operation: "hosted.create"}, "198.51.100.42")
	if err != nil {
		t.Fatal(err)
	}
	agent, _ := res.Data["agent"].(string)
	if e, r := funnelEntry(t, s, agent); e != "ref:skitter-colony" || r != "skitter-colony" {
		t.Fatalf("hosted: entry %q ref %q", e, r)
	}
}

func mcpViaForTest() context.Context { return WithVia(context.Background(), "mcp") }
