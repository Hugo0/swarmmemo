package httpapi

// tools/list lands in the context of every client that attaches the server
// (the framework guides attach every tool), so its size is pinned (C132a):
// output schemas describe the shared result envelope compactly, never
// board.Result's whole type graph once per tool (1.4 MB on /mcp/core before).

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"swarmmemo/internal/web"
)

// mcpToolsListBudget is the most bytes a profile's tools/list answer may be
// (the widest server, pinServer: every service, hosted identities, sign-in).
var mcpToolsListBudget = map[string]int{
	"/mcp":               120_000,
	mcpProfileCore:       90_000,
	web.AssistantMCPPath: 90_000,
}

// mcpOutputSchemaBudget is the most bytes one tool's output schema may be.
const mcpOutputSchemaBudget = 256

// toolsListAnswer is a profile's tools/list answer: its size on the wire
// and its tools.
func toolsListAnswer(t *testing.T, s *Server, path string) (int, []map[string]any) {
	t.Helper()
	r := httptest.NewRequest("POST", "https://swarmmemo.com"+path, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	r.RemoteAddr = "198.51.100.8:12345"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != 200 {
		t.Fatalf("POST %s: %d %s", path, w.Code, w.Body.String())
	}
	raw, _ := dig(out, "result", "tools").([]any)
	var tools []map[string]any
	for _, r := range raw {
		tools = append(tools, r.(map[string]any))
	}
	return w.Body.Len(), tools
}

func TestMCPToolsListSizeBudget(t *testing.T) {
	s := pinServer(t)
	for path, budget := range mcpToolsListBudget {
		n, tools := toolsListAnswer(t, s, path)
		parts := map[string]int{}
		for _, tool := range tools {
			for _, k := range []string{"inputSchema", "outputSchema", "description"} {
				b, _ := json.Marshal(tool[k])
				parts[k] += len(b)
				if k == "outputSchema" && len(b) > mcpOutputSchemaBudget {
					t.Errorf("%s %s: output schema is %d bytes, over %d", path, tool["name"], len(b), mcpOutputSchemaBudget)
				}
			}
		}
		t.Logf("%s tools/list: %d bytes, %d tools, %v", path, n, len(tools), parts)
		if n > budget {
			t.Errorf("%s tools/list is %d bytes, over its %d budget", path, n, budget)
		}
	}
}

// TestMCPStructuredContentFitsOutputSchema checks results and refusals
// against the output schema tools/list declares for the tool, as a client
// that validates structured content would.
func TestMCPStructuredContentFitsOutputSchema(t *testing.T) {
	s := pinServer(t)
	for _, path := range []string{"/mcp", mcpProfileCore} {
		_, tools := toolsListAnswer(t, s, path)
		schemas := map[string]*jsonschema.Resolved{}
		for _, tool := range tools {
			raw, _ := json.Marshal(tool["outputSchema"])
			var schema jsonschema.Schema
			if err := json.Unmarshal(raw, &schema); err != nil {
				t.Fatalf("%s %s output schema: %v", path, tool["name"], err)
			}
			if schema.Type != "object" {
				t.Errorf("%s %s: output schema type %q, not object", path, tool["name"], schema.Type)
			}
			resolved, err := schema.Resolve(nil)
			if err != nil {
				t.Fatalf("%s %s output schema: %v", path, tool["name"], err)
			}
			schemas[tool["name"].(string)] = resolved
		}
		for _, call := range []struct {
			name string
			args map[string]any
		}{
			{"read_messages", map[string]any{"room": "lobby"}},
			{"post_message", map[string]any{"room": "lobby", "text": "output schema check"}},
			{"list_rooms", map[string]any{}},
			{"find_work", map[string]any{}},
			{"find_agents", map[string]any{}},
			{"list_services", map[string]any{}},
			{"allowance", map[string]any{}},
			{"read_thread", map[string]any{"message_id": "nope"}}, // a refusal
			{"read_agent", map[string]any{}},                      // the SDK refusing the arguments
		} {
			raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": call.name, "arguments": call.args}})
			out := mcpRequest(t, s, path, "", string(raw))
			content, ok := dig(out, "result", "structuredContent").(map[string]any)
			if !ok {
				t.Errorf("%s %s: no structured content: %v", path, call.name, out)
				continue
			}
			if err := schemas[call.name].Validate(content); err != nil {
				t.Errorf("%s %s: structured content does not fit the output schema: %v\n%v", path, call.name, err, content)
			}
		}
	}
}
