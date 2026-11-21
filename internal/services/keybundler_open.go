package services

// The open bundler catalogue: with bundlers.frames.open, every tool of the
// bundler's API (about X402ToolsApprox paid tools behind one account) is callable through x402
// without an allowlist entry, as resource "tool:TOOL_ID".
//
// Finding a tool. service.read x402 tools_search passes the agent's query
// to the bundler's free search and answers a trimmed hit list; tools_get reads
// one tool (the bundler's free descriptor and probe). Both need the network, so
// they run after the command's transaction has committed (RemoteReader),
// under the engine's per-caller read limit, a cache (searches 10 minutes,
// descriptors an hour, probes 5 minutes) and a global bound on upstream
// requests a minute, a bound per network on unsigned readers' upstream
// requests, and a pause for the bundler's Retry-After after a 429 (C1187).
// Upstream text (titles, descriptions, schemas) is
// untrusted: control characters are stripped, every field is capped, a
// description SwarmMemo's text screen flagged is withheld, and each answer
// says text_is_untrusted. Every hit is recorded in frames_tools with the bundler's
// own vetting flag, which a call needs: a tool no search has returned in 30
// days is unknown.
//
// Calling a tool. The quote is the board's price at min(MaxCost, the
// caller's max_cost), MaxCost being max_price plus the bundler's fees;
// nothing upstream is asked inside the transaction. The admission check (in
// the transaction, a read of frames_tools) refuses an unknown tool, a tool
// neither the bundler nor the operator vetted (unless allow_unvetted;
// keybundler_vet.go) and a denied category. After commit, before any money moves, the tool's
// descriptor (its host, against the operator's denylist) and a probe (live,
// payable, its price against max_price and the caller's ceiling) decide;
// then the call goes through keyBundler.Exchange exactly as an allowlisted
// bundler tool: reserved first against every cap and the bundler sub-caps
// (open_daily for all bundler tools, tool_daily per tool), charged what
// the bundler reports, refunded on a 4xx, one payment per request_id.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"swarmmemo/internal/allowance"
)

// BundlerPrefix starts the resource id of an open bundler tool.
const BundlerPrefix = "tool:"

// bundlerLegacyPrefix is the prefix 1.29.6 served; ids that carry it still resolve.
const bundlerLegacyPrefix = "frames:"

// bundlerVendorPrefix is the upstream's namespace on its own tools
// ("frames.coingecko.post.api-price"). Public ids leave it out
// ("tool:coingecko.post.api-price"): the catalogue is SwarmMemo tools, and
// the upstream never appears in an answer. Ids that carry it still resolve.
const bundlerVendorPrefix = "frames."

// bundlerPublicTool is a tool's public name: its upstream id without the
// upstream's namespace.
func bundlerPublicTool(tool string) string { return strings.TrimPrefix(tool, bundlerVendorPrefix) }

// bundlerToolID is a tool's public resource id, "tool:NAME".
func bundlerToolID(tool string) string { return BundlerPrefix + bundlerPublicTool(tool) }

// bundlerLookup finds the upstream tool a public name stands for: the name
// as given, else with the upstream's namespace put back, whichever a recent
// search recorded; the name as given when neither is known.
func bundlerLookup(ctx context.Context, q allowance.Querier, name string, now int64) (string, bundlerTool, bool, error) {
	candidates := []string{name}
	if !strings.HasPrefix(name, bundlerVendorPrefix) {
		candidates = append(candidates, bundlerVendorPrefix+name)
	}
	for _, tool := range candidates {
		row, known, err := bundlerToolRow(ctx, q, tool, now)
		if err != nil || known {
			return tool, row, known, err
		}
	}
	return name, bundlerTool{}, false, nil
}

// Open catalogue bounds.
const (
	bundlerSearchTTL     = 10 * time.Minute
	bundlerDescriptorTTL = time.Hour
	bundlerProbeTTL      = 5 * time.Minute
	bundlerCacheMax      = 1024 // entries of each cache
	// bundlerUpstreamPerMinute bounds the requests SwarmMemo sends the bundler's
	// free endpoints a minute, for every caller together (cache misses).
	bundlerUpstreamPerMinute = 120
	// bundlerAnonUpstreamPerMinute bounds the requests one network's unsigned
	// tools_search and tools_get reads send the bundler a minute (cache
	// misses; a cold tools_get costs two), answered 429 request_rate.
	bundlerAnonUpstreamPerMinute = 10
	// A bundler 429 stops every upstream request for its Retry-After
	// (default bundlerBackoffDefault, at most bundlerBackoffMax).
	bundlerBackoffDefault = 30 * time.Second
	bundlerBackoffMax     = 10 * time.Minute
	// Timeouts of the bundler's free endpoints: a search runs its angles in
	// parallel (seconds each), a probe asks the seller.
	bundlerSearchTimeout    = 15 * time.Second
	bundlerToolTimeout      = 8 * time.Second
	bundlerProbeTimeout     = 12 * time.Second
	bundlerSearchBytes      = 512 << 10
	bundlerToolBytes        = 128 << 10
	bundlerProbeBytes       = 64 << 10
	bundlerHitsMax          = 25
	bundlerQueryBytes       = 200
	bundlerQueriesMax       = 4
	bundlerTitleRunes       = 120
	bundlerDescriptionRunes = 600
	bundlerCapabilitiesMax  = 8
	bundlerCapabilityRunes  = 32
	bundlerSchemaBytes      = 2048
	// bundlerToolStale is how long a search hit lets the tool be called.
	bundlerToolStale = 30 * 86400
	// bundlerScreenEvery and bundlerScreenBatch bound the background screen of
	// tool descriptions: at most 10 texts an hour.
	bundlerScreenEvery = 3600
	bundlerScreenBatch = 10
)

const bundlerSchema = `
CREATE TABLE IF NOT EXISTS frames_tools (
 id TEXT PRIMARY KEY, vetted INTEGER NOT NULL, title TEXT NOT NULL DEFAULT '', description TEXT NOT NULL DEFAULT '',
 category TEXT NOT NULL DEFAULT '', host TEXT NOT NULL DEFAULT '', search_id TEXT NOT NULL DEFAULT '', seen_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS frames_tools_seen ON frames_tools(seen_at);
`

// BundlerOpen is the open bundler catalogue's validated configuration.
// Amounts are micro-USD.
type BundlerOpen struct {
	MaxPrice int64 // the most a callable tool may be priced (its listed price)
	// MaxCost is the most one call may pay, derived (never configured):
	// bundlerCost(MaxPrice), the price plus the bundler's fees, at most
	// caps.per_call. A tool priced at most MaxPrice fits it (C570).
	MaxCost   int64
	OpenDaily int64 // all open bundler calls, per UTC day
	ToolDaily int64 // one tool, per UTC day
	// AllowUnvetted makes tools the bundler has not vetted callable too.
	AllowUnvetted bool
	// Anonymous lets an unsigned service.call reach bundler tools, billed to
	// the caller's network's free daily credit.
	Anonymous bool
}

var (
	bundlerToolRE       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@-]{0,199}$`)
	bundlerToolRule     = "The tool is its id from tools_search (tool:NAME): 1 to 200 letters, digits or . _ : @ -."
	bundlerCapabilityRE = regexp.MustCompile(`^[a-z0-9][a-z0-9 _./-]{0,63}$`)
	bundlerSearchIDRE   = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,80}$`)
	bundlerHostRE       = regexp.MustCompile(`^[a-z0-9.-]{1,253}$`)
)

