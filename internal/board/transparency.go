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
// allowance grants, tier_grant_log, and doc_versions (shared docs' versions,
// private docs too: a leaf holds only ids, the version number and the
// SHA-256, so a member can prove a doc's history; never a paste's),
// notary_public_keys (the notary's public key) and notary_receipts (every
// notary stamp: its hash, sequence, key ID and signature, never who asked),
// so a stamp's proof is the log's. tlog_cursors keeps, per source, the
// last row turned into a leaf. tlogCatchUp appends every newer row, merged by
// (created_at, source rank, seq), inside the caller's transaction: every
// write path calls it before committing, and the background job catches up
// whatever a path without the call wrote. The one-time backfill is the same
// function from cursor zero, so history and new events get identical leaves.
// An identity row is logged once its account is public; the rows from
// before (a hosted identity's handle, claimed at hosted.create) go in just
// before the account's first leaf, and tlogBackfillIdentity appended those
// that earlier versions passed over.
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
	mrand "math/rand/v2"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"swarmmemo/internal/ots"
	"swarmmemo/internal/services"
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
	// An identity.witness leaf: the witnessed link and the witness's verdict
	// (link_kind, since kind names the leaf's own kind).
	LinkKind  string `json:"link_kind,omitempty"`
	Value     string `json:"value,omitempty"`
	Nonce     string `json:"nonce,omitempty"`
	Verdict   string `json:"verdict,omitempty"`
	Signature string `json:"signature,omitempty"`
	// A notary leaf: notary.stamp is the stamped hash, the receipt's key ID
	// and signature (seq is the receipt's); notary.key is the notary's key
	// ID and public key (base64url). Last, so earlier leaves keep their
	// bytes.
	Hash      string `json:"hash,omitempty"`
	KeyID     string `json:"key_id,omitempty"`
	PublicKey string `json:"public_key,omitempty"`
}

// Notary leaves' refs: a stamp by its hash, the key by its key ID (both are
// 64 hex digits, so each has its own prefix).
const (
	notaryStampRef = "notary:"
	notaryKeyRef   = "notary-key:"
)

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
	skip    bool   // advances the cursor without a leaf
	byTime  bool   // an identity leaf judged public as of its second, not of an audit row
	account string // an audit identity row's or a message's account, for deferred identity leaves
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
// identityLogOps are the audit operations logged as identity leaves.
const identityLogOps = `'agent.register','handle.claim','agent.rotate','hosted.claim','identity.link','identity.unlink','agent.profile.publish','agent.profile.remove'`

