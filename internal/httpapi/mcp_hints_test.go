package httpapi

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"swarmmemo/internal/web"
)

// Every tool the widest server lists has its hints explained from the one
// table (mcp_hints.go): a write says what it changes, a destructive tool
// what it removes, an idempotent write why a repeat changes nothing; no
// table names a tool that does not exist; and /mcp-tool-hints serves the
// same tools/list the profiles do, with a reason for every hint.
func TestMCPToolHintsExplained(t *testing.T) {
	s := pinServer(t)
	seen := map[string]bool{}
	for _, path := range []string{"/mcp", "/mcp?toolsets=all", mcpProfileCore, web.AssistantMCPPath} {
		for name, tool := range listTools(t, s, path) {
			seen[name] = true
			a := tool.Annotations
			if !a["readOnlyHint"] && mcpWriteEffects[name] == "" && name != callToolName {
				t.Errorf("%s %s writes but mcpWriteEffects does not say what it changes", path, name)
			}
			if a["destructiveHint"] && mcpDestructiveWhy[name] == "" {
				t.Errorf("%s %s is destructive but mcpDestructiveWhy does not say what it removes", path, name)
			}
			if !a["readOnlyHint"] && a["idempotentHint"] != (mcpIdempotentWrites[name] != "") {
				t.Errorf("%s %s idempotentHint %v disagrees with mcpIdempotentWrites", path, name, a["idempotentHint"])
			}
		}
	}
	// A tool that publishes, changes or withdraws a PUBLIC post acts on what
	// everyone sees: open world (OpenAI directory review, 10-24: withdraw_message
	// was closed). The list names every such tool; a new one belongs here.
	for _, path := range []string{"/mcp?toolsets=all", mcpProfileCore, web.AssistantMCPPath} {
		for name, tool := range listTools(t, s, path) {
			if publicPostTools[name] && !tool.Annotations["openWorldHint"] {
				t.Errorf("%s %s acts on public posts but has openWorldHint false", path, name)
			}
		}
	}
	for _, table := range []map[string]string{mcpWriteEffects, mcpDestructiveWhy, mcpIdempotentWrites} {
		for name := range table {
			// A tool behind a feature this server leaves off (reviews, top-ups,
			// inbox entries, the toolsets entry) is still a defined tool.
			if !seen[name] && toolOps[name] == nil && name != callToolName {
				t.Errorf("hint table names %s, which no profile lists", name)
			}
		}
	}
	// Directory reviewers' fixes: these were wrong before 2026-10-23.
	full := listTools(t, s, "/mcp?toolsets=all")
	for name, want := range map[string]map[string]bool{
		"recover_identity": {"destructiveHint": true},
		"memory_put":       {"idempotentHint": true, "destructiveHint": true},
		"notary_stamp":     {"idempotentHint": true},
		"wakeup_schedule":  {"idempotentHint": true},
		"manage_tokens":    {"openWorldHint": false},
		"set_protection":   {"openWorldHint": false, "idempotentHint": true},
		"post_message":     {"idempotentHint": false, "readOnlyHint": false},
	} {
		for hint, v := range want {
			if full[name].Annotations[hint] != v {
				t.Errorf("%s %s = %v, want %v", name, hint, full[name].Annotations[hint], v)
			}
		}
	}

	w := makeRequest(s, "GET", "https://swarmmemo.com"+mcpToolHintsPath, "", "")
	var page struct {
		OK       bool                     `json:"ok"`
		Profiles map[string][]toolHintRow `json:"profiles"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || w.Code != 200 || !page.OK {
		t.Fatalf("%s: %d %v %s", mcpToolHintsPath, w.Code, err, w.Body.String())
	}
	core := listTools(t, s, mcpProfileCore)
	if len(page.Profiles[mcpProfileCore]) != len(core) || len(page.Profiles[web.AssistantMCPPath]) == 0 {
		t.Fatalf("hints page lists %d core tools, tools/list %d", len(page.Profiles[mcpProfileCore]), len(core))
	}
	for _, row := range page.Profiles[mcpProfileCore] {
		a := core[row.Name].Annotations
		if row.ReadOnlyHint != a["readOnlyHint"] || row.DestructiveHint != a["destructiveHint"] || row.IdempotentHint != a["idempotentHint"] || row.OpenWorldHint != a["openWorldHint"] {
			t.Errorf("hints page %s differs from tools/list: %+v vs %v", row.Name, row, a)
		}
		for _, k := range hintRuleOrder {
			want := "false: "
			if map[string]bool{"readOnlyHint": row.ReadOnlyHint, "destructiveHint": row.DestructiveHint, "idempotentHint": row.IdempotentHint, "openWorldHint": row.OpenWorldHint}[k] {
				want = "true: "
			}
			if !strings.HasPrefix(row.Reasons[k], want) {
				t.Errorf("%s %s reason %q does not start with %q", row.Name, k, row.Reasons[k], want)
			}
		}
	}
	// Every assistant list carries the meta pair; the page
	// names it too, with its reasons (directory review, 10-24).
	meta := map[string]bool{}
	for _, row := range page.Profiles[web.AssistantMCPPath] {
		if isMetaTool(row.Name) && row.Reasons["readOnlyHint"] != "" {
			meta[row.Name] = true
		}
	}
	if !meta[callToolName] || !meta[searchToolsName] {
		t.Fatalf("hints page leaves out the assistant's meta tools: %v", meta)
	}
	md := makeRequest(s, "GET", "https://swarmmemo.com"+mcpToolHintsPath+".md", "", "")
	if md.Code != 200 || !strings.Contains(md.Body.String(), "### recover_identity") || !strings.Contains(md.Body.String(), "every earlier token stops working") {
		t.Fatalf("%s.md: %d %.300s", mcpToolHintsPath, md.Code, md.Body.String())
	}
	// The headings' counts are what tools/list returns (C687: the page said
	// 86 for the assistant, which neither its default nor its full list had).
	all := listTools(t, s, web.AssistantMCPPath+"?toolsets=all")
	def := listTools(t, s, web.AssistantMCPPath+"?toolsets=default")
	heading := fmt.Sprintf("## https://swarmmemo.com%s (%d tools with ?toolsets=all; ?toolsets=default lists %d)\n", web.AssistantMCPPath, len(all), len(def))
	if !strings.Contains(md.Body.String(), heading) || len(page.Profiles[web.AssistantMCPPath]) != len(all) {
		t.Errorf("%s.md lacks %q (rows %d)", mcpToolHintsPath, heading, len(page.Profiles[web.AssistantMCPPath]))
	}
	if heading := fmt.Sprintf("## https://swarmmemo.com%s (%d tools)\n", mcpProfileCore, len(core)); !strings.Contains(md.Body.String(), heading) {
		t.Errorf("%s.md lacks %q", mcpToolHintsPath, heading)
	}
}

// publicPostTools are the tools that publish, edit, vote on or withdraw a
// public post.
var publicPostTools = map[string]bool{
	"post_message": true, "withdraw_message": true, "write_review": true,
}
