package web

import (
	"strings"
	"testing"

	"swarmmemo/docs"
)

// The glossary's lexicon blocks parse: every positioning key once, no
// unknown key, three props with a lead, and a first-run list. A broken
// block also fails init, so a binary cannot ship with one.
func TestLexiconBlocksParse(t *testing.T) {
	glossary := string(docs.Glossary())
	p, err := parsePositioning(glossary)
	if err != nil {
		t.Fatal(err)
	}
	if p != Positioning() || Tagline != p.Category || Descriptor != p.Descriptor || ShortDescription != p.Summary {
		t.Fatal("the positioning names do not read the parsed block")
	}
	// Hugo's tagline (2026-10-15) stays exactly.
	if p.Category != "Talk with agent swarms." {
		t.Errorf("category %q: the tagline is Hugo's and stays exactly", p.Category)
	}
	if !strings.Contains(p.Descriptor, "for AI agents") || !strings.Contains(p.Summary, "for AI agents") {
		t.Errorf("descriptor %q and summary %q keep \"for AI agents\", the words people search with", p.Descriptor, p.Summary)
	}
	if len(p.Category) > 100 {
		t.Errorf("category is %d characters; the MCP Registry takes 100", len(p.Category))
	}
	// C989: importers cut a description at its first colon, so the
	// registry line has none, within the registry's 100 characters.
	if len(p.Registry) > 100 || strings.Contains(p.Registry, ":") {
		t.Errorf("registry %q: at most 100 characters and no colon", p.Registry)
	}
	if len(p.Summary) > 160 {
		t.Errorf("summary is %d characters; search results show 160", len(p.Summary))
	}
	for _, prop := range p.Props {
		if prop.Lead == "" || prop.Rest == "" || !strings.HasSuffix(prop.Rest, ".") {
			t.Errorf("prop %s %q: a lead, a colon and a sentence", prop.Key, prop.Text())
		}
	}
	words, err := parseFirstRun(glossary)
	if err != nil {
		t.Fatal(err)
	}
	if len(words) < 5 {
		t.Fatalf("only %d first-run words parsed; the parser is broken", len(words))
	}
	for _, w := range []string{"credits", "allowance", "escrow", "service.call", "Ed25519"} {
		found := false
		for _, fw := range words {
			found = found || fw.Word == w
		}
		if !found {
			t.Errorf("first-run block lost %q", w)
		}
	}

	// A CRLF checkout (Windows) parses the same.
	crlf := strings.ReplaceAll(glossary, "\n", "\r\n")
	if cp, err := parsePositioning(crlf); err != nil || cp != p {
		t.Errorf("a CRLF glossary: %v, %+v", err, cp)
	}
	if cw, err := parseFirstRun(crlf); err != nil || len(cw) != len(words) || cw[0] != words[0] {
		t.Errorf("a CRLF glossary's first-run block: %v", err)
	}

	// The parser refuses what it cannot read.
	for name, bad := range map[string]string{
		"duplicate": strings.Replace(glossary, "free => Free, no signup.", "free => Free, no signup.\nfree => Free.", 1),
		"unknown":   strings.Replace(glossary, "free => Free, no signup.", "free => Free, no signup.\nslogan => x", 1),
		"missing":   strings.Replace(glossary, "free => Free, no signup.\n", "", 1),
		"no arrow":  strings.Replace(glossary, "free => Free, no signup.", "free: Free, no signup.", 1),
		"no lead":   strings.Replace(glossary, "prop.work => Paid work: ", "prop.work => Paid work ", 1),
	} {
		if bad == glossary {
			t.Fatalf("%s: the fixture did not change the glossary", name)
		}
		if _, err := parsePositioning(bad); err == nil {
			t.Errorf("%s: a broken positioning block parsed", name)
		}
	}
	if _, err := LexiconBlock(glossary+"\n```first-run\nx => y\n```\n", "first-run"); err == nil {
		t.Error("two first-run blocks parsed")
	}
}

// A first-run word matches whole and in any case, never inside another word.
func TestFirstRunWordPattern(t *testing.T) {
	credit := FirstRunWord{Word: "credit"}.Pattern()
	call := FirstRunWord{Word: "service.call"}.Pattern()
	for text, want := range map[string]bool{"Credit is paid": true, "a credit.": true, "creditor": false, "credits": false, "discredit": false} {
		if got := credit.MatchString(text); got != want {
			t.Errorf("credit in %q: %v, want %v", text, got, want)
		}
	}
	for text, want := range map[string]bool{"one service.call each": true, "service call": false, "service.calls": false} {
		if got := call.MatchString(text); got != want {
			t.Errorf("service.call in %q: %v, want %v", text, got, want)
		}
	}
}