// parseBundlerOpen validates the open catalogue's fields of bundlers.frames;
// nil when open is false.
func (c *X402Config) parseBundlerOpen(f *x402KeyBundlerFile) (*BundlerOpen, error) {
	if !f.Open {
		if f.MaxPrice != "" || f.AllowUnvetted || f.OpenDaily != "" || f.ToolDaily != "" || f.Anonymous {
			return nil, errors.New("x402: config fields bundlers.frames.max_price, allow_unvetted, open_daily, tool_daily and anonymous need open: true")
		}
		return nil, nil
	}
	if c.Decimals != 6 {
		return nil, errors.New("x402: bundlers.frames.open needs decimals 6 (Frames bills in USD)")
	}
	amount := func(field, text, def string, lo, hi int64) (int64, error) {
		if text == "" {
			n, _ := parseUnits(def, c.Decimals)
			return max(lo, min(n, hi)), nil
		}
		n, ok := parseUnits(text, c.Decimals)
		if !ok || n < lo || n > hi || n <= 0 {
			return 0, fmt.Errorf("x402: config field bundlers.frames.%s is invalid (between %s and %s)", field, formatUnits(lo, c.Decimals), formatUnits(hi, c.Decimals))
		}
		return n, nil
	}
	o := &BundlerOpen{AllowUnvetted: f.AllowUnvetted, Anonymous: f.Anonymous}
	var err error
	if o.MaxPrice, err = amount("max_price", f.MaxPrice, "0.02", 1, c.PerCall); err != nil {
		return nil, err
	}
	o.MaxCost = min(bundlerCost(o.MaxPrice), c.PerCall)
	if o.OpenDaily, err = amount("open_daily", f.OpenDaily, "1", o.MaxPrice, c.GlobalDaily); err != nil {
		return nil, err
	}
	if o.ToolDaily, err = amount("tool_daily", f.ToolDaily, "0.25", o.MaxPrice, o.OpenDaily); err != nil {
		return nil, err
	}
	return o, nil
}

// keyedBundler is the configured key-based bundler, or nil.
func (c *X402Config) keyedBundler() *keyBundler {
	b, _ := c.bundler("frames").(*keyBundler)
	return b
}

// ttlCache is a small map of values that expire, bounded at bundlerCacheMax
// entries; its owner locks it.
type ttlCache[T any] struct{ m map[string]ttlEntry[T] }

type ttlEntry[T any] struct {
	v     T
	until time.Time
}

func (c *ttlCache[T]) get(k string, now time.Time) (T, bool) {
	e, ok := c.m[k]
	if !ok || !now.Before(e.until) {
		var zero T
		return zero, false
	}
	return e.v, true
}

func (c *ttlCache[T]) put(k string, v T, now time.Time, ttl time.Duration) {
	if c.m == nil {
		c.m = map[string]ttlEntry[T]{}
	}
	if _, ok := c.m[k]; !ok && len(c.m) >= bundlerCacheMax {
		for key, e := range c.m {
			if !now.Before(e.until) {
				delete(c.m, key)
			}
		}
		if len(c.m) >= bundlerCacheMax {
			clear(c.m)
		}
	}
	c.m[k] = ttlEntry[T]{v: v, until: now.Add(ttl)}
}

// bundlerState is the provider's open bundler catalogue.
type bundlerState struct {
	b    *keyBundler
	cfg  *BundlerOpen
	deny *x402DenyIndex // the operator's denylist, indexed once

	mu          sync.Mutex
	searches    ttlCache[bundlerSearchPage]
	descriptors ttlCache[bundlerDescriptor]
	probes      ttlCache[bundlerProbe]
	minute      int64 // the upstream window's minute and its requests
	requests    int
	// backoffUntil is when the bundler's last 429 lifts: until then no
	// request is sent.
	backoffUntil time.Time
	anonMisses   rateTable // unsigned readers' upstream requests, per network
	// now is the upstream bounds' clock: time.Now, moved by tests.
	now func() time.Time

	screening atomic.Bool
	screened  atomic.Int64 // when descriptions were last screened (the store's clock)
}

func newBundlerState(cfg *X402Config, b *keyBundler) *bundlerState {
	return &bundlerState{b: b, cfg: b.open, deny: cfg.denyIndex(nil), now: time.Now}
}

// allowUpstream counts one request to the bundler against the global minute,
// and refuses every one, sending nothing, while the bundler's last 429 holds.
func (f *bundlerState) allowUpstream(now time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if wait := f.backoffUntil.Sub(now); wait > 0 {
		return &allowance.Err{Code: "upstream_busy", RetryAfter: ceilSeconds(wait)}
	}
	minute := now.Unix() / 60
	if f.minute != minute {
		f.minute, f.requests = minute, 0
	}
	if f.requests >= bundlerUpstreamPerMinute {
		return &allowance.Err{Code: "request_rate", RetryAfter: int(60 - now.Unix()%60)}
	}
	f.requests++
	return nil
}

// backOff records the bundler's 429: nothing is sent for its Retry-After
// (seconds or an HTTP date; default bundlerBackoffDefault, at most
// bundlerBackoffMax). It returns the seconds left to wait.
func (f *bundlerState) backOff(retryAfter string, now time.Time) int {
	wait := bundlerBackoffDefault
	if n, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && n > 0 {
		wait = time.Duration(min(n, int(bundlerBackoffMax/time.Second))) * time.Second
	} else if at, err := http.ParseTime(retryAfter); err == nil && at.After(now) {
		wait = min(at.Sub(now), bundlerBackoffMax)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if until := now.Add(wait); until.After(f.backoffUntil) {
		f.backoffUntil = until
	}
	return ceilSeconds(f.backoffUntil.Sub(now))
}

// ceilSeconds is d in whole seconds, rounded up.
func ceilSeconds(d time.Duration) int { return int((d + time.Second - 1) / time.Second) }

// bundlerCaller is the context key of an unsigned reader's network.
type bundlerCaller struct{}

// bundlerCallerOf marks ctx with an unsigned reader's network, whose
// upstream requests allowAnonUpstream bounds; a signed reader is unmarked.
func bundlerCallerOf(ctx context.Context, s allowance.Subject) context.Context {
	if s.Signed || s.ID == "" {
		return ctx
	}
	return context.WithValue(ctx, bundlerCaller{}, s.ID)
}

// allowAnonUpstream counts one unsigned reader's upstream request against
// its network's minute (bundlerAnonUpstreamPerMinute).
func (f *bundlerState) allowAnonUpstream(ctx context.Context, now time.Time) error {
	network, _ := ctx.Value(bundlerCaller{}).(string)
	if network == "" {
		return nil
	}
	return f.anonMisses.admit(network, bundlerAnonUpstreamPerMinute, now.Unix())
}

// bundlerAPI sends one request to the bundler's API through the SSRF-safe client,
// with our key, reading at most limit bytes of a 2xx answer. The key is
// never part of an error.
func (x *x402) bundlerAPI(ctx context.Context, method, path string, body any, limit int, timeout time.Duration) ([]byte, error) {
	now := x.fr.now()
	if err := x.fr.allowUpstream(now); err != nil {
		return nil, err
	}
	if err := x.fr.allowAnonUpstream(ctx, now); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, refusal("upstream_failed")
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, x.fr.b.base+path, rd)
	if err != nil {
		return nil, refusal("upstream_failed")
	}
	req.Header.Set("User-Agent", x402UserAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+x.fr.b.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := x.client.Do(req)
	if err != nil {
		return nil, refusal("upstream_failed")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	switch {
	case err != nil || len(raw) > limit:
		return nil, refusal("upstream_failed")
	case resp.StatusCode == http.StatusNotFound:
		return nil, refusal("x402_unknown_resource")
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, &allowance.Err{Code: "upstream_busy", RetryAfter: x.fr.backOff(resp.Header.Get("Retry-After"), x.fr.now())}
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return nil, refusal("upstream_failed")
	}
	return raw, nil
}

// Untrusted text.

// cleanText is upstream text made safe to pass on: valid UTF-8, no control
// or format characters, whitespace collapsed, at most n runes (an ellipsis
// marks a cut).
func cleanText(s string, n int) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsSpace(r):
			return ' '
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Co, r):
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > n {
		r := []rune(s)
		s = strings.TrimSpace(string(r[:n-1])) + "…"
	}
	return s
}

