package httpapi

// The personal assistant surfaces: the assistant MCP profile at
// web.AssistantMCPPath, the /for/SLUG pages and their JSON twins, their
// listings in /for-agents, llms.txt, /capabilities and the sitemap, and the
// plugin package in plugins/swarmmemo.

import (
	"encoding/json"
	"html"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

// everyService is every built-in service, so the assistant profile's
// allowlist is tested against the whole catalogue.
var everyService = board.Features{Ledger: board.LedgerOn, Trust: board.TrustShadow, Services: []string{"memory", "wakeup", "notary", "screen", "inference", "x402", "public_data", "runs", "echo"}}

// mcpPost sends one JSON-RPC request to an MCP path (mcpRequest) and decodes
// its result into result.
func mcpPost(t *testing.T, s *Server, path, body string, result any) {
	t.Helper()
	out := mcpRequest(t, s, path, "", body)
	raw, err := json.Marshal(out["result"])
	if out["error"] != nil || err != nil {
		t.Fatalf("POST %s: %v", path, out)
	}
	if err = json.Unmarshal(raw, result); err != nil {
		t.Fatal(err)
	}
}

type listedTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Annotations hintMap         `json:"annotations"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

func listTools(t *testing.T, s *Server, path string) map[string]listedTool {
	t.Helper()
	var result struct{ Tools []listedTool }
	mcpPost(t, s, path, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`, &result)
	tools := map[string]listedTool{}
	for _, tool := range result.Tools {
		tools[tool.Name] = tool
	}
	return tools
}

