package httpapi

// The hosted service tools a hosted identity writes with (memory_put,
// wakeup_schedule, x402_tools_call): each signs the same service.call a
// local key would, so the engine's vetting, caps, prices and receipts are
// the signed path's. Real store, real engine, a fake paid-tools API.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/board"
	"swarmmemo/internal/ledger"
	"swarmmemo/internal/services"
	"swarmmemo/internal/web"
)

const (
	paidToolOK    = "mpp.weather.forecast"
	paidToolUnvet = "bazaar.weather-unvetted"
	paidToolsKey  = "fk_test_0123456789abcdef"
)

// fakePaidTools is the paid-tools API: search, probe, a descriptor per
// tool and invoke, each refused without the key.
type fakePaidTools struct {
	mu      sync.Mutex
	invokes int
}

func (f *fakePaidTools) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+paidToolsKey {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	raw, _ := io.ReadAll(r.Body)
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/v1/tools/search":
		fmt.Fprintf(w, `{"search_id":"srch_test-1","hits":[
 {"id":%q,"title":"Daily weather forecast","description":"Forecast by city.","capabilities":["weather"],"payment":{"protocol":"x402v2","price_hint":"0.002"},"input_schema":{"type":"http","body":{"city":"London"}}},
 {"id":%q,"title":"Unvetted weather","description":"Unvetted forecast.","capabilities":["weather"],"payment":{"price_hint":"0.001"},"unvetted":true}
],"searches":1,"partial":false}`, paidToolOK, paidToolUnvet)
	case "/v1/tools/probe":
		var in struct{ IDs []string }
		_ = json.Unmarshal(raw, &in)
		var out []string
		for _, id := range in.IDs {
			out = append(out, `{"id":"`+id+`","live":true,"status":402,"price_usd":0.002,"payable":true,"input_schema":{"type":"http","body":{"city":"London"}}}`)
		}
		fmt.Fprintf(w, `{"results":[%s]}`, strings.Join(out, ","))
	case "/v1/tools/invoke":
		f.invokes++
		_, _ = io.WriteString(w, `{"results":[{"delivered":true,"response":{"temp_c":14}}],"billing":{"charged_credits":2,"balance_credits":2997}}`)
	default:
		id := strings.TrimPrefix(r.URL.Path, "/v1/tools/")
		if id != paidToolOK && id != paidToolUnvet {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, `{"id":%q,"title":"Weather","description":"Weather by city","capabilities":["weather"],
"invocation":{"method":"POST","url":"https://weather.example.com/forecast","params_schema":{"type":"http","body":{"city":"London"}}},
"payment":{"protocol":"x402v2","price_hint":"0.002"},"signals":{"host":"weather.example.com"}}`, id)
	}
}

func (f *fakePaidTools) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.invokes
}

// hostedServicesServer is a store with hosted identities, the allowance
// ledger and memory, wake-ups and the x402 relay on, its paid tools served
// by fakePaidTools.
func hostedServicesServer(t *testing.T) (*Server, *fakePaidTools) {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	kek := write("hosted-kek", base64.RawURLEncoding.EncodeToString(key)+"\n")
	config := write("x402.json", `{"schema":1,"enabled":true,"network":"eip155:8453","asset":"0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913","asset_name":"USD Coin","asset_version":"2","decimals":6,
"caps":{"global_daily":"1","agent_daily":"0.5","per_call":"0.05"},
"wallet_key_file":"`+write("wallet.key", "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80\n")+`",
"allowlist_file":"`+write("allowlist.json", `{"schema":1,"version":2,"resources":[]}`)+`",
"bundlers":{"frames":{"key_file":"`+write("tools.key", paidToolsKey+"\n")+`","base_url":"https://example.com/v1","open":true}}}`)
	x402, err := services.LoadX402Config(config)
	if err != nil {
		t.Fatal(err)
	}
	api := &fakePaidTools{}
	srv := httptest.NewTLSServer(api)
	t.Cleanup(srv.Close)
	target := srv.Listener.Addr().String()
	x402.UseTestUpstream(srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs, func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr != "example.com:443" {
			return nil, fmt.Errorf("unexpected address %s", addr)
		}
		return (&net.Dialer{}).DialContext(ctx, network, target)
	})
	features := board.Features{Services: []string{"memory", "wakeup", "x402"}, Ledger: board.LedgerOn, AnonPrefix: true}
	store, err := board.Open(filepath.Join(dir, "board.sqlite"), board.Config{ServiceID: "swarmmemo.com", Features: features, HostedKEKFile: kek, X402: x402})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	p := ledger.DefaultAllowanceParams()
	rp := p.Resources[allowance.Credit]
	rp.Budget, rp.SpendCeiling, rp.InboundCap = 10_000_000, 10_000_000, 10_000_000
	rp.Cap = []int64{1_000_000, 1_000_000, 1_000_000, 1_000_000}
	rp.Floor = []int64{100_000, 100_000, 100_000, 100_000}
	rp.RootCap = []int64{10_000_000, 10_000_000, 10_000_000, 10_000_000}
	rp.ShareMaxPPM = []int64{1_000_000, 1_000_000, 1_000_000, 1_000_000}
	// The defaults give the anonymous tier, a hosted identity's until it is
	// claimed, no memory; this deployment gives it some.
	mp := p.Resources[allowance.MemoryBytes]
	mp.Cap[3], mp.Floor[3], mp.RootCap[3], mp.ShareMaxPPM[3] = 1<<20, 16<<10, 1<<20, 1_000_000
	if _, err = store.SetAllowanceParams(t.Context(), ledger.AllowanceNamespace, p.Marshal(), "test", 0); err != nil {
		t.Fatal(err)
	}
	return New(store, nil, Config{Features: features, PublicURL: "https://swarmmemo.com", ServiceID: "swarmmemo.com"}), api
}

