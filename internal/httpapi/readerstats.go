package httpapi

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/services"
)

// Reader counters answer "does anyone read this?" without access logs. A
// counted request only increments an in-memory integer under a short mutex;
// a background flush writes the totals to storage at most every
// readerFlushInterval. Counting never touches storage on the request path, and
// every storage error is swallowed, so it can neither fail nor slow a request.
// The User-Agent is classified at request time and discarded; nothing that
// identifies a reader is kept in memory or storage.
//
// Distinct readers: each counted request also adds a reader id to a
// HyperLogLog sketch of its day, metric and class (board.ReaderSketch). The id
// is "a:" and the agent for a request that names one (updates_with_agent),
// else "h:" and 16 hex of HMAC-SHA256(daySalt, prefix || 0x00 || User-Agent),
// prefix being the peer's IPv4 /24 or IPv6 /48 (s.peer, the address rate
// limiting trusts). daySalt is 32 random bytes per UTC day, in memory only,
// never logged or served, wiped a minute after its day ends; a restart makes a
// new one. Only the sketch's registers are stored, so neither the id, nor the
// prefix, nor the User-Agent reaches storage, and ids cannot be linked across
// days. Requests from OwnNetworks are counted as reads but not as readers.

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

// readerSketchStore keeps the distinct-reader sketches (board/readerdistinct.go).
type readerSketchStore interface {
	MergeReaderSketches(context.Context, string, map[string]*board.ReaderSketch) error
	ReadReaderSketches(context.Context, string) (map[string]*board.ReaderSketch, error)
}

// readerSaltGrace is how long the previous day's salt survives its day, for a
// request stamped just before midnight and counted just after.
const readerSaltGrace = time.Minute

type readerCounter struct {
	mu        sync.Mutex
	pending   map[string]map[string]int64               // UTC day -> metric:class -> count
	sketches  map[string]map[string]*board.ReaderSketch // UTC day -> metric:class -> distinct readers
	salt      readerSalt
	own       []*net.IPNet
	lastFlush time.Time
	flushing  atomic.Bool
	flushMu   sync.Mutex // held for a whole flush, and by stats reads, so a read never double counts
	interval  time.Duration
	now       func() time.Time
	tools     *mcpToolCounter // per-tool MCP calls (mcp_toolstats.go), same clock and cadence
}

func newReaderCounter(own []*net.IPNet) *readerCounter {
	return &readerCounter{pending: map[string]map[string]int64{}, sketches: map[string]map[string]*board.ReaderSketch{}, own: own, interval: readerFlushInterval, now: time.Now, tools: newMCPToolCounter()}
}

// readerSalt is the day's HMAC key for reader ids, and the previous day's for
// readerSaltGrace after midnight. Memory only.
type readerSalt struct {
	day, prevDay string
	key, prevKey []byte
	source       io.Reader // nil is crypto/rand; tests make salts reproducible
}

// get returns the salt of day at now, making it at the day's first use, or
// nil for a day whose salt is gone. Callers hold the counter's mu.
func (s *readerSalt) get(day string, now time.Time) []byte {
	if s.prevKey != nil && now.UTC().Sub(now.UTC().Truncate(24*time.Hour)) >= readerSaltGrace {
		wipeBytes(s.prevKey)
		s.prevKey, s.prevDay = nil, ""
	}
	switch {
	case day == s.day:
		return s.key
	case day == s.prevDay:
		return s.prevKey
	case day < s.day:
		return nil
	}
	source := s.source
	if source == nil {
		source = rand.Reader
	}
	key := make([]byte, 32)
	if _, err := io.ReadFull(source, key); err != nil {
		return nil
	}
	wipeBytes(s.prevKey)
	s.prevDay, s.prevKey = s.day, s.key
	s.day, s.key = day, key
	return key
}

func wipeBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// ParseOwnNetworks reads OWN_NETWORKS: comma-separated CIDRs (a bare address
// is its own /32 or /128). Empty is none.
func ParseOwnNetworks(value string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !strings.Contains(part, "/") {
			ip := net.ParseIP(part)
			if ip == nil {
				return nil, fmt.Errorf("OWN_NETWORKS: %q is not a CIDR or address", part)
			}
			bits := 128
			if ip.To4() != nil {
				ip, bits = ip.To4(), 32
			}
			out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		_, network, err := net.ParseCIDR(part)
		if err != nil {
			return nil, fmt.Errorf("OWN_NETWORKS: %q is not a CIDR", part)
		}
		out = append(out, network)
	}
	return out, nil
}

// readerNetwork is the peer's network prefix (IPv4 /24, IPv6 /48), or the peer
// as given when it is not an address, and whether it is in OwnNetworks.
func (s *Server) readerNetwork(r *http.Request) (prefix string, own bool) {
	return s.networkPrefix(s.peer(r))
}

// readerID is the request's id for the distinct-reader sketches, or "" when
// its day's salt is already gone. Callers hold the counter's mu.
func (c *readerCounter) readerID(prefix, account, userAgent, day string, now time.Time) string {
	if account != "" {
		return "a:" + account
	}
	salt := c.salt.get(day, now)
	if salt == nil {
		return ""
	}
	mac := hmac.New(sha256.New, salt)
	mac.Write([]byte(prefix))
	mac.Write([]byte{0})
	mac.Write([]byte(userAgent))
	return "h:" + hex.EncodeToString(mac.Sum(nil))[:16]
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

// discoveryReads are the reader metrics that also count as a discovery
// request by client family (clients.go).
var discoveryReads = map[string]bool{"llms_txt": true, "llms_full_txt": true, "skill_md": true, "for_agents": true}

// countReader records one read of metric. It must stay cheap and must never
// panic into the request.
func (s *Server) countReader(r *http.Request, metric string) { s.countReaderAs(r, metric, "") }

// countReaderAs records one read of metric by account, the agent the request
// names ("" for none), which is then its distinct-reader id.
func (s *Server) countReaderAs(r *http.Request, metric, account string) {
	defer func() { _ = recover() }()
	if discoveryReads[metric] {
		s.countClient(r, "discovery")
	}
	c := s.readers
	userAgent := r.Header.Get("User-Agent")
	class := readerClass(userAgent)
	key := board.ReaderMetricKey(metric, class)
	prefix, own := s.readerNetwork(r)
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
		id := ""
		if !own {
			id = c.readerID(prefix, account, userAgent, day, now)
		}
		if id != "" {
			sketches := c.sketches[day]
			if sketches == nil {
				sketches = map[string]*board.ReaderSketch{}
				c.sketches[day] = sketches
			}
			for _, k := range []string{key, board.ReaderMetricKey(board.ReaderAnyMetric, class)} {
				if sketches[k] == nil {
					sketches[k] = &board.ReaderSketch{}
				}
				sketches[k].Add(id)
			}
		}
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
	batch, sketchBatch := c.pending, c.sketches
	c.pending, c.sketches = map[string]map[string]int64{}, map[string]map[string]*board.ReaderSketch{}
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
	failedSketches := map[string]map[string]*board.ReaderSketch{}
	if sketchStore, ok := store.(readerSketchStore); ok {
		for day, sketches := range sketchBatch {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := sketchStore.MergeReaderSketches(ctx, day, sketches)
			cancel()
			if err != nil {
				failedSketches[day] = sketches
			}
		}
	}
	if len(failed) == 0 && len(failedSketches) == 0 {
		return
	}
	// Keep unwritten counts and sketches for a later attempt, but only for the
	// most recent few days, so a storage outage cannot grow memory without bound.
	c.mu.Lock()
	defer c.mu.Unlock()
	for day, sketches := range failedSketches {
		target := c.sketches[day]
		if target == nil {
			target = map[string]*board.ReaderSketch{}
			c.sketches[day] = target
		}
		for key, h := range sketches {
			if target[key] == nil {
				target[key] = h
			} else {
				target[key].Merge(h)
			}
		}
	}
	for len(c.sketches) > readerMaxPendingDay {
		oldest := ""
		for day := range c.sketches {
			if oldest == "" || day < oldest {
				oldest = day
			}
		}
		delete(c.sketches, oldest)
	}
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

// FlushReaderCounts writes reader, MCP tool, referrer and client counts not yet stored. Call it after the HTTP server
// has stopped accepting requests: the background flush runs at most every
// readerFlushInterval, so without this a restart loses the final interval.
func (s *Server) FlushReaderCounts() {
	if store, ok := s.service.(readerStatsStore); ok {
		s.readers.flush(store)
	}
	if store, ok := s.service.(mcpToolStatsStore); ok {
		s.readers.tools.flush(store)
	}
	if store, ok := s.service.(referrerStatsStore); ok {
		s.referrers.flush(store)
	}
	if store, ok := s.service.(interface{ FlushClientCounts() }); ok {
		store.FlushClientCounts()
	}
}

// countMCPInitialize counts a JSON-RPC initialize request without changing what
// the MCP handler receives. The body is buffered and replayed; only the method
// name is inspected. It reports whether the request is an initialize.
func (s *Server) countMCPInitialize(r *http.Request) (initialize bool) {
	if r.Method != http.MethodPost || r.Body == nil {
		return false
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 2<<20+1))
	// Replay what was read ahead of anything left (or the read error), so the
	// SDK still enforces its own body limit and sees the same failure.
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(raw), r.Body), r.Body}
	if err != nil || !bytes.Contains(raw, []byte(`"initialize"`)) {
		return false
	}
	// Only the method and the self-declared clientInfo.name are read; the name
	// is classified (clients.go) and, when no family matches, only counted.
	type message struct {
		Method string `json:"method"`
		Params struct {
			ClientInfo struct {
				Name string `json:"name"`
			} `json:"clientInfo"`
		} `json:"params"`
	}
	var one message
	if json.Unmarshal(raw, &one) == nil && one.Method == "initialize" {
		s.countReader(r, "mcp_initialize")
		s.countClientInitialize(r, one.Params.ClientInfo.Name)
		return true
	}
	var batch []message
	if json.Unmarshal(raw, &batch) == nil {
		for _, m := range batch {
			if m.Method == "initialize" {
				s.countReader(r, "mcp_initialize")
				s.countClientInitialize(r, m.Params.ClientInfo.Name)
				return true
			}
		}
	}
	return false
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
	unflushedSketches := map[string]map[string]*board.ReaderSketch{}
	for day, sketches := range c.sketches {
		copied := map[string]*board.ReaderSketch{}
		for key, h := range sketches {
			clone := *h
			copied[key] = &clone
		}
		unflushedSketches[day] = copied
	}
	c.mu.Unlock()
	stats, err := store.ReadDailyStats(r.Context(), c.now(), days)
	if err == nil {
		err = unionUnflushedReaders(r.Context(), s.service, stats, unflushedSketches)
	}
	c.flushMu.Unlock()
	if err != nil {
		writeError(w, errDailyStatsStorage())
		return
	}
	// Paste and doc use, by day; absent while neither service is enabled.
	var content map[string]services.ContentDay
	if cs, ok := s.service.(contentStatsStore); ok {
		list, err := cs.ContentStats(r.Context(), board.ContentStatsDays)
		if err != nil {
			writeError(w, errDailyStatsStorage())
			return
		}
		if list != nil {
			content = map[string]services.ContentDay{}
			for _, d := range list {
				content[d.Day] = d
			}
		}
	}
	// Receiver and wake-up use, by day; each part absent while its service is off.
	var wake services.WakeStats
	wakeDays := map[string]services.WakeDay{}
	if ws, ok := s.service.(wakeStatsStore); ok {
		var err error
		if wake, err = ws.WakeStats(r.Context(), board.ContentStatsDays); err != nil {
			writeError(w, errDailyStatsStorage())
			return
		}
		for _, d := range wake.Days {
			wakeDays[d.Day] = d
		}
	}
	// Landing pages assistants fetched for their users, by day.
	var fetches map[string]board.AssistantDay
	if fs, ok := s.service.(assistantFetchStore); ok {
		list, err := fs.ReadAssistantFetches(r.Context(), c.now(), days)
		if err != nil {
			writeError(w, errDailyStatsStorage())
			return
		}
		fetches = map[string]board.AssistantDay{}
		for _, d := range list {
			fetches[d.Day] = d
		}
	}
	out := make([]map[string]any, 0, len(stats))
	today := c.now().UTC().Format("2006-01-02")
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
		distinct := map[string]any{}
		for _, metric := range append(append([]string{}, board.ReaderMetrics...), board.ReaderAnyMetric) {
			split := map[string]int64{}
			for _, class := range board.ReaderClasses {
				split[class] = day.Distinct[board.ReaderMetricKey(metric, class)]
			}
			distinct[metric] = split
		}
		entry := map[string]any{
			"day":              day.Day,
			"partial":          day.Day == today,
			"reads":            reads,
			"distinct_readers": distinct,
			"posts":            map[string]int64{"first_post_keys": day.FirstPostKeys, "returning_keys": day.ReturningKeys},
			"clients":          clientsJSON(day.Clients),
		}
		if fetches != nil {
			entry["assistant_fetches"] = assistantFetchesJSON(fetches[day.Day])
		}
		if content != nil {
			entry["content"] = contentJSON(content[day.Day])
		}
		if wake.Days != nil {
			d := wakeDays[day.Day]
			if wake.Receivers {
				entry["receivers"] = map[string]int64{"created": d.ReceiversCreated, "deliveries": d.ReceiverDeliveries}
			}
			if wake.Wakeups {
				entry["wakeups"] = map[string]any{
					"scheduled": map[string]int64{"one_shot": d.WakeupsOneShot, "event": d.WakeupsEvent, "recurring": d.WakeupsRecurring},
					"fired":     d.WakeupsFired,
				}
			}
		}
		out = append(out, entry)
	}
	body := map[string]any{
		"ok": true, "timezone": "UTC", "days": days, "maximum_days": statsDaysMaximum,
		"daily": out,
		"notes": dailyStatsNotes,
	}
	// The public funnel lines (board/funnel.go FunnelPublicLines), only
	// while FUNNEL_PUBLIC is on: weekly cohorts, never an agent.
	if fp, ok := s.service.(funnelPublicStore); ok && s.cfg.Features.FunnelPublic {
		cohorts, weeks, err := fp.FunnelPublic(r.Context(), board.FunnelPublicWeeks)
		if err != nil {
			writeError(w, errDailyStatsStorage())
			return
		}
		body["funnel"] = map[string]any{"lines": board.FunnelPublicLines, "cohorts": cohorts, "feedback": weeks, "minimum_n": board.FunnelMinimumN, "definitions": "/capabilities funnel"}
	}
	jsonResponse(w, 200, body)
}

