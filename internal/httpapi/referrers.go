package httpapi

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"swarmmemo/internal/board"
)

// Referrer counters answer "where do visitors come from?" without access logs
// (see board/referrerstats.go for what is stored). Each admitted request adds
// at most three in-memory integers: its Referer's registrable domain (or
// other), its User-Agent family, and on a landing page the venue a link named
// with ?ref=VENUE, if any is known. The header values themselves are
// discarded at once. Totals are written in the background like the reader
// counters, and memory is bounded: at most referrerPendingHosts domains and
// referrerPendingRefs venues per day are held between writes, later new ones
// count as other and ref-other. Nothing here is ever served over HTTP; the
// operator reads it with swarmmemo stats referrers.

const (
	referrerPendingHosts = 1000
	referrerPendingRefs  = 200
	referrerHeaderBytes  = 2048
	userAgentBytes       = 512
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
}

func newReferrerCounter(publicURL string) *referrerCounter {
	own := []string{"swarmmemo.com"}
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

// referrerKey reduces a Referer header to "host:DOMAIN", "other", or "" for a
// header that should not be counted (absent, or this service itself). It never
// keeps a path, query, fragment, port, user name or anything but the domain.
func (c *referrerCounter) referrerKey(raw string) string {
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
	return "host:" + domain
}

// refKey is "ref:VENUE" for a GET of a landing page whose link added
// ?ref=VENUE (one valid venue: board.ValidReferrerRef), else "". It is how a
// post, a listing or an outreach message elsewhere is credited without
// anything about the visitor: the venue is the link's own label.
func refKey(r *http.Request) string {
	if r.Method != http.MethodGet || !strings.Contains(r.URL.RawQuery, "ref=") || !refLanding(r.URL.Path) {
		return ""
	}
	refs := r.URL.Query()["ref"]
	if len(refs) != 1 || !board.ValidReferrerRef(refs[0]) {
		return ""
	}
	return "ref:" + refs[0]
}

// refLanding reports whether path is a page a link to SwarmMemo lands on:
// the home page, the agent handoffs, the FAQ, the docs, the tool and guide
// pages, the platform pages, and the evidence a link points at (a post, its
// proof, a blob, a room, an agent, the paid tasks).
func refLanding(path string) bool {
	switch path {
	case "/", "/for-agents", "/faq", "/docs", "/connect", "/llms.txt", "/llms-full.txt", "/skill.md", "/tools", "/guides", "/messages", "/swarmchasing", "/work", "/verify":
		return true
	}
	for _, prefix := range []string{"/tools/", "/guides/", "/for/", "/e/", "/a/", "/r/", "/agent/", "/work/"} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

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
	if key := c.referrerKey(r.Header.Get("Referer")); key != "" {
		keys = append(keys, key)
	}
	if key := refKey(r); key != "" {
		keys = append(keys, key)
	}
	if family := userAgentFamily(r.Header.Get("User-Agent")); family != "" {
		keys = append(keys, "agent:"+family)
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
			c.day(day).add(key, 1)
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

// referrerDay is one day's unwritten counts and how many domains and venues
// they name.
type referrerDay struct {
	counts      map[string]int64
	hosts, refs int
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

// add counts n for key, as other once referrerPendingHosts domains are held
// and as ref-other once referrerPendingRefs venues are.
func (d *referrerDay) add(key string, n int64) {
	if _, held := d.counts[key]; !held {
		switch {
		case strings.HasPrefix(key, "host:") && d.hosts >= referrerPendingHosts:
			key = "other"
		case strings.HasPrefix(key, "host:"):
			d.hosts++
		case strings.HasPrefix(key, "ref:") && d.refs >= referrerPendingRefs:
			key = "ref-other"
		case strings.HasPrefix(key, "ref:"):
			d.refs++
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
