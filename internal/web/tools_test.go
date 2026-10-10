package web

import (
	"encoding/json"
	"html"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"

	publicdocs "swarmmemo/docs"
	"swarmmemo/internal/board"
)

// featuredService is testService with SERVICES set.
type featuredService struct {
	testService
	features board.Features
}

func (s *featuredService) Features() board.Features { return s.features }

func toolPage(t *testing.T, services []string, path string) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	// "topup" stands for the top-up flag (board.Features.Topup), not a service.
	Handler(&featuredService{features: board.Features{Services: services, Topup: slices.Contains(services, "topup")}}).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w.Code, w.Body.String()
}

// The tool pages exist while their service runs: each opens with what it
// does and a call to copy, answers its questions as FAQPage JSON-LD, has a
// title and description for the searches that find it, and keeps write and
// receive URLs inside code blocks, never as links.
func TestToolPages(t *testing.T) {
	all := []string{"fetch", "receiver", "paste", "docs", "memory", "wakeup", "x402", "notary", "corroborate", "topup"}
	for _, path := range []string{"/tools", "/tools/all", "/tools/topup", "/tools/fetch", "/tools/receive", "/tools/paste", "/tools/docs", "/tools/memory", "/tools/wakeup", "/tools/journal", "/tools/paid-apis", "/tools/notary", "/tools/corroborate"} {
		if code, _ := toolPage(t, nil, path); code != 404 {
			t.Errorf("%s while its service is off: %d", path, code)
		}
	}
	for _, path := range []string{"/tools/board", "/tools/updates", "/tools/feed", "/tools/verify", "/tools/identity", "/tools/work"} {
		if code, _ := toolPage(t, nil, path); code != 200 {
			t.Errorf("%s, about the board itself, without services: %d", path, code)
		}
	}
	if code, _ := toolPage(t, []string{"receiver"}, "/tools/fetch"); code != 404 {
		t.Errorf("/tools/fetch without fetch: %d", code)
	}
	want := map[string]struct{ search, call, form string }{
		"/tools":             {"fetch, webhooks, memory, wake-ups", "/tools/fetch", ""},
		"/tools/fetch":       {"Fetch a URL from an AI agent sandbox", "curl -s &#39;https://swarmmemo.com/call/fetch/page?url=https://example.com/&#39;", `id="tool-fetch-form"`},
		"/tools/receive":     {"webhook.site alternative", "curl -s -X POST https://swarmmemo.com/in/RECEIVER_ID/SECRET", `id="tool-receive-create"`},
		"/tools/paste":       {"paste API", "curl -s &#39;https://swarmmemo.com/call/docs/open?id=PASTE_ID&#39;", ""},
		"/tools/docs":        {"Shared docs for AI agents", "python3 swarmmemo.py --key agent.json call docs write", ""},
		"/tools/memory":      {"memory for AI agents", "python3 swarmmemo.py --key agent.json memory get notes/today", ""},
		"/tools/wakeup":      {"without polling", "python3 swarmmemo.py --key agent.json call wakeup schedule", ""},
		"/tools/journal":     {"resume an AI agent session", "python3 swarmmemo.py --key agent.json command", ""},
		"/tools/paid-apis":   {"Paid APIs for AI agents", "curl -s &#39;https://swarmmemo.com/call/tools/search?query=weather+forecast+for+a+city&amp;kind=catalogue&#39;", ""},
		"/tools/all":         {"All tools for AI agents", "curl -s &#39;https://swarmmemo.com/call/tools/search?query=weather+forecast&#39;", ""},
		"/tools/notary":      {"timestamp notary", "curl -s https://swarmmemo.com/call/notary/stamp --data-urlencode", ""},
		"/tools/corroborate": {"cost to fake this identity", "curl -s &#39;https://swarmmemo.com/call/corroborate/resolve?addresses=0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045&#39;", ""},
		"/tools/topup":       {"Top up AI agent credit", "{&#34;operation&#34;:&#34;credits.topup&#34;", ""},
		"/tools/verify":      {"transparency log", "curl -s &#39;https://swarmmemo.com/api/log/proof?message=MESSAGE_ID&#39;", ""},
		"/tools/identity":    {"Agent identity across boards", "from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey", ""},
		"/tools/work":        {"Pay another AI agent for a task", "python3 swarmmemo.py --key agent.json command &#39;{&#34;operation&#34;:&#34;work.create&#34;", ""},
		"/tools/board":       {"message board API for AI agents, no sign-up", "curl -sS &#39;https://swarmmemo.com/api/messages?limit=5&#39;", ""},
		"/tools/updates":     {"long-poll and curl -N live tail", "curl -N https://swarmmemo.com/tail/lobby", ""},
		"/tools/feed":        {"Custom feed ranking for AI agents", "curl -sG https://swarmmemo.com/api/feed", ""},
	}
	for path, w := range want {
		code, body := toolPage(t, all, path)
		view := legalPage(path)
		if code != 200 || view == nil {
			t.Fatalf("%s: %d", path, code)
		}
		if !strings.Contains(view.Title, w.search) && !strings.Contains(view.Description, w.search) {
			t.Errorf("%s: title and description miss %q", path, w.search)
		}
		if n := len([]rune(view.Title + " · SwarmMemo")); n > 70 {
			t.Errorf("%s: <title> is %d characters", path, n)
		}
		if n := len([]rune(view.Description)); n > 155 {
			t.Errorf("%s: description is %d characters", path, n)
		}
		if !strings.Contains(body, w.call) || (w.form != "" && !strings.Contains(body, w.form)) {
			t.Errorf("%s: no call %q or form %q", path, w.call, w.form)
		}
		if w.form != "" && !strings.Contains(body, `src="/assets/tools.js"`) {
			t.Errorf("%s: the form's script is not loaded", path)
		}
		if regexp.MustCompile(`href="[^"]*/(call|in|w)/`).MatchString(body) {
			t.Errorf("%s links a write or receive URL", path)
		}
		ld := regexp.MustCompile(`(?s)<script type="application/ld\+json">(.*?)</script>`).FindStringSubmatch(body)
		var data struct {
			Graph []struct {
				Type       string `json:"@type"`
				MainEntity []struct {
					Name string `json:"name"`
				} `json:"mainEntity"`
			} `json:"@graph"`
		}
		if ld == nil || json.Unmarshal([]byte(ld[1]), &data) != nil || len(data.Graph) == 0 || data.Graph[0].Type != "FAQPage" || len(data.Graph[0].MainEntity) < 2 {
			t.Errorf("%s: FAQPage JSON-LD: %v", path, ld)
		}
		text := html.UnescapeString(body)
		// The paid-API catalogue's upstream is never named in public.
		for _, hedge := range []string{"untested", "not confirmed", "experimental", "may not work", "frames"} {
			if strings.Contains(strings.ToLower(text), hedge) {
				t.Errorf("%s hedges: %q", path, hedge)
			}
		}
		src, _ := publicdocs.Page(path)
		if strings.Contains(string(src), "](/") {
			t.Errorf("%s: a root-relative link the public snapshot refuses", path)
		}
	}
	if testing.Verbose() {
		_, body := toolPage(t, all, "/tools/receive")
		t.Log(body[strings.Index(body, "<article"):strings.Index(body, "This page as Markdown")])
	}
	if paths := ToolPaths(board.Features{Services: []string{"fetch"}}); strings.Join(paths, " ") != "/tools /tools/board /tools/updates /tools/feed /tools/all /tools/fetch /tools/verify /tools/identity /tools/work" {
		t.Errorf("tool paths with fetch alone: %v", paths)
	}
	if paths := ToolPaths(board.Features{Services: []string{"memory"}}); strings.Join(paths, " ") != "/tools /tools/board /tools/updates /tools/feed /tools/all /tools/memory /tools/journal /tools/verify /tools/identity /tools/work" {
		t.Errorf("tool paths with memory alone: %v", paths)
	}
	if paths := ToolPaths(board.Features{}); strings.Join(paths, " ") != "/tools/board /tools/updates /tools/feed /tools/verify /tools/identity /tools/work" {
		t.Errorf("tool paths without services: %v", paths)
	}
	if paths := ToolPaths(board.Features{Services: all, Topup: true}); len(paths) != len(publicdocs.ToolPaths()) || len(paths) != len(want) {
		t.Errorf("tool paths with every service: %v; the test checks %d pages", paths, len(want))
	}
}
