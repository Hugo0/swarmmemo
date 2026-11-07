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
// the price of what was actually paid, when it gets the response; a
// rejected, failed or timed-out call is refunded in full and its loss, if
// any, is ours, bounded by the caps. An answer longer than the call's
// max_bytes is returned cut (truncated: true, with the whole answer's size
// and sha256), never discarded (C578); max_bytes above X402ResponseBytesMax
// costs X402ExtraKiBCredits per KiB returned over it. An account with
// X402DiscardsPerDay discarded answers today (a paid answer that cannot be
// returned at all) is refused before any request until 00:00 UTC.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
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
	"swarmmemo/internal/reviews"
)

// x402UserAgent names every request to a paid seller and its discovery
// documents: many sit behind Cloudflare, which refuses a library default.
const x402UserAgent = "SwarmMemo-x402/1 (+https://swarmmemo.com/protocol.md)"

// x402 bounds.
const (
	X402ArgsMax              = 4 << 10  // the published service_args_bytes
	X402ResponseBytesMax     = 12 << 10 // base64 of it and the wrapper fit X402StoredBodyBytes
	X402ResponseBytesDefault = 8 << 10
	// X402ResponseBytesCeiling is the most a call's max_bytes may ask for,
	// and the most of any catalogue answer read into memory (plus one byte,
	// to know it was longer): a hostile endless body is cut here (C578).
	X402ResponseBytesCeiling = 64 << 10
	// X402BundlerReadBytes is the most of a bundler tool's answer read: its
	// envelope (billing, receipts) is parsed and scrubbed whole before the
	// answer is cut to max_bytes.
	X402BundlerReadBytes = 256 << 10
	// X402ExtraKiBCredits is the price of each KiB returned over
	// X402ResponseBytesMax, when max_bytes asks for more (fetch's per-KiB
	// rate).
	X402ExtraKiBCredits = 1
	// X402StoredBodyBytes bounds an x402 call record: the response as base64
	// (4/3 of X402ResponseBytesCeiling), the receipt and the wrapper.
	X402StoredBodyBytes = StoredBodyCeilingBytes
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
	// Sellers are the allowlist file's sellers: each host's declared
	// pricing_url, its own x402 price list (pricing.go).
	Sellers map[string]string
	// rootCAs replaces the system roots, and testDial the board's dialer
	// (tests only: UseTestUpstream).
	rootCAs  *x509.CertPool
	testDial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// UseTestUpstream sends every upstream request of the relay built from c to
// dial, trusting roots for its TLS: an httptest TLS server stands in for
// every upstream host. Tests in other packages only; nothing in a config
// file or the environment sets it.
func (c *X402Config) UseTestUpstream(roots *x509.CertPool, dial func(ctx context.Context, network, addr string) (net.Conn, error)) {
	c.rootCAs, c.testDial = roots, dial
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
	// dynamic marks a tool of the open bundler catalogue (keybundler_open.go),
	// built for one call; bundlerSearch is the bundler search that found it.
	dynamic       bool
	bundlerSearch string
	// Derived once by indexResource: the canonical URL, the host, its
	// registrable domain and the lower-cased search text.
	canon, host, domain, text string
}

// X402Bundler is the name of the x402 rail, the default bundler.
const X402Bundler = "x402"

// Kinds of catalogue entry, as the resources read and Find name them: an
// x402 resource the relay may call, or an MPP directory entry it lists.
const (
	KindX402 = "x402"
	KindMPP  = "mpp"
)

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
	// fr is the open bundler catalogue (keybundler_open.go); nil while off.
	fr *bundlerState
	// reviews summarises each resource's subject (Deps.Reviews); nil while
	// REVIEWS is off.
	reviews ReviewSummarizer
	// late reads back the payments whose settlement the seller never
	// confirmed (x402_reconcile.go).
	late lateSettlements
}

