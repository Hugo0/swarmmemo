package services

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/services/servicestest"
)

// A throwaway key (Hardhat's first development account, public knowledge);
// nothing here ever reaches a chain.
const x402TestKey = "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"

const (
	testPayTo = "0x209693Bc6afc0C5328bA36FaF03C514EF312287C"
	testUSDC  = "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913"
)

// fakeX402 is a resource server with its facilitator folded in: it answers
// 402 with requirements, verifies the EIP-3009 signature exactly as a
// facilitator would (domain, recipient, amount, window, nonce reuse), and
// then serves the resource.
type fakeX402 struct {
	mu       sync.Mutex
	price    int64
	version  int           // 1 or 2
	reject   string        // a facilitator errorReason to refuse every payment with
	slowPaid time.Duration // delay before answering a paid request
	bigPaid  int           // bytes of a paid response
	free     bool          // answer 200 without asking for payment
	nonces   map[string]int
	paid     int
	payHdrs  int
	lastQ    string
	lastBody string
}

func (f *fakeX402) requirement() map[string]any {
	req := map[string]any{"scheme": "exact", "network": "eip155:8453", "amount": strconv.FormatInt(f.price, 10), "asset": testUSDC,
		"payTo": testPayTo, "maxTimeoutSeconds": 3600, "extra": map[string]any{"name": "USD Coin", "version": "2"}}
	if f.version == 1 {
		delete(req, "amount")
		req["maxAmountRequired"] = strconv.FormatInt(f.price, 10)
		req["network"] = "base"
		req["resource"] = "https://example.com/paid"
	}
	return req
}

