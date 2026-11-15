package web

import (
	"fmt"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	publicdocs "swarmmemo/docs"
	"swarmmemo/internal/board"
)

// C452: the privacy policy states the write records' and write signals'
// retention as the code keeps them, and says the full IP address is stored.
func TestPrivacyStatesWriteRecordRetention(t *testing.T) {
	src, ok := publicdocs.Legal("/privacy")
	if !ok {
		t.Fatal("no privacy source")
	}
	text := string(src)
	for _, want := range []string{
		fmt.Sprintf("encrypted, for %d\n  days", board.RawSignalRetentionDays),
		fmt.Sprintf("**How long:** %d days, then deleted; a record is kept longer only while it is needed as", board.RawSignalRetentionDays),
		fmt.Sprintf("Write records (full IP address and request details):** %d days", board.RawSignalRetentionDays),
		fmt.Sprintf("| %d days; longer only while needed as evidence", board.RawSignalRetentionDays),
		fmt.Sprintf("**Write signals:** %d days, then deleted.", board.WriteSignalsRetentionDays),
		"we store the full IP address",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("privacy policy lacks %q", want)
		}
	}
	for _, stale := range []string{"We don't store IP addresses", "No raw IP addresses are stored", "no full IP address is stored"} {
		if strings.Contains(text, stale) {
			t.Errorf("privacy policy still says %q", stale)
		}
	}
}

// Internal drafting marks must never reach a published legal page, in its
// source or in any rendering of it.
var legalDraftMarks = regexp.MustCompile(`(?i)\[(?:todo|assumed)\b|\bdraft\b|needs hugo|open questions for hugo|assumptions to confirm`)

// legalUpdated is the date line every legal page carries under its title.
var legalUpdated = regexp.MustCompile(`(?m)^Last updated: \d{4}-\d{2}-\d{2}\.`)

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
		if !legalUpdated.MatchString(string(src)) {
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
		for _, link := range []string{`href="` + tc.path + `.md"`, `href="` + tc.other + `"`, `href="/terms#board-rules"`, `href="https://github.com/Hugo0/swarmmemo/issues"`} {
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
		if article := body[start:end]; strings.Contains(article, `href="https://swarmmemo.com`) || !strings.Contains(article, `href="`+tc.other+`"`) || !strings.Contains(article, `href="/terms#board-rules"`) {
			t.Errorf("%s: site links must render same-site, including %s and /terms#board-rules", tc.path, tc.other)
		}
		for _, invented := range []string{"@swarmmemo.com", "mailto:"} {
			if invented == "@swarmmemo.com" {
				// The only addresses on these pages are the room-posting form
				// ROOM@ and the operator's private inbox (OperatorInbox).
				body = strings.ReplaceAll(body, "ROOM@swarmmemo.com", "")
				body = strings.ReplaceAll(body, OperatorInbox, "")
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
	for _, path := range []string{"/", "/for-agents", "/privacy", "/terms"} {
		w := httptest.NewRecorder()
		Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		footer := w.Body.String()
		if i := strings.Index(footer, `<footer`); i >= 0 {
			footer = footer[i:]
		}
		if w.Code != 200 || !strings.Contains(footer, `<a href="/privacy"`) || !strings.Contains(footer, `<a href="/terms"`) {
			t.Errorf("%s: footer must link /privacy and /terms", path)
		}
	}
	// C349: the board rules (once /policy) are /terms#board-rules, linking
	// both full pages; /policy moved there, and the retention part is
	// /privacy#retention.
	w := httptest.NewRecorder()
	Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", "/policy", nil))
	if w.Code != 301 || w.Header().Get("Location") != "/terms#board-rules" {
		t.Fatalf("/policy: %d %q", w.Code, w.Header().Get("Location"))
	}
	w = httptest.NewRecorder()
	Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", "/terms", nil))
	body := w.Body.String()
	main := body[:strings.Index(body, "<footer")]
	if w.Code != 200 || !strings.Contains(main, `id="board-rules"`) || !strings.Contains(main, `<a href="/privacy">Privacy Policy</a>`) || !strings.Contains(main, `<a href="/terms">Terms of Use</a>`) || !strings.Contains(main, "swarmmemo-public-2026-09-05") {
		t.Fatal("/terms must carry the board rules, linking both full pages")
	}
	w = httptest.NewRecorder()
	Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", "/privacy", nil))
	if !strings.Contains(w.Body.String(), `id="retention"`) {
		t.Fatal("/privacy lacks #retention")
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

// Public rooms are for all ages (the attestation app directories rely on):
// every page that states the rules or the moderation standard rules out
// sexual or pornographic content, and the standard hides it.
func TestPublicRoomsRuleOutSexualContent(t *testing.T) {
	const standard = "extreme vulgarity, sexual or pornographic content, sexual content involving minors, and doxxing"
	flat := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	terms, _ := publicdocs.Legal("/terms")
	privacy, _ := publicdocs.Legal("/privacy")
	if s := flat(string(terms)); !strings.Contains(s, "hide only phishing, malware, slur harassment or "+standard) || !strings.Contains(s, "- Post sexual or pornographic content in public rooms.") {
		t.Error("/terms must list sexual or pornographic content as not allowed and in the standard")
	}
	if s := flat(string(privacy)); !strings.Contains(s, "slur harassment or "+standard) || !strings.Contains(s, "Public rooms carry no sexual or pornographic content") {
		t.Error("/privacy must state the standard with sexual or pornographic content")
	}
	w := httptest.NewRecorder()
	Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", "/terms", nil))
	if !strings.Contains(flat(w.Body.String()), "post sexual or pornographic content in public rooms") {
		t.Error("the board rules (/terms#board-rules) must rule out sexual or pornographic content in public rooms")
	}
}
