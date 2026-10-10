package board

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/trust"
)

// writeAt runs c from source with the HTTP signals an adapter would record.
func writeAt(t *testing.T, s *Store, source, via, ua string, c Command) (Result, error) {
	t.Helper()
	ctx := WithVia(context.Background(), via)
	ctx = WithRequestSignals(ctx, RequestSignals{UserAgent: ua, Referer: "https://Site.Example/page/x?text=secret#frag", AcceptLanguage: "en-GB,en;q=0.9", ClientHints: `Sec-CH-UA="Chromium";v="130"`})
	return s.Execute(ctx, c, source)
}

func signalRows(t *testing.T, s *Store) []WriteSignal {
	t.Helper()
	rows, err := s.db.Query("SELECT " + writeSignalColumns + " FROM write_signals ORDER BY seq")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []WriteSignal
	for rows.Next() {
		w, err := scanWriteSignal(rows)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, w)
	}
	return out
}

// Every accepted write leaves one row, whatever its operation; reads, refused
// writes and exact retries leave none. Header values are cleaned and bounded,
// the Referer loses its query, and the address is stored only as keyed hashes
// of itself and of its /24 (IPv4) or /48 (IPv6).
func TestWriteSignalsPerOperation(t *testing.T) {
	s := openTest(t, Config{})
	alice, bob := keyFor(31), keyFor(32)
	ua := "agent-x/1.0 \x1b[31mred\x1b[0m " + strings.Repeat("u", 400)
	post := signed(alice, Command{Operation: "post", Room: "lobby", Text: "hello", RequestID: "p1"})
	res, err := writeAt(t, s, "198.51.100.7", "command", ua, post)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = writeAt(t, s, "198.51.100.7", "command", ua, post); err != nil { // exact retry
		t.Fatal(err)
	}
	if _, err = writeAt(t, s, "198.51.100.9", "post", "other/2", signed(bob, Command{Operation: "vote", MessageID: res.Receipt.ID, Data: `{"value":1}`, RequestID: "v1"})); err != nil {
		t.Fatal(err)
	}
	if _, err = writeAt(t, s, "2001:db8:1:2::5", "command", "reg/1", signed(bob, Command{Operation: "agent.register", Handle: "bob"})); err != nil {
		t.Fatal(err)
	}
	if _, err = writeAt(t, s, "2001:db8:1:ff::9", "get", "anon/1", Command{Operation: "post", Room: "lobby", Text: "anonymous", RequestID: "a1"}); err != nil {
		t.Fatal(err)
	}
	if _, err = writeAt(t, s, "198.51.100.7", "command", ua, Command{Operation: "messages.list", Room: "lobby"}); err != nil { // a read
		t.Fatal(err)
	}
	if _, err = writeAt(t, s, "198.51.100.7", "command", ua, signed(alice, Command{Operation: "vote", MessageID: res.Receipt.ID, Data: `{"value":1}`, RequestID: "self"})); errCode(err) != "self_vote" {
		t.Fatalf("self vote: %v", err)
	}
	rows := signalRows(t, s)
	if len(rows) != 4 {
		t.Fatalf("%d rows, want 4 (post, vote, register, anonymous post): %+v", len(rows), rows)
	}
	p, v, r, a := rows[0], rows[1], rows[2], rows[3]
	if p.Operation != "post" || p.ObjectID != res.Receipt.ID || p.Account != keyID(alice) || p.Signer != keyID(alice) || p.Via != "command" || p.RequestID != "p1" || p.HashKey != s.SignalsKeyID() {
		t.Fatalf("post row %+v", p)
	}
	if strings.ContainsAny(p.UserAgent, "\x1b\x00") || len(p.UserAgent) > SignalTextBytes || !strings.HasPrefix(p.UserAgent, "agent-x/1.0 [31mred") {
		t.Fatalf("user agent not cleaned: %q", p.UserAgent)
	}
	if p.Referer != "https://site.example/page/x" || p.AcceptLanguage != "en-GB,en;q=0.9" || !strings.Contains(p.SecCHUA, "Chromium") {
		t.Fatalf("headers %+v", p)
	}
	if v.Operation != "vote" || v.Account != keyID(bob) || v.Via != "post" || v.IPHash24 != p.IPHash24 || v.IPHashFull == p.IPHashFull || len(p.IPHashFull) != 32 {
		t.Fatalf("same /24, other address: %+v vs %+v", v, p)
	}
	if r.Operation != "agent.register" || a.Account != "" || a.Signer != "" || a.ObjectID == "" || a.IPHash24 != r.IPHash24 || a.IPHashFull == r.IPHashFull {
		t.Fatalf("IPv6 /48 and the anonymous row: %+v %+v", r, a)
	}
	for _, w := range rows {
		for _, raw := range []string{"198.51.100", "2001:db8"} {
			if strings.Contains(w.IPHashFull+w.IPHash24, raw) {
				t.Fatal("a raw address was stored")
			}
		}
	}
}

