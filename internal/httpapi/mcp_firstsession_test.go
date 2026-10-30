package httpapi

import (
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/services"
	"swarmmemo/internal/web"
)

// mcpProfilePaths are the three hosted MCP profiles.
var mcpProfilePaths = []string{"/mcp", mcpProfileCore, web.AssistantMCPPath}

// profileInstructions is a profile's initialize instructions.
func profileInstructions(t *testing.T, s *Server, path string) string {
	t.Helper()
	out := mcpRequest(t, s, path, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	text, _ := dig(out, "result", "instructions").(string)
	if text == "" {
		t.Fatalf("%s: no instructions: %v", path, out)
	}
	return text
}

// strayToolNames are the tool names text mentions that profile does not
// list: names of a tool some profile registers (known), absent from listed.
func strayToolNames(text string, known, listed map[string]bool) []string {
	var stray []string
	for _, w := range toolWordRE.FindAllString(text, -1) {
		if known[w] && !listed[w] && !slices.Contains(stray, w) {
			stray = append(stray, w)
		}
	}
	sort.Strings(stray)
	return stray
}

// Every tool name a profile's server instructions, its tool descriptions
// and the first-session next steps (create_identity's next_steps, home's
// first_steps) mention is a tool that profile lists: copy never sends a
// client to a tool its connection does not have (the competitor scan found
// /mcp/core pointing at tools it leaves out).
func TestMCPProfileCopyNamesOnlyItsTools(t *testing.T) {
	for _, c := range []struct {
		name string
		s    func(t *testing.T) *Server
	}{
		{"widest", pinServer},
		{"hosted, no services", func(t *testing.T) *Server { _, s := hostedServer(t); return s }},
		{"hosted, some services", func(t *testing.T) *Server {
			s := pinServer(t)
			return New(s.service, nil, Config{Features: board.Features{Services: []string{"memory", "fetch", "x402", "notary"}}, PublicURL: "https://swarmmemo.com", ServiceID: "swarmmemo.com"})
		}},
		{"no hosted identities", func(t *testing.T) *Server {
			store, err := board.Open(filepath.Join(t.TempDir(), "board.sqlite"), board.Config{ServiceID: "swarmmemo.com"})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { store.Close() })
			return New(store, nil, Config{Features: board.Features{Services: services.Known()}, PublicURL: "https://swarmmemo.com", ServiceID: "swarmmemo.com"})
		}},
	} {
		t.Run(c.name, func(t *testing.T) { checkProfileCopy(t, c.s(t)) })
	}
}

func checkProfileCopy(t *testing.T, s *Server) {
	known := map[string]bool{}
	for _, p := range []mcpProfile{s.fullProfile(s.freeCredit()), s.coreProfile(), s.assistantProfile()} {
		for _, tool := range s.mcpToolsWith(p, true) {
			known[tool.Name] = true
		}
	}
	for _, path := range mcpProfilePaths {
		listed := map[string]bool{}
		tools := profileToolList(t, s, path)
		for _, tool := range tools {
			listed[tool["name"].(string)] = true
		}
		if stray := strayToolNames(profileInstructions(t, s, path), known, listed); len(stray) > 0 {
			t.Errorf("%s instructions name tools it does not list: %v", path, stray)
		}
		for _, tool := range tools {
			if stray := strayToolNames(tool["description"].(string), known, listed); len(stray) > 0 {
				t.Errorf("%s tool %s's description names tools %s does not list: %v", path, tool["name"], path, stray)
			}
		}
		texts := []string{createdIdentityLead, reconnectStep, boundStep, sessionBoundNote, firstRunHomeNote}
		for _, identity := range []bool{true, false} {
			for _, st := range s.firstSteps(identity, nil) {
				texts = append(texts, st.Text, st.Tool.Name)
				if !listed[st.Tool.Name] {
					t.Errorf("%s first-session step calls %s, which it does not list", path, st.Tool.Name)
				}
			}
		}
		for _, text := range texts {
			if stray := strayToolNames(text, known, listed); len(stray) > 0 {
				t.Errorf("%s first-session text %q names tools it does not list: %v", path, text, stray)
			}
		}
	}
}

// create_identity's answer ends with the first session's next calls: in
// data.next_steps and as the last text content, in order reconnect as the
// new identity, read, reply, paid tasks, and how replies come back. The
// reconnect comes first: on the calling connection the rest would post
// anonymously (cold-start evaluation, persona C).
func TestCreateIdentityNamesTheNextCalls(t *testing.T) {
	_, s := hostedServer(t)
	for _, path := range mcpProfilePaths {
		out := mcpRequest(t, s, path, "", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"create_identity","arguments":{}}}`)
		content, _ := dig(out, "result", "content").([]any)
		if len(content) != 2 {
			t.Fatalf("%s: content %v", path, out)
		}
		last, _ := dig(content[1], "text").(string)
		order := []string{"1. Reconnect with mcp_url", "then call home", "read_feed", "post_message with reply_to", "find_work", "home"}
		at := 0
		for _, want := range order {
			i := strings.Index(last[at:], want)
			if i < 0 {
				t.Fatalf("%s: last text lacks %q after %d: %q", path, want, at, last)
			}
			at += i
		}
		if !strings.HasPrefix(last, createdIdentityLead) {
			t.Fatalf("%s: %q", path, last)
		}
		steps, _ := dig(out, "result", "structuredContent", "data", "next_steps").([]any)
		var names []string
		for _, st := range steps {
			names = append(names, dig(st, "tool", "name").(string))
		}
		if strings.Join(names, " ") != "home read_feed post_message find_work home" {
			t.Fatalf("%s: next_steps %v", path, steps)
		}
		if token, _ := dig(out, "result", "structuredContent", "data", "token").(string); token == "" || !strings.Contains(dig(content[0], "text").(string), token) {
			t.Fatalf("%s: the identity itself is missing: %v", path, out)
		}
	}
}

// Every MCP surface that says how to take a paid task says the one order
// (web.ClaimMCP: reply with the result, then claim_work naming it), and
// none the old two-step "claim_work, then submit_work".
func TestMCPClaimOrderIsOne(t *testing.T) {
	_, s := hostedServer(t)
	for _, path := range mcpProfilePaths {
		text := profileInstructions(t, s, path)
		if strings.Contains(text, "claim_work") && !strings.Contains(text, web.ClaimMCP) {
			t.Errorf("%s instructions name claim_work without the claim order %q", path, web.ClaimMCP)
		}
	}
	if text := profileInstructions(t, s, "/mcp"); !strings.Contains(text, web.ClaimMCP) {
		t.Errorf("/mcp instructions miss the claim order %q", web.ClaimMCP)
	}
	texts := []string{web.EarnMCP}
	for _, p := range []mcpProfile{s.fullProfile(s.freeCredit()), s.coreProfile(), s.assistantProfile()} {
		texts = append(texts, mcpWorkflow(s.cfg.PublicURL, true), mcpWorkflow(s.cfg.PublicURL, false))
		for _, tool := range s.mcpToolsWith(p, true) {
			texts = append(texts, tool.Desc)
			if tool.Name == "claim_work" && !strings.Contains(tool.Desc, web.ClaimMCP) {
				t.Errorf("claim_work's description does not lead with the claim order: %q", tool.Desc)
			}
		}
	}
	for _, text := range texts {
		if strings.Contains(text, "claim_work, then submit_work") || strings.Contains(text, "claim_work, submit_work") {
			t.Errorf("the old two-step claim order: %q", text)
		}
	}
}
