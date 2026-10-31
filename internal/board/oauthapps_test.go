package board

// Sign in with SwarmMemo for apps (oauthapps.go, oauthscopes.go,
// oauthjwt.go), at the store: registered clients and their rules, app
// tokens as JWTs within their scopes and never for keys or identity,
// expiry, refresh, revocation, provenance, token exchange that only
// narrows, and signing-key rotation.

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

const appTestIssuer = "https://swarmmemo.com"

// appTestSetup is a store with an app (Hex Arena) that owns a public room
// and has a registered client.
type appTestSetup struct {
	s      *Store
	app    ed25519.PrivateKey
	client OAuthAppClient
}

func newAppTest(t *testing.T, clientData string) appTestSetup {
	t.Helper()
	return newAppTestOn(t, openHostedTest(t), clientData)
}

func newAppTestOn(t *testing.T, s *Store, clientData string) appTestSetup {
	t.Helper()
	s.SetOAuthIssuer(appTestIssuer)
	app := keyFor(181)
	run(t, s, signed(app, Command{Operation: "app.register", Data: appJSON, RequestID: "app-reg"}))
	run(t, s, signed(app, Command{Operation: "room.create", Room: "hex-table", RequestID: "app-room"}))
	r := run(t, s, signed(app, Command{Operation: "oauth.client.register", Data: clientData, RequestID: "client-reg"}))
	return appTestSetup{s: s, app: app, client: r.Data["client"].(OAuthAppClient)}
}

const appClientJSON = `{"schema":1,"name":"Hex Arena web","redirect_uris":["https://hexarena.example/cb"],"scopes":["identity","app-rooms","dm"],"resources":["https://hexarena.example/mcp"]}`

// signIn makes a hosted identity and signs it in to clientID for resource
// with scope, as the authorization page and token endpoint do.
func (a appTestSetup) signIn(t *testing.T, clientID, resource, scope string) (string, OAuthTokens) {
	t.Helper()
	created := createHosted(t, a.s, "test-origin", "")
	account := created["agent"].(string)
	g := OAuthGrant{ClientID: clientID, ClientName: "Hex", RedirectURI: "https://hexarena.example/cb", Challenge: PKCEChallenge(oauthTestVerifier), Resource: resource, Scope: scope}
	code, err := a.s.OAuthIssueCode(testContext, g, account, created["token_id"].(string), testTime)
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := a.s.OAuthExchangeCode(testContext, code, clientID, g.RedirectURI, oauthTestVerifier, "", testTime)
	if err != nil {
		t.Fatalf("code exchange: %v", err)
	}
	return account, tokens
}

// bearer runs c as /v1/command does with an app token: signed by the
// identity's key through BearerSigner, with the token in ctx.
func bearer(s *Store, token string, c Command) (Result, error) {
	now := s.now().Unix()
	key, pub, _, err := s.BearerSigner(testContext, token, now)
	if err != nil {
		return Result{}, err
	}
	c.PublicKey, c.Timestamp, c.Nonce = pub, now, randomID()
	c.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, Canonical("swarmmemo.com", c)))
	return s.Execute(WithBearerToken(testContext, token), c, "test-origin")
}

func appErrCode(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	var oe *OAuthError
	if errors.As(err, &oe) {
		return oe.Code
	}
	if err == nil {
		return "ok"
	}
	return err.Error()
}

// jwtClaims reads a JWT's claims without checking it.
func jwtClaims(t *testing.T, token string) (map[string]any, map[string]any) {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT: %q", token)
	}
	var header, claims map[string]any
	h, _ := base64.RawURLEncoding.DecodeString(parts[0])
	p, _ := base64.RawURLEncoding.DecodeString(parts[1])
	if json.Unmarshal(h, &header) != nil || json.Unmarshal(p, &claims) != nil {
		t.Fatalf("JWT parts are not JSON: %s", token)
	}
	return header, claims
}

