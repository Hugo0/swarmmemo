package board

import (
	"crypto/ed25519"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/ledger"
)

// Work rewards on the real store, ledger and notary: held at create, paid
// once on accept, released on cancel and at the deadline, kept through a
// reject, limited by a credential's spend limit, undecayed, consistent
// across a restart, and scoped like any private work.

func rewardConfig() Config {
	return Config{Features: Features{Services: []string{"notary", "memory"}, Ledger: LedgerOn}}
}

func setRewardParams(t *testing.T, s *Store) {
	t.Helper()
	p := s.allowanceDefaults()
	rp := p.Resources[allowance.Credit]
	rp.Budget, rp.SpendCeiling, rp.InboundCap, rp.TransferFee, rp.GrantSharePPM = 100_000, 100_000, 100_000, 2, 1_000_000
	rp.Cap = []int64{1000, 1000, 1000, 10}
	rp.Floor = []int64{100, 100, 100, 10}
	rp.RootCap = []int64{5000, 5000, 5000, 10}
	if _, err := s.SetAllowanceParams(testContext, ledger.AllowanceNamespace, p.Marshal(), "work reward test", 0); err != nil {
		t.Fatal(err)
	}
}

func rewardStore(t *testing.T) *Store {
	t.Helper()
	if _, _, ok := ledgerTestOverride(); ok {
		t.Skip("the ledger mode is overridden for the whole suite")
	}
	s := openTest(t, rewardConfig())
	t.Cleanup(s.stopServices)
	setRewardParams(t, s)
	return s
}

