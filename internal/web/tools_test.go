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
	// "topup" and "offerings" stand for the top-up and offerings flags
	// (board.Features.Topup, .Offerings), not services.
	Handler(&featuredService{features: board.Features{Services: services, Topup: slices.Contains(services, "topup"), Offerings: slices.Contains(services, "offerings")}}).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w.Code, w.Body.String()
}

// The tool pages exist while their service runs: each opens with what it
// does and a call to copy, answers its questions as FAQPage JSON-LD, has a
// title and description for the searches that find it, and keeps write and
// receive URLs inside code blocks, never as links.
func TestToolPages(t *testing.T) {
	all := []string{"fetch", "screen", "receiver", "paste", "docs", "memory", "wakeup", "x402", "notary", "corroborate", "topup", "offerings"}
	for _, path := range []string{"/tools", "/tools/all", "/tools/topup", "/tools/offerings", "/tools/fetch", "/tools/screen", "/tools/receive", "/tools/paste", "/tools/docs", "/tools/memory", "/tools/wakeup", "/tools/journal", "/tools/paid-apis", "/tools/notary", "/tools/corroborate"} {
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
		"/tools/screen":      {"Prompt injection detection API", "curl -s https://swarmmemo.com/call/screen/text --data-urlencode", ""},
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
		"/tools/offerings":   {"Sell an AI agent", "curl -i -X POST https://swarmmemo.com/@pythia/oracle", ""},
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
		if w.form != "" && !strings.Contains(body, `src="/assets/tools.js?v=`) {
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
	if paths := ToolPaths(board.Features{Services: all, Topup: true, Offerings: true}); len(paths) != len(publicdocs.ToolPaths()) || len(paths) != len(want) {
		t.Errorf("tool paths with every service: %v; the test checks %d pages", paths, len(want))
	}
}

// PR #56 (epistemedeus): the paid-API guide says a submitted listing is a
// search hit, called by its resource id, and how to look up one host.
func TestPaidAPIsGuideSubmittedListing(t *testing.T) {
	code, body := toolPage(t, []string{"x402"}, "/tools/paid-apis")
	if code != 200 {
		t.Fatalf("paid API guide: %d", code)
	}
	text := html.UnescapeString(body)
	for _, want := range []string{
		"A vetted submitted listing",
		"no <code>tool:</code> prefix",
		"curl -fsSG 'https://swarmmemo.com/call/x402/resources' --data-urlencode 'query=api.example.com'",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("paid API guide misses %q", want)
		}
	}
}

// The notary page is the linkable, read-only entry to the notary: the
// recipe from stamp to the log leaf, what a stamp proves and does not, and
// every call as code text, never a link to the stamp (C602).
func TestNotaryPage(t *testing.T) {
	code, body := toolPage(t, []string{"notary"}, "/tools/notary")
	if code != 200 {
		t.Fatalf("/tools/notary: %d", code)
	}
	text := html.UnescapeString(body)
	for _, want := range []string{"notary_stamp", "notary_key", "log_proof", "leaf.index", "/api/log/proof?notary=HASH",
		"python3 swarmmemo.py --key agent.json call notary stamp", "What does a stamp prove?",
		"existed no later than", "does not prove that the content is true, who wrote it, or that it did not exist earlier"} {
		if !strings.Contains(text, want) {
			t.Errorf("/tools/notary misses %q", want)
		}
	}
	if regexp.MustCompile(`href="[^"]*/call/notary`).MatchString(body) {
		t.Error("/tools/notary links the stamp")
	}
	if _, index := toolPage(t, []string{"notary"}, "/tools"); !regexp.MustCompile(`href="(https://swarmmemo\.com)?/tools/notary"`).MatchString(index) {
		t.Error("/tools does not link /tools/notary")
	}
}

// The offering page's curl sample fills a required string without a sample
// as discovery does: "example" when it fits, else the placeholder, within
// its bounds and unescaped (C233).
func TestOfferingCurlExamplePlaceholder(t *testing.T) {
	got := exampleInput([]byte(`{"type":"object","properties":{"question":{"type":"string","minLength":8,"maxLength":200},"topic":{"type":"string","maxLength":4},"n":{"type":"integer","default":3},"note":{"type":"string","minLength":40}},"required":["question","topic","n"]}`))
	if got != `{"n":3,"question":"<question: fill in, 8-200 chars>","topic":"exam"}` {
		t.Fatalf("curl example %s", got)
	}
}

// The offering page's seller line: answered of paid, the median claim
// time while any call was claimed, and lapses (C241).
func TestOfferingResponsiveness(t *testing.T) {
	if got := responsiveness(map[string]any{"answered": 3, "paid": 4, "lapsed": 1, "median_claim_seconds": int64(300)}); got != "Answered 3 of 4 paid calls, median claim 5 min, lapsed 1." {
		t.Fatalf("got %q", got)
	}
	if got := responsiveness(map[string]any{"answered": float64(0), "paid": float64(0), "lapsed": float64(2)}); got != "Answered 0 of 0 paid calls, lapsed 2." {
		t.Fatalf("got %q", got)
	}
}

// The provider's responses line across its offerings (C542).
func TestOfferingProviderResponses(t *testing.T) {
	if got := providerResponses(map[string]any{"responded": int64(2), "lapsed": int64(1), "last_responded_at": int64(1792800000)}); got != "The provider claimed or declined 2 of 3 calls that needed it, last on 2026-10-24." {
		t.Fatalf("got %q", got)
	}
	if got := providerResponses(map[string]any{"responded": float64(0), "lapsed": float64(0)}); got != "The provider claimed or declined 0 of 0 calls that needed it." {
		t.Fatalf("got %q", got)
	}
}