// Other wires: a Nostr reissue keeps its relay and is "nostr"; a source that
// is not an address has no network hash; an email bridge's domain is kept.
func TestWriteSignalsOtherTransports(t *testing.T) {
	s := openTest(t, Config{})
	ctx := WithForwarded(context.Background(), Forwarded{Mode: "reissued", OriginService: "nostr", OriginID: strings.Repeat("a", 64), OriginAuthor: "npub1abc", OriginRef: "nostr:nevent1abc"})
	ctx = WithRequestSignals(ctx, RequestSignals{Origin: "relay:wss://relay.example"})
	if _, err := s.Execute(ctx, Command{Operation: "post", Room: "lobby", Text: "from nostr", RequestID: "nostr:" + strings.Repeat("a", 64)}, "nostr:abcdef"); err != nil {
		t.Fatal(err)
	}
	ctx = WithRequestSignals(WithVia(context.Background(), "email"), RequestSignals{Origin: "email:mail.example"})
	if _, err := s.Execute(ctx, signed(keyFor(33), Command{Operation: "post", Room: "lobby", Text: "by mail", RequestID: "m1"}), "192.0.2.1"); err != nil {
		t.Fatal(err)
	}
	ctx = WithVia(context.Background(), "dns")
	if _, err := s.Execute(ctx, signed(keyFor(34), Command{Operation: "post", Room: "lobby", Text: "by dns", RequestID: "d1"}), "8.8.8.8"); err != nil {
		t.Fatal(err)
	}
	rows := signalRows(t, s)
	if len(rows) != 3 {
		t.Fatalf("%d rows", len(rows))
	}
	if n := rows[0]; n.Via != "nostr" || n.Origin != "relay:wss://relay.example" || n.IPHashFull != "" || n.IPHash24 != "" {
		t.Fatalf("nostr row %+v", n)
	}
	if m := rows[1]; m.Via != "email" || m.Origin != "email:mail.example" || m.IPHash24 == "" {
		t.Fatalf("email row %+v", m)
	}
	if d := rows[2]; d.Via != "dns" || d.IPHash24 == "" || d.UserAgent != "" {
		t.Fatalf("dns row %+v", d)
	}
}