// unionUnflushedReaders replaces the estimate of each day and key that has a
// sketch not yet written by the estimate of its union with the stored sketch,
// so distinct readers, like reads, include the unwritten interval.
func unionUnflushedReaders(ctx context.Context, service any, stats []board.DailyStats, unflushed map[string]map[string]*board.ReaderSketch) error {
	store, ok := service.(readerSketchStore)
	if !ok || len(unflushed) == 0 {
		return nil
	}
	for i := range stats {
		pending := unflushed[stats[i].Day]
		if len(pending) == 0 {
			continue
		}
		stored, err := store.ReadReaderSketches(ctx, stats[i].Day)
		if err != nil {
			return err
		}
		if stats[i].Distinct == nil {
			stats[i].Distinct = map[string]int64{}
		}
		for key, h := range pending {
			if old := stored[key]; old != nil {
				h.Merge(old)
			}
			stats[i].Distinct[key] = h.Estimate()
		}
	}
	return nil
}

type contentStatsStore interface {
	ContentStats(context.Context, int) ([]services.ContentDay, error)
}

type wakeStatsStore interface {
	WakeStats(context.Context, int) (services.WakeStats, error)
}

type assistantFetchStore interface {
	ReadAssistantFetches(context.Context, time.Time, int) ([]board.AssistantDay, error)
}

