package proofs

import (
	"context"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"
)

// Bond is a refundable USDC deposit over x402 to the operator's wallet: a
// world edge at face value (inputs "cents"), burned by a penalty, refunded
// on request after a cooling period as owed USDC the steward pays. The board
// takes the payment and keeps the bond (standing.bond).
type Bond struct{ base }

func newBond() Bond {
	return Bond{base{Spec{Kind: "bond", Label: "Post a bond", Proves: "You put money at risk here: a refundable USDC deposit a penalty burns.",
		Reveals: "the paying address.", Flow: FlowBond, Method: "x402-deposit", Root: "`bond:` + the deposit's id", Needs: []string{NeedUSDC}, EffortMinutes: 5,
		Priced: "its face value, up to the row's cap; refunded on request after a cooling period with no open penalty",
		Action: `standing.challenge {"schema":1,"kind":"bond","usdc":"2.00"}, pay its 402 quote over x402 (as a credit top-up) and send it again with the payment; after the cooling period standing.challenge {"schema":1,"kind":"bond","action":"refund"} asks for it back`}}}
}

// Assess is the deposit's face value in cents.
func (Bond) Assess(p Proof, _ time.Time) int64 { return p.Inputs["cents"] }

// Config is the registry's configuration: credentials and endpoints.
type Config struct {
	// OAuth are the OAuth providers' client registrations by kind.
	OAuth map[string]OAuthCredentials
	// TelegramBotToken and TelegramBot are the login widget's bot.
	TelegramBotToken string
	TelegramBot      string
	// FarcasterHub is a hub's HTTP API (DefaultFarcasterHub when empty);
	// FarcasterHubKey an optional x-api-key for a hosted hub.
	FarcasterHub    string
	FarcasterHubKey string
	// ENSRPC and BaseRPC are JSON-RPC endpoints (defaults when empty).
	ENSRPC  string
	BaseRPC string
	// Client is the HTTP client (SafeClient when nil); TestBaseURL
	// rewrites every provider URL https://HOST/… to TestBaseURL/HOST/…
	// (tests only).
	Client      *http.Client
	TestBaseURL string
}

// ConfigFromEnv reads the configuration from the environment: for each
// OAuth kind PROOF_KIND_CLIENT_ID and PROOF_KIND_CLIENT_SECRET (GitHub's
// sign-in app: LOGIN_GITHUB_CLIENT_ID and LOGIN_GITHUB_CLIENT_SECRET), then
// PROOF_TELEGRAM_BOT_TOKEN, PROOF_TELEGRAM_BOT_NAME, PROOF_FARCASTER_HUB_URL,
// PROOF_FARCASTER_HUB_KEY, PROOF_ENS_RPC_URL and PROOF_BASE_RPC_URL.
func ConfigFromEnv(getenv func(string) string) Config {
	if getenv == nil {
		getenv = os.Getenv
	}
	pair := func(id, secret string) OAuthCredentials {
		return OAuthCredentials{ClientID: strings.TrimSpace(id), ClientSecret: strings.TrimSpace(secret)}
	}
	// Each name spelled out, so .env.example is held to them
	// (cmd/swarmmemo TestEnvExampleNamesEveryVariableTheServerReads).
	c := Config{OAuth: map[string]OAuthCredentials{
		"google":      pair(getenv("PROOF_GOOGLE_CLIENT_ID"), getenv("PROOF_GOOGLE_CLIENT_SECRET")),
		"x":           pair(getenv("PROOF_X_CLIENT_ID"), getenv("PROOF_X_CLIENT_SECRET")),
		"discord":     pair(getenv("PROOF_DISCORD_CLIENT_ID"), getenv("PROOF_DISCORD_CLIENT_SECRET")),
		"reddit":      pair(getenv("PROOF_REDDIT_CLIENT_ID"), getenv("PROOF_REDDIT_CLIENT_SECRET")),
		"huggingface": pair(getenv("PROOF_HUGGINGFACE_CLIENT_ID"), getenv("PROOF_HUGGINGFACE_CLIENT_SECRET")),
		// GitHub signs people in only (LoginKinds): its standing way is the
		// gist statement, which needs no app.
		"github": pair(getenv("LOGIN_GITHUB_CLIENT_ID"), getenv("LOGIN_GITHUB_CLIENT_SECRET")),
	}}
	c.TelegramBotToken = strings.TrimSpace(getenv("PROOF_TELEGRAM_BOT_TOKEN"))
	c.TelegramBot = strings.TrimPrefix(strings.TrimSpace(getenv("PROOF_TELEGRAM_BOT_NAME")), "@")
	c.FarcasterHub = strings.TrimSpace(getenv("PROOF_FARCASTER_HUB_URL"))
	c.FarcasterHubKey = strings.TrimSpace(getenv("PROOF_FARCASTER_HUB_KEY"))
	c.ENSRPC = strings.TrimSpace(getenv("PROOF_ENS_RPC_URL"))
	c.BaseRPC = strings.TrimSpace(getenv("PROOF_BASE_RPC_URL"))
	return c
}

