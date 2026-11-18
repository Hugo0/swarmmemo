package board

import (
	"crypto/ed25519"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
)

// C1076 (leo, round 481fbfae): a result posted while a standing task still
// took results stays claimable by its worker and acceptable by its requester
// until the deadline, after results_until; a result posted after it is
// refused both ways, and its reply's receipt says it can no longer be
// claimed.

// standingPastResultsUntil opens a 10-day standing task with two replies
// posted before results_until (neither claimed), then moves the clock an
// hour past results_until. It returns the task and the two replies.
func standingPastResultsUntil(t *testing.T, s *Store) (id, r1, r2 string) {
	t.Helper()
	OpenStandingDayForTest(t)
	owner, w1, w2 := keyFor(120), keyFor(121), keyFor(122)
	mintCredit(t, s, keyID(owner), allowance.Paid, 5000)
	id = rewardRequest(t, s, owner, "lobby")
	ttl := int64(10 * 86400)
	run(t, s, standingCreate(s, owner, id, map[string]any{"seats": 2, "reward": 200}, ttl))
	s.now = func() time.Time { return time.Unix(testTime+3600, 0) }
	r1 = replyTo(t, s, w1, id, "Bug: the export button 500s on empty rooms.")
	r2 = replyTo(t, s, w2, id, "Bug: the feed skips a page after a reload.")
	until := testTime + ttl - WorkStandingVerdictTTL
	if st := standingAt(t, s, id); st.ResultsUntil != until {
		t.Fatalf("results_until %d, want %d", st.ResultsUntil, until)
	}
	s.now = func() time.Time { return time.Unix(until+3600, 0) }
	return id, r1, r2
}

func TestStandingOldResultClaimableAfterResultsUntil(t *testing.T) {
	s := rewardStore(t)
	id, r1, _ := standingPastResultsUntil(t, s)
	ack := run(t, s, workWith(s, keyFor(121), Command{Operation: "work.claim", MessageID: id, Target: r1}, nil)).Data["ack"].(WorkAck)
	if ack.WorkID != r1 || ack.State != "submitted" {
		t.Fatalf("claim of an old result: %+v", ack)
	}
	// Its verdict deadline is the task's, not 72 hours on.
	if deadline := testTime + 10*86400; ack.Deadline != deadline {
		t.Fatalf("result deadline %d, want the task's %d", ack.Deadline, deadline)
	}
	acked := run(t, s, workCommand(s, keyFor(120), Command{Operation: "work.accept", MessageID: r1, Amount: 1})).Data["ack"].(WorkAck)
	if acked.State != "accepted" {
		t.Fatalf("accept: %+v", acked)
	}
}

func TestStandingOldResultAcceptableAfterResultsUntil(t *testing.T) {
	s := rewardStore(t)
	id, _, r2 := standingPastResultsUntil(t, s)
	before := creditIn(t, s, keyID(keyFor(122)), "remaining")
	ack := run(t, s, acceptReplyCmd(s, keyFor(120), id, r2, nil)).Data["ack"].(WorkAck)
	if ack.State != "accepted" || ack.WorkID != r2 {
		t.Fatalf("requester accept of an old result: %+v", ack)
	}
	if got := creditIn(t, s, keyID(keyFor(122)), "remaining") - before; got != 200 {
		t.Fatalf("author paid %d, want 200", got)
	}
}

