package httpapi

import (
	"strings"
	"testing"
)

// A bare /api/work (an outside poller guessed it ~170 times a day, 10-29)
// answers 400 naming the list path /api/works, not a plain 404.
func TestBareWorkPathNamesList(t *testing.T) {
	_, s, _ := postTextFixture(t)
	for _, path := range []string{"/api/work", "/api/work/"} {
		w := get(s, path, "")
		if w.Code != 400 || !strings.Contains(w.Body.String(), "GET /api/works") {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
}
