package board

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

// PrivateReadContext is a distinct v3 authority domain, never a public
// delegation or a hint to retry with ordinary identity privileges.
type PrivateReadContext struct {
	Schema     int    `json:"schema"`
	GrantID    string `json:"grant_id"`
	Generation string `json:"generation"`
}

func validPrivateReadContext(c *PrivateReadContext) bool {
	return c != nil && c.Schema == 1 && fingerprintRE.MatchString(c.GrantID) && workIDRE.MatchString(c.Generation)
}
func (c *PrivateReadContext) UnmarshalJSON(raw []byte) error {
	values, err := strictDelegationObject(raw, "schema", "grant_id", "generation")
	var parsed PrivateReadContext
	if err != nil || json.Unmarshal(values["schema"], &parsed.Schema) != nil || json.Unmarshal(values["grant_id"], &parsed.GrantID) != nil || json.Unmarshal(values["generation"], &parsed.Generation) != nil || !validPrivateReadContext(&parsed) {
		return privateReadError("invalid_private_read_context")
	}
	*c = parsed
	return nil
}
func privateReadError(code string) error {
	status, message := 404, "Resource not found."
	switch code {
	case "invalid_private_read_context", "invalid_private_read_data":
		status, message = 400, "Use the exact supported private read schema and fields."
	case "invalid_private_read_proof":
		status, message = 401, "The child must prove possession over the exact enrollment command."
	case "invalid_limit", "invalid_ttl":
		status, message = 400, "The explicit request bound is outside the supported range."
	case "private_read_exists", "private_read_limit", "private_read_generation_mismatch", "private_read_epoch_mismatch", "private_read_already_revoked":
		status, message = 409, "The private read control conflicts with authoritative state."
	case "private_read_rate_limited":
		return &Error{Status: 429, Code: code, Message: "Private read admission is temporarily unavailable.", RetryAfter: 1}
	case "private_read_response_limit":
		status, message = 503, "The complete private read response exceeds its supported bound."
	}
	return problem(status, code, message)
}
func privateReadControl(op string) bool {
	return op == "private_read.create" || op == "private_read.revoke" || op == "private_read.get" || op == "private_read.list"
}
func validatePrivateReadFields(c Command) error {
	fields := "operation public_key signature timestamp nonce room"
	if c.PrivateRead != nil {
		if c.Delegation != nil {
			return privateReadError("invalid_private_read_context")
		}
		fields += " private_read"
		switch c.Operation {
		case "room.get":
		case "messages.list":
			fields += " cursor limit"
		case "message.get":
			fields += " message_id"
		default:
			return privateReadError("not_found")
		}
	} else {
		switch c.Operation {
		case "private_read.create":
			fields += " target proof data ttl request_id"
		case "private_read.revoke":
			fields += " target data request_id"
		case "private_read.get":
			fields += " target"
		case "private_read.list":
			fields += " cursor limit"
		default:
			return privateReadError("invalid_private_read_data")
		}
	}
	allowed := map[string]bool{}
	for _, field := range strings.Fields(fields) {
		allowed[field] = true
	}
	raw, _ := json.Marshal(c)
	var present map[string]json.RawMessage
	_ = json.Unmarshal(raw, &present)
	for field := range present {
		if !allowed[field] {
			return privateReadError("invalid_private_read_data")
		}
	}
	if !slug.MatchString(c.Room) || len(c.Cursor) > 4096 {
		return privateReadError("invalid_private_read_data")
	}
	return nil
}

const privateReadSchema = `
CREATE TABLE IF NOT EXISTS private_read_grants (
 child_id TEXT PRIMARY KEY, public_key TEXT UNIQUE NOT NULL, owner_account TEXT NOT NULL,
 issuer_id TEXT NOT NULL REFERENCES identities(id), issuer_key TEXT NOT NULL,
 room TEXT NOT NULL REFERENCES rooms(name), generation TEXT NOT NULL, access_epoch TEXT NOT NULL,
 created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, revoked_at INTEGER NOT NULL DEFAULT 0,
 payload TEXT NOT NULL, signature TEXT NOT NULL, proof TEXT NOT NULL,
 revoke_payload TEXT NOT NULL DEFAULT '', revoke_signature TEXT NOT NULL DEFAULT '', revoke_generation TEXT NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS private_read_owner ON private_read_grants(owner_account, child_id);
CREATE INDEX IF NOT EXISTS private_read_room ON private_read_grants(room, child_id);
`

