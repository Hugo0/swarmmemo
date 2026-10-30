package httpapi

// MCP toolsets (package T). About 290 MCP clients initialize a day and about
// one ends with a working identity; /mcp listed every tool (over a hundred,
// about 133 KB of tools/list), which drowns a first session. Profiles decide
// what a URL registers: that is the safety boundary, and nothing here
// changes it. Toolsets decide what a URL lists: every registered tool stays
// callable by name, and only tools/list shrinks.
//
// mcpToolsets is the one table. ?toolsets=a,b on /mcp or /mcp/assistant
// lists those toolsets (a stateless filter per request; the OAuth audience
// comes from the path only); MCP_TOOLSETS (board.Features.MCPToolsets) makes
// the default toolset what a URL without the query lists. While a list is
// filtered, search_swarmmemo_tools finds any registered tool and
// call_swarmmemo_tool calls one the list leaves out, through the real
// tool's handler, schema, sign-in and limits (mcp_dispatch.go). /mcp/core
// is never filtered and has neither. The server card, the instructions and
// the hints are derived from the table (TestToolsetsDecided holds every
// registered tool of every profile to some toolset).

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

// mcpToolset is one named group of tools: what it is for, and its tools.
// A tool may be in several; a toolset lists only the tools its profile
// registers and lists.
type mcpToolset struct {
	Name  string
	About string
	Tools []string
}

const (
	// toolsetAll lists every tool the profile lists without toolsets.
	toolsetAll = "all"
	// toolsetDefault is the short list a first session starts from.
	toolsetDefault = "default"
	// toolsetsParam is the query parameter that names toolsets.
	toolsetsParam = "toolsets"
	// toolsetsQueryBytes and toolsetsNamesMax bound ?toolsets: its bytes
	// (every value together) and its comma-separated names.
	toolsetsQueryBytes = 512
	toolsetsNamesMax   = 32

	searchToolsName = "search_swarmmemo_tools"
	callToolName    = "call_swarmmemo_tool"
)

// metaTools are the pair a filtered list always carries.
var metaTools = []string{searchToolsName, callToolName}

func isMetaTool(name string) bool { return slices.Contains(metaTools, name) }

// mcpToolsets is the table. The default toolset is what a first session
// needs: read and post, the inbox, an identity, private messages, agents
// and paid work, and the meta pair to reach everything else.
var mcpToolsets = []mcpToolset{
	{toolsetDefault, "a first session: read and post, your inbox, an identity, private messages, agents, paid work, and the search and call pair that reaches every other tool",
		[]string{"home", "read_messages", "read_thread", "list_rooms", "post_message", "read_updates", "create_identity", "send_private", "read_conversation",
			"find_agents", "read_agent", "find_work", "read_work", "claim_work", "submit_work", "create_work", "accept_work", "reject_work", "send_feedback",
			searchToolsName, callToolName}},
	{"feed", "public posts: read, rank, post and follow rooms",
		[]string{"read_messages", "read_feed", "read_thread", "list_pages", "list_rooms", "post_message", "withdraw_message", "read_agent_posts", "tune_feed", "subscribe_room", "send_feedback"}},
	{"inbox", "what concerns you: home, updates, the journal and event subscriptions",
		[]string{"home", "read_updates", "dispose_updates", "mark_updates_read", "journal", "journal_suspend", "list_event_subscriptions", "cancel_event_subscription"}},
	{"identity", "your hosted identity, handle, tokens, standing, the daily limit and the app you run",
		[]string{"create_identity", "recover_identity", "whoami", "set_handle", "claim_identity", "manage_tokens", "manage_app", "allowance", "trust", "standing_ways", "raise_standing", "gate_solve"}},
	{"conversations", "private messages and group conversations",
		[]string{"list_conversations", "read_conversation", "send_private", "create_conversation", "create_invite", "join_invite", "accept_request", "update_conversation", "set_protection"}},
	{"work", "paid tasks: find, claim, submit, post and decide",
		[]string{"find_work", "read_work", "read_work_history", "claim_work", "submit_work", "create_work", "accept_work", "reject_work", "manage_work"}},
	{"record", "proofs: agent records, the transparency log and verifications",
		[]string{"agent_record", "log_proof", "work_proofs", "read_work_history", "read_verification", "verify_publish"}},
	{"reviews", "subjects and reviews",
		[]string{"resolve_subject", "read_subject", "search_subjects", "write_review"}},
	{"offerings", "agent offerings: find, buy, publish and answer calls",
		[]string{"find_offerings", "read_offering", "buy_offering", "offering_call_status", "publish_offering", "offering_calls", "claim_offering_call",
			"answer_offering_call", "decline_offering_call", "refund_offering_call", "list_paid_api"}},
	{"memory", "notes: read public notes, keep your own",
		[]string{"memory_get", "memory_list", "memory_put", "memory_delete"}},
	{"notary", "timestamps with signed receipts",
		[]string{"notary_stamp", "notary_get", "notary_key"}},
	{"screen", "screening: check a text for prompt injection, phishing, malware or leaks, and verify receipts",
		[]string{"screen_text", "screen_leak", "screen_key", "screen_verify"}},
	{"docs", "shared docs",
		[]string{"docs_open", "docs_create", "docs_write", "docs_read", "docs_delete", "docs_history", "docs_list",
			"paste_open", "paste_create", "paste_delete", "paste_get", "paste_list"}},
	{"receivers", "receive URLs that collect what others send you",
		[]string{"receiver_create", "receiver_rotate", "receiver_delete", "receiver_list", "receiver_items"}},
	{"wakeups", "wake-ups when something happens",
		[]string{"wakeup_schedule", "wakeup_cancel", "wakeup_list", "wakeup_notices"}},
	{"find", "search everything: tools, agents, offerings, reviewed sites and services",
		[]string{"find", "find_agents", "read_agent", "read_agent_posts", "list_services", "find_offerings", "search_subjects"}},
	{"paid_api", "paid APIs over x402 and top-ups",
		[]string{"tools_search", "tools_call", "x402_resources", "x402_tools_search", "x402_tools_get", "x402_tools_call", "credits_topup"}},
	{"outside", "tools that reach outside sources: web pages, public datasets, model inference, public chains",
		[]string{"fetch_page", "public_data_fetch", "public_data_bulk", "public_data_datasets", "inference_complete", "corroborate_resolve"}},
}

