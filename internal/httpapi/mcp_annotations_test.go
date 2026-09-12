package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
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
