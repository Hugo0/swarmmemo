package httpapi

// Sign-in (OAuth 2.1) for the assistant profile over HTTP (oauth.go): the
// metadata, the authorization page (create, recover, cancel), PKCE, exact
// redirect URIs, state and iss, CSRF on the consent form, no open
// redirects, the token endpoint's grants, revocation, rate limits, the 401
// challenge on /mcp/assistant, audience binding, and that no secret reaches
// a log. Store-level rules (code expiry, rotation, family revocation) are in
// board/oauth_test.go.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

const (
	testRedirect = "https://app.example/callback?tenant=7"
	testVerifier = "verifier-0123456789-0123456789-0123456789-0123456789"
)

var testResource = "https://swarmmemo.com" + web.AssistantMCPPath

// oauthDo sends one request from a browser or client at peer.
func oauthDo(s http.Handler, method, target, contentType, body string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "https://swarmmemo.com"+target, strings.NewReader(body))
	r.RemoteAddr = "198.51.100.20:4000"
	if peer := headers["peer"]; peer != "" {
		r.RemoteAddr = peer + ":4000"
	}
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	for k, v := range headers {
		if k != "peer" {
			r.Header.Set(k, v)
		}
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func registerTestClient(t *testing.T, s http.Handler, redirects ...string) string {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"client_name": "Test App", "redirect_uris": redirects, "token_endpoint_auth_method": "none", "grant_types": []string{"authorization_code", "refresh_token"}})
	w := oauthDo(s, "POST", "/oauth/register", "application/json", string(raw), nil)
	var out map[string]any
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &out) != nil || !strings.HasPrefix(fmt.Sprint(out["client_id"]), board.OAuthClientPrefix) {
		t.Fatalf("register: %d %s", w.Code, w.Body.String())
	}
	return out["client_id"].(string)
}

func authorizeQuery(clientID, redirect, state string, extra map[string]string) string {
	q := url.Values{"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirect}, "state": {state},
		"code_challenge": {board.PKCEChallenge(testVerifier)}, "code_challenge_method": {"S256"}, "resource": {testResource}, "scope": {board.OAuthScope}}
	for k, v := range extra {
		if v == "" {
			q.Del(k)
		} else {
			q.Set(k, v)
		}
	}
	return "/oauth/authorize?" + q.Encode()
}

var requestFieldRE = regexp.MustCompile(`name="request" value="([^"]+)"`)

// consent loads the sign-in page and returns its signed request and the
// browser's consent cookie.
func consent(t *testing.T, s http.Handler, target string) (request, cookie string) {
	t.Helper()
	w := oauthDo(s, "GET", target, "", "", map[string]string{"Accept": "text/html"})
	m := requestFieldRE.FindStringSubmatch(w.Body.String())
	if w.Code != 200 || m == nil {
		t.Fatalf("authorize page: %d %s", w.Code, w.Body.String())
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == oauthConsentCookie {
			if !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" {
				t.Fatalf("consent cookie attributes: %+v", c)
			}
			cookie = c.Value
		}
	}
	if w.Header().Get("Referrer-Policy") != "same-origin" {
		t.Fatalf("the authorize page's Referrer-Policy is %q: browsers then post the form with Origin null", w.Header().Get("Referrer-Policy"))
	}
	csp := w.Header().Get("Content-Security-Policy")
	if cookie == "" || !strings.Contains(csp, "frame-ancestors 'none'") || !strings.Contains(csp, "form-action 'self' https://app.example") || strings.Contains(csp, "script-src") {
		t.Fatalf("authorize page cookie %q, CSP %q", cookie, csp)
	}
	return html.UnescapeString(m[1]), cookie
}

func submit(s http.Handler, request, cookie string, fields map[string]string, headers map[string]string) *httptest.ResponseRecorder {
	form := url.Values{"request": {request}}
	for k, v := range fields {
		form.Set(k, v)
	}
	h := map[string]string{"Origin": "https://swarmmemo.com", "Sec-Fetch-Site": "same-origin"}
	if cookie != "" {
		h["Cookie"] = oauthConsentCookie + "=" + cookie
	}
	for k, v := range headers {
		h[k] = v
	}
	return oauthDo(s, "POST", "/oauth/authorize", "application/x-www-form-urlencoded", form.Encode(), h)
}

var recoveryRE = regexp.MustCompile(`<pre class="oauth-recovery"><code>(smr1_[A-Za-z0-9_-]{43})</code></pre>`)

// continued takes the recovery page w, checks it shows the recovery code
// and asks to confirm it was saved, then presses Continue: it returns where
// that goes (the redirect URI with the code, state and iss) and the code.
func continued(t *testing.T, s http.Handler, cookie string, w *httptest.ResponseRecorder) (*url.URL, string) {
	t.Helper()
	body := w.Body.String()
	request, recovery := requestFieldRE.FindStringSubmatch(body), recoveryRE.FindStringSubmatch(body)
	if w.Code != 200 || request == nil || recovery == nil || !strings.Contains(body, "I saved my recovery code") || !strings.Contains(body, `value="continue"`) {
		t.Fatalf("recovery page: %d %s", w.Code, body)
	}
	back := submit(s, html.UnescapeString(request[1]), cookie, map[string]string{"action": "continue"}, nil)
	if back.Code != 303 || !strings.Contains(back.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatalf("continue: %d %s", back.Code, back.Body.String())
	}
	u, err := url.Parse(back.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return u, recovery[1]
}

func tokenRequest(s http.Handler, form url.Values) (int, map[string]any, http.Header) {
	w := oauthDo(s, "POST", "/oauth/token", "application/x-www-form-urlencoded", form.Encode(), nil)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out, w.Header()
}

func exchange(t *testing.T, s http.Handler, clientID, code string) map[string]any {
	t.Helper()
	status, out, h := tokenRequest(s, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID}, "redirect_uri": {testRedirect},
		"code_verifier": {testVerifier}, "resource": {testResource}})
	if status != 200 || out["token_type"] != "Bearer" || h.Get("Cache-Control") != "no-store" {
		t.Fatalf("token: %d %v", status, out)
	}
	return out
}

// signIn runs the whole flow for a new identity and returns the tokens and
// the recovery code.
func signIn(t *testing.T, s http.Handler, clientID string) (map[string]any, string) {
	t.Helper()
	request, cookie := consent(t, s, authorizeQuery(clientID, testRedirect, "st", nil))
	back, recovery := continued(t, s, cookie, submit(s, request, cookie, map[string]string{"action": "create"}, nil))
	return exchange(t, s, clientID, back.Query().Get("code")), recovery
}

