package services

// The open catalogue: pay-per-call resources imported from x402 Bazaar
// discovery APIs on a timer, so agents can find more than the operator's
// hand allowlist. The allowlist stays the override: its entries are pinned
// (trusted, listed first) and its denylist is never imported past.
//
// Candidates and vetting. An imported resource is a candidate: searchable,
// marked vetted: false, and never callable. Its summary is untrusted upstream
// text, served only once it passed screen's classifier (x402_summary_screens)
// or the operator vetted the resource; until then it is withheld. The
// Bazaar lists whatever anyone registers, and an upstream that holds our
// signed authorization can settle it and still fail the call, so no open
// resource is paid for until the operator vets it: pins it in the allowlist,
// or runs swarmmemo x402 vet ID, which binds the vetting to the resource's
// URL, method and recipient (a changed recipient is a new candidate). A
// vetted open resource is callable under the open sub-caps; when a
// signature has left our hands and the call ends without an answer (the
// upstream failed after it, or re-asked with 402), the agent is charged, as
// for a discarded answer, and after two such outcomes the recipient and the
// URL are denied automatically (x402_denied) until the operator vets the
// resource again. A resource our own calls mostly fail (demoted) or a denied
// one is not callable either. Charging the caller's paid credit alone for an
// unvetted resource would need the ledger to reserve from one bucket, which
// it cannot today, so unvetted resources are refused to everyone.
//
// Guardrails, applied on every import and again on every load (a config
// change applies at once): HTTP resources on https://host:443 with a public
// DNS name (the dialer still checks every address); an exact EIP-3009
// payment on our network and asset to a non-zero recipient; a price at most
// the catalogue's max_price (and the per-call cap); GET or POST; query names
// and bodies bounded as for pinned resources; the catalogue's response size
// and timeout; not denied by domain, URL prefix (both compared canonical),
// recipient, category or automatic deny; and, where the default discovery
// API publishes it, at least min_payers_30d recent payers. At most three
// candidates per recipient and per registrable domain are kept (vetted ones
// are exempt), ranked vetted first, then by our own paid answers, then by
// the default discovery's signal; another discovery URL's popularity figures
// are not trusted. Category deny is advisory: a category is guessed from
// upstream text. An open resource's payment also counts against two
// sub-caps: all open resources per UTC day, and each recipient per UTC day.
//
// Storage never deletes the catalogue: an import upserts what it saw, and a
// resource not seen for three refresh intervals is stale, neither listed nor
// callable, until it is seen again. The snapshot in memory is what Quote and
// the resources read use (no I/O); the engine's worker loads it and starts
// an import when one is due (Work), and the import and the screening of
// summaries after it run in the background.
// Every read of the database copies its rows into memory and closes them
// before any filtering, so the one connection is never held for CPU work.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/trust"
)

// Open catalogue bounds.
const (
	X402PageMax           = 50    // resources in one resources read
	x402PageDefault       = 20    //
	x402SearchBytes       = 200   // a search query
	x402SearchWordsMax    = 8     // words in a search query
	x402CatalogueCeiling  = 20000 // open resources, whatever the config says
	x402DiscoveryURLsMax  = 4
	x402DiscoveryPagesMax = 100
	x402DenyMax           = 4096 // entries of each deny list
	x402StatsDays         = 30   // the success rate's window
	x402StatsKeepDays     = 60   // x402_resource_days rows kept
	x402ImportDeadline    = 3 * time.Minute
	x402PageTimeout       = 30 * time.Second
	x402PagePause         = 500 * time.Millisecond
	x402ReloadEvery       = 300 // seconds between snapshot reloads (stats)
	x402UpsertBatch       = 500
	x402LoadPage          = 1000 // catalogue rows read per statement
	// Candidates kept per recipient and per registrable domain: one owner
	// cannot fill the catalogue with look-alikes (vetted ones are exempt).
	x402PerRecipientMax = 3
	x402PerDomainMax    = 3
	// x402DenyAfter is how many open payments that left our hands without an
	// answer (unknown or rejected) deny a recipient and its URL.
	x402DenyAfter = 2
	// x402ScreenPerRefresh bounds the candidate summaries one refresh
	// screens: at 200 a refresh (every 360 minutes by default), 5000
	// candidates are all screened within a week, for a few cents of the Jev
	// budget each refresh. x402ScreenDeadline bounds the screening, apart
	// from the import's deadline so a slow crawl never starves it.
	x402ScreenPerRefresh = 200
	x402ScreenDeadline   = 3 * time.Minute
	x402ScreenIntent     = "show it to agents as the summary of a pay-per-call API they may search for and call"
	// X402DiscoveryCDP is the default discovery API: Coinbase's facilitator,
	// public and without a key. Its popularity figures are the only ones
	// ranking trusts.
	X402DiscoveryCDP = "https://api.cdp.coinbase.com/platform/v2/x402/discovery/resources"
)

const x402CatalogueSchema = `
CREATE TABLE IF NOT EXISTS x402_catalogue (
 id TEXT PRIMARY KEY, bundler TEXT NOT NULL, url TEXT NOT NULL, method TEXT NOT NULL, pay_to TEXT NOT NULL,
 amount INTEGER NOT NULL, query TEXT NOT NULL DEFAULT '[]', body INTEGER NOT NULL DEFAULT 0,
 category TEXT NOT NULL DEFAULT '', summary TEXT NOT NULL DEFAULT '', source TEXT NOT NULL DEFAULT '',
 payers INTEGER NOT NULL DEFAULT 0, calls INTEGER NOT NULL DEFAULT 0, curated INTEGER NOT NULL DEFAULT 0,
 first_seen INTEGER NOT NULL, last_seen INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS x402_catalogue_seen ON x402_catalogue(last_seen);
CREATE TABLE IF NOT EXISTS x402_resource_days (
 resource TEXT NOT NULL, day INTEGER NOT NULL, ok INTEGER NOT NULL DEFAULT 0, failed INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(resource, day));
CREATE INDEX IF NOT EXISTS x402_resource_days_day ON x402_resource_days(day);
CREATE TABLE IF NOT EXISTS x402_vetted (
 id TEXT PRIMARY KEY, url TEXT NOT NULL, method TEXT NOT NULL, pay_to TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('vetted','unvetted')), changed_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS x402_denied (
 kind TEXT NOT NULL CHECK(kind IN ('pay_to','url')), value TEXT NOT NULL, reason TEXT NOT NULL,
 denied_at INTEGER NOT NULL, cleared_at INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(kind, value));
CREATE TABLE IF NOT EXISTS x402_summary_screens (
 hash TEXT PRIMARY KEY, verdict TEXT NOT NULL CHECK(verdict IN ('pass','flag')), screened_at INTEGER NOT NULL);
`

// X402CatalogueConfig is the open catalogue's validated configuration.
// Amounts are atomic units.
type X402CatalogueConfig struct {
	DiscoveryURLs    []string
	Refresh          time.Duration
	PageSize         int
	MaxPages         int
	MaxResources     int
	MaxPrice         int64
	MaxResponseBytes int
	Timeout          time.Duration
	MinPayers        int64
	OpenDaily        int64
	RecipientDaily   int64
	// trusted is the one discovery URL whose popularity figures ranking
	// trusts: X402DiscoveryCDP (tests name their fake).
	trusted string
}

