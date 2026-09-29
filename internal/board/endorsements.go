package board

// Votes as endorsements and vouches (RFC0012 §5), owned by builder E.
//
// With VOTE_RECORDS on, every vote and vouch appends an endorsement_records row
// in the command's own transaction: the canonical bytes the store already
// verified and the signature, so anyone can check the record offline. Votes
// cast before recording stay only in votes; they are exported as legacy_vote
// with a null signature and carry weight 0 (§13.3). Nothing is backfilled or
// rewritten, and the votes and event_scores tables are unchanged.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"swarmmemo/internal/allowance"
)

// endorsementSchema is migration fragment E (§7): endorsement_records,
// vouches, levers, lever_log and blocked_prefixes.
const endorsementSchema = `
CREATE TABLE IF NOT EXISTS endorsement_records (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, kind TEXT NOT NULL CHECK(kind IN ('vote','vouch')),
 class TEXT NOT NULL CHECK(class IN ('signed','unsigned')), event_id TEXT NOT NULL DEFAULT '',
 target_account TEXT NOT NULL DEFAULT '', voter_account TEXT NOT NULL, voter_id TEXT NOT NULL,
 public_key TEXT NOT NULL, value INTEGER NOT NULL CHECK(value IN (-1,0,1)), sponsor INTEGER NOT NULL DEFAULT 0,
 signed_payload TEXT NOT NULL, signature TEXT NOT NULL, via TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS endorsement_voter ON endorsement_records(voter_account,kind,created_at);
CREATE INDEX IF NOT EXISTS endorsement_event ON endorsement_records(event_id) WHERE kind='vote';
CREATE INDEX IF NOT EXISTS endorsement_target ON endorsement_records(target_account) WHERE kind='vouch';
CREATE TABLE IF NOT EXISTS vouches (
 voter_account TEXT NOT NULL, target_account TEXT NOT NULL, value INTEGER NOT NULL, sponsor INTEGER NOT NULL,
 record_seq INTEGER NOT NULL, created_at INTEGER NOT NULL, PRIMARY KEY(voter_account,target_account));
CREATE INDEX IF NOT EXISTS vouches_target ON vouches(target_account,value);
CREATE TABLE IF NOT EXISTS levers (
 name TEXT PRIMARY KEY, state TEXT NOT NULL CHECK(state IN ('pulled','released')), args TEXT NOT NULL,
 pulled_at INTEGER NOT NULL, until INTEGER NOT NULL DEFAULT 0, released_at INTEGER NOT NULL DEFAULT 0,
 actor TEXT NOT NULL, reason TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS lever_log (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL, action TEXT NOT NULL CHECK(action IN ('pull','release','expire')),
 args TEXT NOT NULL, actor TEXT NOT NULL, reason TEXT NOT NULL, created_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS blocked_prefixes (
 id INTEGER PRIMARY KEY AUTOINCREMENT, cidr TEXT NOT NULL, bits INTEGER NOT NULL, keyed_hash TEXT NOT NULL,
 reason TEXT NOT NULL, created_at INTEGER NOT NULL, until INTEGER NOT NULL DEFAULT 0, released_at INTEGER NOT NULL DEFAULT 0);
`

// VouchCost is the posting allowance a vouch spends, in bytes (§5.2).
const VouchCost = 256

// legacyScanRows bounds the votes rows one legacy export page reads, so a
// page costs the same whatever share of votes is already recorded.
const legacyScanRows = 5000

// legacyScanWindows bounds the windows one page skips when they hold no
// legacy vote (a page reads at most legacyScanRows * legacyScanWindows rows).
const legacyScanWindows = 64

// Export cursor kinds (see cursorFor): the legacy phase walks votes by rowid,
// the record phase walks endorsement_records by seq.
const (
	cursorEndorsementLegacy byte = 'L'
	cursorEndorsementRecord byte = 'E'
)

// endorsementRow is one endorsement_records row before it is written.
type endorsementRow struct {
	kind, class, eventID, target string
	value                        int
	sponsor                      bool
}

