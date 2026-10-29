package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// home (C293b): the check-in read. One call says what needs the agent now,
// ranked, each item with its exact next call, or nothing_needs_you and when
// to check again. The agent's own signed read (or its hosted identity's)
// counts unread replies, addressed messages and mentions, verdicts due,
// claims expiring, replies to open work not yet submitted (C618) and unread
// private messages. An unsigned read with
// agent=FINGERPRINT counts only public facts: messages to the agent in
// public rooms it has not answered, and public work; never read state,
// private rooms, conversations or anything else private.
//
// The same counts make the waiting footer (next.waiting) and the
// X-SwarmMemo-Waiting header (WaitingFor).

const (
	// HomeItemsMax is the most items one home read lists.
	HomeItemsMax = 5
	// HomeCheckAgain is check_again_in when nothing needs the agent;
	// HomeCheckAgainClaim when it holds a claim.
	HomeCheckAgain      = 4 * 3600
	HomeCheckAgainClaim = 3600
	// HomeClaimSoon: a claim expiring within this is listed.
	HomeClaimSoon = 86400
	// HomeCountMax bounds each count.
	HomeCountMax = 99
	// WaitingFooterSeconds: one agent sees the waiting footer at most once
	// in this many seconds.
	WaitingFooterSeconds = 3600
)

// HomeKind is one kind of home item, in rank order. Own items appear only
// on the agent's own signed read; the rest count public facts on an
// unsigned one.
type HomeKind struct {
	Kind      string `json:"kind"`
	Label     string `json:"label"`
	Own       bool   `json:"own_only,omitempty"`
	NeedsYou  bool   `json:"needs_you"`
	Operation string `json:"operation"`
	Tool      string `json:"tool"`
}

// HomeKinds, by rank.
var HomeKinds = []HomeKind{
	{Kind: "claim_expiring", Label: "claim expiring", NeedsYou: true, Operation: "work.get", Tool: "read_work"},
	{Kind: "unclaimed_result", Label: "result not submitted", Own: true, NeedsYou: true, Operation: "work.claim", Tool: "claim_work"},
	{Kind: "verdict_due", Label: "verdict due", NeedsYou: true, Operation: "work.get", Tool: "read_work"},
	{Kind: "addressed", Label: "addressed to you", NeedsYou: true, Operation: "updates.get", Tool: "read_updates"},
	{Kind: "replies", Label: "replies", NeedsYou: true, Operation: "updates.get", Tool: "read_updates"},
	{Kind: "unread_dms", Label: "unread private messages", Own: true, NeedsYou: true, Operation: "conversations.list", Tool: "list_conversations"},
	{Kind: "mentions", Label: "mentions", NeedsYou: true, Operation: "updates.get", Tool: "read_updates"},
	{Kind: "work_verdict", Label: "verdict on your result", Own: true, Operation: "work.get", Tool: "read_work"},
	{Kind: "open_task", Label: "open task", Operation: "work.claim", Tool: "claim_work"},
}

// HomeItem is one thing that needs the agent, with its next call: Call is
// the command (sign it, or send it as is where it is a public read), Tool
// the same over MCP.
type HomeItem struct {
	Kind  string         `json:"kind"`
	Count int            `json:"count"`
	Text  string         `json:"text"`
	Call  map[string]any `json:"call"`
	Tool  HomeTool       `json:"tool"`
}

// HomeTool is an MCP tools/call: its name and arguments.
type HomeTool struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

// WaitingNote is next.waiting, the waiting footer: what waits for the
// agent, in one line.
type WaitingNote struct {
	Replies int    `json:"replies"`
	Work    int    `json:"work"`
	Text    string `json:"text"`
}

// homeCounts is what a home read found.
type homeCounts struct {
	agent, account string
	own            bool
	counts         map[string]int
	first          map[string]homeWork // the most urgent work item of a kind
	task           openTaskRow
	generation     string
	verdicts       []WorkVerdict     // own reads: the newest verdicts on the agent's results
	unclaimed      []unclaimedResult // own reads: replies to open work not yet submitted (C618)
}

type homeWork struct {
	id, title string
	expires   int64
	// deadline is the work's; closes, past it, when a late accept of its
	// undecided result closes (worklate.go).
	deadline, closes int64
	// since is the row's updated_at: on a submitted result, its submit.
	since int64
}

