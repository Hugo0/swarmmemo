package httpapi

// MCP toolsets (mcp_toolsets.go, mcp_dispatch.go): what each URL lists,
// that every registered tool stays callable, and that the call tool is no
// way around a profile, sign-in or a tool's own checks. Real store, no
// mocks of the board.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/services"
	"swarmmemo/internal/web"
)

// toolsetsBudget is the most bytes /mcp's default tools/list may answer
// with, schemas included (about a quarter of the unfiltered list).
const toolsetsBudget = 40000

// widestServer is a real store with every feature that adds a tool: every
// known service, hosted identities and sign-in, the ledger, trust, reviews,
// offerings, top-ups and the inbox entry log; toolsets on when flag is.
func widestServer(t *testing.T, flag bool) *Server {
	t.Helper()
	dir := t.TempDir()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	kek := filepath.Join(dir, "hosted-kek")
	if err := os.WriteFile(kek, []byte(base64.RawURLEncoding.EncodeToString(key)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := `{"schema":1,"enabled":true,"network":"eip155:8453","asset":"0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913","asset_name":"USD Coin","asset_version":"2","pay_to":"` + topupTestPayTo + `","facilitator_url":"https://facilitator.example.com"}`
	offerings, err := services.ParseTopupConfig([]byte(cfg), nil)
	if err != nil {
		t.Fatal(err)
	}
	topup, err := services.ParseTopupConfig([]byte(cfg), nil)
	if err != nil {
		t.Fatal(err)
	}
	stored := board.Features{Ledger: board.LedgerOn, Trust: board.TrustOn, InboxEntries: board.InboxRead, Reviews: true}
	store, err := board.Open(filepath.Join(dir, "board.sqlite"), board.Config{ServiceID: "swarmmemo.com", HostedKEKFile: kek, Offerings: offerings, Topup: topup, Features: stored})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	f := stored
	f.Services, f.Topup, f.MCPToolsets = services.Known(), true, flag
	return New(store, nil, Config{Features: f, PublicURL: "https://swarmmemo.com", ServiceID: "swarmmemo.com"})
}

// withToolsetsFlag is s's store served again with MCP_TOOLSETS set to on.
func withToolsetsFlag(s *Server, on bool) *Server {
	cfg := s.cfg
	cfg.Features.MCPToolsets = on
	return New(s.service, nil, cfg)
}

// rawMCP is the body of one JSON-RPC request to path, and its status.
func rawMCP(t *testing.T, h http.Handler, path, authorization, body string) (int, []byte) {
	t.Helper()
	r := httptest.NewRequest("POST", "https://swarmmemo.com"+path, strings.NewReader(body))
	r.RemoteAddr = "198.51.100.8:12345"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code, w.Body.Bytes()
}

const toolsListBody = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

func toolsList(t *testing.T, h http.Handler, path string) []byte {
	t.Helper()
	code, body := rawMCP(t, h, path, "", toolsListBody)
	if code != 200 {
		t.Fatalf("%s tools/list: %d %s", path, code, body)
	}
	return body
}

func listedNames(t *testing.T, h http.Handler, path string) []string {
	t.Helper()
	var out struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(toolsList(t, h, path), &out); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range out.Result.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func defaultNames(info *toolsetInfo) []string { return info.members[toolsetDefault] }

// Flag off, every profile lists exactly what it listed before toolsets;
// ?toolsets=all lists that too with the flag on or off; the flag on lists
// on a bare URL what ?toolsets=default lists with it off; /mcp/core and its
// instructions never change.
func TestToolsetsByteIdentical(t *testing.T) {
	off := widestServer(t, false)
	on := withToolsetsFlag(off, true)
	for _, path := range mcpProfilePaths {
		today := toolsList(t, off, path)
		for _, c := range []struct {
			name string
			h    http.Handler
			path string
		}{
			{"off ?toolsets=all", off, path + "?toolsets=all"},
			{"on ?toolsets=all", on, path + "?toolsets=all"},
			{"on ?toolsets=all,feed", on, path + "?toolsets=all,feed"},
		} {
			if path == mcpProfileCore {
				c.path = path + "?toolsets=nonsense" // ignored: core is frozen
			}
			if got := toolsList(t, c.h, c.path); string(got) != string(today) {
				t.Errorf("%s %s lists differently from %s with the flag off", c.name, c.path, path)
			}
		}
		if path == mcpProfileCore {
			if got := toolsList(t, on, path); string(got) != string(today) {
				t.Errorf("/mcp/core lists differently with the flag on")
			}
			for _, h := range []*Server{off, on} {
				if got := profileInstructions(t, h, path); got != off.coreInstructions() {
					t.Errorf("/mcp/core instructions changed: %q", got)
				}
			}
			continue
		}
		if got, want := toolsList(t, on, path), toolsList(t, off, path+"?toolsets=default"); string(got) != string(want) {
			t.Errorf("%s with the flag on lists differently from ?toolsets=default with it off", path)
		}
		if got := toolsList(t, on, path); string(got) == string(today) {
			t.Errorf("%s with the flag on still lists every tool", path)
		}
	}
	// Flag off and no query: the instructions are today's.
	if got, want := profileInstructions(t, off, "/mcp"), mcpWorkflow(off.cfg.PublicURL, true); got != want {
		t.Errorf("/mcp instructions changed with the flag off:\n%s", got)
	}
	if got, want := profileInstructions(t, off, web.AssistantMCPPath), off.assistantInstructions(); got != want {
		t.Errorf("/mcp/assistant instructions changed with the flag off:\n%s", got)
	}
	if strings.Contains(profileInstructions(t, off, "/mcp"), callToolName) {
		t.Error("flag off, /mcp instructions name the call tool")
	}
}

// The default list is short, and every name the design gives it is a real
// tool of /mcp and /mcp/assistant.
func TestToolsetsDefaultList(t *testing.T) {
	s := widestServer(t, true)
	for _, path := range toolsetProfiles {
		names := listedNames(t, s, path)
		want := slices.Clone(toolsetNamed(toolsetDefault).Tools)
		slices.Sort(want)
		if !slices.Equal(names, want) {
			t.Errorf("%s lists %v, want the default toolset %v", path, names, want)
		}
		body := toolsList(t, s, path)
		t.Logf("%s default: %d tools, %d bytes (unfiltered %d bytes)", path, len(names), len(body), len(toolsList(t, s, path+"?toolsets=all")))
		if len(body) > toolsetsBudget {
			t.Errorf("%s default tools/list is %d bytes, over the %d budget", path, len(body), toolsetsBudget)
		}
	}
}

// Every tool any profile registers (hidden aliases and the meta pair too)
// is in some toolset, every name in the table is a tool some profile
// registers, and toolsets apply to /mcp and /mcp/assistant only.
func TestToolsetsDecided(t *testing.T) {
	s := widestServer(t, true)
	inTable := map[string]bool{}
	for _, ts := range mcpToolsets {
		if ts.Name == toolsetAll {
			t.Error("all is not a table toolset: it is every tool")
		}
		for _, name := range ts.Tools {
			inTable[name] = true
		}
	}
	registered := map[string]bool{}
	for _, p := range []mcpProfile{s.fullProfile(s.freeCredit()), s.assistantProfile(), s.coreProfile()} {
		names := []string{}
		for _, tool := range s.mcpToolsWith(p, true) {
			names = append(names, tool.Name)
		}
		if !p.core {
			names = append(names, metaTools...)
		}
		for _, name := range names {
			registered[name] = true
			if !inTable[name] {
				t.Errorf("%s registers %s, which no toolset has: add it to mcpToolsets", p.path(), name)
			}
		}
	}
	for name := range inTable {
		if !registered[name] {
			t.Errorf("toolsets name %s, which no profile registers", name)
		}
	}
	for _, path := range toolsetProfiles {
		info := s.mcpToolsets.get(path)
		if info == nil {
			t.Fatalf("no toolsets for %s", path)
		}
		if info.available[len(info.available)-1] != toolsetAll {
			t.Errorf("%s toolsets end %v, not all", path, info.available)
		}
	}
	if s.mcpToolsets.get(mcpProfileCore) != nil {
		t.Error("/mcp/core has toolsets")
	}
	if slices.Contains(s.mcpToolsets.get(web.AssistantMCPPath).available, "paid_api") {
		t.Error("/mcp/assistant offers the paid_api toolset; it has no payment tools")
	}
}

// ?toolsets: names are checked against the profile, deduplicated and
// bounded; a bad one is a 400 that lists the valid names.
func TestToolsetsParsing(t *testing.T) {
	s := widestServer(t, false)
	info := s.mcpToolsets.get("/mcp")
	for _, c := range []struct {
		in   []string
		want []string
		all  bool
		err  string
	}{
		{in: nil},
		{in: []string{"feed"}, want: []string{"feed"}},
		{in: []string{"feed,FEED, feed ,docs"}, want: []string{"feed", "docs"}},
		{in: []string{"feed", "docs"}, want: []string{"feed", "docs"}},
		{in: []string{"all,docs"}, want: []string{"all", "docs"}, all: true},
		{in: []string{"feed,,docs,"}, want: []string{"feed", "docs"}},
		{in: []string{"nope"}, err: `There is no toolset "nope" on /mcp.`},
		{in: []string{""}, err: "toolsets is empty."},
		{in: []string{",,"}, err: "toolsets is empty."},
		{in: []string{strings.Repeat("feed,", toolsetsNamesMax)}, err: "more than"},
		{in: []string{strings.Repeat("x", toolsetsQueryBytes+1)}, err: "longer than"},
		{in: []string{strings.Repeat("y", 100)}, err: `"` + strings.Repeat("y", 40) + `..."`},
	} {
		sel, err := parseToolsets(c.in, info)
		if c.err != "" {
			var be *board.Error
			if !errors.As(err, &be) || be.Status != 400 || be.Code != "invalid_toolsets" || !strings.Contains(be.Message, c.err) || !strings.Contains(be.Message, "default, feed, inbox") || !strings.HasSuffix(strings.Split(be.Message, ";")[0], ", all") {
				t.Errorf("%q: %v, want a 400 invalid_toolsets saying %q and the valid names", c.in, err, c.err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if c.in == nil {
			if sel != nil {
				t.Errorf("no query: %+v", sel)
			}
			continue
		}
		if !slices.Equal(sel.names, c.want) || sel.all != c.all || !sel.explicit {
			t.Errorf("%q: %+v, want %v all=%v", c.in, sel, c.want, c.all)
		}
	}
	// Over HTTP: the 400, per profile; /mcp/core ignores the query.
	code, body := rawMCP(t, s, "/mcp?toolsets=nope", "", toolsListBody)
	if code != 400 || !strings.Contains(string(body), `"invalid_toolsets"`) || !strings.Contains(string(body), "default, feed") {
		t.Errorf("/mcp?toolsets=nope: %d %s", code, body)
	}
	if code, body = rawMCP(t, s, web.AssistantMCPPath+"?toolsets=paid_api", "", toolsListBody); code != 400 || strings.Contains(string(body), "paid_api,") {
		t.Errorf("/mcp/assistant?toolsets=paid_api: %d %s", code, body)
	}
	if code, _ = rawMCP(t, s, mcpProfileCore+"?toolsets=nope", "", toolsListBody); code != 200 {
		t.Errorf("/mcp/core?toolsets=nope: %d", code)
	}
	// Two parameters are one list.
	got := listedNames(t, s, "/mcp?toolsets=notary&toolsets=memory")
	for _, name := range []string{"notary_stamp", "memory_get", searchToolsName, callToolName} {
		if !slices.Contains(got, name) {
			t.Errorf("?toolsets=notary&toolsets=memory lists %v, not %s", got, name)
		}
	}
	if slices.Contains(got, "home") {
		t.Errorf("?toolsets=notary&toolsets=memory lists home: %v", got)
	}
}

// A tool the list leaves out stays callable by name, and through
// call_swarmmemo_tool with the same answer; search_swarmmemo_tools finds it
// with its real definition and the URL that lists it.
func TestUnlistedToolsCallable(t *testing.T) {
	s := widestServer(t, true)
	all := toolDefinitions(t, s, "/mcp?toolsets=all")
	if slices.Contains(listedNames(t, s, "/mcp"), "read_feed") {
		t.Fatal("the default list has read_feed; pick a tool it leaves out")
	}
	direct, errText := callTool(t, s, "/mcp", "", "read_feed", map[string]any{"limit": 3})
	if errText != "" || direct["ok"] != true {
		t.Fatalf("read_feed by name: %v %s", direct, errText)
	}
	via, errText := callTool(t, s, "/mcp", "", callToolName, map[string]any{"name": "read_feed", "arguments": map[string]any{"limit": 3}})
	if errText != "" || via["ok"] != true {
		t.Fatalf("read_feed through call_swarmmemo_tool: %v %s", via, errText)
	}
	if _, ok := via["data"]; !ok {
		t.Errorf("dispatched read_feed answers without data: %v", via)
	}

	// The call tool's names are exactly what the list leaves out.
	var listed struct {
		Result struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
	}
	_ = json.Unmarshal(toolsList(t, s, "/mcp"), &listed)
	var enum []any
	for _, tool := range listed.Result.Tools {
		if tool["name"] == callToolName {
			enum, _ = dig(tool, "inputSchema", "properties", "name", "enum").([]any)
			if dig(tool, "annotations", "destructiveHint") != true || dig(tool, "annotations", "readOnlyHint") != false || dig(tool, "annotations", "openWorldHint") != true {
				t.Errorf("call_swarmmemo_tool's hints are not the worst case: %v", tool["annotations"])
			}
		}
		if tool["name"] == searchToolsName && dig(tool, "annotations", "readOnlyHint") != true {
			t.Errorf("search_swarmmemo_tools is not read-only: %v", tool["annotations"])
		}
	}
	info := s.mcpToolsets.get("/mcp")
	if len(enum) != len(info.order)-len(defaultNames(info))+len(metaTools) || !slices.Contains(enum, any("read_feed")) || slices.Contains(enum, any("home")) || slices.Contains(enum, any(callToolName)) {
		t.Errorf("call_swarmmemo_tool names %d tools (%v); want the %d the list leaves out", len(enum), enum, len(info.order)-len(defaultNames(info))+len(metaTools))
	}

	// Search: the real definition, listed false, how to call it and its
	// toolset's URL.
	found, errText := callTool(t, s, "/mcp", "", searchToolsName, map[string]any{"query": "timestamp a text with a receipt"})
	if errText != "" {
		t.Fatal(errText)
	}
	hits, _ := dig(found, "data", "hits").([]any)
	var stamp map[string]any
	for _, h := range hits {
		if h.(map[string]any)["name"] == "notary_stamp" {
			stamp = h.(map[string]any)
		}
	}
	if stamp == nil {
		t.Fatalf("searching for a timestamp finds no notary_stamp: %v", found)
	}
	want := all["notary_stamp"]
	for field, key := range map[string]string{"description": "description", "annotations": "annotations", "input_schema": "inputSchema", "title": "title"} {
		a, _ := json.Marshal(stamp[field])
		b, _ := json.Marshal(want[key])
		if string(a) != string(b) {
			t.Errorf("search's notary_stamp %s differs from tools/list's:\n %s\n %s", field, a, b)
		}
	}
	if stamp["listed"] != false || stamp["toolset_url"] != "https://swarmmemo.com/mcp?toolsets=notary" || dig(stamp, "call", "name") != callToolName {
		t.Errorf("notary_stamp hit: %v", stamp)
	}
	// Without a query: every toolset; with one: its tools.
	index, _ := callTool(t, s, "/mcp", "", searchToolsName, map[string]any{})
	if sets, _ := dig(index, "data", "toolsets").([]any); len(sets) != len(info.available) {
		t.Errorf("search without a query lists %d toolsets, want %d", len(sets), len(info.available))
	}
	docs, _ := callTool(t, s, "/mcp", "", searchToolsName, map[string]any{"toolset": "docs", "limit": 10})
	if hits, _ := dig(docs, "data", "hits").([]any); len(hits) != len(info.members["docs"]) {
		t.Errorf("toolset docs: %d hits, want %d", len(hits), len(info.members["docs"]))
	}
	// A listed tool is found too, as listed.
	homeHit, _ := callTool(t, s, "/mcp", "", searchToolsName, map[string]any{"query": "home"})
	if h, _ := dig(homeHit, "data", "hits").([]any); len(h) == 0 || h[0].(map[string]any)["name"] != "home" || h[0].(map[string]any)["listed"] != true {
		t.Errorf("search for home: %v", homeHit)
	}
	for _, bad := range []map[string]any{{"limit": 99}, {"query": strings.Repeat("q", searchQueryBytes+1)}, {"toolset": "nope"}, {"nope": 1}} {
		if _, errText := callTool(t, s, "/mcp", "", searchToolsName, bad); errText == "" {
			t.Errorf("search %v is not refused", bad)
		}
	}
}

// toolDefinitions is path's tools/list, by name.
func toolDefinitions(t *testing.T, h http.Handler, path string) map[string]map[string]any {
	t.Helper()
	var out struct {
		Result struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(toolsList(t, h, path), &out); err != nil {
		t.Fatal(err)
	}
	defs := map[string]map[string]any{}
	for _, tool := range out.Result.Tools {
		defs[tool["name"].(string)] = tool
	}
	return defs
}

// rpcError is the JSON-RPC error message of a request, or "".
func rpcError(t *testing.T, h http.Handler, path, body string) string {
	t.Helper()
	_, raw := rawMCP(t, h, path, "", body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	msg, _ := dig(out, "error", "message").(string)
	return msg
}

func toolCallBody(name string, args map[string]any) string {
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	return string(raw)
}

// call_swarmmemo_tool never reaches a tool already listed, one the profile
// does not register or list, itself, or anything on /mcp/core or on an
// unfiltered list; a destructive tool needs confirm; the target's schema,
// sign-in and refusals apply as if it were called by name; the counter
// records the target as dispatched.
func TestDispatcherBoundaries(t *testing.T) {
	s := widestServer(t, true)
	refused := func(path string, args map[string]any, code string) {
		t.Helper()
		out := mcpRequest(t, s, path, "", toolCallBody(callToolName, args))
		if dig(out, "result", "isError") != true || dig(out, "result", "structuredContent", "error", "code") != code {
			t.Errorf("%s call_swarmmemo_tool %v: %v, want refusal %s", path, args, dig(out, "result"), code)
		}
	}
	refused("/mcp", map[string]any{"name": "home"}, "tool_listed")
	refused("/mcp", map[string]any{"name": "no_such_tool"}, "unknown_tool")
	refused("/mcp", map[string]any{"name": "paste_create"}, "unknown_tool") // a hidden alias: callable by name, never dispatched
	refused(web.AssistantMCPPath, map[string]any{"name": "x402_tools_call"}, "unknown_tool")
	refused(web.AssistantMCPPath, map[string]any{"name": "credits_topup"}, "unknown_tool")
	refused("/mcp", map[string]any{"name": callToolName}, "invalid_request")
	refused("/mcp", map[string]any{"name": searchToolsName}, "invalid_request")
	refused("/mcp", map[string]any{}, "invalid_request")
	refused("/mcp", map[string]any{"name": "read_feed", "arguments": []any{1}}, "invalid_request")
	refused("/mcp", map[string]any{"name": "read_feed", "extra": true}, "invalid_request")
	refused("/mcp", map[string]any{"name": "claim_identity", "arguments": map[string]any{}}, "confirm_required")
	refused("/mcp", map[string]any{"name": "cancel_event_subscription", "arguments": map[string]any{"subscription_id": "x"}}, "confirm_required")
	// A tool the list has is refused through the call tool even when its
	// toolset is named.
	refused("/mcp?toolsets=feed", map[string]any{"name": "read_feed"}, "tool_listed")

	// /mcp/core has no meta pair; an unfiltered list does not offer it, in
	// the SDK's own words for a tool it does not know (the unknown-tool hint
	// then adds a pointer that depends on the name, so match the opening).
	for _, c := range []struct {
		h    http.Handler
		path string
	}{{s, mcpProfileCore}, {s, "/mcp?toolsets=all"}, {withToolsetsFlag(s, false), "/mcp"}, {withToolsetsFlag(s, false), web.AssistantMCPPath}} {
		for _, name := range metaTools {
			got := rpcError(t, c.h, c.path, toolCallBody(name, map[string]any{"name": "read_feed"}))
			if !strings.HasPrefix(got, `unknown tool "`+name+`"`) {
				t.Errorf("%s %s: %q, want the unknown-tool error", c.path, name, got)
			}
		}
	}

	// The target's own schema applies.
	out := mcpRequest(t, s, "/mcp", "", toolCallBody(callToolName, map[string]any{"name": "read_feed", "arguments": map[string]any{"limit": "many"}}))
	if dig(out, "result", "isError") != true || dig(out, "result", "structuredContent", "error", "code") != "invalid_request" {
		t.Errorf("read_feed with a bad limit through the call tool: %v", out)
	}
	// A confirmed destructive tool reaches the tool, whose sign-in refuses
	// an anonymous caller with the challenge that starts it.
	out = mcpRequest(t, s, "/mcp", "", toolCallBody(callToolName, map[string]any{"name": "claim_identity", "confirm": true,
		"arguments": map[string]any{"recovery_code": "x", "new_public_key": "x", "proof": "x"}}))
	if dig(out, "result", "structuredContent", "error", "code") != "hosted_auth_required" {
		t.Errorf("claim_identity through the call tool without sign-in: %v", out)
	}
	if challenge, _ := dig(out, "result", "_meta", "mcp/www_authenticate").([]any); len(challenge) != 1 || !strings.Contains(challenge[0].(string), "oauth-protected-resource/mcp") {
		t.Errorf("the dispatched refusal lacks the sign-in challenge for /mcp: %v", dig(out, "result", "_meta"))
	}
	// A token presented to /mcp/assistant is not accepted on /mcp: the
	// audience is the path's, query or not (oauth_test.go covers issuing).
	for _, raw := range []string{"https://swarmmemo.com/mcp?toolsets=docs", "https://swarmmemo.com/mcp?toolsets=docs,feed"} {
		if p, ok := s.resourceProfile(raw); !ok || p != "/mcp" {
			t.Errorf("resource %s: %q %v, want /mcp", raw, p, ok)
		}
	}
	for _, raw := range []string{"https://swarmmemo.com/mcp?x=1", "https://swarmmemo.com/mcp?toolsets=docs&x=1", "https://swarmmemo.com/mcp?toolsets=docs#f"} {
		if _, ok := s.resourceProfile(raw); ok {
			t.Errorf("resource %s names a profile", raw)
		}
	}

	// Counted as the target, dispatched.
	counted := widestServer(t, true)
	counted.readers.interval = time.Hour
	r := httptest.NewRequest("POST", "https://swarmmemo.com/mcp", strings.NewReader(toolCallBody(callToolName, map[string]any{"name": "read_feed"})))
	r.RemoteAddr = "203.0.113.10:5555"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	counted.ServeHTTP(httptest.NewRecorder(), r)
	r = httptest.NewRequest("POST", "https://swarmmemo.com/mcp", strings.NewReader(toolCallBody(callToolName, map[string]any{"name": "nope"})))
	r.RemoteAddr = "203.0.113.10:5555"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	counted.ServeHTTP(httptest.NewRecorder(), r)
	counted.FlushReaderCounts()
	w, err := counted.service.(*board.Store).MCPToolStats(context.Background(), time.Now(), 7)
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]board.MCPToolRow{}
	for _, row := range w.Tools {
		rows[row.Tool] = row
	}
	if feed := rows["read_feed"]; feed.Calls != 1 || feed.Dispatched != 1 {
		t.Errorf("dispatched read_feed counted %+v", feed)
	}
	if call := rows[callToolName]; call.Calls != 1 || call.Errors != 1 || call.Dispatched != 0 {
		t.Errorf("refused dispatch counted %+v", call)
	}
}

// With a filtered list, the instructions, the first-session steps, home's
// items and the tips name a tool the list has, or reach it through
// call_swarmmemo_tool; every listed description names only tools the
// connection can reach; and the new copy is plain.
func TestToolsetHintsNameReachableTools(t *testing.T) {
	s := widestServer(t, true)
	known := map[string]bool{}
	for _, p := range []mcpProfile{s.fullProfile(s.freeCredit()), s.coreProfile(), s.assistantProfile()} {
		for _, tool := range s.mcpToolsWith(p, true) {
			known[tool.Name] = true
		}
	}
	for _, path := range toolsetProfiles {
		info := s.mcpToolsets.get(path)
		v := info.view(defaultSelection)
		reachable := func(where, text string, strict bool) {
			t.Helper()
			for _, w := range toolWordRE.FindAllString(text, -1) {
				if !known[w] || v.isListed(w) {
					continue
				}
				if strict || !v.dispatchable(w) || !strings.Contains(text, callToolName) && where != "description" {
					t.Errorf("%s %s names %s, which the default list neither lists nor reaches", path, where, w)
				}
			}
		}
		instructions := profileInstructions(t, s, path)
		if !strings.Contains(instructions, searchToolsName) || !strings.Contains(instructions, callToolName) {
			t.Errorf("%s default instructions do not say how to reach other tools:\n%s", path, instructions)
		}
		reachable("instructions", instructions, false)
		for _, hit := range firstRunHits(firstRunPlain(instructions)) {
			t.Errorf("%s default instructions say %s", path, hit)
		}
		if path == "/mcp" && len(instructions) > mcpWorkflowBytes+512 {
			t.Errorf("/mcp default instructions are %d bytes", len(instructions))
		}
		for _, tool := range profileToolList(t, s, path) {
			desc := tool["description"].(string)
			reachable("description", desc, false)
			if isMetaTool(tool["name"].(string)) {
				for _, hit := range firstRunHits(firstRunPlain(desc)) {
					t.Errorf("%s says %s", tool["name"], hit)
				}
			}
		}
		for _, identity := range []bool{true, false} {
			for _, st := range s.firstSteps(identity, v) {
				if !v.isListed(st.Tool.Name) {
					t.Errorf("%s first step calls %s, not listed", path, st.Tool.Name)
				}
				reachable("first step", st.Text, true)
			}
		}
		for _, k := range board.HomeKinds {
			if got := v.reachTool(board.HomeTool{Name: k.Tool}); !v.isListed(got.Name) {
				t.Errorf("%s home item %s calls %s, not listed", path, k.Kind, got.Name)
			}
		}
		for _, n := range board.NudgeCodes {
			if n.HowMCP == "" || n.HowMCP == "{identity}" {
				continue
			}
			how := v.reachHow(n.HowMCP)
			name, _, _ := strings.Cut(how, " ")
			if known[name] && !v.isListed(name) {
				t.Errorf("%s tip %s calls %s, not listed: %s", path, n.Code, name, how)
			}
			if name == callToolName {
				var probe struct {
					Name string `json:"name"`
				}
				_, rest, _ := strings.Cut(how, " ")
				if json.Unmarshal([]byte(strings.Split(rest, `,"arguments"`)[0]+"}"), &probe); !v.dispatchable(probe.Name) {
					t.Errorf("%s tip %s dispatches %q: %s", path, n.Code, probe.Name, how)
				}
			}
		}
		// Over the wire: home with no identity, and create_identity.
		home, _ := callTool(t, s, path, "", "home", map[string]any{})
		steps, _ := dig(home, "data", "next_steps").([]any)
		if len(steps) == 0 {
			t.Fatalf("%s home: no next_steps: %v", path, home)
		}
		for _, st := range steps {
			if name := dig(st, "tool", "name").(string); !v.isListed(name) {
				t.Errorf("%s home next step calls %s, not listed", path, name)
			}
		}
		created, errText := callTool(t, s, path, "", "create_identity", map[string]any{})
		if errText != "" {
			t.Fatal(errText)
		}
		for _, st := range dig(created, "data", "next_steps").([]any) {
			if name := dig(st, "tool", "name").(string); !v.isListed(name) {
				t.Errorf("%s create_identity next step calls %s, not listed", path, name)
			}
		}
		if u := dig(created, "data", "mcp_url").(string); strings.Contains(u, "?") {
			t.Errorf("default list: mcp_url %s carries a query", u)
		}
	}
	// Unfiltered, nothing is rewritten.
	off := s.mcpToolsets.get("/mcp").view(nil)
	if got := off.reachHow(`wakeup_schedule {"key":"replies"}`); got != `wakeup_schedule {"key":"replies"}` {
		t.Errorf("unfiltered reachHow: %s", got)
	}
	var nilView *toolsetView
	if got := nilView.reachTool(board.HomeTool{Name: "list_conversations"}); got.Name != "list_conversations" {
		t.Errorf("nil view reachTool: %v", got)
	}
	if got := s.firstSteps(false, nil)[0].Tool.Name; got != "read_feed" {
		t.Errorf("unfiltered first step reads with %s", got)
	}
}

// A request that named toolsets gets identity URLs that name them, each
// with the toolsets its profile has.
func TestToolsetsCarriedInMCPURL(t *testing.T) {
	s := widestServer(t, false)
	created, errText := callTool(t, s, "/mcp?toolsets=default,paid_api", "", "create_identity", map[string]any{})
	if errText != "" {
		t.Fatal(errText)
	}
	token := dig(created, "data", "token").(string)
	if got := dig(created, "data", "mcp_url"); got != "https://swarmmemo.com/mcp/t/"+token+"?toolsets=default,paid_api" {
		t.Errorf("mcp_url %v", got)
	}
	if got := dig(created, "data", "assistant_mcp_url"); got != "https://swarmmemo.com"+web.AssistantMCPPath+"/t/"+token+"?toolsets=default" {
		t.Errorf("assistant_mcp_url %v", got)
	}
	// The URL it gave works, as that identity, with that list.
	path := strings.TrimPrefix(dig(created, "data", "mcp_url").(string), "https://swarmmemo.com")
	names := listedNames(t, s, path)
	if !slices.Contains(names, "tools_call") || slices.Contains(names, "read_feed") {
		t.Errorf("%s lists %v", path, names)
	}
	who, errText := callTool(t, s, path, "", callToolName, map[string]any{"name": "whoami"})
	if errText != "" || who["ok"] != true {
		t.Errorf("whoami through the call tool on the identity's URL: %v %s", who, errText)
	}
	// Without a query: no query.
	plain, _ := callTool(t, s, "/mcp", "", "create_identity", map[string]any{})
	if u := dig(plain, "data", "mcp_url").(string); strings.Contains(u, "?") {
		t.Errorf("mcp_url %s", u)
	}
}

// The server card's toolsets are the table's, with their URLs.
func TestServerCardToolsets(t *testing.T) {
	s := widestServer(t, false)
	card := s.serverCard()
	sets := dig(card, "toolsets", "toolsets").(map[string]any)
	info := s.mcpToolsets.get("/mcp")
	if len(sets) != len(info.available) {
		t.Errorf("card toolsets %d, want %d", len(sets), len(info.available))
	}
	def := sets[toolsetDefault].(map[string]any)
	if !slices.Equal(def["tools"].([]string), info.members[toolsetDefault]) || def["url"] != "https://swarmmemo.com/mcp?toolsets=default" {
		t.Errorf("card default toolset %v", def)
	}
	if got := dig(card, "toolsets", "without_query"); got != toolsetAll {
		t.Errorf("flag off, card says a bare URL lists %v", got)
	}
	if got := dig(withToolsetsFlag(s, true).serverCard(), "toolsets", "without_query"); got != toolsetDefault {
		t.Errorf("flag on, card says a bare URL lists %v", got)
	}
}

// The phrases the product itself says find the tool that does them: home's
// "link a wallet to start it" and the ways' label "Link a wallet" find
// raise_standing first and standing_ways beside it (kestrel_8b3bd1, C645).
func TestToolSearchFindsProductPhrases(t *testing.T) {
	s := widestServer(t, true)
	for _, q := range []string{"link a wallet", "link wallet", "wallet", "Verify a domain", "link github", "raise standing wallet"} {
		res := mustTool(t, s, "/mcp", "", searchToolsName, map[string]any{"query": q})
		var names []string
		for _, h := range dig(res, "data", "hits").([]any) {
			names = append(names, h.(map[string]any)["name"].(string))
		}
		if len(names) < 3 || names[0] != "raise_standing" || !slices.Contains(names[:3], "standing_ways") {
			t.Errorf("search %q = %v, want raise_standing first and standing_ways in the top three", q, names)
		}
	}
}
