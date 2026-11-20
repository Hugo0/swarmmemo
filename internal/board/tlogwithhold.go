package board

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

// Withheld leaf bodies (C1126 step 1). Identity leaves copy what an account
// supplied (a handle, an identity link's value) verbatim. When the account
// is erased, the bodies of its identity leaves (and of the witness leaves
// about it) are withheld: tlog_leaves.data becomes empty and nothing else
// changes. The leaf hash stays in tlog_hashes, which is all a proof, a
// checkpoint or a Bitcoin anchor reads, so every proof still verifies.
//
// Protocol rule: a leaf's body may be withheld only when a later
// account.erase leaf names the account. tlog_withheld records, for each
// withheld leaf, that leaf's index (erase_idx) and the account; the schema
// refuses a row without that later leaf and an update of tlog_leaves other
// than data to empty of a leaf it names, and the read side serves a body-less
// leaf only with that row (withheldLeaf).

// WithheldErased is a withheld leaf's reason: its account was erased.
const WithheldErased = "erased"

// withheldOps are the identity leaves an erasure withholds: the operations
// in eraseAuditOps that the log takes as identity leaves (their detail is a
// value the account supplied), and identity.witness (a link's value).
var withheldOps = map[string]bool{"identity.link": true, "identity.unlink": true, "agent.register": true, "handle.claim": true, "identity.witness": true}

// withholdCandidatesSQL reads the identity leaves with a body that may be
// about account ?1: those of its keys, and those of the keys that witnessed
// one of its links (a witness leaf names the witnessed key as target).
const withholdCandidatesSQL = `SELECT l.idx,l.subject,l.data FROM tlog_leaves l WHERE l.kind='identity' AND l.data<>''
 AND (l.subject IN ` + eraseOwn + ` OR l.subject IN (SELECT witness FROM link_witnesses WHERE agent IN ` + eraseOwn + `)) ORDER BY l.idx`

// eraseLeafSQL is the index of account ?'s latest account.erase leaf, or -1.
const eraseLeafSQL = `SELECT coalesce(max(idx),-1) FROM tlog_leaves WHERE subject=? AND kind='identity' AND data LIKE '%"op":"account.erase"%'`

// withholdLeaf is an identity leaf with a body that an erasure of its
// account withholds.
type withholdLeaf struct {
	idx int64
	op  string
}

// withholdCandidates are account's identity leaves that still have a body
// and that its erasure withholds: its own handle claims, registrations and
// identity links, and the witness leaves whose witness or witnessed key is
// one of its keys. Through tx only.
func withholdCandidates(ctx context.Context, tx *sql.Tx, account string) ([]withholdLeaf, error) {
	ids, err := eraseIDs(ctx, tx, account)
	if err != nil {
		return nil, err
	}
	own := map[string]bool{}
	for _, id := range ids {
		own[id] = true
	}
	rows, err := tx.QueryContext(ctx, withholdCandidatesSQL, account)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []withholdLeaf
	for rows.Next() {
		var idx int64
		var subject, data string
		if err = rows.Scan(&idx, &subject, &data); err != nil {
			return nil, err
		}
		var l logLeaf
		if json.Unmarshal([]byte(data), &l) != nil || !withheldOps[l.Op] || !own[subject] && !own[l.Target] {
			continue
		}
		list = append(list, withholdLeaf{idx, l.Op})
	}
	return list, rows.Err()
}

// withholdErased withholds the bodies of account's withholdCandidates that
// come before its latest account.erase leaf, through tx only. withheld
// counts the leaves it blanked now; pending those with a body still waiting
// for a later erase leaf (none yet, or appended after the latest). A second
// call blanks nothing.
func withholdErased(ctx context.Context, tx *sql.Tx, account string, now int64) (withheld, pending int64, err error) {
	var erase int64
	if err = tx.QueryRowContext(ctx, eraseLeafSQL, account).Scan(&erase); err != nil {
		return 0, 0, err
	}
	list, err := withholdCandidates(ctx, tx, account)
	if err != nil {
		return 0, 0, err
	}
	for _, l := range list {
		if l.idx >= erase {
			pending++
			continue
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO tlog_withheld(idx,erase_idx,account,op,created_at) VALUES(?,?,?,?,?)", l.idx, erase, account, l.op, now); err != nil {
			return withheld, pending, fmt.Errorf("withhold leaf %d: %w", l.idx, err)
		}
		if _, err = tx.ExecContext(ctx, "UPDATE tlog_leaves SET data='' WHERE idx=?", l.idx); err != nil {
			return withheld, pending, fmt.Errorf("withhold leaf %d: %w", l.idx, err)
		}
		withheld++
	}
	return withheld, pending, nil
}

