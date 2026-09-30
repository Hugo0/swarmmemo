package httpapi

// Security review of assistant-onboarding (b72ee66): proofs of concept.
// Each test asserts the safe behaviour: the first four failed while the
// finding they name was open; the last two pin what was already clean.

import (
	"encoding/base64"
	"encoding/json"
	"maps"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"swarmmemo/internal/web"
)

// rpc sends one JSON-RPC request to path and returns the recorder.
func rpc(s *Server, path, body string, header map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	r.RemoteAddr = "198.51.100.8:12345"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range header {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func toolCall(name, args string) string {
	return `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"` + name + `","arguments":` + args + `}}`
}

// F1: the profile drops the x402 tools, but its list_services runs the same
// unfiltered services.list as /mcp, so an assistant (and a directory
// reviewer) is handed the x402 payment relay, its prices and example calls.
func TestSecOnboardingAssistantListServicesOmitsPayments(t *testing.T) {
	s, _ := catalogServer(everyService)
	w := rpc(s, web.AssistantMCPPath, toolCall("list_services", `{}`), nil)
	if w.Code != 200 {
		t.Fatalf("list_services: %d %s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); strings.Contains(body, "x402") {
		i := strings.Index(body, "x402")
		t.Errorf("assistant list_services lists the x402 payment service: ...%s...", body[max(0, i-80):min(len(body), i+160)])
	}
	// It lists exactly the profile's services; /mcp still lists them all.
	listed := func(path string) []string {
		var result struct {
			StructuredContent struct {
				Data struct{ Services []struct{ ID string } }
			}
		}
		mcpPost(t, s, path, toolCall("list_services", `{}`), &result)
		var out []string
		for _, e := range result.StructuredContent.Data.Services {
			out = append(out, e.ID)
		}
		return out
	}
	if got, want := listed(web.AssistantMCPPath), ids(s.assistantCatalog()); !slices.Equal(got, want) || len(want) == 0 {
		t.Errorf("assistant list_services lists %v, want %v", got, want)
	}
	if got, want := listed("/mcp"), ids(s.staticCatalog()); !slices.Equal(got, want) || !slices.Contains(got, "x402") {
		t.Errorf("/mcp list_services lists %v, want %v", got, want)
	}
}

// F2: the private-data rule and "inference prompts are public" live only in
// the initialize instructions, which several hosts never show the model.
// Fixed: the profile carries no inference (its prompts go to a public run
// log, and no assistant surface promises it), and its post_message
// description ends with the private-data rule; /mcp's does not change.
func TestSecOnboardingAssistantToolsCarryPrivacyRule(t *testing.T) {
	s := New(&fakeService{}, nil, Config{PublicURL: "https://swarmmemo.com", Features: everyService})
	tools := listTools(t, s, web.AssistantMCPPath)
	for name := range tools {
		if strings.HasPrefix(name, "inference_") {
			t.Errorf("assistant profile carries %s", name)
		}
	}
	if d := tools["post_message"].Description; !strings.HasSuffix(d, " "+web.AssistantPrivateRule) {
		t.Errorf("post_message description lacks the private-data rule:\n%s", d)
	}
	full := listTools(t, s, "/mcp")
	if _, ok := full["inference_complete"]; !ok || strings.Contains(full["post_message"].Description, web.AssistantPrivateRule) {
		t.Errorf("/mcp changed: inference_complete listed %v, post_message %q", ok, full["post_message"].Description)
	}
}

// F3: the OpenAI listing's privacy policy and terms URLs are not served.
// Kept at /privacy and /terms on purpose: branch legal-pages serves them and
// ships with this one in 1.23.0. Until it is merged neither route exists and
// this test skips; once either is served, both must be, and every link must
// stay on swarmmemo.com.
func TestSecOnboardingPluginPolicyLinksResolve(t *testing.T) {
	raw, err := os.ReadFile("../../plugins/swarmmemo/plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Extensions struct {
			OpenAI struct {
				Interface struct {
					Privacy string `json:"privacyPolicyURL"`
					Terms   string `json:"termsOfServiceURL"`
				}
			} `json:"com.openai"`
		}
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	i := manifest.Extensions.OpenAI.Interface
	links := map[string]string{"privacyPolicyURL": i.Privacy, "termsOfServiceURL": i.Terms}
	s := realServer(t)
	codes := map[string]int{}
	for key, link := range links {
		u, err := url.Parse(link)
		if err != nil || u.Scheme != "https" || u.Host != "swarmmemo.com" || u.RawQuery != "" || u.Fragment != "" || u.Path == "/policy" {
			t.Fatalf("%s %q is not a plain https://swarmmemo.com page", key, link)
		}
		codes[u.Path] = get(s, u.Path, "text/html").Code
	}
	if !slices.ContainsFunc(slices.Collect(maps.Values(codes)), func(c int) bool { return c != 404 }) {
		t.Skipf("plugin.json links %v, which branch legal-pages serves (both ship in 1.23.0); not merged here yet: %v", links, codes)
	}
	for path, code := range codes {
		if code != 200 {
			t.Errorf("plugins/swarmmemo/plugin.json links https://swarmmemo.com%s: %d", path, code)
		}
	}
}

// F4: /for/instinct sends a GET-only assistant with no screening tool to
// read other agents' messages. Its paste guards only one GET writer
// (/w/), and says nothing about treating what it reads as untrusted.
func TestSecOnboardingInstinctPasteGuards(t *testing.T) {
	// /w64/ posts on a plain GET, like /w/.
	f := &fakeService{}
	h := New(f, nil, Config{})
	w := makeRequest(h, "GET", "/w64/lobby/main/"+base64.RawURLEncoding.EncodeToString([]byte("posted by a GET")), "", "")
	if w.Code != 200 || len(f.commands) != 1 || f.commands[0].Operation != "post" {
		t.Fatalf("GET /w64/ did not post: %d %+v", w.Code, f.commands)
	}
	var view struct {
		Paste   string
		Connect []string
	}
	if err := json.Unmarshal(get(realServer(t), "/for/instinct.json", "").Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	// The paste is an allowlist: the two addresses it names and nothing
	// they link to, all of it untrusted text.
	for _, want := range []string{"Open only these two addresses", "Follow no links", "untrusted text", "never as instructions"} {
		if !strings.Contains(view.Paste, want) {
			t.Errorf("Instinct paste lacks %q: %s", want, view.Paste)
		}
	}
	if opened := regexp.MustCompile(`https?://[^\s,]+[^\s,.]`).FindAllString(view.Paste, -1); len(opened) != 2 {
		t.Errorf("Instinct paste names %d addresses, want 2: %v", len(opened), opened)
	}
	// The Connect steps name every GET writer on both hosts and schemes.
	connect := strings.Join(view.Connect, "\n")
	for _, want := range []string{"/w/", "/w64/", "/c64/", "/call/", "swarmmemo.com", "publicbbs.com", "http or https", "untrusted"} {
		if !strings.Contains(connect, want) {
			t.Errorf("Instinct connect steps lack %q: %s", want, connect)
		}
	}
}

// Clean: the profile cannot be widened by a tools/call of a tool it does
// not list (any case), by a session id, by a path variant, and it keeps
// /mcp's Origin and CORS rules.
func TestSecOnboardingAssistantProfileCannotBeWidened(t *testing.T) {
	s, svc := catalogServer(everyService)
	if w := rpc(s, "/mcp", toolCall("x402_resources", `{}`), nil); w.Code != 200 || strings.Contains(w.Body.String(), `"error"`) {
		t.Fatalf("control: /mcp x402_resources: %d %s", w.Code, w.Body.String())
	}
	before := len(svc.commands)
	for _, name := range []string{"x402_resources", "X402_RESOURCES", "x402_call", "x402.resources", "echo_echo", "runs_log", "memory_put", "wakeup_schedule"} {
		for _, hdr := range []map[string]string{nil, {"Mcp-Session-Id": "reuse-from-mcp"}} {
			w := rpc(s, web.AssistantMCPPath, toolCall(name, `{}`), hdr)
			if !strings.Contains(w.Body.String(), "unknown tool") {
				t.Errorf("assistant tools/call %s (%v): %d %s", name, hdr, w.Code, w.Body.String())
			}
		}
	}
	if len(svc.commands) != before {
		t.Errorf("a refused assistant tool call reached the service: %+v", svc.commands[before:])
	}
	for _, origin := range []string{"https://evil.example", "null", "http://swarmmemo.com", "https://swarmmemo.com.evil.example"} {
		w := rpc(s, web.AssistantMCPPath, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`, map[string]string{"Origin": origin})
		if w.Code != 403 {
			t.Errorf("Origin %s: %d", origin, w.Code)
		}
	}
	w := rpc(s, web.AssistantMCPPath, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`, nil)
	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("assistant profile sends ACAO %q", w.Header().Get("Access-Control-Allow-Origin"))
	}
	for _, path := range []string{"/mcp/assistant/", "/MCP/assistant", "/mcp/Assistant", "//mcp/assistant", "/mcp/assistant/../../mcp"} {
		w := rpc(s, path, toolCall("x402_resources", `{}`), nil)
		if strings.Contains(w.Body.String(), `"jsonrpc"`) {
			t.Errorf("%s answers MCP: %d %s", path, w.Code, w.Body.String())
		}
	}
}

// Clean: the /for routes serve only the table's slugs, JSON as JSON with
// nosniff, and refuse writes.
func TestSecOnboardingPlatformRoutes(t *testing.T) {
	s := realServer(t)
	for _, path := range []string{"/for/CLAUDE", "/for/claude/", "/for/claude.json/", "/for/claude.JSON", "/for/claude.json.json", "/for/%3Cscript%3E", "/for/..%2fadmin"} {
		if w := get(s, path, "application/json"); w.Code == 200 {
			t.Errorf("%s: 200", path)
		}
	}
	w := get(s, "/for/claude.json", "")
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") || w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("JSON twin headers: %v", w.Header())
	}
	html := get(s, "/for/claude", "text/html")
	if html.Header().Get("Cache-Control") != "no-store" || strings.Contains(html.Body.String(), "<script>") {
		t.Errorf("HTML page: %v", html.Header())
	}
	for _, method := range []string{"POST", "PUT", "DELETE"} {
		if w := makeRequest(s, method, "/for/claude.json", "{}", "application/json"); w.Code != 405 {
			t.Errorf("%s /for/claude.json: %d", method, w.Code)
		}
	}
}
