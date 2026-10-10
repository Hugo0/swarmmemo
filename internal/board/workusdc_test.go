package board

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/services"
)

// One reward rail (RFC 0016): a reward object with credits and USDC,
// promised at create, owed on accept, paid only once a settlement is
// verified on a (fake) chain; refusals change nothing; integer rewards
// keep their shape.

const (
	usdcPayee = "0x00000000000000000000000000000000000000aa"
	usdcOther = "0x00000000000000000000000000000000000000bb"
	usdcToken = "0x00000000000000000000000000000000000000cc"
)

type fakeTx struct {
	status    string
	block     int64
	blockTime int64
	logs      []map[string]any
}

// fakeChain is an EVM JSON-RPC endpoint with a few transactions.
type fakeChain struct {
	mu    sync.Mutex
	head  int64
	txs   map[string]fakeTx
	calls int
}

func (f *fakeChain) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	var result any
	switch req.Method {
	case "eth_getTransactionReceipt":
		var h string
		_ = json.Unmarshal(req.Params[0], &h)
		if t, ok := f.txs[h]; ok {
			result = map[string]any{"status": t.status, "blockNumber": fmt.Sprintf("0x%x", t.block), "logs": t.logs}
		}
	case "eth_blockNumber":
		result = fmt.Sprintf("0x%x", f.head)
	case "eth_getBlockByNumber":
		var n string
		_ = json.Unmarshal(req.Params[0], &n)
		for _, t := range f.txs {
			if fmt.Sprintf("0x%x", t.block) == n {
				result = map[string]any{"timestamp": fmt.Sprintf("0x%x", t.blockTime)}
			}
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
}

func (f *fakeChain) add(hash string, t fakeTx) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.txs[hash] = t
}

func transferLog(token, from, to string, value int64) map[string]any {
	pad := func(a string) string {
		return "0x" + strings.Repeat("0", 24) + strings.TrimPrefix(strings.ToLower(a), "0x")
	}
	return map[string]any{"address": token, "topics": []string{"0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef", pad(from), pad(to)},
		"data": "0x" + fmt.Sprintf("%064x", big.NewInt(value))}
}

func txHash(n int) string { return fmt.Sprintf("0x%064x", n) }

// usdcStore is a reward store whose chain reader is a fakeChain.
func usdcStore(t *testing.T) (*Store, *fakeChain) {
	t.Helper()
	if _, _, ok := ledgerTestOverride(); ok {
		t.Skip("the ledger mode is overridden for the whole suite")
	}
	chain := &fakeChain{head: 100, txs: map[string]fakeTx{}}
	srv := httptest.NewServer(chain)
	t.Cleanup(srv.Close)
	reader, err := services.NewUSDCChain("https://rpc.example.org/base")
	if err != nil {
		t.Fatal(err)
	}
	reader.UseTestRPC(srv.Client(), srv.URL)
	c := rewardConfig()
	c.WorkUSDC = reader
	s := openTest(t, c)
	t.Cleanup(s.stopServices)
	setRewardParams(t, s)
	return s, chain
}

func usdcAsset(s *Store) string { return s.config.WorkUSDC.Asset.String() }

func workWith(s *Store, key ed25519.PrivateKey, c Command, extra map[string]any) Command {
	d := map[string]any{"schema": 1, "generation": s.generation}
	for k, v := range extra {
		d[k] = v
	}
	b, _ := json.Marshal(d)
	c.Data = string(b)
	c.Timestamp = s.now().Unix()
	return signed(key, c)
}

func settle(s *Store, key ed25519.PrivateKey, id, hash string) Command {
	return workWith(s, key, Command{Operation: WorkSettle, MessageID: id}, map[string]any{"tx_hash": hash})
}

