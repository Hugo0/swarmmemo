package board

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
)

// Standing tasks (C278) on the real store and ledger: a claim with a result
// opens its own task, seats and the per-agent and per-day limits hold, an
// accept pays the tier it names from the pool and refills it, a reject and a
// lapse return the seat and the reward, and an undecided result counts as
// unpaid_lapsed.

func standingCreate(s *Store, key ed25519.PrivateKey, id string, extra map[string]any, ttl int64) Command {
	d := map[string]any{"title": "Standing bug bounty", "capabilities": []string{"bugs"}}
	for k, v := range extra {
		d[k] = v
	}
	return workWith(s, key, Command{Operation: "work.create", MessageID: id, TTL: ttl}, d)
}

// standingClaim posts worker's result under the standing task and claims it;
// it returns the reply's ID and the claim's command.
func standingClaim(t *testing.T, s *Store, worker ed25519.PrivateKey, id string, extra map[string]any) (string, Command) {
	t.Helper()
	reply := run(t, s, signed(worker, Command{Operation: "post", Room: "lobby", ReplyTo: id, Text: "Bug: the thing breaks; repro attached " + randomID()[:8], Timestamp: s.now().Unix()})).Receipt.ID
	return reply, workWith(s, worker, Command{Operation: "work.claim", MessageID: id, Target: reply}, extra)
}

func standingAt(t *testing.T, s *Store, id string) *WorkStanding {
	t.Helper()
	w := getTestWork(t, s, id)
	if w.Standing == nil {
		t.Fatalf("%s reads no standing block: %+v", id, w)
	}
	return w.Standing
}

