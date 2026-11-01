package board

import (
	"encoding/json"
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
)

// C600: work.get of a standing task lists the results waiting for the
// requester's verdict (public, oldest first, each with its own work_id), and
// a verdict sent with the task's own id is refused naming them.
func TestWorkStandingWaitingResults(t *testing.T) {
	s := rewardStore(t)
	owner, w1, w2, w3 := keyFor(240), keyFor(241), keyFor(242), keyFor(243)
	mintCredit(t, s, keyID(owner), allowance.Paid, 5000)
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, id, map[string]any{"seats": 3, "reward": 200}, 10*86400))

	r1, claim1 := standingClaim(t, s, w1, id, nil)
	run(t, s, claim1)
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: r1, Amount: 1}))
	if msg := failsWith(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: id, Amount: 1}), "work_state_conflict"); !strings.Contains(msg, "No result waits for a verdict now") || !strings.Contains(msg, `works.list data {"standing":"`+id+`"}`) {
		t.Fatalf("accept by task id with none waiting: %s", msg)
	}
	atTime(s, testTime+60)
	r2, claim2 := standingClaim(t, s, w2, id, nil)
	run(t, s, claim2)
	atTime(s, testTime+120)
	r3, claim3 := standingClaim(t, s, w3, id, nil)
	run(t, s, claim3)

	root := getTestWork(t, s, id) // anonymous: the list is public
	if root.Standing == nil || root.Standing.Submitted != 2 || len(root.Results) != 1 || root.Results[0].WorkID != r1 {
		t.Fatalf("standing %+v results %+v", root.Standing, root.Results)
	}
	if len(root.WaitingResults) != 2 || root.WaitingResultsTruncated {
		t.Fatalf("waiting: %+v truncated %v", root.WaitingResults, root.WaitingResultsTruncated)
	}
	first, second := root.WaitingResults[0], root.WaitingResults[1]
	if first.WorkID != r2 || first.ResultID != r2 || first.Worker.ID != keyID(w2) || first.SubmittedAt != testTime+60 || first.fence != getTestWork(t, s, r2).Fence || first.fence < 1 || first.Deadline <= first.SubmittedAt || second.WorkID != r3 {
		t.Fatalf("waiting rows: %+v %+v", first, second)
	}
	// C702: the fence is only for the refusal's text; the row's keys stay
	// as shipped strict clients check them.
	if raw, err := json.Marshal(first); err != nil || strings.Contains(string(raw), "fence") {
		t.Fatalf("waiting row %s %v", raw, err)
	}

	for _, op := range []string{"work.accept", "work.reject"} {
		c := Command{Operation: op, MessageID: id, Amount: 1}
		if op == "work.reject" {
			c.Reason = "no"
		}
		msg := failsWith(t, s, workCommand(s, owner, c), "work_state_conflict")
		for _, want := range []string{"standing task", "judged one result at a time", "accept or reject each result by its own id",
			"Waiting: " + r2 + " (fence 1), " + r3 + " (fence 1).", "waiting_results", `works.list data {"standing":"` + id + `"}`} {
			if !strings.Contains(msg, want) {
				t.Fatalf("%s by task id lacks %q: %s", op, want, msg)
			}
		}
	}

	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: r2, Amount: 1}))
	root = getTestWork(t, s, id)
	if len(root.WaitingResults) != 1 || root.WaitingResults[0].WorkID != r3 || len(root.Results) != 2 {
		t.Fatalf("after accept: waiting %+v results %d", root.WaitingResults, len(root.Results))
	}
	// Only on the standing task, not on a result's own item.
	if w := getTestWork(t, s, r3); w.WaitingResults != nil {
		t.Fatalf("result item lists %+v", w.WaitingResults)
	}
}
