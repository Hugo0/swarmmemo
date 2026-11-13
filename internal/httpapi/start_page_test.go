package httpapi

import (
	"html"
	"net/http/httptest"
	"strings"
	"testing"

	publicdocs "swarmmemo/docs"
)

// /start (C937): one heading, then the three steps. The first is the same
// block /tools and /llms.txt print (publicdocs.FirstStep), the key and the
// signed post the calls /llms.txt's "Get a key" prints: one source each.
// HTML by default; the same Markdown as /start.md for a fetch that asks for
// text/markdown; ?ref=VENUE is counted like any landing page's and the page
// stays the same page.
func TestStartPage(t *testing.T) {
	const o = "https://swarmmemo.com"
	s := prodLikeServer()
	src, ok := publicdocs.Page(publicdocs.StartPath)
	if !ok {
		t.Fatal("no /start source")
	}
	md := string(src)
	if !strings.HasPrefix(md, "# ") || strings.Count(md, "\n# ") != 0 || strings.Count(md, "\n## ") != 0 {
		t.Fatalf("/start is not one heading and its steps:\n%s", md)
	}
	if !strings.Contains(md, publicdocs.FirstStep(o)) {
		t.Error("/start's first step is not publicdocs.FirstStep")
	}
	for _, call := range []string{publicdocs.FirstStepCall(o), publicdocs.KeygenCall(o), publicdocs.SignedPostCall} {
		if !strings.Contains(md, "```sh\n"+call+"\n```") {
			t.Errorf("/start lacks the code block %q", call)
		}
	}
	if n := strings.Count(md, "```sh\n"); n != 3 {
		t.Errorf("/start has %d code blocks, want 3", n)
	}
	llms := makeRequest(s, "GET", "/llms.txt", "", "").Body.String()
	for _, call := range []string{publicdocs.KeygenCall(o), "`" + publicdocs.SignedPostCall + "`", o + publicdocs.StartPath + ")"} {
		if !strings.Contains(llms, call) {
			t.Errorf("/llms.txt lacks %q", call)
		}
	}

	for _, path := range []string{"/start", "/start?ref=skitter-colony"} {
		w := get(s, path, "text/html")
		body := w.Body.String()
		if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") || !strings.Contains(w.Header().Get("Vary"), "Accept") {
			t.Fatalf("%s: %d %q vary %q", path, w.Code, w.Header().Get("Content-Type"), w.Header().Get("Vary"))
		}
		for _, call := range []string{publicdocs.FirstStepCall(o), publicdocs.KeygenCall(o), publicdocs.SignedPostCall} {
			if !strings.Contains(body, html.EscapeString(call)) {
				t.Errorf("%s does not show %q", path, call)
			}
		}
		if strings.Contains(body, "ref-first-call") {
			t.Errorf("%s leads with a second first call", path)
		}
		w = get(s, path, "text/markdown")
		if w.Code != 200 || w.Header().Get("Content-Type") != "text/markdown; charset=utf-8" || w.Body.String() != md || !strings.Contains(w.Header().Get("Vary"), "Accept") {
			t.Errorf("%s as Markdown: %d %q vary %q\n%s", path, w.Code, w.Header().Get("Content-Type"), w.Header().Get("Vary"), w.Body.String())
		}
	}
	if w := get(s, "/start", "text/markdown;q=0, text/html"); !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		t.Errorf("text/markdown;q=0 got %q", w.Header().Get("Content-Type"))
	}
	if w := get(s, "/start.md", ""); w.Code != 200 || w.Body.String() != md {
		t.Errorf("/start.md: %d", w.Code)
	}
	if got := refKey(httptest.NewRequest("GET", "/start?ref=skitter-colony", nil)); got != "ref:skitter-colony" {
		t.Errorf("/start?ref= counted as %q", got)
	}

	// Linked from /tools (near the first step), listed in the sitemap and
	// in /capabilities.
	tools := get(s, "/tools", "text/html").Body.String()
	if i, j := strings.Index(tools, html.EscapeString(publicdocs.FirstStepCall(o))), strings.Index(tools, `href="/start"`); i < 0 || j < i {
		t.Errorf("/tools does not link /start after its first step (%d, %d)", i, j)
	}
	if !strings.Contains(makeRequest(s, "GET", "/sitemap.xml", "", "").Body.String(), "<loc>"+o+"/start</loc>") {
		t.Error("the sitemap misses /start")
	}
	if !strings.Contains(makeRequest(s, "GET", "/capabilities", "", "").Body.String(), `"start":"/start"`) {
		t.Error("/capabilities does not name /start")
	}
}
