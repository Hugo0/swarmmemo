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
	"strings"
	"unicode/utf8"
)

// Identity links let a key say where else its agent lives: a domain, another
// Ed25519 key, a Nostr key, a URL or an account on another board. Every link is
// shown in exactly one of four states so that an unproven link can never look
// proven: claimed (our key said so), proof_attached (the other side signed a
// statement anyone can check offline), verified (this service checked live
// state, with the time it did), and lapsed (a verified check stopped passing).
// Links belong to the key, not the continuity account: every proof names the
// key's fingerprint, so a rotation does not carry them. See
// docs/rfcs/0009-identity-links.md.

const identityLinkSchema = `
CREATE TABLE IF NOT EXISTS identity_links (
 agent TEXT NOT NULL REFERENCES identities(id), kind TEXT NOT NULL, value TEXT NOT NULL,
 proof TEXT NOT NULL DEFAULT '',
 state TEXT NOT NULL CHECK(state IN ('claimed','proof_attached','verified','lapsed')),
 created_at INTEGER NOT NULL, checked_at INTEGER NOT NULL DEFAULT 0, lapsed_at INTEGER NOT NULL DEFAULT 0,
 failures INTEGER NOT NULL DEFAULT 0, attempted_at INTEGER NOT NULL DEFAULT 0,
 next_check_at INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(agent,kind,value));
CREATE INDEX IF NOT EXISTS identity_link_checks ON identity_links(next_check_at) WHERE next_check_at>0;
`

const (
	IdentityLinkMaxPerKey = 8
	// IdentityLinkStatement is the exact prefix another Ed25519 key signs; the
	// full statement is prefix:SERVICE_ID:OUR_FINGERPRINT:THEIR_KEY.
	IdentityLinkStatement = "swarmmemo-identity-link:1"
	// IdentityLinkTXTPrefix is the TXT record value a domain publishes at
	// _swarmmemo.DOMAIN, followed by the key's sha256 fingerprint.
	IdentityLinkTXTPrefix = "swarmmemo-fingerprint="
)

// IdentityLink is the public shape of one link. Only the fields its state
// justifies are set: checked_at only for a live check that passed, lapsed_at
// only for one that stopped passing, proof and statement only for an attached
// signature that any reader can verify without trusting this service.
type IdentityLink struct {
	Kind      string `json:"kind"`
	Value     string `json:"value"`
	State     string `json:"state"`
	Method    string `json:"method,omitempty"`
	LinkedAt  int64  `json:"linked_at"`
	CheckedAt int64  `json:"checked_at,omitempty"`
	LapsedAt  int64  `json:"lapsed_at,omitempty"`
	Proof     string `json:"proof,omitempty"`
	Statement string `json:"statement,omitempty"`
}

// linkKind is the extension point. normalize returns the one canonical value;
// attach checks any proof offline at link time and returns the initial state;
// check, when set, is a live verification the rechecker repeats on schedule.
// Phase 2 adds Nostr proofs by giving "nostr" an attach, and board profiles by
// giving "board" a check through the webhook dialer; nothing else changes.
type linkKind struct {
	normalize func(value string) (string, bool)
	attach    func(s *Store, fingerprint, value, proof string) (string, error)
	check     func(s *Store, ctx context.Context, fingerprint, value string) linkOutcome
	method    string
}

var linkKinds = map[string]linkKind{
	"domain":  {normalize: normalizeLinkDomain, attach: claimOnly, check: (*Store).checkDomainLink, method: "dns-txt"},
	"ed25519": {normalize: normalizeEd25519Key, attach: attachEd25519Proof, method: "ed25519-signature"},
	"nostr":   {normalize: normalizeNostrKey, attach: claimOnly},
	"url":     {normalize: normalizeLinkURL, attach: claimOnly},
	"board":   {normalize: normalizeLinkURL, attach: claimOnly},
}

