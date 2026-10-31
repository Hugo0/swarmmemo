package proofs

// OAuth 2.0 / OpenID Connect identity providers (Google, X, Discord, Reddit,
// Hugging Face) and Telegram's login widget.
//
// The flow: standing.challenge KIND issues a single-use state bound to the
// key that asked (the board's standing_challenges row) and answers the URL
// of SwarmMemo's own start page, which shows the person which agent the
// account will be linked to, binds the browser with a cookie and goes on to
// AuthURL: the authorization code flow, identity scopes only, PKCE (S256)
// with a verifier only the board can derive (an HMAC of the state under a
// server key), and the one redirect URI the operator registered with the
// provider (PUBLIC_URL/proofs/oauth/KIND/callback, never taken from a
// request). The callback consumes the state, and Verify exchanges the code
// with the verifier, reads the account's stable id and its value inputs, and
// forgets the access token: it is never stored, logged or returned. There is
// no refresh token (no offline scope), so a recheck re-assesses the stored
// inputs (age grows) without calling the provider.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"swarmmemo/internal/trust"
)

// OAuthCredentials are one provider's client registration.
type OAuthCredentials struct {
	ClientID     string
	ClientSecret string
}

// OAuthTTL is how long an OAuth or widget challenge (the state) is usable.
const OAuthTTL = 600

// accountIDRE is a provider's stable account id: letters, digits, _ and -.
var accountIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// OAuth is one OAuth 2.0 / OIDC provider.
type OAuth struct {
	spec      Spec
	authURL   string
	tokenURL  string
	scope     string
	basicAuth bool // the token endpoint authenticates the client with HTTP Basic
	extra     url.Values
	creds     OAuthCredentials
	f         fetcher
	// profile reads the account's stable id, its handle (when the
	// provider has one) and value inputs with an access token.
	profile func(ctx context.Context, f fetcher, token string) (account, error)
	assess  func(in Inputs, now time.Time) int64
	// valueIsHandle makes the proof's value the account's handle, lowercase
	// (GitHub's login, as the gist proof stores it), its root still the id.
	valueIsHandle bool
}

// account is what a provider's profile read answers.
type account struct {
	ID, Handle string
	Inputs     Inputs
}

func (o *OAuth) Spec() Spec    { return o.spec }
func (o *OAuth) Enabled() bool { return o.creds.ClientID != "" && o.creds.ClientSecret != "" }
func (o *OAuth) Assess(p Proof, now time.Time) int64 {
	return o.assess(p.Inputs, now)
}

// PKCEChallenge is the S256 code challenge of a verifier (RFC 7636).
func PKCEChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// PKCEVerifier derives the verifier of a state under the server's key: 43
// characters of base64url, unguessable without the key, so an intercepted
// code and state are useless to anyone but the board.
func PKCEVerifier(key []byte, state string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("swarmmemo-proof-pkce/1\x00" + state))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// AuthURL is the provider's authorization URL for a state, a PKCE verifier
// and the registered redirect URI.
func (o *OAuth) AuthURL(state, verifier, redirect string) string {
	q := url.Values{"response_type": {"code"}, "client_id": {o.creds.ClientID}, "redirect_uri": {redirect}, "scope": {o.scope},
		"state": {state}, "code_challenge": {PKCEChallenge(verifier)}, "code_challenge_method": {"S256"}}
	if o.scope == "" {
		q.Del("scope")
	}
	for k, v := range o.extra {
		q[k] = v
	}
	return o.authURL + "?" + q.Encode()
}

