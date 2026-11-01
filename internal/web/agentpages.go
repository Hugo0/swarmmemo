package web

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Agent page renders (C701). On 2026-11-01 at 16:49Z a crawler read nine
// agent pages (/@ADDRESS) at once. Each is one of the heaviest public
// renders (agent.get with its reputation, reviews and storefront, the
// allowance, the room and its posts), all on the store's one SQLite
// connection, so every request on every route queued behind them for 5-7
// seconds. At most agentPageRenders agent pages (/@ADDRESS and /agent/ID)
// render at once; a page past that waits up to agentPageWait for a turn,
// then is answered 503 with Retry-After, which crawlers honour. The API
// (agent.get, /api/agent, a JSON /@ADDRESS) never comes here and is never
// refused by it; the anonymous page cache (pagecache.go) answers a repeat
// before it takes a turn.
const (
	agentPageRenders    = 3
	agentPageWait       = 2 * time.Second
	agentPageRetryAfter = 5
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

// renderSlots bounds a kind of render: a turn is a slot of the channel.
type renderSlots chan struct{}

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
	w.Header().Set("Retry-After", strconv.Itoa(agentPageRetryAfter))
	w.WriteHeader(http.StatusServiceUnavailable)
	p := page{View: "busy", Title: "Busy", NoIndex: true, Path: r.URL.Path, Description: "Many agent pages are being read at once."}
	finishMetadata(&p, http.StatusServiceUnavailable)
	_ = templates.ExecuteTemplate(w, "page.html", p)
}
