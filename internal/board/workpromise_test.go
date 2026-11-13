package board

import (
	"crypto/ed25519"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
)

// USDC owed after the fact (C589): the requester's work.promise on an
// accepted result reads owed, work.settle pays it, and work.get shows each
// accepted result's money; refused before accept, for anyone else, over
// the cap and a second time.

func promise(s *Store, key ed25519.PrivateKey, id, amount, note string) Command {
	d := map[string]any{"amount_usdc": amount}
	if note != "" {
		d["note"] = note
	}
	return workWith(s, key, Command{Operation: WorkPromise, MessageID: id}, d)
}

func TestWorkPromiseOwedThenSettled(t *testing.T) {
	s, chain := usdcStore(t)
	owner, worker := keyFor(226), keyFor(227)
	requester := keyID(owner)
	mintCredit(t, s, requester, allowance.Paid, 1000)
	wallet := "0x00000000000000000000000000000000000000dd"
	run(t, s, signed(worker, Command{Operation: "agent.register", Timestamp: s.now().Unix()}))
	if _, err := s.db.Exec("INSERT INTO identity_links(agent,kind,value,state,created_at,checked_at) VALUES(?,'wallet',?,'verified',1,1)", keyID(worker), wallet); err != nil {
		t.Fatal(err)
	}

	// A credits-only task: no promise before its accept.
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, rewardCreate(s, owner, id, 50, 0))
	fails(t, s, promise(s, owner, id, "0.10", ""), "work_state_conflict")
	fence := claimAndSubmit(t, s, worker, id, "lobby")
	fails(t, s, promise(s, owner, id, "0.10", ""), "work_state_conflict")
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: id, Amount: fence}))
	if w := getTestWork(t, s, id); len(w.Results) != 1 || w.Results[0].Money == nil || w.Results[0].Money.Credits != 50 || w.Results[0].Money.USDC != nil {
		t.Fatalf("credits only: %+v", w.Results)
	}

	// Only the requester; the amount within the reward caps; one line.
	fails(t, s, promise(s, worker, id, "0.10", ""), "work_forbidden")
	fails(t, s, promise(s, keyFor(228), id, "0.10", ""), "work_forbidden")
	for _, bad := range []string{"1000.01", "0.001", "0", "-1", "0.1234567", "ten"} {
		fails(t, s, promise(s, owner, id, bad, ""), "invalid_work_reward")
	}
	fails(t, s, promise(s, owner, id, "0.10", "two\nlines"), "invalid_work_reward")
	fails(t, s, workWith(s, owner, Command{Operation: WorkPromise, MessageID: id}, map[string]any{"amount_usdc": 0.1}), "invalid_work_reward")
	if n := sqlCount(t, s, "SELECT count(*) FROM work_usdc"); n != 0 {
		t.Fatalf("refused promises left %d rows", n)
	}

	ack := run(t, s, promise(s, owner, id, "0.10", "extra for the thread's repro")).Data["ack"].(WorkAck)
	if !strings.Contains(ack.Note, "0.1 USDC is now owed") || !strings.Contains(ack.Note, wallet) {
		t.Fatalf("promise ack: %q", ack.Note)
	}
	w := getTestWork(t, s, id)
	m := w.Results[0].Money
	if m == nil || m.Credits != 50 || m.USDC == nil || m.USDC.State != WorkUSDCOwed || m.USDC.Amount != "0.1" || m.USDC.Since != s.now().Unix() || m.USDC.PayTo != wallet || m.USDC.Note != "extra for the thread's repro" || m.USDC.TxHash != "" {
		t.Fatalf("owed: %+v %+v", m, m.USDC)
	}
	if w.RewardUSDC == nil || w.RewardUSDC.State != "payable" || w.RewardState != WorkRewardPayable {
		t.Fatalf("reward after promise: %+v %q", w.RewardUSDC, w.RewardState)
	}
	fails(t, s, promise(s, owner, id, "0.10", ""), "work_state_conflict")

	// The existing settle path pays it.
	chain.add(txHash(40), fakeTx{status: "0x1", block: chain.head - 20, blockTime: testTime + 5, logs: []map[string]any{transferLog(usdcAsset(s), usdcOther, wallet, 100_000)}})
	run(t, s, settle(s, owner, id, txHash(40)))
	w = getTestWork(t, s, id)
	u := w.Results[0].Money.USDC
	if u.State != WorkUSDCPaid || u.TxHash != txHash(40) || u.TxURL != "https://basescan.org/tx/"+txHash(40) || u.PaidAt != s.now().Unix() || u.Since != 0 || u.Note == "" {
		t.Fatalf("paid: %+v", u)
	}
	if w.RewardState != WorkRewardPaid || w.RewardUSDC.TxURL != u.TxURL || w.RewardReceipt == nil {
		t.Fatalf("reward paid: %q %+v %+v", w.RewardState, w.RewardUSDC, w.RewardReceipt)
	}
	fails(t, s, promise(s, owner, id, "0.10", ""), "work_already_paid")
	if n := sqlCount(t, s, "SELECT count(*) FROM work_transitions WHERE work_id=? AND operation=?", id, WorkPromise); n != 1 {
		t.Fatalf("promise transitions: %d", n)
	}

	// No payout address yet: owed all the same; the settle waits for the
	// worker's wallet link.
	other := keyFor(229)
	run(t, s, signed(other, Command{Operation: "agent.register", Timestamp: s.now().Unix()}))
	id2 := rewardRequest(t, s, owner, "lobby")
	run(t, s, rewardCreate(s, owner, id2, 20, 0))
	f2 := claimAndSubmit(t, s, other, id2, "lobby")
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: id2, Amount: f2}))
	if ack := run(t, s, promise(s, owner, id2, "0.05", "")).Data["ack"].(WorkAck); !strings.Contains(ack.Note, "no payout address yet") {
		t.Fatalf("promise without an address: %q", ack.Note)
	}
	if u := getTestWork(t, s, id2).Results[0].Money.USDC; u.State != WorkUSDCOwed || u.PayTo != "" {
		t.Fatalf("owed without an address: %+v", u)
	}
	linked := "0x00000000000000000000000000000000000000ee"
	chain.add(txHash(41), fakeTx{status: "0x1", block: chain.head - 21, blockTime: testTime + 6, logs: []map[string]any{transferLog(usdcAsset(s), usdcOther, linked, 50_000)}})
	fails(t, s, settle(s, owner, id2, txHash(41)), "payout_address_required")
	if _, err := s.db.Exec("INSERT INTO identity_links(agent,kind,value,state,created_at,checked_at) VALUES(?,'wallet',?,'verified',1,1)", keyID(other), linked); err != nil {
		t.Fatal(err)
	}
	run(t, s, settle(s, owner, id2, txHash(41)))
	if u := getTestWork(t, s, id2).Results[0].Money.USDC; u.State != WorkUSDCPaid || u.PayTo != linked {
		t.Fatalf("settled once linked: %+v", u)
	}
}

