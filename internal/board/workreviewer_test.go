package board

import (
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
)

// A named reviewer on the real store, ledger and notary: it alone renders
// the verdict, is paid its fee once, is named on the reward's receipt, can
// never be the requester, and a silent reviewer releases every hold.

func reviewedCreate(s *Store, key ed25519.PrivateKey, id string, extra map[string]any, ttl int64) Command {
	d := map[string]any{"schema": 1, "generation": s.generation, "title": "Judged review", "capabilities": []string{"review"}}
	for k, v := range extra {
		d[k] = v
	}
	b, _ := json.Marshal(d)
	return signed(key, Command{Operation: "work.create", MessageID: id, TTL: ttl, Data: string(b), Timestamp: s.now().Unix()})
}

func TestWorkReviewerRendersTheVerdict(t *testing.T) {
	s := rewardStore(t)
	owner, worker, judge := keyFor(240), keyFor(241), keyFor(242)
	requester, payee, reviewer := keyID(owner), keyID(worker), keyID(judge)
	register(t, s, judge)
	mintCredit(t, s, requester, allowance.Paid, 1000)
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, reviewedCreate(s, owner, id, map[string]any{"reward": 300, "reviewer": reviewer, "reviewer_fee": 20}, 0))

	// The reviewer and its fee show before anyone claims.
	w := getTestWork(t, s, id)
	if w.Reviewer == nil || w.Reviewer.ID != reviewer || w.ReviewerFee == nil || w.ReviewerFee.Amount != 20 || w.ReviewerFee.State != "held" || w.Reward.State != "held" {
		t.Fatalf("reviewed work: reviewer %+v fee %+v", w.Reviewer, w.ReviewerFee)
	}
	if held := creditIn(t, s, requester, "held"); held != 320 {
		t.Fatalf("the ledger holds %d", held)
	}
	if list := run(t, s, Command{Operation: "works.list", Target: reviewer}).Data["works"].([]Work); len(list) != 1 || list[0].ID != id {
		t.Fatalf("the reviewer's work list: %+v", list)
	}
	// The reviewer cannot also be the worker.
	fails(t, s, workCommand(s, judge, Command{Operation: "work.claim", MessageID: id, TTL: 600}), "work_forbidden")

	fence := claimAndSubmit(t, s, worker, id, "lobby")
	// The requester gave up the verdict, and can no longer cancel.
	fails(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: id, Amount: fence}), "not_the_reviewer")
	fails(t, s, workCommand(s, owner, Command{Operation: "work.reject", MessageID: id, Amount: fence, Reason: "changed my mind"}), "not_the_reviewer")
	fails(t, s, workCommand(s, worker, Command{Operation: "work.accept", MessageID: id, Amount: fence}), "not_the_reviewer")
	fails(t, s, workCommand(s, owner, Command{Operation: "work.cancel", MessageID: id, Reason: "too late"}), "work_state_conflict")

	// The reviewer's journal shows the result waiting for its verdict.
	b, _, _ := journalOf(t, run(t, s, journalGet(judge, "", 0)))
	items := path(t, b, "open_work", "work", "items").([]any)
	if len(items) != 1 || path(t, items[0], "role") != "reviewer" {
		t.Fatalf("reviewer's open_work: %v", items)
	}

	run(t, s, workCommand(s, judge, Command{Operation: "work.accept", MessageID: id, Amount: fence}))
	if got := creditIn(t, s, payee, "remaining"); got != 300 {
		t.Fatalf("the worker received %d", got)
	}
	if got := creditIn(t, s, reviewer, "remaining"); got != 20 {
		t.Fatalf("the reviewer received %d", got)
	}
	if held := creditIn(t, s, requester, "held"); held != 0 {
		t.Fatalf("a settled work leaks a hold of %d", held)
	}
	w = getTestWork(t, s, id)
	if w.State != "accepted" || w.Reward.State != "paid" || w.ReviewerFee.State != "paid" || w.Reward.Receipt == nil {
		t.Fatalf("after the verdict: %s reward %+v fee %+v", w.State, w.Reward, w.ReviewerFee)
	}
	var st workRewardStatement
	if err := json.Unmarshal([]byte(w.Reward.Receipt.Statement), &st); err != nil || st.Reviewer != reviewer || st.Worker != payee || st.Amount != 300 {
		t.Fatalf("receipt statement: %+v %v", st, err)
	}
	if h := run(t, s, Command{Operation: "work.history", MessageID: id}).Data["reviewer_fee"].(*WorkReward); h.State != "paid" {
		t.Fatalf("work.history reviewer_fee: %+v", h)
	}
	if err := s.Integrity(testContext); err != nil {
		t.Fatal(err)
	}
}