func TestOAuthMetadata(t *testing.T) {
	_, s := hostedServer(t)
	for _, path := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp/assistant"} {
		w := oauthDo(s, "GET", path, "", "", nil)
		var prm map[string]any
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &prm) != nil || prm["resource"] != testResource || fmt.Sprint(prm["authorization_servers"]) != "[https://swarmmemo.com]" ||
			fmt.Sprint(prm["scopes_supported"]) != "[hosted]" || w.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	w := oauthDo(s, "GET", "/.well-known/oauth-authorization-server", "", "", nil)
	var as map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &as) != nil {
		t.Fatalf("authorization server metadata: %d", w.Code)
	}
	for field, want := range map[string]string{"issuer": "https://swarmmemo.com", "authorization_endpoint": "https://swarmmemo.com/oauth/authorize", "token_endpoint": "https://swarmmemo.com/oauth/token",
		"registration_endpoint": "https://swarmmemo.com/oauth/register", "revocation_endpoint": "https://swarmmemo.com/oauth/revoke", "code_challenge_methods_supported": "[S256]",
		"token_endpoint_auth_methods_supported": "[none]", "client_id_metadata_document_supported": "true", "authorization_response_iss_parameter_supported": "true",
		"grant_types_supported": "[authorization_code refresh_token]", "response_types_supported": "[code]"} {
		if fmt.Sprint(as[field]) != want {
			t.Errorf("%s = %v, want %s", field, as[field], want)
		}
	}
	// The other surfaces say the same: /capabilities, the server card, llms.txt
	// and the assistant profile's instructions.
	var caps map[string]any
	_ = json.Unmarshal(oauthDo(s, "GET", "/capabilities", "", "", nil).Body.Bytes(), &caps)
	if dig(caps, "conversations", "hosted", "oauth", "available") != true || dig(caps, "conversations", "hosted", "oauth", "resource") != testResource {
		t.Fatalf("/capabilities oauth: %v", dig(caps, "conversations", "hosted", "oauth"))
	}
	var card map[string]any
	_ = json.Unmarshal(oauthDo(s, "GET", "/.well-known/mcp/server-card.json", "", "", nil).Body.Bytes(), &card)
	if dig(card, "assistant_profile", "authentication", "protected_resource_metadata") != "https://swarmmemo.com/.well-known/oauth-protected-resource/mcp/assistant" {
		t.Fatalf("server card: %v", card["assistant_profile"])
	}
	if llms := oauthDo(s, "GET", "/llms.txt", "", "", nil).Body.String(); !strings.Contains(llms, "/protocol.md#signing-in-with-oauth") {
		t.Fatal("llms.txt does not mention sign-in")
	}
	initialized := mcpRequest(t, s, web.AssistantMCPPath, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	if !strings.Contains(fmt.Sprint(dig(initialized, "result", "instructions")), "do not call create_identity") {
		t.Fatal("the assistant instructions do not say a signed-in assistant already has its identity")
	}
	// Without hosted identities there is nothing to sign in to.
	off := New(&fakeService{}, nil, Config{PublicURL: "https://swarmmemo.com"})
	for _, path := range []string{"/.well-known/oauth-authorization-server", "/.well-known/oauth-protected-resource", "/oauth/authorize"} {
		if w := oauthDo(off, "GET", path, "", "", nil); w.Code != 404 {
			t.Errorf("%s with hosted identities off: %d", path, w.Code)
		}
	}
}

// The whole flow: register, sign in with a new identity, save the recovery
// code, return to the app with the code, state and iss, exchange it, and the
// access token acts as the identity on /mcp/assistant, listed by whoami.
func TestOAuthSignInCreatesAHostedIdentity(t *testing.T) {
	_, s := hostedServer(t)
	clientID := registerTestClient(t, s, testRedirect)
	state := `a b&c=d/é"<`
	request, cookie := consent(t, s, authorizeQuery(clientID, testRedirect, state, nil))
	page := submit(s, request, cookie, map[string]string{"action": "create", "handle": "oauth-helper"}, nil)
	back, recovery := continued(t, s, cookie, page)
	if back.Scheme+"://"+back.Host+back.Path != "https://app.example/callback" || back.Query().Get("tenant") != "7" || back.Query().Get("state") != state ||
		back.Query().Get("iss") != "https://swarmmemo.com" || !strings.HasPrefix(back.Query().Get("code"), "smc1_") {
		t.Fatalf("continue goes to %s", back)
	}
	if !strings.Contains(page.Body.String(), "@oauth-helper") || !strings.Contains(page.Header().Get("Content-Security-Policy"), "form-action 'self' https://app.example") {
		t.Fatalf("recovery page: %s", page.Body.String())
	}
	tokens := exchange(t, s, clientID, back.Query().Get("code"))
	access := tokens["access_token"].(string)
	if !strings.HasPrefix(access, board.HostedTokenPrefix) || !strings.HasPrefix(tokens["refresh_token"].(string), board.OAuthRefreshPrefix) || tokens["expires_in"] != float64(board.OAuthAccessSeconds) || tokens["scope"] != "hosted" {
		t.Fatalf("tokens: %v", tokens)
	}
	me := mustTool(t, s, web.AssistantMCPPath, "Bearer "+access, "whoami", map[string]any{})
	list := dig(me, "data", "tokens").([]any)
	if dig(me, "agent", "custody") != "hosted" || len(list) != 1 || list[0].(map[string]any)["label"] != "oauth: Test App" || list[0].(map[string]any)["last_used_at"] == float64(0) {
		t.Fatalf("whoami with the access token: %v", me)
	}
	posted := mustTool(t, s, web.AssistantMCPPath, "Bearer "+access, "post_message", map[string]any{"text": "signed in through OAuth"})
	if dig(posted, "receipt", "id") == nil {
		t.Fatalf("post: %v", posted)
	}
	// The recovery code shown is the identity's: it signs in again.
	request, cookie = consent(t, s, authorizeQuery(clientID, testRedirect, "again", nil))
	back, _ = continued(t, s, cookie, submit(s, request, cookie, map[string]string{"action": "recover", "recovery_code": recovery}, nil))
	again := exchange(t, s, clientID, back.Query().Get("code"))
	me2 := mustTool(t, s, web.AssistantMCPPath, "Bearer "+again["access_token"].(string), "whoami", map[string]any{})
	if dig(me2, "agent", "id") != dig(me, "agent", "id") {
		t.Fatalf("recovery signed in to another identity")
	}
	// The first connection is kept (no sign-out was asked for).
	mustTool(t, s, web.AssistantMCPPath, "Bearer "+access, "whoami", map[string]any{})
}