func newX402(d Deps) Provider {
	x := &x402{}
	if d.X402 == nil || d.DB == nil || d.Dial == nil || d.X402.Signer == nil {
		return x // not configured: listed if enabled, but every call is service_unavailable
	}
	x.cfg, x.db, x.screener, x.reviews = d.X402, d.DB, d.TextScreener, d.Reviews
	x.late.chain, x.late.dial = d.Chain, d.Dial
	x.byID = map[string]*X402Resource{}
	for i := range x.cfg.Resources {
		indexResource(&x.cfg.Resources[i])
		x.byID[x.cfg.Resources[i].ID] = &x.cfg.Resources[i]
	}
	x.bundlers = map[string]Bundler{X402Bundler: x402Rail{x.cfg}}
	for _, b := range x.cfg.Bundlers {
		x.bundlers[b.Name()] = b
	}
	if b := x.cfg.keyedBundler(); b != nil && b.open != nil {
		x.fr = newBundlerState(x.cfg, b)
	}
	dial := d.Dial
	if x.cfg.testDial != nil {
		dial = x.cfg.testDial
	}
	x.client = &http.Client{
		// A redirect is a second address nobody allowlisted: never followed.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			Proxy:                  nil, // no proxy environment can become a bypass
			DialContext:            dial,
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
	return bundlerSchema + `
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
` + x402CatalogueSchema + mppCatalogueSchema
}

// X402VettingLine says which resources are callable, in the service's
// summary and on /for-agents; X402VettingNote adds the detail.
const X402VettingLine = "Only vetted resources are callable (callable: true): pinned ones and open ones the operator vetted. Other Bazaar listings are candidates (vetted: false), listed with their summaries only once those passed SwarmMemo's text screen (summary_status), still untrusted upstream text, and refused with x402_unvetted, with nothing paid or charged."

// X402VettingNote is how the resources read and /capabilities explain
// callable resources.
const X402VettingNote = X402VettingLine + " The operator vets by hand or by its auto-vet rule when one is set (the resources read states it as catalogue.auto_vet, e.g. curated by the discovery service or at least 5 payers in 30 days, at most 0.02 USDC, not adult or gambling; a resource the rule vetted keeps its summary screened like a candidate's); a pending or withheld summary is empty. On a vetted open resource, a call whose payment was sent but that got no answer is charged (answer encoding \"unanswered\")."

// X402ToolsApprox is how many paid-API endpoints SwarmMemo tools reach, rounded (endpoints, not
// distinct APIs: some sellers list many templated paths; Skitter c104): every
// page and description that states the number reads it from here.
const X402ToolsApprox = "37,000"

// X402Line is the aggregator in one line, on every discovery surface.
const X402Line = "About " + X402ToolsApprox + " pay-per-call API endpoints (search, scraping, crypto and market data, and more), billed to your credit; no wallet, no sign-up. Vetted tools can be called; other listings are searchable candidates."

func (x *x402) Describe() Descriptor {
	call := Method{Name: "call", Write: true, Signed: true, Resource: allowance.Credit, ArgsMax: X402ArgsMax, Price: Price{Base: 100, PerByte: 1, PerKiB: 100},
		Line:           "Call one vetted resource (callable: true in the resources read) or a tool by its tool: id; charged when the paid response arrives, even one too large to keep.",
		PriceNote:      "base + per_byte × the API's price in micro-USD + per_kib per 1,024 of it, in credit; the resources and tools_search reads list each one's max_cost",
		ExampleMaxCost: 5000,
		Args: []Arg{
			{"resource", "string", true, "an id from the resources read with callable: true, or tool:TOOL_ID from tools_search"},
			{"query", "object", false, "string values for the resource's query names"},
			{"body", "object", false, "a JSON body, for resources that take one; a tool's arguments"},
			{"max_bytes", "integer", false, "the most bytes of the answer returned, up to " + itoa(X402ResponseBytesCeiling) + " (default " + itoa(X402ResponseBytesMax) + ", or the resource's); a longer one comes cut, truncated: true; 1 credit per KiB over " + itoa(X402ResponseBytesMax)},
		},
		Example: json.RawMessage(`{"resource":"RESOURCE_ID","query":{"q":"agent message boards"}}`)}
	if x.fr != nil && x.fr.cfg.Anonymous {
		// Without a key: bundler tools only (CheckAnonymous).
		call.Anonymous, call.AnonymousLabel = true, "SwarmMemo tools"
		call.AnonymousNote = "tool: resources only, at most " + formatUnits(x.fr.cfg.MaxCost, 6) + " USD a call, fees included"
		call.AnonymousRate = AnonRate{CallerPerMinute: 2, CallerPerDay: 20, AllPerMinute: 10, AllPerDay: 500}
	}
	return Descriptor{
		ID:      "x402",
		Summary: `Pay-per-call APIs without a wallet or an account: SwarmMemo pays the API and charges you credit, the API's price in micro-USD plus a margin. service.read method "resources" searches the catalogue (query, category, max_price) and lists each resource's query names, maximum price and cost, whether it is callable, and today's budget; call one by its id. ` + X402VettingLine + ` ` + BundlerNote,
		Title:   "x402 relay", Topic: "Tools across the internet",
		Line: X402Line,
		Limits: []Limit{
			{"x402_query_params", X402QueryParamsMax, "", "Query parameters of one call"},
			{"x402_query_value_bytes", X402QueryValueBytes, "bytes", "One query value"},
			{"x402_response_bytes", X402ResponseBytesMax, "bytes", "Response body returned"},
			{"x402_max_bytes", X402ResponseBytesCeiling, "bytes", "Response body returned with max_bytes"},
			{"x402_resources_page", X402PageMax, "", "Resources in one resources read"},
			{"tools_search_hits", bundlerHitsMax, "", "Hits in one tools_search read"},
		},
		Mode: Remote,
		Methods: []Method{
			call,
			{Name: "resources", ArgsMax: 512, Line: "Search the catalogue: each resource with its protocols, query names, maximum price and cost, and whether it is vetted and callable (candidates are listed, not callable), best first; and today's budget.",
				Args: []Arg{
					{"query", "string", false, "up to 8 words that must all appear in the resource's id, category, summary or host"},
					{"category", "string", false, "one of the categories the read lists, e.g. search, scraping, crypto"},
					{"protocol", "string", false, "x402 or mpp (kind mpp entries are listed, never callable)"},
					{"max_price", "string", false, `the most one call may cost, in USD, e.g. "0.01"`},
					{"limit", "integer", false, "resources per page, 1 to 50 (default 20)"},
					{"cursor", "string", false, "next_cursor from the previous page"},
				},
				Example: json.RawMessage(`{"query":"web search","max_price":"0.01"}`)},
			{Name: "tools_search", ArgsMax: 1024, Line: "Search SwarmMemo tools (about " + X402ToolsApprox + " paid-API endpoints) by intent: each hit with its tool: id, description, price, input schema, whether it is vetted and whether it is callable; first_party: true marks a tool the bundler sells itself.",
				Args: []Arg{
					{"query", "string", false, "what the tool should do, up to 200 bytes; or queries"},
					{"queries", "array", false, "2 to 4 phrasings of the same need, searched together"},
					{"capability", "string", false, "a capability to filter on, e.g. weather"},
					{"max_price", "string", false, `the most one call may cost, in USD, e.g. "0.01"`},
				},
				Example: json.RawMessage(`{"query":"weather forecast for a city"}`)},
			{Name: "tools_get", ArgsMax: 512, Line: "Read one tool: its live price, input schema, host and whether it is callable now.",
				Args: []Arg{
					{"id", "string", true, "a tool: id from tools_search"},
				},
				Example: json.RawMessage(`{"id":"tool:TOOL_ID"}`)},
		},
		MaxDuration:   x402MaxDuration,
		StoredBodyMax: X402StoredBodyBytes,
	}
}

type x402Args struct {
	Resource string            `json:"resource"`
	Query    map[string]string `json:"query"`
	Body     json.RawMessage   `json:"body"`
	MaxBytes *int              `json:"max_bytes"`
}

// x402ExtraCredits is the price of the bytes of an answer of n bytes over
// X402ResponseBytesMax: X402ExtraKiBCredits per KiB started.
func x402ExtraCredits(n int) int64 {
	if n <= X402ResponseBytesMax {
		return 0
	}
	return X402ExtraKiBCredits * int64((n-X402ResponseBytesMax+1023)/1024)
}

// x402Limit is the most bytes of an answer the call returns: its max_bytes,
// else the resource's limit.
func x402Limit(a x402Args, res *X402Resource) (int, error) {
	if a.MaxBytes == nil {
		return res.MaxResponseBytes, nil
	}
	if n := *a.MaxBytes; n >= 1 && n <= X402ResponseBytesCeiling {
		return n, nil
	}
	return 0, badArg("max_bytes is 1 to " + itoa(X402ResponseBytesCeiling) + ".")
}

// x402Plan is one validated call: the resource and the exact request.
type x402Plan struct {
	res  *X402Resource
	url  string
	body []byte // POST only
	max  int64  // atomic units: min(allowlisted maximum, per-call cap)
	// limit is the most bytes of the answer returned (max_bytes, else the
	// resource's limit); a longer answer is returned cut.
	limit int
}

// quoted is the most the plan can cost: the price of p.max and of the
// bytes p.limit allows over X402ResponseBytesMax.
func (p x402Plan) quoted(price Price) int64 {
	return price.For(p.max) + x402ExtraCredits(p.limit)
}

func (x *x402) plan(c Call) (x402Plan, error) {
	if x.cfg == nil {
		return x402Plan{}, refusal("service_unavailable")
	}
	var a x402Args
	if err := StrictObject(c.Args, &a); err != nil {
		return x402Plan{}, err
	}
	if tool, ok := bundlerResource(a.Resource); ok && x.fr != nil {
		return x.bundlerPlan(c, tool, a)
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
		return x402Plan{}, badArg("query has at most " + itoa(X402QueryParamsMax) + " parameters.")
	}
	for k, v := range a.Query {
		if len(v) > X402QueryValueBytes {
			return x402Plan{}, tooLarge("invalid_service_data", len(v), X402QueryValueBytes)
		}
		if !slices.Contains(res.Query, k) || q.Has(k) || !utf8.ValidString(v) {
			return x402Plan{}, badArg("query takes only the parameters this resource lists (its query field), as UTF-8 text; the resource fixes the others.")
		}
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()
	p := x402Plan{res: res, url: u.String(), max: min(res.MaxAmount, x.cfg.PerCall)}
	if p.limit, err = x402Limit(a, res); err != nil {
		return x402Plan{}, err
	}
	if len(p.url) > X402URLBytes {
		return x402Plan{}, tooLarge("invalid_service_data", len(p.url), X402URLBytes)
	}
	switch {
	case len(a.Body) > 0 && !res.Body:
		return x402Plan{}, badArg("This resource takes no body; send its inputs in query.")
	case len(a.Body) > 0 && !isObject(a.Body):
		return x402Plan{}, badArg("body is a JSON object.")
	case res.Method == http.MethodPost:
		var b bytes.Buffer
		if len(a.Body) == 0 {
			b.WriteString("{}")
		} else if err := json.Compact(&b, a.Body); err != nil {
			return x402Plan{}, badArg("body is a JSON object.")
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
	return Quote{Resource: allowance.Credit, Max: p.quoted(c.Price)}, nil
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
	over        bool // the body was longer than the limit: body is its first limit bytes
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
	req.Header.Set("User-Agent", x402UserAgent)
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
		r.body, r.over = raw[:limit], true
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
	if c.Quoted > 0 && p.quoted(c.Price) > c.Quoted {
		return Result{}, refusal("x402_price_changed")
	}
	// Unvetted or denied since the snapshot: checked again before any money
	// moves.
	if !x.stillCallable(ctx, p.res) {
		return Result{}, refusal("x402_unvetted")
	}
	// A bundler tool: vetted, not denied, live and priced within the caps,
	// asked now, before any money moves; its maximum becomes its quote.
	if p.res.dynamic {
		if err = x.bundlerGate(ctx, c, &p); err != nil {
			return Result{}, err
		}
	}
	// Refused up front, before any request: an account whose paid answers
	// keep coming back too large (it controls their size through the query).
	var discards int
	if err = x.db.QueryRowContext(ctx, "SELECT count(*) FROM x402_payments WHERE day=? AND account=? AND reason='response_too_large'", c.Now/86400, c.Subject.ID).Scan(&discards); err != nil {
		return Result{}, refusal("upstream_failed")
	}
	if discards >= X402DiscardsPerDay {
		return Result{}, allowance.Until("x402_response_too_large", allowance.NextDay(c.Now), c.Now)
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
	case err != nil && x402PricedRefusal(err):
		return Result{}, withPricingURL(err, x.cfg.PricingURL(p.res.URL))
	case err != nil:
		return Result{}, err
	case receipt == nil: // free today, or nothing delivered: no tool part
		return x.result(c, p, resp, nil)
	case resp.over && p.res.dynamic:
		// A bundler envelope past X402BundlerReadBytes cannot be scrubbed
		// (its billing, its receipts), so nothing of it is returned, and the
		// tool part is not charged: the loss is ours, bounded by the caps and
		// by the account's discards today (C578).
		state, reason := receipt.paidAs("response_too_large")
		pay.finish(state, resp.status, len(resp.body)+1, receipt.Transaction, reason)
		return x.discarded(c, p, resp.status, len(resp.body)+1, receipt), nil
	}
	// A longer answer than the call's limit is returned cut, and charged:
	// the caller gets what was paid for, never nothing (C578).
	res, err := x.result(c, p, resp, receipt)
	if err != nil {
		state, reason := receipt.paidAs("response_too_large")
		pay.finish(state, resp.status, len(resp.body), receipt.Transaction, reason)
		return x.discarded(c, p, resp.status, len(resp.body), receipt), nil
	}
	n := len(resp.body)
	if resp.over {
		n++ // at least
	}
	state, reason := receipt.paidAs("")
	pay.finish(state, resp.status, n, receipt.Transaction, reason)
	return res, nil
}

// paidAs is the state and reason a paid call's row ends in: "paid", or
// "unknown" while the seller answered without confirming a successful
// settlement (C534). Such a row keeps counting in the caps and the agent
// stays charged, since the seller can still settle our signature until it
// expires; reconcileLate reads it back after that. Its reason is
// x402ReasonUnconfirmed, or reason when there is one (response_too_large
// keeps counting the account's discards).
func (r *x402Receipt) paidAs(reason string) (string, string) {
	if r.SettlementStatus != x402SettlementUnconfirmed {
		return "paid", reason
	}
	if reason == "" {
		reason = x402ReasonUnconfirmed
	}
	return "unknown", reason
}

// x402Rail is the x402 bundler: it pays each call in USDC from the relay
// wallet, with an EIP-3009 authorization the upstream's facilitator settles.
type x402Rail struct{ cfg *X402Config }

func (x402Rail) Name() string { return X402Bundler }
func (x402Rail) Ready() bool  { return true } // newX402 requires the signer

func (r x402Rail) Exchange(ctx context.Context, p x402Plan, pay *payment) (x402Response, *x402Receipt, error) {
	cfg := r.cfg
	first, err := pay.fetch(ctx, p, nil, max(x402RequiredBodyBytes, X402ResponseBytesCeiling))
	if err != nil {
		return x402Response{}, nil, refusal("upstream_failed")
	}
	switch {
	case first.status >= 200 && first.status < 300: // free today: nothing to pay
		return first, nil, nil
	case first.status != http.StatusPaymentRequired:
		return x402Response{}, nil, refusal("upstream_failed")
	}
	challenge := first.body
	if first.over {
		challenge = nil // a cut challenge is never parsed
	}
	required, ok := parseX402Required(first.header.Get(x402V2Required), challenge)
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
	paid, err := pay.fetch(ctx, p, http.Header{name: {value}}, X402ResponseBytesCeiling)
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
	receipt := &x402Receipt{
		Amount: strconv.FormatInt(choice.Amount, 10), Price: formatUnits(choice.Amount, cfg.Decimals), Asset: cfg.Asset.String(),
		Network: cfg.Network, Settlement: x402SettlementOnchain, PayTo: choice.PayTo.String(), Payer: auth.From.String(), Nonce: nonce,
		Transaction: settled.Transaction, X402Version: choice.Version,
	}
	if !settled.Success {
		// An answer, but no successful settlement reported (success false,
		// or no or a malformed settlement header): the seller still holds a
		// valid signature, so the charge stands until it expires (C534).
		receipt.SettlementStatus, receipt.Note = x402SettlementUnconfirmed, X402UnconfirmedNote
	}
	return paid, receipt, nil
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
	// An open bundler tool's row carries allowlist_version -1 and counts
	// against the bundler sub-caps: all of them a day, and each tool a day.
	openTool, bundlerDaily, toolDaily := 0, int64(0), int64(0)
	if p.res.dynamic {
		if x.fr == nil {
			return refusal("service_unavailable")
		}
		version, openTool, bundlerDaily, toolDaily = -1, 1, x.fr.cfg.OpenDaily, x.fr.cfg.ToolDaily
	}
	res, err := x.db.ExecContext(ctx, `INSERT INTO x402_payments(id,account,request_key,resource,day,amount,network,asset,pay_to,nonce,valid_before,state,allowlist_version,created_at)
SELECT ?,?,?,?,?,?,?,?,?,?,?,'signed',?,?
WHERE (SELECT COALESCE(SUM(amount),0) FROM x402_payments WHERE day=? AND state<>'failed') + ? <= ?
  AND (SELECT COALESCE(SUM(amount),0) FROM x402_payments WHERE day=? AND account=? AND state<>'failed') + ? <= ?
  AND (? = 0 OR (SELECT COALESCE(SUM(amount),0) FROM x402_payments WHERE day=? AND allowlist_version=0 AND state<>'failed') + ? <= ?)
  AND (? = 0 OR (SELECT COALESCE(SUM(amount),0) FROM x402_payments WHERE day=? AND allowlist_version=0 AND pay_to=? AND state<>'failed') + ? <= ?)
  AND (? = 0 OR (SELECT COALESCE(SUM(amount),0) FROM x402_payments WHERE day=? AND allowlist_version=-1 AND state<>'failed') + ? <= ?)
  AND (? = 0 OR (SELECT COALESCE(SUM(amount),0) FROM x402_payments WHERE day=? AND allowlist_version=-1 AND resource=? AND state<>'failed') + ? <= ?)
  AND NOT EXISTS (SELECT 1 FROM x402_payments WHERE account=? AND request_key=?)`,
		r.id, c.Subject.ID, c.RequestKey, p.res.ID, day, r.amount, r.network, r.asset, r.payTo, r.nonce, r.validBefore, version, c.Now,
		day, r.amount, x.cfg.GlobalDaily, day, c.Subject.ID, r.amount, x.cfg.AgentDaily,
		open, day, r.amount, openDaily, open, day, r.payTo, r.amount, recipientDaily,
		openTool, day, r.amount, bundlerDaily, openTool, day, p.res.ID, r.amount, toolDaily,
		c.Subject.ID, c.RequestKey)
	if err != nil {
		return refusal("upstream_failed") // fail closed: no row, no signature
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		var exists int
		if x.db.QueryRowContext(ctx, "SELECT count(*) FROM x402_payments WHERE account=? AND request_key=?", c.Subject.ID, c.RequestKey).Scan(&exists) == nil && exists > 0 {
			return refusal("upstream_failed") // never a second payment for one call
		}
		return allowance.Until("x402_cap_reached", allowance.NextDay(c.Now), c.Now)
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
	Amount string `json:"amount"` // atomic units
	Price  string `json:"price"`  // the same, in whole tokens
	Asset  string `json:"asset"`
	// Network is the chain an on-chain payment is made on (x402); absent
	// when there is no chain leg (C548).
	Network string `json:"network,omitempty"`
	// Settlement is how the payment settles: "onchain" (x402: an EIP-3009
	// authorization the seller settles on Network, its transaction once
	// reported) or "internal" (a key-based bundler billing SwarmMemo's
	// prepaid account: no network, no transaction).
	Settlement  string `json:"settlement"`
	PayTo       string `json:"pay_to"`
	Payer       string `json:"payer"`
	Nonce       string `json:"nonce"`
	Transaction string `json:"transaction,omitempty"`
	X402Version int    `json:"x402_version,omitempty"` // x402 only
	// SettlementStatus is "unconfirmed" when the seller answered without
	// reporting a successful settlement, and Note says what follows.
	SettlementStatus string `json:"settlement_status,omitempty"`
	Note             string `json:"note,omitempty"`
	// Truncated is true when the answer paid for was returned cut (C578).
	Truncated bool `json:"truncated,omitempty"`
}

// x402SettlementUnconfirmed is a receipt's settlement_status when the seller
// answered without reporting a successful settlement; such a payment row is
// "unknown" with reason x402ReasonUnconfirmed until reconcileLate reads it
// back.
const (
	x402SettlementUnconfirmed = "unconfirmed"
	x402ReasonUnconfirmed     = "settlement_unconfirmed"
)

// A receipt's settlement (x402Receipt.Settlement, C548).
const (
	x402SettlementOnchain  = "onchain"
	x402SettlementInternal = "internal"
)

// X402UnconfirmedNote is the receipt's note for an unconfirmed settlement.
const X402UnconfirmedNote = "The API answered but reported no successful settlement of this payment. The charge stands while the signed payment can still be used; if it expires unused on chain, the charge is refunded."

// X402Settlement is what matches a paid relay call to the chain: the
// network, the EIP-3009 authorization's nonce and, once the upstream
// reported it, the settlement transaction. Public on-chain facts only.
type X402Settlement struct {
	Network     string `json:"network"`
	Nonce       string `json:"nonce"`
	Transaction string `json:"transaction,omitempty"`
}

// x402NonceRE is an EIP-3009 nonce as randomNonce writes it.
var x402NonceRE = regexp.MustCompile(`^0x[0-9a-f]{64}$`)

// X402Settlements are the on-chain facts of the relay payments behind the
// given holds (service calls' holds), keyed by hold id. A call through a
// key-based bundler pays nothing of ours on a chain and has none, nor has a
// payment that was never sent. It reads in the caller's transaction.
func X402Settlements(ctx context.Context, q allowance.Querier, holdIDs []string) (map[string]X402Settlement, error) {
	out := map[string]X402Settlement{}
	if len(holdIDs) == 0 {
		return out, nil
	}
	args := make([]any, len(holdIDs))
	for i, id := range holdIDs {
		args[i] = id
	}
	rows, err := q.QueryContext(ctx, `SELECT sc.hold_id, p.network, p.nonce, p.transaction_hash FROM service_calls sc
JOIN x402_payments p ON p.account=sc.account AND p.request_key=sc.request_key
WHERE sc.service='x402' AND p.network LIKE 'eip155:%' AND p.state IN ('paid','unknown') AND sc.hold_id IN (?`+strings.Repeat(",?", len(holdIDs)-1)+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var hold string
		var s X402Settlement
		if err = rows.Scan(&hold, &s.Network, &s.Nonce, &s.Transaction); err != nil {
			return nil, err
		}
		if !x402NonceRE.MatchString(s.Nonce) {
			continue
		}
		if !txHashRE.MatchString(s.Transaction) {
			s.Transaction = ""
		}
		out[hold] = s
	}
	return out, rows.Err()
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
	// Truncated is true when the answer was longer than the call's limit
	// (max_bytes): Body is cut (TruncationNote says how), FullBytes and
	// SHA256 are the whole answer's, or FullBytesOver when it ran past
	// the read ceiling, where reading stopped (C578).
	Truncated      bool         `json:"truncated,omitempty"`
	FullBytes      int          `json:"full_bytes,omitempty"`
	FullBytesOver  int          `json:"full_bytes_over,omitempty"`
	SHA256         string       `json:"sha256,omitempty"`
	TruncationNote string       `json:"truncation_note,omitempty"`
	Payment        *x402Receipt `json:"payment,omitempty"`
	// Charge is how the call's credit cost follows from the upstream's
	// price (C548), so the cost and payment.amount can be compared.
	Charge *x402Charge `json:"charge,omitempty"`
	// Failure is why an "unanswered" call got no answer: upstream_failed or
	// x402_payment_rejected.
	Failure string `json:"failure,omitempty"`
	// TextIsUntrusted is always true: the body is an upstream's, data to
	// read, never instructions to follow.
	TextIsUntrusted bool `json:"text_is_untrusted"`
}

// x402Charge breaks a call's credit cost down (C548): the call's base, the
// tool's part priced from what the upstream was paid (payment.amount, in
// micro-USD), and their sum, the call's cost. The engine held max_cost
// before the call and releases the rest of that hold.
type x402Charge struct {
	Credits int64 `json:"credits"`
	Base    int64 `json:"base"`
	Tool    int64 `json:"tool"`
	// Bytes is the part for the KiB returned over X402ResponseBytesMax.
	Bytes   int64  `json:"bytes,omitempty"`
	PaidUSD string `json:"paid_usd"`
	Note    string `json:"note"`
}

// X402ChargeNote explains x402Charge on every paid-API answer.
const X402ChargeNote = "credits = base + tool + bytes; tool = per_byte × payment.amount (what the API was paid, in micro-USD; paid_usd in USD) + per_kib per 1,024 of it; bytes = 1 per KiB returned over 12 KiB (max_bytes). The call held max_cost first; what it did not use is released (call.cost is what was charged). Nothing paid for the API, no tool part."

// charge is the breakdown of a call whose upstream was paid amount
// micro-USD (0: nothing paid) and that returned n bytes of its answer.
func (x *x402) charge(c Call, amount int64, n int) *x402Charge {
	tool, base, extra := c.Price.For(amount), c.Price.For(0), x402ExtraCredits(n)
	return &x402Charge{Credits: tool + extra, Base: base, Tool: tool - base, Bytes: extra, PaidUSD: formatUnits(amount, 6), Note: X402ChargeNote}
}

// out is the answer's wrapper for p's resource.
func (x *x402) out(p x402Plan, status int, receipt *x402Receipt) x402Out {
	version := x.cfg.AllowlistVersion
	if p.res.Open || p.res.dynamic {
		version = 0
	}
	return x402Out{Resource: p.res.ID, Bundler: publicBundler(p.res.Bundler), AllowlistVersion: version, Status: status, Payment: receipt, TextIsUntrusted: true}
}

var mimeTypeRE = regexp.MustCompile(`^[a-z0-9!#$&^_.+-]{1,64}/[a-z0-9!#$&^_.+-]{1,64}$`)

// result is the agent's answer: the upstream's bytes as data (JSON inline
// when it is JSON, text when it is UTF-8 text, else base64), never rendered.
// An answer longer than p.limit is cut (x402Cut) and says so.
func (x *x402) result(c Call, p x402Plan, r x402Response, receipt *x402Receipt) (Result, error) {
	out := x.out(p, r.status, receipt)
	mt, _, _ := mime.ParseMediaType(r.contentType)
	mt = strings.ToLower(mt)
	if mimeTypeRE.MatchString(mt) {
		out.ContentType = mt
	}
	limit := p.limit
	if limit <= 0 {
		limit = p.res.MaxResponseBytes
	}
	// No HTML escaping (security review 1.20, M2): '<', '>' and '&' stay one
	// byte, so a JSON answer never grows here, and text that escaping would
	// grow past base64 goes as base64. The body is then at most 4/3 of the
	// limit plus the wrapper, inside X402StoredBodyBytes.
	cut := x402Cut(r.body, r.over, mt, limit)
	out.Encoding, out.Body, out.Bytes = cut.encoding, cut.body, cut.n
	if cut.note != "" {
		out.Truncated, out.TruncationNote = true, cut.note
		if r.over {
			out.FullBytesOver = len(r.body)
		} else {
			sum := sha256.Sum256(r.body)
			out.FullBytes, out.SHA256 = len(r.body), hex.EncodeToString(sum[:])
		}
	}
	amount := int64(0)
	public := map[string]any{"resource": p.res.ID, "http_status": r.status, "response_bytes": len(r.body)}
	if out.Truncated {
		public["truncated"], public["returned_bytes"] = true, cut.n
	}
	if receipt != nil {
		amount, _ = strconv.ParseInt(receipt.Amount, 10, 64)
		public["amount"] = receipt.Amount // not the transaction: it would tie the agent to our wallet's history
		if out.Truncated {
			r := *receipt
			r.Truncated = true
			out.Payment = &r
		}
	}
	out.Charge = x.charge(c, amount, cut.n)
	body := canonicalJSON(out)
	if body == nil || len(body) > X402StoredBodyBytes {
		return Result{}, refusal("x402_response_too_large")
	}
	pub, _ := json.Marshal(public)
	return Result{Body: body, Used: out.Charge.Credits, Public: pub}, nil
}

// Truncation notes (C578): how a cut answer's body was cut.
var (
	x402CutMore     = " Pass max_bytes (up to " + itoa(X402ResponseBytesCeiling) + ") for more."
	x402CutJSONNote = "The answer was longer than max_bytes: body is cut after its last complete element, so it still parses; full_bytes and sha256 are the whole answer's." + x402CutMore
	x402CutTextNote = "The answer was longer than max_bytes: body is its first max_bytes bytes as text (a cut JSON answer, which does not parse); full_bytes and sha256 are the whole answer's." + x402CutMore
	x402CutRawNote  = "The answer was longer than max_bytes: body is its first max_bytes bytes; full_bytes and sha256 are the whole answer's." + x402CutMore
	x402CutOverNote = "The answer was longer than " + itoa(X402ResponseBytesCeiling) + " bytes; reading stopped there (full_bytes_over), so its size and hash are unknown: body is its first max_bytes bytes."
)

// x402CutDepth bounds how deep x402CutJSON descends into the element that
// does not fit.
const x402CutDepth = 16

type x402Encoded struct {
	encoding string
	body     json.RawMessage
	n        int    // bytes of the answer returned
	note     string // why it was cut; "" when it was not
}

// x402Cut encodes raw (over: the answer ran past the read ceiling, raw is
// its first bytes) as at most limit bytes of the answer.
func x402Cut(raw []byte, over bool, mt string, limit int) x402Encoded {
	var compact bytes.Buffer
	if !over && len(bytes.TrimSpace(raw)) > 0 && json.Valid(raw) && utf8.Valid(raw) && json.Compact(&compact, raw) == nil {
		b := compact.Bytes()
		if len(b) <= limit {
			return x402Encoded{encoding: "json", body: b, n: len(b)}
		}
		// Cut after the last complete element, unless that keeps less than
		// half of what fits: then the first bytes, as text.
		if cut, ok := x402CutJSON(b, limit, x402CutDepth); ok && len(cut) >= limit/2 {
			return x402Encoded{encoding: "json", body: cut, n: len(cut), note: x402CutJSONNote}
		}
		e := x402Bytes(b[:limit], true)
		e.note = x402CutTextNote
		return e
	}
	textual := strings.HasPrefix(mt, "text/") || mt == ""
	if !over && len(raw) <= limit {
		if textual && utf8.Valid(raw) {
			return x402Text(raw)
		}
		return x402Encoded{encoding: "base64", body: canonicalJSON(base64.StdEncoding.EncodeToString(raw)), n: len(raw)}
	}
	// Cut: a JSON answer too long to parse goes as text too.
	note := x402CutRawNote
	if over {
		note = x402CutOverNote
	}
	textual = textual || mt == "application/json" || strings.HasSuffix(mt, "+json")
	e := x402Bytes(raw[:min(limit, len(raw))], textual)
	e.note = note
	return e
}

// x402Bytes is a cut prefix as text when cut (at a rune boundary) it is
// UTF-8, else as base64.
func x402Bytes(b []byte, textual bool) x402Encoded {
	if textual {
		t := b
		for i := 0; i < utf8.UTFMax && len(t) > 0 && !utf8.Valid(t); i++ {
			t = t[:len(t)-1]
		}
		if utf8.Valid(t) {
			return x402Text(t)
		}
	}
	return x402Encoded{encoding: "base64", body: canonicalJSON(base64.StdEncoding.EncodeToString(b)), n: len(b)}
}

// x402Text is UTF-8 text b as a JSON string, or base64 when escaping would
// make it longer than base64.
func x402Text(b []byte) x402Encoded {
	body := canonicalJSON(string(b))
	if len(body) > base64.StdEncoding.EncodedLen(len(b))+2 {
		return x402Encoded{encoding: "base64", body: canonicalJSON(base64.StdEncoding.EncodeToString(b)), n: len(b)}
	}
	return x402Encoded{encoding: "text", body: body, n: len(b)}
}

// x402CutJSON is compact JSON v in at most budget bytes, still valid JSON:
// an object or an array keeps its members in order while they fit whole,
// then the one that does not, itself cut, when depth allows and anything of
// it fits. A scalar that does not fit, or no room for "{}", is false.
func x402CutJSON(v []byte, budget, depth int) ([]byte, bool) {
	if len(v) <= budget {
		return v, true
	}
	if depth <= 0 || budget < 2 || len(v) == 0 || (v[0] != '{' && v[0] != '[') {
		return nil, false
	}
	open, end := v[0], byte(']')
	if open == '{' {
		end = '}'
	}
	dec := json.NewDecoder(bytes.NewReader(v))
	if _, err := dec.Token(); err != nil {
		return nil, false
	}
	out := []byte{open}
	room := budget - 1 // the closing delimiter
	for dec.More() {
		var key []byte
		if open == '{' {
			t, err := dec.Token()
			k, ok := t.(string)
			if err != nil || !ok {
				break
			}
			if key = canonicalJSON(k); key == nil {
				break
			}
			key = append(key, ':')
		}
		var val json.RawMessage
		if dec.Decode(&val) != nil {
			break
		}
		sep := 0
		if len(out) > 1 {
			sep = 1
		}
		if len(out)+sep+len(key)+len(val) <= room {
			if sep == 1 {
				out = append(out, ',')
			}
			out = append(append(out, key...), val...)
			continue
		}
		if sub, ok := x402CutJSON(val, room-len(out)-sep-len(key), depth-1); ok {
			if sep == 1 {
				out = append(out, ',')
			}
			out = append(append(out, key...), sub...)
		}
		break
	}
	return append(out, end), true
}

// discarded is the answer to a paid call whose response went over the
// resource's limit: no body, the payment receipt, and the call's price.
func (x *x402) discarded(c Call, p x402Plan, status, n int, receipt *x402Receipt) Result {
	out := x.out(p, status, receipt)
	out.Encoding, out.Body, out.Bytes = "discarded", json.RawMessage("null"), n
	amount, _ := strconv.ParseInt(receipt.Amount, 10, 64)
	if p.res.dynamic {
		amount = 0 // a bundler envelope too long to scrub: no tool part (C578)
	}
	out.Charge = x.charge(c, amount, 0)
	pub, _ := json.Marshal(map[string]any{"resource": p.res.ID, "http_status": status, "response_bytes": n, "amount": receipt.Amount, "discarded": true})
	return Result{Body: canonicalJSON(out), Used: out.Charge.Credits, Public: pub}
}

// unanswered is the answer to a call on a vetted open resource whose
// signed payment left our hands and got no answer (the upstream failed
// after it, or asked again with 402): no body, the payment as sent, and the
// call's price, since the upstream can settle it.
func (x *x402) unanswered(c Call, p x402Plan, pay *payment, code string) Result {
	r := pay.r
	receipt := &x402Receipt{Amount: strconv.FormatInt(r.amount, 10), Price: formatUnits(r.amount, x.cfg.Decimals), Asset: r.asset,
		Network: r.network, Settlement: x402SettlementOnchain, PayTo: r.payTo, Payer: x.cfg.Signer.Address().String(), Nonce: r.nonce}
	if !strings.HasPrefix(r.network, "eip155:") {
		// A key-based bundler's payment has no chain leg; its row's network
		// is the bundler's private name, never shown (C548).
		receipt.Network, receipt.Settlement, receipt.Payer = "", x402SettlementInternal, "swarmmemo"
	}
	out := x.out(p, pay.status, receipt)
	out.Charge = x.charge(c, r.amount, 0)
	out.Encoding, out.Body, out.Failure = "unanswered", json.RawMessage("null"), code
	pub, _ := json.Marshal(map[string]any{"resource": p.res.ID, "http_status": pay.status, "amount": strconv.FormatInt(r.amount, 10), "unanswered": true})
	return Result{Body: canonicalJSON(out), Used: out.Charge.Credits, Public: pub}
}

// resourceSubjects names each resource's review subject (x402:METHOD URL,
// RFC 0015 §4.2) and each MPP entry's (mpp:METHOD URL), and reads the
// summaries of those that have reviews; all empty while REVIEWS is off.
func (x *x402) resourceSubjects(ctx context.Context, q allowance.Querier, list []*X402Resource, mpp []*MPPEntry) (map[string]string, map[string]string, map[string]any, error) {
	if x.reviews == nil || len(list)+len(mpp) == 0 {
		return nil, nil, nil, nil
	}
	ids, mppIDs := map[string]string{}, map[string]string{}
	var subjects []string
	opt := x.reviews.SubjectOptions()
	name := func(typ, method, u string, into map[string]string, id string) {
		if s, err := reviews.Canonical(typ, method+" "+u, opt); err == nil {
			into[id] = s.ID
			subjects = append(subjects, s.ID)
		}
	}
	for _, r := range list {
		name(reviews.TypeX402, r.Method, r.URL, ids, r.ID)
	}
	for _, e := range mpp {
		name(reviews.TypeMPP, e.Method, e.URL, mppIDs, e.ID)
	}
	sums, err := x.reviews.ReviewSummaries(ctx, q, subjects)
	return ids, mppIDs, sums, err
}

// mppEntryJSON is an MPP entry as the resources read lists it: kind mpp,
// its URL (public in the directory, and what an MPP client pays), never
// callable here.
func (x *x402) mppEntryJSON(e *MPPEntry, subject string, summaries map[string]any) map[string]any {
	entry := map[string]any{
		"id": e.ID, "kind": KindMPP, "protocols": e.Protocols, "url": e.URL, "method": e.Method, "category": e.Category, "summary": e.servedSummary(),
		"summary_status": e.summaryStatus, "text_is_untrusted": true, "intent": e.Intent, "currency": e.Currency, "network": e.Network,
		"pinned": false, "vetted": false, "callable": false, "reason": e.Reason(x.cfg.Network),
	}
	if u := x.cfg.PricingURL(e.URL); u != "" {
		entry["pricing_url"] = u
	}
	if e.Price >= 0 {
		entry["price"] = formatUnits(e.Price, 6)
	} else {
		entry["price"] = nil // dynamic, or not in US dollars
	}
	if subject != "" {
		entry["subject_id"] = subject
		if sum, ok := summaries[subject]; ok {
			entry["reviews"] = sum
		}
	}
	return entry
}

// mppSummary is the resources read's catalogue.mpp: the directory, how
// many MPP entries are listed on their own and how many x402 resources the
// directory also lists.
func (x *x402) mppSummary(s *x402Snapshot) map[string]any {
	out := map[string]any{"enabled": x.cfg.Catalogue != nil && x.cfg.Catalogue.MPPDirectory != ""}
	if out["enabled"] == true {
		out["directory"] = x.cfg.Catalogue.MPPDirectory
		out["listed"], out["also_x402"], out["imported_at"] = len(s.mpp.only), len(s.mpp.byKey)-len(s.mpp.only), s.mpp.importedAt
		out["callable"] = "never through the relay: kind mpp entries are listed for agents that pay MPP themselves (reason says why)"
	}
	return out
}

// Read serves "resources": a page of the catalogue that matches the search,
// best first, with maximum prices in USD and in credits; the categories; the
// caps; and what is left of today's budget.
func (x *x402) Read(ctx context.Context, q allowance.Querier, c Call) (json.RawMessage, error) {
	if x.cfg == nil || c.Method != "resources" {
		return nil, refusal("service_unavailable") // tools_search and tools_get are ReadRemote's
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
	var globalSpent, yours, openSpent, bundlerSpent int64
	if err := q.QueryRowContext(ctx, "SELECT COALESCE(SUM(amount),0), COALESCE(SUM(CASE WHEN account=? THEN amount ELSE 0 END),0), COALESCE(SUM(CASE WHEN allowlist_version=0 THEN amount ELSE 0 END),0), COALESCE(SUM(CASE WHEN allowlist_version=-1 THEN amount ELSE 0 END),0) FROM x402_payments WHERE day=? AND state<>'failed'", c.Subject.ID, day).Scan(&globalSpent, &yours, &openSpent, &bundlerSpent); err != nil {
		return nil, err
	}
	page := x.search(s)
	subjects, mppSubjects, summaries, err := x.resourceSubjects(ctx, q, page.resources, page.mpp)
	if err != nil {
		return nil, err
	}
	resources := []any{}
	for _, r := range page.resources {
		m := min(r.MaxAmount, x.cfg.PerCall)
		query := r.Query
		if query == nil {
			query = []string{}
		}
		entry := map[string]any{
			"id": r.ID, "kind": KindX402, "protocols": page.snap.protocolsOf(r), "bundler": publicBundler(r.Bundler), "category": r.Category, "summary": r.servedSummary(),
			"method": r.Method, "query": query, "body": r.Body,
			"max_price": formatUnits(m, x.cfg.Decimals), "max_cost": price.For(m), "max_response_bytes": r.MaxResponseBytes, "pinned": !r.Open,
			"vetted": !r.Open || r.Vetted, "callable": x.callable(r),
		}
		if r.Open {
			entry["text_is_untrusted"], entry["summary_status"] = true, r.summaryStatus
		}
		if u := x.cfg.PricingURL(r.URL); u != "" {
			entry["pricing_url"] = u
		}
		if proof := r.Proof(); proof != "" {
			// Its owner listed it (x402.submit), proven by a domain or wallet link.
			entry["source"], entry["proof"] = "submitted", proof
		}
		if st, ok := page.stats[r.ID]; ok {
			entry["calls_30d"], entry["failed_30d"] = st.ok, st.failed
		}
		if id, ok := subjects[r.ID]; ok {
			entry["subject_id"] = id
			if sum, ok := summaries[id]; ok {
				entry["reviews"] = sum
			}
		}
		resources = append(resources, entry)
	}
	for _, e := range page.mpp {
		resources = append(resources, x.mppEntryJSON(e, mppSubjects[e.ID], summaries))
	}
	cu := func(n int64) string { return formatUnits(n, x.cfg.Decimals) }
	today := map[string]any{"global_spent": cu(globalSpent), "global_remaining": cu(max(0, x.cfg.GlobalDaily-globalSpent))}
	if c.Subject.Signed {
		today["your_spent"], today["your_remaining"] = cu(yours), cu(max(0, x.cfg.AgentDaily-yours))
	}
	caps := map[string]any{"per_call": cu(x.cfg.PerCall), "agent_daily": cu(x.cfg.AgentDaily), "global_daily": cu(x.cfg.GlobalDaily)}
	catalogue := map[string]any{"pinned": len(x.cfg.Resources), "open": page.open, "imported_at": page.importedAt, "enabled": x.cfg.Catalogue != nil,
		"mpp": x.mppSummary(page.snap)}
	if cc := x.cfg.Catalogue; cc != nil {
		caps["open_daily"], caps["recipient_daily"], caps["open_max_price"] = cu(cc.OpenDaily), cu(cc.RecipientDaily), cu(cc.MaxPrice)
		if a := cc.AutoVet; a != nil {
			catalogue["auto_vet"] = a.Describe(x.cfg.Decimals)
		}
		today["open_spent"], today["open_remaining"] = cu(openSpent), cu(max(0, cc.OpenDaily-openSpent))
	}
	tools := map[string]any{"enabled": false}
	if x.fr != nil {
		tools = x.bundlerSummary(price)
		caps["tools_daily"], caps["tools_per_tool_daily"], caps["tools_max_price"] = cu(x.fr.cfg.OpenDaily), cu(x.fr.cfg.ToolDaily), cu(x.fr.cfg.MaxPrice)
		today["tools_spent"], today["tools_remaining"] = cu(bundlerSpent), cu(max(0, x.fr.cfg.OpenDaily-bundlerSpent))
	}
	body := map[string]any{
		"allowlist_version": x.cfg.AllowlistVersion, "network": x.cfg.Network, "asset": x.cfg.Asset.String(), "asset_name": x.cfg.AssetName,
		"paused": x.paused(), "price": price, "resources": resources, "matched": page.matched, "categories": page.categories,
		"bundlers": x.readyBundlers(), "catalogue": catalogue, "tools": tools, "today": today, "caps": caps,
		"vetting": X402VettingNote,
		// Open resources' summaries are upstream text (each is marked too).
		"text_is_untrusted": true,
	}
	if page.next != "" {
		body["next_cursor"] = page.next
	}
	if len(x.cfg.Sellers) > 0 {
		body["pricing"] = X402PricingNote
	}
	return canonicalJSON(body), nil
}

// readyBundlers are the names of the bundlers that can take a call now.
func (x *x402) readyBundlers() []string {
	out := []string{}
	for name, b := range x.bundlers {
		if b.Ready() {
			out = append(out, publicBundler(name))
		}
	}
	slices.Sort(out)
	return out
}

// publicBundler is the name agents see for a bundler: a key-based vendor is
// "tools", whoever provides it.
func publicBundler(name string) string {
	if name == X402Bundler {
		return name
	}
	return "tools"
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
	// Bundlers configures the key-based bundlers (keybundler.go).
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
	// Sellers declare a host's own price list, pricing_url (pricing.go).
	Sellers []x402SellerFile `json:"sellers"`
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
	if err := c.parseSellers(f.Sellers); err != nil {
		return err
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

// X402EphemeralTunnelHosts are the host suffixes of quick-tunnel services
// whose hostnames change on every restart: a listing on one dies within
// hours. x402.submit refuses them before any probe
// (x402_listing_ephemeral_host, x402EphemeralHost), a crawled item on one is
// not admitted, and a row stored before the refusal is marked stale at
// refresh, never deleted, as a path template is (x402PathTemplate).
var X402EphemeralTunnelHosts = []string{
	"trycloudflare.com", "ngrok-free.app", "ngrok.app", "ngrok.io", "loca.lt",
	"localtunnel.me", "serveo.net", "localhost.run", "lhr.life", "pinggy.link", "bore.pub",
}

// x402EphemeralHost reports whether raw's host is one of
// X402EphemeralTunnelHosts or a subdomain of one, matched on label
// boundaries only: xtrycloudflare.com is not matched, a.b.trycloudflare.com
// is.
func x402EphemeralHost(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".") // a root-FQDN dot is the same host (round 6, conclave-pickle)
	for _, suf := range X402EphemeralTunnelHosts {
		if host == suf || strings.HasSuffix(host, "."+suf) {
			return true
		}
	}
	return false
}
