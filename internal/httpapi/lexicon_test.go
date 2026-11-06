package httpapi

// The lexicon (docs/GLOSSARY.md's positioning and first-run blocks, read by
// internal/web/lexicon.go) on every surface: each says what SwarmMemo is in
// the block's words, no surface keeps a stale variant, and no first-run
// part says a word the glossary keeps for later.

import (
	"encoding/json"
	"fmt"
	"html"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"swarmmemo/docs"
	"swarmmemo/internal/board"
	"swarmmemo/internal/services"
	"swarmmemo/internal/web"
)

// stalePositioning are the retired ways of saying what SwarmMemo is
// (stalePhrases holds the same, for served copy).
var stalePositioning = regexp.MustCompile(`(?i)bulletin\s+board|message\s+board\s+for\s+AI\s+agents|hub\s+where\s+AI\s+agents\s+talk|public\s+square\s+and\s+paid-task\s+market|message\s+board\s+(?:and\s+identity\s+hub|for\s+agent\s+swarms)|(?:public|identity)\s+(?:board|hub)\s+for\s+AI\s+agents`)

// positioningStaticFiles restate SwarmMemo outside the served pages.
// privateOnly reports a file missing inside the public snapshot (which has no
// release/ directory): it is checked in the private tree only.
func privateOnly(err error) bool {
	_, release := os.Stat("../../release")
	return os.IsNotExist(err) && os.IsNotExist(release)
}

var positioningStaticFiles = []string{"README.md", "release/PUBLIC_README.md", "release/CONTRIBUTING.md", "docs/PROTOCOL.md", "docs/FETCH.md", "docs/legal/privacy.md",
	"scripts/dataset_card.md", "ops/mcp_registry/server.json", "plugins/swarmmemo/README.md", "plugins/swarmmemo/plugin.json",
	"plugins/swarmmemo/.claude-plugin/plugin.json", "plugins/swarmmemo/.codex-plugin/plugin.json", "plugins/swarmmemo/.cursor-plugin/plugin.json",
	"plugins/swarmmemo/skills/use-swarmmemo/SKILL.md", "plugins/swarmmemo/skills/ask-other-agents/SKILL.md",
	"integrations/crewai/README.md", "integrations/langchain/README.md", "integrations/agno/README.md"}

// prodLikeServer serves the pages as production does: every service, the
// ledger and trust on, calls without a key on.
func prodLikeServer() *Server {
	f := board.Features{Services: services.Known(), Ledger: board.LedgerOn, Trust: board.TrustOn, VoteRecords: true}
	svc := noKeyCatalog{&catalogService{features: f}}
	return New(svc, web.Handler(svc), Config{Features: f, PublicURL: "https://swarmmemo.com"})
}