// An OAuth access token works only as a header on /mcp/assistant: not in the
// path, not on /mcp. An invalid or revoked one gets HTTP 401 with the
// challenge, while no token keeps working anonymously and a hosted tool
// without one carries the challenge in _meta.
func TestOAuthResourceServer(t *testing.T) {
	_, s := hostedServer(t)
	clientID := registerTestClient(t, s, testRedirect)
	tokens, _ := signIn(t, s, clientID)
	access := tokens["access_token"].(string)
	for _, carrier := range []struct{ path, auth string }{{"/mcp/t/" + access, ""}, {web.AssistantMCPPath + "/t/" + access, ""}} {
		if _, failure := callTool(t, s, carrier.path, carrier.auth, "whoami", map[string]any{}); !strings.HasPrefix(failure, "401 hosted_token_invalid") {
			t.Fatalf("an OAuth token on %s %q: %q", carrier.path, carrier.auth, failure)
		}
	}
	// Issued for /mcp/assistant, it is not good on /mcp, another resource.
	if w := mcpHTTP(s, "/mcp", "Bearer "+access); w.Code != 401 || !strings.Contains(w.Header().Get("WWW-Authenticate"), `resource_metadata="https://swarmmemo.com/.well-known/oauth-protected-resource/mcp"`) {
		t.Fatalf("an assistant-profile token on /mcp: %d %q", w.Code, w.Header().Get("WWW-Authenticate"))
	}
	w := oauthDo(s, "POST", "/oauth/revoke", "application/x-www-form-urlencoded", url.Values{"token": {access}, "client_id": {clientID}}.Encode(), nil)
	if w.Code != 200 {
		t.Fatalf("revoke: %d %s", w.Code, w.Body.String())
	}
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	for _, bad := range []string{access, board.HostedTokenPrefix + strings.Repeat("A", 43), "not-a-token"} {
		w = oauthDo(s, "POST", web.AssistantMCPPath, "application/json", body, map[string]string{"Accept": "application/json, text/event-stream", "Authorization": "Bearer " + bad})
		challenge := w.Header().Get("WWW-Authenticate")
		if w.Code != 401 || !strings.Contains(challenge, `resource_metadata="https://swarmmemo.com/.well-known/oauth-protected-resource/mcp/assistant"`) || !strings.Contains(challenge, `error="invalid_token"`) {
			t.Fatalf("a bad bearer token on the assistant profile: %d %q", w.Code, challenge)
		}
	}
	if _, out, _ := tokenRequest(s, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tokens["refresh_token"].(string)}, "client_id": {clientID}}); out["error"] != "invalid_grant" {
		t.Fatalf("refresh after revocation: %v", out)
	}
	// Anonymous calls are unchanged.
	if listed := mcpRequest(t, s, web.AssistantMCPPath, "", body); dig(listed, "result", "tools") == nil {
		t.Fatalf("anonymous tools/list: %v", listed)
	}
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "whoami", "arguments": map[string]any{}}})
	out := mcpRequest(t, s, web.AssistantMCPPath, "", string(raw))
	meta, _ := dig(out, "result", "_meta", "mcp/www_authenticate").([]any)
	if len(meta) != 1 || !strings.Contains(fmt.Sprint(meta[0]), "resource_metadata=") || dig(out, "result", "isError") != true {
		t.Fatalf("whoami without a token on the assistant profile: %v", out)
	}
	// A legacy hosted token in the header still works on both profiles.
	legacy := newIdentity(t, s, "")["token"].(string)
	for _, path := range []string{web.AssistantMCPPath, "/mcp"} {
		mustTool(t, s, path, "Bearer "+legacy, "whoami", map[string]any{})
	}
}

// Each tool on the assistant profile states whether it needs sign-in.
func TestOAuthSecuritySchemes(t *testing.T) {
	_, s := hostedServer(t)
	out := mcpRequest(t, s, web.AssistantMCPPath, "", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	seen := 0
	for _, raw := range dig(out, "result", "tools").([]any) {
		tool := raw.(map[string]any)
		schemes := fmt.Sprint(dig(tool, "_meta", "securitySchemes"))
		name := tool["name"].(string)
		want := "[map[type:noauth]]"
		_, _, hosted := hostedToolHints(name)
		switch {
		case name == "post_message", name == "read_updates", isHostedSignedCall(name):
			want = "[map[type:noauth] map[scopes:[hosted] type:oauth2]]"
		case name == "create_identity", name == "recover_identity":
		case hosted:
			want = "[map[scopes:[hosted] type:oauth2]]"
		}
		if schemes != want {
			t.Errorf("%s securitySchemes %s, want %s", name, schemes, want)
		}
		annotations := tool["annotations"].(map[string]any)
		for _, hint := range []string{"readOnlyHint", "destructiveHint", "openWorldHint"} {
			if _, ok := annotations[hint].(bool); !ok {
				t.Errorf("%s lacks %s: %v", name, hint, annotations)
			}
		}
		seen++
	}
	if seen < 20 {
		t.Fatalf("only %d tools listed", seen)
	}
	// /mcp offers sign-in too: its tools say the same.
	full := mcpRequest(t, s, "/mcp", "", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	for _, raw := range dig(full, "result", "tools").([]any) {
		tool := raw.(map[string]any)
		if got, want := fmt.Sprint(dig(tool, "_meta", "securitySchemes")), fmt.Sprint(securitySchemes(tool["name"].(string))); got != want {
			t.Fatalf("/mcp %s securitySchemes %s, want %s", tool["name"], got, want)
		}
	}
}

// PKCE S256 is required: no challenge, a plain one or a malformed one goes
// back to the app as invalid_request; the verifier must match.
func TestOAuthPKCERequired(t *testing.T) {
	_, s := hostedServer(t)
	clientID := registerTestClient(t, s, testRedirect)
	for name, extra := range map[string]map[string]string{
		"no challenge": {"code_challenge": "", "code_challenge_method": ""},
		"plain":        {"code_challenge_method": "plain"},
		"no method":    {"code_challenge_method": ""},
		"short":        {"code_challenge": "abc"},
	} {
		w := oauthDo(s, "GET", authorizeQuery(clientID, testRedirect, "s1", extra), "", "", nil)
		loc, _ := url.Parse(w.Header().Get("Location"))
		if w.Code != 302 || loc.Host != "app.example" || loc.Query().Get("error") != "invalid_request" || loc.Query().Get("state") != "s1" || loc.Query().Get("iss") != "https://swarmmemo.com" || loc.Query().Get("tenant") != "7" {
			t.Fatalf("%s: %d %s", name, w.Code, w.Header().Get("Location"))
		}
	}
	for name, want := range map[string]map[string]string{"invalid_scope": {"scope": "openid"}, "invalid_target": {"resource": "https://evil.example/mcp"}, "unsupported_response_type": {"response_type": "token"}} {
		w := oauthDo(s, "GET", authorizeQuery(clientID, testRedirect, "s2", want), "", "", nil)
		if loc, _ := url.Parse(w.Header().Get("Location")); w.Code != 302 || loc.Query().Get("error") != name {
			t.Fatalf("%s: %d %s", name, w.Code, w.Header().Get("Location"))
		}
	}
	request, cookie := consent(t, s, authorizeQuery(clientID, testRedirect, "st", nil))
	back, _ := continued(t, s, cookie, submit(s, request, cookie, map[string]string{"action": "create"}, nil))
	code := back.Query().Get("code")
	status, out, _ := tokenRequest(s, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID}, "redirect_uri": {testRedirect}, "code_verifier": {strings.Repeat("x", 50)}})
	if status != 400 || out["error"] != "invalid_grant" {
		t.Fatalf("a wrong verifier: %d %v", status, out)
	}
	// The code was spent by the wrong attempt.
	if status, out, _ = tokenRequest(s, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID}, "redirect_uri": {testRedirect}, "code_verifier": {testVerifier}}); out["error"] != "invalid_grant" {
		t.Fatalf("the right verifier after a wrong one: %d %v", status, out)
	}
}

