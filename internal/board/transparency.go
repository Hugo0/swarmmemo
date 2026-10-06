package board

// The transparency log (ROADMAP §4.12 item 2): an append-only RFC 6962 Merkle
// tree over the public record, signed tree heads (C2SP checkpoints) and
// OpenTimestamps anchors. Anyone can check that a public message, a
// moderation action or a key event is in the log, and that the log only ever
// grew.
//
// Leaves come from the tables that already hold the public record, read as an
// outbox: events in public rooms (messages, edits as superseding versions),
// room_moderation_log in public rooms (hides, restores, room governance),
// audit rows of the identity operations of public accounts and of operator
// allowance grants, and tier_grant_log. tlog_cursors keeps, per source, the
// last row turned into a leaf. tlogCatchUp appends every newer row, merged by
// (created_at, source rank, seq), inside the caller's transaction: every
// write path calls it before committing, and the background job catches up
// whatever a path without the call wrote. The one-time backfill is the same
// function from cursor zero, so history and new events get identical leaves.
// Text is never in a leaf: a message is its id, sequence, room, author, the
// SHA-256 of its text and its signature. Moderation appends a leaf; nothing
// in the log is ever rewritten (triggers refuse UPDATE and DELETE).

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"swarmmemo/internal/ots"
	"swarmmemo/internal/tlog"
)

const tlogSchema = `
CREATE TABLE IF NOT EXISTS tlog_leaves (
 idx INTEGER PRIMARY KEY, kind TEXT NOT NULL, subject TEXT NOT NULL DEFAULT '', ref TEXT NOT NULL DEFAULT '',
 data TEXT NOT NULL, created_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS tlog_leaves_ref ON tlog_leaves(ref) WHERE ref<>'';
CREATE INDEX IF NOT EXISTS tlog_leaves_subject ON tlog_leaves(subject,idx) WHERE subject<>'';
CREATE TABLE IF NOT EXISTS tlog_hashes (
 level INTEGER NOT NULL, idx INTEGER NOT NULL, hash BLOB NOT NULL, PRIMARY KEY(level,idx)) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS tlog_cursors (source TEXT PRIMARY KEY, seq INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS tlog_checkpoints (
 size INTEGER PRIMARY KEY, root BLOB NOT NULL, note TEXT NOT NULL, created_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS tlog_anchors (
 size INTEGER PRIMARY KEY REFERENCES tlog_checkpoints(size), digest TEXT NOT NULL, ots BLOB NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('pending','confirmed','stale')), bitcoin_height INTEGER NOT NULL DEFAULT 0,
 calendars TEXT NOT NULL, submitted_at INTEGER NOT NULL, checked_at INTEGER NOT NULL DEFAULT 0);
CREATE TRIGGER IF NOT EXISTS tlog_leaves_no_update BEFORE UPDATE ON tlog_leaves BEGIN SELECT RAISE(ABORT,'transparency log is append-only'); END;
CREATE TRIGGER IF NOT EXISTS tlog_leaves_no_delete BEFORE DELETE ON tlog_leaves BEGIN SELECT RAISE(ABORT,'transparency log is append-only'); END;
CREATE TRIGGER IF NOT EXISTS tlog_hashes_no_update BEFORE UPDATE ON tlog_hashes BEGIN SELECT RAISE(ABORT,'transparency log is append-only'); END;
CREATE TRIGGER IF NOT EXISTS tlog_hashes_no_delete BEFORE DELETE ON tlog_hashes BEGIN SELECT RAISE(ABORT,'transparency log is append-only'); END;
CREATE TRIGGER IF NOT EXISTS tlog_checkpoints_no_update BEFORE UPDATE ON tlog_checkpoints BEGIN SELECT RAISE(ABORT,'transparency log is append-only'); END;
CREATE TRIGGER IF NOT EXISTS tlog_checkpoints_no_delete BEFORE DELETE ON tlog_checkpoints BEGIN SELECT RAISE(ABORT,'transparency log is append-only'); END;
`

const (
	// LogPageMax bounds /api/log/leaves and /api/log/anchors pages.
	LogPageMax = 256
	// logCatchUpBatch bounds the rows one catch-up reads per source.
	logCatchUpBatch = 2000
	// recordProofsMax bounds the key-event proofs in one record.
	recordProofsMax = 16
)

