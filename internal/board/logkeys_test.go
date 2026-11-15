package board

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"swarmmemo/internal/tlog"
)

// openLogKeyStore opens the database at path with the log key in keyFile.
func openLogKeyStore(t *testing.T, path, keyFile string) *Store {
	t.Helper()
	s, err := Open(path, Config{EchoSimulate: true, LogKeyFile: keyFile})
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Unix(testTime, 0) }
	return s
}

func signLogCheckpoint(t *testing.T, s *Store, author byte, text string) LogCheckpoint {
	t.Helper()
	run(t, s, signed(keyFor(author), Command{Operation: "post", Room: "lobby", Text: text}))
	if ok, err := s.SignCheckpoint(testContext); err != nil || !ok {
		t.Fatal("checkpoint", ok, err)
	}
	cp, err := s.ReadLogCheckpoint(testContext, -1)
	if err != nil {
		t.Fatal(err)
	}
	return cp
}

func logKeyIDOf(t *testing.T, s *Store) string {
	sum := sha256.Sum256(s.transparency.signer.PublicKey())
	return hex.EncodeToString(sum[:])
}

// TestLogKeyHistoryGenesis (C983): the first checkpoint puts the running key
// on the history, key_id = SHA-256 of the public key; checkpoints and anchors
// carry it; first_anchor is the earliest confirmed anchor; a reopen with the
// same key keeps one entry.
func TestLogKeyHistoryGenesis(t *testing.T) {
	dir := t.TempDir()
	path, keyFile := filepath.Join(dir, "board.sqlite"), filepath.Join(dir, "log.key")
	s := openLogKeyStore(t, path, keyFile)
	if _, err := s.ReadLogKeys(testContext); err == nil {
		t.Fatal("a key history before any checkpoint")
	}
	if err := s.CheckLogKey(testContext); err != nil {
		t.Fatal("a fresh log refused its key:", err)
	}
	first := signLogCheckpoint(t, s, 71, "genesis")
	id := logKeyIDOf(t, s)
	if first.KeyID != id || s.LogKeyID() != id || first.VerifierKey != s.LogVerifierKey() {
		t.Fatalf("checkpoint key %s / %s, want %s", first.KeyID, first.VerifierKey, id)
	}
	second := signLogCheckpoint(t, s, 72, "later")
	for _, cp := range []LogCheckpoint{first, second} {
		if _, err := s.db.Exec("INSERT INTO tlog_anchors(size,digest,ots,state,bitcoin_height,calendars,submitted_at) VALUES(?,'d',x'00',?,?,'',?)", cp.Size, map[bool]string{true: "pending", false: "confirmed"}[cp.Size == first.Size], map[bool]int{true: 0, false: 970113}[cp.Size == first.Size], testTime); err != nil {
			t.Fatal(err)
		}
	}
	keys, err := s.ReadLogKeys(testContext)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].KeyID != id || keys[0].VerifierKey != s.LogVerifierKey() || keys[0].FirstSize != first.Size || keys[0].FirstAt != first.CreatedAt || keys[0].RetiredSize != nil || keys[0].RotationNote != nil {
		t.Fatalf("keys %+v", keys)
	}
	// The earliest confirmed anchor, not the earlier pending one.
	if a := keys[0].FirstAnchor; a == nil || a.Size != second.Size || a.State != "confirmed" || a.BitcoinHeight == nil || *a.BitcoinHeight != 970113 || a.OTSURL == "" {
		t.Fatalf("first anchor %+v", keys[0].FirstAnchor)
	}
	anchors, err := s.ReadLogAnchors(testContext, 0, 10)
	if err != nil || len(anchors) != 2 || anchors[0].KeyID != id || anchors[1].KeyID != id {
		t.Fatalf("anchors %+v %v", anchors, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openLogKeyStore(t, path, keyFile)
	defer s.Close()
	if keys, err = s.ReadLogKeys(testContext); err != nil || len(keys) != 1 || keys[0].KeyID != id {
		t.Fatalf("after a reopen: %+v %v", keys, err)
	}
	if err = s.CheckLogKey(testContext); err != nil {
		t.Fatal(err)
	}
}

// TestLogKeyHistoryRefusesUnknownKey: a database whose oldest checkpoint does
// not open under the running key (a lost or swapped log.key) still opens, for
// operator commands, but refuses to serve and signs nothing; a database from
// before the history (schema 52) is seeded from its oldest checkpoint.
func TestLogKeyHistoryRefusesUnknownKey(t *testing.T) {
	dir := t.TempDir()
	path, keyFile, otherKey := filepath.Join(dir, "board.sqlite"), filepath.Join(dir, "log.key"), filepath.Join(dir, "other.key")
	s := openLogKeyStore(t, path, keyFile)
	first := signLogCheckpoint(t, s, 71, "genesis")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Another key on the same database.
	s = openLogKeyStore(t, path, otherKey)
	if err := s.CheckLogKey(testContext); !errors.Is(err, errLogKeyUnknown) {
		t.Fatalf("CheckLogKey = %v", err)
	}
	run(t, s, signed(keyFor(72), Command{Operation: "post", Room: "lobby", Text: "after the swap"}))
	if _, err := s.SignCheckpoint(testContext); !errors.Is(err, errLogKeyUnknown) {
		t.Fatalf("SignCheckpoint with an unknown key = %v", err)
	}
	// An old checkpoint still names the key that signed it.
	if cp, err := s.ReadLogCheckpoint(testContext, first.Size); err != nil || cp.KeyID != first.KeyID || cp.VerifierKey != first.VerifierKey {
		t.Fatalf("old checkpoint %+v %v", cp, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// A schema-52 database: no history yet. The wrong key seeds nothing and
	// is refused; the right one is seeded with the oldest checkpoint.
	db := rawDB(t, path)
	if _, err := db.Exec("DROP TABLE tlog_keys; " + tlogKeySchema); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	s = openLogKeyStore(t, path, otherKey)
	if err := s.CheckLogKey(testContext); !errors.Is(err, errLogKeyUnknown) {
		t.Fatalf("CheckLogKey on an unseeded history = %v", err)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM tlog_keys"); n != 0 {
		t.Fatalf("the wrong key was seeded (%d rows)", n)
	}
	_ = s.Close()
	s = openLogKeyStore(t, path, keyFile)
	defer s.Close()
	keys, err := s.ReadLogKeys(testContext)
	if err != nil || len(keys) != 1 || keys[0].KeyID != first.KeyID || keys[0].FirstSize != first.Size {
		t.Fatalf("seeded keys %+v %v", keys, err)
	}
	if _, err = s.db.Exec("DELETE FROM tlog_keys"); err == nil {
		t.Fatal("a key was deleted from the history")
	}
}

// TestLogKeyHistoryRotation: with a rotation on the history (the write path
// is not built yet, so the test writes it), an old checkpoint reports the key
// that signed it, a new one the new key, and the rotation note opens under
// both.
func TestLogKeyHistoryRotation(t *testing.T) {
	dir := t.TempDir()
	path, keyFile, nextFile := filepath.Join(dir, "board.sqlite"), filepath.Join(dir, "log.key"), filepath.Join(dir, "next.key")
	s := openLogKeyStore(t, path, keyFile)
	old := signLogCheckpoint(t, s, 71, "genesis")
	oldSigner := s.transparency.signer
	run(t, s, signed(keyFor(72), Command{Operation: "post", Room: "lobby", Text: "next"}))
	_ = s.Close()
	nextKey, err := tlog.LoadOrCreateKey(nextFile)
	if err != nil {
		t.Fatal(err)
	}
	nextSigner, err := tlog.NewNoteSigner(oldSigner.Name(), nextKey)
	if err != nil {
		t.Fatal(err)
	}
	at := old.Size + 1
	note, err := tlog.SignRotation(oldSigner, nextSigner, at)
	if err != nil {
		t.Fatal(err)
	}
	db := rawDB(t, path)
	if _, err = db.Exec("UPDATE tlog_keys SET retired_size=?", at); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO tlog_keys(key_id,verifier_key,first_size,rotation_note) VALUES(?,?,?,?)", tlog.KeyID(nextSigner.PublicKey()), nextSigner.VerifierKey(), at, note); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	s = openLogKeyStore(t, path, nextFile)
	defer s.Close()
	if err = s.CheckLogKey(testContext); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.SignCheckpoint(testContext); err != nil || !ok {
		t.Fatal("checkpoint under the new key", ok, err)
	}
	latest, err := s.ReadLogCheckpoint(testContext, -1)
	if err != nil {
		t.Fatal(err)
	}
	early, err := s.ReadLogCheckpoint(testContext, old.Size)
	if err != nil {
		t.Fatal(err)
	}
	if early.KeyID != old.KeyID || early.VerifierKey != oldSigner.VerifierKey() || latest.KeyID != tlog.KeyID(nextSigner.PublicKey()) {
		t.Fatalf("old checkpoint key %s, new %s", early.KeyID, latest.KeyID)
	}
	if _, err = tlog.OpenNote([]byte(early.Note), early.VerifierKey); err != nil {
		t.Fatal("the old checkpoint does not open under the key it reports:", err)
	}
	if _, err = tlog.OpenNote([]byte(latest.Note), latest.VerifierKey); err != nil {
		t.Fatal("the new checkpoint does not open under the key it reports:", err)
	}
	keys, err := s.ReadLogKeys(testContext)
	if err != nil || len(keys) != 2 || keys[0].RetiredSize == nil || *keys[0].RetiredSize != at || keys[1].RotationNote == nil {
		t.Fatalf("keys %+v %v", keys, err)
	}
	r, err := tlog.OpenRotation(*keys[1].RotationNote, keys[0].VerifierKey)
	if err != nil || r.To != keys[1].VerifierKey || r.At != keys[1].FirstSize {
		t.Fatalf("rotation %+v %v", r, err)
	}
}