func TestWorkStandingSeatsAcceptRejectLapse(t *testing.T) {
	s := rewardStore(t)
	owner, w1, w2, w3 := keyFor(170), keyFor(171), keyFor(172), keyFor(173)
	requester := keyID(owner)
	mintCredit(t, s, requester, allowance.Paid, 5000)
	id := rewardRequest(t, s, owner, "lobby")
	ttl := int64(10 * 86400)

	for _, bad := range []map[string]any{
		{"seats": 0, "reward": 300},
		{"seats": 51, "reward": 300},
		{"seats": 2},
		{"per_agent_day": 1, "reward": 300},
		{"seats": 2, "reward": 300, "per_agent_day": 3},
		{"seats": 2, "reward": map[string]any{"usdc": "0.10"}},
		{"seats": 2, "reward": 300, "tiers": []any{map[string]any{"name": "bug", "credits": 300}}},
		{"seats": 2, "tiers": []any{map[string]any{"name": "bug", "credits": 300}, map[string]any{"name": "bug", "credits": 400}}},
		{"seats": 2, "tiers": []any{map[string]any{"name": "a", "credits": 1}, map[string]any{"name": "b", "credits": 1}, map[string]any{"name": "c", "credits": 1}, map[string]any{"name": "d", "credits": 1}}},
		{"seats": 2, "tiers": []any{map[string]any{"name": "bug", "usdc": "0.20"}}},
		{"seats": 2, "reward": 300, "usdc_day_max": "1.00"},
		{"seats": 2, "reward": 300, "reviewer": keyID(w3)},
	} {
		fails(t, s, standingCreate(s, owner, id, bad, ttl), "invalid_work_data")
	}
	fails(t, s, standingCreate(s, owner, id, map[string]any{"seats": 2, "reward": 300}, 3*86400), "invalid_ttl")
	if n := sqlCount(t, s, "SELECT count(*) FROM works"); n != 0 {
		t.Fatalf("refused creates left %d works", n)
	}

	run(t, s, standingCreate(s, owner, id, map[string]any{"seats": 2, "reward": 300, "day_max": 4}, ttl))
	supply := totalCredit(t, s)
	if held := creditIn(t, s, requester, "held"); held != 600 {
		t.Fatalf("the pool holds %d, want 600", held)
	}
	st := standingAt(t, s, id)
	if st.Seats != 2 || st.Open != 2 || st.PerAgentDay != 1 || st.DayMax != 4 || len(st.Tiers) != 1 || st.Tiers[0] != (WorkStandingTier{Name: WorkStandingTierFirst, Credits: 300}) || st.ResultsUntil != testTime+ttl-WorkStandingVerdictTTL {
		t.Fatalf("standing: %+v", st)
	}
	// The root takes no ttl-only claim and no reviewer.
	fails(t, s, workCommand(s, w1, Command{Operation: "work.claim", MessageID: id, TTL: 600}), "standing_needs_result")
	fails(t, s, workWith(s, owner, Command{Operation: WorkReviewerSet, MessageID: id}, map[string]any{"reviewer": keyID(w3)}), "work_state_conflict")

	// A claim with a result opens a task keyed by the reply.
	r1, claim := standingClaim(t, s, w1, id, nil)
	ack := run(t, s, claim).Data["ack"].(WorkAck)
	if ack.WorkID != r1 || ack.State != "submitted" || ack.Fence != 1 || ack.ResolvedFrom != id || ack.Deadline != testTime+WorkStandingVerdictTTL || !strings.Contains(ack.Note, "seat 1 of 2") {
		t.Fatalf("ack: %+v", ack)
	}
	item := getTestWork(t, s, r1)
	// requester_author is the standing task's author, not the result post's (f5654d25, 10-17).
	if item.RequesterAuthor != requester || item.Requester.ID != requester {
		t.Fatalf("result task requester_author %s, requester %s, want %s", item.RequesterAuthor, item.Requester.ID, requester)
	}
	if item.State != "submitted" || item.StandingID != id || item.Worker == nil || item.Worker.ID != keyID(w1) || item.ResultID != r1 || !item.ResultAvailable ||
		item.Reward == nil || item.Reward.Amount != 300 || item.Reward.State != "held" || item.Request == nil || item.Request.VersionID != id || item.Standing != nil {
		t.Fatalf("result task: %+v", item)
	}
	if root := getTestWork(t, s, id); root.State != "open" || root.Worker != nil || root.Standing.Submitted != 1 || root.Standing.Open != 1 || root.Reward.Amount != 300 {
		t.Fatalf("root after one result: %+v %+v", root, root.Standing)
	}
	if held := creditIn(t, s, requester, "held"); held != 600 {
		t.Fatalf("opening a seat changed what is held: %d", held)
	}
	history := run(t, s, Command{Operation: "work.history", MessageID: r1}).Data["transitions"].([]WorkTransition)
	if len(history) != 1 || history[0].Operation != "work.claim" || history[0].State != "submitted" || history[0].ResolvedFrom != id {
		t.Fatalf("history: %+v", history)
	}

	// One result per task; one waiting result per agent.
	fails(t, s, workWith(s, w1, Command{Operation: "work.claim", MessageID: id, Target: r1}, nil), "result_in_use")
	_, again := standingClaim(t, s, w1, id, nil)
	fails(t, s, again, "seat_limit")

	r2, claim2 := standingClaim(t, s, w2, id, nil)
	run(t, s, claim2)
	_, claim3 := standingClaim(t, s, w3, id, nil)
	fails(t, s, claim3, "seats_full")

	// Accept pays the tier from the result's escrow and refills the pool.
	requesterBefore := creditIn(t, s, requester, "remaining")
	acked := run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: r2, Amount: 1})).Data["ack"].(WorkAck)
	if acked.State != "accepted" || acked.WorkID != r2 {
		t.Fatalf("accept: %+v", acked)
	}
	if got := creditIn(t, s, keyID(w2), "remaining"); got != 300 {
		t.Fatalf("the worker got %d credits", got)
	}
	// The worker's 300 left the requester; the refill held 300 more and
	// paid its transfer fee (2).
	if got := creditIn(t, s, requester, "remaining"); got != requesterBefore-302 {
		t.Fatalf("requester remaining %d, want %d", got, requesterBefore-302)
	}
	if held := creditIn(t, s, requester, "held"); held != 600 {
		t.Fatalf("after accept and refill %d held, want 600", held)
	}
	if w := getTestWork(t, s, r2); w.Tier != WorkStandingTierFirst || w.Reward.State != "paid" || w.Reward.Receipt == nil {
		t.Fatalf("accepted result: %+v %+v", w, w.Reward)
	}
	if st = standingAt(t, s, id); st.Submitted != 1 || st.Paid != 1 || st.Open != 1 || st.Results != 2 {
		t.Fatalf("after accept: %+v", st)
	}
	r3, claim3 := standingClaim(t, s, w3, id, nil)
	run(t, s, claim3)

	// Reject is terminal and returns the seat and the reward to the pool.
	fails(t, s, workCommand(s, owner, Command{Operation: "work.cancel", MessageID: r1, Reason: "no"}), "work_state_conflict")
	fails(t, s, workWith(s, owner, Command{Operation: "work.accept", MessageID: r1, Amount: 1}, map[string]any{"tier": "nope"}), "invalid_work_data")
	rejected := workCommand(s, owner, Command{Operation: "work.reject", MessageID: r1, Amount: 1, Reason: "not reproducible"})
	if ack := run(t, s, rejected).Data["ack"].(WorkAck); ack.State != "cancelled" {
		t.Fatalf("reject ack: %+v", ack)
	}
	if w := getTestWork(t, s, r1); w.State != "cancelled" || w.Reward.State != "released" || w.Reward.Reason != "rejected" || w.Worker == nil {
		t.Fatalf("rejected result: %+v %+v", w, w.Reward)
	}
	if st = standingAt(t, s, id); st.Submitted != 1 || st.Open != 1 {
		t.Fatalf("after reject: %+v", st)
	}
	if held := creditIn(t, s, requester, "held"); held != 600 {
		t.Fatalf("after reject %d held, want 600", held)
	}
	page := run(t, s, Command{Operation: "message.get", MessageID: r1})
	if raw, _ := json.Marshal(page.Messages); !strings.Contains(string(raw), `"result_of":"`+r1+`"`) || !strings.Contains(string(raw), `"state":"rejected"`) {
		t.Fatalf("rejected mark: %s", raw)
	}
	// The same agent, the same day: its one result for today is spent.
	_, again = standingClaim(t, s, w1, id, nil)
	fails(t, s, again, "seat_limit")

	// A result undecided past its own deadline lapses: the seat returns at
	// once, its reward on the next claim, and it counts as unpaid_lapsed.
	atTime(s, testTime+WorkStandingVerdictTTL+60)
	if w := getTestWork(t, s, r3); w.State != "expired" {
		t.Fatalf("lapsed result reads %q", w.State)
	}
	root := getTestWork(t, s, id)
	if root.State != "open" || root.Standing.Submitted != 0 || root.Standing.Open != 2 || root.RequesterRecord == nil || root.RequesterRecord.UnpaidLapsed != 1 || root.RequesterRecord.Paid != 1 || root.RequesterRecord.Rejected != 1 {
		t.Fatalf("after lapse: %+v %+v", root.Standing, root.RequesterRecord)
	}
	r4, claim4 := standingClaim(t, s, w1, id, nil)
	run(t, s, claim4)
	if w := getTestWork(t, s, r3); w.Reward.State != "released" || w.Reward.Reason != "requester_lapsed" {
		t.Fatalf("lapsed reward: %+v", w.Reward)
	}
	if held := creditIn(t, s, requester, "held"); held != 600 {
		t.Fatalf("after lapse %d held, want 600", held)
	}

	// day_max: 4 results a day, counted while waiting or once accepted
	// (C614: a rejected one frees its slot). Today (the lapse's day) has
	// one; three more agents may open, the fifth is refused.
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: r4, Amount: 1}))
	for i, k := range []ed25519.PrivateKey{keyFor(174), keyFor(175)} {
		r, c := standingClaim(t, s, k, id, nil)
		run(t, s, c)
		if i == 0 {
			run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: r, Amount: 1}))
		}
	}
	_, c := standingClaim(t, s, keyFor(176), id, nil)
	run(t, s, c)
	_, c = standingClaim(t, s, keyFor(177), id, nil)
	// C585: the refusal says when the next seat opens, in words and in
	// details, and the public reads (work.get, works.list) carry the same.
	// Both seats also hold a result opened today, so the next one opens
	// past the day's reset, at the first of their verdict deadlines.
	now := int64(testTime + WorkStandingVerdictTTL + 60) // atTime above
	next := now + WorkStandingVerdictTTL
	_, err := s.Execute(testContext, c, "test-origin")
	var refused *Error
	want := "No seat is open on this task right now; the next one opens at " + time.Unix(next, 0).UTC().Format(time.RFC3339) + " (daily limit, then a waiting result's verdict deadline): it takes 4 results a day and has them today (day_max)."
	if !errors.As(err, &refused) || refused.Code != "seat_limit" || refused.Message != want || refused.RetryAfter != int(next-now) || refused.NextOpenAt != next {
		t.Fatalf("day_max refusal: %v, want %q", err, want)
	}
	if d, _ := refused.Details.(map[string]any); d["next_open_at"] != next || d["blocked_by"] != StandingBlockedDayMax {
		t.Fatalf("day_max refusal details: %#v", refused.Details)
	}
	if st = standingAt(t, s, id); st.OpenedToday != 4 || st.Open != 0 || st.NextOpenAt != next || st.BlockedBy != StandingBlockedSeats {
		t.Fatalf("day max: %+v", st)
	}
	openList := run(t, s, Command{Operation: "works.list", Kind: "open"})
	found := false
	for _, w := range openList.Data["works"].([]Work) {
		if w.ID == id {
			found = w.Standing != nil && w.Standing.Open == 0 && w.Standing.NextOpenAt == next
		}
	}
	if !found {
		t.Fatalf("works.list misses the standing task's seats open now and next opening: %+v", openList.Data["works"])
	}

	// Only the one refill's transfer fee left the supply; the store is
	// consistent.
	if got := totalCredit(t, s); got != supply-2 {
		t.Fatalf("supply %d -> %d", supply, got)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM works WHERE standing_id=?", id); n != 7 {
		t.Fatalf("%d results opened, want 7", n)
	}
	listPage := run(t, s, Command{Operation: "works.list", Data: `{"schema":1,"standing":"` + id + `"}`, Limit: 2})
	listed := run(t, s, Command{Operation: "works.list", Data: `{"schema":1,"standing":"` + id + `"}`}).Data["works"].([]Work)
	if len(listed) != 7 {
		t.Fatalf("works.list standing lists %d", len(listed))
	}
	// C302: the list carries the standing task's seats, the very object
	// work.get shows, on every page; a result's ID (not a standing task)
	// lists nothing and carries none.
	if got, ok := listPage.Data["standing"].(*WorkStanding); !ok || !reflect.DeepEqual(*got, *standingAt(t, s, id)) {
		t.Fatalf("works.list standing summary %+v, want work.get's %+v", listPage.Data["standing"], standingAt(t, s, id))
	}
	if other := run(t, s, Command{Operation: "works.list", Data: `{"schema":1,"standing":"` + listed[0].ID + `"}`}); len(other.Data["works"].([]Work)) != 0 || other.Data["standing"] != nil {
		t.Fatalf("a result's ID as standing: %+v", other.Data)
	}
	if err := s.Integrity(testContext); err != nil {
		t.Fatal(err)
	}
	// Cancelling the standing task releases the pool; its results' escrows
	// go back to the requester when they end.
	run(t, s, workCommand(s, owner, Command{Operation: "work.cancel", MessageID: id, Reason: "closed"}))
	if held := creditIn(t, s, requester, "held"); held != 300*2 {
		t.Fatalf("after cancel %d held, want the two waiting results' escrows", held)
	}
	if w := getTestWork(t, s, id); w.Reward.State != "released" || w.Standing.Open != 0 {
		t.Fatalf("cancelled standing task: %+v %+v", w.Reward, w.Standing)
	}
}

