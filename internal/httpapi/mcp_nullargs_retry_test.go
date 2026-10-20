package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// An early transient tools/list failure must not permanently disable null
// normalization for later MCP requests and sessions.
func TestMCPNullDiscoveryRetriesAfterTransientListError(t *testing.T) {
	listCalls := 0
	next := func(_ context.Context, method string, req mcp.Request) (mcp.Result, error) {
		switch method {
		case "tools/list":
			listCalls++
			if listCalls == 1 {
				return nil, errors.New("transient tools/list startup failure")
			}
			return &mcp.ListToolsResult{Tools: []*mcp.Tool{{
				Name: "demo",
				InputSchema: map[string]any{
					"type":     "object",
					"required": []string{"required"},
					"properties": map[string]any{
						"required": map[string]any{"type": "string"},
						"optional": map[string]any{"type": "string"},
					},
				},
			}}}, nil
		case "tools/call":
			call := req.(*mcp.CallToolRequest)
			var args map[string]json.RawMessage
			if err := json.Unmarshal(call.Params.Arguments, &args); err != nil {
				return nil, err
			}
			if string(args["optional"]) == "null" {
				return nil, errors.New("SDK refused optional string value null")
			}
			return &mcp.CallToolResult{}, nil
		default:
			t.Fatalf("unexpected method %s", method)
			return nil, nil
		}
	}
	wrapped := dropNullOptionalArgs()(next)
	newCall := func() *mcp.CallToolRequest {
		return &mcp.CallToolRequest{
			Session: &mcp.ServerSession{},
			Params: &mcp.CallToolParamsRaw{
				Name:      "demo",
				Arguments: json.RawMessage(`{"required":"ok","optional":null}`),
			},
		}
	}
	if _, err := wrapped(t.Context(), "tools/call", newCall()); err == nil {
		t.Fatal("first call unexpectedly accepted without any tool schema")
	}
	if _, err := wrapped(t.Context(), "tools/call", newCall()); err != nil {
		t.Fatalf("transient startup failure permanently poisoned optional-null middleware: %v", err)
	}
	if listCalls != 2 {
		t.Fatalf("expected schema discovery to recover on second call, got %d tools/list attempts", listCalls)
	}
	if _, err := wrapped(t.Context(), "tools/call", newCall()); err != nil {
		t.Fatal(err)
	}
	if listCalls != 2 {
		t.Fatalf("complete tool schema should be cached, got %d list attempts", listCalls)
	}
}

// tools/list can fail on a later page after returning valid schema entries.
// Those partial entries must not be cached, or tools absent from the first
// page become permanently unusable for clients sending optional null fields.
func TestMCPNullDiscoveryRetriesAfterLaterPageFailure(t *testing.T) {
	listCalls := 0
	schema := func(name string) *mcp.Tool {
		return &mcp.Tool{
			Name: name,
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"optional": map[string]any{"type": "string"},
				},
			},
		}
	}
	next := func(_ context.Context, method string, req mcp.Request) (mcp.Result, error) {
		switch method {
		case "tools/list":
			listCalls++
			params := req.(*mcp.ListToolsRequest).Params
			switch params.Cursor {
			case "":
				return &mcp.ListToolsResult{
					Tools:      []*mcp.Tool{schema("first")},
					NextCursor: "page-two",
				}, nil
			case "page-two":
				if listCalls == 2 {
					return nil, errors.New("temporary second-page failure")
				}
				return &mcp.ListToolsResult{
					Tools: []*mcp.Tool{schema("second")},
				}, nil
			default:
				t.Fatalf("unexpected cursor %q", params.Cursor)
			}
		case "tools/call":
			call := req.(*mcp.CallToolRequest)
			var args map[string]json.RawMessage
			if err := json.Unmarshal(call.Params.Arguments, &args); err != nil {
				return nil, err
			}
			if string(args["optional"]) == "null" {
				return nil, errors.New("SDK refused optional null on second-page tool")
			}
			return &mcp.CallToolResult{}, nil
		}
		return nil, errors.New("unexpected method")
	}
	wrapped := dropNullOptionalArgs()(next)
	call := func() *mcp.CallToolRequest {
		return &mcp.CallToolRequest{
			Session: &mcp.ServerSession{},
			Params: &mcp.CallToolParamsRaw{
				Name:      "second",
				Arguments: json.RawMessage(`{"optional":null}`),
			},
		}
	}
	if _, err := wrapped(t.Context(), "tools/call", call()); err == nil {
		t.Fatal("first incomplete discovery unexpectedly accepted")
	}
	if _, err := wrapped(t.Context(), "tools/call", call()); err != nil {
		t.Fatalf("second-page retry never recovered: %v", err)
	}
	if listCalls != 4 {
		t.Fatalf("expected 2 attempts of 2 pages each, got %d calls", listCalls)
	}
	if _, err := wrapped(t.Context(), "tools/call", call()); err != nil {
		t.Fatal(err)
	}
	if listCalls != 4 {
		t.Fatalf("complete list should be cached after recovery, got %d calls", listCalls)
	}
}
