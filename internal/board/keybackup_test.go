package board

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func b64Fill(n int, b byte) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat(string([]byte{b}), n)))
}

func backupData(credential string, mutate func(map[string]any)) string {
	d := map[string]any{"schema": 1, "scheme": "passkey-prf-v1", "credential_id": credential, "salt": b64Fill(32, 1), "iv": b64Fill(12, 2), "ciphertext": b64Fill(96, 3), "label": "Test passkey"}
	if mutate != nil {
		mutate(d)
	}
	encoded, _ := json.Marshal(d)
	return string(encoded)
}

func restoreData(credential string) string {
	return `{"schema":1,"credential_id":"` + credential + `"}`
}

func execErr(t *testing.T, s *Store, c Command) *Error {
	t.Helper()
	_, err := s.Execute(testContext, c, "test-origin")
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("%s: want a service error, got %v", c.Operation, err)
	}
	return e
}

func TestKeyBackupPutRestoreReplaceDelete(t *testing.T) {
	s := openTest(t, Config{})
	owner := keyFor(201)
	register(t, s, owner)
	account := keyID(owner)
	cred := b64Fill(32, 9)

	// Status before any backup: signed, empty.
	status := run(t, s, signed(owner, Command{Operation: "key.backup.get"}))
	if status.Data["backup"] != nil {
		t.Fatalf("a new agent has a backup: %v", status.Data)
	}

	put := run(t, s, signed(owner, Command{Operation: "key.backup.put", RequestID: "b1", Data: backupData(cred, nil)}))
	got := put.Data
	if got["key_id"] != account || got["version"] != int64(1) || got["replaced"] != false {
		t.Fatalf("first put: %v", got)
	}
	// The server stores a digest of the credential id, never the id itself.
	if n := sqlCount(t, s, "SELECT count(*) FROM key_backups WHERE account=? AND credential_sha256=?", account, credentialDigest(cred)); n != 1 {
		t.Fatalf("stored rows %d", n)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM key_backups WHERE instr(credential_sha256,?)>0 OR instr(ciphertext,?)>0", cred, cred); n != 0 {
		t.Fatal("the raw credential id is stored")
	}

	// Restore without a key, by account and credential id.
	restored := run(t, s, Command{Operation: "key.backup.get", Target: account, Data: restoreData(cred)})
	r := restored.Data
	if r["account"] != account || r["key_id"] != account || r["ciphertext"] != b64Fill(96, 3) || r["current"] != true || r["salt"] != b64Fill(32, 1) || r["iv"] != b64Fill(12, 2) {
		t.Fatalf("restore: %v", r)
	}

	// A wrong credential id, an unknown account and a missing row all answer the same 404.
	for _, c := range []Command{
		{Operation: "key.backup.get", Target: account, Data: restoreData(b64Fill(32, 8))},
		{Operation: "key.backup.get", Target: strings.Repeat("b", 64), Data: restoreData(cred)},
	} {
		if e := execErr(t, s, c); e.Status != 404 || e.Code != "key_backup_not_found" || e.Message != "No backup matches that account and passkey." {
			t.Fatalf("miss: %+v", e)
		}
	}

	// Replace: one row, version increments, the new credential is the one that finds it.
	cred2 := b64Fill(40, 7)
	put = run(t, s, signed(owner, Command{Operation: "key.backup.put", RequestID: "b2", Data: backupData(cred2, func(d map[string]any) { d["ciphertext"] = b64Fill(64, 4) })}))
	if got := put.Data; got["version"] != int64(2) || got["replaced"] != true {
		t.Fatalf("replace: %v", got)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM key_backups WHERE account=?", account); n != 1 {
		t.Fatalf("rows after replace %d", n)
	}
	if e := execErr(t, s, Command{Operation: "key.backup.get", Target: account, Data: restoreData(cred)}); e.Code != "key_backup_not_found" {
		t.Fatalf("old credential still finds the backup: %+v", e)
	}
	if r := run(t, s, Command{Operation: "key.backup.get", Target: account, Data: restoreData(cred2)}).Data; r["ciphertext"] != b64Fill(64, 4) {
		t.Fatalf("replaced ciphertext: %v", r)
	}

	// Owner status carries no ciphertext.
	st := run(t, s, signed(owner, Command{Operation: "key.backup.get"})).Data["backup"].(map[string]any)
	if st["current"] != true || st["version"] != int64(2) || st["label"] != "Test passkey" || st["ciphertext"] != nil {
		t.Fatalf("status: %v", st)
	}

	// Another agent cannot delete or read status of it.
	other := keyFor(202)
	register(t, s, other)
	if e := execErr(t, s, signed(other, Command{Operation: "key.backup.delete", RequestID: "d0"})); e.Code != "key_backup_not_found" {
		t.Fatalf("other delete: %+v", e)
	}
	run(t, s, signed(owner, Command{Operation: "key.backup.delete", RequestID: "d1"}))
	if n := sqlCount(t, s, "SELECT count(*) FROM key_backups"); n != 0 {
		t.Fatalf("rows after delete %d", n)
	}
	if e := execErr(t, s, signed(owner, Command{Operation: "key.backup.delete", RequestID: "d2"})); e.Status != 404 {
		t.Fatalf("second delete: %+v", e)
	}
}

