package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"swarmmemo/internal/board"
)

// cacheFixture is a page cache in front of a handler that numbers its
// renders and echoes what the request carried of its own.
type cacheFixture struct {
	cache   *pageCache
	renders atomic.Int64
	status  atomic.Int64
	mu      sync.Mutex
	now     time.Time
	changed chan struct{}
	gate    chan struct{} // when set, a render waits for it
}

func newCacheFixture() *cacheFixture {
	f := &cacheFixture{now: time.Unix(1_800_000_000, 0), changed: make(chan struct{})}
	f.status.Store(200)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.gate != nil {
			<-f.gate
		}
		n := f.renders.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(int(f.status.Load()))
		fmt.Fprintf(w, "render %d of %s for %q %q", n, r.URL.RequestURI(), r.Header.Get("Authorization"), r.Header.Get("Cookie"))
	})
	f.cache = newPageCache(next, func() <-chan struct{} {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.changed
	}, func() time.Time {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.now
	})
	return f
}

func (f *cacheFixture) get(t *testing.T, target string, header ...string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("GET", target, nil)
	for i := 0; i+1 < len(header); i += 2 {
		r.Header.Set(header[i], header[i+1])
	}
	w := httptest.NewRecorder()
	f.cache.ServeHTTP(w, r)
	return w
}

func (f *cacheFixture) write() {
	f.mu.Lock()
	defer f.mu.Unlock()
	close(f.changed)
	f.changed = make(chan struct{})
}

func (f *cacheFixture) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

// The cache never gives a personalised render to an anonymous reader, nor
// the anonymous one to a reader that sent credentials or cookies: those
// render on their own and are never kept. Other pages and queries are not
// cached at all.
func TestPageCacheKeepsOnlyAnonymousRenders(t *testing.T) {
	f := newCacheFixture()
	first := f.get(t, "/").Body.String()
	if first != `render 1 of / for "" ""` || f.get(t, "/").Body.String() != first {
		t.Fatalf("anonymous home not served from the cache: %q", first)
	}
	if h := f.get(t, "/").Header(); h.Get("Content-Type") != "text/html; charset=utf-8" || h.Get("Cache-Control") != "no-store" {
		t.Fatalf("cached headers: %v", h)
	}
	for _, header := range [][]string{{"Authorization", "Bearer secret"}, {"Cookie", "grant=abc"}} {
		before := f.renders.Load()
		personal := f.get(t, "/", header...).Body.String()
		if f.renders.Load() != before+1 || !strings.Contains(personal, header[1]) {
			t.Fatalf("%s: served %q, not its own render", header[0], personal)
		}
		if again := f.get(t, "/").Body.String(); again != first {
			t.Fatalf("after a %s request, anonymous got %q", header[0], again)
		}
	}
	for _, target := range []string{"/?sort=new", "/r/lobby", "/stats?bucket=day", "/for-agents"} {
		before := f.renders.Load()
		f.get(t, target)
		f.get(t, target)
		if f.renders.Load() != before+2 {
			t.Fatalf("%s was cached", target)
		}
	}
	// HEAD is answered from the kept GET, without a body.
	r := httptest.NewRequest("HEAD", "/", nil)
	w := httptest.NewRecorder()
	before := f.renders.Load()
	f.cache.ServeHTTP(w, r)
	if w.Code != 200 || w.Body.Len() != 0 || f.renders.Load() != before {
		t.Fatalf("HEAD: %d, %d bytes, %d renders", w.Code, w.Body.Len(), f.renders.Load()-before)
	}
}

// The home page is kept until the board changes or homeCacheTTL passes.
func TestPageCacheHomeRefreshesOnChange(t *testing.T) {
	f := newCacheFixture()
	f.get(t, "/")
	f.write()
	if body := f.get(t, "/").Body.String(); !strings.HasPrefix(body, "render 2 ") {
		t.Fatalf("after a write: %q", body)
	}
	f.advance(homeCacheTTL - time.Second)
	if body := f.get(t, "/").Body.String(); !strings.HasPrefix(body, "render 2 ") {
		t.Fatalf("within the TTL: %q", body)
	}
	f.advance(time.Second)
	if body := f.get(t, "/").Body.String(); !strings.HasPrefix(body, "render 3 ") {
		t.Fatalf("after the TTL: %q", body)
	}
}

