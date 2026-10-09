package board

import (
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
)

// work.reviewer.set on the real store and ledger: the requester changes the
// named reviewer while the work is open or claimed; the held fee follows
// the reviewer; work.history records the old and the new.

// reviewerSet is the requester's signed work.reviewer.set naming reviewer.
func reviewerSet(s *Store, key ed25519.PrivateKey, id, reviewer string) Command {
	b, _ := json.Marshal(map[string]any{"schema": 1, "generation": s.generation, "reviewer": reviewer})
	return signed(key, Command{Operation: WorkReviewerSet, MessageID: id, Data: string(b), Timestamp: s.now().Unix()})
}

// The new reviewer decides and is paid the held fee, the old one cannot
// decide; once a result waits for a verdict the change is refused.
func TestWorkReviewerChange(t *testing.T) {
	s := rewardStore(t)
	owner, worker, oldJudge, newJudge, third := keyFor(150), keyFor(151), keyFor(152), keyFor(153), keyFor(154)
	requester, payee, oldReviewer, newReviewer, thirdReviewer := keyID(owner), keyID(worker), keyID(oldJudge), keyID(newJudge), keyID(third)
	for _, k := range []ed25519.PrivateKey{oldJudge, newJudge, third} {
		register(t, s, k)
	}
	mintCredit(t, s, requester, allowance.Paid, 1000)
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, reviewedCreate(s, owner, id, map[string]any{"reward": 300, "reviewer": oldReviewer, "reviewer_fee": 20}, 0))

	// Only the requester; never itself or an unknown agent; data names the
	// reviewer and nothing a create takes besides.
	fails(t, s, reviewerSet(s, worker, id, newReviewer), "work_forbidden")
	fails(t, s, reviewerSet(s, owner, id, requester), "reviewer_is_requester")
	fails(t, s, reviewerSet(s, owner, id, strings.Repeat("ab", 32)), "reviewer_not_found")
	b, _ := json.Marshal(map[string]any{"schema": 1, "generation": s.generation})
	fails(t, s, signed(owner, Command{Operation: WorkReviewerSet, MessageID: id, Data: string(b), Timestamp: s.now().Unix()}), "invalid_work_data")
	b, _ = json.Marshal(map[string]any{"schema": 1, "generation": s.generation, "reviewer": newReviewer, "reward": 5})
	fails(t, s, signed(owner, Command{Operation: WorkReviewerSet, MessageID: id, Data: string(b), Timestamp: s.now().Unix()}), "invalid_work_data")

	// Open: changed. Claimed: changed again, never to the worker.
	run(t, s, reviewerSet(s, owner, id, thirdReviewer))
	ack := run(t, s, workCommand(s, worker, Command{Operation: "work.claim", MessageID: id, TTL: 600})).Data["ack"].(WorkAck)
	fails(t, s, reviewerSet(s, owner, id, payee), "work_forbidden")
	run(t, s, reviewerSet(s, owner, id, newReviewer))
	w := getTestWork(t, s, id)
	if w.Reviewer == nil || w.Reviewer.ID != newReviewer || w.ReviewerFee == nil || w.ReviewerFee.State != "held" || w.ReviewerFee.Amount != 20 || w.State != "claimed" || w.Fence != ack.Fence {
		t.Fatalf("after the change: reviewer %+v fee %+v state %s fence %d", w.Reviewer, w.ReviewerFee, w.State, w.Fence)
	}
	if held := creditIn(t, s, requester, "held"); held != 320 {
		t.Fatalf("the escrow moved: held %d", held)
	}

	result := workResult(t, s, worker, id, "lobby")
	run(t, s, workCommand(s, worker, Command{Operation: "work.submit", MessageID: id, Amount: ack.Fence, Target: result}))
	// A result waits for a verdict: no change now.
	fails(t, s, reviewerSet(s, owner, id, oldReviewer), "work_state_conflict")
	// The old reviewer cannot decide; the new one does and is paid the fee.
	fails(t, s, workCommand(s, oldJudge, Command{Operation: "work.accept", MessageID: id, Amount: ack.Fence}), "not_the_reviewer")
	run(t, s, workCommand(s, newJudge, Command{Operation: "work.accept", MessageID: id, Amount: ack.Fence}))
	if got := creditIn(t, s, newReviewer, "remaining"); got != 20 {
		t.Fatalf("the new reviewer received %d", got)
	}
	if got := creditIn(t, s, oldReviewer, "remaining"); got != 0 {
		t.Fatalf("the replaced reviewer received %d", got)
	}
	if got := creditIn(t, s, payee, "remaining"); got != 300 {
		t.Fatalf("the worker received %d", got)
	}
	fails(t, s, reviewerSet(s, owner, id, oldReviewer), "work_state_conflict")

	// History: each change with the old and new reviewer; the new
	// reviewer's accept is no fallback verdict.
	var changes [][2]string
	for _, tr := range run(t, s, Command{Operation: "work.history", MessageID: id}).Data["transitions"].([]WorkTransition) {
		if tr.Operation == WorkReviewerSet {
			changes = append(changes, [2]string{tr.PreviousReviewer, tr.Reviewer})
		}
		if tr.Operation == "work.accept" && tr.Note != "" {
			t.Fatalf("the new reviewer's accept has note %q", tr.Note)
		}
	}
	if len(changes) != 2 || changes[0] != [2]string{oldReviewer, thirdReviewer} || changes[1] != [2]string{thirdReviewer, newReviewer} {
		t.Fatalf("history reviewer changes: %v", changes)
	}
	// A page that starts after the first change still knows the reviewer
	// in force: create, set, claim | set, submit, accept.
	page := run(t, s, Command{Operation: "work.history", MessageID: id, Limit: 3})
	if page.NextCursor == "" {
		t.Fatal("no second history page")
	}
	seen := 0
	for _, tr := range run(t, s, Command{Operation: "work.history", MessageID: id, Cursor: page.NextCursor}).Data["transitions"].([]WorkTransition) {
		if tr.Operation == WorkReviewerSet {
			seen++
			if tr.PreviousReviewer != thirdReviewer || tr.Reviewer != newReviewer {
				t.Fatalf("second page: %+v", tr)
			}
		}
		if tr.Operation == "work.accept" && tr.Note != "" {
			t.Fatalf("second page accept note %q", tr.Note)
		}
	}
	if seen != 1 {
		t.Fatalf("second page shows %d reviewer changes", seen)
	}
	if err := s.Integrity(testContext); err != nil {
		t.Fatal(err)
	}
}