func TestKeyBackupRequiresSignatureAndValidData(t *testing.T) {
	s := openTest(t, Config{})
	owner := keyFor(203)
	register(t, s, owner)
	cred := b64Fill(32, 9)
	if e := execErr(t, s, Command{Operation: "key.backup.put", RequestID: "u1", Data: backupData(cred, nil)}); e.Status != 401 {
		t.Fatalf("unsigned put: %+v", e)
	}
	if e := execErr(t, s, Command{Operation: "key.backup.delete", RequestID: "u2"}); e.Status != 401 {
		t.Fatalf("unsigned delete: %+v", e)
	}
	if e := execErr(t, s, Command{Operation: "key.backup.get"}); e.Status != 401 {
		t.Fatalf("unsigned status: %+v", e)
	}
	bad := map[string]func(map[string]any){
		"scheme":     func(d map[string]any) { d["scheme"] = "plaintext" },
		"salt":       func(d map[string]any) { d["salt"] = b64Fill(16, 1) },
		"iv":         func(d map[string]any) { d["iv"] = b64Fill(16, 1) },
		"short":      func(d map[string]any) { d["ciphertext"] = b64Fill(16, 1) },
		"long":       func(d map[string]any) { d["ciphertext"] = b64Fill(2049, 1) },
		"padded":     func(d map[string]any) { d["iv"] = base64.URLEncoding.EncodeToString([]byte("123456789012x")) },
		"credential": func(d map[string]any) { d["credential_id"] = b64Fill(4, 1) },
		"label":      func(d map[string]any) { d["label"] = "a\nb" },
		"extra":      func(d map[string]any) { d["private_key"] = "nope" },
		"schema":     func(d map[string]any) { d["schema"] = 2 },
	}
	for name, mutate := range bad {
		e := execErr(t, s, signed(owner, Command{Operation: "key.backup.put", RequestID: "bad-" + name, Data: backupData(cred, mutate)}))
		if e.Status != 400 || e.Code != "invalid_key_backup" {
			if !(name == "long" && e.Code == "field_limit") {
				t.Errorf("%s: %+v", name, e)
			}
		}
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM key_backups"); n != 0 {
		t.Fatalf("a refused put stored a row")
	}
	// The data cap.
	if e := execErr(t, s, signed(owner, Command{Operation: "key.backup.put", RequestID: "cap", Data: backupData(cred, func(d map[string]any) { d["label"] = strings.Repeat("x", KeyBackupBytes) })})); e.Code != "field_limit" || e.Status != 413 {
		t.Fatalf("cap: %+v", e)
	}
	// A restore read carries only the credential id, and needs a fingerprint target.
	for _, c := range []Command{
		{Operation: "key.backup.get", Target: keyID(owner), Data: backupData(cred, nil)},
		{Operation: "key.backup.get", Target: "atlas", Data: restoreData(cred)},
		{Operation: "key.backup.get", Target: keyID(owner), Data: "{}"},
		{Operation: "key.backup.get", Target: keyID(owner)},
	} {
		if e := execErr(t, s, c); e.Code != "invalid_key_backup" {
			t.Errorf("restore shape %q/%q: %+v", c.Target, c.Data, e)
		}
	}
}