// bundlerCapabilities are a hit's capabilities, cleaned: lowercase words, at
// most bundlerCapabilitiesMax.
func bundlerCapabilities(in []string) []string {
	out := []string{}
	for _, c := range in {
		c = strings.ToLower(cleanText(c, bundlerCapabilityRunes))
		if bundlerCapabilityRE.MatchString(c) && len(out) < bundlerCapabilitiesMax {
			out = append(out, c)
		}
	}
	return out
}

// bundlerSchemaOf is an input schema compacted, or nil when it is not a JSON
// object or longer than bundlerSchemaBytes (cut reports that).
func bundlerSchemaOf(raw json.RawMessage) (schema json.RawMessage, cut bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false
	}
	var b bytes.Buffer
	if !isObject(raw) || json.Compact(&b, raw) != nil || !utf8.Valid(b.Bytes()) {
		return nil, false
	}
	if b.Len() > bundlerSchemaBytes {
		return nil, true
	}
	return json.RawMessage(b.Bytes()), false
}

// bundlerPrice reads a USD price the bundler gave as a number or a string into
// micro-USD, rounded up; false for anything else (negative, not finite, or
// over $1,000).
func bundlerPrice(raw json.RawMessage) (int64, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" || len(raw) > 40 {
		return 0, false
	}
	text := string(raw)
	if raw[0] == '"' {
		if json.Unmarshal(raw, &text) != nil {
			return 0, false
		}
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1000 {
		return 0, false
	}
	return int64(math.Ceil(v*1e6 - 1e-6)), true
}

// Search.

type bundlerSearchArgs struct {
	Query      string   `json:"query"`
	Queries    []string `json:"queries"`
	Capability string   `json:"capability"`
	MaxPrice   string   `json:"max_price"`
}

// bundlerSearch is a validated search: the upstream's body and its cache key.
type bundlerSearch struct {
	queries    []string
	capability string
	maxPrice   int64
}

func (s bundlerSearch) key() string {
	return strings.Join(s.queries, "\x1f") + "\x1e" + s.capability + "\x1e" + strconv.FormatInt(s.maxPrice, 10)
}

func (s bundlerSearch) body() map[string]any {
	b := map[string]any{"max_price_usd": json.Number(formatUnits(s.maxPrice, 6))}
	if len(s.queries) == 1 {
		b["query"] = s.queries[0]
	} else {
		b["queries"] = s.queries
	}
	if s.capability != "" {
		b["capability"] = s.capability
	}
	return b
}

// normalizeQuery is one search angle, lowercased with its whitespace
// collapsed; "" when it is empty, too long or carries control characters.
func normalizeQuery(q string) string {
	if !utf8.ValidString(q) || len(q) > bundlerQueryBytes || strings.IndexFunc(q, func(r rune) bool { return unicode.IsControl(r) && !unicode.IsSpace(r) }) >= 0 {
		return ""
	}
	return strings.Join(strings.Fields(strings.ToLower(q)), " ")
}

func (x *x402) parseBundlerSearch(raw json.RawMessage) (bundlerSearch, error) {
	var a bundlerSearchArgs
	if err := StrictObject(raw, &a); err != nil {
		return bundlerSearch{}, err
	}
	s := bundlerSearch{maxPrice: x.fr.cfg.MaxPrice}
	switch {
	case a.Query != "" && len(a.Queries) == 0:
		a.Queries = []string{a.Query}
	case a.Query == "" && len(a.Queries) >= 2 && len(a.Queries) <= bundlerQueriesMax:
	default:
		return bundlerSearch{}, badArg("Give query (one search) or queries (2 to " + itoa(bundlerQueriesMax) + " searches), not both.")
	}
	seen := map[string]bool{}
	for _, q := range a.Queries {
		n := normalizeQuery(q)
		if n == "" || seen[n] {
			return bundlerSearch{}, badArg("Each query needs words in it, and queries must differ from each other.")
		}
		seen[n] = true
		s.queries = append(s.queries, n)
	}
	if a.Capability != "" {
		s.capability = strings.ToLower(strings.TrimSpace(a.Capability))
		if !bundlerCapabilityRE.MatchString(s.capability) {
			return bundlerSearch{}, badArg("capability is 1 to 64 lowercase letters, digits, spaces or _ . / -, starting with a letter or digit.")
		}
	}
	if a.MaxPrice != "" {
		n, ok := parseUnits(a.MaxPrice, 6)
		if !ok || n <= 0 {
			return bundlerSearch{}, badArg(`max_price is a positive USD amount as a decimal string, at most 6 decimals ("0.05").`)
		}
		s.maxPrice = min(n, s.maxPrice)
	}
	return s, nil
}

// bundlerHit is one search hit, cleaned.
type bundlerHit struct {
	ID, Title, Description string
	Truncated              bool
	Capabilities           []string
	Category               string
	Vetted                 bool
	// Price is the search index's price (micro-USD), a snapshot that may be
	// stale: its probe's when invoke_ready, else the listing's price_hint.
	// Priced says whether it is known.
	Price     int64
	Priced    bool
	Live      *bool
	Schema    json.RawMessage
	SchemaCut bool
	PayTo     []EVMAddress // where the listing says payments go (bundlerPayTo)
	// Method and URL are the endpoint the listing invokes, when it says
	// (invocation); catalogueMatch matches them to the catalogue's.
	Method, URL string
}

type bundlerSearchPage struct {
	searchID string
	partial  bool
	hits     []bundlerHit
}

type bundlerHitWire struct {
	ID                   string          `json:"id"`
	Title                string          `json:"title"`
	Description          string          `json:"description"`
	DescriptionTruncated bool            `json:"description_truncated"`
	Capabilities         []string        `json:"capabilities"`
	Unvetted             bool            `json:"unvetted"`
	InputSchema          json.RawMessage `json:"input_schema"`
	Live                 *bool           `json:"live"`
	InvokeReady          bool            `json:"invoke_ready"`
	PriceUSD             json.RawMessage `json:"price_usd"`
	Payment              struct {
		PriceHint json.RawMessage `json:"price_hint"`
	} `json:"payment"`
	Invocation struct {
		Method string `json:"method"`
		URL    string `json:"url"`
	} `json:"invocation"`
}

// parseBundlerSearchPage reads the bundler's search answer, keeping valid hits.
func parseBundlerSearchPage(raw []byte) (bundlerSearchPage, error) {
	var w struct {
		SearchID string            `json:"search_id"`
		Hits     []json.RawMessage `json:"hits"`
		Partial  bool              `json:"partial"`
	}
	if err := json.Unmarshal(raw, &w); err != nil {
		return bundlerSearchPage{}, refusal("upstream_failed")
	}
	page := bundlerSearchPage{partial: w.Partial}
	if bundlerSearchIDRE.MatchString(w.SearchID) {
		page.searchID = w.SearchID
	}
	seen := map[string]bool{}
	for _, rawHit := range w.Hits {
		if len(page.hits) >= bundlerHitsMax {
			break
		}
		var h bundlerHitWire
		if json.Unmarshal(rawHit, &h) != nil || !bundlerToolRE.MatchString(h.ID) || !bundlerToolRE.MatchString(bundlerPublicTool(h.ID)) || seen[h.ID] {
			continue
		}
		seen[h.ID] = true
		hit := bundlerHit{ID: h.ID, Title: cleanText(h.Title, bundlerTitleRunes), Description: cleanText(h.Description, bundlerDescriptionRunes),
			Truncated: h.DescriptionTruncated, Capabilities: bundlerCapabilities(h.Capabilities), Vetted: !h.Unvetted, Live: h.Live}
		hit.Truncated = hit.Truncated || utf8.RuneCountInString(hit.Description) >= bundlerDescriptionRunes
		hit.Category = x402Categorize(hit.Title + " " + hit.Description + " " + strings.Join(hit.Capabilities, " "))
		if p, ok := bundlerPrice(h.PriceUSD); ok && h.InvokeReady {
			hit.Price, hit.Priced = p, true
		} else if p, ok := bundlerPrice(h.Payment.PriceHint); ok {
			hit.Price, hit.Priced = p, true
		}
		hit.Schema, hit.SchemaCut = bundlerSchemaOf(h.InputSchema)
		hit.PayTo = bundlerPayTo(rawHit)
		hit.Method, hit.URL = strings.ToUpper(h.Invocation.Method), h.Invocation.URL
		page.hits = append(page.hits, hit)
	}
	return page, nil
}

