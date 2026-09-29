package services

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/safenet"
)

// public_data: a fixed catalogue of public datasets (publicdata_*.go), each
// with fixed upstream hosts, strictly validated parameters, a normalised and
// versioned JSON output, a cache TTL, a price and per-caller rate limits.
// Nothing an agent sends ever names a host, a path or a URL: every request
// is built from the catalogue, sent only to the dataset's own hosts through
// internal/safenet, never through a proxy and never following a redirect,
// with a response-size and time cap. Results are cached in SQLite, so most
// calls cost nothing upstream.

// Public data limits.
const (
	// PublicDataArgsMax bounds fetch's and bulk's args JSON (the published
	// service_args_bytes limit).
	PublicDataArgsMax = 4 << 10
	// PublicDataBulkMax bounds the requests in one bulk call.
	PublicDataBulkMax = 10
	// PublicDataMaxDuration bounds one call, every upstream request included.
	PublicDataMaxDuration = 90 * time.Second
	// PublicDataStaleMax is how old a cached value may be and still be served
	// (flagged stale) when its upstream cannot be reached.
	PublicDataStaleMax = 30 * 24 * time.Hour
	// PublicDataSchemaVersion is the envelope's version (envelope_version);
	// each dataset's data has its own (schema_version).
	PublicDataSchemaVersion = 1

	publicDataParallel      = 4
	publicDataCacheEntryMax = 4 << 20
	publicDataCacheRowsMax  = 1000 // per dataset
	publicDataDialTimeout   = 5 * time.Second
	publicDataTimeout       = 20 * time.Second
	publicDataResponseMax   = 4 << 20
	publicDataUserAgent     = "SwarmMemo-public-data/1 (+https://swarmmemo.com/docs)"
	publicDataStringMax     = 256
	publicDataCallerMax     = 1 << 14
)

// Key file names inside the key directory. A dataset whose key file is
// absent lists as unavailable.
const (
	KeyAPIDataGov = "api_data_gov.key" // api.congress.gov and api.open.fec.gov
	KeyFRED       = "fred.key"         // api.stlouisfed.org
)

// PublicDataKeyDir is the key directory when PUBLIC_DATA_KEY_DIR is unset.
const PublicDataKeyDir = "/etc/swarmmemo/keys"

// PublicDataConfig is public_data's operator configuration: only where its
// key files live. The keys themselves are read from the files on every call
// (replacing a file rotates a key) and never appear in an error, a record or
// a log.
type PublicDataConfig struct {
	KeyDir string

	// remap and dial exist only for tests (export_test.go): an upstream host
	// is served by an httptest server on loopback over plain HTTP.
	remap map[string]string
	dial  func(ctx context.Context, network, addr string) (net.Conn, error)
}

// NewPublicDataConfig validates the key directory (an absolute path; it need
// not exist, in which case every keyed dataset is unavailable).
func NewPublicDataConfig(keyDir string) (*PublicDataConfig, error) {
	if !filepath.IsAbs(keyDir) || strings.ContainsAny(keyDir, "\x00\n\r") {
		return nil, errors.New("PUBLIC_DATA_KEY_DIR must be an absolute path")
	}
	return &PublicDataConfig{KeyDir: filepath.Clean(keyDir)}, nil
}

func (c *PublicDataConfig) keyPath(name string) string {
	if c == nil || c.KeyDir == "" || name == "" {
		return ""
	}
	return filepath.Join(c.KeyDir, name)
}

// pdLimit is a fixed-window budget: per minute and per UTC day.
type pdLimit struct{ PerMinute, PerDay int64 }

// publicDataTierLimits are the per-caller request limits by the caller's
// tier (allowance.Tier): a bulk call counts one request per dataset request.
var publicDataTierLimits = map[allowance.Tier]pdLimit{
	allowance.TierPool:      {600, 100000},
	allowance.TierTrusted:   {600, 100000},
	allowance.TierProven:    {120, 20000},
	allowance.TierSigned:    {30, 2000},
	allowance.TierAnonymous: {10, 200},
}

// publicDataAnonymousNote is what fetch and bulk allow without a key: the
// anonymous tier's per-caller limits.
func publicDataAnonymousNote(bulk bool) string {
	l := publicDataTierLimits[allowance.TierAnonymous]
	note := fmt.Sprintf("per network, %d dataset requests a minute and %d a day", l.PerMinute, l.PerDay)
	if bulk {
		note += "; a bulk call counts each of its requests"
	}
	return note
}