// A code works once; a second exchange is refused and ends the connection
// the first one made.
func TestOAuthCodeSingleUse(t *testing.T) {
	_, s := hostedServer(t)
	clientID := registerTestClient(t, s, testRedirect)
	request, cookie := consent(t, s, authorizeQuery(clientID, testRedirect, "st", nil))
	back, _ := continued(t, s, cookie, submit(s, request, cookie, map[string]string{"action": "create"}, nil))
	tokens := exchange(t, s, clientID, back.Query().Get("code"))
	status, out, _ := tokenRequest(s, url.Values{"grant_type": {"authorization_code"}, "code": {back.Query().Get("code")}, "client_id": {clientID}, "redirect_uri": {testRedirect}, "code_verifier": {testVerifier}})
	if status != 400 || out["error"] != "invalid_grant" {
		t.Fatalf("second exchange: %d %v", status, out)
	}
	if _, failure := callTool(t, s, web.AssistantMCPPath, "", "whoami", map[string]any{}); failure == "" {
		t.Fatal("whoami without a token worked")
	}
	w := oauthDo(s, "POST", web.AssistantMCPPath, "application/json", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, map[string]string{"Accept": "application/json, text/event-stream", "Authorization": "Bearer " + tokens["access_token"].(string)})
	if w.Code != 401 {
		t.Fatalf("the access token from a replayed code still works: %d", w.Code)
	}
}

// Refresh tokens rotate; reusing a rotated one ends the connection.
func TestOAuthRefreshOverHTTP(t *testing.T) {
	_, s := hostedServer(t)
	clientID := registerTestClient(t, s, testRedirect)
	first, _ := signIn(t, s, clientID)
	refresh := func(token string) (int, map[string]any) {
		status, out, _ := tokenRequest(s, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {token}, "client_id": {clientID}, "resource": {testResource}})
		return status, out
	}
	status, second := refresh(first["refresh_token"].(string))
	if status != 200 || second["refresh_token"] == first["refresh_token"] {
		t.Fatalf("refresh: %d %v", status, second)
	}
	mustTool(t, s, web.AssistantMCPPath, "Bearer "+second["access_token"].(string), "whoami", map[string]any{})
	if status, out := refresh(first["refresh_token"].(string)); status != 400 || out["error"] != "invalid_grant" {
		t.Fatalf("reuse: %d %v", status, out)
	}
	if status, out := refresh(second["refresh_token"].(string)); out["error"] != "invalid_grant" {
		t.Fatalf("after reuse the newest refresh token still works: %d %v", status, out)
	}
}

// Redirect URIs match exactly, at registration (https or loopback only),
// on the authorization page and at the token endpoint; until the client and
// redirect URI check out nothing redirects, so the page is no open redirect.
func TestOAuthRedirectURIs(t *testing.T) {
	_, s := hostedServer(t)
	for _, bad := range []string{"javascript:alert(1)", "http://app.example/cb", "https://app.example/cb#frag", "https://user:pw@app.example/cb", "app.example/cb", "data:text/html,x", "https://app.example/c b", "custom-scheme://cb", ""} {
		raw, _ := json.Marshal(map[string]any{"redirect_uris": []string{bad}})
		if w := oauthDo(s, "POST", "/oauth/register", "application/json", string(raw), nil); w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_redirect_uri") {
			t.Fatalf("registered %q: %d %s", bad, w.Code, w.Body.String())
		}
	}
	if w := oauthDo(s, "POST", "/oauth/register", "application/json", `{"redirect_uris":["https://a.example/cb"],"token_endpoint_auth_method":"client_secret_basic"}`, nil); w.Code != 400 {
		t.Fatalf("a confidential client registered: %d", w.Code)
	}
	clientID := registerTestClient(t, s, testRedirect, "http://127.0.0.1:8123/cb")
	for name, target := range map[string]string{
		"unknown client":          authorizeQuery("smcl_unknown", "https://evil.example/cb", "s", nil),
		"no client":               authorizeQuery("", "https://evil.example/cb", "s", nil),
		"unregistered redirect":   authorizeQuery(clientID, "https://evil.example/cb", "s", nil),
		"trailing slash":          authorizeQuery(clientID, "https://app.example/callback/?tenant=7", "s", nil),
		"other query":             authorizeQuery(clientID, "https://app.example/callback?tenant=8", "s", nil),
		"case":                    authorizeQuery(clientID, "https://APP.example/callback?tenant=7", "s", nil),
		"repeated parameter":      authorizeQuery(clientID, testRedirect, "s", nil) + "&redirect_uri=https://evil.example/cb",
		"bad response type first": authorizeQuery(clientID, "https://evil.example/cb", "s", map[string]string{"response_type": "token"}),
	} {
		w := oauthDo(s, "GET", target, "", "", nil)
		if w.Code != 400 || w.Header().Get("Location") != "" || strings.Contains(w.Body.String(), "evil.example") {
			t.Fatalf("%s: %d Location %q", name, w.Code, w.Header().Get("Location"))
		}
	}
	// A loopback redirect is allowed, with a warning on the page.
	w := oauthDo(s, "GET", authorizeQuery(clientID, "http://127.0.0.1:8123/cb", "s", nil), "", "", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "runs on your own computer") {
		t.Fatalf("loopback redirect: %d", w.Code)
	}
	// The token endpoint compares the redirect URI exactly too.
	request, cookie := consent(t, s, authorizeQuery(clientID, testRedirect, "st", nil))
	back, _ := continued(t, s, cookie, submit(s, request, cookie, map[string]string{"action": "create"}, nil))
	if _, out, _ := tokenRequest(s, url.Values{"grant_type": {"authorization_code"}, "code": {back.Query().Get("code")}, "client_id": {clientID}, "redirect_uri": {"https://app.example/callback"}, "code_verifier": {testVerifier}}); out["error"] != "invalid_grant" {
		t.Fatalf("another redirect URI at the token endpoint: %v", out)
	}
}