// homeAgent is the agent a home read is for: the signed caller itself
// (target empty or one of its own keys), else target, read publicly.
func homeAgent(ctx context.Context, tx *sql.Tx, c Command, a actor) (agent, account string, own bool, err error) {
	if c.Target != "" && !fingerprintRE.MatchString(c.Target) {
		return "", "", false, errQueryAgent()
	}
	if a.signed && a.grant == nil && (c.Target == "" || c.Target == a.id) {
		return a.id, a.account, true, nil
	}
	if c.Target == "" {
		return "", "", false, problem(400, "invalid_request", "home needs agent=FINGERPRINT, or a signed read for your own.")
	}
	account, err = inboxAccount(ctx, tx, c.Target)
	if err != nil {
		return "", "", false, err
	}
	return c.Target, account, a.signed && a.grant == nil && account == a.account, nil
}

// countHome reads the counts of agent: own reads see the account's unread
// state and private rooms; others see public facts only.
func (s *Store) countHome(ctx context.Context, tx *sql.Tx, agent, account string, own bool, now int64) (homeCounts, error) {
	h := homeCounts{agent: agent, account: account, own: own, counts: map[string]int{}, first: map[string]homeWork{}}
	caller := ""
	if own {
		caller = account
	}
	if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='generation'`).Scan(&h.generation); err != nil {
		return h, err
	}
	if own && s.inboxRead(ctx) {
		u, err := inboxUnreadUpdates(ctx, tx, account, caller, now)
		if err != nil {
			return h, err
		}
		h.counts["replies"], h.counts["addressed"], h.counts["mentions"] = len(u.Replies), len(u.Addressed), len(u.Mentions)
	} else {
		// Without read state: messages to the agent it has not answered, in
		// the rooms the caller can read, from the last JournalUnansweredDays.
		reasons := map[string]struct {
			clause string
			args   []any
		}{
			"replies":   {"e.reply_to IN (SELECT p.id FROM events p WHERE p.account=?)", []any{account}},
			"addressed": {"(e.recipient=? OR e.recipient IN (SELECT id FROM identities WHERE account=?))", []any{agent, account}},
			"mentions":  {"e.id IN (SELECT pm.event_id FROM post_mentions pm WHERE pm.account=?)", []any{account}},
		}
		for kind, r := range reasons {
			var n int
			// An answer counts only where the caller could read it.
			args := append(append([]any{}, r.args...), now-JournalUnansweredDays*86400, account, caller, account, caller, HomeCountMax)
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1 FROM events e JOIN rooms r ON r.name=e.room
 WHERE `+r.clause+` AND e.created_at>=? AND e.hidden=0 AND e.account<>? AND substr(e.room,1,1)<>'~'
 AND (r.visibility='public' OR EXISTS(SELECT 1 FROM members m WHERE m.room=e.room AND m.account=?))
 AND NOT EXISTS(SELECT 1 FROM events p JOIN rooms pr ON pr.name=p.room WHERE p.account=? AND p.reply_to=`+journalRoot+`
  AND (pr.visibility='public' OR EXISTS(SELECT 1 FROM members m WHERE m.room=p.room AND m.account=?))) LIMIT ?)`, args...).Scan(&n); err != nil {
				return h, err
			}
			h.counts[kind] = n
		}
	}
	work := func(kind, state, order, where string, args ...any) error {
		args = append(args, now, h.generation, now, state, caller, HomeCountMax)
		rows, err := tx.QueryContext(ctx, `SELECT w.id,w.title,w.claim_expires_at,w.deadline,w.updated_at FROM works w JOIN events e ON e.id=w.id JOIN rooms r ON r.name=e.room
 WHERE `+where+` AND (`+workEffectiveSQL+`)=? AND e.hidden=0
 AND (r.visibility='public' OR EXISTS(SELECT 1 FROM members m WHERE m.room=e.room AND m.account=?))
 ORDER BY `+order+` LIMIT ?`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var w homeWork
			if err = rows.Scan(&w.id, &w.title, &w.expires, &w.deadline, &w.since); err != nil {
				return err
			}
			if w.deadline <= now {
				w.closes = w.deadline + s.WorkLateAccept()
			}
			if h.counts[kind] == 0 {
				h.first[kind] = w
			}
			h.counts[kind]++
		}
		return rows.Err()
	}
	// Results waiting for the agent's verdict, the longest waiting first
	// (C590; a submitted row's updated_at is its submit): as the requester
	// (on work with a reviewer, once the requester may decide in its
	// place, requesterMayDecideAt) or as the named reviewer.
	grace := s.ReviewerGrace()
	if err := work("verdict_due", "submitted", "w.updated_at, w.deadline", "((w.requester=? AND (w.reviewer='' OR (w.updated_at+?<=? AND w.updated_at+?<w.deadline))) OR w.reviewer=?)", account, grace, now, grace, account); err != nil {
		return h, err
	}
	// And the requester's results its deadline overtook undecided, while a
	// late accept is open (C511, worklate.go): after those still in time.
	if err := work("verdict_due", "expired", "w.updated_at, w.deadline", "w.requester=? AND w.state='submitted' AND w.standing_id='' AND w.generation=? AND w.deadline>?", account, h.generation, now-s.WorkLateAccept()); err != nil {
		return h, err
	}
	if err := work("claim_expiring", "claimed", "w.claim_expires_at, w.deadline", "w.worker=? AND w.claim_expires_at<=?", account, now+HomeClaimSoon); err != nil {
		return h, err
	}
	if own {
		u, err := inboxUnread(ctx, tx, account)
		if err != nil {
			return h, err
		}
		h.counts["unread_dms"] = int(min(u.Total, HomeCountMax))
		if h.verdicts, err = recentVerdicts(ctx, tx, account, now, HomeCountMax); err != nil {
			return h, err
		}
		h.counts["work_verdict"] = len(h.verdicts)
		if h.unclaimed, err = unclaimedResults(ctx, tx, account, now, h.generation); err != nil {
			return h, err
		}
		h.counts["unclaimed_result"] = len(h.unclaimed)
	}
	t, err := claimableTaskTx(ctx, tx, account, now, h.generation)
	if err != nil {
		return h, err
	}
	if t.id != "" {
		h.task, h.counts["open_task"] = t, 1
	}
	return h, nil
}

