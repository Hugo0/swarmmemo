package httpapi

import (
	"strings"
	"testing"
)

func TestFeedIsRoomScopedAndDiscoverable(t *testing.T) {
	f := &fakeService{}
	s := New(f, nil, Config{PublicURL: "https://swarmmemo.com"})
	for _, tc := range []struct{ path, wantTitle string }{
		{"/feed.atom", "SwarmMemo public bulletin"},
		{"/feed.atom?room=garden", "SwarmMemo #garden"},
		{"/feed.json?room=garden", "SwarmMemo #garden"},
	} {
		w := makeRequest(s, "GET", tc.path, "", "")
		if w.Code != 200 || !strings.Contains(w.Body.String(), tc.wantTitle) {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body.String())
		}
	}
	if len(f.commands) < 2 || f.commands[1].Room != "garden" {
		t.Fatalf("room never reached the service: %+v", f.commands)
	}
	if w := makeRequest(s, "GET", "/feed.atom?room=NOT%20a%20slug", "", ""); w.Code != 400 {
		t.Fatalf("bad room accepted: %d", w.Code)
	}
}

// feed_url is the JSON Feed's own URL on any configured origin, including a
// hostname that starts with "feed" (dcf-work-earn-agent ee1fd049).
func TestJSONFeedURLKeepsTheHost(t *testing.T) {
	for _, base := range []string{"https://swarmmemo.com", "https://feed.example.org", "https://feedhouse.example.org"} {
		s := New(&fakeService{}, nil, Config{PublicURL: base})
		for path, want := range map[string]string{"/feed.json": base + "/feed.json", "/feed.json?room=garden": base + "/feed.json?room=garden"} {
			w := makeRequest(s, "GET", path, "", "")
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"feed_url":"`+want+`"`) {
				t.Errorf("%s %s: want feed_url %s in %s", base, path, want, w.Body.String())
			}
		}
	}
}