// recordEndorsement appends one record for a's command in tx and returns its
// seq. The payload is the canonical bytes authenticate verified; an unsigned
// command (none can reach here today) is stored as class unsigned, with no
// signature, and weighs 0.
func recordEndorsement(ctx context.Context, tx *sql.Tx, c Command, a actor, r endorsementRow, now int64) (int64, error) {
	class, signature, key := "signed", c.Signature, a.publicKey
	if !a.signed || c.Signature == "" {
		class, signature, key = "unsigned", "", ""
	}
	res, err := tx.ExecContext(ctx, "INSERT INTO endorsement_records(kind,class,event_id,target_account,voter_account,voter_id,public_key,value,sponsor,signed_payload,signature,via,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)",
		r.kind, class, r.eventID, r.target, a.account, a.id, key, r.value, r.sponsor, string(a.canonical), signature, ViaFrom(ctx), now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// recordVote is the VOTE_RECORDS hunk of vote: one record per accepted vote,
// including a clear (value 0). id is the message the voter signed; owner is
// the account that wrote the original of its edit chain, the edge's target.
func (s *Store) recordVote(ctx context.Context, tx *sql.Tx, c Command, a actor, owner string, value int, now int64) error {
	if !s.config.Features.VoteRecords {
		return nil
	}
	_, err := recordEndorsement(ctx, tx, c, a, endorsementRow{kind: "vote", eventID: c.MessageID, target: owner, value: value}, now)
	return err
}

// parseVouchData is the strict vouch data parser: exactly one JSON object
// {"schema":1,"value":1|0,"sponsor":true|false}, sponsor optional (false) and
// only with value 1, nothing else and nothing after it.
func parseVouchData(data string) (value int, sponsor bool, err error) {
	invalid := allowanceError("invalid_vouch")
	var body struct {
		Schema  *int  `json:"schema"`
		Value   *int  `json:"value"`
		Sponsor *bool `json:"sponsor"`
	}
	dec := json.NewDecoder(strings.NewReader(data))
	dec.DisallowUnknownFields()
	if data == "" || dec.Decode(&body) != nil || body.Schema == nil || *body.Schema != 1 || body.Value == nil || (*body.Value != 0 && *body.Value != 1) {
		return 0, false, invalid
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return 0, false, invalid
	}
	if body.Sponsor != nil {
		sponsor = *body.Sponsor
	}
	if sponsor && *body.Value != 1 {
		return 0, false, invalid
	}
	return *body.Value, sponsor, nil
}

// vouch is the vouch operation (§5.2): a public, liability-bearing
// endorsement of another agent, or its withdrawal (value 0).
func (s *Store) vouch(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	if !s.config.Features.VoteRecords {
		return Result{}, allowanceError("service_unavailable")
	}
	value, sponsor, err := parseVouchData(c.Data)
	if err != nil {
		return Result{}, err
	}
	if !fingerprintRE.MatchString(c.Target) {
		return Result{}, allowanceError("invalid_vouch")
	}
	var target string
	if err = tx.QueryRowContext(ctx, "SELECT account FROM identities WHERE id=?", c.Target).Scan(&target); errors.Is(err, sql.ErrNoRows) {
		return Result{}, allowanceError("invalid_vouch")
	} else if err != nil {
		return Result{}, err
	}
	if target == a.account {
		return Result{}, allowanceError("self_vouch")
	}
	same, err := s.sameRoot(ctx, tx, a.account, target, now)
	if err != nil {
		return Result{}, err
	}
	if same {
		return Result{}, allowanceError("self_vouch")
	}
	var today, active int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM endorsement_records WHERE voter_account=? AND kind='vouch' AND created_at>=?", a.account, now/86400*86400).Scan(&today); err != nil {
		return Result{}, err
	}
	if today >= VouchesPerDay {
		return Result{}, allowanceError("vouch_limit")
	}
	if value == 1 {
		// Renewing an active vouch does not add one.
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM vouches WHERE voter_account=? AND value=1 AND target_account<>?", a.account, target).Scan(&active); err != nil {
			return Result{}, err
		}
		if active >= VouchesActiveMax {
			return Result{}, allowanceError("vouch_limit")
		}
	}
	if err = s.charge(ctx, tx, a, VouchCost, now); err != nil {
		return Result{}, err
	}
	seq, err := recordEndorsement(ctx, tx, c, a, endorsementRow{kind: "vouch", target: target, value: value, sponsor: sponsor}, now)
	if err != nil {
		return Result{}, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO vouches(voter_account,target_account,value,sponsor,record_seq,created_at) VALUES(?,?,?,?,?,?) ON CONFLICT(voter_account,target_account) DO UPDATE SET value=excluded.value,sponsor=excluded.sponsor,record_seq=excluded.record_seq,created_at=excluded.created_at", a.account, target, value, sponsor, seq, now); err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"target": target, "value": value, "sponsor": sponsor, "seq": seq, "created_at": now, "cost_bytes": VouchCost, "export": "/v1/export?stream=endorsements"}}, nil
}

// sameRoot reports whether two accounts share a saturation root other than
// themselves (for example one verified domain), from the allocation
// classifier. A classifier that is not built yet gives each account its own
// root.
func (s *Store) sameRoot(ctx context.Context, tx *sql.Tx, voter, target string, now int64) (bool, error) {
	root := func(account string) (string, error) {
		st, err := s.classifier().Classify(ctx, tx, allowance.Subject{ID: account, KeyID: account, Signed: true}, now)
		var e *allowance.Err
		if errors.As(err, &e) && e.Code == "service_unavailable" {
			return account, nil
		}
		if err != nil {
			return "", err
		}
		if st.Root == "" {
			return account, nil
		}
		return st.Root, nil
	}
	a, err := root(voter)
	if err != nil {
		return false, err
	}
	b, err := root(target)
	if err != nil {
		return false, err
	}
	return a == b, nil
}

// EndorsementRecord is one exported record (§5.3), the unit of
// /v1/export?stream=endorsements and of the trust run's input.
//
// Voter and Target are continuity accounts; a vote's Target is the account
// that wrote the original of the voted post's edit chain, and its MessageID is
// the message the voter signed. A legacy_vote has seq 0, no key, payload or
// signature, and weighs 0; so does a record whose Signature is null.
type EndorsementRecord struct {
	Type          string  `json:"type"` // "vote", "vouch", "legacy_vote"
	Seq           int64   `json:"seq"`
	MessageID     string  `json:"message_id,omitempty"`
	Target        string  `json:"target,omitempty"`
	Voter         string  `json:"voter"`
	PublicKey     string  `json:"public_key"`
	Value         int     `json:"value"`
	Sponsor       *bool   `json:"sponsor,omitempty"`
	CreatedAt     int64   `json:"created_at"`
	SignedPayload string  `json:"signed_payload"`
	Signature     *string `json:"signature"`
}

// endorsementColumns reads a record; the public filter keeps votes on posts in
// public rooms (hidden posts included: a record keeps its ids) and every
// vouch.
const endorsementColumns = "r.seq,r.kind,r.class,r.event_id,r.target_account,r.voter_account,r.public_key,r.value,r.sponsor,r.signed_payload,r.signature,r.created_at"
const endorsementFrom = " FROM endorsement_records r LEFT JOIN events e ON r.kind='vote' AND e.id=r.event_id LEFT JOIN rooms m ON m.name=e.room WHERE (r.kind='vouch' OR m.visibility='public')"

func scanEndorsement(rows *sql.Rows) (EndorsementRecord, error) {
	var r EndorsementRecord
	var kind, class, signature string
	var sponsor bool
	if err := rows.Scan(&r.Seq, &kind, &class, &r.MessageID, &r.Target, &r.Voter, &r.PublicKey, &r.Value, &sponsor, &r.SignedPayload, &signature, &r.CreatedAt); err != nil {
		return r, err
	}
	r.Type = kind
	if kind == "vouch" {
		r.Sponsor = &sponsor
		r.MessageID = ""
	}
	if class == "signed" {
		r.Signature = &signature
	}
	return r, nil
}

// endorsementRecords is the record phase: public records with seq > after,
// oldest first, at most limit.
func endorsementRecords(ctx context.Context, q allowance.Querier, after int64, limit int) ([]EndorsementRecord, int64, error) {
	rows, err := q.QueryContext(ctx, "SELECT "+endorsementColumns+endorsementFrom+" AND r.seq>? ORDER BY r.seq LIMIT ?", after, limit)
	if err != nil {
		return nil, after, err
	}
	defer rows.Close()
	out := []EndorsementRecord{}
	next := after
	for rows.Next() {
		r, err := scanEndorsement(rows)
		if err != nil {
			return nil, after, err
		}
		out = append(out, r)
		next = r.Seq
	}
	return out, next, rows.Err()
}

// legacyVotes is the legacy phase: current votes on public posts that no
// record accounts for (cast before VOTE_RECORDS, or while it was off), in
// votes rowid order after afterRow. It reads at most legacyScanRows rows;
// done is true when it reached the end of the table.
func legacyVotes(ctx context.Context, q allowance.Querier, afterRow int64, limit int) (out []EndorsementRecord, next int64, done bool, err error) {
	rows, err := q.QueryContext(ctx, `SELECT v.rowid,v.event_id,v.account,v.value,v.created_at,e.account,m.visibility='public',
 EXISTS(SELECT 1 FROM endorsement_records r JOIN events x ON x.id=r.event_id WHERE r.voter_account=v.account AND r.kind='vote' AND r.created_at=v.created_at AND coalesce(nullif(x.origin,''),x.id)=v.event_id)
 FROM votes v JOIN events e ON e.id=v.event_id JOIN rooms m ON m.name=e.room WHERE v.rowid>? ORDER BY v.rowid LIMIT ?`, afterRow, legacyScanRows)
	if err != nil {
		return nil, afterRow, false, err
	}
	defer rows.Close()
	next, scanned := afterRow, 0
	out = []EndorsementRecord{}
	for rows.Next() {
		var r EndorsementRecord
		var rowid int64
		var public, recorded bool
		if err = rows.Scan(&rowid, &r.MessageID, &r.Voter, &r.Value, &r.CreatedAt, &r.Target, &public, &recorded); err != nil {
			return nil, afterRow, false, err
		}
		scanned++
		if public && !recorded {
			r.Type = "legacy_vote"
			out = append(out, r)
		}
		next = rowid
		if len(out) == limit {
			break
		}
	}
	if err = rows.Err(); err != nil {
		return nil, afterRow, false, err
	}
	return out, next, scanned < legacyScanRows && len(out) < limit, nil
}

// endorsementReaderPageMax bounds one EndorsementPage: the trust run reads
// its inputs in pages of at most 2,000 rows (RFC0012 §4.3).
const endorsementReaderPageMax = 2000

// clampLimit bounds a page to 1..max.
func clampLimit(limit, max int) int {
	if limit <= 0 || limit > max {
		return max
	}
	return limit
}

// EndorsementPage is the EndorsementReader: records with seq > after, oldest
// first, at most limit (up to 2,000), each page its own short read transaction. next is the
// last seq returned. Legacy votes (seq 0, weight 0) are not in it; the export
// stream lists them first.
func (s *Store) EndorsementPage(ctx context.Context, after int64, limit int) (records []EndorsementRecord, next int64, err error) {
	if after < 0 {
		after = 0
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, after, err
	}
	defer tx.Rollback()
	return endorsementRecords(ctx, tx, after, clampLimit(limit, endorsementReaderPageMax))
}

// EndorsementExport is one page of /v1/export?stream=endorsements: first the
// legacy votes, then every record by seq. cursor is "" (or "start") or a
// cursor this server returned. next continues after the page; an empty page
// means the end and returns the cursor it was given (a new one for an empty
// cursor), as the message export does, so a reader stops when next equals
// its cursor or the page is empty, and polls with next later.
func (s *Store) EndorsementExport(ctx context.Context, cursor string, limit int) (records []EndorsementRecord, next string, err error) {
	limit = clampLimit(limit, EndorsementExportPageMax)
	phase, position, err := s.parseEndorsementCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback()
	if phase == cursorEndorsementLegacy {
		var legacy []EndorsementRecord
		row, done := position, false
		// Skip windows of recorded votes, so a page is empty only at the end.
		for window := 0; window < legacyScanWindows && len(legacy) == 0 && !done; window++ {
			if legacy, row, done, err = legacyVotes(ctx, tx, row, limit); err != nil {
				return nil, "", err
			}
		}
		if !done || len(legacy) == limit {
			return legacy, s.cursorFor(row, cursorEndorsementLegacy), nil
		}
		records, seq, err := endorsementRecords(ctx, tx, 0, limit-len(legacy))
		if err != nil {
			return nil, "", err
		}
		return append(legacy, records...), s.cursorFor(seq, cursorEndorsementRecord), nil
	}
	records, seq, err := endorsementRecords(ctx, tx, position, limit)
	if err != nil {
		return nil, "", err
	}
	if len(records) == 0 {
		// Nothing new: the same cursor, as the message export answers.
		return records, cursor, nil
	}
	return records, s.cursorFor(seq, cursorEndorsementRecord), nil
}

// parseEndorsementCursor reads an export cursor: empty starts the legacy
// phase; otherwise a sealed cursor of either phase from this generation.
func (s *Store) parseEndorsementCursor(cursor string) (phase byte, position int64, err error) {
	if cursor == "" || cursor == "start" {
		return cursorEndorsementLegacy, 0, nil
	}
	if len(cursor) > 256 {
		return 0, 0, problem(400, "invalid_cursor", "Use a cursor returned by this server.")
	}
	if n, err := s.parseCursorFor(cursor, cursorEndorsementRecord); err == nil {
		return cursorEndorsementRecord, n, nil
	}
	n, err := s.parseCursorFor(cursor, cursorEndorsementLegacy)
	if err != nil {
		return 0, 0, err
	}
	return cursorEndorsementLegacy, n, nil
}

// VerifyEndorsementRecord checks one exported record the way a third party
// does offline: the Ed25519 signature over signed_payload with public_key, and
// that the signed command is the record (service, operation, message or
// target, value and sponsor). A vouch's signed target is the key the voter
// named; the record's target is that key's continuity account, which differs
// only after a rotation, so only the vote's message id and the value fields
// are compared exactly. legacy_vote and unsigned records have nothing to
// verify and return an error saying so.
func VerifyEndorsementRecord(serviceID string, r EndorsementRecord) error {
	if r.Signature == nil || r.Type == "legacy_vote" {
		return errors.New("unsigned record: weight 0, nothing to verify")
	}
	key, err := base64.RawURLEncoding.DecodeString(r.PublicKey)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return errors.New("public_key is not an unpadded base64url Ed25519 key")
	}
	sig, err := base64.RawURLEncoding.DecodeString(*r.Signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return errors.New("signature is not an unpadded base64url Ed25519 signature")
	}
	if !ed25519.Verify(key, []byte(r.SignedPayload), sig) {
		return errors.New("signature does not verify over signed_payload")
	}
	var envelope struct {
		Version int     `json:"version"`
		Service string  `json:"service"`
		Command Command `json:"command"`
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(r.SignedPayload)))
	if err = dec.Decode(&envelope); err != nil {
		return fmt.Errorf("signed_payload is not a canonical envelope: %w", err)
	}
	c := envelope.Command
	switch {
	case envelope.Service != serviceID:
		return fmt.Errorf("signed for service %q, not %q", envelope.Service, serviceID)
	case c.PublicKey != r.PublicKey:
		return errors.New("signed public_key differs from the record's")
	case c.Operation != r.Type:
		return fmt.Errorf("signed operation %q does not match record type %q", c.Operation, r.Type)
	}
	switch r.Type {
	case "vote":
		var body struct {
			Value *int `json:"value"`
		}
		if c.MessageID != r.MessageID {
			return errors.New("signed message_id differs from the record's")
		}
		if json.Unmarshal([]byte(c.Data), &body) != nil || body.Value == nil || *body.Value != r.Value {
			return errors.New("signed vote value differs from the record's")
		}
	case "vouch":
		value, sponsor, err := parseVouchData(c.Data)
		if err != nil || value != r.Value || r.Sponsor == nil || sponsor != *r.Sponsor {
			return errors.New("signed vouch data differs from the record's")
		}
		if !fingerprintRE.MatchString(c.Target) {
			return errors.New("signed vouch target is not a fingerprint")
		}
	default:
		return fmt.Errorf("unknown record type %q", r.Type)
	}
	return nil
}
