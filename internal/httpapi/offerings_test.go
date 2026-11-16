package httpapi

import (
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

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
	return offeringServerWith(t, board.Features{})
}

// offeringServerWith is offeringServer with features on (reviews, C200).
func offeringServerWith(t testing.TB, features board.Features) (*Server, *atomic.Int64, *atomic.Int64, ed25519.PrivateKey) {
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
	store, err := board.Open(dbPath, board.Config{ServiceID: "swarmmemo.com", Offerings: cfg, Features: features})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	s := New(store, web.Handler(store), Config{PublicURL: "https://swarmmemo.com", ServiceID: "swarmmemo.com", Features: features})
	key := ed25519.NewKeyFromSeed(append(make([]byte, 31), 168))
	if w := topupRequest(s, signService(key, board.Command{Operation: "agent.register", Handle: "pythia"}), ""); w.Code != 200 {
		t.Fatalf("register: %d %s", w.Code, w.Body.String())
	}
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	// The store holds the database open: wait out its locks as the store
	// does (busy_timeout 5000) instead of failing with SQLITE_BUSY.
	if _, err = raw.Exec("PRAGMA busy_timeout=5000"); err != nil {
		t.Fatal(err)
	}
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
	if page.Code != 200 || !strings.Contains(page.Body.String(), "Consult the oracle") || !strings.Contains(page.Body.String(), `{&#34;question&#34;:&#34;example&#34;}`) || !strings.Contains(page.Body.String(), "Answered 0 of 0 paid calls, lapsed 0.") {
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
	wrong := "xx"
	if strings.HasSuffix(poll, wrong) {
		wrong = "yy"
	}
	if w := makeRequest(s, "GET", "https://swarmmemo.com"+poll[:len(poll)-2]+wrong, "", ""); w.Code != 404 {
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
	// The provider answers; the poll carries the answer, untrusted, with its
	// hash. Nothing logged by route keeps the poll secret.
	answer, _ := json.Marshal(map[string]any{"schema": 1, "body": "70% rain."})
	if w := topupRequest(s, signService(key, board.Command{Operation: "offering.answer", Target: id, Data: string(answer)}), ""); w.Code != 200 || dig(decodeResult(t, w.Body.Bytes()), "data", "call", "state") != "answered" {
		t.Fatalf("answer: %d %s", w.Code, w.Body.String())
	}
	p = makeRequest(s, "GET", "https://swarmmemo.com"+poll, "", "")
	got := decodeResult(t, p.Body.Bytes())
	if dig(got, "data", "call", "state") != "answered" || dig(got, "data", "call", "answer", "body") != "70% rain." || dig(got, "data", "call", "answer", "text_is_untrusted") != true ||
		dig(got, "data", "call", "answer", "sha256") != fmt.Sprintf("%x", sha256.Sum256([]byte("70% rain."))) {
		t.Fatalf("poll after the answer %s", p.Body.String())
	}
	if class := routeClass(poll); strings.Contains(class, poll[strings.LastIndex(poll, "/")+1:]) || strings.Contains(class, id) {
		t.Fatalf("route class %q keeps the poll URL's secret or call", class)
	}
	// /capabilities says how.
	var caps map[string]any
	_ = json.Unmarshal(makeRequest(s, "GET", capabilitiesFullPath, "", "").Body.Bytes(), &caps)
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

// Over MCP, keyless: find and read, buy_offering's 402 carries the x402
// requirement, the paid retry returns the poll URL, and offering_call_status
// reads it. The assistant and core profiles have find and read only: no
// payment tools, and never the provider's.
func TestOfferingsOverMCP(t *testing.T) {
	s, _, _, _ := offeringServer(t)
	list := mustTool(t, s, "/mcp", "", "find_offerings", map[string]any{"query": "oracle"})
	if offerings, _ := dig(list, "data", "offerings").([]any); len(offerings) != 1 {
		t.Fatalf("find_offerings %v", list)
	}
	if got := mustTool(t, s, "/mcp", "", "read_offering", map[string]any{"target": "pythia/oracle"}); dig(got, "data", "offering", "price") != "5" {
		t.Fatalf("read_offering %v", got)
	}
	input := map[string]any{"question": "Rain in Lisbon tomorrow?"}
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "buy_offering", "arguments": map[string]any{"target": "pythia/oracle", "input": input}}})
	out := mcpRequest(t, s, "/mcp", "", string(raw))
	accepts, _ := dig(out, "result", "structuredContent", "error", "details", "x402", "accepts").([]any)
	if dig(out, "result", "structuredContent", "error", "code") != "payment_required" || len(accepts) != 1 {
		t.Fatalf("buy_offering quote %v", out)
	}
	payment := topupPayment(t, accepts[0].(map[string]any), 21)
	call := mustTool(t, s, "/mcp", "", "buy_offering", map[string]any{"target": "pythia/oracle", "input": input, "payment": payment})
	poll, _ := dig(call, "data", "call", "poll").(string)
	if dig(call, "data", "call", "state") != "authorized" || poll == "" {
		t.Fatalf("buy_offering paid %v", call)
	}
	status := mustTool(t, s, "/mcp", "", "offering_call_status", map[string]any{"poll": "https://swarmmemo.com" + poll})
	if dig(status, "data", "call", "state") != "authorized" {
		t.Fatalf("offering_call_status %v", status)
	}
	if _, failure := callTool(t, s, "/mcp", "", "offering_call_status", map[string]any{"poll": poll[:len(poll)-2] + "xx"}); !strings.HasPrefix(failure, "404 offering_call_not_found") {
		t.Fatalf("a wrong poll secret: %q", failure)
	}
	names := func(path string) map[string]bool {
		out := map[string]bool{}
		for _, tool := range profileToolList(t, s, path) {
			out[tool["name"].(string)] = true
		}
		return out
	}
	assistant, core := names(web.AssistantMCPPath), names("/mcp/core")
	if !assistant["find_offerings"] || !assistant["read_offering"] || assistant["buy_offering"] || assistant["offering_call_status"] {
		t.Errorf("assistant profile offering tools: %v", assistant)
	}
	// The core profile moves no money (coreLeftOutTools): it finds and
	// reads offerings, and buys none.
	for _, name := range []string{"find_offerings", "read_offering"} {
		if !core[name] {
			t.Errorf("the core profile does not list %s", name)
		}
	}
	for _, name := range []string{"buy_offering", "offering_call_status", "publish_offering", "claim_offering_call", "answer_offering_call", "refund_offering_call"} {
		if core[name] {
			t.Errorf("the core profile lists %s", name)
		}
	}
}