func TestOAuthAppClientRegistration(t *testing.T) {
	s := openHostedTest(t)
	s.SetOAuthIssuer(appTestIssuer)
	app, other := keyFor(182), keyFor(183)
	register(t, s, other)
	fails(t, s, signed(app, Command{Operation: "oauth.client.register", Data: appClientJSON, RequestID: "c0"}), "app_required")
	run(t, s, signed(app, Command{Operation: "app.register", Data: appJSON, RequestID: "a"}))
	for i, bad := range map[string]string{
		"invalid_client_data:hosted":   `{"schema":1,"name":"X","redirect_uris":["https://hexarena.example/cb"],"scopes":["hosted"]}`,
		"invalid_client_data:twice":    `{"schema":1,"name":"X","redirect_uris":["https://hexarena.example/cb"],"scopes":["dm","dm"]}`,
		"invalid_client_data:none":     `{"schema":1,"name":"X","redirect_uris":[],"scopes":["dm"]}`,
		"invalid_client_data:fragment": `{"schema":1,"name":"X","redirect_uris":["https://hexarena.example/cb#x"],"scopes":["dm"]}`,
		"invalid_client_data:offhost":  `{"schema":1,"name":"X","redirect_uris":["https://hexarena.example/cb"],"scopes":["dm"],"resources":["https://evil.example/mcp"]}`,
		"invalid_client_data:field":    `{"schema":1,"name":"X","redirect_uris":["https://hexarena.example/cb"],"scopes":["dm"],"secret":"x"}`,
		"redirect_not_allowed:dev":     `{"schema":1,"name":"X","redirect_uris":["http://localhost:5173/cb"],"scopes":["dm"]}`,
	} {
		code, _, _ := strings.Cut(i, ":")
		fails(t, s, signed(app, Command{Operation: "oauth.client.register", Data: bad, RequestID: "bad-" + i}), code)
	}
	dev := `{"schema":1,"name":"Hex","redirect_uris":["https://hexarena.example/cb","http://localhost:5173/cb"],"scopes":["identity","app-rooms"],"resources":["https://play.hexarena.example/mcp"],"dev_redirects":true}`
	c := run(t, s, signed(app, Command{Operation: "oauth.client.register", Data: dev, RequestID: "c1"})).Data["client"].(OAuthAppClient)
	if !strings.HasPrefix(c.ID, OAuthAppClientPrefix) || c.App != keyID(app) || len(c.LiveRedirectURIs()) != 2 || !c.DevRedirects {
		t.Fatalf("client = %+v", c)
	}
	// Production: the same client without dev redirects stops matching localhost.
	prod := `{"schema":1,"name":"Hex","redirect_uris":["https://hexarena.example/cb"],"scopes":["identity","app-rooms"],"resources":["https://play.hexarena.example/mcp"]}`
	c = run(t, s, signed(app, Command{Operation: "oauth.client.register", Target: c.ID, Data: prod, RequestID: "c2"})).Data["client"].(OAuthAppClient)
	if got := c.LiveRedirectURIs(); len(got) != 1 || got[0] != "https://hexarena.example/cb" {
		t.Fatalf("live redirects = %v", got)
	}
	// One resource belongs to one app.
	run(t, s, signed(other, Command{Operation: "app.register", Data: `{"schema":1,"name":"Squatter","homepage":"https://hexarena.example"}`, RequestID: "o"}))
	fails(t, s, signed(other, Command{Operation: "oauth.client.register", Data: prod, RequestID: "o1"}), "resource_taken")
	// No app serves one of SwarmMemo's own resources (they name their app).
	spoof := keyFor(189)
	run(t, s, signed(spoof, Command{Operation: "app.register", Data: `{"schema":1,"name":"Spoof","homepage":"https://swarmmemo.com"}`, RequestID: "sp"}))
	fails(t, s, signed(spoof, Command{Operation: "oauth.client.register", Data: `{"schema":1,"name":"s","redirect_uris":["https://swarmmemo.com/cb"],"scopes":["dm"],"resources":["` + s.OAuthAppResource(keyID(app)) + `"]}`, RequestID: "sp1"}), "invalid_client_data")
	// Another app cannot change or retire this one's client.
	fails(t, s, signed(other, Command{Operation: "oauth.client.retire", Target: c.ID, RequestID: "o2"}), "client_not_found")
	// At most 8 live clients.
	for i := 0; i < OAuthAppClientsMax-1; i++ {
		run(t, s, signed(app, Command{Operation: "oauth.client.register", Data: `{"schema":1,"name":"n","redirect_uris":["https://hexarena.example/cb"],"scopes":["dm"]}`, RequestID: "n" + string(rune('a'+i))}))
	}
	fails(t, s, signed(app, Command{Operation: "oauth.client.register", Data: `{"schema":1,"name":"n","redirect_uris":["https://hexarena.example/cb"],"scopes":["dm"]}`, RequestID: "n9"}), "client_limit")
	list := run(t, s, signed(app, Command{Operation: "oauth.clients.list"})).Data["clients"].([]OAuthAppClient)
	if len(list) != OAuthAppClientsMax {
		t.Fatalf("clients.list = %d", len(list))
	}
	retired := run(t, s, signed(app, Command{Operation: "oauth.client.retire", Target: c.ID, RequestID: "r"})).Data["client"].(OAuthAppClient)
	if retired.RetiredAt == 0 {
		t.Fatal("retire did not retire")
	}
	fails(t, s, signed(app, Command{Operation: "oauth.client.retire", Target: c.ID, RequestID: "r2"}), "client_not_found")
	if _, _, ok, _ := s.OAuthAppClient(testContext, c.ID); ok {
		t.Fatal("a retired client still signs in")
	}
}

