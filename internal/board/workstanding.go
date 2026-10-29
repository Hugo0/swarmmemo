package board

// Standing tasks (C278): one paid task that takes many results. The
// requester opens its root request with work.create data seats (and
// optionally per_agent_day, day_max, up to three reward tiers and daily USDC
// caps). A worker does what it does for any task: post the result as a
// reply to the root, then work.claim the root with target = that reply. On a
// standing task that claim does not claim the root: it opens a new work item
// whose ID is the reply's, with the worker, fence 1, state submitted and the
// reply as its result, and from then on it is ordinary work (accept, settle,
// history, receipts, records, inbox).
//
// Money. The root holds a credit pool: the largest tier's credits times
// seats, escrowed once at create. Opening a result moves one reward from the
// pool into the result's own escrow (EscrowSplit: no second fee). An accept
// pays the tier it names (default the first) and returns any excess; a reject
// or a lapse returns the whole reward to the pool while the pool lives, else
// to the requester. After an accept the requester's credits refill the pool
// by one reward (best effort: without them the pool is a seat short). USDC is
// promised per result, never on the root; at accept it is owed in the tier's
// amount, unless the day's USDC caps (usdc_per_agent_day, usdc_day_max) are
// reached, or the USDC owed and unsettled on the task's results would pass
// seats times the largest tier's USDC (C386): then the result is paid in
// credits only and the acknowledgement says so. That last cap bounds what a
// requester can owe on one standing task; settling lowers it.
//
// Seats. A seat holds a result waiting for a verdict; an accepted result
// frees its seat at accept, its USDC paid or owed (C386). Each result has
// its own verdict deadline, WorkStandingVerdictTTL after it opens; undecided by then it lapses (its
// seat and reward return, and it counts as unpaid_lapsed on the requester's
// record). A result whose reply is hidden gives its seat and reward back
// as cancelled, reason hidden, and is left out of that record (C281). A
// reject is terminal for that result (stored cancelled; its last
// transition is the reject). The root takes results until its deadline
// leaves less than the verdict window.
//
// Results cap (C346). Optional results_max bounds the results a standing
// task takes over its life: those accepted (paid or USDC owed) and those
// waiting for a verdict. Rejected, lapsed and hidden results do not count.
// Once they reach it the task is full: standing.open reads 0, standing.full
// true, and a claim is 409 results_max_reached. The requester stops a
// standing task early with work.cancel on the task itself.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/services"
)

const workStandingSchema = `
CREATE TABLE IF NOT EXISTS work_standing (
 work_id TEXT PRIMARY KEY REFERENCES works(id), seats INTEGER NOT NULL, per_agent_day INTEGER NOT NULL,
 day_max INTEGER NOT NULL, tiers TEXT NOT NULL, usdc_per_agent_day INTEGER NOT NULL DEFAULT 0,
 usdc_day_max INTEGER NOT NULL DEFAULT 0, verdict_ttl INTEGER NOT NULL);
`

// Bounds of a standing task.
const (
	WorkStandingSeatsMax = 50
	WorkStandingDayMax   = 1000
	WorkStandingTiersMax = 3
	// WorkStandingResultsMax bounds results_max, the results a standing
	// task takes over its life (C346).
	WorkStandingResultsMax = 10000
	WorkStandingTierFirst  = "reward" // the one tier a reward (no tiers) makes
	// WorkStandingVerdictTTL is each result's own verdict deadline, from the
	// claim that opened it: 3 days.
	WorkStandingVerdictTTL int64 = 3 * 86400
)

var workStandingTierRE = regexp.MustCompile(`^[a-z0-9][a-z0-9+_.-]{0,31}$`)

// WorkStandingTier is one reward a verdict on a standing task's result may
// pay: credits (at least 1) and optionally USDC, a decimal string.
type WorkStandingTier struct {
	Name    string `json:"name"`
	Credits int64  `json:"credits"`
	USDC    string `json:"usdc,omitempty"`
}

func (t WorkStandingTier) usdcUnits() int64 {
	if t.USDC == "" {
		return 0
	}
	u, _ := services.ParseUSDC(t.USDC)
	return u
}

// workStandingData is a standing create's data, parsed.
type workStandingData struct {
	Seats, PerAgentDay, DayMax          int64
	ResultsMax                          int64 // 0: no cap
	Tiers                               []WorkStandingTier
	USDCPerAgentDayText, USDCDayMaxText string
	USDCPerAgentDay, USDCDayMax         int64
}

// workStanding is a standing root's stored configuration.
type workStanding struct {
	ID                          string
	Seats, PerAgentDay, DayMax  int64
	ResultsMax                  int64 // 0: no cap (C346)
	Tiers                       []WorkStandingTier
	USDCPerAgentDay, USDCDayMax int64
	VerdictTTL                  int64
}

// maxCredits is the largest tier's credits: what one seat holds.
func (st *workStanding) maxCredits() int64 {
	var m int64
	for _, t := range st.Tiers {
		m = max(m, t.Credits)
	}
	return m
}

func (st *workStanding) maxUSDC() int64 {
	var m int64
	for _, t := range st.Tiers {
		m = max(m, t.usdcUnits())
	}
	return m
}

// WorkStandingSeats is a standing root's seat summary and the one source of
// its names (C302): work.get standing, works.list data standing
// (?standing=ID) and MCP find_work's compact rows all carry this struct.
// Open is the seats a result can take now; Submitted the results waiting for
// a verdict; Owed the accepted results whose USDC is not yet settled; Paid
// the other accepted results; PerAgentDay the results one agent may open a
// day; Full, sent only when true, that the task holds all the results its
// results_max allows and takes no more claims (C346). PerResult is the
// credits one accepted result is paid by default (the first tier; the
// task's reward is the pool held for every seat); NextOpenAt, sent only
// while a result could not open now, when that lifts by itself (C384,
// standingNextOpen). BlockedBy, sent only while a result could not open
// now, is why (one of WorkStandingBlockedBy), and BlockedNote says it in a
// line, for the reads a worker previews before doing the work. The reading
// agent's own limit (own_result_waiting, per_agent_day) shows only on a
// read for that agent, while seats are open to others (C444).
type WorkStandingSeats struct {
	Seats       int64  `json:"seats"`
	Open        int64  `json:"open"`
	Submitted   int64  `json:"submitted"`
	Owed        int64  `json:"owed"`
	Paid        int64  `json:"paid"`
	PerAgentDay int64  `json:"per_agent_day"`
	Full        bool   `json:"full,omitempty"`
	PerResult   int64  `json:"per_result"`
	NextOpenAt  int64  `json:"next_open_at,omitempty"`
	BlockedBy   string `json:"blocked_by,omitempty"`
	BlockedNote string `json:"blocked_note,omitempty"`
}

// The reasons a standing task takes no result now (standing.blocked_by), in
// the order a read names the first that applies: the task's own (what only
// a verdict or the requester lifts before the day's reset), then the
// reading agent's.
const (
	StandingBlockedResultsMax   = "results_max"
	StandingBlockedResultsUntil = "results_until"
	StandingBlockedSeats        = "seats_awaiting_verdict"
	StandingBlockedPool         = "pool_short"
	StandingBlockedDayMax       = "day_max"
	StandingBlockedOwnResult    = "own_result_waiting"
	StandingBlockedPerAgentDay  = "per_agent_day"
)

// WorkStandingBlockedBy is every standing.blocked_by, for the docs and
// /capabilities.
var WorkStandingBlockedBy = []string{StandingBlockedResultsMax, StandingBlockedResultsUntil, StandingBlockedSeats, StandingBlockedPool, StandingBlockedDayMax, StandingBlockedOwnResult, StandingBlockedPerAgentDay}

// WorkStandingSeatNames is WorkStandingSeats's wire names in order, read
// from its json tags, for the descriptions that list them.
func WorkStandingSeatNames() []string {
	kind := reflect.TypeFor[WorkStandingSeats]()
	names := make([]string, kind.NumField())
	for i := range names {
		names[i], _, _ = strings.Cut(kind.Field(i).Tag.Get("json"), ",")
	}
	return names
}