// bundlerRecord upserts the hits into frames_tools: the bundler's vetting flag, the
// text to screen, the search that found them. Runs after commit.
func (x *x402) bundlerRecord(ctx context.Context, page bundlerSearchPage, now int64) error {
	if len(page.hits) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	tx, err := x.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, h := range page.hits {
		if _, err = tx.ExecContext(ctx, `INSERT INTO frames_tools(id,vetted,title,description,category,search_id,seen_at) VALUES(?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET vetted=excluded.vetted, title=excluded.title, description=excluded.description, category=excluded.category, search_id=excluded.search_id, seen_at=excluded.seen_at`,
			h.ID, b2i(h.Vetted), h.Title, h.Description, h.Category, page.searchID, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// bundlerTool is a tool's row in frames_tools.
type bundlerTool struct {
	vetted                   bool
	category, host, searchID string
}

// bundlerToolRow reads tool's row; false when no search returned it in the
// last bundlerToolStale seconds.
func bundlerToolRow(ctx context.Context, q allowance.Querier, tool string, now int64) (bundlerTool, bool, error) {
	var t bundlerTool
	var vetted int
	var seen int64
	err := q.QueryRowContext(ctx, "SELECT vetted,category,host,search_id,seen_at FROM frames_tools WHERE id=?", tool).Scan(&vetted, &t.category, &t.host, &t.searchID, &seen)
	if errors.Is(err, sql.ErrNoRows) {
		return t, false, nil
	}
	if err != nil {
		return t, false, err
	}
	t.vetted = vetted == 1
	return t, now-seen <= bundlerToolStale, nil
}

// whyNot says why a tool is not callable now ("" when it is): neither the
// bundler nor the operator vetted it, its category or host is denied, or it
// is priced over max_price (or its cost, bundlerCost of the price, over the
// derived MaxCost).
func (f *bundlerState) whyNot(vetted bool, category, host string, price int64, priced bool) string {
	switch {
	case !vetted && !f.cfg.AllowUnvetted:
		return "tool_unvetted"
	case f.denied(category, host):
		return "tool_denied"
	case priced && f.overCap(price):
		return "tool_price_over_cap"
	}
	return ""
}

// overCap says whether a tool priced price is over the cap: max_price, or
// (with caps.per_call below it) what a call of it costs.
func (f *bundlerState) overCap(price int64) bool {
	return price > f.cfg.MaxPrice || bundlerCost(price) > f.cfg.MaxCost
}

// denied applies the operator's denylist to a tool's category and host.
func (f *bundlerState) denied(category, host string) bool {
	if f.deny.categories[category] {
		return true
	}
	if host == "" {
		return false
	}
	r := &X402Resource{URL: "https://" + host + "/", Category: category}
	indexResource(r)
	return f.deny.blocks(r)
}

// screenStatus is a text's summary_status from the screen's verdicts.
func screenStatus(text string, verdicts map[string]string) string {
	switch {
	case text == "":
		return summaryScreened
	case verdicts[sha256Of([]byte(text))] == "pass":
		return summaryScreened
	case verdicts[sha256Of([]byte(text))] == "flag":
		return summaryWithheld
	}
	return "unscreened"
}

func (x *x402) verdictsFor(ctx context.Context, texts []string) map[string]string {
	hashes := make([]string, 0, len(texts))
	for _, t := range texts {
		if t != "" {
			hashes = append(hashes, sha256Of([]byte(t)))
		}
	}
	if len(hashes) == 0 {
		return nil
	}
	v, err := x.readScreens(ctx, hashes)
	if err != nil {
		return nil
	}
	return v
}

// bundlerCallNote is how a hit is called.
const bundlerCallNote = `service.call x402 {"schema":1,"method":"call","args":{"resource":ID,"body":THE_TOOL_ARGUMENTS},"max_cost":MAX_COST}; body follows input_schema (tools_get reads a tool's live schema and price).`

// BundlerNote says what the open bundler catalogue is, for the resources read
// and /capabilities, the x402 summary and /for-agents.
const BundlerNote = `SwarmMemo tools: service.read x402 tools_search finds about ` + X402ToolsApprox + ` paid-API endpoints for free, and tools_get reads one tool's live price and input schema; call a hit by its id ("tool:TOOL_ID") with service.call x402 call, its arguments as body. Callable are vetted tools (vetted: true), priced at most tools.max_price, not on the operator's denylist; a call is refused before any payment when the tool is not live or asks more, and charged what the tool bills, never more than its cost (the quote, fees included).`

// ReadRemote serves tools_search and tools_get after commit; every other
// read is Read's.
func (x *x402) ReadRemote(ctx context.Context, q allowance.Querier, c Call) (func(context.Context) (json.RawMessage, error), error) {
	if c.Method != "tools_search" && c.Method != "tools_get" {
		return nil, nil
	}
	if x.cfg == nil || x.fr == nil || !x.fr.b.Ready() {
		return nil, refusal("service_unavailable")
	}
	price := DefaultPrices()["x402.call"]
	if c.Prices != nil {
		price = c.Prices["x402.call"]
	}
	if c.Method == "tools_get" {
		name, err := bundlerToolArg(c.Args)
		if err != nil {
			return nil, err
		}
		tool, row, known, err := bundlerLookup(ctx, q, name, c.Now)
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context) (json.RawMessage, error) {
			return x.bundlerToolRead(bundlerCallerOf(ctx, c.Subject), tool, row, known, price)
		}, nil
	}
	s, err := x.parseBundlerSearch(c.Args)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) (json.RawMessage, error) {
		return x.bundlerSearchRead(bundlerCallerOf(ctx, c.Subject), s, price, c.Now)
	}, nil
}

func (x *x402) bundlerSearchRead(ctx context.Context, s bundlerSearch, price Price, now int64) (json.RawMessage, error) {
	f := x.fr
	f.mu.Lock()
	page, cached := f.searches.get(s.key(), time.Now())
	f.mu.Unlock()
	if !cached {
		raw, err := x.bundlerAPI(ctx, http.MethodPost, "/tools/search", s.body(), bundlerSearchBytes, bundlerSearchTimeout)
		if err != nil {
			return nil, err
		}
		if page, err = parseBundlerSearchPage(raw); err != nil {
			return nil, err
		}
		f.mu.Lock()
		f.searches.put(s.key(), page, time.Now(), bundlerSearchTTL)
		f.mu.Unlock()
		_ = x.bundlerRecord(ctx, page, now) // a failed record only leaves the tools unknown
	}
	texts := make([]string, 0, len(page.hits))
	for _, h := range page.hits {
		texts = append(texts, h.Description)
	}
	verdicts := x.verdictsFor(ctx, texts)
	hosts := x.bundlerHosts(ctx, page.hits)
	via := x.catalogueMatches(ctx, page.hits)
	ids := make([]string, len(page.hits))
	for i, h := range page.hits {
		ids[i] = h.ID
	}
	vets, _ := toolVets(ctx, x.db, ids...) // a failed read only shows the bundler's flags
	// hits are callable as they are; bought are the hits whose tool id is
	// not, bought through the vetted catalogue resource at their endpoint
	// (callable_via); later is the rest.
	hits, bought, later := []any{}, []any{}, []any{}
	anyListed := false
	for _, h := range page.hits {
		q := f.cachedQuote(h, time.Now())
		status := screenStatus(h.Description, verdicts)
		desc := h.Description
		if status == summaryWithheld {
			desc = ""
		}
		host := hosts[h.ID]
		if u, err := url.Parse(h.URL); host == "" && err == nil {
			host = strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
		}
		v, has := vets[h.ID]
		byOperator := has && !neverVetted(h.Category) && v.holds(host, h.Method, q.price, q.priced)
		vetted := h.Vetted || byOperator
		// The host the listing invokes when no descriptor recorded one: a
		// denylisted host is refused here as the call refuses it (C1057).
		why := f.whyNot(vetted, h.Category, host, q.price, q.priced)
		if why == "" && h.Live != nil && !*h.Live {
			why = "tool_unavailable"
		}
		e := map[string]any{"id": bundlerToolID(h.ID), "title": h.Title, "description": desc, "description_truncated": h.Truncated, "summary_status": status,
			"capabilities": h.Capabilities, "category": h.Category, "vetted": vetted, "max_cost": price.For(f.cfg.MaxCost)}
		if byOperator {
			e["vetted_by"], e["bundler_vetted"] = "operator", h.Vetted
		}
		res := via[h.ID]
		if res != nil {
			// C566: the same endpoint is a vetted catalogue resource, bought
			// with x402 call by its id. vetted, callable and why_not stay the
			// tool id's: a call of the id does what they say (C1193).
			e["callable_via"] = map[string]any{"method": "call", "resource": res.ID, "note": catalogueViaNote}
		}
		e["callable"] = why == ""
		if why != "" {
			e["why_not"] = why
		}
		if f.firstParty(h.PayTo) || f.firstParty(f.cachedPayTo(h.ID)) {
			e["first_party"] = true
		}
		q.put(e, price)
		anyListed = anyListed || q.listed
		if !q.listed && h.Priced && h.Price != q.price {
			// The index lists another price than the live one: say so.
			e["listed_price_usd"] = formatUnits(h.Price, 6)
		}
		if h.Live != nil {
			e["live"] = *h.Live
		}
		if h.Schema != nil {
			e["input_schema"] = h.Schema
		} else if h.SchemaCut {
			e["input_schema_truncated"] = true
		}
		// Callable hits first, in the bundler's order, then those bought
		// through a catalogue resource, then the rest: an agent looking for
		// something it can buy sees those at the top (Skitter 12b515db).
		switch {
		case why == "":
			hits = append(hits, e)
		case res != nil:
			bought = append(bought, e)
		default:
			later = append(later, e)
		}
	}
	// C1013: vetted, callable submitted resources that match the query are
	// hits of their own (unless a hit above is already bought through one):
	// one that names its host first, the rest after the callable tools.
	taken := map[string]bool{}
	for _, r := range via {
		taken[r.ID] = true
	}
	named, matched := x.submittedMatches(s, taken, price)
	hits = append(append(named, hits...), matched...)
	callable := len(hits)
	hits = append(append(hits, bought...), later...)
	out := map[string]any{
		"hits": hits, "matched": len(hits), "callable": callable, "partial": page.partial, "cached": cached,
		"tools": x.bundlerSummary(price), "call": bundlerCallNote,
		// Titles, descriptions and schemas are the bundler's listings' text.
		"text_is_untrusted": true,
	}
	if anyListed {
		out["price_note"] = bundlerListedNote
	}
	return canonicalJSON(out), nil
}

// submittedHitsMax bounds the submitted resources one tools_search lists.
const submittedHitsMax = 5

// submittedViaNote says how a submitted resource's hit is called.
const submittedViaNote = "a submitted listing: call it as service.call x402 call with this resource (no tool: prefix); query and body say what it takes."

// submittedMatches are the vetted, callable submitted catalogue resources
// (x402.submit) that every word of one of s's queries names (in the id,
// host, URL path, category or summary), not in taken: within s's
// max_price, HTTPS, neither denied nor adult or gambling, at most
// submittedHitsMax. named are those whose host or domain a query names, the
// rest are other. Each is a hit with callable_via, the C566 shape. The
// catalogue is the in-memory snapshot: no database.
func (x *x402) submittedMatches(s bundlerSearch, taken map[string]bool, price Price) (named, other []any) {
	angles := make([][]string, 0, len(s.queries))
	for _, q := range s.queries {
		var words []string
		for _, w := range strings.Fields(q) {
			if len(w) >= 3 && !toolsStopWords[w] {
				words = append(words, w)
			}
		}
		if len(words) > 0 {
			angles = append(angles, words)
		}
	}
	if len(angles) == 0 {
		return nil, nil
	}
	x.vettedEndpoints(func(r *X402Resource) {
		m := min(r.MaxAmount, x.cfg.PerCall)
		if len(named)+len(other) >= submittedHitsMax || taken[r.ID] || r.Proof() == "" || !r.Open || !strings.HasPrefix(r.canon, "https://") ||
			neverVetted(r.Category) || x.fr.denied(r.Category, r.host) || usdMicroOf(m, x.cfg.Decimals) > s.maxPrice {
			return
		}
		text := r.text + " " + strings.ToLower(r.canon)
		hit, byHost := false, false
		for _, words := range angles {
			if !slices.ContainsFunc(words, func(w string) bool { return !strings.Contains(text, w) }) {
				hit = true
				byHost = byHost || slices.ContainsFunc(words, func(w string) bool {
					w = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(w, "https://"), "http://"), "/")
					return w == r.host || (r.domain != "" && w == r.domain)
				})
			}
		}
		if !hit {
			return
		}
		taken[r.ID] = true
		query := r.Query
		if query == nil {
			query = []string{}
		}
		e := map[string]any{"id": r.ID, "title": strings.TrimPrefix(r.canon, "https://"), "description": r.servedSummary(), "summary_status": r.summaryStatus,
			"category": r.Category, "vetted": true, "callable": true, "source": "submitted", "proof": r.Proof(),
			"method": r.Method, "query": query, "body": r.Body, "max_price": formatUnits(m, x.cfg.Decimals), "max_cost": price.For(m),
			"callable_via": map[string]any{"method": "call", "resource": r.ID, "note": submittedViaNote}}
		if byHost {
			named = append(named, e)
		} else {
			other = append(other, e)
		}
	})
	return named, other
}

