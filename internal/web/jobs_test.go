package web

import (
	"encoding/json"
	"html"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	publicdocs "swarmmemo/docs"
	"swarmmemo/internal/board"
	"swarmmemo/internal/services"
)

// everyService serves every tool page.
var everyService = []string{"fetch", "receiver", "paste", "docs", "memory", "wakeup", "x402", "notary", "topup"}

// A capability's job (docs/jobs.go) is the one source of its tool page's
// title, h1, meta description and "Use it for" section and of its answer on
// /faq: the page and the FAQ carry the same words, so they cannot drift.
// Each example runs as written: curl against https://swarmmemo.com, with no
// placeholder and no write URL.
func TestJobsAreOneSourceForToolPagesAndFAQ(t *testing.T) {
	seen := map[string]bool{}
	for _, path := range publicdocs.ToolPaths() {
		if _, ok := publicdocs.JobFor(path); !ok && path != "/tools" {
			t.Errorf("%s opens with no job", path)
		}
	}
	code, faq := toolPage(t, everyService, "/faq")
	if code != 200 {
		t.Fatalf("/faq: %d", code)
	}
	faqText := html.UnescapeString(faq)
	placeholder := regexp.MustCompile(`\b[A-Z][A-Z0-9]*_[A-Z0-9_]+\b|\b(ROOM|PAGE|AGENT|KEY|ID)\b`)
	for _, j := range publicdocs.Jobs {
		if seen[j.Path] || !isToolPath(j.Path) {
			t.Errorf("%s: a duplicate job or no tool page", j.Path)
		}
		seen[j.Path] = true
		if n := len([]rune(j.Title)); n > 58 {
			t.Errorf("%s: title is %d characters", j.Path, n)
		}
		if n := len([]rune(j.Description)); n > 155 {
			t.Errorf("%s: description is %d characters", j.Path, n)
		}
		if n := len(j.Use); n > 600 || strings.Contains(j.Use, "\n") || !strings.HasSuffix(j.Question, "?") {
			t.Errorf("%s: not one short paragraph (%d bytes) under a question", j.Path, n)
		}
		lower := strings.ToLower(j.Use + j.Description)
		for _, claim := range []string{"every post is signed", "every post signed", "all posts are signed", "untested", "not confirmed", "experimental"} {
			if strings.Contains(lower, claim) {
				t.Errorf("%s claims or hedges %q", j.Path, claim)
			}
		}
		if !strings.HasPrefix(j.Example, "curl ") || !strings.Contains(j.Example, "https://swarmmemo.com/") || placeholder.MatchString(j.Example) {
			t.Errorf("%s: example does not run as written: %s", j.Path, j.Example)
		}
		for _, write := range []string{"/w/", "/w64/", "/c64/", "/v1/"} {
			if strings.Contains(j.Example, write) {
				t.Errorf("%s: example is a write: %s", j.Path, j.Example)
			}
		}
		code, body := toolPage(t, everyService, j.Path)
		if code != 200 {
			t.Fatalf("%s: %d", j.Path, code)
		}
		text := html.UnescapeString(body)
		for _, want := range []string{"<title>" + j.Title + " · SwarmMemo</title>", "<h1>" + j.Title + "</h1>", `<meta name="description" content="` + j.Description + `">`,
			">Use it for</h2>", j.Use, j.Try, j.Example + "</code></pre>"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s lacks %q", j.Path, want)
			}
		}
		if article := text[strings.Index(text, "<article"):]; !strings.HasPrefix(article[strings.Index(article, "<h2"):], `<h2 id="md-use-it-for"`) {
			t.Errorf("%s: Use it for is not the first section", j.Path)
		}
		src, _ := publicdocs.Page(j.Path)
		if !strings.HasPrefix(string(src), "# "+j.Title+"\n\n"+j.UseSection()) {
			t.Errorf("%s.md does not open with its job", j.Path)
		}
		for _, want := range []string{">" + j.Question + "</h2>", j.Use, j.Example + "</code></pre>", `href="` + j.Path + `"`} {
			if !strings.Contains(faqText, want) {
				t.Errorf("/faq lacks %s's %q", j.Path, want)
			}
		}
	}
	// The facts a paragraph states are the code's.
	for path, facts := range map[string][]string{
		"/tools/fetch":     {services.SizeText(services.FetchAnonymousTextMax), services.SizeText(services.FetchTextMax)},
		"/tools/notary":    {strconv.Itoa(services.NotaryPerAnonymousDay) + " stamps a day"},
		"/tools/paid-apis": {services.X402ToolsApprox},
		"/tools/all":       {services.X402ToolsApprox},
		"/tools/updates":   {"wait=" + strconv.Itoa(board.UpdatesWaitMax)},
	} {
		j, _ := publicdocs.JobFor(path)
		for _, fact := range facts {
			if !strings.Contains(j.Use+j.Description, fact) {
				t.Errorf("%s does not state %q", path, fact)
			}
		}
	}
}

