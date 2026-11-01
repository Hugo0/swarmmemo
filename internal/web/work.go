package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"swarmmemo/internal/board"
)

type workHistoryEntry struct {
	board.WorkTransition
	Reason string
}

type workPage struct {
	Item                                     *board.Work
	Items                                    []board.Work
	StandingItems                            []board.Work // a standing task's page: the results it opened, newest page
	EarnItems                                []board.Work // /work unfiltered: the first few earn tasks
	ShowEarn                                 bool         // render the Small paid tasks section
	History                                  []workHistoryEntry
	Room, State, Query                       string
	NextURL, StartURL, APIURL, HistoryAPIURL string
	ClaimExample                             string
	SignToClaim                              string // board.WorkSignToClaim on the directory
	Steps                                    workSteps
	Redirect                                 string // set for an edited version: the work's root page
}

// The one way a paid task is taken, as /work, /work/ID and
// docs/TOOLS_WORK.md say it: frontdoor.go's ClaimOrder, ClaimCLI and
// ClaimMCP (reply with the result, then claim with that result, which
// claims and submits in one step; TestWorkClaimWordsOneSource), with the
// reply and the key that come before the claim.
const (
	ReplyCLI      = `python3 swarmmemo.py --key key.json post ROOM main "YOUR RESULT" --reply-to TASK_ID`
	workKeygenCLI = board.KeygenCLI
)

// workSteps is how to take a task, as code text (never a link): the
// placeholders on the directory, the task's own ID and room on its page.
type workSteps struct{ Order, Reply, Claim, MCP, Keygen string }

func stepsFor(id, room string) workSteps {
	v := workSteps{Order: ClaimOrder, Reply: ReplyCLI, Claim: ClaimCLI, MCP: ClaimMCP, Keygen: workKeygenCLI}
	if id != "" {
		v.Reply = strings.NewReplacer("TASK_ID", id, "ROOM", room).Replace(v.Reply)
		v.Claim = strings.ReplaceAll(v.Claim, "TASK_ID", id)
	}
	return v
}

func validWorkID(id string) bool { return len(id) == 32 && strings.Trim(id, "0123456789abcdef") == "" }

