package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"
)

const peerSchema = `
CREATE TABLE IF NOT EXISTS peer_cards (
 account TEXT PRIMARY KEY, description TEXT NOT NULL, capabilities TEXT NOT NULL,
 availability TEXT NOT NULL, author TEXT NOT NULL REFERENCES identities(id),
 public_key TEXT NOT NULL, signature TEXT NOT NULL, payload TEXT NOT NULL,
 published_at INTEGER NOT NULL, expires_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS peer_expiry ON peer_cards(expires_at,account);
CREATE TABLE IF NOT EXISTS peer_capabilities (
 account TEXT NOT NULL REFERENCES peer_cards(account) ON DELETE CASCADE,
 capability TEXT NOT NULL, PRIMARY KEY(account,capability));
CREATE INDEX IF NOT EXISTS peer_capability ON peer_capabilities(capability,account);
`

const PeerDefaultTTL int64 = 7 * 86400
const PeerMaxTTL int64 = 30 * 86400

// Profile field bounds, exported so the browser form states the limits the
// service enforces rather than a copy of them.
const (
	ProfileDescriptionBytes = 2048
	ProfileMaxCapabilities  = 16
)

// ProfileAvailability lists the accepted availability values in display order.
func ProfileAvailability() []string { return []string{"available", "busy", "away"} }

// AgentRef deliberately omits last_seen, creation time, and private activity.
type AgentRef struct {
	ID        string `json:"id"`
	PublicKey string `json:"public_key"`
	Handle    string `json:"handle,omitempty"`
}

// Profile is an explicitly published, self-described capability advertisement.
// Its original signing key and exact canonical payload survive key rotation;
// CurrentAgent is a separate server-resolved account-continuity reference.
type Profile struct {
	Schema        int      `json:"schema"`
	Description   string   `json:"description"`
	Capabilities  []string `json:"capabilities"`
	Availability  string   `json:"availability"`
	Author        string   `json:"author"`
	PublicKey     string   `json:"public_key"`
	Signature     string   `json:"signature"`
	SignedPayload string   `json:"signed_payload"`
	PublishedAt   int64    `json:"published_at"`
	// A profile is never hidden for age. ttl sets how long its availability
	// counts as confirmed: RenewedAt is the last publish, FreshUntil the end of
	// that confirmation, and Fresh whether it still holds at read time. After
	// that the profile stays listed; only its availability reads unconfirmed.
	RenewedAt  int64 `json:"renewed_at"`
	FreshUntil int64 `json:"fresh_until"`
	Fresh      bool  `json:"fresh"`
	// ExpiresAt is the deprecated name of FreshUntil, kept for compatibility.
	ExpiresAt     int64    `json:"expires_at"`
	CurrentAgent  AgentRef `json:"current_agent"`
	SelfDescribed bool     `json:"self_described"`
}

func parsePeerData(raw string) (Profile, error) {
	card := Profile{SelfDescribed: true}
	invalid := func() (Profile, error) {
		return Profile{}, problem(400, "invalid_profile", "Data must be a strict schema-1 JSON object with description (up to 2048 UTF-8 bytes), up to 16 unique lowercase capability slugs, and availability available, busy, or away. Maximum encoded Data is 8192 bytes.")
	}
	if len(raw) > 8192 || !utf8.ValidString(raw) {
		return invalid()
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return invalid()
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err = decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok || seen[name] {
			return invalid()
		}
		seen[name] = true
		var value json.RawMessage
		if err = decoder.Decode(&value); err != nil || string(value) == "null" {
			return invalid()
		}
		switch name {
		case "schema":
			err = json.Unmarshal(value, &card.Schema)
		case "description":
			err = json.Unmarshal(value, &card.Description)
		case "capabilities":
			err = json.Unmarshal(value, &card.Capabilities)
		case "availability":
			err = json.Unmarshal(value, &card.Availability)
		default:
			return invalid()
		}
		if err != nil {
			return invalid()
		}
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return invalid()
	}
	if _, err = decoder.Token(); !errors.Is(err, io.EOF) {
		return invalid()
	}
	if len(seen) != 4 || card.Schema != 1 || len(card.Description) > ProfileDescriptionBytes || strings.ContainsRune(card.Description, '\x00') || card.Capabilities == nil || len(card.Capabilities) > ProfileMaxCapabilities {
		return invalid()
	}
	if !slices.Contains(ProfileAvailability(), card.Availability) {
		return invalid()
	}
	capabilities := map[string]bool{}
	for _, capability := range card.Capabilities {
		if !slug.MatchString(capability) || capabilities[capability] {
			return invalid()
		}
		capabilities[capability] = true
	}
	return card, nil
}

func (s *Store) changeProfile(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	if c.Operation == "agent.profile.remove" {
		if err := s.charge(ctx, tx, a, 256, now); err != nil {
			return Result{}, err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM peer_cards WHERE account=?", a.account); err != nil {
			return Result{}, err
		}
		if err := audit(ctx, tx, c.Operation, a.id, a.id, "public capability card removed", now); err != nil {
			return Result{}, err
		}
		return Result{Data: map[string]any{"removed": true, "agent_id": a.id}}, nil
	}
	card, err := parsePeerData(c.Data)
	if err != nil {
		return Result{}, err
	}
	ttl := c.TTL
	if ttl == 0 {
		ttl = PeerDefaultTTL
	}
	if ttl < 60 || ttl > PeerMaxTTL {
		return Result{}, problem(400, "invalid_ttl", fmt.Sprintf("Profile ttl must be 60 seconds to %s; omit it for %s.", LimitText("profile_ttl_maximum_seconds"), LimitText("profile_ttl_default_seconds")))
	}
	if err = s.charge(ctx, tx, a, int64(len(a.canonical))+512, now); err != nil {
		return Result{}, err
	}
	capabilities, _ := json.Marshal(card.Capabilities)
	_, err = tx.ExecContext(ctx, `INSERT INTO peer_cards(account,description,capabilities,availability,author,public_key,signature,payload,published_at,expires_at)
 VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(account) DO UPDATE SET description=excluded.description,capabilities=excluded.capabilities,
 availability=excluded.availability,author=excluded.author,public_key=excluded.public_key,signature=excluded.signature,
 payload=excluded.payload,published_at=excluded.published_at,expires_at=excluded.expires_at`,
		a.account, card.Description, string(capabilities), card.Availability, a.id, a.publicKey, c.Signature, string(a.canonical), now, now+ttl)
	if err != nil {
		return Result{}, err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM peer_capabilities WHERE account=?", a.account); err != nil {
		return Result{}, err
	}
	for _, capability := range card.Capabilities {
		if _, err = tx.ExecContext(ctx, "INSERT INTO peer_capabilities(account,capability) VALUES(?,?)", a.account, capability); err != nil {
			return Result{}, err
		}
	}
	if err = audit(ctx, tx, c.Operation, a.id, a.id, "explicit public capability card", now); err != nil {
		return Result{}, err
	}
	// Receipts do not retain card bodies. A later exact retry acknowledges the
	// original success, but cannot expose or restore a subsequently removed card.
	return Result{Data: map[string]any{"published": true, "agent_id": a.id, "published_at": now, "renewed_at": now, "fresh_until": now + ttl, "expires_at": now + ttl}}, nil
}

func agentReadError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Status: 503, Code: "profile_read_timeout", Message: "The agent directory read exceeded its two-second budget; narrow the query or retry.", RetryAfter: 2}
	}
	return err
}
