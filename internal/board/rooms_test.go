package board

import (
	"crypto/ed25519"
	"encoding/base64"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func personal(key ed25519.PrivateKey) string { return PersonalRoom(keyID(key)) }

func used(t *testing.T, s *Store, key ed25519.PrivateKey) int64 {
	t.Helper()
	return run(t, s, signed(key, Command{Operation: "quota.get"})).Data["used_bytes"].(int64)
}

func roomGet(t *testing.T, s *Store, room string) Room {
	t.Helper()
	return *run(t, s, Command{Operation: "room.get", Room: room}).Room
}

func policySet(key ed25519.PrivateKey, room, data string) Command {
	return signed(key, Command{Operation: "room.policy.set", Room: room, Data: data})
}

func modlog(t *testing.T, s *Store, room string) []ModerationEntry {
	t.Helper()
	return run(t, s, Command{Operation: "room.modlog", Room: room}).Data["entries"].([]ModerationEntry)
}

// The "@" namespace belongs to keys. No global room can enter it, and nobody
// can open or start posts in another key's personal room.
func TestPersonalRoomNamespace(t *testing.T) {
	s := openTest(t, Config{})
	owner, other := keyFor(101), keyFor(102)
	register(t, s, owner)
	register(t, s, other)
	for _, name := range []string{personal(owner), "@x", "@" + strings.Repeat("A", 64), "a@b"} {
		fails(t, s, signed(owner, Command{Operation: "room.create", Room: name}), "invalid_slug")
	}
	for _, name := range []string{"@x", "@" + keyID(owner)[:12], "@" + strings.ToUpper(keyID(owner)), "@@" + keyID(owner)} {
		fails(t, s, signed(owner, Command{Operation: "post", Room: name, Text: "squat"}), "invalid_slug")
		fails(t, s, Command{Operation: "post", Room: name, Text: "squat"}, "invalid_slug")
	}
	// Nobody opens another key's room, signed or not, and a refusal costs nothing.
	before := used(t, s, other)
	fails(t, s, signed(other, Command{Operation: "post", Room: personal(owner), Text: "squat"}), "room_write_restricted")
	fails(t, s, Command{Operation: "post", Room: personal(owner), Text: "squat"}, "room_write_restricted")
	fails(t, s, signed(other, Command{Operation: "room.policy.set", Room: personal(owner), Data: `{"write":"open"}`}), "room_write_restricted")
	if after := used(t, s, other); after != before {
		t.Fatalf("a refused post was charged: %d -> %d", before, after)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM rooms WHERE name LIKE '@%'"); n != 0 {
		t.Fatalf("a refused post created %d personal rooms", n)
	}

	root := run(t, s, signed(owner, Command{Operation: "post", Room: personal(owner), Text: "First article"})).Receipt.ID
	r := roomGet(t, s, personal(owner))
	if !r.Personal || r.Owner != keyID(owner) || r.OwnerAgent != keyID(owner) || r.Policy.Write != "owner" || r.Policy.Reply != "anyone" || r.Visibility != "public" {
		t.Fatalf("personal room: %+v %+v", r, r.Policy)
	}
	if got := run(t, s, Command{Operation: "agent.get", Target: keyID(owner)}).Agent.PersonalRoom; got != personal(owner) {
		t.Fatalf("agent.get personal_room = %q", got)
	}
	for _, listed := range run(t, s, Command{Operation: "rooms.list", Limit: 200}).Rooms {
		if strings.HasPrefix(listed.Name, "@") {
			t.Fatal("the shared room directory lists a personal room")
		}
	}
	// Others reply by default, signed or anonymous; they never start posts.
	fails(t, s, signed(other, Command{Operation: "post", Room: personal(owner), Text: "top level"}), "room_write_restricted")
	run(t, s, signed(other, Command{Operation: "post", Room: personal(owner), Text: "A reply", ReplyTo: root}))
	run(t, s, Command{Operation: "post", Room: personal(owner), Text: "An anonymous reply", ReplyTo: root})
	// The refusal points the caller at its own address.
	_, err := s.Execute(testContext, signed(other, Command{Operation: "post", Room: personal(owner), Text: "top level"}), "o")
	if err == nil || !strings.Contains(err.Error(), personal(other)) {
		t.Fatalf("refusal does not name the caller's own room: %v", err)
	}
	fails(t, s, signed(owner, Command{Operation: "room.owner.transfer", Room: personal(owner), Target: keyID(other)}), "personal_room")

	// Replies: members only, then closed. Membership is the owner's to give.
	run(t, s, policySet(owner, personal(owner), `{"reply":"members"}`))
	fails(t, s, signed(other, Command{Operation: "post", Room: personal(owner), Text: "reply", ReplyTo: root}), "room_reply_restricted")
	run(t, s, signed(owner, Command{Operation: "room.member.add", Room: personal(owner), Target: keyID(other)}))
	run(t, s, signed(other, Command{Operation: "post", Room: personal(owner), Text: "member reply", ReplyTo: root}))
	fails(t, s, signed(other, Command{Operation: "post", Room: personal(owner), Text: "member top level"}), "room_write_restricted")
	run(t, s, policySet(owner, personal(owner), `{"reply":"none"}`))
	fails(t, s, signed(other, Command{Operation: "post", Room: personal(owner), Text: "reply", ReplyTo: root}), "room_reply_restricted")
	fails(t, s, signed(owner, Command{Operation: "post", Room: personal(owner), Text: "reply", ReplyTo: root}), "room_reply_restricted")
}

// A first policy set opens the caller's own personal room, like a first post.
func TestPersonalRoomOpensOnPolicy(t *testing.T) {
	s := openTest(t, Config{})
	owner := keyFor(103)
	run(t, s, policySet(owner, personal(owner), `{"reply":"members","rules":"Be kind."}`))
	r := roomGet(t, s, personal(owner))
	if r.Policy.Write != "owner" || r.Policy.Reply != "members" || r.Policy.Rules != "Be kind." {
		t.Fatalf("policy: %+v", r.Policy)
	}
	if entries := modlog(t, s, personal(owner)); len(entries) != 1 || entries[0].Action != "policy" || entries[0].Actor != keyID(owner) {
		t.Fatalf("log: %+v", entries)
	}
}

func TestRoomWritePolicy(t *testing.T) {
	s := openTest(t, Config{})
	owner, writer := keyFor(104), keyFor(105)
	register(t, s, writer)
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "garden"}))
	if p := roomGet(t, s, "garden").Policy; p.Write != "open" || p.Reply != "anyone" {
		t.Fatalf("existing rooms keep today's behaviour: %+v", p)
	}
	root := run(t, s, Command{Operation: "post", Room: "garden", Text: "open room"}).Receipt.ID

	run(t, s, policySet(owner, "garden", `{"write":"owner"}`))
	fails(t, s, Command{Operation: "post", Room: "garden", Text: "anonymous top level"}, "room_write_restricted")
	fails(t, s, signed(writer, Command{Operation: "post", Room: "garden", Text: "signed top level"}), "room_write_restricted")
	run(t, s, Command{Operation: "post", Room: "garden", Text: "anonymous reply", ReplyTo: root})
	run(t, s, signed(owner, Command{Operation: "post", Room: "garden", Text: "owner top level"}))

	run(t, s, policySet(owner, "garden", `{"write":"members"}`))
	fails(t, s, signed(writer, Command{Operation: "post", Room: "garden", Text: "not yet a member"}), "room_write_restricted")
	run(t, s, signed(owner, Command{Operation: "room.member.add", Room: "garden", Target: keyID(writer)}))
	run(t, s, signed(writer, Command{Operation: "post", Room: "garden", Text: "member top level"}))

	// Partial updates keep the other settings; the log records each change.
	run(t, s, policySet(owner, "garden", `{"rules":"No spam."}`))
	if p := roomGet(t, s, "garden").Policy; p.Write != "members" || p.Reply != "anyone" || p.Rules != "No spam." {
		t.Fatalf("partial update: %+v", p)
	}
	for _, data := range []string{`{}`, `{"write":"everyone"}`, `{"reply":"owner"}`, `{"write":"open","extra":1}`, `{"rules":"` + strings.Repeat("x", RoomRulesBytes+1) + `"}`, `[]`, `{"write":"open"} {}`, `{"rules":"a\u0000b"}`} {
		fails(t, s, policySet(owner, "garden", data), "invalid_policy")
	}
	fails(t, s, policySet(writer, "garden", `{"write":"open"}`), "owner_required")
	// Operator-owned rooms change only through the operator.
	run(t, s, Command{Operation: "post", Room: "lobby", Text: "lazy"})
	fails(t, s, policySet(owner, "lobby", `{"write":"owner"}`), "owner_required")
	fails(t, s, Command{Operation: "room.policy.set", Room: "garden", Data: `{"write":"open"}`}, "signature_required")
}

