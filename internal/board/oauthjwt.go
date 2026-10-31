package board

// Access tokens for apps as JWTs (S7) and token exchange (RFC 8693).
//
// An access token an app's sign-in issues (any scope but "hosted") is a JWT
// (RFC 9068, typ at+jwt) signed with EdDSA (Ed25519) by a server key; the
// public keys are the JWKS at /.well-known/jwks.json, each named by its kid
// (its RFC 7638 thumbprint), so an app checks a token offline. The signing
// key rotates every OAuthSigningKeySeconds; a retired key stays published
// for OAuthSigningKeyGraceSeconds, longer than any token it signed lives.
// Keys are sealed under the hosted identities' KEK, as hosted seeds are.
//
// The JWT is also the connection's current access token (oauth_families
// access_sha256, a hosted_tokens row labelled "oauth: APP"), so revoking the
// connection, recovering or claiming the identity ends it at once here; an
// app that must see that sooner than the token's expiry asks
// /oauth/introspect (RFC 7662).
//
// Token exchange: an app trades a token issued for a resource it owns (its
// own sign-in, or an agent's token for the app's MCP server) for a
// SwarmMemo token for the same identity, with at most the scopes of both the
// subject token and the app's client, for at most the subject token's
// remaining life. The app proves itself with an actor token, a JWT its own
// key signed. Everything done with the exchanged token carries via
// app:APP (bearerCredential), on the message and in the audit log.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"

	"swarmmemo/internal/b64"
	"swarmmemo/internal/services"
)

const (
	// OAuthJWTAccessSeconds is an app token's lifetime (15 minutes).
	OAuthJWTAccessSeconds = 900
	// OAuthSigningKeySeconds is how long one signing key signs.
	OAuthSigningKeySeconds = 30 * 86400
	// OAuthSigningKeyGraceSeconds is how long a retired key stays in the
	// JWKS and still verifies.
	OAuthSigningKeyGraceSeconds = 86400
	// OAuthActorTokenSeconds bounds an actor token's life (exp - iat).
	OAuthActorTokenSeconds = 300
	// OAuthJWTBytes bounds any JWT this server reads.
	OAuthJWTBytes = 4096
	// OAuthGrantTokenExchange and the token types of RFC 8693.
	OAuthGrantTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange"
	OAuthTokenTypeAccess    = "urn:ietf:params:oauth:token-type:access_token"
	OAuthTokenTypeJWT       = "urn:ietf:params:oauth:token-type:jwt"
	// oauthClockSkew is how far in the future an iat may be.
	oauthClockSkew = 60
)

const oauthJWTSchema = `
CREATE TABLE IF NOT EXISTS oauth_signing_keys (
 kid TEXT PRIMARY KEY, public_key TEXT NOT NULL, sealed_private_key TEXT NOT NULL, kek_id TEXT NOT NULL,
 created_at INTEGER NOT NULL, retired_at INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS oauth_assertions_used (jti_sha256 TEXT PRIMARY KEY, expires_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS oauth_exchanged_revoked (jti_sha256 TEXT PRIMARY KEY, expires_at INTEGER NOT NULL);
`

// BearerClaims are an app token's claims.
type BearerClaims struct {
	Issuer   string `json:"iss"`
	Subject  string `json:"sub"`
	Audience string `json:"aud"`
	Expires  int64  `json:"exp"`
	IssuedAt int64  `json:"iat"`
	ID       string `json:"jti"`
	Scope    string `json:"scope"`
	ClientID string `json:"client_id"`
	// Session is the connection (token family) the token belongs to.
	Session string `json:"sid"`
	// App is the app the token is for: its client's, or its resource's.
	App string `json:"app,omitempty"`
	// Actor is the app that exchanged the token (RFC 8693 §4.1).
	Actor *BearerActor `json:"act,omitempty"`
}

// BearerActor is an exchanged token's act claim.
type BearerActor struct {
	Subject  string `json:"sub"`
	ClientID string `json:"client_id"`
}

// bearerGrant is what a bearer token lets a command do (actor.bearer).
type bearerGrant struct {
	scopes   []string
	app      string
	session  string
	exchange bool
}

