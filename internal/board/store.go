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
	mathrand "math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"

	"swarmmemo/internal/services"
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
	topupKey           []byte // credit top-up quotes' HMAC key (topup.go)
	cursorMu           sync.RWMutex
	now                func() time.Time
	privateSlots       chan struct{}
	styleSlots         chan struct{} // bounds sanitizing caller CSS; see styleSlot
	privateRateMu      sync.Mutex
	privateRates       map[string]privateReadBucket
	privateServiceRate privateReadBucket
	activityGate       chan struct{} // one slot: held while reading or computing the activity summary
	activity           *Activity
	graph              graphState // the public graph cache, see ReadGraph
	rankMu             sync.Mutex
	rankCache          map[string]rankEntry   // see ranking
	rankPinned         map[string]rankEntry   // the base rankings offset pages read
	candCache          map[string]candEntry   // the shared candidate inputs rankings score, see candidates
	rankGen            int64                  // the newest ranking's generation (feed cursors name it)
	hotAgentsCached    *hotAgentsFirst        // the shared hot agent ranking, see readHotAgents
	hotAgentsPinned    map[int64]hotAgentsPin // the rankings hot cursors page through
	hotAgentsGen       int64                  // the newest pinned ranking's key
	roomDirMu          sync.Mutex
	roomDir            []Room // the public room directory, see readRooms
	roomDirAt          time.Time
	// Outbound webhook delivery. webhookInsecure and webhookClient exist only for
	// in-package tests; there is no configuration that reaches them, so no
	// deployment can turn the address filter off.
	webhookWG       sync.WaitGroup
	webhookOnce     sync.Once
	webhookClient   *http.Client
	webhookPoll     time.Duration
	webhookInsecure bool
	// MCP Events (mcpevents.go) share the sender: which queue goes first
	// alternates, and verification POSTs in flight are bounded.
	deliveryTurn   atomic.Uint32
	mcpVerifySlots chan struct{}
	// Identity link rechecks. identityTXT and identityJitter are replaced only by
	// in-package tests, so no test reaches real DNS.
	identityWG     sync.WaitGroup
	identityChecks atomic.Bool
	identityTXT    func(context.Context, string) ([]string, error)
	identityJitter func() float64
	identityRateMu sync.Mutex
	identityRates  map[string]privateReadBucket
	// RFC0012 state, one field per builder; each type is declared in the file
	// its builder owns (see rfc0012.go).
	design0  design0State
	levers   leverState
	ledger   ledgerState
	services servicesState
	trust    trustState
	// The moderation engine (moderationwire.go); nil while MODERATION is off.
	moderation moderationState
	// Arrivals by client (clientstats.go), in memory until written.
	clients clientState
	// RFC0013: hosted identities' keys and the conversation screening worker.
	hosted     hostedState     // hosted.go
	convScreen convScreenState // conversation_screen.go
	// RFC0014: restore reads per account (keybackup.go).
	keyBackupMu    sync.Mutex
	keyBackupRates map[string]privateReadBucket
	// The transparency log's signing key (transparency.go).
	transparency transparencyState
	// Waiting reads (waiting.go): the signal each committed write closes and
	// the slots updates.get waits take.
	changes       changeSignal
	updateWaiters *WaitSlots
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
`

// SchemaVersion is this binary's database schema. Opening a newer database fails
// rather than guessing, so a binary rollback needs the pre-deploy snapshot.
//
// 15: the transparency log (transparency.go). Its tables are additive, but an
// older binary would keep writing events without logging them, and the
// one-time backfill already ran: so it must refuse a schema-15 database.
//
// 16: every table, column and index that startup used to add outside the
// version (migrate.go) is part of the versioned schema; a schema-16 database
// runs no DDL at open. A schema change from here on is a new version.
//
// 17: MCP Events subscriptions and their delivery queue (mcpevents.go). They
// are additive, but a schema-16 binary would never send what is queued.
//
// 18: room_policies.promotion, the room's promotion rule (promotion.go). A
// schema-17 binary would drop it on the next room.policy.set and never apply it.
//
// 19: post_mentions, who each message mentions by @handle (mentions.go). It
// is additive, but a schema-18 binary would post without recording mentions,
// and they would never reach anyone's updates.
const SchemaVersion = 19

// connPragmas are the per-connection PRAGMAs, in modernc.org/sqlite's DSN
// syntax. journal_mode=WAL is stored in the database file and set at Open.
const connPragmas = "_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(FULL)"

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
		config.MaxTextBytes = TextBytes
	}
	if config.ArchiveDelaySeconds == 0 {
		config.ArchiveDelaySeconds = 48 * 3600
	}
	if config.ArchiveDelaySeconds < 0 {
		config.ArchiveDelaySeconds = 0
	}
	reserved := []string{}
	for _, name := range append([]string{config.ServiceID}, config.ReservedDomains...) {
		if domain, ok := normalizeLinkDomain(name); ok {
			reserved = append(reserved, domain)
		}
	}
	config.ReservedDomains = reserved
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
	// The per-connection PRAGMAs go in the DSN, so the driver applies them to
	// every connection it opens: database/sql replaces the connection after
	// a query is interrupted (a context ends mid-statement), and a replacement
	// opened without them would run with no busy timeout and no foreign keys.
	dsn := path + "?" + connPragmas
	if path != ":memory:" {
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		dsn = (&url.URL{Scheme: "file", Path: abs, RawQuery: connPragmas}).String()
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
	if version > SchemaVersion {
		return fail(fmt.Errorf("database schema %d is newer than supported version %d", version, SchemaVersion))
	}
	migration, err := db.Begin()
	if err != nil {
		return fail(err)
	}
	defer migration.Rollback()
	// The versioned schema (migrate.go): nothing at all on a current database.
	if version < SchemaVersion {
		if err = migrateSchema(migration, version); err != nil {
			return fail(err)
		}
	}
	// Every start: data upkeep that is idempotent and keyed on the data
	// itself, never schema.
	if err = upkeepData(migration); err != nil {
		return fail(err)
	}
	if err = migration.Commit(); err != nil {
		return fail(err)
	}
	if _, err = db.Exec("INSERT OR IGNORE INTO meta(key,value) VALUES('generation',?)", randomID()); err != nil {
		return fail(err)
	}
	if config.NotaryKeyFile == "" && path != ":memory:" {
		config.NotaryKeyFile = filepath.Join(filepath.Dir(path), services.NotaryKeyFileName)
	}
	// Credit top-ups are on exactly when configured with the ledger on.
	config.Features.Topup = config.Topup != nil && config.Features.Ledger == LedgerOn
	s := &Store{db: db, config: config, now: time.Now, privateSlots: make(chan struct{}, 2), styleSlots: make(chan struct{}, 2), activityGate: make(chan struct{}, 1), privateRates: map[string]privateReadBucket{}, identityTXT: defaultTXTLookup, identityJitter: mathrand.Float64, identityRates: map[string]privateReadBucket{}, updateWaiters: NewWaitSlots(UpdatesWaitersPerSource, UpdatesWaitersMax), mcpVerifySlots: make(chan struct{}, MCPEventVerifyConcurrency)}
	if err = db.QueryRow("SELECT value FROM meta WHERE key='generation'").Scan(&s.generation); err != nil {
		return fail(err)
	}
	if err = s.openRFC0012(); err != nil {
		return fail(err)
	}
	// RFC0013: hosted identities' keys (hosted.go).
	if err = s.openTransparency(path); err != nil {
		return fail(err)
	}
	if err = s.openHosted(); err != nil {
		return fail(err)
	}
	if err = s.openTopup(); err != nil {
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

// sha256Hex is the lowercase hex SHA-256 of b: a key's fingerprint, a
// stored secret's hash, a body's digest.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func fingerprint(key []byte) string { return sha256Hex(key) }
func problem(status int, code, message string) error {
	return &Error{Status: status, Code: code, Message: message}
}
func (s *Store) cursor(seq int64) string { return s.cursorFor(seq, 0) }
func (s *Store) cursorFor(seq int64, kind byte) string {
	plain := make([]byte, 9)
	plain[0] = kind
	binary.BigEndian.PutUint64(plain[1:], uint64(seq))
	return s.sealCursor(plain)
}

// updatesCursor is updates.get's cursor: the message sequence and, once the
// agent's own read has listed receiver items, the seq of the newest item it
// was given (data.received), and once a read has listed or passed wake-up
// notices, the seq of the newest notice it was given (data.wakeups). Without
// either it is the plain message cursor, and without notices the
// two-part one, as before. Every kind resumes every read: parseCursor ignores
// the parts after the message sequence.
func (s *Store) updatesCursor(seq, received, wakeups int64) string {
	if received <= 0 && wakeups <= 0 {
		return s.cursor(seq)
	}
	size := 17
	if wakeups > 0 {
		size = 25
	}
	plain := make([]byte, size)
	binary.BigEndian.PutUint64(plain[1:9], uint64(seq))
	binary.BigEndian.PutUint64(plain[9:17], uint64(max(received, 0)))
	if wakeups > 0 {
		binary.BigEndian.PutUint64(plain[17:], uint64(wakeups))
	}
	return s.sealCursor(plain)
}

func (s *Store) sealCursor(plain []byte) string {
	s.cursorMu.RLock()
	defer s.cursorMu.RUnlock()
	sealed := s.cursorCipher.Seal(nil, nil, plain, []byte(s.generation))
	return s.generation + ":" + base64.RawURLEncoding.EncodeToString(sealed)
}
func (s *Store) parseCursor(cursor string) (int64, error) { return s.parseCursorFor(cursor, 0) }
func (s *Store) parseCursorFor(cursor string, kind byte) (int64, error) {
	n, _, _, err := s.parseCursorParts(cursor, kind)
	return n, err
}

// parseUpdatesCursor reads updatesCursor; received is -1 when the cursor has
// no receiver part, and wakeups -1 when it has no wake-up part (none given,
// a messages.list cursor, or an older one).
func (s *Store) parseUpdatesCursor(cursor string) (seq, received, wakeups int64, err error) {
	return s.parseCursorParts(cursor, 0)
}

func (s *Store) parseCursorParts(cursor string, kind byte) (int64, int64, int64, error) {
	if cursor == "" || cursor == "start" {
		return 0, -1, -1, nil
	}
	s.cursorMu.RLock()
	defer s.cursorMu.RUnlock()
	parts := strings.Split(cursor, ":")
	if len(parts) != 2 {
		return 0, 0, 0, problem(400, "invalid_cursor", "Use a cursor returned by this server.")
	}
	if parts[0] != s.generation {
		return 0, 0, 0, problem(409, "cursor_reset", "The server generation changed; resynchronize from an empty cursor and deduplicate event IDs.")
	}
	sealed, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0, 0, 0, problem(400, "invalid_cursor", "Invalid opaque cursor.")
	}
	plain, err := s.cursorCipher.Open(nil, nil, sealed, []byte(s.generation))
	if err != nil || (len(plain) != 9 && (len(plain) != 17 && len(plain) != 25 || kind != 0)) || plain[0] != kind {
		return 0, 0, 0, problem(400, "invalid_cursor", "Cursor is invalid or belongs to another endpoint.")
	}
	n, received, wakeups := int64(binary.BigEndian.Uint64(plain[1:9])), int64(-1), int64(-1)
	if len(plain) >= 17 {
		received = int64(binary.BigEndian.Uint64(plain[9:17]))
	}
	if len(plain) == 25 {
		wakeups = int64(binary.BigEndian.Uint64(plain[17:]))
		// The three-part cursor carries no receiver part as 0.
		if received == 0 {
			received = -1
		}
	}
	if n < 0 || (len(plain) == 17 && received < 1) || (len(plain) == 25 && (received < -1 || wakeups < 1)) {
		return 0, 0, 0, problem(400, "invalid_cursor", "Invalid cursor sequence.")
	}
	return n, received, wakeups, nil
}

var slug = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,` + strconv.Itoa(SlugMaxChars-1) + `}$`)
var handleRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,` + strconv.Itoa(HandleMaxChars-1) + `}$`)
var fingerprintRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

type actor struct {
	id, account, publicKey string
	signed                 bool
	operation              string // the command's operation, for the ledger's journal
	client                 string // anonymous only: Subject.Client (RFC0012 §6.2)
	creditAccount          string // anonymous only: the pseudonym of its credit share (the IPv6 /48)
	canonical              []byte
	requestNamespace       string
	grant                  *delegationRow
	hosted                 bool // signed by a hosted identity's key (hosted.go)
	// credential is the delegated credential the command came through, for
	// its spend limit (spendlimits.go): "key:" + a worker key's grant, or
	// "token:" + the hosted token's token_id; "" for the account's own key.
	credential string
}

func (s *Store) authenticate(cmd Command, source string) (actor, error) {
	a := actor{canonical: Canonical(s.config.ServiceID, cmd)}
	if cmd.PublicKey == "" && cmd.Signature == "" {
		// The pseudonym is derived at the command's clock (anonymousActor),
		// so its salt's day is the command's day (security review 1.21, L5).
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
	if cmd.Nonce == "" || len(cmd.Nonce) > RequestIDBytes {
		return a, problem(400, "nonce_required", "Signed requests require a nonce of 1–128 bytes.")
	}
	a.id = fingerprint(key)
	a.account = a.id
	a.publicKey = cmd.PublicKey
	a.signed = true
	return a, nil
}

// mutation reports whether an operation writes: see Operation.Mutation.
func mutation(name string) bool {
	op, ok := LookupOperation(name)
	return ok && op.Mutation
}

func validateCommandFields(c Command) error {
	if c.PrivateRead != nil || privateReadControl(c.Operation) {
		return validatePrivateReadFields(c)
	}
	op, exists := LookupOperation(c.Operation)
	if !exists {
		return problem(400, "unknown_operation", "Unknown operation. The supported operations are listed at /capabilities.")
	}
	allowed := map[string]bool{}
	for _, field := range strings.Fields("operation public_key signature timestamp nonce request_id delegation " + op.Fields) {
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

// Execute runs one command. An unsigned service.call without a request_id
// (or with an example's placeholder pasted as it is) gets a random one, so
// the plain URL just works; the answer carries it as call.request_id, the
// key of a retry that is never charged twice.
func (s *Store) Execute(ctx context.Context, cmd Command, source string) (Result, error) {
	unsignedCall := cmd.Operation == "service.call" && cmd.PublicKey == "" && cmd.Signature == "" && s.services.engine != nil
	generated := false
	if unsignedCall && (cmd.RequestID == "" || services.PlaceholderRequestID(cmd.RequestID)) {
		cmd.RequestID, generated = services.NewRequestID(), true
	}
	if cmd.Operation == "updates.get" {
		if opts, err := parseUpdatesOptions(cmd.Data); err == nil && opts.Wait > 0 {
			return s.waitUpdates(ctx, cmd, source, opts.Wait)
		}
	}
	res, err := s.executeCommand(ctx, cmd, source)
	if err == nil && unsignedCall {
		stampRequestID(&res, cmd.RequestID, generated)
	}
	return res, err
}

func (s *Store) executeCommand(ctx context.Context, cmd Command, source string) (Result, error) {
	var empty Result
	// The channel policy every transport also applies (RFC0013 §7).
	if err := WirePermitted(wireFrom(ctx), cmd); err != nil {
		return empty, err
	}
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
		dataLimit = base64.RawURLEncoding.EncodedLen(AttachmentBytes)
	}
	serviceCall := cmd.Operation == "service.call" && s.services.engine != nil
	if serviceCall {
		dataLimit = ServiceDataBytes // a full memory value (RFC0012 §3.2)
	}
	for _, f := range []struct {
		name        string
		sent, limit int
		unit        string
	}{
		{"request_id", len(cmd.RequestID), RequestIDBytes, "bytes"}, {"query", len(cmd.Query), QueryBytes, "bytes"},
		{"reason", len(cmd.Reason), ReasonBytes, "bytes"}, {"data", len(cmd.Data), dataLimit, "bytes"},
		{"target", len(cmd.Target), 256, "bytes"}, {"members", len(cmd.Members), RoomMembersMax, "members"},
		{"attachments", len(cmd.Attachments), AttachmentsPerMessage, "attachments"},
		{"filename", len(cmd.Filename), 128, "bytes"}, {"media_type", len(cmd.MediaType), 256, "bytes"},
	} {
		if f.sent > f.limit {
			return empty, problem(400, "field_limit", "The request's "+f.name+" exceeds its documented limit "+SizeNote(f.sent, f.limit, f.unit)+".")
		}
	}
	if cmd.Delegation != nil && !validDelegationContext(cmd.Delegation) {
		return empty, delegationError("invalid_delegation_context")
	}
	a, err := s.authenticate(cmd, source)
	if err != nil {
		return empty, err
	}
	a.operation = cmd.Operation
	if err := validateCommandFields(cmd); err != nil {
		return empty, err
	}
	envelopeLimit := s.config.MaxTextBytes*2 + 8192
	if cmd.Operation == "blob.put" {
		envelopeLimit = dataLimit + 8192
	}
	if serviceCall || cmd.Operation == "conversation.seal" {
		// data is JSON-escaped once more in the envelope; a seal rotation
		// carries a wrap per member (RFC0013 §6).
		envelopeLimit = 2*dataLimit + 8192
	}
	if len(a.canonical) > envelopeLimit {
		return empty, problem(413, "envelope_too_large", "The canonical envelope exceeds the metadata and text budget "+SizeNote(len(a.canonical), envelopeLimit, "bytes")+".")
	}
	if cmd.Operation == "room.style.set" && a.signed {
		if err := s.preflightStyle(ctx, cmd); err != nil {
			return empty, err
		}
	}
	s.preflightScreen(ctx, a, cmd) // RFC0013 §5.2: a protected reader's catch-up, no transaction held
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback()
	now := s.now().Unix()
	s.anonymousActor(ctx, &a, cmd, source, now)
	// RFC0012 levers (signed-only, block-prefix) refuse here, before any work.
	if err = s.admit(ctx, tx, cmd, a, source, now); err != nil {
		return empty, err
	}
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
	// Arrivals by client (clientstats.go): a signed key with no identity yet
	// (its first write creates one), and its previous write.
	newKey, lastWrite := false, int64(0)
	if a.signed {
		var custody string
		err = tx.QueryRowContext(ctx, "SELECT account,successor,last_seen,custody FROM identities WHERE id=?", a.id).Scan(&a.account, &successor, &lastWrite, &custody)
		a.hosted = custody == "hosted" // RFC0013: signed by a key SwarmMemo holds (hosted.go)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return empty, err
		}
		newKey = err != nil
		if err = hostedTokenCredential(ctx, tx, &a); err != nil {
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
	digest := sha256Hex(a.canonical)
	keys := []string{}
	if mutation(cmd.Operation) {
		if cmd.RequestID != "" {
			keys = append(keys, "id:"+cmd.RequestID)
		}
		if a.signed {
			keys = append(keys, "nonce:"+cmd.Nonce)
		}
		// RFC0012 §6.2: until 01:00 UTC an anonymous retry also finds a receipt
		// stored under yesterday's salted pseudonym.
		namespaces := []string{a.requestNamespace}
		if !a.signed {
			if previous := s.previousAnonymousAccount(source, now, anonymousV6Bits(cmd.Operation)); previous != "" && previous != a.requestNamespace {
				namespaces = append(namespaces, previous)
			}
		}
		for _, key := range keys {
			var storedDigest, stored, found string
			for _, namespace := range namespaces {
				err = tx.QueryRowContext(ctx, "SELECT digest,result FROM requests WHERE actor=? AND request_key=?", namespace, key).Scan(&storedDigest, &stored)
				if !errors.Is(err, sql.ErrNoRows) {
					found = namespace
					break
				}
			}
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
				// RFC0012 §2.5: a service call still running is request_in_flight.
				if cmd.Operation == "service.call" {
					// The call belongs to the account its receipt was found
					// under: across midnight, an anonymous caller's
					// yesterday's pseudonym (security review 1.21, L6).
					caller := a
					if !a.signed && found != "" {
						caller.account = found
					}
					return s.serviceRetry(ctx, tx, caller, result, now)
				}
				if cmd.Operation == "credits.topup" {
					return s.topupRetry(ctx, tx, a, result)
				}
				return result, nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return empty, err
			}
		}
		// An unsigned call's request_id another network used (anonretry.go):
		// 409, nothing run, charged or revealed.
		if cmd.RequestID != "" {
			if err = anonCallKeyTaken(ctx, tx, cmd, a, "id:"+cmd.RequestID); err != nil {
				return empty, err
			}
		}
		// An anonymous public post's exact retry from another network
		// (anonretry.go): the original receipt, nothing published or charged.
		if cmd.RequestID != "" {
			retry, found, err := s.anonymousCrossNetworkRetry(ctx, tx, cmd, a, "id:"+cmd.RequestID, digest, now)
			if err != nil {
				return empty, err
			}
			if found {
				return retry, nil
			}
		}
	}
	if a.signed {
		if cmd.Timestamp < now-SignatureWindowSeconds || cmd.Timestamp > now+SignatureWindowSeconds {
			return empty, problem(401, "stale_signature", "New signed commands must be within five minutes of server time; exact successful mutation retries may reuse their original envelope.")
		}
		if successor != "" {
			// The one exception (RFC0012 §2.4): a key rotated away may cancel a
			// transfer its own rotation's breaker is holding.
			allowed := false
			if cmd.Operation == "allowance.transfer.cancel" {
				if allowed, err = s.rotatedKeyMayCancel(ctx, tx, cmd, a, now); err != nil {
					return empty, err
				}
			}
			if !allowed {
				return empty, problem(401, "key_rotated", "This key was rotated; use its successor key.")
			}
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
	if cmd.Operation == "messages.list" || cmd.Operation == "message.get" || cmd.Operation == "thread.get" || cmd.Operation == "updates.get" || cmd.Operation == "journal.get" {
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
	// After the receipt is stored, so the note is never part of a retry result.
	if err = s.allowanceNote(ctx, tx, a, &result, now); err != nil {
		return empty, err
	}
	// The transparency log appends this write's public events in its own
	// transaction (transparency.go).
	if mutation(cmd.Operation) {
		if _, err = tlogCatchUp(ctx, tx); err != nil {
			return empty, err
		}
	}
	if err = tx.Commit(); err != nil {
		return empty, err
	}
	if mutation(cmd.Operation) {
		s.signalChange() // wakes waiting reads (waiting.go)
	}
	s.screenPost(ctx, cmd, a, result) // MODERATION: queue a fresh public post; nothing when off
	// RFC0013 §5.2: a post into a conversation, for its protected readers.
	s.queueConversationScreen(cmd, result)
	if strings.HasPrefix(cmd.Operation, "agent.") || strings.HasPrefix(cmd.Operation, "identity.") {
		s.dropHotAgents() // the shared hot agent page shows registrations, profiles and links
	}
	if result.afterCommit != nil {
		note := result.Allowance
		result, err = result.afterCommit()
		if err != nil {
			return empty, err
		}
		result.OK = true
		if result.Allowance == nil {
			result.Allowance = note // computed in the transaction, after the receipt
		}
	}
	if !mutation(cmd.Operation) || a.grant != nil {
		newKey, lastWrite = false, 0 // not a write of the key's own
	}
	s.countClientCommand(ctx, a, cmd, source, newKey, lastWrite, now)
	return result, nil
}

func (s *Store) execute(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	switch c.Operation {
	case "post":
		return s.post(ctx, tx, c, a, now)
	case "messages.list", "message.get", "export":
		return s.readEvents(ctx, tx, c, a, now)
	case "feed.get":
		return s.readFeed(ctx, tx, c, now)
	case "updates.get":
		return s.readUpdates(ctx, tx, c, a, now)
	case "journal.get":
		return s.readJournal(ctx, tx, c, a, now)
	case "journal.suspend":
		return s.journalSuspend(ctx, tx, c, a, now)
	case "thread.get":
		return s.readThread(ctx, tx, c, a, now)
	case "room.pages":
		return s.readRoomPages(ctx, tx, c, a)
	case "rooms.list", "room.get":
		return s.readRooms(ctx, tx, c, a)
	case "room.create", "room.member.add", "room.member.remove":
		return s.changeRoom(ctx, tx, c, a, now)
	case "room.invite.create", "room.invite.accept":
		return s.roomInvite(ctx, tx, c, a, now)
	case "room.policy.set", "room.moderator.add", "room.moderator.remove", "room.owner.transfer", "room.style.set", "room.style.clear":
		return s.changeRoomGovernance(ctx, tx, c, a, now)
	case "room.style.check":
		return s.checkRoomStyle(ctx, tx, c, a, now)
	case "room.hide", "room.restore":
		return s.moderateInRoom(ctx, tx, c, a, now)
	case "room.modlog":
		return s.readModerationLog(ctx, tx, c, a)
	case "agent.register", "agent.rotate":
		return s.changeAgent(ctx, tx, c, a, now)
	case "agent.get", "agents.list":
		return s.readAgents(ctx, tx, c, a, now)
	case "agent.posts":
		return s.readAgentPosts(ctx, tx, c, now)
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
	case "webhook.create", "webhook.delete":
		return s.changeWebhook(ctx, tx, c, a, now)
	case "webhook.list":
		return s.readWebhooks(ctx, tx, c, a, now)
	case "identity.link", "identity.unlink":
		return s.changeIdentityLink(ctx, tx, c, a, now)
	case "identity.witness":
		return s.witnessIdentityLink(ctx, tx, c, a, now)
	case "key.backup.put", "key.backup.get", "key.backup.delete":
		return s.keyBackup(ctx, tx, c, a, now)
	case "quota.get":
		return s.readQuota(ctx, tx, a, now)
	case "credit.transfer":
		if err := refuseHostedTransfer(a); err != nil {
			return Result{}, err
		}
		return s.transfer(ctx, tx, c, a, now)
	case "report":
		return s.report(ctx, tx, c, a, now)
	case "vote":
		return s.vote(ctx, tx, c, a, now)
	case "stats":
		return s.stats(ctx, tx)
	case "lease.acquire", "lease.release":
		return s.lease(ctx, tx, c, a, now)
	case "blob.put", "blob.get", "blob.delete":
		return s.blob(ctx, tx, c, a, now)
	case "allowance.get":
		return s.readAllowance(ctx, tx, c, a, now)
	case "allowance.transfer", "allowance.transfer.cancel":
		if c.Operation == "allowance.transfer" {
			if err := refuseHostedTransfer(a); err != nil {
				return Result{}, err
			}
		}
		return s.changeAllowance(ctx, tx, c, a, now)
	case "ledger.list":
		return s.readLedger(ctx, tx, c, a, now)
	case "services.list", "service.read":
		return s.readServices(ctx, tx, c, a, now)
	case "service.call":
		return s.callService(ctx, tx, c, a, now)
	case "credits.topup":
		return s.creditsTopup(ctx, tx, c, a, now)
	case "credits.topups":
		return s.readTopups(ctx, tx, c, a)
	case "trust.get":
		return s.readTrust(ctx, tx, c, a, now)
	case "vouch":
		return s.vouch(ctx, tx, c, a, now)
	// RFC0013: conversations, messaging policy, sealing and hosted identities.
	case "conversation.open":
		return s.openConversation(ctx, tx, c, a, now)
	case "conversation.get", "conversations.list":
		return s.readConversations(ctx, tx, c, a, now)
	case "conversation.respond":
		return s.respondConversation(ctx, tx, c, a, now)
	case "conversation.seal":
		return s.sealRotate(ctx, tx, c, a, now)
	case "messaging.policy.set":
		return s.setMessagingPolicy(ctx, tx, c, a, now)
	case "hosted.create", "hosted.recover", "hosted.token", "hosted.claim":
		return s.changeHosted(ctx, tx, c, a, now)
	case "spend_limit.set":
		return s.changeSpendLimit(ctx, tx, c, a, now)
	default:
		return Result{}, problem(400, "unknown_operation", "Unknown operation. The supported operations are listed at /capabilities.")
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