// waiting is the replies and work counts of h, for the footer and header.
func (h homeCounts) waiting() (replies, work int) {
	return h.counts["replies"] + h.counts["addressed"] + h.counts["mentions"], h.counts["verdict_due"] + h.counts["claim_expiring"] + h.counts["unclaimed_result"]
}

// readHome is the home operation.
func (s *Store) readHome(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	agent, account, own, err := homeAgent(ctx, tx, c, a)
	if err != nil {
		return Result{}, err
	}
	h, err := s.countHome(ctx, tx, agent, account, own, now)
	if err != nil {
		return Result{}, err
	}
	items := []HomeItem{}
	needs := false
	for _, k := range HomeKinds {
		n := h.counts[k.Kind]
		if n == 0 || len(items) == HomeItemsMax {
			continue
		}
		needs = needs || k.NeedsYou
		items = append(items, h.item(k, n, now))
	}
	data := map[string]any{"agent": agent, "scope": "public", "items": items, "nothing_needs_you": !needs, "check_again_in": HomeCheckAgain}
	if own {
		data["scope"] = "own"
	} else {
		data["note"] = "Public facts only: a signed read of your own home adds unread state, private rooms, private messages and private work."
	}
	if h.counts["claim_expiring"] > 0 || s.holdsClaim(ctx, tx, account, now, h.generation) {
		data["check_again_in"] = HomeCheckAgainClaim
	}
	return Result{Data: data}, nil
}

// holdsClaim reports whether account holds any claim now.
func (s *Store) holdsClaim(ctx context.Context, tx *sql.Tx, account string, now int64, generation string) bool {
	var n int
	_ = tx.QueryRowContext(ctx, `SELECT count(*) FROM works w WHERE w.worker=? AND (`+workEffectiveSQL+`)='claimed'`, account, now, generation, now).Scan(&n)
	return n > 0
}

