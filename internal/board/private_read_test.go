package board

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func privateReadEnrollCommand(s *Store, parent, child ed25519.PrivateKey, room string) Command {
	var epoch string
	if err := s.db.QueryRow("SELECT private_access_epoch FROM rooms WHERE name=?", room).Scan(&epoch); err != nil {
		panic(err)
	}
	data, _ := json.Marshal(map[string]any{"schema": 1, "generation": s.generation, "access_epoch": epoch, "disclosure": "private"})
	c := signed(parent, Command{Operation: "private_read.create", Room: room, Target: base64.RawURLEncoding.EncodeToString(child.Public().(ed25519.PublicKey)), Data: string(data), Timestamp: s.now().Unix()})
	c.Proof = base64.RawURLEncoding.EncodeToString(ed25519.Sign(child, Canonical(s.config.ServiceID, c)))
	return c
}
func privateReadEnroll(t *testing.T, s *Store, parent, child ed25519.PrivateKey, room string) *PrivateReadContext {
	t.Helper()
	run(t, s, privateReadEnrollCommand(s, parent, child, room))
	return &PrivateReadContext{Schema: 1, GrantID: keyID(child), Generation: s.generation}
}
func privateReadCommand(s *Store, key ed25519.PrivateKey, grant *PrivateReadContext, c Command) Command {
	copied := *grant
	c.PrivateRead = &copied
	c.Timestamp = s.now().Unix()
	return signed(key, c)
}
func privateReadRevoke(s *Store, parent ed25519.PrivateKey, room, childID string) Command {
	data, _ := json.Marshal(map[string]any{"schema": 1, "generation": s.generation})
	return signed(parent, Command{Operation: "private_read.revoke", Room: room, Target: childID, Data: string(data), Timestamp: s.now().Unix()})
}
func privateReadFixture(t *testing.T) (*Store, ed25519.PrivateKey, ed25519.PrivateKey, *PrivateReadContext) {
	t.Helper()
	s := openTest(t, Config{})
	parent, child := keyFor(181), keyFor(182)
	run(t, s, signed(parent, Command{Operation: "room.create", Room: "reader-room", Visibility: "private"}))
	return s, parent, child, privateReadEnroll(t, s, parent, child, "reader-room")
}
func TestPrivateReadCanonicalContext(t *testing.T) {
	grant := &PrivateReadContext{1, strings.Repeat("a", 64), strings.Repeat("b", 32)}
	c := Command{Operation: "room.get", Room: "secret", PrivateRead: grant}
	want := `{"version":3,"service":"swarmmemo.com","command":{"operation":"room.get","room":"secret","private_read":{"schema":1,"grant_id":"` + grant.GrantID + `","generation":"` + grant.Generation + `"}}}`
	if string(Canonical("swarmmemo.com", c)) != want {
		t.Fatal(string(Canonical("swarmmemo.com", c)))
	}
	raw, _ := json.Marshal(grant)
	for _, invalid := range []string{"null", "{}", "[]", strings.Replace(string(raw), `"schema":1`, `"schema":1.0`, 1), strings.Replace(string(raw), `"schema":1`, `"schema":true`, 1), strings.Replace(string(raw), `"schema":1`, `"schema":1,"schema":1`, 1), strings.Replace(string(raw), `"schema":1`, `"schema":1,"extra":0`, 1), strings.Replace(string(raw), grant.GrantID, strings.ToUpper(grant.GrantID), 1)} {
		var context PrivateReadContext
		if json.Unmarshal([]byte(invalid), &context) == nil {
			t.Fatalf("accepted %s", invalid)
		}
	}
	s, parent, child, ctx := privateReadFixture(t)
	command := privateReadCommand(s, child, ctx, Command{Operation: "room.get", Room: "reader-room", Delegation: &DelegationContext{1, keyID(child), s.generation}})
	fails(t, s, command, "invalid_private_read_context")
	command = signed(parent, Command{Operation: "private_read.list", Room: "reader-room", Delegation: &DelegationContext{1, keyID(parent), s.generation}})
	fails(t, s, command, "invalid_private_read_context")
}