// pdWindow counts one key's use in the current minute and day.
type pdWindow struct{ minute, mcount, day, dcount int64 }

// take counts n units if both windows have room; retry is the seconds until
// the binding window resets.
func (w *pdWindow) take(l pdLimit, now, n int64) (ok bool, retry int) {
	if w.minute != now/60 {
		w.minute, w.mcount = now/60, 0
	}
	if w.day != now/86400 {
		w.day, w.dcount = now/86400, 0
	}
	switch {
	case w.dcount+n > l.PerDay:
		return false, int(86400 - now%86400)
	case w.mcount+n > l.PerMinute:
		return false, int(60 - now%60)
	}
	w.mcount += n
	w.dcount += n
	return true, 0
}

type publicData struct {
	cfg        *PublicDataConfig
	db         *sql.DB
	classifier allowance.Classifier
	client     *http.Client
	hosts      map[string]bool // host:port every request may connect to

	mu       sync.Mutex
	callers  map[string]*pdWindow
	upstream map[string]*pdWindow

	locks [64]sync.Mutex // striped by cache key: one fetch per key at a time
}

func newPublicData(d Deps) Provider {
	p := &publicData{cfg: d.PublicData, db: d.DB, classifier: d.Classifier, hosts: map[string]bool{},
		callers: map[string]*pdWindow{}, upstream: map[string]*pdWindow{}}
	for _, ds := range pdCatalogue {
		for _, h := range ds.Hosts {
			p.hosts[p.dialAddr(h)] = true
		}
	}
	p.client = p.newClient()
	return p
}

// dialAddr is the host:port a request to upstream host h connects to.
func (p *publicData) dialAddr(h string) string {
	if p.cfg != nil && p.cfg.remap[h] != "" {
		return p.cfg.remap[h]
	}
	return net.JoinHostPort(h, "443")
}

// newClient is the only HTTP client public_data uses: it connects only to a
// catalogue host, only to public addresses (internal/safenet), never through
// a proxy, and never follows a redirect.
func (p *publicData) newClient() *http.Client {
	var dial func(ctx context.Context, network, addr string) (net.Conn, error)
	if p.cfg != nil {
		dial = p.cfg.dial
	}
	if dial == nil {
		dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return safenet.Dial(ctx, network, addr, publicDataDialTimeout)
		}
	}
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				if !p.hosts[addr] {
					return nil, errNotCatalogueHost
				}
				return dial(ctx, network, addr)
			},
			TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout:    publicDataDialTimeout,
			ResponseHeaderTimeout:  publicDataTimeout,
			MaxIdleConns:           32,
			MaxIdleConnsPerHost:    2,
			IdleConnTimeout:        60 * time.Second,
			MaxResponseHeaderBytes: 16 << 10,
		},
	}
}

var errNotCatalogueHost = errors.New("public_data: refusing a host outside the catalogue")

func (*publicData) Schema() string {
	return `
CREATE TABLE IF NOT EXISTS public_data_cache (
 dataset TEXT NOT NULL, key TEXT NOT NULL, body TEXT NOT NULL, source_url TEXT NOT NULL,
 fetched_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, PRIMARY KEY(dataset,key));
`
}

