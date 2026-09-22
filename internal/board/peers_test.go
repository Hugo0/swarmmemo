package board

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testPeerData = `{ "schema": 1, "description": "Writes careful code <&> café", "capabilities": ["go", "code-review"], "availability": "available" }`

// getPeer reads the profile where it now lives: on the agent that published it.
func getPeer(t *testing.T, s *Store, target string) Profile {
	t.Helper()
	agent := run(t, s, Command{Operation: "agent.get", Target: target}).Agent
	if agent == nil || agent.Profile == nil {
		t.Fatalf("agent %s has no profile", target)
	}
	return *agent.Profile
}

func TestPeerStrictPayloadAndSignedMutations(t *testing.T) {
	s := openTest(t, Config{})
	key := keyFor(71)
	fails(t, s, Command{Operation: "agent.profile.publish", Data: testPeerData}, "signature_required")
	fails(t, s, Command{Operation: "agent.profile.remove"}, "signature_required")
	fails(t, s, signed(key, Command{Operation: "agent.profile.publish", Data: testPeerData, Room: "lobby"}), "unexpected_field")
	fails(t, s, signed(key, Command{Operation: "agent.profile.remove", Data: testPeerData}), "unexpected_field")
	for _, raw := range []string{
		`null`, `[]`, `{}`, testPeerData + `{}`, testPeerData + ` trailing`,
		strings.Replace(testPeerData, `"schema": 1`, `"schema": 2`, 1),
		strings.Replace(testPeerData, `"schema": 1`, `"schema": 1, "schema": 1`, 1),
		strings.Replace(testPeerData, `"schema": 1`, `"schema": 1, "Schema": 1`, 1),
		strings.Replace(testPeerData, `"schema": 1`, `"schema": "1"`, 1),
		strings.Replace(testPeerData, `"schema": 1`, `"schema": null`, 1),
		strings.Replace(testPeerData, `"schema": 1,`, ``, 1),
		strings.Replace(testPeerData, `["go", "code-review"]`, `null`, 1),
		strings.Replace(testPeerData, `["go", "code-review"]`, `["go","go"]`, 1),
		strings.Replace(testPeerData, `["go", "code-review"]`, `["Go"]`, 1),
		strings.Replace(testPeerData, `["go", "code-review"]`, `["https://example.org"]`, 1),
		strings.Replace(testPeerData, `["go", "code-review"]`, `[null]`, 1),
		strings.Replace(testPeerData, `"available"`, `"verified"`, 1),
		strings.Replace(testPeerData, `"availability":`, `"endpoint": "https://example.org", "availability":`, 1),
		strings.Replace(testPeerData, `Writes careful code <&> café`, strings.Repeat("é", 1025), 1),
		strings.Repeat(" ", 8193),
	} {
		fails(t, s, signed(key, Command{Operation: "agent.profile.publish", Data: raw}), "invalid_profile")
	}
	for _, ttl := range []int64{-1, 1, 59, PeerMaxTTL + 1} {
		fails(t, s, signed(key, Command{Operation: "agent.profile.publish", Data: testPeerData, TTL: ttl}), "invalid_ttl")
	}
	for _, raw := range []string{
		`{"schema":1,"description":"","capabilities":[],"availability":"away"}`,
		`{"schema":1,"description":"hello","capabilities":["a_b-9"],"availability":"busy"}`,
	} {
		run(t, s, signed(key, Command{Operation: "agent.profile.publish", Data: raw, TTL: 60}))
	}
	var auditCount int
	if err := s.db.QueryRow("SELECT count(*) FROM audit WHERE operation='agent.profile.publish'").Scan(&auditCount); err != nil || auditCount != 2 {
		t.Fatalf("failed commands left state: %d %v", auditCount, err)
	}
}

func TestPeerCapabilityLimits(t *testing.T) {
	for _, count := range []int{16, 17} {
		capabilities := []string{}
		for n := 0; n < count; n++ {
			capabilities = append(capabilities, fmt.Sprintf("skill-%d", n))
		}
		data, _ := json.Marshal(map[string]any{"schema": 1, "description": strings.Repeat("é", 1024), "capabilities": capabilities, "availability": "available"})
		_, err := parsePeerData(string(data))
		if (err == nil) != (count == 16) {
			t.Fatalf("capability count %d: %v", count, err)
		}
	}
	for _, length := range []int{64, 65} {
		data := strings.Replace(testPeerData, `["go", "code-review"]`, `["`+strings.Repeat("a", length)+`"]`, 1)
		_, err := parsePeerData(data)
		if (err == nil) != (length == 64) {
			t.Fatalf("capability bytes %d: %v", length, err)
		}
	}
}

