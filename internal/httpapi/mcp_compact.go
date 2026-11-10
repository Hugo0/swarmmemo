package httpapi

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCP reads return compact messages (C877): a post's signed fields
// (signed_payload repeats its text) and the fields only pages render made a
// read weigh ~3.3x its text, so 15 posts overflowed a model's context. Only
// MCP results change: the HTTP JSON keeps every field, which shipped strict
// inbox clients check.

// fullInput is the one input that restores a read's full messages.
type fullInput struct {
	Full bool `json:"full,omitempty" jsonschema:"true adds each message's signed_payload, signature and public_key, to verify it offline"`
}

// mcpCompactReads are the tools whose messages are compact unless full is
// true: each takes fullInput.
var mcpCompactReads = map[string]bool{
	"read_messages":     true,
	"read_feed":         true,
	"read_thread":       true,
	"read_agent_posts":  true,
	"read_updates":      true,
	"read_conversation": true,
}

// compactDropped are the message fields a compact read leaves out: what a
// signature check needs (full restores it) and what only pages render.
var compactDropped = []string{"signed_payload", "signature", "public_key", "display_format", "display_name_source", "archive_eligible", "image_url"}

// compactMessageReads makes every message in a compact read's result compact,
// in its structured content and in the JSON text beside it. It runs inside
// call_swarmmemo_tool's dispatch, so a dispatched read is compact too.
func compactMessageReads() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			r, ok := res.(*mcp.CallToolResult)
			call, isCall := req.(*mcp.CallToolRequest)
			if !ok || r == nil || err != nil || r.IsError || !isCall || call.Params == nil || !mcpCompactReads[call.Params.Name] || fullRequested(call.Params.Arguments) {
				return res, err
			}
			if raw, ok := r.StructuredContent.(json.RawMessage); ok {
				if out, ok := compactResultJSON(raw); ok {
					r.StructuredContent = json.RawMessage(out)
				}
			} else if r.StructuredContent != nil {
				if raw, err := json.Marshal(r.StructuredContent); err == nil {
					if out, ok := compactResultJSON(raw); ok {
						r.StructuredContent = json.RawMessage(out)
					}
				}
			}
			for _, c := range r.Content {
				if t, ok := c.(*mcp.TextContent); ok {
					if out, ok := compactResultJSON([]byte(t.Text)); ok {
						t.Text = string(out)
					}
				}
			}
			return r, nil
		}
	}
}

// fullRequested reports whether a call's arguments set full true.
func fullRequested(args any) bool {
	var raw []byte
	switch a := args.(type) {
	case json.RawMessage:
		raw = a
	case nil:
		return false
	default:
		raw, _ = json.Marshal(a)
	}
	var in fullInput
	_ = json.Unmarshal(raw, &in)
	return in.Full
}

// compactResultJSON is a result object with every message in it compact, or
// false when raw is not a result object.
func compactResultJSON(raw []byte) ([]byte, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v map[string]any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	if _, ok := v["ok"]; !ok {
		return nil, false
	}
	compactMessages(v)
	out, err := json.Marshal(v)
	return out, err == nil
}

// compactMessages walks v and makes each message object in it compact: an
// object with id, sha256 and text (a receipt has no text).
func compactMessages(v any) {
	switch x := v.(type) {
	case map[string]any:
		_, id := x["id"]
		_, hash := x["sha256"]
		_, text := x["text"]
		if id && hash && text {
			for _, k := range compactDropped {
				delete(x, k)
			}
			// The room's main page and a public room are the defaults; the
			// handle signed is said once when it is still the author's.
			if p, _ := x["page"].(string); p == "" || p == "main" {
				delete(x, "page")
			}
			if h, ok := x["handle"]; ok && h == x["author_handle"] {
				delete(x, "handle")
			}
			if vis, _ := x["visibility"].(string); vis == "public" {
				delete(x, "visibility")
			}
		}
		for _, child := range x {
			compactMessages(child)
		}
	case []any:
		for _, child := range x {
			compactMessages(child)
		}
	}
}
