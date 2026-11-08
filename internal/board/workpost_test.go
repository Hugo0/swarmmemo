package board

import (
	"crypto/ed25519"
	"encoding/json"
	"testing"

	"swarmmemo/internal/allowance"
)

// A request posted with data.work (C821) opens as work in the same call, on
// the real store and ledger: a refusal of any work rule refuses the post with
// the work's own problem and leaves no event, no work and no hold; a retry
// returns the same receipt; the two-call path is unchanged.

func postWork(s *Store, key ed25519.PrivateKey, text string, work map[string]any, ttl int64) Command {
	w := map[string]any{"schema": 1, "generation": s.generation, "title": "Rewarded review", "capabilities": []string{"review"}}
	for k, v := range work {
		w[k] = v
	}
	data, _ := json.Marshal(map[string]any{"schema": 1, "work": w})
	return signed(key, Command{Operation: "post", Room: "lobby", Kind: "request", Text: text + testPostSuffix(), Data: string(data), TTL: ttl, RequestID: randomID(), Timestamp: s.now().Unix()})
}

func TestPostedWorkOpensInOneCall(t *testing.T) {
	s := rewardStore(t)
	owner := keyFor(182)
	requester := keyID(owner)
	nothing := func(what string) {
		t.Helper()
		if n := sqlCount(t, s, "SELECT count(*) FROM events WHERE account=?", requester); n != 0 {
			t.Fatalf("%s: %d events published", what, n)
		}
		if n := sqlCount(t, s, "SELECT count(*) FROM works"); n != 0 {
			t.Fatalf("%s: %d works", what, n)
		}
		if held := creditIn(t, s, requester, "held"); held != 0 {
			t.Fatalf("%s: %d credits held", what, held)
		}
	}

	// Only free credit: the reward cannot be held, so nothing is posted.
	msg := failsWith(t, s, postWork(s, owner, "Review my patch", map[string]any{"reward": 50}, 0), "not_transferable")
	if msg != errWorkRewardNotTransferable().(*Error).Message {
		t.Fatalf("not_transferable: %q", msg)
	}
	nothing("not_transferable")
	mintCredit(t, s, requester, allowance.Paid, 5000)
	tiers := []any{map[string]any{"name": "bug", "credits": 300}}
	fails(t, s, postWork(s, owner, "Find bugs", map[string]any{"seats": 2, "reward": 300, "tiers": tiers}, 0), "invalid_work_data")
	nothing("reward and tiers")
	fails(t, s, postWork(s, owner, "Find bugs", map[string]any{"seats": 2, "tiers": tiers}, 3600), "invalid_ttl")
	nothing("standing ttl")
	fails(t, s, postWork(s, owner, "Find bugs", map[string]any{"bogus": 1}, 0), "invalid_work_data")
	nothing("unknown field")
	// ttl goes only with data.work.
	fails(t, s, signed(owner, Command{Operation: "post", Room: "lobby", Kind: "request", Text: "plain" + testPostSuffix(), TTL: 600, Timestamp: s.now().Unix()}), "unexpected_field")

	c := postWork(s, owner, "Review my patch", map[string]any{"reward": 600}, 86400)
	r := run(t, s, c)
	if r.Receipt == nil || r.Receipt.Work == nil || r.Receipt.Work.WorkID != r.Receipt.ID || r.Receipt.Work.State != "open" {
		t.Fatalf("receipt: %+v", r.Receipt)
	}
	if want := r.Receipt.AcceptedAt + 86400; r.Receipt.Work.Deadline != want {
		t.Fatalf("deadline %d, want %d", r.Receipt.Work.Deadline, want)
	}
	w := getTestWork(t, s, r.Receipt.ID)
	if w.State != "open" || w.Reward == nil || w.Reward.Amount != 600 || w.Reward.State != "held" {
		t.Fatalf("work: %+v reward %+v", w, w.Reward)
	}
	if held := creditIn(t, s, requester, "held"); held != 600 {
		t.Fatalf("held %d", held)
	}
	if op := sqlString(t, s, "SELECT operation FROM work_transitions WHERE work_id=? AND sequence=1", r.Receipt.ID); op != "work.create" {
		t.Fatalf("the create's transition is %q", op)
	}
	// An exact retry is the same receipt, nothing posted or held again.
	again := run(t, s, c)
	if !again.Receipt.Duplicate || again.Receipt.ID != r.Receipt.ID || again.Receipt.Work == nil || again.Receipt.Work.WorkID != r.Receipt.ID {
		t.Fatalf("retry: %+v", again.Receipt)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM works"); n != 1 || creditIn(t, s, requester, "held") != 600 {
		t.Fatalf("a retry made %d works", n)
	}

	// A standing task's signed create data is read back from the post.
	st := run(t, s, postWork(s, owner, "Find bugs", map[string]any{"seats": 2, "tiers": tiers, "per_agent_max": 3, "reward_note": "thanks"}, 0))
	if max, err := standingPerAgentMax(testContext, s.db, st.Receipt.ID); err != nil || max != 3 {
		t.Fatalf("per_agent_max %d %v", max, err)
	}
	if got := getTestWork(t, s, st.Receipt.ID); got.RewardNote != "thanks" {
		t.Fatalf("reward_note %q", got.RewardNote)
	}

	// The two-call path still works.
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, rewardCreate(s, owner, id, 40, 0))
	if got := getTestWork(t, s, id); got.Reward == nil || got.Reward.Amount != 40 {
		t.Fatalf("two-call work: %+v", got)
	}
}
