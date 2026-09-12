package board

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Canonical preserves protocol-v1 bytes for ordinary commands; an explicit
// delegation context selects version 2; private read context selects version 3.
// Mixed contexts are rejected by admission. Enrollment/rotation possession proofs
// sign these same bytes with the target key.
// Fields retain Command declaration order. Zero values are omitted; signatures
// and proof are excluded to avoid circular signatures. No normalization occurs.
func Canonical(serviceID string, cmd Command) []byte {
	cmd.Signature, cmd.Proof = "", ""
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	version := 1
	if cmd.Delegation != nil {
		version = 2
	}
	if cmd.PrivateRead != nil {
		version = 3
	}
	_ = e.Encode(struct {
		Version int     `json:"version"`
		Service string  `json:"service"`
		Command Command `json:"command"`
	}{version, serviceID, cmd})
	return bytes.TrimSuffix(b.Bytes(), []byte{'\n'})
}

type Store struct {
	db                 *sql.DB
	config             Config
	generation         string
	cursorCipher       cipher.AEAD
	cursorMu           sync.RWMutex
	now                func() time.Time
	privateSlots       chan struct{}
	privateRateMu      sync.Mutex
	privateRates       map[string]privateReadBucket
	privateServiceRate privateReadBucket
}

const schema = `
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS identities (
 id TEXT PRIMARY KEY, public_key TEXT UNIQUE NOT NULL, account TEXT NOT NULL,
 handle TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL, last_seen INTEGER NOT NULL,
 successor TEXT NOT NULL DEFAULT '');
CREATE UNIQUE INDEX IF NOT EXISTS handles ON identities(handle) WHERE handle <> '';
CREATE INDEX IF NOT EXISTS identity_account ON identities(account);
CREATE TABLE IF NOT EXISTS rooms (
 name TEXT PRIMARY KEY, visibility TEXT NOT NULL CHECK(visibility IN ('public','private')),
 owner TEXT NOT NULL, created_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS members (room TEXT NOT NULL, account TEXT NOT NULL,
 PRIMARY KEY(room,account), FOREIGN KEY(room) REFERENCES rooms(name));
CREATE TABLE IF NOT EXISTS events (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, display_seq INTEGER NOT NULL, id TEXT UNIQUE NOT NULL,
 room TEXT NOT NULL REFERENCES rooms(name), page TEXT NOT NULL, text TEXT NOT NULL,
 kind TEXT NOT NULL, author TEXT NOT NULL, account TEXT NOT NULL,
 handle TEXT NOT NULL, public_key TEXT NOT NULL, signature TEXT NOT NULL,
 payload TEXT NOT NULL, created_at INTEGER NOT NULL, hash TEXT NOT NULL,
 reply_to TEXT NOT NULL, recipient TEXT NOT NULL,
 hidden INTEGER NOT NULL DEFAULT 0, reason TEXT NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS events_room_seq ON events(room,seq);
CREATE INDEX IF NOT EXISTS events_author ON events(account,seq);
CREATE INDEX IF NOT EXISTS events_recipient ON events(recipient,seq);
CREATE INDEX IF NOT EXISTS events_reply ON events(reply_to,room,seq);
CREATE INDEX IF NOT EXISTS events_page_directory ON events(room,page,seq) WHERE hidden=0;
CREATE TABLE IF NOT EXISTS counters (scope TEXT PRIMARY KEY, value INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS blobs (
 id TEXT PRIMARY KEY,room TEXT NOT NULL REFERENCES rooms(name),account TEXT NOT NULL,
 filename TEXT NOT NULL,media_type TEXT NOT NULL,hash TEXT NOT NULL,size INTEGER NOT NULL,
 created_at INTEGER NOT NULL,expires_at INTEGER NOT NULL,deleted INTEGER NOT NULL DEFAULT 0,
 data BLOB);
CREATE INDEX IF NOT EXISTS blobs_expiry ON blobs(expires_at) WHERE deleted=0;
CREATE TABLE IF NOT EXISTS event_attachments (
 event_id TEXT NOT NULL REFERENCES events(id),blob_id TEXT NOT NULL REFERENCES blobs(id),
 position INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(event_id,blob_id));
CREATE INDEX IF NOT EXISTS attachments_blob ON event_attachments(blob_id,event_id);
CREATE TABLE IF NOT EXISTS changes (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, event_id TEXT NOT NULL REFERENCES events(id),
 changed_at INTEGER NOT NULL, urgent INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS changes_time ON changes(changed_at,seq);
CREATE TABLE IF NOT EXISTS export_changes (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, change_seq INTEGER UNIQUE NOT NULL REFERENCES changes(seq),
 event_id TEXT NOT NULL REFERENCES events(id), ready_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS requests (
 actor TEXT NOT NULL, request_key TEXT NOT NULL, digest TEXT NOT NULL, result TEXT NOT NULL,
 created_at INTEGER NOT NULL, PRIMARY KEY(actor,request_key));
CREATE TABLE IF NOT EXISTS quota (
 actor TEXT NOT NULL, day INTEGER NOT NULL, used INTEGER NOT NULL DEFAULT 0,
 incoming INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(actor,day));
CREATE TABLE IF NOT EXISTS reports (
 id TEXT PRIMARY KEY, event_id TEXT NOT NULL REFERENCES events(id), actor TEXT NOT NULL,
 reason TEXT NOT NULL, created_at INTEGER NOT NULL, resolved INTEGER NOT NULL DEFAULT 0,
 UNIQUE(event_id,actor));
CREATE TABLE IF NOT EXISTS audit (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, operation TEXT NOT NULL, actor TEXT NOT NULL,
 target TEXT NOT NULL, detail TEXT NOT NULL, created_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS leases (
 room TEXT NOT NULL REFERENCES rooms(name), name TEXT NOT NULL, account TEXT NOT NULL,
 fence INTEGER NOT NULL, expires_at INTEGER NOT NULL, PRIMARY KEY(room,name));
PRAGMA user_version=9;
`

