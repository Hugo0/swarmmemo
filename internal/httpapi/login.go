package httpapi

// Signing in with X, Google or GitHub in the browser (C666, board/login.go):
// a person creates or recovers a hosted identity with an account they have.
//
//	GET /login                  the providers this server has set up
//	GET /login/KIND             one provider's Continue button
//	POST /login/KIND            to the provider (KIND: x, google, github)
//	GET /login/KIND/callback    back from the provider: create or sign in
//
// An app's sign-in starts at /oauth/authorize instead: provider=KIND there,
// or a "Continue with" button on its sign-in page (a post bound to the
// consent cookie, as every consent form is), and the app's request stays on
// the server, never in a URL another site could hand a browser.
//
// The start on its own is a form post bound to this browser's Strict
// consent cookie (loginStartTag), never a bare GET: a sign-in replaces the
// identity's sign-in token (board.LoginToken), so another site must not be
// able to set one off in a browser that is signed in at the provider.
//
// The start makes a random state, keeps it here (single use, for
// proofs.OAuthTTL) with the kind and the app's request, binds this
// browser to it with a cookie holding only its hash, and redirects to the
// provider with PKCE S256 (the verifier derived from the state under this
// process's key, never sent anywhere but the provider's token endpoint) and
// the kind's one redirect URI. The callback needs the same browser's cookie
// before it spends the state; then the board has the provider confirm the
// code and finds or creates the account's hosted identity.
//
// Where it ends:
//   - from an app (/oauth/authorize, provider=KIND or a "Continue with"
//     link): a new identity shows its recovery code once, as the sign-in
//     page does, and an existing one is named; either way the person
//     presses Continue, and the app gets an authorization code for it
//     (oauth.go's continue, the same mechanism as every OAuth sign-in);
//   - on its own: the page shows the identity's token once, as the MCP URL
//     an assistant connects with (create_identity's mcp_url), with the
//     recovery code for a new identity. Signing in again replaces that
//     token (board.LoginToken).
//
// A provider with no client credentials is not shown anywhere, and its
// paths answer 404 sign_in_provider_off. Codes, states and tokens are never
// logged.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/proofs"
)

// loginPath prefixes the sign-in paths.
const loginPath = "/login/"

// loginPendingMax bounds the sign-ins under way at once.
const loginPendingMax = 4096

// loginFlows is the board's provider sign-in (board/login.go).
type loginFlows interface {
	LoginProviders() []board.LoginProvider
	LoginRedirectURI(kind string) string
	LoginAuthURL(kind, state, verifier string) (string, error)
	LoginComplete(ctx context.Context, kind, code, verifier, source string) (board.LoginResult, error)
	LoginToken(ctx context.Context, account, kind string) (token, tokenID string, err error)
}

// pendingLogin is a sign-in under way: its kind, the app's request (nil
// for a sign-in on its own) and when it lapses.
type pendingLogin struct {
	kind    string
	req     *authRequest
	expires int64
}

// loginFlowsOf is the board's sign-in while hosted identities are on.
func (s *Server) loginFlowsOf() (loginFlows, bool) {
	if s.oauthStore() == nil {
		return nil, false
	}
	f, ok := s.service.(loginFlows)
	return f, ok
}

// loginProviders are the providers set up on this server, in order.
func (s *Server) loginProviders() []board.LoginProvider {
	f, ok := s.loginFlowsOf()
	if !ok {
		return nil
	}
	var out []board.LoginProvider
	for _, p := range f.LoginProviders() {
		if p.Enabled {
			out = append(out, p)
		}
	}
	return out
}

// loginEnabled reports a provider set up on this server.
func (s *Server) loginEnabled(kind string) bool {
	for _, p := range s.loginProviders() {
		if p.Kind == kind {
			return true
		}
	}
	return false
}

// loginCapabilities is /capabilities conversations.hosted.oauth.providers:
// the providers set up here (none listed until the operator configures
// one), and how to use them.
func (s *Server) loginCapabilities() map[string]any {
	f, _ := s.loginFlowsOf()
	providers := []map[string]any{}
	for _, p := range s.loginProviders() {
		providers = append(providers, map[string]any{"kind": p.Kind, "label": p.Label, "start": s.cfg.PublicURL + loginPath + p.Kind, "redirect_uri": f.LoginRedirectURI(p.Kind)})
	}
	return map[string]any{
		"enabled":   providers,
		"kinds":     proofs.LoginKinds,
		"authorize": "add provider=KIND to /oauth/authorize to send the person straight to that provider; a provider not listed in enabled is refused (invalid_request)",
		"identity":  "one provider account is one hosted identity: the first sign-in creates it and links the account (an identity link), later ones sign in to it; an account an agent with its own key already linked is refused (sign_in_key_held)",
		"keeps":     "the account's stable id, as an identity link, and its public age and counts; the GitHub login; never an email, a provider token or a password",
	}
}

