package httpapi

// OAuth 2.1 for the hosted MCP assistant profile (T56), per the MCP
// authorization spec (2025-06-18 and 2025-11-25): /mcp/assistant is a
// protected resource whose authorization server is this origin.
//
//	GET  /.well-known/oauth-protected-resource[/mcp/assistant]  RFC 9728
//	GET  /.well-known/oauth-authorization-server                RFC 8414
//	GET  /oauth/authorize   the sign-in and consent page
//	POST /oauth/authorize   create an identity, sign in with a recovery code, or cancel
//	POST /oauth/token       authorization_code (PKCE S256) and refresh_token grants
//	POST /oauth/register    dynamic client registration (RFC 7591), public clients
//	POST /oauth/revoke      token revocation (RFC 7009)
//
// Signing in IS a hosted identity (board/oauth.go): no email, no password,
// no third-party login. Clients are public (token_endpoint_auth_method
// none) and identify themselves by a client ID metadata document (an HTTPS
// client_id, fetched through safenet) or by dynamic registration. Anonymous
// MCP calls and the /mcp/t/TOKEN path work exactly as before; an OAuth token
// is accepted only in a header, on /mcp/assistant.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"swarmmemo/internal/board"
	"swarmmemo/internal/safenet"
	"swarmmemo/internal/web"
)

const (
	oauthAuthorizePath = "/oauth/authorize"
	oauthTokenPath     = "/oauth/token"
	oauthRegisterPath  = "/oauth/register"
	oauthRevokePath    = "/oauth/revoke"
	oauthPRMPath       = "/.well-known/oauth-protected-resource"
	oauthASMPath       = "/.well-known/oauth-authorization-server"
	// oauthConsentCookie carries the nonce the consent form's signed request
	// is bound to (double submit), so another site cannot post the form.
	oauthConsentCookie = "__Host-swarmmemo-oauth"
	// oauthConsentSeconds is how long a consent page may be submitted.
	oauthConsentSeconds = 900
	oauthFormBytes      = 16 << 10
	oauthStateBytes     = 1024
	// The client ID metadata document fetch (CIMD): bounded in size, time,
	// concurrency and cache.
	cimdBytes      = 8 << 10
	cimdTimeout    = 5 * time.Second
	cimdCacheTTL   = time.Hour
	cimdCacheMax   = 256
	cimdConcurrent = 4
)

// oauthStore is what the board offers OAuth (board.Store).
type oauthStore interface {
	OAuthRegisterClient(ctx context.Context, name string, redirects []string, peer string, now int64) (board.OAuthClient, error)
	OAuthRegisteredClient(ctx context.Context, id string) (board.OAuthClient, bool, error)
	OAuthIssueCode(ctx context.Context, g board.OAuthGrant, account, discardTokenID string, now int64) (string, error)
	OAuthRecover(ctx context.Context, code string, signOut bool, now int64) (board.OAuthRecovered, error)
	OAuthExchangeCode(ctx context.Context, code, clientID, redirectURI, verifier, resource string, now int64) (board.OAuthTokens, error)
	OAuthRefresh(ctx context.Context, refresh, clientID, resource, scope string, now int64) (board.OAuthTokens, error)
	OAuthRevoke(ctx context.Context, token, clientID string, now int64) error
	OAuthAccessToken(ctx context.Context, token string) (bool, error)
}

// oauthStore is the board's OAuth, or nil while hosted identities are off:
// signing in makes or recovers one.
func (s *Server) oauthStore() oauthStore {
	if s.hostedStore() == nil {
		return nil
	}
	o, _ := s.service.(oauthStore)
	return o
}

// oauthState is the Server's OAuth state: the consent forms' MAC key, made
// per process (a restart only invalidates open consent pages), the rate
// limits and the client ID metadata document cache.
type oauthState struct {
	key []byte
	// authorize is the per-network budget of authorization page loads and
	// submissions; token of token, registration and revocation requests,
	// which a vendor's servers make for all their users from few addresses;
	// recovery of recovery-code attempts.
	authorize, token, recovery *Limiter
	cimdMu                     sync.Mutex
	cimd                       map[string]cimdEntry
	cimdSlots                  chan struct{}
	// fetch gets a client ID metadata document; tests replace it.
	fetch func(ctx context.Context, clientID string) ([]byte, error)
}

type cimdEntry struct {
	client board.OAuthClient
	at     time.Time
}

func newOAuthState() oauthState {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic(err)
	}
	return oauthState{key: key, authorize: NewLimiterRate(30, 0.5), token: NewLimiterRate(120, 2), recovery: NewLimiterRate(5, 5.0/600),
		cimd: map[string]cimdEntry{}, cimdSlots: make(chan struct{}, cimdConcurrent), fetch: fetchCIMD}
}

// oauthResource is the protected resource: the assistant profile's URL.
func (s *Server) oauthResource() string { return s.cfg.PublicURL + web.AssistantMCPPath }

// oauthPRMURL is the resource's metadata, at the path-inserted well-known URI.
func (s *Server) oauthPRMURL() string { return s.cfg.PublicURL + oauthPRMPath + web.AssistantMCPPath }

// oauthChallenge is the WWW-Authenticate value of a 401 on the resource.
func (s *Server) oauthChallenge(errorCode, description string) string {
	v := `Bearer resource_metadata="` + s.oauthPRMURL() + `", scope="` + board.OAuthScope + `"`
	if errorCode != "" {
		v += `, error="` + errorCode + `", error_description="` + description + `"`
	}
	return v
}