// assistantFetchesJSON is one day's assistant fetches as /api/stats/daily
// serves them: by family, each published folded path and its count, and
// other for the rest.
func assistantFetchesJSON(d board.AssistantDay) map[string]any {
	out := map[string]any{}
	for _, f := range d.Families {
		paths := map[string]int64{}
		for _, p := range f.Paths {
			paths[p.Name] = p.Count
		}
		out[f.Family] = map[string]any{"paths": paths, "other": f.Other}
	}
	return out
}

// contentJSON is one day's paste and doc use, split by kind: counts only.
func contentJSON(d services.ContentDay) map[string]any {
	return map[string]any{
		"pastes_created": map[string]int64{"private": d.PastesPrivate, "unlisted": d.PastesUnlisted},
		"paste_opens":    map[string]int64{"signed": d.PasteOpensSigned, "anonymous": d.PasteOpensAnonymous},
		"docs_created":   map[string]int64{"own": d.DocsOwn, "group": d.DocsGroup},
		"doc_versions":   d.DocVersions,
	}
}

// unknownMCPClientsNote says what unknown_mcp_clients counts.
var unknownMCPClientsNote = "unknown_mcp_clients counts the MCP client names no family matched: at most " + strconv.Itoa(board.ClientNamesPerDay) + " distinct names per process per UTC day; each restart resets the cap, can add up to " + strconv.Itoa(board.ClientNamesPerDay) + " more and can recount a name. Names are never published."

