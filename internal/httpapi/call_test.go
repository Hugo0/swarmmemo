package httpapi

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/board"
	"swarmmemo/internal/ledger"
	"swarmmemo/internal/services"
	"swarmmemo/internal/web"
)

// A service call without a key is one plain URL: /call/SERVICE/METHOD with
// the method's arguments, max_cost and request_id as query fields, over GET
// or POST, answered in JSON; the same call is a hosted MCP tool. Every
// surface says so from the same catalogue and parameters.

func anonCallServer(t *testing.T, anonCap int64) (*board.Store, *Server) {
	t.Helper()
	f := board.Features{Services: []string{"notary", "memory"}, Ledger: board.LedgerOn, AnonPrefix: true}
	store, err := board.Open(filepath.Join(t.TempDir(), "board.sqlite"), board.Config{ServiceID: "swarmmemo.com", Features: f})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	p := ledger.DefaultAllowanceParams()
	rp := p.Resources[allowance.Credit]
	rp.Budget, rp.SpendCeiling, rp.InboundCap = 1_600_000, 1_600_000, 1_600_000
	rp.Cap = []int64{400_000, 200_000, 100_000, anonCap}
	rp.Floor = []int64{1600, 1600, 1600, anonCap}
	rp.RootCap = []int64{1_600_000, 800_000, 400_000, anonCap}
	rp.ShareMaxPPM = []int64{1_000_000, 1_000_000, 1_000_000, 100_000}
	if _, err = store.SetAllowanceParams(t.Context(), ledger.AllowanceNamespace, p.Marshal(), "test", 0); err != nil {
		t.Fatal(err)
	}
	return store, New(store, nil, Config{Features: f, PublicURL: "https://swarmmemo.com", ServiceID: "swarmmemo.com", AllowInsecureLocal: true})
}

