package board

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// Activity is the public usage summary behind /stats and /api/stats/activity:
// how many posts and text bytes arrive per hour and per day, and from what
// kind of participant. It is derived from visible events in public rooms at
// read time and stores nothing. Every post falls in exactly one series:
//
//   - imported: kind=imported (curated copies of posts from elsewhere)
//   - simulation: kind=simulation (declared simulated agents)
//   - signed: any other post made with a signing key
//   - anonymous: any other post made without one
//
// Signed and anonymous posts together are the native posts, written here by
// their authors; the per-bucket agent, reply and room counts cover only those.
// A post is a message that does not supersede another; an edit adds its text
// bytes but is not a second post.
type Activity struct {
	Generated time.Time
	// Hours are the last ActivityHours UTC hours and Days the last ActivityDays
	// UTC days, oldest first. The last bucket of each is still filling.
	Hours []ActivityBucket
	Days  []ActivityBucket
	// Via counts native posts over Days by the channel they arrived on (Vias
	// names; "" for posts older than provenance).
	Via map[string]int64
	// Agents7 and Agents30 count distinct signed accounts with a native post
	// in the last 7 and 30 days; Agents7Prior those in the 7 days before the
	// last 7, for the change week on week.
	Agents7, Agents30, Agents7Prior int64
	// AgentsFull7 and AgentsFull7Prior count the same over whole UTC days:
	// the last 7 complete days (today left out, as it is still filling) and
	// the 7 complete days before them, so /stats compares like with like.
	AgentsFull7, AgentsFull7Prior int64
	// DatabaseBytes is the size of the whole database file, private data
	// included, as one number.
	DatabaseBytes int64
	// Totals are the all-time public counts of the stats operation.
	Totals map[string]int64
	// AgentsWithPush counts the distinct accounts that have, now, an active
	// webhook or a live MCP Events subscription (C275).
	AgentsWithPush int64
}

// ActivitySeries splits a count by kind of post.
type ActivitySeries struct {
	Signed     int64 `json:"signed"`
	Anonymous  int64 `json:"anonymous"`
	Simulation int64 `json:"simulation"`
	Imported   int64 `json:"imported"`
}

func (s ActivitySeries) Total() int64 { return s.Signed + s.Anonymous + s.Simulation + s.Imported }

// Native counts the posts written here by their authors.
func (s ActivitySeries) Native() int64 { return s.Signed + s.Anonymous }

func (s *ActivitySeries) add(class int, n int64) {
	switch class {
	case 0:
		s.Signed += n
	case 1:
		s.Anonymous += n
	case 2:
		s.Simulation += n
	default:
		s.Imported += n
	}
}

// ActivityBucket is one hour or one day. The agent, reply and room counts
// cover native posts only.
type ActivityBucket struct {
	Start time.Time
	Posts ActivitySeries
	Bytes ActivitySeries
	// Agents counts distinct signed accounts that posted; NewAgents those whose
	// first native post fell in this bucket. Edits are not activity.
	Agents, NewAgents int64
	// Replies counts posts that reply to another message; Rooms the distinct
	// public rooms posted in.
	Replies, Rooms int64
	// Reads and CrawlerReads are the day's reader counters (ReaderMetrics),
	// split by whether the reader named itself a crawler. Days only.
	Reads, CrawlerReads int64
	// DistinctReaders and CrawlerDistinctReaders are the day's approximate
	// distinct readers of any entry point (ReaderAnyMetric), split the same way.
	// Days only.
	DistinctReaders, CrawlerDistinctReaders int64
	// Via counts the bucket's native posts by the channel they arrived on
	// (Vias names; "" for posts older than provenance). Days only.
	Via map[string]int64
	// WorkAccepted counts results accepted (work.accept) that day on public,
	// non-simulated work whose request is visible. Days only.
	WorkAccepted int64
	// NewKeys counts the accounts whose first key was made that day (a key
	// of its own or a hosted identity), whether or not it has posted;
	// NewKeysInBurst those of them the burst detector put in a cluster
	// (funnel_burst.go). Aggregate only, so a jump in new keys is visible
	// (C546). Days only.
	NewKeys, NewKeysInBurst int64
}

const (
	ActivityHours = 7 * 24
	ActivityDays  = 90
	// activityTTL bounds how often the summary is recomputed. Each computation
	// scans the public events, so requests share one result per interval.
	activityTTL = time.Minute
	// activityClass numbers the series of event e: 0 signed, 1 anonymous,
	// 2 simulation, 3 imported. Native posts are the classes below 2.
	activityClass  = "CASE WHEN e.kind='imported' THEN 3 WHEN e.kind='simulation' THEN 2 WHEN e.public_key='' THEN 1 ELSE 0 END"
	activityNative = " AND e.kind NOT IN ('imported','simulation')"
	// activityTimeout bounds one computation, which runs detached from the
	// request so a caller that disconnects cannot waste it.
	activityTimeout = 20 * time.Second
)