func migratePrivateRead(tx *sql.Tx) error {
	var exists int
	if err := tx.QueryRow("SELECT count(*) FROM pragma_table_info('rooms') WHERE name='private_access_epoch'").Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		if _, err := tx.Exec("ALTER TABLE rooms ADD COLUMN private_access_epoch TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
		rows, err := tx.Query("SELECT name FROM rooms WHERE visibility='private'")
		if err != nil {
			return err
		}
		var names []string
		for rows.Next() {
			var name string
			if err = rows.Scan(&name); err != nil {
				rows.Close()
				return err
			}
			names = append(names, name)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, name := range names {
			if _, err = tx.Exec("UPDATE rooms SET private_access_epoch=? WHERE name=?", randomID(), name); err != nil {
				return err
			}
		}
	}
	_, err := tx.Exec(privateReadSchema)
	return err
}

type privateReadRow struct {
	ID, PublicKey, Owner, IssuerID, IssuerKey, Room, Generation, Epoch          string
	Created, Expires, Revoked                                                   int64
	Payload, Signature, Proof, RevokePayload, RevokeSignature, RevokeGeneration string
}

const privateReadColumns = `child_id,public_key,owner_account,issuer_id,issuer_key,room,generation,access_epoch,created_at,expires_at,revoked_at,payload,signature,proof,revoke_payload,revoke_signature,revoke_generation`

func scanPrivateRead(row scanner) (privateReadRow, error) {
	var g privateReadRow
	err := row.Scan(&g.ID, &g.PublicKey, &g.Owner, &g.IssuerID, &g.IssuerKey, &g.Room, &g.Generation, &g.Epoch, &g.Created, &g.Expires, &g.Revoked, &g.Payload, &g.Signature, &g.Proof, &g.RevokePayload, &g.RevokeSignature, &g.RevokeGeneration)
	return g, err
}

// Classify the actual signer before any generic identity/delegation/cache path.
func resolvePrivateRead(ctx context.Context, tx *sql.Tx, c Command, a actor) (*privateReadRow, error) {
	if !a.signed {
		if c.PrivateRead != nil || privateReadControl(c.Operation) {
			return nil, privateReadError("not_found")
		}
		return nil, nil
	}
	g, err := scanPrivateRead(tx.QueryRowContext(ctx, "SELECT "+privateReadColumns+" FROM private_read_grants WHERE child_id=?", a.id))
	if err == nil {
		if c.PrivateRead == nil || c.PrivateRead.GrantID != a.id || c.PrivateRead.Generation != g.Generation || a.publicKey != g.PublicKey {
			return nil, privateReadError("not_found")
		}
		return &g, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if c.PrivateRead != nil {
		return nil, privateReadError("not_found")
	}
	if privateReadControl(c.Operation) {
		var count int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM delegations WHERE child_id=?", a.id).Scan(&count); err != nil {
			return nil, err
		}
		if count != 0 {
			return nil, privateReadError("not_found")
		}
	}
	return nil, nil
}
func privateReadState(ctx context.Context, tx *sql.Tx, g privateReadRow, now int64) (string, error) {
	if g.Revoked != 0 {
		return "revoked", nil
	}
	var generation, epoch, successor, owner, visibility string
	if err := tx.QueryRowContext(ctx, "SELECT value FROM meta WHERE key='generation'").Scan(&generation); err != nil {
		return "", err
	}
	if generation != g.Generation {
		return "epoch_disabled", nil
	}
	if err := tx.QueryRowContext(ctx, "SELECT private_access_epoch,owner,visibility FROM rooms WHERE name=?", g.Room).Scan(&epoch, &owner, &visibility); err != nil {
		return "", err
	}
	if epoch != g.Epoch || owner != g.Owner || visibility != "private" {
		return "room_epoch_disabled", nil
	}
	if err := tx.QueryRowContext(ctx, "SELECT successor FROM identities WHERE id=?", g.IssuerID).Scan(&successor); err != nil {
		return "", err
	}
	if successor != "" {
		return "issuer_rotated", nil
	}
	if now >= g.Expires {
		return "expired", nil
	}
	return "active", nil
}

type PrivateReadAck struct {
	Type                      string `json:"type"`
	Schema                    int    `json:"schema"`
	GrantID                   string `json:"grant_id"`
	ChildID                   string `json:"child_id"`
	Room                      string `json:"room"`
	ServiceID                 string `json:"service_id"`
	Generation                string `json:"generation"`
	GrantGeneration           string `json:"grant_generation"`
	State                     string `json:"state"`
	AcceptedAt                int64  `json:"accepted_at"`
	ExpiresAt                 int64  `json:"expires_at"`
	HistoricalAcknowledgement bool   `json:"historical_acknowledgement"`
}
type PrivateReadSummary struct {
	Type            string `json:"type"`
	Schema          int    `json:"schema"`
	GrantID         string `json:"grant_id"`
	ChildID         string `json:"child_id"`
	ChildPublicKey  string `json:"child_public_key"`
	Room            string `json:"room"`
	IssuerID        string `json:"issuer_id"`
	IssuerPublicKey string `json:"issuer_public_key"`
	Generation      string `json:"generation"`
	AccessEpoch     string `json:"access_epoch"`
	CreatedAt       int64  `json:"created_at"`
	ExpiresAt       int64  `json:"expires_at"`
	RevokedAt       int64  `json:"revoked_at"`
	State           string `json:"state"`
}
type PrivateReadEnrollment struct {
	SignedPayload string `json:"signed_payload"`
	Signature     string `json:"signature"`
	Proof         string `json:"proof"`
}
type PrivateReadRevocation struct {
	SignedPayload string `json:"signed_payload"`
	Signature     string `json:"signature"`
	Generation    string `json:"generation"`
}
type PrivateReadRecord struct {
	PrivateReadSummary
	Enrollment PrivateReadEnrollment `json:"enrollment"`
	Revocation PrivateReadRevocation `json:"revocation"`
}

func privateReadSummary(g privateReadRow, state string) PrivateReadSummary {
	return PrivateReadSummary{Type: "private-room-read-grant", Schema: 1, GrantID: g.ID, ChildID: g.ID, ChildPublicKey: g.PublicKey, Room: g.Room, IssuerID: g.IssuerID, IssuerPublicKey: g.IssuerKey, Generation: g.Generation, AccessEpoch: g.Epoch, CreatedAt: g.Created, ExpiresAt: g.Expires, RevokedAt: g.Revoked, State: state}
}
func privateReadJSON(value any) ([]byte, error) {
	var raw bytes.Buffer
	encoder := json.NewEncoder(&raw)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return raw.Bytes(), nil
}
func privateReadBound(value any, limit int) error {
	raw, err := privateReadJSON(value)
	if err != nil {
		return err
	}
	if len(raw) > limit {
		return privateReadError("private_read_response_limit")
	}
	return nil
}
func privateReadFresh(c Command, now int64) error {
	if c.Timestamp < now-SignatureWindowSeconds || c.Timestamp > now+SignatureWindowSeconds {
		return problem(401, "stale_signature", "New signed commands must be within five minutes of server time.")
	}
	return nil
}
func parsePrivateReadData(raw string, create bool) (string, string, error) {
	fields := []string{"schema", "generation"}
	if create {
		fields = append(fields, "access_epoch", "disclosure")
	}
	values, err := strictDelegationObject([]byte(raw), fields...)
	var schema int
	var generation, epoch, disclosure string
	if err != nil || json.Unmarshal(values["schema"], &schema) != nil || schema != 1 || json.Unmarshal(values["generation"], &generation) != nil || !workIDRE.MatchString(generation) {
		return "", "", privateReadError("invalid_private_read_data")
	}
	if create && (json.Unmarshal(values["access_epoch"], &epoch) != nil || !workIDRE.MatchString(epoch) || json.Unmarshal(values["disclosure"], &disclosure) != nil || disclosure != "private") {
		return "", "", privateReadError("invalid_private_read_data")
	}
	return generation, epoch, nil
}
func (s *Store) privateReadOwner(ctx context.Context, tx *sql.Tx, c Command, a actor, successor string, now int64) (Result, error) {
	if !a.signed {
		return Result{}, privateReadError("not_found")
	}
	creating, revoking := c.Operation == "private_read.create", c.Operation == "private_read.revoke"
	if creating && verifyDelegationProof(c, a.canonical) != nil {
		return Result{}, privateReadError("invalid_private_read_proof")
	}
	if (creating && len(a.canonical)+len(c.Signature)+len(c.Proof) > 4096) || (revoking && len(a.canonical)+len(c.Signature) > 4096) {
		return Result{}, privateReadError("invalid_private_read_data")
	}
	keys := []string{}
	namespace := "private-control:" + a.account
	digestBytes := sha256.Sum256(a.canonical)
	digest := hex.EncodeToString(digestBytes[:])
	if creating || revoking {
		if c.RequestID != "" {
			keys = append(keys, "id:"+c.RequestID)
		}
		keys = append(keys, "nonce:"+c.Nonce)
		for _, key := range keys {
			var savedDigest, saved string
			err := tx.QueryRowContext(ctx, "SELECT digest,result FROM requests WHERE actor=? AND request_key=?", namespace, key).Scan(&savedDigest, &saved)
			if err == nil {
				if savedDigest != digest {
					return Result{}, problem(409, "idempotency_conflict", "This request identifier belongs to a different command.")
				}
				var result Result
				if len(saved) > 1024 || json.Unmarshal([]byte(saved), &result) != nil {
					return Result{}, privateReadError("private_read_response_limit")
				}
				return result, nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return Result{}, err
			}
		}
	}
	if err := privateReadFresh(c, now); err != nil {
		return Result{}, err
	}
	if successor != "" {
		return Result{}, privateReadError("not_found")
	}
	var epoch, current string
	err := tx.QueryRowContext(ctx, "SELECT private_access_epoch FROM rooms WHERE name=? AND owner=? AND visibility='private'", c.Room, a.account).Scan(&epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, privateReadError("not_found")
	}
	if err != nil {
		return Result{}, err
	}
	if err = tx.QueryRowContext(ctx, "SELECT value FROM meta WHERE key='generation'").Scan(&current); err != nil {
		return Result{}, err
	}
	if !creating && !revoking {
		return s.readPrivateControl(ctx, tx, c, a, now, current, epoch)
	}
	generation, wantedEpoch, err := parsePrivateReadData(c.Data, creating)
	if err != nil {
		return Result{}, err
	}
	if generation != current {
		return Result{}, privateReadError("private_read_generation_mismatch")
	}
	var g privateReadRow
	if creating {
		if wantedEpoch != epoch {
			return Result{}, privateReadError("private_read_epoch_mismatch")
		}
		ttl := c.TTL
		if ttl == 0 {
			ttl = 86400
		}
		if ttl < 60 || ttl > 604800 {
			return Result{}, privateReadError("invalid_ttl")
		}
		key, _ := base64.RawURLEncoding.DecodeString(c.Target)
		id := fingerprint(key)
		var existing int
		if err = tx.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM identities WHERE id=?)+(SELECT count(*) FROM delegations WHERE child_id=?)+(SELECT count(*) FROM private_read_grants WHERE child_id=?)", id, id, id).Scan(&existing); err != nil {
			return Result{}, err
		}
		if existing != 0 {
			return Result{}, privateReadError("private_read_exists")
		}
		if err = privateReadCapacity(ctx, tx, a.account, c.Room, current, now); err != nil {
			return Result{}, err
		}
		if err = s.charge(ctx, tx, a, 16384, now); err != nil {
			return Result{}, err
		}
		g = privateReadRow{ID: id, PublicKey: c.Target, Owner: a.account, IssuerID: a.id, IssuerKey: a.publicKey, Room: c.Room, Generation: current, Epoch: epoch, Created: now, Expires: now + ttl, Payload: string(a.canonical), Signature: c.Signature, Proof: c.Proof}
		_, err = tx.ExecContext(ctx, "INSERT INTO private_read_grants(child_id,public_key,owner_account,issuer_id,issuer_key,room,generation,access_epoch,created_at,expires_at,payload,signature,proof) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)", g.ID, g.PublicKey, g.Owner, g.IssuerID, g.IssuerKey, g.Room, g.Generation, g.Epoch, g.Created, g.Expires, g.Payload, g.Signature, g.Proof)
	} else {
		if !fingerprintRE.MatchString(c.Target) {
			return Result{}, privateReadError("not_found")
		}
		g, err = scanPrivateRead(tx.QueryRowContext(ctx, "SELECT "+privateReadColumns+" FROM private_read_grants WHERE child_id=? AND room=? AND owner_account=?", c.Target, c.Room, a.account))
		if errors.Is(err, sql.ErrNoRows) {
			return Result{}, privateReadError("not_found")
		}
		if err != nil {
			return Result{}, err
		}
		if g.Revoked != 0 {
			return Result{}, privateReadError("private_read_already_revoked")
		}
		_, err = tx.ExecContext(ctx, "UPDATE private_read_grants SET revoked_at=?,revoke_payload=?,revoke_signature=?,revoke_generation=? WHERE child_id=?", now, string(a.canonical), c.Signature, current, g.ID)
	}
	if err != nil {
		return Result{}, err
	}
	state := "active"
	if revoking {
		state = "revoked"
	}
	result := Result{OK: true, Data: map[string]any{"ack": PrivateReadAck{Type: "private-room-read-grant-ack", Schema: 1, GrantID: g.ID, ChildID: g.ID, Room: g.Room, ServiceID: s.config.ServiceID, Generation: current, GrantGeneration: g.Generation, State: state, AcceptedAt: now, ExpiresAt: g.Expires, HistoricalAcknowledgement: true}}}
	if err = privateReadBound(result, 1024); err != nil {
		return Result{}, err
	}
	encoded, err := privateReadJSON(result)
	if err != nil {
		return Result{}, err
	}
	for _, key := range keys {
		if _, err = tx.ExecContext(ctx, "INSERT INTO requests(actor,request_key,digest,result,created_at) VALUES(?,?,?,?,?)", namespace, key, digest, string(encoded), now); err != nil {
			return Result{}, err
		}
	}
	return result, nil
}
func privateReadCapacity(ctx context.Context, tx *sql.Tx, owner, room, generation string, now int64) error {
	for _, active := range []bool{false, true} {
		query := "SELECT count(*),coalesce(sum(g.owner_account=?),0),coalesce(sum(g.room=?),0) FROM private_read_grants g"
		args := []any{owner, room}
		global, local := 32768, 4096
		if active {
			global, local = 256, 8
			query += " JOIN rooms r ON r.name=g.room JOIN identities i ON i.id=g.issuer_id WHERE g.revoked_at=0 AND g.expires_at>? AND g.generation=? AND g.access_epoch=r.private_access_epoch AND g.owner_account=r.owner AND r.visibility='private' AND i.successor=''"
			args = append(args, now, generation)
		}
		var total, owned, inRoom int
		if err := tx.QueryRowContext(ctx, query, args...).Scan(&total, &owned, &inRoom); err != nil {
			return err
		}
		if total >= global || owned >= local || inRoom >= local {
			return privateReadError("private_read_limit")
		}
	}
	return nil
}
func (s *Store) readPrivateControl(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64, generation, epoch string) (Result, error) {
	result := Result{OK: true, Data: map[string]any{"service_id": s.config.ServiceID, "generation": generation, "room": c.Room, "access_epoch": epoch}}
	if c.Operation == "private_read.get" {
		if !fingerprintRE.MatchString(c.Target) {
			return Result{}, privateReadError("not_found")
		}
		g, err := scanPrivateRead(tx.QueryRowContext(ctx, "SELECT "+privateReadColumns+" FROM private_read_grants WHERE child_id=? AND room=? AND owner_account=?", c.Target, c.Room, a.account))
		if errors.Is(err, sql.ErrNoRows) {
			return Result{}, privateReadError("not_found")
		}
		if err != nil {
			return Result{}, err
		}
		state, err := privateReadState(ctx, tx, g, now)
		if err != nil {
			return Result{}, err
		}
		result.Data["private_read_grant"] = PrivateReadRecord{PrivateReadSummary: privateReadSummary(g, state), Enrollment: PrivateReadEnrollment{g.Payload, g.Signature, g.Proof}, Revocation: PrivateReadRevocation{g.RevokePayload, g.RevokeSignature, g.RevokeGeneration}}
	} else {
		limit := c.Limit
		if limit == 0 {
			limit = 8
		}
		if limit < 1 || limit > 32 {
			return Result{}, privateReadError("invalid_limit")
		}
		scope := a.account + ":" + c.Room
		cursor, err := s.decodeConversationCursor(c.Cursor, "private_read.list", scope)
		if err != nil {
			return Result{}, err
		}
		rows, err := tx.QueryContext(ctx, "SELECT "+privateReadColumns+" FROM private_read_grants WHERE owner_account=? AND room=? AND child_id>? ORDER BY child_id LIMIT ?", a.account, c.Room, cursor.Page, limit+1)
		if err != nil {
			return Result{}, err
		}
		var stored []privateReadRow
		for rows.Next() {
			g, e := scanPrivateRead(rows)
			if e != nil {
				rows.Close()
				return Result{}, e
			}
			stored = append(stored, g)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return Result{}, err
		}
		more := len(stored) > limit
		if more {
			stored = stored[:limit]
		}
		summaries := []PrivateReadSummary{}
		for _, g := range stored {
			state, err := privateReadState(ctx, tx, g, now)
			if err != nil {
				return Result{}, err
			}
			summaries = append(summaries, privateReadSummary(g, state))
		}
		result.Data["private_read_grants"], result.Data["has_more"] = summaries, more
		if more {
			result.NextCursor = s.encodeConversationCursor(conversationCursor{Domain: "private_read.list", Scope: scope, Page: stored[len(stored)-1].ID})
		}
	}
	if err := privateReadBound(result, 64<<10); err != nil {
		return Result{}, err
	}
	return result, nil
}
