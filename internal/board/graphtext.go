package board

import (
	"context"
	"slices"
	"strconv"
	"strings"
)

// Text layer limits: what one GraphMessages read may name and return.
const (
	GraphSelectMax   = 200   // identities and pools one read may name
	GraphMessagesMax = 2000  // messages one read returns, the newest of the match
	graphScanMax     = 50000 // rows one read scans before it reports truncated
)

// GraphMessage is one public message of the graph's text layer, with the
// metadata an export needs. Text is the newest visible version of the post.
type GraphMessage struct {
	ID        string `json:"id"`
	Thread    string `json:"thread"` // the original post's ID, which replies point at
	Sequence  int64  `json:"sequence"`
	Room      string `json:"room"`
	Page      string `json:"page"`
	Author    string `json:"author"` // fingerprint, or "anonymous"
	Handle    string `json:"handle,omitempty"`
	ReplyTo   string `json:"reply_to,omitempty"`
	CreatedAt int64  `json:"created_at"`
	SHA256    string `json:"sha256"`
	Kind      string `json:"kind"`
	Text      string `json:"text"`
}

// GraphMessageQuery selects text for a graph selection: the posts of the
// named identities (fingerprints, or "anon:ROOM" for a room's anonymous
// pool), optionally in one room. Among keeps only the messages exchanged
// between them: replies whose parent's author is also named, and those parents.
type GraphMessageQuery struct {
	Keys  []string
	Room  string
	Among bool
}

// GraphMessages reads the public text behind a graph selection under the
// graph's own rules: visible messages in public rooms only, never a
// conversation, an addressed message or anything hidden. It returns the
// messages oldest first, and whether the match was cut to its newest part.
func (s *Store) GraphMessages(ctx context.Context, q GraphMessageQuery) ([]GraphMessage, bool, error) {
	if len(q.Keys) == 0 || len(q.Keys) > GraphSelectMax {
		return nil, false, problem(400, "invalid_request", "Name 1 to "+strconv.Itoa(GraphSelectMax)+" identities in ids.")
	}
	if q.Room != "" && (!ValidRoomName(q.Room) || IsConversationRoom(q.Room)) {
		return nil, false, problem(400, "invalid_request", "room must be a public room name.")
	}
	var or []string
	var args, fps []any
	seen := map[string]bool{}
	for _, k := range q.Keys {
		if seen[k] {
			continue
		}
		seen[k] = true
		if room, ok := strings.CutPrefix(k, "anon:"); ok {
			if !ValidRoomName(room) || IsConversationRoom(room) {
				return nil, false, problem(400, "invalid_request", "anon: is followed by a public room name.")
			}
			or = append(or, "(e.public_key='' AND e.room=?)")
			args = append(args, room)
			continue
		}
		if len(k) != 64 || strings.Trim(k, "0123456789abcdef") != "" {
			return nil, false, problem(400, "invalid_request", "Each id is a 64-character lowercase fingerprint or anon:ROOM.")
		}
		fps = append(fps, k)
	}
	if len(fps) > 0 {
		or = append(or, "(e.public_key<>'' AND e.author IN ("+strings.TrimSuffix(strings.Repeat("?,", len(fps)), ",")+"))")
		args = append(args, fps...)
	}
	query := "SELECT e.id,CASE WHEN e.origin<>'' THEN e.origin ELSE e.id END,e.display_seq,e.room,e.page,CASE WHEN e.public_key='' THEN 'anonymous' ELSE e.author END,coalesce((SELECT i.handle FROM identities i WHERE i.id=e.author),''),e.reply_to,e.created_at,e.hash,e.kind,e.text" +
		graphFrom + " AND NOT EXISTS (SELECT 1 FROM events n WHERE n.supersedes=e.id AND n.supersedes<>'' AND n.hidden=0) AND (" + strings.Join(or, " OR ") + ")"
	if q.Room != "" {
		query += " AND e.room=?"
		args = append(args, q.Room)
	}
	query += " ORDER BY e.seq DESC LIMIT ?"
	args = append(args, graphScanMax+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, err
	}
	var out []GraphMessage
	for rows.Next() {
		var m GraphMessage
		if err = rows.Scan(&m.ID, &m.Thread, &m.Sequence, &m.Room, &m.Page, &m.Author, &m.Handle, &m.ReplyTo, &m.CreatedAt, &m.SHA256, &m.Kind, &m.Text); err != nil {
			rows.Close()
			return nil, false, err
		}
		if m.Author == "anonymous" {
			m.Handle = ""
		}
		out = append(out, m)
	}
	if err = closeRows(rows); err != nil {
		return nil, false, err
	}
	truncated := len(out) > graphScanMax
	if truncated {
		out = out[:graphScanMax]
	}
	if q.Among {
		out = graphExchanged(out, len(seen) == 1)
	}
	if len(out) > GraphMessagesMax {
		out, truncated = out[:GraphMessagesMax], true
	}
	slices.Reverse(out)
	if out == nil {
		out = []GraphMessage{}
	}
	return out, truncated, nil
}

// graphSummaryScope is the counters scope of one UTC day's summary spend.
const graphSummaryScope = "graph_summary_microusd:"

// GraphSummarySpend returns the micro-USD spent on graph summaries on day
// (YYYY-MM-DD, UTC). The total survives restarts, so the daily cap does too.
func (s *Store) GraphSummarySpend(ctx context.Context, day string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, "SELECT coalesce((SELECT value FROM counters WHERE scope=?),0)", graphSummaryScope+day).Scan(&n)
	return n, err
}

// AddGraphSummarySpend adds micro-USD to day's summary spend.
func (s *Store) AddGraphSummarySpend(ctx context.Context, day string, micro int64) error {
	return addCounter(ctx, s.db, graphSummaryScope+day, micro)
}

// graphExchanged keeps the replies whose parent is also in msgs (so both
// authors were named) and those parents. Between two different identities a
// self-reply is not an exchange; with one identity named it is.
func graphExchanged(msgs []GraphMessage, single bool) []GraphMessage {
	key := func(m GraphMessage) string {
		if m.Author == "anonymous" {
			return "anon:" + m.Room
		}
		return m.Author
	}
	byThread := make(map[string]int, len(msgs))
	for i, m := range msgs {
		byThread[m.Thread] = i
	}
	keep := make([]bool, len(msgs))
	for i, m := range msgs {
		if m.ReplyTo == "" {
			continue
		}
		if p, ok := byThread[m.ReplyTo]; ok && (single || key(msgs[p]) != key(m)) {
			keep[i], keep[p] = true, true
		}
	}
	kept := msgs[:0]
	for i, m := range msgs {
		if keep[i] {
			kept = append(kept, m)
		}
	}
	return kept
}
