package board

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"time"
)

// DelegationContext is an explicit signed authority assertion, never a hint to
// fall back to root admission. Presence selects canonical envelope version 2.
type DelegationContext struct {
	Schema     int    `json:"schema"`
	GrantID    string `json:"grant_id"`
	Generation string `json:"generation"`
}

func strictDelegationObject(raw []byte, names ...string) (map[string]json.RawMessage, error) {
	bad := delegationError("invalid_delegation_data")
	if len(raw) > 8192 {
		return nil, bad
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	token, err := dec.Token()
	if err != nil || token != json.Delim('{') {
		return nil, bad
	}
	allowed := map[string]bool{}
	for _, name := range names {
		allowed[name] = true
	}
	values := map[string]json.RawMessage{}
	for dec.More() {
		token, err = dec.Token()
		name, ok := token.(string)
		if err != nil || !ok || !allowed[name] || values[name] != nil {
			return nil, bad
		}
		var value json.RawMessage
		if dec.Decode(&value) != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, bad
		}
		values[name] = value
	}
	if _, err = dec.Token(); err != nil {
		return nil, bad
	}
	if _, err = dec.Token(); err != io.EOF || len(values) != len(names) {
		return nil, bad
	}
	return values, nil
}
func (c *DelegationContext) UnmarshalJSON(raw []byte) error {
	values, err := strictDelegationObject(raw, "schema", "grant_id", "generation")
	if err != nil {
		return delegationError("invalid_delegation_context")
	}
	var parsed DelegationContext
	if json.Unmarshal(values["schema"], &parsed.Schema) != nil || json.Unmarshal(values["grant_id"], &parsed.GrantID) != nil || json.Unmarshal(values["generation"], &parsed.Generation) != nil || !validDelegationContext(&parsed) {
		return delegationError("invalid_delegation_context")
	}
	*c = parsed
	return nil
}
func validDelegationContext(c *DelegationContext) bool {
	return c.Schema == 1 && fingerprintRE.MatchString(c.GrantID) && workIDRE.MatchString(c.Generation)
}

const delegationSchema = `
CREATE TABLE IF NOT EXISTS delegations (
 child_id TEXT PRIMARY KEY,public_key TEXT UNIQUE NOT NULL,parent_account TEXT NOT NULL,
 issuer_id TEXT NOT NULL REFERENCES identities(id),issuer_key TEXT NOT NULL,
 room TEXT NOT NULL REFERENCES rooms(name),operations TEXT NOT NULL,generation TEXT NOT NULL,
 created_at INTEGER NOT NULL,expires_at INTEGER NOT NULL,ceiling_bytes INTEGER NOT NULL,
 used_bytes INTEGER NOT NULL DEFAULT 0,revoked_at INTEGER NOT NULL DEFAULT 0,
 payload TEXT NOT NULL,signature TEXT NOT NULL,proof TEXT NOT NULL,
 revoke_payload TEXT NOT NULL DEFAULT '',revoke_signature TEXT NOT NULL DEFAULT '',
 revoke_generation TEXT NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS delegations_parent ON delegations(parent_account,child_id);
CREATE TABLE IF NOT EXISTS event_delegations (
 event_id TEXT PRIMARY KEY REFERENCES events(id),grant_id TEXT NOT NULL REFERENCES delegations(child_id));
`

type delegationRow struct {
	ID, PublicKey, Parent, IssuerID, IssuerKey, Room, Operations, Generation    string
	Created, Expires, Ceiling, Used, Revoked                                    int64
	Payload, Signature, Proof, RevokePayload, RevokeSignature, RevokeGeneration string
}

const delegationColumns = `child_id,public_key,parent_account,issuer_id,issuer_key,room,operations,generation,created_at,expires_at,ceiling_bytes,used_bytes,revoked_at,payload,signature,proof,revoke_payload,revoke_signature,revoke_generation`

