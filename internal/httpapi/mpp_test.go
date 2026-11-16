package httpapi

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/pay"
	"swarmmemo/internal/services"
	"swarmmemo/internal/web"
)

// MPP beside x402 over HTTP (C202b): one 402 carries both challenges, a
// request carries one credential (two are 400), an MPP payment's success
// carries Payment-Receipt and its refusal is an RFC 9457 problem with a
// fresh challenge; with MPP off every header and answer is as before.

// mppTopupServer is topupServer (top-ups on) with MPP set as asked.
func mppTopupServer(t *testing.T, mpp bool) (*Server, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	kek := filepath.Join(t.TempDir(), "hosted-kek")
	if err := os.WriteFile(kek, []byte(base64.RawURLEncoding.EncodeToString(key)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	verified, settled := &atomic.Int64{}, &atomic.Int64{}
	cfg, err := services.ParseTopupConfig([]byte(`{"schema":1,"enabled":true,"network":"eip155:8453","asset":"0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913","asset_name":"USD Coin","asset_version":"2","pay_to":"`+topupTestPayTo+`","facilitator_url":"https://facilitator.example.com"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	facilitator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/verify" {
			verified.Add(1)
			_, _ = w.Write([]byte(`{"isValid":true}`))
			return
		}
		n := settled.Add(1)
		fmt.Fprintf(w, `{"success":true,"transaction":"0x%064x","network":"eip155:8453"}`, n)
	}))
	t.Cleanup(facilitator.Close)
	cfg.UseTestFacilitator(facilitator.Client(), facilitator.URL)
	features := board.Features{Ledger: board.LedgerOn, MPP: mpp}
	store, err := board.Open(filepath.Join(t.TempDir(), "board.sqlite"), board.Config{ServiceID: "swarmmemo.com", HostedKEKFile: kek, Features: features, Topup: cfg})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	features.Topup = true
	return New(store, web.Handler(store), Config{PublicURL: "https://swarmmemo.com", ServiceID: "swarmmemo.com", Features: features}), verified, settled
}

// mppCredential answers the one Payment challenge in h with an
// authorization the fake facilitator takes (it checks no signature): the
// recipient and amount the request names, nonce keccak256(id ‖ realm).
func mppCredential(t *testing.T, h http.Header, validBefore int64) string {
	t.Helper()
	cs, err := pay.ParseMPPChallenges(h.Values("WWW-Authenticate"))
	if err != nil || len(cs) != 1 {
		t.Fatalf("challenges %q: %v", h.Values("WWW-Authenticate"), err)
	}
	req, err := cs[0].Request()
	if err != nil {
		t.Fatal(err)
	}
	v, err := cs[0].Credential(pay.X402AuthorizationWire{From: "0x2222222222222222222222222222222222222222", To: req["recipient"].(string), Value: req["amount"].(string),
		ValidAfter: "0", ValidBefore: strconv.FormatInt(validBefore, 10), Nonce: cs[0].Nonce()}, "0x"+strings.Repeat("ab", 65), "")
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// paidRequest is POST /v1/command with c and the given headers.
func paidRequest(s http.Handler, c board.Command, headers map[string]string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(c)
	r := httptest.NewRequest("POST", "https://swarmmemo.com/v1/command", strings.NewReader(string(body)))
	r.RemoteAddr = "198.51.100.8:12345"
	r.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

// problemOf is an RFC 9457 MPP problem answer: its type's slug and the
// board's error code inside.
func problemOf(t *testing.T, w *httptest.ResponseRecorder) (string, string) {
	t.Helper()
	if w.Header().Get("Content-Type") != pay.ProblemContentType {
		t.Fatalf("not a problem: %d %q %s", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
	var p struct {
		Type   string `json:"type"`
		Status int    `json:"status"`
		Error  struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil || p.Status != w.Code {
		t.Fatalf("problem %s", w.Body.String())
	}
	return strings.TrimPrefix(p.Type, pay.ProblemBase), p.Error.Code
}

func TestMPPTopupOverHTTP(t *testing.T) {
	s, verified, settled := mppTopupServer(t, true)
	key := ed25519.NewKeyFromSeed(append(make([]byte, 31), 202))
	cmd := func() board.Command {
		return signService(key, board.Command{Operation: "credits.topup", Amount: 2_000_000})
	}
	// One 402, both challenges.
	w := paidRequest(s, cmd(), nil)
	if w.Code != 402 || w.Header().Get("PAYMENT-REQUIRED") == "" || len(w.Header().Values("WWW-Authenticate")) != 1 || w.Header().Get("Cache-Control") != "no-store" ||
		!strings.Contains(w.Header().Get("Access-Control-Expose-Headers"), "WWW-Authenticate, Payment-Receipt") {
		t.Fatalf("quote: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	if dig(decodeResult(t, w.Body.Bytes()), "error", "details", "mpp", "www_authenticate") != w.Header().Get("WWW-Authenticate") {
		t.Fatal("details.mpp is not the header")
	}
	cred := mppCredential(t, w.Header(), time.Now().Unix()+300)
	// Two credentials, of two wires or of one, are 400 before anything runs.
	x402 := topupPayment(t, map[string]any{"payTo": topupTestPayTo, "amount": "2000000"}, 3)
	for name, h := range map[string]map[string]string{
		"x402 and MPP": {"PAYMENT-SIGNATURE": x402, "Authorization": cred},
		"two MPP":      {"Authorization": cred, "Payment-Authorization": cred},
		"empty MPP":    {"Payment-Authorization": "Payment "},
		"not Payment":  {"Payment-Authorization": "Bearer x"},
	} {
		if w := paidRequest(s, cmd(), h); w.Code != 400 || !strings.Contains(w.Body.String(), "payment_invalid") {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	if verified.Load() != 0 {
		t.Fatal("two credentials reached the facilitator")
	}
	// Paid over MPP: 200 with Payment-Receipt, not PAYMENT-RESPONSE.
	w = paidRequest(s, cmd(), map[string]string{"Authorization": cred})
	if w.Code != 200 || w.Header().Get("Payment-Receipt") == "" || w.Header().Get("PAYMENT-RESPONSE") != "" || w.Header().Get("Cache-Control") != "private" {
		t.Fatalf("pay: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	res := decodeResult(t, w.Body.Bytes())
	if dig(res, "data", "topup", "state") != "credited" || dig(res, "data", "topup", "protocol") != "mpp" || dig(res, "data", "payment_receipt") != w.Header().Get("Payment-Receipt") {
		t.Fatalf("receipt %v", res)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(w.Header().Get("Payment-Receipt"))
	var receipt map[string]any
	if json.Unmarshal(raw, &receipt) != nil || receipt["status"] != "success" || receipt["reference"] != dig(res, "data", "topup", "transaction") {
		t.Fatalf("Payment-Receipt %s", raw)
	}
	// The same credential again: an RFC 9457 problem, a fresh challenge.
	w = paidRequest(s, cmd(), map[string]string{"Authorization": cred})
	if slug, code := problemOf(t, w); w.Code != 402 || slug != "invalid-challenge" || code != "payment_replayed" || len(w.Header().Values("WWW-Authenticate")) != 1 {
		t.Fatalf("replay: %d %s %s %v", w.Code, slug, code, w.Header())
	}
	// A malformed credential.
	w = paidRequest(s, cmd(), map[string]string{"Authorization": "Payment bm90IGpzb24"})
	if slug, code := problemOf(t, w); w.Code != 402 || slug != "malformed-credential" || code != "payment_invalid" {
		t.Fatalf("malformed: %d %s %s", w.Code, slug, code)
	}
	if verified.Load() != 1 || settled.Load() != 1 {
		t.Fatalf("verify %d, settle %d", verified.Load(), settled.Load())
	}
	// Surfaces.
	caps := decodeResult(t, get(s, capabilitiesFullPath, "application/json").Body.Bytes())
	if p, _ := dig(caps, "payments", "protocols").([]any); len(p) != 2 || p[1] != "mpp" || dig(caps, "topup", "mpp", "method") != "evm" {
		t.Fatalf("capabilities payments %v topup %v", caps["payments"], caps["topup"])
	}
	if body := get(s, "/llms-full.txt", "").Body.String(); !strings.Contains(body, "/protocol.md#mpp") {
		t.Fatal("llms-full.txt has no MPP line while MPP is on")
	}
}

func TestMPPOfferingOverHTTP(t *testing.T) {
	s, verified, settled, key := offeringServerWith(t, board.Features{MPP: true})
	input := `{"question":"Rain in Lisbon tomorrow?"}`
	w := offeringPost(s, "/@pythia/oracle", input, "")
	if w.Code != 402 || w.Header().Get("PAYMENT-REQUIRED") == "" || len(w.Header().Values("WWW-Authenticate")) != 1 {
		t.Fatalf("quote: %d %v", w.Code, w.Header())
	}
	var body struct {
		Version int `json:"x402Version"`
	}
	if json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Version != 2 {
		t.Fatalf("the 402 body is no longer x402's: %s", w.Body.String())
	}
	// The discovery probe carries an (unpayable) MPP challenge too.
	probe := offeringPost(s, "/@pythia/oracle", "", "")
	if probe.Code != 402 || len(probe.Header().Values("WWW-Authenticate")) != 1 {
		t.Fatalf("probe: %d %v", probe.Code, probe.Header())
	}
	// Paid over MPP: 202, the poll in Location, no receipt yet.
	cred := mppCredential(t, w.Header(), time.Now().Unix()+630)
	r := httptest.NewRequest("POST", "https://swarmmemo.com/@pythia/oracle", strings.NewReader(input))
	r.RemoteAddr = "198.51.100.9:12345"
	r.Header.Set("Authorization", cred)
	paid := httptest.NewRecorder()
	s.ServeHTTP(paid, r)
	if paid.Code != 202 || paid.Header().Get("Payment-Receipt") != "" || verified.Load() != 1 {
		t.Fatalf("paid: %d %v %s", paid.Code, paid.Header(), paid.Body.String())
	}
	poll := paid.Header().Get("Location")
	id, _ := dig(decodeResult(t, paid.Body.Bytes()), "data", "call", "call").(string)
	if p := makeRequest(s, "GET", "https://swarmmemo.com"+poll, "", ""); p.Code != 200 || p.Header().Get("Payment-Receipt") != "" {
		t.Fatalf("poll before the claim: %d %v", p.Code, p.Header())
	}
	// The same credential with another input: refused as MPP refuses.
	r = httptest.NewRequest("POST", "https://swarmmemo.com/@pythia/oracle", strings.NewReader(`{"question":"Other?"}`))
	r.RemoteAddr = "198.51.100.9:12345"
	r.Header.Set("Authorization", cred)
	other := httptest.NewRecorder()
	s.ServeHTTP(other, r)
	if slug, code := problemOf(t, other); other.Code != 402 || slug != "verification-failed" || code != "payment_mismatch" || len(other.Header().Values("WWW-Authenticate")) != 1 {
		t.Fatalf("other input: %d %s %s", other.Code, slug, code)
	}
	// Claimed: the poll answer carries the receipt.
	if w := topupRequest(s, signService(key, board.Command{Operation: "offering.claim", Target: id}), ""); w.Code != 200 || settled.Load() != 1 {
		t.Fatalf("claim: %d %s", w.Code, w.Body.String())
	}
	p := makeRequest(s, "GET", "https://swarmmemo.com"+poll, "", "")
	if p.Code != 200 || p.Header().Get("Payment-Receipt") == "" || dig(decodeResult(t, p.Body.Bytes()), "data", "call", "payment_receipt") != p.Header().Get("Payment-Receipt") {
		t.Fatalf("poll after the claim: %d %v %s", p.Code, p.Header(), p.Body.String())
	}
	// /openapi.json lists both protocols in the scanners' shape.
	var doc map[string]any
	_ = json.Unmarshal(get(s, "/openapi.json", "").Body.Bytes(), &doc)
	protocols, _ := dig(doc, "paths", "/@pythia/oracle", "post", "x-payment-info", "protocols").([]any)
	if len(protocols) != 2 || dig(protocols[1], "mpp", "method") != "evm" || dig(protocols[1], "mpp", "intent") != "charge" {
		t.Fatalf("x-payment-info.protocols %v", protocols)
	}
}

// With MPP off, nothing on the wire changes: no challenge header, the same
// exposed headers, an Authorization: Payment header is not a payment (the
// x402 one beside it pays as before), and no MPP surface.
func TestMPPOffHTTPUnchanged(t *testing.T) {
	s, _, settled := mppTopupServer(t, false)
	key := ed25519.NewKeyFromSeed(append(make([]byte, 31), 203))
	cmd := signService(key, board.Command{Operation: "credits.topup", Amount: 2_000_000})
	w := paidRequest(s, cmd, nil)
	if w.Code != 402 || w.Header().Get("WWW-Authenticate") != "" || w.Header().Get("Access-Control-Expose-Headers") != "X-Next-Cursor, Retry-After, Link, PAYMENT-REQUIRED, PAYMENT-RESPONSE" ||
		dig(decodeResult(t, w.Body.Bytes()), "error", "details", "mpp") != nil {
		t.Fatalf("quote: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	raw, _ := base64.StdEncoding.DecodeString(w.Header().Get("PAYMENT-REQUIRED"))
	var required struct {
		Accepts []map[string]any `json:"accepts"`
	}
	_ = json.Unmarshal(raw, &required)
	w = paidRequest(s, signService(key, board.Command{Operation: "credits.topup", Amount: 2_000_000}), map[string]string{"PAYMENT-SIGNATURE": topupPayment(t, required.Accepts[0], 5), "Authorization": "Payment abc"})
	if w.Code != 200 || w.Header().Get("PAYMENT-RESPONSE") == "" || w.Header().Get("Payment-Receipt") != "" || settled.Load() != 1 {
		t.Fatalf("x402 beside an ignored Authorization: %d %s", w.Code, w.Body.String())
	}
	caps := decodeResult(t, get(s, capabilitiesFullPath, "application/json").Body.Bytes())
	if dig(caps, "payments", "protocols") != nil || dig(caps, "topup", "mpp") != nil || dig(caps, "topup", "protocols") != nil {
		t.Fatalf("capabilities %v %v", caps["payments"], caps["topup"])
	}
	if strings.Contains(get(s, "/llms.txt", "").Body.String(), "#mpp") {
		t.Fatal("llms.txt mentions MPP while it is off")
	}
	o, _, _, _ := offeringServer(t)
	q := offeringPost(o, "/@pythia/oracle", `{"question":"?"}`, "")
	var doc map[string]any
	_ = json.Unmarshal(get(o, "/openapi.json", "").Body.Bytes(), &doc)
	if q.Code != 402 || q.Header().Get("WWW-Authenticate") != "" || q.Header().Get("Access-Control-Expose-Headers") != "PAYMENT-REQUIRED, Location, Retry-After" ||
		len(dig(doc, "paths", "/@pythia/oracle", "post", "x-payment-info", "protocols").([]any)) != 1 {
		t.Fatalf("offering 402 with MPP off: %d %v", q.Code, q.Header())
	}
}

// FuzzMPPPaymentHeaders sends the keyless offering call every mix of
// payment headers: never a 5xx, never a settlement, and credentials of
// two wires are 400. Run with -fuzz for at most 60 seconds.
func FuzzMPPPaymentHeaders(f *testing.F) {
	x402 := base64.StdEncoding.EncodeToString([]byte(`{"x402Version":2}`))
	f.Add("", "", "", "", `{"question":"q"}`)
	f.Add(x402, "", "Payment eyJ9", "", `{"question":"q"}`)
	f.Add("", x402, "", "Payment eyJjaGFsbGVuZ2UiOnt9fQ", `{}`)
	f.Add("", "", "Bearer smh_x", "payment ", `{"question":"q"}`)
	f.Add("", "", "Payment "+strings.Repeat("A", 9000), "", `{"question":"q"}`)
	f.Add("", "", "Payment eyJjaGFsbGVuZ2UiOnsiaWQiOiJ4In0sInBheWxvYWQiOnt9fQ", "Basic Zm9v", `null`)
	s, _, settled, _ := offeringServerWith(f, board.Features{MPP: true})
	var n atomic.Uint32
	f.Fuzz(func(t *testing.T, sig, xpay, auth, payAuth, input string) {
		r := httptest.NewRequest("POST", "https://swarmmemo.com/@pythia/oracle", strings.NewReader(input))
		// A fresh network each time, so the rate limits rarely answer first.
		i := n.Add(1)
		r.RemoteAddr = fmt.Sprintf("10.%d.%d.%d:12345", byte(i>>16), byte(i>>8), byte(i))
		for k, v := range map[string]string{"PAYMENT-SIGNATURE": sig, "X-PAYMENT": xpay, "Authorization": auth, "Payment-Authorization": payAuth} {
			if v != "" && !strings.ContainsAny(v, "\r\n\x00") {
				r.Header.Set(k, v)
			}
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code >= 500 {
			t.Fatalf("%q %q %q %q %q: %d %s", sig, xpay, auth, payAuth, input, w.Code, w.Body.String())
		}
		_, mpp := pay.MPPToken(r.Header.Get("Authorization"))
		if (r.Header.Get("PAYMENT-SIGNATURE") != "" || r.Header.Get("X-PAYMENT") != "") && (mpp || r.Header.Get("Payment-Authorization") != "") && w.Code != 400 && w.Code != 429 {
			t.Fatalf("two wires answered %d", w.Code)
		}
		if settled.Load() != 0 {
			t.Fatal("a fuzzed request settled")
		}
	})
}