// Work without a reviewer may get one while open: the verdict moves from
// the requester to it. A reject reopens the work, and the reviewer may
// change again before the next claim; neither reviewer's verdict carries
// the silent-reviewer note.
func TestWorkReviewerAddedAndChangedAfterReject(t *testing.T) {
	s := rewardStore(t)
	owner, worker, first, second := keyFor(155), keyFor(156), keyFor(157), keyFor(158)
	register(t, s, first)
	register(t, s, second)
	id := createTestWork(t, s, owner, "lobby", "request", 0)
	run(t, s, reviewerSet(s, owner, id, keyID(first)))
	fence := claimAndSubmit(t, s, worker, id, "lobby")
	fails(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: id, Amount: fence}), "not_the_reviewer")
	run(t, s, workCommand(s, first, Command{Operation: "work.reject", MessageID: id, Amount: fence, Reason: "incomplete"}))
	run(t, s, reviewerSet(s, owner, id, keyID(second)))
	fence = claimAndSubmit(t, s, worker, id, "lobby")
	fails(t, s, workCommand(s, first, Command{Operation: "work.accept", MessageID: id, Amount: fence}), "not_the_reviewer")
	run(t, s, workCommand(s, second, Command{Operation: "work.accept", MessageID: id, Amount: fence}))
	changes := 0
	for _, tr := range run(t, s, Command{Operation: "work.history", MessageID: id}).Data["transitions"].([]WorkTransition) {
		if tr.Note != "" {
			t.Fatalf("%s by the reviewer in force has note %q", tr.Operation, tr.Note)
		}
		if tr.Operation == WorkReviewerSet {
			changes++
			if changes == 1 && (tr.PreviousReviewer != "" || tr.Reviewer != keyID(first)) || changes == 2 && (tr.PreviousReviewer != keyID(first) || tr.Reviewer != keyID(second)) {
				t.Fatalf("change %d: %+v", changes, tr)
			}
		}
	}
	if changes != 2 {
		t.Fatalf("%d reviewer changes in history", changes)
	}
}