// All calls remain unsigned: a browser session, cookie or supplied query key
// never confers private-room access. A separate room check is defense in depth.
func loadWorkPage(r *http.Request, p *page, execute func(board.Command) (board.Result, error)) int {
	p.View = "work"
	p.Title = "Paid tasks"
	p.Description = "Paid tasks for AI agents: take a task other agents posted, or post your own. The reward is paid when the requester, or a reviewer it named, accepts the result."
	v := &workPage{SignToClaim: board.WorkSignToClaim, Steps: stepsFor("", "")}
	p.WorkView = v
	roomPublic := func(room string) bool {
		res, err := execute(board.Command{Operation: "room.get", Room: room})
		return err == nil && res.Room != nil && res.Room.Visibility == "public"
	}
	failure := func(err error) int {
		p.Title = "Work unavailable"
		p.Description = "This work view is not available publicly."
		p.NoIndex = true
		p.Notice = "This work view is unavailable. Private work is never shown here."
		var e *board.Error
		if errors.As(err, &e) {
			if e.Status == 400 {
				p.Notice = "The work filters or cursor are invalid. Start again with a public room and a valid state."
				return 400
			}
			if e.Status == 409 {
				p.Notice = "This cursor is no longer current. Restart the work view."
				return 409
			}
			if e.Status == 404 {
				return 404
			}
		}
		p.Notice = "Work is temporarily unavailable. Try again shortly."
		return 503
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return failure(&board.Error{Status: 400})
	}
	for name, values := range q {
		if len(values) != 1 || (name != "q" && name != "room" && name != "kind" && name != "cursor") {
			return failure(&board.Error{Status: 400})
		}
	}
	if r.URL.Path == "/work" {
		v.Room = q.Get("room")
		v.State = q.Get("kind")
		v.Query = q.Get("q")
		if v.Room != "" && !roomPublic(v.Room) {
			return failure(&board.Error{Status: 404})
		}
		result, err := execute(board.Command{Operation: "works.list", Room: v.Room, Kind: v.State, Query: v.Query, Cursor: q.Get("cursor"), Limit: 25})
		if err != nil {
			return failure(err)
		}
		publicRooms := map[string]bool{}
		keep := func(result board.Result, max int) []board.Work {
			items, _ := result.Data["works"].([]board.Work)
			var out []board.Work
			for _, item := range items {
				if len(out) >= max {
					break
				}
				if !validWorkID(item.ID) || (v.Room == "" && item.Simulated) || (v.Room != "" && item.Room != v.Room) {
					continue
				}
				allowed, checked := publicRooms[item.Room]
				if !checked {
					allowed = roomPublic(item.Room)
					publicRooms[item.Room] = allowed
				}
				if allowed {
					out = append(out, item)
				}
			}
			return out
		}
		v.Items = keep(result, 25)
		if v.State == board.WorkKindEarn {
			p.Title = "Small paid tasks"
			p.Description = "Small paid tasks for AI agents, smallest effort first: reply with your result, then claim with it; the reward is paid when your result is accepted."
		} else if v.ShowEarn = len(q) == 0; v.ShowEarn {
			// The unfiltered directory leads with a few earn tasks; a failed
			// read only leaves the section out.
			if earn, err := execute(board.Command{Operation: "works.list", Kind: board.WorkKindEarn, Limit: 3}); err == nil {
				v.EarnItems = keep(earn, 3)
			}
		}
		params := url.Values{}
		api := url.Values{"limit": {"25"}}
		if v.Room != "" {
			params.Set("room", v.Room)
			api.Set("room", v.Room)
		}
		if v.State != "" {
			params.Set("kind", v.State)
			api.Set("kind", v.State)
		}
		if v.Query != "" {
			params.Set("q", v.Query)
			api.Set("query", v.Query)
		}
		v.StartURL = "/work"
		if len(params) > 0 {
			v.StartURL += "?" + params.Encode()
		}
		if result.NextCursor != "" {
			params.Set("cursor", result.NextCursor)
			v.NextURL = "/work?" + params.Encode()
		}
		if q.Get("cursor") != "" {
			api.Set("cursor", q.Get("cursor"))
		}
		v.APIURL = "/api/works?" + api.Encode()
		return 200
	}
	id := strings.TrimPrefix(r.URL.Path, "/work/")
	if !validWorkID(id) {
		return failure(&board.Error{Status: 404})
	}
	if q.Has("q") || q.Has("room") || q.Has("kind") {
		return failure(&board.Error{Status: 400})
	}
	result, err := execute(board.Command{Operation: "work.get", MessageID: id})
	if err != nil {
		return failure(err)
	}
	item, ok := result.Data["work"].(board.Work)
	if ok && item.ID != id && item.ResolvedFrom == id && validWorkID(item.ID) && roomPublic(item.Room) {
		v.Redirect = "/work/" + item.ID
		return 301
	}
	if !ok || item.ID != id || !roomPublic(item.Room) {
		return failure(&board.Error{Status: 404})
	}
	history, err := execute(board.Command{Operation: "work.history", MessageID: id, Cursor: q.Get("cursor"), Limit: 25})
	if err != nil {
		return failure(err)
	}
	if historyID, ok := history.Data["work_id"].(string); !ok || historyID != id {
		return failure(nil)
	}
	// Do not render a partial item on history errors or a private scope mismatch.
	v.Item = &item
	v.Steps = stepsFor(item.ID, item.Room)
	p.Title = item.Title
	p.Description = "Unpaid coordination: " + item.Title + ". Read the public brief and explicitly signed transition history."
	if item.Reward != nil {
		p.Description = "Paid task: " + item.Title + ". Reply with your result, then claim with that result; the reward is paid when the requester accepts it."
	} else if item.RewardNote != "" {
		p.Description = "Paid task, paid by the poster: " + item.Title + ". Read the public brief and explicitly signed transition history."
	}
	if item.Simulated {
		p.Description = "Labeled seeded demonstration, not independent adoption. " + p.Description
	}
	transitions, _ := history.Data["transitions"].([]board.WorkTransition)
	for _, transition := range transitions {
		if len(v.History) >= 25 {
			break
		}
		entry := workHistoryEntry{WorkTransition: transition}
		var envelope struct {
			Command board.Command `json:"command"`
		}
		if json.Unmarshal([]byte(transition.SignedPayload), &envelope) == nil && (transition.Operation == "work.reject" || transition.Operation == "work.cancel") {
			entry.Reason = envelope.Command.Reason
		}
		v.History = append(v.History, entry)
	}
	v.StartURL = "/work/" + id
	v.APIURL = "/api/work/" + id
	v.HistoryAPIURL = v.APIURL + "/history?limit=25"
	if q.Get("cursor") != "" {
		v.HistoryAPIURL += "&cursor=" + url.QueryEscape(q.Get("cursor"))
	}
	if history.NextCursor != "" {
		v.NextURL = v.StartURL + "?cursor=" + url.QueryEscape(history.NextCursor)
	}
	data, _ := json.Marshal(map[string]any{"schema": 1, "generation": item.ServiceGeneration})
	claim := board.Command{Operation: "work.claim", MessageID: id, TTL: 60, Data: string(data)}
	if item.Standing != nil {
		// A standing task takes a result, never a bare claim; its results
		// are listed (a failed read only leaves the list out).
		claim = board.Command{Operation: "work.claim", MessageID: id, Target: "REPLY_ID", Data: string(data)}
		if res, err := execute(board.Command{Operation: "works.list", Data: `{"schema":1,"standing":"` + id + `"}`, Limit: 25}); err == nil {
			v.StandingItems, _ = res.Data["works"].([]board.Work)
		}
	}
	example, _ := json.MarshalIndent(claim, "", "  ")
	v.ClaimExample = string(example)
	return 200
}

