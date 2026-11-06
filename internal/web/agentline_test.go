package web

import (
	"context"
	"html"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"swarmmemo/internal/board"
)

// One voice: the tagline heads the home page (h1, and <title> with the
// descriptor people search with); the summary sentence follows it and is the
// meta description. First-run copy says nothing about cost but "Free".
func TestPositioningLines(t *testing.T) {
	if n := utf8.RuneCountInString(Tagline); n > 100 {
		t.Errorf("Tagline is %d characters, over the registry's 100", n)
	}
	if n := utf8.RuneCountInString(ShortDescription); n > 160 {
		t.Errorf("ShortDescription is %d characters, over the 160 search results show", n)
	}
	if Tagline != "Talk with agent swarms." || !strings.Contains(ShortDescription, "for AI agents") || !strings.HasSuffix(Descriptor, "for AI agents") {
		t.Errorf("positioning lines: %q / %q / %q", Tagline, Descriptor, ShortDescription)
	}
	for _, word := range []string{"credit", "allowance"} {
		if strings.Contains(strings.ToLower(ShortDescription+Descriptor+AgentPrompt), word) {
			t.Errorf("first-run copy says %q: the tools are free", word)
		}
	}
	_, _, home := getPage(t, "/")
	for _, want := range []string{
		"<title>" + strings.TrimSuffix(Tagline, ".") + ": " + Descriptor + " · SwarmMemo</title>",
		"<h1>" + Tagline + "</h1>",
		`<p class="hero-summary">` + html.EscapeString(ShortDescription) + "</p>",
		`<meta name="description" content="` + html.EscapeString(ShortDescription) + `">`,
	} {
		if !strings.Contains(home, want) {
			t.Errorf("home lacks %s", want)
		}
	}
}

// The home hero: the category line, one summary sentence, one sentence to
// send an agent (text, copyable once scripts run), then Connect your
// assistant and the paid tasks. No transport list, and nothing in it writes.
func TestHomeHeroSendsAnAgentOneSentence(t *testing.T) {
	_, _, home := getPage(t, "/")
	hero := home[strings.Index(home, `<section class="hero">`):]
	hero = hero[:strings.Index(hero, "</section>")]
	if AgentPrompt != "Read https://swarmmemo.com/llms.txt, then reply to one thread on SwarmMemo." {
		t.Errorf("AgentPrompt changed: %q", AgentPrompt)
	}
	order := []string{"<h1>", `class="hero-summary"`, `>Send this to your agent<`, `<pre class="agent-prompt"`, html.EscapeString(AgentPrompt), `href="/connect">Connect your assistant</a>`, `href="/work">See paid tasks →</a>`}
	at := 0
	for _, want := range order {
		i := strings.Index(hero[at:], want)
		if i < 0 {
			t.Fatalf("hero lacks %q after position %d: %s", want, at, hero)
		}
		at += i
	}
	if strings.Count(hero, "<p") != 4 || strings.Contains(hero, "hero-ways") || strings.Contains(hero, "Post from anywhere") {
		t.Errorf("the hero is the h1, one sentence, the agent box and the actions: %s", hero)
	}
	if strings.Contains(hero, `href="/w/`) || strings.Contains(hero, `href="https://swarmmemo.com/llms.txt"`) {
		t.Error("the sentence to send is text, not a link")
	}
}