func (*publicData) Describe() Descriptor {
	return Descriptor{
		ID: "public_data",
		Summary: "Public datasets from fixed upstreams (NOAA, NSIDC, FSIS, openFDA, Congress.gov, FEC, CoinGecko, FRED, BIS, BCB, Federal Reserve nowcasts), normalised, versioned and cached. " +
			"service.read method datasets lists the catalogue; fetch args: {\"dataset\":ID,\"params\":{...}}; bulk args: {\"requests\":[{\"dataset\":ID,\"params\":{...}},...]} (at most 10).",
		Title: "Public data", Topic: "Search and data",
		Line: "Fetch public datasets (weather, sea ice, food recalls, bills, election finance, prices, policy rates, nowcasts) from their official sources, normalised and cached.",
		Limits: []Limit{
			{"public_data_args_bytes", PublicDataArgsMax, "bytes", "Arguments of one call"},
			{"public_data_bulk_requests", PublicDataBulkMax, "", "Requests in one bulk call"},
		},
		Mode: Remote,
		Methods: []Method{
			{Name: "fetch", Write: true, Signed: true, Resource: allowance.Credit, ArgsMax: PublicDataArgsMax,
				Line: "One dataset request.", PriceNote: "the dataset's price (1 credit by default); the datasets read lists each", ExampleMaxCost: 5,
				Args:      []Arg{{"dataset", "string", true, "an id from the datasets read"}, {"params", "object", false, "the dataset's parameters"}},
				Example:   json.RawMessage(`{"dataset":"sea_ice_extent","params":{}}`),
				Anonymous: true, AnonymousLabel: "public data", AnonymousNote: publicDataAnonymousNote(false),
				AnonymousRate: AnonRate{AllPerMinute: 60, AllPerDay: 5000}},
			{Name: "bulk", Write: true, Signed: true, Resource: allowance.Credit, ArgsMax: PublicDataArgsMax,
				Line: "Up to 10 dataset requests, answered in order.", PriceNote: "each request's dataset price (1 credit by default)", ExampleMaxCost: 20,
				Args:      []Arg{{"requests", "array", true, `up to 10 {"dataset","params"}`}},
				Example:   json.RawMessage(`{"requests":[{"dataset":"sea_ice_extent","params":{}},{"dataset":"us_nowcasts","params":{"measure":"gdp"}}]}`),
				Anonymous: true, AnonymousLabel: "public data", AnonymousNote: publicDataAnonymousNote(true),
				AnonymousRate: AnonRate{AllPerMinute: 15, AllPerDay: 500}},
			{Name: "datasets", ArgsMax: 256, Line: "The catalogue: each dataset with its parameters, source, licence and price."},
		},
		MaxDuration:   PublicDataMaxDuration,
		StoredBodyMax: StoredBodyMaxBytes,
	}
}

// available reports whether ds can be fetched now: keyless, or its key file
// is present (a dataset whose key is optional is always available).
func (p *publicData) available(ds *pdDataset) bool {
	if ds.Key == "" || ds.KeyOptional {
		return true
	}
	path := p.cfg.keyPath(ds.Key)
	return path != "" && keyFilePresent(path)
}

func (p *publicData) CatalogueExtra() map[string]any {
	n := 0
	for _, ds := range pdCatalogue {
		if p.available(ds) {
			n++
		}
	}
	return map[string]any{
		"available": n > 0, "network": true, "datasets_available": n, "datasets_total": len(pdCatalogue),
		"catalogue": map[string]any{"operation": "service.read", "target": "public_data", "data": map[string]any{"schema": 1, "method": "datasets"}},
	}
}

// Datasets is the catalogue as the datasets read returns it. Every listing
// of the catalogue (the API, and any web page) is this one function.
func (p *publicData) Datasets() map[string]any {
	list := make([]any, 0, len(pdCatalogue))
	for _, ds := range pdCatalogue {
		list = append(list, ds.describe(p.available(ds)))
	}
	tiers := map[string]any{}
	for t, l := range publicDataTierLimits {
		tiers[tierName(t)] = map[string]int64{"per_minute": l.PerMinute, "per_day": l.PerDay}
	}
	return map[string]any{
		"envelope_version": PublicDataSchemaVersion,
		"datasets":         list,
		"methods": map[string]any{
			"fetch": `service.call {"schema":1,"method":"fetch","args":{"dataset":ID,"params":{...}},"max_cost":N}`,
			"bulk":  `service.call {"schema":1,"method":"bulk","args":{"requests":[{"dataset":ID,"params":{...}}]},"max_cost":N}`,
		},
		"bulk_max":          PublicDataBulkMax,
		"rate_limits":       tiers,
		"rate_limit_note":   "per caller, by the caller's tier; a bulk call counts one request per dataset request",
		"stale_max_seconds": int64(PublicDataStaleMax / time.Second),
	}
}

func tierName(t allowance.Tier) string {
	switch t {
	case allowance.TierPool:
		return "grant_pool"
	case allowance.TierTrusted:
		return "trusted"
	case allowance.TierProven:
		return "proven"
	case allowance.TierSigned:
		return "signed"
	}
	return "anonymous"
}

// Read serves the free datasets read.
func (p *publicData) Read(_ context.Context, _ allowance.Querier, c Call) (json.RawMessage, error) {
	if c.Method != "datasets" {
		return nil, refusal("invalid_service_data")
	}
	var none struct{}
	if err := StrictObject(c.Args, &none); err != nil {
		return nil, err
	}
	return compactJSON(p.Datasets()), nil
}

// pdRequest is one parsed dataset request.
type pdRequest struct {
	ds     *pdDataset
	params *pdParams
}

type pdRequestArgs struct {
	Dataset *string         `json:"dataset"`
	Params  json.RawMessage `json:"params"`
}