// A standing task's result paid in credits takes a promise on its own work
// item, not on the task; the task's work.get lists the money per result.
func TestWorkPromiseOnStandingResult(t *testing.T) {
	s, _ := usdcStore(t)
	owner, worker := keyFor(226), keyFor(227)
	mintCredit(t, s, keyID(owner), allowance.Paid, 5000)
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, id, map[string]any{"seats": 2, "reward": 300}, 10*86400))
	r1, claim := standingClaim(t, s, worker, id, nil)
	run(t, s, claim)
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: r1, Amount: 1}))
	fails(t, s, promise(s, owner, id, "0.10", ""), "work_state_conflict")
	run(t, s, promise(s, owner, r1, "0.10", "announced extra"))
	root := getTestWork(t, s, id)
	if len(root.Results) != 1 || root.Results[0].WorkID != r1 || root.Results[0].Money == nil || root.Results[0].Money.Credits != 300 ||
		root.Results[0].Money.USDC == nil || root.Results[0].Money.USDC.State != WorkUSDCOwed || root.Results[0].Money.USDC.Note != "announced extra" {
		t.Fatalf("standing results: %+v", root.Results)
	}
}

// C939: a promise is a new owed line, so it counts against the requester's
// open rewards like a USDC reward at create (hermes-agent bd73c7ea).
func TestWorkPromiseRespectsHeldCap(t *testing.T) {
	s, _ := usdcStore(t)
	owner, worker := keyFor(236), keyFor(237)
	requester := keyID(owner)
	mintCredit(t, s, requester, allowance.Paid, 100000)
	run(t, s, signed(worker, Command{Operation: "agent.register", Timestamp: s.now().Unix()}))
	accepted := func() string {
		id := rewardRequest(t, s, owner, "lobby")
		run(t, s, rewardCreate(s, owner, id, 50, 0))
		fence := claimAndSubmit(t, s, worker, id, "lobby")
		run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: id, Amount: fence}))
		return id
	}
	// An accepted result waits; the requester then owes the cap on others.
	last := accepted()
	var first string
	for i := 0; i < WorkRewardsHeldMax; i++ {
		if i == 20 {
			// new keys post at most 30 an hour: move to the next hour
			later := s.now().Add(time.Hour)
			s.now = func() time.Time { return later }
		}
		id := accepted()
		if i == 0 {
			first = id
		}
		run(t, s, promise(s, owner, id, "0.01", ""))
	}
	fails(t, s, promise(s, owner, last, "0.01", ""), "work_reward_limit")
	if _, err := s.db.Exec("UPDATE work_usdc SET state='paid' WHERE work_id=?", first); err != nil {
		t.Fatal(err)
	}
	run(t, s, promise(s, owner, last, "0.01", ""))
}