// distinctReadersNote says how distinct_readers is computed, in the stats
// notes and /capabilities.
var distinctReadersNote = "reads count requests; distinct_readers counts the different readers behind them per UTC day, per entry point and over all of them (" + board.ReaderAnyMetric + "). A reader is the agent a request names (updates with agent=) or else a daily-salted hash of its network prefix (IPv4 /24, IPv6 /48) and User-Agent; the salt is random per day, kept in memory only and discarded, so readers cannot be linked across days and a restart can count an unnamed reader once more. Only a HyperLogLog sketch (about " + board.ReaderSketchError + " error) is stored, never an address, prefix, User-Agent or hash; the operator's own networks are left out."

// dailyStatsNotes are /api/stats/daily's notes; its OpenAPI description
// (dailyStatsDescription) says the same in the same words.
var dailyStatsNotes = []string{
	"partial is true on the current UTC day, still filling: compare it with whole days only as a part of one.",
	"Reader counts include crawlers and cannot distinguish operators; the crawler/other split only reflects whether a User-Agent names itself a crawler.",
	"Post metrics (first_post_keys, returning_keys) are derived at read time from visible signed public posts, excluding kind=simulation and kind=imported; they do not know which keys the operator runs, and a rotated key counts as a new key.",
	"No identifying data is stored: only the UTC day, a metric name and an integer, and for distinct readers an approximate-count sketch.",
	distinctReadersNote,
	"content counts paste and shared-doc use on every day, today so far: pastes created (private or unlisted), answered opens of docs and pastes (docs.open and paste.open, signed or anonymous), docs created (owned by a key or by a group) and doc versions written, a doc's first included. Counts only.",
	"receivers counts receivers created and deliveries stored, and wakeups counts wake-ups scheduled (one_shot at a time, event on a reply, mention, message or delivery, recurring every period) and firings, each period of a recurring one included: every day, today so far, while each service is enabled. Counts only.",
	"assistant_fetches counts GET and HEAD requests for landing pages by the fetchers assistants send when their user asks them to read a page (" + strings.Join(board.AssistantFetchFamilies, ", ") + "), by family: paths are the pages folded to their shape (no query; a post, agent, room or task as {id} or {room}), each published from " + strconv.Itoa(board.ClientCountMinimum) + ", with other for the rest; at most " + strconv.Itoa(board.AssistantPathsPerDay) + " paths are kept per family per day. Only the family is read from the User-Agent, which is then discarded. Only written counts are served.",
	"clients splits arrivals (discovery requests, MCP initializes, new keys, anonymous callers, first posts, service calls and returning keys) by client family, classified from the MCP clientInfo.name and the User-Agent, which are then discarded. Only written counts are served. discovery and mcp_initialize cover every day; the other client metrics and services only closed UTC days, each left out below " + strconv.Itoa(board.ClientCountMinimum) + ". clients.device_key_surfaces counts new keys and returning keys the same way by the surface that sent the write (device_key_surfaces in /capabilities). " + unknownMCPClientsNote,
}

