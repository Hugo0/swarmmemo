package board

// OAuth 2.1 for the hosted MCP assistant profile (T56): signing in IS a
// hosted identity, never a second account system. The authorization page
// (internal/httpapi/oauth.go) creates a hosted identity or recovers one with
// its recovery code; the access token it then issues is an ordinary hosted
// token (smh1_, a hosted_tokens row, hashed, listed by whoami with
// last_used_at, revoked by manage_tokens), bound to one resource and expiring
// after an hour. A refresh token (smo1_) renews it, rotating on every use;
// presenting a refresh token that was already rotated revokes its whole
// family (the connection), as does revoking its access token, recovering the
// identity or claiming it.
//
// Every secret is stored as SHA-256 only. Authorization codes are single use,
// expire after OAuthCodeSeconds and are bound to the client, the exact
// redirect URI, the PKCE S256 challenge and the resource. Each method here is
// one transaction that touches only that transaction, never the pool.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const (
	// OAuthScope is the one scope: act as your hosted identity, with exactly
	// what a hosted token may do.
	OAuthScope = "hosted"
	// OAuthCodeSeconds is how long an authorization code may be redeemed
	// (OAuth 2.1 recommends at most 10 minutes).
	OAuthCodeSeconds = 600
	// OAuthAccessSeconds is an access token's lifetime.
	OAuthAccessSeconds = 3600
	// OAuthRefreshSeconds is how long a refresh token stays usable; each use
	// rotates it and starts a new period.
	OAuthRefreshSeconds = 30 * 86400
	// OAuthRefreshPrefix marks refresh tokens for secret scanners.
	OAuthRefreshPrefix = "smo1_"
	// OAuthClientPrefix starts a dynamically registered client_id.
	OAuthClientPrefix = "smcl_"
	// OAuthConnectionsMax bounds an identity's live OAuth connections (token
	// families); a new one past it signs the oldest out.
	OAuthConnectionsMax = 4
	// OAuthRedirectURIsMax and OAuthRedirectURIBytes bound a client's
	// redirect URIs; OAuthClientNameBytes its name.
	OAuthRedirectURIsMax  = 8
	OAuthRedirectURIBytes = 512
	OAuthClientNameBytes  = 80
	// OAuthClientsPerNetworkDaily and OAuthClientsDaily bound dynamic client
	// registration.
	OAuthClientsPerNetworkDaily = 500
	OAuthClientsDaily           = 5000

	oauthCodePrefix = "smc1_"
)

const oauthSchema = `
CREATE TABLE IF NOT EXISTS oauth_clients (
 client_id TEXT PRIMARY KEY, client_name TEXT NOT NULL, redirect_uris TEXT NOT NULL,
 created_at INTEGER NOT NULL, network TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS oauth_clients_network ON oauth_clients(network,created_at);
CREATE INDEX IF NOT EXISTS oauth_clients_created ON oauth_clients(created_at);
CREATE TABLE IF NOT EXISTS oauth_codes (
 code_sha256 TEXT PRIMARY KEY, client_id TEXT NOT NULL, client_name TEXT NOT NULL, redirect_uri TEXT NOT NULL,
 code_challenge TEXT NOT NULL, resource TEXT NOT NULL, scope TEXT NOT NULL, account TEXT NOT NULL,
 created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, used_at INTEGER NOT NULL DEFAULT 0, family_id TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS oauth_families (
 family_id TEXT PRIMARY KEY, account TEXT NOT NULL, client_id TEXT NOT NULL, client_name TEXT NOT NULL,
 resource TEXT NOT NULL, scope TEXT NOT NULL, created_at INTEGER NOT NULL,
 access_sha256 TEXT NOT NULL UNIQUE, access_expires_at INTEGER NOT NULL,
 refresh_sha256 TEXT NOT NULL UNIQUE, refresh_expires_at INTEGER NOT NULL,
 rotated_at INTEGER NOT NULL DEFAULT 0, revoked_at INTEGER NOT NULL DEFAULT 0, revoked_reason TEXT NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS oauth_families_account ON oauth_families(account,revoked_at);
CREATE TABLE IF NOT EXISTS oauth_refresh_used (refresh_sha256 TEXT PRIMARY KEY, family_id TEXT NOT NULL, used_at INTEGER NOT NULL);
`