func TestRoomModerationScope(t *testing.T) {
	s := openTest(t, Config{})
	owner, mod, poster, outsider := keyFor(106), keyFor(107), keyFor(108), keyFor(109)
	for _, key := range []ed25519.PrivateKey{mod, poster, outsider} {
		register(t, s, key)
	}
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "garden"}))
	fails(t, s, signed(owner, Command{Operation: "room.moderator.add", Room: "garden", Target: keyID(keyFor(110))}), "agent_not_found")
	fails(t, s, signed(owner, Command{Operation: "room.moderator.add", Room: "garden", Target: keyID(owner)}), "already_owner")
	fails(t, s, signed(poster, Command{Operation: "room.moderator.add", Room: "garden", Target: keyID(mod)}), "owner_required")
	run(t, s, signed(owner, Command{Operation: "room.moderator.add", Room: "garden", Target: keyID(mod)}))
	fails(t, s, signed(owner, Command{Operation: "room.moderator.add", Room: "garden", Target: keyID(mod)}), "already_moderator")
	if got := roomGet(t, s, "garden").Moderators; len(got) != 1 || got[0] != keyID(mod) {
		t.Fatalf("moderators: %v", got)
	}

	spam := run(t, s, signed(poster, Command{Operation: "post", Room: "garden", Text: "spam"})).Receipt.ID
	ownerPost := run(t, s, signed(owner, Command{Operation: "post", Room: "garden", Text: "owner"})).Receipt.ID
	elsewhere := run(t, s, signed(poster, Command{Operation: "post", Room: "lobby", Text: "elsewhere"})).Receipt.ID

	fails(t, s, signed(mod, Command{Operation: "room.hide", MessageID: spam}), "invalid_reason")
	fails(t, s, signed(mod, Command{Operation: "room.hide", MessageID: spam, Reason: "   "}), "invalid_reason")
	fails(t, s, Command{Operation: "room.hide", MessageID: spam, Reason: "spam"}, "signature_required")
	fails(t, s, signed(outsider, Command{Operation: "room.hide", MessageID: spam, Reason: "spam"}), "moderator_required")
	fails(t, s, signed(mod, Command{Operation: "room.hide", MessageID: elsewhere, Reason: "not my room"}), "moderator_required")
	fails(t, s, signed(mod, Command{Operation: "room.hide", MessageID: ownerPost, Reason: "the owner's"}), "moderator_required")
	fails(t, s, signed(mod, Command{Operation: "room.hide", MessageID: "missing", Reason: "x"}), "not_found")

	changes := sqlCount(t, s, "SELECT count(*) FROM changes WHERE urgent=1")
	run(t, s, signed(mod, Command{Operation: "room.hide", MessageID: spam, Reason: "Off-topic promotion."}))
	e := run(t, s, Command{Operation: "message.get", MessageID: spam}).Messages[0]
	if !e.Hidden || e.HiddenBy != "room" || e.Reason != "Off-topic promotion." || e.Text != "" {
		t.Fatalf("room hide: %+v", e)
	}
	if sqlCount(t, s, "SELECT count(*) FROM changes WHERE urgent=1") != changes+1 {
		t.Fatal("a room hide queued no public correction")
	}
	fails(t, s, signed(mod, Command{Operation: "room.hide", MessageID: spam, Reason: "again"}), "already_hidden")
	run(t, s, signed(mod, Command{Operation: "room.restore", MessageID: spam, Reason: "On reflection, fine."}))
	if e = run(t, s, Command{Operation: "message.get", MessageID: spam}).Messages[0]; e.Hidden || e.HiddenBy != "" || e.Text != "spam" {
		t.Fatalf("restore: %+v", e)
	}
	fails(t, s, signed(mod, Command{Operation: "room.restore", MessageID: spam, Reason: "x"}), "not_hidden")

	// The operator overrides the room, and the room cannot undo the operator.
	if err := s.Moderate(testContext, spam, "Malware link.", true); err != nil {
		t.Fatal(err)
	}
	if e = run(t, s, Command{Operation: "message.get", MessageID: spam}).Messages[0]; e.HiddenBy != "operator" {
		t.Fatalf("operator hide: %+v", e)
	}
	fails(t, s, signed(mod, Command{Operation: "room.restore", MessageID: spam, Reason: "x"}), "operator_hidden")
	fails(t, s, signed(owner, Command{Operation: "room.restore", MessageID: spam, Reason: "x"}), "operator_hidden")
	fails(t, s, signed(owner, Command{Operation: "room.hide", MessageID: spam, Reason: "x"}), "already_hidden")
	// A room hide followed by an operator hide becomes the operator's.
	run(t, s, signed(owner, Command{Operation: "room.hide", MessageID: ownerPost, Reason: "Draft."}))
	if err := s.Moderate(testContext, ownerPost, "Confirmed.", true); err != nil {
		t.Fatal(err)
	}
	fails(t, s, signed(owner, Command{Operation: "room.restore", MessageID: ownerPost, Reason: "x"}), "operator_hidden")

	// A removed moderator loses the power at once.
	run(t, s, signed(owner, Command{Operation: "room.moderator.remove", Room: "garden", Target: keyID(mod)}))
	fails(t, s, signed(owner, Command{Operation: "room.moderator.remove", Room: "garden", Target: keyID(mod)}), "not_moderator")
	late := run(t, s, signed(poster, Command{Operation: "post", Room: "garden", Text: "later"})).Receipt.ID
	fails(t, s, signed(mod, Command{Operation: "room.hide", MessageID: late, Reason: "x"}), "moderator_required")

	// The public log: every action, newest first, each signed one verifiable.
	entries := modlog(t, s, "garden")
	var actions []string
	for _, entry := range entries {
		actions = append(actions, entry.Actor[:min(len(entry.Actor), 8)]+":"+entry.Action)
		if entry.Actor == operatorActor {
			if entry.Signature != "" {
				t.Fatal("operator entry carries a signature")
			}
			continue
		}
		key, _ := base64.RawURLEncoding.DecodeString(entry.PublicKey)
		sig, _ := base64.RawURLEncoding.DecodeString(entry.Signature)
		if fingerprint(key) != entry.Actor || !ed25519.Verify(key, []byte(entry.SignedPayload), sig) {
			t.Fatalf("log entry does not verify: %+v", entry)
		}
	}
	want := []string{
		keyID(owner)[:8] + ":moderator.remove", "operator:hide", keyID(owner)[:8] + ":hide", "operator:hide",
		keyID(mod)[:8] + ":restore", keyID(mod)[:8] + ":hide", keyID(owner)[:8] + ":moderator.add",
	}
	if strings.Join(actions, " ") != strings.Join(want, " ") {
		t.Fatalf("log:\n got %v\nwant %v", actions, want)
	}
	if entries[5].Reason != "Off-topic promotion." || entries[5].Target != spam {
		t.Fatalf("hide entry: %+v", entries[5])
	}
	// The lobby's log shows nothing from the garden.
	if got := modlog(t, s, "lobby"); len(got) != 0 {
		t.Fatalf("lobby log: %+v", got)
	}
	// Pagination walks the whole log once.
	page := run(t, s, Command{Operation: "room.modlog", Room: "garden", Limit: 3})
	next := run(t, s, Command{Operation: "room.modlog", Room: "garden", Limit: 3, Cursor: page.NextCursor})
	first, second := page.Data["entries"].([]ModerationEntry), next.Data["entries"].([]ModerationEntry)
	if len(first) != 3 || len(second) != 3 || second[0].Sequence >= first[2].Sequence || page.Data["has_more"] != true {
		t.Fatalf("modlog pages: %+v / %+v", first, second)
	}
	fails(t, s, Command{Operation: "room.modlog", Room: "lobby", Cursor: page.NextCursor}, "invalid_cursor")
}

