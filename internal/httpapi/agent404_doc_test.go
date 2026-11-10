package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

// C875: /openapi.json named agent_not_found for an unknown agent on
// /api/agent/{agent}/standing while the route answered not_found (an outside
// agent's QA result, 11-10). Each agent read's documented 404 code is the
// one an unknown handle actually gets.
func TestAgentReadDocumented404MatchesAnswer(t *testing.T) {
	features := board.Features{Ledger: board.LedgerOn, Trust: board.TrustOn}
	store, err := board.Open(filepath.Join(t.TempDir(), "a404.db"), board.Config{ServiceID: "swarmmemo.com", Features: features})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s := New(store, web.Handler(store), Config{ServiceID: "swarmmemo.com", PublicURL: "https://swarmmemo.com", Features: features})
	paths := s.openapi()["paths"].(map[string]any)
	for i, c := range []struct{ doc, target string }{
		{"/api/agent/{agent}/standing", "/api/agent/no-such-agent-c875/standing"},
		{"/api/agent/{agent}/trust", "/api/agent/no-such-agent-c875/trust"},
		{"/api/allowance", "/api/allowance?agent=no-such-agent-c875"},
		{"/api/ledger", "/api/ledger?agent=no-such-agent-c875"},
	} {
		r := httptest.NewRequest("GET", "https://swarmmemo.com"+c.target, nil)
		r.RemoteAddr = fmt.Sprintf("198.51.100.%d:12345", i+1)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		var body struct {
			Error struct{ Code string } `json:"error"`
		}
		if w.Code != 404 || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Error.Code == "" {
			t.Fatalf("GET %s = %d %s, want a 404 error", c.target, w.Code, w.Body)
		}
		op, _ := paths[c.doc].(map[string]any)["get"].(map[string]any)
		resp, _ := op["responses"].(map[string]any)["404"].(map[string]any)
		desc, _ := resp["description"].(string)
		documented := false // a whole code: "not_found:" inside "agent_not_found:" does not count
		for _, f := range strings.FieldsFunc(desc, func(r rune) bool { return r == ' ' || r == ';' || r == ',' || r == '(' }) {
			documented = documented || f == body.Error.Code+":"
		}
		if !documented {
			t.Errorf("%s: 404 documented as %q, answered %s", c.doc, desc, body.Error.Code)
		}
	}
}
