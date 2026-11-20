package board

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/tlog"
)

// withheldFixture is a public log with alice's handle and identity link, the
// witness leaves of bob (registered) and dave (a key with no leaf of its
// own) about that link, and carol's leaves and her witness of bob's link,
// which no erasure of alice may touch.
type withheldFixture struct {
	s                *Store
	a, b, c, d       string
	before           []string // every leaf's data before the erasure
	hashes           [][]byte // every leaf's stored hash before the erasure
	bobLeaf, msgLeaf int64
}

func newWithheldFixture(t *testing.T) withheldFixture {
	t.Helper()
	s := openTest(t, Config{})
	steppingClock(s)
	alice, bob, carol, dave := keyFor(231), keyFor(232), keyFor(233), keyFor(234)
	f := withheldFixture{s: s, a: keyID(alice), b: keyID(bob), c: keyID(carol), d: keyID(dave)}
	run(t, s, signed(alice, Command{Operation: "agent.register", Handle: "alice-withheld"}))
	run(t, s, signed(bob, Command{Operation: "agent.register", Handle: "bob-kept"}))
	run(t, s, signed(carol, Command{Operation: "agent.register", Handle: "carol-kept"}))
	run(t, s, signed(carol, Command{Operation: "post", Room: "lobby", Text: "carol public words", RequestID: "w1"}))
	tx, err := s.db.BeginTx(testContext, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := s.now().Unix()
	if _, err = tx.Exec("INSERT INTO identities(id,public_key,account,created_at,last_seen) VALUES(?,'dave-public-key',?,?,?)", f.d, f.d, now, now); err != nil {
		t.Fatal(err)
	}
	// As identitylinks.go and identitywitness.go write them.
	if err = audit(testContext, tx, "identity.link", f.a, "github", "identity link verified: alice-gh-secret", now); err != nil {
		t.Fatal(err)
	}
	if err = audit(testContext, tx, "identity.link", f.b, "github", "identity link verified: bob-gh", now); err != nil {
		t.Fatal(err)
	}
	for _, w := range [][2]string{{f.b, f.a}, {f.d, f.a}, {f.c, f.b}} {
		value := map[string]string{f.a: "alice-gh-secret", f.b: "bob-gh"}[w[1]]
		if _, err = tx.Exec("INSERT INTO link_witnesses(witness,agent,kind,value,verdict,nonce,signature,signed_payload,created_at) VALUES(?,?,'github',?,'verified','n','sig','{}',?)", w[0], w[1], value, now); err != nil {
			t.Fatal(err)
		}
	}
	if err = tlogCatchUpAll(testContext, tx); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SignCheckpoint(testContext); err != nil {
		t.Fatal(err)
	}
	f.before = logLeaves(t, s)
	for i := range f.before {
		var h []byte
		if err = s.db.QueryRow("SELECT hash FROM tlog_hashes WHERE level=0 AND idx=?", i).Scan(&h); err != nil {
			t.Fatal(err)
		}
		f.hashes = append(f.hashes, h)
	}
	f.bobLeaf, f.msgLeaf = -1, -1
	for i, d := range f.before {
		if strings.Contains(d, `"op":"agent.register"`) && strings.Contains(d, "bob-kept") {
			f.bobLeaf = int64(i)
		}
		if strings.Contains(d, `"kind":"message"`) {
			f.msgLeaf = int64(i)
		}
	}
	if f.bobLeaf < 0 || f.msgLeaf < 0 || !strings.Contains(strings.Join(f.before, "\n"), "alice-gh-secret") {
		t.Fatalf("fixture log:\n%s", strings.Join(f.before, "\n"))
	}
	return f
}

// aliceLeaves are the indexes the erasure of alice must withhold: her
// identity leaves with a value and the witness leaves about her link.
func (f withheldFixture) aliceLeaves() []int64 {
	var out []int64
	for i, d := range f.before {
		var l logLeaf
		_ = json.Unmarshal([]byte(d), &l)
		if l.Kind == "identity" && withheldOps[l.Op] && (l.Agent == f.a || l.Target == f.a) {
			out = append(out, int64(i))
		}
	}
	return out
}

func onRecord(t *testing.T, s *Store, account string) bool {
	t.Helper()
	var on bool
	if err := s.db.QueryRow("SELECT "+onRecordSQL, account).Scan(&on); err != nil {
		t.Fatal(err)
	}
	return on
}

// C1126 step 1: an erasure withholds the bodies of the account's identity
// leaves and of the witness leaves about it. They are served with data null,
// withheld "erased", the leaf hash they always had and erased_by, the later
// account.erase leaf; every inclusion and consistency proof still verifies,
// against checkpoints from before and after; every other leaf is byte for
// byte unchanged; a second run withholds nothing.
func TestEraseWithholdsIdentityLeaves(t *testing.T) {
	f := newWithheldFixture(t)
	s := f.s
	want := f.aliceLeaves()
	if len(want) != 4 { // register (with handle), link, two witnesses
		t.Fatalf("alice's leaves %v in\n%s", want, strings.Join(f.before, "\n"))
	}
	cp1, err := s.ReadLogCheckpoint(testContext, -1)
	if err != nil {
		t.Fatal(err)
	}
	if onRecord(t, s, f.d) {
		t.Fatal("a witness-only key reads on the record")
	}

	dry, err := s.OperatorEraseAccount(testContext, f.a, "req-c1126", false)
	if err != nil {
		t.Fatal(err)
	}
	if got := eraseCount(dry, "tlog_leaves", eraseLeavesAction); got != 4 {
		t.Fatalf("dry run counts %d leaves: %+v", got, dry.Counts)
	}
	if strings.Join(logLeaves(t, s), "\n") != strings.Join(f.before, "\n") {
		t.Fatal("the dry run changed the log")
	}
	r, err := s.OperatorEraseAccount(testContext, f.a, "req-c1126", true)
	if err != nil {
		t.Fatal(err)
	}
	if got := eraseCount(r, "tlog_leaves", eraseLeavesAction); got != 4 {
		t.Fatalf("receipt counts %d leaves: %+v", got, r.Counts)
	}
	var erase int64
	if err = s.db.QueryRow(eraseLeafSQL, f.a).Scan(&erase); err != nil || erase < int64(len(f.before)) {
		t.Fatalf("erase leaf %d: %v", erase, err)
	}
	after := logLeaves(t, s)
	withheld := map[int64]bool{}
	for _, i := range want {
		withheld[i] = true
	}
	for i, d := range f.before {
		if withheld[int64(i)] != (after[i] == "") || !withheld[int64(i)] && after[i] != d {
			t.Errorf("leaf %d: %q, was %q", i, after[i], d)
		}
		var h []byte
		if err = s.db.QueryRow("SELECT hash FROM tlog_hashes WHERE level=0 AND idx=?", i).Scan(&h); err != nil || string(h) != string(f.hashes[i]) {
			t.Errorf("leaf %d hash changed: %v", i, err)
		}
	}
	if _, err = s.SignCheckpoint(testContext); err != nil {
		t.Fatal(err)
	}
	cp2, err := s.ReadLogCheckpoint(testContext, -1)
	if err != nil {
		t.Fatal(err)
	}

	// /api/log/leaves: withheld leaves with their hash; no erased value.
	leaves, _, err := s.ReadLogLeaves(testContext, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range leaves {
		if !withheld[e.Index] {
			if e.Withheld != "" || e.Hash != newLogEntry(e.Index, e.Kind, e.Data).Hash {
				t.Errorf("leaf %d: %+v", e.Index, e)
			}
			continue
		}
		if e.Withheld != WithheldErased || e.Data != "" || e.ErasedBy != erase || e.Hash != base64.StdEncoding.EncodeToString(f.hashes[e.Index]) {
			t.Errorf("withheld leaf %d: %+v", e.Index, e)
		}
	}
	raw, _ := json.Marshal(leaves)
	for _, leak := range []string{"alice-withheld", "alice-gh-secret"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("/api/log/leaves serves %q", leak)
		}
	}
	for _, kept := range []string{"bob-kept", "carol-kept", "bob-gh", `"data":null,"leaf_hash":"`, `"withheld":"erased","erased_by":`} {
		if !strings.Contains(string(raw), kept) {
			t.Fatalf("/api/log/leaves lacks %q: %s", kept, raw)
		}
	}

	// Every proof still verifies, against both checkpoints.
	root := func(cp LogCheckpoint) tlog.Hash {
		h, err := tlog.ParseHash(cp.Root)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	for _, cp := range []LogCheckpoint{cp1, cp2} {
		for i := int64(0); i < cp.Size; i++ {
			p, err := s.ReadLogProof(testContext, i, "", cp.Size)
			if err != nil {
				t.Fatalf("proof of %d in %d: %v", i, cp.Size, err)
			}
			h, err := tlog.ParseHash(p.Leaf.Hash)
			if err != nil {
				t.Fatal(err)
			}
			if err = tlog.VerifyInclusion(i, cp.Size, h, decodeHashes(t, p.Proof), root(cp)); err != nil {
				t.Fatalf("leaf %d in %d: %v", i, cp.Size, err)
			}
			if !withheld[i] {
				if p.Erasure != nil {
					t.Fatalf("leaf %d has an erasure", i)
				}
				continue
			}
			if p.Erasure == nil || p.Erasure.Leaf.Index != erase || !strings.Contains(p.Erasure.Leaf.Data, `"op":"account.erase"`) {
				t.Fatalf("withheld leaf %d: erasure %+v", i, p.Erasure)
			}
			if cp.Size <= erase {
				if p.Erasure.Proof != nil {
					t.Fatalf("an erasure proven outside checkpoint %d", cp.Size)
				}
				continue
			}
			if err = tlog.VerifyInclusion(erase, cp.Size, tlog.LeafHash([]byte(p.Erasure.Leaf.Data)), decodeHashes(t, p.Erasure.Proof), root(cp)); err != nil {
				t.Fatalf("erasure of leaf %d: %v", i, err)
			}
		}
	}
	c, err := s.ReadLogConsistency(testContext, cp1.Size, cp2.Size)
	if err != nil {
		t.Fatal(err)
	}
	if err = tlog.VerifyConsistency(cp1.Size, cp2.Size, decodeHashes(t, c.Proof), root(cp1), root(cp2)); err != nil {
		t.Fatal(err)
	}
	if got, err := tlog.TreeHash(cp1.Size, txHashReader(testContext, s.db)); err != nil || got != root(cp1) {
		t.Fatalf("the old checkpoint's root: %v", err)
	}

	// Readers: a witness-only key stays off the record; a withheld leaf is
	// found by its hash, so its audit row is never logged twice.
	if onRecord(t, s, f.d) {
		t.Fatal("a withheld witness leaf puts its witness on the record")
	}
	if !onRecord(t, s, f.a) || !onRecord(t, s, f.b) {
		t.Fatal("withheld own leaves or bob's leaves no longer count")
	}
	var l logLeaf
	if err = json.Unmarshal([]byte(f.before[want[0]]), &l); err != nil || l.Op != "agent.register" {
		t.Fatalf("leaf %d: %s", want[0], f.before[want[0]])
	}
	tx, err := s.db.BeginTx(testContext, nil)
	if err != nil {
		t.Fatal(err)
	}
	size := int64(len(after))
	n, err := appendIdentityRows(testContext, tx, []identityRow{{at: l.At, op: l.Op, actor: l.Agent, target: l.Target, detail: l.Detail}}, &size, txHashReader(testContext, tx))
	_ = tx.Rollback()
	if err != nil || n != 0 {
		t.Fatalf("a withheld leaf's row appended again: %d %v", n, err)
	}
	// The record: bob keeps his handle history; his witness leaf about
	// alice is proven withheld, with the erase leaf.
	rec, err := s.ReadLogRecord(testContext, f.b)
	if err != nil {
		t.Fatal(err)
	}
	recJSON, _ := json.Marshal(rec)
	if len(rec.Record.Handles) != 1 || rec.Record.Handles[0].Handle != "bob-kept" || strings.Contains(string(recJSON), "alice-gh-secret") || !strings.Contains(string(recJSON), `"withheld":"erased"`) {
		t.Fatalf("bob's record: %s", recJSON)
	}
	for _, p := range rec.Record.Proofs {
		if p.Leaf.Withheld != "" && (p.Erasure == nil || p.Erasure.Proof == nil) {
			t.Fatalf("a withheld key event without its erasure proof: %+v", p)
		}
	}
	if rec, err = s.ReadLogRecord(testContext, f.a); err == nil && len(rec.Record.Handles) != 0 {
		t.Fatalf("the erased account's handle history: %+v", rec.Record.Handles)
	}

	// A second run withholds nothing.
	again, err := s.OperatorEraseAccount(testContext, f.a, "req-c1126-again", true)
	if err != nil {
		t.Fatal(err)
	}
	if got := eraseCount(again, "tlog_leaves", eraseLeavesAction); got != 0 {
		t.Fatalf("second run: %d leaves", got)
	}
}

// C1126: the schema allows exactly one change to the log: an identity
// leaf's data to empty when tlog_withheld names the later account.erase leaf of
// its account. Every other update and every delete is refused, and the read
// side refuses to serve a body-less leaf that has no such row.
func TestWithheldLeafTriggers(t *testing.T) {
	f := newWithheldFixture(t)
	s := f.s
	if _, err := s.OperatorEraseAccount(testContext, f.a, "req-c1126", true); err != nil {
		t.Fatal(err)
	}
	var erase, w int64
	if err := s.db.QueryRow(eraseLeafSQL, f.a).Scan(&erase); err != nil {
		t.Fatal(err)
	}
	w = f.aliceLeaves()[0]
	for _, q := range []string{
		"UPDATE tlog_leaves SET data='x' WHERE idx=?1",                                                                  // bob's leaf, new data
		"UPDATE tlog_leaves SET data='' WHERE idx=?1",                                                                   // bob's leaf, no tlog_withheld row
		"UPDATE tlog_leaves SET data='' WHERE idx=?3",                                                                   // a message leaf
		"UPDATE tlog_leaves SET data='{}' WHERE idx=?2",                                                                 // a withheld leaf gets new data
		"UPDATE tlog_leaves SET subject='x' WHERE idx=?2",                                                               // another column of a withheld leaf
		"UPDATE tlog_leaves SET data='',created_at=1 WHERE idx=?2",                                                      // data and another column
		"UPDATE tlog_leaves SET kind='message' WHERE idx=?2",                                                            //
		"UPDATE tlog_leaves SET ref='x' WHERE idx=?2",                                                                   //
		"UPDATE tlog_leaves SET idx=100000 WHERE idx=?2",                                                                //
		"DELETE FROM tlog_leaves WHERE idx=?2",                                                                          //
		"DELETE FROM tlog_leaves WHERE idx=?1",                                                                          //
		"UPDATE tlog_hashes SET hash=x'00' WHERE level=0 AND idx=?2",                                                    //
		"DELETE FROM tlog_hashes WHERE level=0 AND idx=?2",                                                              //
		"UPDATE tlog_withheld SET erase_idx=erase_idx+1 WHERE idx=?2",                                                   //
		"DELETE FROM tlog_withheld WHERE idx=?2",                                                                        //
		"INSERT INTO tlog_withheld(idx,erase_idx,account,op,created_at) VALUES(?1,?4,'" + f.b + "','agent.register',1)", // the erase leaf names alice, not bob
		"INSERT INTO tlog_withheld(idx,erase_idx,account,op,created_at) VALUES(?3,?4,'" + f.a + "','post',1)",           // not an identity leaf
		"INSERT INTO tlog_withheld(idx,erase_idx,account,op,created_at) VALUES(?1,?1,'" + f.a + "','agent.register',1)", // not an erase leaf after it
		"INSERT INTO tlog_withheld(idx,erase_idx,account,op,created_at) VALUES(?4,?4,'" + f.a + "','account.erase',1)",  // the erase leaf itself
	} {
		// Indexes written in, so no statement fails on its arguments alone.
		q = strings.NewReplacer("?1", fmt.Sprint(f.bobLeaf), "?2", fmt.Sprint(w), "?3", fmt.Sprint(f.msgLeaf), "?4", fmt.Sprint(erase)).Replace(q)
		if _, err := s.db.Exec(q); err == nil {
			t.Errorf("allowed: %s", q)
		} else if !strings.Contains(err.Error(), "append-only") && !strings.Contains(err.Error(), "withheld only") && !strings.Contains(err.Error(), "constraint") {
			t.Errorf("%s: %v", q, err)
		}
	}
	if got := logLeaves(t, s); got[f.bobLeaf] != f.before[f.bobLeaf] || got[f.msgLeaf] != f.before[f.msgLeaf] || got[w] != "" {
		t.Fatal("a refused statement changed the log")
	}
	// A body-less leaf outside the rule is never served.
	if _, err := s.db.Exec("DROP TRIGGER tlog_leaves_no_update"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE tlog_leaves SET data='' WHERE idx=?", f.bobLeaf); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ReadLogLeaves(testContext, 0, -1); err == nil || !strings.Contains(err.Error(), "no later account.erase leaf") {
		t.Fatalf("a body-less leaf without an erasure: %v", err)
	}
}

// C1126 backfill: an account erased before step 1 (its account.erase leaf
// logged, its identity leaves still with their bodies, as on production)
// loses those bodies when the erase is run again; erased_by is the first
// erasure's leaf, which the leaves come before.
func TestEraseRerunWithholdsEarlierErasure(t *testing.T) {
	s := openTest(t, Config{})
	steppingClock(s)
	erin := keyFor(235)
	e := keyID(erin)
	run(t, s, signed(erin, Command{Operation: "agent.register", Handle: "erin-before-c1126"}))
	run(t, s, signed(keyFor(236), Command{Operation: "agent.register", Handle: "frank-kept"}))
	tx, err := s.db.BeginTx(testContext, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = audit(context.Background(), tx, "account.erase", "operator", e, "erased on the owner's request; "+erasureReceiptDetail+strings.Repeat("ab", 32), s.now().Unix()); err != nil {
		t.Fatal(err)
	}
	if err = tlogCatchUpAll(testContext, tx); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var first int64
	if err = s.db.QueryRow(eraseLeafSQL, e).Scan(&first); err != nil || first < 0 {
		t.Fatalf("first erase leaf %d: %v", first, err)
	}
	if !strings.Contains(strings.Join(logLeaves(t, s), "\n"), "erin-before-c1126") {
		t.Fatal("fixture: the handle is not in the log")
	}
	dry, err := s.OperatorEraseAccount(testContext, e, "c1126-backfill", false)
	if err != nil {
		t.Fatal(err)
	}
	if got := eraseCount(dry, "tlog_leaves", eraseLeavesAction); got != 1 {
		t.Fatalf("dry run: %d leaves", got)
	}
	if _, err = s.OperatorEraseAccount(testContext, e, "c1126-backfill", true); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(logLeaves(t, s), "\n")
	if strings.Contains(all, "erin-before-c1126") || !strings.Contains(all, "frank-kept") {
		t.Fatalf("after the re-run:\n%s", all)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM tlog_withheld WHERE account=? AND erase_idx=?", e, first); n != 1 {
		t.Fatalf("withheld by the first erasure: %d", n)
	}
}

// Schema 55 upgrades a schema-54 database to the same sqlite_master as a new
// one: tlog_withheld, and tlog_leaves_no_update dropped and recreated
// narrowed (the old trigger refused every update).
func TestSchema55Upgrade(t *testing.T) { schema29UpgradeFrom(t, 54) }

// privateKeyAccount adds a key that is its own account and never public (no
// registration, no public post), with an identity audit row the log must
// never take: a linked device.
func privateKeyAccount(t *testing.T, s *Store, seed byte) string {
	t.Helper()
	id := keyID(keyFor(seed))
	now := s.now().Unix()
	tx, err := s.db.BeginTx(testContext, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec("INSERT INTO identities(id,public_key,account,created_at,last_seen) VALUES(?,?,?,?,?)", id, "pk-"+id, id, now, now); err != nil {
		t.Fatal(err)
	}
	if err = audit(testContext, tx, "device.add", id, "private-device-key-"+id[:8], "device linked", now); err != nil {
		t.Fatal(err)
	}
	if err = tlogCatchUpAll(testContext, tx); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return id
}

// C1126 review: erasing an account that was never public appends no
// identity leaf of its own. Only a witness: its witness leaf is withheld,
// behind exactly one new leaf, the erase leaf. Only named in another kind of
// leaf (a grant): nothing is appended at all. Its device row never reaches
// the log.
func TestErasePrivateAccountPublishesNothing(t *testing.T) {
	s := openTest(t, Config{})
	steppingClock(s)
	alice := keyFor(237)
	a := keyID(alice)
	run(t, s, signed(alice, Command{Operation: "agent.register", Handle: "alice-public"}))
	witness := privateKeyAccount(t, s, 238)
	granted := privateKeyAccount(t, s, 239)
	tx, err := s.db.BeginTx(testContext, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := s.now().Unix()
	if _, err = tx.Exec("INSERT INTO link_witnesses(witness,agent,kind,value,verdict,nonce,signature,signed_payload,created_at) VALUES(?,?,'github','alice-gh','verified','n','sig','{}',?)", witness, a, now); err != nil {
		t.Fatal(err)
	}
	if err = audit(testContext, tx, "allowance.grant", "operator", granted, "1000 post bytes", now); err != nil {
		t.Fatal(err)
	}
	if err = tlogCatchUpAll(testContext, tx); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	before := logLeaves(t, s)
	joined := strings.Join(before, "\n")
	if !strings.Contains(joined, `"op":"identity.witness","agent":"`+witness) || !strings.Contains(joined, `"kind":"grant"`) || strings.Contains(joined, "device linked") {
		t.Fatalf("fixture log:\n%s", joined)
	}

	if _, err = s.OperatorEraseAccount(testContext, witness, "req-private-witness", true); err != nil {
		t.Fatal(err)
	}
	after := logLeaves(t, s)
	if len(after) != len(before)+1 || !strings.Contains(after[len(before)], `"op":"account.erase","agent":"`+witness) {
		t.Fatalf("erasing a private witness appended:\n%s", strings.Join(after[len(before):], "\n"))
	}
	for i, d := range before {
		if strings.Contains(d, `"op":"identity.witness"`) {
			if after[i] != "" {
				t.Fatalf("the witness leaf kept its body: %s", after[i])
			}
		} else if after[i] != d {
			t.Fatalf("leaf %d changed: %s", i, after[i])
		}
	}

	if _, err = s.OperatorEraseAccount(testContext, granted, "req-private-grant", true); err != nil {
		t.Fatal(err)
	}
	if again := logLeaves(t, s); len(again) != len(after) {
		t.Fatalf("erasing an account named only in a grant appended:\n%s", strings.Join(again[len(after):], "\n"))
	}
	if strings.Contains(strings.Join(logLeaves(t, s), "\n"), "device linked") {
		t.Fatal("a private account's device row is in the log")
	}
}

// C1126 review: a restore of a copy older than an erasure has no
// account.erase leaf, so the replay logs the erasure again and withholds
// the account's identity leaves; their bodies are never served again.
func TestErasureReplayWithholdsLeaves(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "board.sqlite")
	open := func() *Store {
		t.Helper()
		s, err := Open(path, Config{})
		if err != nil {
			t.Fatal(err)
		}
		s.now = func() time.Time { return time.Unix(testTime, 0) }
		return s
	}
	alice := keyFor(240)
	a := keyID(alice)
	s := open()
	run(t, s, signed(alice, Command{Operation: "agent.register", Handle: "alice-restored"}))
	if !strings.Contains(strings.Join(logLeaves(t, s), "\n"), "alice-restored") {
		t.Fatal("fixture: the handle is not in the log")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s = open()
	if _, err = s.OperatorEraseAccount(testContext, a, "req-restore", true); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Remove(path + suffix)
	}
	if err = os.WriteFile(path, snapshot, 0600); err != nil {
		t.Fatal(err)
	}
	s = open()
	defer s.Close()
	leaves, _, err := s.ReadLogLeaves(testContext, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(leaves)
	if strings.Contains(string(raw), "alice-restored") || !strings.Contains(string(raw), `"withheld":"erased"`) {
		t.Fatalf("after the restore and replay: %s", raw)
	}
	var erase int64
	if err = s.db.QueryRow(eraseLeafSQL, a).Scan(&erase); err != nil || erase < 0 {
		t.Fatalf("no account.erase leaf after the replay: %d %v", erase, err)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM tlog_withheld WHERE account=? AND erase_idx=?", a, erase); n != 1 {
		t.Fatalf("withheld by the replayed erasure: %d", n)
	}
}