// logLeaf is a leaf's canonical bytes: compact JSON in this field order, zero
// fields omitted, HTML characters not escaped.
type logLeaf struct {
	V          int    `json:"v"`
	Kind       string `json:"kind"`
	At         int64  `json:"at"`
	Op         string `json:"op,omitempty"`
	ID         string `json:"id,omitempty"`
	Seq        int64  `json:"seq,omitempty"`
	Room       string `json:"room,omitempty"`
	Agent      string `json:"agent,omitempty"`
	Target     string `json:"target,omitempty"`
	TextSHA256 string `json:"text_sha256,omitempty"`
	Supersedes string `json:"supersedes,omitempty"`
	ReplyTo    string `json:"reply_to,omitempty"`
	Tier       int64  `json:"tier,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Detail     string `json:"detail,omitempty"`
	Signature  string `json:"signature,omitempty"`
}

func (l logLeaf) bytes() []byte {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	_ = e.Encode(l)
	return bytes.TrimSuffix(b.Bytes(), []byte{'\n'})
}

// pendingLeaf is one source row on its way into the log.
type pendingLeaf struct {
	at      int64
	rank    int // the source's order among rows of one second
	source  string
	seq     int64
	leaf    logLeaf
	subject string
	ref     string
	skip    bool // advances the cursor without a leaf
}

func (p pendingLeaf) key() [3]int64 { return [3]int64{p.at, int64(p.rank), p.seq} }

func keyLess(a, b [3]int64) bool {
	return slices.Compare(a[:], b[:]) < 0
}

type logSource struct {
	name  string
	table string // whose max(seq) is the source's high-water mark
	rank  int
	query string // newer rows than ?, oldest first, at most ? rows
	scan  func(*sql.Rows) (pendingLeaf, error)
}

// eventIDRE is a message id (randomID).
var eventIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

// logSources are the outbox, in source rank: in one transaction an audit row
// (a handle claimed on a post) precedes the message, which precedes room
// governance and tier changes.
var logSources = []logSource{
	{"audit", "audit", 0, `SELECT au.seq,au.operation,au.actor,au.target,au.detail,au.created_at,coalesce(i.account,'') FROM audit au LEFT JOIN identities i ON i.id=au.actor
 WHERE au.seq>? AND au.operation IN ('agent.register','handle.claim','agent.rotate','hosted.claim','identity.link','identity.unlink','agent.profile.publish','agent.profile.remove','allowance.grant') ORDER BY au.seq LIMIT ?`,
		func(rows *sql.Rows) (pendingLeaf, error) {
			var p pendingLeaf
			var op, actor, target, detail, account string
			if err := rows.Scan(&p.seq, &op, &actor, &target, &detail, &p.at, &account); err != nil {
				return p, err
			}
			if op == "allowance.grant" {
				if strings.HasPrefix(target, "anon:") {
					p.skip = true
				}
				p.leaf = logLeaf{Kind: "grant", Op: op, Agent: actor, Target: target, Detail: detail}
				p.subject = target
				return p, nil
			}
			p.leaf = logLeaf{Kind: "identity", Op: op, Agent: actor, Target: target, Detail: detail}
			p.subject, p.ref = actor, account // ref: the account, checked for publicness below
			return p, nil
		}},
	{"events", "events", 1, `SELECT e.seq,e.id,e.display_seq,e.room,e.author,e.hash,e.signature,e.supersedes,e.reply_to,e.created_at FROM events e JOIN rooms r ON r.name=e.room
 WHERE e.seq>? AND r.visibility='public' ORDER BY e.seq LIMIT ?`,
		func(rows *sql.Rows) (pendingLeaf, error) {
			var p pendingLeaf
			l := logLeaf{Kind: "message"}
			if err := rows.Scan(&p.seq, &l.ID, &l.Seq, &l.Room, &l.Agent, &l.TextSHA256, &l.Signature, &l.Supersedes, &l.ReplyTo, &p.at); err != nil {
				return p, err
			}
			p.leaf, p.subject, p.ref = l, l.Agent, l.ID
			return p, nil
		}},
	{"room_log", "room_moderation_log", 2, `SELECT l.seq,l.room,l.action,l.actor,l.target,l.reason,l.detail,l.signature,l.created_at FROM room_moderation_log l JOIN rooms r ON r.name=l.room
 WHERE l.seq>? AND r.visibility='public' ORDER BY l.seq LIMIT ?`,
		func(rows *sql.Rows) (pendingLeaf, error) {
			var p pendingLeaf
			l := logLeaf{Kind: "moderation"}
			if err := rows.Scan(&p.seq, &l.Room, &l.Op, &l.Agent, &l.Target, &l.Reason, &l.Detail, &l.Signature, &p.at); err != nil {
				return p, err
			}
			p.leaf, p.subject = l, l.Agent
			if eventIDRE.MatchString(l.Target) {
				p.ref = l.Target // a message id: its proof lookup finds the hide too
			}
			return p, nil
		}},
	{"tier", "tier_grant_log", 3, `SELECT seq,account,tier,action,reason,created_at FROM tier_grant_log WHERE seq>? ORDER BY seq LIMIT ?`,
		func(rows *sql.Rows) (pendingLeaf, error) {
			var p pendingLeaf
			l := logLeaf{Kind: "tier"}
			if err := rows.Scan(&p.seq, &l.Target, &l.Tier, &l.Op, &l.Reason, &p.at); err != nil {
				return p, err
			}
			p.leaf, p.subject = l, l.Target
			return p, nil
		}},
}

// tlogCatchUp appends a leaf for every outbox row newer than its source's
// cursor, in (created_at, rank, seq) order, within tx. It reads and writes
// only through tx (one SQLite connection: never the pool under a held tx).
// It returns how many rows it consumed.
func tlogCatchUp(ctx context.Context, tx *sql.Tx) (int, error) {
	// Each source's new rows in seq order, and its high-water mark: the
	// cursor moves past rows that are not logged (private rooms, other
	// operations) so they are never read again.
	lists := make([][]pendingLeaf, len(logSources))
	cursors := make([]int64, len(logSources))
	high := make([]int64, len(logSources))
	truncated := make([]bool, len(logSources))
	for i, src := range logSources {
		if err := tx.QueryRowContext(ctx, "SELECT seq FROM tlog_cursors WHERE source=?", src.name).Scan(&cursors[i]); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		if err := tx.QueryRowContext(ctx, "SELECT coalesce(max(seq),0) FROM "+src.table).Scan(&high[i]); err != nil {
			return 0, err
		}
		if high[i] <= cursors[i] {
			continue
		}
		rows, err := tx.QueryContext(ctx, src.query, cursors[i], logCatchUpBatch)
		if err != nil {
			return 0, err
		}
		for rows.Next() {
			p, err := src.scan(rows)
			if err != nil {
				rows.Close()
				return 0, err
			}
			p.rank, p.source = src.rank, src.name
			p.leaf.V, p.leaf.At = 1, p.at
			lists[i] = append(lists[i], p)
		}
		if err = rows.Close(); err != nil {
			return 0, err
		}
		truncated[i] = len(lists[i]) == logCatchUpBatch
	}
	var size int64
	if err := tx.QueryRowContext(ctx, "SELECT coalesce(max(idx)+1,0) FROM tlog_leaves").Scan(&size); err != nil {
		return 0, err
	}
	read := txHashReader(ctx, tx)
	consumed := 0
	// Merge by (created_at, rank, seq), keeping each source in seq order, and
	// stop when a truncated source runs out: its next rows are unknown.
	for {
		best := -1
		for i := range lists {
			if len(lists[i]) == 0 {
				if truncated[i] {
					best = -2
					break
				}
				continue
			}
			if best == -1 || keyLess(lists[i][0].key(), lists[best][0].key()) {
				best = i
			}
		}
		if best < 0 {
			break
		}
		p := lists[best][0]
		lists[best] = lists[best][1:]
		cursors[best] = p.seq
		consumed++
		if p.skip {
			continue
		}
		if p.leaf.Kind == "identity" {
			public, err := accountPublicAsOf(ctx, tx, p.ref, p.at, p.seq)
			if err != nil {
				return 0, err
			}
			if !public {
				continue
			}
			p.ref = ""
		}
		data := p.leaf.bytes()
		stored, err := tlog.AppendHashes(size, tlog.LeafHash(data), read)
		if err != nil {
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO tlog_leaves(idx,kind,subject,ref,data,created_at) VALUES(?,?,?,?,?,?)", size, p.leaf.Kind, p.subject, p.ref, string(data), p.at); err != nil {
			return 0, err
		}
		for _, h := range stored {
			if _, err = tx.ExecContext(ctx, "INSERT INTO tlog_hashes(level,idx,hash) VALUES(?,?,?)", h.Level, h.Index, h.Hash[:]); err != nil {
				return 0, err
			}
		}
		size++
	}
	complete := !slices.Contains(truncated, true)
	for i, src := range logSources {
		seq := cursors[i]
		if complete {
			seq = max(seq, high[i])
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO tlog_cursors(source,seq) VALUES(?,?) ON CONFLICT(source) DO UPDATE SET seq=excluded.seq WHERE excluded.seq<>tlog_cursors.seq", src.name, seq); err != nil {
			return 0, err
		}
	}
	return consumed, nil
}

// accountPublicAsOf is publicAccountSQL as of an audit row: the account had
// a public post by then, or had registered or published a profile at or
// before that row. It reads only rows that existed then, so the backfill and
// the live path decide alike.
func accountPublicAsOf(ctx context.Context, tx *sql.Tx, account string, at, auditSeq int64) (bool, error) {
	if account == "" {
		return false, nil
	}
	var public bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM events e JOIN rooms r ON r.name=e.room WHERE e.account=? AND r.visibility='public' AND e.created_at<=?)
 OR EXISTS(SELECT 1 FROM audit au JOIN identities ai ON ai.id=au.actor WHERE ai.account=? AND au.operation IN ('agent.register','agent.profile.publish') AND au.seq<=?)`, account, at, account, auditSeq).Scan(&public)
	return public, err
}

