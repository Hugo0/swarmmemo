package web

import (
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// The unfiltered /work leads with an "Earn credits" section (a few earn
// tasks, the steps, a link to the whole list); /work?kind=earn is that list,
// titled Earn credits. Filtered pages read no earn list.
func TestWorkSSREarnCredits(t *testing.T) {
	earn := board.Work{ID: webWorkID, Room: "lobby", Title: "Witness a link", State: "open", StoredState: "open", Requester: board.AgentRef{ID: strings.Repeat("2", 64)},
		Reward: &board.WorkReward{Amount: 40, Unit: "credit", State: "held"}, Capabilities: []string{board.WorkEarnTag}, Deadline: 1789171200}
	other := earn
	other.ID, other.Title, other.Reward, other.Capabilities = strings.Repeat("b", 32), "Unpaid chat", nil, []string{}
	var kinds []string
	s := &testService{execute: func(c board.Command) (board.Result, error) {
		switch c.Operation {
		case "room.get":
			return board.Result{OK: true, Room: &board.Room{Name: c.Room, Visibility: "public"}}, nil
		case "works.list":
			kinds = append(kinds, c.Kind)
			if c.Kind == board.WorkKindEarn {
				return board.Result{OK: true, Data: map[string]any{"works": []board.Work{earn}}}, nil
			}
			return board.Result{OK: true, Data: map[string]any{"works": []board.Work{other}}}, nil
		}
		return board.Result{OK: true}, nil
	}}
	get := func(target string) (int, string) {
		w := httptest.NewRecorder()
		Handler(s).ServeHTTP(w, httptest.NewRequest("GET", target, nil))
		return w.Code, w.Body.String()
	}

	code, body := get("/work")
	if code != 200 || !slices.Equal(kinds, []string{"", board.WorkKindEarn}) {
		t.Fatalf("/work: %d, lists %q", code, kinds)
	}
	for _, want := range []string{`<h2 id="earn-heading">Earn credits</h2>`, "Out of credits? Do a small paid task", "Witness a link", "Reward 40 credits (held)", `href="/work?kind=earn"`, `href="/api/works?kind=earn"`, "Unpaid chat"} {
		if !strings.Contains(body, want) {
			t.Errorf("/work misses %q", want)
		}
	}
	if strings.Index(body, "Witness a link") > strings.Index(body, "Unpaid chat") {
		t.Error("the earn section does not lead the directory")
	}

	kinds = nil
	code, body = get("/work?kind=earn")
	if code != 200 || !slices.Equal(kinds, []string{board.WorkKindEarn}) {
		t.Fatalf("/work?kind=earn: %d, lists %q", code, kinds)
	}
	for _, want := range []string{"<title>Earn credits", "<h1>Earn credits</h1>", "smallest effort first", `<option value="earn" selected>`, "Witness a link", "kind=earn"} {
		if !strings.Contains(body, want) {
			t.Errorf("/work?kind=earn misses %q", want)
		}
	}
	if strings.Contains(body, `id="earn-heading"`) {
		t.Error("the earn list repeats the earn section")
	}

	kinds = nil
	if code, body = get("/work?kind=open"); code != 200 || !slices.Equal(kinds, []string{"open"}) || strings.Contains(body, `id="earn-heading"`) {
		t.Fatalf("a filtered page shows the earn section: %d %q", code, kinds)
	}
}

// The FAQ answers what to do when out of credits, pointing at the earn list.
func TestFAQOutOfCredits(t *testing.T) {
	w := httptest.NewRecorder()
	Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", "/faq", nil))
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "What do I do when I&#39;m out of credits?") || !strings.Contains(body, "/work?kind=earn") || !strings.Contains(body, "kind=earn&amp;limit=5") {
		t.Fatalf("/faq %d misses the out-of-credits answer", w.Code)
	}
}
