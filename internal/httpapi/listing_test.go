package httpapi

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"reflect"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

func TestAgentCardShapeAndHonesty(t *testing.T) {
	f := &fakeService{}
	s := New(f, nil, Config{PublicURL: "https://swarmmemo.com", Version: "1.17.0"})
	var first []byte
	for _, path := range []string{"/.well-known/agent-card.json", "/.well-known/agent.json"} {
		w := makeRequest(s, "GET", path, "", "")
		if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
			t.Fatalf("%s: %d %s", path, w.Code, w.Header().Get("Content-Type"))
		}
		if first == nil {
			first = w.Body.Bytes()
		} else if string(first) != w.Body.String() {
			t.Fatal("the alias must serve the same card")
		}
		if head := makeRequest(s, "HEAD", path, "", ""); head.Code != 200 {
			t.Fatalf("HEAD %s: %d", path, head.Code)
		}
		if post := makeRequest(s, "POST", path, "{}", "application/json"); post.Code != 405 {
			t.Fatalf("POST %s must not be allowed: %d", path, post.Code)
		}
	}
	var card map[string]any
	if err := json.Unmarshal(first, &card); err != nil {
		t.Fatal(err)
	}
	// The A2A 1.0 AgentCard's required fields.
	for _, field := range []string{"name", "description", "supportedInterfaces", "version", "capabilities", "defaultInputModes", "defaultOutputModes", "skills"} {
		if _, ok := card[field]; !ok {
			t.Fatalf("the card lacks required field %s", field)
		}
	}
	if card["version"] != "1.17.0" || card["name"] != "SwarmMemo" || card["description"] != serviceListing.Description {
		t.Fatalf("identity: %v %v %v", card["name"], card["version"], card["description"])
	}
	// No A2A binding exists, so none is claimed, not even by an older field.
	if interfaces, ok := card["supportedInterfaces"].([]any); !ok || len(interfaces) != 0 {
		t.Fatalf("supportedInterfaces must be an empty list: %v", card["supportedInterfaces"])
	}
	for _, field := range []string{"url", "preferredTransport", "additionalInterfaces", "protocolVersion", "securitySchemes"} {
		if _, ok := card[field]; ok {
			t.Fatalf("the card must not claim %s", field)
		}
	}
	capabilities := card["capabilities"].(map[string]any)
	if capabilities["streaming"] != false || capabilities["pushNotifications"] != false {
		t.Fatalf("A2A streaming and push are not offered: %v", capabilities)
	}
	extension := capabilities["extensions"].([]any)[0].(map[string]any)
	params := extension["params"].(map[string]any)
	if extension["required"] != false || params["a2a_binding"] != false || params["http_commands"] != "https://swarmmemo.com/v1/command" || params["mcp"] != "https://swarmmemo.com/mcp" {
		t.Fatalf("the extension must describe the real interface: %v", extension)
	}
	if !strings.HasSuffix(extension["uri"].(string), "/protocol.md#a2a-agent-card") {
		t.Fatalf("extension uri: %v", extension["uri"])
	}
	protocol, err := os.ReadFile("../../docs/PROTOCOL.md")
	if err != nil || !strings.Contains(string(protocol), "\n### A2A agent card\n") {
		t.Fatal("the extension uri must name a real PROTOCOL.md section")
	}
	if card["documentationUrl"] != "https://swarmmemo.com/llms.txt" {
		t.Fatalf("documentationUrl: %v", card["documentationUrl"])
	}
	provider := card["provider"].(map[string]any)
	if provider["organization"] == "" || provider["url"] != "https://swarmmemo.com" {
		t.Fatalf("provider: %v", provider)
	}
	icon := strings.TrimPrefix(card["iconUrl"].(string), "https://swarmmemo.com/assets/")
	if _, err := os.Stat("../web/assets/" + icon); err != nil {
		t.Fatalf("iconUrl names a missing asset: %v", err)
	}
	for _, raw := range card["skills"].([]any) {
		skill := raw.(map[string]any)
		for _, field := range []string{"id", "name", "description"} {
			if value, _ := skill[field].(string); value == "" {
				t.Fatalf("skill without %s: %v", field, skill)
			}
		}
		if tags, _ := skill["tags"].([]any); len(tags) == 0 {
			t.Fatalf("skill without tags: %v", skill)
		}
	}
	if len(f.commands) != 0 {
		t.Fatal("serving the agent card dispatched commands")
	}
	// Discoverable from the documents that list the others.
	if caps := s.capabilities(); caps["a2a_agent_card"] != "/.well-known/agent-card.json" {
		t.Fatal("/capabilities must link the agent card")
	}
	if !strings.Contains(s.instructions(), "/.well-known/agent-card.json") {
		t.Fatal("/llms.txt must link the agent card")
	}
}

func TestAgentCardSkillsFollowOperations(t *testing.T) {
	s := New(&fakeService{}, nil, Config{})
	seen := map[string]bool{}
	skills := s.agentCard()["skills"].([]map[string]any)
	if len(skills) != len(a2aSkills) {
		t.Fatal("every skill spec becomes one card skill")
	}
	for i, spec := range a2aSkills {
		if seen[spec.ID] {
			t.Fatalf("duplicate skill id %s", spec.ID)
		}
		seen[spec.ID] = true
		if len(spec.Operations) == 0 {
			t.Fatalf("skill %s names no operation", spec.ID)
		}
		for _, name := range spec.Operations {
			op, ok := board.LookupOperation(name)
			if !ok {
				t.Fatalf("skill %s names %s, which is not in the operation registry", spec.ID, name)
			}
			if !strings.Contains(skills[i]["description"].(string), name+": "+op.Summary) {
				t.Fatalf("skill %s must quote the registry summary of %s", spec.ID, name)
			}
		}
	}
}

// TestRegistryRecordMatchesListing holds the reviewed MCP Registry record to
// the description the service itself serves, so the two cannot drift apart.
// The public snapshot has no ops/ directory; there it has nothing to compare.
func TestRegistryRecordMatchesListing(t *testing.T) {
	if len(serviceListing.Description) > 100 {
		t.Fatalf("the registry refuses descriptions over 100 characters: %d", len(serviceListing.Description))
	}
	raw, err := os.ReadFile("../../ops/mcp_registry/server.json")
	if errors.Is(err, fs.ErrNotExist) {
		t.Skip("no ops/mcp_registry in this tree")
	}
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	cardJSON, _ := json.Marshal(New(&fakeService{}, nil, Config{PublicURL: "https://swarmmemo.com"}).serverCard())
	var card map[string]any
	_ = json.Unmarshal(cardJSON, &card)
	for _, field := range []string{"name", "title", "description", "websiteUrl", "repository", "icons"} {
		if !reflect.DeepEqual(record[field], card[field]) {
			t.Errorf("server.json %s = %v, the served card says %v", field, record[field], card[field])
		}
	}
	remote := card["remotes"].([]any)[0].(map[string]any)
	if !reflect.DeepEqual(record["remotes"], []any{map[string]any{"type": remote["type"], "url": remote["url"]}}) {
		t.Errorf("server.json remotes = %v, the served card says %v", record["remotes"], card["remotes"])
	}
}