// AuthOrigin is the scheme and host of the provider's authorization URL:
// a sign-in form's CSP form-action names it, since browsers apply
// form-action to the redirect after a post.
func (o *OAuth) AuthOrigin() string {
	u, err := url.Parse(o.authURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// Verify exchanges the code (with the PKCE verifier and the same redirect
// URI) for an access token and reads the account with it.
func (o *OAuth) Verify(ctx context.Context, e Evidence) (Proof, error) {
	if !o.Enabled() {
		return Proof{}, ErrDisabled
	}
	if e.Code == "" || len(e.Code) > 2048 || e.Verifier == "" || e.RedirectURI == "" {
		return Proof{}, ErrFailed
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {e.Code}, "redirect_uri": {e.RedirectURI}, "code_verifier": {e.Verifier}}
	header := http.Header{}
	if o.basicAuth {
		header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(url.QueryEscape(o.creds.ClientID)+":"+url.QueryEscape(o.creds.ClientSecret))))
	} else {
		form.Set("client_id", o.creds.ClientID)
		form.Set("client_secret", o.creds.ClientSecret)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	// A refused code (400 invalid_grant, 401) is definite: it cannot be
	// tried again.
	if err := o.f.do(ctx, request{method: http.MethodPost, url: o.tokenURL, header: header, form: form, into: &tok, allow4x: true}); err != nil {
		return Proof{}, err
	}
	if tok.AccessToken == "" || (tok.TokenType != "" && !strings.EqualFold(tok.TokenType, "bearer")) {
		return Proof{}, ErrFailed
	}
	acct, err := o.profile(ctx, o.f, tok.AccessToken)
	if err != nil {
		return Proof{}, err
	}
	if !accountIDRE.MatchString(acct.ID) {
		return Proof{}, ErrFailed
	}
	if acct.Inputs == nil {
		acct.Inputs = Inputs{}
	}
	if !handleRE.MatchString(acct.Handle) {
		acct.Handle = ""
	}
	value := acct.ID
	if o.valueIsHandle {
		if acct.Handle == "" {
			return Proof{}, ErrFailed
		}
		value = strings.ToLower(acct.Handle)
	}
	return Proof{Kind: o.spec.Kind, Value: value, Root: o.spec.Kind + ":" + acct.ID, Handle: acct.Handle, Inputs: acct.Inputs, VerifiedAt: e.Now.Unix()}, nil
}

// handleRE is a provider handle the board may show or suggest: an X
// username or a GitHub login (letters, digits, _ and -, at most 39).
var handleRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,38}$`)

// Recheck keeps the proof: no token is kept, so it re-assesses the stored
// inputs (the account's age grows).
func (o *OAuth) Recheck(_ context.Context, p Proof, _ Evidence) (Proof, error) { return p, nil }

// isoTime reads an RFC 3339 time as unix seconds (0 when unreadable).
func isoTime(s string) int64 {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil || t.Unix() <= 0 {
		return 0
	}
	return t.Unix()
}

func oauthSpec(kind, label, proves, reveals, priced, env string) Spec {
	return Spec{Kind: kind, Label: label, Proves: proves, Reveals: reveals, Flow: FlowOAuth, Method: "oauth2-pkce",
		Root: "`" + kind + ":` + the account's stable id", Priced: priced, Needs: []string{kind, NeedBrowser}, EffortMinutes: 3, Env: []string{"PROOF_" + env + "_CLIENT_ID", "PROOF_" + env + "_CLIENT_SECRET"},
		ChallengeTTL: OAuthTTL,
		Action:       `standing.challenge {"schema":1,"kind":"` + kind + `"}, open data.auth_url in a browser and approve; the link is verified when ` + label[len("Link "):] + ` sends you back`}
}

const oauthReveals = "the account's stable id (never its name, email or token)."

func newGoogle(c OAuthCredentials, f fetcher) *OAuth {
	return &OAuth{spec: oauthSpec("google", "Link Google", "You control a Google account (sign in with Google, the openid scope only).", oauthReveals,
		"a fixed 50: Google tells no account age", "GOOGLE"),
		authURL: "https://accounts.google.com/o/oauth2/v2/auth", tokenURL: "https://oauth2.googleapis.com/token", scope: "openid",
		extra: url.Values{"prompt": {"select_account"}}, creds: c, f: f,
		profile: func(ctx context.Context, f fetcher, token string) (account, error) {
			var u struct {
				Sub string `json:"sub"`
			}
			if err := f.do(ctx, request{url: "https://openidconnect.googleapis.com/v1/userinfo", header: bearer(token), into: &u, allow4x: true}); err != nil {
				return account{}, err
			}
			return account{ID: u.Sub}, nil
		},
		assess: func(Inputs, time.Time) int64 { return 50 }}
}

func newX(c OAuthCredentials, f fetcher) *OAuth {
	return &OAuth{spec: oauthSpec("x", "Link X", "You control an X (Twitter) account; its age and followers count.", oauthReveals,
		"account age (up to 100, one-year ramp) + followers (up to 50, saturating at 100)", "X"),
		authURL: "https://x.com/i/oauth2/authorize", tokenURL: "https://api.x.com/2/oauth2/token", scope: "users.read tweet.read",
		basicAuth: true, creds: c, f: f,
		profile: func(ctx context.Context, f fetcher, token string) (account, error) {
			var u struct {
				Data struct {
					ID        string `json:"id"`
					Username  string `json:"username"`
					CreatedAt string `json:"created_at"`
					Metrics   struct {
						Followers int64 `json:"followers_count"`
					} `json:"public_metrics"`
				} `json:"data"`
			}
			if err := f.do(ctx, request{url: "https://api.x.com/2/users/me?user.fields=created_at,public_metrics", header: bearer(token), into: &u, allow4x: true}); err != nil {
				return account{}, err
			}
			return account{ID: u.Data.ID, Handle: u.Data.Username, Inputs: Inputs{"created_at": isoTime(u.Data.CreatedAt), "followers": u.Data.Metrics.Followers}}, nil
		},
		assess: func(in Inputs, now time.Time) int64 {
			return trust.AccountCents(ageDays(in["created_at"], now), 100, 365, in["followers"], 50, 100)
		}}
}

// discordEpochMS is Discord's snowflake epoch (2015-01-01) in milliseconds.
const discordEpochMS = 1420070400000

// SnowflakeTime is a Discord id's creation time in unix seconds (0 when the
// id is not a snowflake).
func SnowflakeTime(id string) int64 {
	n, err := strconv.ParseUint(id, 10, 64)
	if err != nil || n == 0 {
		return 0
	}
	return int64((n>>22)+discordEpochMS) / 1000
}

func newDiscord(c OAuthCredentials, f fetcher) *OAuth {
	return &OAuth{spec: oauthSpec("discord", "Link Discord", "You control a Discord account; its age counts.", oauthReveals,
		"account age (up to 100, one-year ramp, from the snowflake id)", "DISCORD"),
		authURL: "https://discord.com/oauth2/authorize", tokenURL: "https://discord.com/api/oauth2/token", scope: "identify",
		basicAuth: true, extra: url.Values{"prompt": {"consent"}}, creds: c, f: f,
		profile: func(ctx context.Context, f fetcher, token string) (account, error) {
			var u struct {
				ID string `json:"id"`
			}
			if err := f.do(ctx, request{url: "https://discord.com/api/users/@me", header: bearer(token), into: &u, allow4x: true}); err != nil {
				return account{}, err
			}
			created := SnowflakeTime(u.ID)
			if created == 0 {
				return account{}, ErrFailed
			}
			return account{ID: u.ID, Inputs: Inputs{"created_at": created}}, nil
		},
		assess: func(in Inputs, now time.Time) int64 {
			return trust.AccountCents(ageDays(in["created_at"], now), 100, 365, 0, 0, 0)
		}}
}

func newReddit(c OAuthCredentials, f fetcher) *OAuth {
	return &OAuth{spec: oauthSpec("reddit", "Link Reddit", "You control a Reddit account; its age and karma count.", oauthReveals,
		"account age (up to 120, one-year ramp) + karma (up to 80, saturating at 1,000)", "REDDIT"),
		authURL: "https://www.reddit.com/api/v1/authorize", tokenURL: "https://www.reddit.com/api/v1/access_token", scope: "identity",
		basicAuth: true, extra: url.Values{"duration": {"temporary"}}, creds: c, f: f,
		profile: func(ctx context.Context, f fetcher, token string) (account, error) {
			var u struct {
				ID         string  `json:"id"`
				CreatedUTC float64 `json:"created_utc"`
				TotalKarma int64   `json:"total_karma"`
			}
			if err := f.do(ctx, request{url: "https://oauth.reddit.com/api/v1/me", header: bearer(token), into: &u, allow4x: true}); err != nil {
				return account{}, err
			}
			created := int64(0)
			if u.CreatedUTC > 0 && u.CreatedUTC < math.MaxInt32*4 {
				created = int64(u.CreatedUTC)
			}
			return account{ID: u.ID, Inputs: Inputs{"created_at": created, "karma": u.TotalKarma}}, nil
		},
		assess: func(in Inputs, now time.Time) int64 {
			return trust.AccountCents(ageDays(in["created_at"], now), 120, 365, in["karma"], 80, 1000)
		}}
}

// hfNameRE is a Hugging Face username.
var hfNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,95}$`)

