package board

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/tlog"
)

func erasePreview(t *testing.T, s *Store, key []byte, rid string) map[string]any {
	t.Helper()
	r := run(t, s, signedNow(s, key, Command{Operation: "account.erase", Data: `{"schema":1,"step":"preview"}`, RequestID: rid}))
	return r.Data
}

func eraseConfirm(s *Store, key []byte, hold, text, rid string) Command {
	return signedNow(s, key, Command{Operation: "account.erase", Data: `{"schema":1,"step":"confirm","hold":"` + hold + `","confirm":"` + text + `"}`, RequestID: rid})
}

// C1012 build 2: the owner's signed account.erase. A preview changes
// nothing and returns counts and a hold; a confirm needs that hold and the
// typed text, refuses open obligations, erases, records erasure_log, signs
// the receipt with the log key and logs its hash as an account.erase leaf.
// The key then signs nothing more; another account is untouched.
func TestAccountEraseOwner(t *testing.T) {
	s := openTest(t, Config{})
	alice, bob := keyFor(211), keyFor(212)
	a, b := keyID(alice), keyID(bob)
	run(t, s, signedNow(s, alice, Command{Operation: "agent.register", Handle: "alice-owner-erase"}))
	register(t, s, bob)
	post := run(t, s, signedNow(s, alice, Command{Operation: "post", Room: "lobby", Text: "alice words to erase", RequestID: "ae1"})).Receipt.ID
	bobPost := run(t, s, signedNow(s, bob, Command{Operation: "post", Room: "lobby", Text: "bob stays", ReplyTo: post, RequestID: "be1"})).Receipt.ID
	bobBefore := eraseRows(t, s, b)

	fails(t, s, Command{Operation: "account.erase", Data: `{"schema":1,"step":"preview"}`, RequestID: "anon"}, "signature_required")
	fails(t, s, signedNow(s, alice, Command{Operation: "account.erase", Data: `{"schema":1,"step":"later"}`, RequestID: "bad"}), "invalid_erase_data")
	fails(t, s, signedNow(s, alice, Command{Operation: "account.erase", Data: `{"schema":1,"step":"confirm"}`, RequestID: "bad2"}), "invalid_erase_data")

	p := erasePreview(t, s, alice, "pv1")
	if p["confirm_text"] != "erase "+a[:8] || p["hold"] == "" || p["confirm_after"].(int64) != testTime {
		t.Fatalf("preview: %v", p)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM events WHERE id=? AND text='alice words to erase' AND hidden=0", post); n != 1 {
		t.Fatal("a preview changed the post")
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM erasure_log"); n != 0 {
		t.Fatal("a preview wrote erasure_log")
	}
	hold := p["hold"].(string)
	fails(t, s, eraseConfirm(s, alice, hold, "erase "+b[:8], "c0"), "erase_confirm_mismatch")
	fails(t, s, eraseConfirm(s, alice, "1.2.x", "erase "+a[:8], "c1"), "erase_hold_invalid")
	// Another account's hold, and a hold whose counts changed, are refused.
	fails(t, s, eraseConfirm(s, alice, erasePreview(t, s, bob, "pvb")["hold"].(string), "erase "+a[:8], "c2"), "erase_hold_invalid")
	run(t, s, signedNow(s, alice, Command{Operation: "post", Room: "lobby", Text: "one more", RequestID: "ae2"}))
	fails(t, s, eraseConfirm(s, alice, hold, "erase "+a[:8], "c3"), "erase_hold_invalid")

	// Open work refuses, in the preview's blocking and the confirm alike.
	if _, err := s.db.Exec("INSERT INTO works(id,requester,title,capabilities,state,generation,created_at,updated_at,deadline,worker) VALUES(?,?,'t','[]','claimed','g',1,1,1,?)", bobPost, b, a); err != nil {
		t.Fatal(err)
	}
	fails(t, s, signedNow(s, alice, Command{Operation: "account.erase", Data: `{"schema":1,"step":"preview"}`, RequestID: "pv2"}), "erase_pending_obligations")
	if _, err := s.db.Exec("UPDATE works SET state='accepted' WHERE id=?", bobPost); err != nil {
		t.Fatal(err)
	}

	hold = erasePreview(t, s, alice, "pv3")["hold"].(string)
	r := run(t, s, eraseConfirm(s, alice, hold, "erase "+a[:8], "c4"))
	hash, _ := r.Data["receipt_sha256"].(string)
	if len(hash) != 64 || r.Data["warning"] != nil {
		t.Fatalf("confirm: %v", r.Data)
	}
	receipt, _ := json.Marshal(r.Data["receipt"])
	if sha256Hex(receipt) != hash {
		t.Fatalf("receipt hash %s is not the receipt's %s", hash, sha256Hex(receipt))
	}
	text, err := tlog.OpenNote([]byte(r.Data["signed_receipt"].(string)), s.LogVerifierKey())
	if err != nil || !strings.Contains(text, "receipt_sha256 "+hash+"\n") || !strings.Contains(text, "account "+a+"\n") {
		t.Fatalf("signed receipt %q: %v", text, err)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM events WHERE account=? AND (text<>'' OR hidden=0)", a); n != 0 {
		t.Fatalf("%d of alice's posts kept text or stayed visible", n)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM erasure_log WHERE account=? AND receipt_sha256=?", a, hash); n != 1 {
		t.Fatal("no erasure_log row")
	}
	var leaf string
	if err := s.db.QueryRow("SELECT data FROM tlog_leaves WHERE kind='identity' AND subject=? AND data LIKE '%account.erase%'", a).Scan(&leaf); err != nil {
		t.Fatalf("no account.erase leaf: %v", err)
	}
	if !strings.Contains(leaf, `"hash":"`+hash+`"`) || strings.Contains(leaf, "counts") || strings.Contains(leaf, "detail") {
		t.Fatalf("leaf %s", leaf)
	}
	if eraseRows(t, s, b) != bobBefore {
		t.Fatal("bob's rows changed")
	}
	fails(t, s, signedNow(s, alice, Command{Operation: "post", Room: "lobby", Text: "after", RequestID: "ae3"}), "key_revoked")
	// The side copy a restore replays.
	side, err := readErasureSide(s.erasureSide)
	if err != nil || len(side) != 1 || side[0].ReceiptSHA256 != hash {
		t.Fatalf("side file: %v %v", side, err)
	}
}

