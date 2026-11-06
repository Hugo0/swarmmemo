package httpapi

import (
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

// C772: an /api/ path with a trailing "/" or no route at all answered the
// mux's text/plain "404 page not found". Every exact GET route /openapi.json
// (every service and feature on) lists 308s to its path from the trailing-slash form (keeping the query),
// and an /api/ miss is the JSON error envelope.
func TestAPITrailingSlashAndMissAreJSON(t *testing.T) {
	features := board.Features{Ledger: board.LedgerOn, Trust: board.TrustOn, Services: []string{"memory", "wakeup", "notary", "screen", "public_data", "echo"}}
	store, err := board.Open(filepath.Join(t.TempDir(), "slash.db"), board.Config{ServiceID: "swarmmemo.com", Features: features})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s := New(store, web.Handler(store), Config{ServiceID: "swarmmemo.com", PublicURL: "https://swarmmemo.com", Features: features})
	n := 0
	serve := func(method, target string) *httptest.ResponseRecorder {
		n++
		r := httptest.NewRequest(method, "https://swarmmemo.com"+target, nil)
		r.RemoteAddr = fmt.Sprintf("198.51.%d.%d:12345", n/250, n%250+1)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	var exact []string
	for path, ops := range s.openapi()["paths"].(map[string]any) {
		if _, get := ops.(map[string]any)["get"]; get && strings.HasPrefix(path, "/api/") && !strings.Contains(path, "{") {
			exact = append(exact, path)
		}
	}
	sort.Strings(exact)
	for _, reported := range []string{"/api/works", "/api/messages", "/api/services", "/api/trust/standing"} {
		if !slices.Contains(exact, reported) {
			t.Errorf("%s is not an exact GET route in /openapi.json", reported)
		}
	}
	if len(exact) < 20 {
		t.Fatalf("only %d exact GET /api routes in /openapi.json", len(exact))
	}
	for _, path := range exact {
		for _, method := range []string{"GET", "HEAD"} {
			w := serve(method, path+"/?limit=1")
			if w.Code != 308 || w.Header().Get("Location") != path+"?limit=1" {
				t.Errorf("%s %s/ = %d Location %q, want 308 %s?limit=1", method, path, w.Code, w.Header().Get("Location"), path)
			}
		}
		if w := serve("POST", path+"/"); strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
			t.Errorf("POST %s/ = %d %s: text/plain", path, w.Code, w.Body)
		}
	}
	for _, path := range []string{"/api/nonexistent", "/api/nonexistent/", "/api/works/x/y/z/", "/api/trust/standing/x"} {
		w := serve("GET", path)
		if w.Code != 404 || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") || !strings.Contains(w.Body.String(), `"code":"not_found"`) || !strings.Contains(w.Body.String(), `"ok":false`) {
			t.Errorf("GET %s = %d %q %s, want the JSON 404", path, w.Code, w.Header().Get("Content-Type"), w.Body)
		}
	}
}
