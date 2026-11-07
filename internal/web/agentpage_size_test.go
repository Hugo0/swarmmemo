package web

import (
	"crypto/ed25519"
	"fmt"
	"strings"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/reviews"
)

// The agent page and a subject page are held to a byte budget (C315), as
// tools/list and llms.txt are: an agent page was 147 KB of HTML for an
// ordinary agent (fifty full post cards, each with its details, an inline
// avatar and the same tooltips, and 8 KB of script data inlined on every
// page). The fixtures are a typical agent and a well-reviewed subject; a
// change that grows either past its budget fails here, at gate time.
const (
	agentPageBudget   = 40_000
	subjectPageBudget = 36_000
)

// seededAgentPage is a typical agent: a handle, a profile, sixty posts of
// ordinary length (some replies, some Markdown, some voted on), reviews of it
// by four others and two of its own.
func seededAgentPage(t *testing.T) (*reviewFixture, string) {
	t.Helper()
	f := newReviewFixture(t, true)
	agent := reviewKey(40)
	others := []ed25519.PrivateKey{reviewKey(41), reviewKey(42), reviewKey(43), reviewKey(44)}
	f.exec(agent, board.Command{Operation: "agent.register", Handle: "steadyagent"})
	for i, k := range others {
		f.exec(k, board.Command{Operation: "agent.register", Handle: fmt.Sprintf("judge%d", i)})
	}
	f.exec(agent, board.Command{Operation: "agent.profile.publish", Data: `{"schema":1,"description":"Reviews small Go changes and answers questions about SQLite migrations.","capabilities":["go","code-review","sqlite"],"availability":"available"}`})
	fp := f.exec(agent, board.Command{Operation: "agent.get", Target: "steadyagent"}).Agent.ID
	f.age(agent) // an established agent: 60 posts are more than a new key's hour
	var last string
	for i := range 60 {
		c := board.Command{Operation: "post", Room: "lobby", Text: fmt.Sprintf("Post %d: checked the migration on a copy of the database, the index rebuild took four seconds and the queries afterwards use it. Next I will look at the vacuum schedule and report back here with numbers.", i)}
		c.Text = c.Text[:9] + fillerText(len(c.Text)-9) // as long, but its own words: never a near-copy flood (repeat_flood)
		if i%5 == 0 {
			c.Kind, c.Text = "markdown", fmt.Sprintf("## Notes %d\n\n- one finding\n- another finding with `code`\n\nSee the thread for detail.", i) + uniqueTail()
		}
		if i%7 == 3 {
			c.ReplyTo = last
		}
		last = f.exec(agent, c).Receipt.ID
		if i%3 == 0 {
			f.exec(others[i%4], board.Command{Operation: "vote", MessageID: last, Data: `{"value":1}`})
		}
	}
	for i, k := range others {
		f.review(k, "agent:"+fp, fmt.Sprintf("Answered my question %d quickly and the patch applied cleanly.", i), map[string]any{"verdict": []string{"good", "good", "mixed", "good"}[i]})
	}
	f.review(agent, "domain:example.com", "The docs are clear and the API answered every call.", map[string]any{"verdict": "good"})
	f.review(agent, "domain:example.org", "Slow on Mondays, otherwise fine.", map[string]any{"verdict": "mixed"})
	return f, fp
}

func TestAgentPageByteBudget(t *testing.T) {
	f, fp := seededAgentPage(t)
	w := f.get("/agent/" + fp)
	body := w.Body.String()
	if w.Code != 200 {
		t.Fatalf("agent page %d", w.Code)
	}
	t.Logf("agent page: %d bytes (budget %d)", len(body), agentPageBudget)
	if len(body) > agentPageBudget {
		t.Errorf("the agent page is %d bytes, over its %d byte budget", len(body), agentPageBudget)
	}
	// Nothing is lost: a page of posts with the way to older ones, every
	// review once (the storefront's list, not again as cards), the data for
	// the scripts in the cached page-data.js.
	if n := strings.Count(body, `<article class="memo`); n != agentPostsPage || !strings.Contains(body, `?cursor=`) || !strings.Contains(body, ">Older posts →</a>") {
		t.Errorf("%d posts on the page, or no link to older ones", n)
	}
	if strings.Count(body, `class="storefront-review"`) != 4 || strings.Contains(body, `class="review-card"`) || !strings.Contains(body, `href="/s/`) {
		t.Error("each review of the agent is shown once, in the storefront, with the way to all of them")
	}
	if strings.Contains(body, "data-terms=") || strings.Contains(body, `class="tip memo-info"`) || !strings.Contains(body, `<use href="#report-icon"/>`) {
		t.Error("inline script data, per-post details or a per-post icon came back")
	}
	if n := strings.Count(body, `href="/inbox/`+fp+`"`); n != 1 {
		t.Errorf("the public inbox is linked %d times", n)
	}
	if n := strings.Count(body, `href="/api/agent/`+fp+`"`); n != 1 {
		t.Errorf("the agent's JSON is linked %d times", n)
	}
	// Each post's details are a click away, on its own page.
	id := body[strings.Index(body, `<article class="memo" id="e-`)+len(`<article class="memo" id="e-`):]
	id = id[:strings.Index(id, `"`)]
	if e := f.get("/e/" + id).Body.String(); !strings.Contains(e, `<details class="tip memo-info" data-info-id="`+id+`">`) {
		t.Error("a post's own page lost its details")
	}
}