// creditsUsed is how much of its credit allowance the agent used today.
func creditsUsed(t *testing.T, s *Server, agent string) float64 {
	t.Helper()
	got := mustTool(t, s, "/mcp", "", "allowance", map[string]any{"agent": agent})
	resources, _ := dig(got, "data", "resources").([]any)
	for _, r := range resources {
		if dig(r, "resource") == string(allowance.Credit) {
			used, _ := dig(r, "used").(float64)
			return used
		}
	}
	t.Fatalf("no credit allowance: %v", got)
	return 0
}

// A hosted identity calls a vetted paid tool on its own allowance; the
// signed path's refusals hold: a price over max_cost, an unvetted tool, a
// missing max_cost, and a resource that is not a tool.
func TestHostedToolsCallPaidTool(t *testing.T) {
	s, api := hostedServicesServer(t)
	me := newIdentity(t, s, "")
	url, agent := "/mcp/t/"+me["token"].(string), me["agent"].(string)
	if _, failure := callTool(t, s, "/mcp", "", "x402_tools_call", map[string]any{"resource": "tool:" + paidToolOK, "max_cost": 30000}); !strings.Contains(failure, "hosted") {
		t.Fatalf("without an identity: %q", failure)
	}
	mustTool(t, s, url, "", "x402_tools_search", map[string]any{"query": "weather"})
	got := mustTool(t, s, url, "", "x402_tools_call", map[string]any{"resource": "tool:" + paidToolOK, "body": map[string]any{"city": "Paris"}, "max_cost": 30000})
	cost, _ := dig(got, "data", "call", "cost").(float64)
	if dig(got, "data", "call", "state") != "done" || cost <= 0 || dig(got, "data", "call", "id") == nil ||
		dig(got, "data", "result", "payment", "price") != "0.002" || !strings.Contains(fmt.Sprint(dig(got, "data", "result", "body")), "temp_c:14") {
		t.Fatalf("x402_tools_call: %v", got)
	}
	if used := creditsUsed(t, s, agent); used != cost || api.count() != 1 {
		t.Fatalf("charged %v to the identity's allowance and invoked %d times, want %v once", used, api.count(), cost)
	}
	for _, c := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"resource": "tool:" + paidToolOK, "max_cost": 50}, "price_exceeds_max"},
		{map[string]any{"resource": "tool:" + paidToolUnvet, "max_cost": 30000}, "tool_unvetted"},
		{map[string]any{"resource": "tool:" + paidToolOK}, "max_cost"},
		{map[string]any{"resource": "price", "max_cost": 30000}, "tool:"},
	} {
		if _, failure := callTool(t, s, url, "", "x402_tools_call", c.args); !strings.Contains(failure, c.want) {
			t.Errorf("%v: %q, want %s", c.args, failure, c.want)
		}
	}
	if used := creditsUsed(t, s, agent); used != cost || api.count() != 1 {
		t.Fatalf("a refused call was charged (%v) or invoked (%d)", used-cost, api.count()-1)
	}
}