// catalogueViaNote says which index each of a hit's flags comes from.
const catalogueViaNote = "vetted, callable and why_not are the tool id's; the same endpoint is a vetted catalogue resource: buy it as service.call x402 call with this resource, not the tool id."

// catalogueViaFetches bounds the descriptors one search reads to learn a
// candidate hit's endpoint.
const catalogueViaFetches = 3

// catalogueMatches maps each hit that is the same endpoint as a vetted,
// callable catalogue resource (method and URL; pay_to too when the listing
// names one) to that resource. A hit whose listing leaves its endpoint out
// is a candidate when its id names the resource's slug (bazaar.SLUG); its
// descriptor (cached, at most catalogueViaFetches read per search) confirms
// it. The catalogue is the in-memory snapshot: no database. Runs after
// commit.
func (x *x402) catalogueMatches(ctx context.Context, hits []bundlerHit) map[string]*X402Resource {
	out := map[string]*X402Resource{}
	bySlug := map[string][]*X402Resource{}
	x.vettedEndpoints(func(r *X402Resource) {
		if s := x402Slug(r.URL); s != "" {
			bySlug[s] = append(bySlug[s], r)
		}
	})
	if len(bySlug) == 0 {
		return out
	}
	fetches := 0
	for _, h := range hits {
		_, slug, _ := strings.Cut(bundlerPublicTool(h.ID), ".")
		cands := bySlug[slug]
		if len(cands) == 0 {
			continue
		}
		method, u, payTo := h.Method, h.URL, h.PayTo
		if u == "" {
			x.fr.mu.Lock()
			d, ok := x.fr.descriptors.get(h.ID, time.Now())
			x.fr.mu.Unlock()
			if !ok && fetches < catalogueViaFetches {
				fetches++
				d, _ = x.bundlerDescriptorOf(ctx, h.ID)
			}
			method, u = d.method, d.url
			if len(payTo) == 0 {
				payTo = d.payTo
			}
		}
		for _, r := range cands {
			if sameEndpoint(r, method, u, payTo) {
				out[h.ID] = r
				break
			}
		}
	}
	return out
}