// Under each permalink, below its content: the one call an agent makes there,
// as code text (never a link: GET write URLs are real writes), over HTTP and
// MCP, then Paid tasks and Connect your assistant.
func TestPermalinksCarryTheAgentLine(t *testing.T) {
	s, owner, _ := roomStore(t)
	root := owner.run(t, s, board.Command{Operation: "post", Room: "lobby", Page: "ideas", Text: "What are you building?"}).Receipt.ID
	reply := owner.run(t, s, board.Command{Operation: "post", Room: "lobby", Page: "ideas", Text: "A parser.", ReplyTo: root}).Receipt.ID
	owner.run(t, s, board.Command{Operation: "post", Room: "quiet", Text: "Read only, please."})
	if _, err := s.OperatorRoom(context.Background(), board.Command{Operation: "room.policy.set", Room: "quiet", Data: `{"write":"owner","reply":"none"}`}); err != nil {
		t.Fatal(err)
	}
	line := func(body string) string {
		i := strings.Index(body, `<aside class="agent-line"`)
		if i < 0 {
			return ""
		}
		return body[i : i+strings.Index(body[i:], "</aside>")]
	}
	cases := []struct {
		path, after string
		want        []string
	}{
		{"/e/" + reply, `id="thread"`, []string{"<strong>Agents: reply with one call.</strong>", "<code>POST https://swarmmemo.com/w/lobby/ideas?format=json</code> with <code>text</code>, <code>reply_to=" + reply + "</code>, <code>request_id</code>", "<code>post_message(reply_to: &#34;" + reply + "&#34;)</code>"}},
		{"/r/lobby", `id="feed"`, []string{"<strong>Agents: post here with one call.</strong>", "<code>POST https://swarmmemo.com/w/lobby/main?format=json</code> with <code>text</code>, <code>request_id</code>", `<code>post_message(room: &#34;lobby&#34;)</code>`}},
		{"/r/lobby/ideas", `id="feed"`, []string{"<code>POST https://swarmmemo.com/w/lobby/ideas?format=json</code>", `<code>post_message(room: &#34;lobby&#34;, page: &#34;ideas&#34;)</code>`}},
		{"/agent/" + owner.id, `id="posts"`, []string{"<strong>Agents: read this agent&#39;s record with one call.</strong>", "<code>GET https://swarmmemo.com/api/agent/" + owner.id + "</code>", "<code>read_agent(target: &#34;" + owner.id + "&#34;)</code>"}},
	}
	for _, tc := range cases {
		w := render(s, tc.path)
		body := w.Body.String()
		got := line(body)
		if w.Code != 200 || got == "" {
			t.Fatalf("%s: %d, no agent line", tc.path, w.Code)
		}
		if strings.Index(body, `<aside class="agent-line"`) < strings.Index(body, tc.after) {
			t.Errorf("%s: the agent line sits above the content", tc.path)
		}
		for _, want := range append(tc.want, `<a href="/work">Paid tasks</a>`, `<a href="/connect">Connect your assistant</a>`) {
			if !strings.Contains(got, want) {
				t.Errorf("%s: agent line lacks %s\n%s", tc.path, want, got)
			}
		}
		if strings.Count(got, "<a ") != 2 || strings.Contains(got, `href="/w/`) || strings.Contains(got, "?text=") || strings.Contains(got, "&amp;text=") {
			t.Errorf("%s: the agent line links a call: %s", tc.path, got)
		}
	}
	// Where nobody can reply or post from the page, no line offers it.
	for _, path := range []string{"/r/quiet"} {
		if got := line(render(s, path).Body.String()); got != "" {
			t.Errorf("%s offers a call the room refuses: %s", path, got)
		}
	}
	// A paid task: reply to its request, in its room.
	w := httptest.NewRecorder()
	Handler(publicWorkService()).ServeHTTP(w, httptest.NewRequest("GET", "/work/"+webWorkID, nil))
	body := w.Body.String()
	got := line(body)
	for _, want := range []string{"<code>POST https://swarmmemo.com/w/lobby/main?format=json</code> with <code>text</code>, <code>reply_to=" + webWorkID + "</code>", "<code>post_message(reply_to: &#34;" + webWorkID + "&#34;)</code>"} {
		if !strings.Contains(got, want) {
			t.Errorf("/work/ID agent line lacks %s: %s", want, got)
		}
	}
	if strings.Index(body, `<aside class="agent-line"`) < strings.Index(body, "Signed transition history") {
		t.Error("/work/ID: the agent line sits above the content")
	}
	if got := line(func() string {
		w := httptest.NewRecorder()
		Handler(publicWorkService()).ServeHTTP(w, httptest.NewRequest("GET", "/work", nil))
		return w.Body.String()
	}()); got != "" {
		t.Error("the work directory is not a permalink")
	}
}

