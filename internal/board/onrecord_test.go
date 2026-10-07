package board

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// witnessLeaves are the identity.witness leaves, by index.
func witnessLeaves(t *testing.T, s *Store) map[int64]logLeaf {
	t.Helper()
	out := map[int64]logLeaf{}
	for i, d := range logLeaves(t, s) {
		var l logLeaf
		if err := json.Unmarshal([]byte(d), &l); err != nil {
			t.Fatal(err)
		}
		if l.Op == "identity.witness" {
			out[int64(i)] = l
		}
	}
	return out
}

func TestWitnessAppendsIdentityLeafWithProof(t *testing.T) {
	s, _ := linkTest(t)
	steppingClock(s)
	a, b, hidden := keyFor(170), keyFor(171), keyFor(172)
	registerAll(t, s, a, b)
	const url = "https://example.org/agents/a"
	run(t, s, linkCommand(a, "identity.link", "url", url))
	nonce := "on-record-witness-0123456789"
	run(t, s, witnessCommand(b, keyID(a), "url", url, nonce, "verified"))
	// A private-only key's link: witnessed, but never logged.
	run(t, s, linkCommand(hidden, "identity.link", "url", "https://example.org/private-only"))
	run(t, s, witnessCommand(b, keyID(hidden), "url", "https://example.org/private-only", "private-witness-0123456789", "failed"))

	leaves := witnessLeaves(t, s)
	if len(leaves) != 1 {
		t.Fatalf("witness leaves: %+v", leaves)
	}
	var index int64
	var leaf logLeaf
	for index, leaf = range leaves {
	}
	var signature string
	if err := s.db.QueryRow("SELECT signature FROM link_witnesses WHERE witness=? AND agent=?", keyID(b), keyID(a)).Scan(&signature); err != nil {
		t.Fatal(err)
	}
	want := logLeaf{V: 1, Kind: "identity", At: leaf.At, Op: "identity.witness", Agent: keyID(b), Target: keyID(a), LinkKind: "url", Value: url, Nonce: nonce, Verdict: "verified", Signature: signature}
	if leaf != want || leaf.At == 0 {
		t.Fatalf("witness leaf\n got %+v\nwant %+v", leaf, want)
	}
	if all := strings.Join(logLeaves(t, s), "\n"); strings.Contains(all, keyID(hidden)) || strings.Contains(all, "private-only") {
		t.Fatalf("a private-only key reached the log:\n%s", all)
	}
	if _, err := s.SignCheckpoint(testContext); err != nil {
		t.Fatal(err)
	}
	cp, err := s.ReadLogCheckpoint(testContext, -1)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := s.ReadLogProof(testContext, index, "", -1)
	if err != nil {
		t.Fatal(err)
	}
	verifyInclusion(t, s, proof, verifyCheckpoint(t, s, cp))
	if proof.Leaf.Data != string(want.bytes()) {
		t.Fatalf("proved leaf %s", proof.Leaf.Data)
	}
	// The witness is a key event on the witness's record.
	record, err := s.ReadLogRecord(testContext, keyID(b))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range record.Record.Proofs {
		found = found || p.Leaf.Index == index
	}
	if !found {
		t.Fatalf("the witness leaf is not among the witness's key events: %+v", record.Record.Proofs)
	}
}

// TestWitnessBackfillAppendsEarlierWitnesses: a log from before the witness
// source (1.38: every other source caught up, no witness cursor) appends the
// earlier witnesses on its next catch-up, after the leaves it had, which stay
// exactly as they were.
func TestWitnessBackfillAppendsEarlierWitnesses(t *testing.T) {
	s, _ := linkTest(t)
	steppingClock(s)
	a, b := keyFor(173), keyFor(174)
	registerAll(t, s, a, b)
	const url = "https://example.org/agents/backfill"
	run(t, s, linkCommand(a, "identity.link", "url", url))
	run(t, s, witnessCommand(b, keyID(a), "url", url, "backfill-witness-0123456789", "verified"))
	run(t, s, signed(a, Command{Operation: "post", Room: "lobby", Text: "after the witness"}))
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
	// 1.38's log: the witness source at its high-water mark, so it logs nothing.
	if _, err = tx.Exec("INSERT INTO tlog_cursors(source,seq) SELECT 'witness',max(seq) FROM link_witnesses"); err != nil {
		t.Fatal(err)
	}
	if _, err = tlogCatchUp(testContext, tx); err != nil {
		t.Fatal(err)
	}
	old := txLeaves(t, tx)
	if strings.Contains(strings.Join(old, "\n"), "identity.witness") {
		t.Fatal("the 1.38 shape logged a witness")
	}
	// 1.39 opens it: no witness cursor yet.
	if _, err = tx.Exec("DELETE FROM tlog_cursors WHERE source='witness'"); err != nil {
		t.Fatal(err)
	}
	if _, err = tlogCatchUp(testContext, tx); err != nil {
		t.Fatal(err)
	}
	got := txLeaves(t, tx)
	if len(got) != len(old)+1 || strings.Join(got[:len(old)], "\n") != strings.Join(old, "\n") ||
		!strings.Contains(got[len(old)], `"op":"identity.witness"`) || !strings.Contains(got[len(old)], url) {
		t.Fatalf("backfill:\n%s", strings.Join(got, "\n"))
	}
	// And only once.
	if _, err = tlogCatchUp(testContext, tx); err != nil {
		t.Fatal(err)
	}
	if again := txLeaves(t, tx); len(again) != len(got) {
		t.Fatalf("a second catch-up appended %d leaves", len(again)-len(got))
	}
}

