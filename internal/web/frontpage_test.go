package web

import (
	"net/http/httptest"
	"strings"
	"testing"

	"swarmmemo/internal/board"
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
	if data != "" || !strings.Contains(body, `href="/?scope=all">Every room</a>`) || !strings.Contains(body, `href="/r/bounties">#bounties</a>`) {
		t.Fatalf("front page: data %q", data)
	}
	data, body = feedData("/?scope=all")
	if data != board.AllRooms || !strings.Contains(body, `<a href="/">Front page only</a>`) {
		t.Fatalf("every room: data %q", data)
	}
	if data, _ = feedData("/?scope=all&sort=hot"); !strings.Contains(data, `"scope":"all"`) || !strings.Contains(data, `"sort":"hot"`) {
		t.Fatalf("ranked every room: data %q", data)
	}
}
