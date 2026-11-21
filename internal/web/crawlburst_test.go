package web

import (
	"context"
	"fmt"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"swarmmemo/internal/board"
)

// crawlService is a board under a crawl: thread.get (a conversation page)
// waits for gate, the way a crawl of many conversations holds every
// conversation turn; room.get takes roomDelay, like a ranked room feed on a
// busy connection. It signals changes, so Handler serves it through the
// page cache.
type crawlService struct {
	gate      chan struct{}
	roomDelay time.Duration
	threads   atomic.Int64 // thread.get reads in flight
	rooms     atomic.Int64 // room.get reads
	mu        sync.Mutex
	changes   chan struct{}
}

func (s *crawlService) Execute(_ context.Context, c board.Command, _ string) (board.Result, error) {
	switch c.Operation {
	case "thread.get":
		s.threads.Add(1)
		defer s.threads.Add(-1)
		<-s.gate
		m := board.Message{ID: c.MessageID, Room: board.DefaultRoom, Page: board.DefaultPage, Text: "hello", Visibility: "public", Type: "message"}
		return board.Result{OK: true, Messages: []board.Message{m}, Data: map[string]any{"root_id": c.MessageID}}, nil
	case "room.get":
		s.rooms.Add(1)
		time.Sleep(s.roomDelay)
		return board.Result{OK: true, Room: &board.Room{Name: c.Room, Visibility: "public"}}, nil
	}
	return board.Result{OK: true}, nil
}
func (*crawlService) Moderate(context.Context, string, string, bool) error { return nil }
func (*crawlService) Close() error                                         { return nil }

func (s *crawlService) Changes() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.changes
}

// write ends what the page cache keeps, as a committed write does.
func (s *crawlService) write() {
	s.mu.Lock()
	defer s.mu.Unlock()
	close(s.changes)
	s.changes = make(chan struct{})
}

// A crawl burst (C1179) that holds every conversation turn and every place
// to wait for one, while many anonymous readers read the same room at once,
// leaves every reader of that room served: they share one render from the
// room page cache instead of each taking one of roomPageRenders turns, which
// refused all but roomPageRenders*(1+WaitersPerTurn) of them 503 busy. A
// reader with a cookie, or a room page with a query, still renders its own.
func TestCrawlBurstLeavesRoomReadersServed(t *testing.T) {
	s := &crawlService{gate: make(chan struct{}), roomDelay: 300 * time.Millisecond, changes: make(chan struct{})}
	h := Handler(s)
	get := func(path string, header ...string) int {
		r := httptest.NewRequest("GET", path, nil)
		for i := 0; i+1 < len(header); i += 2 {
			r.Header.Set(header[i], header[i+1])
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	// The crawl: distinct conversations, every turn taken and every place
	// to wait for one held.
	var crawl sync.WaitGroup
	for i := range eventPageRenders * (1 + WaitersPerTurn) {
		crawl.Add(1)
		go func() { defer crawl.Done(); get(fmt.Sprintf("/e/%032x", i)) }()
	}
	deadline := time.Now().Add(5 * time.Second)
	for s.threads.Load() < eventPageRenders {
		if time.Now().After(deadline) {
			t.Fatalf("only %d conversation renders started", s.threads.Load())
		}
		time.Sleep(time.Millisecond)
	}
	if code := get(fmt.Sprintf("/e/%032x", 999)); code != 503 {
		t.Fatalf("the crawl did not fill the conversation turns: %d", code)
	}
	// The readers of one room, far more at once than its turns and places
	// to wait.
	readers := 4 * roomPageRenders * (1 + WaitersPerTurn)
	codes := make(chan int, readers)
	var wg sync.WaitGroup
	for range readers {
		wg.Add(1)
		go func() { defer wg.Done(); codes <- get("/r/lobby") }()
	}
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != 200 {
			t.Fatalf("a reader of /r/lobby during the crawl got %d", code)
		}
	}
	if n := s.rooms.Load(); n != 1 {
		t.Fatalf("%d readers of /r/lobby cost %d room renders, want 1", readers, n)
	}
	// Within the TTL a repeat is served without a render; a write ends it.
	if get("/r/lobby"); s.rooms.Load() != 1 {
		t.Fatal("a repeat within the TTL rendered again")
	}
	s.write()
	if code := get("/r/lobby"); code != 200 || s.rooms.Load() != 2 {
		t.Fatalf("after a write: %d, %d renders", code, s.rooms.Load())
	}
	// A reader of its own, or a query, renders on its own turn.
	for _, tc := range [][]string{{"/r/lobby", "Cookie", "grant=abc"}, {"/r/lobby?sort=new"}, {"/r/lobby/notes"}} {
		before := s.rooms.Load()
		if code := get(tc[0], tc[1:]...); code != 200 || s.rooms.Load() != before+1 {
			t.Fatalf("%v: %d, %d renders", tc, code, s.rooms.Load()-before)
		}
	}
	close(s.gate)
	crawl.Wait()
}
