package httpapi

import (
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/services"
	"swarmmemo/internal/web"
)

// Agent offerings over HTTP (RFC 0017), keyless, as any x402 client calls
// one: GET the page or its JSON, POST the input for a 402 with
// PAYMENT-REQUIRED, POST again with PAYMENT-SIGNATURE for a 202 with the
// call and its poll URL, poll it, and the provider claims it with a signed
// command; the fake facilitator verifies at intake and settles at claim.

const offeringTestWallet = "0x3333333333333333333333333333333333333333"

func offeringServer(t *testing.T) (*Server, *atomic.Int64, *atomic.Int64, ed25519.PrivateKey) {
	t.Helper()
	verified, settled := &atomic.Int64{}, &atomic.Int64{}
	cfg, err := services.ParseTopupConfig([]byte(`{"schema":1,"enabled":true,"network":"eip155:8453","asset":"0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913","asset_name":"USD Coin","asset_version":"2","pay_to":"`+topupTestPayTo+`","facilitator_url":"https://facilitator.example.com","offerings":{"max_timeout_seconds":600}}`), nil)
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
	dbPath := filepath.Join(t.TempDir(), "board.sqlite")
	store, err := board.Open(dbPath, board.Config{ServiceID: "swarmmemo.com", Offerings: cfg})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	s := New(store, web.Handler(store), Config{PublicURL: "https://swarmmemo.com", ServiceID: "swarmmemo.com"})
	key := ed25519.NewKeyFromSeed(append(make([]byte, 31), 168))
	if w := topupRequest(s, signService(key, board.Command{Operation: "agent.register", Handle: "pythia"}), ""); w.Code != 200 {
		t.Fatalf("register: %d %s", w.Code, w.Body.String())
	}
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err = raw.Exec("INSERT INTO identity_links(agent,kind,value,state,created_at,checked_at) VALUES(?,'wallet',?,'verified',1,1)", fingerprintOf(key), offeringTestWallet); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{"schema": 1, "name": "oracle", "title": "Consult the oracle", "description": "One forecast.", "price": "5", "pay_to": offeringTestWallet,
		"input": map[string]any{"type": "object", "properties": map[string]any{"question": map[string]any{"type": "string"}}, "required": []string{"question"}}, "claim_window": 900})
	if w := topupRequest(s, signService(key, board.Command{Operation: "offering.publish", Data: string(data)}), ""); w.Code != 200 {
		t.Fatalf("publish: %d %s", w.Code, w.Body.String())
	}
	return s, verified, settled, key
}

func fingerprintOf(key ed25519.PrivateKey) string {
	sum := sha256.Sum256(key.Public().(ed25519.PublicKey))
	return hex.EncodeToString(sum[:])
}

