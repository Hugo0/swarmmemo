package httpapi

import (
	"os"
	"regexp"
	"strings"
	"testing"

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