func TestPrivateReadSharedCanonicalVectors(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/private_read_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Version int `json:"version"`
		Vectors []struct {
			Name       string  `json:"name"`
			Service    string  `json:"service"`
			Seed       string  `json:"seed_hex"`
			PublicKey  string  `json:"public_key"`
			Command    Command `json:"command"`
			Canonical  string  `json:"canonical"`
			Signature  string  `json:"signature"`
			TargetSeed string  `json:"target_seed_hex"`
			Proof      string  `json:"proof"`
		} `json:"vectors"`
	}
	if json.Unmarshal(raw, &fixture) != nil || fixture.Version != 1 || len(fixture.Vectors) != 6 {
		t.Fatal("shared fixture schema")
	}
	for _, v := range fixture.Vectors {
		t.Run(v.Name, func(t *testing.T) {
			canonical := Canonical(v.Service, v.Command)
			if string(canonical) != v.Canonical {
				t.Fatal("canonical bytes differ", string(canonical))
			}
			seed, err := hex.DecodeString(v.Seed)
			if err != nil {
				t.Fatal(err)
			}
			key := ed25519.NewKeyFromSeed(seed)
			if base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey)) != v.PublicKey || base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, canonical)) != v.Signature {
				t.Fatal("signature differs")
			}
			if v.TargetSeed != "" {
				seed, err = hex.DecodeString(v.TargetSeed)
				if err != nil {
					t.Fatal(err)
				}
				if base64.RawURLEncoding.EncodeToString(ed25519.Sign(ed25519.NewKeyFromSeed(seed), canonical)) != v.Proof {
					t.Fatal("proof differs")
				}
			}
		})
	}
}
func TestPrivateReadScopedAuthorityAndMinimalResponse(t *testing.T) {
	s, parent, child, grant := privateReadFixture(t)
	run(t, s, signed(parent, Command{Operation: "room.create", Room: "other-private", Visibility: "private"}))
	inside := run(t, s, signed(parent, Command{Operation: "post", Room: "reader-room", Text: "Private original <&> 雪"})).Receipt.ID
	outside := run(t, s, signed(parent, Command{Operation: "post", Room: "other-private", Text: "OTHER PRIVATE CANARY"})).Receipt.ID
	public := run(t, s, signed(parent, Command{Operation: "post", Room: "public-place", Text: "public"})).Receipt.ID
	read := func(c Command) Command { return privateReadCommand(s, child, grant, c) }
	room := run(t, s, read(Command{Operation: "room.get", Room: "reader-room"}))
	raw, _ := json.Marshal(room)
	if string(raw) != `{"ok":true,"data":{"private_room":{"name":"reader-room","visibility":"private"}}}` {
		t.Fatal(string(raw))
	}
	for _, id := range []string{outside, public, strings.Repeat("0", 32)} {
		fails(t, s, read(Command{Operation: "message.get", Room: "reader-room", MessageID: id}), "not_found")
	}
	fails(t, s, read(Command{Operation: "message.get", Room: "other-private", MessageID: outside}), "not_found")
	got := run(t, s, read(Command{Operation: "message.get", Room: "reader-room", MessageID: inside}))
	if len(got.Messages) != 1 || got.Generation != s.generation || got.Messages[0].Author != keyID(parent) || got.Messages[0].DelegationID != "" || got.Messages[0].ArchiveEligible || !strings.HasPrefix(got.Messages[0].SignedPayload, `{"version":1,`) {
		t.Fatal(got)
	}
	listed := run(t, s, read(Command{Operation: "messages.list", Room: "reader-room", Cursor: "start", Limit: 1}))
	if len(listed.Messages) != 1 || listed.Messages[0].ID != inside {
		t.Fatal(listed)
	}
	if err := s.Moderate(testContext, inside, "private moderation", true); err != nil {
		t.Fatal(err)
	}
	tombstone := run(t, s, read(Command{Operation: "message.get", Room: "reader-room", MessageID: inside})).Messages[0]
	if tombstone.Text != "" || tombstone.SignedPayload != "" || tombstone.Signature != "" || !tombstone.Hidden {
		t.Fatal(tombstone)
	}
	if sqlCount(t, s, "SELECT count(*) FROM identities WHERE id=?", keyID(child)) != 0 || sqlCount(t, s, "SELECT count(*) FROM members WHERE account=?", keyID(child)) != 0 || sqlCount(t, s, "SELECT count(*) FROM requests WHERE actor=?", keyID(child)) != 0 {
		t.Fatal("child persisted as ordinary authority")
	}
}
func TestPrivateReadForbiddenFieldsAndKeyClasses(t *testing.T) {
	s, parent, child, grant := privateReadFixture(t)
	for _, op := range []string{"post", "stats", "rooms.list", "thread.get", "quota.get", "blob.get", "agent.register", "delegation.get", "private_read.list"} {
		fails(t, s, privateReadCommand(s, child, grant, Command{Operation: op, Room: "reader-room"}), "not_found")
	}
	for _, command := range []Command{{Operation: "messages.list", Page: "main"}, {Operation: "messages.list", Query: "secret"}, {Operation: "messages.list", To: keyID(parent)}, {Operation: "messages.list", Limit: 101}, {Operation: "room.get", RequestID: "forbidden"}, {Operation: "room.get", Amount: 1}} {
		command.Room = "reader-room"
		code := "invalid_private_read_data"
		if command.Limit == 101 {
			code = "invalid_limit"
		}
		fails(t, s, privateReadCommand(s, child, grant, command), code)
	}
	for _, op := range []string{"post", "agent.register", "stats", "quota.get"} {
		fails(t, s, signed(child, Command{Operation: op}), "not_found")
	}
	fails(t, s, privateReadCommand(s, keyFor(183), grant, Command{Operation: "room.get", Room: "reader-room"}), "not_found")
	rotation := signed(parent, Command{Operation: "agent.rotate", Target: base64.RawURLEncoding.EncodeToString(child.Public().(ed25519.PublicKey))})
	rotation.Proof = base64.RawURLEncoding.EncodeToString(ed25519.Sign(child, Canonical(s.config.ServiceID, rotation)))
	fails(t, s, rotation, "agent_exists")
	run(t, s, signed(parent, Command{Operation: "room.create", Room: "public-grant", Visibility: "public"}))
	fails(t, s, grantCommand(s, parent, child, "public-grant", 3600, 10000, []string{"post"}), "delegation_exists")
	publicChild := keyFor(184)
	enroll(t, s, parent, publicChild, "public-grant", 3600, 10000)
	fails(t, s, privateReadEnrollCommand(s, parent, publicChild, "reader-room"), "private_read_exists")
	fails(t, s, privateReadEnrollCommand(s, parent, parent, "reader-room"), "private_read_exists")
	fails(t, s, signed(publicChild, Command{Operation: "private_read.list", Room: "reader-room"}), "not_found")
	run(t, s, privateReadRevoke(s, parent, "reader-room", keyID(child)))
	fails(t, s, signed(child, Command{Operation: "agent.register"}), "not_found")
	fails(t, s, privateReadEnrollCommand(s, parent, child, "reader-room"), "private_read_exists")
}
func TestPrivateReadOwnerProofReplayAndLifecycle(t *testing.T) {
	s, parent, child, grant := privateReadFixture(t)
	other := keyFor(185)
	register(t, s, other)
	for _, c := range []Command{{Operation: "private_read.list", Room: "reader-room"}, {Operation: "private_read.get", Room: "reader-room", Target: keyID(child)}, {Operation: "private_read.list", Room: "absent"}} {
		fails(t, s, signed(other, c), "not_found")
	}
	creation := privateReadEnrollCommand(s, parent, keyFor(186), "reader-room")
	creation.RequestID = "durable-create"
	creation = signed(parent, creation)
	creation.Proof = base64.RawURLEncoding.EncodeToString(ed25519.Sign(keyFor(186), Canonical(s.config.ServiceID, creation)))
	first := run(t, s, creation)
	bad := creation
	bad.Proof = strings.Repeat("a", 86)
	fails(t, s, bad, "invalid_private_read_proof")
	var firstJSON, replayJSON any
	_ = json.Unmarshal(jsonBytes(first), &firstJSON)
	_ = json.Unmarshal(jsonBytes(run(t, s, creation)), &replayJSON)
	if !reflect.DeepEqual(firstJSON, replayJSON) {
		t.Fatal("cached ack changed")
	}
	revoke := privateReadRevoke(s, parent, "reader-room", keyID(child))
	run(t, s, revoke)
	fails(t, s, privateReadCommand(s, child, grant, Command{Operation: "room.get", Room: "reader-room"}), "not_found")
	fails(t, s, privateReadRevoke(s, parent, "reader-room", keyID(child)), "private_read_already_revoked")
	run(t, s, revoke)
	record := run(t, s, signed(parent, Command{Operation: "private_read.get", Room: "reader-room", Target: keyID(child)})).Data["private_read_grant"].(PrivateReadRecord)
	if record.State != "revoked" || record.Revocation.SignedPayload == "" || record.Enrollment.Proof == "" {
		t.Fatal(record)
	}
	now := testTime + 8*86400
	s.now = func() time.Time { return time.Unix(now, 0) }
	run(t, s, creation)
	run(t, s, revoke)
	fails(t, s, privateReadCommand(s, keyFor(186), &PrivateReadContext{1, keyID(keyFor(186)), s.generation}, Command{Operation: "room.get", Room: "reader-room"}), "not_found")
}
func jsonBytes(value any) []byte { raw, _ := json.Marshal(value); return raw }
func TestPrivateReadMemberEpochAndRotation(t *testing.T) {
	s, parent, child, grant := privateReadFixture(t)
	member := keyFor(187)
	register(t, s, member)
	epoch := func() string {
		var value string
		if err := s.db.QueryRow("SELECT private_access_epoch FROM rooms WHERE name='reader-room'").Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	original := epoch()
	run(t, s, signed(parent, Command{Operation: "room.member.remove", Room: "reader-room", Target: keyID(member)}))
	if epoch() != original {
		t.Fatal("absent deletion changed epoch")
	}
	run(t, s, signed(parent, Command{Operation: "room.member.add", Room: "reader-room", Target: keyID(member)}))
	remove := signed(parent, Command{Operation: "room.member.remove", Room: "reader-room", Target: keyID(member)})
	run(t, s, remove)
	updated := epoch()
	if updated == original {
		t.Fatal("actual removal did not invalidate")
	}
	run(t, s, remove)
	if epoch() != updated {
		t.Fatal("replay changed epoch")
	}
	run(t, s, signed(parent, Command{Operation: "room.member.add", Room: "reader-room", Target: keyID(member)}))
	fails(t, s, privateReadCommand(s, child, grant, Command{Operation: "room.get", Room: "reader-room"}), "not_found")
	newChild := keyFor(188)
	newGrant := privateReadEnroll(t, s, parent, newChild, "reader-room")
	next := keyFor(189)
	rotation := signed(parent, Command{Operation: "agent.rotate", Target: base64.RawURLEncoding.EncodeToString(next.Public().(ed25519.PublicKey))})
	rotation.Proof = base64.RawURLEncoding.EncodeToString(ed25519.Sign(next, Canonical(s.config.ServiceID, rotation)))
	run(t, s, rotation)
	fails(t, s, privateReadCommand(s, newChild, newGrant, Command{Operation: "room.get", Room: "reader-room"}), "not_found")
	record := run(t, s, signed(next, Command{Operation: "private_read.get", Room: "reader-room", Target: keyID(newChild)})).Data["private_read_grant"].(PrivateReadRecord)
	if record.State != "issuer_rotated" || record.IssuerID != keyID(parent) {
		t.Fatal(record)
	}
	run(t, s, privateReadRevoke(s, next, "reader-room", keyID(newChild)))
}
func TestPrivateReadSchemaMigrationAndRecovery(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "migration.sqlite")
	s, err := Open(path, Config{})
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Unix(testTime, 0) }
	parent := keyFor(190)
	run(t, s, signed(parent, Command{Operation: "room.create", Room: "migrated", Visibility: "private"}))
	if _, err = s.db.Exec("DROP TABLE private_read_grants; ALTER TABLE rooms DROP COLUMN private_access_epoch; PRAGMA user_version=7"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.now = func() time.Time { return time.Unix(testTime, 0) }
	var epoch string
	s.db.QueryRow("SELECT private_access_epoch FROM rooms WHERE name='migrated'").Scan(&epoch)
	if !workIDRE.MatchString(epoch) || sqlCount(t, s, "PRAGMA user_version") != 9 {
		t.Fatal("migration")
	}
	child := keyFor(191)
	grant := privateReadEnroll(t, s, parent, child, "migrated")
	if err = s.RotateGeneration(testContext); err != nil {
		t.Fatal(err)
	}
	fails(t, s, privateReadCommand(s, child, grant, Command{Operation: "room.get", Room: "migrated"}), "not_found")
	record := run(t, s, signed(parent, Command{Operation: "private_read.get", Room: "migrated", Target: keyID(child)})).Data["private_read_grant"].(PrivateReadRecord)
	if record.State != "epoch_disabled" {
		t.Fatal(record)
	}
	ack := run(t, s, privateReadRevoke(s, parent, "migrated", keyID(child))).Data["ack"].(PrivateReadAck)
	if ack.Generation == ack.GrantGeneration {
		t.Fatal("recovery acknowledgment lost original generation")
	}
}
func TestPrivateReadRateAndConcurrencyAdmission(t *testing.T) {
	s, _, child, grant := privateReadFixture(t)
	read := func() Command {
		return privateReadCommand(s, child, grant, Command{Operation: "room.get", Room: "reader-room"})
	}
	for i := 0; i < 10; i++ {
		run(t, s, read())
	}
	fails(t, s, read(), "private_read_rate_limited")
	if len(s.privateRates) != 3 {
		t.Fatal("unexpected bucket entries")
	}
	for i := 0; i < 3; i++ {
		fails(t, s, privateReadCommand(s, keyFor(byte(192+i)), grant, Command{Operation: "room.get", Room: "reader-room"}), "not_found")
	}
	if len(s.privateRates) != 3 {
		t.Fatal("unknown keys allocated rate state")
	}
	s.now = func() time.Time { return time.Unix(testTime+1, 0) }
	run(t, s, read())
	s.privateSlots <- struct{}{}
	s.privateSlots <- struct{}{}
	fails(t, s, read(), "private_read_rate_limited")
	<-s.privateSlots
	<-s.privateSlots
	// Aggregate rejection leaves the eligible child bucket unchanged.
	s.privateRates["child:"+keyID(child)] = privateReadBucket{tokens: 10, updated: s.now(), seen: s.now()}
	s.privateServiceRate = privateReadBucket{tokens: 0, updated: s.now(), seen: s.now()}
	fails(t, s, read(), "private_read_rate_limited")
	if s.privateRates["child:"+keyID(child)].tokens != 10 {
		t.Fatal("partial token debit")
	}
}
func TestPrivateReadCompleteResponseBound(t *testing.T) {
	s, parent, child, grant := privateReadFixture(t)
	for i := 0; i < 40; i++ {
		run(t, s, signed(parent, Command{Operation: "post", Room: "reader-room", Text: strings.Repeat("<", 16384)}))
	}
	read := func(limit int) Command {
		return privateReadCommand(s, child, grant, Command{Operation: "messages.list", Room: "reader-room", Cursor: "start", Limit: limit})
	}
	fails(t, s, read(40), "private_read_response_limit")
	result := run(t, s, read(1))
	if len(result.Messages) != 1 || len(result.Messages[0].Text) != 16384 {
		t.Fatal("complete single event lost")
	}
	if err := privateReadBound(Result{OK: true}, len(jsonBytes(Result{OK: true}))); err == nil {
		t.Fatal("final newline not counted")
	}
}

