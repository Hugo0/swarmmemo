package web

import (
	"encoding/json"
	"html"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	publicdocs "swarmmemo/docs"
	"swarmmemo/internal/markdown"
)

func getPage(t *testing.T, path string) (int, string, string) {
	t.Helper()
	w := httptest.NewRecorder()
	Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w.Code, w.Header().Get("Location"), w.Body.String()
}

// The guide answers the questions people ask in their words: each question
// is a heading whose first paragraph answers it, and the page's FAQPage and
// HowTo JSON-LD say exactly what the page says.
func TestMessagesGuideAnswersTheQuestionsPeopleAsk(t *testing.T) {
	src, ok := publicdocs.Page("/messages")
	if !ok {
		t.Fatal("docs/MESSAGES.md is not embedded")
	}
	if strings.Contains(string(src), "](/") {
		t.Error("the source has a root-relative link the public snapshot refuses")
	}
	view := legalPage("/messages")
	if view == nil || view.Heading != "How do I connect my Claude Code or Codex agent to someone else's agent?" {
		t.Fatalf("heading: %+v", view)
	}
	if n := len([]rune(view.Title + " · SwarmMemo")); n > 60 {
		t.Errorf("<title> is %d characters, over 60", n)
	}
	if n := len([]rune(view.Description)); n > 155 || !strings.Contains(view.Description, "Claude Code") || !strings.Contains(view.Description, "Codex") || !strings.Contains(view.Description, "ChatGPT") {
		t.Errorf("description (%d characters) must name Claude Code, Codex and ChatGPT within 155: %q", n, view.Description)
	}
	asked := map[string]bool{}
	for _, qa := range view.Questions {
		asked[qa[0]] = true
		if qa[1] == "" || strings.HasPrefix(qa[1], "`") {
			t.Errorf("%q has no answering paragraph", qa[0])
		}
	}
	for _, q := range []string{view.Heading, "Can two AI agents from different people talk privately?", DebugHeading,
		"How do I stop prompt injection when my agent talks to another agent?", "How do I keep my agent from leaking secrets?",
		"Can a Grok, ChatGPT or Muse agent message another agent?", "Is it end-to-end encrypted?"} {
		if !asked[q] {
			t.Errorf("the guide does not ask %q", q)
		}
	}
	code, _, body := getPage(t, "/messages")
	if code != 200 || strings.Count(body, "<h1") != 1 || !strings.Contains(body, "<h1>"+html.EscapeString(view.Heading)+"</h1>") ||
		!strings.Contains(body, "<title>"+html.EscapeString(view.Title)+" · SwarmMemo</title>") || !strings.Contains(body, `href="/messages.md"`) {
		t.Fatalf("/messages: %d", code)
	}
	// Each answer is the page's own text.
	text := strings.Join(strings.Fields(html.UnescapeString(regexp.MustCompile(`<[^>]+>`).ReplaceAllString(body, ""))), " ")
	for _, qa := range view.Questions {
		if !strings.Contains(text, strings.Join(strings.Fields(qa[1]), " ")) {
			t.Errorf("the answer to %q is not the page's text: %q", qa[0], qa[1])
		}
	}
	ld := regexp.MustCompile(`(?s)<script type="application/ld\+json">(.*?)</script>`).FindStringSubmatch(body)
	if ld == nil {
		t.Fatal("/messages has no JSON-LD")
	}
	var data struct {
		Graph []struct {
			Type       string `json:"@type"`
			Name       string `json:"name"`
			MainEntity []struct {
				Name   string `json:"name"`
				Answer struct {
					Text string `json:"text"`
				} `json:"acceptedAnswer"`
			} `json:"mainEntity"`
			Step []struct {
				Text string `json:"text"`
			} `json:"step"`
		} `json:"@graph"`
	}
	if err := json.Unmarshal([]byte(ld[1]), &data); err != nil || len(data.Graph) != 2 {
		t.Fatalf("JSON-LD: %v %s", err, ld[1])
	}
	faq, howTo := data.Graph[0], data.Graph[1]
	if faq.Type != "FAQPage" || len(faq.MainEntity) != len(view.Questions) || faq.MainEntity[0].Answer.Text != view.Questions[0][1] {
		t.Errorf("FAQPage: %+v", faq)
	}
	if howTo.Type != "HowTo" || howTo.Name != DebugHeading || len(howTo.Step) < 4 || howTo.Step[0].Text != view.Steps[0] {
		t.Errorf("HowTo: %+v", howTo)
	}
	// /cases, the old name, lands on the debugging section.
	if CasesTarget() != "/messages#md-how-do-i-debug-an-issue-with-someone-else-s-agent" || !strings.Contains(string(view.Body), `id="`+strings.TrimPrefix(CasesTarget(), "/messages#")+`"`) {
		t.Errorf("the debugging anchor %s is not on the page", CasesTarget())
	}
	if code, location, _ := getPage(t, "/cases"); code != 301 || location != CasesTarget() {
		t.Errorf("/cases: %d %q", code, location)
	}
	if markdown.Title(string(src)) != view.Heading {
		t.Error("the h1 is not the document's title")
	}
}

// Some assistants' web tools open only URLs they have seen as links, so /docs
// and the home page link every /for page and both MCP endpoints.
func TestDocsAndHomeLinkTheAssistantPages(t *testing.T) {
	for _, path := range []string{"/", "/docs"} {
		code, _, body := getPage(t, path)
		if code != 200 {
			t.Fatalf("%s: %d", path, code)
		}
		for _, target := range append(PlatformPaths(), "/mcp", AssistantMCPPath) {
			if !strings.Contains(body, `<a href="`+target+`">`) {
				t.Errorf("%s does not link %s", path, target)
			}
		}
	}
}

// The guide is reachable from every page that talks to agents and assistants,
// by the words people use.
func TestMessagesGuideIsLinked(t *testing.T) {
	for path, phrase := range map[string]string{
		"/":            `<a href="/messages">talk privately with someone else's agent</a>`,
		"/for-agents":  `<a href="/messages">Connect your Claude Code or Codex agent to someone else's agent</a>`,
		"/docs":        `<a href="/messages">connect your Claude Code or Codex agent to someone else's agent</a>`,
		"/for/claude":  `<a href="/messages">How</a>`,
		"/for/chatgpt": `<a href="/messages">How</a>`,
	} {
		code, _, body := getPage(t, path)
		if code != 200 || !strings.Contains(body, phrase) {
			t.Errorf("%s does not link the guide with %s", path, phrase)
		}
	}
	code, _, body := getPage(t, "/for/chatgpt.json")
	if code != 200 || !strings.Contains(body, `"link":"/messages"`) {
		t.Error("/for/chatgpt.json lacks the guide its page links")
	}
}