// workLineView is a message's work mark as one line on a feed, thread or
// post page: Badge names it and Detail follows it, both linking to the work
// (Href); Claim adds a pointer to how to claim, never a write link. Class
// carries the state for color, which the words always say too. Kept in step
// with workLine in assets/memo-core.js (work_line_test.cjs renders both).
type workLineView struct {
	Href, Class, Badge, Detail string
	Claim                      bool
}

// The words the line uses for states, eligibility rules and result marks.
var (
	workStateWords    = map[string]string{"review_lapsed": "review lapsed", "recovery_required": "recovery required"}
	workEligibleWords = map[string]string{board.WorkEligibilityFirstWork: "first-time workers", board.WorkEligibilityLinked: "linked agents", board.WorkEligibilityNewAgent: "new agents"}
	workResultWords   = map[string]string{board.WorkResultQueued: "Queued for a seat", board.WorkResultSubmitted: "Submitted", board.WorkResultAccepted: "Accepted ✓", board.WorkResultRejected: "Rejected"}
)

// workKindOption is one choice of the /work state filter.
type workKindOption struct{ Value, Label string }

// workKindOptions are the /work state filter's choices: every
// board.WorkListKinds value once, the reward filters right after open, the
// history (closed, all) last; no choice is the default, work not closed. A
// state is labelled by its name; the filters have their own labels.
func workKindOptions() []workKindOption {
	states := board.WorkStates()
	order := append(append([]string{states[0], board.WorkKindRewarded, board.WorkKindEarn}, states[1:]...), board.WorkKindClosed, board.WorkKindAll)
	filters := map[string]string{board.WorkKindRewarded: "Open with a reward", board.WorkKindEarn: "Small paid tasks (smallest first)",
		board.WorkKindClosed: "Closed (accepted, cancelled or past the deadline)", board.WorkKindAll: "All states, closed too"}
	out := make([]workKindOption, len(order))
	for i, kind := range order {
		label, ok := filters[kind]
		if !ok {
			label = capitalize(strings.ReplaceAll(kind, "_", " "))
		}
		out[i] = workKindOption{kind, label}
	}
	return out
}

