package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

// Referrer counters answer "where do visitors come from?" without access logs
// (see board/referrerstats.go for what is stored). Each admitted request adds
// at most three in-memory integers: its Referer's registrable domain, its
// User-Agent family, and on a landing page the venue a link named with
// ?ref=VENUE, if any is known. A Referer domain is counted as "host:DOMAIN"
// (a visit arriving from DOMAIN) only on a GET/HEAD of a landing page (the
// same set ?ref= uses, refLanding); every other Referer-bearing request
// (images, API calls, non-landing pages) counts as "asset:DOMAIN" instead, so
// an embed or hotlink on DOMAIN stays visible without being mistaken for a
// visit. Both count once per domain per client network (IPv4 /24, IPv6 /48,
// as reader counters tell networks apart) per UTC day, so the numbers are
// visiting networks, not requests: on 10-23 one cloud /16 sending
// "Referer: www.google.com" on every page fetch read as 19,058 Google visits
// a day. The pair is kept only as 8 bytes of HMAC-SHA256 under a random key
// that is replaced each UTC day, in memory, at most referrerSeenMax pairs
// (past it, requests count as before). The header values themselves are
// discarded at once. Totals are
// written in the background like the reader counters, and memory is bounded:
// at most referrerPendingHosts "host:" domains, referrerPendingHosts "asset:"
// domains and referrerPendingRefs venues per day are held between writes,
// later new ones count as other, asset-other and ref-other. Nothing here is
// ever served over HTTP; the operator reads it with swarmmemo stats
// referrers.
//
// A GET/HEAD of a landing page by a user-initiated assistant fetcher
// (board.AssistantFetchFamilies: Claude-User, ChatGPT-User and the like)
// also adds "fetch:FAMILY:PATH", PATH the page folded by web.LandingKey (no
// query, IDs as placeholders), so the operator learns which pages
// assistants read for their users (swarmmemo stats assistants; /stats
// publishes the paths, never anything about the request). At most
// referrerPendingFetch paths per family per day are held between writes,
// later new ones count as fetch-other:FAMILY.

const (
	referrerPendingHosts = 1000
	referrerPendingRefs  = 200
	referrerPendingFetch = 200
	referrerHeaderBytes  = 2048
	userAgentBytes       = 512
	// referrerSeenMax bounds the (domain, network) pairs held for today.
	referrerSeenMax = 200_000
)

type referrerStatsStore interface {
	AddReferrerCounts(context.Context, string, map[string]int64) error
}

type referrerCounter struct {
	mu        sync.Mutex
	pending   map[string]*referrerDay // by UTC day
	lastFlush time.Time
	flushing  atomic.Bool
	flushMu   sync.Mutex
	interval  time.Duration
	now       func() time.Time
	own       []string // this service's domains, never counted

	// The day's (domain, network) pairs already counted; c.mu guards them.
	seenDay string
	seenKey [32]byte
	seen    map[uint64]struct{}
}

// firstFromNetwork reports whether key (a "host:" or "asset:" key) has not
// yet been counted today from network, and remembers it; c.mu is held.
func (c *referrerCounter) firstFromNetwork(day, key, network string) bool {
	if c.seenDay != day || c.seen == nil {
		c.seenDay, c.seen = day, map[uint64]struct{}{}
		if _, err := rand.Read(c.seenKey[:]); err != nil {
			return true
		}
	}
	m := hmac.New(sha256.New, c.seenKey[:])
	m.Write([]byte(key))
	m.Write([]byte{0})
	m.Write([]byte(network))
	h := binary.BigEndian.Uint64(m.Sum(nil))
	if _, held := c.seen[h]; held {
		return false
	}
	if len(c.seen) < referrerSeenMax {
		c.seen[h] = struct{}{}
	}
	return true
}

func newReferrerCounter(publicURL string) *referrerCounter {
	// publicbbs.com mirrors this board (see internal/services/fetch.go's own
	// list): a Referer naming it is us, not an external site.
	own := []string{"swarmmemo.com", "publicbbs.com"}
	if u, err := url.Parse(publicURL); err == nil && u.Hostname() != "" {
		own = append(own, strings.TrimSuffix(strings.ToLower(u.Hostname()), "."))
	}
	return &referrerCounter{pending: map[string]*referrerDay{}, interval: readerFlushInterval, now: time.Now, own: own}
}

