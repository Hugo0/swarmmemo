package board

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
)

// A named reviewer silent ReviewerSilenceDays after a submit lets the
// requester decide in its place, before the deadline, on the real store,
// ledger and notary: the reward moves as usual, the reviewer fee goes back
// to the requester, the history notes the fallback, and the receipt still
// names the reviewer.

func atTime(s *Store, unix int64) { s.now = func() time.Time { return time.Unix(unix, 0) } }

func failsWith(t *testing.T, s *Store, c Command, code string) string {
	t.Helper()
	_, err := s.Execute(testContext, c, "test-origin")
	var e *Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
	return e.Message
}

func TestWorkSilentReviewerRequesterAccepts(t *testing.T) {
	s := rewardStore(t)
	owner, worker, judge, outsider := keyFor(230), keyFor(231), keyFor(232), keyFor(233)
	requester, payee, reviewer := keyID(owner), keyID(worker), keyID(judge)
	register(t, s, judge)
	register(t, s, outsider)
	mintCredit(t, s, requester, allowance.Paid, 1000)
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, reviewedCreate(s, owner, id, map[string]any{"reward": 300, "reviewer": reviewer, "reviewer_fee": 20}, 0))
	if w := getTestWork(t, s, id); w.RequesterMayDecideAt != 0 {
		t.Fatalf("open work shows requester_may_decide_at %d", w.RequesterMayDecideAt)
	}
	fence := claimAndSubmit(t, s, worker, id, "lobby")
	submitted := s.now().Unix()
	at := submitted + ReviewerSilenceDays*86400
	if w := getTestWork(t, s, id); w.RequesterMayDecideAt != at {
		t.Fatalf("requester_may_decide_at %d, want %d", w.RequesterMayDecideAt, at)
	}

	// Before the reviewer has been silent 3 days the requester is refused,
	// told when it may decide.
	atTime(s, at-1)
	msg := failsWith(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: id, Amount: fence}), "not_the_reviewer")
	if !strings.Contains(msg, time.Unix(at, 0).UTC().Format(time.RFC3339)) {
		t.Fatalf("the refusal does not say when: %s", msg)
	}
	before := creditIn(t, s, requester, "remaining")

	// Only the requester gains the verdict; anyone else is still refused.
	atTime(s, at)
	fails(t, s, workCommand(s, outsider, Command{Operation: "work.accept", MessageID: id, Amount: fence}), "not_the_reviewer")
	fails(t, s, workCommand(s, worker, Command{Operation: "work.accept", MessageID: id, Amount: fence}), "not_the_reviewer")
	b, _, _ := journalOf(t, run(t, s, signed(owner, Command{Operation: "journal.get", Timestamp: s.now().Unix()})))
	if items := path(t, b, "open_work", "work", "items").([]any); len(items) != 1 || !strings.Contains(path(t, items[0], "next").(string), "silent") {
		t.Fatalf("requester's open_work: %v", items)
	}
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: id, Amount: fence}))

	if got := creditIn(t, s, payee, "remaining"); got != 300 {
		t.Fatalf("the worker received %d", got)
	}
	if got := creditIn(t, s, reviewer, "remaining"); got != 0 {
		t.Fatalf("the silent reviewer received %d", got)
	}
	// remaining counts held credit: only the reward left, the fee came back.
	if got := creditIn(t, s, requester, "remaining"); got != before-300 {
		t.Fatalf("the requester has %d, want the fee back: %d", got, before-300)
	}
	if held := creditIn(t, s, requester, "held"); held != 0 {
		t.Fatalf("a settled work leaks a hold of %d", held)
	}
	w := getTestWork(t, s, id)
	if w.State != "accepted" || w.Reward.State != "paid" || w.ReviewerFee.State != "released" || w.ReviewerFee.Reason != "reviewer_silent" || w.RequesterMayDecideAt != 0 {
		t.Fatalf("after the fallback: %s reward %+v fee %+v", w.State, w.Reward, w.ReviewerFee)
	}
	// The receipt still names the reviewer, and the requester key that decided.
	if w.Reward.Receipt == nil {
		t.Fatal("no receipt")
	}
	var st workRewardStatement
	if err := json.Unmarshal([]byte(w.Reward.Receipt.Statement), &st); err != nil || st.Reviewer != reviewer || st.DecidedBy != requester || st.Worker != payee || st.Amount != 300 {
		t.Fatalf("receipt statement: %+v %v", st, err)
	}
	// The history notes the fallback on the verdict only.
	h := run(t, s, Command{Operation: "work.history", MessageID: id}).Data
	transitions := h["transitions"].([]WorkTransition)
	last := transitions[len(transitions)-1]
	if last.Operation != "work.accept" || last.Note != WorkReviewerSilentNote || last.Note != "reviewer silent 3 days; requester decided" {
		t.Fatalf("the fallback transition: %+v", last)
	}
	for _, tr := range transitions[:len(transitions)-1] {
		if tr.Note != "" {
			t.Fatalf("transition %s has note %q", tr.Operation, tr.Note)
		}
	}
	if fee := h["reviewer_fee"].(*WorkReward); fee.State != "released" || fee.Reason != "reviewer_silent" {
		t.Fatalf("work.history reviewer_fee: %+v", fee)
	}
	// The reviewer comes too late now.
	fails(t, s, workCommand(s, judge, Command{Operation: "work.accept", MessageID: id, Amount: fence}), "work_state_conflict")
	if err := s.Integrity(testContext); err != nil {
		t.Fatal(err)
	}
}

