package httpapi

// One word per concept: docs/GLOSSARY.md names the canonical word for each
// concept and ends with a ```banned block of phrases copy must not use. This
// test holds every served page and document, /llms.txt, the MCP server card
// (the tool descriptions) and the strings in the browser scripts to it, so a
// retired synonym cannot drift back. Wire names keep their spelling; a phrase
// is matched only as a whole phrase, never inside an identifier, class name or
// path (HTML tags, scripts and styles are stripped first).

import (
	"encoding/json"
	"html"
	"os"
	"regexp"
	"strings"
	"testing"

	publicdocs "swarmmemo/docs"
	"swarmmemo/internal/web"
)

type bannedPhrase struct {
	phrase, instead string
	re              *regexp.Regexp
}

// glossaryBanned parses the ```banned block: one "phrase => instead" a line.
func glossaryBanned(t *testing.T) []bannedPhrase {
	t.Helper()
	text := string(publicdocs.Glossary())
	_, block, ok := strings.Cut(text, "```banned\n")
	if !ok {
		t.Fatal("docs/GLOSSARY.md has no ```banned block")
	}
	block, _, ok = strings.Cut(block, "```")
	if !ok {
		t.Fatal("docs/GLOSSARY.md: the ```banned block is not closed")
	}
	var out []bannedPhrase
	for _, line := range strings.Split(block, "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		phrase, instead, ok := strings.Cut(line, "=>")
		phrase, instead = strings.TrimSpace(phrase), strings.TrimSpace(instead)
		if !ok || phrase == "" || instead == "" {
			t.Fatalf("docs/GLOSSARY.md banned line %q is not \"phrase => instead\"", line)
		}
		words := strings.Fields(phrase)
		for i, w := range words {
			words[i] = regexp.QuoteMeta(w)
		}
		// Not part of a longer word, identifier, path, anchor or class name.
		re := regexp.MustCompile(`(?i)(?:^|[^\w\-./#@~])` + strings.Join(words, `\s+`) + `(?:$|[^\w\-])`)
		out = append(out, bannedPhrase{phrase, instead, re})
	}
	if len(out) < 5 {
		t.Fatalf("docs/GLOSSARY.md bans only %d phrases; the parser is broken", len(out))
	}
	return out
}

// glossaryDontSay parses the entries' "Don't say:" lists into banned
// phrases, the entry's word being what to say instead. A one-word synonym
// ("user", "event", "order") is also an everyday word, and a synonym with a
// qualifier ("bot (for a persona)") is banned only in that sense, so only
// the unqualified phrases of two or more words are enforced: those can only
// mean the concept.
func glossaryDontSay(t *testing.T) []bannedPhrase {
	t.Helper()
	entry := regexp.MustCompile(`(?m)^- \*\*([^*]+)\*\*:.*Don't say: ([^.]*)\.`)
	var out []bannedPhrase
	for _, m := range entry.FindAllStringSubmatch(string(publicdocs.Glossary()), -1) {
		for _, phrase := range strings.Split(m[2], ",") {
			phrase = strings.TrimSpace(phrase)
			if strings.ContainsAny(phrase, "()`") || !strings.Contains(phrase, " ") {
				continue
			}
			words := strings.Fields(phrase)
			for i, w := range words {
				words[i] = regexp.QuoteMeta(w)
			}
			re := regexp.MustCompile(`(?i)(?:^|[^\w\-./#@~])` + strings.Join(words, `\s+`) + `(?:$|[^\w\-])`)
			out = append(out, bannedPhrase{phrase, m[1], re})
		}
	}
	if len(out) < 5 {
		t.Fatalf("docs/GLOSSARY.md: only %d \"Don't say\" phrases parsed; the parser is broken", len(out))
	}
	return out
}

var (
	htmlDropped = regexp.MustCompile(`(?is)<script\b.*?</script>|<style\b.*?</style>|<!--.*?-->`)
	htmlTag     = regexp.MustCompile(`(?s)<[^>]*>`)
	jsString    = regexp.MustCompile("'(?:[^'\\\\\\n]|\\\\.)*'|\"(?:[^\"\\\\\\n]|\\\\.)*\"|`(?:[^`\\\\]|\\\\.)*`")
)

// visibleText is what a reader sees of a served body: an HTML page's text
// without tags, scripts and styles; any other body as served.
func visibleText(body string) string {
	if !strings.Contains(body, "<html") && !strings.Contains(body, "<!doctype") && !strings.Contains(body, "<!DOCTYPE") {
		return body
	}
	return html.UnescapeString(htmlTag.ReplaceAllString(htmlDropped.ReplaceAllString(body, " "), " "))
}