// A discovery probe (POST with no input, no payment) answers 402 with the
// x402 bazaar extension, its info valid against its schema and its example
// body an input the offering accepts; the probe's requirement cannot be
// paid, with no input or with input, and nothing reaches the facilitator.
// Wrong input is still 400; a payable 402 carries the same extension, and a
// payment echoing it (as x402 clients do) is taken.
func TestOfferingDiscoveryProbe(t *testing.T) {
	s, verified, _, _ := offeringServer(t)
	type paymentRequired struct {
		Version    int              `json:"x402Version"`
		Resource   map[string]any   `json:"resource"`
		Accepts    []map[string]any `json:"accepts"`
		Extensions struct {
			Bazaar struct {
				Info   map[string]any  `json:"info"`
				Schema json.RawMessage `json:"schema"`
			} `json:"bazaar"`
		} `json:"extensions"`
		SwarmMemo map[string]any `json:"swarmmemo"`
	}
	read402 := func(w *httptest.ResponseRecorder) paymentRequired {
		t.Helper()
		var pr, fromHeader paymentRequired
		if w.Code != 402 || json.Unmarshal(w.Body.Bytes(), &pr) != nil || pr.Version != 2 || len(pr.Accepts) != 1 {
			t.Fatalf("402: %d %s", w.Code, w.Body.String())
		}
		raw, err := base64.StdEncoding.DecodeString(w.Header().Get("PAYMENT-REQUIRED"))
		if err != nil || json.Unmarshal(raw, &fromHeader) != nil || fromHeader.Extensions.Bazaar.Info == nil {
			t.Fatalf("PAYMENT-REQUIRED header without the bazaar extension: %s", raw)
		}
		var schema jsonschema.Schema
		if err := json.Unmarshal(pr.Extensions.Bazaar.Schema, &schema); err != nil {
			t.Fatalf("bazaar schema: %v %s", err, pr.Extensions.Bazaar.Schema)
		}
		resolved, err := schema.Resolve(nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := resolved.Validate(pr.Extensions.Bazaar.Info); err != nil {
			t.Fatalf("bazaar info does not follow its schema: %v %s", err, w.Body.String())
		}
		return pr
	}
	var example []byte
	for _, body := range []string{"", "{}", " null "} {
		pr := read402(offeringPost(s, "/@pythia/oracle", body, ""))
		in, _ := pr.Extensions.Bazaar.Info["input"].(map[string]any)
		if in["type"] != "http" || in["method"] != "POST" || in["bodyType"] != "json" || dig(in, "body", "question") != "example" ||
			pr.Accepts[0]["extra"].(map[string]any)["quote"] != "discovery-only" || dig(pr.SwarmMemo, "offering", "discovery") != true ||
			pr.Resource["url"] != "https://swarmmemo.com/@pythia/oracle" || dig(pr.Extensions.Bazaar.Info, "output", "type") != "json" {
			t.Fatalf("probe %q: %+v", body, pr)
		}
		example, _ = json.Marshal(in["body"])
	}
	// Wrong input is no probe.
	for _, body := range []string{`{"question":5}`, `{"other":"x"}`, `[]`, `nonsense`} {
		if w := offeringPost(s, "/@pythia/oracle", body, ""); w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_offering_input") {
			t.Fatalf("input %s: %d %s", body, w.Code, w.Body.String())
		}
	}
	// The probe's requirement is not payable: no input is still refused, and
	// with input the quote is not this server's.
	probe := read402(offeringPost(s, "/@pythia/oracle", "", ""))
	if w := offeringPost(s, "/@pythia/oracle", "", topupPayment(t, probe.Accepts[0], 41)); w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_offering_input") {
		t.Fatalf("paid probe without input: %d %s", w.Code, w.Body.String())
	}
	if w := offeringPost(s, "/@pythia/oracle", string(example), topupPayment(t, probe.Accepts[0], 42)); w.Code != 400 || !strings.Contains(w.Body.String(), "payment_mismatch") {
		t.Fatalf("paid probe with input: %d %s", w.Code, w.Body.String())
	}
	if verified.Load() != 0 {
		t.Fatalf("a probe's payment reached the facilitator (%d verifies)", verified.Load())
	}
	// The example body is an input the offering takes: a payable 402 with
	// the same extension, and a payment echoing it is accepted.
	w := offeringPost(s, "/@pythia/oracle", string(example), "")
	pr := read402(w)
	if q := pr.Accepts[0]["extra"].(map[string]any)["quote"]; q == "discovery-only" || dig(pr.SwarmMemo, "offering", "input_sha256") == nil {
		t.Fatalf("payable 402 %+v", pr)
	}
	var full, envelope map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &full)
	raw, _ := base64.StdEncoding.DecodeString(topupPayment(t, pr.Accepts[0], 43))
	_ = json.Unmarshal(raw, &envelope)
	envelope["resource"], envelope["extensions"] = full["resource"], full["extensions"]
	raw, _ = json.Marshal(envelope)
	if w := offeringPost(s, "/@pythia/oracle", string(example), base64.StdEncoding.EncodeToString(raw)); w.Code != 202 || verified.Load() != 1 {
		t.Fatalf("paid with the echoed extension: %d %s", w.Code, w.Body.String())
	}
	// The offering's JSON is unchanged for a GET asking for JSON.
	if w := makeRequestWithAccept(s, "https://swarmmemo.com/@pythia/oracle", "application/json"); w.Code != 200 || dig(decodeResult(t, w.Body.Bytes()), "data", "offering", "price") != "5" {
		t.Fatalf("GET json: %d %s", w.Code, w.Body.String())
	}
}