// Registry is every provider, in the order surfaces show them.
type Registry struct {
	order     []string
	providers map[string]Provider
	// logins are the sign-in providers (LoginKinds): X and Google are the
	// proof providers themselves, GitHub its own OAuth app.
	logins map[string]*OAuth
}

// LoginKinds are the providers a person can sign in with to create or
// recover a hosted identity (/login/KIND), in the order pages show them.
var LoginKinds = []string{"x", "google", "github"}

// New builds the registry.
func New(c Config) *Registry {
	f := fetcher{client: c.Client, testBase: c.TestBaseURL}
	ps := []Provider{
		newDomain(), newWallet(), newGitHub(), newPow(),
		newGoogle(c.OAuth["google"], f), newX(c.OAuth["x"], f), newDiscord(c.OAuth["discord"], f), newTelegram(c.TelegramBotToken, c.TelegramBot),
		newReddit(c.OAuth["reddit"], f), newHuggingFace(c.OAuth["huggingface"], f),
		newFarcaster(c.FarcasterHub, c.FarcasterHubKey, f), newENS(c.ENSRPC, c.BaseRPC, f), newNPM(f), newPyPI(f), newBond(),
	}
	r := &Registry{providers: map[string]Provider{}, logins: map[string]*OAuth{"github": newGitHubLogin(c.OAuth["github"], f)}}
	for _, p := range ps {
		k := p.Spec().Kind
		r.order = append(r.order, k)
		r.providers[k] = p
		if o, ok := p.(*OAuth); ok && slices.Contains(LoginKinds, k) {
			r.logins[k] = o
		}
	}
	return r
}

// Login is a sign-in provider (LoginKinds), configured or not.
func (r *Registry) Login(kind string) (*OAuth, bool) {
	o, ok := r.logins[kind]
	return o, ok
}

// Default is the registry with no credentials: every kind listed, the
// OAuth and widget kinds disabled. Specs, prices and docs read it.
var Default = New(Config{})

// Kinds lists the kinds in order.
func (r *Registry) Kinds() []string { return append([]string(nil), r.order...) }

// Get is a kind's provider.
func (r *Registry) Get(kind string) (Provider, bool) {
	p, ok := r.providers[kind]
	return p, ok
}

// Status is a kind's status for /capabilities: enabled, or
// disabled-no-credentials.
func (r *Registry) Status(kind string) string {
	if p, ok := r.providers[kind]; ok && p.Enabled() {
		return "enabled"
	}
	return "disabled-no-credentials"
}

// Check runs a provider's Verify with a bound on its time.
func Check(ctx context.Context, p Provider, e Evidence) (Proof, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*fetchTimeout)
	defer cancel()
	if !p.Enabled() {
		return Proof{}, ErrDisabled
	}
	return p.Verify(ctx, e)
}
