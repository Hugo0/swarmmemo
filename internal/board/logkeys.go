package board

// The log key's history (C983): every key that has signed the log's
// checkpoints, oldest first, served at /api/log/keys. A key's key_id is the
// hex SHA-256 of its raw public key. The genesis key is seeded at open from
// the running key (LOG_KEY_FILE), with first_size the oldest checkpoint, which
// must open under it; its proof of age is that checkpoint's Bitcoin anchor.
// A later key would carry a rotation note signed by the key it retires and by
// itself (tlog.SignRotation); writing one is not built yet, so a running key
// that is not on the history signs nothing (CheckLogKey refuses startup).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"swarmmemo/internal/tlog"
)

const tlogKeySchema = `
CREATE TABLE IF NOT EXISTS tlog_keys (
 key_id TEXT PRIMARY KEY, verifier_key TEXT NOT NULL UNIQUE, first_size INTEGER NOT NULL,
 retired_size INTEGER, rotation_note TEXT);
CREATE TRIGGER IF NOT EXISTS tlog_keys_no_delete BEFORE DELETE ON tlog_keys BEGIN SELECT RAISE(ABORT,'the log key history is append-only'); END;
`

// LogKeysPath is the route of the key history.
const LogKeysPath = "/api/log/keys"

// logKeyRow is one stored key.
type logKeyRow struct {
	keyID, verifierKey string
	firstSize          int64
	retired            sql.NullInt64
	rotation           sql.NullString
}

// LogKey is one entry of /api/log/keys.
type LogKey struct {
	KeyID       string `json:"key_id"`
	VerifierKey string `json:"verifier_key"`
	// FirstSize is the size of the oldest checkpoint this key signed, FirstAt
	// when it was signed.
	FirstSize int64 `json:"first_size"`
	FirstAt   int64 `json:"first_at"`
	// FirstAnchor is the earliest Bitcoin-confirmed anchor of a checkpoint
	// this key signed (the earliest pending one before any confirms): the
	// key existed and signed by that block.
	FirstAnchor  *LogKeyAnchor `json:"first_anchor"`
	RetiredSize  *int64        `json:"retired_size"`
	RotationNote *string       `json:"rotation_note"`
}

// LogKeyAnchor is a key's first anchor: Digest is SHA-256 of the checkpoint
// note of Size, in the Bitcoin block BitcoinHeight once confirmed.
type LogKeyAnchor struct {
	Size          int64  `json:"size"`
	Digest        string `json:"digest"`
	State         string `json:"state"`
	BitcoinHeight *int64 `json:"bitcoin_height"`
	OTSURL        string `json:"ots_url"`
	NoteURL       string `json:"note_url"`
}

var errLogKeyUnknown = errors.New("transparency log: the running log key (LOG_KEY_FILE) is not on the key history and key rotation is not built; restore the log key file")

// ensureLogKey puts the running key on the history when the history is
// empty: with first_size the oldest checkpoint, which must open under it, or
// pending (the size about to be signed) when no checkpoint exists yet;
// pending < 0 seeds nothing then. A running key not on a non-empty history,
// or one the oldest checkpoint does not open under, is errLogKeyUnknown. It
// runs inside tx and touches nothing else.
func (s *Store) ensureLogKey(ctx context.Context, tx *sql.Tx, pending int64) error {
	vkey := s.transparency.signer.VerifierKey()
	id := tlog.KeyID(s.transparency.signer.PublicKey())
	var known, total int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FILTER (WHERE key_id=?),count(*) FROM tlog_keys", id).Scan(&known, &total); err != nil {
		return err
	}
	if known > 0 {
		return nil
	}
	if total > 0 {
		return errLogKeyUnknown
	}
	var size int64
	var note string
	err := tx.QueryRowContext(ctx, "SELECT size,note FROM tlog_checkpoints ORDER BY size LIMIT 1").Scan(&size, &note)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if pending < 0 {
			return nil
		}
		size = pending
	case err != nil:
		return err
	default:
		if _, err = tlog.OpenNote([]byte(note), vkey); err != nil {
			return fmt.Errorf("%w (the oldest checkpoint, size %d, does not open under it)", errLogKeyUnknown, size)
		}
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO tlog_keys(key_id,verifier_key,first_size) VALUES(?,?,?)", id, vkey, size)
	return err
}

