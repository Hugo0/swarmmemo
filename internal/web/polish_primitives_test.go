package web

import (
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// Server-rendered primitives contract (no browser): buttons carry verbs without navigation arrows, the composer
// destination is signposted and its editing control is hidden until the script runs, the native fixed action and
// read-only fields stay, and the public-room datalist is emitted for the home view.
func TestPrimitivesServerRenderedContract(t *testing.T) {
	button := regexp.MustCompile(`<(button|a class="button[^"]*")[^>]*>[^<]*[↗→←]`)
	for _, path := range []string{"/", "/rooms", "/agents", "/me", "/docs", "/for-agents", "/work", "/limits", "/policy"} {
		w := httptest.NewRecorder()
		Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		body := w.Body.String()
		if m := button.FindString(body); m != "" {
			t.Errorf("%s: button label carries a navigation arrow: %q", path, m)
		}
	}
	w := httptest.NewRecorder()
	Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	body := w.Body.String()
	for _, want := range []string{
		`<summary>Leave a message</summary>`,
		`<span class="compose-destination-label">To</span>`,
		`<span class="compose-destination-value" id="compose-destination-value">#lobby /main</span>`,
		`<button type="button" class="quiet-button compose-change" id="compose-change" aria-controls="compose-settings" hidden>Change</button>`,
		`action="/w/lobby/main?format=json" method="post"`,
		`name="room" value="lobby" required readonly`,
		`name="page" value="main" required readonly`,
		`list="room-options"`,
		`<datalist id="room-options">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("home composer missing %q", want)
		}
	}
	if strings.Contains(body, "Destination:") {
		t.Error("old inert destination label still rendered")
	}
}