// JWK is one public key of the JWKS.
type JWK struct {
	KeyType string `json:"kty"`
	Curve   string `json:"crv"`
	X       string `json:"x"`
	KeyID   string `json:"kid"`
	Use     string `json:"use"`
	Alg     string `json:"alg"`
}

func invalidToken() error {
	return problem(401, "invalid_token", "This bearer token is not valid here: it is malformed, expired, revoked, for another resource, or its sign-in ended. Sign in again, or exchange a fresh token (/protocol.md#sign-in-with-swarmmemo).")
}

// SetOAuthIssuer names the authorization server (the public origin) app
// tokens carry as iss; the HTTP adapter sets it once at startup.
func (s *Store) SetOAuthIssuer(origin string) {
	origin = strings.TrimRight(origin, "/")
	s.oauthIssuer.Store(&origin)
}

// issuer is the iss of app tokens ("" until SetOAuthIssuer).
func (s *Store) issuer() string {
	if p := s.oauthIssuer.Load(); p != nil {
		return *p
	}
	return ""
}

// OAuthAppResource is the audience of the tokens SwarmMemo accepts for app.
func (s *Store) OAuthAppResource(app string) string { return s.issuer() + "/app/" + app }

// LooksLikeJWT reports a compact JWS shape: three base64url parts.
func LooksLikeJWT(token string) bool {
	if len(token) > OAuthJWTBytes || strings.Count(token, ".") != 2 {
		return false
	}
	return strings.Trim(token, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.") == ""
}

// jwkThumbprint is the RFC 7638 thumbprint of an Ed25519 public key.
func jwkThumbprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256([]byte(`{"crv":"Ed25519","kty":"OKP","x":"` + base64.RawURLEncoding.EncodeToString(pub) + `"}`))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func signingAAD(kid string) string { return "oauth-signing-key:" + kid }

// oauthSigner is the current signing key, made or rotated in tx when it is
// missing, old, or cannot be opened (its KEK left the KEK file).
func (s *Store) oauthSigner(ctx context.Context, tx *sql.Tx, now int64) (string, ed25519.PrivateKey, error) {
	var kid, sealed, kekID string
	var created int64
	err := tx.QueryRowContext(ctx, "SELECT kid,sealed_private_key,kek_id,created_at FROM oauth_signing_keys WHERE retired_at=0 ORDER BY created_at DESC,kid LIMIT 1").Scan(&kid, &sealed, &kekID, &created)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", nil, err
	}
	if err == nil && created > now-OAuthSigningKeySeconds {
		if key, err := s.openSeed(signingAAD(kid), sealed, kekID); err == nil {
			return kid, key, nil
		}
	}
	return s.rotateSigner(ctx, tx, now)
}

// rotateSigner retires every signing key and makes a new one.
func (s *Store) rotateSigner(ctx context.Context, tx *sql.Tx, now int64) (string, ed25519.PrivateKey, error) {
	if !s.HostedEnabled() {
		return "", nil, hostedUnavailable("off")
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", nil, err
	}
	kid := jwkThumbprint(pub)
	seed := key.Seed()
	sealed, kekID := s.sealSeed(signingAAD(kid), seed)
	clear(seed)
	if _, err = tx.ExecContext(ctx, "UPDATE oauth_signing_keys SET retired_at=? WHERE retired_at=0", now); err != nil {
		return "", nil, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO oauth_signing_keys(kid,public_key,sealed_private_key,kek_id,created_at) VALUES(?,?,?,?,?)",
		kid, base64.RawURLEncoding.EncodeToString(pub), sealed, kekID, now); err != nil {
		return "", nil, err
	}
	if err = audit(ctx, tx, "oauth.signing_key", "server", kid, "rotated", now); err != nil {
		return "", nil, err
	}
	return kid, key, nil
}

// OAuthRotateSigningKey retires the signing key now and makes a new one
// (operators, after a suspected leak; the old one stays published for the
// grace period unless its row is retired further back).
func (s *Store) OAuthRotateSigningKey(ctx context.Context, now int64) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	kid, key, err := s.rotateSigner(ctx, tx, now)
	clear(key)
	if err != nil {
		return "", err
	}
	return kid, tx.Commit()
}

