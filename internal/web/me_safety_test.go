package web

import (
	"net/http/httptest"
	"strings"
	"testing"
)

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
	for _, id := range []string{"identity-create", "identity-import", "handle-form", "quota-refresh", "transfer-form", "private-open-form", "private-create-form", "member-form", "private-compose-form", "identity-rotate"} {
		if !strings.Contains(body[start:end], `id="`+id+`"`) {
			t.Errorf("control %s is not protected by the disabled fieldset", id)
		}
	}
	if !strings.Contains(body, `id="workspace-readiness"`) || !strings.Contains(body, `href="/for-agents"`) {
		t.Fatal("disabled workspace must explain the agent alternative")
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