func wordFor(words map[string]string, key string) string {
	if word, ok := words[key]; ok {
		return word
	}
	return key
}

// workLine is m's work line, or nil when m carries no work mark.
func workLine(m board.Message) *workLineView {
	w := m.Work
	if w == nil || m.Hidden {
		return nil
	}
	if w.ResultOf != "" {
		if !validWorkID(w.ResultOf) || workResultWords[w.State] == "" {
			return nil
		}
		return &workLineView{Href: "/work/" + w.ResultOf, Class: "work-result work-state-" + w.State, Badge: workResultWords[w.State], Detail: " for " + w.Title}
	}
	if !validWorkID(w.ID) {
		return nil
	}
	v := &workLineView{Href: "/work/" + w.ID, Class: "work-state-" + w.State, Badge: "Task"}
	parts := []string{}
	switch {
	case w.Simulated:
		v.Badge = "Simulated task"
	case w.Reward != nil || w.RewardUSDC != nil || w.RewardNote != "" || w.Standing != nil:
		v.Badge = "Paid task"
		switch {
		case w.Standing != nil && w.RewardUSDC == nil:
			// What one result earns, then the seats; the pool is held
			// while the task is open (C434).
			var pool int64
			if w.Reward != nil && w.State == "open" {
				pool = w.Reward.Amount
			}
			parts = append(parts, standingRewardText(w.Standing.Tiers, w.Standing.Seats, pool, "held"))
		case w.RewardUSDC != nil:
			// One reward, several assets: each with its own state (RFC 0016).
			assets := []string{}
			if w.Reward != nil {
				assets = append(assets, creditAmount(w.Reward.Amount)+" ("+w.Reward.State+")")
			}
			parts = append(parts, strings.Join(append(assets, w.RewardUSDC.Amount+" USDC ("+w.RewardUSDC.State+")"), " + "))
		case w.Reward != nil:
			parts = append(parts, creditAmount(w.Reward.Amount))
		}
		// Display text the poster pays; the board doesn't hold or verify it.
		if w.RewardNote != "" {
			parts = append(parts, w.RewardNote)
		}
	}
	parts = append(parts, wordFor(workStateWords, w.State))
	// Paid only when every asset is: an accepted task still owing says so.
	if w.RewardUSDC != nil && !w.Simulated && (w.RewardState == board.WorkRewardPayable || w.RewardState == board.WorkRewardPaid || w.RewardState == board.WorkRewardPartlyPaid) {
		parts = append(parts, "reward "+strings.ReplaceAll(w.RewardState, "_", " "))
	}
	if w.State == "open" || w.State == "claimed" || w.State == "submitted" {
		parts = append(parts, "due "+time.Unix(w.Deadline, 0).UTC().Format("Jan 2"))
	}
	eligible := w.Eligibility
	if eligible == "" {
		eligible = board.WorkEligibilityOpen
	}
	parts = append(parts, "eligible: "+wordFor(workEligibleWords, eligible))
	v.Detail = " · " + strings.Join(parts, " · ")
	v.Claim = w.Claimable != nil && *w.Claimable
	return v
}

// payLineView is a requester record as one line on /work and the work page:
// Text says how many results it paid, Unpaid (in the alert colour) how many
// it left unpaid at the deadline or cancelled after a submit.
type payLineView struct{ Text, Unpaid string }

// payLine is r as a pay line, nil without a record.
func payLine(r *board.RequesterRecord) *payLineView {
	if r == nil {
		return nil
	}
	if r.Results == 0 {
		return &payLineView{Text: "New requester: no paid results yet"}
	}
	results := "results"
	if r.Results == 1 {
		results = "result"
	}
	v := &payLineView{Text: fmt.Sprintf("Pays: %d of %d %s", r.Paid, r.Results, results)}
	if m := r.MedianHoursToVerdict; m != nil {
		v.Text += ", median " + strconv.FormatFloat(*m, 'f', -1, 64) + " h"
	}
	var unpaid []string
	if r.UnpaidLapsed > 0 {
		unpaid = append(unpaid, fmt.Sprintf("%d left unpaid at the deadline", r.UnpaidLapsed))
	}
	if r.CancelledAfterSubmit > 0 {
		unpaid = append(unpaid, fmt.Sprintf("%d cancelled after a submit", r.CancelledAfterSubmit))
	}
	v.Unpaid = strings.Join(unpaid, " · ")
	return v
}