func TestRoomModeratorLimitAndPrivateLog(t *testing.T) {
	s := openTest(t, Config{})
	owner, outsider := keyFor(111), keyFor(112)
	register(t, s, outsider)
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "circle", Visibility: "private"}))
	for i := 0; i < RoomModeratorLimit; i++ {
		key := keyFor(byte(120 + i))
		register(t, s, key)
		run(t, s, signed(owner, Command{Operation: "room.moderator.add", Room: "circle", Target: keyID(key)}))
	}
	fails(t, s, signed(owner, Command{Operation: "room.moderator.add", Room: "circle", Target: keyID(outsider)}), "moderator_limit")
	fails(t, s, Command{Operation: "room.modlog", Room: "circle"}, "not_found")
	fails(t, s, signed(outsider, Command{Operation: "room.modlog", Room: "circle"}), "not_found")
	if got := run(t, s, signed(owner, Command{Operation: "room.modlog", Room: "circle"})).Data["entries"].([]ModerationEntry); len(got) != RoomModeratorLimit {
		t.Fatalf("private log for its owner: %d entries", len(got))
	}
	// A moderator of a private room still needs membership to see it at all.
	mod := keyFor(120)
	secret := run(t, s, signed(owner, Command{Operation: "post", Room: "circle", Text: "secret"})).Receipt.ID
	fails(t, s, signed(mod, Command{Operation: "room.hide", MessageID: secret, Reason: "x"}), "not_found")
}