// LinkKinds lists the accepted kinds for discovery, in a stable order.
func LinkKinds() []string { return []string{"domain", "ed25519", "nostr", "url", "board"} }

func linkError(code string) error {
	switch code {
	case "link_not_found":
		return problem(404, "link_not_found", "This key has no link of that kind and value.")
	case "link_limit":
		return problem(409, "link_limit", "This key already holds the maximum of 8 identity links; unlink one first.")
	case "link_delegated":
		return problem(403, "link_delegated", "A scoped child grant cannot link or unlink identities for its parent.")
	case "invalid_link_value":
		return problem(400, "invalid_link_value", "The value is not a valid canonical value for this kind; see /protocol.md#linking-identities.")
	case "invalid_link_proof":
		return problem(400, "invalid_link_proof", "The proof is not a valid signature by the linked key over the exact link statement, or this kind takes no proof.")
	case "link_reserved":
		return problem(400, "link_reserved", "This service's own domains cannot be linked by an agent.")
	}
	return problem(400, "invalid_link", `Data must be a strict JSON object {"schema":1,"kind":KIND,"value":VALUE} with an optional "proof", at most 1024 bytes; kind is domain, ed25519, nostr, url or board.`)
}

func claimOnly(_ *Store, _, _, proof string) (string, error) {
	if proof != "" {
		return "", linkError("invalid_link_proof")
	}
	return "claimed", nil
}

// LinkStatement is the exact byte string another Ed25519 key signs to attach
// its half of a link. Exported so the docs, tests and clients share one form.
func LinkStatement(serviceID, fingerprint, theirKey string) string {
	return IdentityLinkStatement + ":" + serviceID + ":" + fingerprint + ":" + theirKey
}

func attachEd25519Proof(s *Store, fingerprint, value, proof string) (string, error) {
	if proof == "" {
		return "claimed", nil
	}
	key, _ := base64.RawURLEncoding.DecodeString(value)
	sig, err := base64.RawURLEncoding.DecodeString(proof)
	if err != nil || len(sig) != ed25519.SignatureSize || base64.RawURLEncoding.EncodeToString(sig) != proof ||
		!ed25519.Verify(key, []byte(LinkStatement(s.config.ServiceID, fingerprint, value)), sig) {
		return "", linkError("invalid_link_proof")
	}
	return "proof_attached", nil
}

// smallOrderPoints are the encodings (sign bit ignored) of the eight points of
// small order. A signature "by" such a key can be produced without any secret,
// so a proof from one would attest nothing; libsodium refuses the same list.
var smallOrderPoints = [][32]byte{
	{0x00},
	{0x01},
	{0x26, 0xe8, 0x95, 0x8f, 0xc2, 0xb2, 0x27, 0xb0, 0x45, 0xc3, 0xf4, 0x89, 0xf2, 0xef, 0x98, 0xf0, 0xd5, 0xdf, 0xac, 0x05, 0xd3, 0xc6, 0x33, 0x39, 0xb1, 0x38, 0x02, 0x88, 0x6d, 0x53, 0xfc, 0x05},
	{0xc7, 0x17, 0x6a, 0x70, 0x3d, 0x4d, 0xd8, 0x4f, 0xba, 0x3c, 0x0b, 0x76, 0x0d, 0x10, 0x67, 0x0f, 0x2a, 0x20, 0x53, 0xfa, 0x2c, 0x39, 0xcc, 0xc6, 0x4e, 0xc7, 0xfd, 0x77, 0x92, 0xac, 0x03, 0x7a},
	{0xec, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f},
	{0xed, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f},
	{0xee, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f},
}

func normalizeEd25519Key(raw string) (string, bool) {
	key, err := base64.RawURLEncoding.Strict().DecodeString(raw)
	if err != nil || len(key) != ed25519.PublicKeySize || base64.RawURLEncoding.EncodeToString(key) != raw {
		return "", false
	}
	masked := [32]byte(key)
	masked[31] &= 0x7f
	for _, point := range smallOrderPoints {
		if masked == point {
			return "", false
		}
	}
	return raw, true
}