func (f *fakeX402) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.lastQ = r.URL.RawQuery
	if r.Body != nil {
		b := make([]byte, 4096)
		n, _ := r.Body.Read(b)
		f.lastBody = string(b[:n])
	}
	price, version, reject, slow, big, free := f.price, f.version, f.reject, f.slowPaid, f.bigPaid, f.free
	f.mu.Unlock()
	if free {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"free":true}`))
		return
	}
	header := r.Header.Get("PAYMENT-SIGNATURE")
	if version == 1 {
		header = r.Header.Get("X-PAYMENT")
	}
	ask := func(reason string) {
		required := map[string]any{"x402Version": version, "error": "payment required", "accepts": []any{f.requirement()}}
		raw, _ := json.Marshal(required)
		if reason != "" {
			settle, _ := json.Marshal(map[string]any{"success": false, "errorReason": reason, "transaction": "", "network": "eip155:8453"})
			w.Header().Set("PAYMENT-RESPONSE", base64.StdEncoding.EncodeToString(settle))
		}
		if version == 2 {
			w.Header().Set("PAYMENT-REQUIRED", base64.StdEncoding.EncodeToString(raw))
			raw = []byte("{}")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write(raw)
	}
	if header == "" {
		ask("")
		return
	}
	f.mu.Lock()
	f.payHdrs++
	f.mu.Unlock()
	reason := f.verify(header, price, version)
	if reason == "" && reject != "" {
		reason = reject
	}
	if reason != "" {
		ask(reason)
		return
	}
	if slow > 0 {
		time.Sleep(slow)
	}
	f.mu.Lock()
	f.paid++
	f.mu.Unlock()
	settle, _ := json.Marshal(map[string]any{"success": true, "transaction": "0x" + strings.Repeat("ab", 32), "network": "eip155:8453"})
	w.Header().Set("PAYMENT-RESPONSE", base64.StdEncoding.EncodeToString(settle))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if big > 0 {
		_, _ = w.Write([]byte(`{"x":"` + strings.Repeat("y", big) + `"}`))
		return
	}
	_, _ = w.Write([]byte(`{"price":{"bitcoin":{"usd":61234.5}}}`))
}

// verify is the facilitator's check; it returns an x402 errorReason or "".
func (f *fakeX402) verify(header string, price int64, version int) string {
	raw, err := base64.StdEncoding.DecodeString(header)
	if err != nil {
		return "invalid_payload"
	}
	var p struct {
		Version  int             `json:"x402Version"`
		Accepted json.RawMessage `json:"accepted"`
		Scheme   string          `json:"scheme"`
		Network  string          `json:"network"`
		Payload  struct {
			Signature     string `json:"signature"`
			Authorization struct {
				From, To, Value, ValidAfter, ValidBefore, Nonce string
			} `json:"authorization"`
		} `json:"payload"`
	}
	if json.Unmarshal(raw, &p) != nil || p.Version != version {
		return "invalid_x402_version"
	}
	if version == 2 && !strings.Contains(string(p.Accepted), `"payTo":"`+testPayTo+`"`) {
		return "invalid_payment_requirements"
	}
	if version == 1 && (p.Scheme != "exact" || p.Network != "base") {
		return "invalid_scheme"
	}
	a := p.Payload.Authorization
	from, ok1 := ParseEVMAddress(a.From)
	to, ok2 := ParseEVMAddress(a.To)
	value, _ := strconv.ParseInt(a.Value, 10, 64)
	after, _ := strconv.ParseInt(a.ValidAfter, 10, 64)
	before, _ := strconv.ParseInt(a.ValidBefore, 10, 64)
	nonceRaw, err := hex.DecodeString(strings.TrimPrefix(a.Nonce, "0x"))
	sigRaw, err2 := hex.DecodeString(strings.TrimPrefix(p.Payload.Signature, "0x"))
	if !ok1 || !ok2 || err != nil || err2 != nil || len(nonceRaw) != 32 || len(sigRaw) != 65 {
		return "invalid_payload"
	}
	if to.String() != testPayTo {
		return "invalid_exact_evm_payload_recipient_mismatch"
	}
	if value != price {
		return "invalid_exact_evm_payload_authorization_value_mismatch"
	}
	now := time.Now().Unix()
	if after > now || before < now+6 || before-now > 300 {
		return "invalid_exact_evm_payload_authorization_valid_before"
	}
	auth := TransferAuthorization{From: from, To: to, Value: value, ValidAfter: after, ValidBefore: before}
	copy(auth.Nonce[:], nonceRaw)
	var sig [65]byte
	copy(sig[:], sigRaw)
	usdc, _ := ParseEVMAddress(testUSDC)
	signer, ok := recoverAddress(auth.Digest(EIP712Domain{Name: "USD Coin", Version: "2", ChainID: 8453, VerifyingContract: usdc}), sig)
	if !ok || signer != from {
		return "invalid_exact_evm_payload_signature"
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nonces[a.Nonce]++
	if f.nonces[a.Nonce] > 1 {
		return "nonce_already_used"
	}
	return ""
}

type x402Harness struct {
	t      *testing.T
	fake   *fakeX402
	srv    *httptest.Server
	db     *sql.DB
	meter  *servicestest.Meter
	engine *Engine
	cfg    *X402Config
	dials  atomic.Int64
	now    int64
	seq    int
}

func testX402Config(t *testing.T, caps string, resources string) *X402Config {
	t.Helper()
	key, _ := hex.DecodeString(x402TestKey)
	signer, err := signerFromKeyBytes(key)
	if err != nil {
		t.Fatal(err)
	}
	config := `{"schema":1,"enabled":true,"network":"eip155:8453","asset":"` + testUSDC + `","asset_name":"USD Coin","asset_version":"2","decimals":6,
"caps":` + caps + `,"wallet_key_file":"unused","allowlist_file":"unused"}`
	allowlist := `{"schema":1,"version":7,"resources":` + resources + `}`
	cfg, err := ParseX402Config([]byte(config), []byte(allowlist), signer)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

const testResources = `[
 {"id":"price","url":"https://example.com/paid","method":"GET","pay_to":"` + testPayTo + `","max_price":"0.01","query":["ids","vs"],"timeout_seconds":2,"summary":"coin prices","source":"test"},
 {"id":"search","url":"https://example.com/paid?fixed=1","method":"POST","pay_to":"` + testPayTo + `","max_price":"0.02","body":true,"max_response_bytes":1024,"timeout_seconds":2}
]`

func newX402Harness(t *testing.T, fake *fakeX402, caps string) *x402Harness {
	t.Helper()
	return newX402HarnessWith(t, fake, caps, testResources)
}

func newX402HarnessWith(t *testing.T, fake *fakeX402, caps, resources string) *x402Harness {
	t.Helper()
	if fake.nonces == nil {
		fake.nonces = map[string]int{}
	}
	if fake.version == 0 {
		fake.version = 2
	}
	h := &x402Harness{t: t, fake: fake, now: 1_700_000_000}
	h.srv = httptest.NewTLSServer(fake)
	t.Cleanup(h.srv.Close)
	h.cfg = testX402Config(t, caps, resources)
	h.cfg.rootCAs = h.srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err = db.Exec(Schema); err != nil {
		t.Fatal(err)
	}
	h.db = db
	target := h.srv.Listener.Addr().String()
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		// Stands in for the webhook dialer: every allowlisted host is the fake.
		h.dials.Add(1)
		if addr != "example.com:443" {
			return nil, errors.New("unexpected address " + addr)
		}
		return (&net.Dialer{}).DialContext(ctx, network, target)
	}
	reg := NewBuiltinRegistry([]string{"x402"}, Deps{DB: db, Dial: dial, X402: h.cfg})
	h.meter = servicestest.NewMeter(1 << 40)
	h.engine = NewEngine(Config{DB: db, Registry: reg, Meter: h.meter, Now: func() int64 { return h.now }})
	t.Cleanup(h.engine.Stop)
	return h
}

func onlyPrice(max string) string {
	return `[{"id":"price","url":"https://example.com/paid","method":"GET","pay_to":"` + testPayTo + `","max_price":"` + max + `","timeout_seconds":2}]`
}

var testSubject = allowance.Subject{ID: "acct-1", KeyID: "k1", Signed: true}

// call runs one service.call x402 through the engine: quote and reserve in a
// transaction, then the remote run after commit.
func (h *x402Harness) call(subject allowance.Subject, args string, maxCost int64) (map[string]any, error) {
	h.seq++
	return h.callKey(subject, args, maxCost, fmt.Sprintf("id:%d", h.seq))
}

func (h *x402Harness) callKey(subject allowance.Subject, args string, maxCost int64, key string) (map[string]any, error) {
	ctx := context.Background()
	tx, err := h.db.Begin()
	if err != nil {
		h.t.Fatal(err)
	}
	out, err := h.engine.Call(ctx, tx, Request{Service: "x402", Data: fmt.Sprintf(`{"schema":1,"method":"call","args":%s,"max_cost":%d}`, args, maxCost), Subject: subject, RequestKey: key}, h.now)
	if err != nil {
		tx.Rollback()
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		h.t.Fatal(err)
	}
	if out.After == nil {
		h.t.Fatalf("x402 must run after commit: %+v", out)
	}
	return out.After()
}

func (h *x402Harness) payments() []map[string]any {
	rows, err := h.db.Query("SELECT account, amount, state, reason, transaction_hash, nonce, day FROM x402_payments ORDER BY created_at, id")
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var account, state, reason, tx, nonce string
		var amount, day int64
		if err := rows.Scan(&account, &amount, &state, &reason, &tx, &nonce, &day); err != nil {
			h.t.Fatal(err)
		}
		out = append(out, map[string]any{"account": account, "amount": amount, "state": state, "reason": reason, "tx": tx, "nonce": nonce, "day": day})
	}
	return out
}

// charged is the credits the fake ledger kept for the account: committed
// holds only (a refunded hold is zero).
func (h *x402Harness) charged() int64 {
	entries, err := h.meter.Entries(context.Background(), h.db)
	if err != nil {
		h.t.Fatal(err)
	}
	var n int64
	for _, e := range entries {
		if e.State == "committed" {
			n += e.Units
		}
	}
	return n
}

func errCode(err error) string {
	var e *allowance.Err
	if errors.As(err, &e) {
		return e.Code
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

// The price of an x402 call in credits: 100 + amount + 100 per started 1,024
// micro-USDC (version 0 of the "x402.call" parameter).
func creditsFor(micro int64) int64 { return 100 + micro + 100*((micro+1023)/1024) }

func TestX402PaysAndReturns(t *testing.T) {
	for _, version := range []int{2, 1} {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			h := newX402Harness(t, &fakeX402{price: 1500, version: version}, `{}`)
			maxCost := creditsFor(10000) // the allowlisted maximum, $0.01
			out, err := h.call(testSubject, `{"resource":"price","query":{"ids":"bitcoin","vs":"usd"}}`, maxCost)
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			raw, _ := json.Marshal(out)
			var got struct {
				Call   CallRecord
				Result x402Out
			}
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if got.Call.State != "done" || got.Call.Cost != creditsFor(1500) || got.Call.MaxCost != maxCost {
				t.Fatalf("call record: %s", raw)
			}
			r := got.Result
			if r.Resource != "price" || r.Status != 200 || r.Encoding != "json" || string(r.Body) != `{"price":{"bitcoin":{"usd":61234.5}}}` || r.AllowlistVersion != 7 || r.ContentType != "application/json" {
				t.Fatalf("result: %s", raw)
			}
			if p := r.Payment; p == nil || p.Amount != "1500" || p.Price != "0.0015" || p.PayTo != testPayTo || p.Payer != "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266" ||
				p.Transaction != "0x"+strings.Repeat("ab", 32) || p.X402Version != version || p.Network != "eip155:8453" {
				t.Fatalf("receipt: %s", raw)
			}
			if h.fake.paid != 1 || h.fake.lastQ != "ids=bitcoin&vs=usd" {
				t.Fatalf("paid %d, query %q", h.fake.paid, h.fake.lastQ)
			}
			if h.charged() != creditsFor(1500) {
				t.Fatalf("charged %d", h.charged())
			}
			pays := h.payments()
			if len(pays) != 1 || pays[0]["state"] != "paid" || pays[0]["amount"] != int64(1500) || pays[0]["tx"] != "0x"+strings.Repeat("ab", 32) {
				t.Fatalf("payments: %v", pays)
			}
			if h.dials.Load() != 2 {
				t.Fatalf("every request must go through the dialer: %d", h.dials.Load())
			}
		})
	}
}

func TestX402PostBodyAndFixedQuery(t *testing.T) {
	h := newX402Harness(t, &fakeX402{price: 3000}, `{}`)
	if _, err := h.call(testSubject, `{"resource":"search","body":{"q":"agents"}}`, creditsFor(20000)); err != nil {
		t.Fatal(err)
	}
	if h.fake.lastQ != "fixed=1" || h.fake.lastBody != `{"q":"agents"}` {
		t.Fatalf("query %q body %q", h.fake.lastQ, h.fake.lastBody)
	}
}

func TestX402ArgumentsAreAllowlisted(t *testing.T) {
	h := newX402Harness(t, &fakeX402{price: 1000}, `{}`)
	for args, want := range map[string]string{
		`{"resource":"nope"}`:                                                                       "x402_unknown_resource",
		`{"resource":"price","query":{"evil":"1"}}`:                                                 "invalid_service_data",
		`{"resource":"price","body":{"a":1}}`:                                                       "invalid_service_data",
		`{"resource":"search","query":{"fixed":"2"}}`:                                               "invalid_service_data",
		`{"resource":"search","body":[1]}`:                                                          "invalid_service_data",
		`{"resource":"price","url":"https://evil.test/"}`:                                           "invalid_service_data",
		`{"resource":"price","query":{"ids":1}}`:                                                    "invalid_service_data",
		`{"resource":"price","query":{"ids":"` + strings.Repeat("a", X402QueryValueBytes+1) + `"}}`: "invalid_service_data",
	} {
		if _, err := h.call(testSubject, args, 1<<30); errCode(err) != want {
			t.Errorf("%s: got %v, want %s", args, err, want)
		}
	}
	if h.fake.payHdrs != 0 || h.dials.Load() != 0 {
		t.Fatal("a refused call must not reach the network")
	}
}

func TestX402PriceOverMax(t *testing.T) {
	// The agent's max_cost below the allowlisted maximum: refused before
	// anything runs or is reserved.
	h := newX402Harness(t, &fakeX402{price: 1000}, `{}`)
	if _, err := h.call(testSubject, `{"resource":"price"}`, creditsFor(10000)-1); errCode(err) != "price_exceeds_max" {
		t.Fatalf("max_cost below the quote: %v", err)
	}
	if h.dials.Load() != 0 {
		t.Fatal("nothing may be fetched")
	}
	// The upstream asks more than the allowlisted maximum: nothing signed,
	// nothing paid, nothing charged.
	h.fake.price = 10001
	if _, err := h.call(testSubject, `{"resource":"price"}`, creditsFor(10000)); errCode(err) != "x402_price_changed" {
		t.Fatalf("upstream price above the allowlist: %v", err)
	}
	if h.fake.payHdrs != 0 || len(h.payments()) != 0 || h.charged() != 0 {
		t.Fatalf("signed or charged: %d %v %d", h.fake.payHdrs, h.payments(), h.charged())
	}
}

func TestX402FacilitatorRejection(t *testing.T) {
	h := newX402Harness(t, &fakeX402{price: 1000, reject: "insufficient_funds"}, `{}`)
	if _, err := h.call(testSubject, `{"resource":"price"}`, creditsFor(10000)); errCode(err) != "x402_payment_rejected" {
		t.Fatalf("rejection: %v", err)
	}
	if h.fake.payHdrs != 1 {
		t.Fatalf("a rejected payment is never retried: %d payment headers", h.fake.payHdrs)
	}
	pays := h.payments()
	if len(pays) != 1 || pays[0]["state"] != "rejected" || pays[0]["reason"] != "insufficient_funds" || h.charged() != 0 {
		t.Fatalf("payments %v charged %d", pays, h.charged())
	}
	// A rejected authorization still counts against the caps (fail closed).
	var spent int64
	_ = h.db.QueryRow("SELECT SUM(amount) FROM x402_payments").Scan(&spent)
	if spent != 1000 {
		t.Fatalf("spent %d", spent)
	}
}

func TestX402NeverPaysTwice(t *testing.T) {
	h := newX402Harness(t, &fakeX402{price: 1000}, `{}`)
	if _, err := h.callKey(testSubject, `{"resource":"price"}`, creditsFor(10000), "id:same"); err != nil {
		t.Fatal(err)
	}
	// The engine answers an exact retry from the call record; even a provider
	// run for the same request key cannot sign a second authorization.
	x := h.engine.Registry().providers["x402"].(*x402)
	c := Call{Service: "x402", Method: "call", Args: json.RawMessage(`{"resource":"price"}`), Subject: testSubject, RequestKey: "id:same", Now: h.now, Price: DefaultPrices()["x402.call"]}
	if _, err := x.Run(context.Background(), nil, c); errCode(err) != "upstream_failed" {
		t.Fatalf("second run for one request key: %v", err)
	}
	if h.fake.paid != 1 || h.fake.payHdrs != 1 || len(h.payments()) != 1 {
		t.Fatalf("paid twice: %d %d %v", h.fake.paid, h.fake.payHdrs, h.payments())
	}
	// Distinct calls use distinct nonces; the fake facilitator saw no reuse.
	for range 3 {
		if _, err := h.call(testSubject, `{"resource":"price"}`, creditsFor(10000)); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[any]bool{}
	for _, p := range h.payments() {
		if seen[p["nonce"]] {
			t.Fatalf("nonce reused: %v", p["nonce"])
		}
		seen[p["nonce"]] = true
	}
	for n, c := range h.fake.nonces {
		if c != 1 {
			t.Fatalf("nonce %s presented %d times", n, c)
		}
	}
}

func TestX402ReplayedNonceIsRejectedNotRetried(t *testing.T) {
	// The upstream claims the nonce was used (a replay, or a lying server):
	// the call fails, is refunded, and no second authorization is signed.
	h := newX402Harness(t, &fakeX402{price: 1000, reject: "nonce_already_used"}, `{}`)
	if _, err := h.call(testSubject, `{"resource":"price"}`, creditsFor(10000)); errCode(err) != "x402_payment_rejected" {
		t.Fatalf("replay: %v", err)
	}
	if h.fake.payHdrs != 1 || len(h.payments()) != 1 || h.charged() != 0 {
		t.Fatalf("retried or charged: %d %v %d", h.fake.payHdrs, h.payments(), h.charged())
	}
}

func TestX402Timeout(t *testing.T) {
	h := newX402Harness(t, &fakeX402{price: 1000, slowPaid: 3 * time.Second}, `{}`)
	start := time.Now()
	if _, err := h.call(testSubject, `{"resource":"price"}`, creditsFor(10000)); errCode(err) != "service_unavailable" && errCode(err) != "upstream_failed" {
		t.Fatalf("timeout: %v", err)
	}
	if d := time.Since(start); d > 2800*time.Millisecond {
		t.Fatalf("the resource timeout (2 s) was not applied: %s", d)
	}
	pays := h.payments()
	if len(pays) != 1 || pays[0]["state"] != "unknown" || h.charged() != 0 {
		t.Fatalf("a timed-out payment is unknown, counted, and not charged: %v %d", pays, h.charged())
	}
}

func TestX402OversizedResponse(t *testing.T) {
	h := newX402Harness(t, &fakeX402{price: 1000, bigPaid: 2000}, `{}`)
	// "search" allows 1,024 bytes. We paid, so the call is charged and its
	// answer discarded (security review 1.20, M1).
	out, err := h.call(testSubject, `{"resource":"search"}`, creditsFor(20000))
	if err != nil {
		t.Fatalf("oversized: %v", err)
	}
	result := resultOf(out)
	if result["encoding"] != "discarded" || result["body"] != nil || result["payment"] == nil {
		t.Fatalf("oversized: %v", result)
	}
	pays := h.payments()
	if len(pays) != 1 || pays[0]["state"] != "paid" || pays[0]["reason"] != "response_too_large" || h.charged() != creditsFor(1000) {
		t.Fatalf("oversized: %v %d", pays, h.charged())
	}
}

func TestX402Caps(t *testing.T) {
	h := newX402HarnessWith(t, &fakeX402{price: 4000}, `{"per_call":"0.01","agent_daily":"0.01","global_daily":"0.015"}`, onlyPrice("0.01"))
	other := allowance.Subject{ID: "acct-2", KeyID: "k2", Signed: true}
	if _, err := h.call(testSubject, `{"resource":"price"}`, creditsFor(10000)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.call(testSubject, `{"resource":"price"}`, creditsFor(10000)); err != nil {
		t.Fatal(err)
	}
	// 8,000 spent by acct-1; 4,000 more would pass 10,000.
	if _, err := h.call(testSubject, `{"resource":"price"}`, creditsFor(10000)); errCode(err) != "x402_cap_reached" {
		t.Fatalf("agent cap: %v", err)
	}
	if _, err := h.call(other, `{"resource":"price"}`, creditsFor(10000)); err != nil {
		t.Fatal(err)
	}
	// 12,000 spent in all; 4,000 more would pass the global 15,000.
	if _, err := h.call(other, `{"resource":"price"}`, creditsFor(10000)); errCode(err) != "x402_cap_reached" {
		t.Fatalf("global cap: %v", err)
	}
	if h.fake.paid != 3 || h.fake.payHdrs != 3 {
		t.Fatalf("paid %d", h.fake.paid)
	}
	// A new UTC day resets the daily caps.
	h.now += 86400
	if _, err := h.call(testSubject, `{"resource":"price"}`, creditsFor(10000)); err != nil {
		t.Fatalf("next day: %v", err)
	}
}

func TestX402CapsHoldUnderConcurrency(t *testing.T) {
	h := newX402HarnessWith(t, &fakeX402{price: 1000}, `{"per_call":"0.002","agent_daily":"0.002","global_daily":"0.005"}`, onlyPrice("0.002"))
	x := h.engine.Registry().providers["x402"].(*x402)
	var wg sync.WaitGroup
	var ok atomic.Int64
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := allowance.Subject{ID: fmt.Sprintf("acct-%d", i), Signed: true}
			c := Call{Service: "x402", Method: "call", Args: json.RawMessage(`{"resource":"price"}`), Subject: s, RequestKey: "id:c", Now: h.now, Price: DefaultPrices()["x402.call"]}
			if _, err := x.Run(context.Background(), nil, c); err == nil {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	var spent int64
	_ = h.db.QueryRow("SELECT COALESCE(SUM(amount),0) FROM x402_payments").Scan(&spent)
	if spent > 5000 || ok.Load() > 5 || h.fake.paid > 5 {
		t.Fatalf("the global cap was exceeded: spent %d, ok %d, paid %d", spent, ok.Load(), h.fake.paid)
	}
}

func TestX402KillSwitch(t *testing.T) {
	h := newX402Harness(t, &fakeX402{price: 1000}, `{}`)
	kill := filepath.Join(t.TempDir(), "x402.off")
	h.cfg.KillFile = kill
	if err := os.WriteFile(kill, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.call(testSubject, `{"resource":"price"}`, creditsFor(10000)); errCode(err) != "service_unavailable" {
		t.Fatalf("kill file: %v", err)
	}
	if h.dials.Load() != 0 || h.charged() != 0 {
		t.Fatal("a paused relay must not fetch or charge")
	}
	_ = os.Remove(kill)
	if _, err := h.call(testSubject, `{"resource":"price"}`, creditsFor(10000)); err != nil {
		t.Fatalf("resumed: %v", err)
	}
	h.cfg.Paused = true
	if _, err := h.call(testSubject, `{"resource":"price"}`, creditsFor(10000)); errCode(err) != "service_unavailable" {
		t.Fatalf("paused flag: %v", err)
	}
}

func TestX402FreeResource(t *testing.T) {
	h := newX402Harness(t, &fakeX402{free: true}, `{}`)
	out, err := h.call(testSubject, `{"resource":"price"}`, creditsFor(10000))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out)
	if !strings.Contains(string(raw), `"body":{"free":true}`) || strings.Contains(string(raw), `"payment"`) || h.charged() != 100 || len(h.payments()) != 0 {
		t.Fatalf("free: %s charged %d", raw, h.charged())
	}
}

func TestX402Unconfigured(t *testing.T) {
	for name, d := range map[string]Deps{
		"no config": {DB: &sql.DB{}, Dial: (&net.Dialer{}).DialContext},
		"no dialer": {DB: &sql.DB{}, X402: &X402Config{Signer: &keySigner{}}},
		"no db":     {Dial: (&net.Dialer{}).DialContext, X402: &X402Config{Signer: &keySigner{}}},
	} {
		p := newX402(d)
		if _, err := p.Quote(Call{Args: json.RawMessage(`{"resource":"price"}`)}); errCode(err) != "service_unavailable" {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestX402ResourcesRead(t *testing.T) {
	h := newX402Harness(t, &fakeX402{price: 2000}, `{}`)
	if _, err := h.call(testSubject, `{"resource":"price"}`, creditsFor(10000)); err != nil {
		t.Fatal(err)
	}
	got, err := h.engine.Read(context.Background(), h.db, Request{Service: "x402", Data: `{"schema":1,"method":"resources"}`, Subject: testSubject}, h.now)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(got)
	for _, want := range []string{`"id":"price"`, `"max_price":"0.01"`, fmt.Sprintf(`"max_cost":%d`, creditsFor(10000)), `"global_spent":"0.002"`, `"your_spent":"0.002"`, `"global_remaining":"1.998"`, `"allowlist_version":7`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("resources lacks %s: %s", want, raw)
		}
	}
	if strings.Contains(strings.ToLower(string(raw)), x402TestKey[:16]) {
		t.Fatal("the key leaked")
	}
}

func TestX402ConfigValidation(t *testing.T) {
	good := `{"id":"a","url":"https://api.example.com/x","method":"GET","pay_to":"` + testPayTo + `","max_price":"0.01"}`
	key, _ := hex.DecodeString(x402TestKey)
	signer, _ := signerFromKeyBytes(key)
	base := `{"schema":1,"enabled":true,"network":"eip155:8453","asset":"` + testUSDC + `","asset_name":"USD Coin","asset_version":"2","decimals":6,"caps":%s,"wallet_key_file":"k","allowlist_file":"a"}`
	parse := func(caps, resources string) error {
		_, err := ParseX402Config([]byte(fmt.Sprintf(base, caps)), []byte(`{"schema":1,"version":1,"resources":[`+resources+`]}`), signer)
		return err
	}
	if err := parse(`{}`, good); err != nil {
		t.Fatalf("good: %v", err)
	}
	for name, c := range map[string][2]string{
		"global above ceiling":  {`{"global_daily":"21"}`, good},
		"per call above agent":  {`{"per_call":"0.5","agent_daily":"0.25"}`, good},
		"too many decimals":     {`{"per_call":"0.0000001"}`, good},
		"negative":              {`{"per_call":"-1"}`, good},
		"http":                  {`{}`, strings.Replace(good, "https://", "http://", 1)},
		"ip literal":            {`{}`, strings.Replace(good, "api.example.com", "10.0.0.1", 1)},
		"port":                  {`{}`, strings.Replace(good, "api.example.com", "api.example.com:8443", 1)},
		"userinfo":              {`{}`, strings.Replace(good, "https://", "https://u:p@", 1)},
		"max above per call":    {`{}`, strings.Replace(good, `"0.01"`, `"0.06"`, 1)},
		"bad pay_to":            {`{}`, strings.Replace(good, testPayTo, "0x123", 1)},
		"zero pay_to":           {`{}`, strings.Replace(good, testPayTo, "0x0000000000000000000000000000000000000000", 1)},
		"method":                {`{}`, strings.Replace(good, `"GET"`, `"DELETE"`, 1)},
		"GET with body":         {`{}`, strings.Replace(good, `"GET"`, `"GET","body":true`, 1)},
		"duplicate id":          {`{}`, good + "," + good},
		"unknown field":         {`{}`, strings.Replace(good, `"method"`, `"headers":{},"method"`, 1)},
		"huge response":         {`{}`, strings.Replace(good, `"GET"`, `"GET","max_response_bytes":999999`, 1)},
		"long timeout":          {`{}`, strings.Replace(good, `"GET"`, `"GET","timeout_seconds":60`, 1)},
		"bad query param names": {`{}`, strings.Replace(good, `"GET"`, `"GET","query":["a b"]`, 1)},
	} {
		if err := parse(c[0], c[1]); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := ParseX402Config([]byte(strings.Replace(fmt.Sprintf(base, `{}`), "eip155:8453", "base", 1)), []byte(`{"schema":1,"version":1,"resources":[]}`), signer); err == nil {
		t.Error("a v1 network name must be refused in the config")
	}
	if _, err := ParseX402Config([]byte(fmt.Sprintf(base, `{}`)), []byte(`{"schema":1,"version":0,"resources":[]}`), signer); err == nil {
		t.Error("allowlist version 0 must be refused")
	}
}

func TestX402LoadConfigFromFiles(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "wallet.key")
	if err := os.WriteFile(keyPath, []byte(x402TestKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	listPath := filepath.Join(dir, "allowlist.json")
	_ = os.WriteFile(listPath, []byte(`{"schema":1,"version":3,"resources":[]}`), 0o644)
	cfgPath := filepath.Join(dir, "x402.json")
	body := `{"schema":1,"enabled":true,"network":"eip155:8453","asset":"` + testUSDC + `","asset_name":"USD Coin","asset_version":"2","decimals":6,"caps":{},"wallet_key_file":"` + keyPath + `","allowlist_file":"` + listPath + `"}`
	_ = os.WriteFile(cfgPath, []byte(body), 0o644)
	cfg, err := LoadX402Config(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GlobalDaily != 2_000_000 || cfg.AgentDaily != 250_000 || cfg.PerCall != 50_000 || cfg.AllowlistVersion != 3 || cfg.ChainID != 8453 {
		t.Fatalf("defaults: %+v", cfg)
	}
	if s := fmt.Sprintf("%+v %#v", cfg, cfg); strings.Contains(s, x402TestKey[:16]) {
		t.Fatalf("formatting the config leaks the key: %s", s)
	}
	_ = os.WriteFile(cfgPath, []byte(strings.Replace(body, `"enabled":true`, `"enabled":false`, 1)), 0o644)
	if _, err = LoadX402Config(cfgPath); !errors.Is(err, ErrX402Disabled) {
		t.Fatalf("disabled: %v", err)
	}
}

func TestFormatAndParseUnits(t *testing.T) {
	for n, s := range map[int64]string{0: "0", 1: "0.000001", 1500: "0.0015", 2_000_000: "2", 2_500_000: "2.5", 10: "0.00001"} {
		if got := formatUnits(n, 6); got != s {
			t.Errorf("formatUnits(%d) = %s", n, got)
		}
		if back, ok := parseUnits(s, 6); !ok || back != n {
			t.Errorf("parseUnits(%s) = %d %v", s, back, ok)
		}
	}
	for _, s := range []string{"", ".5", "1.", "01", "1e3", "-1", "0.0000001", "1,5", " 1"} {
		if _, ok := parseUnits(s, 6); ok {
			t.Errorf("parseUnits(%q) accepted", s)
		}
	}
}

func FuzzX402Required(f *testing.F) {
	req := func(v int, extra string) string {
		if v == 1 {
			return `{"x402Version":1,"accepts":[{"scheme":"exact","network":"base","maxAmountRequired":"1000","asset":"` + testUSDC + `","payTo":"` + testPayTo + `","maxTimeoutSeconds":60` + extra + `}]}`
		}
		return `{"x402Version":2,"resource":{"url":"https://example.com/paid"},"accepts":[{"scheme":"exact","network":"eip155:8453","amount":"1000","asset":"` + testUSDC + `","payTo":"` + testPayTo + `","maxTimeoutSeconds":60,"extra":{"name":"USD Coin","version":"2"}` + extra + `}],"extensions":{"bazaar":{"info":{}}}}`
	}
	for _, s := range []string{req(1, ""), req(2, ""), req(2, `,"amount":"99999999999999999999"`), `{"x402Version":2,"accepts":[]}`, `{}`, `[]`, `{"x402Version":3}`, req(2, `,"scheme":"upto"`)} {
		f.Add("", []byte(s))
		f.Add(base64.StdEncoding.EncodeToString([]byte(s)), []byte("{}"))
	}
	payTo, _ := ParseEVMAddress(testPayTo)
	usdc, _ := ParseEVMAddress(testUSDC)
	key, _ := hex.DecodeString(x402TestKey)
	signer, _ := signerFromKeyBytes(key)
	f.Fuzz(func(t *testing.T, header string, body []byte) {
		r, ok := parseX402Required(header, body)
		if !ok {
			return
		}
		if (r.Version != 1 && r.Version != 2) || len(r.Accepts) == 0 || len(r.Accepts) > x402AcceptsMax || len(r.Resource) > x402RawFieldBytes || len(r.Extensions) > x402RawFieldBytes {
			t.Fatalf("out of bounds: %+v", r)
		}
		c, err := chooseX402(r, x402Want{Network: "eip155:8453", Asset: usdc, AssetName: "USD Coin", AssetVersion: "2", PayTo: payTo, Max: 10000})
		if err != nil {
			if code := errCode(err); code != "x402_not_payable" && code != "x402_price_changed" {
				t.Fatalf("refusal %v", err)
			}
			return
		}
		if c.Amount <= 0 || c.Amount > 10000 || c.PayTo != payTo || c.TimeoutSecs <= 0 {
			t.Fatalf("chose outside the want: %+v", c)
		}
		a := TransferAuthorization{From: signer.Address(), To: c.PayTo, Value: c.Amount, ValidAfter: 1, ValidBefore: 2}
		sig, err := signer.SignAuthorization(EIP712Domain{Name: "USD Coin", Version: "2", ChainID: 8453, VerifyingContract: usdc}, a)
		if err != nil {
			t.Fatal(err)
		}
		name, value, err := x402PaymentHeader(r, c, a, sig)
		if err != nil {
			return // an echoed field that does not compact: nothing is sent
		}
		raw, err := base64.StdEncoding.DecodeString(value)
		if err != nil || !json.Valid(raw) || (name != x402V2Signature && name != x402V1Signature) {
			t.Fatalf("payment header %s %q", name, value)
		}
		_ = parseX402Settlement(header, string(body))
	})
}

// The operator's example files (deploy/, not in the public snapshot) parse.
func TestX402ExampleFilesParse(t *testing.T) {
	config, err := os.ReadFile("../../deploy/x402.example.json")
	if errors.Is(err, os.ErrNotExist) {
		t.Skip("deploy/ is not in this tree")
	}
	if err != nil {
		t.Fatal(err)
	}
	list, err := os.ReadFile("../../deploy/x402-allowlist.example.json")
	if err != nil {
		t.Fatal(err)
	}
	key, _ := hex.DecodeString(x402TestKey)
	signer, _ := signerFromKeyBytes(key)
	cfg, err := ParseX402Config(config, list, signer)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ChainID != 8453 || cfg.GlobalDaily != 2_000_000 || len(cfg.Resources) != 5 {
		t.Fatalf("example: %+v", cfg)
	}
}

func TestImportX402Bazaar(t *testing.T) {
	cfg := testX402Config(t, `{}`, `[]`)
	page := `{"items":[
 {"resource":"https://api.example.com/v1/search","type":"http","x402Version":2,"description":"Search\nthe web <b>now</b>",
  "accepts":[{"scheme":"exact","network":"eip155:8453","amount":"7000","asset":"` + testUSDC + `","payTo":"` + testPayTo + `","maxTimeoutSeconds":60,"extra":{"name":"USD Coin","version":"2"}}],
  "extensions":{"bazaar":{"info":{"input":{"type":"http","method":"POST","bodyType":"json","body":{"query":"x"}}}}}},
 {"resource":"https://api.example.com/v1/price","type":"http","x402Version":2,
  "accepts":[{"scheme":"exact","network":"eip155:8453","amount":"1000","asset":"` + testUSDC + `","payTo":"` + testPayTo + `","maxTimeoutSeconds":60}],
  "extensions":{"bazaar":{"info":{"input":{"type":"http","method":"GET","queryParams":{"ids":"bitcoin","bad name":"x"}}}}}},
 {"resource":"https://api.example.com/v1/dear","x402Version":2,"accepts":[{"scheme":"exact","network":"eip155:8453","amount":"999999","asset":"` + testUSDC + `","payTo":"` + testPayTo + `"}]},
 {"resource":"http://api.example.com/plain","x402Version":2,"accepts":[{"scheme":"exact","network":"eip155:8453","amount":"1","asset":"` + testUSDC + `","payTo":"` + testPayTo + `"}]},
 {"resource":"https://api.example.com/solana","x402Version":2,"accepts":[{"scheme":"exact","network":"solana:5eykt4UsFv8P8NJdTREpY1vzqKqZKvdp","amount":"1","asset":"EPjFWdd5","payTo":"x"}]}
]}`
	got, skipped, err := ImportX402Bazaar([]byte(page), cfg, "bazaar test")
	if err != nil || skipped != 3 || len(got) != 2 {
		t.Fatalf("import: %+v %d %v", got, skipped, err)
	}
	if got[0].ID != "api-example-com-v1-search" || got[0].Method != "POST" || !got[0].Body || got[0].MaxPrice != "0.007" || got[0].Summary != "UNREVIEWED Bazaar text: Search the web <b>now</b>" {
		t.Fatalf("search: %+v", got[0])
	}
	if got[1].Method != "GET" || len(got[1].Query) != 1 || got[1].Query[0] != "ids" || got[1].PayTo != testPayTo {
		t.Fatalf("price: %+v", got[1])
	}
	// Every candidate is a valid allowlist entry once reviewed.
	raw, _ := json.Marshal(got)
	if _, err := ParseX402Config([]byte(`{"schema":1,"enabled":true,"network":"eip155:8453","asset":"`+testUSDC+`","asset_name":"USD Coin","asset_version":"2","decimals":6,"caps":{},"wallet_key_file":"k","allowlist_file":"a"}`),
		[]byte(`{"schema":1,"version":1,"resources":`+string(raw)+`}`), cfg.Signer); err != nil {
		t.Fatalf("candidates must parse as an allowlist: %v", err)
	}
}