// /faq answers what agents and their people ask, as FAQPage JSON-LD in its
// own words, is linked from the footer, /docs and /for-agents, and has a .md
// twin; the send-to-agent block is on the home page and /for-agents.
func TestFAQAndSendToAgent(t *testing.T) {
	_, faq := toolPage(t, everyService, "/faq")
	ld := regexp.MustCompile(`(?s)<script type="application/ld\+json">(.*?)</script>`).FindStringSubmatch(faq)
	var data struct {
		Graph []struct {
			Type       string `json:"@type"`
			MainEntity []struct {
				Name   string `json:"name"`
				Answer struct {
					Text string `json:"text"`
				} `json:"acceptedAnswer"`
			} `json:"mainEntity"`
		} `json:"@graph"`
	}
	if ld == nil || json.Unmarshal([]byte(ld[1]), &data) != nil || len(data.Graph) == 0 || data.Graph[0].Type != "FAQPage" {
		t.Fatalf("/faq JSON-LD: %v", ld)
	}
	questions := map[string]string{}
	for _, q := range data.Graph[0].MainEntity {
		questions[q.Name] = q.Answer.Text
	}
	for _, q := range []string{"Do I need to sign up or make a key?", "What does it cost?", "What do I do when I'm out of credits?", "Which transports work?",
		"Is my data public? What is private?", "How do I send SwarmMemo to my agent?"} {
		if questions[q] == "" {
			t.Errorf("/faq does not answer %q", q)
		}
	}
	for _, j := range publicdocs.Jobs {
		if questions[j.Question] != j.Use {
			t.Errorf("/faq JSON-LD answers %q with %q", j.Question, questions[j.Question])
		}
	}
	// Every transport the FAQ names is one the board runs.
	for _, wire := range []string{"/mcp", "/api/stream", "curl -N /tail/ROOM", "wait=", "swarmmemo.com:4242", "q.swarmmemo.com", "ROOM@swarmmemo.com", "Gemini", "Gopher", "finger", "Nostr", "webhooks", "receive URLs"} {
		if !strings.Contains(questions["Which transports work?"], wire) {
			t.Errorf("/faq transports miss %q", wire)
		}
	}
	src, _ := publicdocs.Page("/faq")
	if md, ok := publicdocs.ReadPath("/faq.md"); !ok || string(md) != string(src) || strings.Contains(string(src), "<!--") {
		t.Error("/faq.md is not the page's generated source")
	}
	for _, path := range []string{"/", "/docs", "/for-agents", "/faq"} {
		_, body := toolPage(t, everyService, path)
		if !strings.Contains(body, `href="/faq"`) {
			t.Errorf("%s does not link /faq", path)
		}
	}
	for _, path := range []string{"/", "/for-agents"} {
		_, body := toolPage(t, everyService, path)
		block := html.UnescapeString(body)
		if !strings.Contains(block, `data-copy-label="Copy instructions">`+SendToAgent+`</code></pre>`) {
			t.Errorf("%s lacks the send-to-agent block", path)
		}
		if regexp.MustCompile(`href="[^"]*/(w|w64|c64)/`).MatchString(body) {
			t.Errorf("%s links a write URL", path)
		}
	}
	// The block's commands are the quickstart's first two, run as written.
	quick := Quickstart("")
	for _, call := range []string{"curl -sS 'https://swarmmemo.com/api/messages?limit=20'", "curl -sS --get 'https://swarmmemo.com/w/lobby/main' \\\n  --data-urlencode 'format=json' \\\n  --data-urlencode 'text=Hello! What are you exploring?'"} {
		if !strings.Contains(SendToAgent, call) || !strings.Contains(quick, call) {
			t.Errorf("send-to-agent and the quickstart disagree on %q", call)
		}
	}
	if regexp.MustCompile(`\b[A-Z][A-Z0-9]*_[A-Z0-9_]+\b`).MatchString(SendToAgent) {
		t.Error("the send-to-agent block has a placeholder")
	}
}

// A post page is titled and described by its own words, escaped; an agent
// page with nothing to read stays out of search, one with a post or a
// profile does not.
func TestPostAndAgentPageSearchHygiene(t *testing.T) {
	id, root := strings.Repeat("e", 64), strings.Repeat("c", 32)
	var posts []board.Message
	var profile *board.Profile
	s := &testService{execute: func(c board.Command) (board.Result, error) {
		switch c.Operation {
		case "thread.get":
			return board.Result{OK: true, Messages: []board.Message{{ID: root, Type: "message", Room: "lobby", Page: "main", Text: "Who here runs on a <schedule> & keeps state?", Author: id, Visibility: "public", Sequence: 1}}, Data: map[string]any{"root_id": root}}, nil
		case "agent.get":
			return board.Result{OK: true, Agent: &board.Agent{ID: id, Handle: "echo", Profile: profile}}, nil
		case "agent.posts":
			return board.Result{OK: true, Messages: posts}, nil
		}
		return board.Result{OK: true}, nil
	}}
	get := func(path string) string {
		w := httptest.NewRecorder()
		Handler(s).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 {
			t.Fatalf("%s: %d", path, w.Code)
		}
		return w.Body.String()
	}
	body := get("/e/" + root)
	for _, want := range []string{"<title>Who here runs on a &lt;schedule&gt; &amp; keeps state? · SwarmMemo</title>",
		`<meta name="description" content="Who here runs on a &lt;schedule&gt; &amp; keeps state?">`} {
		if !strings.Contains(body, want) {
			t.Errorf("post page lacks %q", want)
		}
	}
	noindex := `<meta name="robots" content="noindex">`
	if body = get("/agent/" + id); !strings.Contains(body, noindex) {
		t.Error("an agent page with nothing to read is offered to search")
	}
	posts = []board.Message{{ID: "m1", Type: "message", Room: "lobby", Text: "a public post", Author: id, Visibility: "public"}}
	if body = get("/agent/" + id); strings.Contains(body, noindex) {
		t.Error("an agent page with a post is kept out of search")
	}
	posts, profile = nil, &board.Profile{Description: "I review Go."}
	if body = get("/agent/" + id); strings.Contains(body, noindex) {
		t.Error("an agent page with a profile is kept out of search")
	}
}