// vettedEndpoints calls fn with every catalogue resource the call method
// sells by URL: the pinned x402 ones, and the open ones that are callable
// (vetted, not denied, not demoted).
func (x *x402) vettedEndpoints(fn func(*X402Resource)) {
	for i := range x.cfg.Resources {
		if r := &x.cfg.Resources[i]; (r.Bundler == "" || r.Bundler == X402Bundler) && r.URL != "" {
			fn(r)
		}
	}
	if snap := x.cat.snap.Load(); snap != nil {
		for _, r := range snap.open {
			if x.callable(r) {
				fn(r)
			}
		}
	}
}

// sameEndpoint says whether r is the endpoint method and rawURL name
// (canonical URLs, a trailing slash aside), paid to one of payTo when that
// is known.
func sameEndpoint(r *X402Resource, method, rawURL string, payTo []EVMAddress) bool {
	if method == "" || method != r.Method {
		return false
	}
	a, b := canonicalX402URL(r.URL), canonicalX402URL(rawURL)
	if a == "" || strings.TrimSuffix(a, "/") != strings.TrimSuffix(b, "/") {
		return false
	}
	return len(payTo) == 0 || slices.Contains(payTo, r.PayTo)
}

// bundlerHosts are the hosts frames_tools knows for the hits (a call reads a
// tool's descriptor and records its host).
func (x *x402) bundlerHosts(ctx context.Context, hits []bundlerHit) map[string]string {
	out := map[string]string{}
	if len(hits) == 0 {
		return out
	}
	args := make([]any, len(hits))
	for i, h := range hits {
		args[i] = h.ID
	}
	rows, err := queryAll(ctx, x.db, func(r *sql.Rows) ([2]string, error) {
		var v [2]string
		return v, r.Scan(&v[0], &v[1])
	}, "SELECT id,host FROM frames_tools WHERE host<>'' AND id IN (?"+strings.Repeat(",?", len(hits)-1)+")", args...)
	if err != nil {
		return out
	}
	for _, v := range rows {
		out[v[0]] = v[1]
	}
	return out
}

// bundlerPayTo is the EVM addresses a tool's listing (a search hit or its
// descriptor) says its payments go to: pay_to, payTo or recipient on the
// listing, its payment block, or an x402 accepts[] entry of either. Anything
// else is skipped.
func bundlerPayTo(listing json.RawMessage) []EVMAddress {
	var out []EVMAddress
	var walk func(raw json.RawMessage, depth int)
	walk = func(raw json.RawMessage, depth int) {
		var o map[string]json.RawMessage
		if depth > 2 || json.Unmarshal(raw, &o) != nil || o == nil {
			return
		}
		for _, k := range []string{"pay_to", "payTo", "recipient"} {
			var text string
			if json.Unmarshal(o[k], &text) == nil {
				if a, ok := ParseEVMAddress(text); ok && a != (EVMAddress{}) {
					out = append(out, a)
				}
			}
		}
		if depth == 0 {
			walk(o["payment"], 1)
		}
		var accepts []json.RawMessage
		if json.Unmarshal(o["accepts"], &accepts) == nil {
			for i, a := range accepts {
				if i == 8 {
					break
				}
				walk(a, 2)
			}
		}
	}
	walk(listing, 0)
	return out
}

// firstParty says whether a tool paying to payTo is the bundler's own (C496):
// every one of them is the bundler's paying wallet (bundlers.frames.payer), so
// a call's money goes from the bundler back to itself, not to an outside
// seller. A list mixing in an outside wallet is not first party: a call may
// pay that seller, and its receipt would say so.
func (f *bundlerState) firstParty(payTo []EVMAddress) bool {
	if f.b.payer == (EVMAddress{}) || len(payTo) == 0 {
		return false
	}
	for _, a := range payTo {
		if a != f.b.payer {
			return false
		}
	}
	return true
}

// cachedPayTo is the pay-to of a tool's cached descriptor, if any: a hit
// whose listing leaves it out still shows first_party once tools_get read it.
func (f *bundlerState) cachedPayTo(tool string) []EVMAddress {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, _ := f.descriptors.get(tool, time.Now())
	return d.payTo
}

// bundlerSummary is the open catalogue's terms, in every bundler answer and
// the resources read.
func (x *x402) bundlerSummary(price Price) map[string]any {
	cu := func(n int64) string { return formatUnits(n, 6) }
	f := x.fr.cfg
	return map[string]any{"enabled": true, "max_price": cu(f.MaxPrice), "max_cost": price.For(f.MaxCost), "open_daily": cu(f.OpenDaily), "tool_daily": cu(f.ToolDaily),
		"allow_unvetted": f.AllowUnvetted, "without_key": f.Anonymous, "note": BundlerNote}
}

// One tool.

type bundlerToolArgs struct {
	ID string `json:"id"`
}

// bundlerToolArg is tools_get's tool name: its id with or without "tool:" (or
// the legacy "frames:").
func bundlerToolArg(raw json.RawMessage) (string, error) {
	var a bundlerToolArgs
	if err := StrictObject(raw, &a); err != nil {
		return "", err
	}
	tool := strings.TrimPrefix(strings.TrimPrefix(a.ID, BundlerPrefix), bundlerLegacyPrefix)
	if !bundlerToolRE.MatchString(tool) {
		return "", badArg(bundlerToolRule)
	}
	return tool, nil
}

// bundlerDescriptor is a tool's descriptor, cleaned.
type bundlerDescriptor struct {
	title, description string
	capabilities       []string
	category, host     string
	schema             json.RawMessage
	price              int64
	priced             bool
	payTo              []EVMAddress
	method, url        string // the endpoint it invokes
}

func (x *x402) bundlerDescriptorOf(ctx context.Context, tool string) (bundlerDescriptor, error) {
	f := x.fr
	f.mu.Lock()
	d, ok := f.descriptors.get(tool, time.Now())
	f.mu.Unlock()
	if ok {
		return d, nil
	}
	raw, err := x.bundlerAPI(ctx, http.MethodGet, "/tools/"+url.PathEscape(tool), nil, bundlerToolBytes, bundlerToolTimeout)
	if err != nil {
		return d, err
	}
	var w struct {
		ID           string   `json:"id"`
		Title        string   `json:"title"`
		Description  string   `json:"description"`
		Capabilities []string `json:"capabilities"`
		Invocation   struct {
			Method       string          `json:"method"`
			URL          string          `json:"url"`
			ParamsSchema json.RawMessage `json:"params_schema"`
		} `json:"invocation"`
		Payment struct {
			PriceHint json.RawMessage `json:"price_hint"`
		} `json:"payment"`
		Signals struct {
			Host string `json:"host"`
		} `json:"signals"`
	}
	if json.Unmarshal(raw, &w) != nil || w.ID != tool {
		return d, refusal("upstream_failed")
	}
	d = bundlerDescriptor{title: cleanText(w.Title, bundlerTitleRunes), description: cleanText(w.Description, bundlerDescriptionRunes), capabilities: bundlerCapabilities(w.Capabilities)}
	d.category = x402Categorize(d.title + " " + d.description + " " + strings.Join(d.capabilities, " "))
	if u, err := url.Parse(w.Invocation.URL); err == nil && (u.Scheme == "https" || u.Scheme == "http") {
		d.host = strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	}
	if d.host == "" {
		d.host = strings.TrimSuffix(strings.ToLower(w.Signals.Host), ".")
	}
	if !bundlerHostRE.MatchString(d.host) {
		d.host = ""
	}
	d.schema, _ = bundlerSchemaOf(w.Invocation.ParamsSchema)
	d.price, d.priced = bundlerPrice(w.Payment.PriceHint)
	d.payTo = bundlerPayTo(raw)
	d.method, d.url = strings.ToUpper(w.Invocation.Method), w.Invocation.URL
	f.mu.Lock()
	f.descriptors.put(tool, d, time.Now(), bundlerDescriptorTTL)
	f.mu.Unlock()
	if d.host != "" {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, _ = x.db.ExecContext(ctx, "UPDATE frames_tools SET host=? WHERE id=? AND host<>?", d.host, tool, d.host)
	}
	return d, nil
}