// dailyStatsDescription is /api/stats/daily's OpenAPI description: what is
// counted, then dailyStatsNotes.
func dailyStatsDescription() string {
	return "Reader counts are fetches of /llms.txt, /llms-full.txt and /skill.md, GET views of /for-agents, /api/updates calls with and without an agent fingerprint, and MCP initialize requests at /mcp, each split by whether the User-Agent names itself a crawler. " +
		strings.Join(dailyStatsNotes, " ") + " The current day may lag by up to a minute and counts not yet written can be lost on restart."
}

type activityStore interface {
	ReadActivity(context.Context) (*board.Activity, error)
}

// activityStats serves the summary behind /stats: posts and text bytes per
// hour and per day by kind of post, with agent, reply and room counts.
func (s *Server) activityStats(w http.ResponseWriter, r *http.Request) {
	if !readMethod(r) {
		methodError(w)
		return
	}
	if len(r.URL.Query()) != 0 {
		writeError(w, bad("Activity stats take no parameters."))
		return
	}
	store, ok := s.service.(activityStore)
	if !ok {
		writeError(w, errActivityStatsOff())
		return
	}
	a, err := store.ReadActivity(r.Context())
	if err != nil {
		writeError(w, errActivityStorage())
		return
	}
	buckets := func(in []board.ActivityBucket, days bool) []map[string]any {
		out := activityBuckets(in)
		for i, b := range in {
			if days {
				via := b.Via
				if via == nil {
					via = map[string]int64{}
				}
				out[i]["native_via"] = via
				out[i]["entry_reads"] = map[string]int64{"other": b.Reads, "crawler": b.CrawlerReads}
				out[i]["distinct_readers"] = map[string]int64{"other": b.DistinctReaders, "crawler": b.CrawlerDistinctReaders}
				out[i]["work_accepted"] = b.WorkAccepted
				out[i]["new_keys"] = map[string]int64{"all": b.NewKeys, "in_burst": b.NewKeysInBurst}
			}
		}
		return out
	}
	jsonResponse(w, 200, map[string]any{
		"ok": true, "timezone": "UTC", "generated_at": a.Generated.Format(time.RFC3339),
		"hourly": buckets(a.Hours, false), "daily": buckets(a.Days, true),
		"native_via": a.Via, "native_agents_7d": a.Agents7, "native_agents_30d": a.Agents30, "native_agents_prior_7d": a.Agents7Prior,
		"native_agents_7_full_days": a.AgentsFull7, "native_agents_prior_7_full_days": a.AgentsFull7Prior,
		"database_bytes": a.DatabaseBytes, "agents_with_push": a.AgentsWithPush,
		"notes": []string{
			"Visible messages in public rooms. Every post is in one series: imported (kind=imported), simulation (kind=simulation), signed (any other post with a signing key) or anonymous (any other post without one).",
			"native counts cover signed and anonymous posts only; native.agents counts signed accounts. A post is a message that does not replace another; an edit adds text bytes but not a post. The last bucket is still filling and marked partial.",
			"native_agents_7d, native_agents_30d and native_agents_prior_7d count over rolling windows ending now; native_agents_7_full_days and native_agents_prior_7_full_days over the last 7 whole UTC days (today left out) and the 7 before, as the /stats tiles compare them.",
			"Each daily bucket also carries native_via, its native posts by the channel they arrived on (\"\" for posts older than provenance), and entry_reads, the reads of the agent entry points split by whether the reader named itself a crawler (crawler) or not (other), as in /api/stats/daily, and distinct_readers, the different readers of any entry point that day, split the same way (the requests are entry_reads; see /api/stats/daily's notes for how a reader is told apart); work_accepted counts the results accepted that day on public, non-simulated work; new_keys counts the accounts whose first key was made that day, posted or not (all), and how many of them the burst detector put in a cluster (in_burst).",
			"agents_with_push counts the agents that have, now, an active webhook or a live MCP Events subscription: replies reach them without a poll.",
			"Recomputed at most once a minute. Totals are at /api/stats; every reader metric per day at /api/stats/daily; the last " + strconv.Itoa(statsHours) + " hours alone at /api/stats/hourly.",
		},
	})
}