func TestStandingNewResultRefusedAfterResultsUntil(t *testing.T) {
	s := rewardStore(t)
	id, _, _ := standingPastResultsUntil(t, s)
	w3 := keyFor(123)
	res := run(t, s, signed(w3, Command{Operation: "post", Room: "lobby", ReplyTo: id, Text: "Bug: late one " + randomID()[:8], Timestamp: s.now().Unix()}))
	r3 := res.Receipt.ID
	line := WorkHintLine("", res.Receipt)
	if res.Receipt.ReplyWork != "" || !strings.Contains(line, "can no longer be claimed") || strings.Contains(line, "work claim") {
		t.Fatalf("late reply's hint: %q (%+v)", line, res.Receipt)
	}
	fails(t, s, workWith(s, w3, Command{Operation: "work.claim", MessageID: id, Target: r3}, nil), "work_state_conflict")
	fails(t, s, acceptReplyCmd(s, keyFor(120), id, r3, nil), "work_state_conflict")
	// An anonymous late reply hears the same, not how to sign and claim it.
	anon := run(t, s, Command{Operation: "post", Room: "lobby", ReplyTo: id, Text: "Bug: anonymous late one " + randomID()[:8]})
	if line := WorkHintLine("", anon.Receipt); !strings.Contains(line, "can no longer be claimed") {
		t.Fatalf("anonymous late reply's hint: %q", line)
	}
}

// C1079: past results_until, a signed reply posted by then stays on its
// author's home as unclaimed_result, saying to claim by the deadline, until
// the deadline; a reply posted after results_until is never listed.
func TestHomeUnclaimedResultAfterResultsUntil(t *testing.T) {
	s := rewardStore(t)
	OpenStandingDayForTest(t)
	owner, early, other, late := keyFor(130), keyFor(131), keyFor(133), keyFor(132)
	mintCredit(t, s, keyID(owner), allowance.Paid, 5000)
	id := rewardRequest(t, s, owner, "lobby")
	ttl := int64(5 * 86400)
	run(t, s, standingCreate(s, owner, id, map[string]any{"seats": 2, "reward": 200}, ttl))
	deadline := testTime + ttl
	until := deadline - WorkStandingVerdictTTL
	unclaimed := func(key ed25519.PrivateKey) []HomeItem {
		t.Helper()
		var out []HomeItem
		for _, it := range run(t, s, signed(key, Command{Operation: "home", Timestamp: s.now().Unix()})).Data["items"].([]HomeItem) {
			if it.Kind == "unclaimed_result" {
				out = append(out, it)
			}
		}
		return out
	}
	s.now = func() time.Time { return time.Unix(testTime+3600, 0) }
	old := replyTo(t, s, early, id, "Bug: the export button 500s on empty rooms.")
	kept := replyTo(t, s, other, id, "Bug: search drops accents.")
	if u := unclaimed(early); len(u) != 1 || strings.Contains(u[0].Text, "result by ") {
		t.Fatalf("before results_until: %+v", u)
	}

	// Past results_until: the old reply is still listed, with the deadline.
	s.now = func() time.Time { return time.Unix(until+3600, 0) }
	u := unclaimed(early)
	if len(u) != 1 || u[0].Call["message_id"] != id || u[0].Call["target"] != old {
		t.Fatalf("old reply after results_until: %+v", u)
	}
	if by := "claim the task with it as your result by " + time.Unix(deadline, 0).UTC().Format(time.RFC3339); !strings.Contains(u[0].Text, by) {
		t.Fatalf("old reply's line %q, want %q", u[0].Text, by)
	}
	// A reply posted after results_until is not.
	replyTo(t, s, late, id, "Bug: the feed skips a page after a reload.")
	if u := unclaimed(late); len(u) != 0 {
		t.Fatalf("a reply after results_until is listed: %+v", u)
	}
	// Its call claims it.
	ack := run(t, s, workWith(s, early, Command{Operation: "work.claim", MessageID: id, Target: old}, nil)).Data["ack"].(WorkAck)
	if ack.WorkID != old || ack.State != "submitted" {
		t.Fatalf("claim from the home item: %+v", ack)
	}
	if u := unclaimed(early); len(u) != 0 {
		t.Fatalf("listed after its claim: %+v", u)
	}

	// Past the deadline nothing is listed: the other old reply, listed
	// until then, is gone.
	if u := unclaimed(other); len(u) != 1 || u[0].Call["target"] != kept {
		t.Fatalf("other old reply before the deadline: %+v", u)
	}
	s.now = func() time.Time { return time.Unix(deadline+3600, 0) }
	if u := unclaimed(other); len(u) != 0 {
		t.Fatalf("listed past the deadline: %+v", u)
	}
}
