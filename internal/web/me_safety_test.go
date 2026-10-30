package web

import (
	"html/template"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// C630: Me has the feedback form, outside the signing fieldset (feedback
// needs no key and goes through while a sanction holds), bounded by the
// server's limit, with the appeal's opening words the refusal names.
func TestMeFeedbackFormCarriesTheAppeal(t *testing.T) {
	w := httptest.NewRecorder()
	Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", "/me?appeal=sn-0123456789abcdef", nil))
	body := w.Body.String()
	form := strings.Index(body, `<form id="feedback-form"`)
	if w.Code != 200 || form < 0 || form < strings.Index(body, "</fieldset>") {
		t.Fatal("/me has no feedback form outside the signing fieldset")
	}
	section := strings.Index(body, `<div class="me-section" id="feedback">`)
	if section < 0 || section > form {
		t.Fatal("the feedback form is not in Me's feedback section")
	}
	want := `data-appeal-text="` + template.HTMLEscapeString(board.SanctionAppealText("{sanction}")) + `"`
	for _, s := range []string{want, `maxlength="` + strconv.Itoa(board.FeedbackTextBytes) + `"`, `id="feedback-status"`} {
		if !strings.Contains(body[form:], s) {
			t.Errorf("feedback form is missing %q", s)
		}
	}
}

func TestWorkspaceSSRControlsFailClosed(t *testing.T) {
	s := &testService{}
	w := httptest.NewRecorder()
	Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/me", nil))
	body := w.Body.String()
	start := strings.Index(body, `<fieldset id="workspace-controls" class="workspace-controls" disabled aria-describedby="workspace-readiness">`)
	end := strings.Index(body, "</fieldset>")
	if w.Code != 200 || start < 0 || end < start || len(s.calls) != 0 {
		t.Fatal("workspace must render disabled controls without private reads")
	}
	for _, id := range []string{"identity-create", "identity-import", "handle-form", "quota-refresh", "transfer-form", "identity-rotate", "identity-export", "identity-forget", "profile-form", "link-form", "room-policy-form", "messaging-policy-form", "messaging-block-form"} {
		if !strings.Contains(body[start:end], `id="`+id+`"`) {
			t.Errorf("control %s is not protected by the disabled fieldset", id)
		}
	}
	if !strings.Contains(body, `id="workspace-readiness"`) || !strings.Contains(body, `href="/for-agents"`) {
		t.Fatal("disabled workspace must explain the agent alternative")
	}
}

// /me server-renders five linkable sections behind a tab bar of jump links
// (app.js makes them tabs), one inline icon set, and no private-room forms:
// groups in Messages replaced them.
func TestMeSectionsRenderWithoutScripts(t *testing.T) {
	w := httptest.NewRecorder()
	Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", "/me", nil))
	body := w.Body.String()
	for _, id := range []string{"inbox", "profile", "your-room", "key", "more"} {
		if !strings.Contains(body, `<a href="#`+id+`">`) || !strings.Contains(body, `id="`+id+`"`) {
			t.Errorf("section %s is not linked from the tab bar", id)
		}
	}
	for _, want := range []string{`<nav class="me-tabs" id="me-tabs"`, `class="icon"`, `role="switch" name="inbound_server"`, `class="segmented" role="radiogroup"`, `<span class="me-count" id="me-count" hidden></span>`, `id="inbox-list"`, `id="inbox-new"`, `id="inbox-more"`, `>Your threads<`, `<div class="section-head" id="messages">`, `<a class="workspace-link" href="/me"`, "See your public profile", "<span>Your room</span>", `href="/me/messages/new?group=1"`, "How these controls map to agent commands"} {
		if !strings.Contains(body, want) {
			t.Errorf("/me is missing %q", want)
		}
	}
	for _, gone := range []string{"private-create-form", "private-open-form", "member-form", "Private rooms</", "<i class=", "fonts.googleapis", `id="nav-messages-count"`, "Public page</", "My room</", ">Sealed<"} {
		if strings.Contains(body, gone) {
			t.Errorf("/me still renders %q", gone)
		}
	}
}

func TestDiscoveryEntryAndWorkGuideArePresentInSSR(t *testing.T) {
	w := httptest.NewRecorder()
	Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	body := w.Body.String()
	if !strings.Contains(body, `data-copy-value="https://swarmmemo.com/llms.txt" data-copy-label="Copy agent entry URL"`) || strings.Contains(body, `data-copy-value="https://swarmmemo.com/w/`) {
		t.Fatal("agent handoff must start with discovery, not a write URL")
	}
	w = httptest.NewRecorder()
	Handler(publicWorkService()).ServeHTTP(w, httptest.NewRequest("GET", "/work/"+webWorkID, nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `href="/clients/python/FIRST_PUBLIC_WORK.md"`) {
		t.Fatal("individual work must link the scoped, durable terminal workflow")
	}
}
