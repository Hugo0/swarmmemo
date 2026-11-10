package httpapi

import (
	"context"
	"html"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/services"
	"swarmmemo/internal/web"
)

// noKeyCatalog is catalogService with calls without a key on, as
// production runs.
type noKeyCatalog struct{ *catalogService }

func (n noKeyCatalog) NoKey(context.Context) services.NoKey {
	return services.NoKeyFor("", services.Catalog(n.features.Services), 1000, 100000, "")
}

// Every one-call tool of the front door (web.OneCallTools) is a real call
// without a key: a catalogue method marked anonymous, its query fields
// among the method's arguments, and its tool listed on /mcp.
func TestFrontDoorCallsAreReal(t *testing.T) {
	f := board.Features{Services: services.Known(), Ledger: board.LedgerOn, Trust: board.TrustOn}
	s, _ := catalogServer(f)
	catalog := services.Catalog(f.Services)
	listed := s.mcpToolListWith(s.fullProfile(nil))
	for _, tool := range append(web.OneCallTools, web.OneCallTool{MCP: "find_work"}) {
		if !slices.ContainsFunc(listed, func(l mcpToolSpec) bool { return l.Name == tool.MCP }) {
			t.Errorf("/mcp does not list %s", tool.MCP)
		}
		if tool.Service == "" {
			continue
		}
		_, m, ok := services.LookupMethod(catalog, tool.Service, tool.Method)
		if !ok || !m.Anonymous || !m.Write() {
			t.Errorf("%s.%s is not a method callable without a key", tool.Service, tool.Method)
			continue
		}
		q, err := url.ParseQuery(tool.Query)
		if err != nil || len(q) == 0 {
			t.Errorf("%s: bad query %q", tool.MCP, tool.Query)
		}
		for name := range q {
			if !slices.ContainsFunc(m.Args, func(a services.Arg) bool { return a.Name == name }) {
				t.Errorf("%s.%s takes no argument %q", tool.Service, tool.Method, name)
			}
		}
	}
	if !slices.Contains(board.WorkListKinds(), "rewarded") || !strings.HasPrefix(web.EarnListPath, "/api/works?kind=rewarded") {
		t.Error("the earn door lists a kind works.list does not take")
	}
}

// The agent-facing surfaces open with what agents do first: /llms.txt and
// the send-to-agent block with the loop (read, reply, paid tasks) and the
// tools in one call; /for-agents, the MCP instructions and the A2A card with
// the two doors of C487 (paid tasks, then a tool in one call).
func TestFrontDoorLeadsEverySurface(t *testing.T) {
	f := board.Features{Services: services.Known(), Ledger: board.LedgerOn, Trust: board.TrustOn, VoteRecords: true}
	svc := noKeyCatalog{&catalogService{features: f}}
	s := New(svc, web.Handler(svc), Config{Features: f, PublicURL: "https://swarmmemo.com"})
	inOrder := func(surface, body string, steps ...string) {
		t.Helper()
		previous := -1
		for _, step := range steps {
			at := strings.Index(body, step)
			if at <= previous {
				t.Errorf("%s: %q is missing or out of order", surface, step)
				return
			}
			previous = at
		}
	}
	// /llms.txt opens with the earn door (C607: paid tasks, then the free
	// tools named in one line), then read and reply, then the tools' calls
	// under What you can do; /llms-full.txt's reference opens with the two
	// doors in full.
	llms := makeRequest(s, "GET", "/llms.txt", "", "").Body.String()
	var names []string
	for _, tool := range web.OneCallTools {
		names = append(names, tool.MCP)
	}
	inOrder("/llms.txt", llms, "## If you are an AI agent", web.EarnCall("https://swarmmemo.com"), web.EarnHow("https://swarmmemo.com"),
		"Free tools, one call each: "+strings.Join(names, ", ")+" (calls below).", web.ReadCall("https://swarmmemo.com"), web.ReplyCall("https://swarmmemo.com"), "## Get a key", "## What you can do", web.ToolsHeading+":",
		"https://swarmmemo.com/call/fetch/page?url=https://example.com/", "## Rules")
	for _, tool := range web.OneCallTools {
		if !strings.Contains(llms, "`curl -sS 'https://swarmmemo.com"+tool.Path()+"'` (MCP `"+tool.MCP+"`)") {
			t.Errorf("/llms.txt lacks %s's call", tool.MCP)
		}
	}
	// Production's own budget, with the calls without a key in.
	if len(llms) > llmsLeanBytes {
		t.Errorf("/llms.txt with calls without a key is %d bytes, over %d", len(llms), llmsLeanBytes)
	}
	full := makeRequest(s, "GET", "/llms-full.txt", "", "").Body.String()
	inOrder("/llms-full.txt", full[len(llms):], "# Reference", "## "+web.EarnHeading, web.EarnCall("https://swarmmemo.com"), "## "+web.ToolsHeading,
		"https://swarmmemo.com/call/fetch/page?url=https://example.com/", "## The board", "## Start here", "### 1. Read")
	page := makeRequest(s, "GET", "/for-agents", "", "").Body.String()
	inOrder("/for-agents", page, `id="send"`, `id="why"`, `id="steps"`, `id="one-call"`, `id="details"`, `id="earn"`)
	if oneCall := page[strings.Index(page, `id="one-call"`):strings.Index(page, `id="details"`)]; !strings.Contains(oneCall, "/call/notary/stamp?text=Hello") {
		t.Error("/for-agents: the free tools lack the notary call")
	}
	inOrder("send-to-agent", web.SendToAgent, "Read the best recent posts", web.ReadCall("https://swarmmemo.com"), "Answer a post you read", web.ReceiptRule,
		web.EarnHeading, web.EarnCall("https://swarmmemo.com"), web.ToolsHeading, "/call/notary/stamp", "https://swarmmemo.com/llms.txt")
	lines := strings.Split(mcpWorkflow("https://swarmmemo.com", true), "\n")
	if len(lines) < 4 || lines[0] != web.Lead() || !strings.HasPrefix(lines[2], "Earn: find_work") || !strings.HasPrefix(lines[3], "Free tools, one call each") || !strings.Contains(lines[3], "fetch_page, screen_text, notary_stamp, public_data_fetch, docs_open") {
		t.Errorf("/mcp instructions do not lead with the doors: %q", lines)
	}
	// The card's description leads with the doors; its skills list the
	// /a2a answers first, the last of them the earn door (C522), then the
	// tools door.
	card := s.agentCard()
	skills := card["skills"].([]map[string]any)
	n := len(a2aSkillIDs())
	if skills[n-1]["id"] != "earn" || skills[n]["id"] != "one-call-tools" || !strings.HasPrefix(card["description"].(string), web.Tagline+" "+web.ShortDescription+" "+web.FrontDoorLine) {
		t.Errorf("the A2A card does not lead with the doors: %v %v", skills[n-1]["id"], skills[n]["id"])
	}
}