// oauthRoute serves the OAuth endpoints and the directory's domain
// verification; false for any other path.
func (s *Server) oauthRoute(w http.ResponseWriter, r *http.Request) bool {
	path := r.URL.Path
	if s.cfg.AppsChallengeToken != "" && path == s.appsChallengePath() {
		if !readMethod(r) {
			methodError(w)
			return true
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, s.cfg.AppsChallengeToken)
		return true
	}
	switch path {
	case oauthPRMPath, oauthPRMPath + web.AssistantMCPPath, oauthASMPath, oauthAuthorizePath, oauthTokenPath, oauthRegisterPath, oauthRevokePath:
	default:
		return false
	}
	o := s.oauthStore()
	if o == nil {
		writeError(w, &board.Error{Status: 404, Code: "not_found", Message: "Sign-in (OAuth) is off on this server: hosted identities are off."})
		return true
	}
	switch path {
	case oauthPRMPath, oauthPRMPath + web.AssistantMCPPath:
		if !readMethod(r) {
			methodError(w)
			return true
		}
		w.Header().Set("Cache-Control", "public, max-age=3600")
		jsonResponse(w, 200, s.protectedResourceMetadata())
	case oauthASMPath:
		if !readMethod(r) {
			methodError(w)
			return true
		}
		w.Header().Set("Cache-Control", "public, max-age=3600")
		jsonResponse(w, 200, s.authorizationServerMetadata())
	case oauthAuthorizePath:
		s.authorize(w, r, o)
	case oauthTokenPath:
		s.tokenEndpoint(w, r, o)
	case oauthRegisterPath:
		s.registerEndpoint(w, r, o)
	case oauthRevokePath:
		s.revokeEndpoint(w, r, o)
	}
	return true
}

// appsChallengePath is where the plugin directory's domain verification
// token is served.
func (s *Server) appsChallengePath() string {
	if s.cfg.AppsChallengePath != "" {
		return s.cfg.AppsChallengePath
	}
	return DefaultAppsChallengePath
}

// DefaultAppsChallengePath is the OpenAI plugin directory's domain
// verification path.
const DefaultAppsChallengePath = "/.well-known/openai-apps-challenge"

var appsChallengeTokenRE = regexp.MustCompile(`^[A-Za-z0-9._~+/=-]{8,512}$`)
var appsChallengePathRE = regexp.MustCompile(`^/\.well-known/[A-Za-z0-9._-]{1,64}$`)

// ValidAppsChallenge reports whether a configured domain verification path
// and token may be served: a path under /.well-known/ (or empty for the
// default) and a token of 8 to 512 URL-safe characters.
func ValidAppsChallenge(path, token string) bool {
	return (path == "" || appsChallengePathRE.MatchString(path)) && appsChallengeTokenRE.MatchString(token)
}

func (s *Server) protectedResourceMetadata() map[string]any {
	return map[string]any{
		"resource":                 s.oauthResource(),
		"authorization_servers":    []string{s.cfg.PublicURL},
		"scopes_supported":         []string{board.OAuthScope},
		"bearer_methods_supported": []string{"header"},
		"resource_name":            "SwarmMemo for assistants",
		"resource_documentation":   s.cfg.PublicURL + "/protocol.md#signing-in-with-oauth",
		"resource_policy_uri":      s.cfg.PublicURL + "/privacy",
		"resource_tos_uri":         s.cfg.PublicURL + "/terms",
	}
}

func (s *Server) authorizationServerMetadata() map[string]any {
	origin := s.cfg.PublicURL
	return map[string]any{
		"issuer":                                         origin,
		"authorization_endpoint":                         origin + oauthAuthorizePath,
		"token_endpoint":                                 origin + oauthTokenPath,
		"registration_endpoint":                          origin + oauthRegisterPath,
		"revocation_endpoint":                            origin + oauthRevokePath,
		"scopes_supported":                               []string{board.OAuthScope},
		"response_types_supported":                       []string{"code"},
		"response_modes_supported":                       []string{"query"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"token_endpoint_auth_methods_supported":          []string{"none"},
		"revocation_endpoint_auth_methods_supported":     []string{"none"},
		"code_challenge_methods_supported":               []string{"S256"},
		"client_id_metadata_document_supported":          true,
		"authorization_response_iss_parameter_supported": true,
		"service_documentation":                          origin + "/protocol.md#signing-in-with-oauth",
		"op_policy_uri":                                  origin + "/privacy",
		"op_tos_uri":                                     origin + "/terms",
	}
}

// oauthJSONError is a token, registration or revocation error answer.
func oauthJSONError(w http.ResponseWriter, err error) {
	var oe *board.OAuthError
	if !errors.As(err, &oe) {
		oe = &board.OAuthError{Status: 500, Code: "server_error", Description: "The request failed; retry shortly."}
		var be *board.Error
		if errors.As(err, &be) && be.Status == 503 {
			oe = &board.OAuthError{Status: 503, Code: "temporarily_unavailable", Description: be.Message}
		}
	}
	if oe.Status == 401 {
		w.Header().Set("WWW-Authenticate", `Basic realm="swarmmemo"`)
	}
	oauthJSON(w, oe.Status, map[string]any{"error": oe.Code, "error_description": oe.Description})
}

func oauthJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	jsonResponse(w, status, v)
}

func oauthBad(code, description string) *board.OAuthError {
	return &board.OAuthError{Status: 400, Code: code, Description: description}
}

// oauthForm parses a form-encoded POST body, refusing a repeated parameter
// (RFC 6749 §3.1) and parameters in the query.
func oauthForm(w http.ResponseWriter, r *http.Request) (url.Values, error) {
	if r.Method != http.MethodPost {
		return nil, &board.OAuthError{Status: 405, Code: "invalid_request", Description: "Use POST."}
	}
	if mt, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";"); strings.TrimSpace(strings.ToLower(mt)) != "application/x-www-form-urlencoded" {
		return nil, oauthBad("invalid_request", "Send application/x-www-form-urlencoded.")
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, oauthFormBytes))
	if err != nil {
		return nil, oauthBad("invalid_request", "The body is too large.")
	}
	form, err := url.ParseQuery(string(body))
	if err != nil || r.URL.RawQuery != "" {
		return nil, oauthBad("invalid_request", "Malformed form, or parameters in the query.")
	}
	for name, values := range form {
		if len(values) > 1 {
			return nil, oauthBad("invalid_request", "The parameter "+name+" is repeated.")
		}
	}
	return form, nil
}

