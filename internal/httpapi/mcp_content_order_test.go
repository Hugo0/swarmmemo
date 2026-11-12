package httpapi

import (
	"encoding/json"
	"reflect"
	"testing"

	"swarmmemo/internal/board"
)

// Every tool whose answer carries prose beside its JSON keeps the JSON
// first: content[0] parses as the same object as structuredContent, and the
// prose follows (C928: clients that parse content[0] failed on a lead).
func TestMCPMultiContentJSONFirst(t *testing.T) {
	_, s := hostedServerWith(t, board.Features{Ledger: board.LedgerOn, Trust: board.TrustOn})
	call := func(name string, args map[string]any) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
		out := mcpRequest(t, s, "/mcp", "", string(raw))
		if dig(out, "result", "isError") == true {
			t.Fatalf("%s refused: %v", name, out)
		}
		content, _ := dig(out, "result", "content").([]any)
		structured, _ := dig(out, "result", "structuredContent").(map[string]any)
		if len(content) < 2 || structured == nil {
			t.Fatalf("%s: want JSON plus prose and structuredContent: %v", name, out)
		}
		var first map[string]any
		if err := json.Unmarshal([]byte(dig(content[0], "text").(string)), &first); err != nil {
			t.Fatalf("%s: content[0] is not JSON: %v %v", name, err, content[0])
		}
		if !reflect.DeepEqual(first, structured) {
			t.Fatalf("%s: content[0] differs from structuredContent:\n%v\n%v", name, first, structured)
		}
		for i, c := range content[1:] {
			var v any
			if json.Unmarshal([]byte(dig(c, "text").(string)), &v) == nil {
				t.Errorf("%s: content[%d] is JSON again, want prose: %v", name, i+1, c)
			}
		}
		return structured
	}
	// create_identity without a session: unbound, so unboundLead follows.
	created := call("create_identity", map[string]any{})
	if dig(created, "data", "bound") != false {
		t.Fatalf("create_identity bound without a session: %v", created)
	}
	agent, _ := dig(created, "data", "agent").(string)
	mustTool(t, s, "/mcp", "Bearer "+dig(created, "data", "token").(string), "post_message", map[string]any{"text": "Hello from C928."}) // a public agent now
	if trust := call("trust", map[string]any{"agent": agent}); dig(trust, "data", "summary") == nil {
		t.Fatalf("trust has no summary: %v", trust)
	}
	call("recover_identity", map[string]any{"recovery_code": dig(created, "data", "recovery_code")})
}
