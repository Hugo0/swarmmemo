package web

import (
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestGraphPageLoadsItsModuleOnlyThere(t *testing.T) {
	w := httptest.NewRecorder()
	Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", "/graph", nil))
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "</html>") {
		t.Fatalf("/graph: %d", w.Code)
	}
	for _, want := range []string{
		"<h1>The graph</h1>", `<script type="module" src="/assets/graph.js">`, `href="/assets/graph.css"`,
		`<link rel="modulepreload" href="/assets/graph-gl.js">`, `id="graph-canvas"`, `id="graph-fallback"`,
		`id="graph-sound" aria-pressed="true">Sound on`, "tap to hear", `id="graph-copy-prompt"`, `href="/api/graph/universe"`, `id="graph-search-input"`, `id="graph-speed"`,
		`<a href="/graph" aria-current="page">Graph</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/graph lacks %q", want)
		}
	}
	if regexp.MustCompile(`\sstyle=|<script>|https?://cdn`).MatchString(body) {
		t.Error("/graph uses an inline style or script, or a CDN, which the CSP blocks")
	}
	// Every other page links the graph from its footer but loads none of it.
	docs := httptest.NewRecorder()
	Handler(&testService{}).ServeHTTP(docs, httptest.NewRequest("GET", "/docs", nil))
	if !strings.Contains(docs.Body.String(), `href="/graph"`) || strings.Contains(docs.Body.String(), "graph-gl.js") || strings.Contains(docs.Body.String(), "graph.js") {
		t.Error("/docs must link /graph without loading its scripts")
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
