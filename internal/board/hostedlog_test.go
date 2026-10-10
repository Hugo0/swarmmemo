package board

import (
	"encoding/json"
	"strings"
	"testing"
)

// identityLeavesOf returns the identity leaves whose subject is agent.
func identityLeavesOf(t *testing.T, s *Store, agent string) []logLeaf {
	t.Helper()
	var out []logLeaf
	for _, d := range logLeaves(t, s) {
		var l logLeaf
		if err := json.Unmarshal([]byte(d), &l); err != nil {
			t.Fatal(err)
		}
		if l.Kind == "identity" && l.Agent == agent {
			out = append(out, l)
		}
	}
	return out
}

// checkHostedRecord asserts the record of a public hosted identity shows the
// handle it claimed at creation, as a key event whose proof verifies, and
// agent.get's record its first leaf's time.
func checkHostedRecord(t *testing.T, s *Store, agent, handle string, createdAt, firstAt int64) {
	t.Helper()
	if _, err := s.SignCheckpoint(testContext); err != nil {
		t.Fatal(err)
	}
	rec, err := s.ReadLogRecord(testContext, handle)
	if err != nil {
		t.Fatal(err)
	}
	r := rec.Record
	if r.Agent != agent || r.Handle != handle || len(r.Handles) != 1 || r.Handles[0].Handle != handle || r.Handles[0].At != createdAt || r.Counts.KeyEvents != 1 {
		t.Fatalf("record: agent %s handle %s handles %+v counts %v", r.Agent, r.Handle, r.Handles, r.Counts)
	}
	if r.FirstSeen != createdAt {
		t.Fatalf("first_seen %d, created %d", r.FirstSeen, createdAt)
	}
	body := verifyCheckpoint(t, s, r.Checkpoint)
	if len(r.Proofs) != 1 {
		t.Fatalf("proofs %d", len(r.Proofs))
	}
	verifyInclusion(t, s, r.Proofs[0], body)
	got := run(t, s, Command{Operation: "agent.get", Target: agent})
	if got.Agent.Record == nil || got.Agent.Record.FirstAt != firstAt {
		t.Fatalf("agent.get record: %+v", got.Agent.Record)
	}
}

// A hosted identity's handle, claimed at hosted.create, is logged as a
// handle.claim leaf with its creation time once the identity is public, as a
// keyed agent's claim is; while it is private-only, nothing is logged.
func TestHostedHandleClaimIsLogged(t *testing.T) {
	s := openHostedTest(t)
	steppingClock(s)
	created := createHosted(t, s, "test-origin", "hosted-logged")
	agent := created["agent"].(string)
	if l := identityLeavesOf(t, s, agent); len(l) != 0 {
		t.Fatalf("a private-only hosted identity is logged: %+v", l)
	}
	var createdAt int64
	if err := s.db.QueryRow("SELECT created_at FROM identities WHERE id=?", agent).Scan(&createdAt); err != nil {
		t.Fatal(err)
	}
	key := hostedKey(t, s, created["token"].(string))
	run(t, s, signed(key, Command{Operation: "post", Room: "lobby", Text: "hello from a hosted identity"}))
	leaves := logLeaves(t, s)
	last := leaves[len(leaves)-2:]
	if !strings.Contains(last[0], `"op":"handle.claim"`) || !strings.Contains(last[0], `"detail":"hosted-logged"`) || !strings.Contains(last[1], `"kind":"message"`) {
		t.Fatalf("the handle claim precedes the first public post:\n%s", strings.Join(leaves, "\n"))
	}
	l := identityLeavesOf(t, s, agent)
	if len(l) != 1 || l[0].Op != "handle.claim" || l[0].At != createdAt || l[0].Target != agent {
		t.Fatalf("identity leaves: %+v", l)
	}
	// A second post logs no second claim.
	run(t, s, signed(key, Command{Operation: "post", Room: "lobby", Text: "again"}))
	if l := identityLeavesOf(t, s, agent); len(l) != 1 {
		t.Fatalf("identity leaves after a second post: %+v", l)
	}
	checkHostedRecord(t, s, agent, "hosted-logged", createdAt, createdAt)
}

// A keyed agent that claims its handle on a private post is logged the same
// way when it first posts publicly; one that claims it on a public post keeps
// its one claim leaf.
func TestPrivateHandleClaimLoggedWhenPublic(t *testing.T) {
	s := openTest(t, Config{})
	steppingClock(s)
	quiet := keyFor(71)
	run(t, s, signed(quiet, Command{Operation: "room.create", Room: "quiet-room", Visibility: "private"}))
	run(t, s, signed(quiet, Command{Operation: "post", Room: "quiet-room", Text: "private first", Handle: "quiet-one"}))
	if l := identityLeavesOf(t, s, keyID(quiet)); len(l) != 0 {
		t.Fatalf("a private-only key is logged: %+v", l)
	}
	run(t, s, signed(quiet, Command{Operation: "post", Room: "lobby", Text: "now public"}))
	if l := identityLeavesOf(t, s, keyID(quiet)); len(l) != 1 || l[0].Op != "handle.claim" || l[0].Detail != "quiet-one" {
		t.Fatalf("identity leaves: %+v", l)
	}
	loud := keyFor(72)
	run(t, s, signed(loud, Command{Operation: "post", Room: "lobby", Text: "public first", Handle: "loud-one"}))
	run(t, s, signed(loud, Command{Operation: "post", Room: "lobby", Text: "and again"}))
	if l := identityLeavesOf(t, s, keyID(loud)); len(l) != 1 || l[0].Op != "handle.claim" {
		t.Fatalf("identity leaves: %+v", l)
	}
}