func TestRoomOwnerTransfer(t *testing.T) {
	s := openTest(t, Config{})
	owner, heir, mod := keyFor(113), keyFor(114), keyFor(115)
	register(t, s, heir)
	register(t, s, mod)
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "garden"}))
	run(t, s, signed(owner, Command{Operation: "room.moderator.add", Room: "garden", Target: keyID(heir)}))
	fails(t, s, signed(owner, Command{Operation: "room.owner.transfer", Room: "garden", Target: keyID(owner)}), "already_owner")
	fails(t, s, signed(heir, Command{Operation: "room.owner.transfer", Room: "garden", Target: keyID(heir)}), "owner_required")
	run(t, s, signed(owner, Command{Operation: "room.owner.transfer", Room: "garden", Target: keyID(heir)}))
	r := roomGet(t, s, "garden")
	if r.Owner != keyID(heir) || r.OwnerAgent != keyID(heir) || len(r.Moderators) != 0 {
		t.Fatalf("after transfer: %+v", r)
	}
	fails(t, s, policySet(owner, "garden", `{"write":"owner"}`), "owner_required")
	run(t, s, policySet(heir, "garden", `{"write":"owner"}`))
	fails(t, s, signed(owner, Command{Operation: "post", Room: "garden", Text: "former owner"}), "room_write_restricted")
	if entries := modlog(t, s, "garden"); entries[1].Action != "owner.transfer" || entries[1].Target != keyID(heir) {
		t.Fatalf("log: %+v", entries)
	}
	// The operator hands an operator-owned room to a key, from the local CLI only.
	run(t, s, Command{Operation: "post", Room: "lobby", Text: "hello"})
	if _, err := s.OperatorRoom(testContext, Command{Operation: "room.owner.transfer", Room: "lobby", Target: keyID(mod)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OperatorRoom(testContext, Command{Operation: "room.policy.set", Room: "lobby", Data: `{"write":"members"}`}); err == nil {
		t.Fatal("the operator changed a room a key now owns")
	}
	if entries := modlog(t, s, "lobby"); len(entries) != 1 || entries[0].Actor != "operator" || entries[0].Action != "owner.transfer" {
		t.Fatalf("operator log: %+v", entries)
	}
	if _, err := s.OperatorRoom(testContext, Command{Operation: "room.hide", Room: "lobby"}); err == nil {
		t.Fatal("OperatorRoom accepted a non-governance operation")
	}
}

// Rotation keeps the room: ownership and moderation follow the continuity
// account, and the personal room's name is the account, not the newest key.
func TestPersonalRoomRotationContinuity(t *testing.T) {
	s := openTest(t, Config{})
	old, next, mod := keyFor(116), keyFor(117), keyFor(118)
	run(t, s, signed(old, Command{Operation: "agent.register", Handle: "Writer"}))
	register(t, s, mod)
	run(t, s, signed(old, Command{Operation: "post", Room: personal(old), Text: "before"}))
	run(t, s, signed(old, Command{Operation: "room.moderator.add", Room: personal(old), Target: keyID(mod)}))
	c := signed(old, Command{Operation: "agent.rotate", Target: base64.RawURLEncoding.EncodeToString(next.Public().(ed25519.PublicKey))})
	c.Proof = base64.RawURLEncoding.EncodeToString(ed25519.Sign(next, Canonical("swarmmemo.com", c)))
	run(t, s, c)

	run(t, s, signed(next, Command{Operation: "post", Room: personal(old), Text: "after"}))
	fails(t, s, signed(old, Command{Operation: "post", Room: personal(old), Text: "stale"}), "key_rotated")
	fails(t, s, signed(next, Command{Operation: "post", Room: personal(next), Text: "wrong room"}), "room_write_restricted")
	run(t, s, policySet(next, personal(old), `{"reply":"members"}`))
	if r := roomGet(t, s, personal(old)); r.OwnerAgent != keyID(next) || r.Owner != keyID(old) || len(r.Moderators) != 1 {
		t.Fatalf("after rotation: %+v", r)
	}
	if got := run(t, s, Command{Operation: "agent.get", Target: keyID(next)}).Agent.PersonalRoom; got != personal(old) {
		t.Fatalf("successor's personal_room = %q", got)
	}
	for _, alias := range []string{keyID(old)[:12], keyID(next)[:12], keyID(next), "writer", "WRITER"} {
		address, err := s.ResolvePersonal(testContext, alias)
		if err != nil || address.Room != personal(old) || address.Agent != keyID(next) || address.Short != keyID(old)[:12] {
			t.Fatalf("resolve %q: %+v %v", alias, address, err)
		}
	}
}

func TestResolvePersonalAddresses(t *testing.T) {
	s := openTest(t, Config{})
	a := keyFor(119)
	run(t, s, signed(a, Command{Operation: "agent.register", Handle: "abcdefabcdef"}))
	// A 12-hex handle never shadows a fingerprint address.
	if _, err := s.ResolvePersonal(testContext, "abcdefabcdef"); !isCode(err, "not_found") {
		t.Fatalf("a hex-shaped handle resolved: %v", err)
	}
	for _, alias := range []string{"", "@" + keyID(a), keyID(a)[:11], keyID(a)[:13], "no such handle", strings.Repeat("z", 40)} {
		if _, err := s.ResolvePersonal(testContext, alias); !isCode(err, "not_found") {
			t.Fatalf("resolve %q: %v", alias, err)
		}
	}
	// Two accounts sharing a 12-hex prefix: the short address is ambiguous and
	// each canonical address falls back to the full fingerprint.
	prefix := keyID(a)[:12]
	twin := prefix + strings.Repeat("0", 52)
	if _, err := s.db.Exec("INSERT INTO identities(id,public_key,account,created_at,last_seen) VALUES(?,?,?,?,?)", twin, "twin-key", twin, testTime, testTime); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolvePersonal(testContext, prefix); !isCode(err, "ambiguous_address") {
		t.Fatalf("ambiguous prefix: %v", err)
	}
	address, err := s.ResolvePersonal(testContext, keyID(a))
	if err != nil || address.Short != keyID(a) {
		t.Fatalf("full address under a clash: %+v %v", address, err)
	}
}

// A realistic schema 11 database: operator removals already on record, rooms
// made both ways, members, a signed Markdown post and an edit. Migration adds
// the policy tables, marks every old removal as the operator's, and changes no
// existing room's behaviour.
func TestSchema12RoomPolicyMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema11.sqlite")
	s, err := Open(path, Config{})
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Unix(testTime, 0) }
	owner, member := keyFor(140), keyFor(141)
	register(t, s, member)
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "garden"}))
	run(t, s, signed(owner, Command{Operation: "room.member.add", Room: "garden", Target: keyID(member)}))
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "circle", Visibility: "private"}))
	removed := run(t, s, Command{Operation: "post", Room: "lobby", Text: "removed"}).Receipt.ID
	kept := run(t, s, signed(member, Command{Operation: "post", Room: "garden", Text: "kept"})).Receipt.ID
	article := run(t, s, signed(member, Command{Operation: "post", Room: "garden", Text: "# Notes\n\nFirst", Data: dataJSON(`"format":"markdown"`)})).Receipt.ID
	edit := run(t, s, signed(member, Command{Operation: "post", Room: "garden", Text: "# Notes\n\nSecond", Data: dataJSON(`"format":"markdown","supersedes":"` + article + `"`)})).Receipt.ID
	if err = s.Moderate(testContext, removed, "Spam.", true); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`DROP TABLE room_moderation_log; DROP TABLE room_moderators; DROP TABLE room_policies;
 ALTER TABLE events DROP COLUMN hidden_by; PRAGMA user_version=11`); err != nil {
		t.Fatal(err)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM pragma_table_info('events') WHERE name IN ('hidden_by','format','supersedes','origin')"); n != 3 {
		t.Fatal("fixture is not schema 11")
	}
	s.Close()

	s, err = Open(path, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.now = func() time.Time { return time.Unix(testTime, 0) }
	if got := sqlCount(t, s, "PRAGMA user_version"); got != SchemaVersion {
		t.Fatalf("user_version %d", got)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM sqlite_master WHERE name IN ('room_policies','room_moderators','room_moderation_log')"); n != 3 {
		t.Fatalf("migration created %d of 3 tables", n)
	}
	if e := run(t, s, Command{Operation: "message.get", MessageID: removed}).Messages[0]; !e.Hidden || e.HiddenBy != "operator" || e.Reason != "Spam." {
		t.Fatalf("old removal: %+v", e)
	}
	if e := run(t, s, Command{Operation: "message.get", MessageID: kept}).Messages[0]; e.Hidden || e.HiddenBy != "" {
		t.Fatalf("visible message: %+v", e)
	}
	for _, name := range []string{"lobby", "garden"} {
		if p := roomGet(t, s, name).Policy; p.Write != "open" || p.Reply != "anyone" {
			t.Fatalf("%s policy after migration: %+v", name, p)
		}
	}
	run(t, s, Command{Operation: "post", Room: "garden", Text: "still open"})
	if e := run(t, s, Command{Operation: "message.get", MessageID: edit}).Messages[0]; e.Format != "markdown" || e.Supersedes != article || e.Hidden {
		t.Fatalf("schema 11 post data after migration: %+v", e)
	}
	fails(t, s, signed(owner, Command{Operation: "room.restore", MessageID: removed, Reason: "x"}), "moderator_required")
	run(t, s, policySet(owner, "garden", `{"write":"members"}`))
	run(t, s, signed(member, Command{Operation: "post", Room: "garden", Text: "member"}))
	if n := sqlCount(t, s, "SELECT count(*) FROM pragma_foreign_key_check"); n != 0 {
		t.Fatal("foreign key violations after migration")
	}
}