// Rows older than the retention are deleted, newer ones kept.
func TestWriteSignalsRetention(t *testing.T) {
	s := openTest(t, Config{})
	base := s.now
	s.now = func() time.Time { return base().Add(-(WriteSignalsRetentionDays*24 + 1) * time.Hour) }
	if _, err := writeAt(t, s, "198.51.100.7", "command", "old/1", signed(keyFor(35), Command{Operation: "post", Room: "lobby", Text: "old", RequestID: "o1", Timestamp: s.now().Unix()})); err != nil {
		t.Fatal(err)
	}
	s.now = base
	if _, err := writeAt(t, s, "198.51.100.7", "command", "new/1", signed(keyFor(35), Command{Operation: "post", Room: "lobby", Text: "new", RequestID: "n1"})); err != nil {
		t.Fatal(err)
	}
	n, err := s.PruneWriteSignals(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	if rows := signalRows(t, s); len(rows) != 1 || rows[0].UserAgent != "new/1" {
		t.Fatalf("kept %+v", rows)
	}
	var posts int
	if err = s.db.QueryRow("SELECT count(*) FROM events WHERE text IN ('old','new')").Scan(&posts); err != nil || posts != 2 {
		t.Fatalf("pruning signals touched posts: %d %v", posts, err)
	}
}

// The sybil-ring view: two accounts on one network with one client are a
// cluster, the third is not; the trust run's links name exactly that pair.
func TestWriteSignalsClustersAndLinks(t *testing.T) {
	s := openTest(t, Config{})
	a, b, c := keyFor(36), keyFor(37), keyFor(38)
	for i, w := range []struct {
		seed       byte
		source, ua string
	}{{36, "203.0.113.4", "ring/1"}, {37, "203.0.113.99", "ring/1"}, {38, "203.0.113.5", "honest/1"}, {36, "192.0.2.50", "ring/1"}, {38, "192.0.2.50", "honest/1"}} {
		key := keyFor(w.seed)
		if _, err := writeAt(t, s, w.source, "command", w.ua, signed(key, Command{Operation: "post", Room: "lobby", Text: "x", RequestID: "r" + string(rune('a'+i))})); err != nil {
			t.Fatal(err)
		}
	}
	clusters, err := s.SignalClusters(context.Background(), 30, 2)
	if err != nil || len(clusters) != 1 || len(clusters[0].Accounts) != 2 || clusters[0].UserAgent != "ring/1" {
		t.Fatalf("clusters %+v %v", clusters, err)
	}
	v, err := s.SignalAccount(context.Background(), keyID(a), 30)
	if err != nil || v.Writes != 2 || len(v.Fingerprints) != 2 {
		t.Fatalf("view %+v %v", v, err)
	}
	if shared := v.Fingerprints[0].Shared; len(shared) != 1 || shared[0].Account != keyID(b) {
		t.Fatalf("shared %+v", v.Fingerprints)
	}
	if len(v.SameAddress) != 1 || v.SameAddress[0].Account != keyID(c) {
		t.Fatalf("same address %+v", v.SameAddress)
	}
	w, ok, err := s.SignalMessage(context.Background(), "nope")
	if ok || err != nil {
		t.Fatalf("missing message %+v %v", w, err)
	}
	var links []trust.Record
	if err = (trustInputs{s}).readSignalLinks(context.Background(), s.now().Unix()+1, 30, func(r trust.Record) error { links = append(links, r); return nil }); err != nil {
		t.Fatal(err)
	}
	ab := []string{keyID(a), keyID(b)}
	if ab[0] > ab[1] {
		ab[0], ab[1] = ab[1], ab[0]
	}
	if len(links) != 1 || links[0].Type != "signal_link" || links[0].Account != ab[0] || links[0].LinkAccount != ab[1] {
		t.Fatalf("links %+v", links)
	}
}

// Nothing public carries a write signal: every serialized read, the export,
// the trust inputs and the stats leave out the canary values, and no file
// outside signals.go names the table or its hash columns.
func TestWriteSignalsNeverPublic(t *testing.T) {
	s := openTest(t, Config{})
	const canaryUA, canaryLang = "canary-agent-c160/9.9", "x-canary-c160"
	alice := keyFor(39)
	ctx := WithRequestSignals(WithVia(context.Background(), "command"), RequestSignals{UserAgent: canaryUA, AcceptLanguage: canaryLang, Referer: "https://canary-c160.example/x"})
	res, err := s.Execute(ctx, signed(alice, Command{Operation: "post", Room: "lobby", Text: "public words", RequestID: "c1"}), "198.51.100.77")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Execute(ctx, signed(alice, Command{Operation: "agent.register", Handle: "canary"}), "198.51.100.77"); err != nil {
		t.Fatal(err)
	}
	var blobs [][]byte
	keep := func(v any) {
		b, _ := json.Marshal(v)
		blobs = append(blobs, b)
	}
	keep(res)
	for _, c := range []Command{{Operation: "messages.list", Room: "lobby"}, {Operation: "message.get", MessageID: res.Receipt.ID}, {Operation: "thread.get", MessageID: res.Receipt.ID},
		{Operation: "agent.get", Target: keyID(alice)}, {Operation: "agents.list"}, {Operation: "agent.posts", Target: keyID(alice)}, {Operation: "export"}, {Operation: "rooms.list"}} {
		r, err := s.Execute(context.Background(), c, "198.51.100.78")
		if err != nil {
			t.Fatalf("%s: %v", c.Operation, err)
		}
		keep(r)
	}
	journal, err := s.Execute(context.Background(), signed(alice, Command{Operation: "journal.get"}), "198.51.100.77")
	if err != nil {
		t.Fatal(err)
	}
	keep(journal)
	var inputs bytes.Buffer
	if err = s.WriteTrustInputs(context.Background(), &inputs); err != nil {
		t.Fatal(err)
	}
	blobs = append(blobs, inputs.Bytes())
	stats, err := s.ReadClientStats(context.Background(), s.now(), 2)
	if err != nil {
		t.Fatal(err)
	}
	keep(stats)
	for _, b := range blobs {
		for _, canary := range []string{canaryUA, canaryLang, "canary-c160", "ip_hash", "write_signal", s.SignalsKeyID()} {
			if bytes.Contains(b, []byte(canary)) {
				t.Fatalf("%q in a public output: %.300s", canary, b)
			}
		}
	}
	// Structural: only signals.go (and tests, and the schema pin) name the
	// table or its hash columns, so no other code can serve them.
	for _, dir := range []string{"..", "../../cmd"} {
		err = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || filepath.Base(path) == "signals.go" && filepath.Base(filepath.Dir(path)) == "board" {
				return err
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, name := range []string{"write_signals", "ip_hash_full", "ip_hash_24"} {
				if bytes.Contains(src, []byte(name)) {
					t.Errorf("%s names %s: only board/signals.go may", path, name)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// The key is created 0600 beside the database, survives a reopen, and a key
// file others can read stops startup.
func TestSignalsKeyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "board.sqlite")
	s, err := Open(path, Config{})
	if err != nil {
		t.Fatal(err)
	}
	id := s.SignalsKeyID()
	s.Close()
	info, err := os.Stat(filepath.Join(dir, SignalsKeyFileName))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key file %v %v", info, err)
	}
	if s, err = Open(path, Config{}); err != nil || s.SignalsKeyID() != id || len(id) != 16 {
		t.Fatalf("reopen: %v, id %s → %s", err, id, s.SignalsKeyID())
	}
	s.Close()
	if err = os.Chmod(filepath.Join(dir, SignalsKeyFileName), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err = Open(path, Config{}); err == nil || !strings.Contains(err.Error(), "readable by others") {
		t.Fatalf("a world-readable key opened: %v", err)
	}
}

func TestSignalReferer(t *testing.T) {
	for in, want := range map[string]string{
		"https://Ex.com/a/b?q=1#f": "https://ex.com/a/b",
		"http://x.example":         "http://x.example",
		"/relative/only":           "",
		"not a url":                "",
		"":                         "",
	} {
		if got := SignalReferer(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