// WorkStanding is a standing root's seats as reads show them: the seat
// summary, flattened, and the rest. Results is every result it opened;
// ResultsMax, when set, the accepted and waiting results it takes in all;
// OpenedToday the day's results day_max counts (waiting or accepted, C614).
// USDCOwed and USDCOwedMax, sent when a tier pays USDC, are the USDC owed and
// not yet settled on its results and the most it may be (seats times the
// largest tier's): an accept that would pass it pays credits only (C386).
type WorkStanding struct {
	WorkStandingSeats
	Results         int64              `json:"results"`
	ResultsMax      int64              `json:"results_max,omitempty"`
	DayMax          int64              `json:"day_max"`
	OpenedToday     int64              `json:"opened_today"`
	Tiers           []WorkStandingTier `json:"tiers"`
	USDCPerAgentDay string             `json:"usdc_per_agent_day,omitempty"`
	USDCDayMax      string             `json:"usdc_day_max,omitempty"`
	USDCOwed        string             `json:"usdc_owed,omitempty"`
	USDCOwedMax     string             `json:"usdc_owed_max,omitempty"`
	VerdictTTL      int64              `json:"verdict_ttl"`
	ResultsUntil    int64              `json:"results_until"`
	// Successor is the standing task the requester named as the next round
	// (work.successor.set, C391); NextRound, sent only while this task is
	// not claimable, the first claimable task along that chain, at most
	// WorkStandingSuccessorDepth links on.
	Successor string `json:"successor,omitempty"`
	NextRound string `json:"next_round,omitempty"`
}

// WorkStandingSuccessorDepth bounds a successor chain: a successor whose
// own chain runs longer is refused, and next_round looks no further.
const WorkStandingSuccessorDepth = 8

// standingSuccessor is the successor the newest work.successor.set on a
// standing task named, or "": the signed transition is the record.
func standingSuccessor(ctx context.Context, tx *sql.Tx, id string) (string, error) {
	var payload string
	err := tx.QueryRowContext(ctx, `SELECT payload FROM work_transitions WHERE work_id=? AND operation=? ORDER BY sequence DESC LIMIT 1`, id, WorkSuccessorSet).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var envelope struct {
		Command struct {
			Data string `json:"data"`
		} `json:"command"`
	}
	var data struct {
		Successor string `json:"successor"`
	}
	if json.Unmarshal([]byte(payload), &envelope) != nil || json.Unmarshal([]byte(envelope.Command.Data), &data) != nil || !workIDRE.MatchString(data.Successor) {
		return "", fmt.Errorf("board: standing task %s has an unreadable successor", id)
	}
	return data.Successor, nil
}

func invalidSuccessor(msg string) error {
	return problem(400, "invalid_work_data", msg+" A successor is another standing task of yours, open, readable by everyone who reads this one (the same room or a public one), whose own successors never lead back here, at most "+fmt.Sprint(WorkStandingSuccessorDepth)+" links on.")
}

// checkStandingSuccessor admits next as w's successor (C391): another
// standing task (not a result) by the same requester, open now, in w's room
// or a public one, and no cycle: next's chain of successors, at most
// WorkStandingSuccessorDepth links, never returns to w.
func checkStandingSuccessor(ctx context.Context, tx *sql.Tx, w workRow, root workRoot, next, generation string, now int64) error {
	if next == w.ID {
		return invalidSuccessor("A standing task cannot be its own successor.")
	}
	n, err := scanWork(tx.QueryRowContext(ctx, `SELECT `+workColumns+` FROM works w WHERE id=?`, next))
	if errors.Is(err, sql.ErrNoRows) {
		return invalidSuccessor("The successor names no work.")
	}
	if err != nil {
		return err
	}
	st, err := loadWorkStanding(ctx, tx, next)
	if err != nil {
		return err
	}
	if st == nil || n.StandingID != "" {
		return invalidSuccessor("The successor is not a standing task.")
	}
	if n.Requester != w.Requester {
		return invalidSuccessor("The successor is another requester's task.")
	}
	var room, visibility string
	var hidden bool
	if err = tx.QueryRowContext(ctx, `SELECT e.room,e.hidden,coalesce(r.visibility,'') FROM events e LEFT JOIN rooms r ON r.name=e.room WHERE e.id=?`, next).Scan(&room, &hidden, &visibility); err != nil {
		return err
	}
	if hidden || (room != root.Room && visibility != "public") {
		return invalidSuccessor("The successor is not readable by this task's readers.")
	}
	if state, err := effectiveWork(ctx, tx, next, generation, now); err != nil {
		return err
	} else if state != "open" {
		return invalidSuccessor("The successor is " + state + ", not open.")
	}
	at := next
	for range WorkStandingSuccessorDepth {
		if at, err = standingSuccessor(ctx, tx, at); err != nil {
			return err
		}
		if at == "" {
			return nil
		}
		if at == w.ID {
			return invalidSuccessor("That would make a cycle: the successor's own successors lead back to this task.")
		}
	}
	return invalidSuccessor("The successor's chain is too long.")
}