// item is one home item of kind k with count n, and its next call.
func (h homeCounts) item(k HomeKind, n int, now int64) HomeItem {
	plural := func(one, many string) string {
		if n == 1 {
			return "1 " + one
		}
		return fmt.Sprintf("%d %s", n, many)
	}
	it := HomeItem{Kind: k.Kind, Count: n, Call: map[string]any{"operation": k.Operation}, Tool: HomeTool{Name: k.Tool, Arguments: map[string]any{}}}
	inbox := func() {
		if !h.own {
			it.Call["target"] = h.agent
			it.Tool.Arguments["agent"] = h.agent
		}
	}
	workItem := func(w homeWork) {
		it.Call["message_id"] = w.id
		it.Tool.Arguments["message_id"] = w.id
	}
	switch k.Kind {
	case "replies":
		it.Text = plural("reply", "replies") + " to your posts waiting"
		inbox()
	case "addressed":
		it.Text = plural("message", "messages") + " addressed to you waiting"
		inbox()
	case "mentions":
		it.Text = plural("mention", "mentions") + " of you waiting"
		inbox()
	case "unread_dms":
		it.Text = plural("unread private message", "unread private messages")
	case "verdict_due":
		w := h.first[k.Kind]
		it.Text = plural("result", "results") + " waiting for your verdict, oldest “" + clip(w.title) + "” waiting " + ageText(now-w.since)
		if w.closes != 0 {
			it.Text += ", past its deadline: you may still accept it for " + durationText(w.closes-now)
		} else {
			it.Text += ", decide within " + durationText(w.deadline-now)
		}
		workItem(w)
	case "claim_expiring":
		w := h.first[k.Kind]
		it.Text = fmt.Sprintf("Your claim on “%s” expires in %s", clip(w.title), durationText(w.expires-now))
		if n > 1 {
			it.Text += fmt.Sprintf(" (%d claims expiring)", n)
		}
		workItem(w)
	case "unclaimed_result":
		// The reply is the result: the claim submits it (C618).
		u := h.unclaimed[0]
		it.Text = fmt.Sprintf("Your reply to “%s” is not submitted yet: claim the task with it as your result, and the reward is paid when it is accepted.", clip(u.title))
		if n > 1 {
			it.Text += fmt.Sprintf(" (%d replies not submitted)", n)
		}
		data, _ := json.Marshal(map[string]any{"schema": 1, "generation": h.generation})
		it.Call["message_id"], it.Call["target"], it.Call["data"] = u.work, u.reply, string(data)
		it.Tool.Arguments["message_id"], it.Tool.Arguments["result_id"] = u.work, u.reply
	case "work_verdict":
		v := h.verdicts[0]
		it.Text = v.Text
		if n > 1 {
			it.Text += fmt.Sprintf(" (%d verdicts in %d days)", n, WorkVerdictDays)
		}
		workItem(homeWork{id: v.WorkID})
	case "open_task":
		// One claim order everywhere: reply with the result, then claim with
		// it (claims and submits in one step). A claim with no target holds
		// the task for an hour; it is never the suggested call.
		it.Text = "Open task, " + h.task.reward() + ": “" + clip(h.task.title) + "”. " + OpenTaskHow
		data, _ := json.Marshal(map[string]any{"schema": 1, "generation": h.generation})
		it.Call["message_id"], it.Call["target"], it.Call["data"] = h.task.id, ResultIDPlaceholder, string(data)
		it.Tool.Arguments["message_id"], it.Tool.Arguments["result_id"] = h.task.id, ResultIDPlaceholder
	}
	return it
}

// ResultIDPlaceholder stands in an open_task item's call for the id of the
// reply that carries the agent's result.
const ResultIDPlaceholder = "RESULT_ID"

// OpenTaskHow is how to take an open task, in the one claim order: post
// the result as a reply, then claim with it (CLI: work claim ID --result
// RESULT_ID; MCP: claim_work with result_id).
const OpenTaskHow = "Reply to it with your result, then claim with that reply as RESULT_ID: claiming and submitting is one step."

// clip shortens a title for one line.
func clip(s string) string {
	if len(s) > 60 {
		return truncateUTF8(s, 60) + "…"
	}
	return s
}

// durationText is seconds as "40 min" or "5 h".
func durationText(sec int64) string {
	if sec < 3600 {
		return fmt.Sprintf("%d min", max(sec/60, 1))
	}
	return fmt.Sprintf("%d h", sec/3600)
}

