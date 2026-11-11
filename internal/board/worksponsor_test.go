package board

import (
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/ledger"
)

// C889: a fresh identity with only free daily credits gets its first task
// of the day sponsored by the house, within the sponsor parameters.

func setSponsorParams(t *testing.T, s *Store, body string) {
	t.Helper()
	if _, err := s.SetAllowanceParams(testContext, SponsorParamsNamespace, []byte(body), "sponsor test", 0); err != nil {
		t.Fatal(err)
	}
}

func lastingIn(t *testing.T, s *Store, account string) int64 {
	t.Helper()
	return sqlCount(t, s, "SELECT coalesce(sum(remaining),0) FROM ledger_lots WHERE account=? AND resource='credit' AND state='live' AND bucket<>'free'", account)
}

func hasHint(res Result, code string) string {
	if res.Next == nil {
		return ""
	}
	for _, h := range res.Next.Hints {
		if h.Code == code {
			return h.Message
		}
	}
	return ""
}

func TestSponsoredFirstTask(t *testing.T) {
	s := rewardStore(t)
	owner, worker := keyFor(240), keyFor(241)
	register(t, s, owner)
	register(t, s, worker)
	requester := keyID(owner)

	// Over the sponsored maximum: refused, never partly sponsored, and the
	// refusal names the maximum.
	over := rewardRequest(t, s, owner, "lobby")
	msg := failsWith(t, s, rewardCreate(s, owner, over, 101, 0), "not_transferable")
	if !strings.Contains(msg, "at most 100 credits") {
		t.Fatalf("over the maximum: %q", msg)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM ledger_holds WHERE account=?", SponsorAccount); n != 0 {
		t.Fatalf("a refused create left %d house holds", n)
	}

	// At the maximum: held by the house, the requester pays only the fee.
	id := rewardRequest(t, s, owner, "lobby")
	res := run(t, s, rewardCreate(s, owner, id, 100, 0))
	ack := res.Data["ack"].(WorkAck)
	if !strings.Contains(ack.Note, "Sponsored: SwarmMemo holds this 100-credit reward for your first task today") {
		t.Fatalf("sponsored note: %q", ack.Note)
	}
	if r := getTestWork(t, s, id).Reward; r == nil || r.State != "held" || r.Amount != 100 {
		t.Fatalf("held reward: %+v", r)
	}
	if creditIn(t, s, SponsorAccount, "held") != 100 || creditIn(t, s, requester, "held") != 0 {
		t.Fatalf("held by house %d, by requester %d", creditIn(t, s, SponsorAccount, "held"), creditIn(t, s, requester, "held"))
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM audit WHERE operation=? AND actor=? AND target=?", WorkSponsorOp, requester, id); n != 1 {
		t.Fatalf("audit rows %d", n)
	}

	// A second sponsored task the same day: refused as before.
	second := rewardRequest(t, s, owner, "lobby")
	if msg = failsWith(t, s, rewardCreate(s, owner, second, 10, 0), "not_transferable"); msg != errWorkRewardNotTransferable().(*Error).Message {
		t.Fatalf("second task: %q", msg)
	}

	// An unrelated worker is paid from the house escrow.
	fence := claimAndSubmit(t, s, worker, id, "lobby")
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: id, Amount: fence}))
	if got := creditIn(t, s, keyID(worker), "remaining"); got != 100 {
		t.Fatalf("the worker received %d", got)
	}
	if lastingIn(t, s, requester) != 0 {
		t.Fatal("the requester gained lasting credit")
	}
	if err := s.Integrity(testContext); err != nil {
		t.Fatal(err)
	}
}

func TestSponsoredTaskGlobalCapAndExpiry(t *testing.T) {
	s := rewardStore(t)
	setSponsorParams(t, s, `{"schema":1,"task_reward_max":100,"task_daily_per_identity":1,"task_daily_total":150}`)
	a, b, c := keyFor(242), keyFor(243), keyFor(244)
	register(t, s, c)
	register(t, s, a)
	register(t, s, b)

	first := rewardRequest(t, s, a, "lobby")
	run(t, s, rewardCreate(s, a, first, 100, 3600))
	// The day's total is 150: another 60 is over it.
	other := rewardRequest(t, s, b, "lobby")
	if msg := failsWith(t, s, rewardCreate(s, b, other, 60, 0), "not_transferable"); msg != errWorkRewardNotTransferable().(*Error).Message {
		t.Fatalf("over the daily total: %q", msg)
	}

	// The deadline passes unclaimed: the reward returns to the house, never
	// to the requester.
	s.now = func() time.Time { return time.Unix(testTime+3600, 0) }
	if _, err := s.SweepAllowance(testContext); err != nil {
		t.Fatal(err)
	}
	if r := getTestWork(t, s, first).Reward; r.State != "released" || r.Reason != "expired" {
		t.Fatalf("after the deadline: %+v", r)
	}
	if lastingIn(t, s, keyID(a)) != 0 || creditIn(t, s, SponsorAccount, "held") != 0 || creditIn(t, s, SponsorAccount, "remaining") != 100 {
		t.Fatalf("after expiry: requester %d, house held %d remaining %d", lastingIn(t, s, keyID(a)), creditIn(t, s, SponsorAccount, "held"), creditIn(t, s, SponsorAccount, "remaining"))
	}

	// Next day: the house reuses what came back before minting more.
	s.now = func() time.Time { return time.Unix(testTime+86400, 0) }
	next := rewardRequest(t, s, b, "lobby")
	run(t, s, rewardCreate(s, b, next, 80, 0))
	if n := sqlCount(t, s, "SELECT count(*) FROM ledger_entries WHERE account=? AND kind='grant'", SponsorAccount); n != 1 {
		t.Fatalf("house mints %d, want 1 (the first)", n)
	}

	// Sponsorship off: refused as before.
	setSponsorParams(t, s, `{"schema":1,"task_reward_max":0,"task_daily_per_identity":1,"task_daily_total":150}`)
	off := rewardRequest(t, s, c, "lobby")
	failsWith(t, s, rewardCreate(s, c, off, 10, 0), "not_transferable")
	if _, err := s.SetAllowanceParams(testContext, SponsorParamsNamespace, []byte(`{"schema":1,"task_reward_max":-1,"task_daily_per_identity":1,"task_daily_total":150}`), "bad", 0); err == nil {
		t.Fatal("a negative task_reward_max was accepted")
	}
	if err := s.Integrity(testContext); err != nil {
		t.Fatal(err)
	}
}

