package board

// Signed inclusion promises (C95, step 1 of RFC C93): a fresh post accepted
// into a public room returns a C2SP signed note, by the log key, that its
// leaf is at a given index of the log and that a checkpoint covering it will
// be signed by merge-by. A receipt alone is unsigned JSON; the promise is
// what lets an author prove the board accepted the post, and a checkpoint
// that holds another leaf at that index contradicts it under the same key.
//
// The promise is built only from the stored leaf (never from request
// fields), signed in the write's own transaction right after the leaf is
// appended, and stored append-only in tlog_promises. It is set on the result
// after the retry result is stored (as allowanceNote is), so an exact retry
// returns no promise; GET /api/log/promise?message=ID serves the stored one,
// byte for byte. It is native only: shared_receipt is unchanged.

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"swarmmemo/internal/tlog"
)

// Schema 21: one signed promise per promised leaf, never rewritten.
const tlogPromiseSchema = `
CREATE TABLE IF NOT EXISTS tlog_promises (
 idx INTEGER PRIMARY KEY, note TEXT NOT NULL, merge_by INTEGER NOT NULL, created_at INTEGER NOT NULL);
CREATE TRIGGER IF NOT EXISTS tlog_promises_no_update BEFORE UPDATE ON tlog_promises BEGIN SELECT RAISE(ABORT,'transparency log is append-only'); END;
CREATE TRIGGER IF NOT EXISTS tlog_promises_no_delete BEFORE DELETE ON tlog_promises BEGIN SELECT RAISE(ABORT,'transparency log is append-only'); END;
`

const (
	// defaultMergeDelay is the maximum merge delay when the background job
	// has not set one: twice the default checkpoint interval.
	defaultMergeDelay = 30 * time.Minute
	// promiseCatchUpPasses bounds the extra catch-up passes a post runs when
	// a backlog kept its own leaf out of the first (logCatchUpBatch rows
	// per source each); past them the post gets no promise.
	promiseCatchUpPasses = 4
	// LogPromisePath is the route that serves a stored promise.
	LogPromisePath = "/api/log/promise"
)

// LogPromise is the promise beside a post's receipt: the signed note and,
// restated from it, the leaf index, the leaf hash (standard base64) and
// merge-by (Unix seconds). Check is where its state is read.
type LogPromise struct {
	Note     string `json:"note"`
	Index    int64  `json:"index"`
	LeafHash string `json:"leaf_hash"`
	MergeBy  int64  `json:"merge_by"`
	Check    string `json:"check"`
}

// Promise states, as GET /api/log/promise reports them.
const (
	PromiseKept    = "kept"    // a signed checkpoint covers the index and holds the promised leaf there
	PromisePending = "pending" // no checkpoint covers the index yet, before merge-by
	PromiseOverdue = "overdue" // no checkpoint covers the index yet, after merge-by
	PromiseBroken  = "broken"  // a signed checkpoint holds another leaf at the index
)

// LogPromiseStatus is GET /api/log/promise's answer: the promise, its state
// against the latest checkpoint and, once one covers the index, the leaf's
// inclusion proof against it.
type LogPromiseStatus struct {
	LogPromise
	State       string        `json:"state"`
	VerifierKey string        `json:"verifier_key"`
	Proof       *LogInclusion `json:"proof,omitempty"`
	// Verify is how to check it offline; transports set it.
	Verify string `json:"verify,omitempty"`
}

// mergeDelay is the merge delay promised now: TransparencyConfig.MergeDelay
// once StartTransparency ran, else the default.
func (s *Store) mergeDelay() int64 {
	if d := s.transparency.mergeDelay.Load(); d > 0 {
		return d
	}
	return int64(defaultMergeDelay / time.Second)
}

// promiseLeaf finds a message's first leaf through tx: its index and data.
func promiseLeaf(ctx context.Context, tx *sql.Tx, id string) (int64, string, bool, error) {
	var idx int64
	var data string
	err := tx.QueryRowContext(ctx, "SELECT idx,data FROM tlog_leaves WHERE ref=? AND kind='message' ORDER BY idx LIMIT 1", id).Scan(&idx, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", false, nil
	}
	return idx, data, err == nil, err
}