// OAuthJWKS is the published signing keys: the current one and those
// retired within the grace period. One short read, outside any transaction.
func (s *Store) OAuthJWKS(ctx context.Context, now int64) ([]JWK, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT kid,public_key FROM oauth_signing_keys WHERE retired_at=0 OR retired_at>? ORDER BY created_at DESC,kid LIMIT 8", now-OAuthSigningKeyGraceSeconds)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := []JWK{}
	for rows.Next() {
		var k JWK
		if err = rows.Scan(&k.KeyID, &k.X); err != nil {
			return nil, err
		}
		k.KeyType, k.Curve, k.Use, k.Alg = "OKP", "Ed25519", "sig", "EdDSA"
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// signJWT makes a compact JWS of claims with the current signing key.
func (s *Store) signJWT(ctx context.Context, tx *sql.Tx, claims BearerClaims, now int64) (string, error) {
	if s.issuer() == "" {
		return "", errors.New("oauth: no issuer set (SetOAuthIssuer)")
	}
	kid, key, err := s.oauthSigner(ctx, tx, now)
	if err != nil {
		return "", err
	}
	defer clear(key)
	header, _ := json.Marshal(jwsHeader{Alg: "EdDSA", Typ: "at+jwt", Kid: kid})
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	return input + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(input))), nil
}

type jwsHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ,omitempty"`
	Kid string `json:"kid,omitempty"`
}

// parsedJWS is a compact JWS split and decoded, not yet verified.
type parsedJWS struct {
	header    jwsHeader
	payload   []byte
	input     []byte
	signature []byte
}

// ParseJWS splits and decodes a compact JWS (RFC 7515) of at most
// OAuthJWTBytes: strict base64url, a header with only alg EdDSA, typ and
// kid, and a 64-byte signature. It verifies nothing.
func ParseJWS(token string) (parsedJWS, error) {
	var p parsedJWS
	if !LooksLikeJWT(token) {
		return p, invalidToken()
	}
	parts := strings.Split(token, ".")
	header, err1 := b64.URL(parts[0])
	payload, err2 := b64.URL(parts[1])
	sig, err3 := b64.URL(parts[2])
	if err1 != nil || err2 != nil || err3 != nil || len(sig) != ed25519.SignatureSize || len(payload) == 0 {
		return p, invalidToken()
	}
	if services.StrictObject(header, &p.header) != nil || p.header.Alg != "EdDSA" {
		return p, invalidToken()
	}
	p.payload, p.input, p.signature = payload, []byte(parts[0]+"."+parts[1]), sig
	return p, nil
}

// verifyAppToken checks an app token's signature (a published key named
// by kid), its type and its times, and reads its claims. It does not check
// that its connection is still live (bearerLive).
func (s *Store) verifyAppToken(ctx context.Context, q queryer, token string, now int64) (BearerClaims, error) {
	var c BearerClaims
	p, err := ParseJWS(token)
	if err != nil || s.issuer() == "" || p.header.Typ != "at+jwt" || p.header.Kid == "" || len(p.header.Kid) > 64 {
		return c, invalidToken()
	}
	var encoded string
	err = q.QueryRowContext(ctx, "SELECT public_key FROM oauth_signing_keys WHERE kid=? AND (retired_at=0 OR retired_at>?)", p.header.Kid, now-OAuthSigningKeyGraceSeconds).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return c, invalidToken()
	}
	if err != nil {
		return c, err
	}
	pub, err := b64.URL(encoded)
	if err != nil || len(pub) != ed25519.PublicKeySize || !ed25519.Verify(pub, p.input, p.signature) {
		return c, invalidToken()
	}
	if services.StrictObject(p.payload, &c) != nil || c.Issuer != s.issuer() || !fingerprintRE.MatchString(c.Subject) ||
		c.Expires <= now || c.IssuedAt > now+oauthClockSkew || c.Session == "" || c.ClientID == "" {
		return BearerClaims{}, invalidToken()
	}
	if _, ok := ParseOAuthScopes(c.Scope); !ok {
		return BearerClaims{}, invalidToken()
	}
	if c.Actor != nil && (c.Actor.Subject != "app:"+c.App || c.App == "") {
		return BearerClaims{}, invalidToken()
	}
	return c, nil
}

// bearerKey is a live app token's identity key, sealed.
type bearerKey struct{ publicKey, sealed, kekID string }