func TestKeyBackupRateLimits(t *testing.T) {
	s := openTest(t, Config{})
	owner := keyFor(204)
	register(t, s, owner)
	cred := b64Fill(32, 9)
	for i := range KeyBackupPutsPerDay {
		run(t, s, signed(owner, Command{Operation: "key.backup.put", RequestID: "r" + string(rune('a'+i)), Data: backupData(cred, nil)}))
	}
	if e := execErr(t, s, signed(owner, Command{Operation: "key.backup.put", RequestID: "over", Data: backupData(cred, nil)})); e.Status != 429 || e.Code != "key_backup_rate_limited" {
		t.Fatalf("put over the day's cap: %+v", e)
	}
	// Restore reads: a burst, then refused, then refilled with time.
	clock := time.Unix(testTime, 0)
	s.now = func() time.Time { return clock }
	for range KeyBackupReadsPerHour {
		run(t, s, Command{Operation: "key.backup.get", Target: keyID(owner), Data: restoreData(cred)})
	}
	if e := execErr(t, s, Command{Operation: "key.backup.get", Target: keyID(owner), Data: restoreData(cred)}); e.Status != 429 {
		t.Fatalf("restore over the cap: %+v", e)
	}
	// Misses count too, so guessing credential ids is bounded.
	if e := execErr(t, s, Command{Operation: "key.backup.get", Target: keyID(owner), Data: restoreData(b64Fill(32, 1))}); e.Status != 429 {
		t.Fatalf("a miss over the cap: %+v", e)
	}
	clock = clock.Add(time.Hour)
	run(t, s, Command{Operation: "key.backup.get", Target: keyID(owner), Data: restoreData(cred)})
}

// A rotation keeps the backup but marks it not current; the old key can no
// longer touch it and the new key replaces it.
func TestKeyBackupAcrossRotation(t *testing.T) {
	s := openTest(t, Config{})
	old, next := keyFor(205), keyFor(206)
	register(t, s, old)
	account := keyID(old)
	cred := b64Fill(32, 9)
	run(t, s, signed(old, Command{Operation: "key.backup.put", RequestID: "p1", Data: backupData(cred, nil)}))
	rot := Command{Operation: "agent.rotate", Target: base64.RawURLEncoding.EncodeToString(next.Public().(ed25519.PublicKey)), RequestID: "rot", Timestamp: testTime, Nonce: "rot-nonce"}
	rot.PublicKey = base64.RawURLEncoding.EncodeToString(old.Public().(ed25519.PublicKey))
	canonical := Canonical("swarmmemo.com", rot)
	rot.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(old, canonical))
	rot.Proof = base64.RawURLEncoding.EncodeToString(ed25519.Sign(next, canonical))
	run(t, s, rot)

	r := run(t, s, Command{Operation: "key.backup.get", Target: account, Data: restoreData(cred)}).Data
	if r["current"] != false || r["key_id"] != account {
		t.Fatalf("after rotation the backup should be kept and marked not current: %v", r)
	}
	// Looked up by the new key's id, it is the same account's backup.
	if r := run(t, s, Command{Operation: "key.backup.get", Target: keyID(next), Data: restoreData(cred)}).Data; r["account"] != account {
		t.Fatalf("by new key id: %v", r)
	}
	if e := execErr(t, s, signed(old, Command{Operation: "key.backup.put", RequestID: "p2", Data: backupData(cred, nil)})); e.Code != "key_rotated" {
		t.Fatalf("rotated key put: %+v", e)
	}
	st := run(t, s, signed(next, Command{Operation: "key.backup.get"})).Data["backup"].(map[string]any)
	if st["current"] != false {
		t.Fatalf("status after rotation: %v", st)
	}
	run(t, s, signed(next, Command{Operation: "key.backup.put", RequestID: "p3", Data: backupData(cred, nil)}))
	r = run(t, s, Command{Operation: "key.backup.get", Target: account, Data: restoreData(cred)}).Data
	if r["current"] != true || r["key_id"] != keyID(next) || r["account"] != account {
		t.Fatalf("after re-backup: %v", r)
	}
}

