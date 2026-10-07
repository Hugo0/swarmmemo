package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

// RFC0012 §11 product surfaces. Invariant 9: every number a page shows comes
// from the function its JSON API serves, in the same order. Each page cell
// carries data-key (its path in the API answer) and data-value (the raw
// number); these tests fetch the page and the API from one server and hold
// every cell to the API.

const parityAgent = "7d42f0c944f372b51d832074e62aa3a7829575508504c001a80e0900ff5db5af"

// rfc0012Service answers like a store with the RFC0012 engine on, from fixed
// fixtures shaped as the RFC specifies.
type rfc0012Service struct {
	fakeService
	features board.Features
	note     *board.AllowanceNote
}

func (s *rfc0012Service) Features() board.Features { return s.features }

func (s *rfc0012Service) Execute(ctx context.Context, c board.Command, peer string) (board.Result, error) {
	switch c.Operation {
	case "allowance.get":
		if s.features.Ledger == board.LedgerOff {
			return board.Result{}, &board.Error{Status: 503, Code: "service_unavailable", Message: "off"}
		}
		return board.Result{OK: true, Data: fixtureMap(allowanceFixture), Allowance: s.note}, nil
	case "trust.get":
		if s.features.Trust == board.TrustOff {
			return board.Result{}, &board.Error{Status: 503, Code: "service_unavailable", Message: "off"}
		}
		return board.Result{OK: true, Data: fixtureMap(trustFixture)}, nil
	case "agent.get":
		return board.Result{OK: true, Agent: &board.Agent{ID: parityAgent, Handle: "khepri", Posts: 3, CreatedAt: 1759000000, LastSeen: 1759100000}}, nil
	case "post":
		res, err := s.fakeService.Execute(ctx, c, peer)
		res.Allowance = s.note
		return res, err
	case "room.create", "work.claim":
		// Every write carries the note with the ledger on (C36).
		return board.Result{OK: true, Data: map[string]any{"room": c.Room}, Allowance: s.note}, nil
	}
	return s.fakeService.Execute(ctx, c, peer)
}

func (s *rfc0012Service) ReadActivity(context.Context) (*board.Activity, error) {
	a := &board.Activity{Generated: time.Unix(1759190400, 0).UTC(), Via: map[string]int64{}, Totals: map[string]int64{}}
	for i := range 14 {
		a.Days = append(a.Days, board.ActivityBucket{Start: time.Unix(1759190400-int64(13-i)*86400, 0).UTC()})
	}
	for i := range 24 {
		a.Hours = append(a.Hours, board.ActivityBucket{Start: time.Unix(1759190400-int64(23-i)*3600, 0).UTC()})
	}
	return a, nil
}

func (s *rfc0012Service) AllowanceStats(ctx context.Context, days int) (map[string]any, error) {
	return fixtureMap(waterfallFixture), nil
}

func (s *rfc0012Service) TrustDistribution(ctx context.Context) (map[string]any, error) {
	return fixtureMap(trustDistributionFixture), nil
}

func fixtureMap(raw string) map[string]any {
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		panic(err)
	}
	return m
}