// /stats is kept for statsCacheTTL whatever is written, then served stale
// while one render refreshes it, and rendered afresh once too old.
func TestPageCacheStatsServesStaleWhileRefreshing(t *testing.T) {
	f := newCacheFixture()
	f.get(t, "/stats")
	f.write()
	if body := f.get(t, "/stats").Body.String(); !strings.HasPrefix(body, "render 1 ") {
		t.Fatalf("a write ended /stats: %q", body)
	}
	if body := f.get(t, "/stats?bucket=hour").Body.String(); !strings.HasPrefix(body, "render 2 of /stats?bucket=hour") {
		t.Fatalf("hourly /stats shares the daily entry: %q", body)
	}
	f.advance(statsCacheTTL)
	if body := f.get(t, "/stats").Body.String(); !strings.HasPrefix(body, "render 1 ") {
		t.Fatalf("stale /stats not served at once: %q", body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.HasPrefix(f.get(t, "/stats").Body.String(), "render 3 ") {
		if time.Now().After(deadline) {
			t.Fatal("stale /stats never refreshed")
		}
		time.Sleep(time.Millisecond)
	}
	f.advance(statsCacheStale)
	if body := f.get(t, "/stats").Body.String(); !strings.HasPrefix(body, "render 4 ") {
		t.Fatalf("too old a /stats served: %q", body)
	}
}

// Readers that arrive during a render share it; an error status is passed on
// and not kept.
func TestPageCacheSingleflightAndErrors(t *testing.T) {
	f := newCacheFixture()
	f.gate = make(chan struct{})
	var wg sync.WaitGroup
	bodies := make([]string, 20)
	for i := range bodies {
		wg.Add(1)
		go func() {
			defer wg.Done()
			bodies[i] = f.get(t, "/").Body.String()
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(f.gate)
	wg.Wait()
	f.gate = nil
	for _, body := range bodies {
		if body != bodies[0] || !strings.HasPrefix(body, "render 1 ") {
			t.Fatalf("concurrent readers: %q", bodies)
		}
	}
	if n := f.renders.Load(); n != 1 {
		t.Fatalf("%d renders for one burst", n)
	}
	f = newCacheFixture()
	f.status.Store(503)
	if w := f.get(t, "/"); w.Code != 503 {
		t.Fatalf("status %d", w.Code)
	}
	f.status.Store(200)
	if body := f.get(t, "/").Body.String(); !strings.HasPrefix(body, "render 2 ") || f.get(t, "/").Code != 200 {
		t.Fatalf("an error render was kept, or rendered twice: %q", body)
	}
}

// On the board itself, a post shows on the cached home page at once.
func TestPageCacheHomeShowsANewPostAtOnce(t *testing.T) {
	s, err := board.Open(filepath.Join(t.TempDir(), "cache.db"), board.Config{ServiceID: "swarmmemo.com"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	h := Handler(s)
	writer := newWebKey(54)
	writer.run(t, s, board.Command{Operation: "post", Room: "lobby", Text: "The first post"})
	home := func() string {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
		return w.Body.String()
	}
	if !strings.Contains(home(), "The first post") {
		t.Fatal("home lacks the first post")
	}
	second := writer.run(t, s, board.Command{Operation: "post", Room: "lobby", Text: "A second post, just now"})
	if !strings.Contains(home(), "A second post, just now") {
		t.Fatal("the cached home page hides a new post")
	}
	// The operator's hide is not a command, and ends the page as well.
	if err := s.Moderate(t.Context(), second.Receipt.ID, "fixture", true); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(home(), "A second post, just now") {
		t.Fatal("the cached home page still shows a hidden post")
	}
}

// A bounded ResponseCache keeps at most its bound: a full cache drops what
// has expired first, then its oldest entry, and never a fresh newer one.
func TestResponseCacheBound(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var renders atomic.Int64
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		renders.Add(1)
		fmt.Fprint(w, r.URL.Path)
	})
	c := NewResponseCache(3, func() time.Time { return now })
	serve := func(key string, ttl time.Duration) string {
		w := httptest.NewRecorder()
		c.Serve(w, httptest.NewRequest("GET", "/"+key, nil), key, CachePolicy{TTL: ttl}, next)
		return w.Body.String()
	}
	for i, key := range []string{"a", "b", "c"} {
		serve(key, time.Minute)
		now = now.Add(time.Duration(i+1) * time.Second)
	}
	serve("d", time.Minute) // full: a, the oldest, goes
	if c.Len() != 3 || renders.Load() != 4 {
		t.Fatalf("%d kept, %d renders", c.Len(), renders.Load())
	}
	if serve("b", time.Minute) != "/b" || renders.Load() != 4 {
		t.Fatal("b was dropped before a")
	}
	if serve("a", time.Minute) != "/a" || renders.Load() != 5 || c.Len() != 3 {
		t.Fatalf("a was kept past the bound: %d renders, %d kept", renders.Load(), c.Len())
	}
	// An expired entry goes before any fresh one.
	serve("short", time.Second) // replaces the oldest (c)
	now = now.Add(2 * time.Second)
	serve("e", time.Minute)
	if c.Len() != 3 {
		t.Fatalf("%d kept", c.Len())
	}
	before := renders.Load()
	serve("d", time.Minute)
	serve("a", time.Minute)
	if renders.Load() != before {
		t.Fatal("a fresh entry was dropped while an expired one was kept")
	}
}
