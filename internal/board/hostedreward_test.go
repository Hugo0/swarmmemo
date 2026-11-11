package board

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
)

// C883: a hosted identity may hold credit work rewards within a rolling
// daily cap, paid only to workers not linked to it; everything else that
// moves its credit out stays hosted_transfer, and a keyed requester is
// unaffected.

func hostedRewardStore(t *testing.T) *Store {
	t.Helper()
	if _, _, ok := ledgerTestOverride(); ok {
		t.Skip("the ledger mode is overridden for the whole suite")
	}
	cfg := rewardConfig()
	cfg.HostedKEKFile = writeKEK(t, 0o600)
	cfg.Features.WorkRejectFee = true
	s := openTest(t, cfg)
	t.Cleanup(s.stopServices)
	setRewardParams(t, s)
	return s
}

// viaMCP runs c as a hosted identity's call does, and returns its error.
func viaMCP(s *Store, c Command) (Result, error) {
	return s.Execute(WithVia(testContext, "mcp"), c, "test-origin")
}

func mustMCP(t *testing.T, s *Store, c Command) Result {
	t.Helper()
	r, err := viaMCP(s, c)
	if err != nil {
		t.Fatalf("%s: %v", c.Operation, err)
	}
	return r
}

func failsMCP(t *testing.T, s *Store, c Command, code string) *Error {
	t.Helper()
	_, err := viaMCP(s, c)
	var e *Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("%s: want %s, got %v", c.Operation, code, err)
	}
	return e
}

func hostedRequest(t *testing.T, s *Store, key ed25519.PrivateKey) string {
	t.Helper()
	return mustMCP(t, s, signed(key, Command{Operation: "post", Room: "lobby", Kind: "request", Text: "Review my patch for credits" + testPostSuffix(), Timestamp: s.now().Unix()})).Receipt.ID
}

func workCreateData(s *Store, key ed25519.PrivateKey, id string, extra map[string]any) Command {
	d := map[string]any{"schema": 1, "generation": s.generation, "title": "Rewarded review", "capabilities": []string{"review"}}
	for k, v := range extra {
		d[k] = v
	}
	b, _ := json.Marshal(d)
	return signed(key, Command{Operation: "work.create", MessageID: id, Data: string(b), Timestamp: s.now().Unix()})
}

func TestHostedWorkRewardCapAndPayout(t *testing.T) {
	s := hostedRewardStore(t)
	created := createHosted(t, s, "test-origin", "")
	hosted := hostedKey(t, s, created["token"].(string))
	account := created["agent"].(string)
	mintCredit(t, s, account, allowance.Paid, 10_000)
	worker := keyFor(160)
	register(t, s, worker)

	// Under the cap: held, claimed and accepted by an unrelated worker.
	paid := hostedRequest(t, s, hosted)
	mustMCP(t, s, rewardCreate(s, hosted, paid, 1500, 0))
	if r := getTestWork(t, s, paid).Reward; r == nil || r.State != "held" || r.Amount != 1500 {
		t.Fatalf("held reward: %+v", r)
	}
	fence := claimAndSubmit(t, s, worker, paid, "lobby")
	mustMCP(t, s, workCommand(s, hosted, Command{Operation: "work.accept", MessageID: paid, Amount: fence}))
	if got := creditIn(t, s, keyID(worker), "remaining"); got != 1500 {
		t.Fatalf("the worker received %d", got)
	}

	// Over the cap: 1500 paid + 600 > 2000, refused with a wait.
	over := hostedRequest(t, s, hosted)
	e := failsMCP(t, s, rewardCreate(s, hosted, over, 600, 0), "hosted_reward_cap")
	if e.Status != 429 || e.RetryAfter <= 0 {
		t.Fatalf("cap refusal: %+v", e)
	}
	// Exactly to the cap is allowed; a cancel returns it and frees the cap.
	mustMCP(t, s, rewardCreate(s, hosted, over, 500, 0))
	held := creditIn(t, s, account, "held")
	if held != 500 {
		t.Fatalf("held %d", held)
	}
	before := creditIn(t, s, account, "remaining")
	mustMCP(t, s, workCommand(s, hosted, Command{Operation: "work.cancel", MessageID: over, Reason: "changed my mind"}))
	if r := getTestWork(t, s, over).Reward; r.State != "released" || r.Reason != "cancelled" || creditIn(t, s, account, "held") != 0 || creditIn(t, s, account, "remaining") != before {
		t.Fatalf("cancel: %+v", r)
	}
	again := hostedRequest(t, s, hosted)
	mustMCP(t, s, rewardCreate(s, hosted, again, 500, 0))

	// A day later the paid reward has left the window.
	s.now = func() time.Time { return time.Unix(testTime+HostedWorkRewardWindow+1, 0) }
	later := hostedRequest(t, s, hosted)
	mustMCP(t, s, rewardCreate(s, hosted, later, 1500, 0))

	// The operator tunes the cap in the hosted parameters.
	if _, err := s.SetAllowanceParams(testContext, HostedParamsNamespace, []byte(`{"schema":1,"per_network_daily":200,"global_daily":10000,"networks":[],"work_reward_daily":0}`), "stop hosted rewards", 0); err != nil {
		t.Fatal(err)
	}
	stopped := hostedRequest(t, s, hosted)
	failsMCP(t, s, rewardCreate(s, hosted, stopped, 1, 0), "hosted_reward_cap")
	// A version published before the field keeps the default.
	if _, err := s.SetAllowanceParams(testContext, HostedParamsNamespace, []byte(`{"schema":1,"per_network_daily":200,"global_daily":10000,"networks":[]}`), "older shape", 0); err != nil {
		t.Fatal(err)
	}
	mustMCP(t, s, rewardCreate(s, hosted, stopped, 1, 0))
	if _, err := s.SetAllowanceParams(testContext, HostedParamsNamespace, []byte(`{"schema":1,"per_network_daily":200,"global_daily":10000,"networks":[],"work_reward_daily":-1}`), "bad", 0); err == nil {
		t.Fatal("a negative work_reward_daily was accepted")
	}
	if err := s.Integrity(testContext); err != nil {
		t.Fatal(err)
	}
}

