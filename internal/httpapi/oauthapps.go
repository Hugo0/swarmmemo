package httpapi

// Sign in with SwarmMemo for apps (S2) and app tokens (S7), the HTTP side:
//
//	GET  /.well-known/jwks.json  the keys app tokens are signed with (RFC 7517),
//	                             beside the agent card's (agentCardKeys)
//	POST /oauth/introspect       is an app token live (RFC 7662)
//	GET  /oauth/userinfo         {sub, handle, band} for a token with scope identity
//	POST /v1/command             with Authorization: Bearer APP_TOKEN, an unsigned
//	                             command signed as the person, within the token's scopes
//
// The token exchange grant is in tokenEndpoint (oauth.go); the board does
// every check (board/oauthjwt.go, oauthscopes.go).

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"swarmmemo/internal/board"
)

const (
	oauthJWKSPath       = agentCardKeysPath // one JWK Set: the agent card's key and app tokens' keys
	oauthIntrospectPath = "/oauth/introspect"
	oauthUserinfoPath   = "/oauth/userinfo"
)

// appGrant is the scope and resource an authorization request gets.
type appGrant struct {
	Scope, Resource, AppName, AppHome string
}

// authorizeGrant resolves an authorization request's scope and resource
// for client, or the RFC 6749 error to send back. A registered app client
// asks for app scopes within its ceiling, for its app's SwarmMemo resource
// or one it serves; any other client asks for hosted on an MCP profile, or
// for app scopes on a resource a registered app serves (an agent signing in
// to that app's MCP server).
func (s *Server) authorizeGrant(ctx context.Context, o oauthStore, client board.OAuthClient, scope, resource string) (appGrant, string, string) {
	appScopes := func(ceiling []string) (string, string, string) {
		if scope == "" {
			return strings.Join(ceiling, " "), "", ""
		}
		want, ok := board.ParseOAuthScopes(scope)
		if !ok || slices.ContainsFunc(want, func(w string) bool { return !slices.Contains(ceiling, w) }) {
			return "", "invalid_scope", "this app may ask for " + strings.Join(ceiling, " ")
		}
		return strings.Join(want, " "), "", ""
	}
	if client.App != "" {
		g := appGrant{Resource: o.OAuthAppResource(client.App), AppName: client.AppName, AppHome: client.AppHomepage}
		if resource != "" && resource != g.Resource && !slices.Contains(client.Resources, resource) {
			return g, "invalid_target", "the resource is " + g.Resource + " or one this app serves"
		}
		if resource != "" {
			g.Resource = resource
		}
		var code, why string
		g.Scope, code, why = appScopes(client.Scopes)
		return g, code, why
	}
	if resource == "" {
		if scope != "" && scope != board.OAuthScope {
			return appGrant{}, "invalid_scope", "the only scope here is " + board.OAuthScope
		}
		return appGrant{Scope: board.OAuthScope, Resource: s.oauthResource(oauthDefaultProfile)}, "", ""
	}
	if profile, ok := s.resourceProfile(resource); ok {
		if scope != "" && scope != board.OAuthScope {
			return appGrant{}, "invalid_scope", "the only scope on " + s.oauthResource(profile) + " is " + board.OAuthScope
		}
		return appGrant{Scope: board.OAuthScope, Resource: s.oauthResource(profile)}, "", ""
	}
	app, ceiling, ok, err := o.OAuthResourceOwner(ctx, resource)
	if err != nil || !ok {
		return appGrant{}, "invalid_target", s.resourcesLine() + " Or a resource a registered app serves."
	}
	g := appGrant{Resource: resource, AppName: app.Name, AppHome: app.Homepage}
	var code, why string
	g.Scope, code, why = appScopes(ceiling)
	return g, code, why
}

