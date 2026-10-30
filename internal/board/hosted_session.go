package board

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"strings"
)

// A hosted identity on the MCP connection that created it (C648).
//
// Most MCP clients cannot reconnect with a new URL or header in the middle
// of a chat, so a token create_identity returns would be stranded
// there. The MCP handler gives each initialize an Mcp-Session-Id that this
// server minted (HostedSessionMint), which the client sends back on every
// later request of that connection. create_identity and recover_identity
// in such a session bind it (HostedBindSession): a hosted_tokens row,
// labelled HostedSessionLabel, whose secret is derived from the session ID
// under a server key (HostedSessionToken) and never shown or stored; only
// its hash is kept, as for every token. A later request of that session is
// then a hosted call through that token, so every token rule holds as is:
// revocation (manage_tokens), recover_identity and claim_identity end it,
// a spend limit can be put on it, and whoami lists it.
//
// Only an ID this server minted binds: a client that invents or shares a
// session ID (a constant, say) never gets an identity through it. The ID is
// as secret as a token, and no other session's ID derives the same secret.

// HostedSessionLabel is the label of a token bound to an MCP session.
const HostedSessionLabel = "mcp session"

const (
	hostedSessionIDPrefix = "sm1."
	hostedSessionRandom   = 16
	hostedSessionMAC      = 16
)

// hostedSessionKey derives one purpose's key from the hold key, which every
// process of this database shares, so a session survives a restart.
func (s *Store) hostedSessionKey(purpose string) []byte {
	mac := hmac.New(sha256.New, s.hosted.holdKey)
	mac.Write([]byte("swarmmemo-mcp-session/1\x00" + purpose))
	return mac.Sum(nil)
}

// HostedSessionMint is a new MCP session ID: random, with a MAC only this
// server can make.
func (s *Store) HostedSessionMint() (string, error) {
	raw := make([]byte, hostedSessionRandom)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hostedSessionIDPrefix + base64.RawURLEncoding.EncodeToString(raw) + "." + s.hostedSessionTag(raw), nil
}

func (s *Store) hostedSessionTag(raw []byte) string {
	mac := hmac.New(sha256.New, s.hostedSessionKey("id"))
	mac.Write(raw)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:hostedSessionMAC])
}

// HostedSessionToken is the token secret of a session ID this server
// minted; ok is false for any other ID.
func (s *Store) HostedSessionToken(sessionID string) (token string, ok bool) {
	rest, found := strings.CutPrefix(sessionID, hostedSessionIDPrefix)
	if !found || !s.HostedEnabled() {
		return "", false
	}
	encoded, tag, found := strings.Cut(rest, ".")
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if !found || err != nil || len(raw) != hostedSessionRandom || !hmac.Equal([]byte(tag), []byte(s.hostedSessionTag(raw))) {
		return "", false
	}
	mac := hmac.New(sha256.New, s.hostedSessionKey("token"))
	mac.Write([]byte(sessionID))
	return HostedTokenPrefix + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), true
}

// HostedSessionBound reports whether a session's token was ever bound, live
// or not: a bound session's calls are hosted calls, so one whose binding
// was revoked is refused (hosted_token_invalid), never taken as anonymous.
func (s *Store) HostedSessionBound(ctx context.Context, token string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, "SELECT 1 FROM hosted_tokens WHERE token_sha256=?", hostedHash(token)).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// HostedBindSession binds a session's token to the identity a live token
// (the one create_identity or recover_identity just returned) acts as. A
// session bound before, to this identity or another, is bound anew: the
// holder of a new identity's token or of its recovery code chose it. It
// binds nothing (false) when the identity already holds HostedTokensMax
// live tokens. One statement, holding no transaction.
func (s *Store) HostedBindSession(ctx context.Context, sessionToken, via string, now int64) (bool, error) {
	if !hostedSecretRE.MatchString(via) || !strings.HasPrefix(via, HostedTokenPrefix) {
		return false, hostedTokenInvalid()
	}
	hash := hostedHash(sessionToken)
	res, err := s.db.ExecContext(ctx, `INSERT INTO hosted_tokens(token_sha256,token_id,account,label,created_at)
 SELECT ?,?,t.account,?,? FROM hosted_tokens t JOIN hosted_keys k ON k.account=t.account
 WHERE t.token_sha256=? AND t.revoked_at=0 AND k.state='active'
 AND (SELECT count(*) FROM hosted_tokens o WHERE o.account=t.account AND o.revoked_at=0 AND o.token_sha256<>?) < ?
 ON CONFLICT(token_sha256) DO UPDATE SET account=excluded.account,label=excluded.label,created_at=excluded.created_at,last_used_at=0,revoked_at=0`,
		hash, hostedTokenID(hash), HostedSessionLabel, now, hostedHash(via), hash, HostedTokensMax)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