// The store's AllowanceStats: tiers out of order and resources keyed by day
// index, to hold the one normalising function to its sorting.
const waterfallFixture = `{"params_version":3,
 "resources":[
  {"resource":"memory_bytes","day":20359,"budget":16777216,"budget_effective":16777216,"spent":4096,"spent_paid":0,"unallocated":0,
   "tiers":[{"tier":3,"size":8388608,"want":8000000,"claimed":4096,"claimants":1},{"tier":1,"size":4194304,"want":4000000}]},
  {"resource":"post_bytes","day":20359,"budget":67108864,"budget_effective":67108864,"spent":1048576,"spent_paid":0,"unallocated":3355443,
   "tiers":[
    {"tier":4,"size":16777216,"want":0,"spill_in":2097152,"claimed":917504,"lent":65536,"claimants":58},
    {"tier":1,"size":10066329,"want":10066329,"spill_out":1048576,"claimed":2097152,"borrowed":0,"claimants":3},
    {"tier":2,"size":8388608,"want":8388608,"spill_out":1048576,"claimed":1048576,"claimants":2},
    {"tier":3,"size":28521267,"want":20000000,"spill_in":0,"claimed":9437184,"borrowed":65536,"claimants":17},
    {"tier":0,"size":0,"want":0}]}],
 "history":[
  {"day":20357,"resource":"post_bytes","budget":67108864,"issued":30000000,"spent":2500000,"claimants":70},
  {"day":20358,"resource":"post_bytes","budget":67108864,"issued":31000000,"spent":2600000,"claimants":75}],
 "services":[
  {"service":"memory","resource":"memory_bytes","bucket":"free","units":4096,"calls":3},
  {"service":"board","resource":"post_bytes","bucket":"free","units":1048576,"calls":211},
  {"service":"board","resource":"post_bytes","bucket":"granted","units":2048,"calls":1}],
 "transfers":{"resource":"post_bytes","count":4,"volume":262144,"pending":1,"largest_recipient_share_ppm":2500},
 "levers":[{"name":"tier4-shrink","args":"250000","since":1759180000,"until":0,"reason":"Anonymous flood from one network."}]}`

const trustDistributionFixture = `{"mode":"shadow","run":812,"as_of":1759190400,"stale":false,
 "collateral_log10_bins":[40,12,5,1],
 "tier_counts":{"1":2,"2":1,"3":57},"would_be_counts":{"1":3,"2":6,"3":51}}`

const allowanceFixture = `{"schema":1,"agent":"` + parityAgent + `","tier":3,"tier_name":"signed","reason":"Any signed account.","params_version":3,
 "resources":{"post_bytes":{"entitlement":4194304,"used":131072,"remaining":4063232,"incoming":0,"resets_at":1759276800,"prospective":false},
  "memory_bytes":{"entitlement":1048576,"used":0,"remaining":1048576,"incoming":0,"resets_at":1759276800,"prospective":true}}}`

const trustFixture = `{"schema":1,"agent":"` + parityAgent + `","mode":"shadow","run":812,
 "as_of":1759190400,"computed_at":1759192210,"params_version":3,"stale":false,
 "inputs_through":{"events_seq":91234,"endorsements_seq":5521,"ledger_seq":40211},
 "collateral":{"total":1830,"log10":3.26,"unit":"see params"},
 "proofs":[{"kind":"domain","value":"example.com","root":"domain:example.com","state":"verified",
   "forge":1200,"rent":400,"age_days":210,"curve":"ramp","weight_ppm":555000,"contribution":222,
   "saturated_by":null}],
 "endorsements":{"flow":{"a":13,"b":11,"effective":11,"unit":"1/20 fair share"},
   "endorsers":[
    {"agent":"9eb0e9479b2e18fc1502fe50106a09524c834d535cd43c72c50716f558ac213c","kinds":["vote","vouch"],"weight_ppm":712000,"flow":6},
    {"agent":"5e4dd880110baa732132e832c12288ee143dae88304b5da13e1b1a54c9eab7d7","kinds":["vote"],"weight_ppm":500000,"flow":3},
    {"agent":"c7e49413552853422b1396f0af47733e811bcb255572a270bf265c9e19378994","kinds":["vote"],"weight_ppm":400000,"flow":1},
    {"agent":"9cf9c2894cacd6ec6bf9d119a9f88102869bfdf9b7bc2d8bcac71e817a5556a0","kinds":["vote"],"weight_ppm":300000,"flow":1},
    {"agent":"57848efe655d7c3c66013378f7bf2c7264a907536f55733e56891b079636e410","kinds":["vote"],"weight_ppm":200000,"flow":0},
    {"agent":"1111111111111111111111111111111111111111111111111111111111111111","kinds":["vote"],"weight_ppm":100000,"flow":0}],
   "endorsers_total":9,"down_votes":1},
 "liability":{"penalties":[],"evidence":[]},
 "sponsor":{"sponsored_by":null,"sponsoring":2,"dividends":{"units":0,"resource":"credit"}},
 "breaker":{"active":false},
 "tier":{"would_be":2,"effective":3,"reason":"shadow: Design 0 rules allocate"},
 "caveats":["history is lagged one run"]}`