// appResource reports a resource app tokens are issued for: SwarmMemo's
// own resource for an app, or one a registered app serves.
func (s *Server) appResource(ctx context.Context, o oauthStore, resource string) bool {
	if app, ok := strings.CutPrefix(resource, s.cfg.PublicURL+"/app/"); ok && len(app) == 64 && strings.Trim(app, "0123456789abcdef") == "" {
		return true
	}
	_, _, ok, err := o.OAuthResourceOwner(ctx, resource)
	return ok && err == nil
}

// scopeLines are the consent page's plain lines for an app scope list.
func scopeLines(scope string) []string {
	names, ok := board.ParseOAuthScopes(scope)
	if !ok {
		return nil
	}
	var lines []string
	for _, spec := range board.OAuthScopes() {
		if slices.Contains(names, spec.Name) {
			lines = append(lines, spec.Plain)
		}
	}
	return lines
}

// oauthAppRoute serves introspection and userinfo; false for any other
// path. The JWKS is the one at agentCardKeysPath.
func (s *Server) oauthAppRoute(w http.ResponseWriter, r *http.Request, o oauthStore) bool {
	now := time.Now().Unix()
	switch r.URL.Path {
	case oauthIntrospectPath:
		if !s.oauth.token.Admit(s.peer(r)) {
			w.Header().Set("Retry-After", "2")
			oauthJSON(w, 429, map[string]any{"error": "slow_down", "error_description": "Too many requests from this network; retry shortly."})
			return true
		}
		form, err := oauthForm(w, r)
		if err != nil {
			oauthJSONError(w, err)
			return true
		}
		out, err := o.OAuthIntrospection(r.Context(), form.Get("token"), now)
		if err != nil {
			oauthJSONError(w, err)
			return true
		}
		oauthJSON(w, 200, out)
	case oauthUserinfoPath:
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			methodError(w)
			return true
		}
		out, err := o.OAuthUserinfo(r.Context(), bearerToken(r.Header.Get("Authorization")), now)
		if err != nil {
			s.bearerError(w, err)
			return true
		}
		w.Header().Set("Cache-Control", "no-store")
		jsonResponse(w, 200, out)
	default:
		return false
	}
	return true
}

// bearerError writes a refusal of an app token with its RFC 6750
// challenge.
func (s *Server) bearerError(w http.ResponseWriter, err error) {
	var be *board.Error
	if errors.As(err, &be) {
		switch be.Code {
		case "invalid_token":
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token", error_description="The access token is invalid or expired"`)
		case "insufficient_scope":
			w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope"`)
		}
	}
	writeError(w, err)
}

// bearerCommand runs a /v1/command body sent with an app's token: the
// body is unsigned, SwarmMemo signs it as the token's identity (as hosted
// MCP signs a tool's command), and the board runs it within the token's
// scopes with via app:APP. Each connection makes hostedRatePerMinute calls.
func (s *Server) bearerCommand(w http.ResponseWriter, r *http.Request, cmd board.Command, token string) {
	o := s.oauthStore()
	if o == nil {
		s.bearerError(w, board.HostedOff())
		return
	}
	if cmd.PublicKey != "" || cmd.Signature != "" || cmd.Nonce != "" || cmd.Timestamp != 0 || cmd.Delegation != nil || cmd.PrivateRead != nil {
		writeError(w, &board.Error{Status: 400, Code: "ambiguous_command", Message: "With an app's token, send the command unsigned: no public_key, signature, timestamp, nonce, delegation or private_read; SwarmMemo signs it as the person who signed in."})
		return
	}
	if _, bridged := r.Header[http.CanonicalHeaderKey(bridgeHeader)]; bridged {
		writeError(w, &board.Error{Status: 403, Code: "bridge_unverified", Message: "An app's token is not a bridge: send the command without " + bridgeHeader + "."})
		return
	}
	key, _, claims, err := o.BearerSigner(r.Context(), token, time.Now().Unix())
	clear(key)
	if err != nil {
		s.bearerError(w, err)
		return
	}
	if !s.hostedLimiter.Admit("oauth:" + claims.Session) {
		writeError(w, errSignInRate())
		return
	}
	ctx := board.WithBearerToken(board.WithVia(r.Context(), "command"), token)
	sign := func(c board.Command, now, at int64) (board.Command, error) {
		key, publicKey, _, err := o.BearerSigner(ctx, token, now)
		if err != nil {
			return board.Command{}, err
		}
		return signAs(s.cfg.ServiceID, key, publicKey, c, now, at), nil
	}
	s.executeWith(w, r.WithContext(ctx), cmd, func(ctx context.Context, c board.Command) (board.Result, error) {
		res, err := execSignedRetry(c, sign, func(signed board.Command) (board.Result, error) { return s.service.Execute(ctx, signed, s.peer(r)) })
		var be *board.Error
		if errors.As(err, &be) && (be.Code == "invalid_token" || be.Code == "insufficient_scope") {
			s.bearerError(discardHeaders{w}, err)
		}
		return res, err
	})
}

