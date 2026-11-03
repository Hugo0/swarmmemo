package web

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// At most eventPageRenders conversation pages render at once (C726), on
// turns of their own: past the cap one waits up to renderWait, then is
// answered the busy page without reading the store; agent pages and other
// pages are never held by it, and a waiting page renders once a turn frees.
func TestEventPageRendersAreCapped(t *testing.T) {
	s := &gatedAgentService{gate: make(chan struct{}), op: "thread.get"}
	h := Handler(s)
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w
	}
	var wg sync.WaitGroup
	codes := make(chan int, eventPageRenders+1)
	for i := range eventPageRenders {
		wg.Add(1)
		go func() { defer wg.Done(); codes <- get(fmt.Sprintf("/e/%032x", i)).Code }()
	}
	deadline := time.Now().Add(5 * time.Second)
	for s.inflight.Load() < eventPageRenders {
		if time.Now().After(deadline) {
			t.Fatalf("only %d renders started", s.inflight.Load())
		}
		time.Sleep(time.Millisecond)
	}
	for _, other := range []string{"/", "/r/lobby", "/@" + strings.Repeat("ab", 6)} {
		if w := get(other); w.Code == 503 {
			t.Fatalf("%s was held by the conversation page cap", other)
		}
	}
	start := time.Now()
	busy := get(fmt.Sprintf("/e/%032x/proof", 99))
	if busy.Code != 503 || busy.Header().Get("Retry-After") != "5" || !strings.Contains(busy.Body.String(), "This page is busy.") || !strings.Contains(busy.Body.String(), "noindex") {
		t.Fatalf("past the cap: %d %v %q", busy.Code, busy.Header(), busy.Body.String())
	}
	if waited := time.Since(start); waited < renderWait {
		t.Fatalf("answered busy after %v, before waiting %v", waited, renderWait)
	}
	if s.reads.Load() != eventPageRenders {
		t.Fatalf("store reads %d, want %d", s.reads.Load(), eventPageRenders)
	}
	wg.Add(1)
	go func() { defer wg.Done(); codes <- get(fmt.Sprintf("/e/%032x", 7)).Code }()
	time.Sleep(50 * time.Millisecond)
	close(s.gate)
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != 200 {
			t.Fatalf("a capped render answered %d", code)
		}
	}
	if s.most.Load() != eventPageRenders || s.reads.Load() != eventPageRenders+1 {
		t.Fatalf("most in flight %d, reads %d", s.most.Load(), s.reads.Load())
	}
}

func TestEventPagePath(t *testing.T) {
	id := strings.Repeat("a", 32)
	for p, want := range map[string]bool{
		"/e/" + id: true, "/e/" + id + "/slug": true, "/e/" + id + "/proof": true, "/e/" + id + "/history": true, "/e/" + id + "/": true,
		"/e/": false, "/e//x": false, "/e/" + id + "/x/y": false, "/r/lobby": false, "/@atlas": false,
	} {
		if eventPagePath(p) != want {
			t.Errorf("eventPagePath(%q) = %v", p, !want)
		}
	}
}

// A conversation page (/e/ID, its slug, /proof and /history) is kept by its
// path and whole query, until a write or eventPageCacheTTL, in a bounded
// cache of its own; credentials or cookies render on their own and are
// never kept, and deeper paths are not cached (C726).
func TestPageCacheKeepsConversationPages(t *testing.T) {
	f := newCacheFixture()
	id := strings.Repeat("cd", 16)
	for _, target := range []string{"/e/" + id, "/e/" + id + "/a-title", "/e/" + id + "/proof", "/e/" + id + "/history", "/e/" + id + "?cursor=x"} {
		first := f.get(t, target).Body.String()
		if again := f.get(t, target).Body.String(); again != first {
			t.Fatalf("%s not kept: %q then %q", target, first, again)
		}
	}
	if a, b := f.get(t, "/e/"+id).Body.String(), f.get(t, "/e/"+id+"?cursor=x").Body.String(); a == b {
		t.Fatalf("a query shares the bare page's entry: %q", a)
	}
	before := f.renders.Load()
	for _, header := range [][]string{{"Authorization", "Bearer secret"}, {"Cookie", "grant=abc"}} {
		if body := f.get(t, "/e/"+id, header...).Body.String(); !strings.Contains(body, header[1]) {
			t.Fatalf("%s: served %q, not its own render", header[0], body)
		}
	}
	f.get(t, "/e/"+id+"/x/y")
	f.get(t, "/e/"+id+"/x/y")
	if f.renders.Load() != before+4 {
		t.Fatalf("renders %d, want 4 uncached", f.renders.Load()-before)
	}
	if body := f.get(t, "/e/"+id).Body.String(); strings.Contains(body, "secret") || strings.Contains(body, "grant") {
		t.Fatalf("a credentialed render was kept: %q", body)
	}
	f.advance(eventPageCacheTTL - time.Second)
	before = f.renders.Load()
	if f.get(t, "/e/"+id); f.renders.Load() != before {
		t.Fatal("not kept within the TTL")
	}
	f.advance(time.Second)
	if f.get(t, "/e/"+id); f.renders.Load() != before+1 {
		t.Fatal("kept past the TTL")
	}
	f.write()
	if f.get(t, "/e/"+id); f.renders.Load() != before+2 {
		t.Fatal("a write did not end the conversation page")
	}
	home := f.get(t, "/").Body.String()
	for i := range eventPageCacheEntries + 10 {
		f.get(t, fmt.Sprintf("/e/%032x", i))
	}
	if n := f.cache.events.Len(); n != eventPageCacheEntries {
		t.Fatalf("conversation pages kept %d, want %d", n, eventPageCacheEntries)
	}
	if f.cache.agents.Len() != 0 || f.get(t, "/").Body.String() != home {
		t.Fatal("conversation pages pushed another page out")
	}
}
