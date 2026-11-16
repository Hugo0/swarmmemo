package board

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Raw write signals (C452): beside each write signal's hashed row
// (signals.go), the request as it arrived, for abuse handling and for
// reporting illegal posts to the authorities: the client address the hashes
// are made from, the TCP peer and the X-Forwarded-For chain when the proxy is
// trusted, every request header (credential-bearing ones replaced by
// "<redacted>": rawSecretHeader), the HTTP and TLS versions, an MCP call's
// clientInfo, and the write's operation, receipt id, room, account and
// channel. Reads are never recorded: only an accepted write reaches
// recordWriteSignal.
//
// Each record is sealed with AES-256-GCM (a random nonce per record) under
// the raw signals key (RAW_SIGNALS_KEY_FILE, default signals-raw.key beside
// the database, created 0600), bound to its row's clear columns. The records
// live in their own SQLite file (RAW_SIGNALS_DB_FILE, default signals-raw.db
// beside the database), not in the board database, its replica or its
// encrypted copies, so no schema bump and no backup carries them. A record is
// stored right after its write commits; one lost to a crash in between costs
// one record, never the write.
//
// Operator only: nothing but `swarmmemo signals raw` opens them. Kept
// RawSignalRetentionDays, then deleted by PruneWriteSignals (the hourly
// maintenance loop).

const (
	// RawSignalRetentionDays is how long a raw write signal is kept.
	RawSignalRetentionDays = 365
	// RawSignalsKeyFileName is the raw signals key's default file beside the database.
	RawSignalsKeyFileName = "signals-raw.key"
	// RawSignalsDBFileName is the raw signals' default database beside the board's.
	RawSignalsDBFileName = "signals-raw.db"
	// rawSignalHeaders bounds the headers one record keeps, and
	// rawSignalValueBytes each value.
	rawSignalHeaders    = 64
	rawSignalValues     = 8
	rawSignalValueBytes = 1024
	// rawRedacted replaces a credential-bearing header's value.
	rawRedacted = "<redacted>"
	// rawSignalListMax bounds one account listing.
	rawSignalListMax  = 1000
	rawSignalsVersion = 1
)

const rawSignalsSchema = `
CREATE TABLE IF NOT EXISTS raw_signals (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, created_at INTEGER NOT NULL, operation TEXT NOT NULL,
 object_id TEXT NOT NULL DEFAULT '', account TEXT NOT NULL DEFAULT '', via TEXT NOT NULL DEFAULT '',
 key_id TEXT NOT NULL, sealed BLOB NOT NULL);
CREATE INDEX IF NOT EXISTS raw_signals_object ON raw_signals(object_id);
CREATE INDEX IF NOT EXISTS raw_signals_account ON raw_signals(account,created_at);
CREATE INDEX IF NOT EXISTS raw_signals_created ON raw_signals(created_at);
`

// MCPClientInfo is an MCP call's self-declared client (clientInfo).
type MCPClientInfo struct {
	Name    string `json:"name,omitempty"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version,omitempty"`
}

// RawSignal is one decrypted raw record, for the operator.
type RawSignal struct {
	Seq          int64               `json:"-"`
	KeyID        string              `json:"-"`
	CreatedAt    int64               `json:"created_at"`
	Operation    string              `json:"operation"`
	ObjectID     string              `json:"object_id,omitempty"`
	RequestID    string              `json:"request_id,omitempty"`
	Account      string              `json:"account,omitempty"`
	Signer       string              `json:"signer,omitempty"`
	Via          string              `json:"via,omitempty"`
	Room         string              `json:"room,omitempty"`
	IP           string              `json:"ip,omitempty"`
	RemoteAddr   string              `json:"remote_addr,omitempty"`
	ForwardedFor string              `json:"forwarded_for,omitempty"`
	Proto        string              `json:"proto,omitempty"`
	TLS          string              `json:"tls,omitempty"`
	Origin       string              `json:"origin,omitempty"`
	Headers      map[string][]string `json:"headers,omitempty"`
	MCPClient    *MCPClientInfo      `json:"mcp_client,omitempty"`
	// Unreadable says why the record did not open (another key, a damaged
	// row); only the clear columns are set then.
	Unreadable string `json:"-"`
}

