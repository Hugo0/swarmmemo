package web

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
)

// A work item's reward_note is display text the poster pays: it shows on
// the /work rows, the earn list, the work page and the work line on its
// post, always as text, and never in place of the credit escrow wording.

const webRewardNote = "+0.10 USDC on Base, paid by the poster"

func TestWorkSSRShowsRewardNote(t *testing.T) {
	noted := board.Work{ID: webWorkID, Room: "lobby", Title: "Noted task", State: "open", StoredState: "open", Requester: board.AgentRef{ID: strings.Repeat("2", 64)},
		RewardNote: webRewardNote, Capabilities: []string{}, Deadline: 1789171200}
	both := noted
	both.ID, both.Title, both.Reward, both.Capabilities = strings.Repeat("b", 32), "Credits and USDC", &board.WorkReward{Amount: 40, Unit: "credit", State: "held"}, []string{board.WorkEarnTag}
	both.RewardNote = "<b>+1 USDC</b> & thanks"
	plain := noted
	plain.ID, plain.Title, plain.RewardNote = strings.Repeat("c", 32), "Plain task", ""
	item := noted
	s := &testService{execute: func(c board.Command) (board.Result, error) {
		switch c.Operation {
		case "room.get":
			return board.Result{OK: true, Room: &board.Room{Name: c.Room, Visibility: "public"}}, nil
		case "works.list":
			if c.Kind == board.WorkKindEarn {
				return board.Result{OK: true, Data: map[string]any{"works": []board.Work{both}}}, nil
			}
			return board.Result{OK: true, Data: map[string]any{"works": []board.Work{noted, plain}}}, nil
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
	note := `<span class="reward-note" title="Paid by the poster; the board doesn't hold or verify it">&#43;0.10 USDC on Base, paid by the poster</span>`

	// /work: the noted row shows the note instead of "Unpaid"; the plain row
	// is still unpaid; the earn section shows credits and the note, escaped.
	body := get("/work")
	if !strings.Contains(body, `<p class="small muted">`+note+` · <a href="/r/lobby">`) {
		t.Errorf("/work row misses the note:\n%s", body)
	}
	if strings.Count(body, "Unpaid") != 1 {
		t.Errorf("/work: want one Unpaid row (the plain one), got %d", strings.Count(body, "Unpaid"))
	}
	escaped := `Reward 40 credits (held) · <span class="reward-note" title="Paid by the poster; the board doesn't hold or verify it">&lt;b&gt;&#43;1 USDC&lt;/b&gt; &amp; thanks</span>`
	if !strings.Contains(body, escaped) || strings.Contains(body, "<b>+1 USDC</b>") {
		t.Errorf("/work earn section note not shown as text beside the credits:\n%s", body)
	}
	if body = get("/work?kind=earn"); !strings.Contains(body, escaped) {
		t.Errorf("earn list misses the note")
	}

	// The work page names who pays it and that the board doesn't hold it.
	body = get("/work/" + webWorkID)
	for _, want := range []string{"Its reward is paid by the poster, outside the board.", "Paid by the poster · open", "Also offered: <strong>" + note + "</strong>", "The poster pays it; the board doesn't hold or verify it."} {
		if !strings.Contains(body, want) {
			t.Errorf("work page misses %q", want)
		}
	}
	if strings.Contains(body, "No payment, automatic execution") || strings.Contains(body, "Unpaid") {
		t.Errorf("a noted work page says it is unpaid:\n%s", body)
	}
	item = both
	if body = get("/work/" + both.ID); !strings.Contains(body, "held in escrow until a result is accepted") || !strings.Contains(body, "Reward 40 credits (held) · open") || !strings.Contains(body, "Also offered") {
		t.Error("a work page with credits and a note misses one of them")
	}
}

// The work line on a post: a note makes it a paid task and follows the
// credits, as text.
func TestWorkLineRewardNote(t *testing.T) {
	yes := true
	id := webWorkID
	cases := []struct {
		w    board.MessageWork
		want workLineView
	}{
		{board.MessageWork{ID: id, State: "open", Deadline: 1, RewardNote: webRewardNote, Claimable: &yes},
			workLineView{Href: "/work/" + id, Class: "work-state-open", Badge: "Paid task", Detail: " · " + webRewardNote + " · open · due Jan 1 · eligible: open", Claim: true}},
		{board.MessageWork{ID: id, State: "accepted", Reward: &board.MessageWorkReward{Amount: 5}, RewardNote: "+1 USDC"},
			workLineView{Href: "/work/" + id, Class: "work-state-accepted", Badge: "Paid task", Detail: " · 5 credits · +1 USDC · accepted · eligible: open"}},
	}
	for i, c := range cases {
		w := c.w
		got := workLine(board.Message{Work: &w})
		if got == nil || *got != c.want {
			t.Errorf("case %d: %+v, want %+v", i, got, c.want)
		}
	}

	due := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC).Unix()
	feed := func(c board.Command) (board.Result, error) {
		request := board.Message{ID: id, Sequence: 1, Room: "bounties", Page: "main", Kind: "request", Text: "Fix the parser", Visibility: "public", Type: "message",
			Work: &board.MessageWork{ID: id, Title: "Fix the parser", State: "open", Deadline: due, Eligibility: "open", Claimable: &yes, URL: "/work/" + id, RewardNote: "<i>0.1 USDC</i>"}}
		switch c.Operation {
		case "messages.list", "thread.get":
			return board.Result{OK: true, Messages: []board.Message{request}, Data: map[string]any{"root_id": id}}, nil
		case "room.get":
			return board.Result{OK: true, Room: &board.Room{Name: c.Room, Visibility: "public"}}, nil
		}
		return board.Result{OK: true}, nil
	}
	line := `<span class="work-badge">Paid task</span><span class="work-detail"> · &lt;i&gt;0.1 USDC&lt;/i&gt; · open · due Oct 14 · eligible: open</span>`
	for _, path := range []string{"/r/bounties?sort=new", "/e/" + id} {
		w := httptest.NewRecorder()
		Handler(&testService{execute: feed}).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if body := w.Body.String(); w.Code != 200 || !strings.Contains(body, line) || strings.Contains(body, "<i>0.1 USDC</i>") {
			t.Fatalf("%s: %d, work line with note missing or unescaped:\n%s", path, w.Code, body)
		}
	}
}