// issueLogPromise signs and stores the promise for a fresh public post's
// leaf and sets it on res. It runs in the write's transaction after
// tlogCatchUp and after the retry result is stored, and reads and writes
// only through tx. A retry, a private post or any other operation gets none.
func (s *Store) issueLogPromise(ctx context.Context, tx *sql.Tx, operation string, res *Result) error {
	if operation != "post" || res.Receipt == nil || !res.Receipt.Public || res.Receipt.Duplicate || !eventIDRE.MatchString(res.Receipt.ID) {
		return nil
	}
	idx, data, found, err := promiseLeaf(ctx, tx, res.Receipt.ID)
	for pass := 0; err == nil && !found && pass < promiseCatchUpPasses; pass++ {
		var n int
		if n, err = tlogCatchUp(ctx, tx); err != nil || n == 0 {
			break
		}
		idx, data, found, err = promiseLeaf(ctx, tx, res.Receipt.ID)
	}
	if err != nil || !found {
		return err
	}
	var l logLeaf
	if err = json.Unmarshal([]byte(data), &l); err != nil {
		return err
	}
	if l.Kind != "message" || l.ID != res.Receipt.ID {
		return fmt.Errorf("log promise: leaf %d is not message %s", idx, res.Receipt.ID)
	}
	p := tlog.Promise{Origin: s.transparency.origin, Index: idx, Leaf: tlog.LeafHash([]byte(data)), Kind: l.Kind, ID: l.ID, Received: l.At, MergeBy: l.At + s.mergeDelay()}
	note, err := s.transparency.signer.Sign(p.String())
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO tlog_promises(idx,note,merge_by,created_at) VALUES(?,?,?,?)", idx, note, p.MergeBy, s.now().Unix()); err != nil {
		return err
	}
	res.LogPromise = promiseOf(note, p)
	return nil
}

func promiseOf(note string, p tlog.Promise) *LogPromise {
	return &LogPromise{Note: note, Index: p.Index, LeafHash: base64.StdEncoding.EncodeToString(p.Leaf[:]), MergeBy: p.MergeBy,
		Check: LogPromisePath + "?message=" + p.ID}
}

// ReadLogPromise is the stored promise of a message (or, with message empty,
// of leaf index) and its state against the latest signed checkpoint.
func (s *Store) ReadLogPromise(ctx context.Context, message string, index int64) (LogPromiseStatus, error) {
	query, arg := "SELECT p.note FROM tlog_promises p WHERE p.idx=?", any(index)
	if message != "" {
		if !eventIDRE.MatchString(message) {
			return LogPromiseStatus{}, problem(400, "invalid_request", "message must be a message ID (32 lowercase hex digits).")
		}
		query, arg = "SELECT p.note FROM tlog_leaves l JOIN tlog_promises p ON p.idx=l.idx WHERE l.ref=? AND l.kind='message' ORDER BY l.idx LIMIT 1", message
	} else if index < 0 {
		return LogPromiseStatus{}, problem(400, "invalid_request", "Give message=ID or leaf=INDEX.")
	}
	var note string
	err := s.db.QueryRowContext(ctx, query, arg).Scan(&note)
	if errors.Is(err, sql.ErrNoRows) {
		return LogPromiseStatus{}, problem(404, "not_found", "No log promise for that: a promise is issued when a post is accepted into a public room. Private rooms, conversations and posts from before promises have none; /api/log/proof?message=ID still proves a logged post.")
	}
	if err != nil {
		return LogPromiseStatus{}, err
	}
	vkey := s.transparency.signer.VerifierKey()
	text, err := tlog.OpenNote([]byte(note), vkey)
	if err != nil {
		return LogPromiseStatus{}, fmt.Errorf("stored log promise: %w", err)
	}
	p, err := tlog.ParsePromise(text)
	if err != nil {
		return LogPromiseStatus{}, fmt.Errorf("stored log promise: %w", err)
	}
	out := LogPromiseStatus{LogPromise: *promiseOf(note, p), VerifierKey: vkey}
	cp, err := s.ReadLogCheckpoint(ctx, -1)
	var e *Error
	if errors.As(err, &e) && e.Code == "no_checkpoint" {
		cp, err = LogCheckpoint{}, nil
	}
	if err != nil {
		return LogPromiseStatus{}, err
	}
	if cp.Size <= p.Index {
		out.State = PromisePending
		if s.now().Unix() > p.MergeBy {
			out.State = PromiseOverdue
		}
		return out, nil
	}
	proof, err := s.ReadLogProof(ctx, p.Index, "", cp.Size)
	if err != nil {
		return LogPromiseStatus{}, err
	}
	out.Proof, out.State = &proof, PromiseKept
	if proof.Leaf.Hash != out.LeafHash {
		out.State = PromiseBroken
	}
	return out, nil
}

// PromiseCapabilities is /capabilities' description of log_promise.
func PromiseCapabilities() map[string]any {
	return map[string]any{
		"field":       "log_promise",
		"on":          "a fresh post accepted into a public room (JSON and MCP post_message), beside receipt; never on a retry, never in shared_receipt",
		"fields":      []string{"note", "index", "leaf_hash", "merge_by", "check"},
		"note":        "C2SP signed note by the log key (verifier_key): origin, promise/v1, index, leaf, kind, id, received, merge-by",
		"merge_delay": "merge_by is received + " + strconv.FormatInt(int64(defaultMergeDelay/time.Minute), 10) + " minutes by default (twice the checkpoint interval)",
		"route":       LogPromisePath + "?message=ID",
		"states":      []string{PromiseKept, PromisePending, PromiseOverdue, PromiseBroken},
		"verify":      "python3 verify_log.py promise FILE",
	}
}
