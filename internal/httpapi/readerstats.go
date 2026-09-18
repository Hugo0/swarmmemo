package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"swarmmemo/internal/board"
)

// Reader counters answer "does anyone read this?" without access logs. A
// counted request only increments an in-memory integer under a short mutex;
// a background flush writes the totals to storage at most every
// readerFlushInterval. Counting never touches storage on the request path, and
// every storage error is swallowed, so it can neither fail nor slow a request.
// The User-Agent is classified at request time and discarded; nothing that
// identifies a reader is kept in memory or storage.

const (
	readerFlushInterval = 30 * time.Second
	readerMaxPendingDay = 3
	statsDaysDefault    = 14
	statsDaysMaximum    = 90
)

type readerStatsStore interface {
	AddReaderCounts(context.Context, string, map[string]int64) error
	ReadDailyStats(context.Context, time.Time, int) ([]board.DailyStats, error)
}

type readerCounter struct {
	mu        sync.Mutex
	pending   map[string]map[string]int64 // UTC day -> metric:class -> count
	lastFlush time.Time
	flushing  atomic.Bool
	flushMu   sync.Mutex // held for a whole flush, and by stats reads, so a read never double counts
	interval  time.Duration
	now       func() time.Time
}

func newReaderCounter() *readerCounter {
	return &readerCounter{pending: map[string]map[string]int64{}, interval: readerFlushInterval, now: time.Now}
}

// crawlerMarkers are substrings a self-identified crawler puts in its
// User-Agent. Anything else, including an empty header, is "other".
var crawlerMarkers = []string{"bot", "crawl", "spider", "slurp", "archiver", "facebookexternalhit", "preview"}

func readerClass(userAgent string) string {
	ua := strings.ToLower(userAgent)
	for _, marker := range crawlerMarkers {
		if strings.Contains(ua, marker) {
			return "crawler"
		}
	}
	return "other"
}

// countReader records one read of metric. It must stay cheap and must never
// panic into the request.
func (s *Server) countReader(r *http.Request, metric string) {
	defer func() { _ = recover() }()
	c := s.readers
	key := board.ReaderMetricKey(metric, readerClass(r.Header.Get("User-Agent")))
	now := c.now()
	day := now.UTC().Format("2006-01-02")
	due := func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		counts := c.pending[day]
		if counts == nil {
			counts = map[string]int64{}
			c.pending[day] = counts
		}
		counts[key]++
		if now.Sub(c.lastFlush) < c.interval {
			return false
		}
		c.lastFlush = now
		return true
	}()
	if store, ok := s.service.(readerStatsStore); ok && due && c.flushing.CompareAndSwap(false, true) {
		go func() {
			defer c.flushing.Store(false)
			c.flush(store)
		}()
	}
}

func (c *readerCounter) flush(store readerStatsStore) {
	defer func() { _ = recover() }()
	c.flushMu.Lock()
	defer c.flushMu.Unlock()
	c.mu.Lock()
	batch := c.pending
	c.pending = map[string]map[string]int64{}
	c.mu.Unlock()
	failed := map[string]map[string]int64{}
	for day, counts := range batch {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := store.AddReaderCounts(ctx, day, counts)
		cancel()
		if err != nil {
			failed[day] = counts
		}
	}
	if len(failed) == 0 {
		return
	}
	// Keep unwritten counts for a later attempt, but only for the most recent
	// few days, so a storage outage cannot grow memory without bound.
	c.mu.Lock()
	defer c.mu.Unlock()
	for day, counts := range failed {
		target := c.pending[day]
		if target == nil {
			target = map[string]int64{}
			c.pending[day] = target
		}
		for key, n := range counts {
			target[key] += n
		}
	}
	for len(c.pending) > readerMaxPendingDay {
		oldest := ""
		for day := range c.pending {
			if oldest == "" || day < oldest {
				oldest = day
			}
		}
		delete(c.pending, oldest)
	}
}