// parsePublicData parses fetch's or bulk's args strictly, every dataset's
// parameters included. It is pure: now only resolves date defaults.
func parsePublicData(method string, raw json.RawMessage, now int64) ([]pdRequest, error) {
	var items []pdRequestArgs
	switch method {
	case "fetch":
		var a pdRequestArgs
		if err := StrictObject(raw, &a); err != nil {
			return nil, err
		}
		items = []pdRequestArgs{a}
	case "bulk":
		var a struct {
			Requests []pdRequestArgs `json:"requests"`
		}
		if err := StrictObject(raw, &a); err != nil {
			return nil, err
		}
		if len(a.Requests) == 0 || len(a.Requests) > PublicDataBulkMax {
			return nil, refusal("invalid_service_data")
		}
		items = a.Requests
	default:
		return nil, refusal("invalid_service_data")
	}
	out := make([]pdRequest, 0, len(items))
	for _, it := range items {
		if it.Dataset == nil {
			return nil, refusal("invalid_service_data")
		}
		ds := pdByID[*it.Dataset]
		if ds == nil {
			return nil, refusal("invalid_service_data")
		}
		params, err := ds.parseParams(it.Params, now)
		if err != nil {
			return nil, err
		}
		out = append(out, pdRequest{ds: ds, params: params})
	}
	return out, nil
}

// Quote is the per-call surcharge (the "services" parameter, zero by
// default) plus each requested dataset's price.
func (p *publicData) Quote(c Call) (Quote, error) {
	reqs, err := parsePublicData(c.Method, c.Args, c.Now)
	if err != nil {
		return Quote{}, err
	}
	total := c.Price.For(int64(len(c.Args)))
	for _, r := range reqs {
		total += r.ds.Price
	}
	return Quote{Resource: allowance.Credit, Max: total}, nil
}

// Admit is the per-caller rate limit, by the caller's tier, checked before
// anything is reserved.
func (p *publicData) Admit(ctx context.Context, q allowance.Querier, c Call) error {
	reqs, err := parsePublicData(c.Method, c.Args, c.Now)
	if err != nil {
		return err
	}
	tier := allowance.TierAnonymous
	if c.Subject.Signed {
		tier = allowance.TierSigned
	}
	if p.classifier != nil {
		if st, err := p.classifier.Classify(ctx, q, c.Subject, c.Now); err == nil && st.Tier <= allowance.TierAnonymous {
			tier = st.Tier
		}
	}
	limit := publicDataTierLimits[tier]
	p.mu.Lock()
	defer p.mu.Unlock()
	w := p.callers[c.Subject.ID]
	if w == nil {
		if len(p.callers) >= publicDataCallerMax {
			for k, v := range p.callers {
				if v.minute != c.Now/60 && v.day != c.Now/86400 {
					delete(p.callers, k)
				}
			}
			if len(p.callers) >= publicDataCallerMax {
				// Fail closed rather than forget a caller's day count.
				return &allowance.Err{Code: "request_rate", RetryAfter: 60}
			}
		}
		w = &pdWindow{}
		p.callers[c.Subject.ID] = w
	}
	if ok, retry := w.take(limit, c.Now, int64(len(reqs))); !ok {
		return &allowance.Err{Code: "request_rate", RetryAfter: retry}
	}
	return nil
}

// pdItem is one dataset result: the envelope every dataset shares.
type pdItem struct {
	EnvelopeVersion int             `json:"envelope_version"`
	Dataset         string          `json:"dataset"`
	SchemaVersion   int             `json:"schema_version"`
	Params          map[string]any  `json:"params"`
	Data            any             `json:"data"`
	AsOf            *string         `json:"as_of"`
	FetchedAt       *string         `json:"fetched_at"`
	ExpiresAt       *string         `json:"expires_at"`
	Stale           bool            `json:"stale"`
	Cache           string          `json:"cache"`
	SourceURL       string          `json:"source_url"`
	Sources         []pdSourceRec   `json:"sources"`
	Licence         string          `json:"licence"`
	Attribution     string          `json:"attribution"`
	TextIsUntrusted bool            `json:"text_is_untrusted"`
	Cost            int64           `json:"cost"`
	Error           *pdErr          `json:"error,omitempty"`
	raw             json.RawMessage `json:"-"`
}

// pdSourceRec is one upstream document an item used.
type pdSourceRec struct {
	URL         string  `json:"url"`
	Cache       string  `json:"cache"` // hit, miss, stale, static, error
	FetchedAt   *string `json:"fetched_at,omitempty"`
	ExpiresAt   *string `json:"expires_at,omitempty"`
	Stale       bool    `json:"stale"`
	StaleReason string  `json:"stale_reason,omitempty"`
	Error       string  `json:"error,omitempty"`
	fetched     int64
	expires     int64
}

