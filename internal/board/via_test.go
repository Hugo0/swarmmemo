package board

import (
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func viaRun(t *testing.T, s *Store, via string, c Command) Result {
	t.Helper()
	r, err := s.Execute(WithVia(testContext, via), c, "test-origin")
	if err != nil {
		t.Fatalf("%s via %s: %v", c.Operation, via, err)
	}
	return r
}

func viaFails(t *testing.T, s *Store, via string, c Command, code string) {
	t.Helper()
	_, err := s.Execute(WithVia(testContext, via), c, "test-origin")
	var e *Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("via %s: want %s, got %v", via, code, err)
	}
}

// The registry is the single source: every value is a short token with a
// label and a carrier, groups name only real values, and the PROTOCOL list is
// generated from it (TestGeneratedProtocolSections).
func TestViaRegistry(t *testing.T) {
	token := regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,15}$`)
	seen := map[string]bool{}
	for _, v := range Vias() {
		if !token.MatchString(v.Name) || v.Label == "" || v.Carrier == "" || seen[v.Name] {
			t.Errorf("bad or repeated via %+v", v)
		}
		seen[v.Name] = true
	}
	for _, want := range []string{"ui", "get", "post", "put", "mkcol", "x-text", "c64", "command", "mcp", "dns", "tcp", "gemini", "email", "nostr"} {
		if !seen[want] {
			t.Errorf("via %q is missing", want)
		}
	}
	for name, members := range ViaGroups() {
		if seen[name] {
			t.Errorf("group %q shadows a via", name)
		}
		for _, m := range members {
			if !seen[m] {
				t.Errorf("group %q names unknown via %q", name, m)
			}
		}
	}
	if !BridgeVia("email") || BridgeVia("nostr") || BridgeVia("dns") || BridgeVia("nope") {
		t.Error("bridge values")
	}
	if ViaFrom(WithVia(context.Background(), "carrier-pigeon")) != "" || ViaFrom(context.Background()) != "" {
		t.Error("an unknown or missing via must read as none")
	}
}

// via is stored from the adapter's context, never from the command, and
// comes back on every read and in the export.
func TestPostRecordsVia(t *testing.T) {
	s := openTest(t, Config{ArchiveDelaySeconds: -1})
	key := keyFor(230)
	dns := viaRun(t, s, "dns", signed(key, Command{Operation: "post", Text: "over a resolver"})).Receipt.ID
	none := run(t, s, Command{Operation: "post", Text: "no adapter"}).Receipt.ID
	odd := viaRun(t, s, "carrier-pigeon", Command{Operation: "post", Text: "unknown"}).Receipt.ID
	if m := get(t, s, dns); m.Via != "dns" {
		t.Fatalf("stored via %q", m.Via)
	}
	for _, id := range []string{none, odd} {
		if m := get(t, s, id); m.Via != "" {
			t.Fatalf("%s stored via %q", id, m.Via)
		}
	}
	// The command cannot carry it: there is no such field.
	for _, field := range CommandFields() {
		if field == "via" {
			t.Fatal("via became a command field")
		}
	}
	if op, _ := LookupOperation("post"); strings.Contains(" "+op.Fields+" ", " via ") {
		t.Fatal("post accepts a via field")
	}
	export := run(t, s, Command{Operation: "export", Limit: 10})
	found := false
	for _, m := range export.Messages {
		if m.ID == dns {
			found = m.Via == "dns"
		}
	}
	if !found {
		t.Fatalf("export lost via: %+v", export.Messages)
	}
	// A tombstone keeps how it arrived; only the body goes.
	if err := s.Moderate(testContext, dns, "test", true); err != nil {
		t.Fatal(err)
	}
	if m := get(t, s, dns); m.Via != "dns" || m.Text != "" {
		t.Fatalf("tombstone: %+v", m)
	}
}

// write_via binds top-level posts and replies, for everyone including the
// owner; groups expand; a post whose adapter recorded nothing is refused;
// edits are never frozen; reads are untouched.
func TestWriteViaPolicy(t *testing.T) {
	s := openTest(t, Config{})
	owner, writer := keyFor(231), keyFor(232)
	register(t, s, writer)
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "wires"}))
	article := viaRun(t, s, "ui", signed(writer, Command{Operation: "post", Room: "wires", Text: "# One", Data: dataJSON(`"format":"markdown"`)})).Receipt.ID
	for _, bad := range []string{`{"write_via":["dns","dns"]}`, `{"write_via":["carrier-pigeon"]}`, `{"write_via":"dns"}`, `{"write_via":[1]}`, `{"write_via":{"dns":true}}`} {
		fails(t, s, policySet(owner, "wires", bad), "invalid_policy")
	}
	run(t, s, policySet(owner, "wires", `{"write_via":["dns","tcp"]}`))
	if p := roomGet(t, s, "wires").Policy; strings.Join(p.WriteVia, ",") != "dns,tcp" || p.Write != "open" {
		t.Fatalf("policy %+v", p)
	}
	for _, via := range []string{"ui", "get", "command", "mcp", "", "email"} {
		viaFails(t, s, via, Command{Operation: "post", Room: "wires", Text: "no"}, "room_via_restricted")
		viaFails(t, s, via, signed(owner, Command{Operation: "post", Room: "wires", Text: "owner too"}), "room_via_restricted")
		viaFails(t, s, via, signed(writer, Command{Operation: "post", Room: "wires", Text: "reply", ReplyTo: article}), "room_via_restricted")
	}
	before := used(t, s, writer)
	viaFails(t, s, "get", signed(writer, Command{Operation: "post", Room: "wires", Text: "costs nothing"}), "room_via_restricted")
	if used(t, s, writer) != before {
		t.Fatal("a refused post was charged")
	}
	viaRun(t, s, "dns", signed(writer, Command{Operation: "post", Room: "wires", Text: "resolver"}))
	viaRun(t, s, "tcp", Command{Operation: "post", Room: "wires", Text: "socket reply", ReplyTo: article})
	// An edit of your own post is not a new post, whichever channel carries it.
	edit := viaRun(t, s, "ui", signed(writer, Command{Operation: "post", Room: "wires", Text: "# Two", Data: dataJSON(`"format":"markdown","supersedes":"`+article+`"`)})).Receipt.ID
	if m := get(t, s, edit); m.Via != "ui" || m.Supersedes != article {
		t.Fatalf("edit %+v", m)
	}
	// Groups expand when checked and are stored as written.
	run(t, s, policySet(owner, "wires", `{"write_via":["http"]}`))
	for _, via := range []string{"get", "post", "put", "mkcol", "x-text", "c64", "command"} {
		viaRun(t, s, via, Command{Operation: "post", Room: "wires", Text: "http " + via})
	}
	for _, via := range []string{"ui", "mcp", "dns"} {
		viaFails(t, s, via, Command{Operation: "post", Room: "wires", Text: "not http"}, "room_via_restricted")
	}
	if p := roomGet(t, s, "wires").Policy; strings.Join(p.WriteVia, ",") != "http" {
		t.Fatalf("group stored as %+v", p.WriteVia)
	}
	// Omitting write_via keeps it; [] clears it; the log records each change.
	run(t, s, policySet(owner, "wires", `{"rules":"Wires only."}`))
	if p := roomGet(t, s, "wires").Policy; len(p.WriteVia) != 1 {
		t.Fatalf("an unrelated change dropped write_via: %+v", p)
	}
	if detail := modlog(t, s, "wires")[0].Detail; !strings.Contains(detail, `"write_via":["http"]`) {
		t.Fatalf("modlog detail %s", detail)
	}
	run(t, s, policySet(owner, "wires", `{"write_via":[]}`))
	viaRun(t, s, "ui", Command{Operation: "post", Room: "wires", Text: "open again"})
	// rooms.list carries it too.
	run(t, s, policySet(owner, "wires", `{"write_via":["gemini"]}`))
	for _, r := range run(t, s, Command{Operation: "rooms.list"}).Rooms {
		if r.Name == "wires" && strings.Join(r.Policy.WriteVia, ",") != "gemini" {
			t.Fatalf("rooms.list policy %+v", r.Policy)
		}
	}
	// Reading is never restricted, on any channel.
	if got := viaRun(t, s, "gemini", Command{Operation: "messages.list", Room: "wires"}); len(got.Messages) == 0 {
		t.Fatal("a restricted room became unreadable")
	}
	// The operator sets it on an operator-owned room from the CLI.
	run(t, s, Command{Operation: "post", Room: "lulz", Text: "opened"})
	if _, err := s.OperatorRoom(testContext, Command{Operation: "room.policy.set", Room: "lulz", Data: `{"write_via":["mcp"]}`}); err != nil {
		t.Fatal(err)
	}
	viaFails(t, s, "ui", Command{Operation: "post", Room: "lulz", Text: "no"}, "room_via_restricted")
	viaRun(t, s, "mcp", Command{Operation: "post", Room: "lulz", Text: "yes"})
}

// Schema 14 adds events.via and room_policies.write_via to a schema 13
// database without touching anything else; old messages have no via.
func TestSchema14ViaMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema13.sqlite")
	s, err := Open(path, Config{})
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Unix(testTime, 0) }
	owner := keyFor(233)
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "garden"}))
	run(t, s, policySet(owner, "garden", `{"write":"owner","rules":"Be kind."}`))
	old := run(t, s, signed(owner, Command{Operation: "post", Room: "garden", Text: "before via"})).Receipt.ID
	if _, err = s.db.Exec("ALTER TABLE events DROP COLUMN via; ALTER TABLE room_policies DROP COLUMN write_via; PRAGMA user_version=13"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.now = func() time.Time { return time.Unix(testTime, 0) }
	if got := sqlCount(t, s, "PRAGMA user_version"); got != int64(SchemaVersion) || SchemaVersion < 14 {
		t.Fatalf("user_version %d", got)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM pragma_table_info('events') WHERE name='via'") + sqlCount(t, s, "SELECT count(*) FROM pragma_table_info('room_policies') WHERE name='write_via'"); n != 2 {
		t.Fatalf("migration added %d of 2 columns", n)
	}
	if m := get(t, s, old); m.Via != "" || m.Text != "before via" {
		t.Fatalf("old message %+v", m)
	}
	if p := roomGet(t, s, "garden").Policy; p.Write != "owner" || p.Rules != "Be kind." || len(p.WriteVia) != 0 {
		t.Fatalf("policy after migration %+v", p)
	}
	run(t, s, policySet(owner, "garden", `{"write_via":["tcp"]}`))
	viaRun(t, s, "tcp", signed(owner, Command{Operation: "post", Room: "garden", Text: "after"}))
	// Reopening an already migrated database changes nothing.
	s.Close()
	if s, err = Open(path, Config{}); err != nil {
		t.Fatal(err)
	}
	if p := roomGet(t, s, "garden").Policy; strings.Join(p.WriteVia, ",") != "tcp" {
		t.Fatalf("second open %+v", p)
	}
}

// A post the Nostr bridge reissued is via nostr, read from its forward
// record (the via column stays empty), and passes a nostr-only room while
// the same text over any other channel does not.
func TestNostrViaComesFromForwarded(t *testing.T) {
	s := openTest(t, Config{})
	run(t, s, Command{Operation: "post", Room: "nostr", Text: "opened"})
	if _, err := s.OperatorRoom(testContext, Command{Operation: "room.policy.set", Room: "nostr", Data: `{"write_via":["nostr"]}`}); err != nil {
		t.Fatal(err)
	}
	f := Forwarded{Mode: "reissued", OriginService: "nostr", OriginID: strings.Repeat("ab", 32), OriginAuthor: "npub1xyz", OriginRef: "nostr:nevent1xyz"}
	res, err := s.Execute(WithVia(WithForwarded(testContext, f), "tcp"), Command{Operation: "post", Room: "nostr", Text: "from a relay"}, "nostr:"+strings.Repeat("cd", 32))
	if err != nil {
		t.Fatal(err)
	}
	if m := get(t, s, res.Receipt.ID); m.Via != "nostr" || m.Forwarded == nil {
		t.Fatalf("bridged post %+v", m)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM events WHERE id=? AND via=''", res.Receipt.ID); n != 1 {
		t.Fatal("nostr was stored twice")
	}
	viaFails(t, s, "tcp", Command{Operation: "post", Room: "nostr", Text: "not from a relay"}, "room_via_restricted")
	for _, m := range run(t, s, Command{Operation: "messages.list", Room: "nostr"}).Messages {
		if m.ID == res.Receipt.ID && m.Via != "nostr" {
			t.Fatalf("list lost the derived via: %+v", m)
		}
	}
}