func TestWorkReviewerRejectKeepsTheHold(t *testing.T) {
	s := rewardStore(t)
	owner, first, second, judge := keyFor(243), keyFor(244), keyFor(245), keyFor(246)
	requester, reviewer := keyID(owner), keyID(judge)
	register(t, s, judge)
	mintCredit(t, s, requester, allowance.Paid, 1000)
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, reviewedCreate(s, owner, id, map[string]any{"reward": 200, "reviewer": reviewer, "reviewer_fee": 10}, 0))

	fence := claimAndSubmit(t, s, first, id, "lobby")
	run(t, s, workCommand(s, judge, Command{Operation: "work.reject", MessageID: id, Amount: fence, Reason: "does not build"}))
	w := getTestWork(t, s, id)
	if w.State != "open" || w.Reward.State != "held" || w.ReviewerFee.State != "paid" || creditIn(t, s, requester, "held") != 200 {
		t.Fatalf("after the reject: %s reward %+v fee %+v", w.State, w.Reward, w.ReviewerFee)
	}
	if got := creditIn(t, s, reviewer, "remaining"); got != 10 {
		t.Fatalf("the reviewer received %d for its verdict", got)
	}
	// The next worker is judged by the same reviewer; the fee is paid once.
	fence = claimAndSubmit(t, s, second, id, "lobby")
	run(t, s, workCommand(s, judge, Command{Operation: "work.accept", MessageID: id, Amount: fence}))
	if got := creditIn(t, s, keyID(second), "remaining"); got != 200 {
		t.Fatalf("the second worker received %d", got)
	}
	if got := creditIn(t, s, keyID(first), "remaining"); got != 0 {
		t.Fatalf("the rejected worker received %d", got)
	}
	if got := creditIn(t, s, reviewer, "remaining"); got != 10 {
		t.Fatalf("the reviewer was paid twice: %d", got)
	}
	if err := s.Integrity(testContext); err != nil {
		t.Fatal(err)
	}
}

func TestWorkReviewerRefusals(t *testing.T) {
	s := rewardStore(t)
	owner, judge, outsider := keyFor(247), keyFor(248), keyFor(249)
	requester, reviewer := keyID(owner), keyID(judge)
	register(t, s, judge)
	mintCredit(t, s, requester, allowance.Paid, 1000)
	id := rewardRequest(t, s, owner, "lobby")

	// Not the requester's own key, nor a key linked to it either way.
	fails(t, s, reviewedCreate(s, owner, id, map[string]any{"reviewer": requester}, 0), "reviewer_is_requester")
	claimed := keyFor(250)
	register(t, s, claimed)
	run(t, s, linkCommand(owner, "identity.link", "ed25519", pubKey(claimed)))
	fails(t, s, reviewedCreate(s, owner, id, map[string]any{"reviewer": keyID(claimed)}, 0), "reviewer_is_requester")
	mirror := keyFor(251)
	register(t, s, mirror)
	run(t, s, linkCommand(mirror, "identity.link", "ed25519", pubKey(owner)))
	fails(t, s, reviewedCreate(s, owner, id, map[string]any{"reviewer": keyID(mirror)}, 0), "reviewer_is_requester")
	// An unknown agent, a malformed one, a fee without a reviewer.
	fails(t, s, reviewedCreate(s, owner, id, map[string]any{"reviewer": strings.Repeat("ab", 32)}, 0), "reviewer_not_found")
	fails(t, s, reviewedCreate(s, owner, id, map[string]any{"reviewer": "AB"}, 0), "invalid_work_data")
	fails(t, s, reviewedCreate(s, owner, id, map[string]any{"reviewer_fee": 5}, 0), "invalid_work_reward")
	fails(t, s, reviewedCreate(s, owner, id, map[string]any{"reviewer": reviewer, "reviewer_fee": 0}, 0), "invalid_work_reward")
	// A private room's reviewer must be able to read it.
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "judged-private", Visibility: "private", Members: []string{reviewer}}))
	register(t, s, outsider)
	private := rewardRequest(t, s, owner, "judged-private")
	fails(t, s, reviewedCreate(s, owner, private, map[string]any{"reviewer": keyID(outsider)}, 0), "reviewer_not_found")
	run(t, s, reviewedCreate(s, owner, private, map[string]any{"reviewer": reviewer}, 0))
	if n := sqlCount(t, s, "SELECT count(*) FROM works WHERE id=?", id); n != 0 {
		t.Fatalf("a refused reviewer left %d works", n)
	}
	// A reviewer is only named on create.
	run(t, s, reviewedCreate(s, owner, id, map[string]any{"reviewer": reviewer}, 0))
	claim := workCommand(s, outsider, Command{Operation: "work.claim", MessageID: id, TTL: 600})
	claim.Data = strings.Replace(claim.Data, `"schema":1`, `"schema":1,"reviewer":"`+requester+`"`, 1)
	claim.Nonce = ""
	fails(t, s, signed(outsider, claim), "invalid_work_data")
	// Before a claim the requester can still cancel; the fee-less reviewer
	// work releases nothing.
	run(t, s, workCommand(s, owner, Command{Operation: "work.cancel", MessageID: id, Reason: "not needed"}))
	if w := getTestWork(t, s, id); w.State != "cancelled" || w.Reviewer == nil || w.ReviewerFee != nil {
		t.Fatalf("cancelled: %+v", w)
	}
}