// bearerLive checks that the token's connection still acts: not revoked,
// the identity still hosted and active, and, for a token the connection
// issued (not an exchanged one), that it is still the connection's current
// access token; and that its scopes are within the connection's.
func (s *Store) bearerLive(ctx context.Context, q queryer, token string, c BearerClaims, now int64) (bearerKey, error) {
	var k bearerKey
	var account, accessHash, scope, state string
	var accessExpires, refreshExpires, revoked int64
	err := q.QueryRowContext(ctx, `SELECT f.account,f.access_sha256,f.access_expires_at,f.refresh_expires_at,f.revoked_at,f.scope,k.public_key,k.sealed_private_key,k.kek_id,k.state
 FROM oauth_families f JOIN hosted_keys k ON k.account=f.account WHERE f.family_id=?`, c.Session).
		Scan(&account, &accessHash, &accessExpires, &refreshExpires, &revoked, &scope, &k.publicKey, &k.sealed, &k.kekID, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return k, invalidToken()
	}
	if err != nil {
		return k, err
	}
	granted, _ := ParseOAuthScopes(c.Scope)
	family, ok := ParseOAuthScopes(scope)
	if account != c.Subject || revoked != 0 || state != "active" || refreshExpires <= now || !ok || !scopeSubset(granted, family) {
		return bearerKey{}, invalidToken()
	}
	if c.Actor == nil && (accessHash != hostedHash(token) || accessExpires <= now) {
		return bearerKey{}, invalidToken()
	}
	// An exchanged token ends with the client that exchanged it
	// (oauth.client.retire), as the sign-ins made through it do, and when
	// that client revokes it (/oauth/revoke).
	if c.Actor != nil {
		client, ok, err := readAppClient(ctx, q, c.ClientID)
		if err != nil {
			return bearerKey{}, err
		}
		if !ok || client.RetiredAt != 0 || client.App != c.App {
			return bearerKey{}, invalidToken()
		}
		var revokedJTI int
		if err = q.QueryRowContext(ctx, "SELECT count(*) FROM oauth_exchanged_revoked WHERE jti_sha256=?", exchangedJTI(c)).Scan(&revokedJTI); err != nil {
			return bearerKey{}, err
		}
		if revokedJTI != 0 {
			return bearerKey{}, invalidToken()
		}
	}
	return k, nil
}

// BearerSigner resolves an app token presented to /v1/command to its
// identity's private key and public key, as HostedSigner does for a hosted
// token: one short read outside any transaction; the caller zeroes the key
// after one signature. Only a token whose audience is SwarmMemo's own
// resource for its app is accepted.
func (s *Store) BearerSigner(ctx context.Context, token string, now int64) (ed25519.PrivateKey, string, BearerClaims, error) {
	if !s.HostedEnabled() {
		return nil, "", BearerClaims{}, hostedUnavailable("off")
	}
	c, err := s.verifyAppToken(ctx, s.db, token, now)
	if err != nil {
		return nil, "", c, err
	}
	if c.App == "" || c.Audience != s.OAuthAppResource(c.App) {
		return nil, "", c, invalidToken()
	}
	k, err := s.bearerLive(ctx, s.db, token, c, now)
	if err != nil {
		return nil, "", c, err
	}
	if paused, err := s.hostedPaused(ctx, s.db, now); err != nil || paused {
		if err == nil {
			err = hostedUnavailable("paused by a public lever (see /api/levers)")
		}
		return nil, "", c, err
	}
	key, err := s.openSeed(c.Subject, k.sealed, k.kekID)
	if err != nil {
		return nil, "", c, err
	}
	return key, k.publicKey, c, nil
}

type bearerTokenKey struct{}

// WithBearerToken records the app token a /v1/command request carried, so
// the command it signs runs with that token's scopes and provenance. Only
// the HTTP adapter calls it.
func WithBearerToken(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, bearerTokenKey{}, token)
}

func bearerFrom(ctx context.Context) string {
	token, _ := ctx.Value(bearerTokenKey{}).(string)
	return token
}