// openLogKeys seeds the history (a read-write open) and loads it. A running
// key the history refuses is not an error here: an operator command on a
// database copy without its key file (a rollout's migrated clone) still
// opens; CheckLogKey refuses to serve with it, and SignCheckpoint signs
// nothing.
func (s *Store) openLogKeys() error {
	ctx := context.Background()
	if !s.config.ReadOnly {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err = s.ensureLogKey(ctx, tx, -1); err != nil && !errors.Is(err, errLogKeyUnknown) {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	return s.loadLogKeys(ctx)
}

func (s *Store) loadLogKeys(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "SELECT key_id,verifier_key,first_size,retired_size,rotation_note FROM tlog_keys ORDER BY first_size,key_id")
	if err != nil {
		return err
	}
	defer rows.Close()
	var keys []logKeyRow
	for rows.Next() {
		var k logKeyRow
		if err = rows.Scan(&k.keyID, &k.verifierKey, &k.firstSize, &k.retired, &k.rotation); err != nil {
			return err
		}
		keys = append(keys, k)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	s.transparency.keysMu.Lock()
	s.transparency.keys = keys
	s.transparency.keysMu.Unlock()
	return nil
}

// CheckLogKey refuses to serve with a log key that is not on the key history
// (a lost or swapped LOG_KEY_FILE): it would sign checkpoints readers cannot
// tie to the keys they pinned. The server calls it before StartTransparency.
func (s *Store) CheckLogKey(ctx context.Context) error {
	if s.config.ReadOnly {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = s.ensureLogKey(ctx, tx, -1); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return s.loadLogKeys(ctx)
}

// logKeyFor is the key that signed the checkpoint of size: the history's
// entry covering it, else the running key (no checkpoint signed yet).
func (s *Store) logKeyFor(size int64) (keyID, verifierKey string) {
	s.transparency.keysMu.RLock()
	defer s.transparency.keysMu.RUnlock()
	for _, k := range s.transparency.keys {
		if k.firstSize <= size && (!k.retired.Valid || size < k.retired.Int64) {
			return k.keyID, k.verifierKey
		}
	}
	return tlog.KeyID(s.transparency.signer.PublicKey()), s.transparency.signer.VerifierKey()
}

// LogKeyID is the key_id of the running log key.
func (s *Store) LogKeyID() string { return tlog.KeyID(s.transparency.signer.PublicKey()) }

// ReadLogKeys is the key history, oldest first; 503 no_checkpoint before the
// first checkpoint puts the running key on it.
func (s *Store) ReadLogKeys(ctx context.Context) ([]LogKey, error) {
	if err := s.loadLogKeys(ctx); err != nil {
		return nil, err
	}
	s.transparency.keysMu.RLock()
	rows := append([]logKeyRow(nil), s.transparency.keys...)
	s.transparency.keysMu.RUnlock()
	if len(rows) == 0 {
		return nil, errNoCheckpoint()
	}
	out := make([]LogKey, 0, len(rows))
	for _, k := range rows {
		e := LogKey{KeyID: k.keyID, VerifierKey: k.verifierKey, FirstSize: k.firstSize}
		if k.retired.Valid {
			e.RetiredSize = &k.retired.Int64
		}
		if k.rotation.Valid {
			e.RotationNote = &k.rotation.String
		}
		if err := s.db.QueryRowContext(ctx, "SELECT coalesce(max(created_at),0) FROM tlog_checkpoints WHERE size=?", k.firstSize).Scan(&e.FirstAt); err != nil {
			return nil, err
		}
		end := int64(1 << 62)
		if k.retired.Valid {
			end = k.retired.Int64
		}
		var a LogKeyAnchor
		var height int64
		err := s.db.QueryRowContext(ctx, "SELECT size,digest,state,bitcoin_height FROM tlog_anchors WHERE size>=? AND size<? AND state<>'stale' ORDER BY state<>'confirmed',size LIMIT 1", k.firstSize, end).
			Scan(&a.Size, &a.Digest, &a.State, &height)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return nil, err
		default:
			if height > 0 {
				a.BitcoinHeight = &height
			}
			size := fmt.Sprint(a.Size)
			a.OTSURL, a.NoteURL = "/api/log/anchors/"+size+".ots", "/api/log/checkpoint/note?size="+size
			e.FirstAnchor = &a
		}
		out = append(out, e)
	}
	return out, nil
}
