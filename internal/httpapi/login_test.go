package httpapi

// Signing in with X, Google or GitHub (login.go, board/login.go) against a
// fake provider (httptest): PKCE S256 checked end to end by the provider,
// exact redirect URIs, the browser binding and single-use state, one
// provider account = one hosted identity (sign in again, never a second
// one; an account an own-key agent linked is refused), an unconfigured
// provider hidden and refused, and the app path (/oauth/authorize
// provider=x) continuing to the app with an authorization code.

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/proofs"
	"swarmmemo/internal/web"
)

// fakeProvider is X and GitHub in one httptest server. approve plays the
// person approving at the provider: it records the code with the PKCE
// challenge and redirect URI the authorization URL carried, and the token
// endpoint checks the verifier and the redirect URI against them, once.
type fakeProvider struct {
	*httptest.Server
	mu    sync.Mutex
	codes map[string]fakeGrant
}

type fakeGrant struct{ challenge, redirect, user string }

var fakeUsers = map[string]map[string]any{
	"alice": {"id": "4242", "username": "hexplayer", "created_at": "2020-01-01T00:00:00Z", "public_metrics": map[string]any{"followers_count": 12}},
	"bob":   {"id": "5353", "username": "bob_on_x", "created_at": "2021-01-01T00:00:00Z", "public_metrics": map[string]any{"followers_count": 3}},
	"carol": {"id": "6464", "username": "carol", "created_at": "2019-01-01T00:00:00Z", "public_metrics": map[string]any{"followers_count": 1}},
	"octo":  {"id": float64(583231), "login": "Octo-Cat", "created_at": "2011-01-25T18:44:36Z", "public_repos": 8, "followers": 20},
}

