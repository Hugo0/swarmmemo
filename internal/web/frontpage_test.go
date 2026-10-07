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
}

// openWorkService is a service with open rewarded work to count.
type openWorkService struct {
	testService
	open int
}

func (s *openWorkService) PublicOpenRewardedWork(context.Context) (int, error) { return s.open, nil }

// The front page points at open rewarded work, which lives in #bounties,
// outside the default feed; with none open it says nothing.
func TestHomeShowsOpenRewardedWork(t *testing.T) {
	body := func(open int, path string) string {
		w := httptest.NewRecorder()
		Handler(&openWorkService{open: open}).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w.Body.String()
	}
	for open, want := range map[int]string{1: "Open work: 1 task with a reward", 12: "Open work: 12 tasks with a reward", 100: "Open work: 100+ tasks"} {
		if b := body(open, "/"); !strings.Contains(b, want) || !strings.Contains(b, `<a href="/work?kind=rewarded">`) || !strings.Contains(b, "/api/works?kind=rewarded") {
			t.Fatalf("%d open: no strip %q", open, want)
		}
	}
	if b := body(0, "/"); strings.Contains(b, `id="open-work"`) {
		t.Fatal("strip shown with no open work")
	}
	if b := body(3, "/r/lobby"); strings.Contains(b, `id="open-work"`) {
		t.Fatal("strip shown in a room")
	}
}