// reservedLinkDomain is the one rule a bare DNS name check cannot know: this
// service's own names, and names under them, are not an agent's to claim.
func (s *Store) reservedLinkDomain(name string) bool {
	for _, reserved := range s.config.ReservedDomains {
		if name == reserved || strings.HasSuffix(name, "."+reserved) {
			return true
		}
	}
	return false
}

type linkData struct {
	Kind, Value, Proof string
}

// parseLinkData accepts only the documented object; an unknown, repeated or
// null field is an error, matching how webhook and profile data are parsed.
func parseLinkData(raw string, withProof bool) (linkData, error) {
	var d linkData
	if len(raw) > 1024 || !utf8.ValidString(raw) {
		return d, linkError("invalid_link")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return d, linkError("invalid_link")
	}
	seen := map[string]bool{}
	schema := 0
	for decoder.More() {
		token, err = decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok || seen[name] {
			return d, linkError("invalid_link")
		}
		seen[name] = true
		var value json.RawMessage
		if err = decoder.Decode(&value); err != nil || bytes.Equal(value, []byte("null")) {
			return d, linkError("invalid_link")
		}
		switch {
		case name == "schema":
			err = json.Unmarshal(value, &schema)
		case name == "kind":
			err = json.Unmarshal(value, &d.Kind)
		case name == "value":
			err = json.Unmarshal(value, &d.Value)
		case name == "proof" && withProof:
			err = json.Unmarshal(value, &d.Proof)
		default:
			return d, linkError("invalid_link")
		}
		if err != nil {
			return d, linkError("invalid_link")
		}
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return d, linkError("invalid_link")
	}
	if _, err = decoder.Token(); !errors.Is(err, io.EOF) {
		return d, linkError("invalid_link")
	}
	if schema != 1 || !seen["kind"] || !seen["value"] || (seen["proof"] && d.Proof == "") {
		return d, linkError("invalid_link")
	}
	if _, ok := linkKinds[d.Kind]; !ok {
		return d, linkError("invalid_link")
	}
	return d, nil
}

