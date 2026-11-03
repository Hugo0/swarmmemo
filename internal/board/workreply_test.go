package board

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
)

// C691: work.accept with target, a direct reply's ID, accepts a result its
// author never claimed or submitted: one transaction, the claim's checks, the
// requester's signed accept as the only new transition, credits to the
// author, USDC only to its payout address on record.

// replyTo posts key's direct reply to id in lobby.
func replyTo(t *testing.T, s *Store, key ed25519.PrivateKey, id, text string) string {
	t.Helper()
	return run(t, s, signed(key, Command{Operation: "post", Room: "lobby", ReplyTo: id, Text: text + " " + randomID()[:8], Timestamp: s.now().Unix()})).Receipt.ID
}

// acceptReplyCmd is the requester's accept naming reply, with no fence.
func acceptReplyCmd(s *Store, key ed25519.PrivateKey, id, reply string, extra map[string]any) Command {
	return workWith(s, key, Command{Operation: "work.accept", MessageID: id, Target: reply}, extra)
}

func TestWorkAcceptReplyPlainTask(t *testing.T) {
	s := rewardStore(t)
	owner, author := keyFor(60), keyFor(61)
	mintCredit(t, s, keyID(owner), allowance.Paid, 1000)
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, rewardCreate(s, owner, id, 300, 0))
	reply := replyTo(t, s, author, id, "Here is the review: two bugs in parse.go")
	before := creditIn(t, s, keyID(author), "remaining")

	// A fence is refused: the reply is the attempt.
	bad := workWith(s, owner, Command{Operation: "work.accept", MessageID: id, Target: reply, Amount: 1}, nil)
	fails(t, s, bad, "work_fence_mismatch")
	// result_id in data says where it goes.
	fails(t, s, workWith(s, owner, Command{Operation: "work.accept", MessageID: id}, map[string]any{"result_id": reply}), "invalid_work_data")

	cmd := acceptReplyCmd(s, owner, id, reply, nil)
	ack := run(t, s, cmd).Data["ack"].(WorkAck)
	if ack.State != "accepted" || ack.Fence != 1 || !strings.Contains(ack.Note, "Accepted reply "+reply) || !strings.Contains(ack.Note, "claimed and submitted it for its author") {
		t.Fatalf("ack: %+v", ack)
	}
	w := getTestWork(t, s, id)
	if w.State != "accepted" || w.ResultID != reply || w.Reward == nil || w.Reward.State != "paid" || w.Fence != 1 {
		t.Fatalf("work: %+v %+v", w, w.Reward)
	}
	if got := creditIn(t, s, keyID(author), "remaining") - before; got != 300 {
		t.Fatalf("author paid %d credits, want 300", got)
	}
	// The history is the create and the requester's own signed accept,
	// whose amount (none) and fence (the claim's) a verifier reads.
	trs := run(t, s, Command{Operation: "work.history", MessageID: id}).Data["transitions"].([]WorkTransition)
	if len(trs) != 2 || trs[1].Operation != "work.accept" || trs[1].Fence != 1 || trs[1].State != "accepted" || trs[1].Author != keyID(owner) {
		t.Fatalf("history: %+v", trs)
	}
	pub, _ := base64.RawURLEncoding.DecodeString(trs[1].PublicKey)
	sig, _ := base64.RawURLEncoding.DecodeString(trs[1].Signature)
	if !ed25519.Verify(pub, []byte(trs[1].SignedPayload), sig) || trs[1].SignedPayload != string(Canonical("swarmmemo.com", cmd)) {
		t.Fatal("the accept's transition is not the requester's signed command")
	}
	var env struct {
		Command map[string]any `json:"command"`
	}
	_ = json.Unmarshal([]byte(trs[1].SignedPayload), &env)
	if env.Command["target"] != reply || env.Command["amount"] != nil {
		t.Fatalf("signed command: %v", env.Command)
	}
	// The replay is the same answer; a second accept is a state conflict.
	run(t, s, cmd)
	fails(t, s, acceptReplyCmd(s, owner, id, reply, nil), "work_state_conflict")
}

