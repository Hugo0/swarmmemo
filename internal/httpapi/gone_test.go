package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The 1.0 rename removes names rather than aliasing them. A retired JSON route
// answers 410 with a body a caller can act on without reading prose: ok:false, a
// stable code, the exact replacement, and where the full map lives. It must never
// redirect, or a client keeps a working call under a name nothing else uses.
func TestRetiredJSONRoutesAreGoneAndMachineActionable(t *testing.T) {
	id := strings.Repeat("a", 64)
	for _, tc := range []struct{ path, replacement string }{
		{"/api/events", "/api/messages"},
		{"/api/events?limit=5", "/api/messages"},
		{"/api/identities", "/api/agents"},
		{"/api/peers?query=go", "/api/agents"},
		{"/api/identity/" + id, "/api/agent/" + id},
		{"/api/peer/" + id, "/api/agent/" + id},
	} {
		f := &fakeService{}
		w := makeRequest(New(f, nil, Config{}), "GET", tc.path, "", "")
		if w.Code != http.StatusGone {
			t.Errorf("%s: status %d, want 410", tc.path, w.Code)
			continue
		}
		if loc := w.Header().Get("Location"); loc != "" {
			t.Errorf("%s: redirected to %q; retired routes must not redirect", tc.path, loc)
		}
		if len(f.commands) != 0 {
			t.Errorf("%s: reached the board", tc.path)
		}
		var body struct {
			OK    bool `json:"ok"`
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
			Replacement string `json:"replacement"`
			Migration   string `json:"migration"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Errorf("%s: body is not JSON: %v", tc.path, err)
			continue
		}
		if body.OK || body.Error.Code != "route_gone" || body.Replacement != tc.replacement || body.Migration != "/migration" {
			t.Errorf("%s: unusable 410 body %s", tc.path, w.Body.String())
		}
		if !strings.Contains(body.Error.Message, tc.replacement) {
			t.Errorf("%s: message does not name the replacement: %s", tc.path, body.Error.Message)
		}
	}
}

// /for-agents is cited from llms.txt and from external listings. It was dropped
// from the nav, never from the service, and must not be caught by the 410 sweep.
func TestForAgentsAndNeutralAliasesSurviveTheRename(t *testing.T) {
	for _, path := range []string{"/api/messages", "/api/agents", "/recent", "/who"} {
		f := &fakeService{}
		w := makeRequest(New(f, nil, Config{}), "GET", path, "", "")
		if w.Code == http.StatusGone {
			t.Errorf("%s must keep working", path)
		}
	}
	w := httptest.NewRecorder()
	New(&fakeService{}, nil, Config{}).ServeHTTP(w, httptest.NewRequest("GET", "/for-agents", nil))
	if w.Code == http.StatusGone {
		t.Error("/for-agents must not be retired: it is cited outside SwarmMemo")
	}
}
