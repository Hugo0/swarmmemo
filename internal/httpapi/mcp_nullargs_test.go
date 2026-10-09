package httpapi

// C125: mcpadapt clients (crewai-tools' MCPServerAdapter) send null for every
// optional argument left unset; those calls succeed on every profile, and a
// required argument's null is still refused naming the field.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"swarmmemo/internal/web"
)

func TestMCPOptionalNullArgsAreAbsent(t *testing.T) {
	s, _ := hostedServicesServer(t)
	me := newIdentity(t, s, "")
	token := me["token"].(string)
	for _, profile := range []string{"/mcp", web.AssistantMCPPath, mcpProfileCore} {
		url := profile + "/t/" + token
		read := map[string]any{"older": nil, "target": nil, "room": nil, "page": nil, "sort": nil, "offset": nil, "limit": nil, "query": nil, "kind": nil}
		if got := mustTool(t, s, url, "", "read_messages", read); got["ok"] != true {
			t.Fatalf("%s read_messages with null optionals: %v", profile, got)
		}
		mustTool(t, s, url, "", "memory_put", map[string]any{"key": "c125", "value": "x", "max_cost": nil, "request_id": nil})
		if listed := mustTool(t, s, url, "", "memory_list", map[string]any{"prefix": nil, "cursor": nil, "max_cost": nil}); !strings.Contains(fmt.Sprint(listed), "c125") {
			t.Fatalf("%s memory_list with null optionals: %v", profile, listed)
		}
		if _, failure := callTool(t, s, url, "", "memory_get", map[string]any{"key": nil, "max_cost": nil}); !strings.Contains(failure, "/properties/key") {
			t.Fatalf("%s memory_get with key null: %q", profile, failure)
		}
		if _, failure := callTool(t, s, url, "", "read_thread", map[string]any{"message_id": nil}); !strings.Contains(failure, "/properties/message_id") {
			t.Fatalf("%s read_thread with message_id null: %q", profile, failure)
		}
	}
}

func TestNullRuleOf(t *testing.T) {
	rule := nullRuleOf(&jsonschema.Schema{Type: "object", Required: []string{"id"}, Properties: map[string]*jsonschema.Schema{
		"id": {Type: "string"}, "older": {Type: "string"}, "tags": {Types: []string{"null", "array"}}, "any": {},
	}})
	if !rule.required["id"] || rule.required["older"] || rule.nullable["older"] || !rule.nullable["tags"] || !rule.nullable["any"] {
		t.Fatalf("rule: %+v", rule)
	}
	m := nullRuleOf(map[string]any{"type": "object", "required": []string{"key"}, "properties": map[string]any{"key": map[string]any{"type": "string"}, "v": map[string]any{"type": []string{"string", "null"}}}})
	if !m.required["key"] || m.nullable["key"] || !m.nullable["v"] {
		t.Fatalf("map rule: %+v", m)
	}
}