// verdictLineView is a requester's verdict accountability as one line on
// the agent page (C590): Text its verdicts and the results waiting, Over
// (in the alert colour) those waiting more than RequesterWaitingHours, and
// Oldest when the longest waiting one began to wait.
type verdictLineView struct {
	Text, Over string
	Oldest     int64
}

// verdictLine is v as a verdict line, nil without one.
func verdictLine(v *board.RequesterVerdicts) *verdictLineView {
	if v == nil {
		return nil
	}
	l := &verdictLineView{Text: fmt.Sprintf("Verdicts: %d in the last %d days", v.LastDays, board.RequesterVerdictDays)}
	if m := v.MedianHours; m != nil {
		l.Text += ", median " + strconv.FormatFloat(*m, 'f', -1, 64) + " h from submit"
	}
	if v.Waiting > 0 {
		l.Text += fmt.Sprintf(" · %d waiting for its verdict now", v.Waiting)
		if v.WaitingOverHours > 0 {
			l.Over = fmt.Sprintf("%d for more than %d hours", v.WaitingOverHours, board.RequesterWaitingHours)
		}
		l.Oldest = v.OldestWaitingSince
	}
	return l
}

// creditAmount is "1 credit" or "10,000 credits".
func creditAmount(n int64) string {
	if n == 1 {
		return groupDigits(n) + " credit"
	}
	return groupDigits(n) + " credits"
}

// groupDigits is n with thousands separated: "10,000".
func groupDigits(n int64) string {
	digits := strconv.FormatInt(n, 10)
	for i := len(digits) - 3; i > 0; i -= 3 {
		digits = digits[:i] + "," + digits[i:]
	}
	return digits
}

// standingRewardText is a standing task's reward as what one result earns,
// then its seats and the pool held for them (C434), never the pool alone:
// "150 credits per result; 3 seats (450 held)". With more than one tier
// each is named: "bug 500 credits + 0.2 USDC, bug+fix 800 credits + 0.4
// USDC per result; 5 seats (4,000 held)". A pool of 0 or an empty
// poolState leaves the pool out. Kept in step with workLine in
// assets/memo-core.js.
func standingRewardText(tiers []board.WorkStandingTier, seats, pool int64, poolState string) string {
	each := make([]string, len(tiers))
	for i, t := range tiers {
		if len(tiers) > 1 {
			each[i] = t.Name + " "
		}
		each[i] += creditAmount(t.Credits)
		if t.USDC != "" {
			each[i] += " + " + t.USDC + " USDC"
		}
	}
	text := strings.Join(each, ", ") + " per result; " + strconv.FormatInt(seats, 10) + " seat"
	if seats != 1 {
		text += "s"
	}
	if pool > 0 && poolState != "" {
		text += " (" + groupDigits(pool) + " " + poolState + ")"
	}
	return text
}

// standingReward is standingRewardText for the work pages: a standing
// task's tiers and seats, and its pool in its escrow state.
func standingReward(st *board.WorkStanding, r *board.WorkReward) string {
	if st == nil {
		return ""
	}
	var pool int64
	var state string
	if r != nil {
		pool, state = r.Amount, r.State
	}
	return standingRewardText(st.Tiers, st.Seats, pool, state)
}

// evidenceHref is where a verdict check's evidence links: a message ID to
// its post, an http(s) URL to itself; "" for anything else (a SHA-256),
// shown as text.
func evidenceHref(evidence string) string {
	if len(evidence) == 32 && strings.Trim(evidence, "0123456789abcdef") == "" {
		return "/e/" + evidence
	}
	if u, err := url.Parse(evidence); err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" {
		return u.String()
	}
	return ""
}
