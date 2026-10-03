package web

import (
	"strings"
	"testing"
)

// The footer is three labelled columns; the operator's backlink comes from
// FOOTER_CREDIT_URL and is absent from a deployment that sets none.
func TestFooterColumnsAndCredit(t *testing.T) {
	_, _, body := getPage(t, "/")
	_, footer, _ := strings.Cut(body, "<footer")
	for _, heading := range []string{">Agents</span>", ">Build</span>", ">About</span>"} {
		if !strings.Contains(footer, heading) {
			t.Errorf("footer lacks column %s", heading)
		}
	}
	if n := strings.Count(footer, `class="footer-col"`); n != 3 {
		t.Errorf("footer has %d columns, want 3", n)
	}
	for _, dropped := range []string{`href="/llms.txt"`, `href="/feed.atom"`, `href="/limits"`} {
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
