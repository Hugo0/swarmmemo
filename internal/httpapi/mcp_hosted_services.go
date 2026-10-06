package httpapi

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"swarmmemo/internal/board"
	"swarmmemo/internal/services"
	"swarmmemo/internal/web"
)

// hostedServices are the services a hosted identity calls through tools of
// its own, one per signed method, named SERVICE_METHOD (receiver_create,
// receiver_items): the things a sandboxed assistant cannot do for itself,
// such as a drop box for its callbacks (fetch_page needs no key, so it is
// a public tool like every method that takes an unsigned call). Each
// tool signs as the identity, so it needs a hosted identity like the
// conversation tools.
var hostedServices = []string{services.ReceiverID, services.FetchID}

// hostedServiceTools are those tools for the enabled services in catalog.
func hostedServiceTools(catalog []services.Entry) []serviceTool {
	var out []serviceTool
	for _, e := range catalog {
		if !slices.Contains(hostedServices, e.ID) {
			continue
		}
		for _, m := range e.Methods {
			if (!m.Write() && !m.Signed) || (m.Write() && m.Anonymous) {
				continue // a public tool already: an unsigned read, or a call that needs no key
			}
			desc := e.Title + ": " + m.Line
			if m.Write() {
				desc += " Charged to your identity's credit (" + m.PriceText() + "); max_cost is optional, your ceiling."
			}
			desc += " " + services.UntrustedNote + tokenNote
			out = append(out, serviceTool{spec: mcpToolSpec{web.MCPToolName(e, m), !m.Write(), desc}, entry: e, method: m})
		}
	}
	return out
}

// isHostedServiceTool reports whether name is one of hostedServiceTools for
// any built-in service, enabled or not (sign-in's security schemes).
func isHostedServiceTool(name string) bool {
	for _, t := range hostedServiceTools(services.Catalog(hostedServices)) {
		if t.spec.Name == name {
			return true
		}
	}
	return false
}

// hostedCallSchema is a hosted write's input: its arguments plus an
// optional max_cost.
func hostedCallSchema(m services.MethodEntry) map[string]any {
	schema := argsSchema(m.Args)
	schema["properties"].(map[string]any)[services.CallFieldMaxCost] = map[string]any{"type": "integer", "minimum": 0, "description": "optional: your ceiling in " + m.Resource + "; a higher price is refused and nothing is spent"}
	return schema
}

// addHostedServiceTools registers hostedServiceTools: each signs a
// service.call or service.read as the caller's hosted identity.
func (s *Server) addHostedServiceTools(server *mcp.Server, tool func(string) *mcp.Tool, catalog []services.Entry) {
	for _, st := range hostedServiceTools(catalog) {
		t := tool(st.spec.Name)
		target, m := st.entry.ID, st.method
		if m.Write() {
			t.InputSchema = hostedCallSchema(m)
		} else {
			t.InputSchema = argsSchema(m.Args)
		}
		mcp.AddTool(server, t, func(ctx context.Context, _ *mcp.CallToolRequest, in map[string]any) (*mcp.CallToolResult, board.Result, error) {
			hc, err := s.hostedCaller(ctx)
			if err != nil {
				return nil, board.Result{}, toolError(err)
			}
			args := map[string]any{}
			var maxCost any = m.MaxCost()
			for k, v := range in {
				if k == services.CallFieldMaxCost && m.Write() {
					maxCost = v
					continue
				}
				args[k] = v
			}
			data := map[string]any{"schema": 1, "method": m.Name, "args": args}
			op := "service.read"
			if m.Write() {
				data["max_cost"], op = maxCost, "service.call"
			}
			raw, err := json.Marshal(data)
			if err != nil {
				return nil, board.Result{}, toolError(err)
			}
			res, err := hc.exec(board.Command{Operation: op, Target: target, Data: string(raw)})
			if err != nil {
				return nil, board.Result{}, toolError(err)
			}
			return nil, res, nil
		})
	}
}