// key_backups is additive: an earlier database (schema 15) without the table
// opens, gains it, migrates to schema 16, and an exact retry of a put replays.
func TestKeyBackupSchemaIsAdditive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "board.sqlite")
	s, err := Open(path, Config{})
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Unix(testTime, 0) }
	owner := keyFor(207)
	register(t, s, owner)
	if _, err = s.db.Exec("DROP TABLE key_backups; PRAGMA user_version=15"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if n := sqlCount(t, s, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='key_backups'"); n != 1 {
		t.Fatal("key_backups was not created on upgrade")
	}
	if v := sqlCount(t, s, "PRAGMA user_version"); v != SchemaVersion || SchemaVersion != 18 {
		t.Fatalf("user_version %d after the migration", v)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM identities WHERE id=?", keyID(owner)); n != 1 {
		t.Fatal("existing identity lost")
	}
	s.now = func() time.Time { return time.Unix(testTime, 0) }
	put := signed(owner, Command{Operation: "key.backup.put", RequestID: "retry", Data: backupData(b64Fill(32, 9), nil)})
	run(t, s, put)
	again := run(t, s, put)
	if again.Receipt != nil && !again.Receipt.Duplicate {
		t.Fatal("exact retry was not a replay")
	}
	if v := sqlCount(t, s, "SELECT version FROM key_backups"); v != 1 {
		t.Fatalf("a retry stored twice: version %d", v)
	}
}

func TestKeyBackupReadLimiterFullEvictsNotRefuses(t *testing.T) {
	s := &Store{now: func() time.Time { return time.Unix(1_800_000_000, 0) }}
	for i := 0; i < keyBackupRateEntries; i++ {
		if !s.admitKeyBackupRead(fmt.Sprintf("%064x", i)) {
			t.Fatalf("fill %d refused", i)
		}
	}
	victim := strings.Repeat("ab", 32)
	if !s.admitKeyBackupRead(victim) {
		t.Fatal("a full limiter locked a new account out of restore")
	}
	if len(s.keyBackupRates) > keyBackupRateEntries {
		t.Fatalf("limiter grew to %d", len(s.keyBackupRates))
	}
	for i := 1; i < KeyBackupReadsPerHour; i++ {
		s.admitKeyBackupRead(victim)
	}
	if s.admitKeyBackupRead(victim) {
		t.Fatal("per-account limit not enforced")
	}
}

func FuzzKeyBackupData(f *testing.F) {
	f.Add(`{"schema":1,"scheme":"passkey-prf-v1","credential_id":"AAAAAAAAAAAAAAAAAAAAAA","salt":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","iv":"AAAAAAAAAAAAAAAA","ciphertext":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}`, false)
	f.Add(`{"schema":1,"credential_id":"AAAAAAAAAAAAAAAAAAAAAA"}`, true)
	f.Fuzz(func(t *testing.T, data string, restore bool) {
		d, err := parseKeyBackup(data, restore)
		if err != nil {
			return
		}
		if len(data) > KeyBackupBytes || d.Schema != 1 || !b64Len(d.CredentialID, 16, 1023) {
			t.Fatalf("accepted %q", data)
		}
		if restore && (d.Ciphertext != "" || d.Salt != "" || d.IV != "" || d.Scheme != "" || d.Label != "") {
			t.Fatalf("restore accepted extra fields %q", data)
		}
		if !restore && (d.Scheme != keyBackupScheme || !b64Len(d.Salt, 32, 32) || !b64Len(d.IV, 12, 12) || !b64Len(d.Ciphertext, 48, 2048) || !utf8.ValidString(d.Label) || strings.ContainsAny(d.Label, "\x00\r\n")) {
			t.Fatalf("put accepted %q", data)
		}
	})
}
