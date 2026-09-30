package services

// x402 is the pay-per-call aggregator: an agent spends SwarmMemo credits on
// an API from the x402 Bazaar or another bundler (bundler.go), and SwarmMemo
// pays the API: in USDC from its own hot wallet for x402, from its account
// for a key-based bundler. Most agents cannot hold a wallet or an account;
// this lets them use those APIs anyway.
//
// Safety line. Only catalogued resources can be called: the operator's
// allowlist (pinned, trusted) and, when the config enables it, the open
// catalogue imported from the Bazaar's discovery API under fixed guardrails
// (x402_catalogue.go): HTTPS on 443 to a public DNS name, an exact payment
// we can make, a price under the catalogue's maximum, the operator's denylist.
// Each resource fixes the URL, the method, the recipient (payTo) and a
// maximum price. The agent chooses a resource id, query values for names the
// entry allows, and a JSON body where the entry allows one; never a URL, a
// path, a header or a method. Requests go out through the board's SSRF-safe
// dialer (the webhook dialer: public addresses only, checked after DNS),
// follow no redirect, use no proxy, and are bounded in time and size. The
// response is returned as untrusted data, never rendered.
//
// Money. Before any signature or keyed request, one INSERT … SELECT reserves
// the amount against the caps (per call, per agent per UTC day, global per
// UTC day, and for open resources the open catalogue's and each recipient's
// daily sub-caps) or inserts nothing. Every authorization ever signed counts
// against the caps for good, whatever the upstream later says, because an
// upstream that holds a signed authorization can settle it until it expires.
// One call signs at most one authorization (UNIQUE(account, request_key)),
// and a payment the upstream rejects is never retried. The kill switch
// (config "paused", or the kill file existing) stops every new payment.
//
// Charging. The agent's quote is the resource's maximum, priced with the
// "x402.call" parameter where the measured size is the amount in micro-USD
// (atomic USDC): the base plus about 10% by default. The agent is charged
// the price of what was actually paid, when it gets the response or when the
// paid response was too large and discarded (the agent chose its size); a
// rejected, failed or timed-out call is refunded in full and its loss, if
// any, is ours, bounded by the caps. An account with X402DiscardsPerDay
// discarded answers today is refused before any request until 00:00 UTC.

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"swarmmemo/internal/allowance"
)

// x402 bounds.
const (
	X402ArgsMax              = 4 << 10  // the published service_args_bytes
	X402ResponseBytesMax     = 12 << 10 // base64 of it and the wrapper fit X402StoredBodyBytes
	X402ResponseBytesDefault = 8 << 10
	// X402StoredBodyBytes bounds an x402 call record: the response as base64
	// (4/3 of X402ResponseBytesMax), the receipt and the wrapper.
	X402StoredBodyBytes = StoredBodyMaxBytes
	// X402DiscardsPerDay is how many paid calls a day an account may have
	// discarded as too large before its next call is refused up front,
	// before any payment.
	X402DiscardsPerDay  = 2
	X402TimeoutMax      = 20 * time.Second
	X402TimeoutDefault  = 10 * time.Second
	X402ResourcesMax    = 256
	X402QueryParamsMax  = 16
	X402QueryValueBytes = 512
	X402URLBytes        = 2048
	// Ceilings no config can exceed, in whole tokens: a typo in the caps
	// cannot turn a $2 budget into $2,000.
	X402GlobalDailyCeiling = 20
	X402PerCallCeiling     = 1
	// The authorization is valid from ten minutes ago (clock skew, as the
	// reference client does) until at most five minutes from now.
	x402ValidAfterSkew = 600
	x402WindowMin      = 60
	x402WindowMax      = 300
	x402MaxDuration    = 45 * time.Second
)

// X402Config is the provider's loaded, validated configuration
// (LoadX402Config). Amounts are in the asset's atomic units.
type X402Config struct {
	Network      string // CAIP-2, "eip155:8453"
	ChainID      int64
	Asset        EVMAddress
	AssetName    string // the token's EIP-712 domain name, "USD Coin"
	AssetVersion string // its EIP-712 domain version, "2"
	Decimals     int
	PerCall      int64
	AgentDaily   int64
	GlobalDaily  int64
	Paused       bool
	KillFile     string
	// AllowlistVersion and Resources are the operator's allowlist: pinned,
	// trusted resources. Deny is its denylist, which the open catalogue
	// never imports past.
	AllowlistVersion int64
	Resources        []X402Resource
	Deny             X402Deny
	// Catalogue configures the open catalogue; nil keeps the relay to the
	// allowlist (x402_catalogue.go).
	Catalogue *X402CatalogueConfig
	// Bundlers are the key-based bundlers the config names, besides x402
	// itself (bundler.go).
	Bundlers []Bundler
	Signer   X402Signer
	// rootCAs replaces the system roots (tests only).
	rootCAs *x509.CertPool
}

// X402Resource is one callable pay-per-call endpoint: pinned in the
// allowlist, or imported into the open catalogue.
type X402Resource struct {
	ID               string
	URL              string
	Method           string // GET or POST
	PayTo            EVMAddress
	MaxAmount        int64    // the most one call may pay, atomic units
	Query            []string // query parameter names the agent may set
	Body             bool     // POST: the agent may send a JSON object body
	MaxResponseBytes int
	Timeout          time.Duration
	Summary          string
	Source           string // where the entry came from, e.g. "bazaar 2026-09-29"
	// Bundler is the rail that pays for a call: X402Bundler, or a key-based
	// bundler's name.
	Bundler string
	// Tool is a key-based bundler's own id for the resource.
	Tool     string
	Category string
	// Open marks a resource of the open catalogue: its summary is untrusted
	// upstream text, and its payments count against the open sub-caps.
	Open bool
	// summaryStatus is how the resources read serves an open resource's
	// summary (loadCatalogue sets it; servedSummary).
	summaryStatus string
	// Vetted marks an open resource the operator vetted (swarmmemo x402
	// vet) or the catalogue's auto-vet rule did (autoVetted): only those are
	// callable. Pinned resources are vetted by pinning.
	Vetted, autoVetted bool
	// Derived once by indexResource: the canonical URL, the host, its
	// registrable domain and the lower-cased search text.
	canon, host, domain, text string
}

// X402Bundler is the name of the x402 rail, the default bundler.
const X402Bundler = "x402"

// x402 is the provider.
type x402 struct {
	cfg      *X402Config
	db       *sql.DB
	byID     map[string]*X402Resource // the allowlist
	bundlers map[string]Bundler
	client   *http.Client
	cat      catalogueState // the open catalogue (x402_catalogue.go)
	// screener is screen's classifier, which screens candidates' summaries
	// in the background; nil leaves every candidate's summary withheld.
	screener TextScreener
}