// A standing task's tiers go beside seats, a sibling of reward, never
// inside the reward object: a reward object that carries tiers (or any
// other unexpected key, once seats names a standing task) gets a dedicated
// hint instead of the generic reward-object refusal (Skitter c65).
func TestWorkStandingTiersMisplacedInReward(t *testing.T) {
	s := rewardStore(t)
	owner := keyFor(179)
	mintCredit(t, s, keyID(owner), allowance.Paid, 1000)
	id := rewardRequest(t, s, owner, "lobby")
	tiers := []any{map[string]any{"name": "bug", "credits": 500}}

	for _, data := range []map[string]any{
		{"seats": 3, "reward": map[string]any{"tiers": tiers}},
		{"seats": 3, "reward": map[string]any{"tiers": tiers, "credits": 300}},
		// Any unexpected key in reward, once seats is present, gets the
		// same hint: tiers is the likely mistake even when misspelled.
		{"seats": 3, "reward": map[string]any{"triers": tiers}},
	} {
		c := standingCreate(s, owner, id, data, 10*86400)
		_, err := s.Execute(testContext, c, "test-origin")
		var e *Error
		if !errors.As(err, &e) || e.Code != "invalid_work_reward" {
			t.Fatalf("%+v: want invalid_work_reward, got %v", data, err)
		}
		want := `tiers go beside seats in the work data, not inside reward: {"seats":3,"tiers":[{"name":"bug","credits":500}]}`
		if e.Message != want {
			t.Fatalf("%+v: message %q, want %q", data, e.Message, want)
		}
	}

	// A reward object naming tiers gets the hint even without seats at the
	// top level (not a standing task at all): tiers never goes in reward.
	fails(t, s, rewardCreate(s, owner, id, map[string]any{"tiers": tiers}, 0), "invalid_work_reward")

	// Without seats, an unrelated unexpected key in reward still gets the
	// ordinary reward-object refusal, not the tiers hint.
	fails(t, s, rewardCreate(s, owner, id, map[string]any{"eth": "1"}, 0), "invalid_work_reward")
}

// TestWorkStandingClientVector: the data the clients build for a standing
// create (C311; clients/python and clients/javascript assert they build
// exactly testdata/work_standing_create.json's data, the CLI from its argv)
// opens that standing task on the real store, every field as sent.
func TestWorkStandingClientVector(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/work_standing_create.json")
	if err != nil {
		t.Fatal(err)
	}
	var vector struct{ Generation, Data string }
	if err = json.Unmarshal(raw, &vector); err != nil {
		t.Fatal(err)
	}
	s, _ := usdcStore(t)
	owner := keyFor(183)
	mintCredit(t, s, keyID(owner), allowance.Paid, 5000)
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, signed(owner, Command{Operation: "work.create", MessageID: id, TTL: 10 * 86400, Timestamp: s.now().Unix(),
		Data: strings.ReplaceAll(vector.Data, `"`+vector.Generation+`"`, `"`+s.generation+`"`)}))
	got := standingAt(t, s, id)
	want := []WorkStandingTier{{Name: "bug", Credits: 300, USDC: "0.2"}, {Name: "bug+fix", Credits: 500, USDC: "0.4"}} // normalized
	if got.Seats != 5 || got.PerAgentDay != 1 || got.DayMax != 5 || !reflect.DeepEqual(got.Tiers, want) || got.USDCPerAgentDay != "0.4" || got.USDCDayMax != "2" {
		t.Fatalf("standing task from the client vector: %+v", got)
	}
	if held := creditIn(t, s, keyID(owner), "held"); held != 2500 {
		t.Fatalf("pool %d, want 2500 (the largest tier times seats)", held)
	}
}