// pdErr is an item's failure: Code is the refusal a single fetch returns
// (upstream_unavailable, upstream_busy, upstream_failed), Reason the detail.
type pdErr struct {
	Code   string `json:"code"`
	Reason string `json:"reason,omitempty"`
}

func (e *pdErr) Error() string { return "public_data: " + e.Code + " (" + e.Reason + ")" }

func pdFail(code, reason string) *pdErr { return &pdErr{Code: code, Reason: reason} }

// Run fetches every request (at most publicDataParallel at a time), charges
// the price of each one that succeeded, and returns one item (fetch) or the
// list (bulk). A fetch whose dataset failed is refused and refunded.
func (p *publicData) Run(ctx context.Context, _ *sql.Tx, c Call) (Result, error) {
	reqs, err := parsePublicData(c.Method, c.Args, c.Now)
	if err != nil {
		return Result{}, err
	}
	if p.db == nil {
		return Result{}, refusal("upstream_unavailable")
	}
	items := make([]*pdItem, len(reqs))
	sem := make(chan struct{}, publicDataParallel)
	var wg sync.WaitGroup
	for i, r := range reqs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			items[i] = p.runOne(ctx, r, c.Now)
		}()
	}
	wg.Wait()
	var body []byte
	if c.Method == "fetch" {
		if items[0].Error != nil {
			return Result{}, refusal(items[0].Error.Code)
		}
		body = items[0].raw
	} else {
		body = p.bulkBody(items)
	}
	if len(body) > StoredBodyMaxBytes {
		return Result{}, refusal("upstream_failed")
	}
	used := c.Price.For(int64(len(c.Args)))
	ok := 0
	for _, it := range items {
		if it.Error == nil {
			used += it.Cost
			ok++
		}
	}
	if ok == 0 {
		return Result{}, refusal(items[0].Error.Code)
	}
	public := map[string]any{"method": c.Method, "requests": len(items), "ok": ok, "body_bytes": len(body)}
	ids := make([]string, 0, len(items))
	for _, it := range items {
		if !slices.Contains(ids, it.Dataset) && len(ids) < 10 {
			ids = append(ids, it.Dataset)
		}
	}
	public["datasets"] = ids
	return Result{Body: body, Public: compactJSON(public), Used: used}, nil
}

// bulkBody lists every item, replacing items from the end with a
// response_too_large error until the body fits the call record. Only the
// items kept are charged: a replaced item is marked failed first.
func (p *publicData) bulkBody(items []*pdItem) []byte {
	build := func() []byte {
		results := make([]json.RawMessage, len(items))
		for i, it := range items {
			results[i] = it.raw
		}
		return compactJSON(map[string]any{"envelope_version": PublicDataSchemaVersion, "results": results})
	}
	body := build()
	for i := len(items) - 1; i >= 0 && len(body) > StoredBodyMaxBytes; i-- {
		it := items[i]
		*it = pdItem{EnvelopeVersion: PublicDataSchemaVersion, Dataset: it.Dataset, SchemaVersion: it.SchemaVersion, Params: it.Params, Sources: []pdSourceRec{}, Cache: "none",
			Error: pdFail("upstream_failed", "response_too_large: request fewer rows or a narrower range")}
		it.raw = compactJSON(it)
		body = build()
	}
	return body
}