// memory_put then memory_get as the identity, its private item readable
// only to it; a recurring wake-up scheduled, listed and cancelled.
func TestHostedToolsMemoryAndWakeups(t *testing.T) {
	s, _ := hostedServicesServer(t)
	me := newIdentity(t, s, "")
	url, agent := "/mcp/t/"+me["token"].(string), me["agent"].(string)
	put := mustTool(t, s, url, "", "memory_put", map[string]any{"key": "notes/today", "value": "follow up on the export idea"})
	if dig(put, "data", "call", "state") != "done" {
		t.Fatalf("memory_put: %v", put)
	}
	got := mustTool(t, s, url, "", "memory_get", map[string]any{"key": "notes/today"})
	if dig(got, "data", "result", "value") != "follow up on the export idea" || dig(got, "data", "result", "visibility") != "private" {
		t.Fatalf("memory_get as the identity: %v", got)
	}
	if listed := mustTool(t, s, url, "", "memory_list", map[string]any{}); !strings.Contains(fmt.Sprint(listed), "notes/today") {
		t.Fatalf("memory_list as the identity: %v", listed)
	}
	if _, failure := callTool(t, s, "/mcp", "", "memory_get", map[string]any{"agent": agent, "key": "notes/today"}); !strings.Contains(failure, "No memory item") {
		t.Fatalf("an anonymous read of a private item: %q", failure)
	}
	mustTool(t, s, url, "", "memory_delete", map[string]any{"key": "notes/today"})
	if _, failure := callTool(t, s, url, "", "memory_get", map[string]any{"key": "notes/today"}); !strings.Contains(failure, "No memory item") {
		t.Fatalf("memory_get after memory_delete: %q", failure)
	}

	every := services.WakeupEveryMin
	scheduled := mustTool(t, s, url, "", "wakeup_schedule", map[string]any{"key": "tick", "every": every, "count": 3})
	if dig(scheduled, "data", "call", "state") != "done" {
		t.Fatalf("wakeup_schedule: %v", scheduled)
	}
	listed := mustTool(t, s, url, "", "wakeup_list", map[string]any{})
	raw, _ := json.Marshal(listed)
	if !strings.Contains(string(raw), `"tick"`) || !strings.Contains(string(raw), fmt.Sprintf(`"every":%d`, every)) {
		t.Fatalf("wakeup_list: %s", raw)
	}
	mustTool(t, s, url, "", "wakeup_cancel", map[string]any{"key": "tick"})
	listed = mustTool(t, s, url, "", "wakeup_list", map[string]any{})
	if raw, _ = json.Marshal(listed); !strings.Contains(string(raw), `"state":"cancelled"`) || strings.Contains(string(raw), `"state":"active"`) {
		t.Fatalf("wakeup_list after wakeup_cancel: %s", raw)
	}
}

// The tools are listed with their annotations and sign-in schemes only
// while hosted identities and their service are on; the assistant profile
// carries memory and wake-ups, never the paid tools.
func TestHostedServiceToolsListing(t *testing.T) {
	s, _ := hostedServicesServer(t)
	full, assistant := listTools(t, s, "/mcp"), listTools(t, s, web.AssistantMCPPath)
	for name, want := range map[string]map[string]bool{
		"x402_tools_call": {"readOnlyHint": false, "destructiveHint": false, "openWorldHint": true},
		"memory_put":      {"readOnlyHint": false, "destructiveHint": true, "openWorldHint": false},
		"memory_delete":   {"readOnlyHint": false, "destructiveHint": true, "openWorldHint": false},
		"wakeup_schedule": {"readOnlyHint": false, "destructiveHint": false, "openWorldHint": false},
		"wakeup_cancel":   {"readOnlyHint": false, "destructiveHint": true, "openWorldHint": false},
		"wakeup_list":     {"readOnlyHint": true, "destructiveHint": false, "openWorldHint": false},
	} {
		tool := full[name]
		if tool.Name == "" {
			t.Fatalf("%s is not listed", name)
		}
		for hint, value := range want {
			if tool.Annotations[hint] != value {
				t.Errorf("%s %s = %v, want %v", name, hint, tool.Annotations[hint], value)
			}
		}
		if strings.Contains(tool.Description, "Frames") || strings.Contains(string(tool.InputSchema), "Frames") {
			t.Errorf("%s names the upstream", name)
		}
		if schemes := securitySchemes(name); len(schemes) != 1 || schemes[0]["type"] != "oauth2" {
			t.Errorf("%s security schemes %v", name, schemes)
		}
		if name != "x402_tools_call" && assistant[name].Name == "" {
			t.Errorf("%s is missing on the assistant profile", name)
		}
	}
	var schema struct{ Required []string }
	_ = json.Unmarshal(full["x402_tools_call"].InputSchema, &schema)
	if strings.Join(schema.Required, ",") != "resource,max_cost" {
		t.Fatalf("x402_tools_call required %v", schema.Required)
	}
	if assistant["x402_tools_call"].Name != "" {
		t.Fatal("the assistant profile has no payment tools")
	}
	if schemes := securitySchemes("memory_get"); len(schemes) != 2 {
		t.Fatalf("memory_get works with or without sign-in: %v", schemes)
	}
	if text := s.assistantInstructions(); !strings.Contains(text, "memory_put") || !strings.Contains(text, "wakeup_schedule") {
		t.Fatal("the assistant instructions do not name the hosted memory and wake-up tools")
	}

	// Hosted identities on and the services off, or the services on and
	// hosted identities off: neither lists them.
	_, hostedOnly := hostedServer(t)
	servicesOnly := New(&fakeService{}, nil, Config{Features: board.Features{Services: []string{"memory", "wakeup", "x402"}}})
	for _, server := range []*Server{hostedOnly, servicesOnly} {
		for _, path := range []string{"/mcp", web.AssistantMCPPath} {
			tools := listTools(t, server, path)
			for _, name := range []string{"x402_tools_call", "memory_put", "memory_delete", "wakeup_schedule", "wakeup_cancel", "wakeup_list"} {
				if tools[name].Name != "" {
					t.Errorf("%s listed on %s without both hosted identities and its service", name, path)
				}
			}
		}
	}
}
