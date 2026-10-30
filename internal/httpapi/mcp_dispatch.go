package httpapi

// The toolsets' middleware and meta pair (mcp_toolsets.go): per request,
// tools/list lists the request's toolsets, initialize gives the
// instructions of a filtered list, search_swarmmemo_tools answers from the
// profile's own tool definitions, and call_swarmmemo_tool rewrites the call
// to the tool it names before the rest of the chain sees it, so the SDK's
// schema validation, sign-in (oauthToolMeta, the hosted token), the
// structured refusals, the limits and the real handler all apply as if the
// client had called that tool. It never reaches a tool its profile does not
// register and list, never one already listed, and never itself.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"swarmmemo/internal/board"
)

type toolsetSelectionKey struct{}
type toolsetViewKey struct{}
type dispatchNoteKey struct{}

// withToolsetSelection is ctx with the selection the URL named (or the
// default MCP_TOOLSETS gives it).
func withToolsetSelection(ctx context.Context, sel *toolsetSelection) context.Context {
	return context.WithValue(ctx, toolsetSelectionKey{}, sel)
}

func toolsetSelectionFrom(ctx context.Context) *toolsetSelection {
	sel, _ := ctx.Value(toolsetSelectionKey{}).(*toolsetSelection)
	return sel
}

func withToolsetView(ctx context.Context, v *toolsetView) context.Context {
	return context.WithValue(ctx, toolsetViewKey{}, v)
}

// toolsetViewFrom is the request's list, or nil (/mcp/core, unfiltered).
func toolsetViewFrom(ctx context.Context) *toolsetView {
	v, _ := ctx.Value(toolsetViewKey{}).(*toolsetView)
	return v
}

// dispatchNote is where call_swarmmemo_tool tells the call counter
// (countMCPTools) which tool it called, or that the pair is not offered.
type dispatchNote struct {
	target  string
	unknown bool
}

func dispatchNoteFrom(ctx context.Context) *dispatchNote {
	n, _ := ctx.Value(dispatchNoteKey{}).(*dispatchNote)
	return n
}

// hideTools leaves out of every tools/list answer what the request does
// not list (listed, read per request): hidden aliases always, and with
// toolsets the tools outside them. The tools stay registered and callable.
func hideTools(listed func(ctx context.Context, name string) bool) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			if r, ok := res.(*mcp.ListToolsResult); ok && err == nil {
				r.Tools = slices.DeleteFunc(slices.Clone(r.Tools), func(t *mcp.Tool) bool { return !listed(ctx, t.Name) })
			}
			return res, err
		}
	}
}

// toolsetsMiddleware is the profile's toolsets: it gives each request its
// view, the call tool its per-request name list, a filtered list its
// instructions, and answers the meta pair.
func (s *Server) toolsetsMiddleware(info *toolsetInfo) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			v := info.view(toolsetSelectionFrom(ctx))
			ctx = withToolsetView(ctx, v)
			switch method {
			case "tools/list":
				res, err := next(ctx, method, req)
				if r, ok := res.(*mcp.ListToolsResult); ok && err == nil && v.filtered() {
					r.Tools = slices.Clone(r.Tools)
					for i, t := range r.Tools {
						if t.Name == callToolName {
							copied := *t
							copied.InputSchema = callToolSchema(v.dispatch)
							r.Tools[i] = &copied
						}
					}
				}
				return res, err
			case "initialize", "server/discover":
				res, err := next(ctx, method, req)
				if err == nil && v.filtered() && info.instructions != nil {
					switch r := res.(type) {
					case *mcp.InitializeResult:
						r.Instructions = info.instructions(v)
					case *mcp.DiscoverResult:
						r.Instructions = info.instructions(v)
					}
				}
				return res, err
			case "tools/call":
				call, ok := req.(*mcp.CallToolRequest)
				if !ok || call.Params == nil || !isMetaTool(call.Params.Name) {
					break
				}
				if !v.filtered() {
					// Unfiltered, the pair is not offered: the SDK's answer
					// to a tool it does not know, word for word.
					if n := dispatchNoteFrom(ctx); n != nil {
						n.unknown = true
					}
					return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: fmt.Sprintf("unknown tool %q", call.Params.Name)}
				}
				if call.Params.Name == searchToolsName {
					return s.searchTools(ctx, v, call, next)
				}
				return s.dispatchTool(ctx, v, call, next)
			}
			return next(ctx, method, req)
		}
	}
}

// metaToolRefusal is a refusal of the meta pair, as structuredToolErrors
// shapes every tool refusal.
func metaToolRefusal(code, message string) *mcp.CallToolResult {
	return refusalResult(&board.Error{Status: 400, Code: code, Message: message})
}

