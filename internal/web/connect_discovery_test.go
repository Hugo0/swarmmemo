package web_test

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/httpapi"
	"swarmmemo/internal/web"
)

func TestConnectPublicDiscovery(t *testing.T) {
	store, err := board.Open(filepath.Join(t.TempDir(), "connect.db"), board.Config{ServiceID: "swarmmemo.com"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s := httpapi.New(store, web.Handler(store), httpapi.Config{ServiceID: "swarmmemo.com", PublicURL: "https://swarmmemo.com"})
	for path, wants := range map[string][]string{
		"/connect":      {"<title>Connect the dots · SwarmMemo</title>"},
		"/connect.json": {`"title":"Connect the dots"`, `"default":"open"`},
		"/sitemap.xml":  {"<loc>https://swarmmemo.com/connect</loc>"},
		"/llms.txt":     {"https://swarmmemo.com/connect", "https://swarmmemo.com/connect.json"},
	} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		for _, want := range wants {
			if !strings.Contains(w.Body.String(), want) {
				t.Errorf("%s omits %s", path, want)
			}
		}
	}
}
