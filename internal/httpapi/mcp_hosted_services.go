package httpapi

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"swarmmemo/internal/board"
	"swarmmemo/internal/services"
	"swarmmemo/internal/web"
)

// hostedServices are the services a hosted identity calls through tools of
// its own, one per signed method, named SERVICE_METHOD (receiver_create,
// memory_put, wakeup_schedule): the things a sandboxed assistant cannot do
// for itself, such as a drop box for its callbacks, notes that outlive the
// conversation, a wake-up or a paid tool on its own allowance (fetch_page
// needs no key, so it is a public tool like every method that takes an
// unsigned call). Each tool signs the same service.call or service.read a
// local key would, so the engine's vetting, caps, prices and receipts are
// the signed path's. It needs a hosted identity like the conversation
// tools.
var hostedServices = []string{services.ReceiverID, services.FetchID, "memory", "wakeup", "x402"}

// hostedShape is a hosted tool whose name, line or arguments differ from
// its method's: x402's call, as a hosted identity makes it, calls SwarmMemo
// tools by their tool: id, and its ceiling is never left to a default.
type hostedShape struct {
	name, line string
	args       []services.Arg
	// prefix is what the resource argument must start with.
	prefix string
	// maxCostRequired makes max_cost a required argument.
	maxCostRequired bool
}

// hostedShapes are keyed SERVICE.METHOD.
var hostedShapes = map[string]hostedShape{
	"x402.call": {
		name: "x402_tools_call",
		line: "Call a SwarmMemo tool by its " + services.FramesPrefix + " id (x402_tools_search finds one, x402_tools_get reads its live price and input schema), with the tool's arguments as body. Only vetted tools are callable; an unvetted one is refused and nothing is charged. data.call is the receipt: cost is what was charged, and data.result.payment what SwarmMemo paid.",
		args: []services.Arg{
			{Name: "resource", Type: "string", Required: true, Note: services.FramesPrefix + "TOOL_ID from x402_tools_search"},
			{Name: "body", Type: "object", Note: "the tool's arguments, as its input schema states"},
		},
		prefix: services.FramesPrefix, maxCostRequired: true,
	},
}

// hostedServiceEffects are the hosted service tools that replace or remove
// what the identity stored (destructive); every other one only adds, reads
// or spends.
var hostedServiceEffects = map[string]bool{"receiver_rotate": true, "receiver_delete": true, "memory_put": true, "memory_delete": true, "wakeup_cancel": true}

// hostedServiceClosedWorld are the services whose hosted tools reach
// nothing outside SwarmMemo's own store.
var hostedServiceClosedWorld = []string{"memory", "wakeup"}

// hostedSignedReads are public reads that a hosted caller makes signed, so
// its own private items answer too (keyed SERVICE.METHOD).
var hostedSignedReads = map[string]bool{"memory.get": true, "memory.list": true}

// hostedOwnReadNote is what a hostedSignedReads tool adds while hosted
// identities are on.
const hostedOwnReadNote = " With a hosted identity it is signed as you, so your own private items answer too (leave agent out)."

// hostedServiceTool is m's hosted tool and its shape; ok is false for a
// method that is a public tool already: an unsigned read, or a call that
// needs no key.
func hostedServiceTool(e services.Entry, m services.MethodEntry) (serviceTool, hostedShape, bool) {
	shape, shaped := hostedShapes[e.ID+"."+m.Name]
	if !shaped && ((!m.Write() && !m.Signed) || (m.Write() && m.Anonymous)) {
		return serviceTool{}, shape, false
	}
	if shape.name == "" {
		shape.name = web.MCPToolName(e, m)
	}
	if shape.line == "" {
		shape.line = m.Line
	}
	if shape.args == nil {
		shape.args = m.Args
	}
	desc := e.Title + ": " + shape.line
	if m.Write() {
		desc += " Charged to your identity's credit (" + m.PriceText() + "); "
		if shape.maxCostRequired {
			desc += "max_cost is required, your ceiling: a higher price is refused and nothing is spent."
		} else {
			desc += "max_cost is optional, your ceiling."
		}
	}
	desc += " " + services.UntrustedNote + tokenNote
	return serviceTool{spec: mcpToolSpec{shape.name, !m.Write(), desc}, entry: e, method: m}, shape, true
}

// hostedServiceTools are those tools for the enabled services in catalog.
func hostedServiceTools(catalog []services.Entry) []serviceTool {
	var out []serviceTool
	for _, e := range catalog {
		if !slices.Contains(hostedServices, e.ID) {
			continue
		}
		for _, m := range e.Methods {
			if t, _, ok := hostedServiceTool(e, m); ok {
				out = append(out, t)
			}
		}
	}
	return out
}

// isHostedServiceTool reports whether name is one of hostedServiceTools for
// any built-in service, enabled or not (sign-in's security schemes).
func isHostedServiceTool(name string) bool {
	_, _, ok := hostedServiceHints(name)
	return ok
}

// isHostedSignedRead reports whether name is a public tool that signs as a
// hosted caller (hostedSignedReads), so it takes sign-in or none.
func isHostedSignedRead(name string) bool {
	for _, e := range services.Catalog(hostedServices) {
		for _, m := range e.Methods {
			if hostedSignedReads[e.ID+"."+m.Name] && web.MCPToolName(e, m) == name {
				return true
			}
		}
	}
	return false
}

// hostedServiceHints are a hosted service tool's destructive and closed-world
// hints; ok is false for any other tool.
func hostedServiceHints(name string) (destructive, closedWorld, ok bool) {
	for _, t := range hostedServiceTools(services.Catalog(hostedServices)) {
		if t.spec.Name == name {
			return hostedServiceEffects[name], slices.Contains(hostedServiceClosedWorld, t.entry.ID), true
		}
	}
	return false, false, false
}