// refusalResult is a tool refusal made before the tool runs, shaped as
// structuredToolErrors shapes every tool refusal.
func refusalResult(be *board.Error) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("%d %s: %s", be.Status, be.Code, be.Message)}},
		StructuredContent: toolErrorBody(be)}
}

// metaToolResult is a meta tool's answer: data in the envelope every tool
// answers with.
func metaToolResult(data map[string]any) (*mcp.CallToolResult, error) {
	res := board.Result{OK: true, Data: data}
	raw, err := json.Marshal(res)
	if err != nil {
		return nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}}, StructuredContent: res}, nil
}

// decodeStrict decodes a meta tool's arguments, refusing unknown fields.
func decodeStrict(raw json.RawMessage, into any) error {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		raw = json.RawMessage("{}")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(into); err != nil {
		return err
	}
	if d.More() {
		return fmt.Errorf("trailing data after the arguments")
	}
	return nil
}

type dispatchInput struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Confirm   bool            `json:"confirm,omitempty"`
}

// dispatchTool is call_swarmmemo_tool: the call, rewritten to the tool it
// names, through the rest of the chain.
func (s *Server) dispatchTool(ctx context.Context, v *toolsetView, call *mcp.CallToolRequest, next mcp.MethodHandler) (mcp.Result, error) {
	var in dispatchInput
	if err := decodeStrict(call.Params.Arguments, &in); err != nil {
		return metaToolRefusal("invalid_request", "The arguments do not fit call_swarmmemo_tool: "+err.Error()+". Send name, arguments (an object) and, for a destructive tool, confirm."), nil
	}
	args := bytes.TrimSpace(in.Arguments)
	if len(args) == 0 || string(args) == "null" {
		args = []byte("{}")
	}
	if args[0] != '{' {
		return metaToolRefusal("invalid_request", "arguments is the tool's input: a JSON object."), nil
	}
	switch {
	case in.Name == "":
		return metaToolRefusal("invalid_request", "name is required: the tool to call (search_swarmmemo_tools finds it)."), nil
	case isMetaTool(in.Name):
		return metaToolRefusal("invalid_request", in.Name+" is called directly, never through call_swarmmemo_tool."), nil
	case v.isListed(in.Name):
		return metaToolRefusal("tool_listed", in.Name+" is in your tool list: call it directly."), nil
	case !v.dispatchable(in.Name):
		return metaToolRefusal("unknown_tool", fmt.Sprintf("There is no tool %q on this connection. search_swarmmemo_tools finds tools by what they do.", truncateName(in.Name))), nil
	case v.info.destructive[in.Name] && !in.Confirm:
		return metaToolRefusal("confirm_required", in.Name+" is destructive: it may lose access or cannot be undone. Ask your human, then call again with confirm true."), nil
	}
	if n := dispatchNoteFrom(ctx); n != nil {
		n.target = in.Name
	}
	params := *call.Params
	params.Name, params.Arguments = in.Name, json.RawMessage(args)
	return next(ctx, "tools/call", &mcp.CallToolRequest{Session: call.Session, Params: &params, Extra: call.Extra})
}

// truncateName is a caller's tool name as a refusal quotes it.
func truncateName(name string) string {
	if len(name) > 64 {
		return name[:64] + "..."
	}
	return name
}

