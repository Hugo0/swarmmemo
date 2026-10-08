package httpapi

// One error vocabulary across the wires (C53): the same mistake in a
// service call is the same code and message on /call/, a signed
// service.call, an unsigned one on /v1/command and the hosted MCP tools.
// Real store, real engine.

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

type wireError struct{ status, code, message string }

func (e wireError) String() string { return e.status + " " + e.code + ": " + e.message }

// httpWireError is the error of an HTTP answer; status "200" for none.
func httpWireError(t *testing.T, code int, raw []byte) wireError {
	t.Helper()
	var body struct {
		Error *board.Error `json:"error"`
	}
	_ = json.Unmarshal(raw, &body)
	if body.Error == nil {
		return wireError{status: fmt.Sprint(code), message: string(raw)}
	}
	return wireError{fmt.Sprint(code), body.Error.Code, body.Error.Message}
}

// mcpWireError is a hosted tool's refusal: its structured error, which
// carries the HTTP API's status, code and message.
func mcpWireError(t *testing.T, s *Server, path, name string, args map[string]any) wireError {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	out := mcpRequest(t, s, path, "", string(raw))
	if dig(out, "result", "isError") != true {
		return wireError{status: "200", message: fmt.Sprint(out)}
	}
	e, _ := dig(out, "result", "structuredContent", "error").(map[string]any)
	status, _ := e["status"].(float64)
	code, _ := e["code"].(string)
	message, _ := e["message"].(string)
	return wireError{fmt.Sprint(int(status)), code, message}
}

func TestOneErrorVocabularyAcrossWires(t *testing.T) {
	// notary takes calls without a key, so the network has credit for them.
	s, _ := hostedServicesServer(t, "notary")
	me := newIdentity(t, s, "")
	hosted := "/mcp/t/" + me["token"].(string)
	key := ed25519.NewKeyFromSeed([]byte(strings.Repeat("v", 32)))
	n := 0
	command := func(target, method string, args map[string]any, maxCost int64, sign bool) wireError {
		t.Helper()
		n++
		d := map[string]any{"schema": 1, "method": method, "args": args}
		if maxCost >= 0 {
			d["max_cost"] = maxCost
		}
		data, _ := json.Marshal(d)
		c := board.Command{Operation: "service.call", Target: target, Data: string(data), RequestID: fmt.Sprintf("vocabulary-request-%04d", n)}
		if sign {
			c = signService(key, c)
		}
		body, _ := json.Marshal(c)
		w := makeRequest(s, "POST", "https://swarmmemo.com/v1/command", string(body), "application/json")
		return httpWireError(t, w.Code, w.Body.Bytes())
	}
	call := func(target, method string, fields url.Values) wireError {
		t.Helper()
		w := makeRequest(s, "GET", "https://swarmmemo.com/call/"+target+"/"+method+"?"+fields.Encode(), "", "")
		return httpWireError(t, w.Code, w.Body.Bytes())
	}
	tool := func(name string, args map[string]any) wireError {
		t.Helper()
		return mcpWireError(t, s, "/mcp", name, args)
	}
	paid := "tool:" + paidToolOK
	for _, tc := range []struct {
		name, code string
		wires      map[string]wireError
	}{
		{"unknown service", "invalid_service", map[string]wireError{
			"/call/":   call("nope", "stamp", url.Values{"text": {"x"}}),
			"signed":   command("nope", "stamp", map[string]any{"text": "x"}, 10, true),
			"unsigned": command("nope", "stamp", map[string]any{"text": "x"}, 10, false),
		}},
		{"unknown method", "invalid_service", map[string]wireError{
			"/call/":   call("memory", "nope", url.Values{"key": {"k"}}),
			"signed":   command("memory", "nope", map[string]any{"key": "k"}, 10, true),
			"unsigned": command("memory", "nope", map[string]any{"key": "k"}, 10, false),
		}},
		{"unknown tool", "invalid_service", map[string]wireError{
			"/call/":   call("tools", "call", url.Values{"id": {"swarmmemo:nope.page"}}),
			"signed":   command("tools", "call", map[string]any{"id": "swarmmemo:nope.page"}, 10, true),
			"unsigned": command("tools", "call", map[string]any{"id": "swarmmemo:nope.page"}, 10, false),
			"MCP":      tool("tools_call", map[string]any{"id": "swarmmemo:nope.page"}),
		}},
		{"unknown argument", "invalid_service_data", map[string]wireError{
			"/call/":   call("tools", "call", url.Values{"id": {paid}, "nope": {"1"}, "max_cost": {"30000"}}),
			"signed":   command("tools", "call", map[string]any{"id": paid, "nope": 1}, 30000, true),
			"unsigned": command("tools", "call", map[string]any{"id": paid, "nope": 1}, 30000, false),
			"MCP":      tool("tools_call", map[string]any{"id": paid, "nope": 1, "max_cost": 30000}),
		}},
		{"missing max_cost", "invalid_service_data", map[string]wireError{
			"/call/":   call("tools", "call", url.Values{"id": {paid}}),
			"signed":   command("tools", "call", map[string]any{"id": paid}, -1, true),
			"unsigned": command("tools", "call", map[string]any{"id": paid}, -1, false),
			"MCP":      tool("tools_call", map[string]any{"id": paid}),
			"MCP, a hosted identity's x402_tools_call": mcpWireError(t, s, hosted, "x402_tools_call", map[string]any{"resource": paid}),
		}},
		{"an argument of the wrong type", "invalid_service_data", map[string]wireError{
			"signed":   command("notary", "stamp", map[string]any{"text": 7}, 10, true),
			"unsigned": command("notary", "stamp", map[string]any{"text": 7}, 10, false),
		}},
	} {
		var first wireError
		var firstWire string
		wires := slices.Sorted(maps.Keys(tc.wires))
		for _, wire := range wires {
			got := tc.wires[wire]
			// An MCP refusal carries the code and message; its status is
			// the HTTP API's alone.
			if strings.HasPrefix(wire, "MCP") {
				got.status = "400"
			}
			if got.status != "400" || got.code != tc.code {
				t.Errorf("%s over %s: %s, want 400 %s", tc.name, wire, got, tc.code)
			}
			if firstWire == "" {
				first, firstWire = got, wire
			} else if got != first {
				t.Errorf("%s: %s says %s, %s says %s", tc.name, wire, got, firstWire, first)
			}
		}
		t.Logf("%s: %s", tc.name, first)
	}
}