// /openapi.json lists each active offering's keyless call as a paid POST
// operation in the profile x402scan reads (x-payment-info with a fixed
// price and protocol x402, security [], the input schema as requestBody),
// never a retired one, with the provider's text shortened, and each entry
// bounded so the document stays bounded.
func TestOfferingsInOpenAPI(t *testing.T) {
	s, _, _, key := offeringServer(t)
	long := strings.Repeat("A long description. ", 100)
	for _, name := range []string{"o2", "o3"} {
		data, _ := json.Marshal(map[string]any{"schema": 1, "name": name, "title": "Offering " + name, "description": long, "price": "0.25", "pay_to": offeringTestWallet})
		if w := topupRequest(s, signService(key, board.Command{Operation: "offering.publish", Data: string(data)}), ""); w.Code != 200 {
			t.Fatalf("publish %s: %d %s", name, w.Code, w.Body.String())
		}
	}
	if w := topupRequest(s, signService(key, board.Command{Operation: "offering.retire", Target: "o2"}), ""); w.Code != 200 {
		t.Fatalf("retire: %d %s", w.Code, w.Body.String())
	}
	w := makeRequest(s, "GET", "https://swarmmemo.com/openapi.json", "", "")
	if w.Code != 200 || w.Header().Get("Cache-Control") != "public, max-age=60" {
		t.Fatalf("openapi: %d %q", w.Code, w.Header().Get("Cache-Control"))
	}
	var doc struct {
		Info  map[string]any            `json:"info"`
		Paths map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	item, ok := doc.Paths["/@pythia/oracle"]
	if !ok || len(item) != 1 || item["post"] == nil {
		t.Fatalf("offering path: %v", item)
	}
	op := item["post"].(map[string]any)
	if dig(op, "x-payment-info", "price", "mode") != "fixed" || dig(op, "x-payment-info", "price", "amount") != "5" || dig(op, "x-payment-info", "price", "currency") != "USD" ||
		dig(op, "x-payment-info", "protocols") == nil || op["x-payment-info"].(map[string]any)["protocols"].([]any)[0].(map[string]any)["x402"] == nil {
		t.Fatalf("x-payment-info: %v", op["x-payment-info"])
	}
	if sec, ok := op["security"].([]any); !ok || len(sec) != 0 || op["summary"] != "Consult the oracle" || dig(op, "responses", "402") == nil {
		t.Fatalf("operation: %v", op)
	}
	schema := dig(op, "requestBody", "content", "application/json", "schema")
	if dig(schema, "properties", "question", "type") != "string" || dig(schema, "required") == nil || dig(op, "requestBody", "required") != true {
		t.Fatalf("requestBody: %v", op["requestBody"])
	}
	if _, ok := doc.Paths["/@pythia/o2"]; ok {
		t.Fatal("a retired offering is listed")
	}
	o3 := doc.Paths["/@pythia/o3"]["post"].(map[string]any)
	if desc := o3["description"].(string); !strings.Contains(desc, "(untrusted)") || strings.Contains(desc, long) || !strings.HasSuffix(desc, "…") ||
		dig(o3, "requestBody", "content", "application/json", "schema", "type") != "object" || dig(o3, "x-payment-info", "price", "amount") != "0.25" {
		t.Fatalf("o3: %v", o3)
	}
	// Bounded: at most OfferingsDirectoryMax entries, each within 6 KiB
	// (the input schema is at most 2 KiB, the text 2×280 bytes).
	entries := s.offeringDirectory()
	if len(entries) != 2 || len(entries) > board.OfferingsDirectoryMax {
		t.Fatalf("entries: %d", len(entries))
	}
	for path, item := range doc.Paths {
		if strings.HasPrefix(path, "/@") {
			if raw, _ := json.Marshal(item); len(raw) > 6<<10 {
				t.Fatalf("%s is %d bytes", path, len(raw))
			}
		}
	}
	if got, err := s.service.(offeringDirectoryStore).OfferingsDirectory(t.Context(), 1); err != nil || len(got) != 1 {
		t.Fatalf("limit 1: %v %d", err, len(got))
	}
	// Retiring takes the path out once the cached list is read again.
	if w := topupRequest(s, signService(key, board.Command{Operation: "offering.retire", Target: "oracle"}), ""); w.Code != 200 {
		t.Fatalf("retire oracle: %d %s", w.Code, w.Body.String())
	}
	s.offeringDirCache.mu.Lock()
	s.offeringDirCache.at = time.Time{}
	s.offeringDirCache.mu.Unlock()
	if w := makeRequest(s, "GET", "https://swarmmemo.com/openapi.json", "", ""); strings.Contains(w.Body.String(), `"/@pythia/oracle"`) || !strings.Contains(w.Body.String(), `"/@pythia/o3"`) {
		t.Fatal("retired oracle still listed")
	}
}

// The keyless POST holds input to the stored schema by JSON value: an enum
// stored escaped by encoding/json takes "R&D" sent either way, an escaped
// "ü" and 1.0 for 1; null is refused for a typed or required property
// (Survivor 5fd2747d, 729ee908).
func TestOfferingHTTPInputEnumByValueAndNull(t *testing.T) {
	s, _, _, key := offeringServer(t)
	data, _ := json.Marshal(map[string]any{"schema": 1, "name": "lookup", "title": "Look it up", "description": "One lookup.", "price": "5", "pay_to": offeringTestWallet, "claim_window": 900,
		"input": map[string]any{"type": "object", "properties": map[string]any{
			"dept": map[string]any{"type": "string", "enum": []string{"R&D", "Ops"}, "default": "R&D"},
			"city": map[string]any{"type": "string", "enum": []string{"Zürich"}},
			"n":    map[string]any{"type": "integer", "enum": []int{1, 2}}},
			"required": []string{"dept"}}})
	if w := topupRequest(s, signService(key, board.Command{Operation: "offering.publish", Data: string(data)}), ""); w.Code != 200 {
		t.Fatalf("publish: %d %s", w.Code, w.Body.String())
	}
	bs := `\`
	esc := strings.NewReplacer("&", bs+"u0026", "ü", bs+"u00fc").Replace
	for _, body := range []string{`{"dept":"R&D"}`, esc(`{"dept":"R&D"}`), esc(`{"dept":"Ops","city":"Zürich"}`), `{"dept":"Ops","city":"Zürich","n":1.0}`} {
		if w := offeringPost(s, "/@pythia/lookup", body, ""); w.Code != 402 || !strings.Contains(w.Body.String(), "input_sha256") {
			t.Fatalf("input %s: %d %s", body, w.Code, w.Body.String())
		}
	}
	for body, detail := range map[string]string{
		`{"dept":"R&D2"}`:            `\"dept\" is not one of its enum.`,
		`{"dept":null}`:              `\"dept\" is required.`,
		`{"dept":"Ops","city":null}`: `\"city\" is a string.`,
		`{"dept":"Ops","n":null}`:    `\"n\" is a integer.`,
	} {
		if w := offeringPost(s, "/@pythia/lookup", body, ""); w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_offering_input") || !strings.Contains(w.Body.String(), detail) {
			t.Fatalf("input %s: %d %s", body, w.Code, w.Body.String())
		}
	}
}