// withholdAfterErasure appends the account.erase leaf the audit row just
// written in tx asks for (catching the log up), then withholds; every leaf
// it withholds must then be withheld, or the erasure would leave a body
// public that its receipt counts as withheld.
func withholdAfterErasure(ctx context.Context, tx *sql.Tx, account string, now int64) error {
	if err := tlogCatchUpAll(ctx, tx); err != nil {
		return err
	}
	_, pending, err := withholdErased(ctx, tx, account, now)
	if err == nil && pending > 0 {
		err = fmt.Errorf("erasure of %s: %d identity leaves have no account.erase leaf after them", account, pending)
	}
	return err
}

// tlogCatchUpAll runs tlogCatchUp through tx until every outbox row is
// read, so the leaves about an account are all in the log.
func tlogCatchUpAll(ctx context.Context, tx *sql.Tx) error {
	for {
		n, err := tlogCatchUp(ctx, tx)
		if err != nil || n == 0 {
			return err
		}
	}
}

// withheldLeaf is the later account.erase leaf that lets leaf idx be served
// without its body: its tlog_withheld row, whose erase leaf is after idx and
// names the row's account. A body-less leaf without one breaks the protocol
// rule, so it is an error, never served.
func withheldLeaf(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, idx int64) (int64, error) {
	var erase int64
	err := q.QueryRowContext(ctx, `SELECT w.erase_idx FROM tlog_withheld w JOIN tlog_leaves e ON e.idx=w.erase_idx
 WHERE w.idx=? AND w.erase_idx>w.idx AND e.kind='identity' AND e.subject=w.account AND e.data LIKE '%"op":"account.erase"%'`, idx).Scan(&erase)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("transparency log leaf %d has no body and no later account.erase leaf", idx)
	}
	return erase, err
}

// servedLeaf is leaf index as served: its data and the hash of it, or, for a
// withheld leaf, no data, the stored leaf hash and the erase leaf after it.
func servedLeaf(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, index int64, kind, data string) (LogEntry, error) {
	if data != "" {
		return newLogEntry(index, kind, data), nil
	}
	erase, err := withheldLeaf(ctx, q, index)
	if err != nil {
		return LogEntry{}, err
	}
	var hash []byte
	if err = q.QueryRowContext(ctx, "SELECT hash FROM tlog_hashes WHERE level=0 AND idx=?", index).Scan(&hash); err != nil {
		return LogEntry{}, fmt.Errorf("transparency log hash 0/%d: %w", index, err)
	}
	return LogEntry{Index: index, Kind: kind, Hash: base64.StdEncoding.EncodeToString(hash), Withheld: WithheldErased, ErasedBy: erase}, nil
}

// MarshalJSON writes a withheld leaf with "data":null, its withheld reason
// and erased_by; every other leaf as before (data as stored, no HTML
// escaping of its own: the outer encoder decides, as for any field).
func (e LogEntry) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	var err error
	if e.Withheld == "" {
		type plain struct {
			Index int64  `json:"index"`
			Kind  string `json:"kind"`
			Data  string `json:"data"`
			Hash  string `json:"leaf_hash"`
		}
		err = enc.Encode(plain{e.Index, e.Kind, e.Data, e.Hash})
	} else {
		type withheld struct {
			Index    int64   `json:"index"`
			Kind     string  `json:"kind"`
			Data     *string `json:"data"`
			Hash     string  `json:"leaf_hash"`
			Withheld string  `json:"withheld"`
			ErasedBy int64   `json:"erased_by"`
		}
		err = enc.Encode(withheld{e.Index, e.Kind, nil, e.Hash, e.Withheld, e.ErasedBy})
	}
	return bytes.TrimSuffix(b.Bytes(), []byte{'\n'}), err
}
