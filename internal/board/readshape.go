package board

import (
	"context"
	"database/sql"
	"log/slog"
	"strings"
	"time"
)

// slowRead is how long one readEvents page query may hold the single
// connection before it is logged (C688).
const slowRead = time.Second

// resolveStrings reads a one-column query's values, resolved before a page
// query so the page can walk one index in order (C688).
func resolveStrings(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// inTerm is col matching one of vals: col=? for one value (an ordered index
// walk), col IN (...) for several, and false for none.
func inTerm(col string, vals []string) (string, []any) {
	args := make([]any, len(vals))
	for i, v := range vals {
		args[i] = v
	}
	switch len(vals) {
	case 0:
		return "0", nil
	case 1:
		return col + "=?", args
	}
	return col + " IN (" + strings.TrimSuffix(strings.Repeat("?,", len(vals)), ",") + ")", args
}

// shapeTermMax bounds each term of a where shape.
const shapeTermMax = 60

// whereShape names a page query's filters for a log line: each where term
// as written, whitespace folded and cut to shapeTermMax bytes. Terms hold
// only columns, operators, constants and ? placeholders, never a value.
func whereShape(where []string, order string) string {
	terms := make([]string, len(where))
	for i, w := range where {
		w = strings.Join(strings.Fields(w), " ")
		if len(w) > shapeTermMax {
			w = w[:shapeTermMax] + "..."
		}
		terms[i] = w
	}
	return strings.Join(terms, " AND ") + " ORDER BY e.seq " + order
}

// logSlowRead logs a page query that held the connection for slowRead or
// longer: its operation, where shape and duration, never its arguments.
func logSlowRead(operation string, where []string, order string, took time.Duration) {
	if took < slowRead {
		return
	}
	slog.Warn("slow read", "operation", operation, "shape", whereShape(where, order), "duration_ms", took.Milliseconds())
}