// mintCredit gives account units of credit in bucket b (a top-up's paid
// credit, or granted credit that decays).
func mintCredit(t *testing.T, s *Store, account string, b allowance.Bucket, units int64) {
	t.Helper()
	tx, err := s.db.BeginTx(testContext, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = s.ledger.led.Mint(testContext, tx, account, allowance.Credit, b, units, "work reward test", s.now().Unix()); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func rewardCreate(s *Store, key ed25519.PrivateKey, id string, reward any, ttl int64) Command {
	d := map[string]any{"schema": 1, "generation": s.generation, "title": "Rewarded review", "capabilities": []string{"review"}, "reward": reward}
	b, _ := json.Marshal(d)
	return signed(key, Command{Operation: "work.create", MessageID: id, TTL: ttl, Data: string(b), Timestamp: s.now().Unix()})
}

func rewardRequest(t *testing.T, s *Store, key ed25519.PrivateKey, room string) string {
	t.Helper()
	return run(t, s, signed(key, Command{Operation: "post", Room: room, Kind: "request", Text: "Review my patch for credits", Timestamp: s.now().Unix()})).Receipt.ID
}

func creditIn(t *testing.T, s *Store, account, column string) int64 {
	t.Helper()
	return sqlCount(t, s, "SELECT coalesce(sum("+column+"),0) FROM ledger_lots WHERE account=? AND resource='credit' AND state='live'", account)
}

// claimAndSubmit has worker claim work id and submit a result; it returns
// the submit's fence.
func claimAndSubmit(t *testing.T, s *Store, worker ed25519.PrivateKey, id, room string) int64 {
	t.Helper()
	ack := run(t, s, workCommand(s, worker, Command{Operation: "work.claim", MessageID: id, TTL: 600})).Data["ack"].(WorkAck)
	result := workResult(t, s, worker, id, room)
	run(t, s, workCommand(s, worker, Command{Operation: "work.submit", MessageID: id, Amount: ack.Fence, Target: result}))
	return ack.Fence
}

func TestWorkRewardHeldAndPaidOnce(t *testing.T) {
	s := rewardStore(t)
	owner, worker := keyFor(140), keyFor(141)
	requester, payee := keyID(owner), keyID(worker)
	mintCredit(t, s, requester, allowance.Paid, 1000)
	id := rewardRequest(t, s, owner, "lobby")

	for _, bad := range []any{0, -5, WorkRewardMax + 1} {
		fails(t, s, rewardCreate(s, owner, id, bad, 0), "invalid_work_reward")
	}
	fails(t, s, rewardCreate(s, owner, id, 1.5, 0), "invalid_work_data")
	fails(t, s, rewardCreate(s, owner, id, 5000, 0), "quota_exhausted")
	sim := run(t, s, signed(owner, Command{Operation: "post", Kind: "simulation", Text: "lab"})).Receipt.ID
	fails(t, s, rewardCreate(s, owner, sim, 10, 0), "invalid_work_reward")
	if n := sqlCount(t, s, "SELECT count(*) FROM works"); n != 0 {
		t.Fatalf("a refused reward left %d works", n)
	}
	run(t, s, rewardCreate(s, owner, id, 600, 0))
	w := getTestWork(t, s, id)
	if w.Reward == nil || w.Reward.Amount != 600 || w.Reward.State != "held" || w.Reward.Fee != 2 || w.Reward.Unit != "credit" {
		t.Fatalf("held reward: %+v", w.Reward)
	}
	if held := creditIn(t, s, requester, "held"); held != 600 {
		t.Fatalf("the ledger holds %d", held)
	}
	// A reward is only on create.
	claim := workCommand(s, worker, Command{Operation: "work.claim", MessageID: id, TTL: 600})
	withReward := claim
	withReward.Data = strings.Replace(claim.Data, `"schema":1`, `"schema":1,"reward":5`, 1)
	withReward.Nonce = ""
	fails(t, s, signed(worker, withReward), "invalid_work_data")

	fence := claimAndSubmit(t, s, worker, id, "lobby")
	// The worker cannot accept its own submission.
	fails(t, s, workCommand(s, worker, Command{Operation: "work.accept", MessageID: id, Amount: fence}), "work_forbidden")
	accept := workCommand(s, owner, Command{Operation: "work.accept", MessageID: id, Amount: fence})
	first, _ := json.Marshal(run(t, s, accept).Data["ack"])
	if got := creditIn(t, s, payee, "remaining"); got != 600 {
		t.Fatalf("the worker received %d", got)
	}
	// An exact retry replays the acknowledgement; a new accept conflicts.
	again, _ := json.Marshal(run(t, s, accept).Data["ack"])
	var firstJSON, againJSON any
	_ = json.Unmarshal(first, &firstJSON)
	_ = json.Unmarshal(again, &againJSON)
	if !reflect.DeepEqual(firstJSON, againJSON) {
		t.Fatalf("retried accept: %s vs %s", again, first)
	}
	fails(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: id, Amount: fence}), "work_state_conflict")
	fails(t, s, workCommand(s, owner, Command{Operation: "work.cancel", MessageID: id, Reason: "after the fact"}), "work_state_conflict")
	if got := creditIn(t, s, payee, "remaining"); got != 600 || sqlCount(t, s, "SELECT count(*) FROM ledger_transfers WHERE to_account=?", payee) != 1 {
		t.Fatalf("paid more than once: %d", got)
	}
	if held := creditIn(t, s, requester, "held"); held != 0 {
		t.Fatalf("a paid reward leaks a hold of %d", held)
	}

	w = getTestWork(t, s, id)
	r := w.Reward
	if r == nil || r.State != "paid" || r.TransferID == "" || r.SettledAt != testTime || r.Receipt == nil {
		t.Fatalf("paid reward: %+v", r)
	}
	if sha256Hex([]byte(r.Receipt.Statement)) != r.Receipt.Hash || r.Receipt.Notary != "/api/notary/"+r.Receipt.Hash {
		t.Fatalf("receipt: %+v", r.Receipt)
	}
	var st workRewardStatement
	if err := json.Unmarshal([]byte(r.Receipt.Statement), &st); err != nil || st.Schema != WorkRewardSchema || st.WorkID != id || st.Worker != payee || st.Requester != requester || st.Amount != 600 || st.TransferID != r.TransferID || st.ResultID != w.ResultID {
		t.Fatalf("statement: %+v %v", st, err)
	}
	stamped := run(t, s, Command{Operation: "service.read", Target: "notary", Data: `{"schema":1,"method":"get","args":{"hash":"` + r.Receipt.Hash + `"}}`})
	if body, _ := json.Marshal(stamped); !strings.Contains(string(body), `"seq"`) || !strings.Contains(string(body), r.Receipt.Hash) {
		t.Fatalf("no notary receipt: %+v", stamped)
	}
	// Reads: the directory, the history, the worker's ledger and journal.
	list := run(t, s, Command{Operation: "works.list", Kind: "accepted"}).Data["works"].([]Work)
	if len(list) != 1 || list[0].Reward == nil || list[0].Reward.State != "paid" {
		t.Fatalf("works.list: %+v", list)
	}
	if h := run(t, s, Command{Operation: "work.history", MessageID: id}).Data["reward"].(*WorkReward); h.State != "paid" {
		t.Fatalf("work.history reward: %+v", h)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM ledger_entries WHERE account=? AND kind='transfer_in' AND op='work_reward' AND public_ref=?", payee, r.TransferID); n != 1 {
		t.Fatalf("worker's journal lines: %d", n)
	}
	b, _, _ := journalOf(t, run(t, s, journalGet(worker, "", 0)))
	items := path(t, b, "open_work", "work", "items").([]any)
	if len(items) != 1 || path(t, items[0], "work", "reward", "state") != "paid" || !strings.Contains(path(t, items[0], "next").(string), "600 credits") {
		t.Fatalf("worker's open_work: %v", items)
	}
	if err := s.Integrity(testContext); err != nil {
		t.Fatal(err)
	}
}