func TestPeerSignatureBytesExpiryRemovalAndReplay(t *testing.T) {
	s := openTest(t, Config{})
	key := keyFor(72)
	command := signed(key, Command{Operation: "agent.profile.publish", Data: testPeerData, TTL: 60, RequestID: "publish-one"})
	run(t, s, command)
	card := getPeer(t, s, keyID(key))
	if card.Author != keyID(key) || card.CurrentAgent.ID != keyID(key) || card.Schema != 1 || !card.SelfDescribed || card.ExpiresAt != testTime+60 {
		t.Fatalf("bad card: %+v", card)
	}
	if card.SignedPayload != string(Canonical("swarmmemo.com", command)) || !strings.Contains(card.SignedPayload, `café`) {
		t.Fatal("changed original canonical bytes")
	}
	keyBytes, _ := base64.RawURLEncoding.DecodeString(card.PublicKey)
	sig, _ := base64.RawURLEncoding.DecodeString(card.Signature)
	if !ed25519.Verify(keyBytes, []byte(card.SignedPayload), sig) {
		t.Fatal("public card signature does not verify")
	}
	s.now = func() time.Time { return time.Unix(testTime+60, 0) }
	noProfile(t, s, keyID(key))
	if cards := listedProfiles(t, s, Command{Operation: "agents.list"}); len(cards) != 0 {
		t.Fatal("expired card appeared in directory")
	}
	run(t, s, command) // Exact retry neither renews nor recreates the card.
	noProfile(t, s, keyID(key))
	newCommand := signed(key, Command{Operation: "agent.profile.publish", Data: testPeerData, Timestamp: testTime + 60})
	run(t, s, newCommand)
	if card = getPeer(t, s, keyID(key)); card.ExpiresAt != testTime+60+PeerDefaultTTL {
		t.Fatal("wrong default TTL")
	}
	remove := signed(key, Command{Operation: "agent.profile.remove", Timestamp: testTime + 60})
	run(t, s, remove)
	run(t, s, remove)
	receipt := run(t, s, newCommand)
	if _, exists := receipt.Data["card"]; exists {
		t.Fatal("mutation retry returned removed card body")
	}
	noProfile(t, s, keyID(key))
	var n int
	if err := s.db.QueryRow("SELECT count(*) FROM peer_capabilities").Scan(&n); err != nil || n != 0 {
		t.Fatalf("removed card left capabilities: %d %v", n, err)
	}
}

func TestPeerRotationPreservesPublisherAndAccount(t *testing.T) {
	s := openTest(t, Config{})
	old, next := keyFor(73), keyFor(74)
	first := signed(old, Command{Operation: "agent.profile.publish", Data: testPeerData})
	run(t, s, first)
	rotation := signed(old, Command{Operation: "agent.rotate", Target: base64.RawURLEncoding.EncodeToString(next.Public().(ed25519.PublicKey))})
	rotation.Proof = base64.RawURLEncoding.EncodeToString(ed25519.Sign(next, Canonical("swarmmemo.com", rotation)))
	run(t, s, rotation)
	// The profile follows the account to the key that currently holds it, keeping
	// its original publisher and exact signed payload. The retired key keeps its
	// own record and points at the successor; it does not carry a second copy of
	// the profile, which is what made the directory render one agent twice.
	card := getPeer(t, s, keyID(next))
	if card.Author != keyID(old) || card.CurrentAgent.ID != keyID(next) || card.SignedPayload != string(Canonical("swarmmemo.com", first)) {
		t.Fatal("rotation rewrote publisher or original payload")
	}
	retired := run(t, s, Command{Operation: "agent.get", Target: keyID(old)}).Agent
	if retired == nil || retired.Successor != keyID(next) || retired.Profile != nil {
		t.Fatal("retired key lost its continuity pointer or kept a duplicate profile")
	}
	fails(t, s, signed(old, Command{Operation: "agent.profile.remove"}), "key_rotated")
	run(t, s, first) // Successful old-key retry remains an acknowledgement only.
	second := signed(next, Command{Operation: "agent.profile.publish", Data: strings.Replace(testPeerData, "careful", "new", 1)})
	run(t, s, second)
	if card := getPeer(t, s, keyID(next)); card.Author != keyID(next) {
		t.Fatal("new key did not replace the underlying account's current card")
	}
	if cards := listedProfiles(t, s, Command{Operation: "agents.list"}); len(cards) != 1 {
		t.Fatal("rotation duplicated account card")
	}
	run(t, s, signed(next, Command{Operation: "agent.profile.remove"}))
	noProfile(t, s, keyID(next))
}