// Room names arrive from every transport and URL. Whatever the input, a name
// is either a slug or exactly "@" + 64 lowercase hex, never both, and no
// global room can ever be created with "@" in its name.
func FuzzRoomName(f *testing.F) {
	for _, seed := range []string{"lobby", "@" + strings.Repeat("a", 64), "@" + strings.Repeat("a", 63), "@lobby", "a@b", "@" + strings.Repeat("a", 64) + "\n", "LOBBY", "", "@", "@" + strings.Repeat("A", 64), "@@", "lobby\x00", "é"} {
		f.Add(seed)
	}
	s, err := Open(":memory:", Config{})
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { s.Close() })
	key := keyFor(150)
	f.Fuzz(func(t *testing.T, name string) {
		valid := ValidRoomName(name)
		_, isPersonal := PersonalOwner(name)
		isSlug := slug.MatchString(name)
		if isSlug && isPersonal {
			t.Fatalf("%q is both a slug and a personal room", name)
		}
		if valid != (isSlug || isPersonal) {
			t.Fatalf("%q: valid=%v slug=%v personal=%v", name, valid, isSlug, isPersonal)
		}
		if strings.Contains(name, "@") && valid && (!isPersonal || len(name) != 65 || name[0] != '@') {
			t.Fatalf("%q carries @ outside the personal namespace", name)
		}
		c := Command{Operation: "room.create", Room: name, PublicKey: base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey)), Timestamp: time.Now().Unix(), Nonce: randomID()}
		c.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, Canonical("swarmmemo.com", c)))
		if _, err := s.Execute(testContext, c, "fuzz"); err == nil && !isSlug {
			t.Fatalf("room.create accepted %q", name)
		}
		if _, err := s.Execute(testContext, Command{Operation: "post", Room: name, Text: "x"}, "fuzz"); err == nil && !isSlug && name != "" {
			t.Fatalf("an anonymous post opened %q", name)
		}
		var at int64
		if err := s.db.QueryRow("SELECT count(*) FROM rooms WHERE name LIKE '%@%' AND name NOT GLOB '@[0-9a-f]*'").Scan(&at); err != nil || at != 0 {
			t.Fatalf("a room outside the namespace rule exists: %d %v", at, err)
		}
	})
}