func TestWorkSilentReviewerReviewerStillDecides(t *testing.T) {
	s := rewardStore(t)
	owner, worker, judge := keyFor(234), keyFor(235), keyFor(236)
	requester, payee, reviewer := keyID(owner), keyID(worker), keyID(judge)
	register(t, s, judge)
	mintCredit(t, s, requester, allowance.Paid, 1000)
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, reviewedCreate(s, owner, id, map[string]any{"reward": 300, "reviewer": reviewer, "reviewer_fee": 20}, 0))
	fence := claimAndSubmit(t, s, worker, id, "lobby")
	// Past the silence window, the reviewer may still decide first, and is paid.
	atTime(s, s.now().Unix()+ReviewerSilenceDays*86400+60)
	run(t, s, workCommand(s, judge, Command{Operation: "work.accept", MessageID: id, Amount: fence}))
	if creditIn(t, s, payee, "remaining") != 300 || creditIn(t, s, reviewer, "remaining") != 20 {
		t.Fatal("the reviewer's verdict did not pay the worker and the reviewer")
	}
	w := getTestWork(t, s, id)
	if w.ReviewerFee.State != "paid" {
		t.Fatalf("fee %+v", w.ReviewerFee)
	}
	var st workRewardStatement
	if err := json.Unmarshal([]byte(w.Reward.Receipt.Statement), &st); err != nil || st.Reviewer != reviewer || st.DecidedBy != "" {
		t.Fatalf("receipt statement: %+v %v", st, err)
	}
	for _, tr := range run(t, s, Command{Operation: "work.history", MessageID: id}).Data["transitions"].([]WorkTransition) {
		if tr.Note != "" {
			t.Fatalf("a reviewer's verdict has note %q", tr.Note)
		}
	}
	fails(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: id, Amount: fence}), "work_state_conflict")
}

func TestWorkSilentReviewerRequesterRejects(t *testing.T) {
	s := rewardStore(t)
	owner, first, second, judge := keyFor(237), keyFor(238), keyFor(239), keyFor(240)
	requester, reviewer := keyID(owner), keyID(judge)
	register(t, s, judge)
	mintCredit(t, s, requester, allowance.Paid, 1000)
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, reviewedCreate(s, owner, id, map[string]any{"reward": 200, "reviewer": reviewer, "reviewer_fee": 10}, 0))
	fence := claimAndSubmit(t, s, first, id, "lobby")
	atTime(s, s.now().Unix()+ReviewerSilenceDays*86400)
	run(t, s, workCommand(s, owner, Command{Operation: "work.reject", MessageID: id, Amount: fence, Reason: "wrong file"}))
	w := getTestWork(t, s, id)
	if w.State != "open" || w.Reward.State != "held" || w.ReviewerFee.State != "released" || w.ReviewerFee.Reason != "reviewer_silent" || creditIn(t, s, requester, "held") != 200 {
		t.Fatalf("after the fallback reject: %s reward %+v fee %+v", w.State, w.Reward, w.ReviewerFee)
	}
	if creditIn(t, s, reviewer, "remaining") != 0 {
		t.Fatal("the silent reviewer was paid")
	}
	transitions := run(t, s, Command{Operation: "work.history", MessageID: id}).Data["transitions"].([]WorkTransition)
	if last := transitions[len(transitions)-1]; last.Operation != "work.reject" || last.Note != WorkReviewerSilentNote {
		t.Fatalf("the fallback reject: %+v", last)
	}
	// The next submit restarts the reviewer's window: the requester is refused again.
	fence = claimAndSubmit(t, s, second, id, "lobby")
	fails(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: id, Amount: fence}), "not_the_reviewer")
	run(t, s, workCommand(s, judge, Command{Operation: "work.accept", MessageID: id, Amount: fence}))
	if creditIn(t, s, keyID(second), "remaining") != 200 || creditIn(t, s, reviewer, "remaining") != 0 {
		t.Fatal("the second verdict paid the wrong accounts")
	}
	if err := s.Integrity(testContext); err != nil {
		t.Fatal(err)
	}
}