type x402CatalogueFile struct {
	DiscoveryURLs    []string    `json:"discovery_urls"`
	RefreshMinutes   json.Number `json:"refresh_minutes"`
	PageSize         json.Number `json:"page_size"`
	MaxPages         json.Number `json:"max_pages"`
	MaxResources     json.Number `json:"max_resources"`
	MaxPrice         string      `json:"max_price"`
	MaxResponseBytes json.Number `json:"max_response_bytes"`
	TimeoutSeconds   json.Number `json:"timeout_seconds"`
	MinPayers        json.Number `json:"min_payers_30d"`
	OpenDaily        string      `json:"open_daily"`
	RecipientDaily   string      `json:"recipient_daily"`
}

// X402Deny is what the open catalogue never admits: a host that is or is
// under a denied domain, a URL under a denied prefix, a denied recipient, a
// denied category. URL prefixes are kept canonical (canonicalX402URL).
type X402Deny struct {
	Domains    []string
	URLs       []string
	PayTo      map[EVMAddress]bool
	Categories []string
}

type x402DenyFile struct {
	Domains     []string `json:"domains"`
	DomainsFile string   `json:"domains_file"`
	URLs        []string `json:"urls"`
	PayTo       []string `json:"pay_to"`
	Categories  []string `json:"categories"`
}

// parseCatalogue validates the "catalogue" section; absent leaves it off.
func (c *X402Config) parseCatalogue(f *x402CatalogueFile) error {
	if f == nil {
		return nil
	}
	bad := func(field string) error { return fmt.Errorf("x402: config field catalogue.%s is invalid", field) }
	cc := &X402CatalogueConfig{trusted: X402DiscoveryCDP}
	urls := f.DiscoveryURLs
	if len(urls) == 0 {
		urls = []string{X402DiscoveryCDP}
	}
	if len(urls) > x402DiscoveryURLsMax {
		return bad("discovery_urls (at most 4)")
	}
	for _, u := range urls {
		if err := checkX402URL(u); err != nil {
			return bad("discovery_urls (" + err.Error() + ")")
		}
	}
	cc.DiscoveryURLs = slices.Clone(urls)
	ints := []struct {
		n           json.Number
		name        string
		def, lo, hi int64
		set         func(int64)
	}{
		{f.RefreshMinutes, "refresh_minutes (60 to 10080)", 360, 60, 10080, func(v int64) { cc.Refresh = time.Duration(v) * time.Minute }},
		{f.PageSize, "page_size (1 to 1000)", 500, 1, 1000, func(v int64) { cc.PageSize = int(v) }},
		{f.MaxPages, "max_pages (1 to 100)", 60, 1, x402DiscoveryPagesMax, func(v int64) { cc.MaxPages = int(v) }},
		{f.MaxResources, "max_resources (1 to 20000)", 5000, 1, x402CatalogueCeiling, func(v int64) { cc.MaxResources = int(v) }},
		{f.MaxResponseBytes, fmt.Sprintf("max_response_bytes (1 to %d)", X402ResponseBytesMax), X402ResponseBytesDefault, 1, X402ResponseBytesMax, func(v int64) { cc.MaxResponseBytes = int(v) }},
		{f.TimeoutSeconds, "timeout_seconds (1 to 20)", 10, 1, int64(X402TimeoutMax / time.Second), func(v int64) { cc.Timeout = time.Duration(v) * time.Second }},
		{f.MinPayers, "min_payers_30d (0 to 1000000)", 1, 0, 1_000_000, func(v int64) { cc.MinPayers = v }},
	}
	for _, it := range ints {
		v := it.def
		if it.n != "" {
			n, err := strconv.ParseInt(it.n.String(), 10, 64)
			if err != nil || n < it.lo || n > it.hi {
				return bad(it.name)
			}
			v = n
		}
		it.set(v)
	}
	amounts := []struct {
		text, name, def string
		dst             *int64
	}{
		{f.MaxPrice, "max_price", "0.02", &cc.MaxPrice},
		{f.OpenDaily, "open_daily", "0.5", &cc.OpenDaily},
		{f.RecipientDaily, "recipient_daily", "0.1", &cc.RecipientDaily},
	}
	for _, a := range amounts {
		text := a.text
		if text == "" {
			text = a.def
		}
		n, ok := parseUnits(text, c.Decimals)
		if !ok || n <= 0 {
			return bad(a.name)
		}
		*a.dst = n
	}
	if cc.MaxPrice > c.PerCall || cc.MaxPrice > cc.RecipientDaily || cc.RecipientDaily > cc.OpenDaily || cc.OpenDaily > c.GlobalDaily {
		return errors.New("x402: catalogue caps must satisfy max_price <= recipient_daily <= open_daily <= caps.global_daily, and max_price <= caps.per_call")
	}
	c.Catalogue = cc
	return nil
}

// parseDeny validates the allowlist's "deny" section; domains_file, when
// named, adds one domain per line (# comments).
func (c *X402Config) parseDeny(f *x402DenyFile, readFile func(string) ([]byte, error)) error {
	c.Deny = X402Deny{PayTo: map[EVMAddress]bool{}}
	if f == nil {
		return nil
	}
	bad := func(field string) error { return fmt.Errorf("x402: allowlist deny.%s is invalid", field) }
	domains := slices.Clone(f.Domains)
	if f.DomainsFile != "" {
		raw, err := readFile(f.DomainsFile)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return bad("domains_file (not found)")
		case err != nil || len(raw) > x402FileBytesMax:
			return bad("domains_file (unreadable or over 1 MiB)")
		}
		for _, line := range strings.Split(string(raw), "\n") {
			line, _, _ = strings.Cut(line, "#")
			if line = strings.TrimSpace(line); line != "" {
				domains = append(domains, line)
			}
		}
	}
	if len(domains) > x402DenyMax || len(f.URLs) > x402DenyMax || len(f.PayTo) > x402DenyMax || len(f.Categories) > x402DenyMax {
		return bad("lists (at most 4096 entries each)")
	}
	for _, d := range domains {
		d = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(d)), ".")
		if d == "" || strings.ContainsAny(d, "/:@ ") || !strings.Contains(d, ".") {
			return bad("domains (" + d + ")")
		}
		c.Deny.Domains = append(c.Deny.Domains, d)
	}
	for _, u := range f.URLs {
		if checkX402URL(u) != nil {
			return bad("urls")
		}
		c.Deny.URLs = append(c.Deny.URLs, canonicalX402URL(u))
	}
	for _, p := range f.PayTo {
		a, ok := ParseEVMAddress(p)
		if !ok {
			return bad("pay_to")
		}
		c.Deny.PayTo[a] = true
	}
	for _, cat := range f.Categories {
		if !x402CategoryRE.MatchString(cat) {
			return bad("categories")
		}
		c.Deny.Categories = append(c.Deny.Categories, cat)
	}
	return nil
}

// canonicalX402URL is raw as deny prefixes compare it: scheme and host
// lower-cased, a trailing dot and the scheme's default port dropped,
// percent-escapes of unreserved characters decoded and the others
// upper-cased, the path cleaned (a trailing slash kept); the query as sent.
// "" when raw does not parse.
func canonicalX402URL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if port := u.Port(); port != "" && !(scheme == "https" && port == "443") && !(scheme == "http" && port == "80") {
		host += ":" + port
	}
	p := normalizeEscapes(u.EscapedPath())
	trailing := strings.HasSuffix(p, "/")
	if p == "" {
		p = "/"
	}
	if p = path.Clean(p); trailing && p != "/" {
		p += "/"
	}
	out := scheme + "://" + host + p
	if u.RawQuery != "" || u.ForceQuery {
		out += "?" + u.RawQuery
	}
	return out
}