// loginOrigins are the enabled providers' authorization origins, for a
// sign-in form's CSP form-action (oauthHeaders).
func (s *Server) loginOrigins() []string {
	var out []string
	for _, p := range s.loginProviders() {
		if p.Origin != "" {
			out = append(out, p.Origin)
		}
	}
	return out
}

// loginStartTag is a start form's field: a MAC of this browser's consent
// cookie (consentNonce), so only this site's own page, in the same browser,
// starts a sign-in.
func (s *Server) loginStartTag(nonce string) string {
	mac := hmac.New(sha256.New, s.oauth.key)
	mac.Write([]byte("swarmmemo-login-start/1\x00" + nonce))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// loginStartOK checks a start form: posted by this site's page, with the
// tag for this browser's consent cookie.
func (s *Server) loginStartOK(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, oauthFormBytes)
	if err := r.ParseForm(); err != nil || !s.sameOriginForm(r) {
		return false
	}
	c, err := r.Cookie(oauthConsentCookie)
	return err == nil && c.Value != "" && hmac.Equal([]byte(r.PostForm.Get("t")), []byte(s.loginStartTag(c.Value)))
}

// loginCookie names the browser binding; __Host- (Secure, path /, no
// domain) except on an insecure local server.
func (s *Server) loginCookie() string {
	if s.cfg.AllowInsecureLocal {
		return "sm_login"
	}
	return "__Host-swarmmemo-login"
}

// loginTag is the cookie's value for a state: its hash, never the state.
func loginTag(state string) string {
	sum := sha256.Sum256([]byte("swarmmemo-login-browser/1\x00" + state))
	return hex.EncodeToString(sum[:])
}

// loginVerifier is a sign-in's PKCE verifier: derived from the state and
// the kind under this process's key, so none is stored or sent but to the
// provider's token endpoint.
func (s *Server) loginVerifier(kind, state string) string {
	return proofs.PKCEVerifier(s.oauth.key, "login\x00"+kind+"\x00"+state)
}

// loginPut keeps a sign-in under way; false when too many are.
func (s *Server) loginPut(state string, p pendingLogin, now int64) bool {
	s.oauth.loginMu.Lock()
	defer s.oauth.loginMu.Unlock()
	if len(s.oauth.logins) >= loginPendingMax {
		for k, v := range s.oauth.logins {
			if v.expires <= now {
				delete(s.oauth.logins, k)
			}
		}
		if len(s.oauth.logins) >= loginPendingMax {
			return false
		}
	}
	s.oauth.logins[loginTag(state)] = p
	return true
}

// loginTake spends a sign-in under way: once, unexpired.
func (s *Server) loginTake(state string, now int64) (pendingLogin, bool) {
	s.oauth.loginMu.Lock()
	defer s.oauth.loginMu.Unlock()
	key := loginTag(state)
	p, ok := s.oauth.logins[key]
	delete(s.oauth.logins, key)
	return p, ok && p.expires > now
}