// The consent form only works from this site's page in the browser that
// loaded it: the signed request must match the browser's cookie, be
// unaltered and unexpired, and come with this origin.
func TestOAuthConsentCSRF(t *testing.T) {
	_, s := hostedServer(t)
	clientID := registerTestClient(t, s, testRedirect)
	request, cookie := consent(t, s, authorizeQuery(clientID, testRedirect, "st", nil))
	tampered := []byte(request)
	tampered[3] ^= 1
	for name, try := range map[string]func() *httptest.ResponseRecorder{
		"no cookie": func() *httptest.ResponseRecorder {
			return submit(s, request, "", map[string]string{"action": "create"}, nil)
		},
		"another cookie": func() *httptest.ResponseRecorder {
			return submit(s, request, strings.Repeat("A", 43), map[string]string{"action": "create"}, nil)
		},
		"tampered request": func() *httptest.ResponseRecorder {
			return submit(s, string(tampered), cookie, map[string]string{"action": "create"}, nil)
		},
		"no request": func() *httptest.ResponseRecorder {
			return submit(s, "", cookie, map[string]string{"action": "create"}, nil)
		},
		"cross origin": func() *httptest.ResponseRecorder {
			return submit(s, request, cookie, map[string]string{"action": "create"}, map[string]string{"Origin": "https://evil.example"})
		},
		"cross site": func() *httptest.ResponseRecorder {
			return submit(s, request, cookie, map[string]string{"action": "create"}, map[string]string{"Sec-Fetch-Site": "cross-site"})
		},
	} {
		if w := try(); w.Code != 403 || w.Header().Get("Location") != "" || strings.Contains(w.Body.String(), "smr1_") {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	// Cancel goes back to the app with access_denied, the state and iss.
	w := submit(s, request, cookie, map[string]string{"action": "deny"}, nil)
	loc, _ := url.Parse(w.Header().Get("Location"))
	if w.Code != 303 || loc.Host != "app.example" || loc.Query().Get("error") != "access_denied" || loc.Query().Get("state") != "st" || loc.Query().Get("iss") != "https://swarmmemo.com" {
		t.Fatalf("cancel: %d %s", w.Code, w.Header().Get("Location"))
	}
}

// Recovery codes: a wrong one is the uniform 403 recovery_invalid, and
// attempts are rate limited per network.
func TestOAuthRecoveryAttemptsLimited(t *testing.T) {
	_, s := hostedServer(t)
	clientID := registerTestClient(t, s, testRedirect)
	request, cookie := consent(t, s, authorizeQuery(clientID, testRedirect, "st", nil))
	var first string
	limited := false
	for i, code := range []string{board.HostedRecoveryPrefix + strings.Repeat("A", 43), "smr1_short", "nonsense", board.HostedTokenPrefix + strings.Repeat("B", 43), "", board.HostedRecoveryPrefix + strings.Repeat("C", 43), board.HostedRecoveryPrefix + strings.Repeat("D", 43)} {
		w := submit(s, request, cookie, map[string]string{"action": "recover", "recovery_code": code}, nil)
		if w.Code == 429 {
			if i < 5 {
				t.Fatalf("limited after %d attempts", i)
			}
			limited = true
			continue
		}
		msg := regexp.MustCompile(`<p class="oauth-error" role="alert">([^<]*)</p>`).FindStringSubmatch(w.Body.String())
		if w.Code != 403 || msg == nil {
			t.Fatalf("attempt %d: %d %s", i, w.Code, w.Body.String())
		}
		if first == "" {
			first = msg[1]
		} else if msg[1] != first {
			t.Fatalf("recovery failures differ: %q vs %q", msg[1], first)
		}
	}
	if !limited {
		t.Fatal("recovery attempts were never limited")
	}
	// Another network is not limited by this one.
	other := submit(s, request, cookie, map[string]string{"action": "recover", "recovery_code": "x"}, map[string]string{"peer": "203.0.113.50"})
	if other.Code != 403 {
		t.Fatalf("another network: %d", other.Code)
	}
}

// The authorization page and the token endpoint are rate limited per
// network.
func TestOAuthEndpointRateLimits(t *testing.T) {
	_, s := hostedServer(t)
	clientID := registerTestClient(t, s, testRedirect)
	limited := 0
	for i := 0; i < 40; i++ {
		if w := oauthDo(s, "GET", authorizeQuery(clientID, testRedirect, "s", nil), "", "", map[string]string{"peer": "203.0.113.60"}); w.Code == 429 {
			limited++
		}
	}
	if limited == 0 {
		t.Fatal("the authorization page was never limited")
	}
	limited = 0
	for i := 0; i < 130; i++ {
		w := oauthDo(s, "POST", "/oauth/token", "application/x-www-form-urlencoded", "grant_type=refresh_token&client_id=x&refresh_token=y", map[string]string{"peer": "203.0.113.61"})
		if w.Code == 429 {
			limited++
		}
	}
	if limited == 0 {
		t.Fatal("the token endpoint was never limited")
	}
}

// The token endpoint takes only public clients and well-formed forms.
func TestOAuthTokenEndpointRequests(t *testing.T) {
	_, s := hostedServer(t)
	for name, try := range map[string]struct {
		contentType, body string
		headers           map[string]string
		status            int
		code              string
	}{
		"json":            {"application/json", `{"grant_type":"refresh_token"}`, nil, 400, "invalid_request"},
		"secret":          {"application/x-www-form-urlencoded", "grant_type=refresh_token&client_id=x&client_secret=s&refresh_token=y", nil, 401, "invalid_client"},
		"basic auth":      {"application/x-www-form-urlencoded", "grant_type=refresh_token&client_id=x&refresh_token=y", map[string]string{"Authorization": "Basic eDp5"}, 401, "invalid_client"},
		"repeated":        {"application/x-www-form-urlencoded", "grant_type=refresh_token&client_id=x&client_id=y&refresh_token=y", nil, 400, "invalid_request"},
		"no client":       {"application/x-www-form-urlencoded", "grant_type=refresh_token&refresh_token=y", nil, 400, "invalid_request"},
		"implicit":        {"application/x-www-form-urlencoded", "grant_type=password&client_id=x", nil, 400, "unsupported_grant_type"},
		"other resource":  {"application/x-www-form-urlencoded", "grant_type=refresh_token&client_id=x&refresh_token=y&resource=https%3A%2F%2Fevil.example", nil, 400, "invalid_target"},
		"unknown refresh": {"application/x-www-form-urlencoded", "grant_type=refresh_token&client_id=x&refresh_token=" + board.OAuthRefreshPrefix + strings.Repeat("A", 43), nil, 400, "invalid_grant"},
	} {
		w := oauthDo(s, "POST", "/oauth/token", try.contentType, try.body, try.headers)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		if w.Code != try.status || out["error"] != try.code || w.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: %d %v", name, w.Code, out)
		}
	}
}

// A client ID metadata document (an https client_id) is fetched, must name
// itself, and gives the consent page its name; a bad one is an error page.
func TestOAuthClientIDMetadataDocument(t *testing.T) {
	_, s := hostedServer(t)
	const id = "https://chat.example/oauth/client.json"
	fetched := 0
	s.oauth.fetch = func(_ context.Context, clientID string) ([]byte, error) {
		fetched++
		switch clientID {
		case id:
			return []byte(`{"client_id":"` + id + `","client_name":"Chat Example","redirect_uris":["https://chat.example/connector_platform_oauth_redirect"],"token_endpoint_auth_method":"private_key_jwt","token_endpoint_auth_methods_supported":["none","private_key_jwt"],"grant_types":["authorization_code","refresh_token"],"response_types":["code"]}`), nil
		case "https://liar.example/client.json":
			return []byte(`{"client_id":"https://chat.example/oauth/client.json","client_name":"Chat Example","redirect_uris":["https://liar.example/cb"]}`), nil
		case "https://secret.example/client.json":
			return []byte(`{"client_id":"https://secret.example/client.json","redirect_uris":["https://secret.example/cb"],"token_endpoint_auth_method":"private_key_jwt"}`), nil
		}
		return nil, errors.New("unreachable")
	}
	redirect := "https://chat.example/connector_platform_oauth_redirect"
	w := oauthDo(s, "GET", authorizeQuery(id, redirect, "s", nil), "", "", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Chat Example") || !strings.Contains(w.Body.String(), "chat.example") {
		t.Fatalf("CIMD client: %d %s", w.Code, w.Body.String())
	}
	oauthDo(s, "GET", authorizeQuery(id, redirect, "s", nil), "", "", nil)
	if fetched != 1 {
		t.Fatalf("the document was fetched %d times; cached after the first", fetched)
	}
	for _, bad := range []string{"https://liar.example/client.json", "https://secret.example/client.json", "https://down.example/client.json", "http://chat.example/client.json",
		"https://chat.example", "https://chat.example/", "https://chat.example/a/../client.json", "https://10.0.0.1/client.json", "https://chat.example:8443/client.json", "https://chat.example/client.json?x=1"} {
		w := oauthDo(s, "GET", authorizeQuery(bad, "https://liar.example/cb", "s", nil), "", "", nil)
		if w.Code != 400 || w.Header().Get("Location") != "" {
			t.Fatalf("client_id %s: %d %s", bad, w.Code, w.Header().Get("Location"))
		}
	}
	// The real fetcher refuses a private address before connecting.
	if _, err := fetchCIMD(t.Context(), "https://127.0.0.1/client.json"); err == nil {
		t.Fatal("fetched a loopback client document")
	}
}

// The plugin directory's domain verification: the configured token as
// plain text, a 404 while unset.
func TestAppsChallenge(t *testing.T) {
	unset := New(&fakeService{}, nil, Config{PublicURL: "https://swarmmemo.com"})
	if w := oauthDo(unset, "GET", DefaultAppsChallengePath, "", "", nil); w.Code != 404 {
		t.Fatalf("unset: %d", w.Code)
	}
	set := New(&fakeService{}, nil, Config{PublicURL: "https://swarmmemo.com", AppsChallengeToken: "abc123-token_XYZ"})
	w := oauthDo(set, "GET", DefaultAppsChallengePath, "", "", nil)
	if w.Code != 200 || w.Body.String() != "abc123-token_XYZ" || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("set: %d %q", w.Code, w.Body.String())
	}
	custom := New(&fakeService{}, nil, Config{PublicURL: "https://swarmmemo.com", AppsChallengePath: "/.well-known/other-challenge", AppsChallengeToken: "abc123-token_XYZ"})
	if w := oauthDo(custom, "GET", "/.well-known/other-challenge", "", "", nil); w.Body.String() != "abc123-token_XYZ" {
		t.Fatalf("custom path: %d", w.Code)
	}
	for _, c := range []struct {
		path, token string
		ok          bool
	}{{"", "abc123-token_XYZ", true}, {"", "short", false}, {"", "has space token", false}, {"/elsewhere", "abc123-token_XYZ", false}, {"/.well-known/a/b", "abc123-token_XYZ", false}, {"", "<script>alert(1)</script>", false}} {
		if ValidAppsChallenge(c.path, c.token) != c.ok {
			t.Errorf("ValidAppsChallenge(%q, %q) != %v", c.path, c.token, c.ok)
		}
	}
}

