package web

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// feedStatsService answers FeedStats with fixed stats over a real store.
type feedStatsService struct {
	*board.Store
	stats *board.FeedStats
}

func (c feedStatsService) FeedStats(context.Context) (*board.FeedStats, error) { return c.stats, nil }

// inboxReadService is a real store whose flags say INBOX_ENTRIES=read, with
// fixed inbox stats.
type inboxReadService struct {
	*board.Store
	stats *board.InboxStats
}

func (c inboxReadService) Features() board.Features {
	f := c.Store.Features()
	f.InboxEntries = board.InboxRead
	return f
}
func (c inboxReadService) InboxStats(context.Context) (*board.InboxStats, error) { return c.stats, nil }

// C71: /stats has an inbox line (counts only) and Me a waiting list, only
// under INBOX_ENTRIES=read.
func TestInboxStatsAndWaitingList(t *testing.T) {
	f := newArticleFixture(t)
	f.post(board.Command{Room: "lobby", Page: "main", Text: "hello inbox"})
	if body := f.get("/stats").Body.String(); strings.Contains(body, "stats-inbox") {
		t.Fatal("/stats draws the inbox while it is off")
	}
	if body := f.get("/me").Body.String(); strings.Contains(body, "me-waiting") {
		t.Fatal("Me lists waiting entries while the inbox is off")
	}
	st := &board.InboxStats{Days: 30, Entries: map[string]int64{"addressed": 3, "request": 1}, Dispositions: map[string]int64{"replied": 2, "answered_elsewhere": 1}, Waiting: 1}
	get := func(path string) string {
		w := httptest.NewRecorder()
		Handler(inboxReadService{f.store, st}).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w.Body.String()
	}
	stats := get("/stats")
	for _, want := range []string{
		`<h3 id="stats-inbox">Inboxes</h3>`,
		`<dt>Inbox entries</dt><dd class="stat-value">4</dd><dd class="stat-note">3 addressed, 1 request</dd>`,
		`<dt>Marked done</dt><dd class="stat-value">3</dd><dd class="stat-note">2 replied, 1 answered elsewhere</dd>`,
		`<dt>Waiting for an answer</dt><dd class="stat-value">1</dd>`,
	} {
		if !strings.Contains(stats, want) {
			t.Errorf("/stats lacks %q", want)
		}
	}
	if me := get("/me"); !strings.Contains(me, `id="me-waiting-list"`) || !strings.Contains(me, `id="me-waiting-count"`) {
		t.Error("Me lacks the waiting list")
	}
}

// /stats shows saved feed profiles, the most-forked and the most-followed
// rooms; nothing while the memory service is off.
func TestStatsPageShowsFeeds(t *testing.T) {
	f := newArticleFixture(t)
	f.post(board.Command{Room: "lobby", Page: "main", Text: "hello stats"})
	if body := f.get("/stats").Body.String(); strings.Contains(body, "stats-feeds") {
		t.Fatal("/stats draws feeds while memory is off")
	}
	agent := strings.Repeat("ab", 32)
	st := &board.FeedStats{Public: 3, Private: 1,
		MostForked: []board.FeedForkedStat{{Agent: agent, Handle: "weaver", Name: "research first", Forks: 2}},
		MostRooms:  []board.FeedRoomStat{{Room: "research", Subscribers: 4}}}
	w := httptest.NewRecorder()
	Handler(feedStatsService{f.store, st}).ServeHTTP(w, httptest.NewRequest("GET", "/stats", nil))
	body := w.Body.String()
	for _, want := range []string{
		`<h3 id="stats-feeds">Personal feeds</h3>`,
		`<dt>Saved feed profiles</dt><dd class="stat-value">4</dd><dd class="stat-note">3 public, 1 private</dd>`,
		`<a href="/agent/` + agent + `">weaver: research first</a></th><td class="num">2</td>`,
		`<a href="/r/research">#research</a></th><td class="num">4</td>`,
		`<a href="/api/stats/feeds">/api/stats/feeds</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/stats lacks %q", want)
		}
	}
}