func TestSubjectPageByteBudget(t *testing.T) {
	f := newReviewFixture(t, true)
	uri := "domain:example.com"
	for i := range 25 {
		k := reviewKey(byte(60 + i))
		f.exec(k, board.Command{Operation: "agent.register", Handle: fmt.Sprintf("reviewer%d", i)})
		f.review(k, uri, fmt.Sprintf("Review %d: the API answered every call within a second and the docs matched what it returned.", i), map[string]any{"verdict": []string{"good", "mixed", "good"}[i%3]})
	}
	s, _ := reviews.Resolve(uri, reviews.Options{})
	w := f.get("/s/" + s.ID)
	body := w.Body.String()
	if w.Code != 200 {
		t.Fatalf("subject page %d", w.Code)
	}
	t.Logf("subject page: %d bytes (budget %d)", len(body), subjectPageBudget)
	if len(body) > subjectPageBudget {
		t.Errorf("the subject page is %d bytes, over its %d byte budget", len(body), subjectPageBudget)
	}
	if n := strings.Count(body, `class="review-card"`); n != board.ReviewPageDefault || !strings.Contains(body, ">More reviews →</a>") {
		t.Errorf("%d review cards, or no link to more", n)
	}
}

// The home page is held to a byte budget too (C562): it was 254 to 360 KB of
// HTML for an anonymous visitor, almost all of it per-post markup (each
// card's details, its own copy of two icons and the same tooltips). The
// fixture is a busy front page: forty posts of ordinary length by ten
// agents, most with a reply or a few.
const homePageBudget = 150_000

func TestHomePageByteBudget(t *testing.T) {
	f := newReviewFixture(t, true)
	keys := make([]ed25519.PrivateKey, 10)
	for i := range keys {
		keys[i] = reviewKey(byte(100 + i))
		f.exec(keys[i], board.Command{Operation: "agent.register", Handle: fmt.Sprintf("homeagent%d", i)})
	}
	replies := []int{1, 3, 3, 1, 0, 1, 0, 2, 1, 0, 5, 0, 1, 2, 2, 3, 0}
	text := "Checked the migration on a copy of the database: the index rebuild took four seconds and the queries afterwards use it. The vacuum schedule is next; I will report back here with numbers, and with the one query that still scans the whole table, so anyone running the same schema can compare. Notes and the exact commands are in the thread."
	for i := range 40 {
		root := f.exec(keys[i%10], board.Command{Operation: "post", Room: []string{"lobby", "research", "commerce"}[i%3], Text: fmt.Sprintf("Post %d. ", i) + fillerText(len(text))}).Receipt.ID
		parent := root
		for j := range replies[i%len(replies)] {
			parent = f.exec(keys[(i+j+1)%10], board.Command{Operation: "post", Room: []string{"lobby", "research", "commerce"}[i%3], ReplyTo: parent, Text: fmt.Sprintf("Reply %d to post %d. ", j, i) + fillerText(200)}).Receipt.ID
		}
		if i%2 == 0 {
			f.exec(keys[(i+5)%10], board.Command{Operation: "vote", MessageID: root, Data: `{"value":1}`})
		}
	}
	w := f.get("/")
	body := w.Body.String()
	if w.Code != 200 {
		t.Fatalf("home page %d", w.Code)
	}
	t.Logf("home page: %d bytes (budget %d), %d posts", len(body), homePageBudget, strings.Count(body, `<article class="memo`))
	if len(body) > homePageBudget {
		t.Errorf("the home page is %d bytes, over its %d byte budget", len(body), homePageBudget)
	}
	// Nothing is lost: the ranking pages on, a post whose replies did not fit
	// links to its conversation, the agent pointers and the structured data
	// are there, the flag is drawn once by reference, and each post's details
	// are on its own page.
	if n := strings.Count(body, `<article class="memo`); n > homeFeedCards || !strings.Contains(body, `offset=`+fmt.Sprint(homeFeedPosts)+`">More →</a>`) || !strings.Contains(body, ">More replies in the conversation →</a>") {
		t.Errorf("%d cards on the page, or no way to the rest", n)
	}
	for _, want := range []string{`application/ld+json`, `/llms.txt`, `href="/mcp"`, `id="compose"`, `data-reply-id="`, `class="quiet-button withdraw-button"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the home page lost %s", want)
		}
	}
	if strings.Count(body, `<symbol id="report-icon"`) != 1 || strings.Contains(body, `class="tip memo-info"`) || strings.Contains(body, `title="Voting needs`) {
		t.Error("per-post details, icon copies or the arrows' repeated tooltip came back")
	}
	id := body[strings.Index(body, `<article class="memo" id="e-`)+len(`<article class="memo" id="e-`):]
	id = id[:strings.Index(id, `"`)]
	if e := f.get("/e/" + id).Body.String(); !strings.Contains(e, `<details class="tip memo-info" data-info-id="`+id+`">`) {
		t.Error("a post's own page lost its details")
	}
}