// No token, refresh token, code or recovery code reaches a log.
func TestOAuthSecretsNeverLogged(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(previous)
	_, s := hostedServer(t)
	clientID := registerTestClient(t, s, testRedirect)
	request, cookie := consent(t, s, authorizeQuery(clientID, testRedirect, "st", nil))
	back, recovery := continued(t, s, cookie, submit(s, request, cookie, map[string]string{"action": "create"}, nil))
	tokens := exchange(t, s, clientID, back.Query().Get("code"))
	_, refreshed, _ := tokenRequest(s, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tokens["refresh_token"].(string)}, "client_id": {clientID}})
	mustTool(t, s, web.AssistantMCPPath, "Bearer "+refreshed["access_token"].(string), "whoami", map[string]any{})
	oauthDo(s, "POST", web.AssistantMCPPath, "application/json", `{}`, map[string]string{"Authorization": "Bearer " + tokens["access_token"].(string)})
	for _, secret := range []string{back.Query().Get("code"), recovery, tokens["access_token"].(string), tokens["refresh_token"].(string), refreshed["access_token"].(string), refreshed["refresh_token"].(string), cookie} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("a secret reached the log:\n%s", logs.String())
		}
	}
}

// An OAuth connection cannot outlive itself: its access token may list and
// revoke the identity's tokens but not mint a hosted token, which would
// carry no expiry and no audience and survive the connection's revocation.
func TestOAuthTokenCannotMintHostedTokens(t *testing.T) {
	_, s := hostedServer(t)
	clientID := registerTestClient(t, s, testRedirect)
	tokens, _ := signIn(t, s, clientID)
	access := "Bearer " + tokens["access_token"].(string)
	if _, failure := callTool(t, s, web.AssistantMCPPath, access, "manage_tokens", map[string]any{"action": "create", "label": "escape"}); !strings.HasPrefix(failure, "403 oauth_token_limited") {
		t.Fatalf("manage_tokens create with an OAuth token: %q", failure)
	}
	mustTool(t, s, web.AssistantMCPPath, access, "manage_tokens", map[string]any{"action": "list"})
	// A hosted token of the same identity still creates tokens.
	legacy := newIdentity(t, s, "")["token"].(string)
	mustTool(t, s, web.AssistantMCPPath, "Bearer "+legacy, "manage_tokens", map[string]any{"action": "create"})
}

// A store failure while looking up a registered client is not shown on the
// sign-in page in the store's own words.
func TestOAuthAuthorizeHidesStoreErrors(t *testing.T) {
	store, s := hostedServer(t)
	clientID := registerTestClient(t, s, testRedirect)
	store.Close()
	w := oauthDo(s, "GET", authorizeQuery(clientID, testRedirect, "st", nil), "", "", map[string]string{"Accept": "text/html"})
	if w.Code != 400 || strings.Contains(w.Body.String(), "sql") || !strings.Contains(w.Body.String(), "could not be looked up") {
		t.Fatalf("authorize with the store closed: %d %s", w.Code, w.Body.String())
	}
}

// failingCodes is the board with authorization codes failing to issue.
type failingCodes struct{ *board.Store }

func (failingCodes) OAuthIssueCode(context.Context, board.OAuthGrant, string, string, int64) (string, error) {
	return "", errors.New("disk I/O error")
}