var allOn = board.Features{Ledger: board.LedgerOn, Trust: board.TrustShadow, Services: []string{"memory"}, VoteRecords: true}

func rfc0012Server(f board.Features) (*Server, *rfc0012Service) {
	svc := &rfc0012Service{features: f}
	return New(svc, web.Handler(svc), Config{Features: f}), svc
}

var cellRE = regexp.MustCompile(`data-key="([^"]+)" data-value="(-?\d+)"`)

// pageCells returns the page's data-key/data-value pairs in document order.
func pageCells(t *testing.T, body string) [][2]string {
	t.Helper()
	var out [][2]string
	for _, m := range cellRE.FindAllStringSubmatch(body, -1) {
		out = append(out, [2]string{m[1], m[2]})
	}
	return out
}

// lookup follows a dotted path (object keys and array indexes) in decoded JSON.
func lookup(v any, path string) (any, bool) {
	for _, part := range strings.Split(path, ".") {
		switch node := v.(type) {
		case map[string]any:
			next, ok := node[part]
			if !ok {
				return nil, false
			}
			v = next
		case []any:
			i, err := strconv.Atoi(part)
			if err != nil || i < 0 || i >= len(node) {
				return nil, false
			}
			v = node[i]
		default:
			return nil, false
		}
	}
	return v, true
}

// holdToAPI checks every page cell against the API answer: the key exists,
// the value is equal, and rows of one list appear in the API's order.
func holdToAPI(t *testing.T, surface string, cells [][2]string, api any) {
	t.Helper()
	if len(cells) == 0 {
		t.Fatalf("%s shows no API-backed numbers", surface)
	}
	last := map[string]int{}
	index := regexp.MustCompile(`^(.*?)\.(\d+)\.`)
	for _, c := range cells {
		v, ok := lookup(api, c[0])
		if !ok {
			t.Errorf("%s shows %s, which the API does not return", surface, c[0])
			continue
		}
		n, ok := v.(float64)
		if !ok {
			if b, isBool := v.(bool); isBool && b {
				n, ok = 1, true
			}
		}
		if !ok || strconv.FormatInt(int64(n), 10) != c[1] {
			t.Errorf("%s shows %s = %s, the API says %v", surface, c[0], c[1], v)
		}
		for rest := c[0]; ; {
			m := index.FindStringSubmatch(rest)
			if m == nil {
				break
			}
			i, _ := strconv.Atoi(m[2])
			if prev, seen := last[m[1]]; seen && i < prev {
				t.Errorf("%s lists %s out of the API's order", surface, c[0])
			}
			last[m[1]] = i
			rest = strings.Replace(rest, m[0], m[1]+"#"+m[2]+"#", 1)
		}
	}
}

