package board

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// rfc0013Tables are the tables conversation_schema.go creates.
var rfc0013Tables = []string{"conversations", "conversation_members", "contact_blocks", "messaging_settings", "message_screens", "hosted_keys", "hosted_tokens", "hosted_issuance", "seal_epochs", "seal_wraps"}

func checkConversationSchema(t *testing.T, s *Store) {
	t.Helper()
	for _, table := range rfc0013Tables {
		if sqlCount(t, s, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", table) != 1 {
			t.Errorf("table %s is missing", table)
		}
	}
	for _, index := range []string{"conversations_pair", "conversation_members_account", "members_account", "hosted_tokens_account"} {
		if sqlCount(t, s, "SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?", index) != 1 {
			t.Errorf("index %s is missing", index)
		}
	}
	for _, column := range conversationColumns {
		if sqlCount(t, s, "SELECT count(*) FROM pragma_table_info(?) WHERE name=?", column.table, column.name) != 1 {
			t.Errorf("column %s.%s is missing", column.table, column.name)
		}
	}
	if got := sqlCount(t, s, "PRAGMA user_version"); got != SchemaVersion {
		t.Errorf("user_version %d, want %d: RFC0013 is additive", got, SchemaVersion)
	}
}

func TestConversationSchemaOnFreshDatabase(t *testing.T) {
	s := openTest(t, Config{})
	checkConversationSchema(t, s)
	key := keyFor(131)
	register(t, s, key)
	if got := sqlCount(t, s, "SELECT count(*) FROM identities WHERE id=? AND custody='self'", keyID(key)); got != 1 {
		t.Fatalf("a new identity's custody is not self: %d", got)
	}
	if _, err := s.db.Exec("UPDATE identities SET custody='someone' WHERE id=?", keyID(key)); err == nil {
		t.Fatal("custody accepted a value other than self or hosted")
	}
	// Running the column migration again changes nothing.
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = migrateConversations(tx); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	checkConversationSchema(t, s)
}

