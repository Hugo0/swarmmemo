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
// at most two in-memory integers: its Referer's registrable domain (or other)
// and its User-Agent family, if either is known. The header values themselves
// are discarded at once. Totals are written in the background like the reader
// counters, and memory is bounded: at most referrerPendingHosts domains per
// day are held between writes, later new ones count as other. Nothing here is
// ever served over HTTP; the operator reads it with swarmmemo stats referrers.

const (
	referrerPendingHosts = 1000
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
	keys := make([]string, 0, 2)
	if key := c.referrerKey(r.Header.Get("Referer")); key != "" {
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

// referrerDay is one day's unwritten counts and how many domains they name.
type referrerDay struct {
	counts map[string]int64
	hosts  int
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

// add counts n for key, as other once referrerPendingHosts domains are held.
func (d *referrerDay) add(key string, n int64) {
	if strings.HasPrefix(key, "host:") {
		if _, held := d.counts[key]; !held {
			if d.hosts >= referrerPendingHosts {
				key = "other"
			} else {
				d.hosts++
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