// discardHeaders sets bearerError's challenge on w and drops its body:
// executeWith writes the error itself.
type discardHeaders struct{ w http.ResponseWriter }

func (d discardHeaders) Header() http.Header         { return d.w.Header() }
func (d discardHeaders) Write(b []byte) (int, error) { return len(b), nil }
func (d discardHeaders) WriteHeader(int)             {}

// oauthAppCapabilities is /capabilities conversations.hosted.oauth.apps.
func (s *Server) oauthAppCapabilities() map[string]any {
	return map[string]any{
		"scopes":            board.OAuthScopes(),
		"clients":           "a registered app's own clients (oauth.client.register): client_id " + board.OAuthAppClientPrefix + "…, exact redirect URIs (localhost only with dev_redirects), a scope ceiling and up to " + strconv.Itoa(board.OAuthAppResourcesMax) + " resources it serves; at most " + strconv.Itoa(board.OAuthAppClientsMax) + " live per app",
		"operations":        []string{"oauth.client.register", "oauth.client.retire", "oauth.clients.list", "oauth.connections.list", "oauth.connection.revoke"},
		"access_token":      map[string]any{"is": "a JWT (typ at+jwt, alg EdDSA) signed by a key in the JWKS", "expires_in": board.OAuthJWTAccessSeconds, "audience": s.cfg.PublicURL + "/app/APP_FINGERPRINT, or a resource the app serves", "carrier": "Authorization: Bearer on POST /v1/command (an unsigned command, signed as the person within the token's scopes)"},
		"never":             "keys, identity, devices, grants, recovery and credentials: an app's token is refused for them whatever its scope",
		"provenance":        "every action through an app's token carries via app:APP_FINGERPRINT, on the message and in the audit log",
		"jwks_uri":          s.cfg.PublicURL + oauthJWKSPath,
		"key_rotation":      map[string]any{"every_seconds": board.OAuthSigningKeySeconds, "retired_key_published_seconds": board.OAuthSigningKeyGraceSeconds, "kid": "RFC 7638 thumbprint"},
		"token_exchange":    map[string]any{"grant_type": board.OAuthGrantTokenExchange, "subject_token_type": board.OAuthTokenTypeAccess, "actor_token_type": board.OAuthTokenTypeJWT, "actor_token": "a JWT (EdDSA) a live key of the app signed: iss and sub the client_id, aud " + s.cfg.PublicURL + oauthTokenPath + ", a jti used once, at most " + strconv.Itoa(board.OAuthActorTokenSeconds) + " seconds", "rule": "narrows only: scopes within both the subject token's and the client's, life within the subject token's"},
		"introspection":     s.cfg.PublicURL + oauthIntrospectPath,
		"userinfo":          s.cfg.PublicURL + oauthUserinfoPath,
		"connections_tools": "manage_tokens connections and revoke_connection; manage_app register_client, list_clients and retire_client",
	}
}
