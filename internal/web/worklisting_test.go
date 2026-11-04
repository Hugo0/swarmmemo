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