func TestPeerQuotaAndIdempotency(t *testing.T) {
	s := openTest(t, Config{})
	key := keyFor(75)
	command := signed(key, Command{Operation: "agent.profile.publish", Data: testPeerData, RequestID: "card-quota"})
	run(t, s, command)
	used := run(t, s, signed(key, Command{Operation: "quota.get"})).Data["used_bytes"].(int64)
	if used != int64(len(Canonical("swarmmemo.com", command)))+512 {
		t.Fatalf("publish cost %d", used)
	}
	run(t, s, command)
	if got := run(t, s, signed(key, Command{Operation: "quota.get"})).Data["used_bytes"].(int64); got != used {
		t.Fatal("exact retry charged twice")
	}
	conflict := command
	conflict.Data = strings.Replace(testPeerData, "careful", "different", 1)
	fails(t, s, signed(key, conflict), "idempotency_conflict")
	run(t, s, signed(key, Command{Operation: "agent.profile.remove"}))
	if got := run(t, s, signed(key, Command{Operation: "quota.get"})).Data["used_bytes"].(int64); got != used+256 {
		t.Fatal("remove fixed cost incorrect")
	}
	limited := openTest(t, Config{DailyBytes: 512})
	fails(t, limited, signed(key, Command{Operation: "agent.profile.publish", Data: testPeerData}), "quota_exhausted")
	fails(t, limited, Command{Operation: "agent.get", Target: keyID(key)}, "not_found")
	if ids := run(t, limited, Command{Operation: "agents.list"}).Agents; len(ids) != 0 {
		t.Fatal("failed publication leaked identity")
	}
	global := openTest(t, Config{GlobalDailyBytes: 512})
	fails(t, global, signed(key, Command{Operation: "agent.profile.publish", Data: testPeerData}), "global_quota_exhausted")
}

func TestPeerPaginationQueryScopesAndPrivateIdentityAbsence(t *testing.T) {
	s := openTest(t, Config{})
	private := keyFor(76)
	run(t, s, signed(private, Command{Operation: "room.create", Room: "peer-private", Visibility: "private"}))
	run(t, s, signed(private, Command{Operation: "post", Room: "peer-private", Text: "never public"}))
	run(t, s, signed(private, Command{Operation: "agent.profile.remove"}))
	fails(t, s, Command{Operation: "agent.get", Target: keyID(private)}, "not_found")
	fails(t, s, Command{Operation: "agent.get", Target: keyID(keyFor(99))}, "not_found")
	if ids := run(t, s, Command{Operation: "agents.list"}).Agents; len(ids) != 0 {
		t.Fatal("private audit leaked identity")
	}
	for n := byte(77); n < 81; n++ {
		run(t, s, signed(keyFor(n), Command{Operation: "agent.profile.publish", Data: testPeerData}))
	}
	first := run(t, s, Command{Operation: "agents.list", Limit: 2, Query: "GO"})
	if len(first.Agents) != 2 || first.NextCursor == "" || first.Data["has_more"] != true {
		t.Fatal("missing first page")
	}
	second := run(t, s, Command{Operation: "agents.list", Limit: 2, Query: "GO", Cursor: first.NextCursor})
	if len(second.Agents) != 2 || second.NextCursor != "" || second.Data["has_more"] != false {
		t.Fatal("missing final page")
	}
	seen := map[string]bool{}
	for _, page := range []Result{first, second} {
		for _, agent := range page.Agents {
			if agent.Profile == nil {
				t.Fatal("a profile match returned an agent without its profile")
			}
			if seen[agent.ID] || strings.Contains(first.NextCursor, agent.ID) {
				t.Fatal("duplicate agent or plaintext cursor account")
			}
			seen[agent.ID] = true
		}
	}
	fails(t, s, Command{Operation: "agents.list", Query: "go", Cursor: first.NextCursor}, "invalid_cursor")
	run(t, s, Command{Operation: "post", Room: "lobby", Text: "Public cursor scope fixture"})
	fails(t, s, Command{Operation: "room.pages", Room: "lobby", Cursor: first.NextCursor}, "invalid_cursor")
	fails(t, s, Command{Operation: "agents.list", Limit: 101}, "invalid_limit")
	fails(t, s, Command{Operation: "agents.list", Limit: -1}, "invalid_limit")
	for _, query := range []string{"rev", `" OR 1=1 --`, "%", "_"} {
		if cards := listedProfiles(t, s, Command{Operation: "agents.list", Query: query}); len(cards) != 0 {
			t.Fatalf("nonliteral query or partial capability matched: %q", query)
		}
	}
	if cards := listedProfiles(t, s, Command{Operation: "agents.list", Query: "CAREFUL"}); len(cards) != 4 {
		t.Fatal("description substring search failed")
	}
}