func TestAppTokenScopesAndProvenance(t *testing.T) {
	a := newAppTest(t, appClientJSON)
	s := a.s
	account, tokens := a.signIn(t, a.client.ID, s.OAuthAppResource(keyID(a.app)), "identity app-rooms")
	header, claims := jwtClaims(t, tokens.AccessToken)
	if header["alg"] != "EdDSA" || header["typ"] != "at+jwt" || claims["sub"] != account || claims["aud"] != appTestIssuer+"/app/"+keyID(a.app) ||
		claims["scope"] != "identity app-rooms" || claims["app"] != keyID(a.app) || tokens.ExpiresIn != OAuthJWTAccessSeconds || tokens.RefreshToken == "" {
		t.Fatalf("app token: %v %v %+v", header, claims, tokens)
	}
	// Allowed: a post in the app's room, marked via app:APP.
	res, err := bearer(s, tokens.AccessToken, Command{Operation: "post", Room: "hex-table", Text: "my move", RequestID: "p1"})
	if err != nil {
		t.Fatalf("post in the app's room: %v", err)
	}
	m := run(t, s, Command{Operation: "message.get", MessageID: res.Receipt.ID}).Messages[0]
	if m.Via != "app:"+keyID(a.app) || m.Author != account {
		t.Fatalf("message via %q author %q", m.Via, m.Author)
	}
	if _, err := bearer(s, tokens.AccessToken, Command{Operation: "agent.get", Target: account}); err != nil {
		t.Fatalf("agent.get self: %v", err)
	}
	// The same post again is the same receipt (request_id).
	again, err := bearer(s, tokens.AccessToken, Command{Operation: "post", Room: "hex-table", Text: "my move", RequestID: "p1"})
	if err == nil && again.Receipt.ID != res.Receipt.ID {
		t.Fatalf("retry made a second post")
	}
	// Refused: outside the scopes, and every key or identity operation.
	for name, c := range map[string]Command{
		"another room":    {Operation: "post", Room: "lobby", Text: "hi", RequestID: "x1"},
		"a handle":        {Operation: "post", Room: "hex-table", Text: "hi", Handle: "taken-over", RequestID: "x2"},
		"another agent":   {Operation: "agent.get", Target: keyID(a.app)},
		"wake-ups":        {Operation: "service.call", Target: "wakeup", Data: `{"schema":1,"method":"list"}`, RequestID: "x3"},
		"rotate":          {Operation: "agent.rotate", Target: strings.Repeat("A", 43), RequestID: "x4"},
		"handle":          {Operation: "agent.register", Handle: "new-name", RequestID: "x5"},
		"tokens":          {Operation: "hosted.token", Data: `{"schema":1,"action":"create"}`, RequestID: "x6"},
		"claim":           {Operation: "hosted.claim", Data: `{"schema":1,"new_public_key":"x","proof":"y","recovery_code":"z"}`, RequestID: "x7"},
		"transfer":        {Operation: "credit.transfer", Target: keyID(a.app), Amount: 1, RequestID: "x8"},
		"device":          {Operation: "device.revoke", Target: keyID(a.app), RequestID: "x9"},
		"delegation":      {Operation: "delegations.list"},
		"link":            {Operation: "identity.link", Data: `{"schema":1}`, RequestID: "x10"},
		"connections":     {Operation: "oauth.connections.list"},
		"sign out others": {Operation: "oauth.connection.revoke", Target: "x", RequestID: "x11"},
		"spend limit":     {Operation: "spend_limit.set", Target: "x", Data: `{"schema":1}`, RequestID: "x12"},
		"app register":    {Operation: "app.register", Data: appJSON, RequestID: "x13"},
		"key backup":      {Operation: "key.backup.delete", RequestID: "x14"},
	} {
		if _, err := bearer(s, tokens.AccessToken, c); appErrCode(err) != "insufficient_scope" {
			t.Errorf("%s (%s): %s, want insufficient_scope", name, c.Operation, appErrCode(err))
		}
	}
	// Expiry: past exp the token is refused at the door and in the command.
	s.now = func() time.Time { return time.Unix(testTime+OAuthJWTAccessSeconds, 0) }
	if _, err := bearer(s, tokens.AccessToken, Command{Operation: "room.get", Room: "hex-table"}); appErrCode(err) != "invalid_token" {
		t.Fatalf("expired token: %s", appErrCode(err))
	}
	s.now = func() time.Time { return time.Unix(testTime, 0) }
	// Refresh: a narrower token is fine, a wider one is not; the old
	// access token stops working.
	if _, err := s.OAuthRefresh(testContext, tokens.RefreshToken, a.client.ID, "", "identity app-rooms wakeups", testTime); appErrCode(err) != "invalid_scope" {
		t.Fatalf("wider refresh: %s", appErrCode(err))
	}
	narrow, err := s.OAuthRefresh(testContext, tokens.RefreshToken, a.client.ID, "", "identity", testTime)
	if err != nil || narrow.Scope != "identity" {
		t.Fatalf("narrow refresh: %+v %v", narrow, err)
	}
	if _, err := bearer(s, tokens.AccessToken, Command{Operation: "room.get", Room: "hex-table"}); appErrCode(err) != "invalid_token" {
		t.Fatalf("rotated access token: %s", appErrCode(err))
	}
	if _, err := bearer(s, narrow.AccessToken, Command{Operation: "room.get", Room: "hex-table"}); appErrCode(err) != "insufficient_scope" {
		t.Fatalf("narrowed token in a room: %s", appErrCode(err))
	}
	// Userinfo and introspection.
	info, err := s.OAuthUserinfo(testContext, narrow.AccessToken, testTime)
	if err != nil || info["sub"] != account || info["band"] == nil {
		t.Fatalf("userinfo: %v %v", info, err)
	}
	if in, _ := s.OAuthIntrospection(testContext, narrow.AccessToken, testTime); in["active"] != true || in["sub"] != account {
		t.Fatalf("introspection: %v", in)
	}
	// The person lists the connection and signs the app out with their own key.
	key := hostedKeyOf(t, s, account)
	conns := run(t, s, signed(key, Command{Operation: "oauth.connections.list"})).Data["connections"].([]OAuthConnection)
	if len(conns) != 1 || conns[0].App != keyID(a.app) || conns[0].AppName != "Hex Arena" || conns[0].Scope != "identity app-rooms" {
		t.Fatalf("connections = %+v", conns)
	}
	fails(t, s, signed(keyFor(184), Command{Operation: "oauth.connection.revoke", Target: conns[0].ID, RequestID: "nope"}), "connection_not_found")
	run(t, s, signed(key, Command{Operation: "oauth.connection.revoke", Target: conns[0].ID, RequestID: "rv"}))
	if _, err := bearer(s, narrow.AccessToken, Command{Operation: "agent.get", Target: account}); appErrCode(err) != "invalid_token" {
		t.Fatalf("token after sign-out: %s", appErrCode(err))
	}
	if in, _ := s.OAuthIntrospection(testContext, narrow.AccessToken, testTime); in["active"] != false {
		t.Fatalf("introspection after sign-out: %v", in)
	}
}

