package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"swarmmemo/internal/web"
)

var resultTypeCalls int

// resultTypeCall is one tools/call on path at protocol version: the modern
// one (2026-07-28) with its headers and _meta, an older one with the
// MCP-Protocol-Version header alone. It returns the result, or nil and the
// JSON-RPC error.
func resultTypeCall(t *testing.T, h http.Handler, path, version, name string, args map[string]any) (map[string]any, any) {
	t.Helper()
	params := map[string]any{"name": name, "arguments": args}
	modern := version >= "2026-07-28"
	if modern {
		params["_meta"] = modernMeta
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": params})
	r := httptest.NewRequest("POST", "https://swarmmemo.com"+path, strings.NewReader(string(body)))
	resultTypeCalls++ // a fresh address each, under the per-address rate
	r.RemoteAddr = fmt.Sprintf("10.%d.%d.%d:12345", resultTypeCalls>>16&255, resultTypeCalls>>8&255, resultTypeCalls&255)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("MCP-Protocol-Version", version)
	if modern {
		r.Header.Set("Mcp-Method", "tools/call")
		r.Header.Set("Mcp-Name", name)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s %s on %s: %d %s", version, name, path, w.Code, w.Body.String())
	}
	res, _ := out["result"].(map[string]any)
	return res, out["error"]
}

// Every tools/call result of a 2026-07-28 request says resultType, whatever
// answered it: each tool of every profile (anonymous and hosted), the meta
// pair and their refusals, and a tool's own refusal; a 2025-06-18 result
// never does. A 2026-07-28 client rejects a result without it (C645).
func TestEveryModernToolResultHasResultType(t *testing.T) {
	s := widestServer(t, true)
	token := newIdentity(t, s, "resulttype-walker")["token"].(string)
	type call struct {
		name string
		args map[string]any
	}
	extra := []call{
		{searchToolsName, map[string]any{"query": "link a wallet"}},
		{searchToolsName, map[string]any{"nope": 1}},
		{callToolName, map[string]any{}},
		{callToolName, map[string]any{"name": "no_such_tool"}},
		{callToolName, map[string]any{"name": "raise_standing", "arguments": map[string]any{}}},
		{"post_message", map[string]any{}},
		{"no_such_tool", map[string]any{}},
	}
	for _, path := range []string{"/mcp", web.AssistantMCPPath, mcpProfileCore, "/mcp/t/" + token, web.AssistantMCPPath + "/t/" + token} {
		names := listedNames(t, s, path)
		for _, name := range listedNames(t, s, path+"?toolsets=all") {
			if !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
		calls := extra
		for _, name := range names {
			calls = append(calls, call{name, map[string]any{}})
		}
		results := 0
		for _, c := range calls {
			for _, version := range []string{"2026-07-28", "2025-06-18"} {
				res, rpcErr := resultTypeCall(t, s, path, version, c.name, c.args)
				if res == nil {
					if rpcErr == nil {
						t.Fatalf("%s %s on %s: neither result nor error", version, c.name, path)
					}
					continue
				}
				results++
				got, has := res["resultType"]
				if version == "2026-07-28" && got != "complete" {
					t.Errorf("%s %s %v on %s: resultType = %v; keys %v", version, c.name, c.args, path, got, keysOf(res))
				}
				if version != "2026-07-28" && has {
					t.Errorf("%s %s on %s: resultType %v on a legacy result", version, c.name, path, got)
				}
			}
		}
		if results < 2*len(names) {
			t.Errorf("%s: only %d results for %d tools", path, results, len(names))
		}
	}
}