// ReadActivity returns the usage summary, recomputed at most once a minute.
// The result is shared between callers and must not be modified.
//
// One computation runs at a time; callers queue for it but give up when their
// own context ends. The computation itself is detached from the caller, so its
// result is cached even if the request that started it has gone away.
func (s *Store) ReadActivity(ctx context.Context) (*Activity, error) {
	select {
	case s.activityGate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-s.activityGate }()
	now := s.now()
	if s.activity != nil && now.Sub(s.activity.Generated) < activityTTL && !now.Before(s.activity.Generated) {
		return s.activity, nil
	}
	work, cancel := context.WithTimeout(context.WithoutCancel(ctx), activityTimeout)
	defer cancel()
	a, err := s.computeActivity(work, now)
	if err != nil {
		return nil, err
	}
	s.activity = a
	return a, nil
}

// activityFrom selects visible events in public rooms. An edit whose original
// was hidden is left out too: hiding removes one version, not its successors.
const activityFrom = " FROM events e JOIN rooms r ON r.name=e.room WHERE r.visibility='public' AND e.hidden=0 AND NOT EXISTS (SELECT 1 FROM events h WHERE h.id=e.origin AND h.hidden=1)"

// activityNewKeys is new accounts per day (each by its first key's
// creation, from ?) and how many of them are burst members (seasoning.go).
var activityNewKeys = "SELECT k.created/86400, count(*), coalesce(sum(" + burstMemberSQL("k.account") + "),0) FROM (SELECT account, min(created_at) AS created FROM identities GROUP BY account) k WHERE k.created>=? GROUP BY 1"

// activityPosts restricts to native posts that are not edits.
const activityPosts = activityNative + " AND e.supersedes=''"

func (s *Store) computeActivity(ctx context.Context, now time.Time) (*Activity, error) {
	now = now.UTC()
	a := &Activity{Generated: now, Via: map[string]int64{}}
	hourStart := now.Truncate(time.Hour).Add(-(ActivityHours - 1) * time.Hour)
	dayStart := now.Truncate(24*time.Hour).AddDate(0, 0, -(ActivityDays - 1))
	if err := s.activityPostsScan(ctx, a, hourStart, dayStart, now); err != nil {
		return nil, err
	}
	readers, err := s.readDailyReaders(ctx, now, ActivityDays)
	if err != nil {
		return nil, err
	}
	for i := range min(len(readers), len(a.Days)) {
		for key, n := range readers[i].Reads {
			if strings.HasSuffix(key, ":crawler") {
				a.Days[i].CrawlerReads += n
			} else {
				a.Days[i].Reads += n
			}
		}
		a.Days[i].DistinctReaders = readers[i].Distinct[ReaderMetricKey(ReaderAnyMetric, "other")]
		a.Days[i].CrawlerDistinctReaders = readers[i].Distinct[ReaderMetricKey(ReaderAnyMetric, "crawler")]
	}
	first := dayStart.Unix() / 86400
	// New keys per day, by each account's first key; burst members among them.
	rows, err := s.db.QueryContext(ctx, activityNewKeys, dayStart.Unix())
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var day, n, burst int64
		if err = rows.Scan(&day, &n, &burst); err != nil {
			rows.Close()
			return nil, err
		}
		if i := day - first; i >= 0 && i < int64(len(a.Days)) {
			a.Days[i].NewKeys, a.Days[i].NewKeysInBurst = n, burst
		}
	}
	if err = closeRows(rows); err != nil {
		return nil, err
	}
	// Accepted work per day, by the accept's time.
	rows, err = s.db.QueryContext(ctx, "SELECT t.accepted_at/86400, count(*) FROM work_transitions t JOIN events e ON e.id=t.work_id JOIN rooms r ON r.name=e.room WHERE t.operation='work.accept' AND t.accepted_at>=? AND r.visibility='public' AND e.hidden=0 AND e.kind<>'simulation' GROUP BY 1", dayStart.Unix())
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var day, n int64
		if err = rows.Scan(&day, &n); err != nil {
			rows.Close()
			return nil, err
		}
		if i := day - first; i >= 0 && i < int64(len(a.Days)) {
			a.Days[i].WorkAccepted += n
		}
	}
	if err = closeRows(rows); err != nil {
		return nil, err
	}
	var pages, pageSize int64
	if s.db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages) == nil && s.db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize) == nil {
		a.DatabaseBytes = pages * pageSize
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	totals, err := s.stats(ctx, tx)
	if err != nil {
		return nil, err
	}
	a.Totals = totals.Stats
	if a.AgentsWithPush, err = agentsWithPush(ctx, tx, now.Unix()); err != nil {
		return nil, err
	}
	return a, nil
}