func TestWorkStandingTiersAndUSDCCaps(t *testing.T) {
	s, _ := usdcStore(t)
	owner := keyFor(180)
	requester := keyID(owner)
	mintCredit(t, s, requester, allowance.Paid, 5000)
	id := rewardRequest(t, s, owner, "lobby")
	tiers := []any{map[string]any{"name": "bug", "credits": 300, "usdc": "0.20"}, map[string]any{"name": "bug+fix", "credits": 500, "usdc": "0.40"}}
	fails(t, s, standingCreate(s, owner, id, map[string]any{"seats": 3, "tiers": tiers, "usdc_per_agent_day": "0.30"}, 10*86400), "invalid_work_data")
	run(t, s, standingCreate(s, owner, id, map[string]any{"seats": 3, "tiers": tiers, "usdc_per_agent_day": "0.40", "usdc_day_max": "0.50"}, 10*86400))
	if held := creditIn(t, s, requester, "held"); held != 1500 {
		t.Fatalf("pool %d, want 1500", held)
	}
	if w := getTestWork(t, s, id); w.RewardUSDC != nil {
		t.Fatalf("the standing task itself promises USDC: %+v", w.RewardUSDC)
	}
	// Its mark on message reads carries the terms per result beside the
	// pool, so a feed never shows the pool as one result's reward (C434).
	mark := workMarks(run(t, s, Command{Operation: "message.get", MessageID: id}).Messages)[id]
	wantTerms := &MessageWorkStanding{Seats: 3, Tiers: []WorkStandingTier{{Name: "bug", Credits: 300, USDC: "0.2"}, {Name: "bug+fix", Credits: 500, USDC: "0.4"}}}
	if mark == nil || mark.Reward == nil || mark.Reward.Amount != 1500 || !reflect.DeepEqual(mark.Standing, wantTerms) {
		t.Fatalf("standing mark: %+v (standing %+v)", mark, mark.Standing)
	}

	w1, w2 := keyFor(181), keyFor(182)
	_, bare := standingClaim(t, s, w1, id, nil)
	fails(t, s, bare, "payout_address_required")
	r1, c1 := standingClaim(t, s, w1, id, map[string]any{"payout_address": usdcPayee})
	run(t, s, c1)
	if w := getTestWork(t, s, r1); w.RewardUSDC == nil || w.RewardUSDC.State != "promised" || w.RewardUSDC.Amount != "0.4" || !strings.EqualFold(w.RewardUSDC.PayTo, usdcPayee) {
		t.Fatalf("promised: %+v", w.RewardUSDC)
	}
	r2, c2 := standingClaim(t, s, w2, id, map[string]any{"payout_address": usdcOther})
	run(t, s, c2)

	// bug+fix: 500 credits and 0.40 USDC owed.
	run(t, s, workWith(s, owner, Command{Operation: "work.accept", MessageID: r1, Amount: 1}, map[string]any{"tier": "bug+fix"}))
	if w := getTestWork(t, s, r1); w.Tier != "bug+fix" || w.Reward.Amount != 500 || w.RewardUSDC.State != "payable" || w.RewardUSDC.Amount != "0.4" {
		t.Fatalf("bug+fix: %+v %+v", w.Reward, w.RewardUSDC)
	}
	if got := creditIn(t, s, keyID(w1), "remaining"); got != 500 {
		t.Fatalf("w1 has %d", got)
	}
	if st := standingAt(t, s, id); st.Owed != 1 || st.Submitted != 1 {
		t.Fatalf("owed seat: %+v", st)
	}

	// The default tier (bug): 0.20 more would pass the 0.50 day cap, so the
	// result is paid in credits only, and the excess credits return.
	acked := run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: r2, Amount: 1})).Data["ack"].(WorkAck)
	if !strings.Contains(acked.Note, "paid in credits only: 300 credits") || !strings.Contains(acked.Note, "no USDC is owed") {
		t.Fatalf("capped note: %q", acked.Note)
	}
	w := getTestWork(t, s, r2)
	if w.Tier != "bug" || w.Reward.Amount != 300 || w.Reward.State != "paid" || w.Reward.Receipt == nil || w.RewardUSDC.State != "void" || w.RewardUSDC.Reason != "usdc_cap" {
		t.Fatalf("capped: %+v %+v", w.Reward, w.RewardUSDC)
	}
	if got := creditIn(t, s, keyID(w2), "remaining"); got != 300 {
		t.Fatalf("w2 has %d", got)
	}
	// Two refills of 500 and the 200 bug's excess back: 1500 held again.
	if held := creditIn(t, s, requester, "held"); held != 1500 {
		t.Fatalf("pool after the accepts %d, want 1500", held)
	}
	fails(t, s, workWith(s, owner, Command{Operation: "work.accept", MessageID: id, Amount: 1}, map[string]any{"tier": "bug"}), "work_state_conflict")
	if err := s.Integrity(testContext); err != nil {
		t.Fatal(err)
	}
}

// The ledger keeps every lot's held units equal to its open holds' parts
// through splits and joins.
func TestWorkStandingLedgerConservation(t *testing.T) {
	s := rewardStore(t)
	owner := keyFor(185)
	requester := keyID(owner)
	mintCredit(t, s, requester, allowance.Paid, 3000)
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, id, map[string]any{"seats": 3, "reward": 200, "per_agent_day": 1}, 10*86400))
	for i := range 3 {
		r, c := standingClaim(t, s, keyFor(byte(186+i)), id, nil)
		run(t, s, c)
		if i == 1 {
			run(t, s, workCommand(s, owner, Command{Operation: "work.reject", MessageID: r, Amount: 1, Reason: "no"}))
		}
	}
	if bad := sqlCount(t, s, `SELECT count(*) FROM ledger_lots l WHERE l.held<>(SELECT coalesce(sum(hp.units),0) FROM ledger_hold_parts hp JOIN ledger_holds h ON h.id=hp.hold_id WHERE hp.lot_id=l.id AND h.state='held')`); bad != 0 {
		t.Fatalf("%d lots whose held units differ from their holds' parts", bad)
	}
	if bad := sqlCount(t, s, `SELECT count(*) FROM ledger_holds h WHERE h.state='held' AND h.max_units<>(SELECT coalesce(sum(units),0) FROM ledger_hold_parts WHERE hold_id=h.id)`); bad != 0 {
		t.Fatalf("%d holds whose parts do not add up to their units", bad)
	}
	if held := creditIn(t, s, requester, "held"); held != 600 {
		t.Fatalf("held %d", held)
	}
}