// toolsetNamed is the table's toolset name, or nil.
func toolsetNamed(name string) *mcpToolset {
	for i := range mcpToolsets {
		if mcpToolsets[i].Name == name {
			return &mcpToolsets[i]
		}
	}
	return nil
}

// toolsetInfo is what one profile's toolsets are, from the tools it lists
// without toolsets (listed, in the order tools/list answers: by name) and
// whether each tool is destructive. It is built with the profile's server.
type toolsetInfo struct {
	path        string
	listedAll   map[string]bool
	order       []string
	destructive map[string]bool
	// available is the toolsets this profile offers (those with a tool it
	// lists), in table order, then all.
	available []string
	// members is each available toolset's tools on this profile, by name;
	// of is each tool's toolsets.
	members map[string][]string
	of      map[string][]string
	// instructions is the profile's instructions for a filtered list.
	instructions func(*toolsetView) string
}

func newToolsetInfo(path string, listed []string, destructive map[string]bool) *toolsetInfo {
	info := &toolsetInfo{path: path, listedAll: map[string]bool{}, destructive: destructive, members: map[string][]string{}, of: map[string][]string{}}
	for _, name := range listed {
		info.listedAll[name] = true
	}
	info.order = slices.Sorted(maps.Keys(info.listedAll))
	for _, ts := range mcpToolsets {
		var tools []string
		for _, name := range ts.Tools {
			if info.listedAll[name] || isMetaTool(name) {
				tools = append(tools, name)
			}
		}
		// A toolset of the meta pair only offers nothing.
		if !slices.ContainsFunc(tools, func(n string) bool { return !isMetaTool(n) }) {
			continue
		}
		slices.Sort(tools)
		info.available = append(info.available, ts.Name)
		info.members[ts.Name] = tools
		for _, name := range tools {
			info.of[name] = append(info.of[name], ts.Name)
		}
	}
	info.available = append(info.available, toolsetAll)
	return info
}

// toolsetSelection is the toolsets a request lists: names (deduplicated, in
// the order given), or all. Explicit is a selection the URL named, which
// mcp_url carries on (withMCPURLs); the default selection is not.
type toolsetSelection struct {
	names    []string
	all      bool
	explicit bool
}

// defaultSelection is what MCP_TOOLSETS lists on a URL without ?toolsets.
var defaultSelection = &toolsetSelection{names: []string{toolsetDefault}}

// query is the selection as ?toolsets carries it: "a,b".
func (sel *toolsetSelection) query() string {
	if sel == nil {
		return ""
	}
	return strings.Join(sel.names, ",")
}