func TestWorkUSDCRewardOwedThenSettledOnChain(t *testing.T) {
	s, chain := usdcStore(t)
	owner, worker := keyFor(160), keyFor(161)
	requester, payee := keyID(owner), keyID(worker)
	mintCredit(t, s, requester, allowance.Paid, 1000)
	id := rewardRequest(t, s, owner, "lobby")

	// An amount in prose is refused while USDC rewards are on.
	note := rewardCreate(s, owner, id, 10, 0)
	var d map[string]any
	_ = json.Unmarshal([]byte(note.Data), &d)
	d["reward_note"] = "+0.10 USDC on accept"
	b, _ := json.Marshal(d)
	note.Data, note.Nonce = string(b), ""
	fails(t, s, signed(owner, note), "reward_note_amount")

	run(t, s, rewardCreate(s, owner, id, map[string]any{"credits": 600, "usdc": "0.10"}, 0))
	w := getTestWork(t, s, id)
	if w.Reward == nil || w.Reward.State != "held" || w.RewardUSDC == nil || w.RewardUSDC.State != "promised" || w.RewardUSDC.Units != 100_000 || w.RewardUSDC.Amount != "0.1" || w.RewardState != WorkRewardHeld {
		t.Fatalf("created: %+v %+v %q", w.Reward, w.RewardUSDC, w.RewardState)
	}
	if w.RewardUSDC.Network != "eip155:8453" || w.RewardUSDC.Asset != usdcAsset(s) {
		t.Fatalf("asset: %+v", w.RewardUSDC)
	}

	// The submit needs a payout address: none in data, no wallet link.
	ack := run(t, s, workCommand(s, worker, Command{Operation: "work.claim", MessageID: id, TTL: 600})).Data["ack"].(WorkAck)
	result := workResult(t, s, worker, id, "lobby")
	fails(t, s, workCommand(s, worker, Command{Operation: "work.submit", MessageID: id, Amount: ack.Fence, Target: result}), "payout_address_required")
	fails(t, s, workWith(s, worker, Command{Operation: "work.submit", MessageID: id, Amount: ack.Fence, Target: result}, map[string]any{"payout_address": "0x12"}), "invalid_work_data")
	run(t, s, workWith(s, worker, Command{Operation: "work.submit", MessageID: id, Amount: ack.Fence, Target: result}, map[string]any{"payout_address": usdcPayee}))
	if w = getTestWork(t, s, id); !strings.EqualFold(w.RewardUSDC.PayTo, usdcPayee) || w.RewardUSDC.PayToSource != "data" {
		t.Fatalf("payout address: %+v", w.RewardUSDC)
	}
	// Settling before accept: nothing is owed yet.
	fails(t, s, settle(s, owner, id, txHash(1)), "work_state_conflict")

	acked := run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: id, Amount: ack.Fence})).Data["ack"].(WorkAck)
	if !strings.Contains(acked.Note, "0.1 USDC is now owed") || !strings.Contains(acked.Note, "work.settle") {
		t.Fatalf("accept note: %q", acked.Note)
	}
	w = getTestWork(t, s, id)
	if w.State != "accepted" || w.Reward.State != "paid" || w.RewardUSDC.State != "payable" || w.RewardState != WorkRewardPayable || w.RewardReceipt != nil || w.Reward.Receipt != nil {
		t.Fatalf("accepted: %+v %+v %q %+v", w.Reward, w.RewardUSDC, w.RewardState, w.RewardReceipt)
	}
	// The message mark shows each asset.
	page := run(t, s, Command{Operation: "message.get", MessageID: id})
	raw, _ := json.Marshal(page.Messages)
	if !strings.Contains(string(raw), `"reward_usdc":{"amount":"0.1","state":"payable"}`) || !strings.Contains(string(raw), `"reward_state":"payable"`) || !strings.Contains(string(raw), `"state":"paid"`) {
		t.Fatalf("message mark: %s", raw)
	}

	head := chain.head
	payee20, other20 := usdcPayee, usdcOther
	chain.add(txHash(2), fakeTx{status: "0x0", block: head - 10, blockTime: testTime + 50, logs: []map[string]any{transferLog(usdcAsset(s), other20, payee20, 100_000)}})
	chain.add(txHash(3), fakeTx{status: "0x1", block: head, blockTime: testTime + 51, logs: []map[string]any{transferLog(usdcAsset(s), other20, payee20, 100_000)}})
	chain.add(txHash(4), fakeTx{status: "0x1", block: head - 11, blockTime: testTime + 52, logs: []map[string]any{transferLog(usdcAsset(s), other20, other20, 100_000), transferLog(usdcToken, other20, payee20, 100_000)}})
	chain.add(txHash(5), fakeTx{status: "0x1", block: head - 12, blockTime: testTime + 53, logs: []map[string]any{transferLog(usdcAsset(s), other20, payee20, 99_999)}})
	chain.add(txHash(6), fakeTx{status: "0x1", block: head - 13, blockTime: 0, logs: []map[string]any{transferLog(usdcAsset(s), other20, payee20, 100_000)}})
	chain.add(txHash(7), fakeTx{status: "0x1", block: head - 14, blockTime: testTime + 54, logs: []map[string]any{transferLog(usdcAsset(s), other20, payee20, 60_000), transferLog(usdcAsset(s), other20, payee20, 40_000)}})
	chain.add(txHash(8), fakeTx{status: "0x1", block: head - 15, blockTime: testTime + 55, logs: []map[string]any{transferLog(usdcAsset(s), other20, payee20, 100_000)}})

	fails(t, s, settle(s, worker, id, txHash(7)), "work_forbidden")
	calls := chain.calls
	fails(t, s, settle(s, keyFor(162), id, txHash(7)), "work_forbidden")
	if chain.calls != calls {
		t.Fatal("a key that is not the requester made the board read the chain")
	}
	fails(t, s, settle(s, owner, id, "0xABC"), "invalid_work_data")
	fails(t, s, settle(s, owner, id, txHash(1)), "settle_tx_not_found")
	fails(t, s, settle(s, owner, id, txHash(2)), "settle_tx_failed")
	fails(t, s, settle(s, owner, id, txHash(3)), "settle_unconfirmed")
	fails(t, s, settle(s, owner, id, txHash(4)), "settle_wrong_recipient")
	fails(t, s, settle(s, owner, id, txHash(5)), "settle_amount_short")
	fails(t, s, settle(s, owner, id, txHash(6)), "settle_tx_before_work")
	if w = getTestWork(t, s, id); w.RewardUSDC.State != "payable" || w.RewardUSDC.TxHash != "" {
		t.Fatalf("a refused settle changed the reward: %+v", w.RewardUSDC)
	}

	// Two transfers in one transaction add up to the amount owed.
	settled := run(t, s, settle(s, owner, id, txHash(7))).Data["ack"].(WorkAck)
	if settled.State != "accepted" || !strings.Contains(settled.Note, "settled by "+txHash(7)) {
		t.Fatalf("settle ack: %+v", settled)
	}
	w = getTestWork(t, s, id)
	if w.RewardUSDC.State != "paid" || w.RewardUSDC.TxHash != txHash(7) || w.RewardUSDC.PaidAmount != "0.1" || w.RewardUSDC.Block != head-14 || !strings.EqualFold(w.RewardUSDC.Payer, other20) || w.RewardState != WorkRewardPaid {
		t.Fatalf("paid: %+v %q", w.RewardUSDC, w.RewardState)
	}
	if w.RewardReceipt == nil || !strings.Contains(w.RewardReceipt.Statement, `"schema":"swarmmemo-work-reward/2"`) || !strings.Contains(w.RewardReceipt.Statement, `"unit":"credit","amount":"600"`) || !strings.Contains(w.RewardReceipt.Statement, `"tx_hash":"`+txHash(7)+`"`) || !strings.Contains(w.RewardReceipt.Statement, `"note":"sender_unlinked"`) || w.RewardUSDC.Reason != "sender_unlinked" || w.RewardReceipt.Hash != sha256Hex([]byte(w.RewardReceipt.Statement)) {
		t.Fatalf("receipt: %+v", w.RewardReceipt)
	}
	if !strings.Contains(w.RewardReceipt.Statement, `"worker":"`+payeeAccount(t, s, payee)+`"`) {
		t.Fatalf("receipt worker: %s", w.RewardReceipt.Statement)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM notary_receipts WHERE hash=?", w.RewardReceipt.Hash); n != 1 {
		t.Fatalf("notary receipts for the reward: %d", n)
	}
	history := run(t, s, Command{Operation: "work.history", MessageID: id}).Data
	transitions := history["transitions"].([]WorkTransition)
	last := transitions[len(transitions)-1]
	if last.Operation != WorkSettle || last.State != "accepted" || !strings.Contains(last.SignedPayload, txHash(7)) || history["reward_state"] != WorkRewardPaid || history["reward_usdc"].(*WorkRewardUSDC).State != "paid" {
		t.Fatalf("history: %+v %v", last, history["reward_state"])
	}
	// Idempotent by hash; a second payment is not recorded.
	again := run(t, s, settle(s, owner, id, txHash(7))).Data["ack"].(WorkAck)
	if !strings.Contains(again.Note, "Already settled") {
		t.Fatalf("again: %+v", again)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM work_transitions WHERE work_id=? AND operation='work.settle'", id); n != 1 {
		t.Fatalf("settle transitions: %d", n)
	}
	fails(t, s, settle(s, owner, id, txHash(8)), "work_already_paid")

	// One transaction settles one item.
	id2 := rewardRequest(t, s, owner, "lobby")
	run(t, s, rewardCreate(s, owner, id2, map[string]any{"usdc": "0.10"}, 0))
	f2 := run(t, s, workWith(s, worker, Command{Operation: "work.claim", MessageID: id2, TTL: 600}, map[string]any{"payout_address": usdcPayee})).Data["ack"].(WorkAck).Fence
	r2 := workResult(t, s, worker, id2, "lobby")
	run(t, s, workCommand(s, worker, Command{Operation: "work.submit", MessageID: id2, Amount: f2, Target: r2}))
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: id2, Amount: f2}))
	if w = getTestWork(t, s, id2); w.Reward != nil || w.RewardUSDC.PayToSource != "data" || w.RewardState != WorkRewardPayable {
		t.Fatalf("usdc only: %+v %+v %q", w.Reward, w.RewardUSDC, w.RewardState)
	}
	fails(t, s, settle(s, owner, id2, txHash(7)), "settle_tx_used")
	run(t, s, settle(s, owner, id2, txHash(8)))
	if w = getTestWork(t, s, id2); w.RewardState != WorkRewardPaid || w.RewardReceipt == nil || strings.Contains(w.RewardReceipt.Statement, `"unit":"credit"`) {
		t.Fatalf("usdc only paid: %q %+v", w.RewardState, w.RewardReceipt)
	}
}