// OAuthError is an OAuth error answer (RFC 6749 §5.2): Code is its error
// field and Description its error_description.
type OAuthError struct {
	Status      int
	Code        string
	Description string
}

func (e *OAuthError) Error() string { return e.Code + ": " + e.Description }

func oauthError(code string, status int, description string) *OAuthError {
	return &OAuthError{Status: status, Code: code, Description: description}
}

// invalidGrant is the one answer for every code or refresh token problem:
// unknown, used, expired, revoked, of another client or another redirect
// URI, or a wrong PKCE verifier (no oracle).
func invalidGrant() *OAuthError {
	return oauthError("invalid_grant", 400, "The authorization code or refresh token is invalid, expired, already used or revoked, or was issued to another client; sign in again.")
}

type audienceKey struct{}

// WithTokenAudience records the resource a request presented its hosted
// token to: the MCP profile's URL when the token came in an Authorization
// header, empty for a path token. A token issued through OAuth is accepted
// only by its own resource, in a header (MCP authorization: audience
// binding; a token never travels in a URL).
func WithTokenAudience(ctx context.Context, resource string) context.Context {
	return context.WithValue(ctx, audienceKey{}, resource)
}

func tokenAudience(ctx context.Context) string {
	aud, _ := ctx.Value(audienceKey{}).(string)
	return aud
}

// oauthRandom is prefix and 32 random bytes in base64url, and its SHA-256.
func oauthRandom(prefix string) (secret, hash string, err error) { return hostedSecret(prefix) }

// OAuthClient is a client as the authorization page and token endpoint see
// it: a registered one, or one described by its client ID metadata document.
type OAuthClient struct {
	ID           string   `json:"client_id"`
	Name         string   `json:"client_name"`
	RedirectURIs []string `json:"redirect_uris"`
}

// OAuthRegisterClient is dynamic client registration (RFC 7591) of a public
// client: its name and redirect URIs, already checked by the caller
// (httpapi.validRedirectURI), counted against the daily caps of the peer's
// network (its anonymous /24 or /48 pseudonym, as hosted issuance counts)
// and of the server.
func (s *Store) OAuthRegisterClient(ctx context.Context, name string, redirects []string, peer string, now int64) (OAuthClient, error) {
	network := s.anonymousAccount(peer, now, AnonCreditPrefixV6Bits)
	if !s.HostedEnabled() {
		return OAuthClient{}, oauthError("temporarily_unavailable", 400, "Sign-in is off on this server.")
	}
	if len(redirects) == 0 || len(redirects) > OAuthRedirectURIsMax || len(name) > OAuthClientNameBytes {
		return OAuthClient{}, oauthError("invalid_client_metadata", 400, "A client needs 1 to 8 redirect_uris and a client_name of at most 80 bytes.")
	}
	id, _, err := oauthRandom(OAuthClientPrefix)
	if err != nil {
		return OAuthClient{}, err
	}
	id = id[:len(OAuthClientPrefix)+22] // 16 random bytes are plenty for an identifier that is not a secret
	raw, _ := json.Marshal(redirects)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return OAuthClient{}, err
	}
	defer tx.Rollback()
	day := now - now%86400
	var mine, all int64
	if err = tx.QueryRowContext(ctx, "SELECT coalesce(sum(network=?),0),count(*) FROM oauth_clients WHERE created_at>=?", network, day).Scan(&mine, &all); err != nil {
		return OAuthClient{}, err
	}
	if mine >= OAuthClientsPerNetworkDaily || all >= OAuthClientsDaily {
		return OAuthClient{}, oauthError("temporarily_unavailable", 429, "Today's client registrations are used up; retry after 00:00 UTC, or use a client ID metadata document.")
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO oauth_clients(client_id,client_name,redirect_uris,created_at,network) VALUES(?,?,?,?,?)", id, name, string(raw), now, network); err != nil {
		return OAuthClient{}, err
	}
	if err = tx.Commit(); err != nil {
		return OAuthClient{}, err
	}
	return OAuthClient{ID: id, Name: name, RedirectURIs: redirects}, nil
}