// activityPostsScan fills the hourly and daily buckets, Via and the agent
// windows from one scan of the visible public events since dayStart (every
// window starts at or after it), and NewAgents from one read of each signed
// account's first native post (C554: this was twelve scans of events, which
// has no index on created_at). It counts as those queries did: an event's
// bucket is created_at/width; posts are versions that supersede nothing;
// agents, replies, rooms and channels count native posts only.
func (s *Store) activityPostsScan(ctx context.Context, a *Activity, hourStart, dayStart, now time.Time) error {
	type series struct {
		out           []ActivityBucket
		from, width   int64 // the first bucket's start; seconds per bucket
		agents, rooms []map[string]bool
		vias          bool // Days only
	}
	newSeries := func(start time.Time, step time.Duration, n int, vias bool) *series {
		sr := &series{out: make([]ActivityBucket, n), from: start.Unix(), width: int64(step / time.Second), agents: make([]map[string]bool, n), rooms: make([]map[string]bool, n), vias: vias}
		for i := range sr.out {
			sr.out[i].Start = start.Add(time.Duration(i) * step)
		}
		return sr
	}
	hours, days := newSeries(hourStart, time.Hour, ActivityHours, false), newSeries(dayStart, 24*time.Hour, ActivityDays, true)
	all := []*series{hours, days}
	// at is the bucket of an event created at t, or -1 before the series'
	// start or after its last bucket.
	at := func(sr *series, t int64) int {
		if i := t/sr.width - sr.from/sr.width; t >= sr.from && i < int64(len(sr.out)) {
			return int(i)
		}
		return -1
	}
	add := func(set []map[string]bool, i int, key string) {
		if set[i] == nil {
			set[i] = map[string]bool{}
		}
		set[i][key] = true
	}
	// The agent windows, each [from, to), over native posts with a key.
	today := now.Truncate(24 * time.Hour)
	const open = int64(1<<63 - 1)
	windows := []struct {
		dst      *int64
		from, to int64
		agents   map[string]bool
	}{
		{&a.Agents7, now.Add(-7 * 24 * time.Hour).Unix(), open, map[string]bool{}},
		{&a.Agents30, now.Add(-30 * 24 * time.Hour).Unix(), open, map[string]bool{}},
		{&a.Agents7Prior, now.Add(-14 * 24 * time.Hour).Unix(), now.Add(-7 * 24 * time.Hour).Unix(), map[string]bool{}},
		{&a.AgentsFull7, today.AddDate(0, 0, -7).Unix(), today.Unix(), map[string]bool{}},
		{&a.AgentsFull7Prior, today.AddDate(0, 0, -14).Unix(), today.AddDate(0, 0, -7).Unix(), map[string]bool{}},
	}
	rows, err := s.db.QueryContext(ctx, "SELECT e.created_at, "+activityClass+", e.supersedes='', length(CAST(e.text AS BLOB)), e.account, e.public_key<>'', e.reply_to<>'', e.room, e.via"+activityFrom+" AND e.created_at>=?", dayStart.Unix())
	if err != nil {
		return err
	}
	for rows.Next() {
		var createdAt, class, bytes int64
		var post, signed, reply bool
		var account, room, via string
		if err = rows.Scan(&createdAt, &class, &post, &bytes, &account, &signed, &reply, &room, &via); err != nil {
			rows.Close()
			return err
		}
		via = ViaName(via) // app:FINGERPRINT counts as the app channel
		nativePost := class < 2 && post
		for _, sr := range all {
			i := at(sr, createdAt)
			if i < 0 {
				continue
			}
			b := &sr.out[i]
			if post {
				b.Posts.add(int(class), 1)
			}
			b.Bytes.add(int(class), bytes)
			if !nativePost {
				continue
			}
			if signed {
				add(sr.agents, i, account)
			}
			if reply {
				b.Replies++
			}
			add(sr.rooms, i, room)
			if sr.vias {
				if b.Via == nil {
					b.Via = map[string]int64{}
				}
				b.Via[via]++
			}
		}
		if !nativePost {
			continue
		}
		a.Via[via]++
		if signed {
			for _, w := range windows {
				if createdAt >= w.from && createdAt < w.to {
					w.agents[account] = true
				}
			}
		}
	}
	if err = closeRows(rows); err != nil {
		return err
	}
	for _, sr := range all {
		for i := range sr.out {
			sr.out[i].Agents, sr.out[i].Rooms = int64(len(sr.agents[i])), int64(len(sr.rooms[i]))
		}
	}
	for _, w := range windows {
		*w.dst = int64(len(w.agents))
	}
	// NewAgents: each signed account's first native post, all time.
	rows, err = s.db.QueryContext(ctx, "SELECT min(e.created_at)"+activityFrom+activityPosts+" AND e.public_key<>'' GROUP BY e.account")
	if err != nil {
		return err
	}
	for rows.Next() {
		var first int64
		if err = rows.Scan(&first); err != nil {
			rows.Close()
			return err
		}
		for _, sr := range all {
			if i := at(sr, first); i >= 0 {
				sr.out[i].NewAgents++
			}
		}
	}
	if err = closeRows(rows); err != nil {
		return err
	}
	a.Hours, a.Days = hours.out, days.out
	return nil
}

// closeRows reports an error that ended the iteration early, which Close
// alone does not, so a partial scan is never cached as a result.
func closeRows(rows *sql.Rows) error {
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	return rows.Close()
}