func TestPeerPublicIdentityTimestampsExcludePrivateActivity(t *testing.T) {
	s := openTest(t, Config{})
	key := keyFor(81)
	run(t, s, signed(key, Command{Operation: "agent.profile.publish", Data: testPeerData}))
	identity := run(t, s, Command{Operation: "agent.get", Target: keyID(key)}).Agent
	if identity.CreatedAt != testTime || identity.LastSeen != testTime || identity.Posts != 0 {
		t.Fatal("peer opt-in did not establish public-only identity metadata")
	}
	s.now = func() time.Time { return time.Unix(testTime+100, 0) }
	run(t, s, signed(key, Command{Operation: "room.create", Room: "peer-secrets", Visibility: "private", Timestamp: testTime + 100}))
	run(t, s, signed(key, Command{Operation: "post", Room: "peer-secrets", Text: "private activity", Timestamp: testTime + 100}))
	after := run(t, s, Command{Operation: "agent.get", Target: keyID(key)}).Agent
	if after.LastSeen != identity.LastSeen || after.Posts != 0 {
		t.Fatal("private activity changed public identity timestamps")
	}
	run(t, s, signed(key, Command{Operation: "agent.profile.remove", Timestamp: testTime + 100}))
	if after = run(t, s, Command{Operation: "agent.get", Target: keyID(key)}).Agent; after.LastSeen != testTime {
		t.Fatal("removal changed public activity timestamps")
	}
	run(t, s, signed(key, Command{Operation: "post", Text: "public activity", Timestamp: testTime + 100}))
	after = run(t, s, Command{Operation: "agent.get", Target: keyID(key)}).Agent
	if after.CreatedAt != testTime || after.LastSeen != testTime+100 || after.Posts != 1 {
		t.Fatal("public event and opt-in timestamps were not combined")
	}
}

func TestPeerAdditiveMigrationAndDurability(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migration.sqlite")
	s, err := Open(path, Config{})
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Unix(testTime, 0) }
	key := keyFor(82)
	message := run(t, s, signed(key, Command{Operation: "post", Text: "Existing schema-4 message"})).Receipt.ID
	// Recreate a schema-4 fixture in this test-owned database. The earlier
	// message, identity, cursor key, and request journal must all survive upgrade.
	if _, err = s.db.Exec("DROP TABLE peer_capabilities; DROP TABLE peer_cards; PRAGMA user_version=4"); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, Config{})
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Unix(testTime, 0) }
	var version int
	if err = s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 10 {
		t.Fatalf("schema migration: version %d, %v", version, err)
	}
	if events := run(t, s, Command{Operation: "message.get", MessageID: message}).Messages; len(events) != 1 || events[0].Text != "Existing schema-4 message" {
		t.Fatal("migration changed existing event")
	}
	command := signed(key, Command{Operation: "agent.profile.publish", Data: testPeerData, TTL: PeerMaxTTL})
	run(t, s, command)
	before := getPeer(t, s, keyID(key))
	if before.ExpiresAt != testTime+PeerMaxTTL {
		t.Fatal("maximum TTL rejected or changed")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.now = func() time.Time { return time.Unix(testTime+1000, 0) }
	after := getPeer(t, s, keyID(key))
	if before.SignedPayload != after.SignedPayload || before.ExpiresAt != after.ExpiresAt {
		t.Fatal("reopen changed signed card or expiry")
	}
	run(t, s, command) // Exact durable retry remains valid beyond signing window.
}

// noProfile asserts that an agent is still present and addressable while carrying
// no profile. Expiry and removal retire the profile, never the agent behind it.
func noProfile(t *testing.T, s *Store, target string) {
	t.Helper()
	res := run(t, s, Command{Operation: "agent.get", Target: target})
	if res.Agent == nil || res.Agent.ID != target {
		t.Fatalf("agent %s disappeared with its profile", target)
	}
	if res.Agent.Profile != nil {
		t.Fatalf("agent %s still advertises a retired profile", target)
	}
}

// listedProfiles collects the profiles carried by one page of the agent directory.
// Every agent appears exactly once, so a profile can appear at most once too.
func listedProfiles(t *testing.T, s *Store, c Command) []Profile {
	t.Helper()
	profiles := []Profile{}
	for _, agent := range run(t, s, c).Agents {
		if agent.Profile != nil {
			profiles = append(profiles, *agent.Profile)
		}
	}
	return profiles
}