func txHashReader(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) tlog.HashReader {
	return tlog.HashReaderFunc(func(level int, index int64) (tlog.Hash, error) {
		var raw []byte
		var h tlog.Hash
		if err := q.QueryRowContext(ctx, "SELECT hash FROM tlog_hashes WHERE level=? AND idx=?", level, index).Scan(&raw); err != nil {
			return h, fmt.Errorf("transparency log hash %d/%d: %w", level, index, err)
		}
		if len(raw) != len(h) {
			return h, errors.New("transparency log: malformed stored hash")
		}
		copy(h[:], raw)
		return h, nil
	})
}

// migrateTransparency creates the log's tables and, the first time, backfills
// the whole public history into it. Schema 15.
func migrateTransparency(tx *sql.Tx) error {
	if _, err := tx.Exec(tlogSchema); err != nil {
		return err
	}
	for {
		n, err := tlogCatchUp(context.Background(), tx)
		if err != nil {
			return fmt.Errorf("transparency log backfill: %w", err)
		}
		if n == 0 {
			return nil
		}
	}
}

// transparencyState is the log's signing key and background job state.
type transparencyState struct {
	signer *tlog.NoteSigner
	origin string
	mu     sync.Mutex // one checkpoint at a time
}

// openTransparency loads the log key (LOG_KEY_FILE, default log.key beside
// the database; an in-memory database gets a key that lives as long as it).
func (s *Store) openTransparency(path string) error {
	var key ed25519.PrivateKey
	var err error
	keyFile := s.config.LogKeyFile
	if keyFile == "" && path != ":memory:" {
		keyFile = filepath.Join(filepath.Dir(path), tlog.KeyFileName)
	}
	if keyFile == "" {
		_, key, err = ed25519.GenerateKey(rand.Reader)
	} else {
		key, err = tlog.LoadOrCreateKey(keyFile)
	}
	if err != nil {
		return err
	}
	s.transparency.origin = s.config.ServiceID + "/log"
	s.transparency.signer, err = tlog.NewNoteSigner(s.transparency.origin, key)
	return err
}

