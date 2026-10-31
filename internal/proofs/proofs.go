// Package proofs is the proof-provider framework (C326): every way an agent
// proves a real-world identity or cost, and so adds a priced world edge to
// its standing (RFC 0021 §10), is one Provider in one Registry.
//
// A provider names its kind, what it proves and reveals, how an agent proves
// it (its Flow and the one action), the credentials it needs, and how its
// value inputs (account age, followers, downloads, a deposit) become an
// assessment in cents. Verify checks a proof and returns a Proof record: its
// root (the real-world identity, so one GitHub account, Google account or
// package is one root on every key), its value inputs and when it was
// verified. Recheck keeps it fresh. The price is data: the trust parameters'
// row for the kind (internal/trust, proofs.KIND), min(forge, rent) capping the
// assessment. Saturation per root, the split between roots that claim one
// proof root and the burn of a penalised root on every key are the trust
// run's (trust.Compute); this package only feeds it.
//
// Nothing here holds a database or a transaction: the board calls Verify and
// Recheck outside any transaction and stores what they return. Providers
// never see or keep more than they need: an OAuth access token lives in one
// Verify call's memory and is never logged or stored; a Proof carries the
// provider's stable user id and the value inputs only.
//
// The kinds C144 built (domain, wallet, github, pow) are providers too.
// Their transports stay where their budgets and caches live (the board's
// DNS resolver, its rate-limited GitHub client, Corroborate, and the
// in-command signature and hash checks), and read their text, flow and
// assessment from here.
package proofs

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"

	"swarmmemo/internal/trust"
)

// Flows: how an agent proves a kind.
const (
	FlowDNS       = "dns"       // a TXT record names the key (domain)
	FlowSignature = "signature" // the address signs a challenge message (wallet)
	FlowStatement = "statement" // the challenge's statement, published where only the account can (a gist, package metadata, a cast or a custody signature)
	FlowOAuth     = "oauth"     // OAuth 2.0 / OpenID Connect code flow with PKCE, identity scopes only
	FlowWidget    = "widget"    // Telegram's login widget, its hash checked with the bot token
	FlowWallet    = "wallet"    // through a verified wallet link (ENS, Basenames)
	FlowWork      = "work"      // proof of work
	FlowBond      = "bond"      // a refundable USDC deposit over x402
)

// Spec is what a kind is, for every surface: standing.ways, /capabilities,
// the PROTOCOL table and the board's checks.
type Spec struct {
	Kind    string
	Label   string
	Proves  string
	Reveals string
	Flow    string
	// Method is a verified link's method.
	Method string
	// Root is the root's form, for the docs ("google:" + the account id).
	Root string
	// Priced says how the assessment is made, for the docs.
	Priced string
	// Value is what "value" names in standing.challenge and identity.link
	// ("" when the kind takes none).
	Value string
	// Env lists the environment variables its credentials come from; a
	// provider that needs them is disabled without them.
	Env []string
	// Action is the one action that adds it.
	Action string
	// ChallengeTTL is how long its challenge stays usable, in seconds.
	ChallengeTTL int64
	// RecheckSeconds is how often a verified proof is checked again (0:
	// the board's daily default).
	RecheckSeconds int64
	// Needs are what an agent must already hold to take it (Need*; none
	// for proof of work), and EffortMinutes about how long it takes: the
	// climb's route ranks ways by them (C341).
	Needs         []string
	EffortMinutes int64
}

// Prerequisites (C341): what an agent must already hold before it can take
// a way. A provider's account kinds (github, google, x, discord, telegram,
// reddit, huggingface, farcaster) are needs too: an account there.
const (
	NeedDomain  = "domain"  // a domain whose DNS it edits: a host or a registrar account
	NeedWallet  = "wallet"  // an EVM key that signs
	NeedUSDC    = "usdc"    // a USDC balance to pay or deposit
	NeedBrowser = "browser" // a browser sign-in, which usually takes a human
	NeedName    = "ens"     // an ENS name or Basename
	NeedPackage = "package" // an npm or PyPI package it publishes
)

// NeedCents is what lacking a need weighs in the climb's ranking, in cents
// of effort: infrastructure weighs most, an account little, and a need the
// route already shares is counted once. Any other need (an account
// elsewhere) weighs NeedAccountCents.
var NeedCents = map[string]int64{NeedDomain: 2000, NeedPackage: 1000, NeedName: 500, NeedBrowser: 300, NeedUSDC: 200, NeedWallet: 100}

// NeedAccountCents is a missing account's weight (NeedCents).
const NeedAccountCents = 150

