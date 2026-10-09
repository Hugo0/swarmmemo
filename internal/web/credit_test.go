package web

import (
	"strings"
	"testing"
)

// The footer is four labelled columns that repeat nothing in the top nav; the
// tools are one link to their index, not a column of tool pages. The
// operator's backlink comes from FOOTER_CREDIT_URL and is absent from a
// deployment that sets none.
func TestFooterColumnsAndCredit(t *testing.T) {
	_, _, body := getPage(t, "/")
	_, footer, _ := strings.Cut(body, "<footer")
	for _, heading := range []string{">Agents</span>", ">Build</span>", ">Record</span>", ">About</span>"} {
		if !strings.Contains(footer, heading) {
			t.Errorf("footer lacks column %s", heading)
		}
	}
	if n := strings.Count(footer, `class="footer-col"`); n != 4 {
		t.Errorf("footer has %d columns, want 4", n)
	}
	for _, dropped := range []string{`href="/llms.txt"`, `href="/feed.atom"`, `href="/limits"`, `href="/tools/memory"`, `href="/agents"`, `href="/docs"`} {
		if strings.Contains(footer, dropped) {
			t.Errorf("footer still links %s", dropped)
		}
	}
	if strings.Contains(footer, "footer-credit") {
		t.Error("credit shown without FOOTER_CREDIT_URL")
	}
	if err := SetFooterCredit("http://example.org"); err == nil {
		t.Error("a non-https credit URL was accepted")
	}
	if err := SetFooterCredit("https://www.example.org/"); err != nil {
		t.Fatal(err)
	}
	defer SetFooterCredit("")
	_, _, body = getPage(t, "/")
	if !strings.Contains(body, `<a class="footer-credit" href="https://www.example.org/" rel="noopener">made with love by example.org</a>`) {
		t.Error("credit not rendered")
	}
}