func scanDelegation(row scanner) (delegationRow, error) {
	var g delegationRow
	err := row.Scan(&g.ID, &g.PublicKey, &g.Parent, &g.IssuerID, &g.IssuerKey, &g.Room, &g.Operations, &g.Generation, &g.Created, &g.Expires, &g.Ceiling, &g.Used, &g.Revoked, &g.Payload, &g.Signature, &g.Proof, &g.RevokePayload, &g.RevokeSignature, &g.RevokeGeneration)
	return g, err
}

type DelegationAck struct {
	GrantID      string `json:"grant_id"`
	ChildID      string `json:"child_id"`
	Generation   string `json:"generation"`
	ServiceID    string `json:"service_id"`
	State        string `json:"state"`
	AcceptedAt   int64  `json:"accepted_at"`
	ExpiresAt    int64  `json:"expires_at"`
	CeilingBytes int64  `json:"ceiling_bytes"`
}
type DelegationStatus struct {
	GrantID        string `json:"grant_id"`
	Generation     string `json:"generation"`
	ServiceID      string `json:"service_id"`
	State          string `json:"state"`
	CreatedAt      int64  `json:"created_at"`
	ExpiresAt      int64  `json:"expires_at"`
	CeilingBytes   int64  `json:"ceiling_bytes"`
	UsedBytes      int64  `json:"used_bytes"`
	RemainingBytes int64  `json:"remaining_bytes"`
}
type DelegationRecord struct {
	DelegationStatus
	PublicKey           string   `json:"public_key"`
	PrincipalID         string   `json:"principal_id"`
	IssuerID            string   `json:"issuer_id"`
	IssuerPublicKey     string   `json:"issuer_public_key"`
	Room                string   `json:"room"`
	Disclosure          string   `json:"disclosure"`
	Operations          []string `json:"operations"`
	SignedPayload       string   `json:"signed_payload"`
	Signature           string   `json:"signature"`
	Proof               string   `json:"proof"`
	RevokedAt           int64    `json:"revoked_at,omitempty"`
	RevocationPayload   string   `json:"revocation_payload,omitempty"`
	RevocationSignature string   `json:"revocation_signature,omitempty"`
}

func delegationError(code string) error {
	status, message := 403, "The delegated request is not authorized."
	switch code {
	case "invalid_delegation_context", "invalid_delegation_data":
		status, message = 400, "Use the exact supported delegation schema and fields."
	case "invalid_delegation_proof":
		status, message = 401, "The target key must prove possession over the exact enrollment command."
	case "delegation_not_found", "delegation_scope_mismatch":
		status, message = 404, "Delegation or scoped resource not found."
	case "delegation_exists", "delegation_limit", "delegation_already_revoked", "delegation_generation_mismatch":
		status, message = 409, "This delegation request conflicts with current authoritative state."
	case "delegation_quota_exhausted":
		status, message = 429, "This grant's lifetime byte ceiling is exhausted; it does not replenish at midnight."
	}
	return problem(status, code, message)
}

func verifyDelegationProof(c Command, canonical []byte) error {
	key, err := base64.RawURLEncoding.DecodeString(c.Target)
	if err != nil || len(key) != ed25519.PublicKeySize || base64.RawURLEncoding.EncodeToString(key) != c.Target {
		return delegationError("invalid_delegation_proof")
	}
	proof, err := base64.RawURLEncoding.DecodeString(c.Proof)
	if err != nil || len(proof) != ed25519.SignatureSize || base64.RawURLEncoding.EncodeToString(proof) != c.Proof || !ed25519.Verify(key, canonical, proof) {
		return delegationError("invalid_delegation_proof")
	}
	return nil
}

func (s *Store) resolveDelegation(ctx context.Context, tx *sql.Tx, c Command, a *actor) error {
	a.requestNamespace = a.account
	if !a.signed {
		if c.Delegation != nil {
			return delegationError("invalid_delegation_context")
		}
		return nil
	}
	g, err := scanDelegation(tx.QueryRowContext(ctx, "SELECT "+delegationColumns+" FROM delegations WHERE child_id=?", a.id))
	if errors.Is(err, sql.ErrNoRows) {
		if c.Delegation != nil {
			return delegationError("delegation_not_found")
		}
		return nil
	}
	if err != nil {
		return err
	}
	if c.Delegation == nil {
		return delegationError("delegation_required")
	}
	if c.Delegation.GrantID != a.id || c.Delegation.Generation != g.Generation || g.PublicKey != a.publicKey {
		return delegationError("delegation_context_mismatch")
	}
	a.grant = &g
	a.account = g.Parent
	a.requestNamespace = "delegate:" + g.ID
	return nil
}