// hostedCallSchema is a hosted write's input: its arguments plus max_cost,
// optional unless the shape requires it.
func hostedCallSchema(m services.MethodEntry, shape hostedShape) map[string]any {
	schema := argsSchema(shape.args)
	note := "optional: your ceiling in " + m.Resource + "; a higher price is refused and nothing is spent. Left out, the quote for the arguments is the ceiling"
	if shape.maxCostRequired {
		note = "required: your ceiling in " + m.Resource + "; a higher price is refused and nothing is spent"
		required, _ := schema["required"].([]string)
		schema["required"] = append(required, services.CallFieldMaxCost)
	}
	schema["properties"].(map[string]any)[services.CallFieldMaxCost] = map[string]any{"type": "integer", "minimum": 0, "description": note}
	return schema
}

// hostedCallData is the data of the service.call or service.read a hosted
// tool signs: its input less max_cost is the args object.
func hostedCallData(m services.MethodEntry, shape hostedShape, in map[string]any) (string, error) {
	args := map[string]any{}
	// Left out, the quote for the arguments is the ceiling, as on the
	// keyless call tools: a recurring wake-up costs one credit a firing.
	var maxCost any
	if !shape.maxCostRequired {
		maxCost = services.CallDefaultMaxCost
	}
	for k, v := range in {
		if k == services.CallFieldMaxCost && m.Write() {
			maxCost = v
			continue
		}
		args[k] = v
	}
	if r, _ := args["resource"].(string); shape.prefix != "" && !strings.HasPrefix(r, shape.prefix) {
		return "", bad("resource must be a " + shape.prefix + " id from x402_tools_search.")
	}
	data := map[string]any{"schema": 1, "method": m.Name, "args": args}
	if m.Write() {
		if maxCost == nil {
			return "", bad("max_cost is required: the most this call may cost, in " + m.Resource + ".")
		}
		data["max_cost"] = maxCost
	}
	raw, err := json.Marshal(data)
	return string(raw), err
}

// addHostedServiceTools registers hostedServiceTools: each signs a
// service.call or service.read as the caller's hosted identity.
func (s *Server) addHostedServiceTools(server *mcp.Server, tool func(string) *mcp.Tool, catalog []services.Entry) {
	for _, e := range catalog {
		if !slices.Contains(hostedServices, e.ID) {
			continue
		}
		for _, m := range e.Methods {
			st, shape, ok := hostedServiceTool(e, m)
			if !ok {
				continue
			}
			t := tool(st.spec.Name)
			target := e.ID
			if m.Write() {
				t.InputSchema = hostedCallSchema(m, shape)
			} else {
				t.InputSchema = argsSchema(shape.args)
			}
			mcp.AddTool(server, t, func(ctx context.Context, _ *mcp.CallToolRequest, in map[string]any) (*mcp.CallToolResult, board.Result, error) {
				hc, err := s.hostedCaller(ctx)
				if err != nil {
					return nil, board.Result{}, toolError(err)
				}
				data, err := hostedCallData(m, shape, in)
				if err != nil {
					return nil, board.Result{}, toolError(err)
				}
				op := "service.read"
				if m.Write() {
					op = "service.call"
				}
				return hostedRun(hc, board.Command{Operation: op, Target: target, Data: data})
			})
		}
	}
}

// hostedSignedRead is a hostedSignedReads tool called with a hosted token:
// the same service.read, signed as the identity.
func (s *Server) hostedSignedRead(ctx context.Context, c board.Command) (*mcp.CallToolResult, board.Result, error) {
	hc, err := s.hostedCaller(ctx)
	if err != nil {
		return nil, board.Result{}, toolError(err)
	}
	return hostedRun(hc, c)
}

// hostedRun signs c as hc's identity and runs it.
func hostedRun(hc *hostedCaller, c board.Command) (*mcp.CallToolResult, board.Result, error) {
	res, err := hc.exec(c)
	if err != nil {
		return nil, board.Result{}, toolError(err)
	}
	return nil, res, nil
}

// hostedServicesNamed names the services of catalog that have hosted
// service tools, by title: "Memory, Wake-ups and x402 relay".
func hostedServicesNamed(catalog []services.Entry) string {
	var titles []string
	for _, t := range hostedServiceTools(catalog) {
		if !slices.Contains(titles, t.entry.Title) {
			titles = append(titles, t.entry.Title)
		}
	}
	if len(titles) < 2 {
		return strings.Join(titles, "")
	}
	return strings.Join(titles[:len(titles)-1], ", ") + " and " + titles[len(titles)-1]
}

// hostedServicesLine is what the instructions say about the memory and
// wake-up tools of a hosted identity, for those of catalog's services that
// are on; empty when neither is.
func hostedServicesLine(catalog []services.Entry) string {
	var parts []string
	on := func(id string) bool {
		return slices.ContainsFunc(catalog, func(e services.Entry) bool { return e.ID == id })
	}
	if on("memory") {
		parts = append(parts, "memory_put keeps a note of your own (private unless you say public) and memory_get reads it back")
	}
	if on("wakeup") {
		parts = append(parts, "wakeup_schedule sets a wake-up (once, recurring, or on a reply) that arrives in read_updates, wakeup_list shows them and wakeup_cancel stops one")
	}
	if len(parts) == 0 {
		return ""
	}
	return "With a hosted identity, " + strings.Join(parts, "; ") + "."
}