// Racing claims for the last seat on the one connection: exactly one opens
// a result, the others are seats_full, and the pool moves once.
func TestWorkStandingSeatRace(t *testing.T) {
	s := rewardStore(t)
	owner := keyFor(195)
	requester := keyID(owner)
	mintCredit(t, s, requester, allowance.Paid, 3000)
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, id, map[string]any{"seats": 1, "reward": 200, "day_max": 10}, 10*86400))
	cmds := make([]Command, 4)
	for i := range cmds {
		_, cmds[i] = standingClaim(t, s, keyFor(byte(196+i)), id, nil)
	}
	errs := make([]error, len(cmds))
	var wg sync.WaitGroup
	for k := range cmds {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[k] = s.Execute(testContext, cmds[k], "test-origin")
		}()
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		var e *Error
		switch {
		case err == nil:
			won++
		case !errors.As(err, &e) || e.Code != "seats_full":
			t.Fatalf("a losing claim: %v", err)
		}
	}
	if won != 1 || sqlCount(t, s, "SELECT count(*) FROM works WHERE standing_id=?", id) != 1 || creditIn(t, s, requester, "held") != 200 {
		t.Fatalf("%d claims won; held %d", won, creditIn(t, s, requester, "held"))
	}
	if err := s.Integrity(testContext); err != nil {
		t.Fatal(err)
	}
}

// A populated schema-34 database migrates to exactly the schema a new
// database gets, keeps every row, and a second start runs no DDL.
func TestSchema35UpgradeFrom34(t *testing.T) {
	ddl, err := os.ReadFile(filepath.Join("testdata", "schema34.sql"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "schema34.db")
	db := rawDB(t, path)
	_, body, _ := strings.Cut(string(ddl), "do not edit.\n")
	blocks := strings.Split(strings.TrimPrefix(body, "-- "), "\n-- ")
	for _, tables := range []bool{true, false} {
		for _, block := range blocks {
			head, statement, _ := strings.Cut(block, "\n")
			if strings.HasPrefix(head, "table ") != tables {
				continue
			}
			if _, err = db.Exec(statement); err != nil {
				t.Fatalf("%s: %v", head, err)
			}
		}
	}
	if _, err = db.Exec("PRAGMA user_version=34"); err != nil {
		t.Fatal(err)
	}
	seedEveryTable(t, db)
	before := tableCounts(t, db)
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	openClose(t, path, Config{})
	db = rawDB(t, path)
	if v := pragmaInt(t, db, "user_version"); v != SchemaVersion || SchemaVersion < 35 {
		t.Fatalf("user_version %d, SchemaVersion %d", v, SchemaVersion)
	}
	if got, fresh := normalizedSchema(t, db), freshSchema(t, Config{}); got != fresh {
		t.Fatalf("upgraded schema differs from a new database's:\n%s", schemaDiff(got, fresh))
	}
	after := tableCounts(t, db)
	for table, n := range before {
		if after[table] < n {
			t.Errorf("%s: %d rows before, %d after", table, n, after[table])
		}
	}
	var blank int64
	if err = db.QueryRow("SELECT count(*) FROM works WHERE standing_id='' AND standing_tier=''").Scan(&blank); err != nil || blank != int64(before["works"]) {
		t.Fatalf("existing works: %d blank of %d (%v)", blank, before["works"], err)
	}
	cookie := pragmaInt(t, db, "schema_version")
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	openClose(t, path, Config{})
	if got := pragmaInt(t, rawDB(t, path), "schema_version"); got != cookie {
		t.Fatalf("schema cookie %d -> %d: the second start changed the schema", cookie, got)
	}
}

// FuzzWorkStandingData: any create data either parses into a consistent
// standing task or is refused as invalid_work_data, never a panic.
func FuzzWorkStandingData(f *testing.F) {
	gen := strings.Repeat("a", 32)
	for _, seed := range []string{
		`"seats":2,"reward":300`,
		`"seats":3,"tiers":[{"name":"bug","credits":300,"usdc":"0.20"}],"usdc_day_max":"2"`,
		`"seats":1,"per_agent_day":1,"day_max":1000,"reward":{"credits":5,"usdc":"0.01"}`,
		`"seats":3,"results_max":3,"reward":5`,
		`"seats":3,"results_max":2,"reward":5`,
		`"seats":-1,"tiers":[]`,
		`"seats":2,"tiers":[{"name":"","credits":0}]`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, fields string) {
		raw := fmt.Sprintf(`{"schema":1,"generation":%q,"title":"t","capabilities":[],%s}`, gen, fields)
		d, err := parseWorkData(raw, "work.create")
		if err != nil {
			var e *Error
			if !errors.As(err, &e) || e.Status != 400 {
				t.Fatalf("%s: %v", raw, err)
			}
			return
		}
		st := d.Standing
		if st.Seats == 0 {
			return
		}
		if st.Seats > WorkStandingSeatsMax || st.PerAgentDay < 1 || st.PerAgentDay > st.Seats || st.DayMax < 1 || st.DayMax > WorkStandingDayMax || len(st.Tiers) < 1 || len(st.Tiers) > WorkStandingTiersMax ||
			(st.ResultsMax != 0 && (st.ResultsMax < st.Seats || st.ResultsMax > WorkStandingResultsMax)) {
			t.Fatalf("%s parsed into %+v", raw, st)
		}
		for _, tier := range st.Tiers {
			if tier.Credits < 1 || (st.USDCDayMax > 0 && tier.usdcUnits() > st.USDCDayMax) || (st.USDCPerAgentDay > 0 && tier.usdcUnits() > st.USDCPerAgentDay) {
				t.Fatalf("%s: tier %+v", raw, tier)
			}
		}
	})
}

// A simulation can't hold a standing pool: tiers carry the reward, so seats on
// a non-request root are refused like any reward (C278 review).
func TestWorkStandingRefusesSimulationRoot(t *testing.T) {
	s := rewardStore(t)
	owner := keyFor(180)
	mintCredit(t, s, keyID(owner), allowance.Paid, 5000)
	sim := run(t, s, signed(owner, Command{Operation: "post", Room: "lobby", Kind: "simulation", Text: "A practice task", Timestamp: s.now().Unix()})).Receipt.ID
	fails(t, s, standingCreate(s, owner, sim, map[string]any{"seats": 2, "tiers": []any{map[string]any{"name": "bug", "credits": 50, "usdc": "1.00"}}}, 10*86400), "invalid_work_reward")
	if held := creditIn(t, s, keyID(owner), "held"); held != 0 {
		t.Fatalf("a refused simulation pool holds %d", held)
	}
}

