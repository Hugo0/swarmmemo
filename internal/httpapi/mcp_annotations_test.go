package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

func TestMCPToolListAnnotationsWithoutServiceCommands(t *testing.T) {
	service := &fakeService{err: errors.New("tool discovery must not execute service commands")}
	server := httptest.NewServer(New(service, nil, Config{}))
	defer server.Close()
	expected := map[string]bool{
		"post_message":  false,
		"read_messages": true, "read_feed": true, "read_updates": true, "read_thread": true, "list_pages": true,
		"list_rooms": true, "find_agents": true, "read_agent": true, "read_agent_posts": true,
		"find_work": true, "read_work": true, "read_work_history": true,
		"log_proof": true, "agent_record": true,
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

// A plain GET of either MCP endpoint (a browser, a web tool, an agent
// following the docs link) explains how to connect, and the curl line it
// publishes works verbatim. A GET asking for an event stream still gets the
// transport's answer.
func TestMCPGetExplainsHowToConnect(t *testing.T) {
	s := New(&fakeService{}, nil, Config{PublicURL: "https://swarmmemo.com"})
	curlRE := regexp.MustCompile(`^curl -s (\S+) -H '([^']*)' -H '([^']*)' -d '([^']*)'$`)
	for _, path := range []string{"/mcp", "/mcp/assistant"} {
		get := httptest.NewRequest(http.MethodGet, "https://swarmmemo.com"+path, nil)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, get)
		if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
			t.Fatalf("GET %s = %d %q", path, w.Code, w.Header().Get("Content-Type"))
		}
		var curl []string
		for _, line := range strings.Split(w.Body.String(), "\n") {
			if m := curlRE.FindStringSubmatch(line); m != nil {
				curl = m
			}
		}
		if curl == nil || curl[1] != "https://swarmmemo.com"+path {
			t.Fatalf("GET %s has no runnable curl for itself:\n%s", path, w.Body.String())
		}
		post := httptest.NewRequest(http.MethodPost, curl[1], strings.NewReader(curl[4]))
		for _, h := range curl[2:4] {
			name, value, _ := strings.Cut(h, ": ")
			post.Header.Set(name, value)
		}
		w = httptest.NewRecorder()
		s.ServeHTTP(w, post)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"tools"`) {
			t.Fatalf("the published curl for %s = %d %s", path, w.Code, w.Body.String())
		}
		stream := httptest.NewRequest(http.MethodGet, "https://swarmmemo.com"+path, nil)
		stream.Header.Set("Accept", "text/event-stream")
		w = httptest.NewRecorder()
		s.ServeHTTP(w, stream)
		if w.Code == http.StatusOK && strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
			t.Fatalf("a GET asking for a stream on %s got the note", path)
		}
	}
}

// A tool's refusal carries the HTTP API's error body as its structured
// content, {"ok":false,"error":{code,message}}, with the code and message
// HTTP answers for the same request, beside the text a model reads.
func TestMCPToolErrorsAreStructured(t *testing.T) {
	_, s := endorsementServer(t, board.Features{})
	text := strings.Repeat("x", 20000)
	missing := strings.Repeat("a", 32)
	for _, c := range []struct {
		tool     string
		args     map[string]any
		command  string
		code     string
		mentions string
	}{
		{"read_thread", map[string]any{"message_id": missing}, `{"operation":"thread.get","message_id":"` + missing + `"}`, "not_found", "not found"},
		{"post_message", map[string]any{"text": text}, `{"operation":"post","text":"` + text + `"}`, "text_too_large", "(20000/16384 bytes)"},
	} {
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": c.tool, "arguments": c.args}})
		out := mcpRequest(t, s, "/mcp", "", string(raw))
		w := makeRequest(s, "POST", "/v1/command", c.command, "application/json")
		var body map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		code, message := dig(out, "result", "structuredContent", "error", "code"), dig(out, "result", "structuredContent", "error", "message")
		content, _ := dig(out, "result", "content").([]any)
		if dig(out, "result", "isError") != true || dig(out, "result", "structuredContent", "ok") != false || code != c.code || len(content) == 0 {
			t.Fatalf("%s: %v", c.tool, out)
		}
		if code != dig(body, "error", "code") || message != dig(body, "error", "message") || !strings.Contains(strings.ToLower(fmt.Sprint(message)), c.mentions) {
			t.Fatalf("%s: MCP %v %q, HTTP %d %v", c.tool, code, message, w.Code, body)
		}
		if content[0].(map[string]any)["text"] != message {
			t.Fatalf("%s: the text a model reads: %v", c.tool, content[0])
		}
	}
	// The SDK refusing the arguments is invalid_request, in the same shape.
	out := mcpRequest(t, s, "/mcp", "", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read_thread","arguments":{"message_id":7}}}`)
	if dig(out, "result", "isError") != true || dig(out, "result", "structuredContent", "error", "code") != "invalid_request" {
		t.Fatalf("argument refusal: %v", out)
	}
}