func TestSponsoredTaskLinkedWorkerRefused(t *testing.T) {
	s := rewardStore(t)
	owner, sibling := keyFor(245), keyFor(246)
	register(t, s, owner)
	register(t, s, sibling)
	for _, acct := range []string{keyID(owner), keyID(sibling)} {
		if _, err := s.db.Exec("INSERT INTO write_signals(created_at,operation,account,via,hash_key,ip_hash_24) VALUES(?,'post',?,'http',?,'net-c889')", testTime, acct, s.signals.id); err != nil {
			t.Fatal(err)
		}
	}
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, rewardCreate(s, owner, id, 50, 0))
	fence := claimAndSubmit(t, s, sibling, id, "lobby")
	failsWith(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: id, Amount: fence}), "work_forbidden")
	if creditIn(t, s, keyID(sibling), "remaining") != 0 {
		t.Fatal("a linked worker was paid")
	}
	run(t, s, workCommand(s, owner, Command{Operation: "work.cancel", MessageID: id, Reason: "linked worker"}))
	if r := getTestWork(t, s, id).Reward; r.State != "released" || lastingIn(t, s, keyID(owner)) != 0 || creditIn(t, s, SponsorAccount, "remaining") != 50 {
		t.Fatalf("cancel: %+v", r)
	}
}

// The dogfood case: a fresh hosted identity, no money, hires through MCP.
func TestSponsoredTaskHostedIdentity(t *testing.T) {
	s := hostedRewardStore(t)
	// A hosted identity's free credit is the anonymous share: enough here
	// for its posts and the transfer fee.
	p := s.allowanceDefaults()
	rp := p.Resources[allowance.Credit]
	rp.Budget, rp.SpendCeiling, rp.InboundCap, rp.TransferFee, rp.GrantSharePPM = 100_000, 100_000, 100_000, 2, 500_000
	rp.Floor, rp.Cap, rp.FloorSlicePPM, rp.AnonymousSharePPM = 100, 1000, 1_000_000, 100_000
	if _, err := s.SetAllowanceParams(testContext, ledger.AllowanceNamespace, p.Marshal(), "sponsor test", 0); err != nil {
		t.Fatal(err)
	}
	created := createHosted(t, s, "test-origin", "")
	hosted := hostedKey(t, s, created["token"].(string))
	id := hostedRequest(t, s, hosted)
	res := mustMCP(t, s, rewardCreate(s, hosted, id, 100, 0))
	if ack := res.Data["ack"].(WorkAck); !strings.Contains(ack.Note, "Sponsored:") {
		t.Fatalf("hosted sponsored note: %q", ack.Note)
	}
	if creditIn(t, s, SponsorAccount, "held") != 100 {
		t.Fatal("the house does not hold the hosted identity's reward")
	}
}

func TestUnpaidTaskHint(t *testing.T) {
	s := rewardStore(t)
	owner := keyFor(247)
	register(t, s, owner)
	id := rewardRequest(t, s, owner, "lobby")
	res := run(t, s, workCreateData(s, owner, id, map[string]any{"reward_note": "a thank-you"}))
	// In the ack note, not next.hints: shipped outbox clients refuse extra keys on a work ack.
	if hasHint(res, "unpaid_task") != "" {
		t.Fatal("unpaid text went to next.hints")
	}
	msg := res.Data["ack"].(WorkAck).Note
	if !strings.Contains(msg, "unpaid") || !strings.Contains(msg, "sponsors your first task") {
		t.Fatalf("unpaid note: %q", msg)
	}
	paid := rewardRequest(t, s, owner, "lobby")
	if res = run(t, s, rewardCreate(s, owner, paid, 20, 0)); strings.Contains(res.Data["ack"].(WorkAck).Note, "unpaid") {
		t.Fatal("a rewarded task was called unpaid")
	}
}