// loginRoute serves /login and /login/...; false for any other path.
func (s *Server) loginRoute(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != "/login" && !strings.HasPrefix(r.URL.Path, loginPath) {
		return false
	}
	w.Header().Del("Access-Control-Allow-Origin")
	w.Header().Del("Access-Control-Expose-Headers")
	flows, ok := s.loginFlowsOf()
	if !ok {
		s.loginPage(w, 404, loginView{Title: "Sign-in is off", Error: "Sign-in is off on this server: hosted identities are off."})
		return true
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/login"), "/")
	kind, action, _ := strings.Cut(rest, "/")
	start := rest != "" && action == "" && !strings.Contains(rest, "/")
	if !readMethod(r) && !(start && r.Method == http.MethodPost) {
		methodError(w)
		return true
	}
	if !s.oauth.authorize.Admit(s.peer(r)) {
		w.Header().Set("Retry-After", "10")
		s.loginPage(w, 429, loginView{Title: "Too many attempts", Error: "Too many sign-in attempts from this network. Wait a minute and try again."})
		return true
	}
	switch {
	case rest == "":
		s.loginPage(w, 200, loginView{Title: "Sign in to SwarmMemo", Providers: s.loginProviders(), List: true, Tag: s.loginStartTag(s.consentNonce(w, r))})
	case start && r.Method == http.MethodPost:
		if !s.loginStartOK(w, r) {
			s.loginPage(w, 403, loginView{Title: "This sign-in form expired", Error: "This form expired or did not come from this site's sign-in page. Start again from /login."})
			return true
		}
		s.loginBegin(w, r, flows, kind, nil)
	case start:
		// One provider's button: a link from elsewhere starts nothing by itself.
		if _, err := flows.LoginAuthURL(kind, "", ""); err != nil {
			s.loginPage(w, proofErrorStatus(err), loginView{Title: "This sign-in is not available", Error: loginErrorText(err)})
			return true
		}
		s.loginPage(w, 200, loginView{Title: "Sign in with " + board.LoginLabel(kind), Providers: []board.LoginProvider{{Kind: kind, Label: board.LoginLabel(kind)}}, List: true, Tag: s.loginStartTag(s.consentNonce(w, r))})
	case action == "callback":
		s.loginCallback(w, r, flows, kind)
	default:
		s.loginPage(w, 404, loginView{Title: "Unknown sign-in path", Error: "Start from /login, or from an app's sign-in page."})
	}
	return true
}

// loginBegin sends the browser to the provider; req is the app's request
// (from /oauth/authorize), nil for a sign-in on its own. Callers have bound
// the start to this browser or to an authorization request.
func (s *Server) loginBegin(w http.ResponseWriter, r *http.Request, flows loginFlows, kind string, req *authRequest) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		writeError(w, errPageNotShown())
		return
	}
	state := base64.RawURLEncoding.EncodeToString(raw)
	authURL, err := flows.LoginAuthURL(kind, state, s.loginVerifier(kind, state))
	if err != nil {
		s.loginPage(w, proofErrorStatus(err), loginView{Title: "This sign-in is not available", Error: loginErrorText(err)})
		return
	}
	now := time.Now().Unix()
	if !s.loginPut(state, pendingLogin{kind: kind, req: req, expires: now + proofs.OAuthTTL}, now) {
		w.Header().Set("Retry-After", "60")
		s.loginPage(w, 503, loginView{Title: "Too many sign-ins", Error: "Too many sign-ins are under way right now. Try again in a minute."})
		return
	}
	http.SetCookie(w, &http.Cookie{Name: s.loginCookie(), Value: loginTag(state), Path: "/", MaxAge: proofs.OAuthTTL, HttpOnly: true,
		Secure: !s.cfg.AllowInsecureLocal, SameSite: http.SameSiteLaxMode})
	s.loginHeaders(w)
	http.Redirect(w, r, authURL, http.StatusFound)
}

