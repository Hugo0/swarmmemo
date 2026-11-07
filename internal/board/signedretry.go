package board

// Signed post retries under a new request_id (C789). A client that retries a
// signed post after a lost answer, but signs the retry with a fresh
// request_id and nonce, is not a byte-identical replay, so the requests table
// never finds it and the post would appear twice (kosoyy, #reviews, 32 s
// apart). A signed public post whose command, apart from request_id, nonce,
// timestamp and signature, equals one the same key posted within
// SignedRetrySeconds is answered exactly as a replay of that earlier post:
// its stored receipt with duplicate: true, nothing published or charged.
// A different room, reply_to, kind, text, data or attachments is a new post,
// and so is the same post once the window has passed. A hidden earlier post
// still answers, and stays hidden.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// SignedRetrySeconds is how long an identical signed public post answers as
// the same key's earlier one.
const SignedRetrySeconds = 120

// signedRetryScan bounds the account's newest events the lookup reads
// (events_author), so it never scans an account's history.
const signedRetryScan = 32

const signedRetryQuery = `SELECT e.id,e.seq,e.hash,e.created_at,e.payload,e.room FROM
 (SELECT id,seq,hash,created_at,payload,room,author,text,reply_to,recipient FROM events WHERE account=? ORDER BY seq DESC LIMIT ?) e
 JOIN rooms r ON r.name=e.room
 WHERE e.author=? AND e.created_at>=? AND e.text=? AND e.reply_to=? AND e.recipient='' AND e.payload<>'' AND r.visibility='public'
 ORDER BY e.seq DESC`

// retryBody is a command's bytes without what a fresh signature changes.
func (s *Store) retryBody(c Command) []byte {
	c.RequestID, c.Nonce, c.Timestamp = "", "", 0
	return Canonical(s.config.ServiceID, c)
}

// signedPostRetry finds the same key's identical public post from the last
// SignedRetrySeconds and returns the answer a replay of it gets. It reads
// through tx only; the caller returns without committing, like a replay.
func (s *Store) signedPostRetry(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, bool, error) {
	if !a.signed || c.Operation != "post" || c.To != "" || a.account == "" {
		return Result{}, false, nil
	}
	rows, err := tx.QueryContext(ctx, signedRetryQuery, a.account, signedRetryScan, a.id, now-SignedRetrySeconds, c.Text, c.ReplyTo)
	if err != nil {
		return Result{}, false, err
	}
	want := s.retryBody(c)
	var match struct {
		id, hash, nonce string
		seq, at         int64
	}
	for rows.Next() {
		var id, hash, payload, room string
		var seq, at int64
		if err = rows.Scan(&id, &seq, &hash, &at, &payload, &room); err != nil {
			rows.Close()
			return Result{}, false, err
		}
		if IsConversationRoom(room) {
			continue
		}
		var stored struct {
			Command Command `json:"command"`
		}
		if json.Unmarshal([]byte(payload), &stored) != nil {
			continue
		}
		if string(s.retryBody(stored.Command)) == string(want) {
			match.id, match.hash, match.nonce, match.seq, match.at = id, hash, stored.Command.Nonce, seq, at
			break
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return Result{}, false, err
	}
	if match.id == "" {
		return Result{}, false, nil
	}
	// The earlier post's own stored answer, as its replay returns it.
	var stored string
	err = tx.QueryRowContext(ctx, "SELECT result FROM requests WHERE actor=? AND request_key=?", a.requestNamespace, "nonce:"+match.nonce).Scan(&stored)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Result{}, false, err
	}
	if err == nil {
		var original Result
		if json.Unmarshal([]byte(stored), &original) == nil && original.Receipt != nil && original.Receipt.ID == match.id {
			original.Receipt.Duplicate = true
			return original, true, nil
		}
	}
	return Result{OK: true, Receipt: &Receipt{ID: match.id, Hash: match.hash, Cursor: s.cursor(match.seq), AcceptedAt: match.at, Public: true, Duplicate: true}}, true, nil
}