// bearerCredential sets a.bearer and a.credential for a command signed
// through an app token, checked again here in the command's transaction:
// a hosted identity's key, its own live token. It returns ctx carrying the
// app's provenance, via app:APP.
func (s *Store) bearerCredential(ctx context.Context, tx *sql.Tx, a *actor, now int64) (context.Context, error) {
	token := bearerFrom(ctx)
	if token == "" {
		return ctx, nil
	}
	if !a.hosted {
		return ctx, invalidToken()
	}
	c, err := s.verifyAppToken(ctx, tx, token, now)
	if err != nil {
		return ctx, err
	}
	if c.Subject != a.account || c.App == "" || c.Audience != s.OAuthAppResource(c.App) {
		return ctx, invalidToken()
	}
	if _, err = s.bearerLive(ctx, tx, token, c, now); err != nil {
		return ctx, err
	}
	scopes, _ := ParseOAuthScopes(c.Scope)
	a.bearer = &bearerGrant{scopes: scopes, app: c.App, session: c.Session, exchange: c.Actor != nil}
	a.credential = "oauth:" + c.Session
	return WithVia(ctx, AppVia(c.App)), nil
}

// AppVia is the provenance of what an app does with a person's token.
func AppVia(app string) string { return appViaPrefix + app }

// OAuthIntrospection is /oauth/introspect's answer (RFC 7662): active false
// for anything but a live app token.
func (s *Store) OAuthIntrospection(ctx context.Context, token string, now int64) (map[string]any, error) {
	inactive := map[string]any{"active": false}
	if !s.HostedEnabled() || !LooksLikeJWT(token) {
		return inactive, nil
	}
	c, err := s.verifyAppToken(ctx, s.db, token, now)
	if err != nil {
		return inactive, nil
	}
	if _, err = s.bearerLive(ctx, s.db, token, c, now); err != nil {
		var be *Error
		if errors.As(err, &be) {
			return inactive, nil
		}
		return nil, err
	}
	out := map[string]any{"active": true, "iss": c.Issuer, "sub": c.Subject, "aud": c.Audience, "exp": c.Expires, "iat": c.IssuedAt,
		"jti": c.ID, "scope": c.Scope, "client_id": c.ClientID, "token_type": "Bearer"}
	if c.App != "" {
		out["app"] = c.App
	}
	if c.Actor != nil {
		out["act"] = c.Actor
	}
	return out, nil
}

// OAuthUserinfo is /oauth/userinfo for a live app token with the identity
// scope: {sub, handle, band}.
func (s *Store) OAuthUserinfo(ctx context.Context, token string, now int64) (map[string]any, error) {
	if !s.HostedEnabled() {
		return nil, hostedUnavailable("off")
	}
	c, err := s.verifyAppToken(ctx, s.db, token, now)
	if err != nil {
		return nil, err
	}
	if _, err = s.bearerLive(ctx, s.db, token, c, now); err != nil {
		return nil, err
	}
	scopes, _ := ParseOAuthScopes(c.Scope)
	if !slices.Contains(scopes, "identity") {
		return nil, insufficientScope(scopes)
	}
	var handle string
	if err = s.db.QueryRowContext(ctx, "SELECT handle FROM identities WHERE id=?", c.Subject).Scan(&handle); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	st, err := s.standing(ctx, s.db, actor{id: c.Subject, account: c.Subject, signed: true, hosted: true}, now)
	if err != nil {
		return nil, err
	}
	return map[string]any{"sub": c.Subject, "handle": handle, "band": st.Band.Name()}, nil
}

// OAuthExchangeRequest is a token exchange request (RFC 8693 §2.1).
type OAuthExchangeRequest struct {
	ClientID, SubjectToken, SubjectTokenType, ActorToken, ActorTokenType, Scope, Resource string
}

// actorClaims are an actor token's claims: a JWT the app's own key signed.
type actorClaims struct {
	Issuer   string `json:"iss"`
	Subject  string `json:"sub"`
	Audience string `json:"aud"`
	Expires  int64  `json:"exp"`
	IssuedAt int64  `json:"iat"`
	ID       string `json:"jti"`
}

// OAuthTokenEndpoint is the audience an actor token names.
func (s *Store) OAuthTokenEndpoint() string { return s.issuer() + "/oauth/token" }

