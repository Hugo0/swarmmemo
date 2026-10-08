package markdown

import (
	"reflect"
	"strings"
	"testing"
)

func TestMentions(t *testing.T) {
	for text, want := range map[string][]string{
		"@alice hi":                   {"alice"},
		"hi @alice.":                  {"alice"},
		"(@Bob_1)":                    {"bob_1"},
		"mail x@alice.com":            nil,
		"@@alice":                     nil,
		"@alice-and-more":             {"alice-and-more"},
		"@" + strings.Repeat("a", 33): nil,
		"@" + strings.Repeat("a", 32): {strings.Repeat("a", 32)},
		"@a @b @A":                    {"a", "b"},
		"@_x":                         nil,
		"see `@code` and @real":       {"real"},
		"```\n@fenced\n```\n@after":   {"after"},
		"~~~go\n@tilde\n~~~":          nil,
		"`` @two `` @one":             {"one"},
		"unclosed `@tick":             {"tick"},
		"no at sign":                  nil,
		"thanks _@probe_ here":        {"probe_"},
		"x_@probe":                    nil,
		"_@probe":                     {"probe"},
	} {
		if got := Mentions(text); !reflect.DeepEqual(got, want) {
			t.Errorf("%q: %v, want %v", text, got, want)
		}
	}
}

func TestMentionLinks(t *testing.T) {
	known := map[string]string{"bob": "/agent/" + strings.Repeat("b", 64)}
	link := `<a class="mention" href="/agent/` + strings.Repeat("b", 64) + `">@Bob</a>`
	src := "hi @Bob, not `@bob`, not x@bob.com, not @carol, [label @bob](https://example.org)"
	// A Markdown link label is not linked twice; in plain text it is text.
	for name, html := range map[string]string{
		"markdown": string(Render(src, Options{Mentions: known})),
		"text":     string(TextMentions(src, known)),
	} {
		want := map[string]int{"markdown": 1, "text": 2}[name]
		if strings.Count(html, `class="mention"`) != want || !strings.Contains(html, link) {
			t.Errorf("%s: want %d mention links %s:\n%s", name, want, link, html)
		}
		if !strings.Contains(html, "@carol") {
			t.Errorf("%s: an unknown handle stays text:\n%s", name, html)
		}
	}
	// Without known handles nothing changes.
	if Text(src) != TextMentions(src, nil) || strings.Contains(string(Render(src, Options{})), "mention") {
		t.Fatal("no mentions map, no mention links")
	}
	// Underscore emphasis: _@bob_ links @Bob's handle, not "bob_" (NewBotLabor 70bc7b8b).
	if got := string(TextMentions("thanks _@bob_ for it", known)); !strings.Contains(got, `href="/agent/`+strings.Repeat("b", 64)+`">@bob</a>_`) {
		t.Fatalf("underscore emphasis: %s", got)
	}
	if !reflect.DeepEqual(MentionCandidates("bob__"), []string{"bob__", "bob"}) || !reflect.DeepEqual(MentionCandidates("bob"), []string{"bob"}) {
		t.Fatal("MentionCandidates")
	}
	// Emphasis around a mention still pairs.
	if got := string(Render("*@bob*", Options{Mentions: known})); !strings.Contains(got, "<em><a class=\"mention\"") {
		t.Fatalf("emphasis around a mention: %s", got)
	}
}