// normalizeEscapes decodes %XX escapes of unreserved characters (RFC 3986:
// letters, digits, - . _ ~) and upper-cases the hex of every other escape.
func normalizeEscapes(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	unhex := func(c byte) (byte, bool) {
		switch {
		case '0' <= c && c <= '9':
			return c - '0', true
		case 'a' <= c && c <= 'f':
			return c - 'a' + 10, true
		case 'A' <= c && c <= 'F':
			return c - 'A' + 10, true
		}
		return 0, false
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			hi, ok1 := unhex(s[i+1])
			lo, ok2 := unhex(s[i+2])
			if ok1 && ok2 {
				c := hi<<4 | lo
				if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~' {
					b.WriteByte(c)
				} else {
					b.WriteString("%" + strings.ToUpper(s[i+1:i+3]))
				}
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// x402Suffixes are the multi-label public suffixes trust folds domains
// under ("co.uk"): a registrable domain is one label under them, else the
// last two labels.
var x402Suffixes = trust.DefaultParams().DomainSuffixes

// indexResource fills r's derived fields once, so neither a search nor a
// deny check parses its URL again: the canonical URL, the host, its
// registrable domain and the lower-cased search text.
func indexResource(r *X402Resource) {
	r.canon = canonicalX402URL(r.URL)
	r.host = ""
	if u, err := url.Parse(r.URL); err == nil {
		r.host = strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	}
	r.domain = strings.TrimPrefix(trust.DomainRoot(r.host, x402Suffixes), "domain:")
	r.text = strings.ToLower(r.ID + " " + r.Category + " " + r.host + " " + r.Summary)
}

// x402DenyIndex is the denylist, the pinned endpoints and the automatic
// denies, indexed for one load or import: a domain set looked up by every
// suffix of a host, URL prefixes grouped by host, and sets.
type x402DenyIndex struct {
	domains    map[string]bool
	prefixes   map[string][]string // canonical host → canonical URL prefixes
	payTo      map[EVMAddress]bool
	categories map[string]bool
	pinned     map[string]bool // "METHOD canonical URL" of every pinned resource
	auto       *x402Denied
}

func (c *X402Config) denyIndex(auto *x402Denied) *x402DenyIndex {
	d := &x402DenyIndex{domains: map[string]bool{}, prefixes: map[string][]string{}, payTo: c.Deny.PayTo, categories: map[string]bool{}, pinned: map[string]bool{}, auto: auto}
	for _, dom := range c.Deny.Domains {
		d.domains[dom] = true
	}
	for _, p := range c.Deny.URLs {
		p = canonicalX402URL(p) // idempotent; entries set by hand stay canonical too
		if u, err := url.Parse(p); err == nil {
			d.prefixes[u.Host] = append(d.prefixes[u.Host], p)
		}
	}
	for _, cat := range c.Deny.Categories {
		d.categories[cat] = true
	}
	for _, p := range c.Resources {
		d.pinned[p.Method+" "+canonicalX402URL(p.URL)] = true
	}
	return d
}

// blocks reports whether the denylist refuses r (indexed).
func (d *x402DenyIndex) blocks(r *X402Resource) bool {
	if r.canon == "" || d.payTo[r.PayTo] || d.categories[r.Category] || d.auto.has(r) {
		return true
	}
	for host := r.host; host != ""; {
		if d.domains[host] {
			return true
		}
		_, rest, ok := strings.Cut(host, ".")
		if !ok {
			break
		}
		host = rest
	}
	for _, prefix := range d.prefixes[r.host] {
		if strings.HasPrefix(r.canon, prefix) {
			return true
		}
	}
	return false
}

// blocks reports whether the denylist refuses r.
func (d X402Deny) blocks(r *X402Resource) bool {
	c := &X402Config{Deny: d}
	rr := *r
	indexResource(&rr)
	return c.denyIndex(nil).blocks(&rr)
}

// admitOpen applies the guardrails to an open resource and fixes its
// bounds; false refuses it.
func (c *X402Config) admitOpen(r *X402Resource) bool {
	return c.admitOpenWith(r, c.denyIndex(nil))
}

// admitOpenWith is admitOpen with the deny index built once for many rows.
func (c *X402Config) admitOpenWith(r *X402Resource, d *x402DenyIndex) bool {
	cc := c.Catalogue
	if cc == nil || r.Bundler != X402Bundler || checkX402URL(r.URL) != nil || (r.Method != http.MethodGet && r.Method != http.MethodPost) ||
		r.Body && r.Method != http.MethodPost || r.PayTo == (EVMAddress{}) || r.MaxAmount <= 0 || r.MaxAmount > min(cc.MaxPrice, c.PerCall) ||
		len(r.Query) > X402QueryParamsMax || !x402IDRE.MatchString(r.ID) {
		return false
	}
	for _, name := range r.Query {
		if !x402ParamRE.MatchString(name) {
			return false
		}
	}
	indexResource(r)
	if d.blocks(r) || d.pinned[r.Method+" "+r.canon] { // a pinned endpoint is never also open
		return false
	}
	r.Open, r.MaxResponseBytes, r.Timeout = true, cc.MaxResponseBytes, cc.Timeout
	return true
}

// x402Categories are the categories, in the order they are tried: the
// first whose word appears in a resource's URL or description names it.
var x402Categories = []struct {
	name  string
	words []string
}{
	{"adult", []string{"nsfw", "porn", "xxx", "adult content", "onlyfans"}},
	{"gambling", []string{"casino", "gambling", "betting", "sportsbook", "lottery"}},
	{"search", []string{"search", "serp", "lookup", "find "}},
	{"scraping", []string{"scrape", "crawl", "extract", "contents", "webpage", "browser", "screenshot", "fetch"}},
	{"crypto", []string{"crypto", "token", "coin", "defi", "wallet", "onchain", "on-chain", "dex", "swap", "nft", "solana", "ethereum", "blockchain", "erc20", "memecoin"}},
	{"finance", []string{"stock", "forex", "finance", "market", "price", "equity", "earnings"}},
	{"ai", []string{"llm", "gpt", "inference", "completion", "chat", "model", "generate", "agent", "openrouter", "embedding"}},
	{"media", []string{"image", "video", "audio", "speech", "tts", "music", "photo"}},
	{"news", []string{"news", "headline"}},
	{"weather", []string{"weather", "forecast"}},
	{"social", []string{"twitter", "tweet", "x.com", "reddit", "farcaster", "social", "linkedin"}},
	{"people", []string{"email", "contact", "person", "company", "enrich"}},
	{"data", []string{"data", "api"}},
}

// x402Categorize names text's category; "other" when no word matches.
func x402Categorize(text string) string {
	t := strings.ToLower(text)
	for _, c := range x402Categories {
		for _, w := range c.words {
			if strings.Contains(t, w) {
				return c.name
			}
		}
	}
	return "other"
}

// Snapshot and state.

type callStats struct{ ok, failed int64 }

// x402Snapshot is the catalogue as calls and reads see it: immutable once
// published.
type x402Snapshot struct {
	open       []*X402Resource // ranked, best first
	byID       map[string]*X402Resource
	signal     map[string]bazaarSignal
	stats      map[string]callStats // ours, last x402StatsDays days, pinned and open
	paid       map[string]int64     // our paid answers, last x402StatsDays days
	categories map[string]int       // the open resources, by category
	importedAt int64
	loadedAt   int64
}

// x402Denied is the automatic denylist (x402_denied): recipients and
// canonical URLs whose payments left our hands without an answer too often.
// Copied on write.
type x402Denied struct {
	payTo map[string]bool
	url   map[string]bool
}

func (d *x402Denied) has(r *X402Resource) bool {
	return d != nil && (d.payTo[r.PayTo.String()] || d.url[r.canon])
}

// catalogueState is the provider's view of the open catalogue.
type catalogueState struct {
	snap      atomic.Pointer[x402Snapshot]
	denied    atomic.Pointer[x402Denied]
	importing atomic.Bool
	nextSync  atomic.Int64 // when the next import is due (unix seconds)
	pruned    atomic.Int64 // the UTC day x402_resource_days was last pruned
	lastError atomic.Pointer[string]
}

// lookup finds a resource: pinned first, then open (candidates included:
// callable says whether it may be paid).
func (x *x402) lookup(id string) (*X402Resource, bool) {
	if r, ok := x.byID[id]; ok {
		return r, true
	}
	if s := x.cat.snap.Load(); s != nil {
		r, ok := s.byID[id] // empty unless the catalogue is on
		return r, ok
	}
	return nil, false
}

// callable reports whether r may be paid for now: pinned, or open and
// vetted, not denied and not demoted by our own failed calls. No I/O.
func (x *x402) callable(r *X402Resource) bool {
	if !r.Open {
		return true
	}
	if !r.Vetted || x.cat.denied.Load().has(r) {
		return false
	}
	s := x.cat.snap.Load()
	return s == nil || !s.stats[r.ID].demoted()
}

// Work is the engine worker's hook, every pass: it loads the snapshot when
// it is missing or old, prunes old call counts once a UTC day, and starts a
// background import when one is due, which then screens the candidates'
// summaries (screenSummaries). It runs outside any request's transaction.
func (x *x402) Work(ctx context.Context, _ *sql.DB, now int64) (int, error) {
	if x.cfg == nil {
		return 0, nil
	}
	cc := x.cfg.Catalogue
	if s := x.cat.snap.Load(); s == nil || now-s.loadedAt >= x402ReloadEvery {
		_ = x.loadCatalogue(ctx, now) // a failed load keeps the old snapshot
		// After a restart, the last import's time says when the next is due,
		// so restarts never hammer the discovery APIs.
		if s := x.cat.snap.Load(); s != nil && cc != nil && x.cat.nextSync.Load() == 0 && s.importedAt > 0 {
			x.cat.nextSync.Store(s.importedAt + int64(cc.Refresh/time.Second))
		}
	}
	if day := now / 86400; x.cat.pruned.Load() != day {
		x.cat.pruned.Store(day)
		x.pruneStats(ctx, day)
	}
	if cc == nil || x.paused() || now < x.cat.nextSync.Load() || !x.cat.importing.CompareAndSwap(false, true) {
		return 0, nil
	}
	x.cat.nextSync.Store(now + int64(cc.Refresh/time.Second))
	go func() {
		defer x.cat.importing.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), x402ImportDeadline)
		_, _ = x.importCatalogue(ctx, now) // the store's clock, as every load
		cancel()
		ctx, cancel = context.WithTimeout(context.Background(), x402ScreenDeadline)
		defer cancel()
		_, _ = x.screenSummaries(ctx, now)
	}()
	return 0, nil
}