func newFakeProvider(t testing.TB) *fakeProvider {
	p := &fakeProvider{codes: map[string]fakeGrant{}}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api.x.com/2/oauth2/token", "/github.com/login/oauth/access_token":
			_ = r.ParseForm()
			if r.URL.Path == "/api.x.com/2/oauth2/token" && r.Header.Get("Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("xid:xsecret")) {
				http.Error(w, `{"error":"invalid_client"}`, 401)
				return
			}
			if r.URL.Path == "/github.com/login/oauth/access_token" && (r.PostForm.Get("client_id") != "ghid" || r.PostForm.Get("client_secret") != "ghsecret") {
				http.Error(w, `{"error":"invalid_client"}`, 401)
				return
			}
			p.mu.Lock()
			g, ok := p.codes[r.PostForm.Get("code")]
			delete(p.codes, r.PostForm.Get("code"))
			p.mu.Unlock()
			if !ok || r.PostForm.Get("redirect_uri") != g.redirect || proofs.PKCEChallenge(r.PostForm.Get("code_verifier")) != g.challenge {
				http.Error(w, `{"error":"invalid_grant"}`, 400)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at-" + g.user, "token_type": "bearer"})
		case "/api.x.com/2/users/me":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": fakeUsers[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer at-")]})
		case "/api.github.com/user":
			_ = json.NewEncoder(w).Encode(fakeUsers[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer at-")])
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(p.Close)
	return p
}

// approve reads an authorization URL as the provider would and answers the
// callback query string for user.
func (p *fakeProvider) approve(t *testing.T, authURL, user string) (callback string) {
	t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || len(q.Get("code_challenge")) != 43 || q.Get("state") == "" {
		t.Fatalf("authorization URL %s", authURL)
	}
	code := "code-" + user + "-" + q.Get("state")[:8]
	p.mu.Lock()
	p.codes[code] = fakeGrant{challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri"), user: user}
	p.mu.Unlock()
	redirect, _ := url.Parse(q.Get("redirect_uri"))
	return redirect.Path + "?" + url.Values{"code": {code}, "state": {q.Get("state")}}.Encode()
}

func loginServer(t testing.TB, p *fakeProvider) (*board.Store, *Server) {
	t.Helper()
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	kek := filepath.Join(t.TempDir(), "hosted-kek")
	if err := os.WriteFile(kek, []byte(base64.RawURLEncoding.EncodeToString(key)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// X and GitHub are set up; Google is not.
	reg := proofs.New(proofs.Config{OAuth: map[string]proofs.OAuthCredentials{"x": {ClientID: "xid", ClientSecret: "xsecret"}, "github": {ClientID: "ghid", ClientSecret: "ghsecret"}},
		Client: p.Client(), TestBaseURL: p.URL})
	features := board.Features{Trust: board.TrustOn}
	store, err := board.Open(filepath.Join(t.TempDir(), "board.sqlite"), board.Config{ServiceID: "swarmmemo.com", HostedKEKFile: kek, Features: features, Proofs: reg, PublicURL: "https://swarmmemo.com"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store, New(store, nil, Config{PublicURL: "https://swarmmemo.com", ServiceID: "swarmmemo.com", Features: features})
}

// browser is one browser's cookie jar over oauthDo.
type browser struct {
	s       http.Handler
	cookies map[string]string
}

func (b *browser) get(path string) *httptest.ResponseRecorder {
	return b.do("GET", path, nil)
}

// post submits a form as this site's own page does (Origin, Sec-Fetch-Site).
func (b *browser) post(path string, form url.Values) *httptest.ResponseRecorder {
	return b.do("POST", path, form)
}

func (b *browser) do(method, path string, form url.Values) *httptest.ResponseRecorder {
	var jar []string
	for k, v := range b.cookies {
		jar = append(jar, k+"="+v)
	}
	h := map[string]string{"Cookie": strings.Join(jar, "; "), "Accept": "text/html"}
	contentType, body := "", ""
	if form != nil {
		h["Origin"], h["Sec-Fetch-Site"] = "https://swarmmemo.com", "same-origin"
		contentType, body = "application/x-www-form-urlencoded", form.Encode()
	}
	w := oauthDo(b.s, method, path, contentType, body, h)
	for _, c := range w.Result().Cookies() {
		if c.MaxAge < 0 {
			delete(b.cookies, c.Name)
		} else {
			b.cookies[c.Name] = c.Value
		}
	}
	return w
}

var startFormRE = regexp.MustCompile(`<form method="post" action="(/login/[a-z]+)"><input type="hidden" name="t" value="([^"]+)">`)

// start opens /login/KIND and presses its Continue button, or opens a path
// that goes straight to the provider (/oauth/authorize?provider=), and
// returns the provider's authorization URL.
func (b *browser) start(t *testing.T, path string) string {
	t.Helper()
	w := b.get(path)
	if m := startFormRE.FindStringSubmatch(w.Body.String()); w.Code == 200 && m != nil {
		w = b.post(m[1], url.Values{"t": {html.UnescapeString(m[2])}})
	}
	if w.Code != http.StatusFound || !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") || w.Header().Get("Referrer-Policy") != "same-origin" {
		t.Fatalf("start %s: %d %s", path, w.Code, w.Body.String())
	}
	return w.Header().Get("Location")
}

var (
	mcpURLRE = regexp.MustCompile(`<code>https://swarmmemo\.com/mcp/t/(smh1_[A-Za-z0-9_-]{43})</code>`)
	agentRE  = regexp.MustCompile(`<code>([0-9a-f]{64})</code>`)
)

func TestLoginWithProviders(t *testing.T) {
	p := newFakeProvider(t)
	_, s := loginServer(t, p)
	b := &browser{s: s, cookies: map[string]string{}}

	// Only the providers set up are shown, on /login and in /capabilities.
	list := b.get("/login").Body.String()
	if !strings.Contains(list, "Continue with X") || !strings.Contains(list, "Continue with GitHub") || strings.Contains(list, "Google") {
		t.Fatalf("/login: %s", list)
	}
	var caps map[string]any
	_ = json.Unmarshal(oauthDo(s, "GET", "/capabilities", "", "", nil).Body.Bytes(), &caps)
	enabled := fmt.Sprint(dig(caps, "conversations", "hosted", "oauth", "providers", "enabled"))
	if !strings.Contains(enabled, "kind:x") || !strings.Contains(enabled, "kind:github") || strings.Contains(enabled, "google") ||
		!strings.Contains(enabled, "redirect_uri:https://swarmmemo.com/login/x/callback") {
		t.Fatalf("capabilities providers: %s", enabled)
	}
	// A provider not set up is refused cleanly; an unknown one is not found.
	if w := b.get("/login/google"); w.Code != 404 || !strings.Contains(w.Body.String(), "not set up on this server") {
		t.Fatalf("/login/google: %d %s", w.Code, w.Body.String())
	}
	if w := b.get("/login/discord"); w.Code != 404 {
		t.Fatalf("/login/discord: %d", w.Code)
	}
	if w := oauthDo(s, "PUT", "/login/x", "", "", nil); w.Code != 405 {
		t.Fatalf("PUT /login/x: %d", w.Code)
	}
	// The start is a form bound to this browser: a link from another site,
	// or a post without this browser's cookie and tag, starts nothing.
	if w := b.get("/login/x"); w.Code != 200 || w.Header().Get("Location") != "" || !startFormRE.MatchString(w.Body.String()) {
		t.Fatalf("GET /login/x: %d %s", w.Code, w.Header().Get("Location"))
	}
	if w := oauthDo(s, "POST", "/login/x", "application/x-www-form-urlencoded", "t=x", map[string]string{"Origin": "https://swarmmemo.com"}); w.Code != 403 {
		t.Fatalf("POST /login/x without the cookie: %d", w.Code)
	}
	other := &browser{s: s, cookies: map[string]string{}}
	other.get("/login")
	if w := other.post("/login/x", url.Values{"t": {s.loginStartTag(b.cookies[oauthConsentCookie])}}); w.Code != 403 {
		t.Fatalf("POST /login/x with another browser's tag: %d", w.Code)
	}
	m := startFormRE.FindStringSubmatch(b.get("/login/x").Body.String())
	cross := &browser{s: s, cookies: b.cookies}
	if w := cross.do("POST", "/login/x", nil); w.Code != 403 {
		t.Fatalf("POST /login/x with no form: %d", w.Code)
	}
	if w := oauthDo(s, "POST", "/login/x", "application/x-www-form-urlencoded", url.Values{"t": {html.UnescapeString(m[2])}}.Encode(),
		map[string]string{"Origin": "https://evil.example", "Cookie": oauthConsentCookie + "=" + b.cookies[oauthConsentCookie]}); w.Code != 403 {
		t.Fatalf("POST /login/x from another origin: %d", w.Code)
	}

	// On its own: the provider gets the exact redirect URI and a PKCE
	// challenge; the page shows a new identity's MCP URL and recovery code.
	authURL := b.start(t, "/login/x")
	if !strings.HasPrefix(authURL, "https://x.com/i/oauth2/authorize?") || !strings.Contains(authURL, "redirect_uri="+url.QueryEscape("https://swarmmemo.com/login/x/callback")) {
		t.Fatalf("auth URL %s", authURL)
	}
	callback := p.approve(t, authURL, "alice")
	// Another browser (no cookie) cannot finish it, and does not spend it.
	if w := oauthDo(s, "GET", callback, "", "", nil); w.Code != 400 {
		t.Fatalf("callback without the cookie: %d", w.Code)
	}
	w := b.get(callback)
	body := w.Body.String()
	token, agent := mcpURLRE.FindStringSubmatch(body), agentRE.FindStringSubmatch(body)
	if w.Code != 200 || token == nil || agent == nil || !recoveryRE.MatchString(body) || !strings.Contains(body, "@hexplayer") || strings.Contains(body, "at-alice") {
		t.Fatalf("callback: %d %s", w.Code, body)
	}
	if again := b.get(callback); again.Code != 400 {
		t.Fatalf("replayed callback: %d", again.Code)
	}
	me := mustTool(t, s, "/mcp", "Bearer "+token[1], "whoami", map[string]any{})
	if dig(me, "agent", "id") != agent[1] || dig(me, "agent", "custody") != "hosted" {
		t.Fatalf("whoami: %v", me)
	}
	links := fmt.Sprint(dig(me, "agent", "links"))
	if !strings.Contains(links, "kind:x") || !strings.Contains(links, "value:4242") {
		t.Fatalf("the X account is not linked: %s", links)
	}

	// The same X account signs in to the same identity: a new token that
	// replaces the sign-in token, and no recovery code.
	w = b.get(p.approve(t, b.start(t, "/login/x"), "alice"))
	body = w.Body.String()
	token2, agent2 := mcpURLRE.FindStringSubmatch(body), agentRE.FindStringSubmatch(body)
	if w.Code != 200 || token2 == nil || agent2 == nil || agent2[1] != agent[1] || recoveryRE.MatchString(body) || !strings.Contains(body, "Welcome back") {
		t.Fatalf("second sign-in: %d %s", w.Code, body)
	}
	mustTool(t, s, "/mcp", "Bearer "+token2[1], "whoami", map[string]any{})
	if _, failure := callTool(t, s, "/mcp", "Bearer "+token2[1], "whoami", map[string]any{}); failure != "" {
		t.Fatalf("new token: %s", failure)
	}

	// A refused code creates nothing.
	bad := b.start(t, "/login/x")
	u, _ := url.Parse(bad)
	if w := b.get("/login/x/callback?" + url.Values{"code": {"forged"}, "state": {u.Query().Get("state")}}.Encode()); w.Code != 400 || !strings.Contains(w.Body.String(), "did not confirm") {
		t.Fatalf("forged code: %d %s", w.Code, w.Body.String())
	}

	// GitHub: the value is the login, the root GitHub's numeric id, as the
	// gist proof has them.
	w = b.get(p.approve(t, b.start(t, "/login/github"), "octo"))
	gh := agentRE.FindStringSubmatch(w.Body.String())
	if w.Code != 200 || gh == nil || gh[1] == agent[1] {
		t.Fatalf("github: %d %s", w.Code, w.Body.String())
	}
	ghToken := mcpURLRE.FindStringSubmatch(w.Body.String())
	links = fmt.Sprint(dig(mustTool(t, s, "/mcp", "Bearer "+ghToken[1], "whoami", map[string]any{}), "agent", "links"))
	if !strings.Contains(links, "value:octo-cat") {
		t.Fatalf("github link: %s", links)
	}

	// An X account an agent with its own key linked (an OAuth proof) never
	// makes a hosted identity.
	seed := make([]byte, 32)
	seed[0] = 66
	key := ed25519.NewKeyFromSeed(seed)
	if w := tlsCommand(s, signedLinkCommand(key, "agent.register", "", 0)); w.Code != 200 {
		t.Fatalf("register: %d %s", w.Code, w.Body.String())
	}
	cw := tlsCommand(s, signedStanding(key, "standing.challenge", `{"schema":1,"kind":"x"}`, 1))
	var ch struct {
		Data struct {
			AuthURL string `json:"auth_url"`
		} `json:"data"`
	}
	if json.Unmarshal(cw.Body.Bytes(), &ch) != nil || ch.Data.AuthURL == "" {
		t.Fatalf("challenge: %d %s", cw.Code, cw.Body.String())
	}
	pb := &browser{s: s, cookies: map[string]string{}}
	startPage := pb.get(strings.TrimPrefix(ch.Data.AuthURL, "https://swarmmemo.com")).Body.String()
	m = regexp.MustCompile(`href="(https://x\.com/i/oauth2/authorize\?[^"]+)"`).FindStringSubmatch(startPage)
	if m == nil {
		t.Fatalf("proof start page: %s", startPage)
	}
	if w := pb.get(p.approve(t, html.UnescapeString(m[1]), "carol")); w.Code != 200 {
		t.Fatalf("proof callback: %d %s", w.Code, w.Body.String())
	}
	if w := b.get(p.approve(t, b.start(t, "/login/x"), "carol")); w.Code != 409 || !strings.Contains(w.Body.String(), "holds its own key") {
		t.Fatalf("own-key account: %d %s", w.Code, w.Body.String())
	}
}

// From an app: /oauth/authorize?provider=x goes straight to X and back to a
// page that names the identity; Continue sends the app its code, which
// exchanges for a token acting as that identity. A new account gets the
// recovery page first. A provider not set up goes back to the app as an
// error, and the sign-in page offers only the providers set up.
func TestLoginFromAnApp(t *testing.T) {
	p := newFakeProvider(t)
	_, s := loginServer(t, p)
	clientID := registerTestClient(t, s, testRedirect)

	page := oauthDo(s, "GET", authorizeQuery(clientID, testRedirect, "st", nil), "", "", map[string]string{"Accept": "text/html"}).Body.String()
	if !strings.Contains(page, "Continue with X") || !strings.Contains(page, "Continue with GitHub") || strings.Contains(page, "Continue with Google") || !strings.Contains(page, `value="provider:x"`) || strings.Contains(page, "/login/x?") {
		t.Fatalf("sign-in page: %s", page)
	}
	// "Continue with X" there is this browser's own consent form: bound to
	// its cookie, and the form may post on to the provider.
	b0 := &browser{s: s, cookies: map[string]string{}}
	req0 := requestFieldRE.FindStringSubmatch(b0.get(authorizeQuery(clientID, testRedirect, "zero", nil)).Body.String())
	if req0 == nil {
		t.Fatal("no request field")
	}
	if forged := submit(s, html.UnescapeString(req0[1]), "", map[string]string{"action": "provider:x"}, nil); forged.Code != 403 {
		t.Fatalf("provider:x without the cookie: %d", forged.Code)
	}
	if w := submit(s, html.UnescapeString(req0[1]), b0.cookies[oauthConsentCookie], map[string]string{"action": "provider:discord"}, nil); w.Code != 404 || w.Header().Get("Location") != "" {
		t.Fatalf("provider:discord: %d %s", w.Code, w.Header().Get("Location"))
	}
	if w := submit(s, html.UnescapeString(req0[1]), b0.cookies[oauthConsentCookie], map[string]string{"action": "provider:x"}, nil); w.Code != 302 ||
		!strings.HasPrefix(w.Header().Get("Location"), "https://x.com/i/oauth2/authorize?") || !strings.Contains(w.Header().Get("Content-Security-Policy"), "https://x.com") {
		t.Fatalf("provider:x: %d %s", w.Code, w.Header().Get("Location"))
	}
	if csp := oauthDo(s, "GET", authorizeQuery(clientID, testRedirect, "st", nil), "", "", map[string]string{"Accept": "text/html"}).Header().Get("Content-Security-Policy"); !strings.Contains(csp, "https://x.com https://github.com") {
		t.Fatalf("sign-in page form-action: %s", csp)
	}
	off := oauthDo(s, "GET", authorizeQuery(clientID, testRedirect, "st", map[string]string{"provider": "google"}), "", "", nil)
	if loc, _ := url.Parse(off.Header().Get("Location")); off.Code != 302 || loc.Query().Get("error") != "invalid_request" || loc.Query().Get("state") != "st" {
		t.Fatalf("provider=google: %d %s", off.Code, off.Header().Get("Location"))
	}

	// A new X account: the recovery page, then Continue.
	b := &browser{s: s, cookies: map[string]string{}}
	w := b.get(p.approve(t, b.start(t, authorizeQuery(clientID, testRedirect, "first", map[string]string{"provider": "x"})), "bob"))
	back, recovery := continued(t, s, b.cookies[oauthConsentCookie], w)
	if back.Query().Get("state") != "first" || recovery == "" || !strings.Contains(w.Body.String(), "linked to your X account") {
		t.Fatalf("new identity: %s", w.Body.String())
	}
	first := exchange(t, s, clientID, back.Query().Get("code"))
	me := mustTool(t, s, web.AssistantMCPPath, "Bearer "+first["access_token"].(string), "whoami", map[string]any{})

	// The same account again, from another browser: named, one click.
	b2 := &browser{s: s, cookies: map[string]string{}}
	w = b2.get(p.approve(t, b2.start(t, authorizeQuery(clientID, testRedirect, "second", map[string]string{"provider": "x"})), "bob"))
	body := w.Body.String()
	request := requestFieldRE.FindStringSubmatch(body)
	if w.Code != 200 || request == nil || !strings.Contains(body, "signed in with X") || !strings.Contains(body, fmt.Sprint(dig(me, "agent", "id"))) || recoveryRE.MatchString(body) {
		t.Fatalf("signed-in page: %d %s", w.Code, body)
	}
	// The form is bound to this browser's consent cookie.
	if forged := submit(s, html.UnescapeString(request[1]), "", map[string]string{"action": "continue"}, nil); forged.Code != 403 {
		t.Fatalf("continue without the cookie: %d", forged.Code)
	}
	cont := submit(s, html.UnescapeString(request[1]), b2.cookies[oauthConsentCookie], map[string]string{"action": "continue"}, nil)
	loc, _ := url.Parse(cont.Header().Get("Location"))
	if cont.Code != 303 || loc.Query().Get("state") != "second" || loc.Query().Get("iss") != "https://swarmmemo.com" {
		t.Fatalf("continue: %d %s", cont.Code, cont.Header().Get("Location"))
	}
	second := exchange(t, s, clientID, loc.Query().Get("code"))
	me2 := mustTool(t, s, web.AssistantMCPPath, "Bearer "+second["access_token"].(string), "whoami", map[string]any{})
	if dig(me2, "agent", "id") != dig(me, "agent", "id") {
		t.Fatalf("the same X account signed in to another identity: %v %v", dig(me2, "agent", "id"), dig(me, "agent", "id"))
	}

	// Cancelling at the provider goes back to the app.
	b3 := &browser{s: s, cookies: map[string]string{}}
	u, _ := url.Parse(b3.start(t, authorizeQuery(clientID, testRedirect, "third", map[string]string{"provider": "x"})))
	w = b3.get("/login/x/callback?" + url.Values{"error": {"access_denied"}, "state": {u.Query().Get("state")}}.Encode())
	if loc, _ := url.Parse(w.Header().Get("Location")); w.Code != 303 || loc.Query().Get("error") != "access_denied" || loc.Query().Get("state") != "third" {
		t.Fatalf("cancel: %d %s", w.Code, w.Header().Get("Location"))
	}
}

// FuzzLoginRoute: the sign-in paths take any path and query from anyone, and
// must never panic, redirect anywhere but the provider's authorization URL
// or an app's registered redirect URI, or answer a 5xx. Run with -fuzz for
// at most 60 s.
func FuzzLoginRoute(f *testing.F) {
	for _, seed := range []string{"/login", "/login/x", "/login/x/callback?code=a&state=b", "/login/google", "/login/x?t=e30.AAAA",
		"/login//callback", "/login/x/callback?error=access_denied&state=", "/login/../oauth/authorize", "/login/github/callback/extra", "/login/x?t=%zz"} {
		f.Add(seed)
	}
	p := newFakeProvider(f)
	_, s := loginServer(f, p)
	s.oauth.authorize = NewLimiterRate(1<<30, 1<<30)
	f.Fuzz(func(t *testing.T, target string) {
		if !strings.HasPrefix(target, "/login") || strings.ContainsAny(target, " \r\n#") {
			return
		}
		r, err := http.NewRequest("GET", "https://swarmmemo.com"+target, nil)
		if err != nil {
			return
		}
		r.RemoteAddr = "198.51.100.30:4000"
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code >= 500 && w.Code != 503 {
			t.Fatalf("%s: %d", target, w.Code)
		}
		if loc := w.Header().Get("Location"); loc != "" && !strings.HasPrefix(loc, "https://x.com/i/oauth2/authorize?") && !strings.HasPrefix(loc, "https://github.com/login/oauth/authorize?") {
			t.Fatalf("%s redirects to %s", target, loc)
		}
	})
}
