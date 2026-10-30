package httpapi

import (
	"context"
	"crypto/ed25519"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"swarmmemo/internal/board"
)

// countingStore is the board store, counting the commands that reach it.
type countingStore struct {
	*board.Store
	mu  sync.Mutex
	ops map[string]int
}

func (c *countingStore) Execute(ctx context.Context, cmd board.Command, source string) (board.Result, error) {
	c.mu.Lock()
	c.ops[cmd.Operation]++
	c.mu.Unlock()
	return c.Store.Execute(ctx, cmd, source)
}

func (c *countingStore) count(op string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ops[op]
}

type hotFixture struct {
	store *countingStore
	s     *Server
	mu    sync.Mutex
	now   time.Time
}

func newHotFixture(t *testing.T) *hotFixture {
	t.Helper()
	b, err := board.Open(filepath.Join(t.TempDir(), "hot.db"), board.Config{ServiceID: "swarmmemo.com"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	f := &hotFixture{store: &countingStore{Store: b, ops: map[string]int{}}, now: time.Now()}
	f.s = New(f.store, nil, Config{ServiceID: "swarmmemo.com", PublicURL: "https://swarmmemo.com"})
	f.s.hotNow = func() time.Time {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.now
	}
	return f
}

func (f *hotFixture) advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	f.mu.Unlock()
}

func (f *hotFixture) run(t *testing.T, c board.Command) board.Result {
	t.Helper()
	res, err := f.store.Store.Execute(context.Background(), c, "203.0.113.50")
	if err != nil {
		t.Fatalf("%s: %v", c.Operation, err)
	}
	return res
}

// getWith is a GET from addr with the given headers (name, value pairs).
func (f *hotFixture) getWith(path, addr string, header ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	r.RemoteAddr = addr
	for i := 0; i+1 < len(header); i += 2 {
		r.Header.Set(header[i], header[i+1])
	}
	w := httptest.NewRecorder()
	f.s.ServeHTTP(w, r)
	return w
}

// A second anonymous read of /e/ID or /api/agent/AGENT, from another
// network, is the first one's answer and never reaches the store; the text
// and JSON forms are kept apart.
func TestHotReadsServeAnonymousRepeatsFromCache(t *testing.T) {
	f := newHotFixture(t)
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	id := f.run(t, signService(key, board.Command{Operation: "post", Room: "lobby", Text: "a hot post", RequestID: "hot-1"})).Receipt.ID
	agent := fingerprintOf(key)
	for _, c := range []struct{ path, op string }{{"/e/" + id, "message.get"}, {"/api/agent/" + agent, "agent.get"}} {
		first := f.getWith(c.path, "198.51.100.8:1000")
		if first.Code != 200 || f.store.count(c.op) != 1 {
			t.Fatalf("%s: %d after %d reads: %s", c.path, first.Code, f.store.count(c.op), first.Body)
		}
		second := f.getWith(c.path, "192.0.2.77:2000")
		if second.Code != 200 || second.Body.String() != first.Body.String() || second.Header().Get("Content-Type") != first.Header().Get("Content-Type") || f.store.count(c.op) != 1 {
			t.Fatalf("%s: the repeat read the store (%d) or differs", c.path, f.store.count(c.op))
		}
		// The JSON form is its own answer, kept as well.
		j := f.getWith(c.path+"?format=json", "192.0.2.77:2000")
		f.getWith(c.path+"?format=json", "192.0.2.78:2000")
		if j.Code != 200 || !strings.HasPrefix(j.Header().Get("Content-Type"), "application/json") || f.store.count(c.op) != 2 {
			t.Fatalf("%s json: %d %q after %d reads", c.path, j.Code, j.Header().Get("Content-Type"), f.store.count(c.op))
		}
		// Past hotReadTTL the store is read again.
		f.advance(hotReadTTL)
		f.getWith(c.path, "198.51.100.8:1000")
		if f.store.count(c.op) != 3 {
			t.Fatalf("%s: %d reads after the TTL", c.path, f.store.count(c.op))
		}
	}
	// /capabilities is kept too.
	before := f.store.count("services.list")
	f.getWith("/capabilities", "198.51.100.8:1000")
	caps := f.store.count("services.list")
	f.getWith("/capabilities", "192.0.2.77:2000")
	if f.store.count("services.list") != caps {
		t.Fatalf("/capabilities read the store again: %d, %d, %d", before, caps, f.store.count("services.list"))
	}
}

// A read that names its caller (Authorization, a cookie, a signed query, any
// other query) always reads the store, and is never what an anonymous
// reader is served.
func TestHotReadsBypassForKeyedReads(t *testing.T) {
	f := newHotFixture(t)
	id := f.run(t, board.Command{Operation: "post", Room: "lobby", Text: "anon post", RequestID: "hot-2"}).Receipt.ID
	path := "/e/" + id
	anon := f.getWith(path, "198.51.100.8:1000") // kept
	for _, c := range []struct {
		target string
		header []string
	}{
		{path, []string{"Authorization", "Bearer not-a-token"}},
		{path, []string{"Cookie", "lounge=1"}},
		{path + "?public_key=AAAA&signature=BBBB&timestamp=1&nonce=n", nil},
		{path + "?room=lobby", nil},
	} {
		n := f.store.count("message.get")
		w := f.getWith(c.target, "198.51.100.8:1000", c.header...)
		// Its own read, or its own refusal (a signed read over plain HTTP).
		if f.store.count("message.get") != n+1 && w.Body.String() == anon.Body.String() {
			t.Fatalf("%s %v was served the anonymous answer: %d %s", c.target, c.header, w.Code, w.Body)
		}
	}
	if f.s.hotCache().Len() != 1 {
		t.Fatalf("keyed reads were kept: %d entries", f.s.hotCache().Len())
	}
	// A failed read (an unknown agent) is never kept.
	for range 2 {
		n := f.store.count("agent.get")
		if w := f.getWith("/api/agent/nobody", "198.51.100.8:1000"); w.Code != 404 || f.store.count("agent.get") == n {
			t.Fatalf("unknown agent: %d, read the store: %t", w.Code, f.store.count("agent.get") > n)
		}
	}
}

// A hide, a restore and a withdraw end the kept answers at once: the next
// anonymous read is the store's, within the TTL. A new post does not.
func TestHotReadsEndOnHideAndWithdraw(t *testing.T) {
	f := newHotFixture(t)
	key := ed25519.NewKeyFromSeed([]byte(strings.Repeat("w", 32)))
	id := f.run(t, signService(key, board.Command{Operation: "post", Room: "lobby", Text: "soon hidden", RequestID: "hot-3"})).Receipt.ID
	path := "/e/" + id
	if w := f.getWith(path, "198.51.100.8:1000"); !strings.Contains(w.Body.String(), "soon hidden") {
		t.Fatalf("first read: %s", w.Body)
	}
	// An unrelated new post leaves it kept.
	f.run(t, board.Command{Operation: "post", Room: "lobby", Text: "another", RequestID: "hot-4"})
	f.getWith(path, "192.0.2.1:1000")
	if f.store.count("message.get") != 1 {
		t.Fatalf("a new post ended the kept answer: %d reads", f.store.count("message.get"))
	}
	if err := f.store.Moderate(context.Background(), id, "test hide", true); err != nil {
		t.Fatal(err)
	}
	if w := f.getWith(path, "192.0.2.1:1000"); f.store.count("message.get") != 2 || strings.Contains(w.Body.String(), "soon hidden") {
		t.Fatalf("hidden post served from the cache: %d reads, %s", f.store.count("message.get"), w.Body)
	}
	if err := f.store.Moderate(context.Background(), id, "test restore", false); err != nil {
		t.Fatal(err)
	}
	if w := f.getWith(path, "192.0.2.1:1000"); f.store.count("message.get") != 3 || !strings.Contains(w.Body.String(), "soon hidden") {
		t.Fatalf("restored post: %d reads, %s", f.store.count("message.get"), w.Body)
	}
	f.getWith(path, "192.0.2.1:1000") // kept again
	f.run(t, signService(key, board.Command{Operation: "message.withdraw", MessageID: id, RequestID: "hot-5"}))
	if w := f.getWith(path, "192.0.2.1:1000"); f.store.count("message.get") != 4 || strings.Contains(w.Body.String(), "soon hidden") {
		t.Fatalf("withdrawn post served from the cache: %d reads, %s", f.store.count("message.get"), w.Body)
	}
	// An agent write (here agent.register) ends a kept agent answer.
	other := ed25519.NewKeyFromSeed([]byte(strings.Repeat("o", 32)))
	f.run(t, signService(other, board.Command{Operation: "post", Room: "lobby", Text: "still here", RequestID: "hot-6a"}))
	agent := "/api/agent/" + fingerprintOf(other)
	base := f.store.count("agent.get")
	f.getWith(agent, "192.0.2.1:1000")
	f.getWith(agent, "192.0.2.1:1000")
	if f.store.count("agent.get") != base+1 {
		t.Fatalf("agent not kept: %d reads", f.store.count("agent.get")-base)
	}
	f.run(t, signService(other, board.Command{Operation: "agent.register", RequestID: "hot-6"}))
	f.getWith(agent, "192.0.2.1:1000")
	if f.store.count("agent.get") != base+2 {
		t.Fatalf("an agent write left the kept answer: %d reads", f.store.count("agent.get")-base)
	}
}

// A burst of readers of one answer costs one store read (singleflight); the
// size bound is web.ResponseCache's (TestResponseCacheBound).
func TestHotReadsBurst(t *testing.T) {
	f := newHotFixture(t)
	id := f.run(t, board.Command{Operation: "post", Room: "lobby", Text: "burst", RequestID: "hot-7"}).Receipt.ID
	var wg sync.WaitGroup
	var ok atomic.Int64
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if w := f.getWith("/e/"+id, "198.51.100."+string(rune('0'+i%10))+":1000"); w.Code == 200 {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 32 || f.store.count("message.get") > 2 {
		t.Fatalf("burst: %d answered, %d store reads", ok.Load(), f.store.count("message.get"))
	}
}