func newHuggingFace(c OAuthCredentials, f fetcher) *OAuth {
	return &OAuth{spec: oauthSpec("huggingface", "Link Hugging Face", "You control a Hugging Face account; its age and followers count.", oauthReveals,
		"account age (up to 100, one-year ramp) + followers (up to 50, saturating at 20)", "HUGGINGFACE"),
		authURL: "https://huggingface.co/oauth/authorize", tokenURL: "https://huggingface.co/oauth/token", scope: "openid profile",
		basicAuth: true, creds: c, f: f,
		profile: func(ctx context.Context, f fetcher, token string) (account, error) {
			var u struct {
				Sub      string `json:"sub"`
				Username string `json:"preferred_username"`
			}
			if err := f.do(ctx, request{url: "https://huggingface.co/oauth/userinfo", header: bearer(token), into: &u, allow4x: true}); err != nil {
				return account{}, err
			}
			in := Inputs{}
			// The public overview gives age and followers; without it the
			// account still links, at 0 until a later link reads them.
			var o struct {
				CreatedAt string `json:"createdAt"`
				Followers int64  `json:"numFollowers"`
			}
			if hfNameRE.MatchString(u.Username) && f.do(ctx, request{url: "https://huggingface.co/api/users/" + url.PathEscape(u.Username) + "/overview", into: &o}) == nil {
				in["created_at"], in["followers"] = isoTime(o.CreatedAt), o.Followers
			}
			return account{ID: u.Sub, Inputs: in}, nil
		},
		assess: func(in Inputs, now time.Time) int64 {
			return trust.AccountCents(ageDays(in["created_at"], now), 100, 365, in["followers"], 50, 20)
		}}
}

