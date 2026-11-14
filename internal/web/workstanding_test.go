package web

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
)

// A standing task (C278) reads as one on /work and on its page: reward per
// result, seats open, paid count, and the results it opened; each result's
// page links back to it.
func TestWorkSSRStandingTask(t *testing.T) {
	standingID, resultID := strings.Repeat("c", 32), strings.Repeat("d", 32)
	requester := board.AgentRef{ID: strings.Repeat("2", 64)}
	root := board.Work{ID: standingID, Room: "bounties", Title: "Standing bug bounty", State: "open", StoredState: "open", Requester: requester, Eligibility: "open",
		Reward: &board.WorkReward{Amount: 14000, Unit: "credit", State: "held"}, Deadline: 1789171200,
		Standing: &board.WorkStanding{WorkStandingSeats: board.WorkStandingSeats{Seats: 10, Open: 7, Submitted: 3, Paid: 12, PerAgentDay: 1}, Results: 15, DayMax: 10, VerdictTTL: board.WorkStandingVerdictTTL, ResultsUntil: 1788912000,
			Tiers: []board.WorkStandingTier{{Name: "bug", Credits: 2000, USDC: "0.2"}, {Name: "bug+fix", Credits: 2000, USDC: "0.4"}}}}
	result := board.Work{ID: resultID, Room: "bounties", Title: "Standing bug bounty", State: "cancelled", StoredState: "cancelled", Requester: requester, Eligibility: "open", StandingID: standingID, Deadline: 1788566400}
	s := &testService{execute: func(c board.Command) (board.Result, error) {
		switch c.Operation {
		case "room.get":
			return board.Result{OK: true, Room: &board.Room{Name: c.Room, Visibility: "public"}}, nil
		case "works.list":
			if strings.Contains(c.Data, standingID) {
				return board.Result{OK: true, Data: map[string]any{"works": []board.Work{result}}}, nil
			}
			return board.Result{OK: true, Data: map[string]any{"works": []board.Work{root}}}, nil
		case "work.get":
			if c.MessageID == resultID {
				return board.Result{OK: true, Data: map[string]any{"work": result}}, nil
			}
			return board.Result{OK: true, Data: map[string]any{"work": root}}, nil
		case "work.history":
			return board.Result{OK: true, Data: map[string]any{"work_id": c.MessageID, "transitions": []board.WorkTransition{}}}, nil
		}
		return board.Result{OK: true}, nil
	}}
	get := func(target string) string {
		w := httptest.NewRecorder()
		Handler(s).ServeHTTP(w, httptest.NewRequest("GET", target, nil))
		if w.Code != 200 {
			t.Fatalf("%s: %d", target, w.Code)
		}
		return w.Body.String()
	}
	line := "Standing · 2000 credits + 0.2 USDC per result (or another tier) · Seats open now: 7 of 10 · 12 paid"
	// C434: the reward reads per result, then the seats and the pool held.
	// (the template escapes the helper's "+" as "&#43;")
	reward := "Reward bug 2,000 credits &#43; 0.2 USDC, bug&#43;fix 2,000 credits &#43; 0.4 USDC per result; 10 seats (14,000 held)"
	if body := get("/work?kind=open"); !strings.Contains(body, line) || !strings.Contains(body, reward) || strings.Contains(body, "Reward 14000") {
		t.Errorf("/work card misses %q or %q, or shows the pool as the reward", line, reward)
	}
	body := get("/work/" + standingID)
	if strings.Contains(body, "Reward 14000") {
		t.Error("the standing page shows the pool as the reward")
	}
	// C481, C585: the state badge says the standing task stays open, and the
	// seats open today follow it, next to the reward.
	for _, want := range []string{line, reward, "(14,000 held) · open</span>", "</p>\n<p class=\"small standing-line\">Standing · 2000", `<h2 id="standing-heading">Standing task</h2>`, "</code>: 2000 credits + 0.4 USDC", `href="/work/` + resultID + `"`, "rejected", `&#34;target&#34;: &#34;REPLY_ID&#34;`} {
		if !strings.Contains(body, want) {
			t.Errorf("standing page misses %q", want)
		}
	}
	if body = get("/work/" + resultID); !strings.Contains(body, `Result for <a href="/work/`+standingID+`">the standing task</a>`) || !strings.Contains(body, "<strong>rejected</strong>") {
		t.Error("the result page does not link its standing task")
	}
}

