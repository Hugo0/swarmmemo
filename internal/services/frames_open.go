package services

// The open Frames catalogue: with bundlers.frames.open, every tool of the
// Frames API (~37k paid tools behind one account) is callable through x402
// without an allowlist entry, as resource "frames:TOOL_ID".
//
// Finding a tool. service.read x402 frames_search passes the agent's query
// to Frames' free search and answers a trimmed hit list; frames_tool reads
// one tool (Frames' free descriptor and probe). Both need the network, so
// they run after the command's transaction has committed (RemoteReader),
// under the engine's per-caller read limit, a cache (searches 10 minutes,
// descriptors an hour, probes 5 minutes) and a global bound on upstream
// requests a minute. Upstream text (titles, descriptions, schemas) is
// untrusted: control characters are stripped, every field is capped, a
// description SwarmMemo's text screen flagged is withheld, and each answer
// says text_is_untrusted. Every hit is recorded in frames_tools with Frames'
// own vetting flag, which a call needs: a tool no search has returned in 30
// days is unknown.
//
// Calling a tool. The quote is the board's price at min(max_price, the
// caller's max_cost); nothing upstream is asked inside the transaction. The
// admission check (in the transaction, a read of frames_tools) refuses an
// unknown tool, a tool Frames has not vetted (unless allow_unvetted) and a
// denied category. After commit, before any money moves, the tool's
// descriptor (its host, against the operator's denylist) and a probe (live,
// payable, its price against max_price and the caller's ceiling) decide;
// then the call goes through frames.Exchange exactly as an allowlisted
// Frames tool: reserved first against every cap and the Frames sub-caps
// (open_daily for all Frames tools, tool_daily per tool), charged what
// Frames reports, refunded on a 4xx, one payment per request_id.

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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"swarmmemo/internal/allowance"
)

// FramesPrefix starts the resource id of an open Frames tool.
const FramesPrefix = "frames:"

// Open Frames bounds.
const (
	framesSearchTTL     = 10 * time.Minute
	framesDescriptorTTL = time.Hour
	framesProbeTTL      = 5 * time.Minute
	framesCacheMax      = 1024 // entries of each cache
	// framesUpstreamPerMinute bounds the requests SwarmMemo sends Frames'
	// free endpoints a minute, for every caller together (cache misses).
	framesUpstreamPerMinute = 120
	// Timeouts of Frames' free endpoints: a search runs its angles in
	// parallel (seconds each), a probe asks the seller.
	framesSearchTimeout    = 15 * time.Second
	framesToolTimeout      = 8 * time.Second
	framesProbeTimeout     = 12 * time.Second
	framesSearchBytes      = 512 << 10
	framesToolBytes        = 128 << 10
	framesProbeBytes       = 64 << 10
	framesHitsMax          = 25
	framesQueryBytes       = 200
	framesQueriesMax       = 4
	framesTitleRunes       = 120
	framesDescriptionRunes = 600
	framesCapabilitiesMax  = 8
	framesCapabilityRunes  = 32
	framesSchemaBytes      = 2048
	// framesToolStale is how long a search hit lets the tool be called.
	framesToolStale = 30 * 86400
	// framesScreenEvery and framesScreenBatch bound the background screen of
	// tool descriptions: at most 10 texts an hour.
	framesScreenEvery = 3600
	framesScreenBatch = 10
)

const framesSchema = `
CREATE TABLE IF NOT EXISTS frames_tools (
 id TEXT PRIMARY KEY, vetted INTEGER NOT NULL, title TEXT NOT NULL DEFAULT '', description TEXT NOT NULL DEFAULT '',
 category TEXT NOT NULL DEFAULT '', host TEXT NOT NULL DEFAULT '', search_id TEXT NOT NULL DEFAULT '', seen_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS frames_tools_seen ON frames_tools(seen_at);
`

