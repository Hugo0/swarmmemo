package httpapi

// Sign in with SwarmMemo for apps over HTTP (oauthapps.go): the consent
// page lists the scopes and marks development returns, unknown redirects
// never redirect, the token endpoint answers app tokens (JWTs) and
// exchanges them, /v1/command runs an app token's command within its scopes
// with via app:APP, and the JWKS, introspection and userinfo answer.

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"net/url"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
)

// signedAt is c signed by key now, as an own-key app signs it.
func signedNow(key ed25519.PrivateKey, c board.Command) board.Command {
	c.PublicKey = base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
	c.Timestamp, c.Nonce = time.Now().Unix(), randomNonce()
	c.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, board.Canonical("swarmmemo.com", c)))
	return c
}

func TestSignInWithSwarmMemoOverHTTP(t *testing.T) {
	store, s := hostedServer(t)
	ctx := t.Context()
	app := ed25519.NewKeyFromSeed(make([]byte, 32))
	exec := func(c board.Command) board.Result {
		t.Helper()
		res, err := store.Execute(ctx, signedNow(app, c), "198.51.100.9")
		if err != nil {
			t.Fatalf("%s: %v", c.Operation, err)
		}
		return res
	}
	exec(board.Command{Operation: "app.register", Data: `{"schema":1,"name":"Hex Arena","homepage":"https://app.example"}`, RequestID: "a"})
	client := exec(board.Command{Operation: "oauth.client.register", RequestID: "c",
		Data: `{"schema":1,"name":"Hex Arena","redirect_uris":["` + testRedirect + `","http://localhost:5173/cb"],"scopes":["identity","app-rooms"],"dev_redirects":true}`}).Data["client"].(board.OAuthAppClient)
	appFP := client.App

	page := func(extra map[string]string) (int, string, string) {
		w := oauthDo(s, "GET", authorizeQuery(client.ID, testRedirect, "st", extra), "", "", map[string]string{"Accept": "text/html"})
		return w.Code, w.Body.String(), w.Header().Get("Location")
	}
	// The consent page names the app and says what each scope allows.
	code, body, _ := page(map[string]string{"scope": "identity app-rooms", "resource": ""})
	if code != 200 || !strings.Contains(body, "read and post as you in the rooms this app owns") || !strings.Contains(body, "see who you are") ||
		!strings.Contains(html.UnescapeString(body), "for the app Hex Arena (https://app.example)") || strings.Contains(body, "Development:") {
		t.Fatalf("consent page: %d %s", code, body)
	}
	// A localhost return is marked as development.
	w := oauthDo(s, "GET", authorizeQuery(client.ID, "http://localhost:5173/cb", "st", map[string]string{"scope": "", "resource": ""}), "", "", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Development:") {
		t.Fatalf("dev consent page: %d", w.Code)
	}
	// An unknown redirect URI is an error page, never a redirect.
	w = oauthDo(s, "GET", authorizeQuery(client.ID, "https://app.example/other", "st", map[string]string{"scope": "", "resource": ""}), "", "", nil)
	if w.Code != 400 || w.Header().Get("Location") != "" {
		t.Fatalf("unknown redirect: %d %q", w.Code, w.Header().Get("Location"))
	}
	// A scope outside the ceiling, or hosted, goes back as invalid_scope.
	for _, scope := range []string{"wakeups", "hosted", "identity bogus"} {
		if code, _, loc := page(map[string]string{"scope": scope, "resource": ""}); code != 302 || !strings.Contains(loc, "error=invalid_scope") {
			t.Fatalf("scope %q: %d %s", scope, code, loc)
		}
	}
	if code, _, loc := page(map[string]string{"scope": "identity", "resource": "https://elsewhere.example/mcp"}); code != 302 || !strings.Contains(loc, "error=invalid_target") {
		t.Fatalf("foreign resource: %d %s", code, loc)
	}

	// Sign in: the answer is an app token for the app's SwarmMemo resource.
	request, cookie := consent(t, s, authorizeQuery(client.ID, testRedirect, "st", map[string]string{"scope": "identity app-rooms", "resource": ""}))
	back, _ := continued(t, s, cookie, submit(s, request, cookie, map[string]string{"action": "create"}, nil))
	status, tokens, _ := tokenRequest(s, url.Values{"grant_type": {"authorization_code"}, "code": {back.Query().Get("code")}, "client_id": {client.ID},
		"redirect_uri": {testRedirect}, "code_verifier": {testVerifier}})
	access, _ := tokens["access_token"].(string)
	if status != 200 || tokens["scope"] != "identity app-rooms" || !board.LooksLikeJWT(access) || fmt.Sprint(tokens["expires_in"]) != fmt.Sprint(board.OAuthJWTAccessSeconds) {
		t.Fatalf("token: %d %v", status, tokens)
	}
	command := func(token, body string) (int, map[string]any, string) {
		w := oauthDo(s, "POST", "/v1/command", "application/json", body, map[string]string{"Authorization": "Bearer " + token})
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out, w.Header().Get("WWW-Authenticate")
	}
	// The app's token opens one of the app's rooms (app/APP/…), its person
	// a member.
	table := "app/" + appFP + "/table"
	if status, out, _ := command(access, `{"operation":"room.create","room":"`+table+`","request_id":"r"}`); status != 200 {
		t.Fatalf("bearer room.create: %d %v", status, out)
	}
	status, out, _ := command(access, `{"operation":"post","room":"`+table+`","text":"my move","request_id":"m1"}`)
	id, _ := dig(out, "receipt", "id").(string)
	if status != 200 || id == "" {
		t.Fatalf("bearer post: %d %v", status, out)
	}
	if status, again, _ := command(access, `{"operation":"post","room":"`+table+`","text":"my move","request_id":"m1"}`); status != 200 || dig(again, "receipt", "id") != id {
		t.Fatalf("bearer retry: %d %v", status, again)
	}
	got, err := store.Execute(ctx, signedNow(app, board.Command{Operation: "message.get", MessageID: id}), "198.51.100.9")
	if err != nil || got.Messages[0].Via != "app:"+appFP {
		t.Fatalf("via: %v %v", got.Messages, err)
	}
	if status, out, challenge := command(access, `{"operation":"post","room":"lobby","text":"hi","request_id":"m2"}`); status != 403 || dig(out, "error", "code") != "insufficient_scope" || !strings.Contains(challenge, "insufficient_scope") {
		t.Fatalf("outside the scope: %d %v %q", status, out, challenge)
	}
	if status, out, _ := command(access, `{"operation":"hosted.token","data":"{\"schema\":1,\"action\":\"list\"}"}`); status == 200 {
		t.Fatalf("a token operation through an app token: %v", out)
	}
	if status, out, _ := command(access, `{"operation":"room.get","room":"`+table+`","public_key":"x"}`); status != 400 || dig(out, "error", "code") != "ambiguous_command" {
		t.Fatalf("a signed body with an app token: %d %v", status, out)
	}
	if status, _, challenge := command(access[:len(access)-4]+"AAAA", `{"operation":"room.get","room":"`+table+`"}`); status != 401 || !strings.Contains(challenge, "invalid_token") {
		t.Fatalf("a forged token: %d %q", status, challenge)
	}
	// An app token is not a hosted token: /mcp does not take it.
	if w := mcpHTTP(s, "/mcp", "Bearer "+access); w.Code != 401 {
		t.Fatalf("an app token on /mcp: %d", w.Code)
	}

	// JWKS, introspection and userinfo.
	var jwks struct{ Keys []board.JWK }
	_ = json.Unmarshal(oauthDo(s, "GET", "/.well-known/jwks.json", "", "", nil).Body.Bytes(), &jwks)
	header, _ := base64.RawURLEncoding.DecodeString(strings.Split(access, ".")[0])
	if len(jwks.Keys) != 1 || !strings.Contains(string(header), `"kid":"`+jwks.Keys[0].KeyID+`"`) {
		t.Fatalf("jwks %v header %s", jwks, header)
	}
	w = oauthDo(s, "POST", "/oauth/introspect", "application/x-www-form-urlencoded", url.Values{"token": {access}}.Encode(), nil)
	var in map[string]any
	if json.Unmarshal(w.Body.Bytes(), &in) != nil || in["active"] != true || in["app"] != appFP {
		t.Fatalf("introspect: %d %s", w.Code, w.Body.String())
	}
	w = oauthDo(s, "GET", "/oauth/userinfo", "", "", map[string]string{"Authorization": "Bearer " + access})
	var info map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &info) != nil || info["sub"] == nil || info["band"] == nil {
		t.Fatalf("userinfo: %d %s", w.Code, w.Body.String())
	}

	// Token exchange at the token endpoint narrows the token.
	actor := board.SignActorToken(app, client.ID, "https://swarmmemo.com/oauth/token", time.Now().Unix())
	status, exchanged, _ := tokenRequest(s, url.Values{"grant_type": {board.OAuthGrantTokenExchange}, "client_id": {client.ID}, "subject_token": {access},
		"subject_token_type": {board.OAuthTokenTypeAccess}, "actor_token": {actor}, "actor_token_type": {board.OAuthTokenTypeJWT}, "scope": {"app-rooms"}})
	if status != 200 || exchanged["issued_token_type"] != board.OAuthTokenTypeAccess || exchanged["scope"] != "app-rooms" || exchanged["refresh_token"] != nil {
		t.Fatalf("exchange: %d %v", status, exchanged)
	}
	if status, out, _ := command(exchanged["access_token"].(string), `{"operation":"agent.get","target":"`+fmt.Sprint(info["sub"])+`"}`); status != 403 || dig(out, "error", "code") != "insufficient_scope" {
		t.Fatalf("the exchanged token kept identity: %d %v", status, out)
	}

	// Discovery: the metadata and /capabilities name the scopes and the JWKS.
	var caps map[string]any
	_ = json.Unmarshal(oauthDo(s, "GET", "/capabilities", "", "", nil).Body.Bytes(), &caps)
	if dig(caps, "conversations", "hosted", "oauth", "apps", "jwks_uri") != "https://swarmmemo.com/.well-known/jwks.json" {
		t.Fatalf("capabilities apps: %v", dig(caps, "conversations", "hosted", "oauth", "apps"))
	}
	var as map[string]any
	_ = json.Unmarshal(oauthDo(s, "GET", "/.well-known/oauth-authorization-server", "", "", nil).Body.Bytes(), &as)
	if fmt.Sprint(as["scopes_supported"]) != "[hosted identity app-rooms dm wakeups]" || as["introspection_endpoint"] == nil || as["userinfo_endpoint"] == nil {
		t.Fatalf("metadata: %v", as)
	}
}
