package web

import (
	"net/http/httptest"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// C476: the work page carries the Claim panel the signed-in browser key
// fills (app.js drawWorkAct, from a signed work.get). It starts hidden, names
// the work, and says whether the server read it as claimable, so a keyless
// reader is pointed to Me only while a claim is possible.
func TestWorkSSRClaimPanel(t *testing.T) {
	id := strings.Repeat("f", 32)
	item := board.Work{ID: id, Room: "bounties", Title: "Claimable fixture", State: "open", StoredState: "open", Requester: board.AgentRef{ID: strings.Repeat("5", 64)},
		Eligibility: "open", Deadline: 1789171200, Claimable: true}
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
	get := func(target string) string {
		w := httptest.NewRecorder()
		Handler(s).ServeHTTP(w, httptest.NewRequest("GET", target, nil))
		if w.Code != 200 {
			t.Fatalf("%s: %d", target, w.Code)
		}
		return w.Body.String()
	}
	panel := `<section class="work-act" id="claim" aria-labelledby="claim-heading" data-work-id="` + id + `" data-claimable="%s" hidden><h2 id="claim-heading">Claim</h2>`
	if body := get("/work/" + id); !strings.Contains(body, strings.Replace(panel, "%s", "true", 1)) || !strings.Contains(body, `id="work-act-status" class="form-status" role="status"`) {
		t.Error("the work page misses its claimable Claim panel")
	}
	browser := `<a href="#claim">Claim with that reply below</a>`
	if body := get("/work/" + id); !strings.Contains(body, browser) {
		t.Error("open work does not point a browser poster at its Claim panel (C598)")
	}
	// A full standing task is not claimable: the panel says so for app.js.
	item.Claimable = false
	item.Standing = &board.WorkStanding{WorkStandingSeats: board.WorkStandingSeats{Seats: 2, Submitted: 2, PerAgentDay: 1, NextOpenAt: 1789084800}, DayMax: 2,
		VerdictTTL: board.WorkStandingVerdictTTL, ResultsUntil: 1788912000, Tiers: []board.WorkStandingTier{{Name: "bug", Credits: 100, USDC: "0.20"}}}
	body := get("/work/" + id)
	if strings.Contains(body, browser) {
		t.Error("unclaimable work points at its Claim panel")
	}
	if !strings.Contains(body, strings.Replace(panel, "%s", "false", 1)) || !strings.Contains(body, "Seats open now: 0 of 2 · Next seats open 2026-09-11 00:00 UTC") {
		t.Error("a full standing task's page misses its unclaimable panel or the seats open now and when the next open (C585)")
	}
	// The directory has no panel: claims are made on the work's own page.
	if body := get("/work"); strings.Contains(body, `class="work-act"`) {
		t.Error("/work renders a Claim panel")
	}
}

// C598: on a conversation page, a signed reply to open, claimable work
// carries "Claim this as your result" for its author's key (app.js
// claimWithReply signs work.claim with target=REPLY_ID). Closed work, an
// anonymous reply, a result already submitted and a reply to a plain post
// carry none; the work page points a browser poster at its Claim panel.
func TestThreadClaimThisAsYourResult(t *testing.T) {
	id, reply, author := strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("c", 64)
	yes, no := true, false
	thread := func(work *board.MessageWork, replies ...board.Message) func(board.Command) (board.Result, error) {
		root := board.Message{ID: id, Sequence: 1, Room: "bounties", Page: "main", Kind: "request", Text: "Check one number", Visibility: "public", Type: "message", Work: work}
		return func(c board.Command) (board.Result, error) {
			switch c.Operation {
			case "thread.get":
				return board.Result{OK: true, Messages: append([]board.Message{root}, replies...), Data: map[string]any{"root_id": id}}, nil
			case "room.get":
				return board.Result{OK: true, Room: &board.Room{Name: c.Room, Visibility: "public"}}, nil
			}
			return board.Result{OK: true}, nil
		}
	}
	signed := board.Message{ID: reply, Sequence: 2, Room: "bounties", Page: "main", ReplyTo: id, Text: "My result", Visibility: "public", Type: "message", PublicKey: "pk", Author: author}
	open := &board.MessageWork{ID: id, Title: "Check one number", State: "open", Deadline: 1789171200, Eligibility: "open", Claimable: &yes, URL: "/work/" + id}
	button := `<button class="quiet-button claim-result-button" type="button" data-claim-work="` + id + `" data-claim-author="` + author + `" hidden>Claim this as your result</button>`
	get := func(feed func(board.Command) (board.Result, error)) string {
		w := httptest.NewRecorder()
		Handler(&testService{execute: feed}).ServeHTTP(w, httptest.NewRequest("GET", "/e/"+id, nil))
		if w.Code != 200 {
			t.Fatalf("/e/%s: %d", id, w.Code)
		}
		return w.Body.String()
	}
	if body := get(thread(open, signed)); !strings.Contains(body, button) || strings.Count(body, "claim-result-button") != 1 {
		t.Errorf("a signed reply to open work misses its one Claim this as your result button")
	}
	closed := *open
	closed.State, closed.Claimable = "submitted", &no
	anonymous := signed
	anonymous.PublicKey, anonymous.Author = "", ""
	result := signed
	result.Work = &board.MessageWork{ResultOf: id, Title: "Check one number", State: "submitted", URL: "/work/" + id}
	for name, feed := range map[string]func(board.Command) (board.Result, error){
		"closed work":     thread(&closed, signed),
		"anonymous reply": thread(open, anonymous),
		"a result":        thread(open, result),
		"a plain post":    thread(nil, signed),
	} {
		if strings.Contains(get(feed), "claim-result-button") {
			t.Errorf("%s offers Claim this as your result", name)
		}
	}
}
