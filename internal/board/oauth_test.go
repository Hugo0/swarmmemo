package board

// OAuth for the assistant profile (oauth.go), at the store: codes are single
// use, expire, and bind the client, redirect URI, PKCE verifier and
// resource; access tokens are hosted tokens bound to their resource and
// expiring; refresh tokens rotate and a reused one ends the connection;
// every revocation path ends the connection too.

import (
	"context"
	"strings"
	"testing"
)

const oauthTestResource = "https://swarmmemo.com/mcp/assistant"

var oauthTestVerifier = strings.Repeat("v", 50)

func oauthTestGrant() OAuthGrant {
	return OAuthGrant{ClientID: "smcl_test", ClientName: "Test App", RedirectURI: "https://app.example/cb", Challenge: PKCEChallenge(oauthTestVerifier), Resource: oauthTestResource, Scope: OAuthScope}
}

// oauthConnect creates a hosted identity and exchanges a code for tokens.
func oauthConnect(t *testing.T, s *Store) (account string, tokens OAuthTokens) {
	t.Helper()
	created := createHosted(t, s, "test-origin", "")
	account = created["agent"].(string)
	code, err := s.OAuthIssueCode(testContext, oauthTestGrant(), account, created["token_id"].(string), testTime)
	if err != nil {
		t.Fatal(err)
	}
	if tokens, err = s.OAuthExchangeCode(testContext, code, "smcl_test", "https://app.example/cb", oauthTestVerifier, oauthTestResource, testTime); err != nil {
		t.Fatal(err)
	}
	return account, tokens
}

func audience(resource string) context.Context { return WithTokenAudience(testContext, resource) }

func isGrantError(err error, code string) bool {
	oe, ok := err.(*OAuthError)
	return ok && oe.Code == code
}

func TestOAuthCodeBindings(t *testing.T) {
	s := openHostedTest(t)
	created := createHosted(t, s, "test-origin", "")
	account := created["agent"].(string)
	issue := func() string {
		t.Helper()
		code, err := s.OAuthIssueCode(testContext, oauthTestGrant(), account, "", testTime)
		if err != nil || !strings.HasPrefix(code, oauthCodePrefix) {
			t.Fatalf("issue: %q %v", code, err)
		}
		return code
	}
	for name, try := range map[string]func(code string) error{
		"wrong verifier": func(code string) error {
			_, err := s.OAuthExchangeCode(testContext, code, "smcl_test", "https://app.example/cb", strings.Repeat("w", 50), "", testTime)
			return err
		},
		"no verifier": func(code string) error {
			_, err := s.OAuthExchangeCode(testContext, code, "smcl_test", "https://app.example/cb", "", "", testTime)
			return err
		},
		"other client": func(code string) error {
			_, err := s.OAuthExchangeCode(testContext, code, "smcl_other", "https://app.example/cb", oauthTestVerifier, "", testTime)
			return err
		},
		"other redirect": func(code string) error {
			_, err := s.OAuthExchangeCode(testContext, code, "smcl_test", "https://app.example/cb/", oauthTestVerifier, "", testTime)
			return err
		},
		"other resource": func(code string) error {
			_, err := s.OAuthExchangeCode(testContext, code, "smcl_test", "https://app.example/cb", oauthTestVerifier, "https://swarmmemo.com/mcp", testTime)
			return err
		},
		"expired": func(code string) error {
			_, err := s.OAuthExchangeCode(testContext, code, "smcl_test", "https://app.example/cb", oauthTestVerifier, "", testTime+OAuthCodeSeconds)
			return err
		},
	} {
		code := issue()
		if err := try(code); !isGrantError(err, "invalid_grant") {
			t.Fatalf("%s: %v", name, err)
		}
		// Any attempt spends the code: the right verifier cannot follow.
		if _, err := s.OAuthExchangeCode(testContext, code, "smcl_test", "https://app.example/cb", oauthTestVerifier, "", testTime); !isGrantError(err, "invalid_grant") {
			t.Fatalf("%s: the code still worked after a failed attempt: %v", name, err)
		}
	}
	// Within ten minutes, with everything right, it works once.
	code := issue()
	tokens, err := s.OAuthExchangeCode(testContext, code, "smcl_test", "https://app.example/cb", oauthTestVerifier, "", testTime+OAuthCodeSeconds-1)
	if err != nil || !strings.HasPrefix(tokens.AccessToken, HostedTokenPrefix) || !strings.HasPrefix(tokens.RefreshToken, OAuthRefreshPrefix) || tokens.ExpiresIn != OAuthAccessSeconds || tokens.Scope != OAuthScope {
		t.Fatalf("exchange: %+v %v", tokens, err)
	}
	// A replayed code is refused and ends the connection it bought.
	if _, err = s.OAuthExchangeCode(testContext, code, "smcl_test", "https://app.example/cb", oauthTestVerifier, "", testTime); !isGrantError(err, "invalid_grant") {
		t.Fatalf("replay: %v", err)
	}
	if _, err = s.HostedAccount(audience(oauthTestResource), tokens.AccessToken, testTime); !isCode(err, "hosted_token_invalid") {
		t.Fatalf("a replayed code left its access token working: %v", err)
	}
	if _, err = s.OAuthRefresh(testContext, tokens.RefreshToken, "smcl_test", "", "", testTime); !isGrantError(err, "invalid_grant") {
		t.Fatalf("a replayed code left its refresh token working: %v", err)
	}
	if OAuthCodeSeconds > 600 {
		t.Fatalf("codes live %d s; at most 10 minutes", OAuthCodeSeconds)
	}
	// Only an active hosted identity gets a code.
	if _, err = s.OAuthIssueCode(testContext, oauthTestGrant(), keyID(keyFor(7)), "", testTime); !isCode(err, "hosted_token_invalid") {
		t.Fatalf("a code for a keyed agent: %v", err)
	}
}

