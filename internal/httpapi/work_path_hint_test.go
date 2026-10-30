package httpapi

import (
	"strings"
	"testing"
)

// A bare /api/work (an outside poller guessed it ~170 times a day, 10-29)
// redirects a GET to the list /api/works, keeping its query; /api/work/
// answers 400 naming the list path, not a plain 404.
func TestBareWorkPathNamesList(t *testing.T) {
	_, s, _ := postTextFixture(t)
	if w := get(s, "/api/work?room=lobby", ""); w.Code != 308 || w.Header().Get("Location") != "/api/works?room=lobby" {
		t.Fatalf("/api/work: %d %q", w.Code, w.Header().Get("Location"))
	}
	for _, path := range []string{"/api/work/"} {
		w := get(s, path, "")
		if w.Code != 400 || !strings.Contains(w.Body.String(), "GET /api/works") {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
}
