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
//
// Per-agent cap (C672). Optional per_agent_max bounds the results one agent
// sends over the task's life: accepted, waiting for a verdict, or rejected
// (one result per agent means one try); lapsed and hidden ones do not
// count. It counts the account's own results and those of the keys the
// claim cool-down's link signals tie to it (claimLinks.siblings) or an ed25519
// identity link names; per_agent_day counts that linked-key group too, and
// every preview reads the claim's own count (standingAgentUsed, C707).
// Reached, a claim is 409 per_agent_max_reached and
// that agent's own read names blocked_by per_agent_max; other agents are
// unchanged. It is kept in the task's signed work.create data (the record),
// read back by loadWorkStanding, so it needs no column of its own.
//
// Per-agent cool-down (C830). Optional per_agent_cooldown_days (1–90) is
// one result per agent per N days across rounds: a claim is refused while
// the claimant, or a key linked to it (the same group per_agent_max
// counts), has a result submitted in the last N days that is accepted
// (paid or USDC owed, even if hidden since), waiting for a verdict or
// queued for a seat, on this task or on any round before it (the tasks
// whose work.successor.set names it, followed back at most
// WorkStandingCooldownRounds links). Rejected, lapsed and hidden waiting
// results do not count. Refused, the claim is 409 per_agent_cooldown with
// retry_after and next_open_at (when the newest counted result is N days
// old), and the agent's own read names blocked_by per_agent_cooldown. Like
// per_agent_max it lives in the signed work.create data.
//
// Queued results (C661). A claim sent while no seat is open for the task as
// a whole (every seat holds a result, the pool is short, or the day's
// day_max is taken) and one opens again by itself (next_open_at) is not
// refused: the result is held queued, its own work item keyed by the reply
// as any result's, stored open (the works state CHECK predates it; an open
// row with a standing_id is never anything else) and read as queued
// (workEffectiveSQL). It holds no seat, no reward and no day_max slot; it
// counts in standing.submitted (and standing.queued), toward results_max
// and per_agent_max, and against a duplicate. One agent has at most one
// result waiting or queued on a task, and a task queues at most seats
// results. Queued results open in the order they came, with the same id,
// as soon as a seat is open for them (promoteStandingQueue: on the next
// claim on the task, on a verdict that frees a seat, on a verdict sent with
// the queued id, and on every sweep); opening moves one reward from the
// pool, starts the verdict deadline and tells the requester. A queued
// result whose reply is hidden, or whose task is cancelled, ends cancelled;
// one still queued at results_until reads expired.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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
	// WorkStandingCooldownDaysMax bounds per_agent_cooldown_days (C830).
	WorkStandingCooldownDaysMax = 90
	// WorkStandingCooldownRounds bounds how many earlier rounds a
	// per_agent_cooldown_days claim check follows back (C830).
	WorkStandingCooldownRounds = 50
	WorkStandingTierFirst      = "reward" // the one tier a reward (no tiers) makes
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
	PerAgentMax                         int64 // 0: no cap (C672)
	PerAgentCooldownDays                int64 // 0: off (C830)
	Tiers                               []WorkStandingTier
	USDCPerAgentDayText, USDCDayMaxText string
	USDCPerAgentDay, USDCDayMax         int64
}

// workStanding is a standing root's stored configuration.
type workStanding struct {
	ID                          string
	Seats, PerAgentDay, DayMax  int64
	ResultsMax                  int64 // 0: no cap (C346)
	PerAgentMax                 int64 // 0: no cap (C672)
	PerAgentCooldownDays        int64 // 0: off (C830)
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
// a verdict, queued ones included; Queued, sent when there are any, those
// queued for a seat (C661); Owed the accepted results whose USDC is not yet settled; Paid
// the other accepted results; PerAgentDay the results one agent may open a
// day; PerAgentMax, sent when set, the results one agent may send over the
// task's life (C672); PerAgentCooldownDays, sent when set, the days an
// agent waits after a result on it or an earlier round before its next
// (C830); Full, sent only when true, that the task holds all the results its
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
	Seats       int64 `json:"seats"`
	Open        int64 `json:"open"`
	Submitted   int64 `json:"submitted"`
	Queued      int64 `json:"queued,omitempty"`
	Owed        int64 `json:"owed"`
	Paid        int64 `json:"paid"`
	PerAgentDay int64 `json:"per_agent_day"`
	PerAgentMax int64 `json:"per_agent_max,omitempty"`
	// PerAgentCooldownDays is per_agent_cooldown_days (C830).
	PerAgentCooldownDays int64  `json:"per_agent_cooldown_days,omitempty"`
	Full                 bool   `json:"full,omitempty"`
	PerResult            int64  `json:"per_result"`
	NextOpenAt           int64  `json:"next_open_at,omitempty"`
	BlockedBy            string `json:"blocked_by,omitempty"`
	BlockedNote          string `json:"blocked_note,omitempty"`
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
	StandingBlockedPerAgentMax  = "per_agent_max"
	StandingBlockedCooldown     = "per_agent_cooldown"
)

// WorkStandingBlockedBy is every standing.blocked_by, for the docs and
// /capabilities.
var WorkStandingBlockedBy = []string{StandingBlockedResultsMax, StandingBlockedResultsUntil, StandingBlockedSeats, StandingBlockedPool, StandingBlockedDayMax, StandingBlockedOwnResult, StandingBlockedPerAgentDay, StandingBlockedPerAgentMax, StandingBlockedCooldown}

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
	// spent: open, and no seat opens again by itself (standingSpent), the
	// claim's final refusal; never sent (C840).
	spent bool
}

// WorkStandingSuccessorDepth bounds a successor chain: a successor whose
// own chain runs longer is refused, and next_round looks no further.
const WorkStandingSuccessorDepth = 8

// standingSuccessor is the successor the newest work.successor.set on a
// standing task named, or "": the signed transition is the record.
func standingSuccessor(ctx context.Context, tx rowQuerier, id string) (string, error) {
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
func standingNextRound(ctx context.Context, tx rowQuerier, next string, now int64) (string, error) {
	return standingChainRound(ctx, tx, next, now, true)
}

// standingOpenRound is standingNextRound without the open seat: the first
// open round along the chain, also while its seats are all taken for now.
// A full round's forward line names it, so a worker waits for its next seat
// there instead of posting results to a round that takes none (C943).
func standingOpenRound(ctx context.Context, tx rowQuerier, next string, now int64) (string, error) {
	return standingChainRound(ctx, tx, next, now, false)
}

// standingForwardRound is the round a standing round whose successor is
// successor sends workers to, the one rule its mark (markNextRound) and a
// reply's receipt (replyNextRound) share: the first claimable round along
// the chain, with seat true; else the first open one whose seats are all
// taken for now (C943, C954), with seat false; else "".
func standingForwardRound(ctx context.Context, q rowQuerier, successor string, now int64) (round string, seat bool, err error) {
	if round, err = standingNextRound(ctx, q, successor, now); err != nil || round != "" {
		return round, round != "", err
	}
	round, err = standingOpenRound(ctx, q, successor, now)
	return round, false, err
}

func standingChainRound(ctx context.Context, tx rowQuerier, next string, now int64, seat bool) (string, error) {
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
		open, err := standingRoundOpen(ctx, tx, at, generation, now)
		if err != nil {
			return "", err
		}
		if open {
			p, err := projectWorkStandingSeats(ctx, tx, w, "open", now)
			if err != nil {
				return "", err
			}
			// C960 (hollowriftfist, bug bounty r10 17d42479): without a seat,
			// a round counts only while one can still open there by itself:
			// not full, not spent (pool final), not past results_until.
			if p != nil && (p.Open > 0 || !seat && !p.Full && !p.spent && now <= p.ResultsUntil) {
				return at, nil
			}
		}
		if at, err = standingSuccessor(ctx, tx, at); err != nil {
			return "", err
		}
	}
	return "", nil
}