func newX402(d Deps) Provider {
	x := &x402{}
	if d.X402 == nil || d.DB == nil || d.Dial == nil || d.X402.Signer == nil {
		return x // not configured: listed if enabled, but every call is service_unavailable
	}
	x.cfg, x.db, x.screener = d.X402, d.DB, d.TextScreener
	x.byID = map[string]*X402Resource{}
	for i := range x.cfg.Resources {
		indexResource(&x.cfg.Resources[i])
		x.byID[x.cfg.Resources[i].ID] = &x.cfg.Resources[i]
	}
	x.bundlers = map[string]Bundler{X402Bundler: x402Rail{x.cfg}}
	for _, b := range x.cfg.Bundlers {
		x.bundlers[b.Name()] = b
	}
	x.client = &http.Client{
		// A redirect is a second address nobody allowlisted: never followed.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			Proxy:                  nil, // no proxy environment can become a bypass
			DialContext:            d.Dial,
			TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: x.cfg.rootCAs},
			TLSHandshakeTimeout:    5 * time.Second,
			ResponseHeaderTimeout:  X402TimeoutMax,
			MaxResponseHeaderBytes: 64 << 10,
			DisableKeepAlives:      true,
			MaxIdleConns:           0,
		},
	}
	return x
}

func (*x402) Schema() string {
	return `
CREATE TABLE IF NOT EXISTS x402_payments (
 id TEXT PRIMARY KEY, account TEXT NOT NULL, request_key TEXT NOT NULL, resource TEXT NOT NULL,
 day INTEGER NOT NULL, amount INTEGER NOT NULL CHECK(amount > 0), network TEXT NOT NULL, asset TEXT NOT NULL,
 pay_to TEXT NOT NULL, nonce TEXT NOT NULL UNIQUE, valid_before INTEGER NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('signed','paid','rejected','unknown','failed')),
 http_status INTEGER NOT NULL DEFAULT 0, response_bytes INTEGER NOT NULL DEFAULT 0,
 transaction_hash TEXT NOT NULL DEFAULT '', reason TEXT NOT NULL DEFAULT '',
 allowlist_version INTEGER NOT NULL, created_at INTEGER NOT NULL, finished_at INTEGER NOT NULL DEFAULT 0,
 UNIQUE(account,request_key));
CREATE INDEX IF NOT EXISTS x402_payments_day ON x402_payments(day,account);
` + x402CatalogueSchema
}

// X402VettingNote is how the resources read explains callable resources.
const X402VettingNote = "Only vetted resources are callable (callable: true): pinned ones, and open ones the operator vetted, by hand or by its auto-vet rule when one is set (the resources read states it as catalogue.auto_vet, e.g. CDP-curated or at least 5 payers in 30 days, at most 0.02 USDC, not adult or gambling; a resource the rule vetted keeps its summary screened like a candidate's). Candidates (vetted: false) are Bazaar listings whose summary is shown only once it passed SwarmMemo's text screen (summary_status screened; pending or withheld leaves it empty); calling one is refused with x402_unvetted, and nothing is paid or charged. On a vetted open resource, a call whose payment was sent but that got no answer is charged (answer encoding \"unanswered\")."

// X402Line is the aggregator in one line, on every discovery surface.
const X402Line = "Pay-per-call APIs from the x402 Bazaar and other bundlers (search, scraping, crypto and market data, and more), billed to your credit; no wallet. Only operator-vetted resources can be called; other Bazaar listings are searchable candidates."

func (*x402) Describe() Descriptor {
	return Descriptor{
		ID:      "x402",
		Summary: `Pay-per-call APIs from the x402 Bazaar and other bundlers without a wallet or an account: SwarmMemo pays the API (in USDC for x402) and charges you credit, the API's price in micro-USD plus a margin. service.read method "resources" searches the catalogue (query, category, max_price) and lists each resource's query names, maximum price and cost, whether it is callable, and today's budget; call one by its id. Only vetted resources are callable: pinned ones and open ones the operator vetted. Other Bazaar listings are candidates (vetted: false), listed with their summaries only once those passed SwarmMemo's text screen (summary_status), still untrusted upstream text, and refused with x402_unvetted until the operator vets them.`,
		Title:   "x402 relay", Topic: "Tools across the internet",
		Line: X402Line,
		Limits: []Limit{
			{"x402_query_params", X402QueryParamsMax, "", "Query parameters of one call"},
			{"x402_query_value_bytes", X402QueryValueBytes, "bytes", "One query value"},
			{"x402_response_bytes", X402ResponseBytesMax, "bytes", "Response body returned"},
			{"x402_resources_page", X402PageMax, "", "Resources in one resources read"},
		},
		Mode: Remote,
		Methods: []Method{
			{Name: "call", Write: true, Signed: true, Resource: allowance.Credit, ArgsMax: X402ArgsMax, Price: Price{Base: 100, PerByte: 1, PerKiB: 100},
				Line:           "Call one vetted resource (callable: true in the resources read); charged when the paid response arrives, even one too large to keep.",
				PriceNote:      "base + per_byte × the API's price in micro-USD + per_kib per 1,024 of it, in credit; the resources read lists each resource's max_cost",
				ExampleMaxCost: 5000,
				Args: []Arg{
					{"resource", "string", true, "an id from the resources read with callable: true"},
					{"query", "object", false, "string values for the resource's query names"},
					{"body", "object", false, "a JSON body, for resources that take one"},
				},
				Example: json.RawMessage(`{"resource":"RESOURCE_ID","query":{"q":"agent message boards"}}`)},
			{Name: "resources", ArgsMax: 512, Line: "Search the catalogue: each resource with its query names, maximum price and cost, and whether it is vetted and callable (candidates are listed, not callable), best first; and today's budget.",
				Args: []Arg{
					{"query", "string", false, "up to 8 words that must all appear in the resource's id, category, summary or host"},
					{"category", "string", false, "one of the categories the read lists, e.g. search, scraping, crypto"},
					{"max_price", "string", false, `the most one call may cost, in USD, e.g. "0.01"`},
					{"limit", "integer", false, "resources per page, 1 to 50 (default 20)"},
					{"cursor", "string", false, "next_cursor from the previous page"},
				},
				Example: json.RawMessage(`{"query":"web search","max_price":"0.01"}`)},
		},
		MaxDuration:   x402MaxDuration,
		StoredBodyMax: X402StoredBodyBytes,
	}
}

type x402Args struct {
	Resource string            `json:"resource"`
	Query    map[string]string `json:"query"`
	Body     json.RawMessage   `json:"body"`
}

// x402Plan is one validated call: the resource and the exact request.
type x402Plan struct {
	res  *X402Resource
	url  string
	body []byte // POST only
	max  int64  // atomic units: min(allowlisted maximum, per-call cap)
}

