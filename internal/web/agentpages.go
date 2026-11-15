package web

import (
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Heavy page renders (C701, C726). On 2026-11-01 at 16:49Z a crawler read
// nine agent pages (/@ADDRESS) at once. Each is one of the heaviest public
// renders (agent.get with its reputation, reviews and storefront, the
// allowance, the room and its posts), all on the store's one SQLite
// connection, so every request on every route queued behind them for 5-7
// seconds. On 2026-11-03 a distributed crawler did the same with
// conversation pages (/e/ID, its /proof and /history): thread.get, the
// versions, the room, and on /proof the log's leaf and checkpoint.
//
// Each kind has its own turns: at most agentPageRenders agent pages
// (/@ADDRESS and /agent/ID) and at most eventPageRenders conversation pages
// render at once; a page past that waits up to renderWait for a turn, then
// is answered 503 with Retry-After, which crawlers honour. One kind's
// crawl never takes the other's turns.
//
// On 2026-11-12 at 12:43Z a crawl from 627 networks (none above 0.3
// requests a second, so no per-network bucket saw it) read room pages
// (/r/ROOM: room.get, the ranked feed with its nested replies) and /find
// (the find engine) at once, which had no turns. Their reads queued 13 s on
// the one connection, held the request pool full, and 406 requests on every
// route were answered 503 busy within a minute (C929). Room pages and /find
// now have turns of their own too: roomPageRenders and findPageRenders. The API (agent.get, /api/agent, a
// JSON /@ADDRESS or /e/ID) never comes here and is never refused by it; the
// anonymous page cache (pagecache.go) answers a repeat before it takes a
// turn.
const (
	agentPageRenders = 3
	eventPageRenders = 3
	roomPageRenders  = 4 // the most-read pages after the home page, which the page cache serves
	findPageRenders  = 2 // the heaviest render: the find engine and its grades
	renderWait       = 2 * time.Second
	renderRetryAfter = 5

	// WaitersPerTurn bounds the readers that wait for a kind's turns: past
	// turns*WaitersPerTurn waiting, the next is answered busy at once
	// (C991). A waiter holds a slot of the server's request pool for up to
	// renderWait; on 2026-11-15 at 02:35:55Z a fetch from 1380 networks
	// (1357 page loads in five seconds, none above a few per network, so no
	// network share saw it) filled the pool with waiters, and every
	// anonymous read on every route, a static file or an agent's
	// /api/messages poll included, was answered 503 busy for some 15 s. With
	// the bound the capped kinds hold at most (turns of every kind) *
	// (1+WaitersPerTurn) slots and the rest of the pool stays free for the
	// rest of the site. A turn frees every few hundred milliseconds, so a
	// reader past that many waiters would not have got one within
	// renderWait anyway.
	WaitersPerTurn = 3
)

// agentPagePath is an agent page: /@ADDRESS (not an offering's
// /@ADDRESS/NAME) or /agent/ID.
func agentPagePath(p string) bool {
	if rest, ok := strings.CutPrefix(p, "/@"); ok {
		return rest != "" && !strings.Contains(rest, "/")
	}
	rest, ok := strings.CutPrefix(p, "/agent/")
	return ok && rest != "" && !strings.Contains(rest, "/")
}

// eventPagePath is a conversation page: /e/ID, /e/ID/SLUG, /e/ID/proof or
// /e/ID/history (loadEventPage answers anything deeper 404 at once).
func eventPagePath(p string) bool {
	rest, ok := strings.CutPrefix(p, "/e/")
	if !ok {
		return false
	}
	id, suffix, _ := strings.Cut(rest, "/")
	return id != "" && !strings.Contains(suffix, "/")
}

// roomPagePath is a room page: /r/ROOM or /r/ROOM/PAGE (the handler answers
// anything deeper 404 at once).
func roomPagePath(p string) bool {
	rest, ok := strings.CutPrefix(p, "/r/")
	if !ok {
		return false
	}
	room, page, _ := strings.Cut(rest, "/")
	return room != "" && !strings.Contains(page, "/")
}

// Turns bounds a kind of work (a page render here, an anonymous agent read
// in httpapi/hotreads.go): at most its size run at once, and at most
// size*WaitersPerTurn wait for one.
type Turns struct {
	slots   chan struct{}
	waiting atomic.Int64
	most    int64 // waiters allowed
}

// NewTurns is n turns with n*WaitersPerTurn places to wait.
func NewTurns(n int) *Turns {
	return &Turns{slots: make(chan struct{}, n), most: int64(n * WaitersPerTurn)}
}

// Take waits up to wait for a turn and reports whether it got one; every
// true is paired with Release. A reader that leaves stops waiting, and one
// that finds every place to wait taken is refused at once.
func (t *Turns) Take(r *http.Request, wait time.Duration) bool {
	select {
	case t.slots <- struct{}{}:
		return true
	default:
	}
	if t.waiting.Add(1) > t.most {
		t.waiting.Add(-1)
		return false
	}
	defer t.waiting.Add(-1)
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case t.slots <- struct{}{}:
		return true
	case <-timer.C:
	case <-r.Context().Done():
	}
	return false
}

// Release ends a turn Take gave.
func (t *Turns) Release() { <-t.slots }

// pageTurns are the turns of each capped kind of page.
type pageTurns struct {
	agents, events, rooms, find *Turns
}

func newPageTurns() pageTurns {
	return pageTurns{agents: NewTurns(agentPageRenders), events: NewTurns(eventPageRenders), rooms: NewTurns(roomPageRenders), find: NewTurns(findPageRenders)}
}

// of is the turns a page at path takes, or nil when it is not capped.
func (t pageTurns) of(path string) *Turns {
	switch {
	case agentPagePath(path):
		return t.agents
	case eventPagePath(path):
		return t.events
	case roomPagePath(path):
		return t.rooms
	case path == "/find":
		return t.find
	}
	return nil
}

// renderBusy answers a page that found no turn: 503, Retry-After, noindex.
func renderBusy(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Retry-After", strconv.Itoa(renderRetryAfter))
	w.WriteHeader(http.StatusServiceUnavailable)
	p := page{View: "busy", Title: "Busy", NoIndex: true, Path: r.URL.Path, Description: "Many pages like this one are being read at once."}
	finishMetadata(&p, http.StatusServiceUnavailable)
	_ = templates.ExecuteTemplate(w, "page.html", p)
}