// C346: a standing task with a results cap says so on /work, and once the
// cap is reached it reads full instead of open seats.
func TestWorkSSRStandingTaskResultsMax(t *testing.T) {
	standingID := strings.Repeat("e", 32)
	requester := board.AgentRef{ID: strings.Repeat("3", 64)}
	standing := &board.WorkStanding{WorkStandingSeats: board.WorkStandingSeats{Seats: 3, Open: 1, Submitted: 1, Paid: 1, PerAgentDay: 1}, Results: 2, ResultsMax: 3, DayMax: 3,
		VerdictTTL: board.WorkStandingVerdictTTL, ResultsUntil: 1788912000, Tiers: []board.WorkStandingTier{{Name: "reward", Credits: 300}}}
	root := board.Work{ID: standingID, Room: "bounties", Title: "Capped bounty", State: "open", StoredState: "open", Requester: requester, Eligibility: "open",
		Reward: &board.WorkReward{Amount: 900, Unit: "credit", State: "held"}, Deadline: 1789171200, Standing: standing}
	s := &testService{execute: func(c board.Command) (board.Result, error) {
		switch c.Operation {
		case "room.get":
			return board.Result{OK: true, Room: &board.Room{Name: c.Room, Visibility: "public"}}, nil
		case "works.list":
			return board.Result{OK: true, Data: map[string]any{"works": []board.Work{root}}}, nil
		}
		return board.Result{OK: true}, nil
	}}
	get := func() string {
		w := httptest.NewRecorder()
		Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/work?kind=open", nil))
		if w.Code != 200 {
			t.Fatalf("/work: %d", w.Code)
		}
		return w.Body.String()
	}
	if body := get(); !strings.Contains(body, "Seats open now: 1 of 3 · 1 paid · up to 3 results") {
		t.Error("/work misses the results cap")
	}
	// C384: no seat open, one opens at the next day boundary.
	standing.Open, standing.NextOpenAt = 0, 1788652800
	if body := get(); !strings.Contains(body, "Seats open now: 0 of 3 · Next seats open 2026-09-06 00:00 UTC · 1 paid") {
		t.Error("/work misses when the next seat opens")
	}
	// C444: why no seat is open, before a worker does the work.
	standing.NextOpenAt, standing.Submitted, standing.BlockedBy, standing.BlockedNote = 0, 3, board.StandingBlockedSeats, "seats full, 3 submitted awaiting verdict"
	if body := get(); !strings.Contains(body, "Seats open now: 0 of 3 (seats full, 3 submitted awaiting verdict) · 1 paid") {
		t.Error("/work misses why no seat is open")
	}
	standing.BlockedBy, standing.BlockedNote = "", ""
	standing.Open, standing.Full, standing.Paid = 0, true, 2
	if body := get(); !strings.Contains(body, "full: all 3 results taken · 2 paid</p>") {
		t.Error("/work does not read the capped task as full")
	}
}

// C678: a stranger reading a standing task's own page with no seat open sees
// the seats open now out of the total, when the next open (UTC, and how far
// off) and the per-agent limit, all from work.get's standing.
func TestWorkSSRStandingNextSeatsOpen(t *testing.T) {
	id := strings.Repeat("9", 32)
	next := time.Now().Unix() + 5*3600 + 600
	standing := &board.WorkStanding{WorkStandingSeats: board.WorkStandingSeats{Seats: 3, Paid: 6, PerAgentDay: 1, PerAgentMax: 2, NextOpenAt: next,
		BlockedBy: board.StandingBlockedDayMax, BlockedNote: "today's 3 results taken (day_max)"}, Results: 6, ResultsMax: 30, DayMax: 3, OpenedToday: 3,
		VerdictTTL: board.WorkStandingVerdictTTL, ResultsUntil: next + 86400, Tiers: []board.WorkStandingTier{{Name: "reward", Credits: 500}}}
	item := board.Work{ID: id, Room: "bounties", Title: "Second-reader seat", State: "open", StoredState: "open", Requester: board.AgentRef{ID: strings.Repeat("4", 64)},
		Eligibility: "open", Reward: &board.WorkReward{Amount: 1500, Unit: "credit", State: "held"}, Deadline: next + 2*86400, Standing: standing}
	s := &testService{execute: func(c board.Command) (board.Result, error) {
		switch c.Operation {
		case "room.get":
			return board.Result{OK: true, Room: &board.Room{Name: c.Room, Visibility: "public"}}, nil
		case "work.get":
			return board.Result{OK: true, Data: map[string]any{"work": item}}, nil
		case "works.list":
			return board.Result{OK: true, Data: map[string]any{"works": []board.Work{}}}, nil
		case "work.history":
			return board.Result{OK: true, Data: map[string]any{"work_id": c.MessageID, "transitions": []board.WorkTransition{}}}, nil
		}
		return board.Result{OK: true}, nil
	}}
	w := httptest.NewRecorder()
	Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/work/"+id, nil))
	if w.Code != 200 {
		t.Fatalf("/work/%s: %d", id, w.Code)
	}
	at := time.Unix(next, 0).UTC().Format("2006-01-02 15:04 UTC")
	want := "Seats open now: 0 of 3 (today&#39;s 3 results taken (day_max)) · Next seats open " + at + " (in 5 h) · 6 paid · up to 30 results · up to 2 results per agent</p>"
	if body := w.Body.String(); !strings.Contains(body, want) {
		t.Errorf("the standing task's page misses its seats, next opening or per-agent limit; want %q", want)
	}
}