// A database the 1.23 code made, rows included, opens and gains the RFC0013
// tables and columns, with the defaults on existing rows.
func TestConversationSchemaUpgradesAnEarlierDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "board.sqlite")
	s, err := Open(path, Config{})
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Unix(testTime, 0) }
	owner := keyFor(132)
	register(t, s, owner)
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "garden"}))
	run(t, s, signed(owner, Command{Operation: "room.policy.set", Room: "garden", Data: `{"write":"owner"}`}))
	drop := "PRAGMA user_version=15; DROP INDEX members_account;"
	for _, table := range []string{"conversation_members", "conversations", "contact_blocks", "messaging_settings", "message_screens", "hosted_tokens", "hosted_keys", "hosted_issuance", "seal_wraps", "seal_epochs"} {
		drop += " DROP TABLE " + table + ";"
	}
	for _, column := range conversationColumns {
		drop += " ALTER TABLE " + column.table + " DROP COLUMN " + column.name + ";"
	}
	if _, err = s.db.Exec(drop); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 { // the second open finds everything in place
		if s, err = Open(path, Config{}); err != nil {
			t.Fatal(err)
		}
		s.now = func() time.Time { return time.Unix(testTime, 0) }
		checkConversationSchema(t, s)
		if got := sqlCount(t, s, "SELECT count(*) FROM identities WHERE id=? AND custody='self'", keyID(owner)); got != 1 {
			t.Fatalf("an existing identity's custody is not self: %d", got)
		}
		if got := sqlCount(t, s, "SELECT count(*) FROM room_policies WHERE room='garden' AND write_policy='owner' AND closed=0 AND closes_at=0 AND max_messages=0"); got != 1 {
			t.Fatalf("an existing room policy lost its values or its defaults: %d", got)
		}
		if err = s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestConversationRoomNames(t *testing.T) {
	name := "~" + strings.Repeat("a", 13) + strings.Repeat("7", 13)
	for room, want := range map[string]bool{
		name: true, "~abcdefghijklmnopqrstuvwxyz": true, "~234567abcdefghijklmnopqrst": true, "~abcdefghijklmnopqrstuvwxy9": false,
		"~" + strings.Repeat("a", 25): false, "~" + strings.Repeat("a", 27): false, "~": false,
		"~" + strings.Repeat("A", 26): false, "~" + strings.Repeat("1", 26): false, "~" + strings.Repeat("8", 26): false,
		"lobby": true, "@" + strings.Repeat("a", 64): true,
	} {
		if ValidRoomName(room) != want || IsConversationRoom(room) != (want && strings.HasPrefix(room, "~")) {
			t.Errorf("%q: valid %v, conversation %v", room, ValidRoomName(room), IsConversationRoom(room))
		}
	}
	s := openTest(t, Config{})
	key := keyFor(133)
	register(t, s, key)
	for _, visibility := range []string{"public", "private"} {
		fails(t, s, signed(key, Command{Operation: "room.create", Room: name, Visibility: visibility}), "invalid_slug")
	}
	// post never opens a conversation room, signed or not.
	fails(t, s, Command{Operation: "post", Room: name, Text: "hello"}, "not_found")
	fails(t, s, signed(key, Command{Operation: "post", Room: name, Text: "hello"}), "not_found")
	fails(t, s, signed(key, Command{Operation: "post", Room: name, Text: "hello", Visibility: "private"}), "not_found")
	if got := sqlCount(t, s, "SELECT count(*) FROM rooms WHERE name=?", name); got != 0 {
		t.Fatalf("a post opened %s", name)
	}
}

// Every RFC0013 operation is built: a malformed rotation and a sealed post
// outside a sealed conversation are refused, and nothing is written.
func TestConversationStubsAnswerNotImplemented(t *testing.T) {
	s := openTest(t, Config{})
	key := keyFor(134)
	register(t, s, key)
	room := "~" + strings.Repeat("b", 26)
	for code, c := range map[string]Command{
		"invalid_seal": {Operation: "conversation.seal", Room: room, Data: `{"schema":1}`},
		"not_sealed":   {Operation: "post", Room: "lobby", Text: "sealed1.1.AAAAAAAAAAAAAAAA.AAAAAAAAAAAAAAAAAAAAAA", Data: dataJSON(`"format":"sealed"`)},
	} {
		c.RequestID = "stub-" + c.Operation
		fails(t, s, signed(key, c), code)
	}
	// Hosted identities are built (hosted_test.go); without a KEK they are off.
	via := WithVia(testContext, "mcp")
	for _, c := range []Command{{Operation: "hosted.create", RequestID: "h1"}, {Operation: "hosted.recover", RequestID: "h2", Data: "{}"}, signed(key, Command{Operation: "hosted.token", RequestID: "h3", Data: "{}"})} {
		if _, err := s.Execute(via, c, "test-origin"); !isCode(err, "hosted_unavailable") {
			t.Errorf("%s over MCP: %v", c.Operation, err)
		}
	}
	if got := sqlCount(t, s, "SELECT count(*) FROM events WHERE format='sealed'"); got != 0 {
		t.Fatal("a sealed post was stored")
	}
}

func TestWirePermitted(t *testing.T) {
	conversation := "~" + strings.Repeat("c", 26)
	for _, tc := range []struct {
		via  string
		c    Command
		code string // "" is permitted
	}{
		// An in-process caller is not a wire.
		{"", Command{Operation: "hosted.create"}, ""},
		{"", Command{Operation: "agent.rotate"}, ""},
		// WireAny: every wire, the unrecorded ones (Gopher, finger) too.
		{"gemini", Command{Operation: "post"}, ""},
		{"unrecorded", Command{Operation: "messages.list"}, ""},
		{"nostr", Command{Operation: "agent.get"}, ""},
		// WireSigned: the HTTPS channels and the wires that carry signed commands.
		{"tcp", Command{Operation: "conversation.open"}, ""},
		{"dns", Command{Operation: "conversation.get"}, ""},
		{"email", Command{Operation: "conversation.respond"}, ""},
		{"command", Command{Operation: "conversations.list"}, ""},
		{"mcp", Command{Operation: "conversation.seal"}, ""},
		{"tcp", Command{Operation: "room.invite.accept"}, ""},
		// The one inbox travels wherever conversations do.
		{"tcp", Command{Operation: "updates.get"}, ""},
		{"dns", Command{Operation: "updates.get"}, ""},
		{"email", Command{Operation: "updates.get"}, ""},
		{"gemini", Command{Operation: "updates.get"}, "unsupported_operation"},
		{"gemini", Command{Operation: "conversation.get"}, "unsupported_operation"},
		{"nostr", Command{Operation: "room.invite.create"}, "unsupported_operation"},
		{"unrecorded", Command{Operation: "conversations.list"}, "unsupported_operation"},
		// Governing a conversation room travels with the conversation.
		{"tcp", Command{Operation: "room.member.add", Room: conversation}, ""},
		{"email", Command{Operation: "room.member.remove", Room: conversation}, ""},
		{"dns", Command{Operation: "room.policy.set", Room: conversation}, ""},
		{"tcp", Command{Operation: "room.member.add", Room: "garden"}, "unsupported_operation"},
		{"tcp", Command{Operation: "room.policy.set", Room: "garden"}, "unsupported_operation"},
		// WireHTTPS.
		{"ui", Command{Operation: "messaging.policy.set"}, ""},
		{"c64", Command{Operation: "identity.link"}, ""},
		{"mcp", Command{Operation: "agent.rotate"}, ""},
		// A conversation's settings and sealing keys travel every signing wire.
		{"tcp", Command{Operation: "messaging.policy.set"}, ""},
		{"email", Command{Operation: "identity.link"}, ""},
		{"dns", Command{Operation: "identity.unlink"}, ""},
		{"gemini", Command{Operation: "identity.link"}, "unsupported_operation"},
		{"dns", Command{Operation: "agent.rotate"}, "unsupported_operation"},
		{"tcp", Command{Operation: "no.such.operation"}, "unsupported_operation"},
		{"command", Command{Operation: "no.such.operation"}, ""},
		// WireMCP.
		{"mcp", Command{Operation: "hosted.create"}, ""},
		{"mcp", Command{Operation: "hosted.claim"}, ""},
		{"command", Command{Operation: "hosted.create"}, "mcp_only"},
		{"ui", Command{Operation: "hosted.recover"}, "mcp_only"},
		{"tcp", Command{Operation: "hosted.token"}, "mcp_only"},
		// Envelope contexts stay on HTTPS.
		{"dns", Command{Operation: "post", Delegation: &DelegationContext{}}, "https_required"},
		{"tcp", Command{Operation: "messages.list", PrivateRead: &PrivateReadContext{}}, "https_required"},
		{"command", Command{Operation: "post", Delegation: &DelegationContext{}}, ""},
		// Service calls: signed only over HTTPS; unsigned calls only over TCP.
		{"tcp", Command{Operation: "service.call", PublicKey: "k", Signature: "s"}, "https_required"},
		{"tcp", Command{Operation: "service.call", Target: "notary"}, ""},
		{"dns", Command{Operation: "service.call", Target: "notary"}, "unsupported_operation"},
		{"email", Command{Operation: "service.read", Target: "notary"}, ""},
		{"command", Command{Operation: "service.call", PublicKey: "k", Signature: "s"}, ""},
	} {
		err := WirePermitted(tc.via, tc.c)
		if (tc.code == "" && err != nil) || (tc.code != "" && !isCode(err, tc.code)) {
			t.Errorf("%s over %q: %v, want %q", tc.c.Operation, tc.via, err, tc.code)
		}
	}
	for _, op := range Operations() {
		if !slices.Contains([]string{WireAny, WireSigned, WireHTTPS, WireMCP}, op.Wire) {
			t.Errorf("%s has no wire", op.Name)
		}
	}
	// A refusal names the operation and the wire, and what the wires carry.
	if err := WirePermitted("dns", Command{Operation: "agent.rotate"}); err == nil || !strings.Contains(err.Error(), "agent.rotate is not carried over DNS") || !strings.Contains(err.Error(), "identity.link") {
		t.Errorf("refusal: %v", err)
	}
}

// executeCommand applies the same policy, so an adapter that skipped it
// still cannot reach an operation its wire does not carry.
func TestExecuteAppliesWirePolicy(t *testing.T) {
	s := openTest(t, Config{})
	key := keyFor(135)
	if _, err := s.Execute(WithVia(testContext, "tcp"), signed(key, Command{Operation: "agent.register"}), "test-origin"); !isCode(err, "unsupported_operation") {
		t.Fatalf("agent.register over tcp: %v", err)
	}
	if _, err := s.Execute(WithVia(testContext, "command"), Command{Operation: "hosted.create", RequestID: "h"}, "test-origin"); !isCode(err, "mcp_only") {
		t.Fatalf("hosted.create over HTTPS: %v", err)
	}
	if got := sqlCount(t, s, "SELECT count(*) FROM identities WHERE id=?", keyID(key)); got != 0 {
		t.Fatal("a refused command registered its key")
	}
	// A recorded wire the board does not list (Gopher, finger) is a wire,
	// not an in-process caller: it carries no signed command, and no
	// hosted-identity operation.
	for code, c := range map[string]Command{"unsupported_operation": signed(key, Command{Operation: "conversations.list"}), "mcp_only": {Operation: "hosted.create", RequestID: "g"}} {
		if _, err := s.Execute(WithVia(testContext, "gopher"), c, "test-origin"); !isCode(err, code) {
			t.Fatalf("%s over gopher: %v, want %s", c.Operation, err, code)
		}
	}
}
