package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"

	"swarmmemo/internal/board"
	"swarmmemo/internal/services"
	"swarmmemo/internal/web"
)

// catalogService answers services.list with the compiled-in catalogue at a
// changed memory.put price, so the tests can tell a surface that reads the
// live catalogue from one that restates the defaults.
type catalogService struct {
	mu       sync.Mutex
	features board.Features
	commands []board.Command
}

const livePutBase = 777

func (c *catalogService) Features() board.Features { return c.features }
func (c *catalogService) Moderate(context.Context, string, string, bool) error {
	return nil
}
func (c *catalogService) Close() error { return nil }
func (c *catalogService) Execute(_ context.Context, cmd board.Command, _ string) (board.Result, error) {
	c.mu.Lock()
	c.commands = append(c.commands, cmd)
	c.mu.Unlock()
	switch cmd.Operation {
	case "services.list":
		catalog := services.Catalog(c.features.Services)
		for i, e := range catalog {
			for j, m := range e.Methods {
				if e.ID == "memory" && m.Name == "put" {
					p := m.Price.(services.Price)
					p.Base = livePutBase
					catalog[i].Methods[j].Price = p
				}
			}
		}
		return board.Result{OK: true, Data: map[string]any{"services": catalog, "prices_version": 3}}, nil
	case "service.read":
		return board.Result{OK: true, Data: map[string]any{"service": cmd.Target}}, nil
	}
	return board.Result{OK: true, Messages: []board.Message{}, Rooms: []board.Room{}}, nil
}

func catalogServer(f board.Features) (*Server, *catalogService) {
	svc := &catalogService{features: f}
	return New(svc, web.Handler(svc), Config{Features: f}), svc
}

func ids(catalog []services.Entry) []string {
	out := []string{}
	for _, e := range catalog {
		out = append(out, e.ID)
	}
	return out
}