// pruneStats drops per-day call counts older than x402StatsKeepDays; the
// success rate reads only the last x402StatsDays.
func (x *x402) pruneStats(ctx context.Context, day int64) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, _ = x.db.ExecContext(ctx, "DELETE FROM x402_resource_days WHERE day<?", day-x402StatsKeepDays)
}

// queryAll runs one query and copies its rows into memory with scan,
// closing them before it returns: the one connection is held only while the
// rows are read.
func queryAll[T any](ctx context.Context, db *sql.DB, scan func(*sql.Rows) (T, error), query string, args ...any) ([]T, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// x402Vetting is one vetted resource's binding: vetting holds only while
// the resource keeps this URL, method and recipient.
type x402Vetting struct{ url, method, payTo string }

func (v x402Vetting) matches(r *X402Resource) bool {
	return v.url == r.URL && v.method == r.Method && v.payTo == r.PayTo.String()
}

// history is what the import and the load rank by, read from our own
// records: vetted resources, paid answers per resource in the stats window,
// and the automatic denies.
type x402History struct {
	vetted map[string]x402Vetting
	paid   map[string]int64
	denied *x402Denied
}

func (x *x402) readHistory(ctx context.Context, now int64) (x402History, error) {
	h := x402History{vetted: map[string]x402Vetting{}, paid: map[string]int64{}, denied: &x402Denied{payTo: map[string]bool{}, url: map[string]bool{}}}
	type kv struct {
		k string
		n int64
	}
	paid, err := queryAll(ctx, x.db, func(r *sql.Rows) (kv, error) {
		var v kv
		return v, r.Scan(&v.k, &v.n)
	}, "SELECT resource, count(*) FROM x402_payments WHERE day>=? AND state='paid' GROUP BY resource", now/86400-x402StatsDays)
	if err != nil {
		return h, err
	}
	for _, p := range paid {
		h.paid[p.k] = p.n
	}
	type vet struct {
		id string
		v  x402Vetting
	}
	vetted, err := queryAll(ctx, x.db, func(r *sql.Rows) (vet, error) {
		var v vet
		return v, r.Scan(&v.id, &v.v.url, &v.v.method, &v.v.payTo)
	}, "SELECT id,url,method,pay_to FROM x402_vetted WHERE state='vetted'")
	if err != nil {
		return h, err
	}
	for _, v := range vetted {
		h.vetted[v.id] = v.v
	}
	denied, err := queryAll(ctx, x.db, func(r *sql.Rows) ([2]string, error) {
		var d [2]string
		return d, r.Scan(&d[0], &d[1])
	}, "SELECT kind,value FROM x402_denied WHERE denied_at>cleared_at")
	if err != nil {
		return h, err
	}
	for _, d := range denied {
		if d[0] == "pay_to" {
			h.denied.payTo[d[1]] = true
		} else {
			h.denied.url[d[1]] = true
		}
	}
	return h, nil
}

// x402RankKey is what ranks an open resource: ours failing last; vetted
// ones first; then our own paid answers; then the trusted discovery signal;
// the lower price; the id.
type x402RankKey struct {
	demoted, vetted bool
	paid            int64
	sig             bazaarSignal
	price           int64
	id              string
}

func (a x402RankKey) before(b x402RankKey) bool {
	switch {
	case a.demoted != b.demoted:
		return b.demoted
	case a.vetted != b.vetted:
		return a.vetted
	case a.paid != b.paid:
		return a.paid > b.paid
	case better(a.sig, b.sig) != better(b.sig, a.sig):
		return better(a.sig, b.sig)
	case a.price != b.price:
		return a.price < b.price
	}
	return a.id < b.id
}

// capOpen keeps, in rank order, at most limit resources and at most
// x402PerRecipientMax candidates per recipient and x402PerDomainMax per
// registrable domain; vetted resources are exempt (and counted).
func capOpen(ranked []*X402Resource, limit int) []*X402Resource {
	perPayTo, perDomain := map[EVMAddress]int{}, map[string]int{}
	out := make([]*X402Resource, 0, min(len(ranked), limit))
	for _, r := range ranked {
		if len(out) >= limit {
			break
		}
		if !r.Vetted && (perPayTo[r.PayTo] >= x402PerRecipientMax || perDomain[r.domain] >= x402PerDomainMax) {
			continue
		}
		perPayTo[r.PayTo]++
		perDomain[r.domain]++
		out = append(out, r)
	}
	return out
}

// importCatalogue fetches every discovery URL, admits what passes the
// guardrails, keeps the best MaxResources, upserts them and reloads the
// snapshot. It returns how many resources it kept. A discovery URL that
// fails ends that URL's crawl; what was read is still kept. It runs in the
// background, never in a request's transaction.
func (x *x402) importCatalogue(ctx context.Context, now int64) (int, error) {
	cc := x.cfg.Catalogue
	if cc == nil {
		return 0, nil
	}
	hist, err := x.readHistory(ctx, now)
	if err != nil {
		return 0, x.importFailed(err)
	}
	idx := x.cfg.denyIndex(hist.denied)
	type found struct {
		r   X402Resource
		sig bazaarSignal
		src string
	}
	byID := map[string]*found{}
	conflict := map[string]bool{}
	full := false
	var firstErr error
	for _, base := range cc.DiscoveryURLs {
		if full {
			break
		}
		host := base
		if u, err := url.Parse(base); err == nil {
			host = u.Hostname()
		}
		// Only the default discovery API's popularity is trusted: another
		// facilitator's figures are whatever it says (security review of
		// the aggregator, M2).
		trusted := base == cc.trusted
		offset := 0
		for page := 0; page < cc.MaxPages && !full; page++ {
			if page > 0 {
				select {
				case <-ctx.Done():
				case <-time.After(x402PagePause):
				}
			}
			items, total, err := x.discoveryPage(ctx, base, offset, cc.PageSize)
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				break
			}
			for _, raw := range items {
				r, sig, ok := bazaarResource(raw, x.cfg, min(cc.MaxPrice, x.cfg.PerCall))
				if !trusted {
					sig = bazaarSignal{}
				}
				if !ok || (sig.Known && sig.Payers < cc.MinPayers) {
					continue
				}
				r.ID = openID(r.ID, r.Method, r.URL)
				if !x.cfg.admitOpenWith(&r, idx) {
					continue
				}
				prev, dup := byID[r.ID]
				if !dup {
					// Bounded work and memory, whatever the discovery lists
					// (L3): twice what is kept is plenty to choose from.
					if len(byID) >= 2*cc.MaxResources {
						full = true
						break
					}
					byID[r.ID] = &found{r, sig, "bazaar " + host}
					continue
				}
				// Listed twice: keep the most conservative reading, never the
				// more flattering one; two recipients for one endpoint drop it.
				if prev.r.PayTo != r.PayTo {
					conflict[r.ID] = true
				}
				prev.sig = conservative(prev.sig, sig)
				prev.r.MaxAmount = min(prev.r.MaxAmount, r.MaxAmount)
			}
			// The server may send fewer items than asked (a lower page cap):
			// the next page starts after what it sent.
			offset += len(items)
			if len(items) == 0 || (total > 0 && int64(offset) >= total) || (total == 0 && len(items) < cc.PageSize) {
				break
			}
		}
	}
	all := make([]*X402Resource, 0, len(byID))
	keys := map[*X402Resource]x402RankKey{}
	for id, f := range byID {
		if conflict[id] {
			continue
		}
		r := &f.r
		v, ok := hist.vetted[id]
		r.Vetted = ok && v.matches(r)
		keys[r] = x402RankKey{vetted: r.Vetted, paid: hist.paid[id], sig: f.sig, price: r.MaxAmount, id: id}
		all = append(all, r)
	}
	sort.Slice(all, func(i, j int) bool { return keys[all[i]].before(keys[all[j]]) })
	all = capOpen(all, cc.MaxResources)
	for start := 0; start < len(all); start += x402UpsertBatch {
		tx, err := x.db.BeginTx(ctx, nil)
		if err != nil {
			return 0, x.importFailed(err)
		}
		for _, r := range all[start:min(start+x402UpsertBatch, len(all))] {
			f := byID[r.ID]
			query, _ := json.Marshal(r.Query)
			if query == nil || string(query) == "null" {
				query = []byte("[]")
			}
			// An id is its method and URL's hash: a row whose URL or method
			// differs is a collision, never overwritten (L2).
			if _, err = tx.ExecContext(ctx, `INSERT INTO x402_catalogue(id,bundler,url,method,pay_to,amount,query,body,category,summary,source,payers,calls,curated,first_seen,last_seen)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET pay_to=excluded.pay_to, amount=excluded.amount, query=excluded.query, body=excluded.body, category=excluded.category,
 summary=excluded.summary, source=excluded.source, payers=excluded.payers, calls=excluded.calls, curated=excluded.curated, last_seen=excluded.last_seen
 WHERE x402_catalogue.url=excluded.url AND x402_catalogue.method=excluded.method`,
				r.ID, r.Bundler, r.URL, r.Method, r.PayTo.String(), r.MaxAmount, string(query), b2i(r.Body), r.Category, r.Summary, f.src,
				f.sig.Payers, f.sig.Calls, b2i(f.sig.Curated), now, now); err != nil {
				_ = tx.Rollback()
				return 0, x.importFailed(err)
			}
		}
		if err = tx.Commit(); err != nil {
			return 0, x.importFailed(err)
		}
	}
	if firstErr != nil && len(all) == 0 {
		return 0, x.importFailed(firstErr)
	}
	x.cat.lastError.Store(nil)
	return len(all), x.loadCatalogue(ctx, now)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (x *x402) importFailed(err error) error {
	msg := "import failed"
	x.cat.lastError.Store(&msg)
	return err
}

// better orders two resources by the facilitator's signal: curated, then
// unique payers, then calls.
func better(a, b bazaarSignal) bool {
	if a.Curated != b.Curated {
		return a.Curated
	}
	if a.Payers != b.Payers {
		return a.Payers > b.Payers
	}
	return a.Calls > b.Calls
}

// conservative is two readings of one resource's signal merged the least
// flattering way: the fewer payers and calls, curated only if both say so,
// known only if both published it.
func conservative(a, b bazaarSignal) bazaarSignal {
	return bazaarSignal{Payers: min(a.Payers, b.Payers), Calls: min(a.Calls, b.Calls), Curated: a.Curated && b.Curated, Known: a.Known && b.Known}
}

// discoveryPage fetches one page of a discovery list through the SSRF-safe
// client: the raw items and the published total.
func (x *x402) discoveryPage(ctx context.Context, base string, offset, limit int) ([]json.RawMessage, int64, error) {
	u, err := url.Parse(base)
	if err != nil {
		return nil, 0, err
	}
	q := u.Query()
	q.Set("type", "http")
	q.Set("limit", strconv.Itoa(limit))
	q.Set("offset", strconv.Itoa(offset))
	u.RawQuery = q.Encode()
	ctx, cancel := context.WithTimeout(ctx, x402PageTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", "SwarmMemo-x402/1 (+https://swarmmemo.com/protocol.md)")
	req.Header.Set("Accept", "application/json")
	resp, err := x.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("x402: discovery answered %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, bazaarPageBytesMax+1))
	if err != nil {
		return nil, 0, err
	}
	page, err := parseBazaarPage(raw)
	if err != nil {
		return nil, 0, err
	}
	return page.Items, page.Total, nil
}

// catalogueRow is one x402_catalogue row as read.
type catalogueRow struct {
	r     X402Resource
	payTo string
	query string
	sig   bazaarSignal
	seen  int64
	hash  string // the summary's SHA-256, its key in x402_summary_screens
}

// loadCatalogue reads the fresh rows, our call counts and paid answers, the
// vetted and denied resources into a new snapshot, applying the guardrails
// again. Each statement reads into memory and closes its rows (the fresh
// rows x402LoadPage at a time) before the filtering, ranking and indexing,
// which hold no connection (security review of the aggregator, M3).
func (x *x402) loadCatalogue(ctx context.Context, now int64) error {
	s := &x402Snapshot{byID: map[string]*X402Resource{}, signal: map[string]bazaarSignal{}, stats: map[string]callStats{}, categories: map[string]int{}, loadedAt: now}
	type statRow struct {
		id string
		st callStats
	}
	stats, err := queryAll(ctx, x.db, func(r *sql.Rows) (statRow, error) {
		var v statRow
		return v, r.Scan(&v.id, &v.st.ok, &v.st.failed)
	}, "SELECT resource, SUM(ok), SUM(failed) FROM x402_resource_days WHERE day>=? GROUP BY resource", now/86400-x402StatsDays)
	if err != nil {
		return err
	}
	for _, v := range stats {
		s.stats[v.id] = v.st
	}
	hist, err := x.readHistory(ctx, now)
	if err != nil {
		return err
	}
	s.paid = hist.paid
	var rows []catalogueRow
	var verdicts map[string]string // summary hash → screen verdict
	if cc := x.cfg.Catalogue; cc != nil {
		stale := now - 3*int64(cc.Refresh/time.Second)
		// Imports keep at most MaxResources each; three intervals of them
		// bound the fresh rows.
		for after := ""; len(rows) < 3*cc.MaxResources; {
			page, err := queryAll(ctx, x.db, func(q *sql.Rows) (catalogueRow, error) {
				var c catalogueRow
				var body, curated int64
				err := q.Scan(&c.r.ID, &c.r.Bundler, &c.r.URL, &c.r.Method, &c.payTo, &c.r.MaxAmount, &c.query, &body, &c.r.Category, &c.r.Summary, &c.r.Source, &c.sig.Payers, &c.sig.Calls, &curated, &c.seen)
				c.r.Body, c.sig.Curated = body != 0, curated != 0
				return c, err
			}, `SELECT id,bundler,url,method,pay_to,amount,query,body,category,summary,source,payers,calls,curated,last_seen
FROM x402_catalogue WHERE last_seen>=? AND id>? ORDER BY id LIMIT ?`, stale, after, min(x402LoadPage, 3*cc.MaxResources-len(rows)))
			if err != nil {
				return err
			}
			rows = append(rows, page...)
			if len(page) < x402LoadPage {
				break
			}
			after = page[len(page)-1].r.ID
		}
		var hashes []string
		seen := map[string]bool{}
		for i := range rows {
			c := &rows[i]
			if c.hash = sha256Of([]byte(c.r.Summary)); !seen[c.hash] {
				seen[c.hash] = true
				hashes = append(hashes, c.hash)
			}
		}
		if verdicts, err = x.readScreens(ctx, hashes); err != nil {
			return err
		}
	}
	// No connection is held from here on.
	x.cat.denied.Store(hist.denied)
	if cc := x.cfg.Catalogue; cc != nil {
		idx := x.cfg.denyIndex(hist.denied)
		keys := make(map[*X402Resource]x402RankKey, len(rows))
		for i := range rows {
			c := &rows[i]
			r := &c.r
			var ok bool
			if r.PayTo, ok = ParseEVMAddress(c.payTo); !ok || json.Unmarshal([]byte(c.query), &r.Query) != nil || !x.cfg.admitOpenWith(r, idx) {
				continue
			}
			if _, pinned := x.byID[r.ID]; pinned {
				continue
			}
			v, ok := hist.vetted[r.ID]
			r.Vetted = ok && v.matches(r)
			r.summaryStatus = summaryStatusOf(r, verdicts[c.hash])
			s.importedAt = max(s.importedAt, c.seen)
			s.signal[r.ID] = c.sig
			keys[r] = x402RankKey{demoted: s.stats[r.ID].demoted(), vetted: r.Vetted, paid: hist.paid[r.ID], sig: c.sig, price: r.MaxAmount, id: r.ID}
			s.open = append(s.open, r)
		}
		sort.SliceStable(s.open, func(i, j int) bool { return keys[s.open[i]].before(keys[s.open[j]]) })
		s.open = capOpen(s.open, cc.MaxResources)
		for _, r := range s.open {
			s.byID[r.ID] = r
			s.categories[r.Category]++
		}
	}
	x.cat.snap.Store(s)
	return nil
}

// demoted reports whether our own paid calls to a resource mostly fail: at
// least three in the window, more than half failed.
func (st callStats) demoted() bool { return st.ok+st.failed >= 3 && st.failed*2 > st.ok+st.failed }

// countCall records one call's outcome for the success rate: ok when the
// agent got an answer; failed when the upstream failed us after our payment
// reached it (sent: no answer, or a second 402). Anything before a payment
// (an error the caller's arguments can cause, a price or payment we cannot
// make) and our own refusals (caps, pause, arguments) count nothing: demotion
// makes a resource uncallable, so no caller may cause it for free.
func (x *x402) countCall(resource string, now int64, err error, sent bool) {
	col := "ok"
	if err != nil {
		if code := errCodeOf(err); !sent || (code != "upstream_failed" && code != "x402_payment_rejected") {
			return
		}
		col = "failed"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = x.db.ExecContext(ctx, "INSERT INTO x402_resource_days(resource,day,"+col+") VALUES(?,?,1) ON CONFLICT(resource,day) DO UPDATE SET "+col+"="+col+"+1", resource, now/86400)
}

func errCodeOf(err error) string {
	var e *allowance.Err
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// Summary screening.

// An open resource's summary_status: its summary is served when the
// operator vetted the resource or the summary passed the text screen, and
// withheld (empty) when the screen flagged it or has not screened it yet.
const (
	summaryVetted   = "vetted"
	summaryScreened = "screened"
	summaryWithheld = "withheld"
	summaryPending  = "pending"
)

// summaryStatusOf is r's summary_status, given its summary's screen verdict
// ("" when not screened). An empty summary has nothing to withhold.
func summaryStatusOf(r *X402Resource, verdict string) string {
	switch {
	case r.Vetted:
		return summaryVetted
	case verdict == "pass" || r.Summary == "":
		return summaryScreened
	case verdict == "flag":
		return summaryWithheld
	}
	return summaryPending
}

// servedSummary is the summary the resources read serves: a pinned
// resource's (the operator's text), an open one's only when vetted or
// screened.
func (r *X402Resource) servedSummary() string {
	if !r.Open || r.summaryStatus == summaryVetted || r.summaryStatus == summaryScreened {
		return r.Summary
	}
	return ""
}

// readScreens reads the screen verdicts of the summaries with these hashes,
// x402LoadPage at a time.
func (x *x402) readScreens(ctx context.Context, hashes []string) (map[string]string, error) {
	out := make(map[string]string, len(hashes))
	for start := 0; start < len(hashes); start += x402LoadPage {
		chunk := hashes[start:min(start+x402LoadPage, len(hashes))]
		args := make([]any, len(chunk))
		for i, h := range chunk {
			args[i] = h
		}
		got, err := queryAll(ctx, x.db, func(r *sql.Rows) ([2]string, error) {
			var v [2]string
			return v, r.Scan(&v[0], &v[1])
		}, "SELECT hash,verdict FROM x402_summary_screens WHERE hash IN (?"+strings.Repeat(",?", len(chunk)-1)+")", args...)
		if err != nil {
			return nil, err
		}
		for _, v := range got {
			out[v[0]] = v[1]
		}
	}
	return out, nil
}

// screenSummaries screens the pending summaries of the snapshot's open
// resources with screen's classifier, best ranked first, at most
// x402ScreenPerRefresh distinct texts, and records each verdict (at
// ScreenThreshold) in x402_summary_screens by the text's SHA-256: an
// identical text is screened once, a changed one again. It does nothing when
// the classifier cannot answer now, and stops at its first error (among
// them the screen sub-cap of the Jev budget spent); what is left stays
// pending until the next refresh. The texts come from the snapshot and the
// verdicts are written in one short transaction afterwards: no connection is
// held while the classifier runs. Background only; it returns how many texts
// it screened.
func (x *x402) screenSummaries(ctx context.Context, now int64) (int, error) {
	s := x.cat.snap.Load()
	if s == nil || x.screener == nil || !x.screener.ScreenAvailable(ctx) {
		return 0, nil
	}
	verdicts := map[string]string{}
	var err error
	for _, r := range s.open {
		if len(verdicts) >= x402ScreenPerRefresh {
			break
		}
		if r.summaryStatus != summaryPending {
			continue
		}
		hash := sha256Of([]byte(r.Summary))
		if _, done := verdicts[hash]; done {
			continue
		}
		var res TextScreen
		if res, err = x.screener.ScreenText(ctx, r.Summary, "tool", x402ScreenIntent); err != nil {
			break
		}
		categories, ok := res.categories()
		if !ok {
			err = errors.New("x402: the summary screen's answer is unusable")
			break
		}
		verdicts[hash] = screenVerdict(categories, ScreenThreshold)
	}
	if len(verdicts) == 0 {
		return 0, err
	}
	// The screens are paid for: written and loaded even when the deadline
	// ended the loop.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	tx, werr := x.db.BeginTx(wctx, nil)
	if werr != nil {
		return 0, werr
	}
	defer tx.Rollback()
	for hash, verdict := range verdicts {
		if _, werr = tx.ExecContext(wctx, "INSERT INTO x402_summary_screens(hash,verdict,screened_at) VALUES(?,?,?) ON CONFLICT(hash) DO NOTHING", hash, verdict, now); werr != nil {
			return 0, werr
		}
	}
	if werr = tx.Commit(); werr != nil {
		return 0, werr
	}
	return len(verdicts), errors.Join(err, x.loadCatalogue(wctx, now))
}

// Vetting.

// X402Vetting is what swarmmemo x402 vet and unvet report: the open
// resource as vetted, and its summary (upstream text, unreviewed).
type X402Vetting struct {
	ID, URL, Method, PayTo, Summary string
	MaxAmount                       int64
	Vetted                          bool
	Cleared                         int // automatic denies cleared by vetting
}

// VetX402 marks the open resource id vetted (vet) or not, binding the
// vetting to its current URL, method and recipient; vetting also clears the
// automatic denies of that URL and recipient. The relay picks it up at its
// next catalogue load (within x402ReloadEvery seconds); a call already
// quoted checks again before it pays. Operator only; never a request path.
func VetX402(ctx context.Context, db *sql.DB, id string, vet bool, now int64) (X402Vetting, error) {
	v := X402Vetting{ID: id, Vetted: vet}
	err := db.QueryRowContext(ctx, "SELECT url,method,pay_to,amount,summary FROM x402_catalogue WHERE id=?", id).Scan(&v.URL, &v.Method, &v.PayTo, &v.MaxAmount, &v.Summary)
	if errors.Is(err, sql.ErrNoRows) {
		return v, fmt.Errorf("x402: no open catalogue resource %q (pinned resources need no vetting)", id)
	}
	if err != nil {
		return v, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return v, err
	}
	defer tx.Rollback()
	state := map[bool]string{true: "vetted", false: "unvetted"}[vet]
	if _, err = tx.ExecContext(ctx, `INSERT INTO x402_vetted(id,url,method,pay_to,state,changed_at) VALUES(?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET url=excluded.url, method=excluded.method, pay_to=excluded.pay_to, state=excluded.state, changed_at=excluded.changed_at`,
		id, v.URL, v.Method, v.PayTo, state, now); err != nil {
		return v, err
	}
	if vet {
		res, err := tx.ExecContext(ctx, "UPDATE x402_denied SET cleared_at=? WHERE denied_at>cleared_at AND ((kind='pay_to' AND value=?) OR (kind='url' AND value=?))", now, v.PayTo, canonicalX402URL(v.URL))
		if err != nil {
			return v, err
		}
		n, _ := res.RowsAffected()
		v.Cleared = int(n)
	}
	return v, tx.Commit()
}

// stillCallable is callable, checked again in the database after the
// command's transaction, before anything is paid: the operator may have
// unvetted the resource, or a concurrent call denied it, since the load.
func (x *x402) stillCallable(ctx context.Context, r *X402Resource) bool {
	if !r.Open {
		return true
	}
	var vetted, denied int
	err := x.db.QueryRowContext(ctx, `SELECT
 (SELECT count(*) FROM x402_vetted WHERE id=? AND state='vetted' AND url=? AND method=? AND pay_to=?),
 (SELECT count(*) FROM x402_denied WHERE denied_at>cleared_at AND ((kind='pay_to' AND value=?) OR (kind='url' AND value=?)))`,
		r.ID, r.URL, r.Method, r.PayTo.String(), r.PayTo.String(), r.canon).Scan(&vetted, &denied)
	return err == nil && vetted > 0 && denied == 0
}

// autoDeny counts the open payments to r's recipient that left our hands
// without an answer since the recipient was last cleared, and denies the
// recipient and r's URL at x402DenyAfter; the denial applies at once in
// memory and persists in x402_denied. Runs after the command committed.
func (x *x402) autoDeny(r *X402Resource, now int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	payTo := r.PayTo.String()
	var n int
	if err := x.db.QueryRowContext(ctx, `SELECT count(*) FROM x402_payments WHERE day>=? AND allowlist_version=0 AND pay_to=? AND state IN ('unknown','rejected')
 AND created_at>(SELECT COALESCE(MAX(cleared_at),0) FROM x402_denied WHERE kind='pay_to' AND value=?)`, now/86400-x402StatsDays, payTo, payTo).Scan(&n); err != nil || n < x402DenyAfter {
		return
	}
	for _, d := range [][2]string{{"pay_to", payTo}, {"url", r.canon}} {
		_, _ = x.db.ExecContext(ctx, `INSERT INTO x402_denied(kind,value,reason,denied_at) VALUES(?,?,'unanswered_payments',?)
ON CONFLICT(kind,value) DO UPDATE SET reason=excluded.reason, denied_at=excluded.denied_at WHERE x402_denied.denied_at<=x402_denied.cleared_at`, d[0], d[1], now)
	}
	for {
		old := x.cat.denied.Load()
		next := &x402Denied{payTo: map[string]bool{payTo: true}, url: map[string]bool{r.canon: true}}
		if old != nil {
			for k := range old.payTo {
				next.payTo[k] = true
			}
			for k := range old.url {
				next.url[k] = true
			}
		}
		if x.cat.denied.CompareAndSwap(old, next) {
			return
		}
	}
}

// Search.

type x402Search struct {
	words    []string
	category string
	maxPrice int64 // 0: any
	limit    int
	offset   int
}

// x402SearchArgs are the resources read's arguments.
type x402SearchArgs struct {
	Query    string      `json:"query"`
	Category string      `json:"category"`
	MaxPrice string      `json:"max_price"`
	Limit    json.Number `json:"limit"`
	Cursor   string      `json:"cursor"`
}

func parseX402Search(raw json.RawMessage, decimals int) (x402Search, error) {
	var a x402SearchArgs
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	if err := StrictObject(raw, &a); err != nil {
		return x402Search{}, err
	}
	s := x402Search{limit: x402PageDefault, category: a.Category}
	if len(a.Query) > x402SearchBytes || (a.Category != "" && !x402CategoryRE.MatchString(a.Category)) {
		return x402Search{}, refusal("invalid_service_data")
	}
	for _, w := range strings.Fields(strings.ToLower(a.Query)) {
		if !slices.Contains(s.words, w) {
			s.words = append(s.words, w)
		}
	}
	if len(s.words) > x402SearchWordsMax {
		return x402Search{}, refusal("invalid_service_data")
	}
	if a.MaxPrice != "" {
		n, ok := parseUnits(a.MaxPrice, decimals)
		if !ok || n <= 0 {
			return x402Search{}, refusal("invalid_service_data")
		}
		s.maxPrice = n
	}
	if a.Limit != "" {
		n, err := strconv.Atoi(a.Limit.String())
		if err != nil || n < 1 || n > X402PageMax {
			return x402Search{}, refusal("invalid_service_data")
		}
		s.limit = n
	}
	if a.Cursor != "" {
		n, err := strconv.Atoi(a.Cursor)
		if err != nil || n < 0 || n > x402CatalogueCeiling+X402ResourcesMax {
			return x402Search{}, refusal("invalid_service_data")
		}
		s.offset = n
	}
	return s, nil
}

type x402SearchPage struct {
	resources  []*X402Resource
	stats      map[string]callStats
	matched    int
	next       string
	open       int
	importedAt int64
	categories map[string]int // every listed resource, by category
}

// search is one page of the listed resources that match s: pinned ones
// first, in the allowlist's order, then open ones, best first. It reads
// only the snapshot's precomputed fields: no parsing, no allocation per
// resource.
func (x *x402) search(s x402Search) x402SearchPage {
	snap := x.cat.snap.Load()
	if snap == nil {
		snap = &x402Snapshot{} // not loaded yet: the allowlist alone
	}
	out := x402SearchPage{stats: snap.stats, open: len(snap.open), importedAt: snap.importedAt, categories: make(map[string]int, len(snap.categories)+4)}
	for k, n := range snap.categories {
		out.categories[k] = n
	}
	match := func(r *X402Resource) bool {
		if (s.category != "" && r.Category != s.category) || (s.maxPrice > 0 && min(r.MaxAmount, x.cfg.PerCall) > s.maxPrice) {
			return false
		}
		for _, w := range s.words {
			if !strings.Contains(r.text, w) {
				return false
			}
		}
		return true
	}
	add := func(r *X402Resource) {
		if !match(r) {
			return
		}
		if out.matched >= s.offset && len(out.resources) < s.limit {
			out.resources = append(out.resources, r)
		}
		out.matched++
	}
	for i := range x.cfg.Resources {
		r := &x.cfg.Resources[i]
		if b := x.bundlers[r.Bundler]; b == nil || !b.Ready() {
			continue
		}
		out.categories[r.Category]++
		add(r)
	}
	denied := x.cat.denied.Load()
	for _, r := range snap.open {
		if !denied.has(r) {
			add(r)
		}
	}
	if s.offset+len(out.resources) < out.matched {
		out.next = strconv.Itoa(s.offset + len(out.resources))
	}
	return out
}

// Statistics.

// X402Day is one UTC day of relay spend, in atomic units, for /stats.
type X402Day struct {
	Day     string `json:"day"`
	Paid    int64  `json:"paid"`    // settled or answered payments
	AtRisk  int64  `json:"at_risk"` // signed with no known outcome: may have settled
	Calls   int64  `json:"calls"`   // paid calls
	Refused int64  `json:"refused"` // payments the upstream rejected
}

// X402Stats is the public spend summary: /stats and /api/stats/x402.
type X402Stats struct {
	Days        []X402Day `json:"days"` // newest last
	GlobalDaily string    `json:"global_daily"`
	OpenDaily   string    `json:"open_daily,omitempty"`
	Pinned      int       `json:"pinned_resources"`
	Open        int       `json:"open_resources"`
	Bundlers    []string  `json:"bundlers"`
	Decimals    int       `json:"decimals"`
	Paused      bool      `json:"paused"`
}

// ReadX402Stats is the relay's spend over the last days UTC days; nil when
// x402 is not configured.
func (r *Registry) ReadX402Stats(ctx context.Context, q allowance.Querier, now int64, days int) (*X402Stats, error) {
	p, err := r.Lookup("x402")
	if err != nil {
		return nil, nil
	}
	x, ok := p.(*x402)
	if !ok || x.cfg == nil {
		return nil, nil
	}
	today := now / 86400
	st := &X402Stats{GlobalDaily: formatUnits(x.cfg.GlobalDaily, x.cfg.Decimals), Pinned: len(x.cfg.Resources), Bundlers: x.readyBundlers(), Decimals: x.cfg.Decimals, Paused: x.paused()}
	if cc := x.cfg.Catalogue; cc != nil {
		st.OpenDaily = formatUnits(cc.OpenDaily, x.cfg.Decimals)
		if s := x.cat.snap.Load(); s != nil {
			st.Open = len(s.open)
		}
	}
	byDay := map[int64]*X402Day{}
	for d := today - int64(days) + 1; d <= today; d++ {
		st.Days = append(st.Days, X402Day{Day: time.Unix(d*86400, 0).UTC().Format("2006-01-02")})
		byDay[d] = &st.Days[len(st.Days)-1]
	}
	rows, err := q.QueryContext(ctx, `SELECT day,
 COALESCE(SUM(CASE WHEN state='paid' THEN amount END),0), COALESCE(SUM(CASE WHEN state IN ('signed','unknown') THEN amount END),0),
 COALESCE(SUM(state='paid'),0), COALESCE(SUM(state='rejected'),0)
FROM x402_payments WHERE day>? GROUP BY day`, today-int64(days))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var day int64
		var d X402Day
		if err = rows.Scan(&day, &d.Paid, &d.AtRisk, &d.Calls, &d.Refused); err != nil {
			return nil, err
		}
		if slot := byDay[day]; slot != nil {
			d.Day = slot.Day
			*slot = d
		}
	}
	return st, rows.Err()
}
