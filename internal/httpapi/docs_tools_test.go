package httpapi

import (
	"os"
	"regexp"
	"strings"
	"testing"

	publicdocs "swarmmemo/docs"
	"swarmmemo/internal/web"
)

// The hosted MCP tools the guides and READMEs tell an assistant to call are
// ones the server registers, on a deployment with every service and hosted
// identities on, as in production (the 1.24 docs audit, T47). A renamed tool
// cannot linger in a published example.
func TestDocumentedMCPToolsExist(t *testing.T) {
	f := everyService
	f.Services = append(append([]string(nil), f.Services...), "fetch", "receiver")
	s := New(&fakeService{}, nil, Config{PublicURL: "https://swarmmemo.com", Features: f})
	_, hosted := hostedServer(t)
	_, receivers := receiverServer(t)
	tools := map[string]bool{}
	withServices, _ := hostedServicesServer(t)
	for _, server := range []*Server{s, hosted, receivers, withServices} {
		for _, path := range []string{"/mcp", web.AssistantMCPPath} {
			for name := range listTools(t, server, path) {
				tools[name] = true
			}
		}
	}
	// Names that read as a hosted tool: a verb the hosted tools use, then
	// words. The local stdio bridge's own tools (stage_post, local_status...)
	// use other verbs.
	toolish := regexp.MustCompile("`((?:create|recover|claim|manage|send|join|accept|set|update|read|list|find|post|screen|notary|memory|wakeup|receiver|fetch|journal|x402|log|agent|whoami)(?:_[a-z]+)+|whoami)`")
	checked := 0
	for _, file := range []string{"docs/MESSAGES.md", "docs/TOOLS.md", "docs/TOOLS_FETCH.md", "docs/TOOLS_RECEIVE.md", "docs/TOOLS_MEMORY.md", "docs/TOOLS_WAKEUP.md", "docs/TOOLS_JOURNAL.md", "docs/TOOLS_PAID_APIS.md", "docs/TOOLS_NOTARY.md", "docs/TOOLS_VERIFY.md", "docs/TOOLS_IDENTITY.md", "docs/TOOLS_WORK.md", "clients/mcp/README.md", "plugins/swarmmemo/README.md"} {
		raw, err := os.ReadFile("../../" + file)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range toolish.FindAllStringSubmatch(string(raw), -1) {
			checked++
			if !tools[m[1]] {
				t.Errorf("%s names MCP tool %s, which no hosted profile registers", file, m[1])
			}
		}
	}
	for _, page := range []string{"/llms.txt", "/for-agents", "/docs"} {
		body := get(s, page, "text/html").Body.String() + get(hosted, page, "text/html").Body.String()
		for _, m := range regexp.MustCompile(`\b((?:create|recover|claim)_identity|send_private|read_conversation|list_conversations|create_conversation|create_invite|join_invite|accept_request|set_protection|update_conversation|manage_tokens)\b`).FindAllStringSubmatch(body, -1) {
			checked++
			if !tools[m[1]] {
				t.Errorf("%s names MCP tool %s, which no hosted profile registers", page, m[1])
			}
		}
	}
	if checked < 30 {
		t.Fatalf("checked only %d tool names; the extractor is broken", checked)
	}
}

// The identity and work pages, about the board itself, are served on every
// deployment: listed in the sitemap and the /tools index, and linked from
// /llms.txt once each, by the lines that already describe them.
func TestIdentityAndWorkToolPagesListed(t *testing.T) {
	s := New(&fakeService{}, nil, Config{PublicURL: "https://swarmmemo.com"})
	sitemap := makeRequest(s, "GET", "/sitemap.xml", "", "").Body.String()
	llms := makeRequest(s, "GET", "/llms.txt", "", "").Body.String()
	index, err := os.ReadFile("../../docs/TOOLS.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/tools/identity", "/tools/work"} {
		if !strings.Contains(sitemap, "<loc>https://swarmmemo.com"+path+"</loc>") {
			t.Errorf("the sitemap misses %s", path)
		}
		if n := strings.Count(llms, "https://swarmmemo.com"+path+"\n"); n != 1 {
			t.Errorf("/llms.txt ends %d lines with %s, want 1", n, path)
		}
		if !strings.Contains(string(index), "(https://swarmmemo.com"+path+")") {
			t.Errorf("the /tools index does not link %s", path)
		}
	}
}