func TestInLabel(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	for at, want := range map[int64]string{-5: "", 0: "", 30: "in under a minute", 125: "in 2 min", 5*3600 + 59: "in 5 h", 47 * 3600: "in 47 h", 3*86400 + 10: "in 3 days"} {
		if got := inLabel(now.Unix()+at, now); got != want {
			t.Errorf("inLabel(now%+d) = %q, want %q", at, got, want)
		}
	}
}

// C391: a standing task that takes no claim and names a claimable next
// round links it on its page.
func TestWorkSSRStandingNextRound(t *testing.T) {
	standingID, nextID := strings.Repeat("a", 32), strings.Repeat("b", 32)
	standing := &board.WorkStanding{WorkStandingSeats: board.WorkStandingSeats{Seats: 3, Full: true, Paid: 3, PerAgentDay: 1}, Results: 3, ResultsMax: 3, DayMax: 3,
		VerdictTTL: board.WorkStandingVerdictTTL, ResultsUntil: 1788912000, Tiers: []board.WorkStandingTier{{Name: "reward", Credits: 300}}, Successor: nextID, NextRound: nextID}
	root := board.Work{ID: standingID, Room: "bounties", Title: "Standing bug bounty, round 4", State: "open", StoredState: "open", Requester: board.AgentRef{ID: strings.Repeat("4", 64)},
		Eligibility: "open", Deadline: 1789171200, Standing: standing}
	s := &testService{execute: func(c board.Command) (board.Result, error) {
		switch c.Operation {
		case "room.get":
			return board.Result{OK: true, Room: &board.Room{Name: c.Room, Visibility: "public"}}, nil
		case "work.get":
			return board.Result{OK: true, Data: map[string]any{"work": root}}, nil
		case "works.list":
			return board.Result{OK: true, Data: map[string]any{"works": []board.Work{}}}, nil
		case "work.history":
			return board.Result{OK: true, Data: map[string]any{"work_id": c.MessageID, "transitions": []board.WorkTransition{}}}, nil
		}
		return board.Result{OK: true}, nil
	}}
	get := func() string {
		w := httptest.NewRecorder()
		Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/work/"+standingID, nil))
		if w.Code != 200 {
			t.Fatalf("/work/%s: %d", standingID, w.Code)
		}
		return w.Body.String()
	}
	if body := get(); !strings.Contains(body, `<strong>This round is full</strong> — the open round is <a href="/work/`+nextID+`">`) {
		t.Error("a full round does not link the open round")
	}
	root.State = "cancelled"
	if body := get(); !strings.Contains(body, `<strong>This round is closed</strong> — the open round is <a href="/work/`+nextID+`">`) {
		t.Error("a closed round does not link the open round")
	}
	// C967: and when that round's next seat opens, when it has none now.
	standing.NextRoundOpenAt = 1789084800
	if body := get(); !strings.Contains(body, `">`+nextID[:12]+`</a> (next seat opens by 2026-09-11 00:00 UTC).</p>`) {
		t.Error("the open round's next seat time is missing")
	}
	standing.NextRoundOpenAt = 0
	standing.NextRound = ""
	if body := get(); strings.Contains(body, "the open round is") {
		t.Error("a round with no claimable successor links one")
	}
}
