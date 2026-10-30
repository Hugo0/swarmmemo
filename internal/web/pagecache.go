package web

import (
	"bytes"
	"context"
	"net/http"
	"sync"
	"time"

	"swarmmemo/internal/board"
)

// The page cache (C554) keeps the anonymous render of the two public pages
// that read the most, the home page and /stats, for a short while, so a
// burst of readers costs one render. Only a request that names nothing of
// its own is served from it or fills it: GET or HEAD of exactly "/",
// "/stats" or "/stats?bucket=hour", with no Authorization and no Cookie
// header (an OAuth grant, a lounge or proof cookie, anything a later
// personalised render could read). Every other request renders as before.
//
// The home page is kept while the board has not changed: an entry holds the
// board's change signal (board.Store.Changes), taken before its render, and
// the next committed write ends it, so a reader who just posted never gets
// the page from before the post. homeCacheTTL bounds what no write signals
// (relative ages, hot ranking's decay, a moderator's hide). /stats reads the
// board's activity summary, itself recomputed once a minute, so it is kept
// for statsCacheTTL whatever is written, and served up to statsCacheStale
// old while one render refreshes it in the background: no reader waits for
// the summary's recomputation unless nothing is cached.
//
// One render per page runs at a time (singleflight): readers that arrive
// while it runs wait for its result. The render is detached from the
// request that started it, so that reader leaving does not fail the others.
// A response other than 200 goes to the readers that waited for it and is
// never kept.
//
// The mechanism is ResponseCache, which the API's hot anonymous reads use
// too (httpapi/hotreads.go, C637).
const (
	homeCacheTTL    = 10 * time.Second
	statsCacheTTL   = time.Minute
	statsCacheStale = 5 * time.Minute
	pageRenderLimit = 20 * time.Second
)

// CachePolicy is how long one cached response is kept.
type CachePolicy struct {
	TTL   time.Duration // served as is until this old
	Stale time.Duration // then, until this old, served while it refreshes; 0: never
	// Changes, when set, is taken before each render: its channel closing
	// ends the entry (a committed write, or a narrower signal).
	Changes func() <-chan struct{}
	// OwnFailures: a response other than 200 is not given to the readers
	// that waited for it either; each renders its own. A handler whose
	// failures depend on the requester (its network's rate, its quota) sets
	// it, so one reader's refusal never reaches another.
	OwnFailures bool
}

// pagePolicy is the page cache's policy of one request.
type pagePolicy struct {
	ttl      time.Duration
	stale    time.Duration
	onChange bool // a committed write ends the entry
}

// cachedPolicy is the policy of a request the cache may serve, by path and
// query, or false.
func cachedPolicy(r *http.Request) (pagePolicy, bool) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
		return pagePolicy{}, false
	}
	switch {
	case r.URL.Path == "/" && r.URL.RawQuery == "":
		return pagePolicy{ttl: homeCacheTTL, onChange: true}, true
	case r.URL.Path == "/stats" && (r.URL.RawQuery == "" || r.URL.RawQuery == "bucket=hour"):
		return pagePolicy{ttl: statsCacheTTL, stale: statsCacheStale}, true
	}
	return pagePolicy{}, false
}

// cachedPage is one kept render.
type cachedPage struct {
	status  int
	header  http.Header
	body    []byte
	at      time.Time
	keep    time.Duration   // the policy's longest age (TTL or Stale)
	changed <-chan struct{} // closed by the first change after the render began; nil: none
}

// pageFlight is a render in progress; done closes when page is set (nil if
// the render panicked).
type pageFlight struct {
	done chan struct{}
	page *cachedPage
}

// ResponseCache keeps whole responses (status, headers, body) by a key the
// caller chooses, for a policy's TTL, with one render per key at a time
// (singleflight) and at most max entries: a full cache first drops what has
// expired or ended, then its oldest entry. It decides nothing about who may
// be served from it: the caller serves only requests whose answer is the
// same for every reader of the key (pageCache below, httpapi/hotreads.go).
type ResponseCache struct {
	now     func() time.Time
	max     int // 0: unbounded (a fixed set of keys)
	mu      sync.Mutex
	pages   map[string]*cachedPage
	flights map[string]*pageFlight
}

// NewResponseCache is an empty cache of at most max entries (0: no bound).
func NewResponseCache(max int, now func() time.Time) *ResponseCache {
	return &ResponseCache{now: now, max: max, pages: map[string]*cachedPage{}, flights: map[string]*pageFlight{}}
}

// Len is how many responses are kept.
func (c *ResponseCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pages)
}