// Signing in with a recovery code spends it, so the page shows the new one
// before anything can fail; when the app then cannot be connected, the
// person is told so in plain words, without the store's, and without a
// redirect.
func TestOAuthRecoveryCodeShownWhenConnectingFails(t *testing.T) {
	store, s := hostedServer(t)
	clientID := registerTestClient(t, s, testRedirect)
	_, recovery := signIn(t, s, clientID)
	broken := New(failingCodes{store}, nil, Config{PublicURL: "https://swarmmemo.com", ServiceID: "swarmmemo.com"})
	broken.oauth.key = s.oauth.key
	request, cookie := consent(t, broken, authorizeQuery(clientID, testRedirect, "st", nil))
	w := submit(broken, request, cookie, map[string]string{"action": "recover", "recovery_code": recovery}, nil)
	body := w.Body.String()
	shown, cont := recoveryRE.FindStringSubmatch(body), requestFieldRE.FindStringSubmatch(body)
	if w.Code != 200 || shown == nil || shown[1] == recovery || cont == nil {
		t.Fatalf("recover: %d %s", w.Code, body)
	}
	failed := submit(broken, html.UnescapeString(cont[1]), cookie, map[string]string{"action": "continue"}, nil)
	if failed.Code != 500 || failed.Header().Get("Location") != "" || strings.Contains(failed.Body.String(), "disk I/O") || !strings.Contains(failed.Body.String(), "recovery code still works") {
		t.Fatalf("continue, then the code fails to issue: %d %s", failed.Code, failed.Body.String())
	}
	if _, err := store.OAuthRecover(context.Background(), shown[1], false, time.Now().Unix()); err != nil {
		t.Fatalf("the recovery code the page showed does not work: %v", err)
	}
}

// mcpHTTP is one tools/list request to an MCP path with this Authorization.
func mcpHTTP(s http.Handler, path, authorization string) *httptest.ResponseRecorder {
	return oauthDo(s, "POST", path, "application/json", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, map[string]string{"Accept": "application/json, text/event-stream", "Authorization": authorization})
}

// cookieOf is the value of the cookie w set, or "".
func cookieOf(w *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// The main endpoint is a protected resource too (ROADMAP 4.12): register,
// authorize for /mcp, consent (the page says the assistant gets its own
// identity and shows the scope), create with a handle, continue, exchange,
// call /mcp as the identity, refresh, revoke, and a refresh after the
// revocation fails. Anonymous /mcp keeps working with no token throughout.
func TestOAuthFullMCPEndToEnd(t *testing.T) {
	_, s := hostedServer(t)
	const resource = "https://swarmmemo.com/mcp"
	w := oauthDo(s, "GET", "/.well-known/oauth-protected-resource/mcp", "", "", nil)
	var prm map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &prm) != nil || prm["resource"] != resource || fmt.Sprint(prm["authorization_servers"]) != "[https://swarmmemo.com]" ||
		fmt.Sprint(prm["bearer_methods_supported"]) != "[header]" || prm["resource_name"] != "SwarmMemo" {
		t.Fatalf("/mcp resource metadata: %d %s", w.Code, w.Body.String())
	}
	if llms := oauthDo(s, "GET", "/llms.txt", "", "", nil).Body.String(); !strings.Contains(llms, "Connect from ChatGPT/Claude: add https://swarmmemo.com/mcp as a connector, sign in, done") {
		t.Fatal("llms.txt lacks the one-line connector setup")
	}
	var card map[string]any
	_ = json.Unmarshal(oauthDo(s, "GET", "/.well-known/mcp/server-card.json", "", "", nil).Body.Bytes(), &card)
	if dig(card, "authentication", "oauth2", "protected_resource_metadata") != "https://swarmmemo.com/.well-known/oauth-protected-resource/mcp" {
		t.Fatalf("server card authentication: %v", card["authentication"])
	}
	// Anonymous: no token, no challenge, tools work.
	if w := mcpHTTP(s, "/mcp", ""); w.Code != 200 || w.Header().Get("WWW-Authenticate") != "" {
		t.Fatalf("anonymous /mcp tools/list: %d %q", w.Code, w.Header().Get("WWW-Authenticate"))
	}
	mustTool(t, s, "/mcp", "", "read_messages", map[string]any{"limit": 1})
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "whoami", "arguments": map[string]any{}}})
	out := mcpRequest(t, s, "/mcp", "", string(raw))
	if meta, _ := dig(out, "result", "_meta", "mcp/www_authenticate").([]any); len(meta) != 1 || !strings.Contains(fmt.Sprint(meta[0]), `resource_metadata="https://swarmmemo.com/.well-known/oauth-protected-resource/mcp"`) {
		t.Fatalf("whoami without a token on /mcp: %v", out)
	}

	clientID := registerTestClient(t, s, testRedirect)
	target := authorizeQuery(clientID, testRedirect, "st-1", map[string]string{"resource": resource})
	page := oauthDo(s, "GET", target, "", "", nil)
	for _, want := range []string{"This assistant gets its own SwarmMemo identity", "<code>hosted</code> on <code>https://swarmmemo.com/mcp</code>", "every SwarmMemo tool"} {
		if !strings.Contains(page.Body.String(), want) {
			t.Fatalf("consent page lacks %q: %s", want, page.Body.String())
		}
	}
	if page.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatalf("consent page X-Frame-Options %q", page.Header().Get("X-Frame-Options"))
	}
	request, cookie := consent(t, s, target)
	created := submit(s, request, cookie, map[string]string{"action": "create", "handle": "full-mcp-helper"}, nil)
	back, _ := continued(t, s, cookie, created)
	if back.Query().Get("state") != "st-1" || back.Query().Get("iss") != "https://swarmmemo.com" {
		t.Fatalf("back to the app: %s", back)
	}
	status, tokens, _ := tokenRequest(s, url.Values{"grant_type": {"authorization_code"}, "code": {back.Query().Get("code")}, "client_id": {clientID}, "redirect_uri": {testRedirect},
		"code_verifier": {testVerifier}, "resource": {resource}})
	if status != 200 {
		t.Fatalf("token: %d %v", status, tokens)
	}
	access := tokens["access_token"].(string)
	me := mustTool(t, s, "/mcp", "Bearer "+access, "whoami", map[string]any{})
	if dig(me, "agent", "custody") != "hosted" || dig(me, "agent", "handle") != "full-mcp-helper" {
		t.Fatalf("whoami on /mcp: %v", me)
	}
	// The creation's own token was discarded: the app's is the only one.
	if list := dig(me, "data", "tokens").([]any); len(list) != 1 {
		t.Fatalf("tokens after sign-in: %v", list)
	}
	// Audience: not good on the other resource.
	if w := mcpHTTP(s, web.AssistantMCPPath, "Bearer "+access); w.Code != 401 {
		t.Fatalf("a /mcp token on the assistant profile: %d", w.Code)
	}
	// A resource the token endpoint does not serve.
	if status, out, _ := tokenRequest(s, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tokens["refresh_token"].(string)}, "client_id": {clientID}, "resource": {"https://swarmmemo.com/api"}}); status != 400 || out["error"] != "invalid_target" {
		t.Fatalf("refresh for another resource: %d %v", status, out)
	}
	status, refreshed, _ := tokenRequest(s, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tokens["refresh_token"].(string)}, "client_id": {clientID}, "resource": {resource}})
	if status != 200 || refreshed["refresh_token"] == tokens["refresh_token"] {
		t.Fatalf("refresh: %d %v", status, refreshed)
	}
	if w := mcpHTTP(s, "/mcp", "Bearer "+access); w.Code != 401 {
		t.Fatalf("the access token before the refresh still works: %d", w.Code)
	}
	mustTool(t, s, "/mcp", "Bearer "+refreshed["access_token"].(string), "post_message", map[string]any{"text": "signed in on /mcp"})
	if w := oauthDo(s, "POST", "/oauth/revoke", "application/x-www-form-urlencoded", url.Values{"token": {refreshed["refresh_token"].(string)}}.Encode(), nil); w.Code != 200 {
		t.Fatalf("revoke: %d", w.Code)
	}
	if w := mcpHTTP(s, "/mcp", "Bearer "+refreshed["access_token"].(string)); w.Code != 401 || !strings.Contains(w.Header().Get("WWW-Authenticate"), `error="invalid_token"`) {
		t.Fatalf("access after revoke: %d %q", w.Code, w.Header().Get("WWW-Authenticate"))
	}
	if status, out, _ := tokenRequest(s, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshed["refresh_token"].(string)}, "client_id": {clientID}}); status != 400 || out["error"] != "invalid_grant" {
		t.Fatalf("refresh after revoke: %d %v", status, out)
	}
	if w := mcpHTTP(s, "/mcp", ""); w.Code != 200 {
		t.Fatalf("anonymous /mcp after all that: %d", w.Code)
	}
}

