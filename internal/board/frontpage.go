package board

// The front page: like a subreddit left out of r/all, a room can stay off the
// default all-rooms feed while remaining fully readable on its own (?room=),
// and through the explicit all-rooms read (scope=all). The setting is the
// room policy's front_page. Unset, it follows the built-in default: every
// public room is on the front page, including a new one made by its first
// post, except the utility rooms of FrontPageOffByDefault and personal rooms.
// A room's owner or moderators may take their room off; its owner may put it
// back to the default (front_page null, or true while the default is on);
// only the operator puts on a room whose default is off, and only the
// operator reverses the operator's own opt-out. /api/updates, threads,
// exports and the live stream are not front pages and never filter by it.
//
// Reads of the front page walk partial indexes over the rooms on it by
// default (events_front1, events_front_top1), so a flood of posts into a room
// off the front page costs those reads nothing.

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"sort"
	"strings"
)

// FrontPageOffByDefault are the utility rooms the default feed leaves out
// until the operator says otherwise: work for hire, tests, board listings
// and trade.
var FrontPageOffByDefault = []string{"bounties", "sandbox", "boards", "commerce"}

// AllRooms is the messages.list data of a read that wants every public room,
// not the front page: the live stream, the DNS head and the Nostr bridge.
const AllRooms = `{"scope":"all"}`

// Stored front_page values; "" follows the default. frontPageOperatorOff is
// the operator's opt-out, which the room's owner cannot reverse.
const (
	frontPageOn          = "on"
	frontPageOff         = "off"
	frontPageOperatorOff = "operator_off"
)

// frontPageDefault is whether a room with no front_page setting is on the
// front page.
func frontPageDefault(room string) bool {
	if _, personal := PersonalOwner(room); personal {
		return false
	}
	for _, name := range FrontPageOffByDefault {
		if room == name {
			return false
		}
	}
	return true
}

// frontPage is a room's effective setting from its stored value.
func frontPage(room, stored string) bool {
	switch stored {
	case frontPageOn:
		return true
	case frontPageOff, frontPageOperatorOff:
		return false
	}
	return frontPageDefault(room)
}

// frontPageSQL is true for a room (joined as r) on the front page: the rule of
// frontPage in SQL, one primary-key lookup per row.
func frontPageSQL() string {
	return `CASE coalesce((SELECT fp.front_page FROM room_policies fp WHERE fp.room=r.name),'') WHEN 'on' THEN 1 WHEN 'off' THEN 0 WHEN 'operator_off' THEN 0
 ELSE (` + frontNameTerms("r.name") + `) END`
}

// frontNameTerms is frontPageDefault in SQL over the room column col. The
// partial indexes below and the reads that name them spell it identically,
// since SQLite matches a partial index's WHERE term by term.
func frontNameTerms(col string) string {
	quoted := make([]string, len(FrontPageOffByDefault))
	for i, name := range FrontPageOffByDefault {
		quoted[i] = "'" + name + "'" // slugs: no quote can occur
	}
	return "substr(" + col + ",1,1)<>'@' AND " + col + " NOT IN (" + strings.Join(quoted, ",") + ")"
}

// rankableTerms select the posts a ranked view can show: visible top-level
// originals of the default kinds (defaultFeedKinds).
const rankableTerms = "reply_to='' AND supersedes='' AND hidden=0 AND kind NOT IN ('simulation','imported')"

// The read indexes, created at startup when missing (additive; no schema
// version). The front-page ones carry FrontPageOffByDefault in their WHERE:
// changing that list needs new index names, or the reads stop using them.
//   - events_front1: every event of a room on the front page by default, for
//     the chronological front page (readFront).
//   - events_front_top1 and events_top1: the rankable posts of the front page
//     and of every room, the hot and top views' candidate walks (rankSource).
//   - room_policies_front: the rooms the operator put on or took off.
var frontIndexes = `
CREATE INDEX IF NOT EXISTS events_front1 ON events(seq) WHERE ` + frontNameTerms("room") + `;
CREATE INDEX IF NOT EXISTS events_front_top1 ON events(seq) WHERE ` + rankableTerms + ` AND ` + frontNameTerms("room") + `;
CREATE INDEX IF NOT EXISTS events_top1 ON events(seq) WHERE ` + rankableTerms + `;
CREATE INDEX IF NOT EXISTS room_policies_front ON room_policies(front_page);
`

// migrateReadIndexes creates the read indexes and the ranking's flag table.
// It runs after the column migrations they depend on (supersedes, front_page).
func migrateReadIndexes(tx *sql.Tx) error {
	_, err := tx.Exec(flagSchema + frontIndexes)
	return err
}

// frontOnRooms are the rooms whose default is off that the operator put on
// the front page: the reads add them to the partial indexes' walk, one
// indexed walk each. Only the operator stores "on", so the list is short;
// frontOnRoomsMax bounds it anyway.
const frontOnRoomsMax = 64

func frontOnRooms(ctx context.Context, tx *sql.Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx, "SELECT room FROM room_policies WHERE front_page=? ORDER BY room LIMIT ?", frontPageOn, 4*frontOnRoomsMax)
	if err != nil {
		return nil, err
	}
	var rooms []string
	for rows.Next() {
		var room string
		if err = rows.Scan(&room); err != nil {
			rows.Close()
			return nil, err
		}
		if !frontPageDefault(room) && len(rooms) < frontOnRoomsMax {
			rooms = append(rooms, room)
		}
	}
	return rooms, closeRows(rows)
}

