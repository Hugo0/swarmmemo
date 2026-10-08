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
		`<h2 id="stats-feeds">Personal feeds</h2>`,
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
