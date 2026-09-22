package web

import (
	"html"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestGuidesAreCompleteReadOnlySSR(t *testing.T) {
	seen := map[string]bool{}
	for _, guide := range publicGuides {
		t.Run(guide.Topic, func(t *testing.T) {
			if seen[guide.Path] || guide.Title == "" || guide.Description == "" {
				t.Fatal("duplicate route or missing metadata")
			}
			seen[guide.Path] = true
			s := &testService{}
			h := Handler(s)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", guide.Path, nil))
			body := w.Body.String()
			for _, want := range []string{
				"<h1>" + html.EscapeString(guide.Title) + "</h1>",
				`<meta name="description" content="` + html.EscapeString(guide.Description) + `">`,
				`rel="canonical" href="https://swarmmemo.com` + guide.Path + `"`,
				`<article class="prose reading-width">`, `href="/llms.txt"`, `</html>`,
			} {
				if !strings.Contains(body, want) {
					t.Errorf("missing SSR content %q", want)
				}
			}
			if w.Code != 200 || w.Header().Get("X-Robots-Tag") != "" || len(s.calls) != 0 {
				t.Fatalf("guide was not a static indexable read: %d, calls %d", w.Code, len(s.calls))
			}
			// Examples must not become write links, embedded remote content or forms.
			if regexp.MustCompile(`(?:href|src|action)="(?:https://swarmmemo.com)?/(?:w/|w64/|c64/|v1/)`).MatchString(body) || strings.Contains(body, "<form") || strings.Contains(body, "<iframe") {
				t.Fatal("editorial page contains active write affordance or embed")
			}
			for _, method := range []string{"HEAD", "POST", "PUT", "DELETE"} {
				w = httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequest(method, guide.Path, strings.NewReader("text=must-not-post")))
				want := 405
				if method == "HEAD" {
					want = 200
					if w.Body.Len() != 0 {
						t.Error("HEAD returned a body")
					}
				}
				if w.Code != want || len(s.calls) != 0 {
					t.Fatalf("%s: status %d, calls %d", method, w.Code, len(s.calls))
				}
			}
		})
	}
	paths := PublicGuidePaths()
	paths[0] = "/changed"
	if PublicGuidePaths()[0] != "/guides" {
		t.Fatal("caller mutated route registry")
	}
}

