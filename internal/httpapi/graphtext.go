package httpapi

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"swarmmemo/internal/board"
)

type graphTextStore interface {
	GraphMessages(ctx context.Context, q board.GraphMessageQuery) ([]board.GraphMessage, bool, error)
}

// Per-peer budgets of the graph's text layer, on top of the server's request
// bucket: reading text is cheap but unbounded scraping through it is not the
// point, and every summary costs money.
const (
	graphTextPerMinute = 60
)

// windowLimiter is a fixed-window counter per key, pruned as it grows.
type windowLimiter struct {
	mu     sync.Mutex
	window time.Duration
	max    int
	counts map[string]windowCount
}

type windowCount struct {
	start time.Time
	n     int
}

func newWindowLimiter(window time.Duration, max int) *windowLimiter {
	return &windowLimiter{window: window, max: max, counts: map[string]windowCount{}}
}

// Allow counts one use by key and reports whether it is within the budget,
// and if not, the seconds until the window resets.
func (l *windowLimiter) Allow(key string, now time.Time) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.counts) > 10000 {
		for k, c := range l.counts {
			if now.Sub(c.start) >= l.window {
				delete(l.counts, k)
			}
		}
		if len(l.counts) > 10000 {
			clear(l.counts)
		}
	}
	c := l.counts[key]
	if now.Sub(c.start) >= l.window || now.Before(c.start) {
		c = windowCount{start: now}
	}
	if c.n >= l.max {
		return false, int(c.start.Add(l.window).Sub(now)/time.Second) + 1
	}
	c.n++
	l.counts[key] = c
	return true, 0
}

// graphSelection parses ids, room and among: the selection both the text
// layer and summaries read.
func graphSelection(get func(string) string) (board.GraphMessageQuery, *board.Error) {
	q := board.GraphMessageQuery{Room: get("room")}
	ids := get("ids")
	if ids == "" {
		return q, bad("ids lists 1 to 200 comma-separated fingerprints or anon:ROOM pools, as /api/graph names them.")
	}
	q.Keys = strings.Split(ids, ",")
	switch get("mode") {
	case "", "author":
	case "among":
		q.Among = true
	default:
		return q, bad("mode is author (the default: everything they posted) or among (only messages exchanged between them).")
	}
	return q, nil
}

// graphMessages serves /api/graph/messages: the public text behind a graph
// selection, for the /graph side panel and its exports.
func (s *Server) graphMessages(w http.ResponseWriter, r *http.Request) {
	if !readMethod(r) {
		methodError(w)
		return
	}
	query := r.URL.Query()
	for key, values := range query {
		if len(values) != 1 || (key != "ids" && key != "room" && key != "mode" && key != "format") {
			writeError(w, bad("Graph messages take ids, and optionally room and mode, each once."))
			return
		}
	}
	q, perr := graphSelection(query.Get)
	if perr != nil {
		writeError(w, perr)
		return
	}
	store, ok := s.service.(graphTextStore)
	if !ok {
		writeError(w, &board.Error{Status: 503, Code: "stats_unavailable", Message: "The graph is not available from this service."})
		return
	}
	if ok, wait := s.graphText.Allow(s.peer(r), time.Now()); !ok {
		writeError(w, &board.Error{Status: 429, Code: "request_rate", Message: "Too many graph text reads. Wait a little and retry.", RetryAfter: wait})
		return
	}
	msgs, truncated, err := store.GraphMessages(r.Context(), q)
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=30")
	jsonResponse(w, 200, map[string]any{"ok": true, "messages": msgs, "count": len(msgs), "truncated": truncated, "maximum": board.GraphMessagesMax,
		"note": "Visible messages in public rooms only, oldest first; text is each post's newest visible version. truncated means only the newest maximum were returned."})
}