func delegationState(ctx context.Context, tx *sql.Tx, g delegationRow, now int64) (string, error) {
	if g.Revoked != 0 {
		return "revoked", nil
	}
	var generation, successor string
	if err := tx.QueryRowContext(ctx, "SELECT value FROM meta WHERE key='generation'").Scan(&generation); err != nil {
		return "", err
	}
	if generation != g.Generation {
		return "epoch_disabled", nil
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

var delegationOperations = map[string]bool{"post": true, "messages.list": true, "message.get": true, "thread.get": true, "room.get": true, "room.pages": true, "works.list": true, "work.get": true, "work.history": true, "work.claim": true, "work.renew": true, "work.submit": true}

func (s *Store) authorizeDelegation(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) error {
	if a.grant == nil {
		return nil
	}
	g := a.grant
	// Fresh, authenticated, original-context own status remains available inactive.
	if c.Operation == "delegation.get" && c.Target == g.ID {
		return nil
	}
	state, err := delegationState(ctx, tx, *g, now)
	if err != nil {
		return err
	}
	if state != "active" {
		return delegationError("delegation_inactive")
	}
	var operations []string
	if err = json.Unmarshal([]byte(g.Operations), &operations); err != nil {
		return err
	}
	allowed := false
	for _, op := range operations {
		if op == c.Operation {
			allowed = true
		}
	}
	if !delegationOperations[c.Operation] || !allowed {
		return delegationError("delegation_forbidden")
	}
	room := c.Room
	switch c.Operation {
	case "message.get", "thread.get", "work.get", "work.history", "work.claim", "work.renew", "work.submit":
		if err = tx.QueryRowContext(ctx, "SELECT e.room FROM events e JOIN rooms r ON r.name=e.room WHERE e.id=? AND r.visibility='public'", c.MessageID).Scan(&room); errors.Is(err, sql.ErrNoRows) {
			return delegationError("delegation_scope_mismatch")
		} else if err != nil {
			return err
		}
	}
	if room == "" || room != g.Room {
		return delegationError("delegation_scope_mismatch")
	}
	var visibility string
	if err = tx.QueryRowContext(ctx, "SELECT visibility FROM rooms WHERE name=?", room).Scan(&visibility); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return delegationError("delegation_scope_mismatch")
		}
		return err
	}
	if visibility != "public" {
		return delegationError("delegation_scope_mismatch")
	}
	if c.Operation == "post" && (c.Visibility != "public" || c.Handle != "" || len(c.Attachments) != 0) {
		return delegationError("delegation_forbidden")
	}
	if (c.Operation == "work.claim" || c.Operation == "work.renew") && (c.TTL <= 0 || c.TTL > g.Expires-now) {
		return problem(400, "invalid_ttl", "A delegated claim must fit entirely within the grant expiry.")
	}
	return nil
}

func parseDelegationData(raw string, create bool) (string, []string, error) {
	fields := []string{"schema", "generation"}
	if create {
		fields = append(fields, "operations", "disclosure")
	} else if len(raw) > 512 {
		return "", nil, delegationError("invalid_delegation_data")
	}
	values, err := strictDelegationObject([]byte(raw), fields...)
	if err != nil {
		return "", nil, err
	}
	var schema int
	var generation string
	if json.Unmarshal(values["schema"], &schema) != nil || schema != 1 || json.Unmarshal(values["generation"], &generation) != nil || !workIDRE.MatchString(generation) {
		return "", nil, delegationError("invalid_delegation_data")
	}
	operations := []string{}
	if create {
		var disclosure string
		if json.Unmarshal(values["disclosure"], &disclosure) != nil || disclosure != "public" || json.Unmarshal(values["operations"], &operations) != nil || len(operations) < 1 || len(operations) > 16 {
			return "", nil, delegationError("invalid_delegation_data")
		}
		seen := map[string]bool{}
		for _, op := range operations {
			if !delegationOperations[op] || seen[op] {
				return "", nil, delegationError("invalid_delegation_data")
			}
			seen[op] = true
		}
	}
	return generation, operations, nil
}