// usdc_per_agent_day counts by payout address too: two accounts paid to one
// wallet share one cap (C281).
func TestWorkStandingUSDCCapByPayoutAddress(t *testing.T) {
	s, _ := usdcStore(t)
	owner := keyFor(205)
	mintCredit(t, s, keyID(owner), allowance.Paid, 5000)
	id := rewardRequest(t, s, owner, "lobby")
	tiers := []any{map[string]any{"name": "bug", "credits": 100, "usdc": "0.40"}}
	run(t, s, standingCreate(s, owner, id, map[string]any{"seats": 3, "tiers": tiers, "usdc_per_agent_day": "0.40", "usdc_day_max": "2.00", "day_max": 3}, 10*86400))
	r1, c1 := standingClaim(t, s, keyFor(206), id, map[string]any{"payout_address": usdcPayee})
	run(t, s, c1)
	r2, c2 := standingClaim(t, s, keyFor(207), id, map[string]any{"payout_address": "0x" + strings.ToUpper(usdcPayee[2:])})
	run(t, s, c2)
	r3, c3 := standingClaim(t, s, keyFor(208), id, map[string]any{"payout_address": usdcOther})
	run(t, s, c3)
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: r1, Amount: 1}))
	if w := getTestWork(t, s, r1); w.RewardUSDC.State != "payable" {
		t.Fatalf("first result: %+v", w.RewardUSDC)
	}
	acked := run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: r2, Amount: 1})).Data["ack"].(WorkAck)
	if w := getTestWork(t, s, r2); w.RewardUSDC.State != "void" || w.RewardUSDC.Reason != "usdc_cap" || !strings.Contains(acked.Note, "by payout address") {
		t.Fatalf("same wallet, other account: %+v %q", w.RewardUSDC, acked.Note)
	}
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: r3, Amount: 1}))
	if w := getTestWork(t, s, r3); w.RewardUSDC.State != "payable" {
		t.Fatalf("another wallet: %+v", w.RewardUSDC)
	}
}

// C614: a rejected result frees the task's day_max slot at once, so a
// premature claim on a one-seat task cannot block it until 00:00 UTC; it
// still spends its worker's per_agent_day, it cannot be accepted after the
// reject, and an accepted result keeps its slot.
func TestWorkStandingRejectFreesDaySlot(t *testing.T) {
	s := rewardStore(t)
	owner, a, b, c := keyFor(210), keyFor(211), keyFor(212), keyFor(213)
	requester := keyID(owner)
	mintCredit(t, s, requester, allowance.Paid, 5000)
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, id, map[string]any{"seats": 1, "reward": 300}, 10*86400))
	if st := standingAt(t, s, id); st.DayMax != 1 || st.PerAgentDay != 1 || st.Open != 1 {
		t.Fatalf("one-seat task: %+v", st)
	}
	refusal := func(c Command) *Error {
		t.Helper()
		_, err := s.Execute(testContext, c, "test-origin")
		var e *Error
		if !errors.As(err, &e) {
			t.Fatalf("claim not refused: %v", err)
		}
		return e
	}
	listed := func() *WorkStanding {
		t.Helper()
		got, _ := run(t, s, Command{Operation: "works.list", Data: `{"schema":1,"standing":"` + id + `"}`}).Data["standing"].(*WorkStanding)
		return got
	}

	ra, ca := standingClaim(t, s, a, id, nil)
	run(t, s, ca)
	if st := standingAt(t, s, id); st.Open != 0 || st.OpenedToday != 1 || st.BlockedBy != StandingBlockedSeats {
		t.Fatalf("seat taken: %+v", st)
	}
	held := creditIn(t, s, requester, "held")

	// The requester rejects the premature claim: the seat, the reward and
	// the day's slot are free at once, on every read.
	run(t, s, workCommand(s, owner, Command{Operation: "work.reject", MessageID: ra, Amount: 1, Reason: "not yet at $5"}))
	st := standingAt(t, s, id)
	if st.Open != 1 || st.OpenedToday != 0 || st.Submitted != 0 || st.BlockedBy != "" || st.NextOpenAt != 0 {
		t.Fatalf("after reject: %+v", st)
	}
	if l := listed(); l == nil || !reflect.DeepEqual(*l, *st) {
		t.Fatalf("works.list standing %+v, want work.get's %+v", l, st)
	}
	if root := getTestWork(t, s, id); !root.Claimable {
		t.Fatalf("not claimable after reject: %+v", root.Standing)
	}
	if got := creditIn(t, s, requester, "held"); got != held {
		t.Fatalf("held %d after reject, want %d", got, held)
	}
	// A rejected result is terminal: no accept pays it afterwards.
	fails(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: ra, Amount: 1}), "work_state_conflict")

	// Its worker's own allowance stays spent for the day: A cannot retry,
	// and its own read says so.
	_, again := standingClaim(t, s, a, id, nil)
	if e := refusal(again); e.Code != "seat_limit" || e.Details.(map[string]any)["blocked_by"] != StandingBlockedPerAgentDay {
		t.Fatalf("A again: %v %#v", e, e.Details)
	}
	if w := run(t, s, signed(a, Command{Operation: "work.get", MessageID: id})).Data["work"].(Work); *w.Eligible || w.Standing.BlockedBy != StandingBlockedPerAgentDay {
		t.Fatalf("A's read: eligible %v %+v", *w.Eligible, w.Standing.WorkStandingSeats)
	}

	// Another agent claims the same day, and its accept keeps the slot.
	rb, cb := standingClaim(t, s, b, id, nil)
	run(t, s, cb)
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: rb, Amount: 1}))
	midnight := testTime - testTime%86400 + 86400
	st = standingAt(t, s, id)
	if st.Open != 0 || st.OpenedToday != 1 || st.Paid != 1 || st.BlockedBy != StandingBlockedDayMax || st.NextOpenAt != midnight {
		t.Fatalf("after accept: %+v", st)
	}
	_, cc := standingClaim(t, s, c, id, nil)
	if e := refusal(cc); e.Code != "seat_limit" || e.Details.(map[string]any)["blocked_by"] != StandingBlockedDayMax || e.NextOpenAt != midnight {
		t.Fatalf("C after the accept: %v %#v", e, e.Details)
	}
	// An accepted result keeps its slot even once hidden: a room that hides
	// it cannot reopen the day for a second payment.
	if _, err := s.db.Exec("UPDATE events SET hidden=1,hidden_by='room' WHERE id=?", rb); err != nil {
		t.Fatal(err)
	}
	if st = standingAt(t, s, id); st.OpenedToday != 1 || st.BlockedBy != StandingBlockedDayMax {
		t.Fatalf("after hiding the accepted result: %+v", st)
	}
	if err := s.Integrity(testContext); err != nil {
		t.Fatal(err)
	}
}