// Before the fix, a hosted identity's claim was passed over for good. The
// start-up backfill appends it once, after the existing leaves, which it
// never changes; later starts append nothing.
func TestHostedIdentityBackfill(t *testing.T) {
	kek := writeKEK(t, 0o600)
	s := openTest(t, Config{HostedKEKFile: kek})
	steppingClock(s)
	created := createHosted(t, s, "test-origin", "hosted-early")
	agent := created["agent"].(string)
	key := hostedKey(t, s, created["token"].(string))
	run(t, s, signed(key, Command{Operation: "post", Room: "lobby", Text: "public before the fix"}))
	private := createHosted(t, s, "test-origin", "hosted-private")
	var createdAt int64
	if err := s.db.QueryRow("SELECT created_at FROM identities WHERE id=?", agent).Scan(&createdAt); err != nil {
		t.Fatal(err)
	}
	// The log as it was: rebuilt with the claim out of sight, so the audit
	// cursor passed it without a leaf; then the row is back as it was.
	tx, err := s.db.BeginTx(testContext, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec("DROP TABLE tlog_anchors; DROP TABLE tlog_checkpoints; DROP TABLE tlog_leaves; DROP TABLE tlog_hashes; DROP TABLE tlog_cursors"); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(tlogSchema); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec("UPDATE audit SET operation='hidden.claim' WHERE actor=? AND operation='handle.claim'", agent); err != nil {
		t.Fatal(err)
	}
	for {
		n, err := tlogCatchUp(testContext, tx)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	if _, err = tx.Exec("UPDATE audit SET operation='handle.claim' WHERE actor=? AND operation='hidden.claim'", agent); err != nil {
		t.Fatal(err)
	}
	old := txLeaves(t, tx)
	if strings.Contains(strings.Join(old, "\n"), `"op":"handle.claim"`) {
		t.Fatalf("the old log has the claim:\n%s", strings.Join(old, "\n"))
	}
	if _, err = tx.Exec("DELETE FROM meta WHERE key='tlog_identity_backfill'"); err != nil {
		t.Fatal(err)
	}
	// A catch-up does not reach it: the account is on the record already.
	if _, err = tlogCatchUp(testContext, tx); err != nil {
		t.Fatal(err)
	}
	if n, err := tlogBackfillIdentity(testContext, tx); err != nil || n != 1 {
		t.Fatalf("backfill appended %d, %v", n, err)
	}
	got := txLeaves(t, tx)
	if len(got) != len(old)+1 || strings.Join(got[:len(old)], "\n") != strings.Join(old, "\n") {
		t.Fatalf("backfill:\n%s", strings.Join(got, "\n"))
	}
	var l logLeaf
	if err = json.Unmarshal([]byte(got[len(old)]), &l); err != nil || l.Op != "handle.claim" || l.Agent != agent || l.Detail != "hosted-early" || l.At != createdAt {
		t.Fatalf("backfilled leaf: %s", got[len(old)])
	}
	// Recorded once; and keyed on the leaves, so a run without the marker
	// appends nothing either.
	if n, err := tlogBackfillIdentity(testContext, tx); err != nil || n != 0 {
		t.Fatalf("second backfill appended %d, %v", n, err)
	}
	if _, err = tx.Exec("DELETE FROM meta WHERE key='tlog_identity_backfill'"); err != nil {
		t.Fatal(err)
	}
	if n, err := tlogBackfillIdentity(testContext, tx); err != nil || n != 0 {
		t.Fatalf("unmarked backfill appended %d, %v", n, err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(logLeaves(t, s), "\n"), private["agent"].(string)) {
		t.Fatal("the backfill logged a private-only hosted identity")
	}
	// On the record since its first post, the first of its leaves in the log.
	var postedAt int64
	if err = s.db.QueryRow("SELECT min(created_at) FROM events WHERE author=?", agent).Scan(&postedAt); err != nil {
		t.Fatal(err)
	}
	checkHostedRecord(t, s, agent, "hosted-early", createdAt, postedAt)
	// Restarts append nothing.
	path := dbPath(t, s)
	before := len(logLeaves(t, s))
	s.Close()
	for range 2 {
		again, err := Open(path, Config{HostedKEKFile: kek})
		if err != nil {
			t.Fatal(err)
		}
		if n := len(logLeaves(t, again)); n != before {
			t.Fatalf("a restart changed the log: %d leaves, want %d", n, before)
		}
		again.Close()
	}
}
