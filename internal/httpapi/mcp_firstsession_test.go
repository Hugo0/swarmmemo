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
		// No session ID: the identity's JSON (C928), then the text that
		// says it is not active here (C878), then the steps.
		if len(content) != 3 || dig(content[1], "text") != unboundLead {
			t.Fatalf("%s: content %v", path, out)
		}
		last, _ := dig(content[2], "text").(string)
		order := []string{"1. Reconnect with mcp_url", "then call home", "2. Ask the agents here", "read_thread", "read_feed", "post_message with reply_to", "find_work", "home"}
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
		if strings.Join(names, " ") != "home post_message read_feed post_message find_work home" {
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
		if strings.Contains(text, "claim_work") && !strings.Contains(text, web.ClaimMCPProse) {
			t.Errorf("%s instructions name claim_work without the claim order %q", path, web.ClaimMCPProse)
		}
		// D56: the instructions say the claim in words, never as inline JSON.
		if strings.Contains(text, `claim_work {`) {
			t.Errorf("%s instructions carry an inline claim_work JSON example", path)
		}
	}
	if text := profileInstructions(t, s, "/mcp"); !strings.Contains(text, web.ClaimMCPProse) {
		t.Errorf("/mcp instructions miss the claim order %q", web.ClaimMCPProse)
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

// An own home with nothing in it leads with the ask (C1000): post_message
// to the lobby, on every profile.
func TestHomeEmptyAsksFirst(t *testing.T) {
	_, s := hostedServer(t)
	token := newIdentity(t, s, "")["token"].(string)
	for _, path := range mcpProfilePaths {
		got := mustTool(t, s, path, "Bearer "+token, "home", map[string]any{})
		steps, _ := dig(got, "data", "next_steps").([]any)
		if dig(got, "data", "scope") != "own" || len(steps) == 0 || dig(steps[0], "tool", "name") != "post_message" ||
			dig(steps[0], "tool", "arguments", "room") != board.DefaultRoom || dig(steps[0], "text") != askStepText {
			t.Fatalf("%s empty own home: %v", path, got)
		}
	}
}

// A top-level post's receipt names read_thread with its id, where its
// answers collect (C1000); a reply's names nothing. Signed and anonymous,
// on every profile.
func TestPostReceiptNamesReadThread(t *testing.T) {
	_, s := hostedServer(t)
	token := newIdentity(t, s, "")["token"].(string)
	questions := []string{"Which embedding model do you use for code search?", "How do you keep a long research task on track?", "What is your favourite way to test a parser?",
		"Has anyone benchmarked SQLite WAL under many readers?", "Where do you keep notes between sessions?", "Who here translates German legal text?"}
	answers := []string{"A small local one.", "A checklist file I reread.", "Fuzzing with a corpus.", "Yes, readers scale well.", "In my memory store.", "I can try a page."}
	n := 0
	for _, path := range mcpProfilePaths {
		for _, auth := range []string{"", "Bearer " + token} {
			post := mustTool(t, s, path, auth, "post_message", map[string]any{"room": "lobby", "text": questions[n]})
			id, _ := dig(post, "receipt", "id").(string)
			steps, _ := dig(post, "data", "next_steps").([]any)
			if id == "" || len(steps) != 1 || dig(steps[0], "tool", "name") != "read_thread" || dig(steps[0], "tool", "arguments", "message_id") != id || dig(steps[0], "text") != postNextText {
				t.Fatalf("%s (auth %t) top-level post: %v", path, auth != "", post)
			}
			reply := mustTool(t, s, path, auth, "post_message", map[string]any{"reply_to": id, "text": answers[n]})
			n++
			if dig(reply, "receipt", "id") == nil || dig(reply, "data", "next_steps") != nil {
				t.Fatalf("%s (auth %t) reply: %v", path, auth != "", reply)
			}
		}
	}
}

// create_identity on a sign-in (OAuth) connection creates nothing: it
// answers already_signed_in with the identity's fingerprint (C1000). A
// connection without one still creates.
func TestCreateIdentitySignedInRefuses(t *testing.T) {
	_, s := hostedServer(t)
	clientID := registerTestClient(t, s, testRedirect)
	tokens, _ := signIn(t, s, clientID)
	access := "Bearer " + tokens["access_token"].(string)
	me := mustTool(t, s, web.AssistantMCPPath, access, "whoami", map[string]any{})
	fp, _ := dig(me, "agent", "id").(string)
	_, failure := callTool(t, s, web.AssistantMCPPath, access, "create_identity", map[string]any{})
	if fp == "" || !strings.HasPrefix(failure, "409 already_signed_in") || !strings.Contains(failure, "signed in as "+fp+"; call home") {
		t.Fatalf("create_identity signed in (%s): %q", fp, failure)
	}
	if _, failure := callTool(t, s, web.AssistantMCPPath, "", "create_identity", map[string]any{}); failure != "" {
		t.Fatalf("create_identity without a sign-in: %q", failure)
	}
}
