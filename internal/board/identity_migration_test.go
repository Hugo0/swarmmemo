package board

import (
	"path/filepath"
	"testing"
	"time"
)

// Directory visibility is decided by matching operation names in the audit table,
// so the schema 8->9 vocabulary rename is only safe if the historical audit rows
// are rewritten with it. Production carried identity.register and peer.publish
// rows; if those were left behind, every agent whose only claim to publicity was
// an explicit registration or profile publication would disappear from /agents
// while still existing. This seeds exactly that shape and proves it survives.
func TestSchema9AuditVocabularyMigrationKeepsDirectoryVisible(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vocabulary.sqlite")
	s, err := Open(path, Config{})
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Unix(testTime, 0) }

	registrant := keyFor(91)  // public only through an explicit registration
	publisher := keyFor(92)   // public only through an explicit profile publication
	privateOnly := keyFor(93) // never registered, never published: must stay hidden

	run(t, s, signed(registrant, Command{Operation: "agent.register", Handle: "schema9-registrant"}))
	run(t, s, signed(publisher, Command{Operation: "agent.register", Handle: "schema9-publisher"}))
	run(t, s, signed(publisher, Command{Operation: "agent.profile.publish", Data: testPeerData}))
	// A key that exists but has no public claim at all.
	run(t, s, signed(privateOnly, Command{Operation: "quota.get"}))

	// Rewind to the exact production shape: schema 8 audit rows in the old vocabulary.
	if _, err = s.db.Exec(`UPDATE audit SET operation='identity.register' WHERE operation='agent.register';
 UPDATE audit SET operation='identity.rotate' WHERE operation='agent.rotate';
 UPDATE audit SET operation='peer.publish' WHERE operation='agent.profile.publish';
 UPDATE audit SET operation='peer.remove' WHERE operation='agent.profile.remove';
 PRAGMA user_version=8`); err != nil {
		t.Fatal(err)
	}
	var legacy int64
	if err = s.db.QueryRow("SELECT count(*) FROM audit WHERE operation IN ('identity.register','peer.publish')").Scan(&legacy); err != nil {
		t.Fatal(err)
	}
	if legacy != 3 {
		t.Fatalf("fixture did not seed the old vocabulary: %d legacy audit rows", legacy)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(path, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.now = func() time.Time { return time.Unix(testTime, 0) }

	if got := sqlCount(t, s, "PRAGMA user_version"); got != 9 {
		t.Fatalf("schema version after migration: %d", got)
	}
	listed := map[string]bool{}
	for _, agent := range run(t, s, Command{Operation: "agents.list", Limit: 100}).Agents {
		listed[agent.ID] = true
	}
	if !listed[keyID(registrant)] {
		t.Fatal("an agent made public by a pre-9 identity.register vanished from the directory")
	}
	if !listed[keyID(publisher)] {
		t.Fatal("an agent made public by a pre-9 peer.publish vanished from the directory")
	}
	if listed[keyID(privateOnly)] {
		t.Fatal("migration leaked a private-only key into the directory")
	}
	for _, target := range []string{keyID(registrant), "schema9-registrant", keyID(publisher)} {
		res := run(t, s, Command{Operation: "agent.get", Target: target})
		if res.Agent == nil {
			t.Fatalf("agent.get %q lost its pre-9 public claim", target)
		}
	}
	fails(t, s, Command{Operation: "agent.get", Target: keyID(privateOnly)}, "not_found")
	if got := sqlCount(t, s, "SELECT count(*) FROM audit WHERE operation IN ('identity.register','identity.rotate','peer.publish','peer.remove')"); got != 0 {
		t.Fatalf("%d audit rows kept the old vocabulary", got)
	}
	if got := sqlCount(t, s, "SELECT count(*) FROM audit WHERE operation='agent.register'"); got != 2 {
		t.Fatalf("agent.register audit rows after migration: %d", got)
	}
	if got := sqlCount(t, s, "SELECT count(*) FROM audit WHERE operation='agent.profile.publish'"); got != 1 {
		t.Fatalf("agent.profile.publish audit rows after migration: %d", got)
	}

	// The profile itself must still ride along with its agent after the rename.
	if getPeer(t, s, keyID(publisher)).CurrentAgent.ID != keyID(publisher) {
		t.Fatal("published profile lost its agent after migration")
	}
}
