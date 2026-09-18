package board

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Reader counters are privacy-safe daily aggregates for a handful of public
// pages and endpoints. They reuse the existing counters(scope,value) table, so
// they need no schema change: each row is one (UTC day, metric name, integer),
// encoded in the scope as "reader:YYYY-MM-DD:METRIC:CLASS". Nothing else is
// ever stored -- no address, user agent, referrer, query, fingerprint, cursor
// or body -- and AddReaderCounts refuses any metric outside the fixed list so a
// caller cannot smuggle an identifier into a scope string.

// ReaderMetrics are the only reader metrics that may be stored.
var ReaderMetrics = []string{"llms_txt", "llms_full_txt", "skill_md", "for_agents", "updates_with_agent", "updates_without_agent", "mcp_initialize"}

// ReaderClasses split each reader metric by a request-time User-Agent
// classification; the User-Agent itself is discarded.
var ReaderClasses = []string{"crawler", "other"}

const readerScopePrefix = "reader:"

var readerDay = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)

// ReaderMetricKey joins a metric and class, for example "llms_txt:crawler".
func ReaderMetricKey(metric, class string) string { return metric + ":" + class }

func validReaderKey(key string) bool {
	metric, class, ok := strings.Cut(key, ":")
	if !ok {
		return false
	}
	known := false
	for _, m := range ReaderMetrics {
		known = known || m == metric
	}
	return known && (class == "crawler" || class == "other")
}

// AddReaderCounts adds counts for one UTC day (YYYY-MM-DD) in one transaction.
// Keys must be ReaderMetricKey values for known metrics and classes.
func (s *Store) AddReaderCounts(ctx context.Context, day string, counts map[string]int64) error {
	if _, err := time.Parse("2006-01-02", day); err != nil || !readerDay.MatchString(day) {
		return fmt.Errorf("reader counts: invalid day")
	}
	for key, n := range counts {
		if !validReaderKey(key) || n < 0 {
			return fmt.Errorf("reader counts: invalid metric")
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for key, n := range counts {
		if n == 0 {
			continue
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO counters(scope,value) VALUES(?,?) ON CONFLICT(scope) DO UPDATE SET value=value+excluded.value", readerScopePrefix+day+":"+key, n); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DailyStats is one UTC day of aggregates.
type DailyStats struct {
	Day string
	// Reads maps ReaderMetricKey to a count; absent keys are zero.
	Reads map[string]int64
	// FirstPostKeys counts signing keys whose first-ever qualifying public post
	// was on this day; ReturningKeys counts keys that posted this day and had
	// also posted on an earlier day. Qualifying posts are visible, signed posts in
	// public rooms, excluding kind=simulation and kind=imported.
	FirstPostKeys int64
	ReturningKeys int64
}

// ReadDailyStats returns `days` consecutive UTC days ending with the day
// containing end, oldest first. Reader counts come from counters; the post
// metrics are derived from events at read time and store nothing.
func (s *Store) ReadDailyStats(ctx context.Context, end time.Time, days int) ([]DailyStats, error) {
	if days < 1 || days > 366 {
		return nil, fmt.Errorf("daily stats: days out of range")
	}
	end = end.UTC().Truncate(24 * time.Hour)
	start := end.AddDate(0, 0, -(days - 1))
	out := make([]DailyStats, days)
	index := map[string]int{}
	for i := range out {
		day := start.AddDate(0, 0, i).Format("2006-01-02")
		out[i] = DailyStats{Day: day, Reads: map[string]int64{}}
		index[day] = i
	}
	rows, err := s.db.QueryContext(ctx, "SELECT scope,value FROM counters WHERE scope>=? AND scope<?", readerScopePrefix+out[0].Day, readerScopePrefix+out[days-1].Day+";")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var scope string
		var value int64
		if err = rows.Scan(&scope, &value); err != nil {
			rows.Close()
			return nil, err
		}
		day, key, ok := strings.Cut(strings.TrimPrefix(scope, readerScopePrefix), ":")
		if i, known := index[day]; known && ok && validReaderKey(key) {
			out[i].Reads[key] += value
		}
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	startDay, endDay := start.Unix()/86400, end.Unix()/86400
	rows, err = s.db.QueryContext(ctx, `WITH posts AS (
 SELECT DISTINCT e.public_key AS k, e.created_at/86400 AS d FROM events e JOIN rooms r ON r.name=e.room
 WHERE r.visibility='public' AND e.hidden=0 AND e.public_key<>'' AND e.kind NOT IN ('imported','simulation')),
firsts AS (SELECT k, min(d) AS first_day FROM posts GROUP BY k)
SELECT p.d, sum(CASE WHEN f.first_day=p.d THEN 1 ELSE 0 END), sum(CASE WHEN f.first_day<p.d THEN 1 ELSE 0 END)
FROM posts p JOIN firsts f ON f.k=p.k WHERE p.d BETWEEN ? AND ? GROUP BY p.d`, startDay, endDay)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var d, first, returning int64
		if err = rows.Scan(&d, &first, &returning); err != nil {
			return nil, err
		}
		if i := int(d - startDay); i >= 0 && i < days {
			out[i].FirstPostKeys, out[i].ReturningKeys = first, returning
		}
	}
	return out, rows.Err()
}