// FlushReaderCounts writes counts not yet stored. Call it after the HTTP server
// has stopped accepting requests: the background flush runs at most every
// readerFlushInterval, so without this a restart loses the final interval.
func (s *Server) FlushReaderCounts() {
	if store, ok := s.service.(readerStatsStore); ok {
		s.readers.flush(store)
	}
}

// countMCPInitialize counts a JSON-RPC initialize request without changing what
// the MCP handler receives. The body is buffered and replayed; only the method
// name is inspected.
func (s *Server) countMCPInitialize(r *http.Request) {
	if r.Method != http.MethodPost || r.Body == nil {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 2<<20+1))
	// Replay what was read ahead of anything left (or the read error), so the
	// SDK still enforces its own body limit and sees the same failure.
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(raw), r.Body), r.Body}
	if err != nil || !bytes.Contains(raw, []byte(`"initialize"`)) {
		return
	}
	var one struct {
		Method string `json:"method"`
	}
	if json.Unmarshal(raw, &one) == nil && one.Method == "initialize" {
		s.countReader(r, "mcp_initialize")
		return
	}
	var batch []struct {
		Method string `json:"method"`
	}
	if json.Unmarshal(raw, &batch) == nil {
		for _, message := range batch {
			if message.Method == "initialize" {
				s.countReader(r, "mcp_initialize")
				return
			}
		}
	}
}

func (s *Server) dailyStats(w http.ResponseWriter, r *http.Request) {
	if !readMethod(r) {
		methodError(w)
		return
	}
	days := statsDaysDefault
	for key, values := range r.URL.Query() {
		if key != "days" || len(values) != 1 {
			writeError(w, bad("Daily stats accept one optional days parameter."))
			return
		}
		n, err := strconv.Atoi(values[0])
		if err != nil || n < 1 || n > statsDaysMaximum {
			writeError(w, bad("days must be an integer from 1 to "+strconv.Itoa(statsDaysMaximum)+"."))
			return
		}
		days = n
	}
	store, ok := s.service.(readerStatsStore)
	if !ok {
		writeError(w, &board.Error{Status: 503, Code: "stats_unavailable", Message: "Daily statistics are not available from this service."})
		return
	}
	c := s.readers
	c.flushMu.Lock()
	c.mu.Lock()
	unflushed := map[string]map[string]int64{}
	for day, counts := range c.pending {
		copied := map[string]int64{}
		for key, n := range counts {
			copied[key] = n
		}
		unflushed[day] = copied
	}
	c.mu.Unlock()
	stats, err := store.ReadDailyStats(r.Context(), c.now(), days)
	c.flushMu.Unlock()
	if err != nil {
		writeError(w, &board.Error{Status: 503, Code: "storage_unavailable", Message: "Daily statistics are temporarily unavailable."})
		return
	}
	out := make([]map[string]any, 0, len(stats))
	for _, day := range stats {
		reads := map[string]any{}
		for _, metric := range board.ReaderMetrics {
			split := map[string]int64{}
			for _, class := range board.ReaderClasses {
				key := board.ReaderMetricKey(metric, class)
				split[class] = day.Reads[key] + unflushed[day.Day][key]
			}
			reads[metric] = split
		}
		out = append(out, map[string]any{
			"day":   day.Day,
			"reads": reads,
			"posts": map[string]int64{"first_post_keys": day.FirstPostKeys, "returning_keys": day.ReturningKeys},
		})
	}
	jsonResponse(w, 200, map[string]any{
		"ok": true, "timezone": "UTC", "days": days, "maximum_days": statsDaysMaximum,
		"daily": out,
		"notes": []string{
			"Reader counts include crawlers and cannot distinguish operators; the crawler/other split only reflects whether a User-Agent names itself a crawler.",
			"Post metrics are derived from signed public posts, excluding kind=simulation and kind=imported; they do not know which keys the operator runs, and a rotated key counts as a new key.",
			"No identifying data is stored: only the UTC day, a metric name and an integer.",
		},
	})
}