// tokenEndpoint is POST /oauth/token.
func (s *Server) tokenEndpoint(w http.ResponseWriter, r *http.Request, o oauthStore) {
	if !s.oauth.token.Admit(s.peer(r)) {
		w.Header().Set("Retry-After", "2")
		oauthJSON(w, 429, map[string]any{"error": "slow_down", "error_description": "Too many token requests from this network; retry shortly."})
		return
	}
	form, err := oauthForm(w, r)
	if err != nil {
		oauthJSONError(w, err)
		return
	}
	// Public clients only: a client secret or assertion is not accepted.
	if r.Header.Get("Authorization") != "" || form.Get("client_secret") != "" || form.Get("client_assertion") != "" {
		oauthJSONError(w, &board.OAuthError{Status: 401, Code: "invalid_client", Description: "Clients here are public: send client_id and PKCE, no client secret or assertion (token_endpoint_auth_method none)."})
		return
	}
	clientID := form.Get("client_id")
	if clientID == "" {
		oauthJSONError(w, oauthBad("invalid_request", "client_id is required."))
		return
	}
	now := time.Now().Unix()
	resource := form.Get("resource")
	if resource != "" && !s.ourResource(resource) {
		oauthJSONError(w, oauthBad("invalid_target", "The only resource is "+s.oauthResource()+"."))
		return
	}
	if resource != "" {
		resource = s.oauthResource()
	}
	var tokens board.OAuthTokens
	switch form.Get("grant_type") {
	case "authorization_code":
		tokens, err = o.OAuthExchangeCode(r.Context(), form.Get("code"), clientID, form.Get("redirect_uri"), form.Get("code_verifier"), resource, now)
	case "refresh_token":
		tokens, err = o.OAuthRefresh(r.Context(), form.Get("refresh_token"), clientID, resource, form.Get("scope"), now)
	default:
		err = oauthBad("unsupported_grant_type", "grant_type is authorization_code or refresh_token.")
	}
	if err != nil {
		oauthJSONError(w, err)
		return
	}
	oauthJSON(w, 200, map[string]any{"access_token": tokens.AccessToken, "token_type": "Bearer", "expires_in": tokens.ExpiresIn,
		"refresh_token": tokens.RefreshToken, "scope": tokens.Scope})
}

// revokeEndpoint is POST /oauth/revoke: 200 whether or not the token was
// one (RFC 7009 §2.2).
func (s *Server) revokeEndpoint(w http.ResponseWriter, r *http.Request, o oauthStore) {
	if !s.oauth.token.Admit(s.peer(r)) {
		w.Header().Set("Retry-After", "2")
		oauthJSON(w, 429, map[string]any{"error": "slow_down", "error_description": "Too many requests from this network; retry shortly."})
		return
	}
	form, err := oauthForm(w, r)
	if err == nil && form.Get("token") == "" {
		err = oauthBad("invalid_request", "token is required.")
	}
	if err == nil {
		err = o.OAuthRevoke(r.Context(), form.Get("token"), form.Get("client_id"), time.Now().Unix())
	}
	if err != nil {
		oauthJSONError(w, err)
		return
	}
	oauthJSON(w, 200, map[string]any{})
}

// registerEndpoint is POST /oauth/register: a public client with its
// redirect URIs and name.
func (s *Server) registerEndpoint(w http.ResponseWriter, r *http.Request, o oauthStore) {
	if r.Method != http.MethodPost {
		methodError(w)
		return
	}
	if !s.oauth.authorize.Admit("register:" + s.peer(r)) {
		w.Header().Set("Retry-After", "2")
		oauthJSON(w, 429, map[string]any{"error": "slow_down", "error_description": "Too many registrations from this network; retry shortly."})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, oauthFormBytes))
	var in struct {
		RedirectURIs []string `json:"redirect_uris"`
		ClientName   string   `json:"client_name"`
		AuthMethod   string   `json:"token_endpoint_auth_method"`
		GrantTypes   []string `json:"grant_types"`
		Responses    []string `json:"response_types"`
	}
	if err != nil || json.Unmarshal(body, &in) != nil {
		oauthJSONError(w, oauthBad("invalid_client_metadata", "Send a JSON object with redirect_uris."))
		return
	}
	if in.AuthMethod != "" && in.AuthMethod != "none" {
		oauthJSONError(w, oauthBad("invalid_client_metadata", "Clients here are public: token_endpoint_auth_method must be none."))
		return
	}
	for _, g := range in.GrantTypes {
		if g != "authorization_code" && g != "refresh_token" {
			oauthJSONError(w, oauthBad("invalid_client_metadata", "grant_types may be authorization_code and refresh_token."))
			return
		}
	}
	for _, t := range in.Responses {
		if t != "code" {
			oauthJSONError(w, oauthBad("invalid_client_metadata", "response_types may only be code."))
			return
		}
	}
	if len(in.RedirectURIs) == 0 || len(in.RedirectURIs) > board.OAuthRedirectURIsMax {
		oauthJSONError(w, oauthBad("invalid_redirect_uri", "Give 1 to 8 redirect_uris."))
		return
	}
	for _, u := range in.RedirectURIs {
		if !validRedirectURI(u) {
			oauthJSONError(w, oauthBad("invalid_redirect_uri", "Each redirect URI must be https, or http on localhost, 127.0.0.1 or [::1], with no fragment or credentials, up to 512 bytes."))
			return
		}
	}
	client, err := o.OAuthRegisterClient(r.Context(), clientName(in.ClientName, ""), in.RedirectURIs, s.peer(r), time.Now().Unix())
	if err != nil {
		oauthJSONError(w, err)
		return
	}
	oauthJSON(w, 201, map[string]any{"client_id": client.ID, "client_id_issued_at": time.Now().Unix(), "client_name": client.Name, "redirect_uris": client.RedirectURIs,
		"token_endpoint_auth_method": "none", "grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"}})
}