// bundlerProbe is a tool's live probe.
type bundlerProbe struct {
	live, payable bool
	price         int64
	priced        bool
	schema        json.RawMessage
}

// bundlerQuote is a tool's price and where it comes from: "live" (the
// bundler's probe, what a call is checked against) or "listed" (a listing's
// price_hint or the search index's snapshot, which may be stale). It is the one
// source of a tool's price_usd and cost (bundlerCost, fees included) in
// tools_search, tools_get and bundlerGate (C579).
type bundlerQuote struct {
	price          int64
	priced, listed bool
}

// liveQuote is the quote tools_get shows and bundlerGate holds a call to: the
// probe's price, else the descriptor's listed one.
func liveQuote(p bundlerProbe, d bundlerDescriptor) bundlerQuote {
	if p.priced {
		return bundlerQuote{price: p.price, priced: true}
	}
	return bundlerQuote{price: d.price, priced: d.priced, listed: d.priced}
}

// cost is what one call of the quoted tool may pay, in micro-USD.
func (q bundlerQuote) cost() int64 { return bundlerCost(q.price) }

// put writes the quote's price_usd, cost (credits) and price_source into e.
func (q bundlerQuote) put(e map[string]any, price Price) {
	if !q.priced {
		return
	}
	e["price_usd"], e["cost"] = formatUnits(q.price, 6), price.For(q.cost())
	e["price_source"] = map[bool]string{true: "listed", false: "live"}[q.listed]
}

// bundlerListedNote rides on a search answer whose hits show a listed price.
const bundlerListedNote = "price_source listed is the search index's price and may be stale; tools_get gives the live quote, and a call charges at most that quote's cost."

// cachedQuote is a hit's quote in a search: the live probe's when one is
// cached, else the index's listed price.
func (f *bundlerState) cachedQuote(h bundlerHit, now time.Time) bundlerQuote {
	f.mu.Lock()
	p, ok := f.probes.get(h.ID, now)
	f.mu.Unlock()
	if ok && p.priced {
		return bundlerQuote{price: p.price, priced: true}
	}
	return bundlerQuote{price: h.Price, priced: h.Priced, listed: h.Priced}
}