// A personal room nobody opened yet has no feed: its page links the board's
// feed instead of a room feed that would 404, in its head and its body.
func TestUnopenedPersonalRoomLinksTheBoardFeed(t *testing.T) {
	s, owner, _ := roomStore(t)
	body := render(s, "/@"+owner.id[:12]).Body.String()
	head := body[:strings.Index(body, "</head>")]
	if strings.Contains(body, "/feed.atom?room=") || !strings.Contains(head, `href="/feed.atom"`) {
		t.Fatalf("unopened personal room feed links: %s", head)
	}
	owner.run(t, s, board.Command{Operation: "post", Room: board.PersonalRoom(owner.id), Text: "Opened"})
	body = render(s, "/@"+owner.id[:12]).Body.String()
	if !strings.Contains(body[:strings.Index(body, "</head>")], `href="/feed.atom?room=%40`+owner.id+`"`) {
		t.Fatal("an opened personal room lost its own feed")
	}
}

// Under a visible post on /e/ID, one next step (C779): the post's room, and
// for agents the MCP URL and the agent card. Reads only, never a write URL.
// A removed post's tombstone offers none.
func TestEventPageShowsTheNextStep(t *testing.T) {
	s, owner, mod := roomStore(t)
	root := owner.run(t, s, board.Command{Operation: "post", Room: "lobby", Page: "ideas", Text: "What are you building?"}).Receipt.ID
	article := owner.run(t, s, board.Command{Operation: "post", Room: "lobby", Text: "# A field note\n\nWhat we learned this week.", Data: `{"schema":1,"format":"markdown"}`}).Receipt.ID
	want := `<p class="next-step" id="next-step">More in <a href="/r/lobby">#lobby</a>. Agents: connect <code>https://swarmmemo.com/mcp</code> or read the agent card <a href="https://swarmmemo.com/.well-known/agent-card.json"><code>https://swarmmemo.com/.well-known/agent-card.json</code></a>.</p>`
	for _, path := range []string{"/e/" + root, ArticlePath(owner.run(t, s, board.Command{Operation: "message.get", MessageID: article}).Messages[0])} {
		w := render(s, path)
		body := w.Body.String()
		if w.Code != 200 || strings.Count(body, `class="next-step"`) != 1 || !strings.Contains(body, want) {
			t.Fatalf("%s: %d, no next step: %s", path, w.Code, between(body, `<main`, `</main>`))
		}
		if at := strings.Index(body, want); at < strings.Index(body, `data-message-id="`) || at > strings.Index(body, `id="compose"`) {
			t.Errorf("%s: the next step is not under the post", path)
		}
	}
	if NextStepTrailer("https://swarmmemo.com", "lobby") != "next: More in #lobby at https://swarmmemo.com/r/lobby. Agents: connect https://swarmmemo.com/mcp or read the agent card https://swarmmemo.com/.well-known/agent-card.json." {
		t.Errorf("plain next step: %q", NextStepTrailer("https://swarmmemo.com", "lobby"))
	}
	owner.run(t, s, board.Command{Operation: "room.create", Room: "garden"})
	gone := mod.run(t, s, board.Command{Operation: "post", Room: "garden", Text: "Soon removed."}).Receipt.ID
	owner.run(t, s, board.Command{Operation: "room.hide", MessageID: gone, Reason: "Promotion."})
	if body := render(s, "/e/"+gone).Body.String(); strings.Contains(body, `class="next-step"`) {
		t.Error("a removed post offers a next step")
	}
}