var logSources = []logSource{
	{"audit", "audit", 0, `SELECT au.seq,au.operation,au.actor,au.target,au.detail,au.created_at,coalesce(i.account,'') FROM audit au LEFT JOIN identities i ON i.id=au.actor
 WHERE au.seq>? AND au.operation IN (` + identityLogOps + `,'allowance.grant') ORDER BY au.seq LIMIT ?`,
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
			p.subject, p.ref, p.account = actor, account, account // ref: the account, checked for publicness below
			return p, nil
		}},
	{"events", "events", 1, `SELECT e.seq,e.id,e.display_seq,e.room,e.author,e.hash,e.signature,e.supersedes,e.reply_to,e.created_at,e.account FROM events e JOIN rooms r ON r.name=e.room
 WHERE e.seq>? AND r.visibility='public' ORDER BY e.seq LIMIT ?`,
		func(rows *sql.Rows) (pendingLeaf, error) {
			var p pendingLeaf
			l := logLeaf{Kind: "message"}
			if err := rows.Scan(&p.seq, &l.ID, &l.Seq, &l.Room, &l.Agent, &l.TextSHA256, &l.Signature, &l.Supersedes, &l.ReplyTo, &p.at, &p.account); err != nil {
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
	{"docs", "doc_versions", 4, `SELECT v.seq,v.id,v.doc,v.version,v.hash,v.created_at,coalesce(d.kind,'doc') FROM doc_versions v LEFT JOIN docs d ON d.id=v.doc WHERE v.seq>? ORDER BY v.seq LIMIT ?`,
		func(rows *sql.Rows) (pendingLeaf, error) {
			// A shared doc's version: its id (the proof's ref), the doc, the
			// version number and the SHA-256 of its text; never the text, the
			// author or the group. A paste's (a doc of kind paste) is not
			// logged, as pastes never were.
			var p pendingLeaf
			var kind string
			l := logLeaf{Kind: "doc"}
			if err := rows.Scan(&p.seq, &l.ID, &l.Target, &l.Seq, &l.TextSHA256, &p.at, &kind); err != nil {
				return p, err
			}
			p.leaf, p.ref, p.skip = l, l.ID, kind == "paste"
			return p, nil
		}},
	{"witness", "link_witnesses", 5, `SELECT w.seq,w.witness,w.agent,w.kind,w.value,w.nonce,w.verdict,w.signature,w.created_at,coalesce(i.account,'') FROM link_witnesses w LEFT JOIN identities i ON i.id=w.agent
 WHERE w.seq>? ORDER BY w.seq LIMIT ?`,
		func(rows *sql.Rows) (pendingLeaf, error) {
			// An identity.witness: the witness key (agent, the subject), the
			// agent it witnessed (target), the link's kind and value (as an
			// identity.link leaf carries it), the nonce, the verdict and the
			// witness's signature. Logged when the witnessed agent was public.
			// A new source starts at cursor zero, so its first run appends
			// the earlier witnesses, as the 1.30.0 backfill did.
			var p pendingLeaf
			l := logLeaf{Kind: "identity", Op: "identity.witness"}
			var account string
			if err := rows.Scan(&p.seq, &l.Agent, &l.Target, &l.LinkKind, &l.Value, &l.Nonce, &l.Verdict, &l.Signature, &p.at, &account); err != nil {
				return p, err
			}
			p.leaf, p.subject, p.ref, p.byTime = l, l.Agent, account, true
			return p, nil
		}},
	{"notary_keys", "notary_public_keys", 6, `SELECT seq,key_id,public_key,created_at FROM notary_public_keys WHERE seq>? ORDER BY seq LIMIT ?`,
		func(rows *sql.Rows) (pendingLeaf, error) {
			// The notary's public key, so a receipt's key_id can be checked
			// against a logged key. It ranks before the stamps of its second.
			var p pendingLeaf
			l := logLeaf{Kind: "notary", Op: "notary.key"}
			if err := rows.Scan(&p.seq, &l.KeyID, &l.PublicKey, &p.at); err != nil {
				return p, err
			}
			p.leaf, p.ref = l, notaryKeyRef+l.KeyID
			return p, nil
		}},
	{"notary", "notary_receipts", 7, `SELECT seq,hash,time,key_id,signature FROM notary_receipts WHERE seq>? ORDER BY seq LIMIT ?`,
		func(rows *sql.Rows) (pendingLeaf, error) {
			// A notary stamp: the hash, the receipt's sequence, key ID and
			// signature; never the account that asked. A new source starts at
			// cursor zero, so its first run appends every earlier receipt
			// (the one-time backfill), each with its original time.
			var p pendingLeaf
			l := logLeaf{Kind: "notary", Op: "notary.stamp"}
			if err := rows.Scan(&p.seq, &l.Hash, &p.at, &l.KeyID, &l.Signature); err != nil {
				return p, err
			}
			l.Seq = p.seq
			p.leaf, p.ref = l, notaryStampRef+l.Hash
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
	checked := map[string]bool{} // accounts whose earlier identity rows are known logged
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
			public, err := accountPublicAsOf(ctx, tx, p.ref, p.at, p.seq, p.byTime)
			if err != nil {
				return 0, err
			}
			if !public {
				continue
			}
			p.ref = ""
		}
		if p.account != "" && !checked[p.account] {
			// The account's first leaf: its identity rows from before it
			// was public (a hosted identity's handle, a handle claimed on a
			// private post) go first, each with its original time. Audit
			// rows up to the cursor are consumed; this row is not one of them.
			checked[p.account] = true
			first, err := accountFirstOnRecord(ctx, tx, p.account)
			if err != nil {
				return 0, err
			}
			if first {
				bound := cursors[0]
				if best == 0 {
					bound = p.seq - 1
				}
				if _, err = logEarlierIdentity(ctx, tx, p.account, bound, &size, read); err != nil {
					return 0, err
				}
			}
		}
		if err := appendLeaf(ctx, tx, &size, read, p.leaf, p.subject, p.ref, p.at); err != nil {
			return 0, err
		}
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

// appendLeaf appends one leaf at index *size through tx and advances it.
func appendLeaf(ctx context.Context, tx *sql.Tx, size *int64, read tlog.HashReader, leaf logLeaf, subject, ref string, at int64) error {
	data := leaf.bytes()
	stored, err := tlog.AppendHashes(*size, tlog.LeafHash(data), read)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO tlog_leaves(idx,kind,subject,ref,data,created_at) VALUES(?,?,?,?,?,?)", *size, leaf.Kind, subject, ref, string(data), at); err != nil {
		return err
	}
	for _, h := range stored {
		if _, err = tx.ExecContext(ctx, "INSERT INTO tlog_hashes(level,idx,hash) VALUES(?,?,?)", h.Level, h.Index, h.Hash[:]); err != nil {
			return err
		}
	}
	*size++
	return nil
}

// onRecordSQL is true when the account ? has a message leaf or an identity
// leaf of its own (a witness leaf is the witness key's, logged because the
// witnessed agent is public, so it does not count).
const onRecordSQL = `EXISTS(SELECT 1 FROM identities k JOIN tlog_leaves l ON l.subject=k.id WHERE k.account=? AND l.subject<>''
 AND (l.kind='message' OR l.kind='identity' AND l.data NOT LIKE '%"op":"identity.witness"%'))`

// accountFirstOnRecord reports whether a keyed account has no leaf of its
// own yet, so the leaf about to be appended puts it on the record.
func accountFirstOnRecord(ctx context.Context, tx *sql.Tx, account string) (bool, error) {
	var first bool
	err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM identities WHERE account=?) AND NOT "+onRecordSQL, account, account).Scan(&first)
	return first, err
}

// identityRow is an audit row logged as an identity leaf.
type identityRow struct {
	seq, at                         int64
	op, actor, target, detail, acct string
}

func (r identityRow) leaf() logLeaf {
	return logLeaf{V: 1, Kind: "identity", At: r.at, Op: r.op, Agent: r.actor, Target: r.target, Detail: r.detail}
}

// logEarlierIdentity appends a leaf for each of the account's identity audit
// rows up to seq bound that has none, oldest first, each with its original
// time; it returns how many. An identity row is logged only once its account
// is public (accountPublicAsOf), so the rows from before are logged when it
// becomes public. A row's leaf is found by its exact bytes, so a second call
// appends nothing.
func logEarlierIdentity(ctx context.Context, tx *sql.Tx, account string, bound int64, size *int64, read tlog.HashReader) (int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT au.seq,au.operation,au.actor,au.target,au.detail,au.created_at FROM identities i JOIN audit au ON au.actor=i.id
 WHERE i.account=? AND au.seq<=? AND au.operation IN (`+identityLogOps+`) ORDER BY au.created_at,au.seq`, account, bound)
	if err != nil {
		return 0, err
	}
	var list []identityRow
	for rows.Next() {
		r := identityRow{acct: account}
		if err = rows.Scan(&r.seq, &r.op, &r.actor, &r.target, &r.detail, &r.at); err != nil {
			rows.Close()
			return 0, err
		}
		list = append(list, r)
	}
	if err = rows.Close(); err != nil {
		return 0, err
	}
	return appendIdentityRows(ctx, tx, list, size, read)
}

func appendIdentityRows(ctx context.Context, tx *sql.Tx, list []identityRow, size *int64, read tlog.HashReader) (int, error) {
	n := 0
	for _, r := range list {
		l := r.leaf()
		var logged bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM tlog_leaves WHERE subject=? AND kind='identity' AND data=?)", r.actor, string(l.bytes())).Scan(&logged); err != nil {
			return n, err
		}
		if logged {
			continue
		}
		if err := appendLeaf(ctx, tx, size, read, l, r.actor, "", r.at); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// tlogBackfillIdentity is the one-time backfill (C29) of the identity
// rows that accounts on the record wrote before they were public: before the
// first-leaf check in tlogCatchUp they were never logged (a hosted identity's
// handle, claimed at creation, most of all). It appends them now, after the
// catch-up, each with its original time; the log is never rewritten. It runs
// in upkeepData at every start, once (recorded in meta), and finds existing
// leaves by their bytes, so it never appends a row twice.
func tlogBackfillIdentity(ctx context.Context, tx *sql.Tx) (int, error) {
	var done int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM meta WHERE key='tlog_identity_backfill'").Scan(&done); err != nil || done > 0 {
		return 0, err
	}
	var cursor int64
	if err := tx.QueryRowContext(ctx, "SELECT seq FROM tlog_cursors WHERE source='audit'").Scan(&cursor); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT au.seq,au.operation,au.actor,au.target,au.detail,au.created_at,i.account FROM audit au JOIN identities i ON i.id=au.actor
 WHERE au.seq<=? AND au.operation IN (`+identityLogOps+`) ORDER BY au.created_at,au.seq`, cursor)
	if err != nil {
		return 0, err
	}
	var all []identityRow
	for rows.Next() {
		var r identityRow
		if err = rows.Scan(&r.seq, &r.op, &r.actor, &r.target, &r.detail, &r.at, &r.acct); err != nil {
			rows.Close()
			return 0, err
		}
		all = append(all, r)
	}
	if err = rows.Close(); err != nil {
		return 0, err
	}
	onRecord := map[string]bool{}
	var list []identityRow
	for _, r := range all {
		on, ok := onRecord[r.acct]
		if !ok {
			if err = tx.QueryRowContext(ctx, "SELECT "+onRecordSQL, r.acct).Scan(&on); err != nil {
				return 0, err
			}
			onRecord[r.acct] = on
		}
		if on {
			list = append(list, r)
		}
	}
	var size int64
	if err = tx.QueryRowContext(ctx, "SELECT coalesce(max(idx)+1,0) FROM tlog_leaves").Scan(&size); err != nil {
		return 0, err
	}
	n, err := appendIdentityRows(ctx, tx, list, &size, txHashReader(ctx, tx))
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO meta(key,value) VALUES('tlog_identity_backfill',?)", strconv.Itoa(n))
	return n, err
}

// accountPublicAsOf is publicAccountSQL as of an audit row: the account had
// a public post by then, or had registered or published a profile at or
// before that row. It reads only rows that existed then, so the backfill and
// the live path decide alike. byTime decides as of the second at instead, for
// a row from a source other than audit.
func accountPublicAsOf(ctx context.Context, tx *sql.Tx, account string, at, auditSeq int64, byTime bool) (bool, error) {
	if account == "" {
		return false, nil
	}
	var public bool
	if byTime {
		err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM events e JOIN rooms r ON r.name=e.room WHERE e.account=? AND r.visibility='public' AND e.created_at<=?)
 OR EXISTS(SELECT 1 FROM audit au JOIN identities ai ON ai.id=au.actor WHERE ai.account=? AND au.operation IN ('agent.register','agent.profile.publish') AND au.created_at<=?)`, account, at, account, at).Scan(&public)
		return public, err
	}
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
	// A message leaf's proof carries what the leaf only hashes, while the
	// message is public and not hidden: its text (SHA-256 is the leaf's
	// text_sha256) and, when signed, signed_payload, the exact canonical
	// bytes the leaf's signature covers. Leaves themselves never change.
	Text          *string `json:"text,omitempty"`
	SignedPayload string  `json:"signed_payload,omitempty"`
	// Anchor is the Bitcoin anchor of the first checkpoint covering the leaf
	// (pending, then confirmed with its block), so the leaf's time bracket
	// and its lag are visible; prove against it with size=anchor.size.
	Anchor *LogAnchor `json:"anchor,omitempty"`
}

// withAnchor sets a proof's Anchor (top-level proofs only).
func (s *Store) withAnchor(ctx context.Context) func(LogInclusion, error) (LogInclusion, error) {
	return func(p LogInclusion, err error) (LogInclusion, error) {
		if err != nil {
			return p, err
		}
		p.Anchor, err = s.leafAnchor(ctx, p.Leaf.Index)
		return p, err
	}
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
	var out LogInclusion
	if message == "" {
		out, err = s.inclusion(ctx, index, cp)
	} else {
		out, err = s.refProof(ctx, message, cp, nil, problem(404, "not_logged", "That message is not in the latest checkpoint: it is private, unknown, or newer than the checkpoint (signed every few minutes)."))
	}
	if out, err = s.withAnchor(ctx)(out, err); err != nil || out.Leaf.Kind != "message" {
		return out, err
	}
	return out, s.attachLoggedMessage(ctx, &out)
}

// attachLoggedMessage adds a message leaf's text and signed payload to its
// proof, so the proof alone checks both the text against text_sha256 and the
// signature offline. Only while the message is readable by anyone: in a
// public room and not hidden; the text must still hash to the leaf's digest.
func (s *Store) attachLoggedMessage(ctx context.Context, p *LogInclusion) error {
	var l logLeaf
	if json.Unmarshal([]byte(p.Leaf.Data), &l) != nil || !eventIDRE.MatchString(l.ID) {
		return nil
	}
	var text, payload, signature, hash string
	err := s.db.QueryRowContext(ctx, `SELECT e.text,e.payload,e.signature,e.hash FROM events e JOIN rooms r ON r.name=e.room
 WHERE e.id=? AND e.hidden=0 AND r.visibility='public'`, l.ID).Scan(&text, &payload, &signature, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if hash != l.TextSHA256 || sha256Hex([]byte(text)) != l.TextSHA256 {
		return nil
	}
	p.Text = &text
	if payload != "" && signature == l.Signature {
		p.SignedPayload = payload
	}
	return nil
}

// notaryProofRE is a stamped hash, or "key" for the notary key.
var notaryProofRE = regexp.MustCompile(`^(?:[0-9a-f]{64}|key)$`)

// ReadNotaryProof is the inclusion proof of a notary stamp's leaf (by its
// hash), with the leaf of the key that signed it as related; or, when hash
// is "key", of the notary key's leaf. Against the checkpoint of size
// (latest when < 0).
func (s *Store) ReadNotaryProof(ctx context.Context, hash string, size int64) (LogInclusion, error) {
	if !notaryProofRE.MatchString(hash) {
		return LogInclusion{}, problem(400, "invalid_request", "notary must be a lowercase SHA-256 hex digest, or key.")
	}
	cp, err := s.checkpointFor(ctx, size)
	if err != nil {
		return LogInclusion{}, err
	}
	if hash == "key" {
		// The running notary's key; without one, the last key logged.
		ref := ""
		if id := services.NotaryKeyID(s.services.notaryKey); id != "" {
			ref = notaryKeyRef + id
		} else if err = s.db.QueryRowContext(ctx, "SELECT ref FROM tlog_leaves WHERE kind='notary' AND substr(ref,1,?)=? AND idx<? ORDER BY idx DESC LIMIT 1",
			len(notaryKeyRef), notaryKeyRef, cp.Size).Scan(&ref); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return LogInclusion{}, err
		}
		return s.withAnchor(ctx)(s.refProof(ctx, ref, cp, nil, problem(404, "not_logged", "The notary key is not in the latest checkpoint: the notary is off here, or its key is newer than the checkpoint (signed every few minutes).")))
	}
	keyLeaf := func(data string) string {
		var l logLeaf
		if json.Unmarshal([]byte(data), &l) != nil || l.KeyID == "" {
			return ""
		}
		return notaryKeyRef + l.KeyID
	}
	return s.withAnchor(ctx)(s.refProof(ctx, notaryStampRef+hash, cp, keyLeaf, problem(404, "not_logged", "No notary receipt for that hash is in the latest checkpoint: it is unknown, or newer than the checkpoint (signed every few minutes).")))
}

// refProof proves the first leaf with ref against cp, with the later leaves
// of the same ref as related, then the leaves of also(first leaf's data)'s
// ref when also is set.
func (s *Store) refProof(ctx context.Context, ref string, cp LogCheckpoint, also func(string) string, notFound error) (LogInclusion, error) {
	if ref == "" {
		return LogInclusion{}, notFound
	}
	found, err := s.refLeaves(ctx, ref, cp.Size)
	if err != nil {
		return LogInclusion{}, err
	}
	if len(found) == 0 {
		return LogInclusion{}, notFound
	}
	out, err := s.inclusion(ctx, found[0], cp)
	if err != nil {
		return out, err
	}
	related := found[1:]
	if also != nil {
		if other := also(out.Leaf.Data); other != "" {
			more, err := s.refLeaves(ctx, other, cp.Size)
			if err != nil {
				return out, err
			}
			related = append(related, more...)
		}
	}
	for _, i := range related {
		r, err := s.inclusion(ctx, i, cp)
		if err != nil {
			return out, err
		}
		// Each related proof carries the checkpoint it was proven against, so
		// it verifies on its own terms (an emptied one still serialized).
		out.Related = append(out.Related, r)
	}
	return out, nil
}

// refLeaves are the indexes of up to 16 leaves with ref below size, oldest
// first.
func (s *Store) refLeaves(ctx context.Context, ref string, size int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT idx FROM tlog_leaves WHERE ref=? AND ref<>'' AND idx<? ORDER BY idx LIMIT 16", ref, size)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var found []int64
	for rows.Next() {
		var i int64
		if err = rows.Scan(&i); err != nil {
			return nil, err
		}
		found = append(found, i)
	}
	return found, rows.Err()
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

// LogAnchor is one checkpoint's OpenTimestamps proof and its timeline:
// checkpoint_at (signed), submitted_at (sent to the calendars), checked_at
// (last asked for the Bitcoin proof), confirmed_at (when this service first
// saw the Bitcoin attestation of block bitcoin_height), block_time (that
// block's own timestamp, once read from its header) and, while pending,
// next_check_at. Explorer is the block's page on a public block explorer.
type LogAnchor struct {
	Size          int64    `json:"size"`
	Digest        string   `json:"digest"`
	State         string   `json:"state"`
	BitcoinHeight int64    `json:"bitcoin_height,omitempty"`
	Calendars     []string `json:"calendars"`
	CheckpointAt  int64    `json:"checkpoint_at,omitempty"`
	SubmittedAt   int64    `json:"submitted_at"`
	CheckedAt     int64    `json:"checked_at,omitempty"`
	ConfirmedAt   int64    `json:"confirmed_at,omitempty"`
	BlockTime     int64    `json:"block_time,omitempty"`
	Explorer      string   `json:"explorer,omitempty"`
	NextCheckAt   int64    `json:"next_check_at,omitempty"`
	OTS           string   `json:"ots"`
	Note          string   `json:"note"`
}

// blockTimeKey prefixes the meta rows that hold a Bitcoin block's timestamp
// by height (blockTimeKey+"970409" = Unix seconds): a fact about the block,
// shared by every anchor in it, so no anchor column is needed.
const blockTimeKey = "btc_block_time:"

// ExplorerBlockURL is a block's page on a public block explorer.
const ExplorerBlockURL = "https://mempool.space/block/"

const anchorColumns = "a.size,a.digest,a.state,a.bitcoin_height,a.calendars,coalesce(c.created_at,0),a.submitted_at,a.checked_at," +
	"coalesce((SELECT CAST(m.value AS INTEGER) FROM meta m WHERE m.key='" + blockTimeKey + "'||a.bitcoin_height),0) FROM tlog_anchors a LEFT JOIN tlog_checkpoints c ON c.size=a.size"

func scanAnchor(row interface{ Scan(...any) error }) (LogAnchor, error) {
	var a LogAnchor
	var cals string
	if err := row.Scan(&a.Size, &a.Digest, &a.State, &a.BitcoinHeight, &cals, &a.CheckpointAt, &a.SubmittedAt, &a.CheckedAt, &a.BlockTime); err != nil {
		return a, err
	}
	a.Calendars = strings.Fields(cals)
	switch a.State {
	case "confirmed":
		// Only a pending anchor is checked again, so the last check of a
		// confirmed one is the one that saw its Bitcoin attestation.
		a.ConfirmedAt = a.CheckedAt
		if a.BitcoinHeight > 0 {
			a.Explorer = ExplorerBlockURL + strconv.FormatInt(a.BitcoinHeight, 10)
		}
	case "pending":
		a.NextCheckAt = anchorNextCheck(a.SubmittedAt, a.CheckedAt)
	}
	size := strconv.FormatInt(a.Size, 10)
	a.OTS, a.Note = "/api/log/anchors/"+size+".ots", "/api/log/checkpoint/note?size="+size
	return a, nil
}

// ReadLogAnchors lists anchors newest first, below size before (all when <= 0).
func (s *Store) ReadLogAnchors(ctx context.Context, before int64, limit int) ([]LogAnchor, error) {
	if before <= 0 {
		before = 1 << 62
	}
	if limit <= 0 || limit > LogPageMax {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+anchorColumns+" WHERE a.size<? ORDER BY a.size DESC LIMIT ?", before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LogAnchor{}
	for rows.Next() {
		a, err := scanAnchor(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// leafAnchor is the anchor of the first checkpoint that covers leaf index
// (a stale one aside): the earliest Bitcoin time bracket of that leaf. Nil
// before one exists.
func (s *Store) leafAnchor(ctx context.Context, index int64) (*LogAnchor, error) {
	a, err := scanAnchor(s.db.QueryRowContext(ctx, "SELECT "+anchorColumns+" WHERE a.size>? AND a.state<>'stale' ORDER BY a.size LIMIT 1", index))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
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

// anchorPollEvery is how often pending anchors are looked at (with up to
// anchorPollJitter added); anchorNextCheck decides which are due.
const (
	anchorPollEvery  = 5 * time.Minute
	anchorPollJitter = 30 * time.Second
)

// StartTransparency runs the log's background job until ctx ends: catch up
// and sign a checkpoint every CheckpointEvery when the tree grew and submit
// it to the calendars at once; every few minutes, retry a submission that
// failed and ask the calendars for the Bitcoin proofs that are due
// (anchorNextCheck). One goroutine does both, so they never overlap; network
// I/O never runs inside a transaction.
func (s *Store) StartTransparency(ctx context.Context, cfg TransparencyConfig) {
	if cfg.CheckpointEvery <= 0 {
		cfg.CheckpointEvery = 15 * time.Minute
	}
	if cfg.OTS == nil && len(cfg.Calendars) > 0 {
		cfg.OTS = &ots.Client{Calendars: cfg.Calendars, Explorer: ots.DefaultExplorer}
	}
	go func() {
		ticker := time.NewTicker(cfg.CheckpointEvery)
		defer ticker.Stop()
		poll := time.NewTimer(anchorPollDelay())
		defer poll.Stop()
		s.transparencyTick(ctx, cfg)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.transparencyTick(ctx, cfg)
			case <-poll.C:
				s.anchorTick(ctx, cfg, 1)
				poll.Reset(anchorPollDelay())
			}
		}
	}()
}

// anchorPollDelay is anchorPollEvery plus jitter, so polls do not line up
// with other clients' on the minute.
func anchorPollDelay() time.Duration {
	return anchorPollEvery + mrand.N(anchorPollJitter)
}

func (s *Store) transparencyTick(ctx context.Context, cfg TransparencyConfig) {
	work, cancel := context.WithTimeout(ctx, 30*time.Second)
	_, err := s.SignCheckpoint(work)
	cancel()
	if err != nil && ctx.Err() == nil {
		slog.Warn("Transparency checkpoint failed", "error", err)
	}
	s.anchorTick(ctx, cfg, 3)
}

// anchorTick submits up to submit unanchored checkpoints, newest first, then
// upgrades the pending anchors that are due, at most 12 (each one GET per
// calendar still pending), all within two minutes.
func (s *Store) anchorTick(ctx context.Context, cfg TransparencyConfig, submit int) {
	if cfg.OTS == nil {
		return
	}
	work, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := s.AnchorCheckpoints(work, cfg.OTS, submit); err != nil && ctx.Err() == nil {
		slog.Warn("OpenTimestamps anchoring failed", "error", err)
	}
	if err := s.UpgradeAnchors(work, cfg.OTS, 12); err != nil && ctx.Err() == nil {
		slog.Info("OpenTimestamps upgrade incomplete", "error", err)
	}
	if err := s.FillBlockTimes(work, cfg.OTS, 4); err != nil && ctx.Err() == nil {
		slog.Info("Bitcoin block times incomplete", "error", err)
	}
}

// FillBlockTimes reads the timestamps of up to limit Bitcoin blocks that
// confirmed anchors name and that have none yet, newest first, from the
// client's block explorer (each header checked against the anchor's
// attestation), and keeps each in meta under blockTimeKey. A block that
// fails is tried again next time.
func (s *Store) FillBlockTimes(ctx context.Context, client *ots.Client, limit int) error {
	if client == nil || client.Explorer == "" {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, "SELECT a.bitcoin_height,min(a.size) FROM tlog_anchors a WHERE a.state='confirmed' AND a.bitcoin_height>0 AND NOT EXISTS (SELECT 1 FROM meta m WHERE m.key='"+blockTimeKey+"'||a.bitcoin_height) GROUP BY a.bitcoin_height ORDER BY a.bitcoin_height DESC LIMIT ?", limit)
	if err != nil {
		return err
	}
	type todo struct{ height, size int64 }
	var work []todo
	for rows.Next() {
		var t todo
		if err = rows.Scan(&t.height, &t.size); err != nil {
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
		var raw []byte
		if err = s.db.QueryRowContext(ctx, "SELECT ots FROM tlog_anchors WHERE size=?", t.size).Scan(&raw); err != nil {
			return err
		}
		file, err := ots.ParseFile(raw)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		root := file.Stamp.BitcoinRoot(uint64(t.height))
		if root == nil {
			errs = append(errs, fmt.Errorf("anchor %d: no attestation of block %d", t.size, t.height))
			continue
		}
		at, err := client.BlockTime(ctx, uint64(t.height), root)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err = s.db.ExecContext(ctx, "INSERT OR IGNORE INTO meta(key,value) VALUES(?,?)", blockTimeKey+strconv.FormatInt(t.height, 10), strconv.FormatInt(at, 10)); err != nil {
			return err
		}
	}
	return errors.Join(errs...)
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

// The upgrade schedule, in seconds from submission. A calendar aggregates
// digests into one Bitcoin transaction every so often and serves the
// Bitcoin attestation once that transaction has confirmations, so nothing
// can come back for the first half hour; then one check per Bitcoin block
// interval for the first three hours (when nearly every proof completes),
// every half hour to a day, every two hours after.
const (
	anchorFirstCheck = 30 * 60
	anchorFastUntil  = 3 * 3600
	anchorFastEvery  = 10 * 60
	anchorSlowUntil  = 24 * 3600
	anchorSlowEvery  = 30 * 60
	anchorIdleEvery  = 2 * 3600
	anchorCheckEarly = 60             // a poll this much early still counts, so polls every anchorPollEvery keep the interval
	anchorStaleAfter = 14 * 24 * 3600 // a proof still pending then is marked stale, never deleted
	anchorPendingMax = 4096           // pending anchors one poll looks at
)

// AnchorTimeline is the expected path from a post to "confirmed", for
// /api/log/anchors and /capabilities.
const AnchorTimeline = "A post is in the next checkpoint (signed every 15 minutes by default when the log grew), submitted to the calendars at once (submitted_at). " +
	"A calendar's Bitcoin transaction is typically mined 10 to 45 minutes later (bitcoin_height: that block's time bounds the post from above), and its proof is served once the transaction has confirmations. " +
	"Pending anchors are checked every 10 minutes from 30 minutes to 3 hours after submission, then every 30 minutes, then every 2 hours (next_check_at); " +
	"confirmed_at is when this service first saw the proof, typically 1 to 1.5 hours after the checkpoint; block_time is the block's own timestamp."

// anchorNextCheck is when a pending anchor submitted at submitted and last
// checked at checked (0: never) is next asked for its Bitcoin proof.
func anchorNextCheck(submitted, checked int64) int64 {
	next := submitted + anchorFirstCheck
	if checked < submitted {
		return next
	}
	every := int64(anchorIdleEvery)
	switch age := checked - submitted; {
	case age < anchorFastUntil:
		every = anchorFastEvery
	case age < anchorSlowUntil:
		every = anchorSlowEvery
	}
	return max(next, checked+every)
}

// UpgradeAnchors asks the calendars for completed proofs of up to limit
// pending anchors that are due (anchorNextCheck), longest unchecked first.
func (s *Store) UpgradeAnchors(ctx context.Context, client *ots.Client, limit int) error {
	now := s.now().Unix()
	if _, err := s.db.ExecContext(ctx, "UPDATE tlog_anchors SET state='stale' WHERE state='pending' AND submitted_at<?", now-anchorStaleAfter); err != nil {
		return err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT size,submitted_at,checked_at FROM tlog_anchors WHERE state='pending' AND submitted_at<=? ORDER BY checked_at,size LIMIT ?", now-anchorFirstCheck+anchorCheckEarly, anchorPendingMax)
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
		var submitted, checked int64
		if err = rows.Scan(&t.size, &submitted, &checked); err != nil {
			rows.Close()
			return err
		}
		if len(work) < limit && now >= anchorNextCheck(submitted, checked)-anchorCheckEarly {
			work = append(work, t)
		}
	}
	if err = rows.Close(); err != nil {
		return err
	}
	var errs []error
	for _, t := range work {
		if err = s.db.QueryRowContext(ctx, "SELECT ots FROM tlog_anchors WHERE size=?", t.size).Scan(&t.raw); err != nil {
			return err
		}
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

// AgentRecord is when an agent went on the record: its first identity or
// message leaf, across its account's keys, and whether a Bitcoin-confirmed
// checkpoint covers it yet. agent.get carries it as record.
type AgentRecord struct {
	FirstLeaf     int64  `json:"first_leaf"`
	FirstAt       int64  `json:"first_at"`
	ProofURL      string `json:"proof_url"`
	Anchored      bool   `json:"anchored"`
	AnchoredAt    int64  `json:"anchored_at,omitempty"`    // when the earliest covering checkpoint was timestamped
	BitcoinHeight int64  `json:"bitcoin_height,omitempty"` // the block that confirmed it
}

// agentRecord reads an agent's record through tx: per key of its account,
// the first identity or message leaf by the subject index (it stops at the
// first match), then the earliest confirmed anchor whose checkpoint covers
// that leaf, by the anchors' primary key. Nil when nothing is logged yet.
func agentRecord(ctx context.Context, tx *sql.Tx, agent string) (*AgentRecord, error) {
	var r AgentRecord
	err := tx.QueryRowContext(ctx, `SELECT l.idx,l.created_at FROM identities k JOIN tlog_leaves l ON l.idx=(
 SELECT f.idx FROM tlog_leaves f WHERE f.subject=k.id AND f.subject<>'' AND f.kind IN ('identity','message') ORDER BY f.idx LIMIT 1)
 WHERE k.account=(SELECT account FROM identities WHERE id=?) ORDER BY l.idx LIMIT 1`, agent).Scan(&r.FirstLeaf, &r.FirstAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.ProofURL = fmt.Sprintf("/api/log/proof?leaf=%d", r.FirstLeaf)
	err = tx.QueryRowContext(ctx, "SELECT submitted_at,bitcoin_height FROM tlog_anchors WHERE size>? AND state='confirmed' ORDER BY size LIMIT 1", r.FirstLeaf).Scan(&r.AnchoredAt, &r.BitcoinHeight)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	r.Anchored = err == nil
	return &r, nil
}