// standingNextRound is the first claimable standing task along the chain
// that starts at next, at most WorkStandingSuccessorDepth links, or "": a
// task whose request is hidden is skipped over, never shown.
func standingNextRound(ctx context.Context, tx *sql.Tx, next string, now int64) (string, error) {
	var generation string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='generation'`).Scan(&generation); err != nil {
		return "", err
	}
	seen := map[string]bool{}
	for at := next; at != "" && !seen[at] && len(seen) < WorkStandingSuccessorDepth; {
		seen[at] = true
		w, err := scanWork(tx.QueryRowContext(ctx, `SELECT `+workColumns+` FROM works w WHERE id=?`, at))
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		var hidden bool
		if err = tx.QueryRowContext(ctx, `SELECT hidden FROM events WHERE id=?`, at).Scan(&hidden); err != nil {
			return "", err
		}
		state, err := effectiveWork(ctx, tx, at, generation, now)
		if err != nil {
			return "", err
		}
		if !hidden && state == "open" {
			p, err := projectWorkStandingSeats(ctx, tx, w, state, now)
			if err != nil {
				return "", err
			}
			if p != nil && p.Open > 0 {
				return at, nil
			}
		}
		if at, err = standingSuccessor(ctx, tx, at); err != nil {
			return "", err
		}
	}
	return "", nil
}

func invalidStanding(msg string) error {
	return problem(400, "invalid_work_data", msg+" "+WorkStandingRule)
}

// WorkStandingRule is a standing create's data, the line its refusals end with.
const WorkStandingRule = `A standing task is work.create data seats (1–50) with a reward that has credits, or tiers (1–3 of {"name":"bug","credits":N,"usdc":"0.20"}, each with credits); optional per_agent_day (default 1, at most seats), day_max (default seats, at most 1000), results_max (results accepted or waiting over its life, seats to 10000; default no cap), and usdc_per_agent_day and usdc_day_max (decimal USDC strings, each at least every tier's USDC). No reviewer, reviewer_fee or reject_fee.`

// parseWorkStandingTiers reads data tiers.
func parseWorkStandingTiers(raw json.RawMessage) ([]WorkStandingTier, error) {
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil || len(items) < 1 || len(items) > WorkStandingTiersMax {
		return nil, invalidStanding("tiers is a list of 1 to 3 tiers.")
	}
	out := []WorkStandingTier{}
	seen := map[string]bool{}
	for _, item := range items {
		var o struct {
			Name    *string `json:"name"`
			Credits *int64  `json:"credits"`
			USDC    *string `json:"usdc"`
		}
		if services.StrictObject(item, &o) != nil || o.Name == nil || o.Credits == nil {
			return nil, invalidStanding("Each tier is {\"name\":\"bug\",\"credits\":N} with an optional \"usdc\":\"0.20\".")
		}
		if !workStandingTierRE.MatchString(*o.Name) || seen[*o.Name] {
			return nil, invalidStanding("Tier names are unique, 1–32 lowercase letters, digits and +_.-, starting with a letter or digit.")
		}
		seen[*o.Name] = true
		if *o.Credits < 1 || *o.Credits > WorkRewardMax {
			return nil, invalidStanding(fmt.Sprintf("A tier's credits are whole, 1 to %d.", WorkRewardMax))
		}
		t := WorkStandingTier{Name: *o.Name, Credits: *o.Credits}
		if o.USDC != nil {
			u, ok := services.ParseUSDC(*o.USDC)
			if !ok || u < WorkUSDCMin || u > WorkUSDCMax {
				return nil, invalidStanding(fmt.Sprintf("A tier's usdc is a decimal string from %s to %s.", services.FormatUSDC(WorkUSDCMin), services.FormatUSDC(WorkUSDCMax)))
			}
			t.USDC = services.FormatUSDC(u)
		}
		out = append(out, t)
	}
	return out, nil
}

// checkWorkStandingData checks a create's standing fields together, and
// fills their defaults.
func checkWorkStandingData(d *workData, seen map[string]bool) error {
	set := false
	for _, name := range []string{"seats", "per_agent_day", "day_max", "results_max", "tiers", "usdc_per_agent_day", "usdc_day_max"} {
		set = set || seen[name]
	}
	if !set {
		return nil
	}
	st := &d.Standing
	if !seen["seats"] {
		return invalidStanding("per_agent_day, day_max, results_max, tiers and the USDC caps go with seats.")
	}
	if st.Seats < 1 || st.Seats > WorkStandingSeatsMax {
		return invalidStanding("seats is 1 to 50.")
	}
	if seen["reviewer"] || seen["reviewer_fee"] || seen["reject_fee"] {
		return invalidStanding("A standing task takes no reviewer, reviewer_fee or reject_fee: the requester judges each result.")
	}
	if !seen["per_agent_day"] {
		st.PerAgentDay = 1
	} else if st.PerAgentDay < 1 || st.PerAgentDay > st.Seats {
		return invalidStanding("per_agent_day is 1 to seats.")
	}
	if !seen["day_max"] {
		st.DayMax = st.Seats
	} else if st.DayMax < 1 || st.DayMax > WorkStandingDayMax {
		return invalidStanding("day_max is 1 to 1000.")
	}
	if seen["results_max"] && (st.ResultsMax < st.Seats || st.ResultsMax > WorkStandingResultsMax) {
		return invalidStanding(fmt.Sprintf("results_max is seats to %d.", WorkStandingResultsMax))
	}
	switch {
	case seen["reward"] && seen["tiers"]:
		return invalidStanding("Send reward or tiers, not both.")
	case seen["reward"]:
		if d.Reward < 1 {
			return invalidStanding("A standing task's reward has credits.")
		}
		t := WorkStandingTier{Name: WorkStandingTierFirst, Credits: d.Reward}
		if d.RewardUSDC > 0 {
			t.USDC = services.FormatUSDC(d.RewardUSDC)
		}
		st.Tiers = []WorkStandingTier{t}
	case !seen["tiers"]:
		return invalidStanding("A standing task pays each result: send reward or tiers.")
	}
	var maxUSDC int64
	for _, t := range st.Tiers {
		maxUSDC = max(maxUSDC, t.usdcUnits())
	}
	for _, c := range []struct {
		name string
		text string
		dest *int64
	}{{"usdc_per_agent_day", st.USDCPerAgentDayText, &st.USDCPerAgentDay}, {"usdc_day_max", st.USDCDayMaxText, &st.USDCDayMax}} {
		if !seen[c.name] {
			continue
		}
		u, ok := services.ParseUSDC(c.text)
		if !ok || u < WorkUSDCMin || u > WorkUSDCMax*WorkStandingDayMax {
			return invalidStanding(c.name + " is a decimal USDC string, at least 0.01.")
		}
		if maxUSDC == 0 {
			return invalidStanding(c.name + " goes with a tier that pays USDC.")
		}
		if u < maxUSDC {
			return invalidStanding("Each tier's USDC must fit within " + c.name + ".")
		}
		*c.dest = u
	}
	return nil
}

func loadWorkStanding(ctx context.Context, tx rowQuerier, id string) (*workStanding, error) {
	st := workStanding{ID: id}
	var tiers string
	err := tx.QueryRowContext(ctx, `SELECT seats,per_agent_day,day_max,results_max,tiers,usdc_per_agent_day,usdc_day_max,verdict_ttl FROM work_standing WHERE work_id=?`, id).
		Scan(&st.Seats, &st.PerAgentDay, &st.DayMax, &st.ResultsMax, &tiers, &st.USDCPerAgentDay, &st.USDCDayMax, &st.VerdictTTL)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal([]byte(tiers), &st.Tiers); err != nil || len(st.Tiers) == 0 {
		return nil, fmt.Errorf("board: standing task %s has no tiers (%v)", id, err)
	}
	return &st, nil
}

// createWorkStanding records a new standing root and holds its credit pool.
// The root itself promises no USDC: each result does.
func (s *Store) createWorkStanding(ctx context.Context, tx *sql.Tx, a actor, w workRow, d workData, now int64) error {
	st := workStanding{ID: w.ID, Seats: d.Standing.Seats, PerAgentDay: d.Standing.PerAgentDay, DayMax: d.Standing.DayMax, ResultsMax: d.Standing.ResultsMax, Tiers: d.Standing.Tiers,
		USDCPerAgentDay: d.Standing.USDCPerAgentDay, USDCDayMax: d.Standing.USDCDayMax, VerdictTTL: WorkStandingVerdictTTL}
	if st.maxUSDC() > 0 || st.USDCPerAgentDay > 0 || st.USDCDayMax > 0 {
		if err := refuseMoney(ctx, "A USDC reward"); err != nil {
			return err
		}
	}
	if st.maxUSDC() > 0 && !s.WorkUSDCEnabled() {
		return errUSDCRewardsUnavailable()
	}
	pool := st.maxCredits() * st.Seats
	if pool > WorkRewardMax {
		return invalidStanding(fmt.Sprintf("The pool, the largest tier's credits times seats, is at most %d credits.", WorkRewardMax))
	}
	tiers, _ := json.Marshal(st.Tiers)
	if _, err := tx.ExecContext(ctx, `INSERT INTO work_standing(work_id,seats,per_agent_day,day_max,results_max,tiers,usdc_per_agent_day,usdc_day_max,verdict_ttl) VALUES(?,?,?,?,?,?,?,?,?)`,
		st.ID, st.Seats, st.PerAgentDay, st.DayMax, st.ResultsMax, string(tiers), st.USDCPerAgentDay, st.USDCDayMax, st.VerdictTTL); err != nil {
		return err
	}
	return s.holdWorkEscrows(ctx, tx, a, w, pool, 0, now)
}

// standingCounts is a standing root's seats as they stand at now. today is
// the results opened since 00:00 UTC that hold a day_max slot: waiting for
// a verdict, or accepted (paid or USDC owed, even if hidden since). A
// rejected, lapsed or hidden waiting result frees its slot at once, so a
// premature claim cannot block a scarce task for the rest of the day
// (C614). mineToday is every result the worker opened today, rejected and
// hidden ones too: per_agent_day is that agent's own allowance, and a
// rejected claim spends it.
type standingCounts struct {
	waiting, owed, accepted, results, today, mineToday, mineWaiting int64
	mineWaitingID                                                   string
	nextVerdict                                                     int64
	owedUSDC                                                        int64 // USDC units accepted and not yet settled (C386)
}

// counted is the results a results_max counts: accepted (paid or USDC
// owed) and waiting for a verdict; rejected, lapsed and hidden ones do not.
func (c standingCounts) counted() int64 { return c.accepted + c.waiting }

// full says whether the standing task holds all the results its
// results_max allows (never without one).
func (st *workStanding) full(c standingCounts) bool {
	return st.ResultsMax > 0 && c.counted() >= st.ResultsMax
}

// errResultsMaxReached is a claim on a full standing task (C346).
func errResultsMaxReached(st *workStanding, c standingCounts) error {
	return &Error{Status: 409, Code: "results_max_reached", Message: fmt.Sprintf("This standing task is full: it takes %s in all (results_max) and holds %d accepted and %d waiting for a verdict, so it takes no more claims. A place opens only if the requester rejects a waiting result or one lapses undecided.",
		plural(st.ResultsMax, "result"), c.accepted, c.waiting), Details: map[string]any{"full": true, "blocked_by": StandingBlockedResultsMax}}
}

// standingHiddenSQL is true for a result whose reply is hidden at any
// version (the item's key, its first version, or an edit superseding it):
// by the operator, the room, or its author's erasure. A hide on any version
// removes the message (postdata.go), so it is the rule eligibleWorkResult
// applies to the bound version too. Such a waiting result holds no seat and leaves the day's day_max (it
// still counts toward its worker's per_agent_day), its seat and reward
// return on the next claim or sweep (cancelled, reason hidden), and it is
// left out of the requester's record: the requester cannot read it to judge
// it (C281).
// Each half reads one index: the key by primary key, its edits by
// events_origin (a partial index, hence origin<>'').
const standingHiddenSQL = `(EXISTS(SELECT 1 FROM events h WHERE h.id=w.id AND h.hidden<>0) OR EXISTS(SELECT 1 FROM events h WHERE h.origin=w.id AND h.origin<>'' AND h.hidden<>0))`

func readStandingCounts(ctx context.Context, tx rowQuerier, id, worker string, now int64) (standingCounts, error) {
	var c standingCounts
	day := now - now%86400
	err := tx.QueryRowContext(ctx, `SELECT
 count(*) FILTER (WHERE w.state='submitted' AND w.deadline>? AND NOT `+standingHiddenSQL+`),
 count(*) FILTER (WHERE w.state='accepted' AND EXISTS(SELECT 1 FROM work_usdc u WHERE u.work_id=w.id AND u.state='payable')),
 count(*) FILTER (WHERE w.state='accepted'),
 count(*),
 count(*) FILTER (WHERE w.created_at>=? AND (w.state='accepted' OR (w.state='submitted' AND w.deadline>? AND NOT `+standingHiddenSQL+`))),
 count(*) FILTER (WHERE w.created_at>=? AND w.worker=?),
 count(*) FILTER (WHERE w.state='submitted' AND w.deadline>? AND w.worker=? AND NOT `+standingHiddenSQL+`),
 coalesce(min(w.id) FILTER (WHERE w.state='submitted' AND w.deadline>? AND w.worker=? AND NOT `+standingHiddenSQL+`),''),
 coalesce(min(w.deadline) FILTER (WHERE w.state='submitted' AND w.deadline>? AND NOT `+standingHiddenSQL+`),0)
 FROM works w WHERE w.standing_id=?`, now, day, now, day, worker, now, worker, now, worker, now, id).
		Scan(&c.waiting, &c.owed, &c.accepted, &c.results, &c.today, &c.mineToday, &c.mineWaiting, &c.mineWaitingID, &c.nextVerdict)
	if err != nil {
		return c, err
	}
	c.owedUSDC, err = standingOwedUSDC(ctx, tx, id)
	return c, err
}

// standingOwedUSDC is the USDC (units) owed and not yet settled on a
// standing task's accepted results: what its owed cap counts (C386).
func standingOwedUSDC(ctx context.Context, tx rowQuerier, id string) (int64, error) {
	var owed int64
	err := tx.QueryRowContext(ctx, `SELECT coalesce(sum(u.amount),0) FROM work_usdc u JOIN works w ON w.id=u.work_id WHERE w.standing_id=? AND u.state='payable'`, id).Scan(&owed)
	return owed, err
}

// owedUSDCMax is the USDC a requester may owe, unsettled, on one standing
// task's results at once: seats times the largest tier's USDC (C386).
func (st *workStanding) owedUSDCMax() int64 { return st.Seats * st.maxUSDC() }

// standingPool is the root's credit pool: what it holds now, or -1 when it
// no longer holds (cancelled, or released at the deadline).
func standingPool(ctx context.Context, tx rowQuerier, id string) (*rewardRow, int64, error) {
	r, err := loadWorkReward(ctx, tx, workRewardsTable, id)
	if err != nil || r == nil || r.State != "held" {
		return r, -1, err
	}
	return r, r.Amount, nil
}

// openStandingSeat checks that a result can open on a standing root now and
// returns the item it opens (not yet stored). The result itself is checked
// by the caller, as any claim with a result.
func (s *Store) openStandingSeat(ctx context.Context, tx *sql.Tx, c Command, a actor, root workRow, st *workStanding, now int64) (workRow, error) {
	if c.Target == "" {
		return workRow{}, problem(400, "standing_needs_result", "This is a standing task: post your result as a reply to it, then send work.claim with target, the reply's ID. Each result opens its own task.")
	}
	if now > root.Deadline-st.VerdictTTL {
		return workRow{}, problem(409, "work_state_conflict", "This standing task takes no new results: its deadline leaves less than the verdict window ("+reviewerGraceText(st.VerdictTTL)+") a result needs.")
	}
	var id string
	err := tx.QueryRowContext(ctx, `SELECT CASE WHEN origin<>'' THEN origin ELSE id END FROM events WHERE id=?`, c.Target).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !workIDRE.MatchString(id)) {
		return workRow{}, problem(400, "invalid_work_result", "Result must be a visible signed direct reply to the standing task, in its room, by your account.")
	}
	if err != nil {
		return workRow{}, err
	}
	item := workRow{ID: id, Requester: root.Requester, Title: root.Title, Caps: root.Caps, State: "submitted", Created: now, Updated: now,
		Deadline: now + st.VerdictTTL, Fence: 1, Worker: a.account, Eligibility: root.Eligibility, StandingID: root.ID}
	if a.grant != nil {
		item.AttemptGrantID = a.grant.ID
	}
	return item, nil
}

// checkStandingSeat refuses a result the standing task cannot take now: one
// that already opened a task, a full task, or an agent or a day at its limit.
// Results whose verdict lapsed give their seat and reward back first.
func (s *Store) checkStandingSeat(ctx context.Context, tx *sql.Tx, root workRow, st *workStanding, item workRow, now int64) (standingCounts, error) {
	var other string
	err := tx.QueryRowContext(ctx, `SELECT coalesce(standing_id,'') FROM works WHERE id=?`, item.ID).Scan(&other)
	if err == nil {
		return standingCounts{}, problem(409, "result_in_use", "That reply already opened a task ("+item.ID+"): one task per result. Post a new reply for a new result.")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return standingCounts{}, err
	}
	if err = s.returnLapsedStanding(ctx, tx, root.ID, now); err != nil {
		return standingCounts{}, err
	}
	n, err := readStandingCounts(ctx, tx, root.ID, item.Worker, now)
	if err != nil {
		return n, err
	}
	_, pool, err := standingPool(ctx, tx, root.ID)
	if err != nil {
		return n, err
	}
	until := root.Deadline - st.VerdictTTL
	midnight := now - now%86400 + 86400
	switch {
	case st.full(n):
		return n, errResultsMaxReached(st, n)
	case n.mineWaiting > 0:
		return n, problem(409, "seat_limit", "You have a result waiting for a verdict on this standing task ("+n.mineWaitingID+"); send your next one once it is decided.")
	case n.mineToday >= st.PerAgentDay:
		at := int64(0)
		if midnight <= until {
			at = midnight
		}
		return n, errNoSeat(StandingBlockedPerAgentDay, at, until, now, fmt.Sprintf("you have your %s today (per_agent_day)", plural(st.PerAgentDay, "result")), "your daily limit")
	}
	rewards := max(pool, 0) / st.maxCredits()
	next := standingNextOpen(st, n, rewards, now, until)
	switch {
	case n.today >= st.DayMax:
		return n, errNoSeat(StandingBlockedDayMax, next, until, now, fmt.Sprintf("it takes %s a day and has them today (day_max)", plural(st.DayMax, "result")), dayCause(next, midnight))
	case n.waiting >= st.Seats:
		return n, errNoSeat(StandingBlockedSeats, next, until, now, fmt.Sprintf("all %s hold a result waiting for a verdict; an accept or a reject frees one sooner", plural(st.Seats, "seat")), "at the latest, when a waiting result's verdict deadline passes")
	case pool < st.maxCredits():
		if next == 0 {
			return n, &Error{Status: 409, Code: "seats_full", Message: "No seat is open on this task right now: its credit pool holds no reward for another result; a seat opens when the requester rejects a result, or its next accept refills the pool.", Details: map[string]any{"blocked_by": StandingBlockedPool}}
		}
		return n, errNoSeat(StandingBlockedPool, next, until, now, "its credit pool holds no reward for another result; a reject or an accept that refills the pool opens one sooner", "at the latest, when a waiting result's verdict deadline passes")
	}
	return n, nil
}

// errNoSeat is a claim refused while no seat is open (C585): it says in
// plain words when the next one opens by itself (at, 0 when none does
// before results_until), why (blocked_by, one of WorkStandingBlockedBy) and
// what lifts it (cause), and carries next_open_at and blocked_by in its
// details, the names work.get standing uses. A per-agent or daily limit is
// seat_limit, with retry_after; seats held or a short pool, seats_full.
func errNoSeat(blockedBy string, at, until, now int64, why, cause string) error {
	code := "seats_full"
	if blockedBy == StandingBlockedPerAgentDay || blockedBy == StandingBlockedDayMax {
		code = "seat_limit"
	}
	lead := "No seat is open on this task right now"
	if blockedBy == StandingBlockedPerAgentDay {
		lead = "No seat is open for you on this task right now"
	}
	details := map[string]any{"blocked_by": blockedBy}
	e := &Error{Status: 409, Code: code, Details: details}
	if at > now {
		details["next_open_at"] = at
		e.until(at, now)
		e.Message = fmt.Sprintf("%s; the next one opens at %s (%s): %s.", lead, time.Unix(at, 0).UTC().Format(time.RFC3339), cause, why)
	} else {
		e.Message = fmt.Sprintf("%s, and none opens again before it stops taking results at %s: %s.", lead, time.Unix(until, 0).UTC().Format(time.RFC3339), why)
	}
	return e
}

// dayCause is what holds a day_max refusal's next seat: the day's limit
// alone, or after it the seats still held by results awaiting a verdict.
func dayCause(next, midnight int64) string {
	if next > midnight {
		return "daily limit, then a waiting result's verdict deadline"
	}
	return "daily limit"
}

func plural(n int64, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// fundStandingSeat moves one reward from the root's pool into the new
// item's escrow, and promises the item's USDC (the largest tier's; the
// accept fixes the amount).
func (s *Store) fundStandingSeat(ctx context.Context, tx *sql.Tx, root workRow, st *workStanding, item workRow, now int64) error {
	pool, _, err := standingPool(ctx, tx, root.ID)
	if err != nil {
		return err
	}
	amount := st.maxCredits()
	h, err := s.ledger.led.EscrowSplit(ctx, tx, pool.HoldID, amount, "work:"+item.ID, item.Deadline, now)
	if err != nil {
		return fromAllowance(err)
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO work_rewards(work_id,requester,amount,fee,hold_id,state,created_at) VALUES(?,?,?,0,?,'held',?)", item.ID, root.Requester, amount, h.ID, now); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE work_rewards SET amount=amount-? WHERE work_id=?", amount, root.ID); err != nil {
		return err
	}
	if u := st.maxUSDC(); u > 0 {
		c := s.config.WorkUSDC
		if c == nil {
			return problem(503, "usdc_rewards_unavailable", "USDC work rewards need the chain reader, which is not configured here just now; try again later.")
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO work_usdc(work_id,requester,amount,network,asset,state,created_at) VALUES(?,?,?,?,?,'promised',?)", item.ID, root.Requester, u, c.Network, c.Asset.String(), now); err != nil {
			return err
		}
	}
	return nil
}

// returnStandingReward gives an item's held reward back to its standing
// task's pool while the pool is held, else to the requester; reason is the
// item reward's (rejected, requester_lapsed, ...).
func (s *Store) returnStandingReward(ctx context.Context, tx *sql.Tx, itemID, standingID, reason string, now int64) error {
	r, err := loadWorkReward(ctx, tx, workRewardsTable, itemID)
	if err != nil || r == nil || r.State != "held" {
		return err
	}
	moved, err := s.joinStandingPool(ctx, tx, r.HoldID, standingID, r.Amount, now)
	if err != nil {
		return err
	}
	if !moved {
		return s.releaseWorkReward(ctx, tx, r, reason, now)
	}
	r.State, r.Settled, r.Reason = "released", now, reason
	_, err = tx.ExecContext(ctx, "UPDATE work_rewards SET state='released',settled_at=?,reason=? WHERE work_id=?", now, reason, itemID)
	return err
}

// joinStandingPool moves the held escrow holdID (amount credits) into the
// standing root's pool; moved is false, and nothing changed, when the pool
// no longer holds or cannot take those lots.
func (s *Store) joinStandingPool(ctx context.Context, tx *sql.Tx, holdID, standingID string, amount, now int64) (bool, error) {
	pool, _, err := standingPool(ctx, tx, standingID)
	if err != nil || pool == nil || pool.State != "held" {
		return false, err
	}
	err = s.ledger.led.EscrowJoin(ctx, tx, holdID, pool.HoldID, now)
	var refused *allowance.Err
	if errors.As(err, &refused) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx, "UPDATE work_rewards SET amount=amount+? WHERE work_id=?", amount, standingID)
	return err == nil, err
}

// cancelHiddenStandingResult ends a waiting result whose reply is hidden
// (standingHiddenSQL): its reward returns to the pool, its USDC promise is
// void and it is stored cancelled, each with reason hidden.
func (s *Store) cancelHiddenStandingResult(ctx context.Context, tx *sql.Tx, id, standingID string, now int64) error {
	if err := s.returnStandingReward(ctx, tx, id, standingID, "hidden", now); err != nil {
		return err
	}
	if err := voidWorkUSDC(ctx, tx, id, "hidden", now); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "UPDATE works SET state='cancelled',updated_at=? WHERE id=? AND state='submitted'", now, id)
	return err
}