// The access token is a hosted token: it signs as the identity on its own
// resource, in a header (a non-empty audience), until it expires, and
// whoami lists it with its client's label; hosted.create's unshown first
// token is revoked.
func TestOAuthAccessTokenIsAHostedToken(t *testing.T) {
	s := openHostedTest(t)
	account, tokens := oauthConnect(t, s)
	got, err := s.HostedAccount(audience(oauthTestResource), tokens.AccessToken, testTime)
	if err != nil || got != account {
		t.Fatalf("access token: %q %v", got, err)
	}
	for name, ctx := range map[string]context.Context{"path (no audience)": testContext, "the full profile": audience("https://swarmmemo.com/mcp"), "another origin": audience("https://evil.example/mcp/assistant")} {
		if _, err = s.HostedAccount(ctx, tokens.AccessToken, testTime); !isCode(err, "hosted_token_invalid") {
			t.Fatalf("%s accepted an OAuth token: %v", name, err)
		}
	}
	if _, err = s.HostedAccount(audience(oauthTestResource), tokens.AccessToken, testTime+OAuthAccessSeconds); !isCode(err, "hosted_token_invalid") {
		t.Fatalf("an expired access token: %v", err)
	}
	key, _, err := s.HostedSigner(audience(oauthTestResource), tokens.AccessToken, testTime)
	if err != nil {
		t.Fatal(err)
	}
	listed := run(t, s, signed(key, Command{Operation: "hosted.token", Data: `{"schema":1,"action":"list"}`}))
	list := listed.Data["tokens"].([]HostedToken)
	if len(list) != 1 || list[0].Label != "oauth: Test App" {
		t.Fatalf("whoami tokens: %+v", list)
	}
	// OAuth connections do not use up the identity's own token slots.
	for i := 0; i < HostedTokensMax; i++ {
		run(t, s, signed(key, Command{Operation: "hosted.token", Data: `{"schema":1,"action":"create","label":"own"}`}))
	}
	fails(t, s, signed(key, Command{Operation: "hosted.token", Data: `{"schema":1,"action":"create"}`}), "token_limit")
	if n := len(run(t, s, signed(key, Command{Operation: "hosted.token", Data: `{"schema":1,"action":"list"}`})).Data["tokens"].([]HostedToken)); n != HostedTokensMax+1 {
		t.Fatalf("whoami lists %d tokens, want the 4 own and the OAuth one", n)
	}
	var stored int
	if err = s.db.QueryRow("SELECT count(*) FROM oauth_families WHERE access_sha256=? OR refresh_sha256=?", hostedHash(tokens.AccessToken), hostedHash(tokens.RefreshToken)).Scan(&stored); err != nil || stored != 1 {
		t.Fatalf("tokens are not stored hashed: %d %v", stored, err)
	}
	for _, table := range []string{"oauth_families", "oauth_codes", "hosted_tokens", "oauth_refresh_used"} {
		var plain int
		if err = s.db.QueryRow("SELECT count(*) FROM "+table+" WHERE instr(CAST(("+columnsConcat(t, s, table)+") AS TEXT),?)>0 OR instr(CAST(("+columnsConcat(t, s, table)+") AS TEXT),?)>0",
			tokens.AccessToken, tokens.RefreshToken).Scan(&plain); err != nil || plain != 0 {
			t.Fatalf("%s holds a token in plain text: %d %v", table, plain, err)
		}
	}
}

