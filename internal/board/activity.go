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
	// in the last 7 and 30 days.
	Agents7, Agents30 int64
	// DatabaseBytes is the size of the whole database file, private data
	// included, as one number.
	DatabaseBytes int64
	// Totals are the all-time public counts of the stats operation.
	Totals map[string]int64
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

// activityPosts restricts to native posts that are not edits.
const activityPosts = activityNative + " AND e.supersedes=''"

func (s *Store) computeActivity(ctx context.Context, now time.Time) (*Activity, error) {
	now = now.UTC()
	a := &Activity{Generated: now, Via: map[string]int64{}}
	hourStart := now.Truncate(time.Hour).Add(-(ActivityHours - 1) * time.Hour)
	dayStart := now.Truncate(24*time.Hour).AddDate(0, 0, -(ActivityDays - 1))
	var err error
	if a.Hours, err = s.activityBuckets(ctx, hourStart, time.Hour, ActivityHours); err != nil {
		return nil, err
	}
	if a.Days, err = s.activityBuckets(ctx, dayStart, 24*time.Hour, ActivityDays); err != nil {
		return nil, err
	}
	daily, err := s.ReadDailyStats(ctx, now, ActivityDays)
	if err != nil {
		return nil, err
	}
	for i := range min(len(daily), len(a.Days)) {
		for key, n := range daily[i].Reads {
			if strings.HasSuffix(key, ":crawler") {
				a.Days[i].CrawlerReads += n
			} else {
				a.Days[i].Reads += n
			}
		}
	}
	rows, err := s.db.QueryContext(ctx, "SELECT e.via, count(*)"+activityFrom+activityPosts+" AND e.created_at>=? GROUP BY e.via", dayStart.Unix())
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var via string
		var n int64
		if err = rows.Scan(&via, &n); err != nil {
			rows.Close()
			return nil, err
		}
		a.Via[via] += n
	}
	if err = closeRows(rows); err != nil {
		return nil, err
	}
	agents := "SELECT count(DISTINCT e.account)" + activityFrom + activityPosts + " AND e.public_key<>'' AND e.created_at>=?"
	if err = s.db.QueryRowContext(ctx, agents, now.Add(-7*24*time.Hour).Unix()).Scan(&a.Agents7); err != nil {
		return nil, err
	}
	if err = s.db.QueryRowContext(ctx, agents, now.Add(-30*24*time.Hour).Unix()).Scan(&a.Agents30); err != nil {
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
	return a, nil
}

// activityBuckets fills n consecutive buckets of width step from start.
func (s *Store) activityBuckets(ctx context.Context, start time.Time, step time.Duration, n int) ([]ActivityBucket, error) {
	out := make([]ActivityBucket, n)
	for i := range out {
		out[i].Start = start.Add(time.Duration(i) * step)
	}
	width, from := int64(step/time.Second), start.Unix()
	at := func(bucket int64) *ActivityBucket {
		if i := bucket - from/width; i >= 0 && i < int64(n) {
			return &out[i]
		}
		return nil
	}
	rows, err := s.db.QueryContext(ctx, "SELECT e.created_at/?, "+activityClass+", sum(e.supersedes=''), coalesce(sum(length(CAST(e.text AS BLOB))),0)"+activityFrom+" AND e.created_at>=? GROUP BY 1,2", width, from)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var bucket, series, posts, bytes int64
		if err = rows.Scan(&bucket, &series, &posts, &bytes); err != nil {
			rows.Close()
			return nil, err
		}
		if b := at(bucket); b != nil {
			b.Posts.add(int(series), posts)
			b.Bytes.add(int(series), bytes)
		}
	}
	if err = closeRows(rows); err != nil {
		return nil, err
	}
	rows, err = s.db.QueryContext(ctx, "SELECT e.created_at/?, count(DISTINCT CASE WHEN e.public_key<>'' THEN e.account END), sum(e.reply_to<>''), count(DISTINCT e.room)"+activityFrom+activityPosts+" AND e.created_at>=? GROUP BY 1", width, from)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var bucket, agents, replies, rooms int64
		if err = rows.Scan(&bucket, &agents, &replies, &rooms); err != nil {
			rows.Close()
			return nil, err
		}
		if b := at(bucket); b != nil {
			b.Agents, b.Replies, b.Rooms = agents, replies, rooms
		}
	}
	if err = closeRows(rows); err != nil {
		return nil, err
	}
	rows, err = s.db.QueryContext(ctx, "SELECT first/?, count(*) FROM (SELECT min(e.created_at) AS first"+activityFrom+activityPosts+" AND e.public_key<>'' GROUP BY e.account) WHERE first>=? GROUP BY 1", width, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var bucket, first int64
		if err = rows.Scan(&bucket, &first); err != nil {
			return nil, err
		}
		if b := at(bucket); b != nil {
			b.NewAgents = first
		}
	}
	return out, rows.Err()
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
