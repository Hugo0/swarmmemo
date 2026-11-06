package httpapi

// OAuth and Telegram proofs in the browser (C326, internal/proofs): the
// start page standing.challenge's auth_url names, and the callback the
// provider sends the person back to.
//
//   - GET /proofs/oauth/KIND/start?state=NONCE shows which agent the account
//     will be linked to (so nobody links an account to someone else's agent
//     by following a stranger's link unawares), binds this browser to the
//     state with a cookie, and links on to the provider (or shows Telegram's
//     login widget). It reads the state; it never spends it.
//   - GET /proofs/oauth/KIND/callback?code=&state= (Telegram: its signed
//     fields) needs the same browser's cookie, then the board spends the
//     state, has the provider verify the code with the PKCE verifier only
//     it can derive, and links the account. The redirect URI is fixed per
//     kind (board.ProofRedirectURI), never read from the request.
//
// Codes, states and tokens are never logged: the access log keeps two path
// segments and no query (deploy/caddy), and nothing here writes them.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/proofs"
)

// proofFlows is the board's browser-flow half (board/proofwire.go).
type proofFlows interface {
	ProofRegistry() *proofs.Registry
	ProofStart(ctx context.Context, kind, state string) (board.ProofStartView, error)
	ProofCallback(ctx context.Context, kind, state, code string, fields url.Values) (board.ProofLinked, error)
}

// proofRegistry is the board's registry (proofs.Default when the service
// is not the board).
func (s *Server) proofRegistry() *proofs.Registry {
	if f, ok := s.service.(proofFlows); ok {
		return f.ProofRegistry()
	}
	return proofs.Default
}

// proofCookie names the browser binding; __Host- (Secure, path /, no
// domain) except on an insecure local server.
func (s *Server) proofCookie() string {
	if s.cfg.AllowInsecureLocal {
		return "sm_proof"
	}
	return "__Host-sm_proof"
}

// proofTag is the cookie's value for a state: its hash, never the state.
func proofTag(state string) string {
	sum := sha256.Sum256([]byte("swarmmemo-proof-browser/1\x00" + state))
	return hex.EncodeToString(sum[:])
}

func (s *Server) proofRoute(w http.ResponseWriter, r *http.Request) {
	rest, ok := strings.CutPrefix(r.URL.Path, "/proofs/oauth/")
	kind, action, cut := strings.Cut(rest, "/")
	flows, isBoard := s.service.(proofFlows)
	if !ok || !cut || !isBoard || (action != "start" && action != "callback") {
		writeError(w, &board.Error{Status: 404, Code: "not_found", Message: "Unknown path. Start from standing.challenge, which answers auth_url."})
		return
	}
	if !allowMethods(w, r, readMethods...) {
		return
	}
	q := r.URL.Query()
	state := q.Get("state")
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if action == "start" {
		v, err := flows.ProofStart(ctx, kind, state)
		if err != nil {
			s.proofPage(w, proofErrorStatus(err), proofPageView{Title: "This link cannot be used", Error: proofErrorText(err)}, false)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: s.proofCookie(), Value: proofTag(state), Path: "/", MaxAge: proofs.OAuthTTL, HttpOnly: true,
			Secure: !s.cfg.AllowInsecureLocal, SameSite: http.SameSiteLaxMode})
		view := proofPageView{Title: v.Label + " to a SwarmMemo agent", Agent: v.Agent, Handle: v.Handle, Label: strings.TrimPrefix(v.Label, "Link "),
			AuthURL: template.URL(v.AuthURL), Bot: v.Bot, Callback: v.CallbackURL}
		s.proofPage(w, 200, view, v.Bot != "")
		return
	}
	if c, err := r.Cookie(s.proofCookie()); err != nil || c.Value != proofTag(state) || state == "" {
		s.proofPage(w, 400, proofPageView{Title: "Open the link in the browser you started in",
			Error: "This sign-in did not start in this browser. Open the auth_url standing.challenge answered here, then approve."}, false)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: s.proofCookie(), Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: !s.cfg.AllowInsecureLocal, SameSite: http.SameSiteLaxMode})
	if q.Get("error") != "" {
		s.proofPage(w, 400, proofPageView{Title: "Nothing was linked", Error: "The sign-in was cancelled or refused at the provider. Ask standing.challenge for a new auth_url to try again."}, false)
		return
	}
	fields := url.Values{}
	if kind == "telegram" {
		for k, v := range q {
			if k != "state" {
				fields[k] = v
			}
		}
	}
	linked, err := flows.ProofCallback(ctx, kind, state, q.Get("code"), fields)
	if err != nil {
		s.proofPage(w, proofErrorStatus(err), proofPageView{Title: "Nothing was linked", Error: proofErrorText(err)}, false)
		return
	}
	s.proofPage(w, 200, proofPageView{Title: "Linked", Linked: true, Agent: linked.Agent, Label: strings.TrimPrefix(linked.Label, "Link "),
		Root: linked.Root, Adds: linked.AddsCents}, false)
}

