package web

import (
	"net/http/httptest"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// C434: a standing task's line says what one result earns, then its seats
// and the pool held while it is open, never the pool as the reward; a task
// with one result reads exactly as before.
func TestWorkLineStandingReward(t *testing.T) {
	yes, no := true, false
	id := webWorkID
	one := []board.WorkStandingTier{{Name: board.WorkStandingTierFirst, Credits: 150}}
	tiers := []board.WorkStandingTier{{Name: "bug", Credits: 500, USDC: "0.2"}, {Name: "bug+fix", Credits: 800, USDC: "0.4"}}
	cases := []struct {
		w           board.MessageWork
		badge, want string
	}{
		{board.MessageWork{ID: id, State: "open", Deadline: 1, Reward: &board.MessageWorkReward{Amount: 450, Unit: "credit"}, Standing: &board.MessageWorkStanding{Seats: 3, Tiers: one}, Claimable: &yes},
			"Paid task", " · 150 credits per result; 3 seats (450 held) · open · due Jan 1 · eligible: open"},
		{board.MessageWork{ID: id, State: "open", Deadline: 1, Reward: &board.MessageWorkReward{Amount: 4000, Unit: "credit"}, Standing: &board.MessageWorkStanding{Seats: 5, Tiers: tiers}, Claimable: &yes},
			"Paid task", " · bug 500 credits + 0.2 USDC, bug+fix 800 credits + 0.4 USDC per result; 5 seats (4,000 held) · open · due Jan 1 · eligible: open"},
		// Closed: the pool is no longer held, so it is left out.
		{board.MessageWork{ID: id, State: "cancelled", Reward: &board.MessageWorkReward{Amount: 1, Unit: "credit"}, Standing: &board.MessageWorkStanding{Seats: 1, Tiers: []board.WorkStandingTier{{Name: "reward", Credits: 1}}}, Claimable: &no},
			"Paid task", " · 1 credit per result; 1 seat · cancelled · eligible: open"},
		// A simulation shows no reward, standing or not.
		{board.MessageWork{ID: id, State: "open", Deadline: 1, Simulated: true, Reward: &board.MessageWorkReward{Amount: 450, Unit: "credit"}, Standing: &board.MessageWorkStanding{Seats: 3, Tiers: one}, Claimable: &yes},
			"Simulated task", " · open · due Jan 1 · eligible: open"},
		// Not standing: unchanged.
		{board.MessageWork{ID: id, State: "open", Deadline: 1, Reward: &board.MessageWorkReward{Amount: 450, Unit: "credit"}, Claimable: &yes},
			"Paid task", " · 450 credits · open · due Jan 1 · eligible: open"},
	}
	for i, c := range cases {
		w := c.w
		got := workLine(board.Message{Work: &w})
		if got == nil || got.Badge != c.badge || got.Detail != c.want {
			t.Errorf("case %d: %+v, want %q %q", i, got, c.badge, c.want)
		}
	}
	held := &board.WorkReward{Amount: 4000, State: "held"}
	st := &board.WorkStanding{WorkStandingSeats: board.WorkStandingSeats{Seats: 5}, Tiers: tiers}
	if got, want := standingReward(st, held), "bug 500 credits + 0.2 USDC, bug+fix 800 credits + 0.4 USDC per result; 5 seats (4,000 held)"; got != want {
		t.Errorf("standingReward = %q, want %q", got, want)
	}
	if got := standingReward(nil, held); got != "" {
		t.Errorf("standingReward without standing = %q", got)
	}
}

// C938: a full standing round's line on the post page and the feed says
// it is full and links its next round; one with neither says nothing.
func TestWorkLineStandingNextRound(t *testing.T) {
	no := false
	id, next := webWorkID, strings.Repeat("b", 32)
	note := "Full: all 10 results taken. The next round is bbbbbbbb."
	mark := board.MessageWork{ID: id, Title: "API QA round 3", State: "open", Deadline: 1, Eligibility: "open", Claimable: &no, URL: "/work/" + id,
		Standing: &board.MessageWorkStanding{Seats: 3, Tiers: []board.WorkStandingTier{{Name: board.WorkStandingTierFirst, Credits: 150}}}}
	plain := mark
	full := mark
	full.NextRound, full.NextNote = next, note
	if got := workLine(board.Message{Work: &full}); got == nil || got.Next != note || got.NextHref != "/e/bbbbbbbb" {
		t.Fatalf("full round: %+v", got)
	}
	if got := workLine(board.Message{Work: &plain}); got == nil || got.Next != "" || got.NextHref != "" {
		t.Fatalf("a round with neither: %+v", got)
	}
	serve := func(w board.MessageWork, path string) string {
		request := board.Message{ID: id, Sequence: 1, Room: "bounties", Page: "main", Kind: "request", Text: "Results are taken until 2026-11-18.", Visibility: "public", Type: "message", Work: &w}
		feed := func(c board.Command) (board.Result, error) {
			switch c.Operation {
			case "messages.list", "thread.get":
				return board.Result{OK: true, Messages: []board.Message{request}, Data: map[string]any{"root_id": id}}, nil
			case "room.get":
				return board.Result{OK: true, Room: &board.Room{Name: c.Room, Visibility: "public"}}, nil
			}
			return board.Result{OK: true}, nil
		}
		rec := httptest.NewRecorder()
		Handler(&testService{execute: feed}).ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 {
			t.Fatalf("%s: %d", path, rec.Code)
		}
		return rec.Body.String()
	}
	want := `<a class="work-next" href="/e/bbbbbbbb">` + note + `</a></p>`
	for _, path := range []string{"/e/" + id, "/r/bounties?sort=new"} {
		if body := serve(full, path); !strings.Contains(body, want) {
			t.Errorf("%s: no forward line %q", path, want)
		}
		if body := serve(plain, path); strings.Contains(body, "work-next") {
			t.Errorf("%s: a round with neither shows a forward line", path)
		}
	}
	alone := mark
	alone.NextNote = "Full: all 10 results taken."
	if body := serve(alone, "/e/"+id); !strings.Contains(body, `<span class="work-next">Full: all 10 results taken.</span></p>`) {
		t.Error("a full round without a next round does not say it is full")
	}
}
