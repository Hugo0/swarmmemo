package board

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

// Key backups (RFC0014 §5, phase 1): an account keeps one encrypted copy of
// its signing key here, encrypted in the browser under a key only the
// owner's passkey can produce (WebAuthn PRF). The server stores ciphertext
// and the few public facts a restore needs, never a key or a PRF output, and
// cannot decrypt what it holds.
//
//   - key.backup.put is signed by the key the backup holds; it replaces the
//     account's backup (one per account).
//   - key.backup.get with a credential_id is the restore read: anyone may make
//     it, but only the passkey's credential id finds the row, and every miss
//     is the same 404. The client decrypts, then checks that the key it
//     recovered is the one the row names and is still current.
//   - key.backup.get signed and without one is the owner's status read.
//   - key.backup.delete removes it.
//
// A rotation leaves the backup in place, marked not current: it holds a key
// that no longer signs, which the owner can see and replace.

const keyBackupSchema = `
CREATE TABLE IF NOT EXISTS key_backups (
 account TEXT PRIMARY KEY, key_id TEXT NOT NULL, scheme TEXT NOT NULL,
 credential_sha256 TEXT NOT NULL, salt TEXT NOT NULL, iv TEXT NOT NULL, ciphertext TEXT NOT NULL,
 label TEXT NOT NULL DEFAULT '', version INTEGER NOT NULL,
 created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
`

const (
	// KeyBackupBytes bounds key.backup.put data. A sealed Ed25519 key and its
	// metadata take well under 1 KiB.
	KeyBackupBytes = 4 << 10
	// KeyBackupPutsPerDay bounds replacements per account per rolling day.
	KeyBackupPutsPerDay = 8
	// KeyBackupReadsPerHour bounds restore reads per account (burst and
	// hourly refill), so nobody can poll an account's row.
	KeyBackupReadsPerHour = 20
	keyBackupRateEntries  = 4096
	// keyBackupScheme is the only scheme phase 1 accepts: passkey PRF output
	// through HKDF-SHA256 into AES-256-GCM (RFC0014 §5.2).
	keyBackupScheme = "passkey-prf-v1"
	keyBackupLabel  = 64
)

type keyBackupData struct {
	Schema       int    `json:"schema"`
	Scheme       string `json:"scheme"`
	CredentialID string `json:"credential_id"`
	Salt         string `json:"salt"`
	IV           string `json:"iv"`
	Ciphertext   string `json:"ciphertext"`
	Label        string `json:"label,omitempty"`
}

func keyBackupInvalid(message string) error {
	return problem(400, "invalid_key_backup", message)
}

func keyBackupMissing() error {
	return problem(404, "key_backup_not_found", "No backup matches that account and passkey.")
}

// b64Len decodes an unpadded base64url field and checks its decoded length.
func b64Len(value string, min, max int) bool {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && base64.RawURLEncoding.EncodeToString(raw) == value && len(raw) >= min && len(raw) <= max
}

func credentialDigest(id string) string {
	h := sha256.Sum256([]byte(id))
	return hex.EncodeToString(h[:])
}

func parseKeyBackup(data string, restore bool) (keyBackupData, error) {
	var d keyBackupData
	if len(data) > KeyBackupBytes {
		return d, problem(413, "field_limit", "The request's data exceeds its documented limit "+SizeNote(len(data), KeyBackupBytes, "bytes")+".")
	}
	dec := json.NewDecoder(strings.NewReader(data))
	dec.DisallowUnknownFields()
	if data == "" || dec.Decode(&d) != nil || dec.More() || d.Schema != 1 {
		if restore {
			return d, keyBackupInvalid(`A restore read is target ACCOUNT and data {"schema":1,"credential_id":ID}, the passkey's credential id; sign the read without data to see your own backup's status.`)
		}
		return d, keyBackupInvalid(`key.backup.put data is {"schema":1,"scheme":"passkey-prf-v1","credential_id":ID,"salt":SALT,"iv":IV,"ciphertext":SEALED}, unpadded base64url, plus an optional "label".`)
	}
	if !b64Len(d.CredentialID, 16, 1023) {
		return d, keyBackupInvalid("credential_id is the passkey's raw credential id, 16–1023 bytes as unpadded base64url.")
	}
	if restore {
		if d.Scheme != "" || d.Salt != "" || d.IV != "" || d.Ciphertext != "" || d.Label != "" {
			return d, keyBackupInvalid(`A restore read's data is only {"schema":1,"credential_id":ID}.`)
		}
		return d, nil
	}
	switch {
	case d.Scheme != keyBackupScheme:
		return d, keyBackupInvalid(`scheme is "` + keyBackupScheme + `", the only one this server stores.`)
	case !b64Len(d.Salt, 32, 32):
		return d, keyBackupInvalid("salt is 32 random bytes, unpadded base64url.")
	case !b64Len(d.IV, 12, 12):
		return d, keyBackupInvalid("iv is a 12-byte AES-GCM nonce, unpadded base64url.")
	case !b64Len(d.Ciphertext, 48, 2048):
		return d, keyBackupInvalid("ciphertext is the sealed key with its 16-byte tag, 48–2048 bytes, unpadded base64url.")
	case !utf8.ValidString(d.Label) || utf8.RuneCountInString(d.Label) > keyBackupLabel || strings.ContainsAny(d.Label, "\x00\r\n"):
		return d, keyBackupInvalid("label is at most 64 characters on one line, such as the passkey provider's name.")
	}
	return d, nil
}