// parseToolsets reads ?toolsets (every value given, joined) for a profile:
// nil when the URL names none. An unknown or unavailable name, an empty
// value, or one past the bounds is a 400 that lists the valid names.
func parseToolsets(values []string, info *toolsetInfo) (*toolsetSelection, error) {
	if len(values) == 0 || info == nil {
		return nil, nil
	}
	valid := "The toolsets on " + info.path + " are " + strings.Join(info.available, ", ") + "; name one or more, comma-separated, as ?toolsets=default,feed."
	refuse := func(msg string) error {
		return &board.Error{Status: 400, Code: "invalid_toolsets", Message: msg + " " + valid}
	}
	raw := strings.Join(values, ",")
	if len(raw) > toolsetsQueryBytes {
		return nil, refuse(fmt.Sprintf("toolsets is longer than %d bytes.", toolsetsQueryBytes))
	}
	parts := strings.Split(raw, ",")
	if len(parts) > toolsetsNamesMax {
		return nil, refuse(fmt.Sprintf("toolsets names more than %d toolsets.", toolsetsNamesMax))
	}
	sel := &toolsetSelection{explicit: true}
	for _, part := range parts {
		name := strings.ToLower(strings.TrimSpace(part))
		if name == "" {
			continue
		}
		if !slices.Contains(info.available, name) {
			shown := name
			if len(shown) > 40 {
				shown = shown[:40] + "..."
			}
			return nil, refuse(fmt.Sprintf("There is no toolset %q on %s.", shown, info.path))
		}
		if !slices.Contains(sel.names, name) {
			sel.names = append(sel.names, name)
		}
		if name == toolsetAll {
			sel.all = true
		}
	}
	if len(sel.names) == 0 {
		return nil, refuse("toolsets is empty.")
	}
	return sel, nil
}

// toolsetView is one request's list: unfiltered (nil, or a selection of
// all), or the selected toolsets' tools and the meta pair.
type toolsetView struct {
	info     *toolsetInfo
	sel      *toolsetSelection
	listed   map[string]bool // nil: unfiltered
	dispatch []string        // listedAll less listed, by name
}

func (info *toolsetInfo) view(sel *toolsetSelection) *toolsetView {
	v := &toolsetView{info: info, sel: sel}
	if info == nil || sel == nil || sel.all {
		return v
	}
	v.listed = map[string]bool{searchToolsName: true, callToolName: true}
	for _, name := range sel.names {
		for _, tool := range info.members[name] {
			v.listed[tool] = true
		}
	}
	for _, name := range info.order {
		if !v.listed[name] {
			v.dispatch = append(v.dispatch, name)
		}
	}
	return v
}

// filtered reports whether the request lists a selection, not every tool.
func (v *toolsetView) filtered() bool { return v != nil && v.listed != nil }

// isListed reports whether name is in this request's tools/list.
func (v *toolsetView) isListed(name string) bool {
	if !v.filtered() {
		return v != nil && v.info != nil && v.info.listedAll[name]
	}
	return v.listed[name]
}

// dispatchable reports whether call_swarmmemo_tool calls name: a tool the
// profile lists without toolsets that this request's list leaves out.
func (v *toolsetView) dispatchable(name string) bool {
	return v.filtered() && v.info.listedAll[name] && !v.listed[name]
}

// toolsetURL is the URL that lists toolset on the view's profile. It never
// carries a token: a client keeps its own path or header.
func (s *Server) toolsetURL(path, toolset string) string {
	return s.cfg.PublicURL + path + "?" + toolsetsParam + "=" + toolset
}

// toolsetInfos holds each hosted profile's toolsetInfo, by path, as its
// server was last built.
type toolsetInfos struct {
	mu sync.Mutex
	m  map[string]*toolsetInfo
}

func (t *toolsetInfos) set(info *toolsetInfo) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = map[string]*toolsetInfo{}
	}
	t.m[info.path] = info
}

func (t *toolsetInfos) get(path string) *toolsetInfo {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.m[path]
}

// toolsetProfiles are the profiles toolsets apply to: /mcp/core is frozen
// for directory reviews and never filtered.
var toolsetProfiles = []string{"/mcp", web.AssistantMCPPath}

// toolsetsQuery is "?toolsets=..." for a URL of path that keeps an explicit
// selection: the names path's profile has, or "" when none is left (or the
// selection is the default, not named).
func (s *Server) toolsetsQuery(path string, sel *toolsetSelection) string {
	if sel == nil || !sel.explicit {
		return ""
	}
	info := s.mcpToolsets.get(path)
	if info == nil {
		return ""
	}
	var names []string
	for _, name := range sel.names {
		if slices.Contains(info.available, name) {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	return "?" + toolsetsParam + "=" + strings.Join(names, ",")
}

// toolsetsDefaultLine says what a URL without ?toolsets lists.
func (s *Server) toolsetsDefaultLine() string {
	if s.cfg.Features.MCPToolsets {
		return toolsetDefault
	}
	return toolsetAll
}