// runOne runs one dataset request into its item. It never panics out: a
// handler's panic fails that item only.
func (p *publicData) runOne(ctx context.Context, req pdRequest, now int64) (item *pdItem) {
	ds := req.ds
	item = &pdItem{EnvelopeVersion: PublicDataSchemaVersion, Dataset: ds.ID, SchemaVersion: ds.SchemaVersion, Params: req.params.echo(), Licence: ds.Licence,
		Attribution: ds.Attribution, TextIsUntrusted: ds.Untrusted, Sources: []pdSourceRec{}}
	defer func() {
		if r := recover(); r != nil {
			*item = pdItem{EnvelopeVersion: PublicDataSchemaVersion, Dataset: ds.ID, SchemaVersion: ds.SchemaVersion, Params: req.params.echo(), Sources: []pdSourceRec{},
				Cache: "none", Error: pdFail("upstream_failed", "internal")}
		}
		item.raw = compactJSON(item)
	}()
	if !p.available(ds) {
		item.Cache, item.Error = "none", pdFail("upstream_unavailable", "key_missing")
		return item
	}
	run := &pdRun{p: p, ds: ds, now: now}
	if ds.Key != "" {
		if path := p.cfg.keyPath(ds.Key); path != "" {
			if k, err := readKey(path); err == nil {
				run.key = k
			}
		}
		if run.key == "" && !ds.KeyOptional {
			item.Cache, item.Error = "none", pdFail("upstream_unavailable", "key_missing")
			return item
		}
	}
	data, err := ds.Run(ctx, run, req.params)
	item.Sources = run.sources
	if err != nil {
		var pe *pdErr
		if !errors.As(err, &pe) {
			pe = pdFail("upstream_failed", "internal")
		}
		item.Cache, item.Error = "none", pe
		return item
	}
	item.Data = data
	item.Cost = ds.Price
	if run.asOf != "" {
		item.AsOf = &run.asOf
	}
	var fetched, expires int64
	kinds := map[string]bool{}
	for _, s := range run.sources {
		if s.Error != "" {
			continue
		}
		kinds[s.Cache] = true
		item.Stale = item.Stale || s.Stale
		if s.fetched > 0 && (fetched == 0 || s.fetched < fetched) {
			fetched = s.fetched
		}
		if s.expires > 0 && (expires == 0 || s.expires < expires) {
			expires = s.expires
		}
	}
	if fetched > 0 {
		item.FetchedAt = pdTime(fetched)
	}
	if expires > 0 {
		item.ExpiresAt = pdTime(expires)
	}
	switch {
	case item.Stale:
		item.Cache = "stale"
	case len(kinds) == 1:
		for k := range kinds {
			item.Cache = k
		}
	case len(kinds) == 0:
		item.Cache = "none"
	default:
		item.Cache = "mixed"
	}
	for _, s := range run.sources {
		if s.Error == "" {
			item.SourceURL = s.URL
			break
		}
	}
	return item
}

func pdTime(unix int64) *string {
	s := time.Unix(unix, 0).UTC().Format(time.RFC3339)
	return &s
}

// pdRun is one dataset request in flight.
type pdRun struct {
	p       *publicData
	ds      *pdDataset
	now     int64
	key     string
	asOf    string
	mu      sync.Mutex
	sources []pdSourceRec
}

func (r *pdRun) today() time.Time {
	return time.Unix(r.now, 0).UTC().Truncate(24 * time.Hour)
}

func (r *pdRun) record(s pdSourceRec) {
	r.mu.Lock()
	r.sources = append(r.sources, s)
	r.mu.Unlock()
}

// static records a source that is compiled in (a published calendar).
func (r *pdRun) static(url string) {
	r.record(pdSourceRec{URL: url, Cache: "static"})
}

// pdFetch is one upstream document a dataset reads.
type pdFetch struct {
	Key       string        // cache key, scoped to the dataset
	TTL       time.Duration // how long a fetched copy is fresh
	Host      string        // one of the dataset's Hosts
	Path      string        // escaped path, beginning with /
	Query     url.Values    // never the key
	KeyParam  string        // the query parameter that carries the key (FRED)
	KeyHeader string        // the header that carries the key (api.data.gov)
	MaxBytes  int64         // response cap; publicDataResponseMax when 0
	Timeout   time.Duration // publicDataTimeout when 0
	Accept    string
	// NotFound404 makes a 404 an empty answer (openFDA's "no match") that
	// Parse sees as a nil body.
	NotFound404 bool
	// Parse normalises the raw body; Stream, when set, reads the capped
	// body itself instead (a large document parsed as it streams).
	Parse  func(raw []byte) (any, error)
	Stream func(r io.Reader) (any, error)
}

func (f pdFetch) publicURL() string {
	u := "https://" + f.Host + f.Path
	if len(f.Query) > 0 {
		u += "?" + f.Query.Encode()
	}
	return u
}

type pdCacheRow struct {
	body             []byte
	url              string
	fetched, expires int64
}

