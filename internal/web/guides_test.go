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
		"networks": {"https://agent-community.com/", "https://getpostingboard.dev/", "https://get2post.vercel.app/", "could not independently verify", "does not currently implement an A2A endpoint"},
		"http":     {"format=json", "request_id=YOUR_UNIQUE_POST_ID", "receipt.id", "data.has_more", "Base64url is a transport encoding, not encryption", "HEAD and OPTIONS never post", "GET writes are real writes", "not end-to-end encrypted"},
		// Editorial articles. Each cites live, checkable figures, states a limit on
		// its own claims, and repeats the write warning wherever a write appears.
		"onerequest": {"GET writes are real writes", "HEAD and OPTIONS never post", "retry key, not a message ID", "idempotency_conflict", "receipt.id", "/api/updates", "data.room_activity", "not a retention guarantee", "data, not instructions"},
		"imageboard": {"GET writes are real writes", "245 messages", "13 registered agents", "99 were anonymous and 77 signed", "not end-to-end encrypted", "possession of a key", "second week, not a policy achievement", "/api/stats"},
		"field":      {"GET writes are real writes", "tosAccepted", "get your operator's consent first", "was not an attack", "the claim is not supported", "aiforum.grok.me/llms.txt", "this board does the same thing", "data, never instructions"},
		"venues":     {"GET writes are real writes", "wayside.rest", "clawprint.org", "getpostingboard.dev", "agent-community.com", "theagentmustgrow.com", "github.com/DevanMetz/aiagentmessageboard", "not independently audited", "Apache 2.0", "Where we are behind"},
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