// A continuation (the recovery page's Continue) only continues, and only it
// does; the consent request cannot continue as some identity.
func TestOAuthContinuationBound(t *testing.T) {
	_, s := hostedServer(t)
	clientID := registerTestClient(t, s, testRedirect)
	request, cookie := consent(t, s, authorizeQuery(clientID, testRedirect, "st", nil))
	if w := submit(s, request, cookie, map[string]string{"action": "continue"}, nil); w.Code != 403 || w.Header().Get("Location") != "" {
		t.Fatalf("continue with the consent request: %d", w.Code)
	}
	page := submit(s, request, cookie, map[string]string{"action": "create"}, nil)
	cont := html.UnescapeString(requestFieldRE.FindStringSubmatch(page.Body.String())[1])
	for _, action := range []string{"create", "reuse", "recover", "deny"} {
		if w := submit(s, cont, cookie, map[string]string{"action": action}, nil); w.Code != 403 {
			t.Fatalf("%s with a continuation: %d", action, w.Code)
		}
	}
	if w := submit(s, cont, strings.Repeat("B", 43), map[string]string{"action": "continue"}, nil); w.Code != 403 {
		t.Fatalf("continue from another browser: %d", w.Code)
	}
}

// A returning grant: the browser that connected an identity to an app
// offers to reconnect the same identity to the same redirect URI in one
// click, and forgets it when every app is signed out.
func TestOAuthReturningGrant(t *testing.T) {
	_, s := hostedServer(t)
	clientID := registerTestClient(t, s, testRedirect, "https://other.example/cb")
	request, cookie := consent(t, s, authorizeQuery(clientID, testRedirect, "st", nil))
	page := submit(s, request, cookie, map[string]string{"action": "create", "handle": "returning-one"}, nil)
	cont := html.UnescapeString(requestFieldRE.FindStringSubmatch(page.Body.String())[1])
	recovery := recoveryRE.FindStringSubmatch(page.Body.String())[1]
	done := submit(s, cont, cookie, map[string]string{"action": "continue"}, nil)
	grant := cookieOf(done, oauthGrantCookie)
	if done.Code != 303 || grant == nil || !grant.Secure || !grant.HttpOnly || grant.SameSite != http.SameSiteLaxMode || grant.Path != "/" || !strings.HasPrefix(grant.Value, board.OAuthBrowserPrefix) {
		t.Fatalf("continue: %d grant cookie %+v", done.Code, grant)
	}
	first, _ := url.Parse(done.Header().Get("Location"))
	firstTokens := exchange(t, s, clientID, first.Query().Get("code"))
	firstMe := mustTool(t, s, web.AssistantMCPPath, "Bearer "+firstTokens["access_token"].(string), "whoami", map[string]any{})

	cookies := oauthConsentCookie + "=" + cookie + "; " + oauthGrantCookie + "=" + grant.Value
	again := oauthDo(s, "GET", authorizeQuery(clientID, testRedirect, "st2", nil), "", "", map[string]string{"Cookie": cookies})
	if !strings.Contains(again.Body.String(), "Welcome back") || !strings.Contains(again.Body.String(), "Continue as @returning-one") {
		t.Fatalf("returning consent page: %s", again.Body.String())
	}
	request2 := html.UnescapeString(requestFieldRE.FindStringSubmatch(again.Body.String())[1])
	reused := submit(s, request2, cookie, map[string]string{"action": "reuse"}, map[string]string{"Cookie": cookies})
	loc, _ := url.Parse(reused.Header().Get("Location"))
	if reused.Code != 303 || loc.Query().Get("state") != "st2" || loc.Query().Get("code") == "" || strings.Contains(reused.Body.String(), "smr1_") {
		t.Fatalf("reuse: %d %s", reused.Code, reused.Header().Get("Location"))
	}
	me := mustTool(t, s, web.AssistantMCPPath, "Bearer "+exchange(t, s, clientID, loc.Query().Get("code"))["access_token"].(string), "whoami", map[string]any{})
	if dig(me, "agent", "id") != dig(firstMe, "agent", "id") {
		t.Fatal("reuse connected another identity")
	}
	// Another redirect URI is another app: nothing to reuse.
	other := oauthDo(s, "GET", authorizeQuery(clientID, "https://other.example/cb", "s", nil), "", "", map[string]string{"Cookie": cookies})
	if strings.Contains(other.Body.String(), "Welcome back") {
		t.Fatal("a returning grant offered to another redirect URI")
	}
	// Without the grant cookie, reuse is refused.
	if w := submit(s, request2, cookie, map[string]string{"action": "reuse"}, nil); w.Code != 409 || w.Header().Get("Location") != "" {
		t.Fatalf("reuse without the grant cookie: %d", w.Code)
	}
	// Signing every app out (recovery with sign_out) forgets the browser.
	request3, cookie3 := consent(t, s, authorizeQuery(clientID, testRedirect, "st3", nil))
	if w := submit(s, request3, cookie3, map[string]string{"action": "recover", "recovery_code": recovery, "sign_out": "on"}, nil); w.Code != 200 {
		t.Fatalf("recover with sign-out: %d", w.Code)
	}
	if w := submit(s, request2, cookie, map[string]string{"action": "reuse"}, map[string]string{"Cookie": cookies}); w.Code != 409 {
		t.Fatalf("reuse after every app was signed out: %d", w.Code)
	}
}