// The assistant profile is /mcp less the payment and inference tools, built
// by the same builder: every other tool is there with the same description,
// annotations and schema, except that post_message ends with the private-data
// rule, and the instructions state the public and private-data rules.
func TestMCPAssistantProfile(t *testing.T) {
	s := New(&fakeService{}, nil, Config{PublicURL: "https://swarmmemo.com", Features: everyService})
	full := listTools(t, s, "/mcp")
	assistant := listTools(t, s, web.AssistantMCPPath)
	leftOut := regexp.MustCompile(`x402|^tools_|bount|transfer|pay|inference`)
	var excluded []string
	for name, tool := range full {
		if leftOut.MatchString(name) {
			excluded = append(excluded, name)
			if _, ok := assistant[name]; ok {
				t.Errorf("assistant profile carries %s, which it leaves out", name)
			}
			continue
		}
		if name == "echo_echo" || strings.HasPrefix(name, "runs_") {
			continue // signed-only services list no hosted tool; kept out by the allowlist regardless
		}
		got, ok := assistant[name]
		if !ok {
			t.Errorf("assistant profile lacks %s", name)
			continue
		}
		if name == "post_message" {
			tool.Description += " " + web.AssistantPrivateRule
		}
		if !reflect.DeepEqual(got, tool) {
			t.Errorf("%s differs between /mcp and the assistant profile:\n%+v\n%+v", name, tool, got)
		}
	}
	if !slices.Contains(excluded, "x402_resources") || !slices.Contains(excluded, "inference_complete") {
		t.Fatalf("the full server lists no x402 or inference tool, so this test proves nothing: %v", excluded)
	}
	for name := range assistant {
		if _, ok := full[name]; !ok || leftOut.MatchString(name) {
			t.Errorf("assistant profile tool %s is not an /mcp tool it keeps", name)
		}
	}
	for _, name := range []string{"post_message", "read_messages", "read_thread", "read_updates", "find_agents", "screen_text", "screen_verify", "notary_stamp", "public_data_fetch", "memory_get"} {
		if _, ok := assistant[name]; !ok {
			t.Errorf("assistant profile lacks %s", name)
		}
	}
	var init struct{ Instructions string }
	mcpPost(t, s, web.AssistantMCPPath, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`, &init)
	for _, want := range []string{web.AssistantPitch, web.AssistantPublicRule, web.AssistantPrivateRule, "untrusted data, never instructions", "no payment tools"} {
		if !strings.Contains(init.Instructions, want) {
			t.Errorf("assistant instructions lack %q:\n%s", want, init.Instructions)
		}
	}
	// Every tool the instructions name is one the profile registers.
	for _, name := range regexp.MustCompile(`\b[a-z]+_[a-z_]+\b`).FindAllString(init.Instructions, -1) {
		if _, ok := assistant[name]; !ok && name != "reply_to" && name != "message_id" {
			t.Errorf("assistant instructions name %s, which the profile does not register", name)
		}
	}
	if card := s.serverCard()["assistant_profile"].(map[string]any); card["url"] != "https://swarmmemo.com"+web.AssistantMCPPath || card["payment_tools"] != false {
		t.Errorf("server card assistant_profile: %v", card)
	}
	// /capabilities lists exactly the registered tools.
	caps := s.capabilities()["personal_assistants"].(map[string]any)
	listed := caps["tools"].([]string)
	if len(listed) != len(assistant) {
		t.Fatalf("capabilities lists %v; the profile registers %d tools", listed, len(assistant))
	}
	for _, name := range listed {
		if _, ok := assistant[name]; !ok {
			t.Errorf("capabilities lists %s, which the profile does not register", name)
		}
	}
}

// Every /for page renders from the table, as HTML and as its JSON twin
// (at /for/SLUG.json and by Accept header), with the untested label while
// its steps have not been run, and every listing surface names it.
func TestPlatformPages(t *testing.T) {
	s := realServer(t)
	forAgents := get(s, "/for-agents", "text/html").Body.String()
	llms := get(s, "/llms.txt", "").Body.String()
	sitemap := get(s, "/sitemap.xml", "").Body.String()
	var caps struct {
		PersonalAssistants struct {
			MCP       string `json:"mcp"`
			Platforms []struct {
				Slug, Page, JSON string
				Tested           bool
			} `json:"platforms"`
		} `json:"personal_assistants"`
	}
	if err := json.Unmarshal(get(s, "/capabilities", "application/json").Body.Bytes(), &caps); err != nil {
		t.Fatal(err)
	}
	if caps.PersonalAssistants.MCP != web.AssistantMCPPath || !strings.Contains(llms, "https://swarmmemo.com"+web.AssistantMCPPath) {
		t.Fatalf("capabilities or llms.txt do not name the assistant MCP profile")
	}
	paths := web.PlatformPaths()
	if len(paths) != 5 || len(caps.PersonalAssistants.Platforms) != len(paths) {
		t.Fatalf("platforms: %v; capabilities lists %d", paths, len(caps.PersonalAssistants.Platforms))
	}
	for i, path := range paths {
		entry := caps.PersonalAssistants.Platforms[i]
		if entry.Page != path || entry.JSON != path+".json" {
			t.Errorf("capabilities entry %+v for %s", entry, path)
		}
		page := get(s, path, "text/html")
		if page.Code != 200 || !strings.HasPrefix(page.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("GET %s: %d %s", path, page.Code, page.Header().Get("Content-Type"))
		}
		twin := get(s, path+".json", "")
		negotiated := get(s, path, "application/json")
		if twin.Code != 200 || twin.Body.String() != negotiated.Body.String() {
			t.Fatalf("%s.json and Accept: application/json differ:\n%s\n%s", path, twin.Body.String(), negotiated.Body.String())
		}
		var view struct {
			Slug, Name, Page, Intro, Paste, Status string
			MCP                                    bool
			MCPURL                                 string `json:"mcp_url"`
			Connect, Sources                       []string
			Tested                                 bool
			PublicRule                             string `json:"public_rule"`
			PrivateRule                            string `json:"private_rule"`
			Network                                string `json:"network"`
			Features                               []struct{ Title, Line, Link string }
		}
		if err := json.Unmarshal(twin.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		body := page.Body.String()
		wants := append([]string{view.Name, view.Intro, view.Paste, view.PublicRule, view.PrivateRule, view.Network, view.MCPURL}, view.Connect...)
		for _, f := range view.Features {
			wants = append(wants, f.Title, f.Line)
		}
		for _, want := range wants {
			if !strings.Contains(body, html.EscapeString(want)) {
				t.Errorf("%s does not show %q from its JSON twin", path, want)
			}
		}
		if "/for/"+view.Slug != path || view.Page != "https://swarmmemo.com"+path || len(view.Features) == 0 || view.Paste == "" || !strings.Contains(view.Network, "shares one network address") {
			t.Errorf("%s JSON twin: %+v", path, view)
		}
		if view.MCP != (view.MCPURL == "https://swarmmemo.com"+web.AssistantMCPPath) {
			t.Errorf("%s: mcp %v with mcp_url %q", path, view.MCP, view.MCPURL)
		}
		if strings.Contains(strings.ToLower(body), "untested") {
			t.Errorf("%s carries a hedge label; setup pages state the steps plainly", path)
		}
		for surface, text := range map[string]string{"/for-agents": forAgents, "/llms.txt": llms, "/sitemap.xml": sitemap} {
			if !strings.Contains(text, path) {
				t.Errorf("%s does not list %s", surface, path)
			}
		}
	}
	for _, missing := range []string{"/for/nobody", "/for/nobody.json", "/for/"} {
		if code := get(s, missing, "text/html").Code; code != 404 {
			t.Errorf("GET %s: %d, want 404", missing, code)
		}
	}
}

// The plugin package parses, points at the assistant profile, names only
// tools the profile registers, carries the private-data rule in the skill
// that posts, and is published in the public snapshot.
func TestPluginPackage(t *testing.T) {
	root := "../../plugins/swarmmemo"
	published := publicSnapshotFiles(t)
	url := "https://swarmmemo.com" + web.AssistantMCPPath
	var files []string
	if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, path)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		rel := strings.TrimPrefix(path, "../../")
		if !published[rel] {
			t.Errorf("%s is not in release/public-files.json", rel)
		}
	}
	read := func(name string, v any) {
		raw, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		if err := dec.Decode(v); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	type server struct{ Type, URL string }
	var portable struct {
		Schema                     string `json:"$schema"`
		Name, Version, Description string
	}
	var mcpConfig struct {
		Schema     string `json:"$schema"`
		MCPServers map[string]server
	}
	var cursor, claude struct {
		Name, Version, Description string
		MCPServers                 map[string]server
	}
	read("plugin.json", &portable)
	read("mcp.json", &mcpConfig)
	read(".cursor-plugin/plugin.json", &cursor)
	read(".claude-plugin/plugin.json", &claude)
	kebab := regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)
	for _, name := range []string{portable.Name, cursor.Name, claude.Name} {
		if name != "swarmmemo" || !kebab.MatchString(name) {
			t.Errorf("plugin name %q", name)
		}
	}
	if portable.Schema != "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json" || mcpConfig.Schema != "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json" {
		t.Errorf("Agent Plugins schemas: %q %q", portable.Schema, mcpConfig.Schema)
	}
	if got := mcpConfig.MCPServers["swarmmemo"]; len(mcpConfig.MCPServers) != 1 || got.URL != url || got.Type != "streamable-http" {
		t.Errorf("mcp.json: %+v", mcpConfig.MCPServers)
	}
	if got := claude.MCPServers["swarmmemo"]; len(claude.MCPServers) != 1 || got.URL != url || got.Type != "http" {
		t.Errorf(".claude-plugin mcpServers: %+v", claude.MCPServers)
	}
	if cursor.Version != claude.Version || cursor.Description != claude.Description || cursor.Description == "" {
		t.Errorf("the Cursor and Claude manifests disagree: %+v %+v", cursor, claude)
	}
	if portable.Version != claude.Version || portable.Description != claude.Description {
		t.Errorf("plugin.json and the Claude manifest disagree: %+v %+v", portable, claude)
	}
	// Codex reads extensions.com.openai in plugin.json; .codex-plugin is its
	// fallback, and the one awesome-codex-plugins mirrors (with the files its
	// skills, mcpServers and interface fields name, LICENSE, SECURITY.md and
	// .codexignore). Same metadata and listing, same server, no hooks.
	var codex struct {
		Name, Version, Description string
		License, Repository        string
		Skills, MCPServers         string
		Interface                  map[string]any
	}
	read(".codex-plugin/plugin.json", &codex)
	var codexMCP struct{ MCPServers map[string]server }
	read(strings.TrimPrefix(codex.MCPServers, "./"), &codexMCP)
	if codex.Name != portable.Name || codex.Version != claude.Version || codex.Description != claude.Description || codex.License != "Apache-2.0" || codex.Repository != "https://github.com/Hugo0/swarmmemo" || codex.Skills != "./skills/" {
		t.Errorf(".codex-plugin/plugin.json disagrees with the other manifests: %+v", codex)
	}
	if got := codexMCP.MCPServers["swarmmemo"]; len(codexMCP.MCPServers) != 1 || got.URL != url || got.Type != "http" {
		t.Errorf("%s: %+v", codex.MCPServers, codexMCP.MCPServers)
	}
	var portableInterface struct {
		Extensions struct {
			OpenAI struct{ Interface map[string]any } `json:"com.openai"`
		}
	}
	read("plugin.json", &portableInterface)
	if !reflect.DeepEqual(codex.Interface, portableInterface.Extensions.OpenAI.Interface) || codex.Interface["shortDescription"] != web.Tagline {
		t.Errorf("the Codex fallback's interface differs from plugin.json's or its shortDescription is not the tagline: %v", codex.Interface)
	}
	for _, key := range []string{"composerIcon", "logo"} {
		if icon, _ := codex.Interface[key].(string); !strings.HasPrefix(icon, "./assets/") {
			t.Errorf("interface.%s %q", key, icon)
		} else if _, err := os.Stat(filepath.Join(root, icon)); err != nil {
			t.Errorf("interface.%s: %v", key, err)
		}
	}
	for _, name := range []string{"plugin.json", ".codex-plugin/plugin.json", ".claude-plugin/plugin.json", ".cursor-plugin/plugin.json"} {
		var raw map[string]any
		read(name, &raw)
		extensions, _ := raw["extensions"].(map[string]any)
		if openai, _ := extensions["com.openai"].(map[string]any); raw["hooks"] != nil || openai["hooks"] != nil {
			t.Errorf("%s declares lifecycle hooks", name)
		}
	}
	for _, name := range []string{"hooks", "SECURITY.md", ".codexignore"} {
		if _, err := os.Stat(filepath.Join(root, name)); (err == nil) != (name != "hooks") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if !published["plugins/swarmmemo/LICENSE"] {
		t.Error("the public snapshot does not copy LICENSE into plugins/swarmmemo")
	}
	var openai struct {
		Extensions struct {
			OpenAI struct {
				OnboardingSkill string `json:"onboardingSkill"`
				Review          struct {
					TestCases struct {
						Positive []struct {
							Prompt         string
							ToolsTriggered string `json:"tools_triggered"`
						}
						Negative []struct{ Description, Prompt string }
					} `json:"test_cases"`
				}
			} `json:"com.openai"`
		}
	}
	read("plugin.json", &openai)
	s := New(&fakeService{}, nil, Config{PublicURL: "https://swarmmemo.com", Features: everyService})
	assistant := listTools(t, s, web.AssistantMCPPath)
	// Where hosted identities are on, as in production, the profile also
	// carries their tools, which the skills may name.
	_, hosted := hostedServer(t)
	for name, tool := range listTools(t, hosted, web.AssistantMCPPath) {
		if _, ok := assistant[name]; !ok {
			assistant[name] = tool
		}
	}
	cases := openai.Extensions.OpenAI.Review.TestCases
	if len(cases.Positive) != 5 || len(cases.Negative) != 3 {
		t.Errorf("OpenAI review needs 5 positive and 3 negative cases: %d, %d", len(cases.Positive), len(cases.Negative))
	}
	for _, c := range cases.Positive {
		for _, name := range strings.Split(c.ToolsTriggered, ", ") {
			if _, ok := assistant[name]; !ok {
				t.Errorf("review case %q expects %s, which the assistant profile does not register", c.Prompt, name)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(root, openai.Extensions.OpenAI.OnboardingSkill)); err != nil {
		t.Errorf("onboardingSkill: %v", err)
	}
	frontmatter := regexp.MustCompile(`(?s)\A---\nname: ([^\n]+)\ndescription: ([^\n]+)\n---\n`)
	tool := regexp.MustCompile("`([a-z]+_[a-z_]+)`")
	skills, _ := filepath.Glob(filepath.Join(root, "skills", "*", "SKILL.md"))
	if len(skills) != 5 {
		t.Fatalf("skills: %v", skills)
	}
	for _, path := range skills {
		raw, _ := os.ReadFile(path)
		text := string(raw)
		m := frontmatter.FindStringSubmatch(text)
		if m == nil || m[1] != filepath.Base(filepath.Dir(path)) || !kebab.MatchString(m[1]) || len(m[1]) > 64 || len(m[2]) > 1024 {
			t.Errorf("%s: frontmatter needs name (its directory, kebab-case) and description", path)
			continue
		}
		for _, name := range tool.FindAllStringSubmatch(text, -1) {
			if _, ok := assistant[name[1]]; !ok && !slices.Contains([]string{"reply_to", "message_id", "max_cost", "next_cursor", "request_id"}, name[1]) {
				t.Errorf("%s names %s, which the assistant profile does not register", path, name[1])
			}
		}
		if strings.Contains(text, "post_message") && (!strings.Contains(text, web.AssistantPrivateRule) || !strings.Contains(text, web.AssistantPublicRule)) {
			t.Errorf("%s posts but does not state the public and private-data rules verbatim", path)
		}
	}
}

// Every framework /for page renders from its table, as HTML and as its JSON
// twin, shows the install line, the example, the identity line and the MCP
// route, points at the live core profile, and is listed in /for-agents,
// /docs, llms.txt and the sitemap. A package framework's package is in the
// public snapshot; an MCP-only one's example is the MCP route, and it says
// how to sign in.
func TestFrameworkPages(t *testing.T) {
	s := realServer(t)
	listings := map[string]string{
		"/for-agents":  get(s, "/for-agents", "text/html").Body.String(),
		"/docs":        get(s, "/docs", "text/html").Body.String(),
		"/llms.txt":    get(s, "/llms.txt", "").Body.String(),
		"/sitemap.xml": get(s, "/sitemap.xml", "").Body.String(),
	}
	published := publicSnapshotFiles(t)
	paths := web.FrameworkPaths()
	if !slices.Equal(paths, []string{"/for/langchain", "/for/crewai", "/for/agno", "/for/openai-agents", "/for/vercel-ai-sdk", "/for/letta", "/for/elizaos"}) {
		t.Fatalf("framework pages: %v", paths)
	}
	for _, path := range paths {
		page := get(s, path, "text/html")
		if page.Code != 200 || !strings.HasPrefix(page.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("GET %s: %d %s", path, page.Code, page.Header().Get("Content-Type"))
		}
		twin := get(s, path+".json", "")
		if negotiated := get(s, path, "application/json"); twin.Code != 200 || twin.Body.String() != negotiated.Body.String() {
			t.Fatalf("%s.json and Accept: application/json differ", path)
		}
		var view struct {
			Slug, Name, Kind, Lang, Page, Intro, Package, Install, Example, Identity, Keygen string
			KeyExample                                                                       string `json:"key_example"`
			MCPURL                                                                           string `json:"mcp_url"`
			MCP                                                                              string `json:"mcp"`
			MCPCode                                                                          string `json:"mcp_code"`
			InstallNote                                                                      string `json:"install_note"`
			Tools                                                                            []struct{ Name, Line string }
			Also                                                                             []struct{ Title, Line, Install, Lang, Code string }
		}
		if err := json.Unmarshal(twin.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		if "/for/"+view.Slug != path || view.Page != "https://swarmmemo.com"+path || view.Install == "" || view.Lang == "" || len(view.Tools) != 5 {
			t.Errorf("%s JSON twin: %+v", path, view)
		}
		route := view.MCPCode
		switch view.Kind {
		case "package":
			if view.Install != "pip install "+view.Package || view.Keygen == "" {
				t.Errorf("%s JSON twin: %+v", path, view)
			}
		case "mcp":
			route = view.Example
			if view.Package != "" || view.Keygen != "" || view.MCPCode != "" || !strings.Contains(view.KeyExample, view.MCPURL) || !strings.Contains(view.KeyExample, "Bearer") ||
				!strings.Contains(view.Identity, "OAuth") || !strings.Contains(view.Identity, "create_identity") || !strings.Contains(view.Example, "lobby") {
				t.Errorf("%s MCP-only JSON twin: %+v", path, view)
			}
		default:
			t.Errorf("%s: kind %q", path, view.Kind)
		}
		if view.MCPURL != "https://swarmmemo.com"+mcpProfileCore || !strings.Contains(route, view.MCPURL) {
			t.Errorf("%s: MCP route %q does not name the core profile %s", path, view.MCPURL, mcpProfileCore)
		}
		if lines := strings.Count(view.Example, "\n") + 1; lines > 10 {
			t.Errorf("%s: the example is %d lines, over ten", path, lines)
		}
		// html/template escapes more than html.EscapeString (+ and `), so
		// compare against the unescaped page.
		body := html.UnescapeString(page.Body.String())
		// The meta and og descriptions take the right article (C132c), and an
		// MCP-only framework's install note reads "Letta's own packages".
		article := map[string]string{"Agno": "an", "OpenAI Agents SDK": "an", "ElizaOS": "an"}[view.Name]
		if article == "" {
			article = "a"
		}
		give := "Give " + article + " " + view.Name + " agent "
		if !strings.Contains(body, `<meta name="description" content="`+give) || !strings.Contains(body, `<meta property="og:description" content="`+give) {
			t.Errorf("%s descriptions do not open %q", path, give)
		}
		if view.Kind == "mcp" && (!strings.Contains(body, ">"+view.Name+"'s own packages only; SwarmMemo itself is one MCP URL") || strings.Contains(body, "The "+view.Name+"'s")) {
			t.Errorf("%s install note does not read %q", path, view.Name+"'s own packages only")
		}
		// C142: @elizaos/plugin-mcp 1.8.2 imports getRequestContext, which
		// the stable @elizaos/core 1.7.2 lacks, and 1.7.0 speaks only SSE;
		// the page pins 1.8.1 and says why.
		if view.Slug == "elizaos" && (view.Install != "bun add @elizaos/plugin-mcp@1.8.1" || !strings.Contains(view.InstallNote, "1.7.2") || !strings.Contains(view.InstallNote, "Streamable HTTP")) {
			t.Errorf("%s install line %q, note %q: want plugin-mcp pinned to 1.8.1 with why", path, view.Install, view.InstallNote)
		}
		// Letta's Python server is retired and letta/letta now ships Letta
		// Code (outside agents, 2026-10-10): the page leads with the Agent
		// SDK pinned to the release it was run against, then Letta Code and
		// the Letta API on Letta Cloud, each pointing at the core profile.
		if view.Slug == "letta" {
			if view.Install != "npm install @letta-ai/letta-agent-sdk@0.8.18" || view.Lang != "typescript" || !strings.Contains(view.InstallNote, "0.8.18") ||
				!strings.Contains(view.InstallNote, "retired") || !strings.Contains(view.InstallNote, "Letta Code") ||
				!strings.Contains(view.Example, `mcpServers: { swarmmemo: { type: "http", url: "`+view.MCPURL+`" } }`) || strings.Contains(view.Example, "8283") {
				t.Errorf("%s: install %q, note %q, example %q", path, view.Install, view.InstallNote, view.Example)
			}
			installs := []string{}
			for _, alt := range view.Also {
				installs = append(installs, alt.Install)
				if alt.Title == "" || alt.Line == "" || alt.Lang == "" || !strings.Contains(alt.Code, view.MCPURL) || strings.Contains(alt.Code, "8283") {
					t.Errorf("%s other route: %+v", path, alt)
				}
			}
			if !slices.Equal(installs, []string{"npm install -g @letta-ai/letta-code", "pip install letta-client==1.12.1"}) {
				t.Errorf("%s other routes install %v", path, installs)
			}
		} else if len(view.Also) != 0 {
			t.Errorf("%s: unexpected other routes %+v", path, view.Also)
		}
		wants := []string{view.Name, view.Intro, view.Install, view.InstallNote, view.Example, view.Identity, view.Keygen, view.KeyExample, view.MCP, view.MCPCode}
		for _, tool := range view.Tools {
			wants = append(wants, tool.Name, tool.Line)
		}
		for _, alt := range view.Also {
			wants = append(wants, alt.Title, alt.Line, alt.Install, alt.Code)
		}
		for _, want := range wants {
			if !strings.Contains(body, want) {
				t.Errorf("%s does not show %q from its JSON twin", path, want)
			}
		}
		for surface, text := range listings {
			if !strings.Contains(text, path) {
				t.Errorf("%s does not list %s", surface, path)
			}
		}
		if view.Kind != "package" {
			continue
		}
		readme, err := os.ReadFile("../../integrations/" + view.Slug + "/README.md")
		if err != nil || !strings.Contains(string(readme), "pip install "+view.Package) || !strings.Contains(string(readme), view.MCPURL) {
			t.Errorf("integrations/%s/README.md does not match the page's install line and MCP URL: %v", view.Slug, err)
		}
		if !published["integrations/"+view.Slug+"/README.md"] || !published["integrations/"+view.Slug+"/pyproject.toml"] {
			t.Errorf("integrations/%s is not in the public snapshot", view.Slug)
		}
	}
}
