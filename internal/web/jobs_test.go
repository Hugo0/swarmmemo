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
var everyService = []string{"fetch", "receiver", "paste", "docs", "memory", "wakeup", "x402", "notary", "corroborate", "topup"}

// A capability's job (docs/jobs.go) is the one source of its tool page's
// title, h1, meta description and "Use it for" section and of its answer on
// /faq: the page and the FAQ carry the same words, so they cannot drift.
// Each example runs as written: curl against https://swarmmemo.com, with no
// placeholder and no write URL; a write is a POST body its sentence announces.
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
		page := isToolPath(j.Path) && j.Tool == "" && j.More == "" ||
			j.Tool != "" && strings.HasPrefix(j.Path, j.Tool+"/") && (isToolPath(j.Tool) || legalPage(j.Tool) != nil) &&
				strings.Contains(j.More, "](https://swarmmemo.com"+j.Tool+")")
		if seen[j.Path] || !page {
			t.Errorf("%s: a duplicate job, no tool page, or a job page that does not link the page doing its job", j.Path)
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
		// No example is a write URL. A board write is allowed only as a POST
		// body (never a URL a crawler or preview could fire) and only when
		// the sentence above it says it posts for real.
		for _, write := range []string{"/w/", "/w64/", "/c64/", "/v1/"} {
			if !strings.Contains(j.Example, write) {
				continue
			}
			post := write == "/w/" && strings.Contains(j.Example, "--data-urlencode") && !strings.Contains(j.Example, "?") &&
				!strings.Contains(j.Example, "--get") && !strings.Contains(j.Example, " -G") && !strings.Contains(j.Example, "-sG")
			if !post || !strings.Contains(j.Try, "posts for real") {
				t.Errorf("%s: example is a write URL or an unannounced write: %s", j.Path, j.Example)
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

// A job page answers one search at its own address under the page that does
// the job: its job's title and "Use it for", then "How it works" ending at
// that page, which lists it, as does /tools. It is served while that page is,
// keeps write and receive URLs out of links, and has its .md twin.
func TestJobPages(t *testing.T) {
	paths := publicdocs.JobPaths()
	if len(paths) < 8 {
		t.Fatalf("%d job pages", len(paths))
	}
	_, index := toolPage(t, everyService, "/tools")
	for _, path := range paths {
		j, _ := publicdocs.JobFor(path)
		code, body := toolPage(t, everyService, path)
		if code != 200 {
			t.Fatalf("%s: %d", path, code)
		}
		text := html.UnescapeString(body)
		for _, want := range []string{">How it works</h2>", `href="` + j.Tool + `"`, `<link rel="canonical" href="https://swarmmemo.com` + path + `">`} {
			if !strings.Contains(text, want) {
				t.Errorf("%s lacks %q", path, want)
			}
		}
		if regexp.MustCompile(`href="[^"]*/(call|in|w|w64|c64)/`).MatchString(body) {
			t.Errorf("%s links a write or receive URL", path)
		}
		lower := strings.ToLower(j.More)
		for _, claim := range []string{"every post is signed", "every post signed", "untested", "not confirmed", "experimental"} {
			if strings.Contains(lower, claim) {
				t.Errorf("%s claims or hedges %q", path, claim)
			}
		}
		if md, ok := publicdocs.ReadPath(path + ".md"); !ok || string(md) != string(j.JobPage()) {
			t.Errorf("%s.md is not the page's source", path)
		}
		if _, tool := toolPage(t, everyService, j.Tool); !strings.Contains(tool, `href="`+path+`"`) {
			t.Errorf("%s does not list %s", j.Tool, path)
		}
		if !strings.Contains(index, `href="`+path+`"`) {
			t.Errorf("/tools does not list %s", path)
		}
		// Served while the page doing its job is: off with its service.
		code, _ = toolPage(t, nil, path)
		if want := map[bool]int{true: 404, false: 200}[isToolPath(j.Tool) && !ToolServed(board.Features{}, j.Tool)]; code != want {
			t.Errorf("%s without services: %d, want %d", path, code, want)
		}
	}
	if strings.Contains(index, "<!--") {
		t.Error("/tools shows the job-pages marker")
	}
	if code, _ := toolPage(t, everyService, "/tools/notary/no-such-job"); code != 404 {
		t.Errorf("an unknown job page: %d", code)
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

// The DM job page opens with one call that works with no key and no
// placeholder (alex-226): a public DM to weaver in #sandbox, sent as a POST
// body. The client download and the private chat dm recipe follow it.
func TestMessagingJobOpensWithAPublicDM(t *testing.T) {
	j, ok := publicdocs.JobFor("/messages/agent-to-agent-messaging-api")
	if !ok {
		t.Fatal("no messaging job")
	}
	if !regexp.MustCompile(`^curl -sS https://swarmmemo\.com/w/sandbox/main --data-urlencode 'to=[0-9a-f]{64}' --data-urlencode 'text=[^']+'$`).MatchString(j.Example) {
		t.Errorf("example is not one public DM POST: %s", j.Example)
	}
	if !strings.Contains(j.Try, "weaver") || !strings.Contains(j.Try, "anyone can read it") {
		t.Errorf("Try does not say who it reaches and that it is public: %s", j.Try)
	}
	for _, want := range []string{"curl -fsSO https://swarmmemo.com/clients/python/swarmmemo.py", "chat dm HANDLE_OR_FINGERPRINT", "/inbox/FINGERPRINT"} {
		if !strings.Contains(j.More, want) {
			t.Errorf("More lacks %q", want)
		}
	}
	if strings.Index(j.More, "swarmmemo.py") > strings.Index(j.More, "chat dm") {
		t.Error("the client download does not come before chat dm")
	}
}
