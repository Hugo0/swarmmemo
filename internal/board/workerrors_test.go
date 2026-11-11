package board

import (
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/ledger"
)

// First-session refusals say what to do in plain words (cold-start
// evaluation 2026-10-21), with their codes unchanged: a reply in the wrong
// room names the right one; a new key's free credit cannot fund a reward,
// and the refusal says why and what can; a verdict whose payment the
// worker cannot receive today tells the requester the work stays
// submitted and when to send it again.
func TestWorkAndReplyRefusalsSayWhatToDo(t *testing.T) {
	s := rewardStore(t)
	// The unsponsored refusal (worksponsor_test.go covers sponsorship).
	setSponsorParams(t, s, `{"schema":1,"task_reward_max":0,"task_daily_per_identity":1,"task_daily_total":2000}`)
	owner, worker := keyFor(160), keyFor(161)
	run(t, s, signed(owner, Command{Operation: "agent.register"}))
	run(t, s, signed(worker, Command{Operation: "agent.register"}))

	root := run(t, s, signed(owner, Command{Operation: "post", Room: "lobby", Text: "a question", Timestamp: s.now().Unix()})).Receipt.ID
	msg := failsWith(t, s, signed(worker, Command{Operation: "post", Room: "bounties", Text: "an answer", ReplyTo: root, Timestamp: s.now().Unix()}), "invalid_reply")
	if !strings.Contains(msg, "room lobby") || !strings.Contains(msg, "POST /w/lobby/main") {
		t.Fatalf("invalid_reply: %q", msg)
	}

	// Free credit only: the reward cannot be held.
	id := rewardRequest(t, s, owner, "lobby")
	if msg = failsWith(t, s, rewardCreate(s, owner, id, 50, 0), "not_transferable"); msg != errWorkRewardNotTransferable().(*Error).Message {
		t.Fatalf("not_transferable: %q", msg)
	}

	// Lasting credit funds it; the worker's inbound cap refuses the accept.
	mintCredit(t, s, keyID(owner), allowance.Paid, 1000)
	run(t, s, rewardCreate(s, owner, id, 50, 0))
	fence := claimAndSubmit(t, s, worker, id, "lobby")
	p := s.allowanceDefaults()
	rp := p.Resources[allowance.Credit]
	rp.Budget, rp.SpendCeiling, rp.InboundCap, rp.TransferFee, rp.GrantSharePPM = 100_000, 100_000, 10, 2, 500_000
	rp.Floor, rp.Cap, rp.FloorSlicePPM, rp.AnonymousSharePPM = 100, 1000, 1_000_000, 100
	if _, err := s.SetAllowanceParams(testContext, ledger.AllowanceNamespace, p.Marshal(), "inbound cap test", 0); err != nil {
		t.Fatal(err)
	}
	msg = failsWith(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: id, Amount: fence}), "recipient_limit")
	if msg != errVerdictRecipientLimit().(*Error).Message || !strings.Contains(msg, "stays submitted") {
		t.Fatalf("recipient_limit: %q", msg)
	}
	if w := getTestWork(t, s, id); w.State != "submitted" || w.Reward == nil || w.Reward.State != "held" {
		t.Fatalf("after a refused accept: %+v", w)
	}
}

// On a board with the compiled-in allowance parameters (no grant pool), an
// operator grant says why it fails and what to publish, not that the
// board's free allowance is used up; and a rewarded accept is paid, since
// credit's inbound cap is its budget.
func TestFreshBoardGrantAndRewardDefaults(t *testing.T) {
	if _, _, ok := ledgerTestOverride(); ok {
		t.Skip("the ledger mode is overridden for the whole suite")
	}
	s := openTest(t, rewardConfig())
	t.Cleanup(s.stopServices)
	owner, worker := keyFor(162), keyFor(163)
	run(t, s, signed(owner, Command{Operation: "agent.register"}))
	run(t, s, signed(worker, Command{Operation: "agent.register"}))
	err := s.GrantAllowance(testContext, keyID(owner), allowance.Credit, allowance.Granted, 500, "test grant")
	if err == nil || !strings.Contains(err.Error(), "grant_share_ppm 0") || !strings.Contains(err.Error(), "params set allowance") {
		t.Fatalf("grant on a fresh board: %v", err)
	}
	mintCredit(t, s, keyID(owner), allowance.Paid, 1000)
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, rewardCreate(s, owner, id, 50, 0))
	fence := claimAndSubmit(t, s, worker, id, "lobby")
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: id, Amount: fence}))
	if w := getTestWork(t, s, id); w.State != "accepted" || w.Reward == nil || w.Reward.State != "paid" {
		t.Fatalf("a rewarded accept on default parameters: %+v", w)
	}
}