// hostedKeyOf opens a hosted identity's key, as its own tools sign.
func hostedKeyOf(t *testing.T, s *Store, account string) ed25519.PrivateKey {
	t.Helper()
	var sealed, kek string
	if err := s.db.QueryRow("SELECT sealed_private_key,kek_id FROM hosted_keys WHERE account=?", account).Scan(&sealed, &kek); err != nil {
		t.Fatal(err)
	}
	key, err := s.openSeed(account, sealed, kek)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestAppTokenDMScope(t *testing.T) {
	a := newAppTest(t, appClientJSON)
	s := a.s
	account, tokens := a.signIn(t, a.client.ID, s.OAuthAppResource(keyID(a.app)), "dm")
	friend, crowd := keyFor(185), keyFor(186)
	register(t, s, friend)
	register(t, s, crowd)
	run(t, s, signed(a.app, Command{Operation: "room.create", Room: "hex-dm", Visibility: "private", Members: []string{account, keyID(friend)}, RequestID: "dm"}))
	run(t, s, signed(a.app, Command{Operation: "room.create", Room: "hex-group", Visibility: "private", Members: []string{account, keyID(friend), keyID(crowd)}, RequestID: "grp"}))
	if _, err := bearer(s, tokens.AccessToken, Command{Operation: "post", Room: "hex-dm", Text: "psst", Visibility: "private", RequestID: "d1"}); err != nil {
		t.Fatalf("dm post: %v", err)
	}
	for room, c := range map[string]Command{
		"three people": {Operation: "post", Room: "hex-group", Text: "hi", Visibility: "private", RequestID: "d2"},
		"public room":  {Operation: "post", Room: "hex-table", Text: "hi", RequestID: "d3"},
	} {
		if _, err := bearer(s, tokens.AccessToken, c); appErrCode(err) != "insufficient_scope" {
			t.Errorf("dm scope, %s: %s", room, appErrCode(err))
		}
	}
}

func TestTokenExchangeOnlyNarrows(t *testing.T) {
	a := newAppTest(t, appClientJSON)
	s := a.s
	endpoint := s.OAuthTokenEndpoint()
	// An agent signs in to the app's MCP server with a dynamic client.
	account, subject := a.signIn(t, "smcl_agent", "https://hexarena.example/mcp", "identity app-rooms")
	if _, claims := jwtClaims(t, subject.AccessToken); claims["aud"] != "https://hexarena.example/mcp" || claims["app"] != keyID(a.app) {
		t.Fatalf("subject claims %v", claims)
	}
	// Not good on SwarmMemo itself: it is for the app's server.
	if _, err := bearer(s, subject.AccessToken, Command{Operation: "room.get", Room: "hex-table"}); appErrCode(err) != "invalid_token" {
		t.Fatalf("subject token on SwarmMemo: %s", appErrCode(err))
	}
	req := func(actor, scope string) OAuthExchangeRequest {
		return OAuthExchangeRequest{ClientID: a.client.ID, SubjectToken: subject.AccessToken, SubjectTokenType: OAuthTokenTypeAccess, ActorToken: actor, ActorTokenType: OAuthTokenTypeJWT, Scope: scope}
	}
	for name, try := range map[string]struct {
		r    OAuthExchangeRequest
		code string
	}{
		"wider than the subject": {req(SignActorToken(a.app, a.client.ID, endpoint, testTime), "identity app-rooms wakeups"), "invalid_scope"},
		"not in the subject":     {req(SignActorToken(a.app, a.client.ID, endpoint, testTime), "dm"), "invalid_scope"},
		"stranger's key":         {req(SignActorToken(keyFor(187), a.client.ID, endpoint, testTime), ""), "invalid_client"},
		"other audience":         {req(SignActorToken(a.app, a.client.ID, "https://evil.example/token", testTime), ""), "invalid_client"},
		"stale actor":            {req(SignActorToken(a.app, a.client.ID, endpoint, testTime-600), ""), "invalid_client"},
		"no actor":               {req("", ""), "invalid_request"},
	} {
		if _, err := s.OAuthExchange(testContext, try.r, testTime); appErrCode(err) != try.code {
			t.Errorf("%s: %s, want %s", name, appErrCode(err), try.code)
		}
	}
	actor := SignActorToken(a.app, a.client.ID, endpoint, testTime)
	got, err := s.OAuthExchange(testContext, req(actor, "app-rooms"), testTime)
	if err != nil || got.Scope != "app-rooms" || got.RefreshToken != "" {
		t.Fatalf("exchange: %+v %v", got, err)
	}
	if _, err := s.OAuthExchange(testContext, req(actor, "app-rooms"), testTime); appErrCode(err) != "invalid_client" {
		t.Fatalf("a replayed actor token: %s", appErrCode(err))
	}
	_, claims := jwtClaims(t, got.AccessToken)
	act, _ := claims["act"].(map[string]any)
	if claims["sub"] != account || claims["aud"] != s.OAuthAppResource(keyID(a.app)) || act["sub"] != "app:"+keyID(a.app) || int64(claims["exp"].(float64)) > testTime+OAuthJWTAccessSeconds {
		t.Fatalf("exchanged claims %v", claims)
	}
	// An exchanged token is not exchanged again.
	again := req(SignActorToken(a.app, a.client.ID, endpoint, testTime), "")
	again.SubjectToken = got.AccessToken
	if _, err := s.OAuthExchange(testContext, again, testTime); appErrCode(err) != "invalid_grant" {
		t.Fatalf("chained exchange: %s", appErrCode(err))
	}
	// Another app cannot exchange a token for this app's resource.
	other := keyFor(188)
	run(t, s, signed(other, Command{Operation: "app.register", Data: `{"schema":1,"name":"Other","homepage":"https://other.example"}`, RequestID: "oa"}))
	oc := run(t, s, signed(other, Command{Operation: "oauth.client.register", Data: `{"schema":1,"name":"o","redirect_uris":["https://other.example/cb"],"scopes":["identity","app-rooms"]}`, RequestID: "oc"})).Data["client"].(OAuthAppClient)
	steal := OAuthExchangeRequest{ClientID: oc.ID, SubjectToken: subject.AccessToken, SubjectTokenType: OAuthTokenTypeAccess, ActorToken: SignActorToken(other, oc.ID, endpoint, testTime), ActorTokenType: OAuthTokenTypeJWT}
	if _, err := s.OAuthExchange(testContext, steal, testTime); appErrCode(err) != "invalid_grant" {
		t.Fatalf("another app's exchange: %s", appErrCode(err))
	}
	// The app posts as the player, and the post says the app relayed it.
	res, err := bearer(s, got.AccessToken, Command{Operation: "post", Room: "hex-table", Text: "gg", RequestID: "ex1"})
	if err != nil {
		t.Fatalf("exchanged post: %v", err)
	}
	m := run(t, s, Command{Operation: "message.get", MessageID: res.Receipt.ID}).Messages[0]
	if m.Author != account || m.Via != "app:"+keyID(a.app) {
		t.Fatalf("exchanged post author %s via %s", m.Author, m.Via)
	}
	var n int
	if err = s.db.QueryRow("SELECT count(*) FROM audit WHERE operation='oauth.exchange' AND detail LIKE ?", "%via app:"+keyID(a.app)+"%").Scan(&n); err != nil || n != 1 {
		t.Fatalf("exchange audit rows %d %v", n, err)
	}
	// Ending the subject's connection ends the exchanged token.
	if err = s.OAuthRevoke(testContext, subject.RefreshToken, "", testTime); err != nil {
		t.Fatal(err)
	}
	if _, err := bearer(s, got.AccessToken, Command{Operation: "room.get", Room: "hex-table"}); appErrCode(err) != "invalid_token" {
		t.Fatalf("exchanged token after its sign-in ended: %s", appErrCode(err))
	}
}

func TestAppTokenSigningKeyRotation(t *testing.T) {
	a := newAppTest(t, appClientJSON)
	s := a.s
	_, tokens := a.signIn(t, a.client.ID, s.OAuthAppResource(keyID(a.app)), "identity")
	header, _ := jwtClaims(t, tokens.AccessToken)
	first := header["kid"].(string)
	keys, err := s.OAuthJWKS(testContext, testTime)
	if err != nil || len(keys) != 1 || keys[0].KeyID != first || keys[0].Curve != "Ed25519" || keys[0].Alg != "EdDSA" {
		t.Fatalf("JWKS = %+v %v", keys, err)
	}
	// The kid is the key's RFC 7638 thumbprint.
	x, _ := base64.RawURLEncoding.DecodeString(keys[0].X)
	if jwkThumbprint(ed25519.PublicKey(x)) != first {
		t.Fatal("kid is not the thumbprint")
	}
	second, err := s.OAuthRotateSigningKey(testContext, testTime+60)
	if err != nil || second == first {
		t.Fatalf("rotate: %q %v", second, err)
	}
	if keys, _ = s.OAuthJWKS(testContext, testTime+60); len(keys) != 2 || keys[0].KeyID != second {
		t.Fatalf("JWKS after rotation = %+v", keys)
	}
	// The token the retired key signed still verifies within the grace.
	if _, err := s.OAuthUserinfo(testContext, tokens.AccessToken, testTime+60); err != nil {
		t.Fatalf("a token of the retired key: %v", err)
	}
	refreshed, err := s.OAuthRefresh(testContext, tokens.RefreshToken, a.client.ID, "", "", testTime+60)
	if err != nil {
		t.Fatal(err)
	}
	if h, _ := jwtClaims(t, refreshed.AccessToken); h["kid"] != second {
		t.Fatalf("new tokens are signed by %v, want %s", h["kid"], second)
	}
	// Past the grace the retired key leaves the JWKS and verifies nothing.
	late := testTime + 60 + OAuthSigningKeyGraceSeconds + 1
	if keys, _ = s.OAuthJWKS(testContext, late); len(keys) != 1 || keys[0].KeyID != second {
		t.Fatalf("JWKS past the grace = %+v", keys)
	}
	if _, err := s.verifyAppToken(testContext, s.db, tokens.AccessToken, testTime+60+OAuthSigningKeyGraceSeconds+1); appErrCode(err) != "invalid_token" {
		t.Fatalf("retired key past grace: %s", appErrCode(err))
	}
	// The key rotates by itself once it is old.
	tx, err := s.db.BeginTx(testContext, nil)
	if err != nil {
		t.Fatal(err)
	}
	third, key, err := s.oauthSigner(context.Background(), tx, testTime+60+OAuthSigningKeySeconds)
	clear(key)
	if err != nil || third == second {
		t.Fatalf("automatic rotation: %q %v", third, err)
	}
	_ = tx.Rollback()
}

// Every app scope's operations are ones a bearer token may be granted, and
// no bearer token is ever valid for key, identity, device, delegation,
// recovery or credential operations.
func TestBearerNeverTouchesKeysOrIdentity(t *testing.T) {
	for _, spec := range OAuthScopes() {
		for _, op := range spec.Operations {
			if bearerNever(op) {
				t.Errorf("scope %s lists %s, which no bearer token may run", spec.Name, op)
			}
		}
	}
	for _, op := range []string{"agent.rotate", "agent.register", "device.add", "device.revoke", "device.request", "key.backup.put", "key.backup.delete",
		"identity.link", "identity.unlink", "delegation.create", "delegation.revoke", "private_read.create", "hosted.create", "hosted.recover",
		"hosted.token", "hosted.claim", "spend_limit.set", "credit.transfer", "allowance.transfer", "room.owner.transfer", "app.register",
		"app.house.create", "app.owner.release", "oauth.client.register", "oauth.client.retire", "oauth.connection.revoke", "oauth.connections.list",
		"account.export", "credits.topup", "webhook.create", "push.subscribe", "vouch", "no.such.operation"} {
		if !bearerNever(op) {
			t.Errorf("a bearer token could run %s", op)
		}
	}
}

func FuzzParseJWS(f *testing.F) {
	f.Add("eyJhbGciOiJFZERTQSJ9.e30." + strings.Repeat("A", 86))
	f.Add("a.b.c")
	f.Add("..")
	f.Add(SignActorToken(keyFor(9), "smapp_x", "https://swarmmemo.com/oauth/token", testTime))
	f.Fuzz(func(t *testing.T, token string) {
		p, err := ParseJWS(token)
		if err == nil && (p.header.Alg != "EdDSA" || len(p.signature) != ed25519.SignatureSize || len(token) > OAuthJWTBytes) {
			t.Fatalf("accepted %q", token)
		}
		var claims BearerClaims
		_ = json.Unmarshal(p.payload, &claims)
		_, _ = ParseOAuthScopes(token)
	})
}

// The identity scope reads who you are as anyone does: never your own
// messaging settings (inbound rules, screening, block list), which only
// your own key or hosted token reads back.
func TestIdentityScopeHidesOwnSettings(t *testing.T) {
	a := newAppTest(t, appClientJSON)
	s := a.s
	account, tokens := a.signIn(t, a.client.ID, s.OAuthAppResource(keyID(a.app)), "identity")
	own := run(t, s, signed(hostedKeyOf(t, s, account), Command{Operation: "agent.get"}))
	if own.Agent == nil || own.Agent.Messaging == nil || len(own.Agent.Messaging.Settings) == 0 {
		t.Fatalf("own read lacks settings: %+v", own.Agent)
	}
	res, err := bearer(s, tokens.AccessToken, Command{Operation: "agent.get"})
	if err != nil {
		t.Fatalf("agent.get with identity: %v", err)
	}
	if res.Agent == nil || res.Agent.ID != account {
		t.Fatalf("agent.get answered %+v", res.Agent)
	}
	if res.Agent.Messaging != nil && len(res.Agent.Messaging.Settings) != 0 {
		t.Fatalf("an app's token read the person's messaging settings: %s", res.Agent.Messaging.Settings)
	}
}

// Retiring the client that exchanged a token ends that token too: "every
// sign-in made through it ends", exchanged ones included.
func TestRetiredClientEndsExchangedTokens(t *testing.T) {
	a := newAppTest(t, appClientJSON)
	s := a.s
	_, subject := a.signIn(t, "smcl_agent", "https://hexarena.example/mcp", "identity app-rooms")
	got, err := s.OAuthExchange(testContext, OAuthExchangeRequest{ClientID: a.client.ID, SubjectToken: subject.AccessToken, SubjectTokenType: OAuthTokenTypeAccess,
		ActorToken: SignActorToken(a.app, a.client.ID, s.OAuthTokenEndpoint(), testTime), ActorTokenType: OAuthTokenTypeJWT}, testTime)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if _, err := bearer(s, got.AccessToken, Command{Operation: "room.get", Room: "hex-table"}); err != nil {
		t.Fatalf("exchanged token: %v", err)
	}
	run(t, s, signed(a.app, Command{Operation: "oauth.client.retire", Target: a.client.ID, RequestID: "retire"}))
	if _, err := bearer(s, got.AccessToken, Command{Operation: "room.get", Room: "hex-table"}); appErrCode(err) != "invalid_token" {
		t.Fatalf("exchanged token after its client retired: %s", appErrCode(err))
	}
	if in, _ := s.OAuthIntrospection(testContext, got.AccessToken, testTime); in["active"] != false {
		t.Fatalf("introspection after retire: %v", in)
	}
}
