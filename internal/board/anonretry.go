package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// AnonCrossNetworkRetrySeconds is how long an anonymous public post's exact
// retry (same request_id, identical canonical bytes) is recognised from
// another network: an edge transport that egresses from several addresses
// can deliver one intended post from two networks seconds apart.
const AnonCrossNetworkRetrySeconds = 10 * 60

// anonRetrySchema indexes anonymous request rows by key, so the cross-network
// lookup below never scans the table. The index is partial: signed and
// delegated rows never take part, and its WHERE terms are repeated verbatim in
// the query so SQLite can use it.
const anonRetrySchema = `
CREATE INDEX IF NOT EXISTS requests_anon_key ON requests(request_key,digest,created_at) WHERE actor >= 'anon:' AND actor < 'anon;';
`

// anonRetryQuery is the oldest matching row from another anonymous network:
// request_key, digest, created_at lower bound, the caller's own namespace.
const anonRetryQuery = `SELECT result FROM requests
 WHERE request_key=? AND digest=? AND created_at>=? AND actor<>?
   AND actor >= 'anon:' AND actor < 'anon;'
 ORDER BY created_at LIMIT 1`

// anonymousCrossNetworkRetry finds an anonymous public post that another
// anonymous network made within AnonCrossNetworkRetrySeconds under the same
// request key with the same digest, and returns its receipt with duplicate:
// true. Called inside the command's transaction after the caller's own
// namespaces found nothing; it reads through tx only.
//
// Why this is safe: the matched command is byte-identical (same digest), so
// the caller already holds everything it says, and the post it made is in a
// public room with no recipient, so its id, hash, cursor and acceptance time
// are on the public record. The answer is rebuilt from those receipt fields
// alone, never from the stored result, so nothing about the first network
// (its allowance, its pseudonym) can travel with it. Anything not plainly
// public (a DM, a private room, a conversation) is skipped and posts as
// before. Signed commands never come here: signing scopes retries to the key.
func (s *Store) anonymousCrossNetworkRetry(ctx context.Context, tx *sql.Tx, c Command, a actor, key, digest string, now int64) (Result, bool, error) {
	if a.signed || a.grant != nil || c.Operation != "post" || c.RequestID == "" || c.To != "" {
		return Result{}, false, nil
	}
	if _, forwarded := forwardedFrom(ctx); forwarded {
		return Result{}, false, nil // a bridged post's subject is its origin key
	}
	var stored string
	err := tx.QueryRowContext(ctx, anonRetryQuery, key, digest, now-AnonCrossNetworkRetrySeconds, a.requestNamespace).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, err
	}
	var original Result
	if err = json.Unmarshal([]byte(stored), &original); err != nil {
		return Result{}, false, err
	}
	if original.Receipt == nil || original.Receipt.ID == "" {
		return Result{}, false, nil
	}
	var room, recipient, visibility string
	err = tx.QueryRowContext(ctx, "SELECT e.room,e.recipient,r.visibility FROM events e JOIN rooms r ON r.name=e.room WHERE e.id=?", original.Receipt.ID).Scan(&room, &recipient, &visibility)
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, err
	}
	if visibility != "public" || recipient != "" || IsConversationRoom(room) {
		return Result{}, false, nil
	}
	r := original.Receipt
	return Result{OK: true, Receipt: &Receipt{ID: r.ID, Hash: r.Hash, Cursor: r.Cursor, AcceptedAt: r.AcceptedAt, Duplicate: true}}, true, nil
}