func TestPrivateReadEmptyIdleAndOwnerPagination(t *testing.T) {
	s, parent, child, grant := privateReadFixture(t)
	for _, cursor := range []string{"", "start"} {
		first := run(t, s, privateReadCommand(s, child, grant, Command{Operation: "messages.list", Room: "reader-room", Cursor: cursor}))
		if len(first.Messages) != 0 || first.NextCursor == "" || first.Generation != s.generation {
			t.Fatal(first)
		}
		idle := run(t, s, privateReadCommand(s, child, grant, Command{Operation: "messages.list", Room: "reader-room", Cursor: first.NextCursor}))
		if idle.NextCursor != first.NextCursor || len(idle.Messages) != 0 {
			t.Fatal("idle cursor changed", idle)
		}
	}
	privateReadEnroll(t, s, parent, keyFor(195), "reader-room")
	first := run(t, s, signed(parent, Command{Operation: "private_read.list", Room: "reader-room", Limit: 1}))
	if first.Data["has_more"] != true || first.NextCursor == "" {
		t.Fatal(first)
	}
	second := run(t, s, signed(parent, Command{Operation: "private_read.list", Room: "reader-room", Limit: 1, Cursor: first.NextCursor}))
	a := first.Data["private_read_grants"].([]PrivateReadSummary)
	b := second.Data["private_read_grants"].([]PrivateReadSummary)
	if len(a) != 1 || len(b) != 1 || a[0].GrantID >= b[0].GrantID || second.Data["has_more"] != false {
		t.Fatal("lexical paging")
	}
	run(t, s, signed(parent, Command{Operation: "room.create", Room: "cursor-other", Visibility: "private"}))
	fails(t, s, signed(parent, Command{Operation: "private_read.list", Room: "cursor-other", Cursor: first.NextCursor}), "invalid_cursor")
}