func TestWorkAcceptReplyRefusals(t *testing.T) {
	s := rewardStore(t)
	owner, author, other, outsider := keyFor(62), keyFor(63), keyFor(64), keyFor(65)
	mintCredit(t, s, keyID(owner), allowance.Paid, 3000)
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, rewardCreate(s, owner, id, 100, 0))
	reply := replyTo(t, s, author, id, "My result")

	// Only the requester decides.
	fails(t, s, acceptReplyCmd(s, outsider, id, reply, nil), "work_forbidden")
	// The requester's own reply.
	own := replyTo(t, s, owner, id, "A note of my own")
	fails(t, s, acceptReplyCmd(s, owner, id, own, nil), "work_forbidden")
	// Not a direct reply: a reply to the reply, and a post elsewhere.
	nested := run(t, s, signed(author, Command{Operation: "post", Room: "lobby", ReplyTo: reply, Text: "more", Timestamp: s.now().Unix()})).Receipt.ID
	fails(t, s, acceptReplyCmd(s, owner, id, nested, nil), "invalid_work_result")
	elsewhere := run(t, s, signed(author, Command{Operation: "post", Room: "lobby", Text: "result at top level", Timestamp: s.now().Unix()})).Receipt.ID
	fails(t, s, acceptReplyCmd(s, owner, id, elsewhere, nil), "invalid_work_result")
	fails(t, s, acceptReplyCmd(s, owner, id, randomID(), nil), "invalid_work_result")
	// An unsigned reply.
	anon := run(t, s, Command{Operation: "post", Room: "lobby", ReplyTo: id, Text: "anonymous result", Timestamp: s.now().Unix()}).Receipt.ID
	fails(t, s, acceptReplyCmd(s, owner, id, anon, nil), "invalid_work_result")
	// A hidden reply.
	hidden := replyTo(t, s, author, id, "hidden result")
	if _, err := s.db.Exec("UPDATE events SET hidden=1 WHERE id=?", hidden); err != nil {
		t.Fatal(err)
	}
	fails(t, s, acceptReplyCmd(s, owner, id, hidden, nil), "invalid_work_result")

	// Claimed by someone else: refused, nothing changes.
	claim := run(t, s, workCommand(s, other, Command{Operation: "work.claim", MessageID: id, TTL: 600})).Data["ack"].(WorkAck)
	fails(t, s, acceptReplyCmd(s, owner, id, reply, nil), "work_state_conflict")
	if w := getTestWork(t, s, id); w.State != "claimed" || w.Fence != claim.Fence {
		t.Fatalf("a refused accept changed the work: %+v", w)
	}

	// The claim holder's own reply is submitted for it, then accepted.
	mine := replyTo(t, s, other, id, "The claim holder's result")
	ack := run(t, s, acceptReplyCmd(s, owner, id, mine, nil)).Data["ack"].(WorkAck)
	if ack.State != "accepted" || ack.Fence != claim.Fence || !strings.Contains(ack.Note, "who held the claim") {
		t.Fatalf("claim holder's reply: %+v", ack)
	}

	// Ineligible: a first_work task and an author that has submitted work.
	fw := rewardRequest(t, s, owner, "lobby")
	run(t, s, workWith(s, owner, Command{Operation: "work.create", MessageID: fw}, map[string]any{"title": "First work only", "capabilities": []string{"review"}, "eligibility": WorkEligibilityFirstWork}))
	late := replyTo(t, s, other, fw, "first work result")
	fails(t, s, acceptReplyCmd(s, owner, fw, late, nil), "not_eligible")
	if w := getTestWork(t, s, fw); w.State != "open" || w.Fence != 0 {
		t.Fatalf("a refused accept changed the work: %+v", w)
	}
	// A first-time author is taken, and then counts as having worked.
	fresh := replyTo(t, s, author, fw, "first work result")
	run(t, s, acceptReplyCmd(s, owner, fw, fresh, nil))
	fw2 := rewardRequest(t, s, owner, "lobby")
	run(t, s, workWith(s, owner, Command{Operation: "work.create", MessageID: fw2}, map[string]any{"title": "First work only", "capabilities": []string{"review"}, "eligibility": WorkEligibilityFirstWork}))
	again := replyTo(t, s, author, fw2, "first work result")
	fails(t, s, acceptReplyCmd(s, owner, fw2, again, nil), "not_eligible")
}

