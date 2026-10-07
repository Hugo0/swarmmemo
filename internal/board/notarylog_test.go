package board

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"swarmmemo/internal/services"
)

// notaryLeaves are the notary leaves in log order.
func notaryLeaves(t *testing.T, leaves []string) []logLeaf {
	t.Helper()
	var out []logLeaf
	for _, d := range leaves {
		var l logLeaf
		if err := json.Unmarshal([]byte(d), &l); err != nil {
			t.Fatal(err)
		}
		if l.Kind == "notary" {
			out = append(out, l)
		}
	}
	return out
}

func stampReceipt(t *testing.T, s *Store, agent ed25519.PrivateKey, text, requestID string) services.NotaryReceipt {
	t.Helper()
	res := run(t, s, svcCall(agent, "notary", "stamp", map[string]any{"text": text}, 1, requestID))
	raw, _ := json.Marshal(svcField(t, res.Data, "result", "receipt"))
	var r services.NotaryReceipt
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	if got := svcField(t, res.Data, "result", "log", "proof"); got != "/api/log/proof?notary="+r.Hash {
		t.Fatalf("stamp answer log proof: %v", got)
	}
	return r
}

// TestNotaryStampIsALogLeaf: a stamp is a notary leaf whose inclusion proof
// verifies against a signed checkpoint, with the notary key's leaf as
// related; the logged key verifies the receipt, and is logged once.
func TestNotaryStampIsALogLeaf(t *testing.T) {
	s := openWakeTest(t, "notary")
	steppingClock(s)
	agent := keyFor(1)
	pub := s.services.notaryKey.Public().(ed25519.PublicKey)
	keyLeaves := notaryLeaves(t, logLeaves(t, s))
	if len(keyLeaves) != 1 || keyLeaves[0].Op != "notary.key" || keyLeaves[0].KeyID != services.NotaryKeyID(s.services.notaryKey) ||
		keyLeaves[0].PublicKey != base64.RawURLEncoding.EncodeToString(pub) {
		t.Fatalf("opening the store logs the notary key once: %+v", keyLeaves)
	}
	r := stampReceipt(t, s, agent, "the plan, v4", "nl1")
	if _, err := s.SignCheckpoint(testContext); err != nil {
		t.Fatal(err)
	}
	cp, err := s.ReadLogCheckpoint(testContext, -1)
	if err != nil {
		t.Fatal(err)
	}
	body := verifyCheckpoint(t, s, cp)
	proof, err := s.ReadNotaryProof(testContext, r.Hash, -1)
	if err != nil {
		t.Fatal(err)
	}
	verifyInclusion(t, s, proof, body)
	want := logLeaf{V: 1, Kind: "notary", At: r.Time, Op: "notary.stamp", Seq: r.Seq, Hash: r.Hash, KeyID: r.KeyID, Signature: r.Signature}
	if proof.Leaf.Kind != "notary" || proof.Leaf.Data != string(want.bytes()) {
		t.Fatalf("stamp leaf\n got %s\nwant %s", proof.Leaf.Data, want.bytes())
	}
	if len(proof.Related) != 1 {
		t.Fatalf("the stamp's proof carries the key's leaf: %+v", proof.Related)
	}
	verifyInclusion(t, s, proof.Related[0], body)
	var key logLeaf
	if err = json.Unmarshal([]byte(proof.Related[0].Leaf.Data), &key); err != nil {
		t.Fatal(err)
	}
	if key.Op != "notary.key" || key.KeyID != r.KeyID || key.PublicKey != r.PublicKey || !services.VerifyNotaryReceipt(key.PublicKey, r) {
		t.Fatalf("logged key %+v does not verify receipt %+v", key, r)
	}
	// notary=key proves the key's leaf alone.
	kp, err := s.ReadNotaryProof(testContext, "key", -1)
	if err != nil || kp.Leaf.Index != proof.Related[0].Leaf.Index {
		t.Fatalf("notary=key: %+v %v", kp.Leaf, err)
	}
	verifyInclusion(t, s, kp, body)
	// A repeat stamp returns the first receipt and appends nothing.
	before := len(logLeaves(t, s))
	if again := stampReceipt(t, s, agent, "the plan, v4", "nl2"); again.Signature != r.Signature {
		t.Fatalf("repeat stamp: %+v", again)
	}
	// Registering the key again is a no-op: one key leaf.
	if err = s.logNotaryKey(); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SignCheckpoint(testContext); err != nil {
		t.Fatal(err)
	}
	if after := len(logLeaves(t, s)); after != before {
		t.Fatalf("a repeat stamp or key registration appended %d leaves", after-before)
	}
	// The read paths say where the proof is.
	back := run(t, s, svcRead(nil, "notary", "get", map[string]any{"hash": r.Hash}))
	if svcField(t, back.Data, "result", "log", "proof") != "/api/log/proof?notary="+r.Hash {
		t.Fatalf("get: %+v", back.Data)
	}
	kr := run(t, s, svcRead(nil, "notary", "key", map[string]any{}))
	if svcField(t, kr.Data, "result", "log", "proof") != services.NotaryKeyLogProof || svcField(t, kr.Data, "result", "public_key") != r.PublicKey {
		t.Fatalf("key: %+v", kr.Data)
	}
	// Unknown and malformed hashes.
	var e *Error
	if _, err = s.ReadNotaryProof(testContext, strings.Repeat("0", 64), -1); !errors.As(err, &e) || e.Code != "not_logged" {
		t.Fatalf("unknown hash: %v", err)
	}
	for _, bad := range []string{"", "KEY", strings.Repeat("A", 64), strings.Repeat("0", 63), "notary:" + r.Hash} {
		if _, err = s.ReadNotaryProof(testContext, bad, -1); !errors.As(err, &e) || e.Status != 400 {
			t.Fatalf("%q: %v", bad, err)
		}
	}
	// Who asked is never logged.
	if all := strings.Join(logLeaves(t, s), "\n"); strings.Contains(all, keyID(agent)) {
		t.Fatalf("the stamping agent reached the log:\n%s", all)
	}
}

