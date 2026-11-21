package httpapi

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

// crawledBoard is a board under the crawl of 2026-11-20/21 (C1179): every
// conversation page read (thread.get) waits for gate, the way a crawl of
// many conversations from thousands of networks holds every conversation
// turn; a room page read (room.get) takes 200 ms. It signals changes, so
// web.Handler serves it through the page cache.
type crawledBoard struct {
	fakeService
	gate    chan struct{}
	threads atomic.Int64
	rooms   atomic.Int64
	changes chan struct{}
}

func (b *crawledBoard) Execute(ctx context.Context, c board.Command, peer string) (board.Result, error) {
	switch c.Operation {
	case "thread.get":
		b.threads.Add(1)
		<-b.gate
		m := board.Message{ID: c.MessageID, Room: board.DefaultRoom, Page: board.DefaultPage, Text: "hello", Visibility: "public", Type: "message"}
		return board.Result{OK: true, Messages: []board.Message{m}, Data: map[string]any{"root_id": c.MessageID}}, nil
	case "room.get":
		b.rooms.Add(1)
		time.Sleep(200 * time.Millisecond)
		return board.Result{OK: true, Room: &board.Room{Name: c.Room, Visibility: "public"}}, nil
	}
	return b.fakeService.Execute(ctx, c, peer)
}

func (b *crawledBoard) Changes() <-chan struct{} { return b.changes }

func pageRequest(s *Server, method, path, client, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:4000"
	r.Header.Set("X-Forwarded-For", client)
	r.Header.Set("X-Forwarded-Proto", "https")
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	} else {
		r.Header.Set("Accept", "text/html,application/xhtml+xml")
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

// A distributed crawl of conversation pages (one or two reads from each of
// hundreds of networks, browser user agents, so no network share sees it)
// cannot starve a signed command, an API read, or the readers of a room
// page: the crawl is shed on the conversation turns, a signed command never
// takes a page turn, and a room's anonymous readers share one render from
// the page cache instead of queueing for roomPageRenders turns (C1179).
func TestCrawlBurstLeavesSignedCommandsAndRoomReadersServed(t *testing.T) {
	b := &crawledBoard{gate: make(chan struct{}), changes: make(chan struct{})}
	s := New(b, web.Handler(b), Config{TrustLoopbackProxy: true})
	network := func(i int) string { return fmt.Sprintf("%d.%d.%d.9", 20+i%180, i/180%250, i%250) }
	const crawlers = 300
	var crawl sync.WaitGroup
	var crawlCodes sync.Map
	var answered atomic.Int64
	for i := range crawlers {
		crawl.Add(1)
		go func() {
			defer crawl.Done()
			w := pageRequest(s, "GET", fmt.Sprintf("/e/%032x", i), network(i), "")
			n, _ := crawlCodes.LoadOrStore(w.Code, new(atomic.Int64))
			n.(*atomic.Int64).Add(1)
			answered.Add(1)
		}()
	}
	// Settled: the crawl holds every conversation turn, and the readers
	// past the places to wait have been refused at once.
	deadline := time.Now().Add(10 * time.Second)
	for b.threads.Load() < 3 || answered.Load() < crawlers-12 {
		if time.Now().After(deadline) {
			t.Fatalf("the crawl did not settle: %d renders, %d answered", b.threads.Load(), answered.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n, ok := crawlCodes.Load(503); !ok || n.(*atomic.Int64).Load() == 0 {
		t.Fatal("the crawl was never shed")
	}
	// Inside the burst: 64 readers of /r/lobby at once, each from its own
	// network, all served from one render.
	const readers = 64
	var wg sync.WaitGroup
	codes := make(chan int, readers)
	for i := range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- pageRequest(s, "GET", "/r/lobby", fmt.Sprintf("%d.%d.1.7", 100+i, 30+i), "").Code
		}()
	}
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != 200 {
			t.Fatalf("a reader of /r/lobby inside the crawl got %d", code)
		}
	}
	if n := b.rooms.Load(); n != 1 {
		t.Fatalf("%d readers of /r/lobby cost %d room renders, want 1", readers, n)
	}
	if w := pageRequest(s, "POST", "/v1/command", network(5), `{"operation":"rooms.list","public_key":"key","signature":"sig"}`); w.Code != 200 {
		t.Fatalf("a signed command inside the crawl: %d %s", w.Code, w.Body.String())
	}
	if w := shareRequest(s, "GET", "/api/messages?room=lobby", "203.0.113.7", ""); w.Code != 200 {
		t.Fatalf("an API read inside the crawl: %d %s", w.Code, w.Body.String())
	}
	close(b.gate)
	crawl.Wait()
}