// newGitHubLogin is GitHub as a sign-in provider (an OAuth app, no scope:
// public profile only). It is not a standing way of its own: GitHub's
// standing way is the gist statement (existing.go), whose root it shares
// (github: + the numeric account id), so one GitHub account is one root
// however it was proved. Its value is the login, as the gist proof's.
func newGitHubLogin(c OAuthCredentials, f fetcher) *OAuth {
	return &OAuth{spec: Spec{Kind: "github", Label: "Link GitHub", Proves: "You control a GitHub account; its age and public activity count.", Reveals: "the GitHub login.",
		Flow: FlowOAuth, Method: "github-oauth", Root: "`github:` + GitHub's numeric account number", Env: []string{"LOGIN_GITHUB_CLIENT_ID", "LOGIN_GITHUB_CLIENT_SECRET"},
		ChallengeTTL: OAuthTTL, Needs: []string{"github", NeedBrowser}, EffortMinutes: 2},
		authURL: "https://github.com/login/oauth/authorize", tokenURL: "https://github.com/login/oauth/access_token", scope: "",
		extra: url.Values{"allow_signup": {"true"}}, creds: c, f: f, valueIsHandle: true,
		profile: func(ctx context.Context, f fetcher, token string) (account, error) {
			var u struct {
				ID          int64  `json:"id"`
				Login       string `json:"login"`
				CreatedAt   string `json:"created_at"`
				PublicRepos int64  `json:"public_repos"`
				Followers   int64  `json:"followers"`
			}
			if err := f.do(ctx, request{url: "https://api.github.com/user", header: bearer(token), into: &u, allow4x: true}); err != nil {
				return account{}, err
			}
			if u.ID <= 0 {
				return account{}, ErrFailed
			}
			return account{ID: strconv.FormatInt(u.ID, 10), Handle: u.Login, Inputs: Inputs{"created_at": isoTime(u.CreatedAt), "public_repos": u.PublicRepos, "followers": u.Followers}}, nil
		},
		assess: func(in Inputs, now time.Time) int64 {
			return trust.GitHubCents(ageDays(in["created_at"], now), in["public_repos"], in["followers"])
		}}
}