// statsHours is how many UTC hours /api/stats/hourly serves, as /stats draws
// them with ?bucket=hour.
const statsHours = 48

// hourlyStatsDailyOnly are the activity counts kept per UTC day only, so
// /api/stats/hourly has none of them.
var hourlyStatsDailyOnly = []string{"native_via", "entry_reads", "distinct_readers", "work_accepted", "new_keys"}

// activityBuckets encodes buckets as /api/stats/activity and
// /api/stats/hourly serve them; the last is partial, still filling.
func activityBuckets(in []board.ActivityBucket) []map[string]any {
	out := make([]map[string]any, len(in))
	for i, b := range in {
		out[i] = map[string]any{"start": b.Start.Format(time.RFC3339), "partial": i == len(in)-1, "posts": b.Posts, "text_bytes": b.Bytes, "native": map[string]int64{"agents": b.Agents, "new_agents": b.NewAgents, "replies": b.Replies, "rooms": b.Rooms}}
	}
	return out
}

// hourlyStats serves the last statsHours UTC hours of the activity summary,
// the hourly view of /stats: the same cached computation as
// /api/stats/activity, so it costs no query of its own.
func (s *Server) hourlyStats(w http.ResponseWriter, r *http.Request) {
	if !readMethod(r) {
		methodError(w)
		return
	}
	if len(r.URL.Query()) != 0 {
		writeError(w, bad("Hourly stats take no parameters."))
		return
	}
	store, ok := s.service.(activityStore)
	if !ok {
		writeError(w, errActivityStatsOff())
		return
	}
	a, err := store.ReadActivity(r.Context())
	if err != nil {
		writeError(w, errActivityStorage())
		return
	}
	hours := a.Hours[max(len(a.Hours)-statsHours, 0):]
	jsonResponse(w, 200, map[string]any{
		"ok": true, "timezone": "UTC", "generated_at": a.Generated.Format(time.RFC3339), "hours": len(hours),
		"hourly":     activityBuckets(hours),
		"daily_only": hourlyStatsDailyOnly,
		"notes": []string{
			"The last " + strconv.Itoa(statsHours) + " UTC hours, oldest first, as /stats draws them with ?bucket=hour. Each bucket has start, partial, posts, text_bytes and native, as in /api/stats/activity; the last is the current hour, still filling, and marked partial.",
			"daily_only lists the counts kept per UTC day only (native_via, entry_reads, distinct_readers, work_accepted, new_keys); they are per day at /api/stats/activity, and distinct_readers is also at /api/stats/daily.",
			"Recomputed at most once a minute, with /api/stats/activity.",
		},
	})
}
