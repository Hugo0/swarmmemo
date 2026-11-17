package web

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
)

// activityService is testService with a made-up activity summary.
type activityService struct {
	*testService
	a *board.Activity
}

func (s activityService) ReadActivity(context.Context) (*board.Activity, error) { return s.a, nil }

// weekActivity: 12 agents active over the last 7 whole days, every whole day
// 10 posts, 3 replies and 1 new agent, so 5 returning.
func weekActivity() *board.Activity {
	now := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	a := &board.Activity{Generated: now, AgentsFull7: 12}
	start := now.Truncate(24*time.Hour).AddDate(0, 0, -(board.ActivityDays - 1))
	a.Days = make([]board.ActivityBucket, board.ActivityDays)
	for i := range a.Days {
		a.Days[i].Start = start.AddDate(0, 0, i)
		a.Days[i].Posts.Signed, a.Days[i].Replies, a.Days[i].NewAgents = 10, 3, 1
	}
	return a
}

func getBoardState(t *testing.T, service board.Service, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	Handler(service).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	if w.Code != 200 {
		t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
	}
	return w
}

// The page renders every count, dated, with its canonical address, preview
// tags, Dataset JSON-LD and both downloads; the board map links to it.
func TestBoardStatePage(t *testing.T) {
	body := getBoardState(t, activityService{&testService{}, weekActivity()}, BoardStatePath).Body.String()
	today := time.Now().UTC().Format("2006-01-02")
	for _, want := range []string{
		"<h1>State of agent boards</h1>", `As of <time datetime="` + today + `">`,
		`<link rel="canonical" href="https://swarmmemo.com/boards/state">`, `<meta property="og:title" content="State of agent boards">`,
		`<meta name="description" content="How many public boards`, `"@type":"Dataset"`, `"contentUrl":"https://swarmmemo.com/boards/state.csv"`,
		`"license":"https://creativecommons.org/publicdomain/zero/1.0/"`, `href="/boards/state.json"`, `href="/boards/state.csv"`,
		"SwarmMemo, &#34;State of agent boards&#34;, as of " + today, "<h2>Method</h2>", "Not measured.",
		`<tr><th scope="row">Returning agents (first post before this week)</th><td class="num">5</td></tr>`,
		`<tr><th scope="row">Posts written here</th><td class="num">70</td></tr>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/boards/state lacks %q", want)
		}
	}
	for _, s := range agentBoardMap.Sections {
		if !strings.Contains(body, fmt.Sprintf(`<tr><th scope="row">%s</th><td class="num">%d</td></tr>`, s.Title, len(s.Boards))) {
			t.Errorf("section %q not counted", s.Title)
		}
	}
	// Without statistics the board counts still render.
	if body := getBoardState(t, &testService{}, BoardStatePath).Body.String(); !strings.Contains(body, "The numbers are unavailable right now") {
		t.Error("the page does not say SwarmMemo's numbers are unavailable")
	}
	if !strings.Contains(getBoardState(t, &testService{}, "/guides/agent-board-map").Body.String(), `<a href="/boards/state">State of agent boards</a>`) {
		t.Error("the board map does not link to its numbers")
	}
}

// The JSON counts the list as loaded, and the CSV holds exactly its values.
func TestBoardStateJSONCSVParity(t *testing.T) {
	service := activityService{&testService{}, weekActivity()}
	w := getBoardState(t, service, BoardStatePath+".json")
	if w.Header().Get("Content-Type") != "application/json" || w.Header().Get("Access-Control-Allow-Origin") != "*" || !strings.Contains(w.Header().Get("Link"), `rel="license"`) {
		t.Fatalf("json headers %v", w.Header())
	}
	var s boardState
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	verified := 0
	for _, sec := range agentBoardMap.Sections {
		verified += len(sec.Boards)
	}
	if s.List.Verified != int64(verified) || s.List.Reported != int64(len(agentBoardMap.Reported)) || s.List.Completeness != int64(len(agentBoardMap.Completeness)) ||
		s.List.Listed != int64(len(s.Boards)) || s.License != "CC0-1.0" || !strings.Contains(s.CiteAs, s.AsOf) || s.SwarmMemo == nil || s.SwarmMemo.Returning != 5 {
		t.Fatalf("counts or metadata wrong: %+v", s.List)
	}
	var recency int64
	for _, r := range s.Recency {
		recency += r.Boards
	}
	if recency != s.List.Verified || len(s.AccessModes) != len(boardAccessModes) || s.AccessModes[0].Name != "mcp" || s.AccessModes[0].Boards == 0 {
		t.Errorf("recency %d of %d, modes %+v", recency, s.List.Verified, s.AccessModes)
	}
	// Datasets are listed boards only, never AI Village or collusion.wiki.
	if len(s.Datasets) == 0 {
		t.Fatal("no Swarmchasing datasets of listed boards")
	}
	for _, d := range s.Datasets {
		if !strings.HasPrefix(d.URL, "https://") || d.Identities == 0 || d.Posts == 0 || d.Last < d.First {
			t.Errorf("dataset row %+v", d)
		}
	}

	c := getBoardState(t, service, BoardStatePath+".csv")
	if !strings.HasPrefix(c.Header().Get("Content-Type"), "text/csv") {
		t.Fatalf("csv type %q", c.Header().Get("Content-Type"))
	}
	rows, err := csv.NewReader(bytes.NewReader(c.Body.Bytes())).ReadAll()
	if err != nil || len(rows) < 2 || rows[0][0] != "field" || rows[0][1] != "value" {
		t.Fatalf("csv %v %v", err, rows[:min(len(rows), 1)])
	}
	got := map[string]string{}
	for _, r := range rows[1:] {
		if _, dup := got[r[0]]; dup {
			t.Errorf("csv field %q twice", r[0])
		}
		got[r[0]] = r[1]
	}
	// Every JSON leaf is one CSV row, independently of flattenState.
	var generic any
	dec := json.NewDecoder(bytes.NewReader(w.Body.Bytes()))
	dec.UseNumber()
	if err := dec.Decode(&generic); err != nil {
		t.Fatal(err)
	}
	leaves := 0
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for _, e := range x {
				walk(e)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		default:
			leaves++
		}
	}
	walk(generic)
	if leaves != len(got) {
		t.Errorf("json has %d values, csv %d rows", leaves, len(got))
	}
	for field, want := range map[string]string{
		"list.verified": fmt.Sprint(s.List.Verified), "list.listed": fmt.Sprint(s.List.Listed), "cite_as": s.CiteAs, "license": "CC0-1.0",
		"sections[" + s.Sections[0].Name + "].boards":             fmt.Sprint(s.Sections[0].Boards),
		"access_modes[mcp].boards":                                fmt.Sprint(s.AccessModes[0].Boards),
		"growth[" + s.Growth[len(s.Growth)-1].Date + "].verified": fmt.Sprint(s.Growth[len(s.Growth)-1].Verified),
		"datasets[" + s.Datasets[0].Name + "].posts_collected":    fmt.Sprint(s.Datasets[0].Posts),
		"swarmmemo_week.returning_agents":                         "5",
	} {
		if got[field] != want {
			t.Errorf("csv %s = %q, want %q", field, got[field], want)
		}
	}
	// Public counts only: no keys, fingerprints or identity keys from the datasets.
	for _, body := range []string{w.Body.String(), c.Body.String()} {
		if regexp.MustCompile(`[0-9a-f]{64}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|public_key|"name:`).MatchString(body) {
			t.Error("the dataset carries an identifier")
		}
	}
}

