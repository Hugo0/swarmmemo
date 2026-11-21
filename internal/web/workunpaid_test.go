package web

import (
	"net/http/httptest"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// C1180: a standing tier that pays nothing reads "accepted, unpaid" on the
// standing task's page (its tiers, its reward line and its accepted
// results) and on the result's own page, never "paid 0" or "0 credits".
func TestWorkSSRStandingUnpaidTier(t *testing.T) {
	standingID, resultID := strings.Repeat("c", 32), strings.Repeat("d", 32)
	requester := board.AgentRef{ID: strings.Repeat("2", 64)}
	worker := board.AgentRef{ID: strings.Repeat("3", 64), Handle: "finder"}
	root := board.Work{ID: standingID, Room: "bounties", Title: "Standing audit", State: "open", StoredState: "open", Requester: requester, Eligibility: "open",
		Reward: &board.WorkReward{Amount: 1000, Unit: "credit", State: "held"}, Deadline: 1789171200,
		Standing: &board.WorkStanding{WorkStandingSeats: board.WorkStandingSeats{Seats: 2, Open: 2, PerAgentDay: 1}, Results: 1, DayMax: 2, VerdictTTL: board.WorkStandingVerdictTTL, ResultsUntil: 1788912000,
			Tiers: []board.WorkStandingTier{{Name: "bug", Credits: 500}, {Name: "valid-negative"}}},
		Results: []board.WorkAcceptedResult{{ResultID: resultID, WorkID: resultID, Worker: worker, Tier: "valid-negative", AcceptedAt: 1788600000}}}
	result := board.Work{ID: resultID, Room: "bounties", Title: "Standing audit", State: "accepted", StoredState: "accepted", Requester: requester, Eligibility: "open", StandingID: standingID, Deadline: 1788566400,
		Tier: "valid-negative", Reward: &board.WorkReward{Amount: 0, Unit: "credit", State: "released", Reason: board.WorkUnpaidTierReason}}
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
	body := get("/work/" + standingID)
	for _, want := range []string{"Reward bug 500 credits, valid-negative (accepted, unpaid) per result", "<code>valid-negative</code>: accepted, unpaid</li>", "tier <code>valid-negative</code> · accepted, unpaid"} {
		if !strings.Contains(body, want) {
			t.Errorf("standing page misses %q", want)
		}
	}
	page := get("/work/" + resultID)
	for _, want := range []string{"Accepted, unpaid · accepted</span>", "accepted, unpaid, as tier <code>valid-negative</code>"} {
		if !strings.Contains(page, want) {
			t.Errorf("result page misses %q", want)
		}
	}
	for name, b := range map[string]string{"standing": body, "result": page} {
		for _, bad := range []string{"paid 0", "0 credits paid", "Reward 0 credits", "valid-negative 0 credits", "paid as tier <code>valid-negative"} {
			if strings.Contains(b, bad) {
				t.Errorf("%s page says %q", name, bad)
			}
		}
	}
}
