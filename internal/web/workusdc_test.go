package web

import (
	"net/http/httptest"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// The work line of a reward with a USDC asset (RFC 0016) gives each asset
// its state, and says the whole reward is paid only when it is.
func TestWorkLineUSDCReward(t *testing.T) {
	yes, no := true, false
	id := webWorkID
	cases := []struct {
		w    board.MessageWork
		want string
	}{
		{board.MessageWork{ID: id, State: "open", Deadline: 1, Reward: &board.MessageWorkReward{Amount: 600, State: "held"}, RewardUSDC: &board.MessageWorkUSDC{Amount: "0.1", State: "promised"}, RewardState: "held", Claimable: &yes},
			" · 600 credits (held) + 0.1 USDC (promised) · open · due Jan 1 · eligible: open"},
		{board.MessageWork{ID: id, State: "accepted", Reward: &board.MessageWorkReward{Amount: 600, State: "paid"}, RewardUSDC: &board.MessageWorkUSDC{Amount: "0.1", State: "payable"}, RewardState: "payable", Claimable: &no},
			" · 600 credits (paid) + 0.1 USDC (payable) · accepted · reward payable · eligible: open"},
		{board.MessageWork{ID: id, State: "accepted", RewardUSDC: &board.MessageWorkUSDC{Amount: "0.25", State: "paid"}, RewardState: "paid", Claimable: &no},
			" · 0.25 USDC (paid) · accepted · reward paid · eligible: open"},
		{board.MessageWork{ID: id, State: "expired", RewardUSDC: &board.MessageWorkUSDC{Amount: "0.25", State: "void"}, RewardState: "released", Claimable: &no},
			" · 0.25 USDC (void) · expired · eligible: open"},
	}
	for i, c := range cases {
		w := c.w
		got := workLine(board.Message{Work: &w})
		if got == nil || got.Badge != "Paid task" || got.Detail != c.want {
			t.Errorf("case %d: %+v, want detail %q", i, got, c.want)
		}
	}
}

// The directory card and the work page show each asset and the whole
// reward's state; payable names the address owed, paid the transaction.
func TestWorkSSRUSDCReward(t *testing.T) {
	item := board.Work{ID: webWorkID, Room: "lobby", Title: "USDC work", State: "accepted", StoredState: "accepted", Requester: board.AgentRef{ID: strings.Repeat("2", 64)},
		Reward: &board.WorkReward{Amount: 500, Unit: "credit", State: "paid"}, Capabilities: []string{}, Deadline: 1789171200, RewardState: "payable",
		RewardUSDC: &board.WorkRewardUSDC{Amount: "0.1", Unit: "usdc", Network: "eip155:8453", State: "payable", PayTo: "0x00000000000000000000000000000000000000Aa"}}
	s := &testService{execute: func(c board.Command) (board.Result, error) {
		switch c.Operation {
		case "room.get":
			return board.Result{OK: true, Room: &board.Room{Name: c.Room, Visibility: "public"}}, nil
		case "works.list":
			return board.Result{OK: true, Data: map[string]any{"works": []board.Work{item}}}, nil
		case "work.get":
			return board.Result{OK: true, Data: map[string]any{"work": item}}, nil
		case "work.history":
			return board.Result{OK: true, Data: map[string]any{"work_id": webWorkID, "transitions": []board.WorkTransition{}}}, nil
		}
		return board.Result{OK: true}, nil
	}}
	w := httptest.NewRecorder()
	Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/work", nil))
	if body := w.Body.String(); w.Code != 200 || !strings.Contains(body, "Reward 500 credits (paid) + 0.1 USDC (payable) · reward payable") {
		t.Fatalf("directory %d %s", w.Code, body)
	}
	w = httptest.NewRecorder()
	Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/work/"+webWorkID, nil))
	body := w.Body.String()
	for _, want := range []string{"Reward 500 credits (paid) + 0.1 USDC (payable) · accepted", "Reward payable. 0.1 USDC on eip155:8453: owed to <code>0x00000000000000000000000000000000000000Aa</code>", "verified on chain"} {
		if w.Code != 200 || !strings.Contains(body, want) {
			t.Errorf("detail %d misses %q", w.Code, want)
		}
	}
}