// A result whose reply is hidden holds no seat: its seat is free at once,
// its reward returns to the pool on the next claim or sweep (cancelled,
// reason hidden), and it is left out of the requester's record, since the
// requester cannot read it to judge it (C281).
func TestWorkStandingHiddenResultFreesSeat(t *testing.T) {
	s := rewardStore(t)
	owner, w1, w2, w3, w4 := keyFor(200), keyFor(201), keyFor(202), keyFor(203), keyFor(204)
	requester := keyID(owner)
	mintCredit(t, s, requester, allowance.Paid, 5000)
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, id, map[string]any{"seats": 2, "reward": 300, "day_max": 3}, 10*86400))
	hide := func(id string) {
		t.Helper()
		if _, err := s.db.Exec("UPDATE events SET hidden=1,hidden_by='room' WHERE id=?", id); err != nil {
			t.Fatal(err)
		}
	}
	state := func(id string) (string, string, string) {
		t.Helper()
		var w, r, reason string
		if err := s.db.QueryRow("SELECT w.state,r.state,r.reason FROM works w JOIN work_rewards r ON r.work_id=w.id WHERE w.id=?", id).Scan(&w, &r, &reason); err != nil {
			t.Fatal(err)
		}
		return w, r, reason
	}
	r1, c1 := standingClaim(t, s, w1, id, nil)
	run(t, s, c1)
	r2, c2 := standingClaim(t, s, w2, id, nil)
	run(t, s, c2)
	_, c3 := standingClaim(t, s, w3, id, nil)
	fails(t, s, c3, "seats_full")

	// The spam result is hidden: its seat and day slot are free at once.
	hide(r1)
	if st := standingAt(t, s, id); st.Submitted != 1 || st.Open != 1 || st.OpenedToday != 1 {
		t.Fatalf("after hiding a result: %+v", st)
	}
	// Its worker's own daily limit still counts it.
	_, again := standingClaim(t, s, w1, id, nil)
	fails(t, s, again, "seat_limit")
	r3, c3 := standingClaim(t, s, w3, id, nil)
	run(t, s, c3)
	if w, r, reason := state(r1); w != "cancelled" || r != "released" || reason != "hidden" {
		t.Fatalf("hidden result: %s %s %s", w, r, reason)
	}
	if held := creditIn(t, s, requester, "held"); held != 600 {
		t.Fatalf("held %d after the hidden result's reward returned, want 600", held)
	}

	// A result hidden and left to lapse: the sweeper cancels it as hidden.
	hide(r3)
	atTime(s, testTime+WorkStandingVerdictTTL+60)
	if _, err := s.SweepAllowance(testContext); err != nil {
		t.Fatal(err)
	}
	if w, r, reason := state(r3); w != "cancelled" || r != "released" || reason != "hidden" {
		t.Fatalf("hidden lapsed result: %s %s %s", w, r, reason)
	}
	if w, r, reason := state(r2); w != "submitted" || r != "released" || reason != "requester_lapsed" {
		t.Fatalf("lapsed result: %s %s %s", w, r, reason)
	}
	// Only the visible lapsed result counts on the record.
	root := getTestWork(t, s, id)
	if rec := root.RequesterRecord; rec == nil || rec.Results != 1 || rec.UnpaidLapsed != 1 {
		t.Fatalf("record: %+v", rec)
	}
	if held := creditIn(t, s, requester, "held"); held != 600 {
		t.Fatalf("held %d, want 600", held)
	}
	_, c4 := standingClaim(t, s, w4, id, nil)
	run(t, s, c4)
	if st := standingAt(t, s, id); st.Submitted != 1 || st.Open != 1 {
		t.Fatalf("after a new result: %+v", st)
	}
	if err := s.Integrity(testContext); err != nil {
		t.Fatal(err)
	}
}

// A hide on any version removes the message, so a result whose edit (the
// version the claim bound) is hidden holds no seat either: the seat is free
// at once and the next claim returns its reward, rather than a seat held
// for a result nobody can accept until it lapses (C408).
func TestWorkStandingHiddenEditFreesSeat(t *testing.T) {
	s := rewardStore(t)
	owner, worker := keyFor(205), keyFor(206)
	mintCredit(t, s, keyID(owner), allowance.Paid, 5000)
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, id, map[string]any{"seats": 1, "reward": 150}, 10*86400))
	reply := run(t, s, signed(worker, Command{Operation: "post", Room: "lobby", ReplyTo: id, Text: "Bug v1", Timestamp: s.now().Unix()})).Receipt.ID
	edit := run(t, s, signedNow(s, worker, Command{Operation: "post", Room: "lobby", ReplyTo: id, Text: "Bug v2 spam", Data: `{"schema":1,"supersedes":"` + reply + `"}`})).Receipt.ID
	run(t, s, workWith(s, worker, Command{Operation: "work.claim", MessageID: id, Target: reply}, nil))
	if st := standingAt(t, s, id); st.Submitted != 1 || st.Open != 0 {
		t.Fatalf("after the claim: %+v", st.WorkStandingSeats)
	}
	if _, err := s.db.Exec("UPDATE events SET hidden=1,hidden_by='room' WHERE id=?", edit); err != nil {
		t.Fatal(err)
	}
	if st := standingAt(t, s, id); st.Submitted != 0 || st.Open != 1 {
		t.Fatalf("after hiding the bound edit: %+v", st.WorkStandingSeats)
	}
	if w := getTestWork(t, s, id); !w.Claimable {
		t.Fatal("the freed seat is not claimable")
	}
	_, next := standingClaim(t, s, keyFor(207), id, nil)
	run(t, s, next)
	var state, reason string
	if err := s.db.QueryRow("SELECT w.state,r.reason FROM works w JOIN work_rewards r ON r.work_id=w.id WHERE w.id=?", reply).Scan(&state, &reason); err != nil {
		t.Fatal(err)
	}
	if state != "cancelled" || reason != "hidden" {
		t.Fatalf("hidden-edit result: %s %s", state, reason)
	}
	if err := s.Integrity(testContext); err != nil {
		t.Fatal(err)
	}
}