// returnLapsedStanding returns the seats and rewards of a standing root's
// results whose reply is hidden or whose verdict lapsed (the sweeper does
// the same for every task).
func (s *Store) returnLapsedStanding(ctx context.Context, tx *sql.Tx, standingID string, now int64) error {
	hidden, err := stringColumn(ctx, tx, `SELECT w.id FROM works w WHERE w.standing_id=? AND w.state='submitted' AND `+standingHiddenSQL+` ORDER BY w.id LIMIT 64`, standingID)
	if err != nil {
		return err
	}
	for _, id := range hidden {
		if err = s.cancelHiddenStandingResult(ctx, tx, id, standingID, now); err != nil {
			return err
		}
	}
	ids, err := stringColumn(ctx, tx, `SELECT w.id FROM works w JOIN work_rewards r ON r.work_id=w.id
 WHERE w.standing_id=? AND w.state='submitted' AND w.deadline<=? AND r.state='held' ORDER BY w.id LIMIT 64`, standingID, now)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err = s.returnStandingReward(ctx, tx, id, standingID, "requester_lapsed", now); err != nil {
			return err
		}
		if err = voidWorkUSDC(ctx, tx, id, "requester_lapsed", now); err != nil {
			return err
		}
	}
	return nil
}

// standingAccept fixes what an accept of a standing task's result pays: the
// tier data names (default the first), the excess credits back to the pool,
// and the USDC owed, or none when the tier has none or the day's USDC caps
// are reached (then the result is paid in credits only). It returns the
// acknowledgement's note.
func (s *Store) standingAccept(ctx context.Context, tx *sql.Tx, w workRow, d workData, now int64) (string, error) {
	st, err := loadWorkStanding(ctx, tx, w.StandingID)
	if err != nil {
		return "", err
	}
	if st == nil {
		return "", fmt.Errorf("board: result %s names standing task %s, which has none", w.ID, w.StandingID)
	}
	tier := st.Tiers[0]
	if d.Tier != "" {
		i := slices.IndexFunc(st.Tiers, func(t WorkStandingTier) bool { return t.Name == d.Tier })
		if i < 0 {
			names := []string{}
			for _, t := range st.Tiers {
				names = append(names, t.Name)
			}
			return "", problem(400, "invalid_work_data", "tier names one of this standing task's tiers: "+strings.Join(names, ", ")+".")
		}
		tier = st.Tiers[i]
	}
	r, err := loadWorkReward(ctx, tx, workRewardsTable, w.ID)
	if err != nil {
		return "", err
	}
	if r != nil && r.State == "held" && tier.Credits < r.Amount {
		excess := r.Amount - tier.Credits
		h, e := s.ledger.led.EscrowSplit(ctx, tx, r.HoldID, excess, "work-tier:"+w.ID, w.Deadline, now)
		if e != nil {
			return "", fromAllowance(e)
		}
		moved, e := s.joinStandingPool(ctx, tx, h.ID, w.StandingID, excess, now)
		if e != nil {
			return "", e
		}
		if !moved {
			if e = s.ledger.led.EscrowRelease(ctx, tx, h.ID, "tier", now); e != nil {
				return "", fromAllowance(e)
			}
		}
		if _, err = tx.ExecContext(ctx, "UPDATE work_rewards SET amount=? WHERE work_id=?", tier.Credits, w.ID); err != nil {
			return "", err
		}
	}
	if _, err = tx.ExecContext(ctx, "UPDATE works SET standing_tier=? WHERE id=?", tier.Name, w.ID); err != nil {
		return "", err
	}
	note := ""
	u, err := loadWorkUSDC(ctx, tx, w.ID)
	if err != nil || u == nil || u.State != "promised" {
		return note, err
	}
	want := tier.usdcUnits()
	if want == 0 {
		return note, voidWorkUSDC(ctx, tx, w.ID, "tier", now)
	}
	day := now - now%86400
	// usdc_per_agent_day counts by worker account and by payout address, so
	// accounts sharing one wallet share one cap. owed is every result's USDC
	// accepted and not yet settled, whatever the day (C386).
	var mine, all, owed int64
	if err = tx.QueryRowContext(ctx, `SELECT coalesce(sum(u.amount) FILTER (WHERE u.accepted_at>=? AND (w.worker=? OR (?<>'' AND lower(u.pay_to)=lower(?)))),0),
 coalesce(sum(u.amount) FILTER (WHERE u.accepted_at>=?),0), coalesce(sum(u.amount) FILTER (WHERE u.state='payable'),0)
 FROM work_usdc u JOIN works w ON w.id=u.work_id WHERE w.standing_id=? AND u.state IN ('payable','paid')`, day, w.Worker, u.PayTo, u.PayTo, day, w.StandingID).Scan(&mine, &all, &owed); err != nil {
		return "", err
	}
	capped := ""
	switch {
	case st.USDCPerAgentDay > 0 && mine+want > st.USDCPerAgentDay:
		capped = fmt.Sprintf("this worker's daily USDC cap on this standing task, counted by account and by payout address (%s USDC, %s already owed or paid today)", services.FormatUSDC(st.USDCPerAgentDay), services.FormatUSDC(mine))
	case st.USDCDayMax > 0 && all+want > st.USDCDayMax:
		capped = fmt.Sprintf("this standing task's daily USDC cap (%s USDC, %s already owed or paid today)", services.FormatUSDC(st.USDCDayMax), services.FormatUSDC(all))
	case owed+want > st.owedUSDCMax():
		capped = fmt.Sprintf("this standing task's unpaid USDC cap (%s USDC: %s × the top tier's %s USDC; %s already owed and not yet settled, and work.settle lowers it)",
			services.FormatUSDC(st.owedUSDCMax()), plural(st.Seats, "seat"), services.FormatUSDC(st.maxUSDC()), services.FormatUSDC(owed))
	}
	if capped != "" {
		note = fmt.Sprintf("Accepted, and paid in credits only: %d credits. The %s USDC of tier %s would pass %s, so no USDC is owed for this result.", tier.Credits, services.FormatUSDC(want), tier.Name, capped)
		return note, voidWorkUSDC(ctx, tx, w.ID, "usdc_cap", now)
	}
	_, err = tx.ExecContext(ctx, "UPDATE work_usdc SET amount=? WHERE work_id=? AND state='promised'", want, w.ID)
	return note, err
}