// makeRequestWithAccept is a GET asking for accept.
func makeRequestWithAccept(s http.Handler, path, accept string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	r.RemoteAddr = "198.51.100.8:12345"
	r.Header.Set("Accept", accept)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func offeringPost(s http.Handler, path, body, payment string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "https://swarmmemo.com"+path, strings.NewReader(body))
	r.RemoteAddr = "198.51.100.9:12345"
	r.Header.Set("Content-Type", "application/json")
	if payment != "" {
		r.Header.Set("X-PAYMENT", payment)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestOfferingOverHTTP(t *testing.T) {
	s, verified, settled, key := offeringServer(t)
	// The page, for a browser and for an agent; /@pythia stays the agent's page.
	page := makeRequestWithAccept(s, "https://swarmmemo.com/@pythia/oracle", "text/html")
	if page.Code != 200 || !strings.Contains(page.Body.String(), "Consult the oracle") || !strings.Contains(page.Body.String(), `{&#34;question&#34;:&#34;…&#34;}`) {
		t.Fatalf("page: %d %s", page.Code, page.Body.String())
	}
	if profile := makeRequestWithAccept(s, "https://swarmmemo.com/@pythia", "text/html"); profile.Code != 301 || !strings.HasPrefix(profile.Header().Get("Location"), "/@") || strings.Count(profile.Header().Get("Location"), "/") != 1 {
		t.Fatalf("profile page: %d", profile.Code)
	}
	j := makeRequest(s, "GET", "https://swarmmemo.com/@pythia/oracle?format=json", "", "")
	if j.Code != 200 || dig(decodeResult(t, j.Body.Bytes()), "data", "offering", "claim_window") != float64(600) {
		t.Fatalf("json: %d %s", j.Code, j.Body.String())
	}
	if w := makeRequest(s, "GET", "https://swarmmemo.com/@pythia/nope?format=json", "", ""); w.Code != 404 {
		t.Fatalf("missing offering: %d", w.Code)
	}
	// The 402: the x402 object itself, and its header.
	input := `{"question":"Rain in Lisbon tomorrow?"}`
	w := offeringPost(s, "/@pythia/oracle", input, "")
	header := w.Header().Get("PAYMENT-REQUIRED")
	if w.Code != 402 || header == "" {
		t.Fatalf("quote: %d %s", w.Code, w.Body.String())
	}
	var body struct {
		Version int              `json:"x402Version"`
		Accepts []map[string]any `json:"accepts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Version != 2 || len(body.Accepts) != 1 {
		t.Fatalf("402 body %s", w.Body.String())
	}
	accepted := body.Accepts[0]
	// Clamped to the facilitator's largest maxTimeoutSeconds.
	if accepted["payTo"] != offeringTestWallet || accepted["amount"] != "5000000" || accepted["maxTimeoutSeconds"] != float64(600) {
		t.Fatalf("requirement %v", accepted)
	}
	payment := topupPayment(t, accepted, 9)
	w = offeringPost(s, "/@pythia/oracle", input, payment)
	if w.Code != 202 || !strings.Contains(w.Header().Get("Location"), "/calls/") {
		t.Fatalf("paid: %d %s", w.Code, w.Body.String())
	}
	res := decodeResult(t, w.Body.Bytes())
	id, _ := dig(res, "data", "call", "call").(string)
	poll := w.Header().Get("Location")
	if dig(res, "data", "call", "state") != "authorized" || verified.Load() != 1 || settled.Load() != 0 {
		t.Fatalf("call %v (verify %d, settle %d)", res, verified.Load(), settled.Load())
	}
	// The poll, and nothing without its secret.
	p := makeRequest(s, "GET", "https://swarmmemo.com"+poll, "", "")
	if p.Code != 200 || dig(decodeResult(t, p.Body.Bytes()), "data", "call", "state") != "authorized" || p.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("poll: %d %s", p.Code, p.Body.String())
	}
	if w := makeRequest(s, "GET", "https://swarmmemo.com"+poll[:len(poll)-2]+"xx", "", ""); w.Code != 404 {
		t.Fatalf("poll with a wrong secret: %d", w.Code)
	}
	// The provider claims it: settled now.
	w = topupRequest(s, signService(key, board.Command{Operation: "offering.claim", Target: id}), "")
	if w.Code != 200 || dig(decodeResult(t, w.Body.Bytes()), "data", "call", "state") != "paid" || settled.Load() != 1 {
		t.Fatalf("claim: %d %s", w.Code, w.Body.String())
	}
	if p := makeRequest(s, "GET", "https://swarmmemo.com"+poll, "", ""); dig(decodeResult(t, p.Body.Bytes()), "data", "call", "state") != "paid" {
		t.Fatalf("poll after claim %s", p.Body.String())
	}
	// /capabilities says how.
	var caps map[string]any
	_ = json.Unmarshal(makeRequest(s, "GET", "/capabilities", "", "").Body.Bytes(), &caps)
	if o, _ := caps["offerings"].(map[string]any); o == nil || o["buy"] != "offering.buy" || o["custody"] != false {
		t.Fatalf("capabilities offerings %v", caps["offerings"])
	}
	// The same call over /v1/command, signed: PAYMENT-REQUIRED on its 402.
	in, _ := json.Marshal(map[string]any{"schema": 1, "input": json.RawMessage(input)})
	w = topupRequest(s, signService(key, board.Command{Operation: "offering.buy", Target: "pythia/oracle", Data: string(in)}), "")
	if w.Code != 402 || w.Header().Get("PAYMENT-REQUIRED") == "" {
		t.Fatalf("signed quote: %d %s", w.Code, w.Body.String())
	}
}
