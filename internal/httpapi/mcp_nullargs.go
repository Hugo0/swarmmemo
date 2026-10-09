package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Optional null arguments (C125): MCP clients built on mcpadapt (crewai-tools'
// MCPServerAdapter and others) send JSON null for every optional argument the
// caller left unset. The SDK validates arguments against the tool's input
// schema, where an optional string is "string", so every such call was
// refused. dropNullOptionalArgs treats a top-level null as absent when the
// property is optional and its schema does not admit null; a required
// property's null still reaches validation and is refused naming the field,
// and a property whose type admits null (["null","array"]) or that has no
// type keeps its null. The advertised schemas are unchanged.

// nullArgRule is what a tool's input schema says about top-level nulls.
type nullArgRule struct {
	required map[string]bool
	nullable map[string]bool // type admits null, or no type at all
}

// dropNullOptionalArgs must be the innermost receiving middleware: it reads
// the schemas from the SDK's own tools/list, which then lists every
// registered tool, hidden aliases included.
func dropNullOptionalArgs() mcp.Middleware {
	var (
		once  sync.Once
		rules map[string]nullArgRule
	)
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			call, ok := req.(*mcp.CallToolRequest)
			if !ok || call.Params == nil || call.Session == nil || !bytes.Contains(call.Params.Arguments, []byte("null")) {
				return next(ctx, method, req)
			}
			var args map[string]json.RawMessage
			if json.Unmarshal(call.Params.Arguments, &args) != nil {
				return next(ctx, method, req)
			}
			var nulls []string
			for name, v := range args {
				if string(bytes.TrimSpace(v)) == "null" {
					nulls = append(nulls, name)
				}
			}
			if len(nulls) == 0 {
				return next(ctx, method, req)
			}
			once.Do(func() { rules = toolNullRules(ctx, next, call.Session) })
			rule, known := rules[call.Params.Name]
			if !known {
				return next(ctx, method, req)
			}
			dropped := false
			for _, name := range nulls {
				if !rule.required[name] && !rule.nullable[name] {
					delete(args, name)
					dropped = true
				}
			}
			if dropped {
				raw, err := json.Marshal(args)
				if err != nil {
					return next(ctx, method, req)
				}
				params := *call.Params
				params.Arguments = raw
				copied := *call
				copied.Params = &params
				req = &copied
			}
			return next(ctx, method, req)
		}
	}
}

// toolNullRules reads every registered tool's input schema from the SDK's
// tools/list (next is the SDK's own handler), following its pages.
func toolNullRules(ctx context.Context, next mcp.MethodHandler, session *mcp.ServerSession) map[string]nullArgRule {
	rules := map[string]nullArgRule{}
	cursor := ""
	for range 100 {
		res, err := next(ctx, "tools/list", &mcp.ListToolsRequest{Session: session, Params: &mcp.ListToolsParams{Cursor: cursor}})
		list, ok := res.(*mcp.ListToolsResult)
		if err != nil || !ok {
			return rules
		}
		for _, t := range list.Tools {
			rules[t.Name] = nullRuleOf(t.InputSchema)
		}
		if list.NextCursor == "" {
			break
		}
		cursor = list.NextCursor
	}
	return rules
}

// nullRuleOf reads the top-level required list and each property's type from
// a schema of any form the SDK holds (*jsonschema.Schema or a map).
func nullRuleOf(schema any) nullArgRule {
	rule := nullArgRule{required: map[string]bool{}, nullable: map[string]bool{}}
	raw, err := json.Marshal(schema)
	if err != nil {
		return rule
	}
	var s struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if json.Unmarshal(raw, &s) != nil {
		return rule
	}
	for _, name := range s.Required {
		rule.required[name] = true
	}
	for name, p := range s.Properties {
		var prop struct {
			Type json.RawMessage `json:"type"`
		}
		_ = json.Unmarshal(p, &prop)
		var one string
		var many []string
		switch {
		case len(prop.Type) == 0:
			rule.nullable[name] = true // no type: null is valid as it is
		case json.Unmarshal(prop.Type, &one) == nil:
			rule.nullable[name] = one == "null"
		case json.Unmarshal(prop.Type, &many) == nil:
			rule.nullable[name] = slices.Contains(many, "null")
		}
	}
	return rule
}