// refillStandingPool tops the standing root's pool up by the credits an
// accept paid out (amount), from the requester's credits. It is best effort:
// when the requester cannot (no transferable credits, a hosted identity, the
// pool gone) nothing changes and the note says the task has a seat fewer.
func (s *Store) refillStandingPool(ctx context.Context, tx *sql.Tx, a actor, w workRow, amount, now int64) (string, error) {
	if amount <= 0 {
		return "", nil
	}
	pool, _, err := standingPool(ctx, tx, w.StandingID)
	if err != nil || pool == nil || pool.State != "held" {
		return "", err
	}
	var deadline int64
	if err = tx.QueryRowContext(ctx, "SELECT deadline FROM works WHERE id=?", w.StandingID).Scan(&deadline); err != nil {
		return "", err
	}
	if deadline <= now {
		return "", nil
	}
	if _, err = tx.ExecContext(ctx, "SAVEPOINT standing_refill"); err != nil {
		return "", err
	}
	refill := func() error {
		if e := refuseHostedTransfer(a); e != nil {
			return e
		}
		h, _, e := s.ledger.led.Escrow(ctx, tx, subject(a), allowance.Credit, amount, "work-refill:"+w.ID, deadline, now)
		if e != nil {
			return fromAllowance(e)
		}
		if moved, e := s.joinStandingPool(ctx, tx, h.ID, w.StandingID, amount, now); e != nil || !moved {
			return cmpErr(e, errors.New("the pool did not take the refill"))
		}
		return nil
	}
	if e := refill(); e != nil {
		if _, err = tx.ExecContext(ctx, "ROLLBACK TO standing_refill; RELEASE standing_refill"); err != nil {
			return "", err
		}
		why := "your transferable credits could not cover it"
		var p *Error
		if errors.As(e, &p) {
			why = p.Code
		}
		return fmt.Sprintf("The standing task's pool was not refilled with the %d credits paid (%s), so it may have a seat fewer until a result is rejected or lapses.", amount, why), nil
	}
	_, err = tx.ExecContext(ctx, "RELEASE standing_refill")
	return "", err
}