func TestWorkSilentReviewerDeadlineStillLapses(t *testing.T) {
	s := rewardStore(t)
	owner, worker, judge := keyFor(241), keyFor(242), keyFor(243)
	requester, reviewer := keyID(owner), keyID(judge)
	register(t, s, judge)
	mintCredit(t, s, requester, allowance.Paid, 1000)
	start := s.now().Unix()
	// A deadline 4 days out: the fallback opens after 3, then the deadline lapses it.
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, reviewedCreate(s, owner, id, map[string]any{"reward": 300, "reviewer": reviewer, "reviewer_fee": 20}, 4*86400))
	// A deadline under 3 days away: the requester never gains the verdict.
	short := rewardRequest(t, s, owner, "lobby")
	run(t, s, reviewedCreate(s, owner, short, map[string]any{"reward": 100, "reviewer": reviewer}, 2*86400))
	fence := claimAndSubmit(t, s, worker, id, "lobby")
	shortFence := claimAndSubmit(t, s, worker, short, "lobby")
	if w := getTestWork(t, s, short); w.State != "submitted" || w.RequesterMayDecideAt != 0 {
		t.Fatalf("short work: %s requester_may_decide_at %d", w.State, w.RequesterMayDecideAt)
	}
	atTime(s, start+2*86400-60)
	msg := failsWith(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: short, Amount: shortFence}), "not_the_reviewer")
	if strings.Contains(msg, "silent") {
		t.Fatalf("a deadline inside the window still offers the fallback: %s", msg)
	}

	atTime(s, start+4*86400)
	if w := getTestWork(t, s, id); w.State != "review_lapsed" || w.RequesterMayDecideAt != 0 {
		t.Fatalf("at the deadline: %s requester_may_decide_at %d", w.State, w.RequesterMayDecideAt)
	}
	fails(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: id, Amount: fence}), "work_state_conflict")
	fails(t, s, workCommand(s, owner, Command{Operation: "work.reject", MessageID: id, Amount: fence, Reason: "late"}), "work_state_conflict")
	if _, err := s.SweepAllowance(testContext); err != nil {
		t.Fatal(err)
	}
	w := getTestWork(t, s, id)
	if w.Reward.State != "released" || w.Reward.Reason != "review_lapsed" || w.ReviewerFee.State != "released" || w.ReviewerFee.Reason != "review_lapsed" {
		t.Fatalf("after the lapse: reward %+v fee %+v", w.Reward, w.ReviewerFee)
	}
	if creditIn(t, s, keyID(worker), "remaining") != 0 || creditIn(t, s, reviewer, "remaining") != 0 {
		t.Fatal("a lapsed review paid someone")
	}
	if err := s.Integrity(testContext); err != nil {
		t.Fatal(err)
	}
}

// C81: work.create on a signed root of another kind names its kind and
// says how to fix it; the code and status stay invalid_work_root, 400.
func TestWorkCreateWrongKindSaysWhich(t *testing.T) {
	s := openTest(t, Config{})
	owner := keyFor(244)
	note := run(t, s, signed(owner, Command{Operation: "post", Kind: "offer", Text: "I can review Go"})).Receipt.ID
	_, err := s.Execute(testContext, workCommand(s, owner, Command{Operation: "work.create", MessageID: note}), "test-origin")
	var e *Error
	if !errors.As(err, &e) || e.Code != "invalid_work_root" || e.Status != 400 {
		t.Fatalf("got %v", err)
	}
	if want := "work.create needs a signed root post of kind request; this post is an offer. Post the task again with kind request."; e.Message != want {
		t.Fatalf("message %q", e.Message)
	}
}