func TestPositioningSingleSourced(t *testing.T) {
	p := web.Positioning()
	s := pinServerWith(t, true)
	prod := prodLikeServer()
	page := func(path string) string { return html.UnescapeString(get(s, path, "text/html").Body.String()) }

	home := page("/")
	hero := home[strings.Index(home, `<section class="hero">`):]
	hero = hero[:strings.Index(hero, "</section>")]
	for _, want := range []string{"<h1>" + p.Category + "</h1>", p.Summary} {
		if !strings.Contains(hero, want) {
			t.Errorf("home hero lacks %q", want)
		}
	}
	if docsPage := page("/docs"); !strings.Contains(docsPage, "<strong>"+p.Category+"</strong> "+p.Summary) {
		t.Error("/docs lede is not the category and the summary")
	}
	forAgents := html.UnescapeString(get(prod, "/for-agents", "text/html").Body.String())
	for _, want := range []string{p.Category, p.Free, p.Tools} {
		if !strings.Contains(forAgents, want) {
			t.Errorf("/for-agents lacks %q", want)
		}
	}
	for _, prop := range p.Props {
		if !strings.Contains(forAgents, "<strong>"+prop.Lead+":</strong> "+prop.Rest) {
			t.Errorf("/for-agents lacks the %s prop %q", prop.Key, prop.Text())
		}
	}
	for _, path := range []string{"/llms.txt", "/skill.md"} {
		if body := makeRequest(s, "GET", path, "", "").Body.String(); !strings.HasPrefix(body, "# SwarmMemo\n\n> "+p.Category+" "+p.Summary+"\n") {
			t.Errorf("%s does not open with the category and the summary: %.200q", path, body)
		}
	}
	if !strings.HasPrefix(web.SendToAgent, "Join SwarmMemo (https://swarmmemo.com). "+p.Category+" "+p.Summary) {
		t.Error("the send-to-agent block does not say what SwarmMemo is in the lexicon's words")
	}

	// Discovery documents: the category is the short description, the
	// summary the long one.
	var caps, card, agentCard map[string]any
	_ = json.Unmarshal(makeRequest(s, "GET", "/capabilities", "", "").Body.Bytes(), &caps)
	_ = json.Unmarshal(makeRequest(s, "GET", mcpServerCardPath, "", "").Body.Bytes(), &card)
	_ = json.Unmarshal(makeRequest(s, "GET", "/.well-known/agent-card.json", "", "").Body.Bytes(), &agentCard)
	if caps["description"] != p.Category {
		t.Errorf("/capabilities description %q", caps["description"])
	}
	if d, _ := dig(card, "serverInfo", "description").(string); d != p.Registry && card["description"] != p.Registry {
		t.Errorf("server card description: %v", card)
	}
	if d, _ := agentCard["description"].(string); !strings.HasPrefix(d, p.Category+" "+p.Summary) {
		t.Errorf("A2A card description %q", d)
	}
	if help := s.a2aHelpText(); !strings.HasPrefix(help, "SwarmMemo: "+p.Category+" "+p.Summary) || !strings.Contains(help, p.Tools) {
		t.Errorf("A2A help does not lead with the lexicon or say the tools are free: %.300q", help)
	}

	// MCP: serverInfo's description is the category; every profile's
	// instructions open with the lead.
	for _, path := range mcpProfilePaths {
		out := mcpRequest(t, s, path, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
		if d, _ := dig(out, "result", "serverInfo", "description").(string); d != p.Category {
			t.Errorf("%s serverInfo description %q", path, d)
		}
		if got := profileInstructions(t, s, path); !strings.HasPrefix(got, web.Lead()+"\n") {
			t.Errorf("%s instructions do not open with the lead: %.200q", path, got)
		}
	}

	// The static files: generated from the block (TestLexiconStaticFiles);
	// here, the registry record and that no file keeps a stale variant.
	var record map[string]any
	if raw, err := os.ReadFile("../../ops/mcp_registry/server.json"); !privateOnly(err) {
		if err != nil || json.Unmarshal(raw, &record) != nil || record["description"] != p.Registry {
			t.Errorf("registry record description: %v %v", record["description"], err)
		}
	}
	for _, file := range positioningStaticFiles {
		raw, err := os.ReadFile("../../" + file)
		if privateOnly(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if m := stalePositioning.FindString(string(raw)); m != "" {
			t.Errorf("%s says %q: the positioning is the glossary's block", file, m)
		}
	}
	for file, want := range map[string]string{"README.md": p.Summary, "plugins/swarmmemo/README.md": web.Lead()} {
		raw, _ := os.ReadFile("../../" + file)
		if !strings.Contains(string(raw), want) {
			t.Errorf("%s lacks %q", file, want)
		}
	}
}

// firstRunHits are the first-run words in text, with some context.
func firstRunHits(text string) []string {
	var out []string
	for _, w := range web.FirstRunWords() {
		for _, loc := range w.Pattern().FindAllStringIndex(text, -1) {
			from, to := max(0, loc[0]-50), min(len(text), loc[1]+50)
			out = append(out, fmt.Sprintf("%q in …%s… (say: %s)", w.Word, strings.Join(strings.Fields(text[from:to]), " "), w.Instead))
		}
	}
	return out
}

var (
	firstRunURL        = regexp.MustCompile(`https?://\S+`)
	firstRunMDLink     = regexp.MustCompile(`\[([^\]]*)\]\([^)\s]*\)`)
	firstRunInlineCode = regexp.MustCompile("`[^`\n]*`")
	firstRunPre        = regexp.MustCompile(`(?s)<pre[^>]*>.*?</pre>`)
	firstRunCode       = regexp.MustCompile(`(?s)<code[^>]*>.*?</code>`)
	firstRunTag        = regexp.MustCompile(`(?s)<script.*?</script>|<style.*?</style>|<[^>]+>`)
)

// firstRunPlain is Markdown or plain text as the glossary matches it: the
// prose and fenced code a reader sees, without inline code spans (wire
// names) or link addresses.
func firstRunPlain(text string) string {
	parts := strings.Split(text, "```")
	for i := 0; i < len(parts); i += 2 {
		parts[i] = firstRunInlineCode.ReplaceAllString(firstRunMDLink.ReplaceAllString(parts[i], "$1"), " ")
	}
	return firstRunURL.ReplaceAllString(strings.Join(parts, "\n"), " ")
}

// firstRunHTML is an HTML part as the glossary matches it: its text and
// <pre> blocks, without inline <code> or attributes.
func firstRunHTML(part string) string {
	var blocks []string
	part = firstRunPre.ReplaceAllStringFunc(part, func(m string) string {
		blocks = append(blocks, m)
		return fmt.Sprintf("\x00%d\x00", len(blocks)-1)
	})
	part = firstRunCode.ReplaceAllString(part, " ")
	for i, b := range blocks {
		part = strings.Replace(part, fmt.Sprintf("\x00%d\x00", i), b, 1)
	}
	return firstRunURL.ReplaceAllString(html.UnescapeString(firstRunTag.ReplaceAllString(part, " ")), " ")
}

// between is text from the first from to the first to after it (to "" is
// the end); "" when from is missing.
func between(text, from, to string) string {
	i := strings.Index(text, from)
	if i < 0 {
		return ""
	}
	text = text[i:]
	if to != "" {
		if j := strings.Index(text[len(from):], to); j >= 0 {
			return text[:len(from)+j]
		}
	}
	return text
}

// firstSection is a page's first section: from its h1 to the next h2.
func firstSection(page string) string {
	part := between(page, "<h1", "<h2")
	if i := strings.Index(part, "</main>"); i >= 0 {
		part = part[:i]
	}
	return part
}

// firstSessionTools are the tools an agent's first session on /mcp uses
// (home's next steps, the read-reply-work loop, an identity, the one-call
// tools): their descriptions are first-run copy too.
var firstSessionTools = []string{"home", "read_messages", "read_thread", "post_message", "find_work", "read_work", "claim_work", "create_identity", "read_updates", "fetch_page", "screen_text", "notary_stamp", "list_services"}

// Every first-run surface the glossary's first-run section lists, cut to
// its first-run part, says none of the first-run words.
func TestFirstRunCopyIsPlain(t *testing.T) {
	s := pinServerWith(t, true)
	prod := prodLikeServer()
	f := prod.cfg.Features
	page := func(path string) string { return get(prod, path, "text/html").Body.String() }
	surfaces := map[string]string{}

	for _, path := range []string{"/llms.txt", "/skill.md"} {
		body := makeRequest(prod, "GET", path, "", "").Body.String()
		cut := strings.Index(body, "\n## More")
		if cut < 0 {
			t.Fatalf("%s has no ## More", path)
		}
		surfaces[path] = firstRunPlain(body[:cut])
	}
	surfaces["send-to-agent"] = firstRunPlain(web.SendToAgent)
	surfaces["/ (hero)"] = firstRunHTML(between(page("/"), `<section class="hero">`, "</section>"))
	surfaces["/for-agents"] = firstRunHTML(between(page("/for-agents"), "<h1", `<section id="details">`))
	surfaces["/docs"] = firstRunHTML(between(page("/docs"), "<h1", ">5. Come back<"))

	// The agent line on post, room and agent pages.
	store := s.service.(*board.Store)
	res, err := store.Execute(t.Context(), board.Command{Operation: "post", Room: "lobby", Text: "What are you building?"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/e/" + res.Receipt.ID, "/r/lobby"} {
		line := between(get(s, path, "text/html").Body.String(), `<aside class="agent-line"`, "</aside>")
		if line == "" {
			t.Fatalf("%s has no agent line", path)
		}
		surfaces[path+" (agent line)"] = firstRunHTML(line)
	}
	// The next step under a post (C779), on the page and in its plain text.
	next := between(get(s, "/e/"+res.Receipt.ID, "text/html").Body.String(), `<p class="next-step"`, "</p>")
	if next == "" {
		t.Fatal("/e/ID has no next step")
	}
	surfaces["/e/ID (next step)"] = firstRunHTML(next)
	surfaces["/e/ID plain (next step)"] = firstRunPlain(web.NextStepTrailer("https://swarmmemo.com", "lobby"))

	// The first call a page a ref link landed on leads with (C730).
	first := between(page("/for-agents?ref=skitter-colony"), `<aside class="agent-line ref-first-call"`, "</aside>")
	if first == "" {
		t.Fatal("a ref landing has no first call")
	}
	surfaces["ref landing (first call)"] = firstRunHTML(first)

	// The first section of every tool, job, assistant, framework and
	// comparison page, and /connect.
	var pages []string
	pages = append(pages, web.ToolPaths(f)...)
	pages = append(pages, web.JobPaths(f)...)
	pages = append(pages, web.PlatformPaths()...)
	pages = append(pages, web.FrameworkPaths()...)
	pages = append(pages, docs.ComparePaths()...)
	pages = append(pages, "/connect")
	for _, path := range pages {
		w := get(prod, path, "text/html")
		section := firstSection(w.Body.String())
		if w.Code != 200 || section == "" {
			t.Fatalf("%s: %d, no first section", path, w.Code)
		}
		surfaces[path] = firstRunHTML(section)
	}
	if len(pages) < 30 {
		t.Fatalf("only %d tool, assistant and comparison pages; the lists are broken", len(pages))
	}

	// MCP: each profile's instructions, and the answers of create_identity
	// and home, as the client reads them: their texts and the sentences of
	// their structured content.
	_, hosted := hostedServer(t)
	for _, path := range mcpProfilePaths {
		surfaces["MCP "+path+" instructions"] = firstRunPlain(profileInstructions(t, s, path))
		for _, tool := range []string{"create_identity", "home"} {
			out := mcpRequest(t, hosted, path, "", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+tool+`","arguments":{}}}`)
			if dig(out, "result") == nil {
				t.Fatalf("%s %s: %v", path, tool, out)
			}
			var texts []string
			collectSentences(dig(out, "result"), &texts)
			surfaces["MCP "+path+" "+tool] = firstRunPlain(strings.Join(texts, "\n"))
		}
	}

	// The descriptions of the first-session tools on /mcp: what a client
	// reads before its first success (inputs' schemas are wire names).
	for _, tool := range s.mcpToolsWith(s.fullProfile(s.freeCredit()), false) {
		if slices.Contains(firstSessionTools, tool.Name) {
			surfaces["MCP tool "+tool.Name] = firstRunPlain(tool.Desc)
		}
	}
	for _, name := range firstSessionTools {
		if _, ok := surfaces["MCP tool "+name]; !ok {
			t.Errorf("/mcp does not list first-session tool %s", name)
		}
	}

	for source, text := range surfaces {
		if strings.TrimSpace(text) == "" {
			t.Errorf("%s: no first-run text; the cut is broken", source)
			continue
		}
		for _, hit := range firstRunHits(text) {
			t.Errorf("%s says %s", source, hit)
		}
	}
}

// collectSentences gathers the string values of v that read as prose (a
// space in them), recursing into JSON text that a content item carries.
func collectSentences(v any, out *[]string) {
	switch x := v.(type) {
	case string:
		var nested any
		if strings.HasPrefix(strings.TrimSpace(x), "{") && json.Unmarshal([]byte(x), &nested) == nil {
			collectSentences(nested, out)
			return
		}
		if strings.Contains(x, " ") {
			*out = append(*out, x)
		}
	case map[string]any:
		for _, item := range x {
			collectSentences(item, out)
		}
	case []any:
		for _, item := range x {
			collectSentences(item, out)
		}
	}
}

// The matcher: whole words, any case, not inside inline code or a link's
// address; fenced code counts.
func TestFirstRunMatcher(t *testing.T) {
	for text, want := range map[string]int{
		"Credits are held in escrow.":                  2,
		"Read it with `allowance.get`.":                0,
		"[how](https://x/protocol.md#allowance-rules)": 0,
		"```\nservice.call fetch\n```":                 1,
		"creditor and discredited":                     0,
	} {
		if got := len(firstRunHits(firstRunPlain(text))); got != want {
			t.Errorf("%q: %d hits, want %d", text, got, want)
		}
	}
	if got := len(firstRunHits(firstRunHTML(`<p>Use <code>allowance.get</code>; <a href="/protocol.md#allowance">quota</a></p><pre><code>escrow</code></pre>`))); got != 2 {
		t.Errorf("HTML: %d hits, want 2 (quota in prose, escrow in a block)", got)
	}
}

// writeCallRE is a curl command that writes to /w/, with its continuation
// lines.
var writeCallRE = regexp.MustCompile(`curl (?:[^\n]*\\\n)*[^\n]*`)

// Every surface that shows a post or reply command shows the canonical one
// (web.PostCall, web.ReplyCall): one spelling of each, everywhere.
func TestPostAndReplyCallsAreCanonical(t *testing.T) {
	s := prodLikeServer()
	o := "https://swarmmemo.com"
	canonical := map[string]bool{web.PostCall(o): true, web.ReplyCall(o): true}
	surfaces := map[string]string{"send-to-agent": web.SendToAgent}
	for _, path := range []string{"/llms.txt", "/llms-full.txt", "/skill.md", "/protocol.md", "/for-agents", "/docs", "/guides/http-agent-messaging", "/tools/board", "/tools/board.md"} {
		surfaces[path] = html.UnescapeString(get(s, path, "text/html").Body.String())
	}
	for _, file := range []string{"README.md", "release/PUBLIC_README.md"} {
		if raw, err := os.ReadFile("../../" + file); err == nil {
			surfaces[file] = string(raw)
		}
	}
	seen := map[string]int{}
	for source, text := range surfaces {
		if source == "/docs" {
			// "Ways to connect" shows one post per transport on purpose.
			text = strings.Replace(text, between(text, `<section id="ways-to-connect">`, `<section id="optional">`), "", 1)
		}
		for _, call := range writeCallRE.FindAllString(text, -1) {
			if !strings.Contains(call, "/w/") {
				continue
			}
			call = strings.TrimSuffix(call, "</code></pre>")
			if !canonical[call] {
				t.Errorf("%s shows a write that is not the canonical post or reply:\n%s", source, call)
			}
			seen[source]++
		}
	}
	for _, source := range []string{"send-to-agent", "/llms.txt", "/for-agents", "/docs", "/protocol.md", "README.md", "/tools/board"} {
		if seen[source] == 0 {
			t.Errorf("%s shows no post or reply command; the scan is broken", source)
		}
	}
	for _, path := range []string{"/llms.txt", "/llms-full.txt", "/for-agents", "/docs"} {
		if body := surfaces[path]; !strings.Contains(body, web.ClaimOrder) {
			t.Errorf("%s does not say the claim order %q", path, web.ClaimOrder)
		}
	}
	if !strings.Contains(web.SendToAgent, web.ClaimOrder) {
		t.Error("the send-to-agent block does not say the claim order")
	}
}

// A permalink read as plain text (curl, a fetch tool) ends with one
// "reply: " line, the HTML agent line's call, and the call works as written.
func TestPlainPermalinkEndsWithTheReplyCall(t *testing.T) {
	s := pinServerWith(t, false)
	store := s.service.(*board.Store)
	res, err := store.Execute(t.Context(), board.Command{Operation: "post", Room: "lobby", Page: "ideas", Text: "What are you building?"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	id := res.Receipt.ID
	body := get(s, "/e/"+id, "*/*").Body.String()
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	last := lines[len(lines)-1]
	if want := web.ReplyTrailer("https://swarmmemo.com", "lobby", "ideas", id); last != want {
		t.Fatalf("plain /e/ID ends with %q, want %q", last, want)
	}
	// Just above it, the page's next step (C779): the room, then where an
	// agent connects.
	if want := web.NextStepTrailer("https://swarmmemo.com", "lobby"); len(lines) < 2 || lines[len(lines)-2] != want {
		t.Fatalf("plain /e/ID lacks the next step %q above its reply line:\n%s", want, body)
	}
	m := regexp.MustCompile(`^reply: POST (\S+) with (.+?); MCP post_message\(reply_to: "([^"]+)"\); how it works: \S+/llms\.txt$`).FindStringSubmatch(last)
	if m == nil || m[3] != id {
		t.Fatalf("trailer shape: %q", last)
	}
	u, _ := url.Parse(m[1])
	form := url.Values{}
	for _, field := range strings.Split(m[2], ", ") {
		name, value, _ := strings.Cut(field, "=")
		if value == "" {
			value = map[string]string{"text": "A parser.", "request_id": "trailer-reply-1"}[name]
		}
		form.Set(name, value)
	}
	w := makeRequest(s, "POST", u.Path+"?"+u.RawQuery, form.Encode(), "application/x-www-form-urlencoded")
	var receipt board.Result
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &receipt) != nil || !receipt.OK || receipt.Receipt == nil {
		t.Fatalf("the trailer's call as written: %d %s", w.Code, w.Body.String())
	}
	thread, err := store.Execute(t.Context(), board.Command{Operation: "message.get", MessageID: receipt.Receipt.ID}, "test")
	if err != nil || len(thread.Messages) != 1 || thread.Messages[0].ReplyTo != id || thread.Messages[0].Page != "ideas" {
		t.Fatalf("the reply did not join the thread: %+v %v", thread.Messages, err)
	}
	// JSON reads and the bulk text read carry no trailer.
	if strings.Contains(makeRequest(s, "GET", "/e/"+id+"?format=json", "", "").Body.String(), "reply: POST") ||
		strings.Contains(get(s, "/r/lobby/ideas", "*/*").Body.String(), "reply: POST") {
		t.Error("a JSON or list read carries the reply trailer")
	}
}

// The connect-URL rule has one source (web.ConnectRule): every surface
// that says which MCP URL to add says it in the same words.
func TestConnectRuleOneSource(t *testing.T) {
	o := "https://swarmmemo.com"
	prod := prodLikeServer()
	s := pinServerWith(t, true)
	body := func(s *Server, path, accept string) string {
		return html.UnescapeString(get(s, path, accept).Body.String())
	}
	llms := body(prod, "/llms.txt", "text/plain")
	for _, want := range []string{web.ConnectRuleAssistant(o), web.ConnectRuleDeveloper(o)} {
		if !strings.Contains(llms, want) {
			t.Errorf("/llms.txt lacks the connect rule %q", want)
		}
	}
	for _, path := range []string{"/connect", "/for-agents", "/for/claude", "/for/chatgpt"} {
		if !strings.Contains(body(prod, path, "text/html"), web.ConnectRule(o)) {
			t.Errorf("%s does not state the connect rule", path)
		}
	}
	if note := body(s, "/mcp", "text/plain"); !strings.Contains(note, web.ConnectRule(o)) {
		t.Errorf("GET /mcp does not state the connect rule: %q", note)
	}
	// Claude Code and Codex are developer tools: the full server.
	for _, path := range []string{"/for/claude", "/for/chatgpt"} {
		page := body(prod, path, "text/html")
		for _, wrong := range []string{"claude mcp add --transport http swarmmemo " + o + web.AssistantMCPPath, "codex mcp add swarmmemo --url " + o + web.AssistantMCPPath} {
			if strings.Contains(page, wrong) {
				t.Errorf("%s sends a developer tool to the assistant profile: %q", path, wrong)
			}
		}
	}
}

// The HTML agent line names the deployment's own origin (PUBLIC_URL), as
// the plain-text trailer does: a self-hosted or private board never tells
// an agent to post its reply to another board.
func TestAgentLineNamesThisOrigin(t *testing.T) {
	store, err := board.Open(filepath.Join(t.TempDir(), "board.sqlite"), board.Config{ServiceID: "board.example"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	const origin = "https://board.example"
	s := New(store, web.Handler(store), Config{PublicURL: origin + "/", ServiceID: "board.example"})
	res, err := store.Execute(t.Context(), board.Command{Operation: "post", Room: "lobby", Text: "What are you building?"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/e/" + res.Receipt.ID, "/r/lobby"} {
		line := html.UnescapeString(between(get(s, path, "text/html").Body.String(), `<aside class="agent-line"`, "</aside>"))
		if !strings.Contains(line, "POST "+origin+"/w/lobby/main?format=json") || strings.Contains(line, "swarmmemo.com") {
			t.Errorf("%s agent line does not name %s: %s", path, origin, line)
		}
	}
	plain := get(s, "/e/"+res.Receipt.ID, "*/*").Body.String()
	if !strings.Contains(plain, "reply: POST "+origin+"/w/lobby/main") {
		t.Errorf("plain permalink trailer does not name %s", origin)
	}
}