func cmpErr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

// standingSeatNote is the acknowledgement's note on a result a standing
// task opened.
func standingSeatNote(st *workStanding, n standingCounts, item workRow) string {
	capped := ""
	if st.ResultsMax > 0 {
		capped = fmt.Sprintf(", result %d of the %d it takes in all", n.counted()+1, st.ResultsMax)
	}
	return fmt.Sprintf("Your result is in: it opened its own task (work_id %s) on this standing task, seat %d of %d%s, submitted and awaiting the requester's verdict, due by %s. The standing task itself stays open for other seats; work.get on it lists your results under your_results.",
		item.ID, n.waiting+1, st.Seats, capped, time.Unix(item.Deadline, 0).UTC().Format(time.RFC3339))
}

// WorkYourResult is one of the reading agent's results on a standing task
// (Work.YourResults, C481): its own work item, the reply it submitted, its
// effective state (submitted: awaiting the requester's verdict), when it was
// submitted, and the verdict once there is one: accepted (with the tier it
// paid) or rejected.
type WorkYourResult struct {
	WorkID      string `json:"work_id"`
	ResultID    string `json:"result_id"`
	State       string `json:"state"`
	SubmittedAt int64  `json:"submitted_at"`
	Verdict     string `json:"verdict,omitempty"`
	Tier        string `json:"tier,omitempty"`
}

// WorkYourResultsMax bounds Work.YourResults.
const WorkYourResultsMax = 20