// A normal accept, without target, is unchanged: it still needs the fence.
func TestWorkAcceptWithoutTargetUnchanged(t *testing.T) {
	s := rewardStore(t)
	owner, worker := keyFor(66), keyFor(67)
	mintCredit(t, s, keyID(owner), allowance.Paid, 1000)
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, rewardCreate(s, owner, id, 100, 0))
	// Open work: an accept without target has nothing to accept.
	fails(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: id, Amount: 1}), "work_state_conflict")
	fence := claimAndSubmit(t, s, worker, id, "lobby")
	fails(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: id}), "work_fence_mismatch")
	ack := run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: id, Amount: fence})).Data["ack"].(WorkAck)
	if ack.State != "accepted" || strings.Contains(ack.Note, "Accepted reply") {
		t.Fatalf("normal accept: %+v", ack)
	}
	trs := run(t, s, Command{Operation: "work.history", MessageID: id}).Data["transitions"].([]WorkTransition)
	if len(trs) != 4 {
		t.Fatalf("normal history: %d transitions", len(trs))
	}
}

func TestWorkAcceptReplyStandingSeat(t *testing.T) {
	s := rewardStore(t)
	owner, w1, w2, w3 := keyFor(68), keyFor(69), keyFor(70), keyFor(71)
	mintCredit(t, s, keyID(owner), allowance.Paid, 5000)
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, id, map[string]any{"seats": 2, "reward": 200, "per_agent_day": 1}, 10*86400))

	text := "Bug: the export button 500s on empty rooms."
	r1 := run(t, s, signed(w1, Command{Operation: "post", Room: "lobby", ReplyTo: id, Text: text, Timestamp: s.now().Unix()})).Receipt.ID
	before := creditIn(t, s, keyID(w1), "remaining")
	ack := run(t, s, acceptReplyCmd(s, owner, id, r1, nil)).Data["ack"].(WorkAck)
	if ack.State != "accepted" || ack.WorkID != r1 || ack.Fence != 1 {
		t.Fatalf("standing ack: %+v", ack)
	}
	if w := getTestWork(t, s, r1); w.State != "accepted" || w.StandingID != id || w.ResultID != r1 {
		t.Fatalf("standing result: %+v", w)
	}
	if got := creditIn(t, s, keyID(w1), "remaining") - before; got != 200 {
		t.Fatalf("author paid %d, want 200", got)
	}
	trs := run(t, s, Command{Operation: "work.history", MessageID: r1}).Data["transitions"].([]WorkTransition)
	if len(trs) != 1 || trs[0].Operation != "work.accept" || trs[0].ResolvedFrom != id || trs[0].Fence != 1 {
		t.Fatalf("standing history: %+v", trs)
	}
	// The standing root is unchanged and stays open.
	if w := getTestWork(t, s, id); w.State != "open" {
		t.Fatalf("root: %+v", w)
	}

	// The same text again: duplicate_result, as for a claim.
	dup := run(t, s, signed(w2, Command{Operation: "post", Room: "lobby", ReplyTo: id, Text: text, Timestamp: s.now().Unix()})).Receipt.ID
	fails(t, s, acceptReplyCmd(s, owner, id, dup, nil), "duplicate_result")
	// per_agent_day: w1's second result today.
	r1b := replyTo(t, s, w1, id, "Bug: another one")
	fails(t, s, acceptReplyCmd(s, owner, id, r1b, nil), "seat_limit")
	// A reply that already opened a task is not taken twice.
	fails(t, s, acceptReplyCmd(s, owner, id, r1, nil), "result_in_use")
	// The result item is not where a reply is accepted.
	r3 := replyTo(t, s, w3, id, "Bug: third")
	fails(t, s, acceptReplyCmd(s, owner, r1, r3, nil), "invalid_work_result")
	run(t, s, acceptReplyCmd(s, owner, id, r3, nil))
	if n := sqlCount(t, s, "SELECT count(*) FROM works WHERE standing_id=? AND state='accepted'", id); n != 2 {
		t.Fatalf("accepted results: %d", n)
	}
}