// clientName is a client's display name: printable, at most
// board.OAuthClientNameBytes, else fallback, else "An unnamed app".
func clientName(name, fallback string) string {
	name = strings.Join(strings.FieldsFunc(name, func(r rune) bool {
		return r < 0x20 || r == 0x7f || r == ' ' || r == ' ' || r == '‮' || r == '‭'
	}), " ")
	name = strings.TrimSpace(name)
	for len(name) > board.OAuthClientNameBytes {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	if name == "" {
		name = fallback
	}
	if name == "" {
		name = "An unnamed app"
	}
	return name
}

// validRedirectURI is an absolute https URI, or http on a loopback host,
// with no fragment, credentials or control characters (MCP: redirect URIs
// are localhost or HTTPS).
func validRedirectURI(raw string) bool {
	if raw == "" || len(raw) > board.OAuthRedirectURIBytes || strings.ContainsFunc(raw, func(r rune) bool { return r <= 0x20 || r == 0x7f || r == '\\' }) {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Fragment != "" || strings.Contains(raw, "#") || u.User != nil || u.Opaque != "" || u.Host == "" {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		return loopbackHost(u.Hostname())
	}
	return false
}

func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ourResource reports a resource indicator naming the assistant profile,
// ignoring the case of scheme and host and a trailing slash.
func (s *Server) ourResource(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Fragment != "" || u.RawQuery != "" || u.User != nil {
		return false
	}
	want, _ := url.Parse(s.oauthResource())
	return strings.EqualFold(u.Scheme, want.Scheme) && strings.EqualFold(u.Host, want.Host) && strings.TrimSuffix(u.Path, "/") == want.Path
}

// oauthClient resolves a client_id: an HTTPS URL is a client ID metadata
// document, fetched (and cached) through safenet; smcl_… is a registered
// client. Anything else is unknown.
func (s *Server) oauthClient(ctx context.Context, o oauthStore, id string) (board.OAuthClient, error) {
	unknown := errors.New("This app's client_id is not known here: it is neither a registered client nor a reachable client ID metadata document.")
	if strings.HasPrefix(id, board.OAuthClientPrefix) && len(id) <= 64 {
		client, ok, err := o.OAuthRegisteredClient(ctx, id)
		if err != nil {
			// The page shows this error: never the store's own words.
			return client, errors.New("This app could not be looked up just now; retry in a moment.")
		}
		if !ok {
			return client, unknown
		}
		return client, nil
	}
	if !validCIMDURL(id) {
		return board.OAuthClient{}, unknown
	}
	s.oauth.cimdMu.Lock()
	entry, ok := s.oauth.cimd[id]
	s.oauth.cimdMu.Unlock()
	if ok && time.Since(entry.at) < cimdCacheTTL {
		return entry.client, nil
	}
	select {
	case s.oauth.cimdSlots <- struct{}{}:
		defer func() { <-s.oauth.cimdSlots }()
	default:
		return board.OAuthClient{}, errors.New("Too many sign-ins are starting at once; retry in a moment.")
	}
	fetchCtx, cancel := context.WithTimeout(ctx, cimdTimeout)
	defer cancel()
	raw, err := s.oauth.fetch(fetchCtx, id)
	if err != nil {
		return board.OAuthClient{}, unknown
	}
	client, err := parseCIMD(id, raw)
	if err != nil {
		return board.OAuthClient{}, err
	}
	s.oauth.cimdMu.Lock()
	if len(s.oauth.cimd) >= cimdCacheMax {
		for k, v := range s.oauth.cimd {
			if time.Since(v.at) >= cimdCacheTTL || len(s.oauth.cimd) >= cimdCacheMax {
				delete(s.oauth.cimd, k)
			}
		}
	}
	s.oauth.cimd[id] = cimdEntry{client: client, at: time.Now()}
	s.oauth.cimdMu.Unlock()
	return client, nil
}

// validCIMDURL is a client ID metadata document URL: https on the default
// port, a path that is not just "/", no query, fragment, credentials or dot
// segments (draft-ietf-oauth-client-id-metadata-document §3).
func validCIMDURL(raw string) bool {
	if len(raw) > 512 || !strings.HasPrefix(raw, "https://") || strings.ContainsFunc(raw, func(r rune) bool { return r <= 0x20 || r == 0x7f || r == '\\' }) {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Contains(raw, "#") || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	if u.Path == "" || u.Path == "/" {
		return false
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return net.ParseIP(u.Hostname()) == nil // a name, not an address
}

// parseCIMD checks a fetched metadata document: its client_id is exactly
// the URL, its redirect URIs are valid, and it can be a public client.
func parseCIMD(id string, raw []byte) (board.OAuthClient, error) {
	var doc struct {
		ClientID       string   `json:"client_id"`
		ClientName     string   `json:"client_name"`
		RedirectURIs   []string `json:"redirect_uris"`
		AuthMethod     string   `json:"token_endpoint_auth_method"`
		AuthMethods    []string `json:"token_endpoint_auth_methods_supported"`
		GrantTypes     []string `json:"grant_types"`
		ResponseTypes  []string `json:"response_types"`
		ClientSecret   string   `json:"client_secret"`
		ClientSecretEx any      `json:"client_secret_expires_at"`
	}
	bad := errors.New("This app's client ID metadata document is not valid here: it must name itself as client_id, list https or localhost redirect_uris, and allow a public client (token_endpoint_auth_method none).")
	if json.Unmarshal(raw, &doc) != nil || doc.ClientID != id || doc.ClientSecret != "" || doc.ClientSecretEx != nil ||
		len(doc.RedirectURIs) == 0 || len(doc.RedirectURIs) > board.OAuthRedirectURIsMax {
		return board.OAuthClient{}, bad
	}
	for _, u := range doc.RedirectURIs {
		if !validRedirectURI(u) {
			return board.OAuthClient{}, bad
		}
	}
	public := doc.AuthMethod == "none" || slices.Contains(doc.AuthMethods, "none") || doc.AuthMethod == "" && len(doc.AuthMethods) == 0
	if !public || len(doc.GrantTypes) > 0 && !slices.Contains(doc.GrantTypes, "authorization_code") || len(doc.ResponseTypes) > 0 && !slices.Contains(doc.ResponseTypes, "code") {
		return board.OAuthClient{}, bad
	}
	host, _ := url.Parse(id)
	return board.OAuthClient{ID: id, Name: clientName(doc.ClientName, host.Hostname()), RedirectURIs: doc.RedirectURIs}, nil
}

// fetchCIMD gets a client ID metadata document over HTTPS through safenet
// (public addresses only, checked at connect), following no redirect, with
// a bounded body.
func fetchCIMD(ctx context.Context, clientID string) ([]byte, error) {
	client := &http.Client{
		Timeout: cimdTimeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return safenet.Dial(ctx, network, addr, cimdTimeout)
			},
			TLSHandshakeTimeout:    cimdTimeout,
			ResponseHeaderTimeout:  cimdTimeout,
			MaxResponseHeaderBytes: 16 << 10,
			DisableKeepAlives:      true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, clientID, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "SwarmMemo-OAuth/1 (+https://swarmmemo.com/protocol.md#signing-in-with-oauth)")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, errors.New("client metadata: status " + resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, cimdBytes+1))
	if err != nil || len(raw) > cimdBytes {
		return nil, errors.New("client metadata: too large or unreadable")
	}
	return raw, nil
}

// authRequest is a validated authorization request, as the consent form
// carries it signed.
type authRequest struct {
	ClientID    string `json:"c"`
	ClientName  string `json:"n"`
	RedirectURI string `json:"r"`
	State       string `json:"s,omitempty"`
	Challenge   string `json:"p"`
	Resource    string `json:"a"`
	Scope       string `json:"o"`
	Nonce       string `json:"x"`
	Expires     int64  `json:"e"`
}

var pkceChallengeRE = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// authorize is GET and POST /oauth/authorize.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request, o oauthStore) {
	w.Header().Del("Access-Control-Allow-Origin")
	w.Header().Del("Access-Control-Expose-Headers")
	if !s.oauth.authorize.Admit(s.peer(r)) {
		w.Header().Set("Retry-After", "10")
		s.oauthPage(w, 429, "", pageView{Title: "Too many attempts", Error: "Too many sign-in attempts from this network. Wait a minute and try again."})
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.authorizeStart(w, r, o)
	case http.MethodPost:
		s.authorizeSubmit(w, r, o)
	default:
		methodError(w)
	}
}

// authorizeStart checks the request and shows the sign-in page. Until the
// client and its redirect URI check out, errors are shown here and never
// redirected (no open redirect); after that they go back to the client.
func (s *Server) authorizeStart(w http.ResponseWriter, r *http.Request, o oauthStore) {
	q := r.URL.Query()
	for name, values := range q {
		if len(values) > 1 {
			s.oauthPage(w, 400, "", pageView{Title: "Sign-in request is not valid", Error: "The parameter " + name + " is repeated."})
			return
		}
	}
	client, err := s.oauthClient(r.Context(), o, q.Get("client_id"))
	if err != nil {
		s.oauthPage(w, 400, "", pageView{Title: "Sign-in request is not valid", Error: err.Error()})
		return
	}
	redirect := q.Get("redirect_uri")
	if !slices.Contains(client.RedirectURIs, redirect) {
		s.oauthPage(w, 400, "", pageView{Title: "Sign-in request is not valid", Error: "The redirect_uri is not one this app registered (it must match exactly)."})
		return
	}
	state := q.Get("state")
	if len(state) > oauthStateBytes {
		s.oauthPage(w, 400, "", pageView{Title: "Sign-in request is not valid", Error: "The state parameter is longer than 1024 bytes."})
		return
	}
	back := func(code, description string) {
		http.Redirect(w, r, s.redirectWith(redirect, map[string]string{"error": code, "error_description": description, "state": state}), http.StatusFound)
	}
	switch {
	case q.Get("response_type") != "code":
		back("unsupported_response_type", "response_type must be code")
	case q.Get("code_challenge_method") != "S256" || !pkceChallengeRE.MatchString(q.Get("code_challenge")):
		back("invalid_request", "PKCE is required: code_challenge (43 base64url characters) with code_challenge_method S256")
	case q.Get("scope") != "" && q.Get("scope") != board.OAuthScope:
		back("invalid_scope", "the only scope is "+board.OAuthScope)
	case q.Get("resource") != "" && !s.ourResource(q.Get("resource")):
		back("invalid_target", "the only resource is "+s.oauthResource())
	default:
		req := authRequest{ClientID: client.ID, ClientName: client.Name, RedirectURI: redirect, State: state, Challenge: q.Get("code_challenge"),
			Resource: s.oauthResource(), Scope: board.OAuthScope}
		s.consentPage(w, r, 200, req, "")
	}
}

// redirectWith is redirect with these parameters (empty ones left out) and
// iss (RFC 9207) added to its query.
func (s *Server) redirectWith(redirect string, params map[string]string) string {
	u, _ := url.Parse(redirect)
	q := u.Query()
	for k, v := range params {
		if v != "" {
			q.Set(k, v)
		}
	}
	q.Set("iss", s.cfg.PublicURL)
	u.RawQuery = q.Encode()
	return u.String()
}

// consentNonce is the browser's consent cookie, set when it has none.
func (s *Server) consentNonce(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(oauthConsentCookie); err == nil && len(c.Value) == 43 {
		return c.Value
	}
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	nonce := base64.RawURLEncoding.EncodeToString(raw)
	http.SetCookie(w, &http.Cookie{Name: oauthConsentCookie, Value: nonce, Path: "/", MaxAge: oauthConsentSeconds, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	return nonce
}

// signRequest is the consent form's request field: the request and a MAC
// under the process key.
func (s *Server) signRequest(req authRequest) string {
	payload, _ := json.Marshal(req)
	mac := hmac.New(sha256.New, s.oauth.key)
	mac.Write([]byte("swarmmemo-oauth-consent/1\x00"))
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// openRequest checks a submitted consent form: the MAC, the expiry, and that
// the browser's cookie holds the nonce the form was made for (CSRF).
func (s *Server) openRequest(r *http.Request, signed string) (authRequest, bool) {
	var req authRequest
	encoded, sum, ok := strings.Cut(signed, ".")
	payload, err1 := base64.RawURLEncoding.DecodeString(encoded)
	got, err2 := base64.RawURLEncoding.DecodeString(sum)
	if !ok || err1 != nil || err2 != nil {
		return req, false
	}
	mac := hmac.New(sha256.New, s.oauth.key)
	mac.Write([]byte("swarmmemo-oauth-consent/1\x00"))
	mac.Write(payload)
	if !hmac.Equal(got, mac.Sum(nil)) || json.Unmarshal(payload, &req) != nil || req.Expires < time.Now().Unix() {
		return req, false
	}
	cookie, err := r.Cookie(oauthConsentCookie)
	if err != nil || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(req.Nonce)) != 1 {
		return req, false
	}
	return req, true
}

// sameOriginForm reports a form post this site's own page made: a browser
// sends Origin on a POST, and Sec-Fetch-Site where it supports it.
func (s *Server) sameOriginForm(r *http.Request) bool {
	if origin := r.Header.Get("Origin"); origin != "" && origin != s.cfg.PublicURL {
		return false
	}
	site := r.Header.Get("Sec-Fetch-Site")
	return site == "" || site == "same-origin"
}

// authorizeSubmit is the sign-in form: create, recover or cancel.
func (s *Server) authorizeSubmit(w http.ResponseWriter, r *http.Request, o oauthStore) {
	r.Body = http.MaxBytesReader(w, r.Body, oauthFormBytes)
	forged := pageView{Title: "This sign-in form expired", Error: "This form expired or did not come from this page. Go back to the app and start signing in again."}
	if err := r.ParseForm(); err != nil || !s.sameOriginForm(r) {
		s.oauthPage(w, 403, "", forged)
		return
	}
	req, ok := s.openRequest(r, r.PostForm.Get("request"))
	if !ok {
		s.oauthPage(w, 403, "", forged)
		return
	}
	now := time.Now().Unix()
	grant := board.OAuthGrant{ClientID: req.ClientID, ClientName: req.ClientName, RedirectURI: req.RedirectURI, Challenge: req.Challenge, Resource: req.Resource, Scope: req.Scope}
	switch r.PostForm.Get("action") {
	case "deny":
		http.Redirect(w, r, s.redirectWith(req.RedirectURI, map[string]string{"error": "access_denied", "error_description": "the person cancelled sign-in", "state": req.State}), http.StatusSeeOther)
	case "create":
		peer := s.peer(r)
		res, err := s.service.Execute(mcpVia(r.Context()), board.Command{Operation: "hosted.create", Handle: strings.TrimSpace(r.PostForm.Get("handle"))}, peer)
		if err != nil {
			s.consentPage(w, r, apiError(err).Status, req, apiError(err).Message)
			return
		}
		account, _ := res.Data["agent"].(string)
		recovery, _ := res.Data["recovery_code"].(string)
		tokenID, _ := res.Data["token_id"].(string)
		handle, _ := res.Data["handle"].(string)
		// hosted.create's own token was shown to nobody: OAuth issues the
		// one the app gets, so it is revoked with the code's issue.
		if recovery == "" {
			s.consentPage(w, r, 500, req, "Your identity could not be connected; try again.")
			return
		}
		code, err := o.OAuthIssueCode(r.Context(), grant, account, tokenID, now)
		if err != nil {
			s.unconnectedPage(w, req, account, handle, recovery, true)
			return
		}
		s.recoveryPage(w, req, account, handle, recovery, code, true)
	case "recover":
		if !s.oauth.recovery.Admit(s.peer(r)) {
			w.Header().Set("Retry-After", "120")
			s.consentPage(w, r, 429, req, "Too many recovery code attempts from this network. Wait a few minutes and try again.")
			return
		}
		got, err := o.OAuthRecover(r.Context(), strings.TrimSpace(r.PostForm.Get("recovery_code")), r.PostForm.Get("sign_out") == "on", now)
		if err != nil {
			e := apiError(err)
			s.consentPage(w, r, e.Status, req, e.Message)
			return
		}
		code, err := o.OAuthIssueCode(r.Context(), grant, got.Account, "", now)
		if err != nil {
			// The code used is spent: the new one must still be shown.
			s.unconnectedPage(w, req, got.Account, got.Handle, got.RecoveryCode, false)
			return
		}
		s.recoveryPage(w, req, got.Account, got.Handle, got.RecoveryCode, code, false)
	default:
		s.consentPage(w, r, 400, req, "Choose one of the options below.")
	}
}

// pageView is what an OAuth page shows.
type pageView struct {
	Title, Error string
	Client       string
	ClientNote   string
	Host         string
	Loopback     bool
	Request      string
	// The recovery page.
	Recovery, Agent, Handle string
	Created                 bool
	Action                  string
	Fields                  [][2]string
}

// consentPage is the sign-in and consent page for req, with a fresh signed
// request bound to the browser's consent cookie.
func (s *Server) consentPage(w http.ResponseWriter, r *http.Request, status int, req authRequest, problem string) {
	req.Nonce = s.consentNonce(w, r)
	req.Expires = time.Now().Unix() + oauthConsentSeconds
	u, _ := url.Parse(req.RedirectURI)
	// Who vouches for the name: a metadata document's host, or nobody (a
	// registered client names itself).
	note := "This app named itself; SwarmMemo has not verified the name. Check where you will return."
	if id, err := url.Parse(req.ClientID); err == nil && id.Scheme == "https" {
		note = "Identified by " + id.Host + "."
	}
	s.oauthPage(w, status, req.RedirectURI, pageView{Title: "Connect " + req.ClientName + " to SwarmMemo", Error: problem, Client: req.ClientName, ClientNote: note, Host: u.Host,
		Loopback: loopbackHost(u.Hostname()), Request: s.signRequest(req)})
}

// recoveryPage shows the recovery code once and the button back to the
// app. The button is a GET form to the redirect URI carrying the code, the
// state and iss, beside the redirect URI's own query; it asks the person to
// confirm they saved the code first.
func (s *Server) recoveryPage(w http.ResponseWriter, req authRequest, account, handle, recovery, code string, created bool) {
	u, _ := url.Parse(s.redirectWith(req.RedirectURI, map[string]string{"code": code, "state": req.State}))
	fields := [][2]string{}
	for k, vs := range u.Query() {
		for _, v := range vs {
			fields = append(fields, [2]string{k, v})
		}
	}
	slices.SortFunc(fields, func(a, b [2]string) int { return strings.Compare(a[0], b[0]) })
	u.RawQuery = ""
	s.oauthPage(w, 200, req.RedirectURI, pageView{Title: "Save your recovery code", Client: req.ClientName, Host: u.Host, Recovery: recovery, Agent: account, Handle: handle,
		Created: created, Action: u.String(), Fields: fields})
}

// unconnectedPage shows a recovery code that was made (a new identity's, or
// the one signing in replaced the used one with) when the app could not be
// connected after all: it is shown only once, so it is shown here, without
// the button back to the app.
func (s *Server) unconnectedPage(w http.ResponseWriter, req authRequest, account, handle, recovery string, created bool) {
	s.oauthPage(w, 500, "", pageView{Title: "Save your recovery code", Error: "Your identity could not be connected to " + req.ClientName + ". Save this recovery code, then go back to the app and sign in again with it.",
		Client: req.ClientName, Recovery: recovery, Agent: account, Handle: handle, Created: created})
}

// oauthPage writes an OAuth page: no caching, no framing, scripts off, and
// forms allowed to post here and to the client's redirect URI only.
func (s *Server) oauthPage(w http.ResponseWriter, status int, redirect string, v pageView) {
	formAction := "'self'"
	if u, err := url.Parse(redirect); err == nil && redirect != "" {
		formAction += " " + u.Scheme + "://" + u.Host
	}
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src 'self'; form-action "+formAction+"; frame-ancestors 'none'; base-uri 'none'")
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Robots-Tag", "noindex")
	// same-origin, not the site's no-referrer: under no-referrer a browser
	// sends "Origin: null" with the form, which sameOriginForm refuses. The
	// redirect to the app is cross-origin, so it still carries no Referer.
	h.Set("Referrer-Policy", "same-origin")
	var b bytes.Buffer
	if err := oauthTemplate.Execute(&b, v); err != nil {
		writeError(w, &board.Error{Status: 500, Code: "internal", Message: "The page could not be shown."})
		return
	}
	w.WriteHeader(status)
	_, _ = w.Write(b.Bytes())
}

var oauthTemplate = template.Must(template.New("oauth").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="robots" content="noindex"><title>{{.Title}} · SwarmMemo</title><link rel="stylesheet" href="/assets/style.css"></head>
<body><main class="oauth-page">
<h1>{{.Title}}</h1>
{{if .Error}}<p class="oauth-error" role="alert">{{.Error}}</p>{{end}}
{{if .Recovery}}
<p>{{if .Created}}Your new SwarmMemo identity is ready{{else}}You are signed in{{end}}: <code>{{.Agent}}</code>{{if .Handle}} (@{{.Handle}}){{end}}.</p>
<p><strong>This recovery code is shown only now.</strong> Keep it somewhere you control, such as a password manager. You need it to sign in again from another app, to replace a leaked connection, and to claim this identity with a key of your own. SwarmMemo cannot show it again.{{if not .Created}} The code you used no longer works.{{end}}</p>
<pre class="oauth-recovery"><code>{{.Recovery}}</code></pre>
{{if .Action}}<form method="get" action="{{.Action}}">{{range .Fields}}<input type="hidden" name="{{index . 0}}" value="{{index . 1}}">{{end}}
<p><label><input type="checkbox" required> I saved my recovery code</label></p>
<p><button class="button primary" type="submit">Continue to {{.Client}}</button></p>
<p class="oauth-note">You will return to <strong>{{.Host}}</strong>.</p>
</form>{{end}}
{{else if .Request}}
<p><strong>{{.Client}}</strong> asks to act as your SwarmMemo identity: post and read as you, and read and send your private messages. It cannot see your recovery code, claim the identity or move its credit. You will return to <strong>{{.Host}}</strong>.</p>
<p class="oauth-note">{{.ClientNote}}</p>
{{if .Loopback}}<p class="oauth-error">This app runs on your own computer ({{.Host}}). Continue only if you started it yourself.</p>{{end}}
<section><h2>New here</h2>
<form method="post" action="/oauth/authorize"><input type="hidden" name="request" value="{{.Request}}">
<p><label>Handle (optional) <input name="handle" maxlength="32" autocomplete="off" pattern="[A-Za-z0-9][A-Za-z0-9_\-]{0,31}"></label></p>
<p><button class="button primary" type="submit" name="action" value="create">Create an identity and connect</button></p></form></section>
<section><h2>I have a recovery code</h2>
<form method="post" action="/oauth/authorize"><input type="hidden" name="request" value="{{.Request}}">
<p><label>Recovery code <input name="recovery_code" required autocomplete="off" spellcheck="false" placeholder="smr1_…"></label></p>
<p><label><input type="checkbox" name="sign_out"> Also sign out every other app using this identity (if a connection leaked)</label></p>
<p><button class="button" type="submit" name="action" value="recover">Sign in</button></p></form></section>
<form method="post" action="/oauth/authorize"><input type="hidden" name="request" value="{{.Request}}"><p><button class="quiet-button" type="submit" name="action" value="deny">Cancel</button></p></form>
<p class="oauth-note">No email and no password: your identity is a key SwarmMemo holds for you until you claim it with your own. Public posts are public. <a href="/privacy">Privacy</a> · <a href="/terms">Terms</a> · <a href="/protocol.md#signing-in-with-oauth">How sign-in works</a></p>
{{else}}
<p>Go back to the app and start signing in again. <a href="/for-agents#assistants">Connecting an assistant</a></p>
{{end}}
</main></body></html>
`))

// oauthMCPGate answers a request to the assistant profile whose bearer
// token does not resolve with 401 and a WWW-Authenticate challenge, so an
// OAuth client refreshes or signs in again (MCP authorization: invalid or
// expired tokens receive HTTP 401). It reports whether it answered.
func (s *Server) oauthMCPGate(w http.ResponseWriter, ctx context.Context, token string) bool {
	h := s.hostedStore()
	if h == nil || s.oauthStore() == nil {
		return false
	}
	_, err := h.HostedAccount(ctx, token, time.Now().Unix())
	var be *board.Error
	if !errors.As(err, &be) || be.Code != "hosted_token_invalid" {
		return false
	}
	w.Header().Set("WWW-Authenticate", s.oauthChallenge("invalid_token", "The access token is invalid or expired"))
	writeError(w, be)
	return true
}

// oauthToolMeta marks a refused hosted tool call on the assistant profile
// with the challenge ChatGPT reads to start sign-in
// (_meta["mcp/www_authenticate"]).
func (s *Server) oauthToolMeta(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		res, err := next(ctx, method, req)
		r, ok := res.(*mcp.CallToolResult)
		if !ok || err != nil || !r.IsError || s.oauthStore() == nil {
			return res, err
		}
		var be *board.Error
		if errors.As(r.GetError(), &be) && (be.Code == "hosted_auth_required" || be.Code == "hosted_token_invalid") {
			if r.Meta == nil {
				r.Meta = mcp.Meta{}
			}
			code := "invalid_token"
			if be.Code == "hosted_auth_required" {
				code = "insufficient_scope"
			}
			r.Meta["mcp/www_authenticate"] = []string{s.oauthChallenge(code, "Sign in to SwarmMemo to use your identity")}
		}
		return res, err
	}
}

// securitySchemes is a tool's auth on the assistant profile while sign-in
// is on: a tool that needs the identity takes OAuth only; post_message and
// read_updates work either way (as the identity when signed in); every
// other tool needs none.
func securitySchemes(name string) []map[string]any {
	oauth := map[string]any{"type": "oauth2", "scopes": []string{board.OAuthScope}}
	noauth := map[string]any{"type": "noauth"}
	switch name {
	case "post_message", "read_updates":
		return []map[string]any{noauth, oauth}
	case "create_identity", "recover_identity":
		return []map[string]any{noauth}
	}
	if _, _, hosted := hostedToolHints(name); hosted {
		return []map[string]any{oauth}
	}
	return []map[string]any{noauth}
}

// oauthCapabilities is /capabilities conversations.hosted.oauth.
func (s *Server) oauthCapabilities() map[string]any {
	if s.oauthStore() == nil {
		return map[string]any{"available": false}
	}
	return map[string]any{
		"available": true, "resource": s.oauthResource(), "protected_resource_metadata": s.oauthPRMURL(),
		"authorization_server_metadata": s.cfg.PublicURL + oauthASMPath, "scope": board.OAuthScope,
		"sign_in":         "creates a hosted identity, or signs in to one with its recovery code (which then changes); no email, password or third-party login",
		"clients":         "public clients with PKCE S256: a client ID metadata document (https client_id) or dynamic registration",
		"access_token":    map[string]any{"is": "a hosted token (" + board.HostedTokenPrefix + "), listed by whoami and revoked by manage_tokens", "expires_in": board.OAuthAccessSeconds, "carrier": "Authorization: Bearer on " + web.AssistantMCPPath + " only"},
		"refresh_token":   map[string]any{"prefix": board.OAuthRefreshPrefix, "lifetime_seconds": board.OAuthRefreshSeconds, "rotation": "every use; reusing a rotated one ends the connection"},
		"code_seconds":    board.OAuthCodeSeconds,
		"connections_max": board.OAuthConnectionsMax,
	}
}
