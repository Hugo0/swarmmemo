package board

import (
	"context"
	"encoding/json"
	"slices"
)

// The operator's parity check before INBOX_ENTRIES=read (C61 step 2):
// `swarmmemo inbox parity`. For the accounts with the most recent entries
// it runs the same updates.get under shadow and under read, in one
// read-only transaction each, and compares what a client of today's fields
// sees: the messages and every data field but data.entries. It reports ids
// and field names only, never text.

// InboxParityAgent is one agent's comparison.
type InboxParityAgent struct {
	Agent    string `json:"agent"`
	Messages int    `json:"messages"`
	Entries  int    `json:"entries"`
	// Differ lists the reads that differ ("own", "public", "own@cursor"),
	// each with the fields that differ.
	Differ map[string][]string `json:"differ,omitempty"`
	// Older is true when every differing message is older than the
	// backfill window: no entry exists for it, which is expected.
	Older bool `json:"older_than_backfill,omitempty"`
}

// InboxParityReport is the check's answer.
type InboxParityReport struct {
	Agents   []InboxParityAgent `json:"agents"`
	Checked  int                `json:"checked"`
	Differ   int                `json:"differ"`
	Expected int                `json:"differ_older_than_backfill"`
	OK       bool               `json:"ok"`
}

// InboxReadParity compares shadow and read for up to agents agents (at most
// 200): each one's own read and an anonymous read with no cursor, and its
// own read from a cursor back cursorBack messages. OK is true when every
// difference is a message older than the backfill window.
func (s *Store) InboxReadParity(ctx context.Context, agents int, cursorBack int64) (InboxParityReport, error) {
	report := InboxParityReport{Agents: []InboxParityAgent{}}
	agents = min(max(agents, 1), 200)
	rows, err := s.db.QueryContext(ctx, `SELECT i.id,i.account FROM (SELECT account,max(seq) AS last FROM inbox_entries GROUP BY account ORDER BY last DESC LIMIT ?) a
 JOIN identities i ON i.account=a.account AND i.successor='' ORDER BY a.last DESC`, agents)
	if err != nil {
		return report, err
	}
	type who struct{ id, account string }
	var list []who
	for rows.Next() {
		var w who
		if err = rows.Scan(&w.id, &w.account); err != nil {
			rows.Close()
			return report, err
		}
		list = append(list, w)
	}
	if err = closeRows(rows); err != nil {
		return report, err
	}
	var top int64
	if err = s.db.QueryRowContext(ctx, "SELECT coalesce(max(seq),0) FROM events").Scan(&top); err != nil {
		return report, err
	}
	back := s.cursor(max(top-cursorBack, 0))
	now := s.now().Unix()
	cutoff := now - InboxBackfillDays*86400
	for _, w := range list {
		own := actor{id: w.id, account: w.account, signed: true}
		anon := actor{account: "parity-anonymous"}
		got := InboxParityAgent{Agent: w.id, Older: true}
		for _, read := range []struct {
			name   string
			a      actor
			cursor string
			full   bool
		}{{"own", own, "", true}, {"public", anon, "", true}, {"own@cursor", own, back, false}} {
			c := Command{Operation: "updates.get", Target: w.id, Cursor: read.cursor, Limit: PageMax}
			shadow, err := s.parityRead(withInboxMode(ctx, InboxShadow), c, read.a, now)
			if err != nil {
				return report, err
			}
			entries, err := s.parityRead(withInboxMode(ctx, InboxRead), c, read.a, now)
			if err != nil {
				return report, err
			}
			if read.name == "own" {
				got.Messages = len(entries.Messages)
				list, _ := entries.Data["entries"].([]UpdateEntry)
				got.Entries = len(list)
			}
			fields, ids := parityDiff(shadow, entries, read.full)
			if len(fields) == 0 {
				continue
			}
			if got.Differ == nil {
				got.Differ = map[string][]string{}
			}
			got.Differ[read.name] = fields
			older, err := s.allOlderThan(ctx, ids, cutoff)
			if err != nil {
				return report, err
			}
			got.Older = got.Older && older
		}
		report.Checked++
		if got.Differ == nil {
			got.Older = false
		} else {
			report.Differ++
			if got.Older {
				report.Expected++
			}
		}
		report.Agents = append(report.Agents, got)
	}
	report.OK = report.Differ == report.Expected
	return report, nil
}

func withInboxMode(ctx context.Context, m InboxMode) context.Context {
	return context.WithValue(ctx, inboxModeKey{}, m)
}

// parityRead is one updates.get as a, in a transaction it rolls back.
func (s *Store) parityRead(ctx context.Context, c Command, a actor, now int64) (Result, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback()
	return s.readUpdates(ctx, tx, c, a, now)
}

// parityDiff is the fields in which two reads differ (messages, and with
// full every data field but entries; otherwise the message lists by reason),
// and the message ids that make the difference.
func parityDiff(a, b Result, full bool) ([]string, []string) {
	var fields []string
	ids := map[string]bool{}
	note := func(x, y []string) {
		for _, id := range x {
			if !slices.Contains(y, id) {
				ids[id] = true
			}
		}
		for _, id := range y {
			if !slices.Contains(x, id) {
				ids[id] = true
			}
		}
	}
	if x, y := a.messageIDList(), b.messageIDList(); !slices.Equal(x, y) {
		fields = append(fields, "messages")
		note(x, y)
	} else if full {
		ja, _ := json.Marshal(a.Messages)
		jb, _ := json.Marshal(b.Messages)
		if string(ja) != string(jb) {
			fields = append(fields, "messages")
		}
	}
	keys := []string{"replies", "addressed", "mentions", "room_activity", "conversations"}
	if full {
		keys = nil
		for k := range a.Data {
			keys = append(keys, k)
		}
		for k := range b.Data {
			if _, ok := a.Data[k]; !ok {
				keys = append(keys, k)
			}
		}
		slices.Sort(keys)
	}
	for _, k := range keys {
		if k == "entries" {
			continue
		}
		x, xok := a.Data[k].([]string)
		y, yok := b.Data[k].([]string)
		if xok && yok {
			if !slices.Equal(x, y) {
				fields = append(fields, k)
				note(x, y)
			}
			continue
		}
		ja, _ := json.Marshal(a.Data[k])
		jb, _ := json.Marshal(b.Data[k])
		if string(ja) != string(jb) {
			fields = append(fields, k)
		}
	}
	out := make([]string, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	slices.Sort(out)
	if len(fields) > 0 && len(out) == 0 {
		// A difference no message id explains (data.received, data.wakeups).
		out = append(out, "")
	}
	return fields, out
}

func (r Result) messageIDList() []string {
	out := make([]string, len(r.Messages))
	for i, m := range r.Messages {
		out[i] = m.ID
	}
	return out
}

// allOlderThan reports whether every message id was created before cutoff.
func (s *Store) allOlderThan(ctx context.Context, ids []string, cutoff int64) (bool, error) {
	for _, id := range ids {
		var created int64
		if err := s.db.QueryRowContext(ctx, "SELECT created_at FROM events WHERE id=?", id).Scan(&created); err != nil || created >= cutoff {
			return false, nil
		}
	}
	return true, nil
}