// TestNotaryBackfillAppendsEarlierStamps: a log from before the notary
// sources (1.39: every other source caught up, no notary cursors) appends the
// notary key and every earlier receipt on its next catch-up, after the
// leaves it had, which stay exactly as they were; and only once. The old
// receipts still verify against the logged key.
func TestNotaryBackfillAppendsEarlierStamps(t *testing.T) {
	s := openWakeTest(t, "notary")
	steppingClock(s)
	agent := keyFor(2)
	r1 := stampReceipt(t, s, agent, "first stamp", "nb1")
	r2 := stampReceipt(t, s, keyFor(3), "second stamp", "nb2")
	run(t, s, signed(agent, Command{Operation: "post", Room: "lobby", Text: "after the stamps"}))
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
	// 1.39's log: the notary sources at their high-water marks, so they log nothing.
	if _, err = tx.Exec("INSERT INTO tlog_cursors(source,seq) SELECT 'notary',max(seq) FROM notary_receipts; INSERT INTO tlog_cursors(source,seq) SELECT 'notary_keys',max(seq) FROM notary_public_keys"); err != nil {
		t.Fatal(err)
	}
	if _, err = tlogCatchUp(testContext, tx); err != nil {
		t.Fatal(err)
	}
	old := txLeaves(t, tx)
	if len(old) == 0 || len(notaryLeaves(t, old)) != 0 {
		t.Fatalf("the 1.39 shape: %v", old)
	}
	// 1.40 opens it: no notary cursors yet.
	if _, err = tx.Exec("DELETE FROM tlog_cursors WHERE source IN ('notary','notary_keys')"); err != nil {
		t.Fatal(err)
	}
	if _, err = tlogCatchUp(testContext, tx); err != nil {
		t.Fatal(err)
	}
	got := txLeaves(t, tx)
	if len(got) != len(old)+3 || strings.Join(got[:len(old)], "\n") != strings.Join(old, "\n") {
		t.Fatalf("backfill:\n%s", strings.Join(got, "\n"))
	}
	// In time order: the stamps at their own times, the key when it was
	// first registered (here, as on an upgraded board, after them).
	var key logLeaf
	var stamps []logLeaf
	for _, l := range notaryLeaves(t, got[len(old):]) {
		if l.Op == "notary.key" {
			key = l
		} else {
			stamps = append(stamps, l)
		}
	}
	if key.PublicKey == "" || len(stamps) != 2 {
		t.Fatalf("backfilled notary leaves: %+v %+v", key, stamps)
	}
	for i, r := range []services.NotaryReceipt{r1, r2} {
		l := stamps[i]
		if l.Op != "notary.stamp" || l.Hash != r.Hash || l.Seq != r.Seq || l.At != r.Time || l.KeyID != r.KeyID || l.Signature != r.Signature {
			t.Fatalf("backfilled stamp %d: %+v for %+v", i, l, r)
		}
		if l.KeyID != key.KeyID || !services.VerifyNotaryReceipt(key.PublicKey, r) {
			t.Fatalf("receipt %d does not verify against the logged key", i)
		}
	}
	// And only once.
	if _, err = tlogCatchUp(testContext, tx); err != nil {
		t.Fatal(err)
	}
	if again := txLeaves(t, tx); len(again) != len(got) {
		t.Fatalf("a second catch-up appended %d leaves", len(again)-len(got))
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// The backfilled stamp proves like a new one.
	if _, err = s.SignCheckpoint(testContext); err != nil {
		t.Fatal(err)
	}
	cp, err := s.ReadLogCheckpoint(testContext, -1)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := s.ReadNotaryProof(testContext, r1.Hash, -1)
	if err != nil {
		t.Fatal(err)
	}
	body := verifyCheckpoint(t, s, cp)
	verifyInclusion(t, s, proof, body)
	if len(proof.Related) != 1 || !strings.Contains(proof.Related[0].Leaf.Data, `"op":"notary.key"`) {
		t.Fatalf("related: %+v", proof.Related)
	}
	verifyInclusion(t, s, proof.Related[0], body)
}