func TestWorkRewardReleasedOnCancelAndExpiryNotReject(t *testing.T) {
	s := rewardStore(t)
	owner, worker := keyFor(142), keyFor(143)
	requester := keyID(owner)
	mintCredit(t, s, requester, allowance.Paid, 1000)
	total := func() int64 { return creditIn(t, s, requester, "remaining") }

	cancelled := rewardRequest(t, s, owner, "lobby")
	run(t, s, rewardCreate(s, owner, cancelled, 300, 0))
	before := total()
	run(t, s, workCommand(s, owner, Command{Operation: "work.cancel", MessageID: cancelled, Reason: "no longer needed"}))
	if r := getTestWork(t, s, cancelled).Reward; r.State != "released" || r.Reason != "cancelled" || creditIn(t, s, requester, "held") != 0 || total() != before {
		t.Fatalf("cancel: %+v, held %d", r, creditIn(t, s, requester, "held"))
	}

	// Reject reopens the work: the reward stays held for the next worker,
	// and the deadline passing releases it.
	rejected := rewardRequest(t, s, owner, "lobby")
	run(t, s, rewardCreate(s, owner, rejected, 200, 3600))
	fence := claimAndSubmit(t, s, worker, rejected, "lobby")
	run(t, s, workCommand(s, owner, Command{Operation: "work.reject", MessageID: rejected, Amount: fence, Reason: "not what I asked"}))
	if r := getTestWork(t, s, rejected).Reward; r.State != "held" || creditIn(t, s, requester, "held") != 200 {
		t.Fatalf("after reject: %+v", r)
	}
	if _, err := s.SweepAllowance(testContext); err != nil {
		t.Fatal(err)
	}
	if r := getTestWork(t, s, rejected).Reward; r.State != "held" {
		t.Fatalf("released before the deadline: %+v", r)
	}
	s.now = func() time.Time { return time.Unix(testTime+3600, 0) }
	if _, err := s.SweepAllowance(testContext); err != nil {
		t.Fatal(err)
	}
	w := getTestWork(t, s, rejected)
	if w.State != "expired" || w.Reward.State != "released" || w.Reward.Reason != "expired" || creditIn(t, s, requester, "held") != 0 {
		t.Fatalf("after the deadline: %s %+v", w.State, w.Reward)
	}
	// Released once: another sweep changes nothing.
	entries := sqlCount(t, s, "SELECT count(*) FROM ledger_entries")
	if _, err := s.SweepAllowance(testContext); err != nil {
		t.Fatal(err)
	}
	if sqlCount(t, s, "SELECT count(*) FROM ledger_entries") != entries {
		t.Fatal("a second sweep wrote to the ledger")
	}
	if fees := sqlCount(t, s, "SELECT coalesce(sum(amount),0) FROM ledger_entries WHERE account=? AND kind='fee' AND op='work_reward'", requester); fees != 4 {
		t.Fatalf("fees %d: the transfer fee is charged once per reward and kept", fees)
	}
	if err := s.Integrity(testContext); err != nil {
		t.Fatal(err)
	}
}

