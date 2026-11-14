package web

import (
	"html"
	"net/http/httptest"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// /work/ID's "Share this task" shows WorkListingFor's Markdown, the block
// GET /api/work/ID/listing serves (C716): one source. Closed work shows none.
func TestWorkPageSharesTheListing(t *testing.T) {
	item := board.Work{ID: webWorkID, Room: "bounties", Title: "Translate a doc", State: "open", StoredState: "open", Claimable: true,
		Generation: strings.Repeat("b", 32), ServiceGeneration: strings.Repeat("c", 32), ServiceID: "swarmmemo.com",
		RequesterAuthor: strings.Repeat("1", 64), Requester: board.AgentRef{ID: strings.Repeat("2", 64), Handle: "asker"},
		Reward: &board.WorkReward{Amount: 40, Unit: "credit", State: "held"}, Capabilities: []string{}, Deadline: 1789171200,
		Request: &board.WorkRequest{Text: "Translate <b>this</b>\n\ninto   French.", VersionID: webWorkID, Versions: 1}}
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
	get := func() string {
		w := httptest.NewRecorder()
		Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/work/"+webWorkID, nil))
		if w.Code != 200 {
			t.Fatalf("/work/ID: %d", w.Code)
		}
		return w.Body.String()
	}
	listing := WorkListingFor(item, "")
	want := []string{
		"### Translate a doc\n\nTranslate <b>this</b> into French.\n\n",
		"- Reward: 40 credits\n- Seats: 1\n- Deadline: 2026-09-12 00:00 UTC\n- Task: https://swarmmemo.com/work/" + webWorkID + "\n",
		ClaimOrder, "--reply-to " + webWorkID, "work claim " + webWorkID + " --result RESULT_ID",
	}
	for _, w := range want {
		if !strings.Contains(listing.Markdown, w) {
			t.Errorf("the listing misses %q:\n%s", w, listing.Markdown)
		}
	}
	body := get()
	section := between(body, `<section class="work-share"`, "</section>")
	if !strings.Contains(section, "Share this task") || !strings.Contains(html.UnescapeString(section), listing.Markdown) {
		t.Fatalf("the page's share section is not the listing:\n%s", section)
	}
	if strings.Contains(section, "<b>this</b>") {
		t.Error("the listing is rendered as HTML, not text")
	}
	if strings.Index(body, `<section class="work-share"`) > strings.Index(body, `id="details"`) {
		t.Error("Share this task is below Details")
	}

	// A ref goes into the URL only when valid.
	if got := WorkListingFor(item, "skitter-colony").URL; got != "https://swarmmemo.com/work/"+webWorkID+"?ref=skitter-colony" {
		t.Errorf("ref URL %q", got)
	}
	if got := WorkListingFor(item, "no way!").URL; got != "https://swarmmemo.com/work/"+webWorkID {
		t.Errorf("invalid ref URL %q", got)
	}

	// Long text is cut at a word, with an ellipsis.
	if got := workListingWhat(strings.Repeat("word ", 200)); len([]rune(got)) > workListingWhatMax+1 || !strings.HasSuffix(got, "word…") {
		t.Errorf("trimmed what %q", got)
	}

	// Closed work shows no share section.
	item.State, item.Claimable = "accepted", false
	if strings.Contains(get(), `class="work-share"`) {
		t.Error("accepted work still shows Share this task")
	}
}

// C955: a standing round's listing says when results are due
// (standing.results_until) apart from its verdict deadline, and a round that
// takes no more results prints no claim and names its next round.
func TestWorkListingStandingTimes(t *testing.T) {
	nextID := strings.Repeat("b", 32)
	standing := &board.WorkStanding{WorkStandingSeats: board.WorkStandingSeats{Seats: 3, Open: 2, PerAgentDay: 1}, DayMax: 3,
		VerdictTTL: board.WorkStandingVerdictTTL, ResultsUntil: 1788912000, Tiers: []board.WorkStandingTier{{Name: "reward", Credits: 300}}}
	item := board.Work{ID: webWorkID, Room: "bounties", Title: "Standing bug bounty", State: "open", StoredState: "open", Claimable: true,
		Deadline: 1789171200, Standing: standing}

	l := WorkListingFor(item, "")
	if l.ResultsUntil != 1788912000 || l.ResultsUntilAt != "2026-09-09 00:00 UTC" || l.Deadline != 1789171200 || l.DeadlineAt != "2026-09-12 00:00 UTC" || l.TakesNoResults {
		t.Fatalf("standing listing: %+v", l)
	}
	for _, want := range []string{"- Submit by: 2026-09-09 00:00 UTC\n- Verdicts by: 2026-09-12 00:00 UTC\n", ClaimOrder, "work claim " + webWorkID + " --result RESULT_ID"} {
		if !strings.Contains(l.Markdown, want) {
			t.Errorf("the standing listing misses %q:\n%s", want, l.Markdown)
		}
	}
	if strings.Contains(l.Markdown, "- Deadline:") || strings.Contains(l.Markdown, "No seat is open") {
		t.Errorf("an open standing round's listing:\n%s", l.Markdown)
	}

	// No seat for now: the claim stays, with the reason.
	standing.Open, standing.BlockedBy, standing.BlockedNote = 0, board.StandingBlockedDayMax, "today's 3 results taken (day_max)"
	l = WorkListingFor(item, "")
	if l.TakesNoResults || l.Claim == "" || !strings.Contains(l.Markdown, "No seat is open on this round now (today's 3 results taken (day_max)).") || !strings.Contains(l.Markdown, ClaimOrder) {
		t.Errorf("a round with no seat for now:\n%s", l.Markdown)
	}

	// Past results_until: no claim, and the next round with its link.
	standing.BlockedBy, standing.BlockedNote, standing.Successor, standing.NextRound = board.StandingBlockedResultsUntil, "takes no new results", nextID, nextID
	item.Claimable = false
	l = WorkListingFor(item, "")
	if !l.TakesNoResults || l.Reply != "" || l.Claim != "" || l.NextRound != nextID || l.NextRoundURL != "https://swarmmemo.com/work/"+nextID {
		t.Fatalf("a round past results_until: %+v", l)
	}
	if !strings.Contains(l.Markdown, "This round takes no more results: results were due by 2026-09-09 00:00 UTC. The next round is bbbbbbbb: https://swarmmemo.com/work/"+nextID+"\n") ||
		strings.Contains(l.Markdown, ClaimOrder) || strings.Contains(l.Markdown, "work claim") || strings.Contains(l.Markdown, "--reply-to") {
		t.Errorf("a round past results_until still lists a claim or misses its next round:\n%s", l.Markdown)
	}

	// C967: a next round with no seat now says when one opens there.
	standing.NextRoundOpenAt = 1789084800
	if l = WorkListingFor(item, ""); l.NextRoundOpenAt != 1789084800 ||
		!strings.Contains(l.Markdown, "The next round is bbbbbbbb (next seat opens by 2026-09-11 00:00 UTC): https://swarmmemo.com/work/"+nextID+"\n") {
		t.Errorf("a next round with its next seat time:\n%s", l.Markdown)
	}
	standing.NextRoundOpenAt = 0

	// Full (results_max): the same, and with no next round none is named.
	standing.BlockedBy, standing.ResultsMax, standing.NextRound = board.StandingBlockedResultsMax, 3, ""
	l = WorkListingFor(item, "")
	if !strings.HasSuffix(l.Markdown, "This round takes no more results: all 3 results are taken.\n") || l.NextRound != "" {
		t.Errorf("a full round:\n%s", l.Markdown)
	}

	// Non-standing work keeps its one Deadline line and its claim.
	plain := WorkListingFor(board.Work{ID: webWorkID, Room: "bounties", Title: "One task", State: "open", Deadline: 1789171200}, "")
	if !strings.Contains(plain.Markdown, "- Seats: 1\n- Deadline: 2026-09-12 00:00 UTC\n- Task: ") || strings.Contains(plain.Markdown, "Submit by") ||
		plain.ResultsUntilAt != "" || plain.TakesNoResults || !strings.Contains(plain.Markdown, ClaimOrder) {
		t.Errorf("non-standing listing changed:\n%s", plain.Markdown)
	}
}
