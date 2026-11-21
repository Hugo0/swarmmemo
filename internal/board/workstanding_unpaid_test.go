package board

import (
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
)

// C1180: a standing tier that pays nothing ("accepted, unpaid"), so a
// requester honours a published "a valid negative result is accepted but
// unpaid" without rejecting valid work. Only after a first tier that pays,
// never with usdc; accepting at it moves no credits and no USDC, leaves no
// reject mark, and counts as accepted for results_max and the work record.
func TestWorkStandingUnpaidTier(t *testing.T) {
	s := rewardStore(t)
	owner, w1, w2, w3 := keyFor(190), keyFor(191), keyFor(192), keyFor(193)
	requester := keyID(owner)
	mintCredit(t, s, requester, allowance.Paid, 100_000)
	id := rewardRequest(t, s, owner, "lobby")
	ttl := int64(10 * 86400)
	tier := func(name string, credits int64, usdc ...string) map[string]any {
		m := map[string]any{"name": name, "credits": credits}
		if len(usdc) > 0 {
			m["usdc"] = usdc[0]
		}
		return m
	}
	for _, bad := range [][]any{
		{tier("valid-negative", 0)},
		{tier("a", 0), tier("b", 0)},
		{tier("valid-negative", 0), tier("bug", 20_000)},
		{tier("bug", 20_000), tier("valid-negative", 0, "0.10")},
		{tier("bug", 20_000), tier("valid-negative", -1)},
	} {
		res := standingCreate(s, owner, id, map[string]any{"seats": 2, "tiers": bad}, ttl)
		fails(t, s, res, "invalid_work_data")
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM works"); n != 0 {
		t.Fatalf("refused creates left %d works", n)
	}

	// A paying tier first, then one that pays nothing: the escrow is sized by
	// the top tier, as before.
	run(t, s, standingCreate(s, owner, id, map[string]any{"seats": 2, "tiers": []any{tier("bug", 20_000), tier("valid-negative", 0)}}, ttl))
	if held := creditIn(t, s, requester, "held"); held != 40_000 {
		t.Fatalf("the pool holds %d, want 40000", held)
	}
	if st := standingAt(t, s, id); len(st.Tiers) != 2 || st.Tiers[1] != (WorkStandingTier{Name: "valid-negative"}) {
		t.Fatalf("tiers: %+v", st.Tiers)
	}

	r1, c1 := standingClaim(t, s, w1, id, nil)
	run(t, s, c1)
	// C1158 composes: before its verdict it holds the top tier's credits, up_to.
	if w := getTestWork(t, s, r1); w.Reward == nil || w.Reward.Amount != 20_000 || !w.Reward.UpTo {
		t.Fatalf("pending: %+v", w.Reward)
	}
	requesterBefore := creditIn(t, s, requester, "remaining")
	ack := run(t, s, workWith(s, owner, Command{Operation: "work.accept", MessageID: r1, Amount: 1}, map[string]any{"tier": "valid-negative"})).Data["ack"].(WorkAck)
	if ack.State != "accepted" || !strings.Contains(ack.Note, "Accepted, unpaid: tier valid-negative pays no credits") || strings.Contains(ack.Note, "paid 0") {
		t.Fatalf("accept ack: %+v", ack)
	}
	// No balance moved: nothing to the worker, nothing from the requester
	// (no refill), and the whole reward is back in the pool.
	if got := creditIn(t, s, keyID(w1), "remaining"); got != 0 {
		t.Fatalf("the worker got %d credits", got)
	}
	if got := creditIn(t, s, requester, "remaining"); got != requesterBefore {
		t.Fatalf("requester remaining %d, want %d", got, requesterBefore)
	}
	if held := creditIn(t, s, requester, "held"); held != 40_000 {
		t.Fatalf("after the unpaid accept %d held, want 40000", held)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM work_usdc"); n != 0 {
		t.Fatalf("%d work_usdc rows", n)
	}
	w := getTestWork(t, s, r1)
	if w.State != "accepted" || w.Tier != "valid-negative" || !w.UnpaidTier() || w.Reward == nil || w.Reward.Amount != 0 || w.Reward.State != "released" ||
		w.Reward.Reason != WorkUnpaidTierReason || w.Reward.UpTo || w.Reward.Receipt != nil || w.RewardUSDC != nil {
		t.Fatalf("unpaid result: %+v %+v", w, w.Reward)
	}
	// No reject mark: the last verdict is the accept, and the worker's
	// record counts it accepted, not paid and not rejected.
	history := run(t, s, Command{Operation: "work.history", MessageID: r1}).Data["transitions"].([]WorkTransition)
	if last := history[len(history)-1]; last.Operation != "work.accept" {
		t.Fatalf("history: %+v", history)
	}
	records, err := workRecords(testContext, s.db, []string{keyID(w1)}, s.now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	if r := records[keyID(w1)]; r == nil || r.AsWorker.Accepted != 1 || r.AsWorker.Paid != 0 || r.AsWorker.Rejected != 0 || r.AsWorker.PaidCredits != 0 {
		t.Fatalf("worker record %+v", r)
	}
	// The root's accepted results list it with its tier, and no money.
	root := getTestWork(t, s, id)
	if len(root.Results) != 1 || root.Results[0].Tier != "valid-negative" || root.Results[0].Money != nil {
		t.Fatalf("accepted results: %+v", root.Results)
	}
	if st := root.Standing; st.Paid != 0 || st.Results != 1 {
		t.Fatalf("standing after the unpaid accept: %+v", st)
	}

	// It counts toward results_max (default seats, 2): one more result
	// fills it, and a third is refused.
	_, c2 := standingClaim(t, s, w2, id, nil)
	run(t, s, c2)
	_, c3 := standingClaim(t, s, w3, id, nil)
	fails(t, s, c3, "results_max_reached")

	// The first tier still pays as before.
	r2 := getTestWork(t, s, id).WaitingResults[0].WorkID
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: r2, Amount: 1}))
	if got := creditIn(t, s, keyID(w2), "remaining"); got != 20_000 {
		t.Fatalf("w2 got %d credits", got)
	}
	if st := standingAt(t, s, id); st.Paid != 1 {
		t.Fatalf("standing after the paid accept: %+v", st)
	}
}
