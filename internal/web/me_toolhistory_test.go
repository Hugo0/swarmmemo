package web

import (
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// C1003: Me's tool history is one section of Settings, closed, and read only
// when it is opened: a visit to Me makes a fixed set of signed reads
// (messages_policy_test.cjs), and service.calls is sent from the panel's
// toggle alone.
func TestMeToolHistoryLoadsOnOpen(t *testing.T) {
	w := httptest.NewRecorder()
	Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", "/me", nil))
	body := w.Body.String()
	settings, section := strings.Index(body, `id="more"`), strings.Index(body, `<div class="me-section" id="tool-history">`)
	if w.Code != 200 || settings < 0 || section < settings {
		t.Fatal("/me has no tool history section in Settings")
	}
	panel := regexp.MustCompile(`<details class="me-sub" id="tool-history-panel"[^>]*>`).FindString(body[section:])
	if panel == "" || strings.Contains(panel, " open") {
		t.Fatalf("tool history panel = %q, want a closed details", panel)
	}
	raw, err := files.ReadFile("assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(raw)
	if n := strings.Count(js, "operation: 'service.calls'"); n != 1 {
		t.Fatalf("app.js sends service.calls from %d places, want 1 (loadToolHistory)", n)
	}
	if strings.Count(js, "void loadToolHistory()") != 2 || !strings.Contains(js, "$('tool-history-panel')?.addEventListener('toggle', event => { if (event.target.open) void loadToolHistory(); });") {
		t.Fatal("loadToolHistory runs from somewhere other than the panel's toggle (and its own reload after a cite)")
	}
}