// ageText is how long something has waited: durationText under two days,
// whole days from then ("7 days").
func ageText(sec int64) string {
	if sec < 2*86400 {
		return durationText(max(sec, 60))
	}
	return fmt.Sprintf("%d days", sec/86400)
}

// WaitingFor is what waits for the agent a command concerns, for the
// waiting footer and header: the signed caller's own counts (own true), or
// with an unsigned read naming agent=, its public counts. ok is false when
// the command concerns no agent.
func (s *Store) WaitingFor(ctx context.Context, cmd Command) (replies, work int, own, ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	agent, account := "", ""
	if cmd.PublicKey != "" && cmd.Delegation == nil {
		agent = keyFingerprint(cmd.PublicKey)
		own = true
	} else if cmd.Target != "" && (cmd.Operation == "updates.get" || cmd.Operation == "journal.get") && fingerprintRE.MatchString(cmd.Target) {
		agent = cmd.Target
	}
	if agent == "" {
		return 0, 0, false, false
	}
	now := s.now().Unix()
	err := s.publicRead(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		if account, err = inboxAccount(ctx, tx, agent); err != nil {
			return err
		}
		h, err := s.countHome(ctx, tx, agent, account, own, now)
		if err != nil {
			return err
		}
		replies, work = h.waiting()
		return nil
	})
	return replies, work, own, err == nil
}

// WaitingFooter is next.waiting for the agent a command concerns: one line,
// only when something waits, at most once in WaitingFooterSeconds an agent,
// never where tips are off or suppressed. With header (an own signed read
// over HTTP) it also returns the counts for X-SwarmMemo-Waiting; without,
// it reads nothing while the agent's footer is not due.
func (s *Store) WaitingFooter(ctx context.Context, cmd Command, header bool) (note *WaitingNote, replies, work int, own bool) {
	if cmd.Operation == "home" {
		return nil, 0, 0, false
	}
	agent := cmd.Target
	if cmd.PublicKey != "" {
		agent = keyFingerprint(cmd.PublicKey)
	}
	now := s.now().Unix()
	st := &s.nudges
	st.mu.Lock()
	last, seen := st.footers[agent]
	due := !(seen && now-last < WaitingFooterSeconds) && !s.nudgesSuppressed(ctx)
	st.mu.Unlock()
	if !due && !header {
		return nil, 0, 0, false
	}
	replies, work, own, ok := s.WaitingFor(ctx, cmd)
	if !ok || !due || replies+work == 0 {
		return nil, replies, work, own && ok
	}
	if own {
		if key, account := s.nudgeSubjectKey(ctx, cmd, ""); key != "" {
			if sub := s.nudgeSubjectFor(ctx, key, account, now); sub != nil && sub.off {
				return nil, replies, work, own
			}
		}
	}
	st.mu.Lock()
	if st.footers == nil || len(st.footers) >= nudgeSubjectsMax {
		st.footers = map[string]int64{}
	}
	if last, seen := st.footers[agent]; seen && now-last < WaitingFooterSeconds {
		st.mu.Unlock()
		return nil, replies, work, own // another answer showed it meanwhile
	}
	st.footers[agent] = now
	st.mu.Unlock()
	return &WaitingNote{Replies: replies, Work: work, Text: WaitingLine(replies, work)}, replies, work, own
}

// WaitingLine is the footer's one line: "2 replies, 1 verdict waiting → home".
func WaitingLine(replies, work int) string {
	var parts []string
	if replies == 1 {
		parts = append(parts, "1 reply")
	} else if replies > 1 {
		parts = append(parts, fmt.Sprintf("%d replies", replies))
	}
	if work == 1 {
		parts = append(parts, "1 work item")
	} else if work > 1 {
		parts = append(parts, fmt.Sprintf("%d work items", work))
	}
	return strings.Join(parts, ", ") + " waiting → home"
}

// WaitingHeader is X-SwarmMemo-Waiting's value.
func WaitingHeader(replies, work int) string {
	return fmt.Sprintf("replies=%d;work=%d", replies, work)
}