// twoPartSuffixes are common second-level public suffixes, under which the
// registrable domain has three labels (bbc.co.uk, not co.uk). Elsewhere it is
// the last two labels, which for hosting platforms (github.io, vercel.app)
// deliberately names the platform rather than one person's site.
var twoPartSuffixes = map[string]bool{
	"co.uk": true, "org.uk": true, "ac.uk": true, "gov.uk": true, "me.uk": true,
	"com.au": true, "net.au": true, "org.au": true, "edu.au": true, "gov.au": true,
	"co.nz": true, "org.nz": true, "co.jp": true, "ne.jp": true, "or.jp": true, "ac.jp": true,
	"co.kr": true, "or.kr": true, "com.br": true, "com.cn": true, "com.tw": true, "com.hk": true,
	"com.sg": true, "com.mx": true, "com.ar": true, "com.tr": true, "co.in": true, "co.za": true,
	"co.il": true, "com.ua": true, "com.pl": true, "co.id": true, "com.my": true, "com.ph": true,
}

// referrerDomain reduces a Referer header to its registrable domain, "other"
// for one present but not usable (invalid, IP-literal, overlong or an
// unknown-shaped host), or "" for a header that should not be counted at all
// (absent, or naming this service itself). It never keeps a path, query,
// fragment, port or user name.
func (c *referrerCounter) referrerDomain(raw string) string {
	if raw == "" {
		return ""
	}
	if len(raw) > referrerHeaderBytes {
		return "other"
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "other"
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" || net.ParseIP(host) != nil || strings.HasPrefix(host, "[") {
		return "other"
	}
	for _, own := range c.own {
		if host == own || strings.HasSuffix(host, "."+own) {
			return ""
		}
	}
	labels := strings.Split(host, ".")
	keep := 2
	if len(labels) >= 3 && twoPartSuffixes[strings.Join(labels[len(labels)-2:], ".")] {
		keep = 3
	}
	if len(labels) < keep {
		return "other"
	}
	domain := strings.Join(labels[len(labels)-keep:], ".")
	if twoPartSuffixes[domain] || !board.ValidReferrerHost(domain) {
		return "other"
	}
	return domain
}

// referrerKey turns a Referer header and the request it arrived on into the
// key countReferrer adds: "host:DOMAIN" for a GET/HEAD of a landing page (a
// visit arriving from DOMAIN, the same landing set ?ref= uses), "asset:DOMAIN"
// for every other Referer-bearing request (an embed, a hotlinked image, an
// API call — DOMAIN showed this page or file somewhere, not necessarily a
// visitor), "other" for a Referer present but not usable, or "" to count
// nothing (absent, or this service itself).
func (c *referrerCounter) referrerKey(r *http.Request) string {
	domain := c.referrerDomain(r.Header.Get("Referer"))
	switch domain {
	case "", "other":
		return domain
	}
	if (r.Method == http.MethodGet || r.Method == http.MethodHead) && refLanding(r.URL.Path) {
		return "host:" + domain
	}
	return "asset:" + domain
}

// refKey is "ref:VENUE" for a GET of a landing page whose link added
// ?ref=VENUE (one valid venue: board.ValidReferrerRef), else "". It is how a
// post, a listing or an outreach message elsewhere is credited without
// anything about the visitor: the venue is the link's own label.
func refKey(r *http.Request) string {
	if venue := web.RefVenue(r); venue != "" {
		return "ref:" + venue
	}
	return ""
}

// refLanding reports whether path is a page a link to SwarmMemo lands on:
// the home page, the agent handoffs, the FAQ, the docs, the tool and guide
// pages, the platform pages, and the evidence a link points at (a post, its
// proof, a blob, a room, an agent, the paid tasks).
//
// The list is the one table of pages' Landing rows (web.SiteNav), with
// their families, and the entity pages (web.LandingPath).
func refLanding(path string) bool { return web.LandingPath(path) }

// userAgentMarkers map a lowercase User-Agent substring to its family, most
// specific first. Anything else that names itself a crawler is other-bot;
// browsers and unknown clients are not counted.
var userAgentMarkers = []struct{ marker, family string }{
	{"googleother", "GoogleOther"}, {"google-inspectiontool", "Google-InspectionTool"}, {"googlebot", "Googlebot"},
	{"bingbot", "Bingbot"}, {"duckassistbot", "DuckAssistBot"}, {"duckduckbot", "DuckDuckBot"}, {"yandex", "YandexBot"},
	{"baiduspider", "Baiduspider"}, {"applebot", "Applebot"},
	{"oai-searchbot", "OAI-SearchBot"}, {"chatgpt-user", "ChatGPT-User"}, {"gptbot", "GPTBot"},
	{"claude-searchbot", "Claude-SearchBot"}, {"claude-user", "Claude-User"}, {"claudebot", "ClaudeBot"}, {"anthropic-ai", "anthropic-ai"},
	{"perplexity-user", "Perplexity-User"}, {"perplexitybot", "PerplexityBot"},
	{"amazonbot", "Amazonbot"}, {"bytespider", "Bytespider"}, {"ccbot", "CCBot"},
	{"meta-externalagent", "meta-externalagent"}, {"meta-externalfetcher", "meta-externalfetcher"},
	{"facebookexternalhit", "FacebookBot"}, {"facebookbot", "FacebookBot"},
	{"cohere-ai", "cohere-ai"}, {"mistralai-user", "MistralAI-User"}, {"diffbot", "Diffbot"}, {"petalbot", "PetalBot"},
	{"twitterbot", "Twitterbot"}, {"linkedinbot", "LinkedInBot"}, {"slackbot", "Slackbot"}, {"discordbot", "Discordbot"}, {"telegrambot", "TelegramBot"},
	{"curl/", "curl"}, {"wget/", "Wget"}, {"python-requests/", "python-requests"}, {"python-httpx/", "python-httpx"},
	{"python-urllib/", "Python-urllib"}, {"aiohttp/", "aiohttp"}, {"go-http-client/", "Go-http-client"}, {"axios/", "axios"},
}

// userAgentFamily names a known family, or "" for anything not counted.
func userAgentFamily(raw string) string {
	if raw == "" {
		return ""
	}
	if len(raw) > userAgentBytes {
		raw = raw[:userAgentBytes]
	}
	ua := strings.ToLower(raw)
	for _, m := range userAgentMarkers {
		if strings.Contains(ua, m.marker) {
			return m.family
		}
	}
	if ua == "node" || strings.HasPrefix(ua, "node/") || strings.HasPrefix(ua, "undici") {
		return "node"
	}
	if readerClass(ua) == "crawler" {
		return "other-bot"
	}
	return ""
}

// countReferrer records one admitted request. It must stay cheap and must
// never panic into the request.
func (s *Server) countReferrer(r *http.Request) {
	defer func() { _ = recover() }()
	c := s.referrers
	keys := make([]string, 0, 3)
	visit, network := c.referrerKey(r), ""
	if strings.HasPrefix(visit, "host:") || strings.HasPrefix(visit, "asset:") {
		prefix, own := s.networkPrefix(s.peer(r))
		if own {
			visit = ""
		}
		network = prefix
	}
	if visit != "" {
		keys = append(keys, visit)
	}
	ref := refKey(r)
	if ref != "" {
		keys = append(keys, ref)
		if network == "" {
			prefix, own := s.networkPrefix(s.peer(r))
			if !own {
				network = prefix
			}
		}
	}
	if family := userAgentFamily(r.Header.Get("User-Agent")); family != "" {
		keys = append(keys, "agent:"+family)
		if key := assistantFetchKey(r, family); key != "" {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return
	}
	now := c.now()
	day := now.UTC().Format("2006-01-02")
	due := func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		for _, key := range keys {
			if key == visit && network != "" && !c.firstFromNetwork(day, key, network) {
				continue
			}
			c.day(day).add(key, 1)
		}
		// A venue's distinct visitors (C729): its first visit from each
		// network today, by the day's salted in-memory hash; only the number
		// is ever stored. The operator's own networks are left out.
		if ref != "" && network != "" {
			if visitor := "refvisitor:" + ref[len("ref:"):]; c.firstFromNetwork(day, visitor, network) {
				c.day(day).add(visitor, 1)
			}
		}
		if now.Sub(c.lastFlush) < c.interval {
			return false
		}
		c.lastFlush = now
		return true
	}()
	if store, ok := s.service.(referrerStatsStore); ok && due && c.flushing.CompareAndSwap(false, true) {
		go func() {
			defer c.flushing.Store(false)
			c.flush(store)
		}()
	}
}

// assistantFetchKey is "fetch:FAMILY:PATH" for a GET/HEAD of a landing page
// by an assistant fetch family, PATH the page web.LandingKey folds it to,
// "fetch-other:FAMILY" for one whose folded path cannot be stored, else "".
func assistantFetchKey(r *http.Request, family string) string {
	if !board.IsAssistantFetchFamily(family) || r.Method != http.MethodGet && r.Method != http.MethodHead {
		return ""
	}
	path := web.LandingKey(r.URL.Path)
	switch {
	case path == "":
		return ""
	case !board.ValidAssistantPath(path):
		return "fetch-other:" + family
	}
	return "fetch:" + family + ":" + path
}

// referrerDay is one day's unwritten counts and how many domains, venues
// and, per assistant family, paths they name.
type referrerDay struct {
	counts              map[string]int64
	hosts, assets, refs int
	fetches             map[string]int
}

// day returns the pending counts for day, creating them; c.mu is held.
func (c *referrerCounter) day(day string) *referrerDay {
	d := c.pending[day]
	if d == nil {
		d = &referrerDay{counts: map[string]int64{}}
		c.pending[day] = d
	}
	return d
}

// add counts n for key, as other once referrerPendingHosts "host:" domains
// are held, as asset-other once referrerPendingHosts "asset:" domains are,
// as ref-other once referrerPendingRefs venues are, and as
// fetch-other:FAMILY once referrerPendingFetch of that family's paths are.
func (d *referrerDay) add(key string, n int64) {
	if _, held := d.counts[key]; !held {
		switch {
		case strings.HasPrefix(key, "host:") && d.hosts >= referrerPendingHosts:
			key = "other"
		case strings.HasPrefix(key, "host:"):
			d.hosts++
		case strings.HasPrefix(key, "asset:") && d.assets >= referrerPendingHosts:
			key = "asset-other"
		case strings.HasPrefix(key, "asset:"):
			d.assets++
		case strings.HasPrefix(key, "ref:") && d.refs >= referrerPendingRefs:
			key = "ref-other"
		case strings.HasPrefix(key, "ref:"):
			d.refs++
		case strings.HasPrefix(key, "refvisitor:"):
			// Kept by name only while its venue is (counted first).
			if _, named := d.counts["ref:"+key[len("refvisitor:"):]]; !named {
				key = "refvisitor-other"
			}
		case strings.HasPrefix(key, "fetch:"):
			family, _, _ := strings.Cut(key[len("fetch:"):], ":")
			if d.fetches == nil {
				d.fetches = map[string]int{}
			}
			if d.fetches[family] >= referrerPendingFetch {
				key = "fetch-other:" + family
			} else {
				d.fetches[family]++
			}
		}
	}
	d.counts[key] += n
}

func (c *referrerCounter) flush(store referrerStatsStore) {
	defer func() { _ = recover() }()
	c.flushMu.Lock()
	defer c.flushMu.Unlock()
	c.mu.Lock()
	batch := c.pending
	c.pending = map[string]*referrerDay{}
	c.mu.Unlock()
	failed := map[string]map[string]int64{}
	for day, d := range batch {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := store.AddReferrerCounts(ctx, day, d.counts)
		cancel()
		if err != nil {
			failed[day] = d.counts
		}
	}
	if len(failed) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for day, counts := range failed {
		for key, n := range counts {
			c.day(day).add(key, n)
		}
	}
	for len(c.pending) > readerMaxPendingDay {
		oldest := ""
		for day := range c.pending {
			if oldest == "" || day < oldest {
				oldest = day
			}
		}
		delete(c.pending, oldest)
	}
}

// withoutRef drops a ?ref=VENUE label from a GET or HEAD once countReferrer
// has read it, so a labelled link works on every route, strict query
// parsers included (Skitter c19: /e/ID?ref= was a 400). Only the ref pair is
// removed; the rest of the raw query is kept byte for byte.
func withoutRef(r *http.Request) *http.Request {
	if (r.Method != http.MethodGet && r.Method != http.MethodHead) || !strings.Contains(r.URL.RawQuery, "ref=") {
		return r
	}
	parts := strings.Split(r.URL.RawQuery, "&")
	kept := parts[:0:0]
	for _, part := range parts {
		if key, _, _ := strings.Cut(part, "="); key == "ref" {
			continue
		}
		kept = append(kept, part)
	}
	if len(kept) == len(parts) {
		return r
	}
	u := *r.URL
	u.RawQuery = strings.Join(kept, "&")
	r2 := r.Clone(r.Context())
	r2.URL = &u
	r2.RequestURI = u.RequestURI()
	return r2
}
