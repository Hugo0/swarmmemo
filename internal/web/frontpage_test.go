package web

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

// The home feed is the front page: every public room except those taken off
// it (front_page false, the blocklist), so it never forces scope=all. Hot is
// the default, the tabs are New, Hot and Top, and no link carries a scope.
func TestHomeFeedIsTheFrontPage(t *testing.T) {
	feedData := func(path string) (string, string) {
		s := &testService{}
		w := httptest.NewRecorder()
		Handler(s).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		for _, c := range s.calls {
			if c.Operation == "messages.list" {
				return c.Data, w.Body.String()
			}
		}
		t.Fatalf("%s read no feed", path)
		return "", ""
	}
	data, body := feedData("/")
	if !strings.Contains(data, `"sort":"hot"`) || strings.Contains(data, `"scope":"all"`) {
		t.Fatalf("home: data %q", data)
	}
	tabs := between(body, `<nav class="sort-tabs" aria-label="Sort posts">`, `</nav>`)
	newAt, hotAt, topAt := strings.Index(tabs, `>New</a>`), strings.Index(tabs, `>Hot</a>`), strings.Index(tabs, `>Top</a>`)
	if newAt < 0 || !(newAt < hotAt && hotAt < topAt) || !strings.Contains(tabs, `<a href="/" aria-current="page"`) {
		t.Errorf("home tabs are not New, Hot (current), Top: %s", tabs)
	}
	for _, gone := range []string{"scope=all", "My feed", "sort-bias", "Include utility rooms", "On the feed", "Public feed"} {
		if strings.Contains(body, gone) {
			t.Errorf("home still shows %q", gone)
		}
	}
	if !strings.Contains(tabs, `href="/me#feed" id="feed-customize"`) {
		t.Error("home lacks the Customize link to Me's feed settings")
	}
	for _, path := range []string{"/?sort=new", "/?q=needle", "/?sort=top"} {
		if data, _ = feedData(path); strings.Contains(data, `"scope":"all"`) {
			t.Errorf("%s: data %q", path, data)
		}
	}
	// A chosen recency still pages on, without a scope.
	if _, body = feedData("/?sort=hot&bias=3&offset=40"); !strings.Contains(body, `href="/?sort=hot&amp;bias=3"`) {
		t.Error("hot pagination drops a chosen bias")
	}
}

// openWorkService is a service with open rewarded work to count.
type openWorkService struct {
	testService
	open int
}

func (s *openWorkService) PublicOpenRewardedWork(context.Context) (int, error) { return s.open, nil }

// The front page points at open rewarded work, which lives in #bounties,
// outside the default feed, once and below the feed: paid work is not the
// first thing a reader sees. With none open it says nothing.
func TestHomeShowsOpenRewardedWork(t *testing.T) {
	body := func(open int, path string) string {
		w := httptest.NewRecorder()
		Handler(&openWorkService{open: open}).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w.Body.String()
	}
	for open, want := range map[int]string{1: "1 paid task open", 12: "12 paid tasks open", 100: "100+ paid tasks open"} {
		b := body(open, "/")
		if !strings.Contains(b, `<a href="/work?kind=rewarded">`+want+` →</a>`) {
			t.Fatalf("%d open: no strip %q", open, want)
		}
		main := b[strings.Index(b, "<main"):strings.Index(b, "</main>")]
		if strings.Count(main, `href="/work?kind=rewarded"`) != 1 || strings.Contains(main, "See paid tasks") {
			t.Fatalf("%d open: paid tasks must have one entry point on the page", open)
		}
		if strings.Index(main, `id="open-work"`) < strings.Index(main, `id="feed"`) {
			t.Fatalf("%d open: the paid tasks line must sit below the feed", open)
		}
	}
	if b := body(0, "/"); strings.Contains(b, `id="open-work"`) {
		t.Fatal("strip shown with no open work")
	}
	if b := body(3, "/r/lobby"); strings.Contains(b, `id="open-work"`) {
		t.Fatal("strip shown in a room")
	}
}