func TestCallRouteWithoutAKey(t *testing.T) {
	_, s := anonCallServer(t, 2000)
	get := func(path string) (int, map[string]any, string) {
		w := makeRequest(s, "GET", "https://swarmmemo.com"+path, "", "")
		var body map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		return w.Code, body, w.Body.String()
	}
	stamp := "/call/notary/stamp?text=" + url.QueryEscape("plan & notes") + "&max_cost=1&request_id=request-id-000001"
	code, body, raw := get(stamp)
	if code != 200 || dig(body, "data", "call", "state") != "done" || dig(body, "data", "call", "cost") != float64(1) || dig(body, "data", "result", "receipt", "hash") == nil {
		t.Fatalf("GET call: %d %s", code, raw)
	}
	// The same URL again (a prefetch, a retry) is the same call, not a new charge.
	_, again, _ := get(stamp)
	if dig(again, "data", "call", "id") != dig(body, "data", "call", "id") {
		t.Fatalf("retry: %v vs %v", dig(again, "data", "call"), dig(body, "data", "call"))
	}
	// POST with a form body.
	w := makeRequest(s, "POST", "https://swarmmemo.com/call/notary/stamp", "text=form&max_cost=1&request_id=request-id-000002", "application/x-www-form-urlencoded")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"done"`) {
		t.Fatalf("POST form: %d %s", w.Code, w.Body.String())
	}
	// A public read works the same way; so does an unsigned POST /v1/command.
	if code, _, raw := get("/call/notary/key"); code != 200 || !strings.Contains(raw, "public_key") {
		t.Fatalf("read over /call/: %d %s", code, raw)
	}
	cmd, _ := json.Marshal(board.Command{Operation: "service.call", Target: "notary", Data: `{"schema":1,"method":"stamp","args":{"text":"json"},"max_cost":1}`, RequestID: "request-id-000003"})
	if w := makeRequest(s, "POST", "https://swarmmemo.com/v1/command", string(cmd), "application/json"); w.Code != 200 {
		t.Fatalf("unsigned /v1/command: %d %s", w.Code, w.Body.String())
	}
	for _, c := range []struct {
		method, path, body, ct string
		status                 int
		code                   string
	}{
		{"GET", "/call/memory/put?key=k&value=v&max_cost=300&request_id=m1", "", "", 401, "signature_required"},
		{"GET", "/call/notary/stamp?text=x&max_cost=1", "", "", 400, "invalid_request"},                                                // no request_id
		{"GET", "/call/notary/stamp?text=x&request_id=z", "", "", 400, "invalid_request"},                                              // no max_cost
		{"GET", "/call/notary/stamp?text=x&max_cost=0&request_id=request-id-00000z0", "", "", 409, "price_exceeds_max"},                // ceiling below price
		{"GET", "/call/notary/stamp?text=x&max_cost=1&request_id=short-id", "", "", 400, "invalid_request"},                            // request_id under 16 characters
		{"GET", "/call/notary/stamp?text=x&max_cost=1&request_id=z1&nope=1", "", "", 400, "invalid_request"},                           // unknown field
		{"GET", "/call/notary/stamp?text=x&max_cost=1&request_id=z2&public_key=abc", "", "", 400, "invalid_request"},                   // signed fields
		{"GET", "/call/notary/stamp?text=x&text=y&max_cost=1&request_id=z3", "", "", 400, "invalid_request"},                           // repeated
		{"POST", "/call/notary/stamp?max_cost=1", "text=x&request_id=z4", "application/x-www-form-urlencoded", 400, "invalid_request"}, // query and body
		{"POST", "/call/notary/stamp", `{"text":"x"}`, "application/json", 400, "invalid_request"},
		{"GET", "/call/nope/x?max_cost=1&request_id=z5", "", "", 400, "invalid_service"},
		{"GET", "/call/notary", "", "", 400, "invalid_request"},
		{"PUT", "/call/notary/stamp", "", "", 405, "method_not_allowed"},
	} {
		w := makeRequest(s, c.method, "https://swarmmemo.com"+c.path, c.body, c.ct)
		if w.Code != c.status || !strings.Contains(w.Body.String(), `"`+c.code+`"`) {
			t.Errorf("%s %s: %d %s, want %d %s", c.method, c.path, w.Code, w.Body.String(), c.status, c.code)
		}
	}
	// Another site's page cannot make its visitors spend their network's
	// share with an image or a frame.
	for mode, want := range map[string]int{"no-cors": 403, "navigate": 403, "cors": 403} {
		r := httptest.NewRequest("GET", "https://swarmmemo.com/call/notary/stamp?text="+mode+"&max_cost=1&request_id=x-"+mode, nil)
		r.RemoteAddr = "198.51.100.8:12345"
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		r.Header.Set("Sec-Fetch-Mode", mode)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("cross-site %s: %d %s", mode, w.Code, w.Body.String())
		}
	}
	if w := makeRequest(s, "GET", "https://swarmmemo.com/robots.txt", "", ""); !strings.Contains(w.Body.String(), "Disallow: /call/") {
		t.Errorf("robots.txt: %s", w.Body.String())
	}
}

func TestCallSurfacesSayItOnce(t *testing.T) {
	_, s := anonCallServer(t, 2000)
	caps := decodeResult(t, makeRequest(s, "GET", "https://swarmmemo.com/capabilities", "", "").Body.Bytes())
	nk, _ := dig(caps, "services", "without_key").(map[string]any)
	line := "No key needed for the notary: 2,000 credits a day per network."
	example := "https://swarmmemo.com/call/notary/stamp?text=Plan+for+2026-09-29%3A+ship+the+catalogue.&max_cost=1&request_id=" + services.AnonymousRequestIDExample
	if nk["available"] != true || nk["line"] != line || nk["example"] != example || nk["credits_per_day"] != float64(2000) || nk["all_credits_per_day"] != float64(160000) {
		t.Fatalf("capabilities without_key: %+v", nk)
	}
	// The example works once its placeholder request_id is replaced with a
	// random one; pasted as it is, it is refused with the rule.
	if w := makeRequest(s, "GET", example, "", ""); w.Code != 400 || !strings.Contains(w.Body.String(), "at least 16 characters") {
		t.Fatalf("the example URL as pasted: %d %s", w.Code, w.Body.String())
	}
	if w := makeRequest(s, "GET", strings.Replace(example, services.AnonymousRequestIDExample, "3f9a0c2b7e5d4a61b8c0", 1), "", ""); w.Code != 200 {
		t.Fatalf("the example URL: %d %s", w.Code, w.Body.String())
	}
	api := makeRequest(s, "GET", "https://swarmmemo.com/api/services", "", "").Body.String()
	if !strings.Contains(api, `"anonymous":true`) || !strings.Contains(api, `"without_key"`) || !strings.Contains(api, line) {
		t.Fatalf("/api/services: %s", api)
	}
	llms := makeRequest(s, "GET", "https://swarmmemo.com/llms.txt", "", "").Body.String()
	if !strings.Contains(llms, line) || !strings.Contains(llms, example) || !strings.Contains(llms, "curl -sS 'https://swarmmemo.com/call/notary/stamp?") {
		t.Fatalf("/llms.txt does not say it: %s", llms)
	}
	// MCP: the method is a hosted tool that bills the caller's network.
	server := httptest.NewServer(s)
	defer server.Close()
	list := mcpCall(t, server.URL, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	found := false
	for _, tool := range dig(list, "result", "tools").([]any) {
		if tool.(map[string]any)["name"] == "notary_stamp" {
			found = true
			desc := tool.(map[string]any)["description"].(string)
			if !strings.Contains(desc, "No key needed") || tool.(map[string]any)["annotations"].(map[string]any)["readOnlyHint"] == true {
				t.Fatalf("notary_stamp tool: %+v", tool)
			}
		}
	}
	if !found {
		t.Fatal("no notary_stamp tool")
	}
	out := mcpCall(t, server.URL, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"notary_stamp","arguments":{"text":"over mcp","max_cost":1,"request_id":"mcp-request-id-0001"}}}`)
	if dig(out, "result", "isError") == true || dig(out, "result", "structuredContent", "data", "call", "state") != "done" {
		t.Fatalf("MCP call without a key: %+v", out)
	}
	out = mcpCall(t, server.URL, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"notary_stamp","arguments":{"text":"short id","max_cost":1,"request_id":"mcp-1"}}}`)
	if dig(out, "result", "isError") != true && dig(out, "error") == nil {
		t.Fatalf("an MCP call with a short request_id must be refused: %+v", out)
	}
	// Each anonymous method's no-key example parses into its own call.
	for _, e := range services.Catalog(services.Known()) {
		ex := web.ServiceExamples("https://swarmmemo.com", e)
		if ex.NoKey == "" {
			continue
		}
		raw := strings.TrimPrefix(strings.Fields(ex.NoKey)[2], "'")
		u, err := url.Parse(strings.TrimSuffix(raw, "'"))
		if err != nil {
			t.Fatal(err)
		}
		_, m, ok := services.LookupMethod(services.Catalog(services.Known()), e.ID, strings.TrimPrefix(u.Path, services.CallPathPrefix+e.ID+"/"))
		if !ok || !m.Anonymous {
			t.Fatalf("%s: no-key example %s", e.ID, ex.NoKey)
		}
		data, rid, err := services.CallData(m, u.Query())
		if _, perr := services.ParseData(data, true); err != nil || perr != nil || rid == "" {
			t.Fatalf("%s: no-key example %s: %v %v", e.ID, ex.NoKey, err, perr)
		}
	}
}

func TestCallSurfacesSilentWithoutCredit(t *testing.T) {
	_, s := anonCallServer(t, 0)
	caps := decodeResult(t, makeRequest(s, "GET", "https://swarmmemo.com/capabilities", "", "").Body.Bytes())
	if dig(caps, "services", "without_key", "available") != false || dig(caps, "services", "without_key", "line") != nil {
		t.Fatalf("without_key with no anonymous credit: %+v", dig(caps, "services", "without_key"))
	}
	llms := makeRequest(s, "GET", "https://swarmmemo.com/llms.txt", "", "").Body.String()
	if strings.Contains(llms, "No key needed") || strings.Contains(llms, "/call/") {
		t.Fatal("/llms.txt advertises calls without a key while there is no credit for them")
	}
	w := makeRequest(s, "GET", "https://swarmmemo.com/call/notary/stamp?text=x&max_cost=1&request_id=o1", "", "")
	if w.Code != 403 || !strings.Contains(w.Body.String(), "signed_only") {
		t.Fatalf("a call with no anonymous credit: %d %s", w.Code, w.Body.String())
	}
}