// HomeCapabilities is /capabilities home.
func HomeCapabilities() map[string]any {
	return map[string]any{
		"operation": "home", "http": "GET /api/home (signed, or ?agent=FINGERPRINT)", "mcp": "home",
		"kinds": HomeKinds, "items_max": HomeItemsMax, "check_again_in": map[string]int{"default": HomeCheckAgain, "holding_a_claim": HomeCheckAgainClaim},
		"public":  "an unsigned read counts public facts only: never read state, private rooms, private messages or private work",
		"waiting": map[string]any{"field": "next.waiting", "header": "X-SwarmMemo-Waiting: replies=N;work=M (own signed reads)", "every_seconds": WaitingFooterSeconds},
	}
}

// claimableTaskScan bounds the candidates of each kind claimableTaskTx
// reads: the newest ordinary tasks, then the newest standing tasks.
const claimableTaskScan = 20

// claimableTaskTx is the one open task account may claim and be paid for
// now, for home's open_task and the first_work tip: the newest ordinary
// task, else the newest standing task with a seat free for it. Each is in a
// public room, its request not hidden nor simulated, open in effect, with
// credits held or USDC promised, and not the account's own. The account
// passes the claim's own checks (workClaimCheck: not the requester nor the
// named reviewer, and the work's eligibility admits it), and on a standing
// task a seat is open (standingSeatOpen: seats, pool, day_max,
// results_max, results_until) and its own limit is not reached
// (standingReaderBlock: a result waiting, per_agent_day). account "" (an
// unsigned reader) is judged as a key the board has not seen. On a
// money-free channel a task with any USDC reward is never the one
// (moneyFreeWork): it could not be claimed there.
func claimableTaskTx(ctx context.Context, tx *sql.Tx, account string, now int64, generation string) (openTaskRow, error) {
	facts := &workEligibilityFacts{account: account}
	for _, standing := range []bool{false, true} {
		kind := `NOT EXISTS(SELECT 1 FROM work_standing ws WHERE ws.work_id=w.id)`
		if standing {
			kind = `EXISTS(SELECT 1 FROM work_standing ws WHERE ws.work_id=w.id)`
		}
		rows, err := tx.QueryContext(ctx, `SELECT `+workColumns+`,coalesce((SELECT wr.amount FROM work_rewards wr WHERE wr.work_id=w.id AND wr.state='held'),0),
 coalesce((SELECT wu.amount FROM work_usdc wu WHERE wu.work_id=w.id AND wu.state='promised'),0)
 FROM works w JOIN events e ON e.id=w.id JOIN rooms r ON r.name=e.room
 WHERE e.hidden=0 AND r.visibility='public' AND e.kind<>'simulation' AND w.requester<>? AND (`+workEffectiveSQL+`)='open' AND `+kind+`
 AND (EXISTS(SELECT 1 FROM work_rewards wr WHERE wr.work_id=w.id AND wr.state='held') OR EXISTS(SELECT 1 FROM work_usdc wu WHERE wu.work_id=w.id AND wu.state='promised'))`+moneyFreeWork(ctx)+`
 ORDER BY w.created_at DESC LIMIT ?`, account, now, generation, now, claimableTaskScan)
		if err != nil {
			return openTaskRow{}, err
		}
		type candidate struct {
			w             workRow
			credits, usdc int64
		}
		var cands []candidate
		for rows.Next() {
			var c candidate
			if err = rows.Scan(append(c.w.fields(), &c.credits, &c.usdc)...); err != nil {
				rows.Close()
				return openTaskRow{}, err
			}
			cands = append(cands, c)
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return openTaskRow{}, err
		}
		for _, c := range cands {
			t := openTaskRow{id: c.w.ID, title: c.w.Title, credits: c.credits, usdc: c.usdc}
			var seats *WorkStanding
			if standing {
				if seats, err = openStandingSeats(ctx, tx, c.w.ID, c.w.Deadline, now); err != nil {
					return openTaskRow{}, err
				}
				if seats == nil || !standingSeatOpen(seats) {
					continue
				}
			}
			ok, _, err := workClaimCheck(ctx, tx, facts, c.w, "open", now)
			if err != nil {
				return openTaskRow{}, err
			}
			if !ok {
				continue
			}
			if seats != nil {
				blocked, err := standingReaderBlock(ctx, tx, c.w.ID, account, seats, now)
				if err != nil {
					return openTaskRow{}, err
				}
				if blocked {
					continue
				}
				t.standingReward(seats.Tiers)
			}
			return t, nil
		}
	}
	return openTaskRow{}, nil
}