func payeeAccount(t *testing.T, s *Store, fingerprint string) string {
	t.Helper()
	var account string
	if err := s.db.QueryRow("SELECT account FROM identities WHERE id=?", fingerprint).Scan(&account); err != nil {
		t.Fatal(err)
	}
	return account
}

func TestWorkUSDCRefusalsVoidAndWalletLink(t *testing.T) {
	s, _ := usdcStore(t)
	owner, worker := keyFor(163), keyFor(164)
	requester := keyID(owner)
	mintCredit(t, s, requester, allowance.Paid, 1000)
	id := rewardRequest(t, s, owner, "lobby")
	for _, bad := range []any{map[string]any{}, map[string]any{"usdc": "0.001"}, map[string]any{"usdc": 0.1}, map[string]any{"usdc": "2000"},
		map[string]any{"usdc": "0.0000001"}, map[string]any{"credits": 0, "usdc": "1"}, map[string]any{"usdc": "1", "eth": "1"}, map[string]any{"usdc": "-1"}} {
		fails(t, s, rewardCreate(s, owner, id, bad, 0), "invalid_work_reward")
	}
	sim := run(t, s, signed(owner, Command{Operation: "post", Kind: "simulation", Text: "lab"})).Receipt.ID
	fails(t, s, rewardCreate(s, owner, sim, map[string]any{"usdc": "1"}, 0), "invalid_work_reward")
	if n := sqlCount(t, s, "SELECT count(*) FROM work_usdc"); n != 0 {
		t.Fatalf("refusals left %d USDC rows", n)
	}

	// Credit-only work: no USDC fields, a payout address is refused, settle
	// has nothing to settle.
	plain := rewardRequest(t, s, owner, "lobby")
	run(t, s, rewardCreate(s, owner, plain, 50, 0))
	if w := getTestWork(t, s, plain); w.RewardUSDC != nil || w.RewardState != WorkRewardHeld {
		t.Fatalf("credit only: %+v %q", w.RewardUSDC, w.RewardState)
	}
	fails(t, s, workWith(s, worker, Command{Operation: "work.claim", MessageID: plain, TTL: 600}, map[string]any{"payout_address": usdcPayee}), "invalid_work_data")
	fence := claimAndSubmit(t, s, worker, plain, "lobby")
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: plain, Amount: fence}))
	if w := getTestWork(t, s, plain); w.Reward.Receipt == nil || !strings.Contains(w.Reward.Receipt.Statement, WorkRewardSchema) || w.RewardState != WorkRewardPaid {
		t.Fatalf("credit receipt unchanged: %+v %q", w.Reward, w.RewardState)
	}
	fails(t, s, settle(s, owner, plain, txHash(9)), "no_usdc_reward")

	// The worker's verified wallet link is the payout address; a reject
	// clears it.
	wallet := "0x00000000000000000000000000000000000000dd"
	run(t, s, signed(worker, Command{Operation: "agent.register", Timestamp: s.now().Unix()}))
	if _, err := s.db.Exec("INSERT INTO identity_links(agent,kind,value,state,created_at,checked_at) VALUES(?,'wallet',?,'verified',1,1)", keyID(worker), wallet); err != nil {
		t.Fatal(err)
	}
	run(t, s, rewardCreate(s, owner, id, map[string]any{"usdc": "0.25"}, 0))
	result := workResult(t, s, worker, id, "lobby")
	ack := run(t, s, workCommand(s, worker, Command{Operation: "work.claim", MessageID: id, Target: result})).Data["ack"].(WorkAck)
	if w := getTestWork(t, s, id); w.RewardUSDC.PayTo != wallet || w.RewardUSDC.PayToSource != "wallet_link" {
		t.Fatalf("wallet link payout: %+v", w.RewardUSDC)
	}
	run(t, s, workCommand(s, owner, Command{Operation: "work.reject", MessageID: id, Amount: ack.Fence, Reason: "not yet"}))
	if w := getTestWork(t, s, id); w.RewardUSDC.PayTo != "" || w.RewardUSDC.State != "promised" {
		t.Fatalf("after reject: %+v", w.RewardUSDC)
	}
	run(t, s, workCommand(s, owner, Command{Operation: "work.cancel", MessageID: id, Reason: "done"}))
	if w := getTestWork(t, s, id); w.RewardUSDC.State != "void" || w.RewardUSDC.Reason != "cancelled" || w.RewardState != WorkRewardReleased {
		t.Fatalf("cancelled: %+v %q", w.RewardUSDC, w.RewardState)
	}

	// The deadline voids an unaccepted promise; the sweeper records it.
	late := rewardRequest(t, s, owner, "lobby")
	run(t, s, rewardCreate(s, owner, late, map[string]any{"credits": 5, "usdc": "1"}, 60))
	base := s.now()
	s.now = func() time.Time { return base.Add(2 * time.Minute) }
	if w := getTestWork(t, s, late); w.RewardUSDC.State != "void" {
		t.Fatalf("read before the sweep: %+v", w.RewardUSDC)
	}
	if _, err := s.SweepAllowance(testContext); err != nil {
		t.Fatal(err)
	}
	if w := getTestWork(t, s, late); w.RewardUSDC.State != "void" || w.RewardUSDC.Reason != "expired" || w.Reward.State != "released" || w.RewardState != WorkRewardReleased {
		t.Fatalf("swept: %+v %+v %q", w.RewardUSDC, w.Reward, w.RewardState)
	}
}