// OAuthExchange is the token-exchange grant. The app (client_id, a
// registered client of an app) proves itself with actor_token; the
// subject token is a live app token for a resource the app owns; the new
// token has at most both scope sets, lives at most as long as the subject
// token, and carries act {sub: app:APP}.
func (s *Store) OAuthExchange(ctx context.Context, r OAuthExchangeRequest, now int64) (OAuthTokens, error) {
	if !s.HostedEnabled() {
		return OAuthTokens{}, oauthError("temporarily_unavailable", 400, "Sign-in is off on this server.")
	}
	if r.SubjectTokenType != OAuthTokenTypeAccess && r.SubjectTokenType != OAuthTokenTypeJWT {
		return OAuthTokens{}, oauthError("invalid_request", 400, "subject_token_type is "+OAuthTokenTypeAccess+".")
	}
	if r.ActorTokenType != OAuthTokenTypeJWT || r.ActorToken == "" {
		return OAuthTokens{}, oauthError("invalid_request", 400, "actor_token is a JWT your app's key signed (actor_token_type "+OAuthTokenTypeJWT+").")
	}
	badClient := oauthError("invalid_client", 401, "The app could not be authenticated: actor_token must be a fresh JWT (EdDSA) that a key of the app behind client_id signed, with iss and sub the client_id, aud "+s.OAuthTokenEndpoint()+", a jti used once, and at most "+strconv.Itoa(OAuthActorTokenSeconds)+" seconds between iat and exp.")
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return OAuthTokens{}, err
	}
	defer tx.Rollback()
	client, ok, err := readAppClient(ctx, tx, r.ClientID)
	if err != nil {
		return OAuthTokens{}, err
	}
	if !ok || client.RetiredAt != 0 {
		return OAuthTokens{}, badClient
	}
	actor, err := s.verifyActorToken(ctx, tx, r.ActorToken, client, now)
	if err != nil {
		return OAuthTokens{}, badClient
	}
	sum := sha256.Sum256([]byte(client.ID + "\x00" + actor.ID))
	if _, err = tx.ExecContext(ctx, "DELETE FROM oauth_assertions_used WHERE expires_at<=?", now); err != nil {
		return OAuthTokens{}, err
	}
	res, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO oauth_assertions_used(jti_sha256,expires_at) VALUES(?,?)", hex.EncodeToString(sum[:]), actor.Expires)
	if err != nil {
		return OAuthTokens{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return OAuthTokens{}, badClient
	}
	subject, err := s.verifyAppToken(ctx, tx, r.SubjectToken, now)
	if err == nil {
		_, err = s.bearerLive(ctx, tx, r.SubjectToken, subject, now)
	}
	if err != nil {
		var be *Error
		if errors.As(err, &be) {
			return OAuthTokens{}, invalidGrant()
		}
		return OAuthTokens{}, err
	}
	// Only a token the connection issued is exchanged (no chains), and only
	// by the app that owns the resource it was issued for.
	owner, err := s.resourceApp(ctx, tx, subject.Audience)
	if err != nil {
		return OAuthTokens{}, err
	}
	if subject.Actor != nil || owner != client.App {
		return OAuthTokens{}, invalidGrant()
	}
	have, _ := ParseOAuthScopes(subject.Scope)
	want := have
	if r.Scope != "" {
		if want, ok = ParseOAuthScopes(r.Scope); !ok {
			return OAuthTokens{}, oauthError("invalid_scope", 400, "scope lists app scopes: "+strings.Join(OAuthScopeNames(), ", ")+".")
		}
	}
	var narrowed []string
	for _, w := range want {
		if slices.Contains(have, w) && slices.Contains(client.Scopes, w) {
			narrowed = append(narrowed, w)
		}
	}
	if len(narrowed) != len(want) || len(narrowed) == 0 {
		return OAuthTokens{}, oauthError("invalid_scope", 400, "An exchange only narrows: ask for scopes both the subject token and your client have.")
	}
	audience := s.OAuthAppResource(client.App)
	if r.Resource != "" && r.Resource != audience {
		return OAuthTokens{}, oauthError("invalid_target", 400, "An exchanged token is for "+audience+".")
	}
	idRaw := make([]byte, 16)
	if _, err = rand.Read(idRaw); err != nil {
		return OAuthTokens{}, err
	}
	scope := strings.Join(narrowed, " ")
	claims := BearerClaims{Issuer: s.issuer(), Subject: subject.Subject, Audience: audience, Expires: min(subject.Expires, now+OAuthJWTAccessSeconds),
		IssuedAt: now, ID: hex.EncodeToString(idRaw), Scope: scope, ClientID: client.ID, Session: subject.Session, App: client.App,
		Actor: &BearerActor{Subject: AppVia(client.App), ClientID: client.ID}}
	token, err := s.signJWT(ctx, tx, claims, now)
	if err != nil {
		return OAuthTokens{}, err
	}
	if err = audit(ctx, tx, "oauth.exchange", client.App, subject.Subject, "via "+AppVia(client.App)+" scope "+scope, now); err != nil {
		return OAuthTokens{}, err
	}
	if err = tx.Commit(); err != nil {
		return OAuthTokens{}, err
	}
	return OAuthTokens{AccessToken: token, ExpiresIn: claims.Expires - now, Scope: scope}, nil
}