// OAuthRegisteredClient is a dynamically registered client, ok false for an
// unknown client_id. One short read, outside any transaction.
func (s *Store) OAuthRegisteredClient(ctx context.Context, id string) (OAuthClient, bool, error) {
	var c OAuthClient
	var raw string
	err := s.db.QueryRowContext(ctx, "SELECT client_id,client_name,redirect_uris FROM oauth_clients WHERE client_id=?", id).Scan(&c.ID, &c.Name, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return c, false, nil
	}
	if err != nil {
		return c, false, err
	}
	if json.Unmarshal([]byte(raw), &c.RedirectURIs) != nil {
		return c, false, errors.New("oauth client: stored redirect_uris are malformed")
	}
	return c, true, nil
}

// OAuthGrant is what an authorization code is bound to.
type OAuthGrant struct {
	ClientID, ClientName, RedirectURI, Challenge, Resource, Scope string
}

// OAuthIssueCode makes an authorization code for account, an active hosted
// identity, bound to g. discardTokenID names a token the identity's
// creation made that nobody was shown (hosted.create's first token), which
// is revoked in the same transaction.
func (s *Store) OAuthIssueCode(ctx context.Context, g OAuthGrant, account, discardTokenID string, now int64) (string, error) {
	code, hash, err := oauthRandom(oauthCodePrefix)
	if err != nil {
		return "", err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var state string
	if err = tx.QueryRowContext(ctx, "SELECT state FROM hosted_keys WHERE account=?", account).Scan(&state); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if state != "active" {
		return "", hostedTokenInvalid()
	}
	if discardTokenID != "" {
		if _, err = tx.ExecContext(ctx, "UPDATE hosted_tokens SET revoked_at=? WHERE account=? AND token_id=? AND revoked_at=0", now, account, discardTokenID); err != nil {
			return "", err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO oauth_codes(code_sha256,client_id,client_name,redirect_uri,code_challenge,resource,scope,account,created_at,expires_at)
 VALUES(?,?,?,?,?,?,?,?,?,?)`, hash, g.ClientID, g.ClientName, g.RedirectURI, g.Challenge, g.Resource, g.Scope, account, now, now+OAuthCodeSeconds); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return code, nil
}

// OAuthRecovered is a hosted identity signed in with its recovery code.
type OAuthRecovered struct {
	Account, Handle, RecoveryCode string
	TokensRevoked                 int64
}

// OAuthRecover signs in with a recovery code: the code works once, so a new
// one is made and shown once (as hosted.recover does). Other connections
// keep working unless signOut is set, which revokes every token and OAuth
// connection as hosted.recover does. Every failure is the one 403
// recovery_invalid.
func (s *Store) OAuthRecover(ctx context.Context, code string, signOut bool, now int64) (OAuthRecovered, error) {
	var out OAuthRecovered
	if !s.HostedEnabled() {
		return out, hostedUnavailable("off")
	}
	if !hostedSecretRE.MatchString(code) || !strings.HasPrefix(code, HostedRecoveryPrefix) {
		return out, recoveryInvalid()
	}
	recovery, recoveryHash, err := hostedSecret(HostedRecoveryPrefix)
	if err != nil {
		return out, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if paused, err := s.hostedPaused(ctx, tx, now); err != nil || paused {
		if err == nil {
			err = hostedUnavailable("paused by a public lever (see /api/levers)")
		}
		return out, err
	}
	err = tx.QueryRowContext(ctx, "SELECT k.account,i.handle FROM hosted_keys k JOIN identities i ON i.id=k.account WHERE k.recovery_sha256=? AND k.state='active'", hostedHash(code)).Scan(&out.Account, &out.Handle)
	if errors.Is(err, sql.ErrNoRows) {
		return OAuthRecovered{}, recoveryInvalid()
	}
	if err != nil {
		return OAuthRecovered{}, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE hosted_keys SET recovery_sha256=? WHERE account=?", recoveryHash, out.Account); err != nil {
		return OAuthRecovered{}, err
	}
	detail := "signed in; other connections kept"
	if signOut {
		res, err := tx.ExecContext(ctx, "UPDATE hosted_tokens SET revoked_at=? WHERE account=? AND revoked_at=0", now, out.Account)
		if err != nil {
			return OAuthRecovered{}, err
		}
		out.TokensRevoked, _ = res.RowsAffected()
		if err = revokeOAuthConnections(ctx, tx, out.Account, "", "recovered", now); err != nil {
			return OAuthRecovered{}, err
		}
		detail = fmt.Sprintf("signed in; %d tokens revoked", out.TokensRevoked)
	}
	if err = audit(ctx, tx, "oauth.recover", out.Account, out.Account, detail, now); err != nil {
		return OAuthRecovered{}, err
	}
	if err = tx.Commit(); err != nil {
		return OAuthRecovered{}, err
	}
	out.RecoveryCode = recovery
	return out, nil
}

// OAuthTokens is a token endpoint answer's secrets.
type OAuthTokens struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int64
	Scope        string
}

var pkceVerifierRE = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)

// PKCEChallenge is the S256 code challenge of verifier (RFC 7636 §4.2).
func PKCEChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// OAuthExchangeCode is the authorization_code grant. The code is spent by
// any attempt that finds it, right or wrong; presenting a spent code again
// revokes the connection it was exchanged for (OAuth 2.1 §4.1.3).
func (s *Store) OAuthExchangeCode(ctx context.Context, code, clientID, redirectURI, verifier, resource string, now int64) (OAuthTokens, error) {
	if !s.HostedEnabled() {
		return OAuthTokens{}, oauthError("temporarily_unavailable", 400, "Sign-in is off on this server.")
	}
	if !strings.HasPrefix(code, oauthCodePrefix) || !hostedTokenShape(code) {
		return OAuthTokens{}, invalidGrant()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return OAuthTokens{}, err
	}
	defer tx.Rollback()
	var g OAuthGrant
	var account, family string
	var expires, used int64
	err = tx.QueryRowContext(ctx, `SELECT client_id,client_name,redirect_uri,code_challenge,resource,scope,account,expires_at,used_at,family_id FROM oauth_codes WHERE code_sha256=?`, hostedHash(code)).
		Scan(&g.ClientID, &g.ClientName, &g.RedirectURI, &g.Challenge, &g.Resource, &g.Scope, &account, &expires, &used, &family)
	if errors.Is(err, sql.ErrNoRows) {
		return OAuthTokens{}, invalidGrant()
	}
	if err != nil {
		return OAuthTokens{}, err
	}
	if used != 0 {
		// A replayed code: whoever holds it may have intercepted it, so the
		// connection it bought is cut.
		if family != "" {
			if err = revokeFamily(ctx, tx, family, "code_replayed", now); err != nil {
				return OAuthTokens{}, err
			}
			if err = tx.Commit(); err != nil {
				return OAuthTokens{}, err
			}
		}
		return OAuthTokens{}, invalidGrant()
	}
	if _, err = tx.ExecContext(ctx, "UPDATE oauth_codes SET used_at=? WHERE code_sha256=?", now, hostedHash(code)); err != nil {
		return OAuthTokens{}, err
	}
	ok := expires > now && clientID == g.ClientID && redirectURI == g.RedirectURI && (resource == "" || resource == g.Resource) &&
		pkceVerifierRE.MatchString(verifier) && subtle.ConstantTimeCompare([]byte(PKCEChallenge(verifier)), []byte(g.Challenge)) == 1
	if !ok {
		// The spent code is committed, so a wrong verifier cannot be retried.
		if err = tx.Commit(); err != nil {
			return OAuthTokens{}, err
		}
		return OAuthTokens{}, invalidGrant()
	}
	tokens, family, err := s.newOAuthConnection(ctx, tx, account, g, now)
	if err != nil {
		return OAuthTokens{}, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE oauth_codes SET family_id=? WHERE code_sha256=?", family, hostedHash(code)); err != nil {
		return OAuthTokens{}, err
	}
	if err = tx.Commit(); err != nil {
		return OAuthTokens{}, err
	}
	return tokens, nil
}

// hostedTokenShape reports a prefix and 43 base64url characters.
func hostedTokenShape(secret string) bool {
	_, rest, ok := strings.Cut(secret, "_")
	return ok && len(rest) == 43 && strings.Trim(rest, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_") == ""
}

// oauthLabel is the hosted token label of a client's connection.
func oauthLabel(clientName string) string {
	label := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, clientName)
	if label == "" {
		label = "client"
	}
	label = "oauth: " + label
	for len(label) > HostedTokenLabelBytes {
		r := []rune(label)
		label = string(r[:len(r)-1])
	}
	return label
}

// newOAuthConnection starts a token family for account: the access token
// (a hosted token) and the refresh token. Past OAuthConnectionsMax live
// connections the oldest is signed out.
func (s *Store) newOAuthConnection(ctx context.Context, tx *sql.Tx, account string, g OAuthGrant, now int64) (OAuthTokens, string, error) {
	var state string
	if err := tx.QueryRowContext(ctx, "SELECT state FROM hosted_keys WHERE account=?", account).Scan(&state); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return OAuthTokens{}, "", err
	}
	if state != "active" {
		return OAuthTokens{}, "", invalidGrant()
	}
	rows, err := tx.QueryContext(ctx, "SELECT family_id FROM oauth_families WHERE account=? AND revoked_at=0 AND refresh_expires_at>? ORDER BY created_at DESC,family_id", account, now)
	if err != nil {
		return OAuthTokens{}, "", err
	}
	var live []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return OAuthTokens{}, "", err
		}
		live = append(live, id)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return OAuthTokens{}, "", err
	}
	for i := OAuthConnectionsMax - 1; i < len(live); i++ {
		if err = revokeFamily(ctx, tx, live[i], "replaced", now); err != nil {
			return OAuthTokens{}, "", err
		}
	}
	familyRaw := make([]byte, 16)
	if _, err = rand.Read(familyRaw); err != nil {
		return OAuthTokens{}, "", err
	}
	family := hex.EncodeToString(familyRaw)
	access, accessHash, err := hostedSecret(HostedTokenPrefix)
	if err != nil {
		return OAuthTokens{}, "", err
	}
	refresh, refreshHash, err := hostedSecret(OAuthRefreshPrefix)
	if err != nil {
		return OAuthTokens{}, "", err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO hosted_tokens(token_sha256,token_id,account,label,created_at) VALUES(?,?,?,?,?)", accessHash, hostedTokenID(accessHash), account, oauthLabel(g.ClientName), now); err != nil {
		return OAuthTokens{}, "", err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO oauth_families(family_id,account,client_id,client_name,resource,scope,created_at,access_sha256,access_expires_at,refresh_sha256,refresh_expires_at)
 VALUES(?,?,?,?,?,?,?,?,?,?,?)`, family, account, g.ClientID, g.ClientName, g.Resource, g.Scope, now, accessHash, now+OAuthAccessSeconds, refreshHash, now+OAuthRefreshSeconds); err != nil {
		return OAuthTokens{}, "", err
	}
	if err = audit(ctx, tx, "oauth.connect", account, account, "token "+hostedTokenID(accessHash), now); err != nil {
		return OAuthTokens{}, "", err
	}
	return OAuthTokens{AccessToken: access, RefreshToken: refresh, ExpiresIn: OAuthAccessSeconds, Scope: g.Scope}, family, nil
}

// OAuthRefresh is the refresh_token grant: the refresh token rotates, the
// previous access token is revoked and a new pair is returned. A refresh
// token that was already rotated revokes its family: one of the two
// holders is not the client.
func (s *Store) OAuthRefresh(ctx context.Context, refresh, clientID, resource, scope string, now int64) (OAuthTokens, error) {
	if !s.HostedEnabled() {
		return OAuthTokens{}, oauthError("temporarily_unavailable", 400, "Sign-in is off on this server.")
	}
	if !strings.HasPrefix(refresh, OAuthRefreshPrefix) || !hostedTokenShape(refresh) {
		return OAuthTokens{}, invalidGrant()
	}
	if scope != "" && scope != OAuthScope {
		return OAuthTokens{}, oauthError("invalid_scope", 400, "The only scope is "+OAuthScope+".")
	}
	hash := hostedHash(refresh)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return OAuthTokens{}, err
	}
	defer tx.Rollback()
	var f struct {
		id, account, clientID, clientName, resource, scope, accessHash string
		refreshExpires, revoked                                        int64
	}
	err = tx.QueryRowContext(ctx, `SELECT family_id,account,client_id,client_name,resource,scope,access_sha256,refresh_expires_at,revoked_at FROM oauth_families WHERE refresh_sha256=?`, hash).
		Scan(&f.id, &f.account, &f.clientID, &f.clientName, &f.resource, &f.scope, &f.accessHash, &f.refreshExpires, &f.revoked)
	if errors.Is(err, sql.ErrNoRows) {
		var family string
		if err = tx.QueryRowContext(ctx, "SELECT family_id FROM oauth_refresh_used WHERE refresh_sha256=?", hash).Scan(&family); err == nil {
			if err = revokeFamily(ctx, tx, family, "refresh_reused", now); err != nil {
				return OAuthTokens{}, err
			}
			if err = tx.Commit(); err != nil {
				return OAuthTokens{}, err
			}
			return OAuthTokens{}, invalidGrant()
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return OAuthTokens{}, err
		}
		return OAuthTokens{}, invalidGrant()
	}
	if err != nil {
		return OAuthTokens{}, err
	}
	if f.revoked != 0 || f.refreshExpires <= now || clientID != f.clientID || (resource != "" && resource != f.resource) {
		return OAuthTokens{}, invalidGrant()
	}
	var state string
	if err = tx.QueryRowContext(ctx, "SELECT state FROM hosted_keys WHERE account=?", f.account).Scan(&state); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return OAuthTokens{}, err
	}
	if state != "active" {
		return OAuthTokens{}, invalidGrant()
	}
	if paused, err := s.hostedPaused(ctx, tx, now); err != nil || paused {
		if err == nil {
			err = oauthError("temporarily_unavailable", 400, "Hosted identities are paused by a public lever (see /api/levers).")
		}
		return OAuthTokens{}, err
	}
	access, accessHash, err := hostedSecret(HostedTokenPrefix)
	if err != nil {
		return OAuthTokens{}, err
	}
	next, nextHash, err := hostedSecret(OAuthRefreshPrefix)
	if err != nil {
		return OAuthTokens{}, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO oauth_refresh_used(refresh_sha256,family_id,used_at) VALUES(?,?,?)", hash, f.id, now); err != nil {
		return OAuthTokens{}, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE hosted_tokens SET revoked_at=? WHERE token_sha256=? AND revoked_at=0", now, f.accessHash); err != nil {
		return OAuthTokens{}, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO hosted_tokens(token_sha256,token_id,account,label,created_at) VALUES(?,?,?,?,?)", accessHash, hostedTokenID(accessHash), f.account, oauthLabel(f.clientName), now); err != nil {
		return OAuthTokens{}, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE oauth_families SET access_sha256=?,access_expires_at=?,refresh_sha256=?,refresh_expires_at=?,rotated_at=? WHERE family_id=?",
		accessHash, now+OAuthAccessSeconds, nextHash, now+OAuthRefreshSeconds, now, f.id); err != nil {
		return OAuthTokens{}, err
	}
	if err = tx.Commit(); err != nil {
		return OAuthTokens{}, err
	}
	return OAuthTokens{AccessToken: access, RefreshToken: next, ExpiresIn: OAuthAccessSeconds, Scope: f.scope}, nil
}