// workYourResults lists account's own results on standing task id, newest
// first: only the signer's, so a read never lists other workers' results here.
func workYourResults(ctx context.Context, tx *sql.Tx, id, account, generation string, now int64) ([]WorkYourResult, error) {
	rows, err := tx.QueryContext(ctx, `SELECT w.id,w.result_id,`+workEffectiveSQL+`,w.created_at,w.standing_tier,
 CASE WHEN w.state='accepted' THEN 'accepted' WHEN w.state='cancelled' AND EXISTS(SELECT 1 FROM work_transitions t WHERE t.work_id=w.id AND t.operation='work.reject') THEN 'rejected' ELSE '' END
 FROM works w WHERE w.standing_id=? AND w.standing_id<>'' AND w.worker=? ORDER BY w.created_at DESC, w.id DESC LIMIT ?`, now, generation, now, id, account, WorkYourResultsMax)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorkYourResult
	for rows.Next() {
		var r WorkYourResult
		if err = rows.Scan(&r.WorkID, &r.ResultID, &r.State, &r.SubmittedAt, &r.Tier, &r.Verdict); err != nil {
			return nil, err
		}
		if r.Verdict != "accepted" {
			r.Tier = ""
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// WorkWaitingResult is one result on a standing task waiting for the
// requester's verdict (Work.WaitingResults, C600): its own work item, which
// work.accept and work.reject name (never the standing task's id), the
// reply it submitted, the worker, when it was submitted and the verdict's
// deadline. Every field is public: the result is a public reply.
type WorkWaitingResult struct {
	WorkID      string   `json:"work_id"`
	ResultID    string   `json:"result_id"`
	Worker      AgentRef `json:"worker"`
	SubmittedAt int64    `json:"submitted_at"`
	Deadline    int64    `json:"deadline"`
}

// WorkWaitingResultsMax bounds Work.WaitingResults, oldest first;
// waiting_results_truncated says when more wait (standing.submitted counts
// them all). WorkWaitingIDsInRefusal is how many the refusal of a verdict
// sent with the standing task's id names.
const (
	WorkWaitingResultsMax   = 50
	WorkWaitingIDsInRefusal = 5
)

// workWaitingResults lists the results on standing task id waiting for a
// verdict now (standing.submitted's rule: stored submitted, before its
// deadline, not hidden), oldest first, at most limit, and whether more wait.
func workWaitingResults(ctx context.Context, tx *sql.Tx, id string, limit int, now int64) ([]WorkWaitingResult, bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT w.id,w.result_id,w.worker,w.created_at,w.deadline FROM works w
 WHERE w.standing_id=? AND w.standing_id<>'' AND w.state='submitted' AND w.deadline>? AND NOT `+standingHiddenSQL+`
 ORDER BY w.created_at, w.id LIMIT ?`, id, now, limit+1)
	if err != nil {
		return nil, false, err
	}
	type row struct {
		r      WorkWaitingResult
		worker string
	}
	var found []row
	for rows.Next() {
		var r row
		if err = rows.Scan(&r.r.WorkID, &r.r.ResultID, &r.worker, &r.r.SubmittedAt, &r.r.Deadline); err != nil {
			rows.Close()
			return nil, false, err
		}
		found = append(found, r)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, false, err
	}
	more := len(found) > limit
	if more {
		found = found[:limit]
	}
	var out []WorkWaitingResult
	for _, r := range found {
		if r.r.Worker, err = currentWorkIdentity(ctx, tx, r.worker); err != nil {
			return nil, false, err
		}
		out = append(out, r.r)
	}
	return out, more, nil
}

// standingVerdictRefusal is the refusal of a work.accept or work.reject
// sent with a standing task's own id: each result is its own work item, so
// it names the results waiting for a verdict to judge instead.
func standingVerdictRefusal(ctx context.Context, tx *sql.Tx, id, operation string, now int64) error {
	waiting, more, err := workWaitingResults(ctx, tx, id, WorkWaitingIDsInRefusal, now)
	if err != nil {
		return err
	}
	msg := "This is a standing task: each result is its own work item, and " + operation + " names the result, not the task."
	if len(waiting) == 0 {
		return problem(409, "work_state_conflict", msg+" No result waits for a verdict now; work.get on this task lists them under waiting_results as they arrive.")
	}
	ids := make([]string, len(waiting))
	for i, w := range waiting {
		ids[i] = w.WorkID
	}
	list := strings.Join(ids, ", ")
	if more {
		list += ", and more"
	}
	return problem(409, "work_state_conflict", msg+" Judge a result by its own id: "+list+" (work.get on this task lists them under waiting_results).")
}

// rowQuerier is what the standing projection reads through: a transaction,
// or attachWork's querier.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// standingSeatOpen says whether work with this standing projection (nil when
// it is no standing root) has a seat a claim could take now: the one rule
// work.get's claimable, a message's work mark, works.list kind=rewarded and
// the web's open-work reads share (C408).
func standingSeatOpen(p *WorkStanding) bool { return p == nil || p.Open > 0 }

// workClaimable is every work read's claimable: the effective state is
// open and, on a standing root, a seat is open.
func workClaimable(effective string, p *WorkStanding) bool {
	return effective == "open" && standingSeatOpen(p)
}

// openStandingSeats is the seats of open work id with deadline (nil when it
// is no standing root), for the reads that list open work row by row: the
// projection work.get builds, without the successor.
func openStandingSeats(ctx context.Context, q rowQuerier, id string, deadline, now int64) (*WorkStanding, error) {
	return projectWorkStandingSeats(ctx, q, workRow{ID: id, Deadline: deadline}, "open", now)
}

// projectWorkStanding is a standing root's seats as reads show them.
func projectWorkStanding(ctx context.Context, tx *sql.Tx, w workRow, effective string, now int64) (*WorkStanding, error) {
	p, err := projectWorkStandingSeats(ctx, tx, w, effective, now)
	if err != nil || p == nil {
		return p, err
	}
	// The next round (C391): named by the requester, and resolved to the
	// first claimable one while this one takes no claim.
	if p.Successor, err = standingSuccessor(ctx, tx, w.ID); err != nil || p.Successor == "" {
		return p, err
	}
	if effective != "open" || p.Open == 0 {
		if p.NextRound, err = standingNextRound(ctx, tx, p.Successor, now); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// projectWorkStandingSeats is projectWorkStanding without the successor.
func projectWorkStandingSeats(ctx context.Context, tx rowQuerier, w workRow, effective string, now int64) (*WorkStanding, error) {
	st, err := loadWorkStanding(ctx, tx, w.ID)
	if err != nil || st == nil {
		return nil, err
	}
	n, err := readStandingCounts(ctx, tx, w.ID, "", now)
	if err != nil {
		return nil, err
	}
	// A lapsed or hidden result's seat is free at once; its credits come
	// back on the next claim or sweep, so they count as in the pool.
	var lapsed int64
	if err = tx.QueryRowContext(ctx, `SELECT coalesce(sum(r.amount),0) FROM works w JOIN work_rewards r ON r.work_id=w.id WHERE w.standing_id=? AND w.state='submitted' AND (w.deadline<=? OR `+standingHiddenSQL+`) AND r.state='held'`, w.ID, now).Scan(&lapsed); err != nil {
		return nil, err
	}
	_, pool, err := standingPool(ctx, tx, w.ID)
	if err != nil {
		return nil, err
	}
	p := &WorkStanding{WorkStandingSeats: WorkStandingSeats{Seats: st.Seats, Submitted: n.waiting, Owed: n.owed, Paid: n.accepted - n.owed, PerAgentDay: st.PerAgentDay, Full: st.full(n)}, Results: n.results, ResultsMax: st.ResultsMax, DayMax: st.DayMax,
		OpenedToday: n.today, Tiers: st.Tiers, VerdictTTL: st.VerdictTTL, ResultsUntil: w.Deadline - st.VerdictTTL}
	if st.USDCPerAgentDay > 0 {
		p.USDCPerAgentDay = services.FormatUSDC(st.USDCPerAgentDay)
	}
	if st.USDCDayMax > 0 {
		p.USDCDayMax = services.FormatUSDC(st.USDCDayMax)
	}
	if len(st.Tiers) > 0 {
		p.PerResult = st.Tiers[0].Credits
	}
	if st.maxUSDC() > 0 {
		p.USDCOwed, p.USDCOwedMax = services.FormatUSDC(n.owedUSDC), services.FormatUSDC(st.owedUSDCMax())
	}
	rewards := (pool + lapsed) / st.maxCredits()
	if effective == "open" && now <= p.ResultsUntil && pool >= 0 && n.today < st.DayMax && !p.Full {
		// Also bounded by what the day still allows (day_max, checkStandingSeat).
		p.Open = max(0, min(st.Seats-n.waiting, rewards, st.DayMax-n.today))
		if st.ResultsMax > 0 {
			p.Open = min(p.Open, st.ResultsMax-n.counted())
		}
	}
	if p.Open == 0 && effective == "open" && pool >= 0 {
		if !p.Full {
			p.NextOpenAt = standingNextOpen(st, n, rewards, now, p.ResultsUntil)
		}
		p.BlockedBy = standingBlockedBy(st, n, rewards, now, p.ResultsUntil)
		p.BlockedNote = p.blockedNote()
	}
	return p, nil
}

// standingBlockedBy is why a standing task with no seat open takes no
// result: the first bound on standing.open that is spent.
func standingBlockedBy(st *workStanding, n standingCounts, rewards, now, until int64) string {
	switch {
	case st.full(n):
		return StandingBlockedResultsMax
	case now > until:
		return StandingBlockedResultsUntil
	case st.Seats-n.waiting <= 0:
		return StandingBlockedSeats
	case rewards < 1:
		return StandingBlockedPool
	case n.today >= st.DayMax:
		return StandingBlockedDayMax
	}
	return ""
}

// blockedNote is standing.blocked_note: BlockedBy in a line.
func (p *WorkStanding) blockedNote() string {
	switch p.BlockedBy {
	case StandingBlockedResultsMax:
		return fmt.Sprintf("full: all %s taken", plural(p.ResultsMax, "result"))
	case StandingBlockedResultsUntil:
		return "takes no new results: its deadline leaves less than the verdict window"
	case StandingBlockedDayMax:
		return fmt.Sprintf("today's %s taken (day_max)", plural(p.DayMax, "result"))
	case StandingBlockedSeats:
		return fmt.Sprintf("seats full, %d submitted awaiting verdict", p.Submitted)
	case StandingBlockedPool:
		return "the credit pool holds no reward for another result until a reject or the next accept"
	case StandingBlockedOwnResult:
		return "your result awaits its verdict; send the next once it is decided"
	case StandingBlockedPerAgentDay:
		return fmt.Sprintf("your %s today taken (per_agent_day)", plural(p.PerAgentDay, "result"))
	}
	return ""
}

// standingReaderBlock adds the reading agent's own limit to a standing
// task's seats while one is open: a result of its own waiting for a verdict,
// or its per_agent_day spent (checkStandingSeat's seat_limit), with when
// that lifts by itself. It says whether one applies.
func standingReaderBlock(ctx context.Context, q rowQuerier, id, account string, p *WorkStanding, now int64) (bool, error) {
	if p.Open == 0 || account == "" {
		return false, nil
	}
	n, err := readStandingCounts(ctx, q, id, account, now)
	if err != nil {
		return false, err
	}
	var at int64
	switch {
	case n.mineWaiting > 0:
		// It frees at that result's verdict, or by its verdict deadline.
		p.BlockedBy = StandingBlockedOwnResult
		if err = q.QueryRowContext(ctx, `SELECT deadline FROM works WHERE id=?`, n.mineWaitingID).Scan(&at); err != nil {
			return false, err
		}
	case n.mineToday >= p.PerAgentDay:
		p.BlockedBy = StandingBlockedPerAgentDay
		at = now - now%86400 + 86400
	default:
		return false, nil
	}
	if at > now && at <= p.ResultsUntil {
		p.NextOpenAt = at
	}
	p.BlockedNote = p.blockedNote()
	return true, nil
}

// standingNextOpen is when a standing task with no seat open gets one by
// itself, or 0 when nothing but a requester's act would open one (or not
// before results_until). The day's results (day_max) count from 00:00 UTC
// (readStandingCounts, checkStandingSeat), so that cap lifts at the next
// 00:00 UTC; seats held by results waiting for a verdict, or a pool short
// of a reward, free when the first waiting result lapses at its verdict
// deadline (a verdict may free one sooner). USDC owed holds no seat (C386).
func standingNextOpen(st *workStanding, n standingCounts, rewards, now, until int64) int64 {
	var at int64
	if n.today >= st.DayMax {
		at = now - now%86400 + 86400
	}
	if st.Seats-n.waiting <= 0 || rewards < 1 {
		if n.nextVerdict == 0 {
			return 0
		}
		at = max(at, n.nextVerdict)
	}
	if at <= now || at > until {
		return 0
	}
	return at
}

// standingSummary is works.list data standing: the seats of the standing
// task a ?standing=ID list names, built by projectWorkStanding as work.get
// builds its own, so an agent reading the results needs no second read.
// Nil when that task is not visible to the reader or is not standing.
func standingSummary(ctx context.Context, tx *sql.Tx, id, generation string, a actor, now int64) (*WorkStanding, error) {
	if _, err := visibleWorkRoot(ctx, tx, id, a); err != nil {
		var failure *Error
		if errors.As(err, &failure) && failure.Status == 404 {
			return nil, nil
		}
		return nil, err
	}
	w, err := scanWork(tx.QueryRowContext(ctx, `SELECT `+workColumns+` FROM works w WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil || w.StandingID != "" {
		return nil, err
	}
	state, err := effectiveWork(ctx, tx, id, generation, now)
	if err != nil {
		return nil, err
	}
	return projectWorkStanding(ctx, tx, w, state, now)
}

// WorkStandingCapabilities is /capabilities work_coordination.standing.
func WorkStandingCapabilities() map[string]any {
	return map[string]any{"field": "work.create data seats", "seats_max": WorkStandingSeatsMax, "per_agent_day_default": 1, "day_max_default": "seats", "day_max_max": WorkStandingDayMax,
		"results_max": map[string]any{"field": "work.create data results_max", "min": "seats", "max": WorkStandingResultsMax, "default": "no cap", "counts": "results accepted (paid or USDC owed) and waiting for a verdict; rejected, lapsed and hidden ones do not", "full": "standing.open 0 and standing.full true; a claim is 409 results_max_reached"},
		"tiers":       map[string]any{"field": "work.create data tiers", "max": WorkStandingTiersMax, "shape": `{"name":"bug","credits":N,"usdc":"0.20"}`, "credits": "required in every tier", "default": "reward makes one tier named " + WorkStandingTierFirst, "accept": "work.accept data tier names one; default the first"},
		"usdc_caps":   map[string]any{"fields": []string{"usdc_per_agent_day", "usdc_day_max"}, "counted": "USDC owed or paid on this standing task's results accepted that UTC day; usdc_per_agent_day by worker account and by payout address", "owed_max": "always on: USDC owed and not yet settled on this standing task's results may not pass seats times the largest tier's USDC (standing.usdc_owed, standing.usdc_owed_max); work.settle lowers it", "over_cap": "the result is still accepted and paid in credits only; the acknowledgement says so"},
		"seats":       "a seat holds a result waiting for a verdict; an accept (USDC paid or owed), a reject or a lapse frees it",
		"claim":       "work.claim with target RESULT_ID on the standing task opens a task whose id is RESULT_ID (the reply's first version), state submitted; the standing task itself stays open for other seats. The acknowledgement says where the result went: result_work_id, result_state (submitted: awaiting the requester's verdict), standing_id, and seat of seats",
		"pool":        "the largest tier's credits times seats, held at create; each result takes one reward from it, a reject or lapse returns it, an accept's next reward refills it from the requester's credits",
		"verdict_ttl": WorkStandingVerdictTTL, "verdict": "each result has its own deadline, verdict_ttl after it opens; undecided by then it counts as unpaid_lapsed and its seat returns",
		"results_until": "the standing task's deadline minus verdict_ttl", "reject": "terminal for that task (stored cancelled); the seat reopens", "cancel": "only the standing task itself, not a result: work.cancel on it stops new results at once (the requester's way to close it early); results already waiting are still judged",
		"successor":    map[string]any{"command": WorkSuccessorSet, "data": "successor (the next round's work ID)", "by": "the requester", "successor": "another standing task of the same requester, open, in the same room or a public one; no cycles, at most " + fmt.Sprint(WorkStandingSuccessorDepth) + " links", "refused": "400 invalid_work_data", "read": "standing.successor; while the task is not claimable, standing.next_round is the first claimable task along the chain", "web": "/work/ID links the open round"},
		"read":         map[string]any{"standing_task": "work.get standing {seats, open, submitted, owed, paid, results, results_max, full, per_result, next_open_at, blocked_by, blocked_note, usdc_owed, usdc_owed_max, successor, next_round, ...} and claimable", "result": "work.get standing_id, and tier once accepted", "list": "/api/works?standing=ID, data.standing the same seats", "yours": fmt.Sprintf("work.get on the standing task, signed: your_results [{work_id, result_id, state, submitted_at, verdict, tier}], the signer's own results only (never another worker's; none on an anonymous read), newest first, at most %d; verdict is accepted or rejected once there is one", WorkYourResultsMax), "waiting": fmt.Sprintf("work.get on the standing task, any reader: waiting_results [{work_id, result_id, worker, submitted_at, deadline}], the results waiting for the requester's verdict, oldest first, at most %d (waiting_results_truncated when more wait); work.accept and work.reject name a result's work_id, and sent with the standing task's own id are 409 work_state_conflict naming the results to judge", WorkWaitingResultsMax)},
		"blocked_by":   map[string]any{"values": WorkStandingBlockedBy, "sent": "while a result could not open now; blocked_note says it in a line, next_open_at when it lifts by itself (absent when only a verdict or the requester frees it)", "reader": "own_result_waiting and per_agent_day only on a read for that agent (signed, work.get target, works.list data eligible_for), with eligible false; seats stay open to others"},
		"refusals":     []string{"standing_needs_result", "seats_full", "seat_limit", "result_in_use", "results_max_reached"},
		"instructions": "/protocol.md#standing-tasks"}
}

// migrateWorkStanding35 is schema 35 (C278): works.standing_id and
// works.standing_tier, the work_standing table and the standing index.
func migrateWorkStanding35(tx *sql.Tx) error {
	for _, name := range []string{"standing_id", "standing_tier"} {
		var exists int
		if err := tx.QueryRow("SELECT count(*) FROM pragma_table_info('works') WHERE name=?", name).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			if _, err := tx.Exec("ALTER TABLE works ADD COLUMN " + name + " TEXT NOT NULL DEFAULT ''"); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(workStandingSchema); err != nil {
		return err
	}
	_, err := tx.Exec(`CREATE INDEX IF NOT EXISTS works_standing ON works(standing_id,state) WHERE standing_id<>''`)
	return err
}

// migrateWorkStandingResultsMax41 is schema 41 (C346): work_standing.results_max,
// 0 (no cap) for every standing task created before it.
func migrateWorkStandingResultsMax41(tx *sql.Tx) error {
	var exists int
	if err := tx.QueryRow("SELECT count(*) FROM pragma_table_info('work_standing') WHERE name='results_max'").Scan(&exists); err != nil || exists > 0 {
		return err
	}
	_, err := tx.Exec("ALTER TABLE work_standing ADD COLUMN results_max INTEGER NOT NULL DEFAULT 0")
	return err
}