func TestHostedWorkRewardOtherTransfersStayRefused(t *testing.T) {
	s := hostedRewardStore(t)
	created := createHosted(t, s, "test-origin", "")
	hosted := hostedKey(t, s, created["token"].(string))
	mintCredit(t, s, created["agent"].(string), allowance.Paid, 10_000)
	other := keyFor(161)
	register(t, s, other)

	failsMCP(t, s, signed(hosted, Command{Operation: "credit.transfer", Target: keyID(other), Amount: 1}), "hosted_transfer")
	failsMCP(t, s, signed(hosted, Command{Operation: "allowance.transfer", Target: keyID(other), Amount: 1, Data: `{"schema":1,"resource":"credit"}`}), "hosted_transfer")
	for name, extra := range map[string]map[string]any{
		"reviewer fee": {"reward": 10, "reviewer": keyID(other), "reviewer_fee": 5},
		"reject fee":   {"reward": 10, "reject_fee": 1},
		"standing":     {"seats": 2, "tiers": []map[string]any{{"name": "ok", "credits": 10}}},
	} {
		id := hostedRequest(t, s, hosted)
		_, err := viaMCP(s, workCreateData(s, hosted, id, extra))
		var e *Error
		if !errors.As(err, &e) || e.Code != "hosted_transfer" {
			t.Fatalf("%s from a hosted identity: %v", name, err)
		}
		if n := sqlCount(t, s, "SELECT count(*) FROM works WHERE id=?", id); n != 0 {
			t.Fatalf("%s: a refused create left the work", name)
		}
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM work_rewards"); n != 0 {
		t.Fatalf("refused creates held %d rewards", n)
	}
}

func TestHostedWorkRewardLinkedWorkerNotPaid(t *testing.T) {
	s := hostedRewardStore(t)
	created := createHosted(t, s, "test-origin", "")
	hosted := hostedKey(t, s, created["token"].(string))
	account := created["agent"].(string)
	mintCredit(t, s, account, allowance.Paid, 10_000)
	sibling := keyFor(162)
	register(t, s, sibling)
	// The thief's own key wrote from the network the token was used from.
	for _, acct := range []string{account, keyID(sibling)} {
		if _, err := s.db.Exec("INSERT INTO write_signals(created_at,operation,account,via,hash_key,ip_hash_24) VALUES(?,'post',?,'http',?,'net-c883')", testTime, acct, s.signals.id); err != nil {
			t.Fatal(err)
		}
	}
	id := hostedRequest(t, s, hosted)
	mustMCP(t, s, rewardCreate(s, hosted, id, 400, 0))
	fence := claimAndSubmit(t, s, sibling, id, "lobby")
	failsMCP(t, s, workCommand(s, hosted, Command{Operation: "work.accept", MessageID: id, Amount: fence}), "work_forbidden")
	if got := creditIn(t, s, keyID(sibling), "remaining"); got != 0 {
		t.Fatalf("a linked worker received %d", got)
	}
	if r := getTestWork(t, s, id).Reward; r.State != "held" {
		t.Fatalf("after the refused accept: %+v", r)
	}
	mustMCP(t, s, workCommand(s, hosted, Command{Operation: "work.cancel", MessageID: id, Reason: "linked worker"}))
	if r := getTestWork(t, s, id).Reward; r.State != "released" || creditIn(t, s, account, "held") != 0 {
		t.Fatalf("cancel after a refused accept: %+v", r)
	}

	// A keyed requester on the same network is unaffected: no cap, and its
	// linked worker is paid as before.
	keyed := keyFor(163)
	register(t, s, keyed)
	mintCredit(t, s, keyID(keyed), allowance.Paid, 10_000)
	if _, err := s.db.Exec("INSERT INTO write_signals(created_at,operation,account,via,hash_key,ip_hash_24) VALUES(?,'post',?,'http',?,'net-c883')", testTime, keyID(keyed), s.signals.id); err != nil {
		t.Fatal(err)
	}
	big := rewardRequest(t, s, keyed, "lobby")
	run(t, s, rewardCreate(s, keyed, big, 3000, 0))
	fence = claimAndSubmit(t, s, sibling, big, "lobby")
	run(t, s, workCommand(s, keyed, Command{Operation: "work.accept", MessageID: big, Amount: fence}))
	if got := creditIn(t, s, keyID(sibling), "remaining"); got != 3000 {
		t.Fatalf("a keyed requester's worker received %d", got)
	}
}