// verifyActorToken checks an actor token against the keys of the client's
// app: its live main key (not rotated away); never a linked device, which
// manages no sign-in clients either (DeviceMainKeyOps).
func (s *Store) verifyActorToken(ctx context.Context, tx *sql.Tx, token string, client OAuthAppClient, now int64) (actorClaims, error) {
	var c actorClaims
	p, err := ParseJWS(token)
	if err != nil {
		return c, err
	}
	if services.StrictObject(p.payload, &c) != nil || c.Issuer != client.ID || c.Subject != client.ID || c.Audience != s.OAuthTokenEndpoint() ||
		c.Expires <= now || c.IssuedAt > now+oauthClockSkew || c.Expires-c.IssuedAt > OAuthActorTokenSeconds || len(c.ID) < 16 || len(c.ID) > 128 {
		return actorClaims{}, invalidToken()
	}
	rows, err := tx.QueryContext(ctx, "SELECT public_key FROM identities WHERE account=? AND successor='' AND revoked_at=0 AND role<>'device' LIMIT 16", client.App)
	if err != nil {
		return c, err
	}
	defer rows.Close()
	for rows.Next() {
		var encoded string
		if err = rows.Scan(&encoded); err != nil {
			return c, err
		}
		if pub, err := b64.URL(encoded); err == nil && len(pub) == ed25519.PublicKeySize && ed25519.Verify(pub, p.input, p.signature) {
			return c, nil
		}
	}
	if err = rows.Err(); err != nil {
		return c, err
	}
	return actorClaims{}, invalidToken()
}

// SignActorToken is an actor token for clientID signed by key, as an app
// makes it (clients and tests).
func SignActorToken(key ed25519.PrivateKey, clientID, tokenEndpoint string, now int64) string {
	id := make([]byte, 16)
	_, _ = rand.Read(id)
	header, _ := json.Marshal(jwsHeader{Alg: "EdDSA", Typ: "JWT"})
	payload, _ := json.Marshal(actorClaims{Issuer: clientID, Subject: clientID, Audience: tokenEndpoint, Expires: now + 120, IssuedAt: now, ID: hex.EncodeToString(id)})
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	return input + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(input)))
}

// exchangedJTI is the key an exchanged token is revoked under.
func exchangedJTI(c BearerClaims) string {
	sum := sha256.Sum256([]byte("exchanged\x00" + c.ClientID + "\x00" + c.ID))
	return hex.EncodeToString(sum[:])
}

// revokeExchanged is /oauth/revoke of an exchanged token (RFC 7009): a live
// one, by its own client when clientID is given, is refused from then on
// (oauth_exchanged_revoked, kept until the token would have expired). The
// subject's connection is the agent's own and stays. ok false when token
// is no exchanged token of this server.
func (s *Store) revokeExchanged(ctx context.Context, tx *sql.Tx, token, clientID string, now int64) (bool, error) {
	c, err := s.verifyAppToken(ctx, tx, token, now)
	if err != nil || c.Actor == nil {
		var be *Error
		if err == nil || errors.As(err, &be) {
			return false, nil
		}
		return false, err
	}
	if clientID != "" && clientID != c.ClientID {
		return true, nil
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM oauth_exchanged_revoked WHERE expires_at<=?", now); err != nil {
		return true, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO oauth_exchanged_revoked(jti_sha256,expires_at) VALUES(?,?)", exchangedJTI(c), c.Expires); err != nil {
		return true, err
	}
	return true, audit(ctx, tx, "oauth.revoke", c.Subject, c.Subject, "exchanged token via "+AppVia(c.App), now)
}