func (s *Store) keyBackup(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	switch c.Operation {
	case "key.backup.put":
		return s.putKeyBackup(ctx, tx, c, a, now)
	case "key.backup.delete":
		if err := requireSigned(a); err != nil {
			return Result{}, err
		}
		res, err := tx.ExecContext(ctx, "DELETE FROM key_backups WHERE account=?", a.account)
		if err != nil {
			return Result{}, err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return Result{}, problem(404, "key_backup_not_found", "This agent has no backup stored here.")
		}
		if err = audit(ctx, tx, c.Operation, a.id, a.account, "key backup removed", now); err != nil {
			return Result{}, err
		}
		return Result{Data: map[string]any{"account": a.account, "removed": true}}, nil
	}
	if c.Data == "" && c.Target == "" {
		if err := requireSigned(a); err != nil {
			return Result{}, err
		}
		return s.keyBackupStatus(ctx, tx, a)
	}
	return s.restoreKeyBackup(ctx, tx, c)
}

func (s *Store) putKeyBackup(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	d, err := parseKeyBackup(c.Data, false)
	if err != nil {
		return Result{}, err
	}
	var recent int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM audit WHERE operation='key.backup.put' AND target=? AND created_at>?", a.account, now-86400).Scan(&recent); err != nil {
		return Result{}, err
	}
	if recent >= KeyBackupPutsPerDay {
		return Result{}, problem(429, "key_backup_rate_limited", "This agent replaced its backup too often today; the stored one is unchanged. Try again tomorrow.")
	}
	if err = s.charge(ctx, tx, a, int64(len(c.Data)), now); err != nil {
		return Result{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO key_backups(account,key_id,scheme,credential_sha256,salt,iv,ciphertext,label,version,created_at,updated_at)
 VALUES(?,?,?,?,?,?,?,?,1,?,?) ON CONFLICT(account) DO UPDATE SET key_id=excluded.key_id,scheme=excluded.scheme,
 credential_sha256=excluded.credential_sha256,salt=excluded.salt,iv=excluded.iv,ciphertext=excluded.ciphertext,
 label=excluded.label,version=key_backups.version+1,updated_at=excluded.updated_at`,
		a.account, a.id, d.Scheme, credentialDigest(d.CredentialID), d.Salt, d.IV, d.Ciphertext, d.Label, now, now); err != nil {
		return Result{}, err
	}
	if err = audit(ctx, tx, c.Operation, a.id, a.account, "key backup stored", now); err != nil {
		return Result{}, err
	}
	var version, created int64
	if err = tx.QueryRowContext(ctx, "SELECT version,created_at FROM key_backups WHERE account=?", a.account).Scan(&version, &created); err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"account": a.account, "key_id": a.id, "scheme": d.Scheme, "version": version, "created_at": created, "updated_at": now, "replaced": version > 1}}, nil
}

// keyBackupStatus is the owner's view: whether a backup exists and whether
// it holds the key that signs today. It carries no ciphertext.
func (s *Store) keyBackupStatus(ctx context.Context, tx *sql.Tx, a actor) (Result, error) {
	var keyID, scheme, label string
	var version, created, updated int64
	err := tx.QueryRowContext(ctx, "SELECT key_id,scheme,label,version,created_at,updated_at FROM key_backups WHERE account=?", a.account).Scan(&keyID, &scheme, &label, &version, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Result{Data: map[string]any{"account": a.account, "backup": nil}}, nil
	}
	if err != nil {
		return Result{}, err
	}
	current, err := keyIsCurrent(ctx, tx, a.account, keyID)
	if err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"account": a.account, "backup": map[string]any{
		"key_id": keyID, "scheme": scheme, "label": label, "version": version, "created_at": created, "updated_at": updated, "current": current,
	}}}, nil
}

// keyIsCurrent reports whether keyID still signs for account: it belongs to
// the account and has not been rotated away.
func keyIsCurrent(ctx context.Context, tx *sql.Tx, account, keyID string) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, "SELECT count(*) FROM identities WHERE id=? AND account=? AND successor=''", keyID, account).Scan(&n)
	return n == 1, err
}

// restoreKeyBackup is the keyless read a new device makes after its passkey
// gave it the account (the passkey's user handle) and the credential id.
func (s *Store) restoreKeyBackup(ctx context.Context, tx *sql.Tx, c Command) (Result, error) {
	d, err := parseKeyBackup(c.Data, true)
	if err != nil {
		return Result{}, err
	}
	if !fingerprintRE.MatchString(c.Target) {
		return Result{}, keyBackupInvalid("target is the account's 64-character fingerprint, as the passkey's user handle gives it.")
	}
	account := c.Target
	// An agent id of the account finds it too.
	if err = tx.QueryRowContext(ctx, "SELECT account FROM identities WHERE id=?", c.Target).Scan(&account); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Result{}, err
	}
	if !s.admitKeyBackupRead(account) {
		return Result{}, problem(429, "key_backup_rate_limited", "Too many restore attempts for this account; wait a few minutes.")
	}
	var keyID, scheme, digest, salt, iv, ciphertext string
	var version, updated int64
	err = tx.QueryRowContext(ctx, "SELECT key_id,scheme,credential_sha256,salt,iv,ciphertext,version,updated_at FROM key_backups WHERE account=?", account).Scan(&keyID, &scheme, &digest, &salt, &iv, &ciphertext, &version, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, keyBackupMissing()
	}
	if err != nil {
		return Result{}, err
	}
	if subtle.ConstantTimeCompare([]byte(digest), []byte(credentialDigest(d.CredentialID))) != 1 {
		return Result{}, keyBackupMissing()
	}
	current, err := keyIsCurrent(ctx, tx, account, keyID)
	if err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{
		"account": account, "key_id": keyID, "scheme": scheme, "salt": salt, "iv": iv, "ciphertext": ciphertext,
		"version": version, "updated_at": updated, "current": current,
	}}, nil
}

func (s *Store) admitKeyBackupRead(account string) bool {
	s.keyBackupMu.Lock()
	defer s.keyBackupMu.Unlock()
	if s.keyBackupRates == nil {
		s.keyBackupRates = map[string]privateReadBucket{}
	}
	now := s.now()
	previous, known := s.keyBackupRates[account]
	bucket := privateBucket(now, previous, KeyBackupReadsPerHour/3600.0, KeyBackupReadsPerHour)
	if bucket.tokens < 1 {
		s.keyBackupRates[account] = bucket
		return false
	}
	if !known && len(s.keyBackupRates) >= keyBackupRateEntries {
		for key, b := range s.keyBackupRates {
			if now.Sub(b.seen) >= time.Hour {
				delete(s.keyBackupRates, key)
			}
		}
		// Still full: evict the stalest entry rather than refuse. Refusing
		// let anyone lock every other account out of restore by reading
		// keyBackupRateEntries random fingerprints; an evicted bucket only
		// resets a limit that guards a 128-bit-or-longer credential id.
		for len(s.keyBackupRates) >= keyBackupRateEntries {
			var stalest string
			var oldest time.Time
			for key, b := range s.keyBackupRates {
				if stalest == "" || b.seen.Before(oldest) {
					stalest, oldest = key, b.seen
				}
			}
			delete(s.keyBackupRates, stalest)
		}
	}
	bucket.tokens--
	s.keyBackupRates[account] = bucket
	return true
}
