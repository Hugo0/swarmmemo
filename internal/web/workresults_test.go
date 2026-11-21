package web

import (
	"net/http/httptest"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// C360: the work page lists the accepted results work.get gives, one compact
// line each (result, worker, tier, rating, time), at parity with the API.
func TestWorkPageShowsAcceptedResults(t *testing.T) {
	rating := 77
	r1, r2 := strings.Repeat("e", 32), strings.Repeat("f", 32)
	item := board.Work{ID: webWorkID, Room: "lobby", Title: "Standing work", State: "open", StoredState: "open", Requester: board.AgentRef{ID: strings.Repeat("2", 64)}, Capabilities: []string{}, Deadline: 1789171200,
		Results: []board.WorkAcceptedResult{
			{ResultID: r2, WorkID: r2, Worker: board.AgentRef{ID: strings.Repeat("3", 64), Handle: "<i>alice</i>"}, Tier: "bug+fix", Rating: &rating, AcceptedAt: 1789000000},
			{ResultID: r1, WorkID: r1, Worker: board.AgentRef{ID: strings.Repeat("4", 64)}, AcceptedAt: 1788000000},
		}}
	s := &testService{execute: func(c board.Command) (board.Result, error) {
		switch c.Operation {
		case "room.get":
			return board.Result{OK: true, Room: &board.Room{Name: c.Room, Visibility: "public"}}, nil
		case "work.get":
			return board.Result{OK: true, Data: map[string]any{"work": item}}, nil
		case "work.history":
			return board.Result{OK: true, Data: map[string]any{"work_id": webWorkID, "transitions": []board.WorkTransition{}}}, nil
		}
		return board.Result{OK: true}, nil
	}}
	w := httptest.NewRecorder()
	Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/work/"+webWorkID, nil))
	body := w.Body.String()
	if w.Code != 200 || strings.Count(body, `class="small accepted-result"`) != 2 || !strings.Contains(body, "<strong>Accepted results</strong>") {
		t.Fatalf("work page %d: %s", w.Code, body)
	}
	for _, want := range []string{
		`<a href="/e/` + r2 + `">`, `by <a href="/agent/` + strings.Repeat("3", 64) + `">&lt;i&gt;alice&lt;/i&gt;</a> · tier <code>bug&#43;fix</code> · rated 77/100 · accepted 2026-09-10 00:26 UTC`,
		`<a href="/e/` + r1 + `">`, `by <a href="/agent/` + strings.Repeat("4", 64) + `">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("work page lacks %q", want)
		}
	}
	if strings.Contains(body, "<i>alice") {
		t.Fatal("a handle reached the page unescaped")
	}
	// No accepted result, no section.
	item.Results = nil
	w = httptest.NewRecorder()
	Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/work/"+webWorkID, nil))
	if strings.Contains(w.Body.String(), "accepted-result") {
		t.Fatal("an empty list rendered")
	}
}

// C600: a standing task's page lists the results waiting for a verdict, each
// linked to its own /work/ID, at parity with work.get waiting_results.
func TestWorkPageShowsWaitingResults(t *testing.T) {
	r1, r2 := strings.Repeat("e", 32), strings.Repeat("f", 32)
	item := board.Work{ID: webWorkID, Room: "lobby", Title: "Standing work", State: "open", StoredState: "open", Requester: board.AgentRef{ID: strings.Repeat("2", 64)}, Capabilities: []string{}, Deadline: 1789171200,
		Standing: &board.WorkStanding{WorkStandingSeats: board.WorkStandingSeats{Seats: 3, Submitted: 3}, Tiers: []board.WorkStandingTier{{Name: "default", Credits: 10}}},
		WaitingResults: []board.WorkWaitingResult{
			{WorkID: r1, ResultID: r1, Worker: board.AgentRef{ID: strings.Repeat("3", 64), Handle: "<i>alice</i>"}, SubmittedAt: 1789000000, Deadline: 1789100000},
			{WorkID: r2, ResultID: r2, Worker: board.AgentRef{ID: strings.Repeat("4", 64)}, SubmittedAt: 1789000100, Deadline: 1789100100},
		}, WaitingResultsTruncated: true}
	s := &testService{execute: func(c board.Command) (board.Result, error) {
		switch c.Operation {
		case "room.get":
			return board.Result{OK: true, Room: &board.Room{Name: c.Room, Visibility: "public"}}, nil
		case "work.get":
			return board.Result{OK: true, Data: map[string]any{"work": item}}, nil
		case "work.history":
			return board.Result{OK: true, Data: map[string]any{"work_id": webWorkID, "transitions": []board.WorkTransition{}}}, nil
		}
		return board.Result{OK: true}, nil
	}}
	w := httptest.NewRecorder()
	Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/work/"+webWorkID, nil))
	body := w.Body.String()
	if w.Code != 200 || strings.Count(body, `class="small waiting-result"`) != 2 {
		t.Fatalf("work page %d: %s", w.Code, body)
	}
	for _, want := range []string{
		"<strong>Waiting for a verdict</strong>", "accept or reject it by its own id",
		`<a href="/work/` + r1 + `">`, `by <a href="/agent/` + strings.Repeat("3", 64) + `">&lt;i&gt;alice&lt;/i&gt;</a> · waiting for a verdict since 2026-09-10 00:26 UTC`,
		`<a href="/work/` + r2 + `">`, "The oldest 2 of 3 are listed.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("work page lacks %q", want)
		}
	}
	item.WaitingResults, item.WaitingResultsTruncated = nil, false
	w = httptest.NewRecorder()
	Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/work/"+webWorkID, nil))
	if strings.Contains(w.Body.String(), "waiting-result") {
		t.Fatal("an empty list rendered")
	}
}

// C1156: a standing task with one result waiting for a verdict and five
// queued for a seat says so in its summary, as work.get does (submitted 6,
// queued 5, one in waiting_results), never "6 waiting for a verdict".
func TestWorkPageSeparatesWaitingFromQueued(t *testing.T) {
	r1 := strings.Repeat("e", 32)
	item := board.Work{ID: webWorkID, Room: "lobby", Title: "Standing work", State: "open", StoredState: "open", Requester: board.AgentRef{ID: strings.Repeat("2", 64)}, Capabilities: []string{}, Deadline: 1789171200,
		Standing:       &board.WorkStanding{WorkStandingSeats: board.WorkStandingSeats{Seats: 1, Submitted: 6, Queued: 5, PerAgentDay: 1}, DayMax: 10, Tiers: []board.WorkStandingTier{{Name: "default", Credits: 10}}},
		WaitingResults: []board.WorkWaitingResult{{WorkID: r1, ResultID: r1, Worker: board.AgentRef{ID: strings.Repeat("3", 64)}, SubmittedAt: 1789000000, Deadline: 1789100000}}}
	s := &testService{execute: func(c board.Command) (board.Result, error) {
		switch c.Operation {
		case "room.get":
			return board.Result{OK: true, Room: &board.Room{Name: c.Room, Visibility: "public"}}, nil
		case "work.get":
			return board.Result{OK: true, Data: map[string]any{"work": item}}, nil
		case "work.history":
			return board.Result{OK: true, Data: map[string]any{"work_id": webWorkID, "transitions": []board.WorkTransition{}}}, nil
		}
		return board.Result{OK: true}, nil
	}}
	page := func() string {
		w := httptest.NewRecorder()
		Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/work/"+webWorkID, nil))
		if w.Code != 200 {
			t.Fatalf("work page %d", w.Code)
		}
		return w.Body.String()
	}
	body := page()
	if n := item.Standing.AwaitingVerdict(); n != int64(len(item.WaitingResults)) || !strings.Contains(body, "1 waiting for a verdict, 5 queued for a seat.") ||
		strings.Contains(body, "6 waiting for a verdict") || strings.Count(body, `class="small waiting-result"`) != 1 {
		t.Fatalf("the summary does not match work.get (%d awaiting): %s", n, body)
	}
	// Truncated, the list counts against the results awaiting a verdict.
	item.WaitingResultsTruncated = true
	if body = page(); !strings.Contains(body, "The oldest 1 of 1 are listed.") {
		t.Error("the truncated list counts queued results")
	}
	// No queued result, no queued clause.
	item.Standing.Submitted, item.Standing.Queued, item.WaitingResultsTruncated = 1, 0, false
	if body = page(); !strings.Contains(body, "1 waiting for a verdict.") || strings.Contains(body, "queued for a seat") {
		t.Error("a task with nothing queued names a queue")
	}
}