func TestWorkReviewLapseReleasesTheHolds(t *testing.T) {
	s := rewardStore(t)
	owner, worker, judge := keyFor(252), keyFor(253), keyFor(254)
	requester, payee, reviewer := keyID(owner), keyID(worker), keyID(judge)
	register(t, s, judge)
	mintCredit(t, s, requester, allowance.Paid, 1000)
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, reviewedCreate(s, owner, id, map[string]any{"reward": 300, "reviewer": reviewer, "reviewer_fee": 20}, 3600))
	// The same lifecycle without a reviewer, for contrast.
	plain := rewardRequest(t, s, owner, "lobby")
	run(t, s, rewardCreate(s, owner, plain, 100, 3600))
	fence := claimAndSubmit(t, s, worker, id, "lobby")
	claimAndSubmit(t, s, worker, plain, "lobby")
	before := creditIn(t, s, requester, "remaining")

	s.now = func() time.Time { return time.Unix(testTime+3600, 0) }
	if w := getTestWork(t, s, id); w.State != "review_lapsed" {
		t.Fatalf("a silent reviewer's work reads %s", w.State)
	}
	if w := getTestWork(t, s, plain); w.State != "expired" || w.Reviewer != nil {
		t.Fatalf("work without a reviewer reads %s", w.State)
	}
	fails(t, s, workCommand(s, judge, Command{Operation: "work.accept", MessageID: id, Amount: fence}), "work_state_conflict")
	if _, err := s.SweepAllowance(testContext); err != nil {
		t.Fatal(err)
	}
	w := getTestWork(t, s, id)
	if w.Reward.State != "released" || w.Reward.Reason != "review_lapsed" || w.ReviewerFee.State != "released" || w.ReviewerFee.Reason != "review_lapsed" {
		t.Fatalf("after the lapse: reward %+v fee %+v", w.Reward, w.ReviewerFee)
	}
	if r := getTestWork(t, s, plain).Reward; r.State != "released" || r.Reason != "expired" {
		t.Fatalf("plain work: %+v", r)
	}
	if held := creditIn(t, s, requester, "held"); held != 0 || creditIn(t, s, requester, "remaining") != before {
		t.Fatalf("the holds did not go back: held %d", held)
	}
	if creditIn(t, s, payee, "remaining") != 0 || creditIn(t, s, reviewer, "remaining") != 0 {
		t.Fatal("a lapsed review paid someone")
	}
	if list := run(t, s, Command{Operation: "works.list", Kind: "review_lapsed"}).Data["works"].([]Work); len(list) != 1 || list[0].ID != id {
		t.Fatalf("works.list review_lapsed: %+v", list)
	}
	if err := s.Integrity(testContext); err != nil {
		t.Fatal(err)
	}
}

// Without the ledger a reviewer still decides; only a fee needs credit.
func TestWorkReviewerWithoutTheLedger(t *testing.T) {
	if _, _, ok := ledgerTestOverride(); ok {
		t.Skip("the ledger mode is overridden for the whole suite")
	}
	s := openTest(t, Config{})
	owner, worker, judge := keyFor(235), keyFor(236), keyFor(237)
	register(t, s, judge)
	id := rewardRequest(t, s, owner, "lobby")
	fails(t, s, reviewedCreate(s, owner, id, map[string]any{"reviewer": keyID(judge), "reviewer_fee": 5}, 0), "service_unavailable")
	run(t, s, reviewedCreate(s, owner, id, map[string]any{"reviewer": keyID(judge)}, 0))
	fence := claimAndSubmit(t, s, worker, id, "lobby")
	fails(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: id, Amount: fence}), "not_the_reviewer")
	run(t, s, workCommand(s, judge, Command{Operation: "work.accept", MessageID: id, Amount: fence}))
	if w := getTestWork(t, s, id); w.State != "accepted" || w.Reward != nil || w.ReviewerFee != nil || w.Reviewer.ID != keyID(judge) {
		t.Fatalf("reviewed unpaid work: %+v", w)
	}
}
