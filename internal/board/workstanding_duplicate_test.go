package board

import (
	"crypto/ed25519"
	"errors"
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
)

// C615: a result on a standing task that repeats, word for word (whitespace
// aside), an earlier waiting or accepted result on that task is refused
// 409 duplicate_result, whoever sends it; a rejected one may be redone, a
// different text is taken, and other tasks are unchanged.

func standingReply(t *testing.T, s *Store, worker ed25519.PrivateKey, id, text string) (string, Command) {
	t.Helper()
	reply := run(t, s, signed(worker, Command{Operation: "post", Room: "lobby", ReplyTo: id, Text: text, Timestamp: s.now().Unix()})).Receipt.ID
	return reply, workWith(s, worker, Command{Operation: "work.claim", MessageID: id, Target: reply}, nil)
}

func TestWorkStandingDuplicateResult(t *testing.T) {
	s := rewardStore(t)
	owner, w1, w2, w3, w4, w5, w6 := keyFor(240), keyFor(241), keyFor(242), keyFor(243), keyFor(244), keyFor(245), keyFor(249)
	requester := keyID(owner)
	mintCredit(t, s, requester, allowance.Paid, 10000)
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, id, map[string]any{"seats": 5, "reward": 100, "day_max": 20}, 10*86400))

	original := "Checked https://example.com/status: 200 OK, notary receipt abc123."
	r1, claim1 := standingReply(t, s, w1, id, original)
	run(t, s, claim1)

	// A second key reposting it word for word, or with other whitespace, is refused.
	for i, copied := range []string{original, "  Checked   https://example.com/status:\n200 OK,\tnotary receipt abc123.  \n"} {
		_, claim := standingReply(t, s, []ed25519.PrivateKey{w2, w6}[i], id, copied)
		_, err := s.Execute(testContext, claim, "test-origin")
		var e *Error
		if !errors.As(err, &e) || e.Code != "duplicate_result" || e.Status != 409 || !strings.Contains(e.Message, r1) || e.Details.(map[string]any)["duplicate_of"] != r1 {
			t.Fatalf("copy %q: want 409 duplicate_result naming %s, got %v", copied, r1, err)
		}
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM works WHERE standing_id=?", id); n != 1 {
		t.Fatalf("refused copies opened tasks: %d results", n)
	}

	// The same receipt hash in a text of one's own is taken: only the text counts.
	r2, claim2 := standingReply(t, s, w2, id, "Checked https://example.com/status myself: 200 OK, notary receipt abc123.")
	run(t, s, claim2)

	// An accepted result still counts.
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: r1, Amount: 1}))
	_, claim3 := standingReply(t, s, w3, id, original)
	fails(t, s, claim3, "duplicate_result")

	// A rejected result does not: someone else may still do that check.
	run(t, s, workCommand(s, owner, Command{Operation: "work.reject", MessageID: r2, Amount: 1, Reason: "not reproducible"}))
	r4, claim4 := standingReply(t, s, w4, id, "Checked https://example.com/status myself: 200 OK, notary receipt abc123.")
	run(t, s, claim4)
	if w := getTestWork(t, s, r4); w.State != "submitted" || w.StandingID != id {
		t.Fatalf("copy of a rejected result: %+v", w)
	}

	// A result that only matches another task's result is taken.
	other := run(t, s, signed(owner, Command{Operation: "post", Room: "lobby", Kind: "request", Text: "Another standing check", Timestamp: s.now().Unix()})).Receipt.ID
	run(t, s, standingCreate(s, owner, other, map[string]any{"seats": 2, "reward": 100}, 10*86400))
	_, claim5 := standingReply(t, s, w5, other, original)
	run(t, s, claim5)
}

// A task that is not a standing one is unchanged: a worker's result may
// repeat the text another task's (or a rejected attempt's) result had.
func TestWorkDuplicateResultNotStanding(t *testing.T) {
	s := rewardStore(t)
	owner, w1, w2 := keyFor(246), keyFor(247), keyFor(248)
	requester := keyID(owner)
	mintCredit(t, s, requester, allowance.Paid, 5000)
	text := "Status page answers 200 OK."
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, rewardCreate(s, owner, id, 100, 0))
	ack := run(t, s, workCommand(s, w1, Command{Operation: "work.claim", MessageID: id, TTL: 600})).Data["ack"].(WorkAck)
	res1 := run(t, s, signed(w1, Command{Operation: "post", Room: "lobby", ReplyTo: id, Text: text, Timestamp: s.now().Unix()})).Receipt.ID
	run(t, s, workCommand(s, w1, Command{Operation: "work.submit", MessageID: id, Amount: ack.Fence, Target: res1}))
	run(t, s, workCommand(s, owner, Command{Operation: "work.reject", MessageID: id, Amount: ack.Fence, Reason: "no"}))

	res2 := run(t, s, signed(w2, Command{Operation: "post", Room: "lobby", ReplyTo: id, Text: text, Timestamp: s.now().Unix()})).Receipt.ID
	got := run(t, s, workCommand(s, w2, Command{Operation: "work.claim", MessageID: id, Target: res2})).Data["ack"].(WorkAck)
	if got.State != "submitted" {
		t.Fatalf("non-standing claim with a repeated text: %+v", got)
	}
}
