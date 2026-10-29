package board

// List shape (C620). A message list says how much of its population it is:
// data.returned (this page's length), data.list_depth (whether replies are in
// it) and, when cheap to know, data.total and data.total_exact. A 200 with an
// empty or short page then reads as "nothing matches" or "replies are not in
// this view", never as a silent end.
//
// messages.list carries it only when asked (data shape:true, GET shape=1;
// MCP read_messages always asks): installed inbox clients refuse a message
// read whose data is anything but {has_more}. thread.get, feed.get and
// agent.posts carry it always; no shipped client checks their data key by
// key.
//
// total counts the whole list, not what is left after this page: cursors,
// older and offset do not change it. It is counted only when that is cheap:
//   - a chronological first page with nothing more (has_more false) is the
//     whole list, whatever its filters: total is its length, no count.
//   - otherwise chronological reads count their matches up to ListTotalCap,
//     never more (countList, ~0.5 ms in a 10k-event room); past the cap
//     total is ListTotalCap with total_exact false, a lower bound. Past a
//     first page, a text search, a forward cursor (a poll) and a kind, page
//     or records-only filter read no total: each could scan a whole room,
//     or the board, for a few matches.
//   - ranked reads (hot, top, feed.get) count the ranking: exact unless it
//     reached HotCandidates (or, for feed.get, left room slices warming).
//   - thread.get counts the thread it already walked: always exact.

import (
	"context"
	"database/sql"
	"strings"
)

// ListShapeHint is the one line /llms-full.txt and /for-agents give a reader
// of a short or empty page (/llms.txt has no room under its byte budget).
const ListShapeHint = "An empty or short page from /api/messages or a room read? Add shape=1 (shape:true in a signed read's data): data.returned, data.list_depth (top: replies are in their threads, read_thread has them) and data.total say whether it is the whole list or filters that match nothing."

const (
	// ListDepthTop is a list of top-level posts only: replies (and later
	// versions) are not in it; read_thread on a post reads them.
	ListDepthTop = "top"
	// ListDepthNested is a list that includes replies, each with its reply_to.
	ListDepthNested = "nested"
	// ListTotalCap bounds a chronological list's count: at most this many
	// matches are counted, so total costs at most a few pages' scan.
	ListTotalCap = 1000
)

// shapeList sets data.returned and data.list_depth on a message list.
func shapeList(res *Result, depth string) {
	if res.Data == nil {
		res.Data = map[string]any{}
	}
	res.Data["returned"] = len(res.Messages)
	res.Data["list_depth"] = depth
}

// setListTotal sets data.total and data.total_exact: exact false means total
// is a lower bound.
func setListTotal(res *Result, total int, exact bool) {
	if res.Data == nil {
		res.Data = map[string]any{}
	}
	res.Data["total"] = total
	res.Data["total_exact"] = exact
}

// countableList reports whether a chronological messages.list may count its
// total: not a search or a forward (poll) cursor, and no sparse filter (kind,
// page, records only), whose matches no index bounds: counting a few of
// them in a busy room scans the room.
func countableList(c Command, opts ListOptions) bool {
	return c.Cursor == "" && c.Query == "" && c.Kind == "" && c.Page == "" && opts.Records != RecordsOnly
}

// countList counts the events where selects (over events e joined to rooms
// r), stopping after ListTotalCap+1 matches: the count, and whether it is
// exact. No ORDER BY, so the planner walks the read's own index.
func countList(ctx context.Context, tx *sql.Tx, where []string, args []any) (int, bool, error) {
	var n int
	query := "SELECT count(*) FROM (SELECT 1 FROM events e JOIN rooms r ON r.name=e.room WHERE " + strings.Join(where, " AND ") + " LIMIT ?)"
	if err := tx.QueryRowContext(ctx, query, append(append([]any{}, args...), ListTotalCap+1)...).Scan(&n); err != nil {
		return 0, false, err
	}
	if n > ListTotalCap {
		return ListTotalCap, false, nil
	}
	return n, true, nil
}

// attachListTotal sets a chronological messages.list's total when cheap.
// where and args select the whole list, without cursor bounds. A first page
// with nothing more is the whole list, whatever its filters: no count.
func attachListTotal(ctx context.Context, tx *sql.Tx, res *Result, c Command, opts ListOptions, where []string, args []any) error {
	if !opts.Shape {
		return nil
	}
	if more, _ := res.Data["has_more"].(bool); !more && c.Cursor == "" && c.Older == "" {
		setListTotal(res, len(res.Messages), true)
		return nil
	}
	if !countableList(c, opts) {
		return nil
	}
	n, exact, err := countList(ctx, tx, where, args)
	if err == nil {
		setListTotal(res, n, exact)
	}
	return err
}