func TestWorkUSDCNeedsTheChainReader(t *testing.T) {
	s := rewardStore(t)
	owner := keyFor(165)
	mintCredit(t, s, keyID(owner), allowance.Paid, 1000)
	id := rewardRequest(t, s, owner, "lobby")
	fails(t, s, rewardCreate(s, owner, id, map[string]any{"credits": 5, "usdc": "1"}, 0), "usdc_rewards_unavailable")
	// Without USDC rewards a note may still name an amount, as before.
	c := rewardCreate(s, owner, id, map[string]any{"credits": 5}, 0)
	var d map[string]any
	_ = json.Unmarshal([]byte(c.Data), &d)
	d["reward_note"] = "+0.10 USDC on Base, paid by the poster"
	b, _ := json.Marshal(d)
	c.Data, c.Nonce = string(b), ""
	run(t, s, signed(owner, c))
	if w := getTestWork(t, s, id); w.Reward.Amount != 5 || w.RewardUSDC != nil {
		t.Fatalf("object with credits only: %+v", w)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM work_usdc"); n != 0 {
		t.Fatalf("work_usdc rows: %d", n)
	}
}

func TestWorkRewardState(t *testing.T) {
	for _, c := range []struct {
		states []string
		want   string
	}{
		{nil, ""}, {[]string{"held", ""}, "held"}, {[]string{"held", "promised"}, "held"}, {[]string{"paid", "payable"}, "payable"},
		{[]string{"pending", "paid"}, "payable"}, {[]string{"paid", "paid"}, "paid"}, {[]string{"", "paid"}, "paid"},
		{[]string{"released", "void"}, "released"}, {[]string{"released", "paid"}, "partly_paid"},
	} {
		if got := workRewardState(c.states...); got != c.want {
			t.Errorf("%v: %q, want %q", c.states, got, c.want)
		}
	}
}

