package board

import (
	"errors"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
)

// C580: two rejected results on one standing task (or work item) within 7
// days pause that account's claims on it for 24 hours after the latest
// rejection; a key in the same burst cluster shares the count, an accept
// never counts, and the requester is untouched.

func claimRefusal(t *testing.T, s *Store, c Command) string {
	t.Helper()
	_, err := s.Execute(testContext, c, "test-origin")
	var e *Error
	if !errors.As(err, &e) || e.Code != "not_eligible" {
		t.Fatalf("want not_eligible, got %v", err)
	}
	return e.Message
}

func TestWorkClaimCooldownStanding(t *testing.T) {
	s := rewardStore(t)
	owner, w1, sibling, other := keyFor(201), keyFor(202), keyFor(203), keyFor(204)
	mintCredit(t, s, keyID(owner), allowance.Paid, 5000)
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, id, map[string]any{"seats": 5, "reward": 10, "per_agent_day": 5, "day_max": 50}, 20*86400))
	storeBurst(t, s, "", w1, sibling)
	at := func(sec int64) { s.now = func() time.Time { return time.Unix(testTime+sec, 0) } }

	// An accepted result never counts: other's two accepts leave it free.
	for i := range 2 {
		at(int64(60 + i*60))
		r, c := standingClaim(t, s, other, id, nil)
		run(t, s, c)
		run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: r, Amount: 1}))
	}

	// Two rejects of w1's results.
	var lastReject int64
	for i := range 2 {
		at(int64(3600 * (i + 1)))
		r, c := standingClaim(t, s, w1, id, nil)
		run(t, s, c)
		run(t, s, workCommand(s, owner, Command{Operation: "work.reject", MessageID: r, Amount: 1, Reason: "template, not a result"}))
		lastReject = s.now().Unix()
	}
	until := time.Unix(lastReject+WorkClaimCooldown, 0).UTC().Format(time.RFC3339)

	at(3 * 3600)
	_, c := standingClaim(t, s, w1, id, nil)
	msg := claimRefusal(t, s, c)
	if !strings.Contains(msg, "paused for you until "+until) || !strings.Contains(msg, "standing task") || !strings.Contains(msg, "rejected in the last 7 days") {
		t.Fatalf("refusal: %s", msg)
	}
	// The sibling key in the same burst cluster is paused too.
	_, c = standingClaim(t, s, sibling, id, nil)
	if msg := claimRefusal(t, s, c); !strings.Contains(msg, until) {
		t.Fatalf("sibling refusal: %s", msg)
	}
	// The read's preview says so before any claim.
	if w := run(t, s, Command{Operation: "work.get", MessageID: id, Target: keyID(w1)}).Data["work"].(Work); w.Eligible == nil || *w.Eligible || !strings.Contains(w.EligibleReason, until) {
		t.Fatalf("preview: eligible %v %q", w.Eligible, w.EligibleReason)
	}
	// An unlinked worker and the requester are untouched.
	r, c := standingClaim(t, s, other, id, nil)
	run(t, s, c)
	run(t, s, workCommand(s, owner, Command{Operation: "work.reject", MessageID: r, Amount: 1, Reason: "one reject only"}))
	at(3*3600 + 60)
	r, c = standingClaim(t, s, other, id, nil)
	run(t, s, c)
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: r, Amount: 1}))

	// 24 hours after the latest rejection it may claim again.
	at(lastReject - testTime + WorkClaimCooldown - 1)
	_, c = standingClaim(t, s, w1, id, nil)
	claimRefusal(t, s, c)
	at(lastReject - testTime + WorkClaimCooldown)
	_, c = standingClaim(t, s, w1, id, nil)
	run(t, s, c)
}