// Telegram is the Telegram login widget: Telegram signs the account's
// fields with a key derived from the bot token (SHA-256), which the board
// checks with HMAC-SHA-256 (core.telegram.org/widgets/login).
type Telegram struct {
	spec  Spec
	token string
	bot   string
}

// TelegramMaxAge is how old a widget answer's auth_date may be, in seconds.
const TelegramMaxAge = 600

func newTelegram(token, bot string) *Telegram {
	return &Telegram{spec: Spec{Kind: "telegram", Label: "Link Telegram", Proves: "You control a Telegram account (the login widget).", Reveals: oauthReveals,
		Flow: FlowWidget, Method: "telegram-login", Root: "`telegram:` + the account's numeric id", Priced: "a fixed 50: Telegram tells no account age",
		Env: []string{"PROOF_TELEGRAM_BOT_TOKEN", "PROOF_TELEGRAM_BOT_NAME"}, ChallengeTTL: OAuthTTL, Needs: []string{"telegram", NeedBrowser}, EffortMinutes: 3,
		Action: `standing.challenge {"schema":1,"kind":"telegram"}, open data.auth_url in a browser and log in with Telegram`}, token: token, bot: bot}
}

func (t *Telegram) Spec() Spec                                                    { return t.spec }
func (t *Telegram) Enabled() bool                                                 { return t.token != "" && t.bot != "" }
func (t *Telegram) Assess(Proof, time.Time) int64                                 { return 50 }
func (t *Telegram) Bot() string                                                   { return t.bot }
func (t *Telegram) Recheck(_ context.Context, p Proof, _ Evidence) (Proof, error) { return p, nil }

// telegramIDRE is a Telegram user id.
var telegramIDRE = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)

// Verify checks the widget's fields: the hash is the HMAC-SHA-256, keyed by
// SHA-256(bot token), of every other field (but the board's own state) as
// sorted key=value lines; auth_date is within TelegramMaxAge.
func (t *Telegram) Verify(_ context.Context, e Evidence) (Proof, error) {
	if !t.Enabled() {
		return Proof{}, ErrDisabled
	}
	got, err := hex.DecodeString(e.Fields.Get("hash"))
	if err != nil || len(got) != sha256.Size {
		return Proof{}, ErrFailed
	}
	var lines []string
	for k, vs := range e.Fields {
		if k == "hash" || k == "state" {
			continue
		}
		if len(vs) != 1 {
			return Proof{}, ErrFailed
		}
		lines = append(lines, k+"="+vs[0])
	}
	sort.Strings(lines)
	key := sha256.Sum256([]byte(t.token))
	mac := hmac.New(sha256.New, key[:])
	mac.Write([]byte(strings.Join(lines, "\n")))
	if !hmac.Equal(mac.Sum(nil), got) {
		return Proof{}, ErrFailed
	}
	at, err := strconv.ParseInt(e.Fields.Get("auth_date"), 10, 64)
	if err != nil || at > e.Now.Unix()+60 || e.Now.Unix()-at > TelegramMaxAge {
		return Proof{}, ErrFailed
	}
	id := e.Fields.Get("id")
	if !telegramIDRE.MatchString(id) {
		return Proof{}, ErrFailed
	}
	return Proof{Kind: "telegram", Value: id, Root: "telegram:" + id, Inputs: Inputs{}, VerifiedAt: e.Now.Unix()}, nil
}