// A requester with a verified wallet link pays from it: a transfer from any
// other address does not settle; one from the linked wallet does, with no
// sender_unlinked note.
func TestWorkUSDCSenderMustBeALinkedWallet(t *testing.T) {
	s, chain := usdcStore(t)
	owner, worker := keyFor(166), keyFor(167)
	linked := "0x00000000000000000000000000000000000000ee"
	run(t, s, signed(owner, Command{Operation: "agent.register", Timestamp: s.now().Unix()}))
	if _, err := s.db.Exec("INSERT INTO identity_links(agent,kind,value,state,created_at,checked_at) VALUES(?,'wallet',?,'verified',1,1)", keyID(owner), linked); err != nil {
		t.Fatal(err)
	}
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, rewardCreate(s, owner, id, map[string]any{"usdc": "0.10"}, 0))
	result := workResult(t, s, worker, id, "lobby")
	fence := run(t, s, workWith(s, worker, Command{Operation: "work.claim", MessageID: id, Target: result}, map[string]any{"payout_address": usdcPayee})).Data["ack"].(WorkAck).Fence
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: id, Amount: fence}))
	chain.add(txHash(20), fakeTx{status: "0x1", block: 80, blockTime: testTime + 5, logs: []map[string]any{transferLog(usdcAsset(s), usdcOther, usdcPayee, 100_000)}})
	chain.add(txHash(21), fakeTx{status: "0x1", block: 81, blockTime: testTime + 6, logs: []map[string]any{transferLog(usdcAsset(s), usdcOther, usdcPayee, 50_000), transferLog(usdcAsset(s), linked, usdcPayee, 60_000)}})
	chain.add(txHash(22), fakeTx{status: "0x1", block: 82, blockTime: testTime + 7, logs: []map[string]any{transferLog(usdcAsset(s), linked, usdcPayee, 100_000)}})
	fails(t, s, settle(s, owner, id, txHash(20)), "settle_sender_unlinked")
	// Only the linked wallet's transfers count toward the amount.
	fails(t, s, settle(s, owner, id, txHash(21)), "settle_amount_short")
	run(t, s, settle(s, owner, id, txHash(22)))
	w := getTestWork(t, s, id)
	if w.RewardUSDC.State != "paid" || w.RewardUSDC.Reason != "" || !strings.EqualFold(w.RewardUSDC.Payer, linked) || (w.RewardReceipt != nil && strings.Contains(w.RewardReceipt.Statement, "sender_unlinked")) {
		t.Fatalf("linked settle: %+v %+v", w.RewardUSDC, w.RewardReceipt)
	}
}