func TestWorkRewardSpendLimitBreakerAndNoDecay(t *testing.T) {
	s := rewardStore(t)
	owner, worker := keyFor(144), keyFor(145)
	requester, payee := keyID(owner), keyID(worker)
	mintCredit(t, s, requester, allowance.Granted, 1000)
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, workCommand(s, owner, Command{Operation: "work.create", MessageID: id, TTL: WorkMaxTTL}))

	// A credential's spend limit applies to the escrow, as to any spend.
	tx, err := s.db.BeginTx(testContext, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ledger.led.SetSpendLimit(testContext, tx, ledger.SpendLimit{Credential: "key:helper", Account: requester, PerDay: ptrInt(100), SetBy: requester}, s.now().Unix()); err != nil {
		t.Fatal(err)
	}
	w, err := scanWork(tx.QueryRowContext(testContext, `SELECT `+workColumns+` FROM works w WHERE id=?`, id))
	if err != nil {
		t.Fatal(err)
	}
	limited := actor{id: requester, account: requester, signed: true, credential: "key:helper"}
	err = s.holdWorkReward(testContext, tx, limited, w, 99, s.now().Unix())
	if !spendLimitIs(err, "credit_per_day 100") {
		t.Fatalf("over the credential's limit: %v", err)
	}
	if err = s.holdWorkReward(testContext, tx, limited, w, 98, s.now().Unix()); err != nil {
		t.Fatalf("within the limit: %v", err)
	}
	_ = tx.Rollback()

	// Granted credit decays daily; held credit does not.
	rewarded := rewardRequest(t, s, owner, "lobby")
	run(t, s, rewardCreate(s, owner, rewarded, 500, WorkMaxTTL))
	for day := int64(1); day <= 20; day++ {
		s.now = func() time.Time { return time.Unix(testTime+day*86400, 0) }
		if _, err = s.SweepAllowance(testContext); err != nil {
			t.Fatal(err)
		}
	}
	if held := creditIn(t, s, requester, "held"); held != 500 {
		t.Fatalf("held credit decayed to %d", held)
	}
	if unheld := creditIn(t, s, requester, "remaining-held"); unheld >= 498 {
		t.Fatalf("the unheld granted credit did not decay: %d", unheld)
	}
	// With the requester's breaker on, the payment waits out the transfer
	// delay, then the sweeper pays it and stamps the receipt.
	fence := claimAndSubmit(t, s, worker, rewarded, "lobby")
	if tx, err = s.db.BeginTx(testContext, nil); err != nil {
		t.Fatal(err)
	}
	if err = s.ledger.led.TripBreaker(testContext, tx, requester, "identity.link", "", s.now().Unix()); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: rewarded, Amount: fence}))
	r := getTestWork(t, s, rewarded).Reward
	if r.State != "pending" || r.ExecuteAt <= s.now().Unix() || creditIn(t, s, payee, "remaining") != 0 {
		t.Fatalf("pending payment: %+v", r)
	}
	at := r.ExecuteAt
	s.now = func() time.Time { return time.Unix(at, 0) }
	if _, err = s.SweepAllowance(testContext); err != nil {
		t.Fatal(err)
	}
	// The moved granted lot keeps its decay clock, so the worker's balance
	// decays from here on; what arrived is the whole reward.
	received := sqlCount(t, s, "SELECT coalesce(sum(amount),0) FROM ledger_entries WHERE account=? AND kind='transfer_in' AND op='work_reward'", payee)
	if r = getTestWork(t, s, rewarded).Reward; r.State != "paid" || r.Receipt == nil || received != 500 {
		t.Fatalf("after the delay: %+v, worker received %d", r, received)
	}
	if err = s.Integrity(testContext); err != nil {
		t.Fatal(err)
	}
}