// C704: an accept with target that names a result already submitted as its
// own task names the one call that works: work.accept on that result, its
// fence as amount. It never tells the requester to post a new reply.
func TestWorkAcceptReplySubmittedResultNamesFence(t *testing.T) {
	s := rewardStore(t)
	owner, w1 := keyFor(68), keyFor(69)
	mintCredit(t, s, keyID(owner), allowance.Paid, 5000)
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, id, map[string]any{"seats": 2, "reward": 200}, 10*86400))
	r1, claim := standingClaim(t, s, w1, id, nil)
	run(t, s, claim)
	want := "accept it directly with work.accept on " + r1 + ", fence 1 as amount, without target"

	// 1) The accept sent to the result, with the result as target.
	msg := failsWith(t, s, acceptReplyCmd(s, owner, r1, r1, nil), "invalid_work_result")
	if !strings.Contains(msg, want) || strings.Contains(msg, "standing task") || strings.Contains(msg, "Post a new reply") {
		t.Fatalf("on the result: %q", msg)
	}
	// 2) The accept sent to the standing task, with the result as target.
	msg = failsWith(t, s, acceptReplyCmd(s, owner, id, r1, nil), "result_in_use")
	if !strings.Contains(msg, want) || strings.Contains(msg, "Post a new reply") {
		t.Fatalf("on the root: %q", msg)
	}
	// The named call works.
	ack := run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: r1, Amount: 1})).Data["ack"].(WorkAck)
	if ack.State != "accepted" || ack.WorkID != r1 {
		t.Fatalf("accept: %+v", ack)
	}
	// Once judged, the refusal says so and names no fence to send.
	msg = failsWith(t, s, acceptReplyCmd(s, owner, id, r1, nil), "result_in_use")
	if !strings.Contains(msg, r1+", accepted") || strings.Contains(msg, "fence") || strings.Contains(msg, "Post a new reply") {
		t.Fatalf("after accept: %q", msg)
	}
}

func TestWorkAcceptReplyUSDC(t *testing.T) {
	s, _ := usdcStore(t)
	owner, linked, bare := keyFor(72), keyFor(73), keyFor(74)
	mintCredit(t, s, keyID(owner), allowance.Paid, 3000)
	wallet := "0x00000000000000000000000000000000000000dd"
	run(t, s, signed(linked, Command{Operation: "agent.register", Timestamp: s.now().Unix()}))
	run(t, s, signed(bare, Command{Operation: "agent.register", Timestamp: s.now().Unix()}))
	if _, err := s.db.Exec("INSERT INTO identity_links(agent,kind,value,state,created_at,checked_at) VALUES(?,'wallet',?,'verified',1,1)", keyID(linked), wallet); err != nil {
		t.Fatal(err)
	}

	// A linked verified wallet is paid: the USDC is owed to it.
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, rewardCreate(s, owner, id, map[string]any{"credits": 100, "usdc": "0.10"}, 0))
	reply := replyTo(t, s, linked, id, "USDC task result")
	ack := run(t, s, acceptReplyCmd(s, owner, id, reply, nil)).Data["ack"].(WorkAck)
	w := getTestWork(t, s, id)
	if w.State != "accepted" || w.RewardUSDC == nil || w.RewardUSDC.State != "payable" || w.RewardUSDC.PayTo != wallet || w.RewardUSDC.PayToSource != "wallet_link" || w.Reward.State != "paid" {
		t.Fatalf("linked wallet: %+v %+v", w.RewardUSDC, w.Reward)
	}
	if !strings.Contains(ack.Note, "0.1 USDC is now owed") {
		t.Fatalf("linked wallet note: %q", ack.Note)
	}

	// No wallet: credits only, the USDC void, and the note says so.
	id2 := rewardRequest(t, s, owner, "lobby")
	run(t, s, rewardCreate(s, owner, id2, map[string]any{"credits": 100, "usdc": "0.10"}, 0))
	reply2 := replyTo(t, s, bare, id2, "USDC task result, no wallet")
	before := creditIn(t, s, keyID(bare), "remaining")
	ack = run(t, s, acceptReplyCmd(s, owner, id2, reply2, nil)).Data["ack"].(WorkAck)
	w = getTestWork(t, s, id2)
	if w.State != "accepted" || w.Reward.State != "paid" || w.RewardUSDC == nil || w.RewardUSDC.State != "void" || w.RewardUSDC.PayTo != "" || w.RewardUSDC.Reason != WorkUSDCNoPayoutAddress {
		t.Fatalf("no wallet: %+v %+v", w.RewardUSDC, w.Reward)
	}
	if !strings.Contains(ack.Note, "Paid in credits only") || !strings.Contains(ack.Note, "0.1 USDC is not owed") || strings.Contains(ack.Note, "now owed") {
		t.Fatalf("no wallet note: %q", ack.Note)
	}
	if got := creditIn(t, s, keyID(bare), "remaining") - before; got != 100 {
		t.Fatalf("credits paid %d, want 100", got)
	}
	// Once it links a wallet, the requester can owe it with work.promise.
	if _, err := s.db.Exec("INSERT INTO identity_links(agent,kind,value,state,created_at,checked_at) VALUES(?,'wallet',?,'verified',1,1)", keyID(bare), usdcOther); err != nil {
		t.Fatal(err)
	}
	run(t, s, workWith(s, owner, Command{Operation: WorkPromise, MessageID: id2}, map[string]any{"amount_usdc": "0.10"}))
	if w = getTestWork(t, s, id2); w.RewardUSDC.State != "payable" || w.RewardUSDC.PayTo != usdcOther {
		t.Fatalf("promised later: %+v", w.RewardUSDC)
	}
}