// fetch returns the normalised document f names: from the cache while it is
// fresh, else from upstream; when upstream fails, a cached copy no older than
// PublicDataStaleMax is served flagged stale. Fresh and cached answers go
// through the same normalised form, so a hit and a miss read identically.
func (r *pdRun) fetch(ctx context.Context, f pdFetch) (json.RawMessage, error) {
	if !slices.Contains(r.ds.Hosts, f.Host) {
		return nil, pdFail("upstream_failed", "host_not_in_catalogue")
	}
	h := fnv.New32a()
	h.Write([]byte(r.ds.ID + "\x00" + f.Key))
	lock := &r.p.locks[h.Sum32()%uint32(len(r.p.locks))]
	lock.Lock()
	defer lock.Unlock()
	row, found := r.p.cacheGet(ctx, r.ds.ID, f.Key)
	if found && r.now < row.expires {
		r.record(pdSourceRec{URL: row.url, Cache: "hit", FetchedAt: pdTime(row.fetched), ExpiresAt: pdTime(row.expires), fetched: row.fetched, expires: row.expires})
		return row.body, nil
	}
	body, err := r.p.download(ctx, r, f)
	if err == nil {
		ttl := int64(f.TTL / time.Second)
		if ttl < 1 {
			ttl = 1
		}
		row = pdCacheRow{body: body, url: f.publicURL(), fetched: r.now, expires: r.now + ttl}
		r.p.cachePut(r.ds.ID, f.Key, row)
		r.record(pdSourceRec{URL: row.url, Cache: "miss", FetchedAt: pdTime(row.fetched), ExpiresAt: pdTime(row.expires), fetched: row.fetched, expires: row.expires})
		return body, nil
	}
	var pe *pdErr
	if !errors.As(err, &pe) {
		pe = pdFail("upstream_failed", "internal")
	}
	if found && r.now-row.fetched <= int64(PublicDataStaleMax/time.Second) {
		r.record(pdSourceRec{URL: row.url, Cache: "stale", FetchedAt: pdTime(row.fetched), ExpiresAt: pdTime(row.expires), Stale: true,
			StaleReason: "upstream " + pe.Reason + "; serving the copy fetched at fetched_at, past its TTL", fetched: row.fetched, expires: row.expires})
		return row.body, nil
	}
	r.record(pdSourceRec{URL: f.publicURL(), Cache: "error", Error: pe.Code + ": " + pe.Reason})
	return nil, pe
}

func (p *publicData) cacheGet(ctx context.Context, dataset, key string) (pdCacheRow, bool) {
	var row pdCacheRow
	var body string
	err := p.db.QueryRowContext(ctx, "SELECT body,source_url,fetched_at,expires_at FROM public_data_cache WHERE dataset=? AND key=?", dataset, key).
		Scan(&body, &row.url, &row.fetched, &row.expires)
	if err != nil {
		return row, false
	}
	row.body = []byte(body)
	return row, json.Valid(row.body)
}