// OAuthRevoke is token revocation (RFC 7009): an access or refresh token of
// an OAuth connection, current or already rotated, revokes the connection.
// Anything else changes nothing; the caller answers 200 either way. A
// clientID that is given must be the connection's.
func (s *Store) OAuthRevoke(ctx context.Context, token, clientID string, now int64) error {
	if !hostedTokenShape(token) || !(strings.HasPrefix(token, HostedTokenPrefix) || strings.HasPrefix(token, OAuthRefreshPrefix)) {
		return nil
	}
	hash := hostedHash(token)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var family, owner string
	err = tx.QueryRowContext(ctx, `SELECT family_id,client_id FROM oauth_families WHERE access_sha256=? OR refresh_sha256=?
 UNION ALL SELECT u.family_id,f.client_id FROM oauth_refresh_used u JOIN oauth_families f ON f.family_id=u.family_id WHERE u.refresh_sha256=? LIMIT 1`, hash, hash, hash).Scan(&family, &owner)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if clientID != "" && clientID != owner {
		return nil
	}
	if err = revokeFamily(ctx, tx, family, "revoked", now); err != nil {
		return err
	}
	return tx.Commit()
}

// OAuthAccessToken reports whether token is an OAuth connection's current
// access token (a rotated one is already revoked). Such a token acts as the
// identity but may not mint hosted tokens (manage_tokens create): a minted
// token has no expiry and no audience and would outlive the connection's
// revocation. One short read, outside any transaction.
func (s *Store) OAuthAccessToken(ctx context.Context, token string) (bool, error) {
	if !strings.HasPrefix(token, HostedTokenPrefix) {
		return false, nil
	}
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM oauth_families WHERE access_sha256=?", hostedHash(token)).Scan(&n)
	return n > 0, err
}