func txLeaves(t *testing.T, tx *sql.Tx) []string {
	t.Helper()
	rows, err := tx.Query("SELECT data FROM tlog_leaves ORDER BY idx")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err = rows.Scan(&d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

func TestAgentRecordIsTheFirstLeafAndItsAnchor(t *testing.T) {
	s := openTest(t, Config{})
	steppingClock(s)
	first, second, private := keyFor(175), keyFor(176), keyFor(177)
	run(t, s, signed(first, Command{Operation: "agent.register", Handle: "first-on-record"}))
	run(t, s, signed(private, Command{Operation: "room.create", Room: "record-private", Visibility: "private"}))
	run(t, s, signed(private, Command{Operation: "post", Room: "record-private", Text: "never logged"}))
	if _, err := s.SignCheckpoint(testContext); err != nil {
		t.Fatal(err)
	}
	// The second agent's first leaf is a signed public post.
	run(t, s, signed(second, Command{Operation: "post", Room: "lobby", Text: "my first post"}))
	run(t, s, signed(first, Command{Operation: "post", Room: "lobby", Text: "a later post"}))
	if _, err := s.SignCheckpoint(testContext); err != nil {
		t.Fatal(err)
	}
	leafOf := func(agent string) (int64, int64) {
		var idx, at int64
		if err := s.db.QueryRow("SELECT idx,created_at FROM tlog_leaves WHERE subject=? AND kind IN ('identity','message') ORDER BY idx LIMIT 1", agent).Scan(&idx, &at); err != nil {
			t.Fatal(err)
		}
		return idx, at
	}
	get := func(key string) *AgentRecord {
		return run(t, s, Command{Operation: "agent.get", Target: key}).Agent.Record
	}
	for _, key := range []string{keyID(first), keyID(second)} {
		idx, at := leafOf(key)
		r := get(key)
		if r == nil || r.FirstLeaf != idx || r.FirstAt != at || r.ProofURL != fmt.Sprintf("/api/log/proof?leaf=%d", idx) || r.Anchored || r.AnchoredAt != 0 {
			t.Fatalf("record of %s: %+v, want leaf %d at %d", key, r, idx, at)
		}
		p, err := s.ReadLogProof(testContext, r.FirstLeaf, "", -1)
		if err != nil || !strings.Contains(p.Leaf.Data, key) {
			t.Fatalf("record proof: %+v %v", p, err)
		}
	}
	if idx, _ := leafOf(keyID(first)); idx != 0 {
		t.Fatalf("the first agent's register is leaf %d", idx)
	}
	if got := get(keyID(second)); !strings.Contains(logLeaves(t, s)[got.FirstLeaf], `"kind":"message"`) {
		t.Fatalf("the second agent's first leaf: %s", logLeaves(t, s)[got.FirstLeaf])
	}
	// The private-only key is no public agent and has no leaf.
	if _, err := s.Execute(testContext, Command{Operation: "agent.get", Target: keyID(private)}, "test-origin"); err == nil {
		t.Fatal("a private-only key is a public agent")
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM tlog_leaves WHERE subject='"+keyID(private)+"' OR data LIKE '%never logged%' OR data LIKE '%record-private%'"); n != 0 {
		t.Fatalf("private leaves: %d", n)
	}

	// Anchors: pending covers nothing; a confirmed anchor of the first
	// checkpoint (size 1) covers only leaf 0; the second covers both.
	var sizes []int64
	rows, err := s.db.Query("SELECT size FROM tlog_checkpoints ORDER BY size")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n int64
		if err = rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		sizes = append(sizes, n)
	}
	rows.Close()
	if len(sizes) != 2 || sizes[0] != 1 {
		t.Fatalf("checkpoints %v", sizes)
	}
	for _, size := range sizes {
		if _, err = s.db.Exec("INSERT INTO tlog_anchors(size,digest,ots,state,calendars,submitted_at) VALUES(?,'d',x'00','pending','[]',?)", size, testTime+size); err != nil {
			t.Fatal(err)
		}
	}
	if get(keyID(first)).Anchored || get(keyID(second)).Anchored {
		t.Fatal("a pending anchor counts as anchored")
	}
	if _, err = s.db.Exec("UPDATE tlog_anchors SET state='confirmed',bitcoin_height=900001 WHERE size=?", sizes[0]); err != nil {
		t.Fatal(err)
	}
	if r := get(keyID(first)); !r.Anchored || r.AnchoredAt != testTime+sizes[0] || r.BitcoinHeight != 900001 {
		t.Fatalf("first record after its checkpoint confirmed: %+v", r)
	}
	if get(keyID(second)).Anchored {
		t.Fatal("a checkpoint that does not cover the leaf anchored it")
	}
	if _, err = s.db.Exec("UPDATE tlog_anchors SET state='confirmed',bitcoin_height=900002 WHERE size=?", sizes[1]); err != nil {
		t.Fatal(err)
	}
	if r := get(keyID(second)); !r.Anchored || r.BitcoinHeight != 900002 {
		t.Fatalf("second record: %+v", r)
	}
	if r := get(keyID(first)); r.BitcoinHeight != 900001 {
		t.Fatalf("the earliest covering anchor: %+v", r)
	}
}

// TestAgentRecordUsesTheSubjectIndex: the lookup never scans the log.
func TestAgentRecordUsesTheSubjectIndex(t *testing.T) {
	s := openTest(t, Config{})
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN SELECT f.idx FROM tlog_leaves f WHERE f.subject=? AND f.subject<>'' AND f.kind IN ('identity','message') ORDER BY f.idx LIMIT 1`, "x")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if joined := strings.Join(plan, "; "); !strings.Contains(joined, "tlog_leaves_subject") {
		t.Fatalf("record lookup plan: %s", joined)
	}
}