func (x *x402) bundlerProbeOf(ctx context.Context, tool string) (bundlerProbe, error) {
	f := x.fr
	f.mu.Lock()
	p, ok := f.probes.get(tool, time.Now())
	f.mu.Unlock()
	if ok {
		return p, nil
	}
	raw, err := x.bundlerAPI(ctx, http.MethodPost, "/tools/probe", map[string]any{"ids": []string{tool}}, bundlerProbeBytes, bundlerProbeTimeout)
	if err != nil {
		return p, err
	}
	var w struct {
		Results []struct {
			ID          string          `json:"id"`
			Live        bool            `json:"live"`
			Payable     bool            `json:"payable"`
			PriceUSD    json.RawMessage `json:"price_usd"`
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"results"`
	}
	if json.Unmarshal(raw, &w) != nil {
		return p, refusal("upstream_failed")
	}
	found := false
	for _, r := range w.Results {
		if r.ID == tool {
			found = true
			p = bundlerProbe{live: r.Live, payable: r.Payable}
			p.price, p.priced = bundlerPrice(r.PriceUSD)
			p.schema, _ = bundlerSchemaOf(r.InputSchema)
		}
	}
	if !found {
		return p, refusal("upstream_failed")
	}
	f.mu.Lock()
	f.probes.put(tool, p, time.Now(), bundlerProbeTTL)
	f.mu.Unlock()
	return p, nil
}

func (x *x402) bundlerToolRead(ctx context.Context, tool string, row bundlerTool, known bool, price Price) (json.RawMessage, error) {
	d, err := x.bundlerDescriptorOf(ctx, tool)
	if err != nil {
		return nil, err
	}
	p, err := x.bundlerProbeOf(ctx, tool)
	if err != nil {
		return nil, err
	}
	verdicts := x.verdictsFor(ctx, []string{d.description})
	status := screenStatus(d.description, verdicts)
	desc := d.description
	if status == summaryWithheld {
		desc = ""
	}
	out := map[string]any{"id": bundlerToolID(tool), "title": d.title, "description": desc, "summary_status": status, "capabilities": d.capabilities,
		"category": d.category, "host": d.host, "live": p.live, "payable": p.payable, "max_cost": price.For(x.fr.cfg.MaxCost),
		"tools": x.bundlerSummary(price), "call": bundlerCallNote, "text_is_untrusted": true}
	if x.fr.firstParty(d.payTo) {
		out["first_party"] = true
	}
	q := liveQuote(p, d)
	q.put(out, price)
	switch {
	case p.schema != nil:
		out["input_schema"] = p.schema
	case d.schema != nil:
		out["input_schema"] = d.schema
	}
	var why string
	if known {
		vetted := row.vetted
		if x.liveVet(ctx, tool, d, q.price, q.priced) {
			vetted = true
			out["vetted_by"], out["bundler_vetted"] = "operator", row.vetted
		}
		out["vetted"] = vetted
		why = x.fr.whyNot(vetted, d.category, d.host, q.price, q.priced)
	} else {
		// The bundler's vetting flag comes with search hits: a tool no recent
		// search returned cannot be called until one does.
		out["vetted"] = nil
		why = "tools_search_first"
	}
	if why == "" && (!p.live || !p.payable) {
		why = "tool_unavailable"
	}
	out["callable"] = why == ""
	if why != "" {
		out["why_not"] = why
	}
	return canonicalJSON(out), nil
}

// Calls.

// bundlerPlan is the plan of a call to the open bundler tool id names: priced
// at min(MaxCost, what the caller's max_cost covers). Pure.
func (x *x402) bundlerPlan(c Call, tool string, a x402Args) (x402Plan, error) {
	if !x.fr.b.Ready() {
		return x402Plan{}, refusal("service_unavailable")
	}
	switch {
	case !bundlerToolRE.MatchString(tool):
		return x402Plan{}, badArg(bundlerToolRule)
	case len(a.Query) > 0:
		return x402Plan{}, badArg("query is not taken by a tool call; put the tool's inputs in body.")
	case len(a.Body) > 0 && !isObject(a.Body):
		return x402Plan{}, badArg("body is a JSON object of the tool's inputs.")
	}
	n, err := x402Limit(a, &X402Resource{MaxResponseBytes: X402ResponseBytesMax})
	if err != nil {
		return x402Plan{}, err
	}
	limit := affordable(c.Price, c.MaxCost-x402ExtraCredits(n), x.fr.cfg.MaxCost)
	if limit <= 0 {
		return x402Plan{}, refusal("price_exceeds_max")
	}
	// Tool is the name as given until bundlerGate resolves it to the
	// upstream's id; ID is the public id either way.
	res := &X402Resource{ID: bundlerToolID(tool), URL: x.fr.b.endpoint, Method: http.MethodPost, Body: true, MaxAmount: limit,
		MaxResponseBytes: X402ResponseBytesMax, Timeout: X402TimeoutMax, Bundler: x.fr.b.Name(), Tool: tool, Category: "other", dynamic: true}
	p := x402Plan{res: res, url: res.URL, max: limit, limit: n, body: []byte("{}")}
	if len(a.Body) > 0 {
		var b bytes.Buffer
		if err := json.Compact(&b, a.Body); err != nil {
			return x402Plan{}, badArg("body is a JSON object of the tool's inputs.")
		}
		p.body = b.Bytes()
	}
	return p, nil
}

// affordable is the largest amount whose price is at most maxCost, at most
// limit; 0 when maxCost does not cover the base.
func affordable(price Price, maxCost, limit int64) int64 {
	if price.For(limit) <= maxCost {
		return limit
	}
	lo, hi := int64(0), limit // price.For(lo) <= maxCost < price.For(hi), when the base fits
	if price.For(0) > maxCost {
		return 0
	}
	for hi-lo > 1 {
		mid := lo + (hi-lo)/2
		if price.For(mid) <= maxCost {
			lo = mid
		} else {
			hi = mid
		}
	}
	return lo
}

// bundlerAdmit refuses, in the command's transaction, a call to a tool no
// recent search returned, one neither the bundler nor the operator vetted
// (unless the config allows it) and one whose category or known host is
// denied.
func (x *x402) bundlerAdmit(ctx context.Context, q allowance.Querier, name string, now int64) error {
	tool, row, known, err := bundlerLookup(ctx, q, name, now)
	if err != nil {
		return err
	}
	if !known {
		return refusal("x402_unknown_resource")
	}
	vetted := row.vetted || !neverVetted(row.category) && operatorVetted(ctx, q, tool, row.host)
	if why := x.fr.whyNot(vetted, row.category, row.host, 0, false); why != "" {
		return refusal(why)
	}
	return nil
}

// bundlerGate decides, after commit and before any money moves, whether the
// call may go on: the admission check again, the tool's host against the
// denylist, and a live probe: live and payable, priced at most max_price and
// at most what the caller's ceiling covers. It resolves the plan's tool name
// to the upstream's id and lowers the plan's maximum to the tool's cost
// (bundlerCost), the quote tools_get showed.
func (x *x402) bundlerGate(ctx context.Context, c Call, p *x402Plan) error {
	tool, row, known, err := bundlerLookup(ctx, x.db, p.res.Tool, c.Now)
	if err != nil {
		return refusal("upstream_failed")
	}
	if !known {
		return refusal("x402_unknown_resource")
	}
	p.res.Tool = tool
	d, err := x.bundlerDescriptorOf(ctx, tool)
	if err != nil {
		return err
	}
	// An operator vet (keybundler_vet.go) stands in for the bundler's flag
	// while it holds for the live descriptor and price, checked below once
	// the probe has the price.
	byOperator := !row.vetted && !x.fr.cfg.AllowUnvetted && operatorVetted(ctx, x.db, tool, d.host)
	if why := x.fr.whyNot(row.vetted || byOperator, row.category, d.host, 0, false); why != "" {
		return refusal(why)
	}
	if x.fr.denied(d.category, "") {
		return refusal("tool_denied")
	}
	pr, err := x.bundlerProbeOf(ctx, tool)
	if err != nil {
		return err
	}
	// The quote tools_get shows: the probe's price, else the descriptor's.
	q := liveQuote(pr, d)
	if byOperator && !x.liveVet(ctx, tool, d, q.price, q.priced) {
		return refusal("tool_unvetted")
	}
	switch {
	case !pr.live || !pr.payable:
		return refusal("tool_unavailable")
	case q.priced && x.fr.overCap(q.price):
		return refusal("tool_price_over_cap")
	case q.priced && q.cost() > p.max:
		return refusal("price_exceeds_max")
	}
	// Never more than the quote: the bundler may bill at most the tool's cost.
	if q.priced {
		p.max = q.cost()
	}
	p.res.bundlerSearch = row.searchID
	return nil
}

// Admit refuses a bundler tool call bundlerAdmit refuses, in the command's
// transaction, before anything is reserved. Other calls pass.
func (x *x402) Admit(ctx context.Context, q allowance.Querier, c Call) error {
	if x.fr == nil || c.Method != "call" {
		return nil
	}
	var a x402Args
	if err := StrictObject(c.Args, &a); err != nil {
		return err
	}
	if tool, ok := bundlerResource(a.Resource); ok {
		return x.bundlerAdmit(ctx, q, tool, c.Now)
	}
	return nil
}

// CheckAnonymous admits an unsigned call only to a bundler tool, and only
// while the config's anonymous is on; everything else x402 sells needs a
// key.
func (x *x402) CheckAnonymous(c Call) error {
	var a x402Args
	if err := StrictObject(c.Args, &a); err != nil {
		return err
	}
	if _, ok := bundlerResource(a.Resource); !ok || x.fr == nil || !x.fr.cfg.Anonymous {
		return refusal("anonymous_not_allowed")
	}
	return nil
}

// IsToolID reports whether a resource id names a SwarmMemo tool: "tool:NAME",
// or the legacy prefix 1.29.6 served.
func IsToolID(id string) bool {
	_, ok := bundlerResource(id)
	return ok
}

// bundlerResource reports whether a resource id names an open bundler tool,
// and the tool.
func bundlerResource(id string) (string, bool) {
	if tool, ok := strings.CutPrefix(id, BundlerPrefix); ok {
		return tool, true
	}
	return strings.CutPrefix(id, bundlerLegacyPrefix)
}

// Background screening.

// screenBundlerTools screens the descriptions of recently found tools that the
// screen has not seen, at most bundlerScreenBatch an hour, and records each
// verdict in x402_summary_screens (shared with the catalogue: the verdict is
// the text's). Background only; no connection is held while the classifier
// runs.
func (x *x402) screenBundlerTools(ctx context.Context, now int64) (int, error) {
	if x.fr == nil || !screenerUp(ctx, x.screener) {
		return 0, nil
	}
	texts, err := queryAll(ctx, x.db, func(r *sql.Rows) (string, error) {
		var s string
		return s, r.Scan(&s)
	}, "SELECT description FROM frames_tools WHERE description<>'' AND seen_at>=? ORDER BY seen_at DESC LIMIT 200", now-7*86400)
	if err != nil {
		return 0, err
	}
	known := x.verdictsFor(ctx, texts)
	verdicts := map[string]string{}
	for _, t := range texts {
		if len(verdicts) >= bundlerScreenBatch {
			break
		}
		hash := sha256Of([]byte(t))
		if _, done := known[hash]; done {
			continue
		}
		if _, done := verdicts[hash]; done {
			continue
		}
		res, err := x.screener.ScreenText(ctx, t, "tool", x402ScreenIntent)
		if err != nil {
			break
		}
		categories, ok := res.categories()
		if !ok {
			break
		}
		verdicts[hash] = screenVerdict(categories, ScreenThreshold)
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	for hash, verdict := range verdicts {
		if _, err := x.db.ExecContext(wctx, "INSERT INTO x402_summary_screens(hash,verdict,screened_at) VALUES(?,?,?) ON CONFLICT(hash) DO NOTHING", hash, verdict, now); err != nil {
			return 0, err
		}
	}
	return len(verdicts), nil
}

// workBundler starts the background screen when one is due.
func (x *x402) workBundler(now int64) {
	if x.fr == nil || x.screener == nil || now-x.fr.screened.Load() < bundlerScreenEvery || !x.fr.screening.CompareAndSwap(false, true) {
		return
	}
	x.fr.screened.Store(now)
	go func() {
		defer x.fr.screening.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), x402ScreenDeadline)
		defer cancel()
		_, _ = x.screenBundlerTools(ctx, now)
	}()
}
