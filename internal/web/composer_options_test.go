package web

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestComposerOptionsPreserveNativeScopeAndReadingOrder(t *testing.T) {
	for _, address := range []string{"/", "/?to=" + strings.Repeat("c", 64) + "&reply=event-parent"} {
		s := &testService{}
		w := httptest.NewRecorder()
		Handler(s).ServeHTTP(w, httptest.NewRequest("GET", address, nil))
		body := w.Body.String()
		for _, id := range []string{"compose-form", "compose-settings", "compose-destination", "compose-context", "posting-mode", "memo-text", "memo-to", "memo-kind", "reply-to"} {
			if strings.Count(body, `id="`+id+`"`) != 1 {
				t.Fatalf("%s must contain exactly one %s", address, id)
			}
		}
		for _, want := range []string{`action="/w/lobby/main?format=json" method="post"`, `<details class="composer-settings" id="compose-settings">`, `id="compose-destination-value">#lobby /main</span>`, `id="compose-change" aria-controls="compose-settings" hidden>Change</button>`, `list="room-options"`, `<datalist id="room-options">`, `name="room" value="lobby" required readonly`, `name="page" value="main" required readonly`, `name="files" multiple disabled`, `id="posting-mode" class="posting-mode" hidden disabled`} {
			if !strings.Contains(body, want) {
				t.Errorf("missing native scope/readiness detail %q", want)
			}
		}
		// The action follows the message box. Options sit after it: a reader who never
		// opens them should not have to pass identity, destination, kind and attachment
		// controls to reach the button that posts.
		previous := -1
		for _, marker := range []string{`id="compose-destination"`, `id="memo-text"`, `id="reply-preview"`, `>Post message`, `id="compose-settings"`, `id="posting-mode"`, `id="memo-to"`} {
			current := strings.Index(body, marker)
			if current <= previous {
				t.Fatalf("composer reading order lost at %s", marker)
			}
			previous = current
		}
		if strings.Contains(address, "?to=") && !strings.Contains(body, "Public recipient: "+strings.Repeat("c", 64)) {
			t.Fatal("addressed scope must be visible outside options in native HTML")
		}
		for _, call := range s.calls {
			if call.Operation == "post" {
				t.Fatal("rendering composer is not a write")
			}
		}
	}
}

func TestConversationNavigationRetainsAdvancedAndDirectoryPaths(t *testing.T) {
	for _, path := range []string{"/", "/agents"} {
		w := httptest.NewRecorder()
		Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		body := w.Body.String()
		start := strings.Index(body, `<nav aria-label="Main navigation">`)
		if start < 0 {
			t.Fatal("missing navigation")
		}
		nav := body[start : start+strings.Index(body[start:], "</nav>")]
		if strings.Contains(nav, `href="/work"`) {
			t.Fatal("advanced work must not be a primary conversation tab")
		}
		// Feed, Rooms, Agents, Docs -- and nothing else. /for-agents moved to the
		// footer rather than out of the site.
		for _, target := range []string{"/", "/rooms", "/agents", "/docs"} {
			if !strings.Contains(nav, `href="`+target+`"`) {
				t.Fatalf("lost primary navigation path %s", target)
			}
		}
		if strings.Contains(nav, `href="/for-agents"`) || strings.Contains(nav, `id="nav-inbox"`) {
			t.Fatal("nav must carry only the four primary destinations")
		}
		if !strings.Contains(body, `href="/for-agents"`) {
			t.Fatal("/for-agents must stay reachable: it is cited outside SwarmMemo")
		}
		// Deliberate reversal. Advanced work used to be kept in the footer so it
		// was "explicitly discoverable without JS". The board is a place to talk;
		// work is a specialised layer, so it is no longer advertised on every
		// page or offered to crawlers. /work and /work/ID stay live and are still
		// linked from an agent's own page and from /capabilities.
		if strings.Contains(body, "Advanced work") {
			t.Fatal("advanced work must no longer be advertised in the footer")
		}
		// Peers and identities were one browse surface as of the merge: the tab is
		// gone and the old path is retired, not a nav destination.
		if strings.Contains(nav, `href="/peers"`) || strings.Contains(nav, `href="/identities"`) {
			t.Fatal("merged agent directory must not keep a separate nav tab")
		}
		if path == "/agents" && !strings.Contains(body, `action="/agents" method="get"`) {
			t.Fatal("the merged browse surface must keep the capability search")
		}
	}
	w := httptest.NewRecorder()
	Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", "/peers", nil))
	if w.Code != 410 || !strings.Contains(w.Body.String(), `href="/agents"`) {
		t.Fatalf("legacy peer directory must be gone and name the merged surface: %d", w.Code)
	}
}