// Serve answers r with key's kept response, or with next's, which it keeps
// when it is a 200.
func (c *ResponseCache) Serve(w http.ResponseWriter, r *http.Request, key string, policy CachePolicy, next http.Handler) {
	c.mu.Lock()
	page := c.pages[key]
	if page != nil {
		switch age := c.now().Sub(page.at); {
		case age < 0 || closed(page.changed):
			page = nil
		case age < policy.TTL:
		case age < policy.Stale:
			c.startLocked(key, r, policy, next) // served stale; a render refreshes it
		default:
			page = nil
		}
	}
	var flight *pageFlight
	if page == nil {
		flight = c.startLocked(key, r, policy, next)
	}
	c.mu.Unlock()
	if flight != nil {
		select {
		case <-flight.done:
			page = flight.page
		case <-r.Context().Done():
			if policy.OwnFailures {
				next.ServeHTTP(w, r) // its own answer to its cancellation (a 503 while shutting down)
			}
			return
		}
	}
	if page == nil || policy.OwnFailures && page.status != http.StatusOK {
		// The render panicked, or failed and failures are each reader's
		// own: this reader gets its own render.
		next.ServeHTTP(w, r)
		return
	}
	for name, values := range page.header {
		w.Header()[name] = append([]string(nil), values...)
	}
	w.WriteHeader(page.status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(page.body)
	}
}

// startLocked returns key's render in progress, or starts one, as a GET of
// r's page detached from r's cancellation. c.mu is held.
func (c *ResponseCache) startLocked(key string, r *http.Request, policy CachePolicy, next http.Handler) *pageFlight {
	if f := c.flights[key]; f != nil {
		return f
	}
	f := &pageFlight{done: make(chan struct{})}
	c.flights[key] = f
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), pageRenderLimit)
	render := r.Clone(ctx)
	render.Method = http.MethodGet
	go func() {
		defer cancel()
		var page *cachedPage
		defer func() {
			// A render that panics keeps nothing; the readers then render
			// themselves, under the server's own recovery.
			if recover() != nil {
				page = nil
			}
			c.mu.Lock()
			if page != nil && page.status == http.StatusOK {
				c.keepLocked(key, page)
			}
			delete(c.flights, key)
			f.page = page
			c.mu.Unlock()
			close(f.done)
		}()
		var changed <-chan struct{}
		if policy.Changes != nil {
			changed = policy.Changes() // before the render: a change during it ends the entry
		}
		at := c.now()
		rec := &pageRecorder{header: http.Header{}}
		next.ServeHTTP(rec, render)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		page = &cachedPage{status: rec.status, header: rec.header, body: rec.body.Bytes(), at: at, keep: max(policy.TTL, policy.Stale), changed: changed}
	}()
	return f
}

// keepLocked stores page under key within the bound: when full, what has
// expired or ended goes first, then the oldest entry. c.mu is held.
func (c *ResponseCache) keepLocked(key string, page *cachedPage) {
	if _, ok := c.pages[key]; !ok && c.max > 0 && len(c.pages) >= c.max {
		now := c.now()
		oldest := ""
		for k, p := range c.pages {
			if age := now.Sub(p.at); age < 0 || age >= p.keep || closed(p.changed) {
				delete(c.pages, k)
				continue
			}
			if oldest == "" || p.at.Before(c.pages[oldest].at) {
				oldest = k
			}
		}
		if len(c.pages) >= c.max && oldest != "" {
			delete(c.pages, oldest)
		}
	}
	c.pages[key] = page
}

// pageCache is the web pages' ResponseCache: cachedPolicy's requests, keyed
// by origin, path and query.
type pageCache struct {
	next    http.Handler
	changes func() <-chan struct{}
	cache   *ResponseCache
}

// withPageCache serves next through a page cache when service signals its
// changes (the board store); any other service is served directly.
func withPageCache(service board.Service, next http.Handler) http.Handler {
	notifier, ok := service.(interface{ Changes() <-chan struct{} })
	if !ok {
		return next
	}
	return newPageCache(next, notifier.Changes, time.Now)
}

func newPageCache(next http.Handler, changes func() <-chan struct{}, now func() time.Time) *pageCache {
	return &pageCache{next: next, changes: changes, cache: NewResponseCache(0, now)}
}

func (c *pageCache) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := cachedPolicy(r)
	if !ok {
		c.next.ServeHTTP(w, r)
		return
	}
	policy := CachePolicy{TTL: p.ttl, Stale: p.stale}
	if p.onChange {
		policy.Changes = c.changes
	}
	// The origin is the deployment's (WithOrigin), part of the key all the same.
	c.cache.Serve(w, r, requestOrigin(r)+" "+r.URL.Path+"?"+r.URL.RawQuery, policy, c.next)
}

func closed(ch <-chan struct{}) bool {
	if ch == nil {
		return false
	}
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// pageRecorder keeps a render's status, headers and body.
type pageRecorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (p *pageRecorder) Header() http.Header { return p.header }

func (p *pageRecorder) WriteHeader(status int) {
	if p.status == 0 {
		p.status = status
	}
}

func (p *pageRecorder) Write(b []byte) (int, error) {
	if p.status == 0 {
		p.status = http.StatusOK
	}
	return p.body.Write(b)
}