func (s *Store) changeIdentityLink(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	if a.grant != nil {
		return Result{}, linkError("link_delegated")
	}
	d, err := parseLinkData(c.Data, c.Operation == "identity.link")
	if err != nil {
		return Result{}, err
	}
	kind := linkKinds[d.Kind]
	value, ok := kind.normalize(d.Value)
	if !ok {
		return Result{}, linkError("invalid_link_value")
	}
	if c.Operation == "identity.unlink" {
		if err = s.charge(ctx, tx, a, 256, now); err != nil {
			return Result{}, err
		}
		res, err := tx.ExecContext(ctx, "DELETE FROM identity_links WHERE agent=? AND kind=? AND value=?", a.id, d.Kind, value)
		if err != nil {
			return Result{}, err
		}
		removed, err := res.RowsAffected()
		if err != nil {
			return Result{}, err
		}
		if removed == 0 {
			return Result{}, linkError("link_not_found")
		}
		if err = audit(ctx, tx, c.Operation, a.id, d.Kind, "identity link removed", now); err != nil {
			return Result{}, err
		}
		return Result{Data: map[string]any{"unlinked": true, "kind": d.Kind, "value": value}}, nil
	}
	if d.Kind == "domain" && s.reservedLinkDomain(value) {
		return Result{}, linkError("link_reserved")
	}
	if d.Kind == "ed25519" && value == a.publicKey {
		return Result{}, linkError("invalid_link_value")
	}
	state, err := kind.attach(s, a.id, value, d.Proof)
	if err != nil {
		return Result{}, err
	}
	var existing string
	var attempted int64
	err = tx.QueryRowContext(ctx, "SELECT state,attempted_at FROM identity_links WHERE agent=? AND kind=? AND value=?", a.id, d.Kind, value).Scan(&existing, &attempted)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Result{}, err
	}
	found := err == nil
	if !found {
		var held int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM identity_links WHERE agent=?", a.id).Scan(&held); err != nil {
			return Result{}, err
		}
		if held >= IdentityLinkMaxPerKey {
			return Result{}, linkError("link_limit")
		}
	}
	if err = s.charge(ctx, tx, a, int64(len(a.canonical))+256, now); err != nil {
		return Result{}, err
	}
	// A live kind is queued for a check now, but never sooner than the minimum
	// interval after its last lookup: linking again is how an agent asks for a
	// recheck, and it must not become a way to aim lookups at a name.
	next := int64(0)
	if kind.check != nil {
		next = max(now, attempted+linkMinInterval)
	}
	if found {
		// Linking again can attach or replace a proof, and asks for a recheck;
		// it never downgrades what a live check already established.
		proof := d.Proof
		if kind.check != nil {
			state, proof = existing, ""
		} else if state == "claimed" && existing == "proof_attached" {
			state = existing
			proof = ""
		}
		query := "UPDATE identity_links SET state=?,next_check_at=? WHERE agent=? AND kind=? AND value=?"
		args := []any{state, next, a.id, d.Kind, value}
		if proof != "" {
			query = "UPDATE identity_links SET state=?,next_check_at=?,proof=? WHERE agent=? AND kind=? AND value=?"
			args = []any{state, next, proof, a.id, d.Kind, value}
		}
		if _, err = tx.ExecContext(ctx, query, args...); err != nil {
			return Result{}, err
		}
	} else if _, err = tx.ExecContext(ctx, "INSERT INTO identity_links(agent,kind,value,proof,state,created_at,next_check_at) VALUES(?,?,?,?,?,?,?)",
		a.id, d.Kind, value, d.Proof, state, now, next); err != nil {
		return Result{}, err
	}
	if err = audit(ctx, tx, c.Operation, a.id, d.Kind, "identity link "+state, now); err != nil {
		return Result{}, err
	}
	data := map[string]any{"kind": d.Kind, "value": value, "state": state}
	switch d.Kind {
	case "domain":
		data["txt_name"] = "_swarmmemo." + value
		data["txt_value"] = IdentityLinkTXTPrefix + a.id
		data["checks_enabled"] = s.identityChecks.Load()
		if next > 0 {
			data["check_after"] = next
		}
	case "ed25519":
		data["statement"] = LinkStatement(s.config.ServiceID, a.id, value)
	}
	return Result{Data: data}, nil
}

// readIdentityLinks returns one key's links in the order they were made. The
// state is stored, never inferred here: the only writers are link, unlink and
// the rechecker, each of which sets the state it proved.
func (s *Store) readIdentityLinks(ctx context.Context, tx *sql.Tx, agent string) ([]IdentityLink, error) {
	rows, err := tx.QueryContext(ctx, "SELECT kind,value,proof,state,created_at,checked_at,lapsed_at FROM identity_links WHERE agent=? ORDER BY created_at,kind,value LIMIT ?", agent, IdentityLinkMaxPerKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	links := []IdentityLink{}
	for rows.Next() {
		var l IdentityLink
		var proof string
		var checked, lapsed int64
		if err = rows.Scan(&l.Kind, &l.Value, &proof, &l.State, &l.LinkedAt, &checked, &lapsed); err != nil {
			return nil, err
		}
		switch l.State {
		case "verified":
			l.Method, l.CheckedAt = linkKinds[l.Kind].method, checked
		case "lapsed":
			l.Method, l.LapsedAt = linkKinds[l.Kind].method, lapsed
			if checked > 0 {
				l.CheckedAt = checked
			}
		case "proof_attached":
			l.Method, l.Proof = linkKinds[l.Kind].method, proof
			if l.Kind == "ed25519" {
				l.Statement = LinkStatement(s.config.ServiceID, agent, l.Value)
			}
		}
		links = append(links, l)
	}
	return links, rows.Err()
}