// LogCheckpoint is a signed tree head.
type LogCheckpoint struct {
	Origin      string `json:"origin"`
	Size        int64  `json:"size"`
	Root        string `json:"root"`
	RootHex     string `json:"root_hex"`
	CreatedAt   int64  `json:"created_at"`
	Note        string `json:"note"`
	VerifierKey string `json:"verifier_key"`
	PublicKey   string `json:"public_key"`
}

// LogVerifierKey is the C2SP verifier key of the log ("NAME+HASH+KEY").
func (s *Store) LogVerifierKey() string { return s.transparency.signer.VerifierKey() }

func (s *Store) checkpointFrom(size, createdAt int64, root []byte, note string) LogCheckpoint {
	return LogCheckpoint{Origin: s.transparency.origin, Size: size, Root: base64.StdEncoding.EncodeToString(root), RootHex: hex.EncodeToString(root),
		CreatedAt: createdAt, Note: note, VerifierKey: s.transparency.signer.VerifierKey(), PublicKey: base64.RawURLEncoding.EncodeToString(s.transparency.signer.PublicKey())}
}

// SignCheckpoint catches the log up and signs a new checkpoint if the tree
// grew since the last one (or none exists). It reports whether it signed.
func (s *Store) SignCheckpoint(ctx context.Context) (bool, error) {
	s.transparency.mu.Lock()
	defer s.transparency.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err = tlogCatchUp(ctx, tx); err != nil {
		return false, err
	}
	var size, last int64
	var count int
	if err = tx.QueryRowContext(ctx, "SELECT coalesce(max(idx)+1,0) FROM tlog_leaves").Scan(&size); err != nil {
		return false, err
	}
	if err = tx.QueryRowContext(ctx, "SELECT coalesce(max(size),0),count(*) FROM tlog_checkpoints").Scan(&last, &count); err != nil {
		return false, err
	}
	if count > 0 && size == last {
		return false, tx.Commit()
	}
	root, err := tlog.TreeHash(size, txHashReader(ctx, tx))
	if err != nil {
		return false, err
	}
	note, err := s.transparency.signer.Sign(tlog.Checkpoint{Origin: s.transparency.origin, Size: size, Root: root}.String())
	if err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO tlog_checkpoints(size,root,note,created_at) VALUES(?,?,?,?)", size, root[:], note, s.now().Unix()); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// ReadLogCheckpoint returns the checkpoint of exactly size leaves, or the
// latest when size is negative.
func (s *Store) ReadLogCheckpoint(ctx context.Context, size int64) (LogCheckpoint, error) {
	query, args := "SELECT size,root,note,created_at FROM tlog_checkpoints ORDER BY size DESC LIMIT 1", []any{}
	if size >= 0 {
		query, args = "SELECT size,root,note,created_at FROM tlog_checkpoints WHERE size=?", []any{size}
	}
	var root []byte
	var note string
	var created int64
	latest := size < 0
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&size, &root, &note, &created)
	if errors.Is(err, sql.ErrNoRows) && latest {
		return LogCheckpoint{}, problem(503, "no_checkpoint", "The log has no signed checkpoint yet; retry in a few minutes.")
	}
	if errors.Is(err, sql.ErrNoRows) {
		return LogCheckpoint{}, problem(404, "not_found", "No checkpoint of that size; GET /api/log/checkpoint is the latest.")
	}
	if err != nil {
		return LogCheckpoint{}, err
	}
	return s.checkpointFrom(size, created, root, note), nil
}

// LogEntry is one leaf.
type LogEntry struct {
	Index int64  `json:"index"`
	Kind  string `json:"kind"`
	Data  string `json:"data"`
	Hash  string `json:"leaf_hash"`
}

func newLogEntry(index int64, kind, data string) LogEntry {
	h := tlog.LeafHash([]byte(data))
	return LogEntry{Index: index, Kind: kind, Data: data, Hash: base64.StdEncoding.EncodeToString(h[:])}
}

