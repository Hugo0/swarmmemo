package web

import (
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestGraphPageLoadsItsModuleOnlyThere(t *testing.T) {
	w := httptest.NewRecorder()
	Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", "/swarmchasing", nil))
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "</html>") {
		t.Fatalf("/swarmchasing: %d", w.Code)
	}
	for _, want := range []string{
		`<h1 id="paper-title">Swarmchasing</h1>`, "Abstract", `<span class="label">Figure 1.</span>`, `<span class="label">Table 1.</span>`, "9,724", `id="graph-expand"`, `<script type="module" src="/assets/graph.js">`, `href="/assets/graph.css"`,
		`<link rel="modulepreload" href="/assets/graph-gl.js">`, `id="graph-canvas"`, `id="graph-fallback"`,
		`id="graph-sound" aria-pressed="false">Sound off`, "tap to hear", `id="graph-copy-prompt"`, `href="/api/graph/universe"`, `id="graph-search-input"`, `id="graph-speed"`,
		`<a href="/swarmchasing" aria-current="page">Research: Swarmchasing</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/swarmchasing lacks %q", want)
		}
	}
	if regexp.MustCompile(`\sstyle=|<script>|https?://cdn`).MatchString(body) {
		t.Error("/swarmchasing uses an inline style or script, or a CDN, which the CSP blocks")
	}
	// Every other page links the graph from its footer but loads none of it.
	docs := httptest.NewRecorder()
	Handler(&testService{}).ServeHTTP(docs, httptest.NewRequest("GET", "/docs", nil))
	if !strings.Contains(docs.Body.String(), `href="/swarmchasing"`) || strings.Contains(docs.Body.String(), "graph-gl.js") || strings.Contains(docs.Body.String(), "graph.js") {
		t.Error("/docs must link /swarmchasing without loading its scripts")
	}
	// The renderer and its decoding worker are served from this site; no
	// third-party bundle is vendored.
	for _, path := range []string{"/assets/graph-gl.js", "/assets/graph-worker.js", "/assets/graph.js"} {
		a := httptest.NewRecorder()
		Handler(&testService{}).ServeHTTP(a, httptest.NewRequest("GET", path, nil))
		if a.Code != 200 || a.Body.Len() < 1000 {
			t.Errorf("%s: %d, %d bytes", path, a.Code, a.Body.Len())
		}
	}
}

// /graph moved to /swarmchasing: the old page and anything under it redirect
// permanently, keeping the query string; the API keeps its /api/graph paths.
func TestGraphRedirectsToSwarmchasing(t *testing.T) {
	for path, want := range map[string]string{
		"/graph":              "/swarmchasing",
		"/graph?focus=a&b=1":  "/swarmchasing?focus=a&b=1",
		"/graph/":             "/swarmchasing",
		"/graph/anything?x=1": "/swarmchasing?x=1",
	} {
		w := httptest.NewRecorder()
		Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 301 || w.Header().Get("Location") != want {
			t.Errorf("%s: %d -> %q, want 301 -> %q", path, w.Code, w.Header().Get("Location"), want)
		}
	}
	w := httptest.NewRecorder()
	Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", "/graphs", nil))
	if w.Code == 301 {
		t.Error("/graphs must not redirect")
	}
}
