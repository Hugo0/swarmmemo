package services

// Security review 1.20 regression tests for the x402 relay. Each one inverts
// a proof of concept (TestSecPoC_*, branch security-review-1.20): it fails
// while the weakness is present.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
)

// M1: a paid call whose answer is too large is charged, not refunded, and an
// account with X402DiscardsPerDay such calls today is refused before it pays
// again: free keys can no longer drain the global USDC budget at no cost.
func TestSec120_X402OversizeIsChargedAndCapped(t *testing.T) {
	resources := `[{"id":"search","url":"https://example.com/paid","method":"POST","pay_to":"` + testPayTo + `","max_price":"0.002","body":true,"max_response_bytes":1024,"timeout_seconds":2}]`
	h := newX402HarnessWith(t, &fakeX402{price: 2000, bigPaid: 4000}, `{"per_call":"0.002","agent_daily":"0.008","global_daily":"0.016"}`, resources)
	s := allowance.Subject{ID: "attacker-0", Signed: true}
	for j := range X402DiscardsPerDay {
		out, err := h.call(s, `{"resource":"search","body":{"numResults":100}}`, creditsFor(2000))
		if err != nil || resultOf(out)["encoding"] != "discarded" {
			t.Fatalf("call %d: %v %v", j, out, err)
		}
	}
	if got, want := h.charged(), int64(X402DiscardsPerDay)*creditsFor(2000); got != want {
		t.Fatalf("paid-but-discarded calls charged %d, want %d", got, want)
	}
	// The next one is refused before anything is paid.
	paid := h.fake.paid
	if _, err := h.call(s, `{"resource":"search","body":{"numResults":100}}`, creditsFor(2000)); errCode(err) != "x402_response_too_large" {
		t.Fatalf("a third oversized call: %v", err)
	}
	if h.fake.paid != paid {
		t.Fatal("the refused call paid the upstream")
	}
	// Another agent is not locked out.
	h.fake.bigPaid = 0
	if _, err := h.call(allowance.Subject{ID: "honest", Signed: true}, `{"resource":"search","body":{"numResults":1}}`, creditsFor(2000)); err != nil {
		t.Fatalf("honest agent: %v", err)
	}
}

// M2: HTML escaping and base64 no longer push an ordinary paid answer past
// the stored-body limit.
func TestSec120_X402EscapingFitsTheStoredBody(t *testing.T) {
	cfg := testX402Config(t, `{}`, testResources)
	x := &x402{cfg: cfg, byID: map[string]*X402Resource{}}
	res := cfg.Resources[0]
	res.MaxResponseBytes = X402ResponseBytesMax
	p := x402Plan{res: &res}
	c := Call{Price: DefaultPrices()["x402.call"]}
	page := strings.Repeat("a < b && c > d ", X402ResponseBytesMax/16)
	body := []byte(`{"results":[{"url":"https://example.org","text":"` + page + `"}]}`)
	if len(body) > X402ResponseBytesMax {
		t.Fatalf("fixture too big: %d", len(body))
	}
	receipt := &x402Receipt{Amount: "1000"}
	r, err := x.result(c, p, x402Response{status: 200, body: body, contentType: "application/json"}, receipt)
	if err != nil {
		t.Fatalf("a %d-byte JSON answer with '<' and '&': %v", len(body), err)
	}
	if !bytes.Contains(r.Body, []byte("a < b && c > d")) {
		t.Fatal("the JSON answer was HTML-escaped")
	}
	for name, resp := range map[string]x402Response{
		"binary":        {status: 200, body: bytes.Repeat([]byte{0xff}, X402ResponseBytesMax), contentType: "application/octet-stream"},
		"control text":  {status: 200, body: bytes.Repeat([]byte{0x01}, X402ResponseBytesMax), contentType: "text/plain"},
		"ampersand txt": {status: 200, body: bytes.Repeat([]byte("&"), X402ResponseBytesMax), contentType: "text/plain"},
	} {
		r, err := x.result(c, p, resp, receipt)
		if err != nil || len(r.Body) > X402StoredBodyBytes || !json.Valid(r.Body) {
			t.Fatalf("%s at the limit: %d bytes, %v", name, len(r.Body), err)
		}
	}
	if (&x402{}).Describe().StoredBodyMax < X402StoredBodyBytes {
		t.Fatal("the engine would refuse the x402 record")
	}
}

// L1: a decimals typo for USD Coin is refused, so the compiled ceilings
// cannot be scaled by a power of ten.
func TestSec120_X402DecimalsPinnedForUSDC(t *testing.T) {
	signer := testX402Config(t, `{}`, testResources).Signer
	allowlist := `{"schema":1,"version":1,"resources":[{"id":"price","url":"https://example.com/paid","method":"GET","pay_to":"` + testPayTo + `","max_price":"0.05"}]}`
	for _, d := range []string{"12", "18", "0"} {
		config := `{"schema":1,"enabled":true,"network":"eip155:8453","asset":"` + testUSDC + `","asset_name":"USD Coin","asset_version":"2","decimals":` + d + `,
"caps":{},"wallet_key_file":"unused","allowlist_file":"unused"}`
		if _, err := ParseX402Config([]byte(config), []byte(allowlist), signer); err == nil {
			t.Fatalf("decimals %s for USD Coin accepted", d)
		}
	}
}

// resultOf decodes a finished call's "result" (raw JSON) into a map.
func resultOf(out map[string]any) map[string]any {
	raw, _ := json.Marshal(out["result"])
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return m
}