// LogInclusion proves one leaf against a checkpoint.
type LogInclusion struct {
	Leaf       LogEntry      `json:"leaf"`
	TreeSize   int64         `json:"tree_size"`
	Proof      []string      `json:"proof"`
	Checkpoint LogCheckpoint `json:"checkpoint"`
	// Related are the other leaves about the same message (hides, restores),
	// each with its own proof.
	Related []LogInclusion `json:"related,omitempty"`
}

func encodeHashes(hs []tlog.Hash) []string {
	out := make([]string, len(hs))
	for i, h := range hs {
		out[i] = base64.StdEncoding.EncodeToString(h[:])
	}
	return out
}

// checkpointFor resolves the size a proof is made against: the checkpoint of
// that exact size, or the latest when size is negative.
func (s *Store) checkpointFor(ctx context.Context, size int64) (LogCheckpoint, error) {
	return s.ReadLogCheckpoint(ctx, size)
}

// ReadLogProof is the inclusion proof of leaf index (or, when message is set,
// of that message's leaf) against the checkpoint of size (latest when < 0).
func (s *Store) ReadLogProof(ctx context.Context, index int64, message string, size int64) (LogInclusion, error) {
	cp, err := s.checkpointFor(ctx, size)
	if err != nil {
		return LogInclusion{}, err
	}
	var related []int64
	if message != "" {
		rows, err := s.db.QueryContext(ctx, "SELECT idx FROM tlog_leaves WHERE ref=? AND ref<>'' AND idx<? ORDER BY idx LIMIT 16", message, cp.Size)
		if err != nil {
			return LogInclusion{}, err
		}
		var found []int64
		for rows.Next() {
			var i int64
			if err = rows.Scan(&i); err != nil {
				rows.Close()
				return LogInclusion{}, err
			}
			found = append(found, i)
		}
		if err = rows.Close(); err != nil {
			return LogInclusion{}, err
		}
		if len(found) == 0 {
			return LogInclusion{}, problem(404, "not_logged", "That message is not in the latest checkpoint: it is private, unknown, or newer than the checkpoint (signed every few minutes).")
		}
		index, related = found[0], found[1:]
	}
	out, err := s.inclusion(ctx, index, cp)
	if err != nil {
		return out, err
	}
	for _, i := range related {
		r, err := s.inclusion(ctx, i, cp)
		if err != nil {
			return out, err
		}
		r.Checkpoint = LogCheckpoint{}
		out.Related = append(out.Related, r)
	}
	return out, nil
}

func (s *Store) inclusion(ctx context.Context, index int64, cp LogCheckpoint) (LogInclusion, error) {
	if index < 0 || index >= cp.Size {
		return LogInclusion{}, problem(404, "not_found", fmt.Sprintf("Leaf %d is not in the checkpoint of %d leaves.", index, cp.Size))
	}
	var kind, data string
	if err := s.db.QueryRowContext(ctx, "SELECT kind,data FROM tlog_leaves WHERE idx=?", index).Scan(&kind, &data); err != nil {
		return LogInclusion{}, err
	}
	proof, err := tlog.InclusionProof(index, cp.Size, txHashReader(ctx, s.db))
	if err != nil {
		return LogInclusion{}, err
	}
	return LogInclusion{Leaf: newLogEntry(index, kind, data), TreeSize: cp.Size, Proof: encodeHashes(proof), Checkpoint: cp}, nil
}

// LogConsistency proves the checkpoint of size From is a prefix of the one of
// size To.
type LogConsistency struct {
	From  LogCheckpoint `json:"from"`
	To    LogCheckpoint `json:"to"`
	Proof []string      `json:"proof"`
}

// ReadLogConsistency proves checkpoint from is a prefix of checkpoint to
// (latest when to < 0). Both must be signed checkpoints.
func (s *Store) ReadLogConsistency(ctx context.Context, from, to int64) (LogConsistency, error) {
	b, err := s.checkpointFor(ctx, to)
	if err != nil {
		return LogConsistency{}, err
	}
	a, err := s.ReadLogCheckpoint(ctx, from)
	if err != nil {
		return LogConsistency{}, err
	}
	if a.Size > b.Size {
		return LogConsistency{}, problem(400, "invalid_request", "from must not be larger than to.")
	}
	proof, err := tlog.ConsistencyProof(a.Size, b.Size, txHashReader(ctx, s.db))
	if err != nil {
		return LogConsistency{}, err
	}
	return LogConsistency{From: a, To: b, Proof: encodeHashes(proof)}, nil
}

// ReadLogLeaves returns leaves [start, end), at most LogPageMax, and the
// current tree size (which may exceed the latest checkpoint).
func (s *Store) ReadLogLeaves(ctx context.Context, start, end int64) ([]LogEntry, int64, error) {
	var size int64
	if err := s.db.QueryRowContext(ctx, "SELECT coalesce(max(idx)+1,0) FROM tlog_leaves").Scan(&size); err != nil {
		return nil, 0, err
	}
	if start < 0 || (end >= 0 && end < start) {
		return nil, size, problem(400, "invalid_request", "Expected 0 <= start <= end.")
	}
	if end < 0 || end > start+LogPageMax {
		end = start + LogPageMax
	}
	rows, err := s.db.QueryContext(ctx, "SELECT idx,kind,data FROM tlog_leaves WHERE idx>=? AND idx<? ORDER BY idx", start, end)
	if err != nil {
		return nil, size, err
	}
	defer rows.Close()
	out := []LogEntry{}
	for rows.Next() {
		var i int64
		var kind, data string
		if err = rows.Scan(&i, &kind, &data); err != nil {
			return nil, size, err
		}
		out = append(out, newLogEntry(i, kind, data))
	}
	return out, size, rows.Err()
}