func (s *Store) changeDelegation(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	if a.grant != nil {
		return Result{}, delegationError("delegation_forbidden")
	}
	generation, operations, err := parseDelegationData(c.Data, c.Operation == "delegation.create")
	if err != nil {
		return Result{}, err
	}
	var current string
	if err = tx.QueryRowContext(ctx, "SELECT value FROM meta WHERE key='generation'").Scan(&current); err != nil {
		return Result{}, err
	}
	if generation != current {
		return Result{}, delegationError("delegation_generation_mismatch")
	}
	var g delegationRow
	if c.Operation == "delegation.create" {
		if c.TTL < 60 || c.TTL > 7*86400 {
			return Result{}, problem(400, "invalid_ttl", "Grant TTL must be 60–604800 seconds.")
		}
		if c.Amount <= 0 || c.Amount > s.config.GlobalDailyBytes {
			return Result{}, problem(400, "invalid_amount", "Grant lifetime ceiling must be positive and within configured global daily capacity.")
		}
		r, e := roomAccess(ctx, tx, c.Room, a)
		if e != nil {
			return Result{}, e
		}
		if r.Visibility != "public" {
			return Result{}, delegationError("delegation_forbidden")
		}
		key, _ := base64.RawURLEncoding.DecodeString(c.Target)
		id := fingerprint(key)
		var exists int
		if err = tx.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM identities WHERE id=?)+(SELECT count(*) FROM delegations WHERE child_id=?)+(SELECT count(*) FROM private_read_grants WHERE child_id=?)", id, id, id).Scan(&exists); err != nil {
			return Result{}, err
		}
		if exists != 0 {
			return Result{}, delegationError("delegation_exists")
		}
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM delegations g JOIN identities i ON i.id=g.issuer_id WHERE g.parent_account=? AND g.revoked_at=0 AND g.expires_at>? AND g.generation=? AND i.successor=''", a.account, now, current).Scan(&exists); err != nil {
			return Result{}, err
		}
		if exists >= 32 {
			return Result{}, delegationError("delegation_limit")
		}
		// Reserve retained enrollment, its one future revocation and request metadata.
		if err = s.charge(ctx, tx, a, int64(len(a.canonical)+len(c.Signature)+len(c.Proof)+4096), now); err != nil {
			return Result{}, err
		}
		encoded, _ := json.Marshal(operations)
		g = delegationRow{ID: id, PublicKey: c.Target, Parent: a.account, IssuerID: a.id, IssuerKey: a.publicKey, Room: c.Room, Operations: string(encoded), Generation: current, Created: now, Expires: now + c.TTL, Ceiling: c.Amount, Payload: string(a.canonical), Signature: c.Signature, Proof: c.Proof}
		_, err = tx.ExecContext(ctx, "INSERT INTO delegations(child_id,public_key,parent_account,issuer_id,issuer_key,room,operations,generation,created_at,expires_at,ceiling_bytes,payload,signature,proof) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)", g.ID, g.PublicKey, g.Parent, g.IssuerID, g.IssuerKey, g.Room, g.Operations, g.Generation, g.Created, g.Expires, g.Ceiling, g.Payload, g.Signature, g.Proof)
	} else {
		if !fingerprintRE.MatchString(c.Target) {
			return Result{}, delegationError("delegation_not_found")
		}
		g, err = scanDelegation(tx.QueryRowContext(ctx, "SELECT "+delegationColumns+" FROM delegations WHERE child_id=? AND parent_account=?", c.Target, a.account))
		if errors.Is(err, sql.ErrNoRows) {
			return Result{}, delegationError("delegation_not_found")
		}
		if err != nil {
			return Result{}, err
		}
		if g.Revoked != 0 {
			return Result{}, delegationError("delegation_already_revoked")
		}
		// No quota debit: enrollment prepaid exactly one bounded control transition.
		_, err = tx.ExecContext(ctx, "UPDATE delegations SET revoked_at=?,revoke_payload=?,revoke_signature=?,revoke_generation=? WHERE child_id=?", now, string(a.canonical), c.Signature, current, g.ID)
	}
	if err != nil {
		return Result{}, err
	}
	state := "active"
	if c.Operation == "delegation.revoke" {
		state = "revoked"
	}
	return Result{Data: map[string]any{"ack": DelegationAck{GrantID: g.ID, ChildID: g.ID, Generation: current, ServiceID: s.config.ServiceID, State: state, AcceptedAt: now, ExpiresAt: g.Expires, CeilingBytes: g.Ceiling}}}, nil
}