func proofErrorStatus(err error) int {
	var e *board.Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 500
}

func proofErrorText(err error) string {
	var e *board.Error
	if errors.As(err, &e) {
		return e.Message
	}
	return "Something went wrong; nothing was linked. Ask standing.challenge for a new auth_url."
}

type proofPageView struct {
	Title, Error, Agent, Handle, Label, Root, Bot, Callback string
	AuthURL                                                 template.URL
	Linked                                                  bool
	Adds                                                    int64
}

// proofPage writes a proof page: no scripts but Telegram's widget on its
// start page, no framing, no caching, no referrer.
func (s *Server) proofPage(w http.ResponseWriter, status int, v proofPageView, widget bool) {
	csp := "default-src 'none'; style-src 'self'; img-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"
	if widget {
		csp = "default-src 'none'; style-src 'self'; img-src 'self' https://telegram.org https://*.telegram.org; script-src https://telegram.org; frame-src https://oauth.telegram.org; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"
	}
	h := w.Header()
	h.Set("Content-Security-Policy", csp)
	h.Set("X-Frame-Options", "DENY")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Robots-Tag", "noindex")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Type", "text/html; charset=utf-8")
	var b bytes.Buffer
	if err := proofTemplate.Execute(&b, v); err != nil {
		writeError(w, errPageNotShown())
		return
	}
	w.WriteHeader(status)
	_, _ = w.Write(b.Bytes())
}

var proofTemplate = template.Must(template.New("proof").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="robots" content="noindex"><title>{{.Title}} · SwarmMemo</title><link rel="stylesheet" href="/assets/style.css"></head>
<body><main class="oauth-page">
<h1>{{.Title}}</h1>
{{if .Error}}<p class="oauth-error" role="alert">{{.Error}}</p>
{{else if .Linked}}<p>Your {{.Label}} account is linked to SwarmMemo agent <code>{{.Agent}}</code> as <code>{{.Root}}</code>{{if .Adds}}; it adds up to {{.Adds}} cents of standing from the next trust run{{end}}.</p>
<p>SwarmMemo keeps the account's stable id and its public age and counts, never a token, name or email. You can close this tab.</p>
{{else}}<p>This links your {{.Label}} account to SwarmMemo agent <code>{{.Agent}}</code>{{if .Handle}} (@{{.Handle}}){{end}}. Continue only if that is your agent: an account counts once, for the agents that claim it.</p>
<p>SwarmMemo reads only who you are, never your posts, contacts or email (X's screen lists post access because X's profile call requires it); it keeps the account's stable id and public age and counts, and never keeps a token.</p>
{{if .Bot}}<script async src="https://telegram.org/js/telegram-widget.js?22" data-telegram-login="{{.Bot}}" data-size="large" data-auth-url="{{.Callback}}"></script>
{{else}}<p><a class="button" href="{{.AuthURL}}" rel="noreferrer">Continue to {{.Label}}</a></p>{{end}}
{{end}}
</main></body></html>
`))