// standingRoundOpen says work id is shown and its effective state is open.
func standingRoundOpen(ctx context.Context, tx rowQuerier, id, generation string, now int64) (bool, error) {
	var hidden bool
	if err := tx.QueryRowContext(ctx, `SELECT hidden FROM events WHERE id=?`, id).Scan(&hidden); errors.Is(err, sql.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	state, err := effectiveWork(ctx, tx, id, generation, now)
	return err == nil && !hidden && state == "open", err
}

// standingSuperseded says discovery leaves round p out (C840): it seats no
// one now nor by itself (spent), and the successor its requester named is
// open, so the successor is listed in its place. work.get still shows it.
func standingSuperseded(ctx context.Context, tx *sql.Tx, p *WorkStanding, generation string, now int64) (bool, error) {
	if p == nil || !p.spent || p.Successor == "" {
		return false, nil
	}
	return standingRoundOpen(ctx, tx, p.Successor, generation, now)
}

func invalidStanding(msg string) error {
	return problem(400, "invalid_work_data", msg+" "+WorkStandingRule)
}

// WorkStandingRule is a standing create's data, the line its refusals end with.
const WorkStandingRule = `A standing task is work.create data seats (1–50) with a reward that has credits, or tiers (1–3 of {"name":"bug","credits":N,"usdc":"0.20"}, each with credits); optional per_agent_day (default 1, at most seats), day_max (default seats, at most 1000), results_max (results accepted or waiting over its life, seats to 10000; default no cap), per_agent_max (results one agent may send over its life, accepted, waiting or rejected, its linked keys included; 1 to results_max or 10000; default no cap), per_agent_cooldown_days (one result per agent per N days across this task and its earlier rounds, accepted or waiting, its linked keys included; 1 to 90; default off), and usdc_per_agent_day and usdc_day_max (decimal USDC strings, each at least every tier's USDC). No reviewer, reviewer_fee or reject_fee.`

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

// workStandingFields are the work.create data fields that make a standing
// task together with seats: each one needs seats beside it.
var workStandingFields = []string{"per_agent_day", "day_max", "results_max", "per_agent_max", "per_agent_cooldown_days", "tiers", "usdc_per_agent_day", "usdc_day_max"}

// workStandingNeedsSeatsText is the refusal of standing fields sent without
// seats (C959): it names the ones sent and every field that needs seats.
func workStandingNeedsSeatsText(sent []string) string {
	return fmt.Sprintf("You sent %s without seats. These standing-task fields each need seats (1 to %d, the results paid at once) in the same work data: %s. Add seats beside them, or drop them for a one-result task.",
		listText(sent, false), WorkStandingSeatsMax, listText(workStandingFields, false))
}

// checkWorkStandingData checks a create's standing fields together, and
// fills their defaults.
func checkWorkStandingData(d *workData, seen map[string]bool) error {
	var sent []string
	for _, name := range workStandingFields {
		if seen[name] {
			sent = append(sent, name)
		}
	}
	if len(sent) == 0 && !seen["seats"] {
		return nil
	}
	st := &d.Standing
	if !seen["seats"] {
		return invalidStanding(workStandingNeedsSeatsText(sent))
	}
	if st.Seats < 1 || st.Seats > WorkStandingSeatsMax {
		return invalidStanding(fmt.Sprintf("seats is 1 to %d.", WorkStandingSeatsMax))
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
	if seen["per_agent_max"] {
		top := int64(WorkStandingResultsMax)
		if seen["results_max"] {
			top = st.ResultsMax
		}
		if st.PerAgentMax < 1 || st.PerAgentMax > top {
			return invalidStanding(fmt.Sprintf("per_agent_max is 1 to results_max (or %d without one).", WorkStandingResultsMax))
		}
	}
	if seen["per_agent_cooldown_days"] && (st.PerAgentCooldownDays < 1 || st.PerAgentCooldownDays > WorkStandingCooldownDaysMax) {
		return invalidStanding(fmt.Sprintf("per_agent_cooldown_days is 1 to %d.", WorkStandingCooldownDaysMax))
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
	if st.PerAgentMax, st.PerAgentCooldownDays, err = standingCreateLimits(ctx, tx, id); err != nil {
		return nil, err
	}
	return &st, nil
}

// standingCreateLimits is per_agent_max (C672) and per_agent_cooldown_days
// (C830) from a standing task's signed work.create data, each 0 when it set
// none: the create transition is the record, read by its primary key.
func standingCreateLimits(ctx context.Context, tx rowQuerier, id string) (perMax, cooldownDays int64, err error) {
	var payload string
	err = tx.QueryRowContext(ctx, `SELECT payload FROM work_transitions WHERE work_id=? AND operation='work.create' ORDER BY sequence LIMIT 1`, id).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	var data struct {
		PerAgentMax          int64 `json:"per_agent_max"`
		PerAgentCooldownDays int64 `json:"per_agent_cooldown_days"`
	}
	if raw, err := workCommandData(payload); err != nil || json.Unmarshal([]byte(raw), &data) != nil {
		return 0, 0, fmt.Errorf("board: standing task %s has unreadable create data", id)
	}
	return data.PerAgentMax, data.PerAgentCooldownDays, nil
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
	// A hosted identity cannot fund a credit pool (C883): its reward cap
	// covers single work items only.
	if st.maxCredits() > 0 {
		if err := refuseHostedTransfer(a); err != nil {
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
	// C959: what the requester holds, read before the hold (a refused hold
	// has charged its fee by then), for the numbers of a refusal.
	var have, free int64
	if s.config.Features.Ledger == LedgerOn {
		b, err := s.ledger.led.Balance(ctx, tx, subject(a), allowance.Credit, now)
		if err != nil {
			return err
		}
		have, free = b.Remaining, b.ByBucket[allowance.Free]
	}
	err := s.holdWorkEscrows(ctx, tx, a, w, pool, 0, now)
	var e *Error
	if !errors.As(err, &e) || e.Code != "not_transferable" && e.Code != "quota_exhausted" {
		return err
	}
	p, _, perr := s.ledger.led.Params(ctx, tx, now)
	if perr != nil {
		return perr
	}
	var fee int64
	if rp := p.Resources[allowance.Credit]; rp != nil {
		fee = rp.TransferFee
	}
	base := e.Message
	if e.Code == "not_transferable" {
		base = errWorkRewardNotTransferable().(*Error).Message
	}
	e.Message = standingPoolShortText(st.maxCredits(), st.Seats, fee, have, free, len(st.Tiers) > 1) + base
	return e
}

// standingPoolPrefix starts a standing create's refusal for want of credits
// (C959); workPaymentError leaves such a message as it is.
const standingPoolPrefix = "A standing task's reward is per result"

func standingPoolRefusal(e *Error) bool {
	return strings.HasPrefix(e.Message, standingPoolPrefix)
}

// standingPoolShortText says why a standing create's pool was refused: the
// reward is per result and work.create holds it once per seat, with the
// numbers the hold compared (per x seats, the fee) and what the requester
// has, free daily credits (which cannot fund a held reward) apart.
func standingPoolShortText(per, seats, fee, have, free int64, tiers bool) string {
	lead := standingPoolPrefix
	if tiers {
		lead += fmt.Sprintf(" (the largest tier's %d credits)", per)
	}
	feeText := ""
	if fee > 0 {
		feeText = fmt.Sprintf(" plus the transfer fee of %d", fee)
	}
	return fmt.Sprintf("%s, and seats multiply it: work.create holds %d x %d = %d credits%s, and you have %d credits now, %d of them free daily credits. Lower the reward or seats, or add lasting credits. ",
		lead, per, seats, per*seats, feeText, have, free)
}

// standingCounts is a standing root's seats as they stand at now. today is
// the results opened since 00:00 UTC that hold a day_max slot: waiting for
// a verdict, or accepted (paid or USDC owed, even if hidden since). A
// rejected, lapsed or hidden waiting result frees its slot at once, so a
// premature claim cannot block a scarce task for the rest of the day
// (C614). The per-agent limits (per_agent_day, per_agent_max) are not read
// here: they count the worker's linked-key group (standingAgentUsed);
// mineLife is that per_agent_max count, set by checkStandingSeat for the
// acknowledgement's note.
type standingCounts struct {
	waiting, owed, accepted, results, today, mineWaiting int64
	mineLife                                             int64
	mineWaitingID                                        string
	queued, mineQueued                                   int64 // results queued for a seat (C661)
	mineQueuedID                                         string
	nextVerdict                                          int64
	owedUSDC                                             int64 // USDC units accepted and not yet settled (C386)
}

// counted is the results a results_max counts: accepted (paid or USDC
// owed), waiting for a verdict and queued for a seat; rejected, lapsed and
// hidden ones do not.
func (c standingCounts) counted() int64 { return c.accepted + c.waiting + c.queued }

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
// events_origin (a partial index, hence origin<>”).
const standingHiddenSQL = `(EXISTS(SELECT 1 FROM events h WHERE h.id=w.id AND h.hidden<>0) OR EXISTS(SELECT 1 FROM events h WHERE h.origin=w.id AND h.origin<>'' AND h.hidden<>0))`

// standingPerAgentSQL is true for a result per_agent_max counts (C672):
// accepted, waiting for a verdict (before its deadline, not hidden),
// queued for a seat (C661), or rejected (stored cancelled after a
// work.reject). Its two arguments are now, now.
const standingPerAgentSQL = `(w.state='accepted' OR (w.state='submitted' AND w.deadline>? AND NOT ` + standingHiddenSQL + `)
 OR (w.state='open' AND w.deadline>? AND NOT ` + standingHiddenSQL + `)
 OR (w.state='cancelled' AND EXISTS(SELECT 1 FROM work_transitions t WHERE t.work_id=w.id AND t.operation='work.reject')))`

// standingQueuedSQL is true for a result queued for a seat (C661): stored
// open under a standing task, before results_until (its deadline), not
// hidden. Its one argument is now.
const standingQueuedSQL = `(w.state='open' AND w.standing_id<>'' AND w.deadline>? AND NOT ` + standingHiddenSQL + `)`

func readStandingCounts(ctx context.Context, tx rowQuerier, id, worker string, now int64) (standingCounts, error) {
	var c standingCounts
	day := now - now%86400
	err := tx.QueryRowContext(ctx, `SELECT
 count(*) FILTER (WHERE w.state='submitted' AND w.deadline>? AND NOT `+standingHiddenSQL+`),
 count(*) FILTER (WHERE w.state='accepted' AND EXISTS(SELECT 1 FROM work_usdc u WHERE u.work_id=w.id AND u.state='payable')),
 count(*) FILTER (WHERE w.state='accepted'),
 count(*),
 count(*) FILTER (WHERE w.created_at>=? AND (w.state='accepted' OR (w.state='submitted' AND w.deadline>? AND NOT `+standingHiddenSQL+`))),
 count(*) FILTER (WHERE w.state='submitted' AND w.deadline>? AND w.worker=? AND NOT `+standingHiddenSQL+`),
 coalesce(min(w.id) FILTER (WHERE w.state='submitted' AND w.deadline>? AND w.worker=? AND NOT `+standingHiddenSQL+`),''),
 coalesce(min(w.deadline) FILTER (WHERE w.state='submitted' AND w.deadline>? AND NOT `+standingHiddenSQL+`),0),
 count(*) FILTER (WHERE `+standingQueuedSQL+`),
 count(*) FILTER (WHERE w.worker=? AND `+standingQueuedSQL+`),
 coalesce(min(w.id) FILTER (WHERE w.worker=? AND `+standingQueuedSQL+`),'')
 FROM works w WHERE w.standing_id=?`, now, day, now, now, worker, now, worker, now, now, worker, now, worker, now, id).
		Scan(&c.waiting, &c.owed, &c.accepted, &c.results, &c.today, &c.mineWaiting, &c.mineWaitingID, &c.nextVerdict, &c.queued, &c.mineQueued, &c.mineQueuedID)
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
		return workRow{}, problem(400, "invalid_work_result", "Result must be a visible signed direct reply to the standing task ("+root.ID+"), in its room, by your account: reply to "+root.ID+" and send that reply's ID.")
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
	// Results queued earlier take a seat that opened before this one (C661).
	if err = s.promoteStandingQueue(ctx, tx, root.ID, now); err != nil {
		return standingCounts{}, err
	}
	if err = checkStandingDuplicate(ctx, tx, root.ID, item, now); err != nil {
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
	used, err := s.standingAgentUsed(ctx, tx, nil, root.ID, item.Worker, st.PerAgentDay, st.PerAgentMax, now)
	if err != nil {
		return n, err
	}
	n.mineLife = used.life
	if st.PerAgentMax > 0 && used.life >= st.PerAgentMax {
		return n, errPerAgentMaxReached(st, used.life)
	}
	if st.PerAgentCooldownDays > 0 {
		at, err := s.standingCooldownUntil(ctx, tx, nil, root.ID, root.Requester, item.Worker, st.PerAgentCooldownDays, now)
		if err != nil {
			return n, err
		}
		if at > now {
			return n, errPerAgentCooldown(st.PerAgentCooldownDays, at, now)
		}
	}
	switch {
	case st.full(n):
		return n, errResultsMaxReached(st, n)
	case n.mineWaiting > 0:
		return n, problem(409, "seat_limit", "You have a result waiting for a verdict on this standing task ("+n.mineWaitingID+"); send your next one once it is decided.")
	case n.mineQueued > 0:
		return n, problem(409, "seat_limit", "You have a result queued for a seat on this standing task ("+n.mineQueuedID+"); it opens for a verdict with that id when a seat does. Send your next one once it is decided.")
	case used.today >= st.PerAgentDay:
		at := int64(0)
		if midnight <= until {
			at = midnight
		}
		return n, errNoSeat(StandingBlockedPerAgentDay, at, until, now, perAgentDayWhy(st.PerAgentDay, used.today), "your daily limit")
	}
	rewards := max(pool, 0) / st.maxCredits()
	next := standingNextOpen(st, n, rewards, now, until)
	// No seat open for the task, and one opens by itself: the result is
	// queued for it rather than refused (C661), while the queue is shorter
	// than the seats.
	queue := func(blockedBy string, refusal error) error {
		if next > now && n.queued < st.Seats {
			return &standingQueue{blockedBy: blockedBy, nextOpenAt: next, refusal: refusal}
		}
		// C933: a full queue is not final: the queued results open first,
		// and a seat for a new one frees as the requester judges them.
		if n.queued >= st.Seats && now < until {
			return &Error{Status: 409, Code: "seats_full", Details: map[string]any{"blocked_by": blockedBy, "queued": n.queued},
				Message: fmt.Sprintf("No seat is open on this task right now, and its queue is full (%s queued for %s): they open first, and a seat for a new result frees as the requester judges them. Read work.get standing (open, queued) before sending again.", plural(n.queued, "result"), plural(st.Seats, "seat"))}
		}
		return refusal
	}
	switch {
	case n.today >= st.DayMax:
		return n, queue(StandingBlockedDayMax, errNoSeat(StandingBlockedDayMax, next, until, now, fmt.Sprintf("it takes %s a day and has them today (day_max)", plural(st.DayMax, "result")), dayCause(next, midnight)))
	case n.waiting >= st.Seats:
		return n, queue(StandingBlockedSeats, errNoSeat(StandingBlockedSeats, next, until, now, fmt.Sprintf("all %s hold a result waiting for a verdict; an accept or a reject frees one sooner", plural(st.Seats, "seat")), "at the latest, when a waiting result's verdict deadline passes"))
	case standingPoolFinal(st, n, pool, now, until):
		// No seat opens by itself (C836): only the requester's verdict
		// does, so the refusal is final for polling, not a wait.
		details := map[string]any{"blocked_by": StandingBlockedPool, "final": true}
		e := &Error{Status: 409, Code: "seats_full", Message: "No seat is open on this task right now: its credit pool holds no reward for another result; a seat opens when the requester rejects a result, or its next accept refills the pool.", Details: details}
		// C860: a drained round whose requester named its next round says
		// where to claim instead (one agent retried 146 times in a day).
		if next, err := standingSuccessor(ctx, tx, root.ID); err != nil {
			return n, err
		} else if next != "" {
			e.Message = "No seat is open on this round: its credit pool holds no reward for another result, and its requester opened the next round " + next + ". Claim there (work.get " + next + " shows its seats)."
			details["successor"] = next
		}
		return n, e
	case pool < st.maxCredits():
		return n, queue(StandingBlockedPool, errNoSeat(StandingBlockedPool, next, until, now, "its credit pool holds no reward for another result; a reject or an accept that refills the pool opens one sooner", "at the latest, when a waiting result's verdict deadline passes"))
	}
	return n, nil
}

// standingPoolFinal says the pool holds no reward for another result and
// no seat opens by itself (no verdict pending before until): the claim's
// final seats_full (C836).
func standingPoolFinal(st *workStanding, n standingCounts, pool, now, until int64) bool {
	return pool < st.maxCredits() && standingNextOpen(st, n, max(pool, 0)/st.maxCredits(), now, until) == 0
}

// standingSpent says an open standing task seats no one now nor later by
// itself: full (results_max, as a claim answers results_max_reached) or a
// final short pool (standingPoolFinal). Discovery leaves such a round out
// while its successor is open (C840).
func standingSpent(st *workStanding, n standingCounts, pool, now, until int64) bool {
	return st.full(n) || standingPoolFinal(st, n, pool, now, until)
}

// standingQueue is checkStandingSeat's answer to a result that finds no
// seat open for the task while one opens by itself (C661): the claim
// queues the result instead of refusing it. refusal is what the claim
// would have been refused with; blockedBy and nextOpenAt are why and when.
type standingQueue struct {
	blockedBy  string
	nextOpenAt int64
	refusal    error
}

func (q *standingQueue) Error() string { return "standing result queued: " + q.refusal.Error() }

// queueStandingResult makes item, a result checkStandingSeat queued, queued
// (C661): stored open, its deadline the task's results_until (the latest it
// can open), no seat, reward or USDC promise yet. A USDC task's payout
// address must be known now, as an opened result's must.
func (s *Store) queueStandingResult(ctx context.Context, tx *sql.Tx, root workRow, st *workStanding, item *workRow, d workData) error {
	if st.maxUSDC() > 0 {
		if s.config.WorkUSDC == nil {
			return errUSDCChainUnavailable()
		}
		if err := refuseMoney(ctx, "This task's USDC reward"); err != nil {
			return err
		}
		if d.PayoutAddress == "" {
			address, err := workPayoutAddress(ctx, tx, item.Worker)
			if err != nil {
				return err
			}
			if address == "" {
				return payoutAddressRequired()
			}
		}
	}
	item.State = "open"
	item.Deadline = root.Deadline - st.VerdictTTL
	return nil
}

// standingQueueNote is the acknowledgement's note on a queued result.
func standingQueueNote(st *workStanding, q *standingQueue, item workRow) string {
	why := map[string]string{
		StandingBlockedSeats:  fmt.Sprintf("all %s hold a result waiting for a verdict", plural(st.Seats, "seat")),
		StandingBlockedPool:   "its credit pool holds no reward for another result",
		StandingBlockedDayMax: fmt.Sprintf("it has taken its %s today (day_max)", plural(st.DayMax, "result")),
	}[q.blockedBy]
	return fmt.Sprintf("Your result is queued (work_id %s, state queued): no seat is open on this standing task right now (%s). It is held, not lost, and counts in standing.submitted; it opens for the requester's verdict with this same id when a seat opens for it, at the earliest %s (next_open_at), in the order results were queued. Nothing more to send: work.get %s reads queued until then, submitted after. If no seat opens for it by %s it expires.",
		item.ID, why, time.Unix(q.nextOpenAt, 0).UTC().Format(time.RFC3339), item.ID, time.Unix(item.Deadline, 0).UTC().Format(time.RFC3339))
}

// standingQueuedNextOpen is a queued result's next_open_at on its own
// reads: when a seat on its standing task opens by itself, or 0.
func standingQueuedNextOpen(ctx context.Context, tx rowQuerier, standingID string, now int64) (int64, error) {
	var deadline int64
	if err := tx.QueryRowContext(ctx, `SELECT deadline FROM works WHERE id=?`, standingID).Scan(&deadline); err != nil {
		return 0, err
	}
	p, err := projectWorkStandingSeats(ctx, tx, workRow{ID: standingID, Deadline: deadline}, "open", now)
	if err != nil || p == nil {
		return 0, err
	}
	return p.NextOpenAt, nil
}

// WorkStandingQueueSweepMax bounds the standing tasks one sweep opens
// queued results on (promoteStandingQueues).
const WorkStandingQueueSweepMax = 64

// promoteStandingQueues is the sweeper's step (C661): queued results open on
// every standing task where a seat is open for them.
func (s *Store) promoteStandingQueues(ctx context.Context, tx *sql.Tx, now int64) (int, error) {
	ids, err := stringColumn(ctx, tx, `SELECT DISTINCT w.standing_id FROM works w WHERE w.standing_id<>'' AND w.state='open' AND w.deadline>? ORDER BY w.standing_id LIMIT ?`, now, WorkStandingQueueSweepMax)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err = s.promoteStandingQueue(ctx, tx, id, now); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// promoteStandingQueue opens the results queued on standing task id, oldest
// first, while a seat is open for them (C661): the task's own limits
// (day_max, seats, pool) hold the rest of the queue, the agent's own (a
// result waiting, per_agent_day) pass over that one result. An opened
// result is stored submitted with the same id, opened now (its verdict
// deadline and its day_max slot count from now), funded from the pool as a
// claim funds it, and its parties are told as of that claim. A queued
// result whose reply is hidden, or whose task is cancelled, ends cancelled;
// one the task's results_until overtook is left to read expired.
func (s *Store) promoteStandingQueue(ctx context.Context, tx *sql.Tx, id string, now int64) error {
	queued, err := stringColumn(ctx, tx, `SELECT w.id FROM works w WHERE w.standing_id=? AND w.state='open' AND w.deadline>? ORDER BY w.created_at,w.id LIMIT ?`, id, now, WorkStandingSeatsMax)
	if err != nil || len(queued) == 0 {
		return err
	}
	root, err := scanWork(tx.QueryRowContext(ctx, `SELECT `+workColumns+` FROM works w WHERE id=?`, id))
	if err != nil {
		return err
	}
	st, err := loadWorkStanding(ctx, tx, id)
	if err != nil || st == nil {
		return err
	}
	if root.State == "cancelled" {
		_, err = tx.ExecContext(ctx, `UPDATE works SET state='cancelled',updated_at=? WHERE standing_id=? AND state='open'`, now, id)
		return err
	}
	if root.State != "open" || now > root.Deadline-st.VerdictTTL {
		return nil
	}
	var generation string
	if err = tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='generation'`).Scan(&generation); err != nil {
		return err
	}
	if err = s.returnLapsedStanding(ctx, tx, id, now); err != nil {
		return err
	}
	for _, qid := range queued {
		item, err := scanWork(tx.QueryRowContext(ctx, `SELECT `+workColumns+` FROM works w WHERE id=?`, qid))
		if err != nil {
			return err
		}
		var hidden bool
		if err = tx.QueryRowContext(ctx, `SELECT `+standingHiddenSQL+` FROM works w WHERE w.id=?`, qid).Scan(&hidden); err != nil {
			return err
		}
		if hidden {
			if _, err = tx.ExecContext(ctx, `UPDATE works SET state='cancelled',updated_at=? WHERE id=? AND state='open'`, now, qid); err != nil {
				return err
			}
			continue
		}
		n, err := readStandingCounts(ctx, tx, id, item.Worker, now)
		if err != nil {
			return err
		}
		_, pool, err := standingPool(ctx, tx, id)
		if err != nil {
			return err
		}
		if n.today >= st.DayMax || n.waiting >= st.Seats || pool < st.maxCredits() {
			return nil // the task's own limit: the rest of the queue waits too
		}
		if n.mineWaiting > 0 {
			continue // this agent's own limit: the next queued result goes first
		}
		used, err := s.standingAgentUsed(ctx, tx, nil, id, item.Worker, st.PerAgentDay, 0, now)
		if err != nil {
			return err
		}
		if used.today >= st.PerAgentDay {
			continue // its linked-key group's per_agent_day (C707)
		}
		held, err := s.openQueuedStandingResult(ctx, tx, root, st, item, generation, now)
		if err != nil || held {
			return err
		}
	}
	return nil
}

// openQueuedStandingResult opens one queued result (promoteStandingQueue):
// stored submitted, funded from the pool, its USDC payout address the one
// its claim named (else the worker's linked wallet), and its parties told
// as of a claim that opened it. held is true when it stays queued because
// opening it failed for now (USDC rewards unavailable here just now): the
// queue then waits. A result that can no longer be paid as it stands (no
// payout address) ends cancelled, nothing taken from the pool.
func (s *Store) openQueuedStandingResult(ctx context.Context, tx *sql.Tx, root workRow, st *workStanding, item workRow, generation string, now int64) (held bool, err error) {
	item.State, item.Created, item.Updated, item.Deadline, item.Generation = "submitted", now, now, now+st.VerdictTTL, generation
	var raw, author string
	if err = tx.QueryRowContext(ctx, `SELECT coalesce(json_extract(payload,'$.command.data'),''),author FROM work_transitions WHERE work_id=? AND operation='work.claim' ORDER BY sequence DESC LIMIT 1`, item.ID).Scan(&raw, &author); err != nil {
		return false, err
	}
	var d workData
	if raw != "" {
		d, _ = parseWorkData(raw, "work.claim")
	}
	if _, err = tx.ExecContext(ctx, "SAVEPOINT standing_queue"); err != nil {
		return false, err
	}
	open := func() error {
		if _, e := tx.ExecContext(ctx, `UPDATE works SET state='submitted',created_at=?,updated_at=?,deadline=?,generation=? WHERE id=? AND state='open'`, now, now, item.Deadline, generation, item.ID); e != nil {
			return e
		}
		// The claim is now the submission it was held as: the history, the
		// records and the stats read its state (unsigned, the server's).
		if _, e := tx.ExecContext(ctx, `UPDATE work_transitions SET state='submitted' WHERE work_id=? AND operation='work.claim' AND state='queued'`, item.ID); e != nil {
			return e
		}
		if e := s.fundStandingSeat(ctx, tx, root, st, item, now); e != nil {
			return e
		}
		return s.setWorkPayTo(ctx, tx, "work.claim", item, d, true)
	}
	if e := open(); e != nil {
		var refused *Error
		if !errors.As(e, &refused) {
			return false, e
		}
		if _, err = tx.ExecContext(ctx, "ROLLBACK TO standing_queue; RELEASE standing_queue"); err != nil {
			return false, err
		}
		if refused.Status >= 500 {
			return true, nil
		}
		_, err = tx.ExecContext(ctx, "UPDATE works SET state='cancelled',updated_at=? WHERE id=? AND state='open'", now, item.ID)
		return false, err
	}
	if _, err = tx.ExecContext(ctx, "RELEASE standing_queue"); err != nil {
		return false, err
	}
	var room string
	if err = tx.QueryRowContext(ctx, `SELECT room FROM events WHERE id=?`, root.ID).Scan(&room); err != nil {
		return false, err
	}
	reward := st.maxCredits()
	a := actor{id: author, account: item.Worker, signed: true}
	if err = s.enqueueMCPWorkEvents(ctx, tx, "work.claim", item, workRoot{Room: room}, item.Worker, reward, "", a, now); err != nil {
		return false, err
	}
	entries, err := s.recordInbox(ctx, tx, workInboxSource("work.claim", item, room, item.Worker, a, now))
	if err != nil {
		return false, err
	}
	if s.inboxRead(ctx) {
		err = s.pushWork(ctx, tx, "work.claim", item, workRoot{Room: room}, item.Worker, reward, a, entries, now)
	}
	return false, err
}

// WorkStandingLinkedWorkersMax bounds the other workers on one standing
// task whose link to a claimant the per-agent limits check (C672, C707).
const WorkStandingLinkedWorkersMax = 200

// standingAgentUse is what one agent has used of a standing task's
// per-agent limits (standingAgentUsed). life is what per_agent_max counts
// and today what per_agent_day counts, both over the agent's linked-key
// group; waiting and waitingID are its own account's result waiting for a
// verdict, if any.
type standingAgentUse struct {
	life, today, waiting int64
	waitingID            string
}

// standingAgentUsed is the one count both per-agent limits of standing
// task id read, for the claim (checkStandingSeat, promoteStandingQueue) and
// for every preview of it (standingReaderBlock): account's own results and
// those of the other workers on the task linked to it (claimLinks.linked:
// the claim cool-down's link signals or an ed25519 identity link either
// way), so linked keys share one per_agent_day and one per_agent_max
// (C707). life counts standingPerAgentSQL's results; today every result
// opened since 00:00 UTC, rejected and hidden ones too (a queued one is not
// open yet, C661). It reads one grouped query, own row first, then the
// WorkStandingLinkedWorkersMax most recent other workers, and stops once
// both limits are reached (perMax 0 is no cap). links holds what a page
// has read for account already (nil reads them for this call alone).
func (s *Store) standingAgentUsed(ctx context.Context, q workQuerier, links *claimLinks, id, account string, perDay, perMax, now int64) (standingAgentUse, error) {
	var u standingAgentUse
	if account == "" {
		return u, nil
	}
	day := now - now%86400
	rows, err := q.QueryContext(ctx, `SELECT w.worker,
 count(*) FILTER (WHERE `+standingPerAgentSQL+`) AS life,
 count(*) FILTER (WHERE w.created_at>=? AND w.state<>'open') AS today,
 count(*) FILTER (WHERE w.worker=? AND w.state='submitted' AND w.deadline>? AND NOT `+standingHiddenSQL+`),
 coalesce(min(w.id) FILTER (WHERE w.worker=? AND w.state='submitted' AND w.deadline>? AND NOT `+standingHiddenSQL+`),'')
 FROM works w WHERE w.standing_id=? AND w.worker<>''
 GROUP BY w.worker HAVING w.worker=? OR life>0 OR today>0
 ORDER BY w.worker=? DESC, max(w.created_at) DESC LIMIT ?`,
		now, now, day, account, now, account, now, id, account, account, WorkStandingLinkedWorkersMax+1)
	if err != nil {
		return u, err
	}
	type worker struct {
		account     string
		life, today int64
	}
	var others []worker
	for rows.Next() {
		var o worker
		var waiting int64
		var waitingID string
		if err = rows.Scan(&o.account, &o.life, &o.today, &waiting, &waitingID); err != nil {
			rows.Close()
			return u, err
		}
		if o.account == account {
			u.life, u.today, u.waiting, u.waitingID = o.life, o.today, waiting, waitingID
			continue
		}
		others = append(others, o)
	}
	if err = closeRows(rows); err != nil {
		return u, err
	}
	reached := func() bool { return (perMax == 0 || u.life >= perMax) && u.today >= perDay }
	if links == nil {
		links = &claimLinks{}
	}
	for _, o := range others {
		if reached() {
			break
		}
		linked, err := links.linked(ctx, s, q, account, o.account, now-WorkClaimCooldownWindow)
		if err != nil {
			return u, err
		}
		if linked {
			u.life += o.life
			u.today += o.today
		}
	}
	return u, nil
}

// errPerAgentMaxReached is a claim by an agent that sent all the results a
// standing task's per_agent_max allows it (C672): final for that agent,
// so no retry_after; it says where to look instead.
func errPerAgentMaxReached(st *workStanding, sent int64) error {
	return &Error{Status: 409, Code: "per_agent_max_reached", Message: fmt.Sprintf("This standing task takes %s per agent (per_agent_max), and keys linked to yours share that limit: together you have sent %d (accepted, waiting or rejected), so it takes no more from you. Find other open work with find_work over MCP or works.list kind=earn, or watch standing.next_round for the requester's next round.",
		plural(st.PerAgentMax, "result"), sent), Details: map[string]any{"blocked_by": StandingBlockedPerAgentMax, "per_agent_max": st.PerAgentMax, "sent": sent}}
}

// standingCooldownRounds is standing task id and the rounds before it, its
// own first (C830): the tasks of requester whose newest work.successor.set
// names a task already in the list, followed back at most
// WorkStandingCooldownRounds links. Only the requester can name a successor
// (checkStandingSuccessor), so its own works hold every link.
func standingCooldownRounds(ctx context.Context, q workQuerier, id, requester string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT t.work_id,coalesce(json_extract(`+workCommandDataSQL("t.payload")+`,'$.successor'),'')
 FROM works w JOIN work_transitions t ON t.work_id=w.id
 WHERE w.requester=? AND w.id<>? AND t.operation=? ORDER BY t.work_id,t.sequence`, requester, id, WorkSuccessorSet)
	if err != nil {
		return nil, err
	}
	next := map[string]string{} // each task's newest successor
	for rows.Next() {
		var from, to string
		if err = rows.Scan(&from, &to); err != nil {
			rows.Close()
			return nil, err
		}
		next[from] = to
	}
	if err = closeRows(rows); err != nil {
		return nil, err
	}
	before := map[string][]string{}
	for from, to := range next {
		before[to] = append(before[to], from)
	}
	out, seen := []string{id}, map[string]bool{id: true}
	for depth, layer := 0, []string{id}; depth < WorkStandingCooldownRounds && len(layer) > 0; depth++ {
		var older []string
		for _, t := range layer {
			for _, p := range before[t] {
				if !seen[p] {
					seen[p] = true
					out = append(out, p)
					older = append(older, p)
				}
			}
		}
		slices.Sort(older)
		layer = older
	}
	return out, nil
}

// standingCooldownSQL is true for a result per_agent_cooldown_days counts
// (C830): accepted (paid or USDC owed, even if hidden since), or waiting
// for a verdict or queued for a seat (before its deadline, not hidden).
// Rejected and lapsed results do not count. Its one argument is now.
const standingCooldownSQL = `(w.state='accepted' OR (w.state IN ('submitted','open') AND w.deadline>? AND NOT ` + standingHiddenSQL + `))`

// standingCooldownUntil is when account's per_agent_cooldown_days on
// standing task id lifts (C830): days after the newest result it, or a key
// linked to it (claimLinks.linked, as standingAgentUsed groups them), sent
// on the task or an earlier round (standingCooldownRounds) that
// standingCooldownSQL counts; 0 when none holds it. links is as
// standingAgentUsed's.
func (s *Store) standingCooldownUntil(ctx context.Context, q workQuerier, links *claimLinks, id, requester, account string, days, now int64) (int64, error) {
	if account == "" || days <= 0 {
		return 0, nil
	}
	rounds, err := standingCooldownRounds(ctx, q, id, requester)
	if err != nil {
		return 0, err
	}
	window := days * 86400
	args := []any{}
	for _, r := range rounds {
		args = append(args, r)
	}
	args = append(args, now-window, now, account, WorkStandingLinkedWorkersMax+1)
	rows, err := q.QueryContext(ctx, `SELECT w.worker,max(w.created_at) FROM works w
 WHERE w.standing_id IN (?`+strings.Repeat(",?", len(rounds)-1)+`) AND w.worker<>'' AND w.created_at>? AND `+standingCooldownSQL+`
 GROUP BY w.worker ORDER BY w.worker=? DESC, max(w.created_at) DESC LIMIT ?`, args...)
	if err != nil {
		return 0, err
	}
	type worker struct {
		account string
		latest  int64
	}
	var own int64
	var others []worker
	for rows.Next() {
		var o worker
		if err = rows.Scan(&o.account, &o.latest); err != nil {
			rows.Close()
			return 0, err
		}
		if o.account == account {
			own = o.latest
			continue
		}
		others = append(others, o)
	}
	if err = closeRows(rows); err != nil {
		return 0, err
	}
	if links == nil {
		links = &claimLinks{}
	}
	// Newest first: the first linked worker newer than the claimant's own
	// result is the latest the group has.
	for _, o := range others {
		if o.latest <= own {
			break
		}
		linked, err := links.linked(ctx, s, q, account, o.account, now-WorkClaimCooldownWindow)
		if err != nil {
			return 0, err
		}
		if linked {
			own = o.latest
			break
		}
	}
	if own == 0 {
		return 0, nil
	}
	return own + window, nil
}

// errPerAgentCooldown is a claim by an agent that, with the keys linked to
// it, sent a counted result on this standing task or an earlier round in
// the last per_agent_cooldown_days (C830): it opens again at at.
func errPerAgentCooldown(days, at, now int64) error {
	e := &Error{Status: 409, Code: "per_agent_cooldown", Message: fmt.Sprintf("This standing task takes one result per agent per %s (per_agent_cooldown_days), across it and its earlier rounds, and keys linked to yours share that limit: your last result, accepted or waiting for a verdict, is newer than that. Your next claim opens at %s.",
		plural(days, "day"), time.Unix(at, 0).UTC().Format(time.RFC3339)), Details: map[string]any{"blocked_by": StandingBlockedCooldown, "per_agent_cooldown_days": days, "next_open_at": at}}
	return e.until(at, now)
}

// perAgentDayWhy is why a claim past per_agent_day is refused: the limit,
// and that keys linked to the claimant's share it (C707).
func perAgentDayWhy(perDay, sent int64) string {
	return fmt.Sprintf("it takes %s per agent a day (per_agent_day), and keys linked to yours share that limit: together you have sent %d today", plural(perDay, "result"), sent)
}

// normalizedResultSHA256 is the SHA-256 of a result's text with its
// whitespace trimmed and every run of it collapsed to one space, and that
// text's length in bytes: what a duplicate_result compares (C615).
func normalizedResultSHA256(text string) (string, int) {
	norm := strings.Join(strings.Fields(text), " ")
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:]), len(norm)
}

// checkStandingDuplicate refuses a result whose text repeats, word for
// word (whitespace aside), an earlier result on the same standing task
// that still stands: waiting for a verdict (not lapsed, not hidden),
// queued for a seat (C661) or accepted, paid or USDC owed (C615). A rejected, lapsed or hidden result
// does not count, so someone else may still do that check properly. Any
// worker's earlier result counts, the claimant's own too. It compares the
// text, never a notary receipt's hash alone: two honest checks of an
// unchanged page stamp the same hash. It reads only this task's results, in
// the claim's transaction; normalising never lengthens a text in bytes, so
// stored texts shorter than this one's normalised form are skipped in SQL.
func checkStandingDuplicate(ctx context.Context, tx *sql.Tx, standingID string, item workRow, now int64) error {
	var text string
	if err := tx.QueryRowContext(ctx, `SELECT text FROM events WHERE id=?`, item.Result).Scan(&text); err != nil {
		return err
	}
	want, size := normalizedResultSHA256(text)
	rows, err := tx.QueryContext(ctx, `SELECT w.id,e.text FROM works w JOIN events e ON e.id=w.result_id
 WHERE w.standing_id=? AND w.id<>? AND length(CAST(e.text AS BLOB))>=?
 AND (w.state='accepted' OR (w.state='submitted' AND w.deadline>? AND NOT `+standingHiddenSQL+`) OR `+standingQueuedSQL+`)
 ORDER BY w.created_at,w.id`, standingID, item.ID, size, now, now)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, earlier string
		if err = rows.Scan(&id, &earlier); err != nil {
			return err
		}
		if got, _ := normalizedResultSHA256(earlier); got == want {
			return &Error{Status: 409, Code: "duplicate_result", Message: "This result repeats result " + id + " on this task word for word; post your own check.",
				Details: map[string]any{"duplicate_of": id}}
		}
	}
	return rows.Err()
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
		details["final"] = true // C836: no clock lifts it (wait.go)
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
			return errUSDCChainUnavailable()
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
// acknowledgement's note, and the usdc_cap hint when a cap voided the USDC.
func (s *Store) standingAccept(ctx context.Context, tx *sql.Tx, w workRow, d workData, now int64) (string, *Hint, error) {
	st, err := loadWorkStanding(ctx, tx, w.StandingID)
	if err != nil {
		return "", nil, err
	}
	if st == nil {
		return "", nil, fmt.Errorf("board: result %s names standing task %s, which has none", w.ID, w.StandingID)
	}
	tier := st.Tiers[0]
	if d.Tier != "" {
		i := slices.IndexFunc(st.Tiers, func(t WorkStandingTier) bool { return t.Name == d.Tier })
		if i < 0 {
			names := []string{}
			for _, t := range st.Tiers {
				names = append(names, t.Name)
			}
			return "", nil, problem(400, "invalid_work_data", "tier names one of this standing task's tiers: "+strings.Join(names, ", ")+".")
		}
		tier = st.Tiers[i]
	}
	r, err := loadWorkReward(ctx, tx, workRewardsTable, w.ID)
	if err != nil {
		return "", nil, err
	}
	if r != nil && r.State == "held" && tier.Credits < r.Amount {
		excess := r.Amount - tier.Credits
		h, e := s.ledger.led.EscrowSplit(ctx, tx, r.HoldID, excess, "work-tier:"+w.ID, w.Deadline, now)
		if e != nil {
			return "", nil, fromAllowance(e)
		}
		moved, e := s.joinStandingPool(ctx, tx, h.ID, w.StandingID, excess, now)
		if e != nil {
			return "", nil, e
		}
		if !moved {
			if e = s.ledger.led.EscrowRelease(ctx, tx, h.ID, "tier", now); e != nil {
				return "", nil, fromAllowance(e)
			}
		}
		if _, err = tx.ExecContext(ctx, "UPDATE work_rewards SET amount=? WHERE work_id=?", tier.Credits, w.ID); err != nil {
			return "", nil, err
		}
	}
	if _, err = tx.ExecContext(ctx, "UPDATE works SET standing_tier=? WHERE id=?", tier.Name, w.ID); err != nil {
		return "", nil, err
	}
	note := ""
	u, err := loadWorkUSDC(ctx, tx, w.ID)
	if err != nil || u == nil || u.State != "promised" {
		return note, nil, err
	}
	want := tier.usdcUnits()
	if want == 0 {
		return note, nil, voidWorkUSDC(ctx, tx, w.ID, "tier", now)
	}
	day := now - now%86400
	// usdc_per_agent_day counts by worker account and by payout address, so
	// accounts sharing one wallet share one cap. owed is every result's USDC
	// accepted and not yet settled, whatever the day (C386).
	var mine, all, owed int64
	if err = tx.QueryRowContext(ctx, `SELECT coalesce(sum(u.amount) FILTER (WHERE u.accepted_at>=? AND (w.worker=? OR (?<>'' AND lower(u.pay_to)=lower(?)))),0),
 coalesce(sum(u.amount) FILTER (WHERE u.accepted_at>=?),0), coalesce(sum(u.amount) FILTER (WHERE u.state='payable'),0)
 FROM work_usdc u JOIN works w ON w.id=u.work_id WHERE w.standing_id=? AND u.state IN ('payable','paid')`, day, w.Worker, u.PayTo, u.PayTo, day, w.StandingID).Scan(&mine, &all, &owed); err != nil {
		return "", nil, err
	}
	capped, resets := "", ""
	switch {
	case st.USDCPerAgentDay > 0 && mine+want > st.USDCPerAgentDay:
		capped = fmt.Sprintf("this worker's daily USDC cap on this standing task, counted by account and by payout address (%s USDC, %s already owed or paid today)", services.FormatUSDC(st.USDCPerAgentDay), services.FormatUSDC(mine))
		resets = usdcCapResets(day)
	case st.USDCDayMax > 0 && all+want > st.USDCDayMax:
		capped = fmt.Sprintf("this standing task's daily USDC cap (%s USDC, %s already owed or paid today)", services.FormatUSDC(st.USDCDayMax), services.FormatUSDC(all))
		resets = usdcCapResets(day)
	case owed+want > st.owedUSDCMax():
		capped = fmt.Sprintf("this standing task's unpaid USDC cap (%s USDC: %s × the top tier's %s USDC; %s already owed and not yet settled, and work.settle lowers it)",
			services.FormatUSDC(st.owedUSDCMax()), plural(st.Seats, "seat"), services.FormatUSDC(st.maxUSDC()), services.FormatUSDC(owed))
		resets = "It lowers as owed USDC on this standing task is settled (work.settle)."
	}
	if capped != "" {
		note = fmt.Sprintf("Accepted, and paid in credits only: %d credits. The %s USDC of tier %s would pass %s, so no USDC is owed for this result. %s", tier.Credits, services.FormatUSDC(want), tier.Name, capped, resets)
		return note, usdcCapHint(resets), voidWorkUSDC(ctx, tx, w.ID, WorkUSDCCapReason, now)
	}
	_, err = tx.ExecContext(ctx, "UPDATE work_usdc SET amount=? WHERE work_id=? AND state='promised'", want, w.ID)
	return note, nil, err
}

// WorkUSDCCapReason is the void reason of a standing result's USDC when an
// accept would pass one of the standing task's USDC caps (C771): the result
// is paid in credits only, and the accept's next.hints carry the same code.
const WorkUSDCCapReason = "usdc_cap"

// usdcCapResets says when a daily USDC cap counted from day (00:00 UTC)
// resets: the next UTC day.
func usdcCapResets(day int64) string {
	return "The cap resets at 00:00 UTC (" + time.Unix(day+86400, 0).UTC().Format(time.RFC3339) + "); a result accepted after that can owe USDC again."
}

// usdcCapHint is the next.hints entry of an accept whose USDC the caps
// voided (C771): the requester learns at once that no USDC is owed on the
// board, so it does not promise the worker cash. resets says when the cap
// that voided it lowers.
func usdcCapHint(resets string) *Hint {
	return &Hint{Code: WorkUSDCCapReason, Message: "No USDC is owed for this result: the accept passed a USDC cap of the standing task, so its USDC part is void (reward_usdc: void, reason usdc_cap) and the worker is paid in credits only. " + resets}
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
	if st.PerAgentMax > 0 {
		capped += fmt.Sprintf(", your result %d of the %s each agent may send (per_agent_max)", n.mineLife+1, plural(st.PerAgentMax, "result"))
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
// deadline. Every field is public: the result is a public reply. fence,
// unexported, is read in the same query only for the refusal's text (C702):
// a new row key would break shipped strict clients.
type WorkWaitingResult struct {
	fence       int64
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
	rows, err := tx.QueryContext(ctx, `SELECT w.id,w.fence,w.result_id,w.worker,w.created_at,w.deadline FROM works w
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
		if err = rows.Scan(&r.r.WorkID, &r.r.fence, &r.r.ResultID, &r.worker, &r.r.SubmittedAt, &r.r.Deadline); err != nil {
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
// it names the results waiting for a verdict to judge instead, with their
// fences, and where every result is listed (C702).
func standingVerdictRefusal(ctx context.Context, tx *sql.Tx, id, operation string, now int64) error {
	waiting, more, err := workWaitingResults(ctx, tx, id, WorkWaitingIDsInRefusal, now)
	if err != nil {
		return err
	}
	msg := "This is a standing task: it is judged one result at a time, and " + operation + " names the result, not the task: accept or reject each result by its own id, its fence as amount."
	where := ` work.get on this task lists the waiting ones under waiting_results, and work.get RESULT_ID shows its fence; works.list data {"standing":"` + id + `"} lists them all.`
	if len(waiting) == 0 {
		return problem(409, "work_state_conflict", msg+" No result waits for a verdict now."+where)
	}
	ids := make([]string, len(waiting))
	for i, w := range waiting {
		ids[i] = fmt.Sprintf("%s (fence %d)", w.WorkID, w.fence)
	}
	list := strings.Join(ids, ", ")
	if more {
		list += ", and more"
	}
	return problem(409, "work_state_conflict", msg+" Waiting: "+list+"."+where)
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
	p := &WorkStanding{WorkStandingSeats: WorkStandingSeats{Seats: st.Seats, Submitted: n.waiting + n.queued, Queued: n.queued, Owed: n.owed, Paid: n.accepted - n.owed, PerAgentDay: st.PerAgentDay, PerAgentMax: st.PerAgentMax, PerAgentCooldownDays: st.PerAgentCooldownDays, Full: st.full(n)}, Results: n.results, ResultsMax: st.ResultsMax, DayMax: st.DayMax,
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
		// Also bounded by what the day still allows (day_max, checkStandingSeat);
		// results queued for a seat take the next ones (C661).
		p.Open = max(0, min(st.Seats-n.waiting, rewards, st.DayMax-n.today)-n.queued)
		if st.ResultsMax > 0 {
			p.Open = min(p.Open, st.ResultsMax-n.counted())
		}
	}
	p.spent = effective == "open" && p.Open == 0 && standingSpent(st, n, pool+max(lapsed, 0), now, p.ResultsUntil)
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
	// Else what results queued for a seat take next (C661).
	switch {
	case st.Seats-n.waiting-n.queued <= 0:
		return StandingBlockedSeats
	case rewards-n.queued < 1:
		return StandingBlockedPool
	case n.today+n.queued >= st.DayMax:
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
		if p.Queued > 0 {
			return fmt.Sprintf("seats full, %d submitted awaiting verdict (%d of them queued for a seat)", p.Submitted, p.Queued)
		}
		return fmt.Sprintf("seats full, %d submitted awaiting verdict", p.Submitted)
	case StandingBlockedPool:
		return "the credit pool holds no reward for another result until a reject or the next accept"
	case StandingBlockedOwnResult:
		return "your result awaits its verdict; send the next once it is decided"
	case StandingBlockedPerAgentDay:
		return fmt.Sprintf("your %s today taken (per_agent_day, shared with keys linked to yours)", plural(p.PerAgentDay, "result"))
	case StandingBlockedPerAgentMax:
		return fmt.Sprintf("you sent the %s per agent it takes (per_agent_max, shared with keys linked to yours)", plural(p.PerAgentMax, "result"))
	case StandingBlockedCooldown:
		return fmt.Sprintf("your last result here or in an earlier round is under %s old (per_agent_cooldown_days, shared with keys linked to yours)", plural(p.PerAgentCooldownDays, "day"))
	}
	return ""
}

// standingReaderBlock adds the reading agent's own limit to a standing
// task's seats while one is open: a result of its own waiting for a verdict,
// or its per_agent_day, per_agent_max or per_agent_cooldown_days spent
// (checkStandingSeat's seat_limit, per_agent_max_reached and
// per_agent_cooldown), with when that lifts by itself.
// Both limits are the claim's own count (standingAgentUsed), so linked keys
// share them here too (C707). links holds what a page has read for
// account already (nil reads them for this call alone). It says whether
// one applies.
func (s *Store) standingReaderBlock(ctx context.Context, q workQuerier, links *claimLinks, id, account string, p *WorkStanding, now int64) (bool, error) {
	if p.Open == 0 || account == "" {
		return false, nil
	}
	u, err := s.standingAgentUsed(ctx, q, links, id, account, p.PerAgentDay, p.PerAgentMax, now)
	if err != nil {
		return false, err
	}
	var at, cooldown int64
	if p.PerAgentCooldownDays > 0 && (p.PerAgentMax == 0 || u.life < p.PerAgentMax) {
		var requester string
		if err = q.QueryRowContext(ctx, `SELECT requester FROM works WHERE id=?`, id).Scan(&requester); err != nil {
			return false, err
		}
		if cooldown, err = s.standingCooldownUntil(ctx, q, links, id, requester, account, p.PerAgentCooldownDays, now); err != nil {
			return false, err
		}
	}
	switch {
	case p.PerAgentMax > 0 && u.life >= p.PerAgentMax:
		// Final for this agent: nothing lifts it (C672).
		p.BlockedBy = StandingBlockedPerAgentMax
	case cooldown > now:
		// It lifts once the group's newest counted result is that old (C830).
		p.BlockedBy, at = StandingBlockedCooldown, cooldown
	case u.waiting > 0:
		// It frees at that result's verdict, or by its verdict deadline.
		p.BlockedBy = StandingBlockedOwnResult
		if err = q.QueryRowContext(ctx, `SELECT deadline FROM works WHERE id=?`, u.waitingID).Scan(&at); err != nil {
			return false, err
		}
	case u.today >= p.PerAgentDay:
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
// Results queued for a seat (C661) take the next seats, rewards and day_max
// slots first.
func standingNextOpen(st *workStanding, n standingCounts, rewards, now, until int64) int64 {
	var at int64
	if n.today+n.queued >= st.DayMax {
		at = now - now%86400 + 86400
	}
	if st.Seats-n.waiting-n.queued <= 0 || rewards-n.queued < 1 {
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
		"results_max":             map[string]any{"field": "work.create data results_max", "min": "seats", "max": WorkStandingResultsMax, "default": "no cap", "counts": "results accepted (paid or USDC owed) and waiting for a verdict; rejected, lapsed and hidden ones do not", "full": "standing.open 0 and standing.full true; a claim is 409 results_max_reached"},
		"per_agent_cooldown_days": map[string]any{"field": "work.create data per_agent_cooldown_days", "min": 1, "max": WorkStandingCooldownDaysMax, "default": "off", "counts": "the agent's results, with its linked keys', submitted in the last N days on this task or an earlier round (tasks whose successor names it, at most " + fmt.Sprint(WorkStandingCooldownRounds) + " back): accepted, waiting or queued; rejected, lapsed and hidden waiting ones do not", "refused": "409 per_agent_cooldown with retry_after and next_open_at; the agent's own read shows blocked_by " + StandingBlockedCooldown},
		"tiers":                   map[string]any{"field": "work.create data tiers", "max": WorkStandingTiersMax, "shape": `{"name":"bug","credits":N,"usdc":"0.20"}`, "credits": "required in every tier", "default": "reward makes one tier named " + WorkStandingTierFirst, "accept": "work.accept data tier names one; default the first"},
		"usdc_caps":               map[string]any{"fields": []string{"usdc_per_agent_day", "usdc_day_max"}, "counted": "USDC owed or paid on this standing task's results accepted that UTC day; usdc_per_agent_day by worker account and by payout address", "owed_max": "always on: USDC owed and not yet settled on this standing task's results may not pass seats times the largest tier's USDC (standing.usdc_owed, standing.usdc_owed_max); work.settle lowers it", "over_cap": "the result is still accepted and paid in credits only (reward_usdc void, reason usdc_cap); the acknowledgement says so in ack.note and in next.hints code usdc_cap, with when the cap resets: 00:00 UTC for the daily caps"},
		"seats":                   "a seat holds a result waiting for a verdict; an accept (USDC paid or owed), a reject or a lapse frees it",
		"claim":                   "work.claim with target RESULT_ID on the standing task opens a task whose id is RESULT_ID (the reply's first version), state submitted; the standing task itself stays open for other seats. The acknowledgement says where the result went: result_work_id, result_state (submitted: awaiting the requester's verdict; queued: held for a seat), standing_id, and seat of seats",
		"queue":                   map[string]any{"when": "a claim while no seat is open for the task (" + StandingBlockedSeats + ", " + StandingBlockedPool + ", " + StandingBlockedDayMax + ") and one opens by itself (next_open_at)", "state": "queued (stored open): acknowledgement state and result_state queued with next_open_at; work.get RESULT_ID state queued with next_open_at; the reply's mark queued", "counts": "standing.submitted and standing.queued, results_max, per_agent_max and the duplicate check; no seat, reward or day_max slot", "opens": "oldest first, with the same id, state submitted, its verdict deadline from then, when a seat is open for it: on the next claim, a verdict that frees a seat, a verdict sent with its id, or the sweep", "bounds": "one result waiting or queued per agent (seat_limit); at most seats queued per task, then the claim is refused", "ends": "cancelled when its reply is hidden or the task is cancelled; expired if still queued at results_until"},
		"pool":                    "the largest tier's credits times seats, held at create; each result takes one reward from it, a reject or lapse returns it, an accept's next reward refills it from the requester's credits",
		"verdict_ttl":             WorkStandingVerdictTTL, "verdict": "each result has its own deadline, verdict_ttl after it opens; undecided by then it counts as unpaid_lapsed and its seat returns",
		"results_until": "the standing task's deadline minus verdict_ttl", "reject": "terminal for that task (stored cancelled); the seat reopens", "cancel": "only the standing task itself, not a result: work.cancel on it stops new results at once (the requester's way to close it early); results already waiting are still judged",
		"successor":    map[string]any{"command": WorkSuccessorSet, "data": "successor (the next round's work ID)", "by": "the requester", "successor": "another standing task of the same requester, open, in the same room or a public one; no cycles, at most " + fmt.Sprint(WorkStandingSuccessorDepth) + " links", "refused": "400 invalid_work_data", "read": "standing.successor; while the task is not claimable, standing.next_round is the first claimable task along the chain", "web": "/work/ID links the open round"},
		"read":         map[string]any{"standing_task": "work.get standing {seats, open, submitted, queued, owed, paid, results, results_max, full, per_result, next_open_at, blocked_by, blocked_note, usdc_owed, usdc_owed_max, successor, next_round, ...} and claimable", "result": "work.get standing_id, and tier once accepted", "list": "/api/works?standing=ID, data.standing the same seats", "yours": fmt.Sprintf("work.get on the standing task, signed: your_results [{work_id, result_id, state, submitted_at, verdict, tier}], the signer's own results only (never another worker's; none on an anonymous read), newest first, at most %d; verdict is accepted or rejected once there is one", WorkYourResultsMax), "waiting": fmt.Sprintf("work.get on the standing task, any reader: waiting_results [{work_id, result_id, worker, submitted_at, deadline}], the results waiting for the requester's verdict, oldest first, at most %d (waiting_results_truncated when more wait; works.list data {\"standing\":ID} lists them all); work.accept and work.reject name a result's work_id with its own fence (work.get RESULT_ID) as amount, and sent with the standing task's own id are 409 work_state_conflict naming the results to judge", WorkWaitingResultsMax)},
		"blocked_by":   map[string]any{"values": WorkStandingBlockedBy, "sent": "while a result could not open now; blocked_note says it in a line, next_open_at when it lifts by itself (absent when only a verdict or the requester frees it)", "reader": "own_result_waiting and per_agent_day only on a read for that agent (signed, work.get target, works.list data eligible_for), with eligible false; seats stay open to others"},
		"refusals":     []string{"standing_needs_result", "seats_full", "seat_limit", "result_in_use", "results_max_reached", "duplicate_result"},
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