// A key changed in the last 7 days delays the erasure: the hold opens 24
// hours after the preview.
func TestAccountEraseRecentKeyWaits(t *testing.T) {
	s := openTest(t, Config{})
	alice := keyFor(213)
	a := keyID(alice)
	register(t, s, alice)
	if _, err := s.db.Exec("INSERT INTO audit(operation,actor,target,detail,created_at) VALUES('agent.rotate',?,?,'key rotation',?)", a, a, testTime-3600); err != nil {
		t.Fatal(err)
	}
	p := erasePreview(t, s, alice, "pw1")
	if p["confirm_after"].(int64) != testTime+EraseDelaySeconds {
		t.Fatalf("preview: %v", p)
	}
	hold := p["hold"].(string)
	fails(t, s, eraseConfirm(s, alice, hold, "erase "+a[:8], "cw1"), "erase_waiting")
	s.now = func() time.Time { return time.Unix(testTime+EraseDelaySeconds+60, 0) }
	run(t, s, eraseConfirm(s, alice, hold, "erase "+a[:8], "cw2"))
}

// A restore from a copy older than an erasure: the next Open replays it
// from the side file, with its original erasure_log row, before anything
// else reads the database.
func TestErasureReplayAfterRestore(t *testing.T) {
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
	alice := keyFor(214)
	a := keyID(alice)
	s := open()
	register(t, s, alice)
	post := run(t, s, signedNow(s, alice, Command{Operation: "post", Room: "lobby", Text: "old snapshot words", RequestID: "rs1"})).Receipt.ID
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s = open()
	hold := erasePreview(t, s, alice, "rp1")["hold"].(string)
	hash := run(t, s, eraseConfirm(s, alice, hold, "erase "+a[:8], "rc1")).Data["receipt_sha256"].(string)
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	// Restore the older copy.
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Remove(path + suffix)
	}
	if err = os.WriteFile(path, snapshot, 0600); err != nil {
		t.Fatal(err)
	}
	s = open()
	defer s.Close()
	if n := sqlCount(t, s, "SELECT count(*) FROM events WHERE id=? AND text='' AND hidden=1 AND hidden_by='author'", post); n != 1 {
		t.Fatal("the restored post was not erased again")
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM erasure_log WHERE account=? AND receipt_sha256=?", a, hash); n != 1 {
		t.Fatal("the restored copy has no erasure_log row")
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM audit WHERE operation='account.erase.replay' AND target=?", a); n != 1 {
		t.Fatal("no replay audit row")
	}
	fails(t, s, signedNow(s, alice, Command{Operation: "post", Room: "lobby", Text: "after restore", RequestID: "rs2"}), "key_revoked")
	// A second start replays nothing more, and a database row missing from
	// the side file is appended to it.
	if n, err := s.replayErasures(context.Background()); err != nil || n != 0 {
		t.Fatalf("second replay: %d %v", n, err)
	}
	if err = os.Remove(s.erasureSide); err != nil {
		t.Fatal(err)
	}
	if _, err = s.replayErasures(context.Background()); err != nil {
		t.Fatal(err)
	}
	if side, _ := readErasureSide(s.erasureSide); len(side) != 1 || side[0].Account != a {
		t.Fatalf("side file not rebuilt: %v", side)
	}
}