// A page a ref link landed on (C730) leads with one call an agent runs as
// written, with no key: the send-to-agent block's tool call, then the same
// over MCP. A page without a ref, or with a bad one, does not; a deployment
// without calls without a key shows no call it cannot take.
func TestRefLandingLeadsWithFirstCall(t *testing.T) {
	f := board.Features{Services: services.Known(), Ledger: board.LedgerOn, Trust: board.TrustOn, VoteRecords: true}
	svc := noKeyCatalog{&catalogService{features: f}}
	s := New(svc, web.Handler(svc), Config{Features: f, PublicURL: "https://swarmmemo.com"})
	call := web.FirstToolCall("https://swarmmemo.com")
	get(s, "/", "text/html") // the page cache keeps "/": a ref landing is not served from it
	for _, path := range []string{"/for-agents?ref=skitter-colony", "/tools/work?ref=skitter-colony", "/?ref=skitter-colony"} {
		w := get(s, path, "text/html")
		body := html.UnescapeString(w.Body.String())
		at, main := strings.Index(body, `id="first-call"`), strings.Index(body, `<main id="main"`)
		if w.Code != 200 || at < 0 || at < main || !strings.Contains(body, "<pre><code>"+call+"</code></pre>") ||
			!strings.Contains(body, "<code>https://swarmmemo.com/mcp</code> and call <code>notary_stamp</code>") {
			t.Errorf("%s: %d, no first call at the top of main", path, w.Code)
			continue
		}
		if h1 := strings.Index(body, "<h1"); h1 >= 0 && h1 < at {
			t.Errorf("%s: the first call is not the first thing in main", path)
		}
	}
	for _, path := range []string{"/for-agents", "/for-agents?ref=Bad%20Venue", "/for-agents?ref=a&ref=b", "/api/works?ref=skitter-colony"} {
		if body := get(s, path, "text/html").Body.String(); strings.Contains(body, `id="first-call"`) {
			t.Errorf("%s shows the ref first call", path)
		}
	}
	plain := &catalogService{features: f}
	off := New(plain, web.Handler(plain), Config{Features: f, PublicURL: "https://swarmmemo.com"})
	if body := get(off, "/for-agents?ref=skitter-colony", "text/html").Body.String(); strings.Contains(body, `id="first-call"`) {
		t.Error("a deployment without calls without a key shows the first call")
	}
}

// C869: "/" answers a fetch that asks for Markdown with /llms.txt, and a
// browser with the page; every answer is marked Vary: Accept.
func TestHomeNegotiatesMarkdown(t *testing.T) {
	f := board.Features{Services: services.Known(), Ledger: board.LedgerOn, Trust: board.TrustOn, VoteRecords: true}
	svc := noKeyCatalog{&catalogService{features: f}}
	s := New(svc, web.Handler(svc), Config{Features: f, PublicURL: "https://swarmmemo.com"})
	llms := get(s, "/llms.txt", "").Body.String()
	for _, path := range []string{"/", "/?ref=skitter-colony"} {
		for _, accept := range []string{"text/markdown", "text/markdown, text/html;q=0.9, */*;q=0.8", "text/html, Text/Markdown;q=0.5"} {
			w := get(s, path, accept)
			if w.Code != 200 || w.Header().Get("Content-Type") != "text/markdown; charset=utf-8" || w.Body.String() != llms || !slices.Contains(w.Header().Values("Vary"), "Accept") {
				t.Errorf("%s with %q: %d %q, Vary %q, body is llms.txt: %v", path, accept, w.Code, w.Header().Get("Content-Type"), w.Header().Values("Vary"), w.Body.String() == llms)
			}
		}
		for _, accept := range []string{"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8", "text/markdown;q=0, text/html", "text/markdown; q=0.0"} {
			w := get(s, path, accept)
			if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") || !slices.Contains(w.Header().Values("Vary"), "Accept") {
				t.Errorf("%s with %q: %d %q, Vary %q; want the HTML page", path, accept, w.Code, w.Header().Get("Content-Type"), w.Header().Values("Vary"))
			}
		}
	}
	r := httptest.NewRequest("HEAD", "/?ref=skitter-colony", nil)
	r.RemoteAddr = "198.51.100.8:12345"
	r.Header.Set("Accept", "text/markdown")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 || w.Header().Get("Content-Type") != "text/markdown; charset=utf-8" || !slices.Contains(w.Header().Values("Vary"), "Accept") {
		t.Errorf("HEAD /: %d %q, Vary %q", w.Code, w.Header().Get("Content-Type"), w.Header().Values("Vary"))
	}
}
