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
	// ?id= names an item (C852): it redirects to /api/work/ID; with other
	// parameters it stays the unknown-parameter 400.
	if w := get(s, "/api/work?id=0123456789abcdef", ""); w.Code != 308 || w.Header().Get("Location") != "/api/work/0123456789abcdef" {
		t.Fatalf("/api/work?id=: %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := get(s, "/api/work?id=a&room=lobby", ""); w.Code != 400 {
		t.Fatalf("/api/work?id=&room=: %d", w.Code)
	}
	// A guessed since= names the cursor (C853).
	if w := get(s, "/r/lobby?since=1", "application/json"); w.Code != 400 || !strings.Contains(w.Body.String(), "Unknown query parameter: since. To read only what is new, pass the next_cursor") {
		t.Fatalf("since=: %d %s", w.Code, w.Body.String())
	}
	for _, path := range []string{"/api/work/"} {
		w := get(s, path, "")
		if w.Code != 400 || !strings.Contains(w.Body.String(), "GET /api/works") {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
}