func TestGlossaryBannedPhrases(t *testing.T) {
	banned := append(glossaryBanned(t), glossaryDontSay(t)...)
	sources := map[string]string{}
	for source, body := range publicCopy(t) {
		switch source {
		case "/glossary", "/glossary.md", "/migration":
			continue // the glossary lists the banned words; /migration maps retired names on purpose
		}
		text := visibleText(body)
		if source == web.BoardMapPath {
			text = withoutBoardEntries(t, text)
		}
		sources[source] = text
	}
	// The browser scripts' strings: what a live update, a badge, a dialog or a
	// status line says. A string without a space is a class name, id or key,
	// never copy; a one-word phrase is looked for only in sentences (a capital
	// letter, no code), since a class list such as 'memo conversation-message'
	// is not copy either.
	sentences := map[string]bool{}
	for _, file := range []string{"app.js", "messages.js", "memo-core.js", "feeds.js", "seal.js", "tools.js"} {
		raw, err := os.ReadFile("../web/assets/" + file)
		if err != nil {
			t.Fatal(err)
		}
		var spaced, prose []string
		for _, s := range jsString.FindAllString(string(raw), -1) {
			if !strings.ContainsAny(s, " \t") || strings.ContainsAny(s, "\n;{}") || strings.Contains(s, "=>") {
				continue
			}
			spaced = append(spaced, s)
			if strings.ContainsAny(s, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") {
				prose = append(prose, s)
			}
		}
		sources["/assets/"+file] = strings.Join(spaced, "\n")
		sources["/assets/"+file+" (sentences)"] = strings.Join(prose, "\n")
		sentences["/assets/"+file] = true
	}
	if len(sources) < 40 {
		t.Fatalf("checked only %d sources", len(sources))
	}
	pending := map[string]bool{}
	for k := range glossaryPending {
		pending[k] = true
	}
	for source, text := range sources {
		for _, b := range banned {
			oneWord := !strings.Contains(b.phrase, " ")
			if sentences[source] && oneWord || strings.HasSuffix(source, " (sentences)") && !oneWord {
				continue
			}
			for _, m := range b.re.FindAllString(text, -1) {
				key := source + ": " + strings.ToLower(strings.Trim(m, " \t\n'\"`.,;:()[]"))
				if glossaryPending[key] {
					delete(pending, key)
					continue
				}
				t.Errorf("%s says %q: the glossary word is %s (docs/GLOSSARY.md)", source, strings.TrimSpace(m), b.instead)
			}
		}
	}
	for key := range pending {
		t.Errorf("glossaryPending lists %q, which no longer appears: remove it", key)
	}
}

// withoutBoardEntries drops the listed boards' own descriptions from the
// board map guide's text: they are generated from the awesome-agent-boards
// list and describe other boards in those boards' words ("each agent keeps
// one stable Agent ID"), updated there, as TestStalePhrasesStayGone also
// allows. The guide's own copy around them is still checked. A description
// the page renders differently stays in, so nothing is skipped silently.
func withoutBoardEntries(t *testing.T, text string) string {
	t.Helper()
	raw, err := os.ReadFile("../web/boardlist/boards.json")
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Boards []map[string]any `json:"boards"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	for _, b := range list.Boards {
		for _, field := range []string{"about", "access", "identity", "note", "reason"} {
			if v, _ := b[field].(string); v != "" {
				text = strings.ReplaceAll(text, v, " ")
			}
		}
	}
	return text
}

// glossaryPending is drift found when the glossary landed, in files another
// change was rewriting at the time. Each entry must still occur (or the test
// asks for its removal), so this list can only shrink.
var glossaryPending = map[string]bool{}

// The glossary is served and linked from where agents and people start.
func TestGlossaryIsLinked(t *testing.T) {
	s := realServer(t)
	if w := get(s, "/glossary", "text/html"); w.Code != 200 || !strings.Contains(w.Body.String(), "One word per concept") {
		t.Fatalf("/glossary answered %d", w.Code)
	}
	for _, page := range []string{"/llms.txt", "/docs"} {
		if body := get(s, page, "text/html").Body.String(); !strings.Contains(body, "/glossary") {
			t.Errorf("%s does not link the glossary", page)
		}
	}
}
