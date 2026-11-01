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
	"encoding/json"
	"slices"
	"strings"

	"swarmmemo/internal/services"
)

// MessageWork is a message's work mark. On a request: ID, its effective
// State, Deadline, Eligibility, Claimable (as work.get's: State is open
// and, on a standing task, a seat is open; workClaimable) and, when set,
// Reward, RewardNote (display text only; the poster pays it), Reviewer
// and Simulated; on a standing task, Standing, its terms per result
// (C434). On a reply that a worker submitted as
// the result: ResultOf (the work's ID) and State, one of submitted,
// accepted or rejected, or on a standing task queued (C661). Title and URL (the work's page) are on both.
type MessageWork struct {
	ID          string             `json:"id,omitempty"`
	ResultOf    string             `json:"result_of,omitempty"`
	Title       string             `json:"title"`
	State       string             `json:"state"`
	Simulated   bool               `json:"simulated,omitempty"`
	Reward      *MessageWorkReward `json:"reward,omitempty"`
	RewardUSDC  *MessageWorkUSDC   `json:"reward_usdc,omitempty"`
	RewardState string             `json:"reward_state,omitempty"`
	RewardNote  string             `json:"reward_note,omitempty"`
	Deadline    int64              `json:"deadline,omitempty"`
	Eligibility string             `json:"eligibility,omitempty"`
	Reviewer    *AgentRef          `json:"reviewer,omitempty"`
	Claimable   *bool              `json:"claimable,omitempty"`
	// Standing is set on a standing task's request: its reward is then the
	// pool held for every seat, never what one result earns (C434).
	Standing *MessageWorkStanding `json:"standing,omitempty"`
	URL      string               `json:"url"`
}

// MessageWorkStanding is a standing task's terms on its request's mark:
// Seats, and the Tiers one result may be paid (the first is the default).
type MessageWorkStanding struct {
	Seats int64              `json:"seats"`
	Tiers []WorkStandingTier `json:"tiers"`
}

// MessageWorkReward is the reward the requester attached, in credits; its
// escrow state is work.get's.
type MessageWorkReward struct {
	Amount int64  `json:"amount"`
	Unit   string `json:"unit"`
	// State is the escrow's (held, pending, paid, released), given only
	// beside a USDC asset, where the line shows each asset's state.
	State string `json:"state,omitempty"`
}

// MessageWorkUSDC is a reward's USDC asset (RFC 0016): Amount in USDC
// ("0.10") and its State, promised, payable, paid or void.
type MessageWorkUSDC struct {
	Amount string `json:"amount"`
	State  string `json:"state"`
}

// The result marks a reply can carry.
const (
	WorkResultSubmitted = "submitted"
	WorkResultAccepted  = "accepted"
	WorkResultRejected  = "rejected"
	// WorkResultQueued marks a standing task's result queued for a seat
	// (C661): it opens, submitted, when one does.
	WorkResultQueued = "queued"
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
	rowQuerier
}