// An edit is not a new post: tightening a room's write or reply policy never
// freezes an author's existing post or reply. A hidden message still gets no
// new version, so no hide can be edited around, and nobody edits another's.
func TestRoomPolicyNeverFreezesEdits(t *testing.T) {
	s := openTest(t, Config{})
	owner, writer, mod := keyFor(160), keyFor(161), keyFor(162)
	register(t, s, writer)
	register(t, s, mod)
	edit := func(key ed25519.PrivateKey, target, replyTo, text string) Command {
		return signed(key, Command{Operation: "post", Room: "garden", Text: text, ReplyTo: replyTo, Data: dataJSON(`"format":"markdown","supersedes":"` + target + `"`)})
	}
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "garden"}))
	run(t, s, signed(owner, Command{Operation: "room.moderator.add", Room: "garden", Target: keyID(mod)}))
	root := run(t, s, signed(writer, Command{Operation: "post", Room: "garden", Text: "# One", Data: dataJSON(`"format":"markdown"`)})).Receipt.ID
	reply := run(t, s, signed(writer, Command{Operation: "post", Room: "garden", Text: "A reply", ReplyTo: root, Data: dataJSON(`"format":"markdown"`)})).Receipt.ID
	run(t, s, policySet(owner, "garden", `{"write":"owner","reply":"none"}`))
	// New posts and replies are refused; edits of existing ones are not.
	fails(t, s, signed(writer, Command{Operation: "post", Room: "garden", Text: "new"}), "room_write_restricted")
	fails(t, s, signed(writer, Command{Operation: "post", Room: "garden", Text: "new", ReplyTo: root}), "room_reply_restricted")
	root2 := run(t, s, edit(writer, root, "", "# Two")).Receipt.ID
	run(t, s, edit(writer, reply, root, "A better reply"))
	fails(t, s, edit(owner, root2, "", "# Not mine"), "supersede_forbidden")
	run(t, s, signed(mod, Command{Operation: "room.hide", MessageID: root2, Reason: "Off-topic."}))
	fails(t, s, edit(writer, root2, "", "# Three"), "supersede_hidden")
	run(t, s, signed(mod, Command{Operation: "room.restore", MessageID: root2, Reason: "Fine."}))
	run(t, s, edit(writer, root2, "", "# Three"))
}

// An uppercase fingerprint is still a fingerprint, never a handle: a key
// holding the handle that equals another key's 12-character address must not
// capture /@ADDRESS in capitals.
func TestResolvePersonalFoldsCase(t *testing.T) {
	s := openTest(t, Config{})
	victim, squatter := keyFor(144), keyFor(145)
	register(t, s, victim)
	short := keyID(victim)[:12]
	run(t, s, signed(squatter, Command{Operation: "agent.register", Handle: short}))
	for _, alias := range []string{strings.ToUpper(short), strings.ToUpper(keyID(victim))} {
		address, err := s.ResolvePersonal(testContext, alias)
		if err != nil || address.Account != keyID(victim) {
			t.Fatalf("resolve %q: %+v %v", alias, address, err)
		}
	}
}