// LogAnchor is one checkpoint's OpenTimestamps proof.
type LogAnchor struct {
	Size          int64    `json:"size"`
	Digest        string   `json:"digest"`
	State         string   `json:"state"`
	BitcoinHeight int64    `json:"bitcoin_height,omitempty"`
	Calendars     []string `json:"calendars"`
	SubmittedAt   int64    `json:"submitted_at"`
	CheckedAt     int64    `json:"checked_at,omitempty"`
}

// ReadLogAnchors lists anchors newest first, below size before (all when <= 0).
func (s *Store) ReadLogAnchors(ctx context.Context, before int64, limit int) ([]LogAnchor, error) {
	if before <= 0 {
		before = 1 << 62
	}
	if limit <= 0 || limit > LogPageMax {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, "SELECT size,digest,state,bitcoin_height,calendars,submitted_at,checked_at FROM tlog_anchors WHERE size<? ORDER BY size DESC LIMIT ?", before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LogAnchor{}
	for rows.Next() {
		var a LogAnchor
		var cals string
		if err = rows.Scan(&a.Size, &a.Digest, &a.State, &a.BitcoinHeight, &cals, &a.SubmittedAt, &a.CheckedAt); err != nil {
			return nil, err
		}
		a.Calendars = strings.Fields(cals)
		out = append(out, a)
	}
	return out, rows.Err()
}

// ReadLogAnchorFile is the .ots proof of the checkpoint of size.
func (s *Store) ReadLogAnchorFile(ctx context.Context, size int64) ([]byte, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, "SELECT ots FROM tlog_anchors WHERE size=?", size).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, problem(404, "not_found", "No OpenTimestamps proof for that checkpoint; GET /api/log/anchors lists them.")
	}
	return raw, err
}

// TransparencyConfig configures the background job.
type TransparencyConfig struct {
	// CheckpointEvery is how often a checkpoint is signed when the tree grew.
	CheckpointEvery time.Duration
	// Calendars are the OpenTimestamps calendars; none turns anchoring off.
	Calendars []string
	// OTS is the calendar client; nil uses one for Calendars.
	OTS *ots.Client
}

