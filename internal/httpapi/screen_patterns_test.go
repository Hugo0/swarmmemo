package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"swarmmemo/internal/leakscan"
	"swarmmemo/internal/services"
)

// GET /api/screen/leak-patterns serves exactly the list the web composer's
// asset and the Python client's LEAK_PATTERNS block carry (a drift guard
// over the generated files), cacheable by ETag, and is in /capabilities and
// the OpenAPI document.
func TestLeakPatternsEndpoint(t *testing.T) {
	s := realServer(t)
	w := get(s, LeakPatternsPath, "")
	body := w.Body.Bytes()
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") || w.Header().Get("ETag") == "" || !bytes.Equal(body, leakscan.PatternsJSON()) {
		t.Fatalf("leak patterns: %d %v %.200s", w.Code, w.Header(), body)
	}
	asset, err := os.ReadFile("../web/assets/leak-patterns.json")
	if err != nil || !bytes.Equal(asset, body) {
		t.Fatalf("internal/web/assets/leak-patterns.json differs from %s; run go generate ./internal/leakscan (%v)", LeakPatternsPath, err)
	}
	if served := get(s, "/assets/leak-patterns.json", ""); served.Code != 200 || !bytes.Equal(served.Body.Bytes(), body) {
		t.Fatalf("/assets/leak-patterns.json: %d", served.Code)
	}
	client, err := os.ReadFile("../../clients/python/swarmmemo.py")
	if err != nil {
		t.Fatal(err)
	}
	_, block, ok := strings.Cut(string(client), `LEAK_PATTERNS = json.loads(r"""`+"\n")
	block, _, ok2 := strings.Cut(block, `""")`)
	if !ok || !ok2 || block != string(body) {
		t.Fatal("clients/python/swarmmemo.py's LEAK_PATTERNS differs from the served list; run go generate ./internal/leakscan")
	}
	r := httptest.NewRequest("GET", LeakPatternsPath, nil)
	r.Header.Set("If-None-Match", w.Header().Get("ETag"))
	cached := httptest.NewRecorder()
	s.ServeHTTP(cached, r)
	if cached.Code != 304 || cached.Body.Len() != 0 {
		t.Fatalf("If-None-Match: %d", cached.Code)
	}
	head := httptest.NewRecorder()
	s.ServeHTTP(head, httptest.NewRequest("HEAD", LeakPatternsPath, nil))
	if head.Code != 200 || head.Body.Len() != 0 {
		t.Fatalf("HEAD: %d %d", head.Code, head.Body.Len())
	}
	if w := get(s, LeakPatternsPath+"?v=2", ""); w.Code != 400 {
		t.Fatalf("a query: %d", w.Code)
	}
	post := httptest.NewRecorder()
	s.ServeHTTP(post, httptest.NewRequest("POST", LeakPatternsPath, nil))
	if post.Code != 405 {
		t.Fatalf("POST: %d", post.Code)
	}
	var caps struct {
		Conversations map[string]any `json:"conversations"`
	}
	if err = json.Unmarshal(get(s, "/capabilities", "application/json").Body.Bytes(), &caps); err != nil || caps.Conversations["leak_patterns"] != LeakPatternsPath || caps.Conversations["screening"] == nil {
		t.Fatalf("capabilities: %v %v", caps.Conversations, err)
	}
	// The pattern check's price comes from the quote's own price, which is
	// free (T57 I4: it once said 1 credit).
	outbound, _ := caps.Conversations["screening"].(map[string]any)["outbound"].(string)
	if services.LeakPatternsPriceText() != "free" || !strings.Contains(outbound, "patterns (free)") || strings.Contains(outbound, "credit") {
		t.Fatalf("screening.outbound: %q", outbound)
	}
	if body := get(s, "/capabilities", "application/json").Body.String(); strings.Contains(body, "patterns (1 credit)") {
		t.Fatalf("capabilities still price the patterns at 1 credit")
	}
	var spec struct {
		Paths      map[string]any `json:"paths"`
		Components struct {
			Schemas map[string]any `json:"schemas"`
		} `json:"components"`
	}
	if err = json.Unmarshal(get(s, "/openapi.json", "").Body.Bytes(), &spec); err != nil || spec.Paths[LeakPatternsPath] == nil || spec.Components.Schemas["LeakResult"] == nil || spec.Components.Schemas["LeakPatterns"] == nil {
		t.Fatalf("openapi: %v", err)
	}
}
