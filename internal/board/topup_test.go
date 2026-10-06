package board

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/ledger"
	"swarmmemo/internal/services"
)

// Credit top-ups on the real engine (SQLite, ledger on) with a fake x402
// facilitator: the board as an x402 server.

const (
	topupUSDC  = "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913"
	topupPayTo = "0x1111111111111111111111111111111111111111"
	topupPayer = "0x2222222222222222222222222222222222222222"
)

// fakeFacilitator is an x402 facilitator: /verify and /settle, each
// answering what the test sets, counting calls.
type fakeFacilitator struct {
	mu             sync.Mutex
	verifies       int
	settles        int
	verifyStatus   int
	verifyBody     string
	settleStatus   int
	settleBody     string
	lastSettleBody map[string]any
	txCounter      int
}

func (f *fakeFacilitator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/verify":
		f.verifies++
		status, out := f.verifyStatus, f.verifyBody
		if status == 0 {
			status, out = 200, `{"isValid":true,"payer":"`+topupPayer+`"}`
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(out))
	case "/settle":
		f.settles++
		f.lastSettleBody = body
		status, out := f.settleStatus, f.settleBody
		if status == 0 {
			f.txCounter++
			status, out = 200, fmt.Sprintf(`{"success":true,"transaction":"0x%064x","network":"eip155:8453","payer":"%s"}`, f.txCounter, topupPayer)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(out))
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeFacilitator) set(fn func(f *fakeFacilitator)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeFacilitator) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.verifies, f.settles
}

