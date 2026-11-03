package web

import (
	"net/http"
	"strconv"
	"strings"
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
// crawl never takes the other's turns. The API (agent.get, /api/agent, a
// JSON /@ADDRESS or /e/ID) never comes here and is never refused by it; the
// anonymous page cache (pagecache.go) answers a repeat before it takes a
// turn.
const (
	agentPageRenders = 3
	eventPageRenders = 3
	renderWait       = 2 * time.Second
	renderRetryAfter = 5
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

// renderSlots bounds a kind of render: a turn is a slot of the channel.
type renderSlots chan struct{}

// pageTurns are the turns of each capped kind of page.
type pageTurns struct {
	agents, events renderSlots
}

func newPageTurns() pageTurns {
	return pageTurns{agents: make(renderSlots, agentPageRenders), events: make(renderSlots, eventPageRenders)}
}

// of is the turns a page at path takes, or nil when it is not capped.
func (t pageTurns) of(path string) renderSlots {
	switch {
	case agentPagePath(path):
		return t.agents
	case eventPagePath(path):
		return t.events
	}
	return nil
}

// take waits up to wait for a turn and reports whether it got one; every
// true is paired with release. A reader that leaves stops waiting.
func (s renderSlots) take(r *http.Request, wait time.Duration) bool {
	select {
	case s <- struct{}{}:
		return true
	default:
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case s <- struct{}{}:
		return true
	case <-t.C:
	case <-r.Context().Done():
	}
	return false
}

func (s renderSlots) release() { <-s }

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