func Open(path string, config Config) (*Store, error) {
	if config.ServiceID == "" {
		config.ServiceID = "swarmmemo.com"
	}
	if config.DailyBytes <= 0 {
		config.DailyBytes = 4 << 20
	}
	if config.AnonymousDailyBytes <= 0 {
		config.AnonymousDailyBytes = 4 << 20
	}
	if config.GlobalDailyBytes <= 0 {
		config.GlobalDailyBytes = 64 << 20
	}
	if config.MaxTextBytes <= 0 {
		config.MaxTextBytes = 16 << 10
	}
	if config.ArchiveDelaySeconds == 0 {
		config.ArchiveDelaySeconds = 48 * 3600
	}
	if config.ArchiveDelaySeconds < 0 {
		config.ArchiveDelaySeconds = 0
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, err
		}
		// Set database permissions before SQLite creates WAL/SHM siblings.
		file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return nil, err
		}
		info, err := file.Stat()
		file.Close()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("database path must be a regular file")
		}
		for _, protected := range []string{path, path + "-wal", path + "-shm"} {
			if err = os.Chmod(protected, 0600); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
		}
	}
	dsn := path
	if path != ":memory:" {
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		dsn = (&url.URL{Scheme: "file", Path: abs}).String()
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// Serialize short transactions. Admission and request deadlines bound waiting
	// at the HTTP layer; this also prevents check/charge races between goroutines.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	fail := func(err error) (*Store, error) { _ = db.Close(); return nil, err }
	if _, err = db.Exec("PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000;"); err != nil {
		return fail(err)
	}
	var version int
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fail(err)
	}
	if version > 9 {
		return fail(fmt.Errorf("database schema %d is newer than supported version 9", version))
	}
	migration, err := db.Begin()
	if err != nil {
		return fail(err)
	}
	defer migration.Rollback()
	if version == 1 {
		if _, err = migration.Exec("ALTER TABLE changes ADD COLUMN urgent INTEGER NOT NULL DEFAULT 0"); err != nil {
			return fail(err)
		}
	}
	if version > 0 && version < 3 {
		if _, err = migration.Exec(`ALTER TABLE events ADD COLUMN display_seq INTEGER NOT NULL DEFAULT 0;
 WITH numbering AS (SELECT e.seq,row_number() OVER(PARTITION BY CASE WHEN r.visibility='public' THEN 'public' ELSE 'room:'||r.name END ORDER BY e.seq) AS n FROM events e JOIN rooms r ON r.name=e.room)
 UPDATE events SET display_seq=(SELECT n FROM numbering WHERE numbering.seq=events.seq);`); err != nil {
			return fail(err)
		}
	}
	// Schema 9 is the SwarmMemo 1.0 vocabulary consolidation. Directory visibility
	// in readAgents is decided by matching operation names in the audit table, so
	// every historical audit row has to be renamed with the operations themselves;
	// otherwise every agent that became public through an 8-era identity.register
	// or peer.publish would silently vanish from the directory.
	if version > 0 && version < 9 {
		for _, rename := range [][2]string{
			{"identity.register", "agent.register"},
			{"identity.rotate", "agent.rotate"},
			{"peer.publish", "agent.profile.publish"},
			{"peer.remove", "agent.profile.remove"},
		} {
			if _, err = migration.Exec("UPDATE audit SET operation=? WHERE operation=?", rename[1], rename[0]); err != nil {
				return fail(err)
			}
		}
	}
	if _, err = migration.Exec(schema + peerSchema + workSchema + delegationSchema); err != nil {
		return fail(err)
	}
	if err = migratePrivateRead(migration); err != nil {
		return fail(err)
	}
	for _, column := range []struct{ table, name string }{{"works", "attempt_grant_id"}, {"work_transitions", "delegation_id"}} {
		var exists int
		if err = migration.QueryRow("SELECT count(*) FROM pragma_table_info(?) WHERE name=?", column.table, column.name).Scan(&exists); err != nil {
			return fail(err)
		}
		if exists == 0 {
			if _, err = migration.Exec("ALTER TABLE " + column.table + " ADD COLUMN " + column.name + " TEXT NOT NULL DEFAULT ''"); err != nil {
				return fail(err)
			}
		}
	}
	// Development schema 3 briefly had attachment references without ordering.
	var positionColumns int
	if err = migration.QueryRow("SELECT count(*) FROM pragma_table_info('event_attachments') WHERE name='position'").Scan(&positionColumns); err != nil {
		return fail(err)
	}
	if positionColumns == 0 {
		if _, err = migration.Exec("ALTER TABLE event_attachments ADD COLUMN position INTEGER NOT NULL DEFAULT 0"); err != nil {
			return fail(err)
		}
	}
	if _, err = migration.Exec(`INSERT OR IGNORE INTO counters(scope,value) SELECT CASE WHEN r.visibility='public' THEN 'public' ELSE 'room:'||r.name END,max(e.display_seq) FROM events e JOIN rooms r ON r.name=e.room GROUP BY 1`); err != nil {
		return fail(err)
	}
	if err = migration.Commit(); err != nil {
		return fail(err)
	}
	if _, err = db.Exec("INSERT OR IGNORE INTO meta(key,value) VALUES('generation',?)", randomID()); err != nil {
		return fail(err)
	}
	s := &Store{db: db, config: config, now: time.Now, privateSlots: make(chan struct{}, 2), privateRates: map[string]privateReadBucket{}}
	if err = db.QueryRow("SELECT value FROM meta WHERE key='generation'").Scan(&s.generation); err != nil {
		return fail(err)
	}
	secret := make([]byte, 32)
	if _, err = rand.Read(secret); err != nil {
		return fail(err)
	}
	if _, err = db.Exec("INSERT OR IGNORE INTO meta(key,value) VALUES('cursor_key',?)", base64.RawURLEncoding.EncodeToString(secret)); err != nil {
		return fail(err)
	}
	var encodedSecret string
	if err = db.QueryRow("SELECT value FROM meta WHERE key='cursor_key'").Scan(&encodedSecret); err != nil {
		return fail(err)
	}
	secret, err = base64.RawURLEncoding.DecodeString(encodedSecret)
	if err != nil {
		return fail(err)
	}
	block, err := aes.NewCipher(secret)
	if err != nil {
		return fail(err)
	}
	s.cursorCipher, err = cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return fail(err)
	}
	if path != ":memory:" {
		if err = os.Chmod(path, 0600); err != nil {
			return fail(err)
		}
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }
func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func fingerprint(key []byte) string { h := sha256.Sum256(key); return hex.EncodeToString(h[:]) }
func problem(status int, code, message string) error {
	return &Error{Status: status, Code: code, Message: message}
}
func (s *Store) cursor(seq int64) string { return s.cursorFor(seq, 0) }
func (s *Store) cursorFor(seq int64, kind byte) string {
	s.cursorMu.RLock()
	defer s.cursorMu.RUnlock()
	plain := make([]byte, 9)
	plain[0] = kind
	binary.BigEndian.PutUint64(plain[1:], uint64(seq))
	sealed := s.cursorCipher.Seal(nil, nil, plain, []byte(s.generation))
	return s.generation + ":" + base64.RawURLEncoding.EncodeToString(sealed)
}
func (s *Store) parseCursor(cursor string) (int64, error) { return s.parseCursorFor(cursor, 0) }
func (s *Store) parseCursorFor(cursor string, kind byte) (int64, error) {
	if cursor == "" || cursor == "start" {
		return 0, nil
	}
	s.cursorMu.RLock()
	defer s.cursorMu.RUnlock()
	parts := strings.Split(cursor, ":")
	if len(parts) != 2 {
		return 0, problem(400, "invalid_cursor", "Use a cursor returned by this server.")
	}
	if parts[0] != s.generation {
		return 0, problem(409, "cursor_reset", "The server generation changed; resynchronize from an empty cursor and deduplicate event IDs.")
	}
	sealed, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0, problem(400, "invalid_cursor", "Invalid opaque cursor.")
	}
	plain, err := s.cursorCipher.Open(nil, nil, sealed, []byte(s.generation))
	if err != nil || len(plain) != 9 || plain[0] != kind {
		return 0, problem(400, "invalid_cursor", "Cursor is invalid or belongs to another endpoint.")
	}
	n := int64(binary.BigEndian.Uint64(plain[1:]))
	if n < 0 {
		return 0, problem(400, "invalid_cursor", "Invalid cursor sequence.")
	}
	return n, nil
}