// Every surface lists exactly the catalogue's services, in its order, with
// the live prices; the web page shows what the API publishes.
func TestServiceSurfacesListTheCatalogue(t *testing.T) {
	f := board.Features{Services: services.Known(), Trust: board.TrustShadow}
	s, svc := catalogServer(f)
	catalog := services.Catalog(f.Services)
	want := ids(catalog)

	// /api/services
	api := decodeResult(t, makeRequest(s, "GET", "/api/services", "", "").Body.Bytes())
	var listed []string
	for _, e := range dig(api, "data", "services").([]any) {
		listed = append(listed, e.(map[string]any)["id"].(string))
	}
	if !slices.Equal(listed, want) {
		t.Fatalf("/api/services lists %v, want %v", listed, want)
	}

	// /capabilities: the entries, their examples, the live price, the gives.
	caps := decodeResult(t, makeRequest(s, "GET", "/capabilities", "", "").Body.Bytes())
	entries := dig(caps, "services", "entries").([]any)
	examples := map[string]map[string]any{}
	listed = nil
	for _, raw := range entries {
		e := raw.(map[string]any)
		listed = append(listed, e["id"].(string))
		examples[e["id"].(string)] = e["examples"].(map[string]any)
	}
	if !slices.Equal(listed, want) {
		t.Fatalf("/capabilities services.entries lists %v, want %v", listed, want)
	}
	if raw, _ := json.Marshal(entries[slices.Index(listed, "memory")]); !strings.Contains(string(raw), `"base":777`) {
		t.Fatalf("/capabilities does not show the live price: %s", raw)
	}
	var capGives []string
	for _, g := range caps["gives"].([]any) {
		capGives = append(capGives, g.(map[string]any)["topic"].(string))
	}

	// /llms-full.txt: a section per service in catalogue order and the live
	// price; /llms.txt: the example call at the live quote. Both: the same
	// gives, one line per tool.
	live, err := svc.Execute(t.Context(), board.Command{Operation: "services.list"}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/llms.txt", "/llms-full.txt"} {
		text := makeRequest(s, "GET", path, "", "").Body.String()
		if path == "/llms.txt" {
			if !strings.Contains(text, web.ToolsText(s.cfg.PublicURL, live.Data["services"].([]services.Entry), services.NoKey{})) {
				t.Errorf("%s does not show the tools section", path)
			}
		} else {
			last := -1
			for _, e := range catalog {
				at := strings.Index(text, "### "+e.Title+" ("+e.ID+")")
				if at < 0 || at < last {
					t.Errorf("%s: %s is missing or out of order", path, e.ID)
				}
				last = at
			}
			if !strings.Contains(text, "put (service.call, signed, 777 + 1 per byte memory_bytes)") {
				t.Errorf("%s does not show the live price", path)
			}
		}
		// /llms.txt names no service but tools: the featured tools and the
		// search stand for the rest.
		serviceTopics := map[string]bool{}
		for _, e := range catalog {
			if e.ID != services.ToolsID && path == "/llms.txt" {
				serviceTopics[e.Topic] = true
			}
		}
		for _, topic := range capGives {
			if !serviceTopics[topic] && !strings.Contains(text, "\n- "+topic+": ") {
				t.Errorf("%s lacks the gives line %q", path, topic)
			}
		}
	}

	// /for-agents: the same gives, a card per service with the published
	// examples and the live price.
	page := getHTML(t, s, "/for-agents")
	var pageGives []string
	for _, m := range regexp.MustCompile(`<li><strong>([^<]+):</strong>`).FindAllStringSubmatch(page[strings.Index(page, `id="gives"`):], -1) {
		pageGives = append(pageGives, html.UnescapeString(m[1]))
	}
	if !slices.Equal(pageGives, capGives) {
		t.Fatalf("/for-agents gives %v, /capabilities gives %v", pageGives, capGives)
	}
	listed = nil
	for _, m := range regexp.MustCompile(`id="service-([a-z0-9_]+)"`).FindAllStringSubmatch(page, -1) {
		listed = append(listed, m[1])
	}
	if !slices.Equal(listed, want) {
		t.Fatalf("/for-agents lists %v, want %v", listed, want)
	}
	unescaped := html.UnescapeString(page)
	for id, ex := range examples {
		for _, wire := range []string{"post", "get", "mcp"} {
			if !strings.Contains(unescaped, ex[wire].(string)) {
				t.Errorf("/for-agents lacks %s's %s example as /capabilities publishes it", id, wire)
			}
		}
	}
	if !strings.Contains(unescaped, "777 + 1 per byte memory_bytes") {
		t.Error("/for-agents does not show the live price")
	}

	// MCP: the server card and tools/list name list_services and one tool
	// per public read and per method callable without a key, exactly, the
	// tools search and call first.
	wantTools := []string{"list_services"}
	ordered := slices.Clone(catalog)
	toolsAt := slices.IndexFunc(ordered, func(e services.Entry) bool { return e.ID == services.ToolsID })
	tools := ordered[toolsAt]
	ordered = append([]services.Entry{tools}, slices.Delete(ordered, toolsAt, toolsAt+1)...)
	for _, e := range ordered {
		for _, m := range e.Methods {
			if !m.Write() && !m.Signed || m.Write() && m.Anonymous {
				wantTools = append(wantTools, web.MCPToolName(e, m))
			}
		}
	}
	var card []string
	for _, tool := range s.serverCard()["tools"].([]map[string]any) {
		card = append(card, tool["name"].(string))
	}
	fixed := map[string]bool{}
	for _, tool := range append(append([]mcpToolSpec(nil), mcpTools...), mcpRFC0012Tools...) {
		fixed[tool.Name] = true
	}
	var generated []string
	for _, name := range card {
		if !fixed[name] {
			generated = append(generated, name)
		}
	}
	if !slices.Equal(generated, wantTools) {
		t.Fatalf("server card service tools %v, catalogue %v", generated, wantTools)
	}
	server := httptest.NewServer(s)
	defer server.Close()
	listTools := mcpCall(t, server.URL, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	var registered []string
	for _, tool := range dig(listTools, "result", "tools").([]any) {
		registered = append(registered, tool.(map[string]any)["name"].(string))
	}
	slices.Sort(registered)
	slices.Sort(card)
	if !slices.Equal(registered, card) {
		t.Fatalf("tools/list %v, server card %v", registered, card)
	}

	// openapi.json: one ServiceData variant per method, and /api/services.
	var openapi map[string]any
	_ = json.Unmarshal(makeRequest(s, "GET", "/openapi.json", "", "").Body.Bytes(), &openapi)
	var variants []string
	for _, v := range dig(openapi, "components", "schemas", "ServiceData", "anyOf").([]any) {
		variants = append(variants, v.(map[string]any)["title"].(string))
	}
	var methods []string
	for _, e := range catalog {
		for _, m := range e.Methods {
			methods = append(methods, e.ID+"."+m.Name)
		}
	}
	if !slices.Equal(variants, methods) {
		t.Fatalf("openapi ServiceData %v, catalogue %v", variants, methods)
	}
	if dig(openapi, "paths", "/api/services") == nil {
		t.Fatal("openapi lacks /api/services")
	}
}

func mcpCall(t *testing.T, url, body string) map[string]any {
	t.Helper()
	request, _ := http.NewRequest(http.MethodPost, url+"/mcp", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(response.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// Every service's examples parse with each wire's real parser: the POST body
// and the /c64/ path with the command decoder, their data with the service
// data parser, and the MCP call through the hosted tool's input schema.
func TestServiceExamplesParseOnEveryWire(t *testing.T) {
	f := board.Features{Services: services.Known()}
	s, svc := catalogServer(f)
	server := httptest.NewServer(s)
	defer server.Close()
	for _, e := range services.Catalog(f.Services) {
		ex := web.ServiceExamples("https://swarmmemo.com", e)
		body := services.FillPlaceholders(ex.Body)
		cmd, err := DecodeCommand([]byte(body))
		if err != nil || cmd.Target != e.ID || !strings.HasPrefix(cmd.Operation, "service.") {
			t.Errorf("%s: POST body %s: %v", e.ID, body, err)
			continue
		}
		if !strings.Contains(ex.POST, "'"+ex.Body+"'") || !strings.Contains(ex.POST, "/v1/command") {
			t.Errorf("%s: POST example does not send its body: %s", e.ID, ex.POST)
		}
		if _, err := services.ParseData(cmd.Data, cmd.Operation == "service.call"); err != nil {
			t.Errorf("%s: POST data: %v", e.ID, err)
		}
		if cmd.Operation == "service.read" {
			if w := makeRequest(s, "POST", "/v1/command", body, "application/json"); w.Code != 200 {
				t.Errorf("%s: the unsigned POST example is refused: %d %s", e.ID, w.Code, w.Body.String())
			}
		}
		if ex.Path == "" {
			if !strings.Contains(ex.GET, "/c64/$(") {
				t.Errorf("%s: GET example: %s", e.ID, ex.GET)
			}
		} else {
			raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(ex.Path, "/c64/"))
			read, derr := DecodeCommand([]byte(services.FillPlaceholders(string(raw))))
			if err != nil || derr != nil || read.Operation != "service.read" || read.Target != e.ID || !strings.Contains(ex.GET, ex.Path) {
				t.Errorf("%s: GET example %s: %v %v", e.ID, ex.GET, err, derr)
			}
			if _, err := services.ParseData(read.Data, false); err != nil {
				t.Errorf("%s: GET data: %v", e.ID, err)
			}
			if w := makeRequest(s, "GET", ex.Path, "", ""); w.Code != 200 {
				t.Errorf("%s: GET %s: %d %s", e.ID, ex.Path, w.Code, w.Body.String())
			}
			// The MCP example calls the hosted tool with the same arguments.
			call := strings.TrimSuffix(strings.TrimPrefix(ex.MCP, "tools/call "), " on https://swarmmemo.com/mcp")
			var params map[string]any
			if err := json.Unmarshal([]byte(services.FillPlaceholders(call)), &params); err != nil {
				t.Errorf("%s: MCP example %s: %v", e.ID, ex.MCP, err)
				continue
			}
			raw, _ = json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": params})
			out := mcpCall(t, server.URL, string(raw))
			if dig(out, "error") != nil || dig(out, "result", "isError") == true {
				t.Errorf("%s: MCP example refused: %+v", e.ID, out)
			}
		}
		if !strings.Contains(ex.MCP, "/mcp") && !strings.Contains(ex.MCP, "clients/mcp/README.md#services") {
			t.Errorf("%s: MCP example: %s", e.ID, ex.MCP)
		}
	}
	// The hosted tools sent exactly the reads the examples describe.
	for _, c := range svc.commands {
		if c.Operation == "service.call" {
			t.Fatalf("an example made a service call without a signature: %+v", c)
		}
	}
}

// With no service enabled no surface mentions them, and the gives name only
// what runs.
func TestServiceSurfacesAbsentWhileOff(t *testing.T) {
	s, svc := catalogServer(board.Features{})
	caps := decodeResult(t, makeRequest(s, "GET", "/capabilities", "", "").Body.Bytes())
	if _, ok := caps["services"]; ok {
		t.Fatal("/capabilities lists services while none is enabled")
	}
	var topics []string
	for _, g := range caps["gives"].([]any) {
		topics = append(topics, g.(map[string]any)["topic"].(string))
	}
	if !slices.Equal(topics, []string{"Voice everywhere", "Private conversations", "Find agents", "Paid tasks", "A record you can prove"}) {
		t.Fatalf("gives with every flag off: %v", topics)
	}
	for _, path := range []string{"/llms.txt", "/llms-full.txt"} {
		text := makeRequest(s, "GET", path, "", "").Body.String()
		if i := strings.Index(text, "# Full command reference"); i > 0 {
			text = text[:i]
		}
		if strings.Contains(text, "## Services") || strings.Contains(text, "/api/services") {
			t.Errorf("%s mentions services while none is enabled", path)
		}
	}
	if page := getHTML(t, s, "/for-agents"); strings.Contains(page, `id="services"`) || strings.Contains(page, "/api/services") {
		t.Error("/for-agents shows services while none is enabled")
	}
	for _, tool := range s.mcpToolList() {
		if tool.Name == "list_services" {
			t.Fatal("list_services while no service is enabled")
		}
	}
	for _, c := range svc.commands {
		if c.Operation == "services.list" {
			t.Fatal("a surface read services.list while no service is enabled")
		}
	}
}

func TestServiceKeyExamplesValidateAgainstOpenAPI(t *testing.T) {
	s, _ := catalogServer(board.Features{Services: services.Known()})
	var spec struct {
		Components struct{ Schemas map[string]json.RawMessage }
	}
	if err := json.Unmarshal(makeRequest(s, "GET", "/openapi.json", "", "").Body.Bytes(), &spec); err != nil {
		t.Fatal(err)
	}
	validators := map[string]*jsonschema.Resolved{}
	for _, name := range []string{"Command", "ServiceData"} {
		var schema jsonschema.Schema
		if err := json.Unmarshal(spec.Components.Schemas[name], &schema); err != nil {
			t.Fatal(err)
		}
		resolved, err := schema.Resolve(nil)
		if err != nil {
			t.Fatal(err)
		}
		validators[name] = resolved
	}
	for _, target := range []string{"screen", "notary"} {
		t.Run(target+".key", func(t *testing.T) {
			const data = `{"schema":1,"method":"key","args":{}}`
			request := map[string]any{"operation": "service.read", "target": target, "data": data}
			if err := validators["Command"].Validate(request); err != nil {
				t.Fatalf("command: %v", err)
			}
			var parsed any
			if err := json.Unmarshal([]byte(data), &parsed); err != nil {
				t.Fatal(err)
			}
			if err := validators["ServiceData"].Validate(parsed); err != nil {
				t.Fatalf("service data: %v", err)
			}
		})
	}
	if err := validators["ServiceData"].Validate(map[string]any{"schema": 1, "method": "nonexistent", "args": map[string]any{}}); err == nil {
		t.Fatal("unknown service method validated")
	}
}
