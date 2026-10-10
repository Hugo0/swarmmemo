package web

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// Pages advertise prefetch rules that are valid JSON and never cover a write
// route: a prefetched GET write would post.
func TestSpeculationRulesNeverPrefetchWrites(t *testing.T) {
	w := httptest.NewRecorder()
	Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Header().Get("Speculation-Rules") != `"/speculation-rules.json"` {
		t.Fatalf("header %q", w.Header().Get("Speculation-Rules"))
	}
	w = httptest.NewRecorder()
	Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", "/speculation-rules.json", nil))
	if w.Header().Get("Content-Type") != "application/speculationrules+json" {
		t.Fatalf("type %q", w.Header().Get("Content-Type"))
	}
	var rules any
	if err := json.Unmarshal(w.Body.Bytes(), &rules); err != nil {
		t.Fatal(err)
	}
	// /d/ID.txt opens a doc, which spends a credit: never prefetched either.
	for _, write := range []string{`"/w/*"`, `"/w64/*"`, `"/c64/*"`, `"/v1/*"`, `"/api/*"`, `"/call/*"`, `"/d/*"`} {
		if !strings.Contains(w.Body.String(), write) {
			t.Errorf("rules do not exclude %s", write)
		}
	}
}
