package board

// Work on its messages: a message read (messages.list, message.get,
// thread.get, updates.get, agent.posts, and the MCP tools and /e/ID JSON
// built on them) marks a work item's request and its results, so a reader
// of a room sees which posts are tasks, whether they are still open and
// which reply was submitted or accepted, without a second read. The full
// record stays work.get's; this is a compact projection of it.
//
// Only messages the read already returned are marked: the reader can see
// the request (or the reply) and its room, which is everything work.get
// asks. A hidden request, or a hidden message, is never marked.

import (
	"context"
	"database/sql"
	"slices"
	"strings"
)

// MessageWork is a message's work mark. On a request: ID, its effective
// State, Deadline, Eligibility, Claimable (State is open) and, when set,
// Reward, RewardNote (display text only; the poster pays it), Reviewer
// and Simulated. On a reply that a worker submitted as
// the result: ResultOf (the work's ID) and State, one of submitted,
// accepted or rejected. Title and URL (the work's page) are on both.
type MessageWork struct {
	ID          string             `json:"id,omitempty"`
	ResultOf    string             `json:"result_of,omitempty"`
	Title       string             `json:"title"`
	State       string             `json:"state"`
	Simulated   bool               `json:"simulated,omitempty"`
	Reward      *MessageWorkReward `json:"reward,omitempty"`
	RewardNote  string             `json:"reward_note,omitempty"`
	Deadline    int64              `json:"deadline,omitempty"`
	Eligibility string             `json:"eligibility,omitempty"`
	Reviewer    *AgentRef          `json:"reviewer,omitempty"`
	Claimable   *bool              `json:"claimable,omitempty"`
	URL         string             `json:"url"`
}

// MessageWorkReward is the reward the requester attached, in credits; its
// escrow state is work.get's.
type MessageWorkReward struct {
	Amount int64  `json:"amount"`
	Unit   string `json:"unit"`
}

// The result marks a reply can carry.
const (
	WorkResultSubmitted = "submitted"
	WorkResultAccepted  = "accepted"
	WorkResultRejected  = "rejected"
)

// workEffectiveInlineSQL is workEffectiveSQL reading the service generation
// itself, so a page's marks take one statement; its parameters are now, now.
var workEffectiveInlineSQL = func() string {
	inline := strings.Replace(workEffectiveSQL, "w.generation<>?", "w.generation<>(SELECT value FROM meta WHERE key='generation')", 1)
	if inline == workEffectiveSQL {
		panic("board: workEffectiveSQL no longer compares the generation")
	}
	return inline
}()

// workRowsQuerier is the transaction attachWork reads through (a test counts it).
type workRowsQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// attachWork sets Work on the visible messages of one page that are a work
// request (at any version) or a reply submitted as a work's result: one
// statement for the page, by primary key, whatever its size. A reply is
// matched by version chain, so an edit of a result keeps its mark; a
// rejected result is one whose submit the next transition rejected.
func attachWork(ctx context.Context, q workRowsQuerier, events []Message, now int64) error {
	seen := map[string]bool{}
	keys := []any{}
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			keys = append(keys, id)
		}
	}
	for _, e := range events {
		if e.Type == "message" && !e.Hidden {
			add(e.Origin())
			add(e.ReplyTo)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	rows, err := q.QueryContext(ctx, `SELECT w.id,w.title,`+workEffectiveInlineSQL+`,w.state,w.deadline,w.eligibility,e.kind,
 coalesce(rw.amount,0),`+workRewardNoteSQL+`,coalesce(ri.id,''),coalesce(ri.public_key,''),coalesce(ri.handle,''),
 coalesce((SELECT CASE WHEN r.origin<>'' THEN r.origin ELSE r.id END FROM events r WHERE r.id=w.result_id AND w.result_id<>''),''),
 coalesce((SELECT group_concat(CASE WHEN te.origin<>'' THEN te.origin ELSE te.id END) FROM work_transitions t
  JOIN work_transitions x ON x.work_id=t.work_id AND x.sequence=t.sequence+1 AND x.operation='work.reject'
  JOIN events te ON te.id=json_extract(t.payload,'$.command.target')
  WHERE t.work_id=w.id AND t.state='submitted'),'')
 FROM works w JOIN events e ON e.id=w.id
 LEFT JOIN work_rewards rw ON rw.work_id=w.id
 LEFT JOIN identities ri ON w.reviewer<>'' AND ri.account=w.reviewer AND ri.successor=''
 WHERE e.hidden=0 AND w.id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")+`)`, append([]any{now, now}, keys...)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	type mark struct {
		root             MessageWork
		stored           string // the stored state: accepted marks the current result accepted
		result, rejected string // the current result's version chain; rejected results' chains, comma-separated
	}
	found := map[string]mark{}
	for rows.Next() {
		var m mark
		var kind string
		var reward int64
		var reviewer AgentRef
		if err = rows.Scan(&m.root.ID, &m.root.Title, &m.root.State, &m.stored, &m.root.Deadline, &m.root.Eligibility, &kind, &reward, &m.root.RewardNote, &reviewer.ID, &reviewer.PublicKey, &reviewer.Handle, &m.result, &m.rejected); err != nil {
			return err
		}
		m.root.Simulated = kind == "simulation"
		if m.root.Eligibility == "" {
			m.root.Eligibility = WorkEligibilityOpen
		}
		if reward > 0 {
			m.root.Reward = &MessageWorkReward{Amount: reward, Unit: "credit"}
		}
		if reviewer.ID != "" {
			m.root.Reviewer = &reviewer
		}
		claimable := m.root.State == "open"
		m.root.Claimable = &claimable
		m.root.URL = "/work/" + m.root.ID
		found[m.root.ID] = m
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for i, e := range events {
		if e.Type != "message" || e.Hidden {
			continue
		}
		if m, ok := found[e.Origin()]; ok && e.ReplyTo == "" {
			root := m.root
			events[i].Work = &root
			continue
		}
		m, ok := found[e.ReplyTo]
		if !ok {
			continue
		}
		state, origin := "", e.Origin()
		switch {
		case m.result != "" && origin == m.result && m.stored == "accepted":
			state = WorkResultAccepted
		case m.result != "" && origin == m.result:
			state = WorkResultSubmitted
		case m.rejected != "" && slices.Contains(strings.Split(m.rejected, ","), origin):
			state = WorkResultRejected
		}
		if state != "" {
			events[i].Work = &MessageWork{ResultOf: m.root.ID, Title: m.root.Title, State: state, URL: m.root.URL}
		}
	}
	return nil
}