// The job pages (docs/jobs.go) are in the sitemap while the page doing their
// job is served: the board's own always, a service's only while it runs.
func TestJobPagesInSitemap(t *testing.T) {
	bare := makeRequest(New(&fakeService{}, nil, Config{PublicURL: "https://swarmmemo.com"}), "GET", "/sitemap.xml", "", "").Body.String()
	f := everyService
	f.Services = append(append([]string(nil), f.Services...), "fetch", "receiver", "notary", "x402")
	full := makeRequest(New(&fakeService{}, nil, Config{PublicURL: "https://swarmmemo.com", Features: f}), "GET", "/sitemap.xml", "", "").Body.String()
	for _, path := range []string{"/messages/agent-to-agent-messaging-api", "/tools/identity/look-up-an-agent-public-key", "/tools/work/hire-an-ai-agent"} {
		if !strings.Contains(bare, "<loc>https://swarmmemo.com"+path+"</loc>") {
			t.Errorf("the sitemap misses %s", path)
		}
	}
	if strings.Contains(bare, "<loc>https://swarmmemo.com/tools/notary/") {
		t.Error("the sitemap lists a notary job page with the notary off")
	}
	for _, path := range publicdocs.JobPaths() {
		if !strings.Contains(full, "<loc>https://swarmmemo.com"+path+"</loc>") {
			t.Errorf("the sitemap with every service misses %s", path)
		}
	}
}

// Every FAQ and tool-page search of SwarmMemo's own tools (docs/jobs.go)
// finds that page's tools first on the real search (C111): the pastebin
// answer leads with docs.create and docs.open, the shared-docs one with
// docs.create and docs.write.
func TestJobSearchesFindTheirTools(t *testing.T) {
	_, s := anonCallServer(t, 2000, "memory", "wakeup", "receiver", "paste", "docs")
	want := map[string][]string{
		"/tools/memory":  {"swarmmemo:memory."},
		"/tools/wakeup":  {"swarmmemo:wakeup."},
		"/tools/receive": {"swarmmemo:receiver."},
		"/tools/paste":   {"swarmmemo:docs.create", "swarmmemo:docs.open"},
		"/tools/docs":    {"swarmmemo:docs.create", "swarmmemo:docs.write"},
		// Job pages (docs/jobs.go) under those tools.
		"/tools/receive/trigger-an-ai-agent-from-a-webhook": {"swarmmemo:receiver."},
		"/tools/wakeup/wake-an-ai-agent-on-reply":           {"swarmmemo:wakeup."},
		"/tools/memory/memory-mcp-server":                   {"swarmmemo:memory."},
	}
	checked := 0
	for _, j := range publicdocs.Jobs {
		_, query, ok := strings.Cut(strings.Trim(strings.TrimPrefix(j.Example, "curl -s "), "'"), "https://swarmmemo.com/call/tools/search?")
		if !ok || !strings.Contains(query, "kind=swarmmemo") {
			continue
		}
		prefixes, ok := want[j.Path]
		if !ok {
			t.Fatalf("%s: no expected tools for %s", j.Path, j.Example)
		}
		w := makeRequest(s, "GET", "/call/tools/search?"+query, "", "")
		var ids []string
		tools, _ := dig(decodeResult(t, w.Body.Bytes()), "data", "result", "tools").([]any)
		for _, e := range tools {
			ids = append(ids, e.(map[string]any)["id"].(string))
		}
		for i, p := range prefixes {
			if w.Code != 200 || len(ids) <= i || !strings.HasPrefix(ids[i], p) {
				t.Fatalf("%s: %s found %v, want %v first\n%s", j.Path, query, ids, prefixes, w.Body.String())
			}
		}
		checked++
	}
	if checked != len(want) {
		t.Fatalf("checked %d of %d searches", checked, len(want))
	}
}