func topupConfig(t *testing.T, f *fakeFacilitator, limits string) *services.TopupConfig {
	t.Helper()
	if limits == "" {
		limits = `{}`
	}
	cfg, err := services.ParseTopupConfig([]byte(`{"schema":1,"enabled":true,"network":"eip155:8453","asset":"`+topupUSDC+`","asset_name":"USD Coin","asset_version":"2",
"pay_to":"`+topupPayTo+`","facilitator_url":"https://facilitator.example.com","limits":`+limits+`}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	cfg.UseTestFacilitator(srv.Client(), srv.URL)
	return cfg
}

func topupStore(t *testing.T, path string, cfg *services.TopupConfig) *Store {
	t.Helper()
	if _, _, ok := ledgerTestOverride(); ok {
		t.Skip("the ledger mode is overridden for the whole suite")
	}
	s, err := Open(path, Config{Features: Features{Ledger: LedgerOn}, Topup: cfg, EchoSimulate: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.now = func() time.Time { return time.Unix(testTime, 0) }
	return s
}

func topupExec(s *Store, ctx context.Context, c Command) (Result, *Error) {
	res, err := s.Execute(ctx, c, "test-origin")
	var e *Error
	if err != nil && !errors.As(err, &e) {
		e = &Error{Code: "unexpected: " + err.Error()}
	}
	return res, e
}

// quote asks for amount credits and returns the requirement the 402 offers,
// read from its PAYMENT-REQUIRED value.
func quote(t *testing.T, s *Store, key ed25519.PrivateKey, amount int64) map[string]any {
	t.Helper()
	_, e := topupExec(s, testContext, signed(key, Command{Operation: "credits.topup", Amount: amount}))
	if e == nil || e.Code != "payment_required" || e.Status != 402 {
		t.Fatalf("quote: want 402 payment_required, got %+v", e)
	}
	header := e.Details.(map[string]any)["payment_required"].(string)
	raw, err := base64.StdEncoding.DecodeString(header)
	if err != nil {
		t.Fatal(err)
	}
	var required struct {
		Version int              `json:"x402Version"`
		Accepts []map[string]any `json:"accepts"`
	}
	if err = json.Unmarshal(raw, &required); err != nil || required.Version != 2 || len(required.Accepts) != 1 {
		t.Fatalf("PAYMENT-REQUIRED %s: %v", raw, err)
	}
	return required.Accepts[0]
}

// pay is the base64 payment payload for accepted, its authorization
// changed by edit.
func pay(t *testing.T, accepted map[string]any, nonce byte, edit func(auth map[string]any)) string {
	t.Helper()
	auth := map[string]any{"from": topupPayer, "to": accepted["payTo"], "value": accepted["amount"],
		"validAfter": fmt.Sprint(testTime - 60), "validBefore": fmt.Sprint(testTime + 600), "nonce": "0x" + strings.Repeat(fmt.Sprintf("%02x", nonce), 32)}
	if edit != nil {
		edit(auth)
	}
	payload := map[string]any{"x402Version": 2, "accepted": accepted, "payload": map[string]any{"signature": "0x" + strings.Repeat("ab", 65), "authorization": auth}}
	raw, _ := json.Marshal(payload)
	return base64.StdEncoding.EncodeToString(raw)
}

func paidCommand(key ed25519.PrivateKey, amount int64, payment string) Command {
	return signed(key, Command{Operation: "credits.topup", Amount: amount, Data: `{"schema":1,"payment":"` + payment + `"}`})
}

func paidBalance(t *testing.T, s *Store, key ed25519.PrivateKey) int64 {
	t.Helper()
	return sqlCount(t, s, "SELECT coalesce(sum(remaining),0) FROM ledger_lots WHERE account=? AND bucket='paid' AND resource='credit' AND state='live'", keyID(key))
}

func TestTopupDisabled(t *testing.T) {
	key := keyFor(90)
	off := topupStore(t, filepath.Join(t.TempDir(), "off.sqlite"), nil)
	register(t, off, key)
	fails(t, off, signed(key, Command{Operation: "credits.topup", Amount: 1_000_000}), "topup_unavailable")
	fails(t, off, signed(key, Command{Operation: "credits.topups"}), "topup_unavailable")
	fails(t, off, Command{Operation: "credits.topup", Amount: 1_000_000}, "signature_required")
	if off.TopupEnabled() || off.TopupCapabilities() != nil || off.Features().Topup {
		t.Fatal("top-ups on without a config")
	}
	// Configured, but the ledger is off: still off.
	s, err := Open(filepath.Join(t.TempDir(), "noledger.sqlite"), Config{Topup: topupConfig(t, &fakeFacilitator{}, "")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.TopupEnabled() || s.Features().Topup {
		t.Fatal("top-ups on with the ledger off")
	}
}

func TestTopupCreditsExactlyOnce(t *testing.T) {
	f := &fakeFacilitator{}
	s := topupStore(t, filepath.Join(t.TempDir(), "b.sqlite"), topupConfig(t, f, ""))
	key := keyFor(91)
	register(t, s, key)
	caps := s.TopupCapabilities()
	if caps["pay_to"] != topupPayTo || caps["withdrawal"] != false || !s.Features().Topup {
		t.Fatalf("capabilities %v", caps)
	}
	accepted := quote(t, s, key, 1_000_000)
	if accepted["scheme"] != "exact" || accepted["network"] != "eip155:8453" || accepted["amount"] != "1000000" || accepted["payTo"] != topupPayTo || !strings.EqualFold(accepted["asset"].(string), topupUSDC) {
		t.Fatalf("requirement %v", accepted)
	}
	if sqlCount(t, s, "SELECT count(*) FROM credit_topups") != 0 {
		t.Fatal("a quote wrote a row")
	}
	payment := pay(t, accepted, 1, nil)
	cmd := signed(key, Command{Operation: "credits.topup", Amount: 1_000_000, Data: `{"schema":1,"payment":"` + payment + `"}`, RequestID: "topup-1"})
	res := run(t, s, cmd)
	receipt := res.Data["topup"].(map[string]any)
	if receipt["state"] != "credited" || receipt["amount"] != int64(1_000_000) || receipt["payer"] != topupPayer || !strings.HasPrefix(receipt["transaction"].(string), "0x") {
		t.Fatalf("receipt %v", receipt)
	}
	if res.Data["payment_response"] == nil {
		t.Fatal("no PAYMENT-RESPONSE value")
	}
	if got := paidBalance(t, s, key); got != 1_000_000 {
		t.Fatalf("paid balance %d", got)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM ledger_entries WHERE kind='topup' AND bucket='paid' AND amount=1000000 AND public_ref=?", receipt["id"]); n != 1 {
		t.Fatalf("%d topup journal entries", n)
	}
	// The lot never decays and never expires.
	if n := sqlCount(t, s, "SELECT count(*) FROM ledger_lots WHERE bucket='paid' AND half_life_days=0 AND expires_at=0"); n != 1 {
		t.Fatalf("paid lot decays or expires (%d)", n)
	}
	// The facilitator saw our requirement, not the payer's echo of it.
	reqs := f.lastSettleBody["paymentRequirements"].(map[string]any)
	if reqs["payTo"] != topupPayTo || reqs["amount"] != "1000000" {
		t.Fatalf("settled against %v", reqs)
	}
	// An exact retry returns the top-up as it stands; nothing settles again.
	again := run(t, s, cmd)
	if again.Data["topup"].(map[string]any)["state"] != "credited" {
		t.Fatalf("retry %v", again.Data)
	}
	if _, settles := f.counts(); settles != 1 {
		t.Fatalf("%d settlements", settles)
	}
	if got := paidBalance(t, s, key); got != 1_000_000 {
		t.Fatalf("paid balance after retry %d", got)
	}
	list := run(t, s, signed(key, Command{Operation: "credits.topups"}))
	if tops := list.Data["topups"].([]map[string]any); len(tops) != 1 || tops[0]["transaction"] != receipt["transaction"] {
		t.Fatalf("credits.topups %v", list.Data)
	}
	// The header carries the same payment (HTTP).
	accepted = quote(t, s, key, 200_000)
	ctx := WithPayment(testContext, pay(t, accepted, 2, nil))
	if res, e := topupExec(s, ctx, signed(key, Command{Operation: "credits.topup", Amount: 200_000})); e != nil || res.Data["topup"].(map[string]any)["state"] != "credited" {
		t.Fatalf("header payment: %+v %v", e, res.Data)
	}
	if got := paidBalance(t, s, key); got != 1_200_000 {
		t.Fatalf("paid balance %d", got)
	}
}

func TestTopupReplayIsNotCreditedTwice(t *testing.T) {
	f := &fakeFacilitator{}
	s := topupStore(t, filepath.Join(t.TempDir(), "b.sqlite"), topupConfig(t, f, ""))
	key, other := keyFor(92), keyFor(93)
	register(t, s, key)
	register(t, s, other)
	accepted := quote(t, s, key, 500_000)
	payment := pay(t, accepted, 3, nil)
	run(t, s, paidCommand(key, 500_000, payment))
	// The same payment in a new command (new nonce, no request_id).
	if _, e := topupExec(s, testContext, paidCommand(key, 500_000, payment)); e == nil || e.Code != "payment_replayed" || e.Status != 409 {
		t.Fatalf("replay: %+v", e)
	}
	// By another agent: the quote is not theirs.
	if _, e := topupExec(s, testContext, paidCommand(other, 500_000, payment)); e == nil || e.Code != "payment_mismatch" {
		t.Fatalf("replay by another agent: %+v", e)
	}
	// Another agent's own quote, the same authorization: refused on the nonce.
	theirs := quote(t, s, other, 500_000)
	if _, e := topupExec(s, testContext, paidCommand(other, 500_000, pay(t, theirs, 3, nil))); e == nil || e.Code != "payment_replayed" {
		t.Fatalf("authorization replayed under a new quote: %+v", e)
	}
	if paidBalance(t, s, key) != 500_000 || paidBalance(t, s, other) != 0 {
		t.Fatal("a replay was credited")
	}
	if _, settles := f.counts(); settles != 1 {
		t.Fatalf("%d settlements", settles)
	}
}

func TestTopupRefusesAnythingButTheQuote(t *testing.T) {
	f := &fakeFacilitator{}
	s := topupStore(t, filepath.Join(t.TempDir(), "b.sqlite"), topupConfig(t, f, ""))
	key := keyFor(94)
	register(t, s, key)
	accepted := quote(t, s, key, 300_000)
	with := func(k string, v any) map[string]any {
		out := map[string]any{}
		for key, value := range accepted {
			out[key] = value
		}
		out[k] = v
		return out
	}
	cases := []struct {
		name    string
		amount  int64
		payment string
		code    string
	}{
		{"value below the amount", 300_000, pay(t, accepted, 10, func(a map[string]any) { a["value"] = "299999" }), "payment_mismatch"},
		{"accepted amount changed", 300_000, pay(t, with("amount", "1"), 11, func(a map[string]any) { a["value"] = "1" }), "payment_mismatch"},
		{"another amount than quoted", 400_000, pay(t, with("amount", "400000"), 12, func(a map[string]any) { a["value"] = "400000" }), "payment_mismatch"},
		{"another asset", 300_000, pay(t, with("asset", "0x3333333333333333333333333333333333333333"), 13, nil), "payment_mismatch"},
		{"another network", 300_000, pay(t, with("network", "eip155:84532"), 14, nil), "payment_mismatch"},
		{"another payTo", 300_000, pay(t, with("payTo", "0x4444444444444444444444444444444444444444"), 15, nil), "payment_mismatch"},
		{"authorization to someone else", 300_000, pay(t, accepted, 16, func(a map[string]any) { a["to"] = "0x4444444444444444444444444444444444444444" }), "payment_mismatch"},
		{"another scheme", 300_000, pay(t, with("scheme", "upto"), 17, nil), "payment_mismatch"},
		{"forged quote", 300_000, pay(t, with("extra", map[string]any{"name": "USD Coin", "version": "2", "quote": "9999999999.AAAA.BBBB"}), 18, nil), "payment_mismatch"},
		{"authorization expired", 300_000, pay(t, accepted, 19, func(a map[string]any) { a["validBefore"] = fmt.Sprint(testTime + 5) }), "payment_expired"},
		{"authorization not yet valid", 300_000, pay(t, accepted, 20, func(a map[string]any) { a["validAfter"] = fmt.Sprint(testTime + 60) }), "payment_expired"},
		{"not base64", 300_000, "%%%", "payment_invalid"},
		{"v1 payload", 300_000, base64.StdEncoding.EncodeToString([]byte(`{"x402Version":1,"scheme":"exact","network":"base","payload":{}}`)), "payment_invalid"},
		{"short signature", 300_000, edited(pay(t, accepted, 21, nil), strings.Repeat("ab", 65), "abab"), "payment_invalid"},
		{"unknown authorization field", 300_000, edited(pay(t, accepted, 23, nil), `"nonce":`, `"extra":1,"nonce":`), "payment_invalid"},
		{"duplicate key", 300_000, edited(pay(t, accepted, 24, nil), `"x402Version":2`, `"x402Version":2,"x402Version":2`), "payment_invalid"},
	}
	for _, c := range cases {
		if _, e := topupExec(s, testContext, paidCommand(key, c.amount, c.payment)); e == nil || e.Code != c.code {
			t.Errorf("%s: want %s, got %+v", c.name, c.code, e)
		}
	}
	if v, settles := f.counts(); v != 0 || settles != 0 {
		t.Fatalf("a refused payment reached the facilitator (%d verify, %d settle)", v, settles)
	}
	if sqlCount(t, s, "SELECT count(*) FROM credit_topups") != 0 || paidBalance(t, s, key) != 0 {
		t.Fatal("a refused payment left a row or credit")
	}
	// The quote itself expires.
	s.now = func() time.Time { return time.Unix(testTime+services.TopupQuoteSeconds+1, 0) }
	late := pay(t, accepted, 22, func(a map[string]any) { a["validBefore"] = fmt.Sprint(testTime + 3600) })
	lateCmd := signed(key, Command{Operation: "credits.topup", Amount: 300_000, Data: `{"schema":1,"payment":"` + late + `"}`, Timestamp: testTime + services.TopupQuoteSeconds + 1})
	if _, e := topupExec(s, testContext, lateCmd); e == nil || e.Code != "payment_expired" {
		t.Fatalf("expired quote: %+v", e)
	}
}

// edited is payment with old replaced by new in its JSON.
func edited(payment, old, new string) string {
	raw, _ := base64.StdEncoding.DecodeString(payment)
	return base64.StdEncoding.EncodeToString([]byte(strings.Replace(string(raw), old, new, 1)))
}

func TestTopupFacilitatorFailureCreditsNothing(t *testing.T) {
	f := &fakeFacilitator{}
	s := topupStore(t, filepath.Join(t.TempDir(), "b.sqlite"), topupConfig(t, f, ""))
	key := keyFor(95)
	register(t, s, key)
	state := func(nonce byte) (string, string) {
		var st, reason string
		if err := s.db.QueryRow("SELECT state,reason FROM credit_topups WHERE auth_nonce=?", "0x"+strings.Repeat(fmt.Sprintf("%02x", nonce), 32)).Scan(&st, &reason); err != nil {
			t.Fatal(err)
		}
		return st, reason
	}
	// The facilitator says the payment is invalid: nothing moved.
	f.set(func(f *fakeFacilitator) {
		f.verifyStatus, f.verifyBody = 200, `{"isValid":false,"invalidReason":"insufficient_funds"}`
	})
	p1 := pay(t, quote(t, s, key, 100_000), 30, nil)
	if _, e := topupExec(s, testContext, paidCommand(key, 100_000, p1)); e == nil || e.Code != "payment_rejected" || e.Status != 402 {
		t.Fatalf("invalid payment: %+v", e)
	}
	if st, reason := state(30); st != "failed" || reason != "insufficient_funds" {
		t.Fatalf("rejected row %s %s", st, reason)
	}
	// The facilitator is down at verify: nothing moved, and the same
	// payment may be presented again once it is back.
	f.set(func(f *fakeFacilitator) { f.verifyStatus, f.verifyBody = 503, `{}` })
	accepted := quote(t, s, key, 100_000)
	p2 := pay(t, accepted, 31, nil)
	if _, e := topupExec(s, testContext, paidCommand(key, 100_000, p2)); e == nil || e.Code != "facilitator_unavailable" || e.Status != 503 {
		t.Fatalf("facilitator down: %+v", e)
	}
	if st, _ := state(31); st != "failed" {
		t.Fatalf("unverified row %s", st)
	}
	// Settlement fails with a server error: unknown, never credited.
	f.set(func(f *fakeFacilitator) { f.verifyStatus, f.settleStatus, f.settleBody = 0, 500, `{"error":"boom"}` })
	p3 := pay(t, quote(t, s, key, 100_000), 32, nil)
	if _, e := topupExec(s, testContext, paidCommand(key, 100_000, p3)); e == nil || e.Code != "payment_unsettled" || e.Status != 502 {
		t.Fatalf("settle error: %+v", e)
	}
	if st, _ := state(32); st != "unknown" {
		t.Fatalf("unsettled row %s", st)
	}
	// "Settled", but with no transaction we can record: unknown too.
	f.set(func(f *fakeFacilitator) { f.settleStatus, f.settleBody = 200, `{"success":true,"transaction":"nope"}` })
	p4 := pay(t, quote(t, s, key, 100_000), 33, nil)
	if _, e := topupExec(s, testContext, paidCommand(key, 100_000, p4)); e == nil || e.Code != "payment_unsettled" {
		t.Fatalf("malformed settlement: %+v", e)
	}
	// Settlement refused outright: failed.
	f.set(func(f *fakeFacilitator) {
		f.settleStatus, f.settleBody = 400, `{"success":false,"errorReason":"invalid_exact_evm_payload_authorization_nonce_used"}`
	})
	p5 := pay(t, quote(t, s, key, 100_000), 34, nil)
	if _, e := topupExec(s, testContext, paidCommand(key, 100_000, p5)); e == nil || e.Code != "payment_rejected" {
		t.Fatalf("settlement refused: %+v", e)
	}
	if paidBalance(t, s, key) != 0 || sqlCount(t, s, "SELECT count(*) FROM ledger_entries WHERE kind='topup'") != 0 {
		t.Fatal("a failed payment was credited")
	}
	// The facilitator is back: the payment that could not be verified goes
	// through once; the rejected and unknown ones stay refused.
	f.set(func(f *fakeFacilitator) { f.settleStatus, f.settleBody = 0, "" })
	if res, e := topupExec(s, testContext, paidCommand(key, 100_000, p2)); e != nil || res.Data["topup"].(map[string]any)["state"] != "credited" {
		t.Fatalf("payment presented again: %+v", e)
	}
	for _, p := range []string{p1, p3, p5} {
		if _, e := topupExec(s, testContext, paidCommand(key, 100_000, p)); e == nil || e.Code != "payment_replayed" {
			t.Fatalf("a failed or unknown payment presented again: %+v", e)
		}
	}
	if paidBalance(t, s, key) != 100_000 {
		t.Fatalf("paid balance %d", paidBalance(t, s, key))
	}
}

func TestTopupLimits(t *testing.T) {
	f := &fakeFacilitator{}
	s := topupStore(t, filepath.Join(t.TempDir(), "b.sqlite"), topupConfig(t, f, `{"min":"0.5","max":"1","account_daily":"1.5"}`))
	key := keyFor(96)
	register(t, s, key)
	for _, amount := range []int64{499_999, 1_000_001, 0, -1} {
		fails(t, s, signed(key, Command{Operation: "credits.topup", Amount: amount}), "topup_amount")
	}
	run(t, s, paidCommand(key, 1_000_000, pay(t, quote(t, s, key, 1_000_000), 40, nil)))
	// 1 + 1 > 1.5: refused before a quote.
	_, e := topupExec(s, testContext, signed(key, Command{Operation: "credits.topup", Amount: 1_000_000}))
	if e == nil || e.Code != "topup_daily_limit" || e.Status != 429 || e.RetryAfter <= 0 {
		t.Fatalf("daily cap: %+v", e)
	}
	// A quote taken before the cap filled is refused at payment too.
	accepted := quote(t, s, key, 500_000)
	run(t, s, paidCommand(key, 500_000, pay(t, accepted, 41, nil)))
	if _, e := topupExec(s, testContext, signed(key, Command{Operation: "credits.topup", Amount: 500_000})); e == nil || e.Code != "topup_daily_limit" {
		t.Fatalf("cap reached: %+v", e)
	}
	if paidBalance(t, s, key) != 1_500_000 {
		t.Fatalf("paid balance %d", paidBalance(t, s, key))
	}
	// The next UTC day the cap is fresh.
	s.now = func() time.Time { return time.Unix(testTime+86400, 0) }
	if _, e := topupExec(s, testContext, signed(key, Command{Operation: "credits.topup", Amount: 500_000, Timestamp: testTime + 86400})); e == nil || e.Code != "payment_required" {
		t.Fatalf("next day: %+v", e)
	}
	// Config bounds.
	for _, limits := range []string{`{"min":"2","max":"1"}`, `{"max":"200","account_daily":"100"}`, `{"account_daily":"1000.000001"}`, `{"min":"0"}`, `{"min":"abc"}`} {
		if _, err := services.ParseTopupConfig([]byte(`{"schema":1,"enabled":true,"network":"eip155:8453","asset":"`+topupUSDC+`","asset_name":"USD Coin","asset_version":"2","pay_to":"`+topupPayTo+`","facilitator_url":"https://f.example.com","limits":`+limits+`}`), nil); err == nil {
			t.Errorf("limits %s accepted", limits)
		}
	}
}

func TestTopupRestartIsIdempotent(t *testing.T) {
	f := &fakeFacilitator{}
	cfg := topupConfig(t, f, "")
	path := filepath.Join(t.TempDir(), "b.sqlite")
	s := topupStore(t, path, cfg)
	key := keyFor(98)
	register(t, s, key)
	payment := pay(t, quote(t, s, key, 700_000), 50, nil)
	cmd := signed(key, Command{Operation: "credits.topup", Amount: 700_000, Data: `{"schema":1,"payment":"` + payment + `"}`, RequestID: "restart-1"})
	run(t, s, cmd)
	// A row a crash left settling.
	if _, err := s.db.Exec(`INSERT INTO credit_topups(id,account,agent,amount,day,network,asset,pay_to,payer,auth_nonce,valid_before,state,created_at)
VALUES('tu_crashed',?,?,300000,?,'eip155:8453',?,?,?,'0xdead',?,'settling',?)`, keyID(key), keyID(key), testTime/86400, topupUSDC, topupPayTo, topupPayer, testTime+600, testTime); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = topupStore(t, path, cfg)
	// The same payment, a new command and the exact retry: no second credit.
	if _, e := topupExec(s, testContext, paidCommand(key, 700_000, payment)); e == nil || e.Code != "payment_replayed" {
		t.Fatalf("replay after restart: %+v", e)
	}
	if res := run(t, s, cmd); res.Data["topup"].(map[string]any)["state"] != "credited" {
		t.Fatalf("retry after restart %v", res.Data)
	}
	if _, settles := f.counts(); settles != 1 || paidBalance(t, s, key) != 700_000 {
		t.Fatalf("%d settlements, paid %d", settles, paidBalance(t, s, key))
	}
	// The interrupted one waits for the operator, uncredited.
	var st string
	if err := s.db.QueryRow("SELECT state FROM credit_topups WHERE id='tu_crashed'").Scan(&st); err != nil || st != "unknown" {
		t.Fatalf("interrupted row %s %v", st, err)
	}
	unknown, err := s.UnknownTopups(testContext)
	if err != nil || len(unknown) != 1 {
		t.Fatalf("unknown %v %v", unknown, err)
	}
	// Its transaction hash cannot be one already credited.
	var credited string
	if err = s.db.QueryRow("SELECT tx_hash FROM credit_topups WHERE state='credited'").Scan(&credited); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ResolveTopup(testContext, "tu_crashed", "credit", credited); err == nil {
		t.Fatal("one transaction credited twice")
	}
	tx := "0x" + strings.Repeat("cd", 32)
	if v, err := s.ResolveTopup(testContext, "tu_crashed", "credit", tx); err != nil || v["state"] != "credited" {
		t.Fatalf("resolve: %v %v", v, err)
	}
	if _, err = s.ResolveTopup(testContext, "tu_crashed", "credit", tx); err == nil {
		t.Fatal("resolved twice")
	}
	if paidBalance(t, s, key) != 1_000_000 {
		t.Fatalf("paid balance %d", paidBalance(t, s, key))
	}
}

// Paid credit is spendable credit: a spend draws it down, and nothing
// else touches it (no decay, no expiry).
func TestTopupPaidCreditIsSpendable(t *testing.T) {
	f := &fakeFacilitator{}
	s := topupStore(t, filepath.Join(t.TempDir(), "b.sqlite"), topupConfig(t, f, ""))
	key := keyFor(99)
	register(t, s, key)
	run(t, s, paidCommand(key, 1_000_000, pay(t, quote(t, s, key, 1_000_000), 60, nil)))
	subj := allowance.Subject{ID: keyID(key), KeyID: keyID(key), Signed: true}
	if _, err := s.ledger.led.Spend(testContext, s.db, subj, allowance.Credit, 300_000, ledger.Ref{Service: "test", Op: "spend"}, testTime); err != nil {
		t.Fatal(err)
	}
	if got := paidBalance(t, s, key); got != 700_000 {
		t.Fatalf("paid balance after a spend %d", got)
	}
	if _, err := s.SweepAllowance(testContext); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Unix(testTime+90*86400, 0) }
	if _, err := s.SweepAllowance(testContext); err != nil {
		t.Fatal(err)
	}
	if got := paidBalance(t, s, key); got != 700_000 {
		t.Fatalf("paid credit decayed to %d", got)
	}
}
