package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestMCPToolListAnnotationsWithoutServiceCommands(t *testing.T) {
	service := &fakeService{err: errors.New("tool discovery must not execute service commands")}
	server := httptest.NewServer(New(service, nil, Config{}))
	defer server.Close()
	expected := map[string]bool{
		"post_message":  false,
		"read_messages": true, "read_updates": true, "read_thread": true, "list_pages": true,
		"list_rooms": true, "find_agents": true, "read_agent": true,
		"find_work": true, "read_work": true, "read_work_history": true,
	}
	for attempt := 0; attempt < 2; attempt++ {
		request, err := http.NewRequest(http.MethodPost, server.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		var envelope struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      int             `json:"id"`
			Error   json.RawMessage `json:"error"`
			Result  struct {
				Tools []struct {
					Name        string          `json:"name"`
					Annotations map[string]bool `json:"annotations"`
				} `json:"tools"`
				NextCursor string `json:"nextCursor"`
			} `json:"result"`
		}
		decodeErr := json.NewDecoder(response.Body).Decode(&envelope)
		response.Body.Close()
		if response.StatusCode != http.StatusOK || decodeErr != nil || len(envelope.Error) != 0 || envelope.JSONRPC != "2.0" || envelope.ID != 1 {
			t.Fatalf("tools/list: status=%d decode=%v error=%s", response.StatusCode, decodeErr, envelope.Error)
		}
		if len(envelope.Result.Tools) != len(expected) || envelope.Result.NextCursor != "" {
			t.Fatalf("unexpected tool count/pagination: %+v", envelope.Result)
		}
		seen := map[string]bool{}
		for _, tool := range envelope.Result.Tools {
			readOnly, known := expected[tool.Name]
			if !known || seen[tool.Name] {
				t.Fatalf("unexpected or duplicated tool %q", tool.Name)
			}
			seen[tool.Name] = true
			// Check the actual wire keys, including explicit false values. Missing
			// destructiveHint defaults to true and is not equivalent to false.
			want := map[string]bool{
				"readOnlyHint": readOnly, "idempotentHint": readOnly,
				"destructiveHint": false, "openWorldHint": true,
			}
			if !reflect.DeepEqual(tool.Annotations, want) {
				t.Errorf("%s annotations = %#v; want %#v", tool.Name, tool.Annotations, want)
			}
		}
		service.mu.Lock()
		calls := len(service.commands)
		service.mu.Unlock()
		if calls != 0 {
			t.Fatalf("listing tools executed %d service commands", calls)
		}
	}
}

// The server card lists exactly the tools the endpoint registers, with the
// same descriptions and read-only flags: both are built from mcpTools.
func TestMCPServerCardMatchesRegisteredTools(t *testing.T) {
	s := New(&fakeService{}, nil, Config{PublicURL: "https://swarmmemo.com"})
	server := httptest.NewServer(s)
	defer server.Close()
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var envelope struct {
		Result struct {
			Tools []struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Annotations map[string]bool `json:"annotations"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	registered := map[string]string{}
	for _, tool := range envelope.Result.Tools {
		registered[tool.Name] = tool.Description + " readOnly=" + strconv.FormatBool(tool.Annotations["readOnlyHint"])
	}
	card := map[string]string{}
	for _, tool := range s.serverCard()["tools"].([]map[string]any) {
		card[tool["name"].(string)] = tool["description"].(string) + " readOnly=" + strconv.FormatBool(tool["readOnly"].(bool))
	}
	if !reflect.DeepEqual(registered, card) || len(card) != len(mcpTools) {
		t.Fatalf("server card and registered MCP tools differ:\n card %v\n tools %v", card, registered)
	}
}

// A connecting MCP client is handed the same quickstart as /llms.txt.
func TestMCPInstructionsAreTheQuickstart(t *testing.T) {
	s := New(&fakeService{}, nil, Config{PublicURL: "https://swarmmemo.com"})
	server := httptest.NewServer(s)
	defer server.Close()
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var envelope struct {
		Result struct {
			Instructions string `json:"instructions"`
		} `json:"result"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(envelope.Result.Instructions, quickstartText("https://swarmmemo.com")) {
		t.Fatalf("MCP instructions are not the quickstart: %.200q", envelope.Result.Instructions)
	}
	if !strings.Contains(makeRequest(s, "GET", "/llms.txt", "", "").Body.String(), quickstartText("https://swarmmemo.com")) {
		t.Fatal("/llms.txt does not carry the quickstart verbatim")
	}
}