// columnsConcat is every column of table joined, for a plain-text search.
func columnsConcat(t *testing.T, s *Store, table string) string {
	t.Helper()
	rows, err := s.db.Query("SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var c string
		_ = rows.Scan(&c)
		cols = append(cols, "coalesce("+c+",'')")
	}
	return strings.Join(cols, "||")
}

// A refresh token rotates: the new pair works, the old access token stops,
// and presenting the old refresh token again ends the connection.
func TestOAuthRefreshRotationAndReuse(t *testing.T) {
	s := openHostedTest(t)
	account, first := oauthConnect(t, s)
	if _, err := s.OAuthRefresh(testContext, first.RefreshToken, "smcl_other", "", "", testTime); !isGrantError(err, "invalid_grant") {
		t.Fatalf("another client refreshed: %v", err)
	}
	if _, err := s.OAuthRefresh(testContext, first.RefreshToken, "smcl_test", "", "openid", testTime); !isGrantError(err, "invalid_scope") {
		t.Fatalf("an unknown scope: %v", err)
	}
	second, err := s.OAuthRefresh(testContext, first.RefreshToken, "smcl_test", oauthTestResource, OAuthScope, testTime+10)
	if err != nil || second.AccessToken == first.AccessToken || second.RefreshToken == first.RefreshToken {
		t.Fatalf("refresh: %+v %v", second, err)
	}
	if _, err = s.HostedAccount(audience(oauthTestResource), first.AccessToken, testTime+10); !isCode(err, "hosted_token_invalid") {
		t.Fatalf("the rotated access token still works: %v", err)
	}
	if got, err := s.HostedAccount(audience(oauthTestResource), second.AccessToken, testTime+10); err != nil || got != account {
		t.Fatalf("the new access token: %v", err)
	}
	// The old refresh token again: refused, and the whole family ends.
	if _, err = s.OAuthRefresh(testContext, first.RefreshToken, "smcl_test", "", "", testTime+20); !isGrantError(err, "invalid_grant") {
		t.Fatalf("reuse: %v", err)
	}
	if _, err = s.HostedAccount(audience(oauthTestResource), second.AccessToken, testTime+20); !isCode(err, "hosted_token_invalid") {
		t.Fatalf("reuse left the newest access token working: %v", err)
	}
	if _, err = s.OAuthRefresh(testContext, second.RefreshToken, "smcl_test", "", "", testTime+20); !isGrantError(err, "invalid_grant") {
		t.Fatalf("reuse left the newest refresh token working: %v", err)
	}
	// An expired refresh token is refused.
	_, third := oauthConnect(t, s)
	if _, err = s.OAuthRefresh(testContext, third.RefreshToken, "smcl_test", "", "", testTime+OAuthRefreshSeconds); !isGrantError(err, "invalid_grant") {
		t.Fatalf("an expired refresh token: %v", err)
	}
}