// revokeFamily signs one OAuth connection out: the family and its current
// access token.
func revokeFamily(ctx context.Context, tx *sql.Tx, family, reason string, now int64) error {
	var account, accessHash string
	var revoked int64
	err := tx.QueryRowContext(ctx, "SELECT account,access_sha256,revoked_at FROM oauth_families WHERE family_id=?", family).Scan(&account, &accessHash, &revoked)
	if errors.Is(err, sql.ErrNoRows) || err == nil && revoked != 0 {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE oauth_families SET revoked_at=?,revoked_reason=? WHERE family_id=?", now, reason, family); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE hosted_tokens SET revoked_at=? WHERE token_sha256=? AND revoked_at=0", now, accessHash); err != nil {
		return err
	}
	return audit(ctx, tx, "oauth.revoke", account, account, reason, now)
}

// revokeOAuthConnections signs out account's OAuth connections: the one
// whose current access token is tokenID, or every one and every unexchanged
// authorization code when tokenID is "".
// hosted.token revoke, hosted.recover and hosted.claim call it beside
// revoking the hosted tokens, so a refresh token cannot bring a revoked
// token back.
func revokeOAuthConnections(ctx context.Context, tx *sql.Tx, account, tokenID, reason string, now int64) error {
	query, args := "UPDATE oauth_families SET revoked_at=?,revoked_reason=? WHERE account=? AND revoked_at=0", []any{now, reason, account}
	if tokenID != "" {
		query += " AND access_sha256 IN (SELECT token_sha256 FROM hosted_tokens WHERE account=? AND token_id=?)"
		args = append(args, account, tokenID)
	}
	if _, err := tx.ExecContext(ctx, query, args...); err != nil || tokenID != "" {
		return err
	}
	// Signing every connection out also spends the codes not yet
	// exchanged, so none starts a connection afterwards.
	_, err := tx.ExecContext(ctx, "UPDATE oauth_codes SET used_at=? WHERE account=? AND used_at=0", now, account)
	return err
}

// oauthLiveFilter is the hosted_tokens condition (alias t) that leaves out
// the access token of an OAuth connection that ended (its refresh token
// expired), so whoami lists live connections only; ? is now.
const oauthLiveFilter = ` AND NOT EXISTS (SELECT 1 FROM oauth_families f WHERE f.access_sha256=t.token_sha256 AND (f.revoked_at<>0 OR f.refresh_expires_at<=?))`

// oauthOwnTokenFilter is the hosted_tokens condition (alias t) that counts
// only tokens made by hosted.token or hosted.create, never OAuth's, for the
// HostedTokensMax cap.
const oauthOwnTokenFilter = ` AND NOT EXISTS (SELECT 1 FROM oauth_families f WHERE f.access_sha256=t.token_sha256)`
