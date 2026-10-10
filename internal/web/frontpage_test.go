package web

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

// The home feed is the front page, with every room one link away; the
// "Every room" view reads scope=all, as the API does.
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
	if !strings.Contains(data, `"sort":"new"`) || !strings.Contains(body, `href="/?scope=all">Include utility rooms</a>`) {
		t.Fatalf("front page: data %q", data)
	}
	data, body = feedData("/?scope=all")
	if !strings.Contains(data, `"scope":"all"`) || !strings.Contains(data, `"sort":"new"`) || !strings.Contains(body, `<a href="/">Front page only</a>`) {
		t.Fatalf("every room: data %q", data)
	}
	if data, _ = feedData("/?scope=all&sort=hot"); !strings.Contains(data, `"scope":"all"`) || !strings.Contains(data, `"sort":"hot"`) {
		t.Fatalf("ranked every room: data %q", data)
	}
	// The sort tabs and bias steps keep the chosen scope, and the default
	// front page's links never add it (dcf-work-earn-agent d28cab6e).
	_, body = feedData("/?scope=all&sort=hot")
	for _, want := range []string{`href="/?scope=all"`, `href="/?sort=hot&amp;scope=all"`, `href="/?sort=top&amp;scope=all"`, `href="/?sort=hot&amp;scope=all&amp;bias=`} {
		if !strings.Contains(body, want) {
			t.Errorf("every room, hot: no %s", want)
		}
	}
	_, body = feedData("/?sort=hot")
	if strings.Contains(body, `sort=top&amp;scope=all`) || strings.Contains(body, `sort=hot&amp;scope=all`) || !strings.Contains(body, `href="/?sort=top"`) {
		t.Error("the front page's sort links must not add scope=all")
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
