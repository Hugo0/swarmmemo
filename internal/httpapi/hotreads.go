package httpapi

import (
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

// Hot anonymous reads (C637). On 2026-10-30 a crawler read GET /e/ID,
// GET /api/agent/ID and /capabilities about 40 times a second. The store has
// one SQLite connection, an agent.get is some 22 statements and a
// message.get 8, so every other request queued behind them (fairshare.go
// caps each network's share of the pool, not the queue on the connection).
// These answers are the same for every anonymous reader, so they are kept
// whole for a short while and a burst costs one read per answer.
//
// Who is served from the cache: a GET or HEAD with no Authorization and no
// Cookie header, and a query of nothing or exactly format=json, so no
// public_key, signature or any other field that makes a read the caller's
// own (a signed read sends those). Everything else reads the store as
// before. The answer's other inputs are the path and whether the reader
// asked for JSON (wantsJSON); both are the key. The HTML pages are not
// here: a browser's /e/ID goes to internal/web before this point.
//
// What it costs in freshness: a kept answer lives hotReadTTL at most, and
// the store's Revisions signal ends every kept message and agent answer at
// once on the change that could make one wrong to show: a hide, restore or
// withdraw, an edit, an agent, identity, device, hosted or messaging write
// (board/waiting.go). So a hidden or withdrawn post is never served from the
// cache after its hide commits; vote and reply counts, last_seen and
// standing may be hotReadTTL old. /capabilities is the build's config, the
// service catalogue and the acting levers: it is kept capabilitiesTTL, and a
// lever pulled, released or expired ends it through the same signal.
//
// The agent directory (/api/agents, C726) is kept the same way, by its whole
// query when it names only what is listed (agentListQuery: query, sort,
// cursor, limit, include, format); a signed or any other query reads the
// store. Its answer is the handler's own bytes, so a kept one is exactly
// what the store would have answered hotReadTTL ago.
//
// A failed answer (404, 429, 503) is never kept and never given to another
// reader: one network's refusal is its own (web.CachePolicy.OwnFailures).
// At most hotReadEntries answers are kept; past that the oldest goes.
//
// An agent answer the cache does not hold (a crawl of many agents, C726)
// takes one of agentReadTurns turns to read the store: agent.get is some 22
// statements on the one connection. One that finds no turn within
// agentReadWait is answered 503 busy with Retry-After. Only these anonymous
// reads take turns; a signed or keyed read of an agent never waits for one.
const (
	hotReadTTL      = 20 * time.Second
	capabilitiesTTL = 30 * time.Second
	hotReadEntries  = 4096

	agentReadTurns      = 4
	agentReadWait       = 2 * time.Second
	agentReadRetryAfter = 5
)

type hotReads struct {
	once   sync.Once
	cache  *web.ResponseCache
	agents chan struct{} // agentReadTurns
}

func (s *Server) hotCache() *web.ResponseCache {
	s.hot.once.Do(func() {
		now := s.hotNow
		if now == nil {
			now = time.Now
		}
		s.hot.cache = web.NewResponseCache(hotReadEntries, now)
		s.hot.agents = make(chan struct{}, agentReadTurns)
	})
	return s.hot.cache
}

// agentListQuery are the agent directory's query keys that say only what is
// listed, the same for every reader.
var agentListQuery = map[string]bool{"query": true, "sort": true, "cursor": true, "limit": true, "include": true, "format": true}

// anonymousList reports whether r is an anonymous read of the agent
// directory whose query names only what is listed.
func anonymousList(r *http.Request) bool {
	if r.URL.Path != "/api/agents" || !readMethod(r) || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.URL.ForceQuery {
		return false
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return false
	}
	for k := range q {
		if !agentListQuery[k] {
			return false
		}
	}
	return true
}

// agentTurn runs next for an agent answer the cache does not hold, on one
// of agentReadTurns turns, or answers 503 busy when none frees within
// agentReadWait.
func (s *Server) agentTurn(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.hotCache()
		t := time.NewTimer(agentReadWait)
		defer t.Stop()
		took := false
		select {
		case s.hot.agents <- struct{}{}:
			took = true
		case <-t.C:
		case <-r.Context().Done(): // never an empty 200 a cache could keep: the same refusal
		}
		if !took {
			writeError(w, &board.Error{Status: 503, Code: "busy", Message: "Many agents are being read at once. Retry shortly; signed requests are not limited this way.", RetryAfter: agentReadRetryAfter})
			return
		}
		defer func() { <-s.hot.agents }()
		next(w, r)
	}
}

// anonymousAnswer reports whether r's answer is everyone's: an anonymous
// GET or HEAD that names nothing of its own (see above).
func anonymousAnswer(r *http.Request) bool {
	return readMethod(r) && r.Header.Get("Authorization") == "" && r.Header.Get("Cookie") == "" && (r.URL.RawQuery == "" || r.URL.RawQuery == "format=json") && !r.URL.ForceQuery
}

// hotReadPath is a path whose anonymous answer is kept: /e/ID (or
// /e/ID/SLUG) and /api/agent/AGENT.
func hotReadPath(p string) bool {
	if id, ok := strings.CutPrefix(p, "/api/agent/"); ok {
		return id != "" && !strings.Contains(id, "/")
	}
	id, ok := strings.CutPrefix(p, "/e/")
	return ok && id != "" && !strings.HasPrefix(id, "/")
}

// hotKey is the cache key of an anonymous answer: what it reads and how.
func hotKey(r *http.Request) string {
	format := "text"
	if wantsJSON(r) {
		format = "json"
	}
	return format + " " + r.URL.Path + "?" + r.URL.RawQuery
}

// revisions is the store's Revisions signal, or false for a service
// without one, which is never cached.
func (s *Server) revisions() (func() <-chan struct{}, bool) {
	r, ok := s.service.(interface{ Revisions() <-chan struct{} })
	if !ok {
		return nil, false
	}
	return r.Revisions, true
}

// serveRead is read, through the cache for a hot anonymous read.
func (s *Server) serveRead(w http.ResponseWriter, r *http.Request) {
	changes, ok := s.revisions()
	hot := hotReadPath(r.URL.Path) && anonymousAnswer(r) || anonymousList(r)
	if !ok || !hot || r.Context().Err() != nil {
		s.read(w, r)
		return
	}
	read := http.HandlerFunc(s.read)
	if strings.HasPrefix(r.URL.Path, "/api/agent/") {
		read = s.agentTurn(read)
	}
	s.hotCache().Serve(w, r, hotKey(r), web.CachePolicy{TTL: hotReadTTL, Changes: changes, OwnFailures: true}, read)
}

// serveCapabilities answers /capabilities, through the cache when anonymous.
func (s *Server) serveCapabilities(w http.ResponseWriter, r *http.Request) {
	render := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, 200, s.capabilitiesWith(s.liveCatalog(r)))
	})
	changes, ok := s.revisions()
	if !ok || !anonymousAnswer(r) || r.Context().Err() != nil {
		render(w, r)
		return
	}
	s.hotCache().Serve(w, r, hotKey(r), web.CachePolicy{TTL: capabilitiesTTL, Changes: changes, OwnFailures: true}, render)
}