// With a named reviewer, the reviewer accepts a reply, and is paid its fee;
// the requester cannot, and the reviewer's own reply is never a result.
func TestWorkAcceptReplyReviewer(t *testing.T) {
	s := rewardStore(t)
	owner, author, judge := keyFor(75), keyFor(76), keyFor(78)
	register(t, s, judge)
	mintCredit(t, s, keyID(owner), allowance.Paid, 1000)
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, reviewedCreate(s, owner, id, map[string]any{"reward": 300, "reviewer": keyID(judge), "reviewer_fee": 20}, 0))
	reply := replyTo(t, s, author, id, "Reviewed result")
	fails(t, s, acceptReplyCmd(s, owner, id, reply, nil), "not_the_reviewer")
	own := replyTo(t, s, judge, id, "The reviewer's own reply")
	fails(t, s, acceptReplyCmd(s, judge, id, own, nil), "work_forbidden")
	before := creditIn(t, s, keyID(judge), "remaining")
	run(t, s, acceptReplyCmd(s, judge, id, reply, nil))
	w := getTestWork(t, s, id)
	if w.State != "accepted" || w.Reward.State != "paid" || w.ReviewerFee.State != "paid" {
		t.Fatalf("reviewed: %+v %+v %+v", w, w.Reward, w.ReviewerFee)
	}
	if got := creditIn(t, s, keyID(judge), "remaining") - before; got != 20 {
		t.Fatalf("reviewer fee %d, want 20", got)
	}
}

// A rated accept of a direct reply rates the reply's author.
func TestWorkAcceptReplyRated(t *testing.T) {
	s := reputationStore(t)
	owner, author := keyFor(79), keyFor(80)
	for i, k := range []ed25519.PrivateKey{owner, author} {
		run(t, s, signed(k, Command{Operation: "agent.register", Handle: "c691agent" + string(rune('a'+i))}))
	}
	mintCredit(t, s, keyID(owner), allowance.Paid, 1_000_000)
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, rewardCreate(s, owner, id, 1000, 0))
	reply := replyTo(t, s, author, id, "A rated result")
	accept := ratedAccept(t, s, owner, id, 0, keyID(author), 90, "solid")
	accept.Target, accept.Nonce, accept.Signature = reply, "", ""
	accept = signed(owner, accept)
	ack := run(t, s, accept)
	if ack.Data["review"] == nil || ack.Data["ack"].(WorkAck).State != "accepted" {
		t.Fatalf("rated reply accept: %+v", ack.Data)
	}
}