// loginCallback finishes a sign-in in the browser that started it.
func (s *Server) loginCallback(w http.ResponseWriter, r *http.Request, flows loginFlows, kind string) {
	q := r.URL.Query()
	state := q.Get("state")
	c, err := r.Cookie(s.loginCookie())
	if err != nil || state == "" || subtle.ConstantTimeCompare([]byte(c.Value), []byte(loginTag(state))) != 1 {
		// Another browser cannot spend this browser's sign-in.
		s.loginPage(w, 400, loginView{Title: "Open the sign-in in the browser you started in",
			Error: "This sign-in did not start in this browser, or it already finished. Start again from /login or from the app."})
		return
	}
	http.SetCookie(w, &http.Cookie{Name: s.loginCookie(), Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: !s.cfg.AllowInsecureLocal, SameSite: http.SameSiteLaxMode})
	now := time.Now().Unix()
	p, ok := s.loginTake(state, now)
	if !ok || p.kind != kind {
		s.loginPage(w, 400, loginView{Title: "This sign-in expired", Error: "This sign-in expired or already finished. Start again from /login or from the app."})
		return
	}
	if q.Get("error") != "" {
		if req := p.req; req != nil {
			s.oauthHeaders(w, req.RedirectURI)
			http.Redirect(w, r, s.redirectWith(req.RedirectURI, map[string]string{"error": "access_denied", "error_description": "the person cancelled sign-in at the provider", "state": req.State}), http.StatusSeeOther)
			return
		}
		s.loginPage(w, 400, loginView{Title: "Nothing was created", Error: "The sign-in was cancelled or refused at " + board.LoginLabel(kind) + ". Start again from /login."})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	res, err := flows.LoginComplete(mcpVia(ctx), kind, q.Get("code"), s.loginVerifier(kind, state), s.peer(r))
	if err != nil {
		s.loginPage(w, proofErrorStatus(err), loginView{Title: "You are not signed in", Error: loginErrorText(err)})
		return
	}
	label := board.LoginLabel(kind)
	if p.req != nil {
		if res.Created && res.TokenID != "" {
			// hosted.create's own token was shown to nobody: the app gets its own.
			if o := s.oauthStore(); o != nil {
				if err := o.OAuthDiscardToken(ctx, res.Account, res.TokenID, now); err != nil {
					slog.Warn("login: discarding the creation token failed", "err", err)
				}
			}
		}
		s.signedInPage(w, r, *p.req, res, label)
		return
	}
	token := res.Token
	if !res.Created {
		if token, _, err = flows.LoginToken(ctx, res.Account, kind); err != nil {
			s.loginPage(w, proofErrorStatus(err), loginView{Title: "You are signed in, but no token was made", Error: loginErrorText(err)})
			return
		}
	}
	s.loginPage(w, 200, loginView{Title: "You are signed in", Done: true, Provider: label, Agent: res.Account, Handle: res.Handle, Created: res.Created,
		Recovery: res.RecoveryCode, MCPURL: s.cfg.PublicURL + "/mcp/t/" + token})
}

// signedInPage continues an app's sign-in after the provider: a new
// identity's recovery code is shown once (as on the sign-in page), an
// existing identity is named; the Continue form, bound to this browser's
// consent cookie again, sends the app its authorization code.
func (s *Server) signedInPage(w http.ResponseWriter, r *http.Request, req authRequest, res board.LoginResult, label string) {
	req.Nonce = s.consentNonce(w, r)
	req.Expires = time.Now().Unix() + oauthConsentSeconds
	req.Account = res.Account
	v := pageView{Title: "Signed in with " + label, Client: req.ClientName, Agent: res.Account, Handle: res.Handle, Provider: label, Request: s.signRequest(req)}
	if u, err := url.Parse(req.RedirectURI); err == nil {
		v.Host = u.Host
	}
	if res.Created {
		v.Title, v.Recovery, v.Created = "Save your recovery code", res.RecoveryCode, true
	} else {
		v.SignedIn = true
	}
	s.oauthPage(w, 200, req.RedirectURI, v)
}

// loginErrorText is a refusal's message for the page.
func loginErrorText(err error) string {
	var e *board.Error
	if errors.As(err, &e) {
		return e.Message
	}
	return "Something went wrong; nothing was created. Start again from /login or from the app."
}

// loginHeaders are a sign-in page's or redirect's headers: the OAuth pages'
// (no caching, no framing, no scripts, forms only to here and to the
// providers, no referrer to another origin).
func (s *Server) loginHeaders(w http.ResponseWriter) { s.oauthHeaders(w, "") }

// loginView is what a sign-in page shows.
type loginView struct {
	Title, Error string
	// List: the providers page.
	List      bool
	Providers []board.LoginProvider
	// Tag binds the start forms to this browser (loginStartTag).
	Tag string
	// Done: signed in on its own.
	Done                                      bool
	Provider, Agent, Handle, Recovery, MCPURL string
	Created                                   bool
}

func (s *Server) loginPage(w http.ResponseWriter, status int, v loginView) {
	s.loginHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	var b bytes.Buffer
	if err := loginTemplate.Execute(&b, v); err != nil {
		writeError(w, errPageNotShown())
		return
	}
	w.WriteHeader(status)
	_, _ = w.Write(b.Bytes())
}

var loginTemplate = template.Must(template.New("login").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="robots" content="noindex"><title>{{.Title}} · SwarmMemo</title><link rel="stylesheet" href="/assets/style.css"></head>
<body><main class="oauth-page">
<h1>{{.Title}}</h1>
{{if .Error}}<p class="oauth-error" role="alert">{{.Error}}</p>
{{else if .List}}{{if .Providers}}<p>Sign in with an account you already have. The first time makes a hosted identity linked to that account; after that, the same account signs in to the same identity.</p>
{{range .Providers}}<form method="post" action="/login/{{.Kind}}"><input type="hidden" name="t" value="{{$.Tag}}"><p><button class="button primary" type="submit">Continue with {{.Label}}</button></p></form>
{{end}}
{{else}}<p>No sign-in provider is set up on this server. An assistant can still make a hosted identity with create_identity.</p>{{end}}
<p class="oauth-note">SwarmMemo asks only who you are (no posts, contacts or email) and keeps the account's stable id as an identity link, never a token or a password. <a href="/privacy">Privacy</a> · <a href="/terms">Terms</a></p>
{{else if .Done}}<p>{{if .Created}}Your new SwarmMemo identity is ready{{else}}Welcome back{{end}}: <code>{{.Agent}}</code>{{if .Handle}} (@{{.Handle}}){{end}}, linked to your {{.Provider}} account.</p>
<p><strong>Connect your assistant with this MCP URL.</strong> It is shown only now and acts as your identity: keep it out of posts. Signing in with {{.Provider}} again makes a new one and ends this one.</p>
<pre class="oauth-recovery"><code>{{.MCPURL}}</code></pre>
{{if .Recovery}}<p><strong>This recovery code is shown only now.</strong> Keep it somewhere you control, apart from the URL. It replaces a leaked URL and lets you claim this identity with a key of your own. SwarmMemo cannot show it again.</p>
<pre class="oauth-recovery"><code>{{.Recovery}}</code></pre>{{end}}
<p class="oauth-note">whoami lists this identity's tokens and manage_tokens revokes them. <a href="/protocol.md#signing-in-with-x-google-or-github">How sign-in works</a></p>
{{end}}
</main></body></html>
`))