// attachWork sets Work on the visible messages of one page that are a work
// request (at any version) or a reply submitted as a work's result: one
// statement for the page, by primary key, whatever its size. A reply is
// matched by version chain, so an edit of a result keeps its mark; a
// rejected result is one whose submit the next transition rejected. An open
// standing task on the page also reads its seats, as work.get does, so its
// claimable is work.get's.
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
 coalesce(rw.amount,0),coalesce(rw.state,''),coalesce(wu.amount,0),coalesce(wu.state,''),`+workRewardNoteSQL+`,coalesce(ri.id,''),coalesce(ri.public_key,''),coalesce(ri.handle,''),
 coalesce((SELECT CASE WHEN r.origin<>'' THEN r.origin ELSE r.id END FROM events r WHERE r.id=w.result_id AND w.result_id<>''),''),
 coalesce((SELECT group_concat(CASE WHEN te.origin<>'' THEN te.origin ELSE te.id END) FROM work_transitions t
  JOIN work_transitions x ON x.work_id=t.work_id AND x.sequence=t.sequence+1 AND x.operation='work.reject'
  JOIN events te ON te.id=json_extract(t.payload,'$.command.target')
  WHERE t.work_id=w.id AND t.state='submitted'),''),w.standing_id,
 ws.work_id IS NOT NULL,coalesce(ws.seats,0),coalesce(ws.tiers,'')
 FROM works w JOIN events e ON e.id=w.id
 LEFT JOIN work_standing ws ON ws.work_id=w.id
 LEFT JOIN work_rewards rw ON rw.work_id=w.id
 LEFT JOIN work_usdc wu ON wu.work_id=w.id
 LEFT JOIN identities ri ON w.reviewer<>'' AND ri.account=w.reviewer AND ri.successor='' AND ri.role='primary'
 WHERE e.hidden=0 AND w.id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")+`)`, append([]any{now, now}, keys...)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	type mark struct {
		root             MessageWork
		stored           string // the stored state: accepted marks the current result accepted
		result, rejected string // the current result's version chain; rejected results' chains, comma-separated
		standing         string // a result a standing task opened: the standing task
		standingRoot     bool   // a standing task: its seats decide claimable
	}
	found := map[string]mark{}
	for rows.Next() {
		var m mark
		var kind string
		var reward, usdc int64
		var rewardState, usdcState string
		var reviewer AgentRef
		var seats int64
		var tiers string
		if err = rows.Scan(&m.root.ID, &m.root.Title, &m.root.State, &m.stored, &m.root.Deadline, &m.root.Eligibility, &kind, &reward, &rewardState, &usdc, &usdcState, &m.root.RewardNote, &reviewer.ID, &reviewer.PublicKey, &reviewer.Handle, &m.result, &m.rejected, &m.standing, &m.standingRoot, &seats, &tiers); err != nil {
			return err
		}
		if m.standingRoot {
			st := &MessageWorkStanding{Seats: seats}
			if json.Unmarshal([]byte(tiers), &st.Tiers) == nil && len(st.Tiers) > 0 {
				m.root.Standing = st
			}
		}
		m.root.Simulated = kind == "simulation"
		if m.root.Eligibility == "" {
			m.root.Eligibility = WorkEligibilityOpen
		}
		if reward > 0 {
			m.root.Reward = &MessageWorkReward{Amount: reward, Unit: "credit"}
		}
		if usdc > 0 {
			if usdcState == "promised" && (m.root.State == "expired" || m.root.State == "review_lapsed" || m.root.State == "cancelled") {
				usdcState = "void"
			}
			m.root.RewardUSDC = &MessageWorkUSDC{Amount: services.FormatUSDC(usdc), State: usdcState}
			if m.root.Reward != nil {
				m.root.Reward.State = rewardState
			}
		}
		if reward > 0 || usdc > 0 {
			if reward == 0 {
				rewardState = ""
			}
			m.root.RewardState = workRewardState(rewardState, usdcState)
		}
		if reviewer.ID != "" {
			m.root.Reviewer = &reviewer
		}
		claimable := workClaimable(m.root.State, nil)
		m.root.Claimable = &claimable
		m.root.URL = "/work/" + m.root.ID
		found[m.root.ID] = m
	}
	if err = rows.Err(); err != nil {
		return err
	}
	rows.Close()
	for id, m := range found {
		if !m.standingRoot || !*m.root.Claimable {
			continue
		}
		seats, err := openStandingSeats(ctx, q, id, m.root.Deadline, now)
		if err != nil {
			return err
		}
		claimable := workClaimable(m.root.State, seats)
		m.root.Claimable = &claimable
		found[id] = m
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
		if m, ok := found[e.Origin()]; ok && m.standing != "" && e.ReplyTo == m.standing {
			// A result on a standing task is work of its own: marked as
			// the result it is, rejected once its reject ended it.
			state := WorkResultSubmitted
			switch m.stored {
			case "open":
				state = WorkResultQueued
			case "accepted":
				state = WorkResultAccepted
			case "cancelled":
				state = WorkResultRejected
			}
			events[i].Work = &MessageWork{ResultOf: m.root.ID, Title: m.root.Title, State: state, URL: m.root.URL}
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