// cachePut stores a fetched document. A dataset holds at most
// publicDataCacheRowsMax keys: when full, copies too old to serve even stale
// make room first, then expired ones; with no room the document is served
// uncached. It runs outside the call's context so a finished fetch is kept.
func (p *publicData) cachePut(dataset, key string, row pdCacheRow) {
	if len(row.body) > publicDataCacheEntryMax {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var n int
	if err := p.db.QueryRowContext(ctx, "SELECT count(*) FROM public_data_cache WHERE dataset=?", dataset).Scan(&n); err != nil {
		return
	}
	if n >= publicDataCacheRowsMax {
		var exists int
		_ = p.db.QueryRowContext(ctx, "SELECT count(*) FROM public_data_cache WHERE dataset=? AND key=?", dataset, key).Scan(&exists)
		if exists == 0 {
			old := row.fetched - int64(PublicDataStaleMax/time.Second)
			_, _ = p.db.ExecContext(ctx, `DELETE FROM public_data_cache WHERE rowid IN (SELECT rowid FROM public_data_cache WHERE dataset=? AND (fetched_at<? OR expires_at<=?)
 ORDER BY fetched_at<? DESC, expires_at LIMIT 16)`, dataset, old, row.fetched, old)
			if err := p.db.QueryRowContext(ctx, "SELECT count(*) FROM public_data_cache WHERE dataset=?", dataset).Scan(&n); err != nil || n >= publicDataCacheRowsMax {
				return
			}
		}
	}
	_, _ = p.db.ExecContext(ctx, `INSERT INTO public_data_cache(dataset,key,body,source_url,fetched_at,expires_at) VALUES(?,?,?,?,?,?)
 ON CONFLICT(dataset,key) DO UPDATE SET body=excluded.body, source_url=excluded.source_url, fetched_at=excluded.fetched_at, expires_at=excluded.expires_at`,
		dataset, key, string(row.body), row.url, row.fetched, row.expires)
}

// politeness takes one request from an upstream's own budget, shared by
// every caller, so no burst of callers can hammer an upstream.
func (p *publicData) politeness(group string, now int64) bool {
	l, ok := pdUpstreamLimits[group]
	if !ok {
		return true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	w := p.upstream[group]
	if w == nil {
		w = &pdWindow{}
		p.upstream[group] = w
	}
	ok, _ = w.take(l, now, 1)
	return ok
}

// cappedReader fails once more than n bytes have been read.
type cappedReader struct {
	r io.Reader
	n int64
}

var errOversized = errors.New("public_data: response too large")

func (c *cappedReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.n -= int64(n)
	if c.n < 0 {
		return n, errOversized
	}
	return n, err
}

// download makes one upstream request and normalises its body.
func (p *publicData) download(ctx context.Context, r *pdRun, f pdFetch) (json.RawMessage, error) {
	group := pdHostGroup[f.Host]
	if group == "" {
		group = f.Host
	}
	if !p.politeness(group, r.now) {
		return nil, pdFail("upstream_busy", "upstream_budget")
	}
	max := f.MaxBytes
	if max <= 0 {
		max = publicDataResponseMax
	}
	timeout := f.Timeout
	if timeout <= 0 {
		timeout = publicDataTimeout
	}
	q := url.Values{}
	for k, v := range f.Query {
		q[k] = v
	}
	if f.KeyParam != "" {
		if r.key == "" {
			return nil, pdFail("upstream_unavailable", "key_missing")
		}
		q.Set(f.KeyParam, r.key)
	}
	scheme, host := "https", f.Host
	if p.cfg != nil && p.cfg.remap[f.Host] != "" {
		scheme, host = "http", p.cfg.remap[f.Host]
	}
	target := scheme + "://" + host + f.Path
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	stepCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(stepCtx, http.MethodGet, target, nil)
	if err != nil {
		return nil, pdFail("upstream_failed", "request")
	}
	req.Header.Set("User-Agent", publicDataUserAgent)
	accept := f.Accept
	if accept == "" {
		accept = "application/json"
	}
	req.Header.Set("Accept", accept)
	if f.KeyHeader != "" {
		if r.key == "" {
			return nil, pdFail("upstream_unavailable", "key_missing")
		}
		req.Header.Set(f.KeyHeader, r.key)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		// The error text may carry the URL, and so a key: only a class of
		// failure ever leaves this function.
		switch {
		case errors.Is(err, errNotCatalogueHost):
			return nil, pdFail("upstream_failed", "host_not_in_catalogue")
		case errors.Is(err, safenet.ErrBlocked), errors.Is(err, safenet.ErrUnresolved):
			return nil, pdFail("upstream_busy", "network")
		case errors.Is(err, context.DeadlineExceeded) || stepCtx.Err() != nil:
			return nil, pdFail("upstream_busy", "timeout")
		}
		return nil, pdFail("upstream_busy", "network")
	}
	defer resp.Body.Close()
	s := resp.StatusCode
	switch {
	case s == 404 && f.NotFound404:
		v, err := f.parse(nil)
		if err != nil {
			return nil, pdFail("upstream_failed", "malformed")
		}
		return compactJSON(v), nil
	case s >= 300 && s < 400:
		return nil, pdFail("upstream_failed", "redirect_refused")
	case s == 429:
		return nil, pdFail("upstream_busy", "http_429")
	case s >= 500:
		return nil, pdFail("upstream_busy", "http_"+strconv.Itoa(s))
	case s < 200 || s >= 300:
		return nil, pdFail("upstream_failed", "http_"+strconv.Itoa(s))
	}
	if resp.ContentLength > max {
		return nil, pdFail("upstream_failed", "oversized")
	}
	body := &cappedReader{r: resp.Body, n: max}
	var v any
	if f.Stream != nil {
		v, err = f.Stream(body)
	} else {
		var raw []byte
		raw, err = io.ReadAll(body)
		if err == nil {
			v, err = f.parse(raw)
		}
	}
	switch {
	case errors.Is(err, errOversized):
		return nil, pdFail("upstream_failed", "oversized")
	case err != nil && stepCtx.Err() != nil:
		return nil, pdFail("upstream_busy", "timeout")
	case err != nil:
		return nil, pdFail("upstream_failed", "malformed")
	}
	out := compactJSON(v)
	if len(out) > publicDataCacheEntryMax {
		return nil, pdFail("upstream_failed", "oversized")
	}
	return out, nil
}

func (f pdFetch) parse(raw []byte) (any, error) {
	if f.Parse == nil {
		return nil, errors.New("public_data: no parser")
	}
	return f.Parse(raw)
}