// rawSignalsState is the raw records' database, key and key id.
type rawSignalsState struct {
	db   *sql.DB // nil: a read-only open with no raw database yet
	aead cipher.AEAD
	id   string
}

// sealedRawSignal is one record sealed during its write, stored after commit.
type sealedRawSignal struct {
	createdAt                         int64
	operation, objectID, account, via string
	sealed                            []byte
}

// openRawSignals loads the raw signals key and opens their database; an
// in-memory board gets an in-memory database and a key that live as long as
// it. A read-only open (C553) creates neither.
func (s *Store) openRawSignals(path string) error {
	memory := path == ":memory:" || strings.HasPrefix(path, "file::memory:")
	keyFile, dbFile := s.config.RawSignalsKeyFile, s.config.RawSignalsDBFile
	if !memory {
		if keyFile == "" {
			keyFile = filepath.Join(filepath.Dir(path), RawSignalsKeyFileName)
		}
		if dbFile == "" {
			dbFile = filepath.Join(filepath.Dir(path), RawSignalsDBFileName)
		}
	}
	var key []byte
	var err error
	if keyFile == "" {
		key = make([]byte, 32)
		_, err = rand.Read(key)
	} else {
		key, err = s.hexKeyFile(keyFile, "raw-signals")
	}
	if err != nil {
		return err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(append([]byte("swarmmemo-raw-signals-key-id\x00"), key...))
	s.rawSignals = rawSignalsState{aead: aead, id: hex.EncodeToString(sum[:8])}
	db, err := openRawSignalsDB(dbFile, s.config.ReadOnly)
	if err != nil {
		return fmt.Errorf("raw signals database: %w", err)
	}
	s.rawSignals.db = db
	return nil
}

func openRawSignalsDB(dbFile string, readOnly bool) (*sql.DB, error) {
	dsn := ":memory:"
	if dbFile != "" {
		abs, err := filepath.Abs(dbFile)
		if err != nil {
			return nil, err
		}
		pragmas := connPragmas
		if readOnly {
			if _, err = os.Stat(abs); errors.Is(err, fs.ErrNotExist) {
				return nil, nil // nothing recorded yet
			}
			pragmas = readOnlyPragmas
		}
		dsn = sqliteFileDSN(abs, pragmas)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if readOnly && dbFile != "" {
		return db, nil
	}
	if err = migrateRawSignals(db); err == nil && dbFile != "" {
		err = os.Chmod(dbFile, 0o600)
	}
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// migrateRawSignals brings the raw signals database to rawSignalsVersion.
func migrateRawSignals(db *sql.DB) error {
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return err
	}
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > rawSignalsVersion {
		return fmt.Errorf("raw signals schema %d is newer than supported version %d", version, rawSignalsVersion)
	}
	if version == rawSignalsVersion {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(rawSignalsSchema); err != nil {
		return err
	}
	if _, err = tx.Exec(fmt.Sprintf("PRAGMA user_version=%d", rawSignalsVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

// RawSignalsKeyID names the current raw signals key; every record stores it.
func (s *Store) RawSignalsKeyID() string { return s.rawSignals.id }

// rawSignalAAD binds a sealed record to its row's clear columns, so a record
// moved to another row does not open.
func rawSignalAAD(keyID string, createdAt int64, operation, objectID, account, via string) []byte {
	return []byte(strings.Join([]string{"swarmmemo-raw-signal-v1", keyID, strconv.FormatInt(createdAt, 10), operation, objectID, account, via}, "\x00"))
}

// sealRawSignal seals one accepted write's raw request, or nil without a key.
func (s *Store) sealRawSignal(ctx context.Context, cmd Command, a actor, source string, result Result, now int64) (*sealedRawSignal, error) {
	if s.rawSignals.aead == nil {
		return nil, nil
	}
	sig := RequestSignalsFrom(ctx)
	rec := RawSignal{
		CreatedAt:    now,
		Operation:    cmd.Operation,
		RequestID:    signalText(cmd.RequestID, SignalTextBytes),
		Via:          signalText(signalVia(ctx), 32),
		Room:         signalText(cmd.Room, SignalTextBytes),
		IP:           signalText(source, 128),
		RemoteAddr:   signalText(sig.RemoteAddr, 128),
		ForwardedFor: signalText(sig.ForwardedFor, rawSignalValueBytes),
		Proto:        signalText(sig.Proto, 32),
		TLS:          signalText(sig.TLS, 64),
		Origin:       signalText(sig.Origin, SignalTextBytes),
		Headers:      rawSignalHeadersOf(sig.Headers),
	}
	if a.signed {
		rec.Account, rec.Signer = a.account, a.id
	}
	if result.Receipt != nil {
		rec.ObjectID = signalText(result.Receipt.ID, SignalTextBytes)
	}
	if c := sig.MCPClient; c != nil {
		rec.MCPClient = &MCPClientInfo{Name: signalText(c.Name, SignalTextBytes), Title: signalText(c.Title, SignalTextBytes), Version: signalText(c.Version, 64)}
	}
	plain, err := json.Marshal(rec)
	if err != nil {
		return nil, err
	}
	out := &sealedRawSignal{createdAt: now, operation: rec.Operation, objectID: rec.ObjectID, account: rec.Account, via: rec.Via}
	out.sealed = s.rawSignals.aead.Seal(nil, nil, plain, rawSignalAAD(s.rawSignals.id, now, out.operation, out.objectID, out.account, out.via))
	return out, nil
}

// storeRawSignal stores a write's sealed record once the write committed.
// A failure is logged (never the record) and costs only the record.
func (s *Store) storeRawSignal(ctx context.Context, r *sealedRawSignal) {
	if r == nil || s.rawSignals.db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if _, err := s.rawSignals.db.ExecContext(ctx, "INSERT INTO raw_signals(created_at,operation,object_id,account,via,key_id,sealed) VALUES(?,?,?,?,?,?,?)",
		r.createdAt, r.operation, r.objectID, r.account, r.via, s.rawSignals.id, r.sealed); err != nil {
		slog.Error("Raw write signal not stored", "operation", r.operation, "err", err)
	}
}

// rawSecretParts are the header-name fragments that mark a credential.
var rawSecretParts = []string{"auth", "cookie", "token", "secret", "key", "password", "passwd", "session", "signature", "credential", "payment", "otp"}

// rawSecretHeader reports whether a header's value can carry a credential
// (Authorization, Cookie, a bearer or bridge token, a payment signature, an
// API key, ...): its value is never stored.
func rawSecretHeader(name string) bool {
	n := strings.ToLower(name)
	for _, part := range rawSecretParts {
		if strings.Contains(n, part) {
			return true
		}
	}
	return false
}

// rawTokenSegment is a path segment long enough to be a token.
var rawTokenSegment = regexp.MustCompile(`[A-Za-z0-9_~.%+=-]{24,}`)

// rawReferer is a Referer without its query or fragment (they can carry
// message text or secrets) and without token-like path segments (a hosted
// identity's MCP URL carries its token in the path).
func rawReferer(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return rawRedacted
	}
	out := strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) + rawTokenSegment.ReplaceAllString(u.EscapedPath(), rawRedacted)
	if u.RawQuery != "" || u.Fragment != "" {
		out += "?" + rawRedacted
	}
	return signalText(out, rawSignalValueBytes)
}

// rawSignalHeadersOf is a request's headers as received, cleaned and
// bounded, with every credential-bearing value replaced by "<redacted>".
func rawSignalHeadersOf(h map[string][]string) map[string][]string {
	if len(h) == 0 {
		return nil
	}
	names := make([]string, 0, len(h))
	for name := range h {
		names = append(names, name)
	}
	sort.Strings(names)
	out := map[string][]string{}
	for _, raw := range names {
		if len(out) >= rawSignalHeaders {
			out["X-Swarmmemo-Headers-Dropped"] = []string{strconv.Itoa(len(names) - rawSignalHeaders)}
			break
		}
		name := textproto.CanonicalMIMEHeaderKey(signalText(raw, 128))
		if name == "" {
			continue
		}
		if rawSecretHeader(name) {
			out[name] = []string{rawRedacted}
			continue
		}
		for i, v := range h[raw] {
			if i >= rawSignalValues {
				break
			}
			if name == "Referer" {
				v = rawReferer(v)
			} else {
				v = signalText(v, rawSignalValueBytes)
			}
			out[name] = append(out[name], v)
		}
	}
	return out
}

// pruneRawSignals deletes raw records older than RawSignalRetentionDays, in
// bounded batches.
func (s *Store) pruneRawSignals(ctx context.Context) (int64, error) {
	if s.rawSignals.db == nil || s.config.ReadOnly {
		return 0, nil
	}
	cutoff := s.now().Unix() - RawSignalRetentionDays*86400
	var total int64
	for {
		r, err := s.rawSignals.db.ExecContext(ctx, "DELETE FROM raw_signals WHERE seq IN (SELECT seq FROM raw_signals WHERE created_at<? ORDER BY seq LIMIT ?)", cutoff, signalPruneBatch)
		if err != nil {
			return total, err
		}
		n, err := r.RowsAffected()
		if err != nil {
			return total, err
		}
		total += n
		if n < signalPruneBatch {
			return total, nil
		}
	}
}

// rawSignalRows decrypts the records a query selects.
func (s *Store) rawSignalRows(ctx context.Context, where string, args ...any) ([]RawSignal, error) {
	if s.rawSignals.db == nil {
		return nil, nil
	}
	rows, err := s.rawSignals.db.QueryContext(ctx, "SELECT seq,created_at,operation,object_id,account,via,key_id,sealed FROM raw_signals WHERE "+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RawSignal
	for rows.Next() {
		var r RawSignal
		var sealed []byte
		if err = rows.Scan(&r.Seq, &r.CreatedAt, &r.Operation, &r.ObjectID, &r.Account, &r.Via, &r.KeyID, &sealed); err != nil {
			return nil, err
		}
		out = append(out, s.openRawSignal(r, sealed))
	}
	return out, rows.Err()
}

// openRawSignal decrypts one record whose clear columns clear holds.
func (s *Store) openRawSignal(clear RawSignal, sealed []byte) RawSignal {
	if clear.KeyID != s.rawSignals.id {
		clear.Unreadable = "sealed under another raw signals key (" + clear.KeyID + ")"
		return clear
	}
	plain, err := s.rawSignals.aead.Open(nil, nil, sealed, rawSignalAAD(clear.KeyID, clear.CreatedAt, clear.Operation, clear.ObjectID, clear.Account, clear.Via))
	if err != nil {
		clear.Unreadable = "does not open under the raw signals key"
		return clear
	}
	var r RawSignal
	if err = json.Unmarshal(plain, &r); err != nil {
		clear.Unreadable = "damaged record"
		return clear
	}
	r.Seq, r.KeyID = clear.Seq, clear.KeyID
	return r
}

// RawSignalsForObject are the raw records of the write that made id (a
// message id or any other receipt id), oldest first.
func (s *Store) RawSignalsForObject(ctx context.Context, id string) ([]RawSignal, error) {
	if id == "" {
		return nil, nil
	}
	return s.rawSignalRows(ctx, "object_id=? ORDER BY seq LIMIT 100", id)
}

// RawSignalsForAccount are an account's raw records of the last days
// (at most RawSignalRetentionDays), oldest first, at most rawSignalListMax.
func (s *Store) RawSignalsForAccount(ctx context.Context, account string, days int) ([]RawSignal, error) {
	if days <= 0 || days > RawSignalRetentionDays {
		days = RawSignalRetentionDays
	}
	from := s.now().Unix() - int64(days)*86400
	rows, err := s.rawSignalRows(ctx, "account=? AND created_at>=? ORDER BY seq DESC LIMIT ?", account, from, rawSignalListMax)
	if err != nil {
		return nil, err
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Seq < rows[j].Seq })
	return rows, nil
}

// RawSignalCount is how many raw records the write that made id has; it
// opens none of them.
func (s *Store) RawSignalCount(ctx context.Context, id string) (int, error) {
	if s.rawSignals.db == nil || id == "" {
		return 0, nil
	}
	var n int
	err := s.rawSignals.db.QueryRowContext(ctx, "SELECT count(*) FROM raw_signals WHERE object_id=?", id).Scan(&n)
	return n, err
}
