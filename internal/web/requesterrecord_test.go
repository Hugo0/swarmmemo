package web

import (
	"net/http/httptest"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// The requester record (RFC C89) on the web: a "Pays" line on rewarded work
// in /work and on the work page, with unpaid counts in the alert colour, a
// plain line for a new requester, the lapse notice on the work page, and the
// "As a requester" panel on the agent page.

func TestPayLineWords(t *testing.T) {
	median := 9.5
	for _, c := range []struct {
		r            *board.RequesterRecord
		text, unpaid string
	}{
		{&board.RequesterRecord{}, "New requester: no paid results yet", ""},
		{&board.RequesterRecord{Results: 1, Paid: 1}, "Pays: 1 of 1 result", ""},
		{&board.RequesterRecord{Results: 8, Paid: 7, UnpaidLapsed: 1, MedianHoursToVerdict: &median}, "Pays: 7 of 8 results, median 9.5 h", "1 left unpaid at the deadline"},
		{&board.RequesterRecord{Results: 4, Paid: 1, Rejected: 1, UnpaidLapsed: 1, CancelledAfterSubmit: 1}, "Pays: 1 of 4 results", "1 left unpaid at the deadline · 1 cancelled after a submit"},
	} {
		v := payLine(c.r)
		if v == nil || v.Text != c.text || v.Unpaid != c.unpaid {
			t.Errorf("%+v: %+v", c.r, v)
		}
	}
	if payLine(nil) != nil {
		t.Fatal("no record still renders a line")
	}
}

func TestWorkSSRShowsTheRequesterRecord(t *testing.T) {
	median := 3.0
	requester := strings.Repeat("2", 64)
	record := &board.RequesterRecord{Results: 6, Paid: 1, Rejected: 1, UnpaidLapsed: 3, CancelledAfterSubmit: 1, MedianHoursToVerdict: &median, DistinctWorkers: 4}
	reward := &board.WorkReward{Amount: 10, Unit: "credit", State: "released", Reason: "requester_lapsed"}
	lapsed := board.Work{ID: webWorkID, Room: "lobby", Title: "Lapsed work", State: "expired", StoredState: "submitted", Requester: board.AgentRef{ID: requester, Handle: "stiffer"}, Reward: reward, RequesterRecord: record, Capabilities: []string{}, Deadline: 1789171200}
	fresh := board.Work{ID: strings.Repeat("b", 32), Room: "lobby", Title: "Fresh work", State: "open", StoredState: "open", Requester: board.AgentRef{ID: strings.Repeat("5", 64)}, Reward: &board.WorkReward{Amount: 5, State: "held"}, RequesterRecord: &board.RequesterRecord{}, Capabilities: []string{}, Deadline: 1789171200}
	unpaid := board.Work{ID: strings.Repeat("c", 32), Room: "lobby", Title: "Unpaid coordination", State: "open", StoredState: "open", Requester: board.AgentRef{ID: strings.Repeat("6", 64)}, RequesterRecord: record, Capabilities: []string{}, Deadline: 1789171200}
	s := &testService{execute: func(c board.Command) (board.Result, error) {
		switch c.Operation {
		case "room.get":
			return board.Result{OK: true, Room: &board.Room{Name: c.Room, Visibility: "public"}}, nil
		case "works.list":
			return board.Result{OK: true, Data: map[string]any{"works": []board.Work{lapsed, fresh, unpaid}}}, nil
		case "work.get":
			return board.Result{OK: true, Data: map[string]any{"work": lapsed}}, nil
		case "work.history":
			return board.Result{OK: true, Data: map[string]any{"work_id": webWorkID, "transitions": []board.WorkTransition{}}}, nil
		case "agent.get":
			return board.Result{OK: true, Agent: &board.Agent{ID: requester, Handle: "stiffer", RequesterRecord: &board.RequesterRecord{Results: 6, Paid: 1, Rejected: 1, UnpaidLapsed: 3, CancelledAfterSubmit: 1, MedianHoursToVerdict: &median, DistinctWorkers: 4,
				LastDays: &board.RequesterWindow{Results: 2, Paid: 1, UnpaidLapsed: 1}, UnpaidWork: []string{webWorkID}}}}, nil
		}
		return board.Result{OK: true}, nil
	}}
	line := `Pays: 1 of 6 results, median 3 h · <span class="pay-unpaid">3 left unpaid at the deadline · 1 cancelled after a submit</span>`
	w := httptest.NewRecorder()
	Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/work?kind=rewarded", nil))
	body := w.Body.String()
	if w.Code != 200 || strings.Count(body, line) != 1 || !strings.Contains(body, "New requester: no paid results yet") || !strings.Contains(body, `href="/agent/`+requester+`#requester"`) {
		t.Fatalf("directory %d %s", w.Code, body)
	}
	// Unrewarded work carries no pay line; the copy tells a worker to check.
	if strings.Count(body, `class="small pay-line"`) != 2 || !strings.Contains(body, "check it before you claim") || !strings.Contains(body, "requester_lapsed") {
		t.Fatalf("pay lines or worker copy: %s", body)
	}

	w = httptest.NewRecorder()
	Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/work/"+webWorkID, nil))
	body = w.Body.String()
	for _, want := range []string{line, "Requester lapsed: the deadline passed with the submitted result undecided", "<code>requester_lapsed</code>", "counts on its public record"} {
		if !strings.Contains(body, want) {
			t.Errorf("work page lacks %q", want)
		}
	}

	w = httptest.NewRecorder()
	Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/agent/"+requester, nil))
	body = w.Body.String()
	if w.Code != 200 || !strings.Contains(body, `id="requester"`) {
		t.Fatalf("agent page %d has no requester panel", w.Code)
	}
	panel := body[strings.Index(body, `id="requester"`):]
	panel = panel[:strings.Index(panel, "</section>")]
	for _, want := range []string{"As a requester", `<dt>Paid</dt><dd class="stat-value">1</dd>`, `<dd class="stat-value pay-unpaid">3</dd>`, `<dd class="stat-value pay-unpaid">1</dd>`, "3 h", `<dt>Distinct workers</dt><dd class="stat-value">4</dd>`,
		"Last 90 days: paid 1 of 2 results, 1 unpaid at the deadline.", `href="/work/` + webWorkID + `"`, "linked to it are left out"} {
		if !strings.Contains(panel, want) {
			t.Errorf("requester panel lacks %q:\n%s", want, panel)
		}
	}

	// An agent with no results has no panel.
	s.execute = func(c board.Command) (board.Result, error) {
		if c.Operation == "agent.get" {
			return board.Result{OK: true, Agent: &board.Agent{ID: requester, RequesterRecord: &board.RequesterRecord{LastDays: &board.RequesterWindow{}}}}, nil
		}
		return board.Result{OK: true}, nil
	}
	w = httptest.NewRecorder()
	Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/agent/"+requester, nil))
	if strings.Contains(w.Body.String(), `id="requester"`) {
		t.Fatal("an agent with no results shows a requester panel")
	}
}