// frontPageFeed reports whether a messages.list read is the default all-rooms
// feed, which shows only front-page rooms: no room, recipient, author, search
// or kind, and no scope=all.
func frontPageFeed(c Command, o ListOptions) bool {
	return c.Operation == "messages.list" && c.Room == "" && c.To == "" && c.Target == "" && c.Query == "" && c.Kind == "" && o.Scope != "all"
}

// frontPageOptOut reports whether data is exactly {"front_page":false}: the
// one policy change a room's moderators may make.
func frontPageOptOut(data string) bool {
	var in map[string]json.RawMessage
	return json.Unmarshal([]byte(data), &in) == nil && len(in) == 1 && string(in["front_page"]) == "false"
}

// The chronological front page walks events_front1, the events of rooms on
// the front page by default, in chunks: frontChunkFirst entries (or four
// pages) first, eight times more each time a chunk leaves the page short, and
// at most FrontScanRows entries per read. A flood into a room off the front
// page by default is not in the index and costs nothing; one into a room
// taken off it, or a private room the reader cannot see, costs a bounded
// walk, after which a cursor read resumes past what was walked.
const (
	frontChunkFirst = 256
	FrontScanRows   = 20000
)

// readFront is the chronological front page: where and args select what the
// reader may read and include frontPageSQL; cursorSeq is the cursor's position
// (a cursor reads forward from it, no cursor reads the newest). The rooms the
// operator put on the front page are read on their own (events_room_seq) and
// merged in.
func (s *Store) readFront(ctx context.Context, tx *sql.Tx, c Command, where []string, args []any, cursorSeq int64, now int64, newest bool) (Result, error) {
	order, cmp, agg, bound := "DESC", "<", "min", int64(math.MaxInt64)
	if c.Older != "" {
		bound = cursorSeq
	}
	if c.Cursor != "" {
		order, cmp, agg, bound = "ASC", ">", "max", cursorSeq
	}
	limit := limitValue(c.Limit)
	cond := strings.Join(where, " AND ")
	scan := func(query string, qargs []any) ([]Message, error) {
		rows, err := tx.QueryContext(ctx, query, qargs...)
		if err != nil {
			return nil, err
		}
		var out []Message
		for rows.Next() {
			e, err := scanEvent(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, e)
		}
		return out, closeRows(rows)
	}
	inner := "SELECT seq FROM events INDEXED BY events_front1 WHERE " + frontNameTerms("room") + " AND seq" + cmp + "? ORDER BY seq " + order + " LIMIT ?"
	events := []Message{}
	edge, scanned, truncated := bound, 0, false
	for chunk := max(frontChunkFirst, 4*limit); ; chunk *= 8 {
		n := min(chunk, FrontScanRows-scanned)
		got, err := scan("SELECT "+eventColumns+" FROM ("+inner+") f CROSS JOIN events e ON e.seq=f.seq JOIN rooms r ON r.name=e.room WHERE "+cond+" ORDER BY e.seq "+order+" LIMIT ?",
			append(append([]any{edge, n}, args...), limit-len(events)))
		if err != nil {
			return Result{}, err
		}
		events = append(events, got...)
		if len(events) >= limit {
			break
		}
		// Where this chunk ended, and whether the index goes on past it.
		var end sql.NullInt64
		var count int
		if err = tx.QueryRowContext(ctx, "SELECT "+agg+"(seq),count(*) FROM ("+inner+")", edge, n).Scan(&end, &count); err != nil {
			return Result{}, err
		}
		scanned += count
		if count < n {
			break
		}
		edge = end.Int64
		if scanned >= FrontScanRows {
			truncated = true
			break
		}
	}
	rooms, err := frontOnRooms(ctx, tx)
	if err != nil {
		return Result{}, err
	}
	for _, room := range rooms {
		q, qargs := "SELECT "+eventColumns+" FROM events e JOIN rooms r ON r.name=e.room WHERE e.room=? AND e.seq"+cmp+"? AND "+cond, []any{room, bound}
		if truncated && order == "ASC" {
			// Nothing past the walk's end yet: the next read resumes there.
			q, qargs = q+" AND e.seq<=?", append(qargs, edge)
		}
		if truncated && newest {
			// Do not jump past unscanned default rooms to an opted-in room.
			q, qargs = q+" AND e.seq>=?", append(qargs, edge)
		}
		got, err := scan(q+" ORDER BY e.seq "+order+" LIMIT ?", append(append(qargs[:2:2], args...), append(qargs[2:], limit)...))
		if err != nil {
			return Result{}, err
		}
		events = append(events, got...)
	}
	sort.Slice(events, func(i, j int) bool {
		if order == "ASC" {
			return events[i].internalSequence < events[j].internalSequence
		}
		return events[i].internalSequence > events[j].internalSequence
	})
	events = events[:min(len(events), limit)]
	fetched := len(events)
	res, err := s.finishPage(ctx, tx, c, events, order, limit, cursorSeq, now, newest)
	if err != nil || !truncated || fetched >= limit {
		return res, err
	}
	res.Data["has_more"] = true
	if newest && len(res.Messages) == 0 {
		// Even an empty scan must let a backward reader continue past rooms
		// that are no longer visible or no longer on the front page.
		opts, _ := parseListOptions(c.Data)
		res.OlderCursor = s.olderCursor(edge, c, opts)
	}
	if order == "ASC" && len(res.Messages) == fetched {
		// The page ends where the walk did, so the reader moves past it.
		res.NextCursor = s.cursor(edge)
	}
	return res, nil
}

// frontPageOnly reports whether data sets front_page and nothing else: the
// one policy change the operator may make to a room a key owns.
func frontPageOnly(data string) bool {
	var in map[string]json.RawMessage
	if json.Unmarshal([]byte(data), &in) != nil || len(in) != 1 {
		return false
	}
	_, ok := in["front_page"]
	return ok
}
