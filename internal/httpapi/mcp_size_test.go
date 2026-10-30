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
// (the widest server, pinServer: every service, hosted identities, sign-in,
// agent offerings). C168B raised /mcp by 6 KB and the core profile by 3 KB
// for the offering tools (about 7.7 KB and 2.9 KB, kept terse). C180 raised
// the core profile by 1 KB for set_handle and post_message handle (about
// 0.6 KB on each profile). C188 raised /mcp by 1.5 KB and the core profile
// by 1 KB for find, the one search (about 1.05 KB and 0.6 KB, kept terse).
// C293b added home and shortened read_updates, find_work, read_work and
// accept_work: every profile shrank, and so did its budget. C299 raised
// every profile by 1 KB for list_paid_api and raise_standing's domain link
// (about 0.9 KB on each, kept terse). C325 raised every profile by 1 KB
// for gate_solve, the SwarmMemo Gate (about 0.8 KB on each, kept terse).
// C353 raised every profile by 3.4 KB for read_verification and
// verify_publish (its checks schema) and the checks note (about 3.3 KB).
// C368 (2026-10-16): after C366 /mcp measured 132,099 of 132,100, so the
// descriptions shed what is said elsewhere: the hosted-identity note is
// "Needs a hosted identity (create_identity)" (the server instructions and
// create_identity say how to reconnect), and max_cost and request_id are
// no longer restated where their input schema already describes them.
// Measured after: /mcp 127,609, core 96,405, assistant 91,080; each budget
// keeps about 2 KB of headroom, so a small addition fits.
// C377-3 (2026-10-16) raised every profile by 3 KB for the work.* data the
// hosted tools lacked (mcp_dataspec.go): create_work and manage_work (about
// 2.6 KB, inputs undescribed where the name says it), post_message
// supersedes, postage on send_private and create_conversation, gate_solve
// bits and the bond on raise_standing. Measured after: /mcp 130,859, core
// 99,655, assistant 94,330, each with about 2 KB of headroom again.
// C559 (2026-10-24): assistant reached 96,498 of 96,500, so the shared
// max_cost and request_id notes became one short phrase each and repeated
// or schema-restating clauses went. Measured after: /mcp 128,703, core
// 96,400, assistant 94,273.
// C662 (2026-10-30): manage_app (app identity and house agents) is on /mcp
// and the assistant profile (every hosted tool is on both), and find_agents
// gained include: /mcp 133,191, assistant 98,333; budgets raised by those.
var mcpToolsListBudget = map[string]int{
	"/mcp":               134_500,
	mcpProfileCore:       101_500,
	web.AssistantMCPPath: 99_000,
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