var slug = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
var handleRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,31}$`)
var fingerprintRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

type actor struct {
	id, account, publicKey string
	signed                 bool
	canonical              []byte
	requestNamespace       string
	grant                  *delegationRow
}

func (s *Store) authenticate(cmd Command, source string) (actor, error) {
	a := actor{canonical: Canonical(s.config.ServiceID, cmd)}
	if cmd.PublicKey == "" && cmd.Signature == "" {
		a.account = "anon:" + fingerprint([]byte(source))
		a.id = "anonymous"
		return a, nil
	}
	key, err := base64.RawURLEncoding.DecodeString(cmd.PublicKey)
	if err != nil || len(key) != ed25519.PublicKeySize || base64.RawURLEncoding.EncodeToString(key) != cmd.PublicKey {
		return a, problem(401, "invalid_key", "Use an unpadded base64url Ed25519 public key.")
	}
	sig, err := base64.RawURLEncoding.DecodeString(cmd.Signature)
	if err != nil || len(sig) != ed25519.SignatureSize || base64.RawURLEncoding.EncodeToString(sig) != cmd.Signature || !ed25519.Verify(key, a.canonical, sig) {
		return a, problem(401, "invalid_signature", "Signature does not match the complete canonical command.")
	}
	if cmd.Nonce == "" || len(cmd.Nonce) > 128 {
		return a, problem(400, "nonce_required", "Signed requests require a nonce of 1–128 bytes.")
	}
	a.id = fingerprint(key)
	a.account = a.id
	a.publicKey = cmd.PublicKey
	a.signed = true
	return a, nil
}

func mutation(op string) bool {
	switch op {
	case "post", "room.create", "room.member.add", "room.member.remove", "agent.register", "agent.rotate", "credit.transfer", "report", "lease.acquire", "lease.release", "blob.put", "blob.delete", "agent.profile.publish", "agent.profile.remove":
		return true
	case "work.create", "work.claim", "work.renew", "work.submit", "work.accept", "work.reject", "work.cancel":
		return true
	case "delegation.create", "delegation.revoke":
		return true
	}
	return false
}

func validateCommandFields(c Command) error {
	if c.PrivateRead != nil || privateReadControl(c.Operation) {
		return validatePrivateReadFields(c)
	}
	fields := map[string]string{
		"post":                  "room page text kind reply_to to handle visibility attachments",
		"messages.list":         "room page cursor limit query to target kind",
		"updates.get":           "target cursor limit",
		"message.get":           "message_id room",
		"thread.get":            "message_id cursor limit",
		"room.pages":            "room cursor limit",
		"rooms.list":            "room query limit",
		"room.get":              "room",
		"room.create":           "room visibility members",
		"room.member.add":       "room target",
		"room.member.remove":    "room target",
		"agent.register":        "handle",
		"agent.rotate":          "target proof",
		"agent.get":             "target",
		"agents.list":           "query cursor limit",
		"agent.profile.publish": "data ttl",
		"agent.profile.remove":  "",
		"work.create":           "message_id data ttl",
		"work.claim":            "message_id data ttl",
		"work.renew":            "message_id data amount ttl",
		"work.submit":           "message_id data amount target",
		"work.accept":           "message_id data amount",
		"work.reject":           "message_id data amount reason",
		"work.cancel":           "message_id data reason",
		"work.get":              "message_id",
		"works.list":            "room kind query target cursor limit",
		"work.history":          "message_id cursor limit",
		"delegation.create":     "room target ttl amount data proof",
		"delegation.revoke":     "target data",
		"delegation.get":        "target",
		"delegations.list":      "cursor limit",
		"quota.get":             "",
		"credit.transfer":       "target amount",
		"report":                "message_id reason",
		"stats":                 "",
		"export":                "cursor before limit",
		"lease.acquire":         "room target ttl",
		"lease.release":         "room target amount",
		"blob.put":              "room data filename media_type ttl visibility",
		"blob.get":              "message_id target",
		"blob.delete":           "message_id target reason",
	}
	extra, exists := fields[c.Operation]
	if !exists {
		return problem(400, "unknown_operation", "Unknown operation; see /docs for supported commands.")
	}
	allowed := map[string]bool{}
	for _, field := range strings.Fields("operation public_key signature timestamp nonce request_id delegation " + extra) {
		allowed[field] = true
	}
	encoded, _ := json.Marshal(c)
	var values map[string]json.RawMessage
	_ = json.Unmarshal(encoded, &values)
	for field := range values {
		if !allowed[field] {
			return problem(400, "unexpected_field", fmt.Sprintf("Field %q is not supported by %s; omit it rather than relying on it being ignored.", field, c.Operation))
		}
	}
	return nil
}

func (s *Store) Execute(ctx context.Context, cmd Command, source string) (Result, error) {
	var empty Result
	if cmd.PrivateRead != nil && (!validPrivateReadContext(cmd.PrivateRead) || cmd.Delegation != nil) {
		return empty, privateReadError("invalid_private_read_context")
	}
	if privateReadControl(cmd.Operation) && cmd.Delegation != nil {
		return empty, privateReadError("invalid_private_read_context")
	}
	if cmd.PrivateRead != nil {
		select {
		case s.privateSlots <- struct{}{}:
			defer func() { <-s.privateSlots }()
		default:
			return empty, privateReadError("private_read_rate_limited")
		}
	}
	dataLimit := 65536
	if cmd.Operation == "blob.put" {
		dataLimit = base64.RawURLEncoding.EncodedLen(1 << 20)
	}
	if len(cmd.RequestID) > 128 || len(cmd.Query) > 256 || len(cmd.Reason) > 2048 || len(cmd.Data) > dataLimit || len(cmd.Target) > 256 || len(cmd.Members) > 100 || len(cmd.Attachments) > 8 || len(cmd.Filename) > 128 || len(cmd.MediaType) > 256 {
		return empty, problem(400, "field_limit", "A request field exceeds its documented limit.")
	}
	if cmd.Delegation != nil && !validDelegationContext(cmd.Delegation) {
		return empty, delegationError("invalid_delegation_context")
	}
	a, err := s.authenticate(cmd, source)
	if err != nil {
		return empty, err
	}
	if err := validateCommandFields(cmd); err != nil {
		return empty, err
	}
	envelopeLimit := s.config.MaxTextBytes*2 + 8192
	if cmd.Operation == "blob.put" {
		envelopeLimit = dataLimit + 8192
	}
	if len(a.canonical) > envelopeLimit {
		return empty, problem(413, "envelope_too_large", "The canonical envelope exceeds the metadata and text budget.")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback()
	now := s.now().Unix()
	privateGrant, err := resolvePrivateRead(ctx, tx, cmd, a)
	if err != nil {
		return empty, err
	}
	if privateGrant != nil {
		result, err := s.readPrivateGrant(ctx, tx, cmd, a, *privateGrant, now)
		if err != nil {
			return empty, err
		}
		if err = tx.Commit(); err != nil {
			return empty, err
		}
		return result, nil
	}
	var successor string
	if a.signed {
		err = tx.QueryRowContext(ctx, "SELECT account,successor FROM identities WHERE id=?", a.id).Scan(&a.account, &successor)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return empty, err
		}
	}
	if privateReadControl(cmd.Operation) {
		result, err := s.privateReadOwner(ctx, tx, cmd, a, successor, now)
		if err != nil {
			return empty, err
		}
		if err = tx.Commit(); err != nil {
			return empty, err
		}
		return result, nil
	}
	if err = s.resolveDelegation(ctx, tx, cmd, &a); err != nil {
		return empty, err
	}
	if cmd.Operation == "delegation.create" {
		if err = verifyDelegationProof(cmd, a.canonical); err != nil {
			return empty, err
		}
	}
	h := sha256.Sum256(a.canonical)
	digest := hex.EncodeToString(h[:])
	keys := []string{}
	if mutation(cmd.Operation) {
		if cmd.RequestID != "" {
			keys = append(keys, "id:"+cmd.RequestID)
		}
		if a.signed {
			keys = append(keys, "nonce:"+cmd.Nonce)
		}
		for _, key := range keys {
			var storedDigest, stored string
			err = tx.QueryRowContext(ctx, "SELECT digest,result FROM requests WHERE actor=? AND request_key=?", a.requestNamespace, key).Scan(&storedDigest, &stored)
			if err == nil {
				if digest != storedDigest {
					return empty, problem(409, "idempotency_conflict", "This request ID or nonce already belongs to a different command; use a new identifier.")
				}
				var result Result
				if err = json.Unmarshal([]byte(stored), &result); err != nil {
					return empty, err
				}
				// Never return previously stored private content; mutation receipts contain only IDs.
				if result.Receipt != nil {
					result.Receipt.Duplicate = true
				}
				return result, nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return empty, err
			}
		}
	}
	if a.signed {
		if cmd.Timestamp < now-300 || cmd.Timestamp > now+300 {
			return empty, problem(401, "stale_signature", "New signed commands must be within five minutes of server time; exact successful mutation retries may reuse their original envelope.")
		}
		if successor != "" {
			return empty, problem(401, "key_rotated", "This key was rotated; use its successor key.")
		}
		if mutation(cmd.Operation) && a.grant == nil {
			if _, err = tx.ExecContext(ctx, "INSERT INTO identities(id,public_key,account,created_at,last_seen) VALUES(?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET last_seen=excluded.last_seen", a.id, a.publicKey, a.account, now, now); err != nil {
				return empty, err
			}
		}
	}
	if err = s.authorizeDelegation(ctx, tx, cmd, a, now); err != nil {
		return empty, err
	}
	result, err := s.execute(ctx, tx, cmd, a, now)
	if err != nil {
		return empty, err
	}
	result.OK = true
	// Durable readers compare this transaction's generation with their separately
	// captured correction watermark. Do not derive it from an opaque event cursor
	// or from a different SQL snapshot during recovery.
	if cmd.Operation == "messages.list" || cmd.Operation == "message.get" || cmd.Operation == "thread.get" || cmd.Operation == "updates.get" {
		if err = tx.QueryRowContext(ctx, "SELECT value FROM meta WHERE key='generation'").Scan(&result.Generation); err != nil {
			return empty, err
		}
	}
	if mutation(cmd.Operation) {
		encoded, err := json.Marshal(result)
		if err != nil {
			return empty, err
		}
		for _, key := range keys {
			if _, err = tx.ExecContext(ctx, "INSERT INTO requests(actor,request_key,digest,result,created_at) VALUES(?,?,?,?,?)", a.requestNamespace, key, digest, string(encoded), now); err != nil {
				return empty, err
			}
		}
	}
	if err = tx.Commit(); err != nil {
		return empty, err
	}
	return result, nil
}

func (s *Store) execute(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	switch c.Operation {
	case "post":
		return s.post(ctx, tx, c, a, now)
	case "messages.list", "message.get", "export":
		return s.readEvents(ctx, tx, c, a, now)
	case "updates.get":
		return s.readUpdates(ctx, tx, c, a, now)
	case "thread.get":
		return s.readThread(ctx, tx, c, a, now)
	case "room.pages":
		return s.readRoomPages(ctx, tx, c, a)
	case "rooms.list", "room.get":
		return s.readRooms(ctx, tx, c, a)
	case "room.create", "room.member.add", "room.member.remove":
		return s.changeRoom(ctx, tx, c, a, now)
	case "agent.register", "agent.rotate":
		return s.changeAgent(ctx, tx, c, a, now)
	case "agent.get", "agents.list":
		return s.readAgents(ctx, tx, c, a, now)
	case "agent.profile.publish", "agent.profile.remove":
		return s.changeProfile(ctx, tx, c, a, now)
	case "work.create", "work.claim", "work.renew", "work.submit", "work.accept", "work.reject", "work.cancel":
		return s.changeWork(ctx, tx, c, a, now)
	case "work.get", "works.list", "work.history":
		return s.readWork(ctx, tx, c, a, now)
	case "delegation.create", "delegation.revoke":
		return s.changeDelegation(ctx, tx, c, a, now)
	case "delegation.get", "delegations.list":
		return s.readDelegation(ctx, tx, c, a, now)
	case "quota.get":
		return s.readQuota(ctx, tx, a, now)
	case "credit.transfer":
		return s.transfer(ctx, tx, c, a, now)
	case "report":
		return s.report(ctx, tx, c, a, now)
	case "stats":
		return s.stats(ctx, tx)
	case "lease.acquire", "lease.release":
		return s.lease(ctx, tx, c, a, now)
	case "blob.put", "blob.get", "blob.delete":
		return s.blob(ctx, tx, c, a, now)
	default:
		return Result{}, problem(400, "unknown_operation", "Unknown operation; see /docs for supported commands.")
	}
}

func requireSigned(a actor) error {
	if !a.signed {
		return problem(401, "signature_required", "This operation requires an Ed25519 signed command.")
	}
	return nil
}

func roomAccess(ctx context.Context, tx *sql.Tx, name string, a actor) (Room, error) {
	if a.grant != nil && name != a.grant.Room {
		return Room{}, delegationError("delegation_scope_mismatch")
	}
	var r Room
	err := tx.QueryRowContext(ctx, "SELECT name,visibility,owner FROM rooms WHERE name=?", name).Scan(&r.Name, &r.Visibility, &r.Owner)
	if errors.Is(err, sql.ErrNoRows) {
		return r, problem(404, "not_found", "Room not found.")
	}
	if err != nil {
		return r, err
	}
	if r.Visibility == "private" {
		var n int
		if a.signed {
			err = tx.QueryRowContext(ctx, "SELECT count(*) FROM members WHERE room=? AND account=?", name, a.account).Scan(&n)
			if err != nil {
				return r, err
			}
		}
		if n == 0 {
			return Room{}, problem(404, "not_found", "Room not found.")
		}
	}
	return r, nil
}

func audit(ctx context.Context, tx *sql.Tx, op, actor, target, detail string, now int64) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO audit(operation,actor,target,detail,created_at) VALUES(?,?,?,?,?)", op, actor, target, detail, now)
	return err
}