// USDC owed holds no seat: an accept frees its seat at once, and the USDC a
// requester owes unsettled on one standing task is capped at seats times the
// top tier's; past it an accept pays credits only, and a settle lowers it
// (C386).
func TestWorkStandingOwedUSDCFreesSeatAndCaps(t *testing.T) {
	s, chain := usdcStore(t)
	owner := keyFor(210)
	mintCredit(t, s, keyID(owner), allowance.Paid, 5000)
	id := rewardRequest(t, s, owner, "lobby")
	tiers := []any{map[string]any{"name": "bug", "credits": 100, "usdc": "0.40"}}
	run(t, s, standingCreate(s, owner, id, map[string]any{"seats": 2, "tiers": tiers, "day_max": 10}, 10*86400))
	if st := standingAt(t, s, id); st.USDCOwed != "0" || st.USDCOwedMax != "0.8" || st.Open != 2 {
		t.Fatalf("fresh: %+v", st)
	}
	accept := func(r string) WorkAck {
		t.Helper()
		return run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: r, Amount: 1})).Data["ack"].(WorkAck)
	}
	r1, c1 := standingClaim(t, s, keyFor(211), id, map[string]any{"payout_address": usdcPayee})
	run(t, s, c1)
	r2, c2 := standingClaim(t, s, keyFor(212), id, map[string]any{"payout_address": usdcOther})
	run(t, s, c2)
	accept(r1)
	accept(r2)
	st := standingAt(t, s, id)
	if st.Owed != 2 || st.Submitted != 0 || st.Open != 2 || st.NextOpenAt != 0 || st.USDCOwed != "0.8" || st.USDCOwedMax != "0.8" {
		t.Fatalf("both seats owed USDC: %+v", st)
	}
	if w := getTestWork(t, s, id); !w.Claimable {
		t.Fatal("a standing task whose seats are all owed USDC is not claimable")
	}

	// Every seat's USDC is owed, yet a claim succeeds; its accept would pass
	// the 0.80 owed cap, so it is paid in credits only.
	r3, c3 := standingClaim(t, s, keyFor(213), id, map[string]any{"payout_address": usdcOther})
	run(t, s, c3)
	acked := accept(r3)
	if !strings.Contains(acked.Note, "paid in credits only: 100 credits") || !strings.Contains(acked.Note, "unpaid USDC cap (0.8 USDC") || !strings.Contains(acked.Note, "0.8 already owed") {
		t.Fatalf("owed-cap note: %q", acked.Note)
	}
	if w := getTestWork(t, s, r3); w.Reward.State != "paid" || w.RewardUSDC.State != "void" || w.RewardUSDC.Reason != "usdc_cap" {
		t.Fatalf("over the owed cap: %+v %+v", w.Reward, w.RewardUSDC)
	}

	// Settling one lowers what is owed: the next accept carries USDC again.
	chain.add(txHash(40), fakeTx{status: "0x1", block: chain.head - 20, blockTime: s.now().Unix(), logs: []map[string]any{transferLog(usdcAsset(s), usdcOther, usdcPayee, 400_000)}})
	run(t, s, settle(s, owner, r1, txHash(40)))
	if st = standingAt(t, s, id); st.Owed != 1 || st.Paid != 2 || st.USDCOwed != "0.4" {
		t.Fatalf("after a settle: %+v", st)
	}
	r4, c4 := standingClaim(t, s, keyFor(214), id, map[string]any{"payout_address": usdcPayee})
	run(t, s, c4)
	if acked = accept(r4); strings.Contains(acked.Note, "credits only") {
		t.Fatalf("after a settle the accept still pays credits only: %q", acked.Note)
	}
	if w := getTestWork(t, s, r4); w.RewardUSDC.State != "payable" || w.RewardUSDC.Amount != "0.4" {
		t.Fatalf("after a settle: %+v", w.RewardUSDC)
	}
	if st = standingAt(t, s, id); st.USDCOwed != "0.8" || st.Open != 2 {
		t.Fatalf("owed again: %+v", st)
	}
	if err := s.Integrity(testContext); err != nil {
		t.Fatal(err)
	}
}

// A round reads its request from the standing root, so a moderator's hide on
// the root removes the request from every round too: work.get and the
// works.list excerpt carry none, while the round's own result stays readable.
func TestWorkStandingHiddenRootRemovesRoundRequest(t *testing.T) {
	s := rewardStore(t)
	owner, worker := keyFor(208), keyFor(209)
	mintCredit(t, s, keyID(owner), allowance.Paid, 5000)
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, id, map[string]any{"seats": 2, "reward": 100}, 10*86400))
	round, claim := standingClaim(t, s, worker, id, nil)
	run(t, s, claim)
	listedRequest := func() *WorkRequest {
		t.Helper()
		for _, w := range run(t, s, Command{Operation: "works.list", Room: "lobby", Data: `{"schema":1,"standing":"` + id + `"}`, Limit: 50}).Data["works"].([]Work) {
			if w.ID == round {
				return w.Request
			}
		}
		t.Fatal("the round is not listed")
		return nil
	}
	if r := getTestWork(t, s, round).Request; r == nil || r.Text != "Review my patch for credits" || r.VersionID != id {
		t.Fatalf("round request before the hide: %+v", r)
	}
	if r := listedRequest(); r == nil || r.Text != "Review my patch for credits" {
		t.Fatalf("listed round request before the hide: %+v", r)
	}
	if _, err := s.db.Exec("UPDATE events SET hidden=1,hidden_by='moderator' WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	w := getTestWork(t, s, round)
	if w.Request != nil {
		t.Fatalf("hidden standing request still shown on the round: %+v", w.Request)
	}
	if w.StandingID != id {
		t.Fatalf("round lost its standing link: %+v", w)
	}
	if r := listedRequest(); r != nil {
		t.Fatalf("hidden standing request still in the listing excerpt: %+v", r)
	}
}

// C585: a refusal while no seat is open says when the next one opens, or
// that none does before the task stops taking results.
func TestStandingNoSeatRefusal(t *testing.T) {
	const now, until = int64(1792886400 + 3600), int64(1793145600) // 2026-10-25T01:00Z
	err := errNoSeat(StandingBlockedDayMax, 1792972800, until, now, "it takes 5 results a day and has them today (day_max)", dayCause(1792972800, 1792972800))
	var e *Error
	want := "No seat is open on this task right now; the next one opens at 2026-10-26T00:00:00Z (daily limit): it takes 5 results a day and has them today (day_max)."
	if !errors.As(err, &e) || e.Code != "seat_limit" || e.Message != want || e.RetryAfter != 82800 || e.NextOpenAt != 1792972800 {
		t.Fatalf("daily limit: %+v", e)
	}
	if d := e.Details.(map[string]any); d["next_open_at"] != int64(1792972800) || d["blocked_by"] != StandingBlockedDayMax {
		t.Fatalf("details: %#v", e.Details)
	}
	err = errNoSeat(StandingBlockedSeats, 0, until, now, "all 2 seats hold a result waiting for a verdict", "")
	if !errors.As(err, &e) || e.Code != "seats_full" || !strings.Contains(e.Message, "none opens again before it stops taking results at 2026-10-28T00:00:00Z") || e.Details.(map[string]any)["next_open_at"] != nil || e.NextOpenAt != 0 || e.RetryAfter != 0 {
		t.Fatalf("no next opening: %+v", e)
	}
}
