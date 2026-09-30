package web

import (
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	publicdocs "swarmmemo/docs"
)

// Internal drafting marks must never reach a published legal page, in its
// source or in any rendering of it.
var legalDraftMarks = regexp.MustCompile(`(?i)\[(?:todo|assumed)\b|\bdraft\b|needs hugo|open questions for hugo|assumptions to confirm`)

func TestLegalPagesRenderFromTheirMarkdownSource(t *testing.T) {
	for _, tc := range []struct{ path, title, other string }{
		{"/privacy", "Privacy Policy", "/terms"},
		{"/terms", "Terms of Use", "/privacy"},
	} {
		src, ok := publicdocs.Legal(tc.path)
		if !ok || !strings.HasPrefix(string(src), "# "+tc.title+"\n") {
			t.Fatalf("%s: no Markdown source titled %q", tc.path, tc.title)
		}
		if m := legalDraftMarks.FindString(string(src)); m != "" {
			t.Errorf("%s source carries a drafting mark: %q", tc.path, m)
		}
		if !strings.Contains(string(src), "Last updated: 2026-09-29") {
			t.Errorf("%s source has no last-updated date", tc.path)
		}
		w := httptest.NewRecorder()
		Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", tc.path, nil))
		body := w.Body.String()
		if w.Code != 200 || !strings.Contains(body, "</html>") || w.Header().Get("X-Robots-Tag") != "" {
			t.Fatalf("%s: %d, indexable %v", tc.path, w.Code, w.Header().Get("X-Robots-Tag") == "")
		}
		if strings.Count(body, "<h1") != 1 || !strings.Contains(body, "<h1>"+tc.title+"</h1>") || !strings.Contains(body, "<title>"+tc.title) {
			t.Errorf("%s: the title must be the page's one h1 and its <title>", tc.path)
		}
		if !strings.Contains(body, `<h2 id="md-contact">`) && !strings.Contains(body, `<h2 id="md-11-contact">`) {
			t.Errorf("%s: sections must be anchored h2s under the page h1", tc.path)
		}
		for _, link := range []string{`href="` + tc.path + `.md"`, `href="` + tc.other + `"`, `href="/policy"`, `href="https://github.com/Hugo0/swarmmemo/issues"`} {
			if !strings.Contains(body, link) {
				t.Errorf("%s: missing link %s", tc.path, link)
			}
		}
		start := strings.Index(body, `<article class="md article-body`)
		end := strings.Index(body, "</article>")
		if start < 0 || end < start {
			t.Fatalf("%s: no rendered article", tc.path)
		}
		if m := legalDraftMarks.FindString(body[start:end]); m != "" {
			t.Errorf("%s page carries a drafting mark: %q", tc.path, m)
		}
		// release/validate_snapshot.py refuses root-relative Markdown links in
		// the public source, so the source names the canonical site, and the
		// rendered page turns those into same-site links.
		if strings.Contains(string(src), "](/") {
			t.Errorf("%s source has a root-relative link the public snapshot refuses", tc.path)
		}
		if article := body[start:end]; strings.Contains(article, `href="https://swarmmemo.com`) || !strings.Contains(article, `href="`+tc.other+`"`) || !strings.Contains(article, `href="/policy"`) {
			t.Errorf("%s: site links must render same-site, including %s and /policy", tc.path, tc.other)
		}
		for _, invented := range []string{"@swarmmemo.com", "mailto:"} {
			if invented == "@swarmmemo.com" {
				// The only address on these pages is the room-posting form ROOM@.
				body = strings.ReplaceAll(body, "ROOM@swarmmemo.com", "")
			}
			if strings.Contains(body, invented) {
				t.Errorf("%s names a contact address (%s) that does not exist", tc.path, invented)
			}
		}
	}
	if legalPage("/policy") != nil || legalPage("/privacy.md") != nil || legalPage("/legal/privacy.md") != nil {
		t.Fatal("only /privacy and /terms are legal pages")
	}
}

func TestLegalPagesAreLinkedFromEveryPageAndThePolicy(t *testing.T) {
	for _, path := range []string{"/", "/for-agents", "/policy", "/privacy", "/terms"} {
		w := httptest.NewRecorder()
		Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		footer := w.Body.String()
		if i := strings.Index(footer, `<footer`); i >= 0 {
			footer = footer[i:]
		}
		if w.Code != 200 || !strings.Contains(footer, `<a href="/privacy"`) || !strings.Contains(footer, `<a href="/terms"`) || !strings.Contains(footer, `<a href="/policy">`) {
			t.Errorf("%s: footer must link /policy, /privacy and /terms", path)
		}
	}
	w := httptest.NewRecorder()
	Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", "/policy", nil))
	body := w.Body.String()
	main := body[:strings.Index(body, "<footer")]
	if w.Code != 200 || !strings.Contains(main, `<a href="/privacy">Privacy Policy</a>`) || !strings.Contains(main, `<a href="/terms">Terms of Use</a>`) {
		t.Fatal("/policy must stay and link both full pages from its body")
	}
	w = httptest.NewRecorder()
	Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", "/for-agents", nil))
	if body := w.Body.String(); !strings.Contains(body, `href="/terms"`) || !strings.Contains(body, `href="/privacy#md-ai-assistants-posting-for-a-person"`) {
		t.Fatal("/for-agents must point agents at the terms and the assistants' rules")
	}
	if !strings.Contains(string(legalPage("/privacy").Body), `id="md-ai-assistants-posting-for-a-person"`) {
		t.Fatal("the assistants' section anchor /for-agents links to is gone")
	}
}
