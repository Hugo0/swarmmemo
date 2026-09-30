package board

import (
	"context"
	"fmt"
	"hash/maphash"
	"log/slog"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Arrivals by client: per UTC day, how agents arrive, split by the kind of
// client that carried them. An adapter classifies each request into one of
// ClientFamilies (httpapi/clients.go holds the table) and passes the family
// in the context; the User-Agent and any client name are discarded there.
// Like the reader counters these reuse counters(scope,value), so they need no
// schema change, and every row is one (UTC day, name, integer):
//
//	client:YYYY-MM-DD:family:FAMILY:METRIC           one of ClientMetrics
//	client:YYYY-MM-DD:family:FAMILY:service:SERVICE  accepted calls of one service
//	client:YYYY-MM-DD:unknown_mcp_clients            distinct MCP client names no family matched
//
// No address, key, fingerprint, pseudonym, User-Agent or client name is
// stored. The subjects that are told apart (to count each once a day) are
// held in memory only, as hashes under a per-process random seed, and
// forgotten at the end of the UTC day. An unknown MCP client name is
// self-chosen text, so it is never stored or published: it is counted, kept
// reduced in memory for the day, and at the day's end its most frequent
// names go to the operator's log (logClientNames). AddClientCounts refuses
// any other key, so no caller can smuggle an identifier into a scope string.
//
// Publication (ReadClientStats) reads stored counts only, never those still
// in memory. discovery and mcp_initialize are published for every day; the
// command metrics (the rest, and services) only for a closed UTC day and
// only from ClientCountMinimum, so polling today's figures cannot tie one
// key's first public post to the client it runs on.

// ClientFamilies are the only client families counted.
var ClientFamilies = []string{"cursor-grok", "openai", "meta-muse", "claude", "gemini", "perplexity", "other-mcp", "scripts", "browsers", "crawlers", "other"}

// ClientMetrics are the only per-family metrics stored.
var ClientMetrics = []string{"discovery", "mcp_initialize", "new_keys", "anonymous_subjects", "first_posts", "service_calls", "returning_1d", "returning_3d", "returning_7d"}

const (
	// ClientNameMaxBytes is how much of a raw client name is ever read.
	ClientNameMaxBytes = 256
	// ClientNameMaxChars bounds one reduced client name.
	ClientNameMaxChars = 32
	// ClientCountMinimum is the smallest command count published for a
	// family on a day; a smaller one is left out.
	ClientCountMinimum    = 3
	clientNamesPerDay     = 100    // unknown MCP client names told apart in memory per day
	clientNamesLogged     = 20     // of which the most frequent are logged at the day's end
	clientServicesPending = 64     // family:service keys held in memory per day
	clientSubjectsPerDay  = 100000 // subjects told apart in memory per day (about 4 MB)
	clientScopePrefix     = "client:"
	clientFlushInterval   = 30 * time.Second
)

// ClientName reduces a self-declared client name to at most
// ClientNameMaxChars of a-z, 0-9, '.', '_' and '-': lowercased, every other
// run of characters one '-', trimmed of separators. "" means nothing usable.
func ClientName(raw string) string {
	if len(raw) > ClientNameMaxBytes {
		raw = raw[:ClientNameMaxBytes]
	}
	var b strings.Builder
	dash := false
	for _, c := range strings.ToLower(raw) {
		switch {
		case c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-':
			b.WriteRune(c)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	name := b.String()
	if len(name) > ClientNameMaxChars {
		name = name[:ClientNameMaxChars]
	}
	return strings.Trim(name, "-._")
}

func validServiceKey(id string) bool {
	if id == "" || len(id) > 48 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func validClientFamily(f string) bool {
	for _, known := range ClientFamilies {
		if f == known {
			return true
		}
	}
	return false
}

// clientRequestMetric reports whether metric counts requests (published for
// every day) rather than commands (published for closed days only).
func clientRequestMetric(metric string) bool {
	return metric == "discovery" || metric == "mcp_initialize"
}

func validClientKey(key string) bool {
	parts := strings.Split(key, ":")
	switch {
	case key == "unknown_mcp_clients":
		return true
	case len(parts) == 3 && parts[0] == "family" && validClientFamily(parts[1]):
		for _, m := range ClientMetrics {
			if parts[2] == m {
				return true
			}
		}
	case len(parts) == 4 && parts[0] == "family" && validClientFamily(parts[1]) && parts[2] == "service":
		return validServiceKey(parts[3])
	}
	return false
}

type clientFamilyKey struct{}

// WithClientFamily records the family an adapter classified the caller into.
// A context without one (a background job, a transport that does not
// classify) is not counted.
func WithClientFamily(ctx context.Context, family string) context.Context {
	return context.WithValue(ctx, clientFamilyKey{}, family)
}

// ClientFamilyFrom is the family recorded in ctx, or "".
func ClientFamilyFrom(ctx context.Context) string {
	f, _ := ctx.Value(clientFamilyKey{}).(string)
	if !validClientFamily(f) {
		return ""
	}
	return f
}

// Facts counted at most once per subject and day.
const (
	subjectContact uint8 = 1 << iota
	subjectPost
)

// clientState is the in-memory side: today's subjects and names, and counts
// not yet written. mu is never held across I/O.
type clientState struct {
	mu        sync.Mutex
	seed      maphash.Seed
	day       string
	subjects  map[uint64]uint8
	names     map[string]int64            // unknown MCP client name -> initializes today; never stored
	services  map[string]bool             // family:service keys counted today
	pending   map[string]map[string]int64 // UTC day -> key -> count
	lastFlush time.Time
	flushing  atomic.Bool
	flushMu   sync.Mutex // held for a whole flush
}

// resetLocked moves the in-memory day forward to day, starting its subjects
// and names, and reports whether day is the current day; c.mu is held. It
// never moves back: a command stamped before midnight that completes after
// it (a slow command, a service call's afterCommit) is not current, and its
// once-a-day facts are not claimed.
func (c *clientState) resetLocked(day string) bool {
	if c.subjects == nil {
		c.seed = maphash.MakeSeed()
	}
	if day < c.day {
		return false
	}
	if day > c.day {
		if len(c.names) > 0 {
			go logClientNames(c.day, c.names)
		}
		c.day, c.subjects, c.names, c.services = day, map[uint64]uint8{}, map[string]int64{}, map[string]bool{}
	}
	return true
}

// logClientNames tells the operator, in the service log only, the ended
// day's most frequent unknown MCP client names, so a new platform can be
// added to the family table. It is the only place a name leaves memory.
func logClientNames(day string, names map[string]int64) {
	slog.Info("arrivals: unknown MCP client names", "day", day, "distinct", len(names), "top", topClientNames(names))
}

// topClientNames is the at most clientNamesLogged most frequent names (each
// ClientName-reduced) with their initializes, most first.
func topClientNames(names map[string]int64) string {
	order := make([]string, 0, len(names))
	for name := range names {
		order = append(order, name)
	}
	sort.Slice(order, func(i, j int) bool {
		return names[order[i]] > names[order[j]] || names[order[i]] == names[order[j]] && order[i] < order[j]
	})
	if len(order) > clientNamesLogged {
		order = order[:clientNamesLogged]
	}
	for i, name := range order {
		order[i] = fmt.Sprintf("%s %d", name, names[name])
	}
	return strings.Join(order, ", ")
}

// claimLocked reports whether fact has not yet been counted today for
// subject, and marks it. An empty subject, or a new one once the day's table
// is full, is never claimed.
func (c *clientState) claimLocked(subject string, fact uint8) bool {
	if subject == "" {
		return false
	}
	h := maphash.String(c.seed, subject)
	flags, held := c.subjects[h]
	if !held && len(c.subjects) >= clientSubjectsPerDay || flags&fact != 0 {
		return false
	}
	c.subjects[h] = flags | fact
	return true
}

// serviceLocked reports whether a family:service key may be counted today:
// one already counted, or a new one while fewer than clientServicesPending
// are. Past that, the family's service_calls still counts the call.
func (c *clientState) serviceLocked(key string) bool {
	if !c.services[key] && len(c.services) >= clientServicesPending {
		return false
	}
	c.services[key] = true
	return true
}

// addLocked adds counts for day; c.mu is held.
func (c *clientState) addLocked(day string, counts map[string]int64) {
	if c.pending == nil {
		c.pending = map[string]map[string]int64{}
	}
	target := c.pending[day]
	if target == nil {
		target = map[string]int64{}
		c.pending[day] = target
	}
	for key, n := range counts {
		target[key] += n
	}
}

// addClientCounts adds counts in memory and starts a background write when
// one is due. It never touches storage on the caller's path.
func (s *Store) addClientCounts(day string, counts map[string]int64, now time.Time) {
	if len(counts) == 0 {
		return
	}
	c := &s.clients
	c.mu.Lock()
	c.addLocked(day, counts)
	due := now.Sub(c.lastFlush) >= clientFlushInterval
	if due {
		c.lastFlush = now
	}
	c.mu.Unlock()
	if due && c.flushing.CompareAndSwap(false, true) {
		go func() {
			defer c.flushing.Store(false)
			s.FlushClientCounts()
		}()
	}
}

// subjectOf is the key a source is told apart by in memory: its network
// prefix (the /64 or /24 an anonymous subject covers), or "" for a source
// that is not an address.
func subjectOf(source string) string {
	if _, err := netip.ParseAddr(source); err != nil {
		return ""
	}
	return anonPrefixKey(source)
}

// CountClient counts one request that is not a command: metric is
// "discovery" or "mcp_initialize". For an MCP initialize whose clientInfo.name
// no family matched, name is that name: the first time it is seen in a day
// (among the first clientNamesPerDay) it adds one to unknown_mcp_clients,
// and the name itself stays in memory. Unknown families or metrics are
// ignored.
func (s *Store) CountClient(family, metric, name string) {
	defer func() { _ = recover() }()
	if !validClientFamily(family) || !clientRequestMetric(metric) {
		return
	}
	now := s.now()
	day := now.UTC().Format("2006-01-02")
	counts := map[string]int64{"family:" + family + ":" + metric: 1}
	if name = ClientName(name); metric == "mcp_initialize" && name != "" {
		c := &s.clients
		c.mu.Lock()
		if c.resetLocked(day) {
			if _, held := c.names[name]; held || len(c.names) < clientNamesPerDay {
				if !held {
					counts["unknown_mcp_clients"] = 1
				}
				c.names[name]++
			}
		}
		c.mu.Unlock()
	}
	s.addClientCounts(day, counts, now)
}

// countClientCommand counts one accepted command by the family in ctx:
// a new signing key (newKey: its first write created its identity), a new
// anonymous subject of the day (its /64 or /24, as anonPrefixKey), a
// subject's first post (a key's first ever, an anonymous subject's first of
// the day), service calls, and a key's return: lastWrite is the key's
// previous write (identities.last_seen, read in the command's transaction;
// 0 for none or when the command is not a key's write), so its first write
// of a day 1, 3 or 7 days after the previous one counts once, exactly, with
// no lookup. Only first_posts reads storage, at most two indexed rows. All
// but anonymous_subjects stay exact across a restart. It runs after the
// command committed and never fails it.
//
// Over /mcp the family is the request's (its User-Agent), not the one its
// initialize named: the hosted MCP transport is stateless, so no session
// carries the initialize's family to later tool calls.
func (s *Store) countClientCommand(ctx context.Context, a actor, cmd Command, source string, newKey bool, lastWrite, now int64) {
	defer func() { _ = recover() }()
	family := ClientFamilyFrom(ctx)
	if family == "" {
		return
	}
	t := time.Unix(now, 0).UTC()
	day := t.Format("2006-01-02")
	dayStart := now - now%86400
	subject := subjectOf(source)
	if a.signed {
		subject = "key:" + a.account
	}
	c := &s.clients
	c.mu.Lock()
	current := c.resetLocked(day)
	contact := current && !a.signed && c.claimLocked(subject, subjectContact)
	post := current && cmd.Operation == "post" && c.claimLocked(subject, subjectPost)
	service := ""
	if current && (cmd.Operation == "service.call" || cmd.Operation == "service.read") && validServiceKey(cmd.Target) && c.serviceLocked(family+":"+cmd.Target) {
		service = cmd.Target
	}
	c.mu.Unlock()
	counts := map[string]int64{}
	add := func(metric string) { counts["family:"+family+":"+metric]++ }
	if newKey {
		add("new_keys")
	}
	if contact {
		add("anonymous_subjects")
	}
	if cmd.Operation == "service.call" || cmd.Operation == "service.read" {
		add("service_calls")
		if service != "" {
			counts["family:"+family+":service:"+service]++
		}
	}
	if post {
		// Exact across restarts: this post is the subject's only one (a key's
		// ever, an anonymous subject's today).
		since := int64(0)
		if !a.signed {
			since = dayStart
		}
		var n int
		if s.db.QueryRowContext(ctx, "SELECT count(*) FROM (SELECT 1 FROM events WHERE account=? AND created_at>=? LIMIT 2)", a.account, since).Scan(&n) == nil && n == 1 {
			add("first_posts")
		}
	}
	if lastWrite > 0 && lastWrite < dayStart {
		switch now/86400 - lastWrite/86400 {
		case 1:
			add("returning_1d")
		case 3:
			add("returning_3d")
		case 7:
			add("returning_7d")
		}
	}
	s.addClientCounts(day, counts, t)
}

// FlushClientCounts writes the counts not yet stored. Call it at shutdown too:
// the background write runs at most every 30 seconds.
func (s *Store) FlushClientCounts() {
	defer func() { _ = recover() }()
	c := &s.clients
	c.flushMu.Lock()
	defer c.flushMu.Unlock()
	c.mu.Lock()
	batch := c.pending
	c.pending = nil
	c.mu.Unlock()
	for day, counts := range batch {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := s.AddClientCounts(ctx, day, counts)
		cancel()
		if err != nil && day >= s.now().UTC().AddDate(0, 0, -2).Format("2006-01-02") {
			// Keep a recent day's counts for a later attempt; older ones are
			// dropped, so a storage outage cannot grow memory without bound.
			c.mu.Lock()
			c.addLocked(day, counts)
			c.mu.Unlock()
		}
	}
}

// AddClientCounts adds counts for one UTC day in one transaction. Keys must
// be of the forms listed at the top of this file.
func (s *Store) AddClientCounts(ctx context.Context, day string, counts map[string]int64) error {
	if _, err := time.Parse("2006-01-02", day); err != nil || !readerDay.MatchString(day) {
		return fmt.Errorf("client counts: invalid day")
	}
	for key, n := range counts {
		if !validClientKey(key) || n < 0 {
			return fmt.Errorf("client counts: invalid key")
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	prefix := clientScopePrefix + day + ":"
	for key, n := range counts {
		if n == 0 {
			continue
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO counters(scope,value) VALUES(?,?) ON CONFLICT(scope) DO UPDATE SET value=value+excluded.value", prefix+key, n); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ClientFamilyDay is one family's published counts for one day.
type ClientFamilyDay struct {
	Counts   map[string]int64 // by ClientMetrics; absent is zero or not published
	Services map[string]int64 // accepted service calls by service, likewise
}

// ClientDay is one UTC day of arrivals by client, as published.
type ClientDay struct {
	Day string
	// Families holds only the families with a published count that day.
	Families map[string]ClientFamilyDay
	// UnknownMCPClients is how many distinct MCP client names no family
	// matched were seen that day (the names are never published).
	UnknownMCPClients int64
}

// ReadClientStats returns `days` consecutive UTC days ending with the day
// containing end, oldest first, as published: stored counts only, the
// command metrics only for days before today and only from
// ClientCountMinimum (clientPublished).
func (s *Store) ReadClientStats(ctx context.Context, end time.Time, days int) ([]ClientDay, error) {
	start, raw, err := s.readClientCounts(ctx, end, days)
	if err != nil {
		return nil, err
	}
	today := s.now().UTC().Format("2006-01-02")
	out := make([]ClientDay, days)
	for i := range out {
		day := start.AddDate(0, 0, i).Format("2006-01-02")
		out[i] = ClientDay{Day: day, Families: map[string]ClientFamilyDay{}, UnknownMCPClients: raw[i]["unknown_mcp_clients"]}
		for key, n := range raw[i] {
			parts := strings.Split(key, ":")
			if parts[0] != "family" || !clientPublished(parts[2], n, day < today) {
				continue
			}
			f, ok := out[i].Families[parts[1]]
			if !ok {
				f = ClientFamilyDay{Counts: map[string]int64{}, Services: map[string]int64{}}
				out[i].Families[parts[1]] = f
			}
			if len(parts) == 4 {
				f.Services[parts[3]] = n
			} else {
				f.Counts[parts[2]] = n
			}
		}
	}
	return out, nil
}

// clientPublished reports whether a family's count n of metric (or of
// "service") is published for a day: a request metric when nonzero, a
// command metric only for a closed day and from ClientCountMinimum.
func clientPublished(metric string, n int64, closed bool) bool {
	if clientRequestMetric(metric) {
		return n > 0
	}
	return closed && n >= ClientCountMinimum
}

// readClientCounts returns the stored counts of `days` consecutive UTC days
// ending with the day containing end, by key, and the first day.
func (s *Store) readClientCounts(ctx context.Context, end time.Time, days int) (time.Time, []map[string]int64, error) {
	if days < 1 || days > 366 {
		return time.Time{}, nil, fmt.Errorf("client stats: days out of range")
	}
	end = end.UTC().Truncate(24 * time.Hour)
	start := end.AddDate(0, 0, -(days - 1))
	raw := make([]map[string]int64, days)
	index := map[string]int{}
	for i := range raw {
		raw[i] = map[string]int64{}
		index[start.AddDate(0, 0, i).Format("2006-01-02")] = i
	}
	rows, err := s.db.QueryContext(ctx, "SELECT scope,value FROM counters WHERE scope>=? AND scope<?", clientScopePrefix+start.Format("2006-01-02"), clientScopePrefix+end.Format("2006-01-02")+";")
	if err != nil {
		return start, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var scope string
		var value int64
		if err = rows.Scan(&scope, &value); err != nil {
			return start, nil, err
		}
		day, key, ok := strings.Cut(strings.TrimPrefix(scope, clientScopePrefix), ":")
		if i, known := index[day]; known && ok && validClientKey(key) {
			raw[i][key] += value
		}
	}
	return start, raw, rows.Err()
}