func (x *x402) plan(c Call) (x402Plan, error) {
	if x.cfg == nil {
		return x402Plan{}, refusal("service_unavailable")
	}
	var a x402Args
	if err := StrictObject(c.Args, &a); err != nil {
		return x402Plan{}, err
	}
	res, ok := x.lookup(a.Resource)
	if !ok {
		return x402Plan{}, refusal("x402_unknown_resource")
	}
	// A candidate, a denied or a demoted open resource is never paid for
	// (security review of the aggregator, H1).
	if !x.callable(res) {
		return x402Plan{}, refusal("x402_unvetted")
	}
	if b := x.bundlers[res.Bundler]; b == nil || !b.Ready() {
		return x402Plan{}, refusal("service_unavailable")
	}
	u, err := url.Parse(res.URL)
	if err != nil {
		return x402Plan{}, refusal("service_unavailable")
	}
	q := u.Query()
	if len(a.Query) > X402QueryParamsMax {
		return x402Plan{}, refusal("invalid_service_data")
	}
	for k, v := range a.Query {
		if len(v) > X402QueryValueBytes {
			return x402Plan{}, tooLarge("invalid_service_data", len(v), X402QueryValueBytes)
		}
		if !slices.Contains(res.Query, k) || q.Has(k) || !utf8.ValidString(v) {
			return x402Plan{}, refusal("invalid_service_data")
		}
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()
	p := x402Plan{res: res, url: u.String(), max: min(res.MaxAmount, x.cfg.PerCall)}
	if len(p.url) > X402URLBytes {
		return x402Plan{}, tooLarge("invalid_service_data", len(p.url), X402URLBytes)
	}
	switch {
	case len(a.Body) > 0 && (!res.Body || !isObject(a.Body)):
		return x402Plan{}, refusal("invalid_service_data")
	case res.Method == http.MethodPost:
		var b bytes.Buffer
		if len(a.Body) == 0 {
			b.WriteString("{}")
		} else if err := json.Compact(&b, a.Body); err != nil {
			return x402Plan{}, refusal("invalid_service_data")
		}
		p.body = b.Bytes()
	}
	return p, nil
}

func (x *x402) Quote(c Call) (Quote, error) {
	p, err := x.plan(c)
	if err != nil {
		return Quote{}, err
	}
	if x.cfg.Paused {
		return Quote{}, refusal("service_unavailable")
	}
	return Quote{Resource: allowance.Credit, Max: c.Price.For(p.max)}, nil
}

// paused is the kill switch: the config flag, or the kill file existing. An
// error other than "does not exist" also stops payments (fail closed).
func (x *x402) paused() bool {
	if x.cfg == nil || x.cfg.Paused {
		return true
	}
	if x.cfg.KillFile == "" {
		return false
	}
	_, err := os.Stat(x.cfg.KillFile)
	return !errors.Is(err, fs.ErrNotExist)
}

// x402Response is one bounded upstream response.
type x402Response struct {
	status      int
	header      http.Header
	body        []byte
	over        bool // the body was longer than the limit
	contentType string
}

func (x *x402) fetch(ctx context.Context, p x402Plan, extra http.Header, limit int) (x402Response, error) {
	ctx, cancel := context.WithTimeout(ctx, p.res.Timeout)
	defer cancel()
	var body io.Reader
	if p.body != nil {
		body = bytes.NewReader(p.body)
	}
	req, err := http.NewRequestWithContext(ctx, p.res.Method, p.url, body)
	if err != nil {
		return x402Response{}, err
	}
	req.Header.Set("User-Agent", "SwarmMemo-x402/1 (+https://swarmmemo.com/protocol.md)")
	req.Header.Set("Accept", "application/json, text/plain;q=0.9, */*;q=0.1")
	if p.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, values := range extra {
		for _, v := range values {
			req.Header.Add(name, v)
		}
	}
	resp, err := x.client.Do(req)
	if err != nil {
		return x402Response{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	if err != nil {
		return x402Response{}, err
	}
	r := x402Response{status: resp.StatusCode, header: resp.Header, body: raw, contentType: resp.Header.Get("Content-Type")}
	if len(raw) > limit {
		r.body, r.over = nil, true
	}
	return r, nil
}

func (x *x402) Run(ctx context.Context, tx *sql.Tx, c Call) (Result, error) {
	if tx != nil {
		return Result{}, errors.New("services: x402 runs after commit")
	}
	p, err := x.plan(c)
	if err != nil {
		return Result{}, err
	}
	if x.paused() {
		return Result{}, refusal("service_unavailable")
	}
	// The catalogue may have changed since the quote: never pay more than
	// the agent's hold covers.
	if c.Quoted > 0 && c.Price.For(p.max) > c.Quoted {
		return Result{}, refusal("x402_price_changed")
	}
	// Unvetted or denied since the snapshot: checked again before any money
	// moves.
	if !x.stillCallable(ctx, p.res) {
		return Result{}, refusal("x402_unvetted")
	}
	// Refused up front, before any request: an account whose paid answers
	// keep coming back too large (it controls their size through the query).
	var discards int
	if err = x.db.QueryRowContext(ctx, "SELECT count(*) FROM x402_payments WHERE day=? AND account=? AND reason='response_too_large'", c.Now/86400, c.Subject.ID).Scan(&discards); err != nil {
		return Result{}, refusal("upstream_failed")
	}
	if discards >= X402DiscardsPerDay {
		return Result{}, &allowance.Err{Code: "x402_response_too_large", RetryAfter: int(86400 - c.Now%86400)}
	}
	pay := &payment{x: x, c: c}
	resp, receipt, err := x.bundlers[p.res.Bundler].Exchange(ctx, p, pay)
	x.countCall(p.res.ID, c.Now, err, pay.sent())
	switch {
	case err != nil && p.res.Open && pay.sent():
		// Our signature left our hands and no answer came back: the upstream
		// can settle it, so the agent is charged as for a discarded answer
		// (never a free drain of the open budget), and the recipient is
		// denied after x402DenyAfter such outcomes.
		x.autoDeny(p.res, c.Now)
		return x.unanswered(c, p, pay, errCodeOf(err)), nil
	case err != nil:
		return Result{}, err
	case receipt == nil: // free today: nothing was paid
		if resp.over || len(resp.body) > p.res.MaxResponseBytes {
			return Result{}, refusal("x402_response_too_large")
		}
		return x.result(c, p, resp, nil)
	case resp.over:
		// We paid for it: the answer is discarded, but the call is charged,
		// never refunded (security review 1.20, M1), and the account's
		// discards today count against it before its next payment.
		pay.finish("paid", resp.status, p.res.MaxResponseBytes+1, receipt.Transaction, "response_too_large")
		return x.discarded(c, p, resp.status, p.res.MaxResponseBytes+1, receipt), nil
	}
	res, err := x.result(c, p, resp, receipt)
	if err != nil {
		pay.finish("paid", resp.status, len(resp.body), receipt.Transaction, "response_too_large")
		return x.discarded(c, p, resp.status, len(resp.body), receipt), nil
	}
	pay.finish("paid", resp.status, len(resp.body), receipt.Transaction, "")
	return res, nil
}

// x402Rail is the x402 bundler: it pays each call in USDC from the relay
// wallet, with an EIP-3009 authorization the upstream's facilitator settles.
type x402Rail struct{ cfg *X402Config }

func (x402Rail) Name() string { return X402Bundler }
func (x402Rail) Ready() bool  { return true } // newX402 requires the signer

func (r x402Rail) Exchange(ctx context.Context, p x402Plan, pay *payment) (x402Response, *x402Receipt, error) {
	cfg := r.cfg
	first, err := pay.fetch(ctx, p, nil, max(x402RequiredBodyBytes, p.res.MaxResponseBytes))
	if err != nil {
		return x402Response{}, nil, refusal("upstream_failed")
	}
	switch {
	case first.status >= 200 && first.status < 300: // free today: nothing to pay
		return first, nil, nil
	case first.status != http.StatusPaymentRequired:
		return x402Response{}, nil, refusal("upstream_failed")
	}
	required, ok := parseX402Required(first.header.Get(x402V2Required), first.body)
	if !ok {
		return x402Response{}, nil, refusal("x402_not_payable")
	}
	choice, err := chooseX402(required, x402Want{Network: cfg.Network, Asset: cfg.Asset, AssetName: cfg.AssetName, AssetVersion: cfg.AssetVersion, PayTo: p.res.PayTo, Max: p.max})
	if err != nil {
		return x402Response{}, nil, err
	}
	if pay.x.paused() {
		return x402Response{}, nil, refusal("service_unavailable")
	}
	now := time.Now().Unix() // the chain's clock, not the store's
	window := min(max(choice.TimeoutSecs, x402WindowMin), x402WindowMax)
	auth := TransferAuthorization{From: cfg.Signer.Address(), To: choice.PayTo, Value: choice.Amount, ValidAfter: now - x402ValidAfterSkew, ValidBefore: now + window}
	nonce, raw, err := randomNonce()
	if err != nil {
		return x402Response{}, nil, refusal("upstream_failed")
	}
	auth.Nonce = raw
	if err = pay.reserve(ctx, p, reservation{amount: auth.Value, network: cfg.Network, asset: cfg.Asset.String(), payTo: auth.To.String(), nonce: nonce, validBefore: auth.ValidBefore}); err != nil {
		return x402Response{}, nil, err
	}
	domain := EIP712Domain{Name: cfg.AssetName, Version: cfg.AssetVersion, ChainID: cfg.ChainID, VerifyingContract: cfg.Asset}
	sig, err := cfg.Signer.SignAuthorization(domain, auth)
	var name, value string
	if err == nil {
		name, value, err = x402PaymentHeader(required, choice, auth, sig)
	}
	if err != nil {
		pay.finish("failed", 0, 0, "", "signing")
		return x402Response{}, nil, refusal("upstream_failed")
	}
	paid, err := pay.fetch(ctx, p, http.Header{name: {value}}, p.res.MaxResponseBytes)
	if err != nil {
		// The upstream holds a valid authorization and may have settled it:
		// the outcome is unknown, the caps keep counting it, the agent is
		// refunded.
		pay.finish("unknown", 0, 0, "", "no_response")
		return x402Response{}, nil, refusal("upstream_failed")
	}
	settled := parseX402Settlement(paid.header.Get(x402V2Response), paid.header.Get(x402V1Response))
	switch {
	case paid.status == http.StatusPaymentRequired:
		pay.finish("rejected", paid.status, len(paid.body), settled.Transaction, settled.Reason)
		return x402Response{}, nil, refusal("x402_payment_rejected")
	case paid.status < 200 || paid.status >= 300:
		pay.finish("unknown", paid.status, len(paid.body), settled.Transaction, "http_status")
		return x402Response{}, nil, refusal("upstream_failed")
	}
	return paid, &x402Receipt{
		Amount: strconv.FormatInt(choice.Amount, 10), Price: formatUnits(choice.Amount, cfg.Decimals), Asset: cfg.Asset.String(),
		Network: cfg.Network, PayTo: choice.PayTo.String(), Payer: auth.From.String(), Nonce: nonce,
		Transaction: settled.Transaction, X402Version: choice.Version,
	}, nil
}

// reserve records a payment before it is committed, in one statement that
// inserts nothing when the amount would exceed the agent's or the global
// daily cap or, for an open resource, the open catalogue's or its
// recipient's daily sub-cap. A "failed" row (never signed, or a definite
// no-charge) counts in none of them (security review of the aggregator,
// M1). A second payment for the same call is refused by
// UNIQUE(account, request_key). An open resource's row carries
// allowlist_version 0.
func (x *x402) reserve(ctx context.Context, c Call, p x402Plan, r reservation) error {
	if r.amount <= 0 || r.amount > x.cfg.PerCall || r.amount > p.max {
		return refusal("x402_price_changed")
	}
	day := c.Now / 86400
	version, open, openDaily, recipientDaily := x.cfg.AllowlistVersion, 0, int64(0), int64(0)
	if p.res.Open {
		version, open = 0, 1
		if cc := x.cfg.Catalogue; cc != nil {
			openDaily, recipientDaily = cc.OpenDaily, cc.RecipientDaily
		}
	}
	res, err := x.db.ExecContext(ctx, `INSERT INTO x402_payments(id,account,request_key,resource,day,amount,network,asset,pay_to,nonce,valid_before,state,allowlist_version,created_at)
SELECT ?,?,?,?,?,?,?,?,?,?,?,'signed',?,?
WHERE (SELECT COALESCE(SUM(amount),0) FROM x402_payments WHERE day=? AND state<>'failed') + ? <= ?
  AND (SELECT COALESCE(SUM(amount),0) FROM x402_payments WHERE day=? AND account=? AND state<>'failed') + ? <= ?
  AND (? = 0 OR (SELECT COALESCE(SUM(amount),0) FROM x402_payments WHERE day=? AND allowlist_version=0 AND state<>'failed') + ? <= ?)
  AND (? = 0 OR (SELECT COALESCE(SUM(amount),0) FROM x402_payments WHERE day=? AND allowlist_version=0 AND pay_to=? AND state<>'failed') + ? <= ?)
  AND NOT EXISTS (SELECT 1 FROM x402_payments WHERE account=? AND request_key=?)`,
		r.id, c.Subject.ID, c.RequestKey, p.res.ID, day, r.amount, r.network, r.asset, r.payTo, r.nonce, r.validBefore, version, c.Now,
		day, r.amount, x.cfg.GlobalDaily, day, c.Subject.ID, r.amount, x.cfg.AgentDaily,
		open, day, r.amount, openDaily, open, day, r.payTo, r.amount, recipientDaily,
		c.Subject.ID, c.RequestKey)
	if err != nil {
		return refusal("upstream_failed") // fail closed: no row, no signature
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		var exists int
		if x.db.QueryRowContext(ctx, "SELECT count(*) FROM x402_payments WHERE account=? AND request_key=?", c.Subject.ID, c.RequestKey).Scan(&exists) == nil && exists > 0 {
			return refusal("upstream_failed") // never a second payment for one call
		}
		return &allowance.Err{Code: "x402_cap_reached", RetryAfter: int(86400 - c.Now%86400)}
	}
	return nil
}

// finishPayment records what happened after signing. It runs even when the
// call's context has ended, briefly; a failure leaves the row "signed", which
// the caps count exactly the same.
func (x *x402) finishPayment(id, state string, status, bytes int, tx, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = x.db.ExecContext(ctx, "UPDATE x402_payments SET state=?, http_status=?, response_bytes=?, transaction_hash=?, reason=?, finished_at=? WHERE id=? AND state='signed'",
		state, status, bytes, tx, reason, time.Now().Unix(), id)
}

type x402Receipt struct {
	Amount      string `json:"amount"` // atomic units
	Price       string `json:"price"`  // the same, in whole tokens
	Asset       string `json:"asset"`
	Network     string `json:"network"`
	PayTo       string `json:"pay_to"`
	Payer       string `json:"payer"`
	Nonce       string `json:"nonce"`
	Transaction string `json:"transaction,omitempty"`
	X402Version int    `json:"x402_version,omitempty"` // x402 only
}

type x402Out struct {
	Resource string `json:"resource"`
	Bundler  string `json:"bundler"`
	// AllowlistVersion is the allowlist's version for a pinned resource, 0
	// for an open one.
	AllowlistVersion int64           `json:"allowlist_version"`
	Status           int             `json:"status"`
	ContentType      string          `json:"content_type,omitempty"`
	Encoding         string          `json:"encoding"` // json, text or base64
	Body             json.RawMessage `json:"body"`
	Bytes            int             `json:"bytes"`
	Payment          *x402Receipt    `json:"payment,omitempty"`
	// Failure is why an "unanswered" call got no answer: upstream_failed or
	// x402_payment_rejected.
	Failure string `json:"failure,omitempty"`
	// TextIsUntrusted is always true: the body is an upstream's, data to
	// read, never instructions to follow.
	TextIsUntrusted bool `json:"text_is_untrusted"`
}

// out is the answer's wrapper for p's resource.
func (x *x402) out(p x402Plan, status int, receipt *x402Receipt) x402Out {
	version := x.cfg.AllowlistVersion
	if p.res.Open {
		version = 0
	}
	return x402Out{Resource: p.res.ID, Bundler: p.res.Bundler, AllowlistVersion: version, Status: status, Payment: receipt, TextIsUntrusted: true}
}

var mimeTypeRE = regexp.MustCompile(`^[a-z0-9!#$&^_.+-]{1,64}/[a-z0-9!#$&^_.+-]{1,64}$`)

// result is the agent's answer: the upstream's bytes as data (JSON inline
// when it is JSON, text when it is UTF-8 text, else base64), never rendered.
func (x *x402) result(c Call, p x402Plan, r x402Response, receipt *x402Receipt) (Result, error) {
	out := x.out(p, r.status, receipt)
	out.Bytes = len(r.body)
	mt, _, _ := mime.ParseMediaType(r.contentType)
	mt = strings.ToLower(mt)
	if mimeTypeRE.MatchString(mt) {
		out.ContentType = mt
	}
	// No HTML escaping (security review 1.20, M2): '<', '>' and '&' stay one
	// byte, so a JSON answer never grows here, and text that escaping would
	// grow past base64 goes as base64. The body is then at most 4/3 of the
	// response limit plus the wrapper, inside X402StoredBodyBytes.
	var compact bytes.Buffer
	switch {
	case len(bytes.TrimSpace(r.body)) > 0 && json.Valid(r.body) && utf8.Valid(r.body) && json.Compact(&compact, r.body) == nil:
		out.Encoding, out.Body = "json", compact.Bytes()
	case utf8.Valid(r.body) && (strings.HasPrefix(mt, "text/") || mt == ""):
		out.Encoding, out.Body = "text", marshalNoEscape(string(r.body))
		if len(out.Body) > base64.StdEncoding.EncodedLen(len(r.body))+2 {
			out.Encoding, out.Body = "base64", marshalNoEscape(base64.StdEncoding.EncodeToString(r.body))
		}
	default:
		out.Encoding, out.Body = "base64", marshalNoEscape(base64.StdEncoding.EncodeToString(r.body))
	}
	body := marshalNoEscape(out)
	if body == nil || len(body) > X402StoredBodyBytes {
		return Result{}, refusal("x402_response_too_large")
	}
	amount := int64(0)
	public := map[string]any{"resource": p.res.ID, "http_status": r.status, "response_bytes": len(r.body)}
	if receipt != nil {
		amount, _ = strconv.ParseInt(receipt.Amount, 10, 64)
		public["amount"] = receipt.Amount // not the transaction: it would tie the agent to our wallet's history
	}
	pub, _ := json.Marshal(public)
	return Result{Body: body, Used: c.Price.For(amount), Public: pub}, nil
}

// marshalNoEscape is json.Marshal without HTML escaping; nil on error.
func marshalNoEscape(v any) []byte {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		return nil
	}
	return bytes.TrimSuffix(b.Bytes(), []byte{'\n'})
}

// discarded is the answer to a paid call whose response went over the
// resource's limit: no body, the payment receipt, and the call's price.
func (x *x402) discarded(c Call, p x402Plan, status, n int, receipt *x402Receipt) Result {
	out := x.out(p, status, receipt)
	out.Encoding, out.Body, out.Bytes = "discarded", json.RawMessage("null"), n
	amount, _ := strconv.ParseInt(receipt.Amount, 10, 64)
	pub, _ := json.Marshal(map[string]any{"resource": p.res.ID, "http_status": status, "response_bytes": n, "amount": receipt.Amount, "discarded": true})
	return Result{Body: marshalNoEscape(out), Used: c.Price.For(amount), Public: pub}
}

// unanswered is the answer to a call on a vetted open resource whose
// signed payment left our hands and got no answer (the upstream failed
// after it, or asked again with 402): no body, the payment as sent, and the
// call's price, since the upstream can settle it.
func (x *x402) unanswered(c Call, p x402Plan, pay *payment, code string) Result {
	r := pay.r
	out := x.out(p, pay.status, &x402Receipt{Amount: strconv.FormatInt(r.amount, 10), Price: formatUnits(r.amount, x.cfg.Decimals), Asset: r.asset,
		Network: r.network, PayTo: r.payTo, Payer: x.cfg.Signer.Address().String(), Nonce: r.nonce})
	out.Encoding, out.Body, out.Failure = "unanswered", json.RawMessage("null"), code
	pub, _ := json.Marshal(map[string]any{"resource": p.res.ID, "http_status": pay.status, "amount": strconv.FormatInt(r.amount, 10), "unanswered": true})
	return Result{Body: marshalNoEscape(out), Used: c.Price.For(r.amount), Public: pub}
}

// Read serves "resources": a page of the catalogue that matches the search,
// best first, with maximum prices in USD and in credits; the categories; the
// caps; and what is left of today's budget.
func (x *x402) Read(ctx context.Context, q allowance.Querier, c Call) (json.RawMessage, error) {
	if x.cfg == nil {
		return nil, refusal("service_unavailable")
	}
	s, err := parseX402Search(c.Args, x.cfg.Decimals)
	if err != nil {
		return nil, err
	}
	price := DefaultPrices()["x402.call"]
	if c.Prices != nil {
		price = c.Prices["x402.call"]
	}
	day := c.Now / 86400
	var globalSpent, yours, openSpent int64
	if err := q.QueryRowContext(ctx, "SELECT COALESCE(SUM(amount),0), COALESCE(SUM(CASE WHEN account=? THEN amount ELSE 0 END),0), COALESCE(SUM(CASE WHEN allowlist_version=0 THEN amount ELSE 0 END),0) FROM x402_payments WHERE day=? AND state<>'failed'", c.Subject.ID, day).Scan(&globalSpent, &yours, &openSpent); err != nil {
		return nil, err
	}
	page := x.search(s)
	resources := []any{}
	for _, r := range page.resources {
		m := min(r.MaxAmount, x.cfg.PerCall)
		query := r.Query
		if query == nil {
			query = []string{}
		}
		entry := map[string]any{
			"id": r.ID, "bundler": r.Bundler, "category": r.Category, "summary": r.servedSummary(), "method": r.Method, "query": query, "body": r.Body,
			"max_price": formatUnits(m, x.cfg.Decimals), "max_cost": price.For(m), "max_response_bytes": r.MaxResponseBytes, "pinned": !r.Open,
			"vetted": !r.Open || r.Vetted, "callable": x.callable(r),
		}
		if r.Open {
			entry["text_is_untrusted"], entry["summary_status"] = true, r.summaryStatus
		}
		if st, ok := page.stats[r.ID]; ok {
			entry["calls_30d"], entry["failed_30d"] = st.ok, st.failed
		}
		resources = append(resources, entry)
	}
	cu := func(n int64) string { return formatUnits(n, x.cfg.Decimals) }
	today := map[string]any{"global_spent": cu(globalSpent), "global_remaining": cu(max(0, x.cfg.GlobalDaily-globalSpent))}
	if c.Subject.Signed {
		today["your_spent"], today["your_remaining"] = cu(yours), cu(max(0, x.cfg.AgentDaily-yours))
	}
	caps := map[string]any{"per_call": cu(x.cfg.PerCall), "agent_daily": cu(x.cfg.AgentDaily), "global_daily": cu(x.cfg.GlobalDaily)}
	catalogue := map[string]any{"pinned": len(x.cfg.Resources), "open": page.open, "imported_at": page.importedAt, "enabled": x.cfg.Catalogue != nil}
	if cc := x.cfg.Catalogue; cc != nil {
		caps["open_daily"], caps["recipient_daily"], caps["open_max_price"] = cu(cc.OpenDaily), cu(cc.RecipientDaily), cu(cc.MaxPrice)
		if a := cc.AutoVet; a != nil {
			catalogue["auto_vet"] = a.Describe(x.cfg.Decimals)
		}
		today["open_spent"], today["open_remaining"] = cu(openSpent), cu(max(0, cc.OpenDaily-openSpent))
	}
	body := map[string]any{
		"allowlist_version": x.cfg.AllowlistVersion, "network": x.cfg.Network, "asset": x.cfg.Asset.String(), "asset_name": x.cfg.AssetName,
		"paused": x.paused(), "price": price, "resources": resources, "matched": page.matched, "categories": page.categories,
		"bundlers": x.readyBundlers(), "catalogue": catalogue, "today": today, "caps": caps,
		"vetting": X402VettingNote,
		// Open resources' summaries are upstream text (each is marked too).
		"text_is_untrusted": true,
	}
	if page.next != "" {
		body["next_cursor"] = page.next
	}
	return marshalNoEscape(body), nil
}

// readyBundlers are the names of the bundlers that can take a call now.
func (x *x402) readyBundlers() []string {
	out := []string{}
	for name, b := range x.bundlers {
		if b.Ready() {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// FormatUnits is formatUnits, for pages that show relay amounts.
func FormatUnits(n int64, decimals int) string { return formatUnits(n, decimals) }

// formatUnits writes atomic units as a decimal in whole tokens: 1500 with 6
// decimals is "0.0015".
func formatUnits(n int64, decimals int) string {
	if n < 0 {
		return "-" + formatUnits(-n, decimals)
	}
	s := strconv.FormatInt(n, 10)
	if decimals == 0 {
		return s
	}
	if len(s) <= decimals {
		s = strings.Repeat("0", decimals-len(s)+1) + s
	}
	whole, frac := s[:len(s)-decimals], strings.TrimRight(s[len(s)-decimals:], "0")
	if frac == "" {
		return whole
	}
	return whole + "." + frac
}

var decimalRE = regexp.MustCompile(`^(0|[1-9][0-9]{0,6})(\.[0-9]{1,18})?$`)

// parseUnits reads a decimal amount of whole tokens ("0.05") into atomic
// units, refusing more fraction digits than the asset has.
func parseUnits(s string, decimals int) (int64, bool) {
	if !decimalRE.MatchString(s) {
		return 0, false
	}
	whole, frac, _ := strings.Cut(s, ".")
	if len(frac) > decimals {
		return 0, false
	}
	n, err := strconv.ParseInt(whole+frac+strings.Repeat("0", decimals-len(frac)), 10, 64)
	return n, err == nil
}

// Operator files.

type x402ConfigFile struct {
	Schema        json.Number `json:"schema"`
	Enabled       bool        `json:"enabled"`
	Paused        bool        `json:"paused"`
	KillFile      string      `json:"kill_file"`
	WalletKeyFile string      `json:"wallet_key_file"`
	Network       string      `json:"network"`
	Asset         string      `json:"asset"`
	AssetName     string      `json:"asset_name"`
	AssetVersion  string      `json:"asset_version"`
	Decimals      json.Number `json:"decimals"`
	Caps          struct {
		GlobalDaily string `json:"global_daily"`
		AgentDaily  string `json:"agent_daily"`
		PerCall     string `json:"per_call"`
	} `json:"caps"`
	AllowlistFile string `json:"allowlist_file"`
	// Catalogue enables the open catalogue (x402_catalogue.go).
	Catalogue *x402CatalogueFile `json:"catalogue"`
	// Bundlers configures the key-based bundlers (frames.go).
	Bundlers *x402BundlersFile `json:"bundlers"`
}

type x402AllowlistFile struct {
	Schema    json.Number `json:"schema"`
	Version   json.Number `json:"version"`
	Resources []struct {
		ID               string      `json:"id"`
		URL              string      `json:"url"`
		Method           string      `json:"method"`
		PayTo            string      `json:"pay_to"`
		MaxPrice         string      `json:"max_price"`
		Query            []string    `json:"query"`
		Body             bool        `json:"body"`
		MaxResponseBytes json.Number `json:"max_response_bytes"`
		TimeoutSeconds   json.Number `json:"timeout_seconds"`
		Summary          string      `json:"summary"`
		Source           string      `json:"source"`
		Category         string      `json:"category"`
		// Bundler and Tool route an entry to a key-based bundler: the tool
		// is the bundler's own id for it, and the bundler fixes the URL.
		Bundler string `json:"bundler"`
		Tool    string `json:"tool"`
	} `json:"resources"`
	// Deny is what the open catalogue never imports (x402_catalogue.go).
	Deny *x402DenyFile `json:"deny"`
}

var (
	x402IDRE        = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	x402ParamRE     = regexp.MustCompile(`^[A-Za-z0-9_.\-\[\]]{1,64}$`)
	x402NetworkRE   = regexp.MustCompile(`^eip155:([1-9][0-9]{0,11})$`)
	x402TokenNameRE = regexp.MustCompile(`^[A-Za-z0-9 ._()-]{1,64}$`)
	x402ToolRE      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@-]{0,127}$`)
	x402CategoryRE  = regexp.MustCompile(`^[a-z]{1,24}$`)
)

// ErrX402Disabled is LoadX402Config's answer for a config with "enabled"
// false: x402 stays off.
var ErrX402Disabled = errors.New("x402: the config says enabled: false")

// LoadX402Config reads the operator's config file, the allowlist it names and
// the wallet key it names, and validates all three. Errors name the field,
// never a secret.
func LoadX402Config(path string) (*X402Config, error) {
	if path == "" {
		return nil, errors.New("x402: X402_CONFIG is not set")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("x402: read config: %w", err)
	}
	var f x402ConfigFile
	if err := StrictObject(raw, &f); err != nil {
		return nil, errors.New("x402: config is not strict JSON with only the documented fields")
	}
	if !f.Enabled {
		return nil, ErrX402Disabled
	}
	list, err := os.ReadFile(f.AllowlistFile)
	if err != nil {
		return nil, fmt.Errorf("x402: read allowlist_file: %w", err)
	}
	if f.WalletKeyFile == "" {
		return nil, errors.New("x402: wallet_key_file is required")
	}
	signer, err := LoadX402Signer(f.WalletKeyFile)
	if err != nil {
		return nil, err
	}
	return parseX402Config(f, list, signer, os.ReadFile, readPrivateFile)
}

// errKeyMode is a bundler key file that group or others can read.
var errKeyMode = errors.New("readable by others")

// readPrivateFile reads a key file that must be a regular file only its
// owner can read, like the wallet key (security review of the aggregator,
// L6). A missing file is fs.ErrNotExist.
func readPrivateFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, errKeyMode
	}
	if info.Size() > x402FileBytesMax {
		return nil, errors.New("key file too large")
	}
	return os.ReadFile(path)
}

// ParseX402Config validates a config and allowlist already read (tests and
// the operator's check command). It reads no file: a bundler's key file and
// the denylist's domains file read as absent.
func ParseX402Config(config, allowlist []byte, signer X402Signer) (*X402Config, error) {
	var f x402ConfigFile
	if err := StrictObject(config, &f); err != nil {
		return nil, errors.New("x402: config is not strict JSON with only the documented fields")
	}
	absent := func(string) ([]byte, error) { return nil, fs.ErrNotExist }
	return parseX402Config(f, allowlist, signer, absent, absent)
}

// x402FileBytesMax bounds a file the config names besides the allowlist (a
// bundler key, the denylist's domains file).
const x402FileBytesMax = 1 << 20

func parseX402Config(f x402ConfigFile, allowlist []byte, signer X402Signer, readFile, readKey func(string) ([]byte, error)) (*X402Config, error) {
	bad := func(field string) error { return fmt.Errorf("x402: config field %s is missing or invalid", field) }
	if f.Schema.String() != "1" {
		return nil, bad("schema (must be 1)")
	}
	c := &X402Config{Paused: f.Paused, KillFile: f.KillFile, Signer: signer, AssetName: f.AssetName, AssetVersion: f.AssetVersion}
	m := x402NetworkRE.FindStringSubmatch(f.Network)
	if m == nil {
		return nil, bad(`network (CAIP-2 "eip155:CHAIN_ID")`)
	}
	c.Network = f.Network
	c.ChainID, _ = strconv.ParseInt(m[1], 10, 64)
	var ok bool
	if c.Asset, ok = ParseEVMAddress(f.Asset); !ok {
		return nil, bad("asset")
	}
	if !x402TokenNameRE.MatchString(f.AssetName) {
		return nil, bad("asset_name")
	}
	if !x402TokenNameRE.MatchString(f.AssetVersion) {
		return nil, bad("asset_version")
	}
	d, err := strconv.Atoi(f.Decimals.String())
	if err != nil || d < 0 || d > 12 {
		return nil, bad("decimals (0 to 12)")
	}
	// USDC has 6 decimals on every chain: a typo would scale every compiled
	// ceiling by a power of ten (security review 1.20, L1).
	if f.AssetName == "USD Coin" && d != 6 {
		return nil, bad("decimals (USD Coin has 6)")
	}
	c.Decimals = d
	unit := int64(1)
	for range d {
		unit *= 10
	}
	caps := []struct {
		text, name, def string
		dst             *int64
	}{
		{f.Caps.GlobalDaily, "caps.global_daily", "2", &c.GlobalDaily},
		{f.Caps.AgentDaily, "caps.agent_daily", "0.25", &c.AgentDaily},
		{f.Caps.PerCall, "caps.per_call", "0.05", &c.PerCall},
	}
	for _, cp := range caps {
		text := cp.text
		if text == "" {
			text = cp.def
		}
		n, ok := parseUnits(text, d)
		if !ok || n <= 0 {
			return nil, bad(cp.name)
		}
		*cp.dst = n
	}
	if c.GlobalDaily > X402GlobalDailyCeiling*unit {
		return nil, fmt.Errorf("x402: caps.global_daily is above the compiled ceiling of %d", X402GlobalDailyCeiling)
	}
	if c.PerCall > X402PerCallCeiling*unit {
		return nil, fmt.Errorf("x402: caps.per_call is above the compiled ceiling of %d", X402PerCallCeiling)
	}
	if c.AgentDaily > c.GlobalDaily || c.PerCall > c.AgentDaily {
		return nil, errors.New("x402: caps must satisfy per_call <= agent_daily <= global_daily")
	}
	if signer == nil {
		return nil, bad("wallet_key_file")
	}
	if err := c.parseBundlers(f.Bundlers, readKey); err != nil {
		return nil, err
	}
	if err := c.parseAllowlist(allowlist, readFile); err != nil {
		return nil, err
	}
	if err := c.parseCatalogue(f.Catalogue); err != nil {
		return nil, err
	}
	return c, nil
}

// bundler is the configured bundler named name ("" is x402), or nil.
func (c *X402Config) bundler(name string) Bundler {
	for _, b := range c.Bundlers {
		if b.Name() == name {
			return b
		}
	}
	return nil
}

func (c *X402Config) parseAllowlist(raw []byte, readFile func(string) ([]byte, error)) error {
	var f x402AllowlistFile
	if err := StrictObject(raw, &f); err != nil {
		return errors.New("x402: allowlist is not strict JSON with only the documented fields")
	}
	if f.Schema.String() != "1" {
		return errors.New("x402: allowlist schema must be 1")
	}
	v, err := strconv.ParseInt(f.Version.String(), 10, 64)
	if err != nil || v < 1 {
		return errors.New("x402: allowlist version must be a positive integer; raise it on every change")
	}
	c.AllowlistVersion = v
	if len(f.Resources) > X402ResourcesMax {
		return fmt.Errorf("x402: at most %d allowlisted resources", X402ResourcesMax)
	}
	seen := map[string]bool{}
	for i, r := range f.Resources {
		bad := func(field string) error {
			return fmt.Errorf("x402: allowlist resource %d (%q): %s is missing or invalid", i, r.ID, field)
		}
		if !x402IDRE.MatchString(r.ID) || seen[r.ID] {
			return bad("id (unique, lowercase letters, digits, . _ -)")
		}
		seen[r.ID] = true
		var payTo EVMAddress
		switch r.Bundler {
		case "", X402Bundler:
			r.Bundler = X402Bundler
			if r.Tool != "" {
				return bad("tool (only for a key-based bundler)")
			}
			var ok bool
			if payTo, ok = ParseEVMAddress(r.PayTo); !ok || payTo == (EVMAddress{}) {
				return bad("pay_to")
			}
		default:
			// A key-based bundler's tool: the bundler fixes the endpoint and
			// the method, and the tool's arguments are the JSON body.
			tb, ok := c.bundler(r.Bundler).(toolBundler)
			if !ok {
				return bad("bundler (not configured)")
			}
			if !x402ToolRE.MatchString(r.Tool) || r.URL != "" || r.PayTo != "" || (r.Method != "" && r.Method != http.MethodPost) || len(r.Query) > 0 {
				return bad("tool (with no url, pay_to or query; method POST)")
			}
			r.URL, r.Method, r.Body = tb.Endpoint(), http.MethodPost, true
		}
		if err := checkX402URL(r.URL); err != nil {
			return bad("url (" + err.Error() + ")")
		}
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			return bad("method (GET or POST)")
		}
		if r.Body && r.Method != http.MethodPost {
			return bad("body (only for POST)")
		}
		if r.Category != "" && !x402CategoryRE.MatchString(r.Category) {
			return bad("category (lowercase letters, 1 to 24)")
		}
		amount, ok := parseUnits(r.MaxPrice, c.Decimals)
		if !ok || amount <= 0 || amount > c.PerCall {
			return bad("max_price (positive, at most caps.per_call)")
		}
		if len(r.Query) > X402QueryParamsMax {
			return bad("query (too many names)")
		}
		for _, name := range r.Query {
			if !x402ParamRE.MatchString(name) {
				return bad("query")
			}
		}
		res := X402Resource{ID: r.ID, URL: r.URL, Method: r.Method, PayTo: payTo, MaxAmount: amount, Query: slices.Clone(r.Query), Body: r.Body,
			MaxResponseBytes: X402ResponseBytesDefault, Timeout: X402TimeoutDefault, Summary: r.Summary, Source: r.Source,
			Bundler: r.Bundler, Tool: r.Tool, Category: r.Category}
		if res.Category == "" {
			res.Category = x402Categorize(r.URL + " " + r.ID + " " + r.Summary)
		}
		if r.MaxResponseBytes != "" {
			n, err := strconv.Atoi(r.MaxResponseBytes.String())
			if err != nil || n < 1 || n > X402ResponseBytesMax {
				return bad(fmt.Sprintf("max_response_bytes (1 to %d)", X402ResponseBytesMax))
			}
			res.MaxResponseBytes = n
		}
		if r.TimeoutSeconds != "" {
			n, err := strconv.Atoi(r.TimeoutSeconds.String())
			if err != nil || n < 1 || time.Duration(n)*time.Second > X402TimeoutMax {
				return bad("timeout_seconds (1 to 20)")
			}
			res.Timeout = time.Duration(n) * time.Second
		}
		if len(r.Summary) > 280 || !utf8.ValidString(r.Summary) || len(r.Source) > 200 || !utf8.ValidString(r.Source) {
			return bad("summary or source (too long)")
		}
		c.Resources = append(c.Resources, res)
	}
	return c.parseDeny(f.Deny, readFile)
}

// checkX402URL admits only a plain public HTTPS endpoint on port 443: no
// credentials, no fragment, no IP literal (the dialer still checks every
// address it connects to).
func checkX402URL(raw string) error {
	if raw == "" || len(raw) > X402URLBytes/2 || !utf8.ValidString(raw) || strings.ContainsAny(raw, " \t\r\n") {
		return errors.New("empty, long or with spaces")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || u.Opaque != "" || u.Hostname() == "" {
		return errors.New("must be https://host/path")
	}
	if port := u.Port(); port != "" && port != "443" {
		return errors.New("port must be 443")
	}
	if net.ParseIP(u.Hostname()) != nil || !strings.Contains(u.Hostname(), ".") {
		return errors.New("host must be a public DNS name")
	}
	return nil
}