// boardlist_history.json is regenerated with the submodule: its last point
// is the list as embedded, and its dates only go forward.
func TestBoardlistHistoryMatchesList(t *testing.T) {
	if len(boardlistHistory) == 0 {
		t.Fatal("no history points")
	}
	for i := 1; i < len(boardlistHistory); i++ {
		if boardlistHistory[i].Date <= boardlistHistory[i-1].Date {
			t.Errorf("history dates out of order at %s", boardlistHistory[i].Date)
		}
	}
	verified := 0
	for _, sec := range agentBoardMap.Sections {
		verified += len(sec.Boards)
	}
	last := boardlistHistory[len(boardlistHistory)-1]
	if last.Verified != int64(verified) || last.Reported != int64(len(agentBoardMap.Reported)) || last.Completeness != int64(len(agentBoardMap.Completeness)) {
		t.Fatalf("boardlist_history.json ends at %+v, the list has %d/%d/%d: run python3 scripts/boardlist_history.py", last, verified, len(agentBoardMap.Reported), len(agentBoardMap.Completeness))
	}
}

// Access modes count words, not substrings, and an unreachable reason is one
// the list words as such.
func TestBoardStateMatchers(t *testing.T) {
	mode := func(id, text string) bool {
		for _, am := range boardAccessModes {
			if am.ID == id {
				return am.re.MatchString(text)
			}
		}
		t.Fatalf("no mode %s", id)
		return false
	}
	if !mode("mcp", "a hosted MCP endpoint") || mode("mcp", "MCPX") || !mode("llms_txt", "the manual is at /llms.txt.") || !mode("rss_atom", "Atom feed") || mode("rss_atom", "Atomic") {
		t.Error("access mode matching")
	}
	for text, want := range map[string]bool{"Offline: the domain no longer resolves.": true, "The HTTPS connection failed when checked.": true, "its named live node and homepage did not respond when checked.": true, "Lookalike of moltbook.com.": false} {
		if boardUnreachable.MatchString(text) != want {
			t.Errorf("unreachable(%q) != %v", text, want)
		}
	}
}

// C1063: each listed board's client label comes from its access sentence, and
// the map row and /boards/state.json carry the same one.
func TestBoardClientLabel(t *testing.T) {
	for access, want := range map[string]string{
		"Read with plain GET; post with a POST request.":                                    "plain_http",
		"Posting needs a join; writes have been paused since 2026-10-16 while it recovers.": "writes_paused",
		"Posting is paused since the trial ended 2026-11-12.":                               "writes_paused",
		"Reading is behind a cookie check that needs a browser User-Agent.":                 "browser_check",
		"Every request first meets a Cloudflare challenge.":                                 "browser_check",
		"A paused thread stays readable; post with a POST request.":                         "plain_http",
	} {
		if got := clientFor(access).ID; got != want {
			t.Errorf("%q: client %s, want %s", access, got, want)
		}
	}
	var s boardState
	if err := json.Unmarshal(getBoardState(t, &testService{}, BoardStatePath+".json").Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	byName := map[string]string{}
	for _, b := range s.Boards {
		if b.Group == "verified" {
			byName[b.Name] = b.Client
		}
	}
	page := getBoardState(t, &testService{}, BoardMapPath).Body.String()
	for _, sec := range agentBoardMap.Sections {
		for _, b := range sec.Boards {
			if byName[b.Name] != b.Client.ID || b.Client.ID == "" {
				t.Errorf("%s: state.json client %q, map %q", b.Name, byName[b.Name], b.Client.ID)
			}
			if !strings.Contains(page, `board-client-`+b.Client.ID+`"`) {
				t.Errorf("map page has no %s label", b.Client.ID)
			}
		}
	}
}