func TestWorkClaimCooldownWorkItem(t *testing.T) {
	s := openTest(t, Config{})
	owner, worker := keyFor(205), keyFor(206)
	id := createTestWork(t, s, owner, "lobby", "request", 20*86400)
	at := func(sec int64) { s.now = func() time.Time { return time.Unix(testTime+sec, 0) } }
	var lastReject int64
	for fence := int64(1); fence <= 2; fence++ {
		at(fence * 600)
		run(t, s, workCommand(s, worker, Command{Operation: "work.claim", MessageID: id, TTL: 300}))
		run(t, s, workCommand(s, owner, Command{Operation: "work.reject", MessageID: id, Amount: fence, Reason: "no"}))
		lastReject = s.now().Unix()
	}
	at(1800)
	msg := claimRefusal(t, s, workCommand(s, worker, Command{Operation: "work.claim", MessageID: id, TTL: 300}))
	if !strings.Contains(msg, "Claims on this task are paused") || !strings.Contains(msg, time.Unix(lastReject+WorkClaimCooldown, 0).UTC().Format(time.RFC3339)) {
		t.Fatalf("refusal: %s", msg)
	}
	// The requester still works its task.
	run(t, s, workCommand(s, owner, Command{Operation: "work.cancel", MessageID: id, Reason: "done"}))
	id = createTestWork(t, s, owner, "lobby", "request", 20*86400)
	run(t, s, workCommand(s, worker, Command{Operation: "work.claim", MessageID: id, TTL: 300}))
}

func TestWorkClaimCooldownExpires(t *testing.T) {
	s := openTest(t, Config{})
	owner, worker := keyFor(207), keyFor(208)
	id := createTestWork(t, s, owner, "lobby", "request", 20*86400)
	at := func(sec int64) { s.now = func() time.Time { return time.Unix(testTime+sec, 0) } }
	for fence := int64(1); fence <= 2; fence++ {
		at(fence * 600)
		run(t, s, workCommand(s, worker, Command{Operation: "work.claim", MessageID: id, TTL: 300}))
		run(t, s, workCommand(s, owner, Command{Operation: "work.reject", MessageID: id, Amount: fence, Reason: "no"}))
	}
	at(1200 + WorkClaimCooldown)
	run(t, s, workCommand(s, worker, Command{Operation: "work.claim", MessageID: id, TTL: 300}))
}

// C706: a requester's accept of a direct reply runs the claim for the
// reply's author, so the author's claim pause holds there too (the defence
// does not depend on who sends the claim), but the refusal is worded for the
// requester: same 403 not_eligible, the author's pause, the requester not
// paused, accept again after the time. The author's own claim keeps the
// worker's wording, and the same accept works once the pause ends.
func TestWorkClaimCooldownAcceptReplyWording(t *testing.T) {
	s := rewardStore(t)
	owner, worker := keyFor(209), keyFor(210)
	mintCredit(t, s, keyID(owner), allowance.Paid, 5000)
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, id, map[string]any{"seats": 5, "reward": 10, "per_agent_day": 5, "day_max": 50}, 20*86400))
	at := func(sec int64) { s.now = func() time.Time { return time.Unix(testTime+sec, 0) } }
	var lastReject int64
	for i := range 2 {
		at(int64(3600 * (i + 1)))
		r, c := standingClaim(t, s, worker, id, nil)
		run(t, s, c)
		run(t, s, workCommand(s, owner, Command{Operation: "work.reject", MessageID: r, Amount: 1, Reason: "template"}))
		lastReject = s.now().Unix()
	}
	until := time.Unix(lastReject+WorkClaimCooldown, 0).UTC().Format(time.RFC3339)

	at(3 * 3600)
	reply := replyTo(t, s, worker, id, "A real result this time")
	msg := claimRefusal(t, s, acceptReplyCmd(s, owner, id, reply, nil))
	for _, want := range []string{"The reply's author is paused on this standing task until " + until, "Your account is not paused", "send the same accept after " + until} {
		if !strings.Contains(msg, want) {
			t.Fatalf("requester refusal lacks %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, "paused for you") {
		t.Fatalf("requester refusal worded for the worker: %s", msg)
	}
	// The worker's own claim keeps the worker's wording.
	_, c := standingClaim(t, s, worker, id, nil)
	if msg := claimRefusal(t, s, c); !strings.Contains(msg, "Claims on this standing task are paused for you until "+until) {
		t.Fatalf("worker refusal: %s", msg)
	}
	// After the pause the same accept goes through.
	at(lastReject - testTime + WorkClaimCooldown)
	if ack := run(t, s, acceptReplyCmd(s, owner, id, reply, nil)).Data["ack"].(WorkAck); ack.State != "accepted" {
		t.Fatalf("accept after pause: %+v", ack)
	}
}