// Every way a token is revoked ends the OAuth connection, so its refresh
// token cannot make a new one: RFC 7009 revocation (by the access or the
// refresh token), manage_tokens revoke (one or all), recovery and claim.
func TestOAuthRevocationEndsTheConnection(t *testing.T) {
	s := openHostedTest(t)
	check := func(name string, tokens OAuthTokens) {
		t.Helper()
		if _, err := s.HostedAccount(audience(oauthTestResource), tokens.AccessToken, testTime); !isCode(err, "hosted_token_invalid") {
			t.Fatalf("%s: the access token still works: %v", name, err)
		}
		if _, err := s.OAuthRefresh(testContext, tokens.RefreshToken, "smcl_test", "", "", testTime); !isGrantError(err, "invalid_grant") {
			t.Fatalf("%s: the refresh token still works: %v", name, err)
		}
	}
	_, a := oauthConnect(t, s)
	if err := s.OAuthRevoke(testContext, a.AccessToken, "smcl_test", testTime); err != nil {
		t.Fatal(err)
	}
	check("revoke by access token", a)
	_, b := oauthConnect(t, s)
	if err := s.OAuthRevoke(testContext, b.RefreshToken, "", testTime); err != nil {
		t.Fatal(err)
	}
	check("revoke by refresh token", b)
	_, c := oauthConnect(t, s)
	if err := s.OAuthRevoke(testContext, c.RefreshToken, "smcl_other", testTime); err != nil {
		t.Fatal(err)
	}
	if _, err := s.HostedAccount(audience(oauthTestResource), c.AccessToken, testTime); err != nil {
		t.Fatalf("another client revoked a connection: %v", err)
	}
	for _, target := range []string{"one", "all"} {
		_, d := oauthConnect(t, s)
		key, _, err := s.HostedSigner(audience(oauthTestResource), d.AccessToken, testTime)
		if err != nil {
			t.Fatal(err)
		}
		which := "all"
		if target == "one" {
			which = hostedTokenID(hostedHash(d.AccessToken))
		}
		run(t, s, signed(key, Command{Operation: "hosted.token", Data: `{"schema":1,"action":"revoke","target":"` + which + `"}`}))
		check("manage_tokens revoke "+target, d)
	}
	// Recovery (hosted.recover) and claim end every connection.
	created := createHosted(t, s, "test-origin", "")
	code, _ := s.OAuthIssueCode(testContext, oauthTestGrant(), created["agent"].(string), "", testTime)
	e, err := s.OAuthExchangeCode(testContext, code, "smcl_test", "https://app.example/cb", oauthTestVerifier, "", testTime)
	if err != nil {
		t.Fatal(err)
	}
	run(t, s, Command{Operation: "hosted.recover", Data: `{"schema":1,"recovery_code":"` + created["recovery_code"].(string) + `"}`})
	check("hosted.recover", e)
}

// Signing in with a recovery code: the code works once and a new one is
// returned; other connections stay unless sign-out is asked for; every
// failure is the one 403 recovery_invalid.
func TestOAuthRecover(t *testing.T) {
	s := openHostedTest(t)
	created := createHosted(t, s, "test-origin", "keeper")
	token := created["token"].(string)
	got, err := s.OAuthRecover(testContext, created["recovery_code"].(string), false, testTime)
	if err != nil || got.Account != created["agent"] || got.Handle != "keeper" || !strings.HasPrefix(got.RecoveryCode, HostedRecoveryPrefix) || got.RecoveryCode == created["recovery_code"] {
		t.Fatalf("recover: %+v %v", got, err)
	}
	if _, err = s.HostedAccount(testContext, token, testTime); err != nil {
		t.Fatalf("signing in signed another connection out: %v", err)
	}
	var first string
	for name, code := range map[string]string{"used": created["recovery_code"].(string), "unknown": HostedRecoveryPrefix + strings.Repeat("A", 43), "malformed": "smr1_x", "a token": token, "empty": ""} {
		_, err := s.OAuthRecover(testContext, code, false, testTime)
		if !isCode(err, "recovery_invalid") {
			t.Fatalf("%s code: %v", name, err)
		}
		if first == "" {
			first = err.Error()
		} else if err.Error() != first {
			t.Fatalf("%s code answers differently: %q vs %q", name, err.Error(), first)
		}
	}
	again, err := s.OAuthRecover(testContext, got.RecoveryCode, true, testTime)
	if err != nil || again.TokensRevoked != 1 {
		t.Fatalf("sign out: %+v %v", again, err)
	}
	if _, err = s.HostedAccount(testContext, token, testTime); !isCode(err, "hosted_token_invalid") {
		t.Fatalf("sign out left a token working: %v", err)
	}
}