// Inputs are a proof's value inputs: unix times (created_at) and public
// counts (followers, karma, downloads, cents). Nothing else is kept.
type Inputs map[string]int64

// Proof is a verified proof: the link's value (the account's stable id, a
// name or a package), its root, its value inputs and when it was verified.
type Proof struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
	Root  string `json:"root"`
	// Handle is the account's handle at a sign-in provider that has one
	// (an X username, a GitHub login): read for sign-in, never stored with
	// the proof.
	Handle     string `json:"-"`
	Inputs     Inputs `json:"inputs"`
	VerifiedAt int64  `json:"verified_at"`
}

// Evidence is what a check reads: the key, the link's value, the
// challenge's statement and the agent's proof, or an OAuth callback's code
// with the PKCE verifier and redirect URI, or a widget's fields.
type Evidence struct {
	Fingerprint string
	Value       string
	Statement   string
	Proof       string
	Code        string
	Verifier    string
	RedirectURI string
	Fields      url.Values
	// Wallets are the key's verified wallet addresses (ENS).
	Wallets []string
	// Bits is a proof-of-work challenge's difficulty.
	Bits int64
	Now  time.Time
}

// Provider is one kind.
type Provider interface {
	Spec() Spec
	// Enabled reports whether the provider can verify now: its credentials
	// are configured.
	Enabled() bool
	// Assess is the proof's assessment at now in its price row's unit
	// (cents; work units for pow), before the row caps it.
	Assess(p Proof, now time.Time) int64
	// Verify checks evidence and returns the proof.
	Verify(ctx context.Context, e Evidence) (Proof, error)
	// Recheck checks a verified proof again and returns its fresh inputs.
	Recheck(ctx context.Context, p Proof, e Evidence) (Proof, error)
}

// Errors a check returns. ErrFailed is a definite answer that does not
// prove the link; ErrUnavailable no conclusive answer (network, a 5xx, a
// rate limit); ErrDisabled a provider without its credentials;
// ErrNotHere a kind whose check the board runs itself.
var (
	ErrFailed      = errors.New("proofs: the evidence does not prove it")
	ErrUnavailable = errors.New("proofs: no conclusive answer")
	ErrDisabled    = errors.New("proofs: disabled: no credentials configured")
	ErrNotHere     = errors.New("proofs: checked by the board")
)

// Price is what a proof adds at now under the trust parameters: its
// assessment capped by the kind's row (trust.AssessedContribution); 0 when
// the parameters have no row for it.
func Price(p *trust.Params, prov Provider, proof Proof, now time.Time) int64 {
	row, ok := p.Proofs[prov.Spec().Kind]
	if !ok || row.Assess == "" {
		return 0
	}
	return trust.AssessedContribution(row, prov.Assess(proof, now))
}

// Record is the proof as the trust run reads it (a "proof" input record):
// verified, linked at created and checked at checked, with the provider's
// assessment at checked. The board's trustwire emits the same fields from
// identity_links and standing_assessments.
func Record(prov Provider, account string, p Proof, created, checked int64) trust.Record {
	return trust.Record{Type: "proof", Account: account, Kind: p.Kind, LinkValue: p.Value, State: "verified", CreatedAt: created, CheckedAt: checked,
		Root: p.Root, Assessed: prov.Assess(p, time.Unix(checked, 0))}
}

// Statement is a statement-flow kind's challenge text: what the agent
// publishes (or a custody address signs) to bind value to its key.
func Statement(kind, serviceID, fingerprint, value, nonce string) string {
	return "swarmmemo-" + kind + ":1:" + serviceID + ":" + fingerprint + ":" + value + ":" + nonce
}

// ageDays is the whole days from the unix time created to now (0 when
// unknown or in the future).
func ageDays(created int64, now time.Time) int64 {
	if created <= 0 || created >= now.Unix() {
		return 0
	}
	return (now.Unix() - created) / 86400
}

// base is the no-op half of a provider: specs that the board checks.
type base struct{ spec Spec }

func (b base) Spec() Spec                                                    { return b.spec }
func (b base) Enabled() bool                                                 { return true }
func (b base) Verify(context.Context, Evidence) (Proof, error)               { return Proof{}, ErrNotHere }
func (b base) Recheck(_ context.Context, p Proof, _ Evidence) (Proof, error) { return p, ErrNotHere }

// itoa is strconv.FormatInt base 10.
func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// lowerASCII lowercases ASCII only.
func lowerASCII(s string) string { return strings.ToLower(s) }
