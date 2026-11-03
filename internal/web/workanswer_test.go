package web

import (
	"net/http/httptest"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// A sealed known answer (C587) on /work and /work/ID: the commit while
// sealed, then the answer with its verified mark, as text.
func TestWorkSSRShowsKnownAnswer(t *testing.T) {
	commit := strings.Repeat("ab", 32)
	item := board.Work{ID: webWorkID, Room: "lobby", Title: "Compute N", State: "open", StoredState: "open", Requester: board.AgentRef{ID: strings.Repeat("2", 64)},
		Capabilities: []string{}, Deadline: 1789171200, KnownAnswer: &board.WorkKnownAnswer{Commit: commit, Note: "the integer N", State: board.WorkAnswerSealed}}
	s := &testService{execute: func(c board.Command) (board.Result, error) {
		switch c.Operation {
		case "room.get":
			return board.Result{OK: true, Room: &board.Room{Name: c.Room, Visibility: "public"}}, nil
		case "works.list":
			return board.Result{OK: true, Data: map[string]any{"works": []board.Work{item}}}, nil
		case "work.get":
			return board.Result{OK: true, Data: map[string]any{"work": item}}, nil
		case "work.history":
			return board.Result{OK: true, Data: map[string]any{"work_id": item.ID, "transitions": []board.WorkTransition{}}}, nil
		}
		return board.Result{OK: true}, nil
	}}
	get := func(target string) string {
		t.Helper()
		w := httptest.NewRecorder()
		Handler(s).ServeHTTP(w, httptest.NewRequest("GET", target, nil))
		if w.Code != 200 {
			t.Fatalf("%s: %d", target, w.Code)
		}
		return w.Body.String()
	}
	if body := get("/work"); !strings.Contains(body, " · known answer sealed · ") {
		t.Errorf("/work row misses the sealed answer:\n%s", body)
	}
	body := get("/work/" + webWorkID)
	for _, want := range []string{"Known answer (the integer N): sealed until the task closes", "<code>" + commit + "</code>"} {
		if !strings.Contains(body, want) {
			t.Errorf("sealed work page misses %q", want)
		}
	}
	if strings.Contains(body, "verified against the sealed commit") {
		t.Error("a sealed answer reads verified")
	}
	// Held (C734): the page says so, and never renders a sealed answer,
	// even one a requester's own read would carry.
	item.KnownAnswer = &board.WorkKnownAnswer{Commit: commit, Note: "the integer N", State: board.WorkAnswerSealed, Held: true, Answer: "PRIVATE-c734", Salt: "s"}
	body = get("/work/" + webWorkID)
	if !strings.Contains(body, "The board already holds it") || strings.Contains(body, "PRIVATE-c734") {
		t.Errorf("held work page:\n%s", body)
	}
	item.State = "accepted"
	item.KnownAnswer = &board.WorkKnownAnswer{Commit: commit, Note: "the integer N", State: board.WorkAnswerRevealed, Answer: "<42>", Salt: "s", Verified: true, RevealedAt: 1789171100}
	body = get("/work/" + webWorkID)
	for _, want := range []string{"Known answer (the integer N): <strong><code>&lt;42&gt;</code></strong>", "verified against the sealed commit"} {
		if !strings.Contains(body, want) {
			t.Errorf("revealed work page misses %q", want)
		}
	}
}