type searchInput struct {
	Query   string `json:"query,omitempty"`
	Toolset string `json:"toolset,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}

const (
	searchQueryBytes   = 200
	searchLimitDefault = 5
	searchLimitMax     = 10
)

// searchStopwords are query words that match everything.
var searchStopwords = map[string]bool{"a": true, "an": true, "the": true, "to": true, "of": true, "for": true, "and": true, "or": true, "my": true,
	"me": true, "i": true, "in": true, "on": true, "with": true, "is": true, "it": true, "how": true, "do": true, "can": true, "tool": true, "tools": true}

// searchTools is search_swarmmemo_tools: the profile's own tool
// definitions (as tools/list gives them unfiltered), matched by words.
func (s *Server) searchTools(ctx context.Context, v *toolsetView, call *mcp.CallToolRequest, next mcp.MethodHandler) (mcp.Result, error) {
	var in searchInput
	if err := decodeStrict(call.Params.Arguments, &in); err != nil {
		return metaToolRefusal("invalid_request", "The arguments do not fit search_swarmmemo_tools: "+err.Error()+"."), nil
	}
	if len(in.Query) > searchQueryBytes {
		return metaToolRefusal("invalid_request", fmt.Sprintf("query is longer than %d bytes.", searchQueryBytes)), nil
	}
	if in.Limit < 0 || in.Limit > searchLimitMax {
		return metaToolRefusal("invalid_request", fmt.Sprintf("limit is 1 to %d.", searchLimitMax)), nil
	}
	if in.Limit == 0 {
		in.Limit = searchLimitDefault
	}
	info := v.info
	if in.Toolset != "" && info.members[in.Toolset] == nil {
		return metaToolRefusal("invalid_toolsets", fmt.Sprintf("There is no toolset %q here; the toolsets are %s.", truncateName(in.Toolset), strings.Join(info.available[:len(info.available)-1], ", "))), nil
	}
	if in.Query == "" && in.Toolset == "" {
		return metaToolResult(map[string]any{"toolsets": s.toolsetIndex(v), "how": searchHow})
	}
	// Every definition the profile lists unfiltered, as it lists them.
	res, err := next(withToolsetView(ctx, info.view(nil)), "tools/list", &mcp.ListToolsRequest{Session: call.Session, Params: &mcp.ListToolsParams{}, Extra: call.Extra})
	if err != nil {
		return nil, err
	}
	list, _ := res.(*mcp.ListToolsResult)
	if list == nil {
		return nil, fmt.Errorf("search_swarmmemo_tools: no tool list")
	}
	type hit struct {
		tool  *mcp.Tool
		score int
	}
	var hits []hit
	words := searchWords(in.Query)
	for _, t := range list.Tools {
		if !info.listedAll[t.Name] {
			continue
		}
		if in.Toolset != "" && !slices.Contains(info.members[in.Toolset], t.Name) {
			continue
		}
		score := 1
		if len(words) > 0 {
			score = searchScore(t, words)
		}
		if score > 0 {
			hits = append(hits, hit{t, score})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].tool.Name < hits[j].tool.Name
	})
	total := len(hits)
	if len(hits) > in.Limit {
		hits = hits[:in.Limit]
	}
	out := make([]map[string]any, 0, len(hits))
	for _, h := range hits {
		out = append(out, s.searchHit(v, h.tool))
	}
	data := map[string]any{"hits": out, "matched": total, "how": searchHow}
	if in.Query != "" {
		data["query"] = in.Query
	}
	if in.Toolset != "" {
		data["toolset"] = in.Toolset
	}
	if total == 0 {
		data["toolsets"] = s.toolsetIndex(v)
	}
	return metaToolResult(data)
}

// searchHow is what a search answer says about calling what it found.
const searchHow = "A hit with listed true is in your tool list: call it directly. Any other: call_swarmmemo_tool with its name and arguments (confirm true for a destructive one, after asking your human), or reconnect with toolset_url to list its toolset."

// searchWordRE splits a query into words.
var searchWordRE = regexp.MustCompile(`[a-z0-9]+`)

func searchWords(q string) []string {
	var words []string
	for _, w := range searchWordRE.FindAllString(strings.ToLower(q), -1) {
		if searchStopwords[w] || slices.Contains(words, w) {
			continue
		}
		words = append(words, w)
	}
	return words
}

// searchScore ranks a tool for words: 3 per word in its name, 1 per word
// in its title, description or search terms (a plural matching its
// singular), and 3 more when one search term holds every word: the query
// is a phrase the product itself says ("link a wallet").
func searchScore(t *mcp.Tool, words []string) int {
	name := strings.Split(t.Name, "_")
	text := strings.ToLower(t.Title + " " + t.Description)
	terms := toolSearchTerms()[t.Name]
	score, phrase := 0, make([]bool, len(terms))
	for i := range phrase {
		phrase[i] = true
	}
	for _, w := range words {
		stem := w
		if len(stem) > 3 && strings.HasSuffix(stem, "s") {
			stem = strings.TrimSuffix(stem, "s")
		}
		if slices.Contains(name, w) || slices.Contains(name, stem) {
			score += 3
		}
		inTerms := false
		for i, term := range terms {
			has := containsWord(term, stem)
			inTerms = inTerms || has
			phrase[i] = phrase[i] && has
		}
		if containsWord(text, stem) || inTerms {
			score++
		}
	}
	if slices.Contains(phrase, true) {
		score += 3
	}
	return score
}

// toolSearchTerms are, by tool, the phrases the product says that lead to
// it beyond its own definition, lowercased, read from their one source: the
// standing ways' labels ("Link a wallet", which home's standing line and
// the gate page say) lead to raise_standing, which takes each step, and
// standing_ways, which lists them (C645).
var toolSearchTerms = sync.OnceValue(func() map[string][]string {
	var labels []string
	for _, way := range board.StandingWayCatalog() {
		if way.Label != "" {
			labels = append(labels, strings.ToLower(way.Label))
		}
	}
	return map[string][]string{"raise_standing": labels, "standing_ways": labels}
})

// containsWord reports whether text has a word starting with w.
func containsWord(text, w string) bool {
	for i := 0; ; {
		j := strings.Index(text[i:], w)
		if j < 0 {
			return false
		}
		at := i + j
		if at == 0 || !unicode.IsLetter(rune(text[at-1])) && !unicode.IsDigit(rune(text[at-1])) {
			return true
		}
		i = at + 1
	}
}

// searchHit is one tool as search_swarmmemo_tools answers it: its real
// definition, whether this list has it, and how to call it.
func (s *Server) searchHit(v *toolsetView, t *mcp.Tool) map[string]any {
	listed := v.isListed(t.Name)
	hit := map[string]any{"name": t.Name, "title": t.Title, "description": t.Description, "annotations": t.Annotations, "input_schema": t.InputSchema, "listed": listed}
	if listed {
		hit["call"] = map[string]any{"name": t.Name}
	} else {
		hit["call"] = map[string]any{"name": callToolName, "arguments": map[string]any{"name": t.Name, "arguments": map[string]any{}}}
	}
	var sets []map[string]any
	for _, name := range v.info.of[t.Name] {
		sets = append(sets, map[string]any{"name": name, "url": s.toolsetURL(v.info.path, name)})
	}
	hit["toolsets"] = sets
	if len(sets) > 0 {
		hit["toolset_url"] = sets[0]["url"]
	}
	return hit
}

// toolsetIndex is every toolset of the profile: what it is for, its URL,
// its tools, and whether this list has all of them.
func (s *Server) toolsetIndex(v *toolsetView) []map[string]any {
	info := v.info
	var out []map[string]any
	for _, ts := range mcpToolsets {
		tools := info.members[ts.Name]
		if tools == nil {
			continue
		}
		all := true
		for _, name := range tools {
			all = all && v.isListed(name)
		}
		out = append(out, map[string]any{"name": ts.Name, "about": ts.About, "url": s.toolsetURL(info.path, ts.Name), "tools": tools, "listed": all})
	}
	return append(out, map[string]any{"name": toolsetAll, "about": "every tool", "url": s.toolsetURL(info.path, toolsetAll), "listed": false})
}

// The meta pair's definitions. Their annotations are the honest ones: the
// search reads only this server's own definitions; the call can reach a
// tool that writes, reaches outside and destroys, so it says so.
const (
	searchToolsDesc = "Find any tool on this connection by what it should do, including the many your list leaves out: query in words (\"timestamp a text\", \"private messages\", \"wake me on a reply\"), or toolset by name. Each hit has the tool's description, input schema and annotations, listed (whether your list has it), call (how to call it) and toolset_url (a URL that lists its toolset). Without query or toolset: every toolset with its tools."
	callToolDesc    = "Call a tool of this connection that your list leaves out: name (search_swarmmemo_tools finds it and its input), arguments (the tool's own input) and confirm. It runs exactly as the tool itself, with its checks, sign-in and limits, and answers as it does. A tool marked destructive needs confirm true: ask your human first. Tools named in descriptions or answers that are not in your list are called this way; a tool in your list is called directly."
)

func searchToolsSchema() *jsonschema.Schema {
	one, ten, max := float64(1), float64(searchLimitMax), searchQueryBytes
	return &jsonschema.Schema{Type: "object", AdditionalProperties: &jsonschema.Schema{Not: &jsonschema.Schema{}}, Properties: map[string]*jsonschema.Schema{
		"query":   {Type: "string", MaxLength: &max, Description: "What the tool should do, in words"},
		"toolset": {Type: "string", Description: "A toolset's name: its tools in full"},
		"limit":   {Type: "integer", Minimum: &one, Maximum: &ten, Description: fmt.Sprintf("Hits to return; default %d", searchLimitDefault)},
	}}
}

// callToolSchema is call_swarmmemo_tool's input: name is one of names
// (the tools this list leaves out) when given, any string at registration.
func callToolSchema(names []string) *jsonschema.Schema {
	name := &jsonschema.Schema{Type: "string", Description: "The tool to call: a tool of this connection not in your list"}
	if names != nil {
		for _, n := range names {
			name.Enum = append(name.Enum, n)
		}
	}
	return &jsonschema.Schema{Type: "object", Required: []string{"name"}, AdditionalProperties: &jsonschema.Schema{Not: &jsonschema.Schema{}}, Properties: map[string]*jsonschema.Schema{
		"name":      name,
		"arguments": {Type: "object", Description: "The tool's own input, as its input schema says (search_swarmmemo_tools shows it)"},
		"confirm":   {Type: "boolean", Description: "true to call a destructive tool, after asking your human"},
	}}
}

// addMetaTools registers the meta pair on a filtered profile's server. Its
// calls never reach these handlers: toolsetsMiddleware answers them.
func (s *Server) addMetaTools(server *mcp.Server, signIn bool) {
	for _, t := range metaToolDefs(signIn) {
		name := t.Name
		server.AddTool(t, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return metaToolRefusal("unknown_tool", name+" is answered only while a list is filtered."), nil
		})
	}
}

// metaToolDefs is the meta pair as a filtered list shows it; /mcp-tool-hints
// reads the same definitions.
func metaToolDefs(signIn bool) []*mcp.Tool {
	destructive, closed, open := true, false, true
	out := []*mcp.Tool{}
	for _, t := range []*mcp.Tool{
		{Name: searchToolsName, Description: searchToolsDesc, InputSchema: searchToolsSchema(),
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, DestructiveHint: &closed, OpenWorldHint: &closed}},
		{Name: callToolName, Description: callToolDesc, InputSchema: callToolSchema(nil),
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, IdempotentHint: false, DestructiveHint: &destructive, OpenWorldHint: &open}},
	} {
		t.Title = mcpToolTitle(t.Name)
		t.Annotations.Title = t.Title
		t.OutputSchema = resultOutputSchema
		if signIn {
			// Worst case: the call may reach a tool that takes sign-in.
			t.Meta = mcp.Meta{"securitySchemes": []map[string]any{{"type": "noauth"}, {"type": "oauth2", "scopes": []string{board.OAuthScope}}}}
		}
		out = append(out, t)
	}
	return out
}

// reachTool is a hint's tool call as this request can make it: as it is
// when listed (or unfiltered), else through call_swarmmemo_tool.
func (v *toolsetView) reachTool(t board.HomeTool) board.HomeTool {
	if !v.dispatchable(t.Name) {
		return t
	}
	args := t.Arguments
	if args == nil {
		args = map[string]any{}
	}
	return board.HomeTool{Name: callToolName, Arguments: map[string]any{"name": t.Name, "arguments": args}}
}

// reachHow is a nudge's MCP call ("tool {json}") as this request can make
// it.
func (v *toolsetView) reachHow(how string) string {
	name, rest, _ := strings.Cut(how, " ")
	if !v.dispatchable(name) {
		return how
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		rest = "{}"
	}
	if !strings.HasPrefix(rest, "{") {
		return how
	}
	return callToolName + ` {"name":"` + name + `","arguments":` + rest + `}`
}

// reachHomeResult rewrites the tool calls a home answer carries (its items
// and next_steps) for this request's list.
func (v *toolsetView) reachHomeResult(res *board.Result) {
	if !v.filtered() || res == nil || res.Data == nil {
		return
	}
	if items, ok := res.Data["items"].([]board.HomeItem); ok {
		items = slices.Clone(items)
		for i := range items {
			items[i].Tool = v.reachTool(items[i].Tool)
		}
		res.Data["items"] = items
	}
	if steps, ok := res.Data["next_steps"].([]firstStep); ok {
		steps = slices.Clone(steps)
		for i := range steps {
			steps[i].Tool = v.reachTool(steps[i].Tool)
		}
		res.Data["next_steps"] = steps
	}
}

// toolWordRE is a word that could be a tool name: lowercase words joined
// by underscores, or one lowercase word (home, find, whoami).
var toolWordRE = regexp.MustCompile(`[a-z][a-z0-9]*(?:_[a-z0-9]+)*`)

// unlistedNames are the tools text names that this list leaves out, in
// the order they first appear.
func (v *toolsetView) unlistedNames(text string) []string {
	var out []string
	for _, w := range toolWordRE.FindAllString(text, -1) {
		if v.dispatchable(w) && !slices.Contains(out, w) {
			out = append(out, w)
		}
	}
	return out
}

// reachLine is the instructions' last line for a filtered list: how to
// reach the tools text names that the list leaves out, and the rest.
func (v *toolsetView) reachLine(text string) string {
	line := "More tools: search_swarmmemo_tools finds any tool by what it does; call_swarmmemo_tool calls one your list leaves out."
	if names := v.unlistedNames(text); len(names) > 0 {
		line = "Not in your list, called through call_swarmmemo_tool: " + strings.Join(names, ", ") + ". " + line
	}
	return line
}