// StartTransparency runs the log's background job until ctx ends: catch up
// and sign a checkpoint every CheckpointEvery when the tree grew, anchor new
// checkpoints and upgrade pending anchors. Network I/O never runs inside a
// transaction.
func (s *Store) StartTransparency(ctx context.Context, cfg TransparencyConfig) {
	if cfg.CheckpointEvery <= 0 {
		cfg.CheckpointEvery = 15 * time.Minute
	}
	if cfg.OTS == nil && len(cfg.Calendars) > 0 {
		cfg.OTS = &ots.Client{Calendars: cfg.Calendars}
	}
	go func() {
		ticker := time.NewTicker(cfg.CheckpointEvery)
		defer ticker.Stop()
		for {
			s.transparencyTick(ctx, cfg)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (s *Store) transparencyTick(ctx context.Context, cfg TransparencyConfig) {
	work, cancel := context.WithTimeout(ctx, 30*time.Second)
	_, err := s.SignCheckpoint(work)
	cancel()
	if err != nil && ctx.Err() == nil {
		slog.Warn("Transparency checkpoint failed", "error", err)
	}
	if cfg.OTS == nil {
		return
	}
	work, cancel = context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err = s.AnchorCheckpoints(work, cfg.OTS, 3); err != nil && ctx.Err() == nil {
		slog.Warn("OpenTimestamps anchoring failed", "error", err)
	}
	if err = s.UpgradeAnchors(work, cfg.OTS, 12); err != nil && ctx.Err() == nil {
		slog.Info("OpenTimestamps upgrade incomplete", "error", err)
	}
}

// anchorDigest is what a checkpoint's anchor timestamps: SHA-256 of the
// signed checkpoint note, exactly as /api/log/checkpoint/note?size= serves it.
func anchorDigest(note string) []byte {
	h := sha256.Sum256([]byte(note))
	return h[:]
}

// AnchorCheckpoints submits up to limit unanchored checkpoints, newest first,
// to the OpenTimestamps calendars.
func (s *Store) AnchorCheckpoints(ctx context.Context, client *ots.Client, limit int) error {
	rows, err := s.db.QueryContext(ctx, "SELECT c.size,c.note FROM tlog_checkpoints c LEFT JOIN tlog_anchors a ON a.size=c.size WHERE a.size IS NULL ORDER BY c.size DESC LIMIT ?", limit)
	if err != nil {
		return err
	}
	type todo struct {
		size int64
		note string
	}
	var work []todo
	for rows.Next() {
		var t todo
		if err = rows.Scan(&t.size, &t.note); err != nil {
			rows.Close()
			return err
		}
		work = append(work, t)
	}
	if err = rows.Close(); err != nil {
		return err
	}
	var errs []error
	for _, t := range work {
		digest := anchorDigest(t.note)
		file, failures := client.Stamp(ctx, digest)
		errs = append(errs, failures...)
		if file == nil {
			continue
		}
		pending, _ := file.Stamp.Status()
		_, err = s.db.ExecContext(ctx, "INSERT OR IGNORE INTO tlog_anchors(size,digest,ots,state,calendars,submitted_at) VALUES(?,?,?,'pending',?,?)",
			t.size, hex.EncodeToString(digest), file.Bytes(), strings.Join(pending, " "), s.now().Unix())
		if err != nil {
			return err
		}
	}
	return errors.Join(errs...)
}

const (
	anchorUpgradeAfter = 2 * 3600       // calendars commit to Bitcoin within hours
	anchorCheckEvery   = 3600           // between upgrade attempts
	anchorStaleAfter   = 14 * 24 * 3600 // a proof still pending then is marked stale, never deleted
)

// UpgradeAnchors asks the calendars for completed proofs of up to limit
// pending anchors.
func (s *Store) UpgradeAnchors(ctx context.Context, client *ots.Client, limit int) error {
	now := s.now().Unix()
	if _, err := s.db.ExecContext(ctx, "UPDATE tlog_anchors SET state='stale' WHERE state='pending' AND submitted_at<?", now-anchorStaleAfter); err != nil {
		return err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT size,ots FROM tlog_anchors WHERE state='pending' AND submitted_at<=? AND checked_at<=? ORDER BY checked_at,size LIMIT ?", now-anchorUpgradeAfter, now-anchorCheckEvery, limit)
	if err != nil {
		return err
	}
	type todo struct {
		size int64
		raw  []byte
	}
	var work []todo
	for rows.Next() {
		var t todo
		if err = rows.Scan(&t.size, &t.raw); err != nil {
			rows.Close()
			return err
		}
		work = append(work, t)
	}
	if err = rows.Close(); err != nil {
		return err
	}
	var errs []error
	for _, t := range work {
		file, err := ots.ParseFile(t.raw)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		changed, err := client.Upgrade(ctx, file)
		if err != nil {
			errs = append(errs, err)
		}
		state, height := "pending", int64(0)
		if _, h := file.Stamp.Status(); h > 0 {
			state, height = "confirmed", int64(h)
		}
		raw := t.raw
		if changed {
			raw = file.Bytes()
		}
		if _, err = s.db.ExecContext(ctx, "UPDATE tlog_anchors SET ots=?,state=?,bitcoin_height=?,checked_at=? WHERE size=?", raw, state, height, s.now().Unix(), t.size); err != nil {
			return err
		}
	}
	return errors.Join(errs...)
}

// LogRecord is an agent's portable dossier.
type LogRecord struct {
	Type        string         `json:"type"`
	Service     string         `json:"service"`
	GeneratedAt int64          `json:"generated_at"`
	Agent       string         `json:"agent"`
	Handle      string         `json:"handle,omitempty"`
	Account     string         `json:"account"`
	Keys        []RecordKey    `json:"keys"`
	Handles     []RecordHandle `json:"handles"`
	Bindings    []RecordLink   `json:"bindings"`
	Counts      map[string]int `json:"counts"`
	FirstSeen   int64          `json:"first_seen"`
	LastSeen    int64          `json:"last_seen"`
	Checkpoint  LogCheckpoint  `json:"checkpoint"`
	Proofs      []LogInclusion `json:"proofs"`
}

type RecordKey struct {
	Fingerprint string `json:"fingerprint"`
	PublicKey   string `json:"public_key"`
	CreatedAt   int64  `json:"created_at"`
	Successor   string `json:"successor,omitempty"`
}

type RecordHandle struct {
	Handle string `json:"handle"`
	At     int64  `json:"at"`
	Key    string `json:"key"`
}

type RecordLink struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
	State string `json:"state"`
}

// SignedRecord is a record and the log key's signed note over its exact
// bytes: the note's text is the record's compact JSON and a newline.
type SignedRecord struct {
	Record LogRecord `json:"record"`
	Note   string    `json:"note"`
}

// ReadLogRecord builds and signs the record of a public agent named by
// handle or fingerprint (any of its keys).
func (s *Store) ReadLogRecord(ctx context.Context, who string) (SignedRecord, error) {
	notFound := problem(404, "agent_not_found", "No public agent has that handle or key fingerprint.")
	var id string
	switch {
	case fingerprintRE.MatchString(who):
		id = who
	case handleRE.MatchString(who):
		if err := s.db.QueryRowContext(ctx, "SELECT id FROM identities WHERE handle=?", strings.ToLower(who)).Scan(&id); errors.Is(err, sql.ErrNoRows) {
			return SignedRecord{}, notFound
		} else if err != nil {
			return SignedRecord{}, err
		}
	default:
		return SignedRecord{}, problem(400, "invalid_agent", "Expected a handle or a 64-character key fingerprint.")
	}
	var account string
	var public bool
	err := s.db.QueryRowContext(ctx, "SELECT i.account,"+publicAccountSQL("i.account")+" FROM identities i WHERE i.id=?", id).Scan(&account, &public)
	if errors.Is(err, sql.ErrNoRows) || err == nil && !public {
		return SignedRecord{}, notFound
	}
	if err != nil {
		return SignedRecord{}, err
	}
	cp, err := s.checkpointFor(ctx, -1)
	if err != nil {
		return SignedRecord{}, err
	}
	r := LogRecord{Type: "swarmmemo.record/v1", Service: s.config.ServiceID, GeneratedAt: s.now().Unix(), Account: account, Keys: []RecordKey{}, Handles: []RecordHandle{}, Bindings: []RecordLink{}, Counts: map[string]int{}, Checkpoint: cp, Proofs: []LogInclusion{}}
	rows, err := s.db.QueryContext(ctx, "SELECT id,public_key,created_at,successor,handle,last_seen FROM identities WHERE account=? ORDER BY created_at,id", account)
	if err != nil {
		return SignedRecord{}, err
	}
	var keys []string
	for rows.Next() {
		var k RecordKey
		var handle string
		var seen int64
		if err = rows.Scan(&k.Fingerprint, &k.PublicKey, &k.CreatedAt, &k.Successor, &handle, &seen); err != nil {
			rows.Close()
			return SignedRecord{}, err
		}
		r.Keys = append(r.Keys, k)
		keys = append(keys, k.Fingerprint)
		if k.Successor == "" {
			r.Agent, r.Handle = k.Fingerprint, handle
		}
		if r.FirstSeen == 0 || k.CreatedAt < r.FirstSeen {
			r.FirstSeen = k.CreatedAt
		}
		r.LastSeen = max(r.LastSeen, seen)
	}
	if err = rows.Close(); err != nil {
		return SignedRecord{}, err
	}
	if r.Agent == "" && len(r.Keys) > 0 {
		r.Agent = r.Keys[len(r.Keys)-1].Fingerprint
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")
	args := make([]any, len(keys))
	for i, k := range keys {
		args[i] = k
	}
	links, err := s.db.QueryContext(ctx, "SELECT kind,value,state FROM identity_links WHERE agent IN ("+placeholders+") AND NOT "+retiredSealKey+" ORDER BY kind,value", args...)
	if err != nil {
		return SignedRecord{}, err
	}
	for links.Next() {
		var l RecordLink
		if err = links.Scan(&l.Kind, &l.Value, &l.State); err != nil {
			links.Close()
			return SignedRecord{}, err
		}
		r.Bindings = append(r.Bindings, l)
	}
	if err = links.Close(); err != nil {
		return SignedRecord{}, err
	}
	var messages, logged int
	if err = s.db.QueryRowContext(ctx, "SELECT count(*) FROM events e JOIN rooms rm ON rm.name=e.room WHERE e.account=? AND rm.visibility='public' AND e.hidden=0", account).Scan(&messages); err != nil {
		return SignedRecord{}, err
	}
	// Key events: the identity leaves of the account's keys, newest last.
	leaves, err := s.db.QueryContext(ctx, "SELECT idx,data,created_at FROM tlog_leaves WHERE subject IN ("+placeholders+") AND subject<>'' AND kind='identity' AND idx<? ORDER BY idx", append(args, cp.Size)...)
	if err != nil {
		return SignedRecord{}, err
	}
	var events []int64
	for leaves.Next() {
		var idx, at int64
		var data string
		if err = leaves.Scan(&idx, &data, &at); err != nil {
			leaves.Close()
			return SignedRecord{}, err
		}
		events = append(events, idx)
		var l logLeaf
		if json.Unmarshal([]byte(data), &l) == nil && (l.Op == "agent.register" || l.Op == "handle.claim") && l.Detail != "" {
			r.Handles = append(r.Handles, RecordHandle{Handle: l.Detail, At: at, Key: l.Agent})
		}
	}
	if err = leaves.Close(); err != nil {
		return SignedRecord{}, err
	}
	if err = s.db.QueryRowContext(ctx, "SELECT count(*) FROM tlog_leaves WHERE subject IN ("+placeholders+") AND subject<>''", args...).Scan(&logged); err != nil {
		return SignedRecord{}, err
	}
	r.Counts["public_messages"], r.Counts["log_entries"], r.Counts["key_events"] = messages, logged, len(events)
	if len(events) > recordProofsMax {
		events = events[len(events)-recordProofsMax:]
	}
	for _, idx := range events {
		p, err := s.inclusion(ctx, idx, cp)
		if err != nil {
			return SignedRecord{}, err
		}
		p.Checkpoint = LogCheckpoint{} // the record's own checkpoint
		r.Proofs = append(r.Proofs, p)
	}
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err = e.Encode(r); err != nil {
		return SignedRecord{}, err
	}
	note, err := s.transparency.signer.Sign(b.String())
	if err != nil {
		return SignedRecord{}, err
	}
	return SignedRecord{Record: r, Note: note}, nil
}
