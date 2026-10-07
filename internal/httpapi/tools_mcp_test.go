package httpapi

// The tools search and call over MCP and /call/: tools_search and tools_call
// lead the hosted tools, tools_call signs as the connection's hosted
// identity when it has one, and a retry with the same request_id returns
// the first answer. Real store, real engine, a stub site for fetch.

import (
	"net/url"
	"slices"
	"strings"
	"testing"

	"swarmmemo/internal/services"
)

func TestToolsOverMCP(t *testing.T) {
	s := hostedPublicCallServer(t, 1_000_000)
	// tools_search and tools_call lead the catalogue's tools; the relay's
	// own tools say tools_call covers them.
	var names []string
	for _, tool := range s.mcpToolList() {
		names = append(names, tool.Name)
	}
	at := slices.Index(names, "list_services")
	if at < 0 || at+2 >= len(names) || names[at+1] != "tools_search" || names[at+2] != "tools_call" {
		t.Fatalf("tools: %v", names)
	}
	for _, tool := range s.mcpToolList() {
		if tool.Name == "tools_search" && (!strings.Contains(tool.Desc, "featured") || !strings.Contains(tool.Desc, "fetch.page")) {
			t.Errorf("tools_search does not lead with the featured tools: %s", tool.Desc)
		}
		if tool.Name == "tools_call" && !strings.Contains(tool.Desc, "Signed with your SwarmMemo identity") {
			t.Errorf("tools_call does not say it signs as the identity: %s", tool.Desc)
		}
	}

	// Without a query: the featured tools this deployment runs, in order.
	got := mustTool(t, s, "/mcp", "", "tools_search", map[string]any{})
	var ids []string
	for _, e := range dig(got, "data", "result", "tools").([]any) {
		ids = append(ids, e.(map[string]any)["id"].(string))
	}
	if strings.Join(ids, " ") != "swarmmemo:fetch.page swarmmemo:notary.stamp" {
		t.Fatalf("featured: %v", ids)
	}

	// Without an identity: the network's call, capped at 8 KiB.
	page := map[string]any{"id": "swarmmemo:fetch.page", "args": map[string]any{"url": "http://site.example/page"}}
	got = mustTool(t, s, "/mcp", "", "tools_call", page)
	text, _ := dig(got, "data", "result", "text").(string)
	if dig(got, "data", "service") != "fetch" || len(text) == 0 || len(text) > services.FetchAnonymousTextMax || dig(got, "data", "call", "request_id") == nil {
		t.Fatalf("tools_call without an identity: %d bytes, %v", len(text), got)
	}

	// With one: signed as the identity, its allowance charged, more text.
	me := newIdentity(t, s, "")
	path, agent := "/mcp/t/"+me["token"].(string), me["agent"].(string)
	big := map[string]any{"id": "swarmmemo:fetch.page", "args": map[string]any{"url": "http://site.example/page", "max_bytes": 32768}, "request_id": "tools-over-mcp-1"}
	before := creditsUsed(t, s, agent)
	first := mustTool(t, s, path, "", "tools_call", big)
	text, _ = dig(first, "data", "result", "text").(string)
	cost, _ := dig(first, "data", "call", "cost").(float64)
	if len(text) <= services.FetchAnonymousTextMax || cost <= 0 {
		t.Fatalf("tools_call as the identity: %d bytes, %v", len(text), first)
	}
	if used := creditsUsed(t, s, agent) - before; used != cost {
		t.Fatalf("charged %v to the identity, want the call's %v", used, cost)
	}
	// The same request_id again: the first answer, never charged twice.
	again := mustTool(t, s, path, "", "tools_call", big)
	if dig(again, "data", "call", "id") != dig(first, "data", "call", "id") || dig(again, "data", "call", "cost") != cost {
		t.Fatalf("retry: %v, first %v", again, first)
	}
	if used := creditsUsed(t, s, agent) - before; used != cost {
		t.Fatalf("a retry was charged: %v, want %v", used, cost)
	}

	// The notary through tools_call, signed: the same as notary_stamp.
	stamp := mustTool(t, s, path, "", "tools_call", map[string]any{"id": "swarmmemo:notary.stamp", "args": map[string]any{"text": "through tools"}})
	if dig(stamp, "data", "service") != "notary" || dig(stamp, "data", "call", "state") != "done" {
		t.Fatalf("notary through tools_call: %v", stamp)
	}
	// A tool id that names nothing, and a paid API without max_cost.
	if _, failure := callTool(t, s, path, "", "tools_call", map[string]any{"id": "swarmmemo:nope.page"}); !strings.Contains(failure, "names no tool") {
		t.Fatalf("an unknown id: %q", failure)
	}
	// The direct tools work as before.
	direct := mustTool(t, s, path, "", "fetch_page", map[string]any{"url": "http://site.example/page", "max_bytes": 32768})
	if dig(direct, "data", "service") != "fetch" || dig(direct, "data", "call", "state") != "done" {
		t.Fatalf("fetch_page: %v", direct)
	}
}

// /call/tools/search and /call/tools/call need no key: the search answers
// as a read, the call as the routed method's call, and a retry with the
// answer's request_id returns the first answer.
func TestToolsOverCallURLs(t *testing.T) {
	s := hostedPublicCallServer(t, 1_000_000)
	form := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	w := secReq(s, "GET", "/call/tools/search?query=read+a+web+page", "", "198.51.100.50:1", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"id":"swarmmemo:fetch.page"`) {
		t.Fatalf("search: %d %s", w.Code, w.Body.String())
	}
	body := `id=swarmmemo:fetch.page&args={"url":"http://site.example/page"}`
	w = secReq(s, "POST", "/call/tools/call", body, "198.51.100.50:1", form)
	first := decodeResult(t, w.Body.Bytes())
	requestID, _ := dig(first, "data", "call", "request_id").(string)
	if w.Code != 200 || dig(first, "data", "service") != "fetch" || dig(first, "data", "method") != "page" || requestID == "" {
		t.Fatalf("call: %d %s", w.Code, w.Body.String())
	}
	w = secReq(s, "POST", "/call/tools/call", body+"&request_id="+url.QueryEscape(requestID), "198.51.100.50:1", form)
	again := decodeResult(t, w.Body.Bytes())
	if w.Code != 200 || dig(again, "data", "call", "id") != dig(first, "data", "call", "id") {
		t.Fatalf("retry: %d %s", w.Code, w.Body.String())
	}
	// A tool that needs a key: refused without one, naming what takes none.
	w = secReq(s, "POST", "/call/tools/call", `id=swarmmemo:memory.put&args={"key":"k","value":"v"}`, "198.51.100.50:1", form)
	if w.Code == 200 {
		t.Fatalf("a key-needing tool without a key: %d %s", w.Code, w.Body.String())
	}
}