// FramesOpen is the open Frames catalogue's validated configuration.
// Amounts are micro-USD.
type FramesOpen struct {
	MaxPrice  int64 // the most one call may pay
	OpenDaily int64 // all open Frames calls, per UTC day
	ToolDaily int64 // one tool, per UTC day
	// AllowUnvetted makes tools Frames has not vetted callable too.
	AllowUnvetted bool
	// Anonymous lets an unsigned service.call reach Frames tools, billed to
	// the caller's network's free daily credit.
	Anonymous bool
}

var (
	framesToolRE       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@-]{0,199}$`)
	framesCapabilityRE = regexp.MustCompile(`^[a-z0-9][a-z0-9 _./-]{0,63}$`)
	framesSearchIDRE   = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,80}$`)
	framesHostRE       = regexp.MustCompile(`^[a-z0-9.-]{1,253}$`)
)

// parseFramesOpen validates the open catalogue's fields of bundlers.frames;
// nil when open is false.
func (c *X402Config) parseFramesOpen(f *x402FramesFile) (*FramesOpen, error) {
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
	o := &FramesOpen{AllowUnvetted: f.AllowUnvetted, Anonymous: f.Anonymous}
	var err error
	if o.MaxPrice, err = amount("max_price", f.MaxPrice, "0.02", 1, c.PerCall); err != nil {
		return nil, err
	}
	if o.OpenDaily, err = amount("open_daily", f.OpenDaily, "1", o.MaxPrice, c.GlobalDaily); err != nil {
		return nil, err
	}
	if o.ToolDaily, err = amount("tool_daily", f.ToolDaily, "0.25", o.MaxPrice, o.OpenDaily); err != nil {
		return nil, err
	}
	return o, nil
}

// framesBundler is the configured Frames bundler, or nil.
func (c *X402Config) framesBundler() *frames {
	b, _ := c.bundler("frames").(*frames)
	return b
}

// ttlCache is a small map of values that expire, bounded at framesCacheMax
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
	if _, ok := c.m[k]; !ok && len(c.m) >= framesCacheMax {
		for key, e := range c.m {
			if !now.Before(e.until) {
				delete(c.m, key)
			}
		}
		if len(c.m) >= framesCacheMax {
			clear(c.m)
		}
	}
	c.m[k] = ttlEntry[T]{v: v, until: now.Add(ttl)}
}

// framesState is the provider's open Frames catalogue.
type framesState struct {
	b    *frames
	cfg  *FramesOpen
	deny *x402DenyIndex // the operator's denylist, indexed once

	mu          sync.Mutex
	searches    ttlCache[framesSearchPage]
	descriptors ttlCache[framesDescriptor]
	probes      ttlCache[framesProbe]
	minute      int64 // the upstream window's minute and its requests
	requests    int

	screening atomic.Bool
	screened  atomic.Int64 // when descriptions were last screened (the store's clock)
}

func newFramesState(cfg *X402Config, b *frames) *framesState {
	return &framesState{b: b, cfg: b.open, deny: cfg.denyIndex(nil)}
}

// allowUpstream counts one request to Frames against the global minute.
func (f *framesState) allowUpstream(now time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	minute := now.Unix() / 60
	if f.minute != minute {
		f.minute, f.requests = minute, 0
	}
	if f.requests >= framesUpstreamPerMinute {
		return &allowance.Err{Code: "request_rate", RetryAfter: int(60 - now.Unix()%60)}
	}
	f.requests++
	return nil
}