func ptrInt(v int64) *int64 { return &v }

func TestWorkRewardSurvivesRestartAndPrivateRooms(t *testing.T) {
	if _, _, ok := ledgerTestOverride(); ok {
		t.Skip("the ledger mode is overridden for the whole suite")
	}
	dbPath := filepath.Join(t.TempDir(), "reward.sqlite")
	open := func() *Store {
		s, err := Open(dbPath, rewardConfig())
		if err != nil {
			t.Fatal(err)
		}
		s.now = func() time.Time { return time.Unix(testTime, 0) }
		return s
	}
	owner, worker, outsider := keyFor(146), keyFor(147), keyFor(148)
	requester, payee := keyID(owner), keyID(worker)
	s := open()
	setRewardParams(t, s)
	mintCredit(t, s, requester, allowance.Paid, 1000)
	register(t, s, worker)
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "paid-private", Visibility: "private", Members: []string{payee}}))
	id := rewardRequest(t, s, owner, "paid-private")
	run(t, s, rewardCreate(s, owner, id, 400, 0))
	fence := claimAndSubmit(t, s, worker, id, "paid-private")
	s.stopServices()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = open()
	t.Cleanup(func() { s.stopServices(); _ = s.Close() })
	if err := s.Integrity(testContext); err != nil {
		t.Fatal(err)
	}
	if held := creditIn(t, s, requester, "held"); held != 400 {
		t.Fatalf("after the restart the ledger holds %d", held)
	}
	var holdState string
	if err := s.db.QueryRow("SELECT h.state FROM work_rewards r JOIN ledger_holds h ON h.id=r.hold_id WHERE r.work_id=?", id).Scan(&holdState); err != nil || holdState != "held" {
		t.Fatalf("hold %q %v", holdState, err)
	}
	// Private work stays private, reward included.
	fails(t, s, Command{Operation: "work.get", MessageID: id}, "not_found")
	fails(t, s, signed(outsider, Command{Operation: "work.get", MessageID: id}), "not_found")
	if list := run(t, s, Command{Operation: "works.list"}).Data["works"].([]Work); len(list) != 0 {
		t.Fatalf("private work in public discovery: %+v", list)
	}
	if w := run(t, s, signed(worker, Command{Operation: "work.get", MessageID: id})).Data["work"].(Work); w.Reward == nil || w.Reward.State != "held" || w.Reward.Amount != 400 {
		t.Fatalf("a member's read: %+v", w.Reward)
	}
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: id, Amount: fence}))
	if got := creditIn(t, s, payee, "remaining"); got != 400 {
		t.Fatalf("worker received %d", got)
	}
	if w := run(t, s, signed(worker, Command{Operation: "work.get", MessageID: id})).Data["work"].(Work); w.Reward.State != "paid" {
		t.Fatalf("after accept: %+v", w.Reward)
	}
}

func TestWorkRewardNeedsTheLedger(t *testing.T) {
	if _, _, ok := ledgerTestOverride(); ok {
		t.Skip("the ledger mode is overridden for the whole suite")
	}
	s := openTest(t, Config{})
	owner := keyFor(149)
	id := rewardRequest(t, s, owner, "lobby")
	fails(t, s, rewardCreate(s, owner, id, 10, 0), "service_unavailable")
	run(t, s, workCommand(s, owner, Command{Operation: "work.create", MessageID: id}))
	if getTestWork(t, s, id).Reward != nil {
		t.Fatal("unrewarded work shows a reward")
	}
	var n int
	if err := s.db.QueryRow("SELECT count(*) FROM work_rewards").Scan(&n); err != nil || n != 0 {
		t.Fatal(n, err)
	}
}