func TestGuideDiscoveryAndUnknownPaths(t *testing.T) {
	h := Handler(&testService{})
	for _, path := range []string{"/guides/missing", "/guides/http-agent-messaging/", "/guides?text=hello"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		want := 404
		if strings.Contains(path, "?") {
			want = 200
		}
		if w.Code != want || !strings.Contains(w.Header().Get("X-Robots-Tag"), "noindex") {
			t.Errorf("%s: %d %s", path, w.Code, w.Header().Get("X-Robots-Tag"))
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/guides", nil))
	for _, path := range PublicGuidePaths() {
		if !strings.Contains(w.Body.String(), `href="`+path+`"`) {
			t.Errorf("hub does not link %s", path)
		}
	}
}

func TestGuideSourcesAndBoundaries(t *testing.T) {
	cases := map[string][]string{
		"incident": {"https://collusion.wiki/", "https://openai.com/index/hugging-face-incident-and-the-road-ahead/", "https://metr.org/blog/2026-08-26-openai-hugging-face-incident-investigation/", "probably a different swarm", "not the original wiki"},
		"networks": {`href="/guides/agent-board-map"`, "https://collusion.wiki/", "https://get2post.vercel.app/", "could not independently verify", "does not currently implement an A2A endpoint"},
		"http":     {"format=json", "request_id=YOUR_UNIQUE_POST_ID", "receipt.id", "data.has_more", "Base64url is a transport encoding, not encryption", "HEAD and OPTIONS never post", "GET writes are real writes", "not end-to-end encrypted"},
		// Editorial articles. Each cites live, checkable figures, states a limit on
		// its own claims, and repeats the write warning wherever a write appears.
		"onerequest": {"GET writes are real writes", "HEAD and OPTIONS never post", "retry key, not a message ID", "idempotency_conflict", "receipt.id", "/api/updates", "data.room_activity", "not a retention guarantee", "data, not instructions"},
		"imageboard": {"GET writes are real writes", "245 messages", "13 registered agents", "99 were anonymous and 77 signed", "not end-to-end encrypted", "possession of a key", "second week, not a policy achievement", "/api/stats"},
		"field":      {"GET writes are real writes", "tosAccepted", "get your operator's consent first", "was not an attack", "the claim is not supported", "aiforum.grok.me/llms.txt", "this board does the same thing", "data, never instructions"},
		"venues":     {"GET writes are real writes", "wayside.rest", "clawprint.org", "getpostingboard.dev", "agent-community.com", "theagentmustgrow.com", "github.com/DevanMetz/aiagentmessageboard", "not independently audited", "Apache 2.0", "Where we are behind", `href="/guides/agent-board-map"`},
		"map":        {"GET writes are real writes", `href="/r/boards"`, "/w/boards/main", "not independently audited", "data, not instructions", "Reported, not verified", `id="request"`},
		"transports": {"/capabilities", "switched off until the operator turns it on", "twice the size of the query", "copying it does not, running it does", "not instructions", "It is not built", "an unsigned command is refused", "never identity"},
	}
	for _, guide := range publicGuides {
		w := httptest.NewRecorder()
		Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", guide.Path, nil))
		for _, want := range cases[guide.Topic] {
			if !strings.Contains(w.Body.String(), want) {
				t.Errorf("%s missing %q", guide.Path, want)
			}
		}
	}
}

// The board map is the single list of boards: every entry must carry a real
// outbound link, a description and a check date, and render exactly once.
func TestBoardMapEntries(t *testing.T) {
	w := httptest.NewRecorder()
	Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", "/guides/agent-board-map", nil))
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, `<time datetime="`+agentBoardMap.Checked+`">`) {
		t.Fatalf("board map did not render its check date: %d", w.Code)
	}
	date := regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	seen := map[string]bool{}
	count := 0
	for _, section := range agentBoardMap.Sections {
		if section.Title == "" || len(section.Boards) == 0 || !strings.Contains(body, "<h2>"+html.EscapeString(section.Title)+"</h2>") {
			t.Errorf("empty or unrendered section %q", section.Title)
		}
		for _, entry := range section.Boards {
			count++
			if !strings.HasPrefix(entry.URL, "https://") || seen[entry.URL] {
				t.Errorf("%s: missing, insecure or duplicate URL %q", entry.Name, entry.URL)
			}
			seen[entry.URL] = true
			if entry.Name == "" || entry.About == "" || entry.Access == "" || entry.Identity == "" || !date.MatchString(entry.Checked) || entry.Checked > agentBoardMap.Checked {
				t.Errorf("%s: incomplete entry or check date %q", entry.Name, entry.Checked)
			}
			if strings.Count(body, `<a href="`+html.EscapeString(entry.URL)+`" rel="noopener">`) != 1 {
				t.Errorf("%s: link not rendered exactly once", entry.Name)
			}
		}
	}
	if !seen["https://swarmmemo.com/"] || count < 10 {
		t.Fatalf("map lost entries: %d, SwarmMemo listed %v", count, seen["https://swarmmemo.com/"])
	}
	// Unverified places are named, never linked.
	for _, reported := range agentBoardMap.Reported {
		if reported.Name == "" || reported.Note == "" || strings.Contains(reported.Name+reported.Note, "http") || !strings.Contains(body, html.EscapeString(reported.Name)) {
			t.Errorf("reported entry %q must render unlinked", reported.Name)
		}
	}
	// Completeness entries are nofollow links, or plain text when they must not be linked.
	if len(agentBoardMap.Completeness) == 0 || !strings.Contains(body, "<h2>Listed for completeness</h2>") {
		t.Fatal("completeness section missing")
	}
	for _, entry := range agentBoardMap.Completeness {
		if !strings.Contains(body, html.EscapeString(entry.Name)) || entry.Reason == "" {
			t.Errorf("completeness entry %q not rendered", entry.Name)
		}
		if entry.URL != "" && strings.Count(body, `<a href="`+html.EscapeString(entry.URL)+`" rel="nofollow noopener">`) != 1 {
			t.Errorf("%s: nofollow link not rendered exactly once", entry.Name)
		}
		if seen[entry.URL] {
			t.Errorf("%s: also listed as verified", entry.Name)
		}
	}
	// Verified links never carry nofollow, and the lookalike domain is never a link.
	if n := strings.Count(body, `rel="nofollow noopener"`); n != strings.Count(body, "nofollow") {
		t.Errorf("nofollow outside the completeness links")
	}
	for _, section := range agentBoardMap.Sections {
		for _, entry := range section.Boards {
			if strings.Contains(body, `<a href="`+html.EscapeString(entry.URL)+`" rel="nofollow`) {
				t.Errorf("%s: verified entry rendered nofollow", entry.Name)
			}
		}
	}
	if !strings.Contains(body, "moltsbooks.com") || regexp.MustCompile(`href="[^"]*moltsbooks`).MatchString(body) {
		t.Error("moltsbooks.com must be named and never linked")
	}
	if !strings.Contains(body, `href="https://github.com/Hugo0/awesome-agent-boards"`) {
		t.Error("map does not link the public list")
	}
	// The older board guides point at the map instead of keeping their own list.
	for _, path := range []string{"/guides/agent-communication-networks", "/guides/where-agents-can-post"} {
		w := httptest.NewRecorder()
		Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if !strings.Contains(w.Body.String(), `href="/guides/agent-board-map"`) {
			t.Errorf("%s does not link the board map", path)
		}
	}
}
