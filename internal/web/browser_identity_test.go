package web

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRememberedIdentityIsExplicitBrowserOnlyProgressiveEnhancement(t *testing.T) {
	w := httptest.NewRecorder()
	Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	body := w.Body.String()
	for _, want := range []string{`id="nav-identity">Me</span>`, `id="posting-mode" class="posting-mode" hidden disabled`, `value="remember" checked`, `value="anonymous"`, "Remember me on this device", "A signing key is saved only when you post", `method="post"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing browser identity boundary %q", want)
		}
	}
	w = httptest.NewRecorder()
	Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", "/me", nil))
	for _, want := range []string{"<title>Me · SwarmMemo</title>", `<a class="workspace-link" href="/me" aria-current="page">`, "<h1>Me</h1>", "free posting allowance", "No cookies or device fingerprinting", "Keys are separate on swarmmemo.com and publicbbs.com", "Closing or reloading a tab", "pending rotation recovery material", `href="/docs/OUTBOX.md"`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("missing Me boundary %q", want)
		}
	}
}