func getJSON(t *testing.T, s http.Handler, method, path, body string) map[string]any {
	t.Helper()
	w := makeRequest(s, method, path, body, "application/json")
	if w.Code != 200 {
		t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func getHTML(t *testing.T, s http.Handler, path string) string {
	t.Helper()
	r := httptest.NewRequest("GET", path, nil)
	r.Header.Set("Accept", "text/html")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("GET %s: %d", path, w.Code)
	}
	return w.Body.String()
}

// /stats and /api/stats/allowance: waterfall, services, transfers, history,
// trust distribution.
func TestAllowanceStatsPageMatchesAPI(t *testing.T) {
	s, _ := rfc0012Server(allOn)
	api := getJSON(t, s, "GET", "/api/stats/allowance", "")
	data := api["data"]
	page := getHTML(t, s, "/stats")
	start := strings.Index(page, `id="stats-allowance"`)
	if start < 0 {
		t.Fatal("/stats has no allowance section with the ledger on")
	}
	holdToAPI(t, "/stats", pageCells(t, page[start:]), data)
	// The one sentence, the tiers in order, and the numbers the API derives.
	for _, want := range []string{web.WaterfallSentence, "Trusted", "Anonymous", "tier4-shrink", "Trust estimates", "under 10", "/api/stats/allowance"} {
		if !strings.Contains(page, want) {
			t.Errorf("/stats lacks %q", want)
		}
	}
	tiers, _ := lookup(data, "allowance.resources.0.tiers")
	if list, _ := tiers.([]any); len(list) != 5 || list[0].(map[string]any)["tier"] != 0.0 || list[4].(map[string]any)["name"] != "anonymous" {
		t.Fatalf("tiers not normalised: %v", tiers)
	}
	if r, _ := lookup(data, "allowance.resources.0.resource"); r != "post_bytes" {
		t.Fatalf("posting is not listed first: %v", r)
	}
	if fill, _ := lookup(data, "allowance.resources.0.tiers.4.fill_ppm"); fill != 52083.0 {
		t.Fatalf("tier 4 fill = %v", fill)
	}
	// Never split by who runs an agent.
	main := page[strings.Index(page, "<h1>"):strings.Index(page, "<footer")]
	for _, unwanted := range []string{"ommunity", "Our agents", "perator"} {
		if strings.Contains(main, unwanted) {
			t.Errorf("/stats sets participants apart: %q", unwanted)
		}
	}
	if regexp.MustCompile(`\sstyle=`).MatchString(page) {
		t.Error("/stats uses an inline style attribute, which the CSP blocks")
	}
	if w := makeRequest(s, "GET", "/api/stats/allowance?days=31", "", ""); w.Code != 400 {
		t.Fatalf("days=31: %d", w.Code)
	}
}

// With only trust on, the page and API carry the trust distribution alone.
func TestAllowanceStatsTrustOnly(t *testing.T) {
	s, _ := rfc0012Server(board.Features{Trust: board.TrustShadow})
	api := getJSON(t, s, "GET", "/api/stats/allowance", "")
	if _, ok := lookup(api["data"], "allowance"); ok {
		t.Fatal("waterfall served with the ledger off")
	}
	page := getHTML(t, s, "/stats")
	if strings.Contains(page, `id="stats-allowance"`) || !strings.Contains(page, `id="stats-trust"`) {
		t.Fatal("trust-only /stats shows the wrong sections")
	}
	holdToAPI(t, "/stats", pageCells(t, page), api["data"])
}

// Agent pages: allowance at a glance and trust parts, against allowance.get
// and trust.get over /v1/command.
func TestAgentPageMatchesAllowanceAndTrustAPI(t *testing.T) {
	s, _ := rfc0012Server(allOn)
	page := getHTML(t, s, "/agent/"+parityAgent)
	allowanceAt, trustAt := strings.Index(page, `id="allowance"`), strings.Index(page, `id="trust"`)
	if allowanceAt < 0 || trustAt < allowanceAt {
		t.Fatal("agent page lacks the allowance and trust sections")
	}
	allowance := getJSON(t, s, "POST", "/v1/command", `{"operation":"allowance.get","target":"`+parityAgent+`"}`)
	answer, err := web.AllowanceAnswerFrom(allowance["data"].(map[string]any))
	if err != nil {
		t.Fatal(err)
	}
	var normalized any
	raw, _ := json.Marshal(answer)
	_ = json.Unmarshal(raw, &normalized)
	holdToAPI(t, "agent allowance", pageCells(t, page[allowanceAt:trustAt]), normalized)
	// The normaliser only orders: each resource's numbers are the API's own.
	resources := allowance["data"].(map[string]any)["resources"].(map[string]any)
	if answer.Resources[0].Resource != "post_bytes" || float64(answer.Resources[0].Remaining) != resources["post_bytes"].(map[string]any)["remaining"] {
		t.Fatalf("allowance normalised wrongly: %+v", answer.Resources)
	}
	trust := getJSON(t, s, "POST", "/v1/command", `{"operation":"trust.get","target":"`+parityAgent+`"}`)
	cells := pageCells(t, page[trustAt:])
	holdToAPI(t, "agent trust", cells, trust["data"])
	endorsers := 0
	for _, c := range cells {
		if strings.HasPrefix(c[0], "endorsements.endorsers.") {
			endorsers++
		}
	}
	if endorsers != 5 {
		t.Fatalf("agent page shows %d endorsers, want the API's first 5", endorsers)
	}
	for _, unwanted := range []string{"is human", "trustworthy", "grade"} {
		if strings.Contains(strings.ToLower(page[trustAt:]), unwanted) {
			t.Errorf("trust copy says %q", unwanted)
		}
	}
}

// A write's result says what the caller got today on every HTTP wire:
// next.allowance in JSON and MCP, one line after the ok line in text.
func TestFirstWriteCarriesAllowanceLine(t *testing.T) {
	s, svc := rfc0012Server(allOn)
	svc.note = &board.AllowanceNote{Line: "Free today: 4 MiB of posting (signed tier), 3.9 MiB left, resets 00:00 UTC. More: link a domain or be endorsed; see /capabilities#allowance.", Resource: "post_bytes", Tier: 3, Entitlement: 4 << 20, Remaining: 4063232, ResetsAt: 1759276800, More: "link a domain or be endorsed"}
	res := getJSON(t, s, "GET", "/w/lobby/main?text=hello&format=json", "")
	next, _ := res["next"].(map[string]any)
	note, _ := next["allowance"].(map[string]any)
	if note["line"] != svc.note.Line || note["remaining"] != 4063232.0 || next["sign_to_get_replies"] == nil {
		t.Fatalf("JSON post lacks next.allowance beside the anonymous advice: %v", res["next"])
	}
	text := makeRequest(s, "GET", "/w/lobby/main?text=hello", "", "").Body.String()
	lines := strings.Split(text, "\n")
	if !strings.HasPrefix(lines[0], "ok ") || !strings.Contains(text, "\n"+svc.note.Line+"\n") {
		t.Fatalf("text post lacks the line after ok:\n%s", text)
	}
	read := getJSON(t, s, "POST", "/v1/command", `{"operation":"allowance.get"}`)
	if n, _ := read["next"].(map[string]any); n == nil || n["allowance"] == nil {
		t.Fatal("allowance.get lacks next.allowance")
	}
	// A write other than a post: JSON on /v1/command and /c64/, the line on
	// the text rendering.
	write := getJSON(t, s, "POST", "/v1/command", `{"operation":"room.create","room":"notes"}`)
	if n, _ := write["next"].(map[string]any); n == nil || n["allowance"].(map[string]any)["line"] != svc.note.Line {
		t.Fatalf("room.create lacks next.allowance: %v", write)
	}
	c64 := "https://swarmmemo.com/c64/" + base64.RawURLEncoding.EncodeToString([]byte(`{"operation":"room.create","room":"notes"}`))
	r := httptest.NewRequest("GET", c64, nil)
	r.Header.Set("Accept", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	var viaC64 map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &viaC64)
	if n, _ := viaC64["next"].(map[string]any); n == nil || n["allowance"] == nil {
		t.Fatalf("c64 room.create lacks next.allowance: %d %s", w.Code, w.Body.String())
	}
	var plain strings.Builder
	WriteText(&plain, board.Result{OK: true, Data: map[string]any{"room": "notes"}, Allowance: svc.note})
	if !strings.Contains(plain.String(), svc.note.Line+"\n") {
		t.Fatalf("text room.create lacks the line:\n%s", plain.String())
	}
	// Without a note (flags off, or an exact retry) nothing is added.
	svc.note = nil
	res = getJSON(t, s, "GET", "/w/lobby/main?text=hello&format=json", "")
	if next, _ := res["next"].(map[string]any); next["allowance"] != nil {
		t.Fatal("next.allowance without a note")
	}
}

// With every flag off the new route falls through, and no page, tool or
// instruction mentions the allowance ledger.
func TestRFC0012SurfacesInvisibleWhileOff(t *testing.T) {
	s, _ := rfc0012Server(board.Features{})
	plain := New(&fakeService{}, nil, Config{})
	if a, b := makeRequest(s, "GET", "/api/stats/allowance", "", ""), makeRequest(plain, "GET", "/api/stats/nothing", "", ""); a.Code != b.Code {
		t.Fatalf("/api/stats/allowance answers %d while off, unknown routes %d", a.Code, b.Code)
	}
	for _, path := range []string{"/stats", "/agent/" + parityAgent, "/for-agents", "/docs"} {
		page := getHTML(t, s, path)
		for _, unwanted := range []string{"data-key=", "Free allowance", "Allowance today", "Trust estimate", web.WaterfallSentence} {
			if strings.Contains(page, unwanted) {
				t.Errorf("%s shows %q with every flag off", path, unwanted)
			}
		}
	}
	for _, path := range []string{"/llms.txt", "/llms-full.txt", "/openapi.json", "/.well-known/mcp/server-card.json"} {
		body := makeRequest(s, "GET", path, "", "").Body.String()
		// llms-full.txt embeds docs/PROTOCOL.md, which documents the off-by-default
		// features as such; the served instructions above it must not.
		served := body
		if path == "/llms-full.txt" {
			served = body[:strings.Index(body, "# Full command reference")]
		}
		if strings.Contains(served, "allowance.get") || strings.Contains(served, web.WaterfallSentence) || strings.Contains(served, "/api/stats/allowance") {
			t.Errorf("%s mentions the allowance ledger with every flag off", path)
		}
		if body != makeRequest(plain, "GET", path, "", "").Body.String() {
			t.Errorf("%s differs from a server without RFC0012 flags", path)
		}
	}
	if len(s.mcpToolList()) != len(mcpTools) {
		t.Fatal("MCP lists RFC0012 tools with every flag off")
	}
}

// With the flags on, agent-facing instructions explain the waterfall in one
// sentence and point at the reads; the MCP tools and server card agree.
func TestRFC0012InstructionsAndToolsWhenOn(t *testing.T) {
	s, _ := rfc0012Server(allOn)
	for _, path := range []string{"/llms.txt", "/llms-full.txt", "/skill.md"} {
		body := makeRequest(s, "GET", path, "", "").Body.String()
		// The short forms name the tools search and the featured memory
		// tool; the long form keeps every service's line.
		tool := "\n- Tools: "
		if path == "/llms-full.txt" {
			tool = "\n- Memory: "
		}
		for _, want := range []string{"## Free allowance", web.WaterfallSentence, "next.allowance", "/api/allowance", tool, "## Trust estimates", "## Vouches"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s lacks %q", path, want)
			}
		}
		// Each service's methods and limits: the long form.
		if path == "/llms-full.txt" && (!strings.Contains(body, "## Memory") || !strings.Contains(body, board.LimitText("memory_value_bytes"))) {
			t.Errorf("%s lacks the memory section", path)
		}
	}
	page := getHTML(t, s, "/for-agents")
	if !strings.Contains(page, "next.allowance") || !strings.Contains(page, "Posting spends a free daily allowance") {
		t.Error("/for-agents quickstart does not explain the allowance")
	}
	var openapi map[string]any
	_ = json.Unmarshal(makeRequest(s, "GET", "/openapi.json", "", "").Body.Bytes(), &openapi)
	description := dig(openapi, "paths", "/v1/command", "post", "description").(string)
	for _, want := range []string{"every write", "quota.get/allowance.get", "next.allowance"} {
		if !strings.Contains(description, want) {
			t.Errorf("command description omits %q with the ledger enabled", want)
		}
	}
	if _, ok := openapi["paths"].(map[string]any)["/api/stats/allowance"]; !ok {
		t.Error("/openapi.json lacks /api/stats/allowance")
	}
	names := map[string]bool{}
	for _, tool := range s.serverCard()["tools"].([]map[string]any) {
		names[tool["name"].(string)] = true
	}
	for _, want := range []string{"allowance", "trust", "memory_get"} {
		if !names[want] {
			t.Errorf("server card lacks %s", want)
		}
	}
	server := httptest.NewServer(s)
	defer server.Close()
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"allowance","arguments":{"agent":"`+parityAgent+`"}}}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var envelope struct {
		Result struct {
			StructuredContent map[string]any `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if v, _ := lookup(envelope.Result.StructuredContent, "data.tier"); v != 3.0 {
		t.Fatalf("MCP allowance tool returned %v", envelope.Result.StructuredContent)
	}
}
