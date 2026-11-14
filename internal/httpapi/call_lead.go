package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A tool call's answer leads with what the tool returned (C977): an agent
// reading a /call/ answer or an MCP tool's JSON met the call record and its
// billing first, and the fetched page last. Only the ORDER of fields
// changes, never a name or a value, so a strict client reads the same
// object: data.result comes first in data, and in that result the fields
// that are the payload (callResultLead) come first.

// callResultLead are the fields of a call's result that lead it, in order.
var callResultLead = []string{"title", "text"}

// jsonField is one member of a JSON object, its value as written.
type jsonField struct {
	key string
	raw json.RawMessage
}

// objectFields reads raw as one JSON object's members in their order, or
// false when raw is not one object.
func objectFields(raw []byte) ([]jsonField, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil {
		return nil, false
	} else if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, false
	}
	var out []jsonField
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key, _ := tok.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, false
		}
		out = append(out, jsonField{key, v})
	}
	if _, err := dec.Token(); err != nil {
		return nil, false
	}
	if _, err := dec.Token(); err == nil {
		return nil, false // something after the object
	}
	return out, true
}

// writeObject writes fields as one compact JSON object, values as written.
func writeObject(fields []jsonField) []byte {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, f := range fields {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(f.key) // keys are plain names; quoting only
		b.Write(k)
		b.WriteByte(':')
		b.Write(f.raw)
	}
	b.WriteByte('}')
	return b.Bytes()
}

// leadWith moves the named fields, in that order, before the rest, which
// keep their order.
func leadWith(fields []jsonField, keys ...string) []jsonField {
	out := make([]jsonField, 0, len(fields))
	for _, k := range keys {
		for _, f := range fields {
			if f.key == k {
				out = append(out, f)
			}
		}
	}
	for _, f := range fields {
		lead := false
		for _, k := range keys {
			lead = lead || f.key == k
		}
		if !lead {
			out = append(out, f)
		}
	}
	return out
}

// leadCallAnswer is a result object (ok, data, ...) whose data is a
// service call's (call and result) with that result first in data and the
// result's payload first in it; false when raw is no such answer.
func leadCallAnswer(raw []byte) ([]byte, bool) {
	raw = bytes.TrimSpace(raw)
	top, ok := objectFields(raw)
	if !ok {
		return nil, false
	}
	for i, f := range top {
		if f.key != "data" {
			continue
		}
		data, ok := objectFields(f.raw)
		if !ok {
			return nil, false
		}
		hasCall, ri := false, -1
		for j, d := range data {
			hasCall = hasCall || d.key == "call"
			if d.key == "result" {
				ri = j
			}
		}
		if !hasCall || ri < 0 {
			return nil, false
		}
		if res, ok := objectFields(data[ri].raw); ok {
			data[ri].raw = writeObject(leadWith(res, callResultLead...))
		}
		top[i].raw = writeObject(leadWith(data, "result"))
		return writeObject(top), true
	}
	return nil, false
}

// callAnswerResponse is jsonResponse for a /call/ route's answer, led by its
// result (leadCallAnswer) when it is a service call's.
func callAnswerResponse(w http.ResponseWriter, status int, v any) {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		jsonResponse(w, status, v)
		return
	}
	out := b.Bytes()
	if lead, ok := leadCallAnswer(out); ok {
		out = append(lead, '\n')
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(out)
}

// leadCallResults orders every MCP tool answer that is a service call's as
// leadCallAnswer does, in its structured content and the JSON text beside
// it, so an MCP client reads the result first as a /call/ reader does.
func leadCallResults() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			r, ok := res.(*mcp.CallToolResult)
			if !ok || r == nil || err != nil || r.IsError {
				return res, err
			}
			if r.StructuredContent != nil {
				raw, isRaw := r.StructuredContent.(json.RawMessage)
				if !isRaw {
					raw, _ = json.Marshal(r.StructuredContent)
				}
				if out, ok := leadCallAnswer(raw); ok {
					r.StructuredContent = json.RawMessage(out)
				}
			}
			for _, c := range r.Content {
				if t, ok := c.(*mcp.TextContent); ok {
					if out, ok := leadCallAnswer([]byte(t.Text)); ok {
						t.Text = string(out)
					}
				}
			}
			return r, nil
		}
	}
}