// Past OAuthConnectionsMax live connections, the oldest is signed out.
func TestOAuthConnectionsMax(t *testing.T) {
	s := openHostedTest(t)
	created := createHosted(t, s, "test-origin", "")
	account := created["agent"].(string)
	var all []OAuthTokens
	for i := 0; i <= OAuthConnectionsMax; i++ {
		code, err := s.OAuthIssueCode(testContext, oauthTestGrant(), account, "", testTime+int64(i))
		if err != nil {
			t.Fatal(err)
		}
		tokens, err := s.OAuthExchangeCode(testContext, code, "smcl_test", "https://app.example/cb", oauthTestVerifier, "", testTime+int64(i))
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, tokens)
	}
	if _, err := s.HostedAccount(audience(oauthTestResource), all[0].AccessToken, testTime+10); !isCode(err, "hosted_token_invalid") {
		t.Fatalf("the oldest connection survived: %v", err)
	}
	for _, tokens := range all[1:] {
		if _, err := s.HostedAccount(audience(oauthTestResource), tokens.AccessToken, testTime+10); err != nil {
			t.Fatalf("a newer connection was signed out: %v", err)
		}
	}
}

// Dynamic registration is capped per network and per day.
func TestOAuthRegistrationCap(t *testing.T) {
	s := openHostedTest(t)
	c, err := s.OAuthRegisterClient(testContext, "App", []string{"https://app.example/cb"}, "198.51.100.7", testTime)
	if err != nil || !strings.HasPrefix(c.ID, OAuthClientPrefix) {
		t.Fatalf("register: %+v %v", c, err)
	}
	got, ok, err := s.OAuthRegisteredClient(testContext, c.ID)
	if err != nil || !ok || got.Name != "App" || got.RedirectURIs[0] != "https://app.example/cb" {
		t.Fatalf("lookup: %+v %v %v", got, ok, err)
	}
	if _, err = s.db.Exec(`WITH RECURSIVE n(v) AS (VALUES(1) UNION ALL SELECT v+1 FROM n WHERE v<?)
 INSERT INTO oauth_clients(client_id,client_name,redirect_uris,created_at,network) SELECT 'smcl_fill'||v,'x','[]',?,(SELECT network FROM oauth_clients WHERE client_id=?) FROM n`, OAuthClientsPerNetworkDaily, testTime, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.OAuthRegisterClient(testContext, "App", []string{"https://app.example/cb"}, "198.51.100.9", testTime); !isGrantError(err, "temporarily_unavailable") {
		t.Fatalf("over the network's cap: %v", err)
	}
	if _, err = s.OAuthRegisterClient(testContext, "App", []string{"https://app.example/cb"}, "203.0.113.9", testTime); err != nil {
		t.Fatalf("another network: %v", err)
	}
}

// Signing every connection out (recovering with sign-out, hosted.recover,
// hosted.claim, revoking all tokens) also spends codes not yet exchanged.
func TestOAuthSignOutSpendsPendingCodes(t *testing.T) {
	s := openHostedTest(t)
	created := createHosted(t, s, "test-origin", "")
	code, err := s.OAuthIssueCode(testContext, oauthTestGrant(), created["agent"].(string), "", testTime)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.OAuthRecover(testContext, created["recovery_code"].(string), true, testTime+1); err != nil {
		t.Fatal(err)
	}
	if _, err = s.OAuthExchangeCode(testContext, code, "smcl_test", "https://app.example/cb", oauthTestVerifier, "", testTime+2); err == nil || err.Error() != invalidGrant().Error() {
		t.Fatalf("a code issued before sign-out still connects: %v", err)
	}
}
