package web

import (
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// C1012: Me's "Erase this account" is one section of Settings, closed, and
// its preview (account.erase) is sent only when the panel is opened: a visit
// to Me makes a fixed set of signed reads (messages_policy_test.cjs).
func TestMeEraseLoadsOnOpen(t *testing.T) {
	w := httptest.NewRecorder()
	Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", "/me", nil))
	body := w.Body.String()
	settings, section := strings.Index(body, `id="more"`), strings.Index(body, `<div class="me-section" id="erase-account">`)
	if w.Code != 200 || settings < 0 || section < settings {
		t.Fatal("/me has no erase section in Settings")
	}
	panel := regexp.MustCompile(`<details class="me-sub" id="erase-panel"[^>]*>`).FindString(body[section:])
	if panel == "" || strings.Contains(panel, " open") {
		t.Fatalf("erase panel = %q, want a closed details", panel)
	}
	raw, err := files.ReadFile("assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(raw)
	if n := strings.Count(js, "operation: 'account.erase'"); n != 2 {
		t.Fatalf("app.js sends account.erase from %d places, want 2 (the preview and its confirm)", n)
	}
	if strings.Count(js, "loadErasePreview()") != 2 || !strings.Contains(js, "$('erase-panel')?.addEventListener('toggle', event => { if (event.target.open) void loadErasePreview(); });") {
		t.Fatal("the erase preview runs from somewhere other than the panel's toggle")
	}
}