func (s *Store) delegationStatus(ctx context.Context, tx *sql.Tx, g delegationRow, now int64) (DelegationStatus, error) {
	state, err := delegationState(ctx, tx, g, now)
	return DelegationStatus{GrantID: g.ID, Generation: g.Generation, ServiceID: s.config.ServiceID, State: state, CreatedAt: g.Created, ExpiresAt: g.Expires, CeilingBytes: g.Ceiling, UsedBytes: g.Used, RemainingBytes: g.Ceiling - g.Used}, err
}
func (s *Store) readDelegation(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if c.Operation == "delegation.get" {
		if !fingerprintRE.MatchString(c.Target) {
			return Result{}, delegationError("delegation_not_found")
		}
		g, err := scanDelegation(tx.QueryRowContext(ctx, "SELECT "+delegationColumns+" FROM delegations WHERE child_id=? AND room IN (SELECT name FROM rooms WHERE visibility='public')", c.Target))
		if errors.Is(err, sql.ErrNoRows) {
			return Result{}, delegationError("delegation_not_found")
		}
		if err != nil {
			return Result{}, err
		}
		status, err := s.delegationStatus(ctx, tx, g, now)
		if err != nil {
			return Result{}, err
		}
		if a.grant != nil {
			return Result{Data: map[string]any{"delegation": status}}, nil
		}
		operations := []string{}
		if err = json.Unmarshal([]byte(g.Operations), &operations); err != nil {
			return Result{}, err
		}
		record := DelegationRecord{DelegationStatus: status, PublicKey: g.PublicKey, PrincipalID: g.Parent, IssuerID: g.IssuerID, IssuerPublicKey: g.IssuerKey, Room: g.Room, Disclosure: "public", Operations: operations, SignedPayload: g.Payload, Signature: g.Signature, Proof: g.Proof, RevokedAt: g.Revoked, RevocationPayload: g.RevokePayload, RevocationSignature: g.RevokeSignature}
		return Result{Data: map[string]any{"delegation": record}}, nil
	}
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	if a.grant != nil {
		return Result{}, delegationError("delegation_forbidden")
	}
	if c.Limit < 0 || c.Limit > 32 {
		return Result{}, problem(400, "invalid_limit", "Grant list limit is 1–32, or zero for default 16.")
	}
	limit := c.Limit
	if limit == 0 {
		limit = 16
	}
	cursor, err := s.decodeConversationCursor(c.Cursor, "delegations.list", a.account)
	if err != nil {
		return Result{}, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT "+delegationColumns+" FROM delegations WHERE parent_account=? AND child_id>? ORDER BY child_id LIMIT ?", a.account, cursor.Page, limit+1)
	if err != nil {
		return Result{}, err
	}
	stored := []delegationRow{}
	for rows.Next() {
		g, e := scanDelegation(rows)
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
	statuses := []DelegationStatus{}
	for _, g := range stored {
		status, e := s.delegationStatus(ctx, tx, g, now)
		if e != nil {
			return Result{}, e
		}
		statuses = append(statuses, status)
	}
	result := Result{Data: map[string]any{"delegations": statuses, "has_more": more}}
	if more {
		result.NextCursor = s.encodeConversationCursor(conversationCursor{Domain: "delegations.list", Scope: a.account, Page: stored[len(stored)-1].ID})
	}
	return result, nil
}