// framesAPI sends one request to Frames' API through the SSRF-safe client,
// with our key, reading at most limit bytes of a 2xx answer. The key is
// never part of an error.
func (x *x402) framesAPI(ctx context.Context, method, path string, body any, limit int, timeout time.Duration) ([]byte, error) {
	if err := x.fr.allowUpstream(time.Now()); err != nil {
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
	req.Header.Set("User-Agent", "SwarmMemo-x402/1 (+https://swarmmemo.com/protocol.md)")
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
		return nil, &allowance.Err{Code: "upstream_busy", RetryAfter: 30}
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

// framesCapabilities are a hit's capabilities, cleaned: lowercase words, at
// most framesCapabilitiesMax.
func framesCapabilities(in []string) []string {
	out := []string{}
	for _, c := range in {
		c = strings.ToLower(cleanText(c, framesCapabilityRunes))
		if framesCapabilityRE.MatchString(c) && len(out) < framesCapabilitiesMax {
			out = append(out, c)
		}
	}
	return out
}

// framesSchemaOf is an input schema compacted, or nil when it is not a JSON
// object or longer than framesSchemaBytes (cut reports that).
func framesSchemaOf(raw json.RawMessage) (schema json.RawMessage, cut bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false
	}
	var b bytes.Buffer
	if !isObject(raw) || json.Compact(&b, raw) != nil || !utf8.Valid(b.Bytes()) {
		return nil, false
	}
	if b.Len() > framesSchemaBytes {
		return nil, true
	}
	return json.RawMessage(b.Bytes()), false
}

// framesPrice reads a USD price Frames gave as a number or a string into
// micro-USD, rounded up; false for anything else (negative, not finite, or
// over $1,000).
func framesPrice(raw json.RawMessage) (int64, bool) {
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

type framesSearchArgs struct {
	Query      string   `json:"query"`
	Queries    []string `json:"queries"`
	Capability string   `json:"capability"`
	MaxPrice   string   `json:"max_price"`
}

// framesSearch is a validated search: the upstream's body and its cache key.
type framesSearch struct {
	queries    []string
	capability string
	maxPrice   int64
}

func (s framesSearch) key() string {
	return strings.Join(s.queries, "\x1f") + "\x1e" + s.capability + "\x1e" + strconv.FormatInt(s.maxPrice, 10)
}

func (s framesSearch) body() map[string]any {
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
	if !utf8.ValidString(q) || len(q) > framesQueryBytes || strings.IndexFunc(q, func(r rune) bool { return unicode.IsControl(r) && !unicode.IsSpace(r) }) >= 0 {
		return ""
	}
	return strings.Join(strings.Fields(strings.ToLower(q)), " ")
}

func (x *x402) parseFramesSearch(raw json.RawMessage) (framesSearch, error) {
	var a framesSearchArgs
	if err := StrictObject(raw, &a); err != nil {
		return framesSearch{}, err
	}
	s := framesSearch{maxPrice: x.fr.cfg.MaxPrice}
	switch {
	case a.Query != "" && len(a.Queries) == 0:
		a.Queries = []string{a.Query}
	case a.Query == "" && len(a.Queries) >= 2 && len(a.Queries) <= framesQueriesMax:
	default:
		return framesSearch{}, refusal("invalid_service_data")
	}
	seen := map[string]bool{}
	for _, q := range a.Queries {
		n := normalizeQuery(q)
		if n == "" || seen[n] {
			return framesSearch{}, refusal("invalid_service_data")
		}
		seen[n] = true
		s.queries = append(s.queries, n)
	}
	if a.Capability != "" {
		s.capability = strings.ToLower(strings.TrimSpace(a.Capability))
		if !framesCapabilityRE.MatchString(s.capability) {
			return framesSearch{}, refusal("invalid_service_data")
		}
	}
	if a.MaxPrice != "" {
		n, ok := parseUnits(a.MaxPrice, 6)
		if !ok || n <= 0 {
			return framesSearch{}, refusal("invalid_service_data")
		}
		s.maxPrice = min(n, s.maxPrice)
	}
	return s, nil
}

// framesHit is one search hit, cleaned.
type framesHit struct {
	ID, Title, Description string
	Truncated              bool
	Capabilities           []string
	Category               string
	Vetted                 bool
	Price                  int64 // micro-USD; Priced says whether it is known
	Priced, Probed         bool  // Probed: the price is a live probe's, not the listing's
	Live                   *bool
	Schema                 json.RawMessage
	SchemaCut              bool
}

type framesSearchPage struct {
	searchID string
	partial  bool
	hits     []framesHit
}

type framesHitWire struct {
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
}

// parseFramesSearchPage reads Frames' search answer, keeping valid hits.
func parseFramesSearchPage(raw []byte) (framesSearchPage, error) {
	var w struct {
		SearchID string            `json:"search_id"`
		Hits     []json.RawMessage `json:"hits"`
		Partial  bool              `json:"partial"`
	}
	if err := json.Unmarshal(raw, &w); err != nil {
		return framesSearchPage{}, refusal("upstream_failed")
	}
	page := framesSearchPage{partial: w.Partial}
	if framesSearchIDRE.MatchString(w.SearchID) {
		page.searchID = w.SearchID
	}
	seen := map[string]bool{}
	for _, rawHit := range w.Hits {
		if len(page.hits) >= framesHitsMax {
			break
		}
		var h framesHitWire
		if json.Unmarshal(rawHit, &h) != nil || !framesToolRE.MatchString(h.ID) || seen[h.ID] {
			continue
		}
		seen[h.ID] = true
		hit := framesHit{ID: h.ID, Title: cleanText(h.Title, framesTitleRunes), Description: cleanText(h.Description, framesDescriptionRunes),
			Truncated: h.DescriptionTruncated, Capabilities: framesCapabilities(h.Capabilities), Vetted: !h.Unvetted, Live: h.Live}
		hit.Truncated = hit.Truncated || utf8.RuneCountInString(hit.Description) >= framesDescriptionRunes
		hit.Category = x402Categorize(hit.Title + " " + hit.Description + " " + strings.Join(hit.Capabilities, " "))
		if p, ok := framesPrice(h.PriceUSD); ok && h.InvokeReady {
			hit.Price, hit.Priced, hit.Probed = p, true, true
		} else if p, ok := framesPrice(h.Payment.PriceHint); ok {
			hit.Price, hit.Priced = p, true
		}
		hit.Schema, hit.SchemaCut = framesSchemaOf(h.InputSchema)
		page.hits = append(page.hits, hit)
	}
	return page, nil
}

// framesRecord upserts the hits into frames_tools: Frames' vetting flag, the
// text to screen, the search that found them. Runs after commit.
func (x *x402) framesRecord(ctx context.Context, page framesSearchPage, now int64) error {
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

// framesTool is a tool's row in frames_tools.
type framesTool struct {
	vetted                   bool
	category, host, searchID string
}

// framesToolRow reads tool's row; false when no search returned it in the
// last framesToolStale seconds.
func framesToolRow(ctx context.Context, q allowance.Querier, tool string, now int64) (framesTool, bool, error) {
	var t framesTool
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
	return t, now-seen <= framesToolStale, nil
}

// whyNot says why a tool is not callable now ("" when it is): Frames has not
// vetted it, its category or host is denied, or its price is over the cap.
func (f *framesState) whyNot(vetted bool, category, host string, price int64, priced bool) string {
	switch {
	case !vetted && !f.cfg.AllowUnvetted:
		return "frames_unvetted"
	case f.denied(category, host):
		return "frames_denied"
	case priced && price > f.cfg.MaxPrice:
		return "frames_price_over_cap"
	}
	return ""
}

// denied applies the operator's denylist to a tool's category and host.
func (f *framesState) denied(category, host string) bool {
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

// framesCallNote is how a hit is called.
const framesCallNote = `service.call x402 {"schema":1,"method":"call","args":{"resource":ID,"body":THE_TOOL_ARGUMENTS},"max_cost":MAX_COST}; body follows input_schema (frames_tool reads a tool's live schema and price).`

// FramesNote says what the open Frames catalogue is, for the resources read
// and /capabilities.
const FramesNote = `Frames tools: service.read x402 frames_search finds tools across the Frames catalogue (about 37,000 paid APIs) for free; call a hit by its id ("frames:TOOL_ID") with service.call x402 call, its arguments as body. Callable are tools Frames vetted (frames_vetted: true), priced at most frames.max_price, not on the operator's denylist; a call is refused before any payment when the tool is not live or asks more, and charged what Frames bills.`

// ReadRemote serves frames_search and frames_tool after commit; every other
// read is Read's.
func (x *x402) ReadRemote(ctx context.Context, q allowance.Querier, c Call) (func(context.Context) (json.RawMessage, error), error) {
	if c.Method != "frames_search" && c.Method != "frames_tool" {
		return nil, nil
	}
	if x.cfg == nil || x.fr == nil || !x.fr.b.Ready() {
		return nil, refusal("service_unavailable")
	}
	price := DefaultPrices()["x402.call"]
	if c.Prices != nil {
		price = c.Prices["x402.call"]
	}
	if c.Method == "frames_tool" {
		tool, err := framesToolArg(c.Args)
		if err != nil {
			return nil, err
		}
		row, known, err := framesToolRow(ctx, q, tool, c.Now)
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context) (json.RawMessage, error) {
			return x.framesToolRead(ctx, tool, row, known, price)
		}, nil
	}
	s, err := x.parseFramesSearch(c.Args)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) (json.RawMessage, error) { return x.framesSearchRead(ctx, s, price, c.Now) }, nil
}

func (x *x402) framesSearchRead(ctx context.Context, s framesSearch, price Price, now int64) (json.RawMessage, error) {
	f := x.fr
	f.mu.Lock()
	page, cached := f.searches.get(s.key(), time.Now())
	f.mu.Unlock()
	if !cached {
		raw, err := x.framesAPI(ctx, http.MethodPost, "/tools/search", s.body(), framesSearchBytes, framesSearchTimeout)
		if err != nil {
			return nil, err
		}
		if page, err = parseFramesSearchPage(raw); err != nil {
			return nil, err
		}
		f.mu.Lock()
		f.searches.put(s.key(), page, time.Now(), framesSearchTTL)
		f.mu.Unlock()
		_ = x.framesRecord(ctx, page, now) // a failed record only leaves the tools unknown
	}
	texts := make([]string, 0, len(page.hits))
	for _, h := range page.hits {
		texts = append(texts, h.Description)
	}
	verdicts := x.verdictsFor(ctx, texts)
	hosts := x.framesHosts(ctx, page.hits)
	hits := []any{}
	for _, h := range page.hits {
		status := screenStatus(h.Description, verdicts)
		desc := h.Description
		if status == summaryWithheld {
			desc = ""
		}
		why := f.whyNot(h.Vetted, h.Category, hosts[h.ID], h.Price, h.Priced)
		if why == "" && h.Live != nil && !*h.Live {
			why = "frames_unavailable"
		}
		e := map[string]any{"id": FramesPrefix + h.ID, "title": h.Title, "description": desc, "description_truncated": h.Truncated, "summary_status": status,
			"capabilities": h.Capabilities, "category": h.Category, "frames_vetted": h.Vetted, "callable": why == "", "max_cost": price.For(f.cfg.MaxPrice)}
		if why != "" {
			e["why_not"] = why
		}
		if h.Priced {
			e["price_usd"], e["cost"] = formatUnits(h.Price, 6), price.For(h.Price)
			e["price_source"] = map[bool]string{true: "probe", false: "listing"}[h.Probed]
		}
		if h.Live != nil {
			e["live"] = *h.Live
		}
		if h.Schema != nil {
			e["input_schema"] = h.Schema
		} else if h.SchemaCut {
			e["input_schema_truncated"] = true
		}
		hits = append(hits, e)
	}
	return marshalNoEscape(map[string]any{
		"hits": hits, "matched": len(hits), "partial": page.partial, "cached": cached,
		"frames": x.framesSummary(price), "call": framesCallNote,
		// Titles, descriptions and schemas are Frames' listings' text.
		"text_is_untrusted": true,
	}), nil
}

// framesHosts are the hosts frames_tools knows for the hits (a call reads a
// tool's descriptor and records its host).
func (x *x402) framesHosts(ctx context.Context, hits []framesHit) map[string]string {
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

// framesSummary is the open catalogue's terms, in every Frames answer and
// the resources read.
func (x *x402) framesSummary(price Price) map[string]any {
	cu := func(n int64) string { return formatUnits(n, 6) }
	f := x.fr.cfg
	return map[string]any{"enabled": true, "max_price": cu(f.MaxPrice), "max_cost": price.For(f.MaxPrice), "open_daily": cu(f.OpenDaily), "tool_daily": cu(f.ToolDaily),
		"allow_unvetted": f.AllowUnvetted, "without_key": f.Anonymous, "note": FramesNote}
}

// One tool.

type framesToolArgs struct {
	ID string `json:"id"`
}

// framesToolArg is frames_tool's tool id, with or without "frames:".
func framesToolArg(raw json.RawMessage) (string, error) {
	var a framesToolArgs
	if err := StrictObject(raw, &a); err != nil {
		return "", err
	}
	tool := strings.TrimPrefix(a.ID, FramesPrefix)
	if !framesToolRE.MatchString(tool) {
		return "", refusal("invalid_service_data")
	}
	return tool, nil
}

// framesDescriptor is a tool's descriptor, cleaned.
type framesDescriptor struct {
	title, description string
	capabilities       []string
	category, host     string
	schema             json.RawMessage
	price              int64
	priced             bool
}

func (x *x402) framesDescriptorOf(ctx context.Context, tool string) (framesDescriptor, error) {
	f := x.fr
	f.mu.Lock()
	d, ok := f.descriptors.get(tool, time.Now())
	f.mu.Unlock()
	if ok {
		return d, nil
	}
	raw, err := x.framesAPI(ctx, http.MethodGet, "/tools/"+url.PathEscape(tool), nil, framesToolBytes, framesToolTimeout)
	if err != nil {
		return d, err
	}
	var w struct {
		ID           string   `json:"id"`
		Title        string   `json:"title"`
		Description  string   `json:"description"`
		Capabilities []string `json:"capabilities"`
		Invocation   struct {
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
	d = framesDescriptor{title: cleanText(w.Title, framesTitleRunes), description: cleanText(w.Description, framesDescriptionRunes), capabilities: framesCapabilities(w.Capabilities)}
	d.category = x402Categorize(d.title + " " + d.description + " " + strings.Join(d.capabilities, " "))
	if u, err := url.Parse(w.Invocation.URL); err == nil && (u.Scheme == "https" || u.Scheme == "http") {
		d.host = strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	}
	if d.host == "" {
		d.host = strings.TrimSuffix(strings.ToLower(w.Signals.Host), ".")
	}
	if !framesHostRE.MatchString(d.host) {
		d.host = ""
	}
	d.schema, _ = framesSchemaOf(w.Invocation.ParamsSchema)
	d.price, d.priced = framesPrice(w.Payment.PriceHint)
	f.mu.Lock()
	f.descriptors.put(tool, d, time.Now(), framesDescriptorTTL)
	f.mu.Unlock()
	if d.host != "" {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, _ = x.db.ExecContext(ctx, "UPDATE frames_tools SET host=? WHERE id=? AND host<>?", d.host, tool, d.host)
	}
	return d, nil
}

// framesProbe is a tool's live probe.
type framesProbe struct {
	live, payable bool
	price         int64
	priced        bool
	schema        json.RawMessage
}

func (x *x402) framesProbeOf(ctx context.Context, tool string) (framesProbe, error) {
	f := x.fr
	f.mu.Lock()
	p, ok := f.probes.get(tool, time.Now())
	f.mu.Unlock()
	if ok {
		return p, nil
	}
	raw, err := x.framesAPI(ctx, http.MethodPost, "/tools/probe", map[string]any{"ids": []string{tool}}, framesProbeBytes, framesProbeTimeout)
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
			p = framesProbe{live: r.Live, payable: r.Payable}
			p.price, p.priced = framesPrice(r.PriceUSD)
			p.schema, _ = framesSchemaOf(r.InputSchema)
		}
	}
	if !found {
		return p, refusal("upstream_failed")
	}
	f.mu.Lock()
	f.probes.put(tool, p, time.Now(), framesProbeTTL)
	f.mu.Unlock()
	return p, nil
}

func (x *x402) framesToolRead(ctx context.Context, tool string, row framesTool, known bool, price Price) (json.RawMessage, error) {
	d, err := x.framesDescriptorOf(ctx, tool)
	if err != nil {
		return nil, err
	}
	p, err := x.framesProbeOf(ctx, tool)
	if err != nil {
		return nil, err
	}
	verdicts := x.verdictsFor(ctx, []string{d.description})
	status := screenStatus(d.description, verdicts)
	desc := d.description
	if status == summaryWithheld {
		desc = ""
	}
	out := map[string]any{"id": FramesPrefix + tool, "title": d.title, "description": desc, "summary_status": status, "capabilities": d.capabilities,
		"category": d.category, "host": d.host, "live": p.live, "payable": p.payable, "max_cost": price.For(x.fr.cfg.MaxPrice),
		"frames": x.framesSummary(price), "call": framesCallNote, "text_is_untrusted": true}
	priceNow, priced := p.price, p.priced
	if !priced {
		priceNow, priced = d.price, d.priced
	}
	if priced {
		out["price_usd"], out["cost"] = formatUnits(priceNow, 6), price.For(priceNow)
	}
	switch {
	case p.schema != nil:
		out["input_schema"] = p.schema
	case d.schema != nil:
		out["input_schema"] = d.schema
	}
	var why string
	if known {
		out["frames_vetted"] = row.vetted
		why = x.fr.whyNot(row.vetted, d.category, d.host, priceNow, priced)
	} else {
		// Frames' vetting flag comes with search hits: a tool no recent
		// search returned cannot be called until one does.
		out["frames_vetted"] = nil
		why = "frames_search_first"
	}
	if why == "" && (!p.live || !p.payable) {
		why = "frames_unavailable"
	}
	out["callable"] = why == ""
	if why != "" {
		out["why_not"] = why
	}
	return marshalNoEscape(out), nil
}

// Calls.

// framesPlan is the plan of a call to the open Frames tool id names: priced
// at min(max_price, what the caller's max_cost covers). Pure.
func (x *x402) framesPlan(c Call, tool string, a x402Args) (x402Plan, error) {
	if !x.fr.b.Ready() {
		return x402Plan{}, refusal("service_unavailable")
	}
	if !framesToolRE.MatchString(tool) || len(a.Query) > 0 || (len(a.Body) > 0 && !isObject(a.Body)) {
		return x402Plan{}, refusal("invalid_service_data")
	}
	limit := affordable(c.Price, c.MaxCost, min(x.fr.cfg.MaxPrice, x.cfg.PerCall))
	if limit <= 0 {
		return x402Plan{}, refusal("price_exceeds_max")
	}
	res := &X402Resource{ID: FramesPrefix + tool, URL: x.fr.b.endpoint, Method: http.MethodPost, Body: true, MaxAmount: limit,
		MaxResponseBytes: X402ResponseBytesMax, Timeout: X402TimeoutMax, Bundler: x.fr.b.Name(), Tool: tool, Category: "other", dynamic: true}
	p := x402Plan{res: res, url: res.URL, max: limit, body: []byte("{}")}
	if len(a.Body) > 0 {
		var b bytes.Buffer
		if err := json.Compact(&b, a.Body); err != nil {
			return x402Plan{}, refusal("invalid_service_data")
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

// framesAdmit refuses, in the command's transaction, a call to a tool no
// recent search returned, one Frames has not vetted (unless the config allows
// it) and one whose category or known host is denied.
func (x *x402) framesAdmit(ctx context.Context, q allowance.Querier, tool string, now int64) error {
	row, known, err := framesToolRow(ctx, q, tool, now)
	if err != nil {
		return err
	}
	if !known {
		return refusal("x402_unknown_resource")
	}
	if why := x.fr.whyNot(row.vetted, row.category, row.host, 0, false); why != "" {
		return refusal(why)
	}
	return nil
}

// framesGate decides, after commit and before any money moves, whether the
// call may go on: the admission check again, the tool's host against the
// denylist, and a live probe: live and payable, priced at most max_price and
// at most what the caller's ceiling covers.
func (x *x402) framesGate(ctx context.Context, c Call, p x402Plan) error {
	tool := p.res.Tool
	row, known, err := framesToolRow(ctx, x.db, tool, c.Now)
	if err != nil {
		return refusal("upstream_failed")
	}
	if !known {
		return refusal("x402_unknown_resource")
	}
	d, err := x.framesDescriptorOf(ctx, tool)
	if err != nil {
		return err
	}
	if why := x.fr.whyNot(row.vetted, row.category, d.host, 0, false); why != "" {
		return refusal(why)
	}
	if x.fr.denied(d.category, "") {
		return refusal("frames_denied")
	}
	pr, err := x.framesProbeOf(ctx, tool)
	if err != nil {
		return err
	}
	switch {
	case !pr.live || !pr.payable:
		return refusal("frames_unavailable")
	case pr.priced && pr.price > x.fr.cfg.MaxPrice:
		return refusal("frames_price_over_cap")
	case pr.priced && pr.price > p.max:
		return refusal("price_exceeds_max")
	}
	p.res.framesSearch = row.searchID
	return nil
}

// Admit refuses a Frames tool call framesAdmit refuses, in the command's
// transaction, before anything is reserved. Other calls pass.
func (x *x402) Admit(ctx context.Context, q allowance.Querier, c Call) error {
	if x.fr == nil || c.Method != "call" {
		return nil
	}
	var a x402Args
	if err := StrictObject(c.Args, &a); err != nil {
		return err
	}
	if tool, ok := framesResource(a.Resource); ok {
		return x.framesAdmit(ctx, q, tool, c.Now)
	}
	return nil
}

// CheckAnonymous admits an unsigned call only to a Frames tool, and only
// while the config's anonymous is on; everything else x402 sells needs a
// key.
func (x *x402) CheckAnonymous(c Call) error {
	var a x402Args
	if err := StrictObject(c.Args, &a); err != nil {
		return err
	}
	if _, ok := framesResource(a.Resource); !ok || x.fr == nil || !x.fr.cfg.Anonymous {
		return refusal("anonymous_not_allowed")
	}
	return nil
}

// framesResource reports whether a resource id names an open Frames tool,
// and the tool.
func framesResource(id string) (string, bool) {
	return strings.CutPrefix(id, FramesPrefix)
}

// Background screening.

// screenFrames screens the descriptions of recently found tools that the
// screen has not seen, at most framesScreenBatch an hour, and records each
// verdict in x402_summary_screens (shared with the catalogue: the verdict is
// the text's). Background only; no connection is held while the classifier
// runs.
func (x *x402) screenFrames(ctx context.Context, now int64) (int, error) {
	if x.fr == nil || x.screener == nil || !x.screener.ScreenAvailable(ctx) {
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
		if len(verdicts) >= framesScreenBatch {
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

// workFrames starts the background screen when one is due.
func (x *x402) workFrames(now int64) {
	if x.fr == nil || x.screener == nil || now-x.fr.screened.Load() < framesScreenEvery || !x.fr.screening.CompareAndSwap(false, true) {
		return
	}
	x.fr.screened.Store(now)
	go func() {
		defer x.fr.screening.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), x402ScreenDeadline)
		defer cancel()
		_, _ = x.screenFrames(ctx, now)
	}()
}
