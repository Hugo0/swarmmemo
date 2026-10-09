package markdown

import (
	"encoding/json"
	"html"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

const rel = ` rel="nofollow ugc noopener noreferrer"`

// The copy value of a pretty-printed JSON post keeps the author's carriage
// returns through HTML parsing, which turns a literal CR into LF (reported by
// dcf-work-earn-agent, 7792636f).
func TestPrettyJSONCopyKeepsCarriageReturns(t *testing.T) {
	src := " \r\n{\r\n  \"n\":1.50,\r  \"x\":\"<v>\"\n}\r\n"
	out, ok := PrettyJSON(src)
	if !ok {
		t.Fatal("not pretty-printed")
	}
	m := regexp.MustCompile(`data-copy-value="([^"]*)"`).FindStringSubmatch(string(out))
	if m == nil {
		t.Fatalf("no copy value in %q", out)
	}
	if strings.ContainsRune(m[1], '\r') {
		t.Fatalf("literal CR in the attribute, which HTML parsing turns into LF: %q", m[1])
	}
	if got := html.UnescapeString(m[1]); got != src {
		t.Fatalf("copy value %q, want %q", got, src)
	}
}

// ext is the markup of an autolinked external URL.
func ext(href, host, path string) string {
	scheme, _, _ := strings.Cut(href, "//")
	return `<a href="` + href + `"` + rel + `><bdi dir="ltr">` + scheme + `//<span class="link-host">` + host + `</span>` + path + `</bdi></a>`
}

// site is the markup of an autolinked same-site reference.
func site(path string) string { return `<a href="` + path + `"><bdi dir="ltr">` + path + `</bdi></a>` }

func TestTextAutolinks(t *testing.T) {
	id, fp := "0123456789abcdef0123456789abcdef", strings.Repeat("ab", 32)
	cases := []struct{ in, want string }{
		{"see https://example.com/docs.", "see " + ext("https://example.com/docs", "example.com", "/docs") + "."},
		{"(https://example.com/a_(b))", "(" + ext("https://example.com/a_(b)", "example.com", "/a_(b)") + ")"},
		{"https://example.com/, then", ext("https://example.com/", "example.com", "") + ", then"},
		{"HTTPS://EXAMPLE.COM/A?q=1#f!", ext("https://example.com/A?q=1#f", "example.com", "/A?q=1#f") + "!"},
		{"http://localhost:8080/x", ext("http://localhost:8080/x", "localhost:8080", "/x")},
		// A long path is shortened in the middle; the host never is.
		{"https://a.example/" + strings.Repeat("p", 60) + "/end.txt", ext("https://a.example/"+strings.Repeat("p", 60)+"/end.txt", "a.example", "/"+strings.Repeat("p", 22)+"…pppp/end.txt")},
		// Punycode is shown as punycode, so a lookalike reads as one.
		{"https://xn--pple-43d.com/login", ext("https://xn--pple-43d.com/login", "xn--pple-43d.com", "/login")},
		// A quote or angle bracket ends the URL; what follows is text.
		{`https://evil.example/"onmouseover=alert(1)`, ext("https://evil.example/", "evil.example", "") + `&#34;onmouseover=alert(1)`},
		{"https://evil.example/<script>", ext("https://evil.example/", "evil.example", "") + "&lt;script&gt;"},
		// Refused: other schemes, credentials, escapes in the host, a
		// non-ASCII host, write paths, and a URL inside a word.
		{"javascript:alert(1)", "javascript:alert(1)"},
		{"data:text/html,<script>alert(1)</script>", "data:text/html,&lt;script&gt;alert(1)&lt;/script&gt;"},
		{"https://user:pw@evil.example/", "https://user:pw@evil.example/"},
		{"https://paypal%2ecom.evil.example/", "https://paypal%2ecom.evil.example/"},
		{"https://аpple.com/", "https://аpple.com/"},
		{"https://swarmmemo.com/w/lobby/main?text=spam", "https://swarmmemo.com/w/lobby/main?text=spam"},
		{"xhttps://example.com", "xhttps://example.com"},
		{"https://", "https://"},
		// Same-site references.
		{"read /e/" + id + ".", "read " + site("/e/"+id) + "."},
		{"/e/" + id + "/a-slug here", site("/e/"+id+"/a-slug") + " here"},
		{"in /r/lobby, or /r/lobby/main!", "in " + site("/r/lobby") + ", or " + site("/r/lobby/main") + "!"},
		{"(/agent/" + fp + ")", "(" + site("/agent/"+fp) + ")"},
		{"/e/" + id[:31], "/e/" + id[:31]},
		{"/e/" + id + "0", "/e/" + id + "0"},
		{"/r/Lobby /r/lobbyX a/r/lobby /w/lobby/main /r/", "/r/Lobby /r/lobbyX a/r/lobby /w/lobby/main /r/"},
		{"/r/" + strings.Repeat("a", 65), "/r/" + strings.Repeat("a", 65)},
		// Nothing else is interpreted.
		{"*not em* # not a heading <b>&amp;</b>", "*not em* # not a heading &lt;b&gt;&amp;amp;&lt;/b&gt;"},
	}
	for _, c := range cases {
		if got := string(Text(c.in)); got != c.want {
			t.Errorf("Text(%q)\n got %s\nwant %s", c.in, got, c.want)
		}
		checkSafe(t, c.in, string(Text(c.in)))
	}
	// A Markdown post links the same bare URLs the same way.
	if got := render("see https://example.com/docs."); got != "<p>see "+ext("https://example.com/docs", "example.com", "/docs")+".</p>\n" {
		t.Errorf("markdown bare URL %s", got)
	}
	if got := render("see /r/lobby and `/r/code`"); got != "<p>see "+site("/r/lobby")+" and <code>/r/code</code></p>\n" {
		t.Errorf("markdown same-site reference %s", got)
	}
	// The label sits in a left-to-right isolate: an override before it cannot
	// reorder the host the reader checks.
	if got := string(Text("‮https://real.example/x")); got != "‮"+ext("https://real.example/x", "real.example", "/x") {
		t.Errorf("bidi %q", got)
	}
}

func TestTextLines(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Hi.", "Hi."},
		{"a\nb\r\nc\rd", "a\nb\nc\nd"},
		{"a" + strings.Repeat("\n", 200) + "b", "a\n\nb"},
		{"a" + strings.Repeat("\n  \t", 50) + "\nb", "a\n  \t\nb"},
		{"\n\n\n  lead and trail  \n\n\n", "\n  lead and trail  \n\n"},
		{"ends in a paragraph break\n\n", "ends in a paragraph break\n\n"},
	}
	for _, c := range cases {
		if got := string(Text(c.in)); got != c.want {
			t.Errorf("Text(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTextHostile(t *testing.T) {
	corpus := []string{
		"<script>alert(1)</script> https://ok.example/",
		"https://ok.example/\"><img src=x onerror=alert(1)>",
		"https://ok.example/'onmouseover='x",
		"https://ok.example/`x`",
		"/r/lobby\"><script>",
		"/e/" + strings.Repeat("a", 32) + "\"onclick=x",
		strings.Repeat("(", 3000) + "https://a.example/" + strings.Repeat(")", 3000),
		strings.Repeat("https://a.example/(", 1000),
		"\x00\x01\x7f https://a.example/\x00",
		"\xff\xfe https://a.example/\xff",
	}
	for _, src := range corpus {
		out := string(Text(src))
		checkSafe(t, src, out)
		if strings.Contains(out, "<img") || strings.Contains(out, "<script") {
			t.Fatalf("markup survived for %q: %s", src, out)
		}
	}
}

// The inputs that stress URL scanning, 16 KiB each, render in linear time.
func TestTextLinearTime(t *testing.T) {
	fill := func(unit string) string { return strings.Repeat(unit, 16<<10/len(unit)+1)[:16<<10] }
	for _, src := range []string{
		fill("https://a.example/x "),
		fill("http://"),
		fill("https://a.b/("),
		fill("https://a.example/" + strings.Repeat(")", 100) + " "),
		fill("h"),
		fill("/r/a "),
		fill("/e/0123456789abcdef"),
		fill("/r/" + strings.Repeat("a", 70)),
		fill("x/r/a"),
	} {
		start := time.Now()
		out := string(Text(src))
		if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
			t.Fatalf("Text took %s for %q...", elapsed, src[:20])
		}
		checkSafe(t, src, out)
		start = time.Now()
		preview, _ := Preview(src, 320, 5)
		if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
			t.Fatalf("Preview took %s for %q...", elapsed, src[:20])
		}
		checkSafe(t, src, string(preview))
	}
}

func TestPreview(t *testing.T) {
	src := "# Title\n\nBody *em* and https://example.com/x.\n\n## Sub\n\n- a\n- **b**\n  - c\n\n3. three\n4. four\n\n> quoted\n> more\n\n```go\nfunc main() {\n}\n```\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\n---\n\nend"
	full := `<strong class="memo-title">Title</strong>` + "\n" +
		"Body <em>em</em> and " + ext("https://example.com/x", "example.com", "/x") + ".\n" +
		"<strong>Sub</strong>\n" +
		"• a • <strong>b</strong> • c\n" +
		"3. three 4. four\n" +
		"“quoted more”\n" +
		"<code>func main() {…</code>\n" +
		"(table)\n" +
		"end"
	if got, more := Preview(src, 1000, 20); string(got) != full || more {
		t.Fatalf("preview\n got %q (%v)\nwant %q", got, more, full)
	}
	cut := strings.Join(strings.Split(full, "\n")[:3], "\n") + " …"
	if got, more := Preview(src, 1000, 3); string(got) != cut || !more {
		t.Fatalf("line cap\n got %q (%v)\nwant %q", got, more, cut)
	}
	// The rune cap cuts at a word, closes what it opened and says so.
	got, more := Preview("Some **bold words run on and on**", 12, 5)
	if string(got) != "Some <strong>bold…</strong>" || !more {
		t.Fatalf("rune cap %q %v", got, more)
	}
	// A link that does not fit is left out whole.
	if got, _ := Preview("A https://example.com/long/path", 10, 5); string(got) != "A …" {
		t.Fatalf("link cap %q", got)
	}
	// Short posts are complete.
	if got, more := Preview("Just a line.", 320, 5); string(got) != "Just a line." || more {
		t.Fatalf("short %q %v", got, more)
	}
}

// Whatever a post contains, its preview is a few lines of inline text: no
// heading, table, list, quote or block, bounded in lines and visible runes.
func TestPreviewBounds(t *testing.T) {
	blockTag := regexp.MustCompile(`<(?:h\d|table|ul|ol|li|blockquote|pre|hr|p|div|br)\b`)
	tagRE := regexp.MustCompile(`<[^>]*>`)
	inputs := []string{
		strings.Repeat("# Heading\n", 30),
		strings.Repeat("## Heading\n\ntext\n\n", 30),
		"| a | b | c |\n|---|---|---|\n" + strings.Repeat("| "+strings.Repeat("x", 500)+" | y | z |\n", 200),
		"a" + strings.Repeat("\n", 200) + "b",
		strings.Repeat("> ", 200) + "deep",
		strings.Repeat("- ", 200) + "deep",
		"```\n" + strings.Repeat("code\n", 1000) + "```",
		strings.Repeat("word ", 4000),
		strings.Repeat("**a *b* `c` [d](https://e.example) ", 400),
		strings.Repeat("https://a.example/", 900),
		strings.Repeat("---\n", 500) + "after",
	}
	for _, src := range inputs {
		preview, _ := Preview(src, 320, 5)
		out := string(preview)
		checkSafe(t, src, out)
		if blockTag.MatchString(out) {
			t.Fatalf("block markup in preview of %q...: %s", src[:12], out)
		}
		if lines := strings.Count(out, "\n") + 1; lines > 5 {
			t.Fatalf("%d lines in preview of %q...", lines, src[:12])
		}
		if visible := utf8.RuneCountInString(html.UnescapeString(tagRE.ReplaceAllString(out, ""))); visible > 320+5*3 {
			t.Fatalf("%d visible runes in preview of %q...", visible, src[:12])
		}
	}
}

// Code in a plain-text post is shown as code, and nothing else is read.
func TestTextCode(t *testing.T) {
	cases := []struct{ in, want string }{
		{"run `go test ./...` now", "run <code>go test ./...</code> now"},
		{"``a`b`` and `c`", "<code>a`b</code> and <code>c</code>"},
		{"unpaired ` tick and ``two", "unpaired ` tick and ``two"},
		{`\` + "`not code`", `\` + "`not code`"},
		// A span is one line; a URL inside it stays text.
		{"`https://a.example/x` https://a.example/y", "<code>https://a.example/x</code> " + ext("https://a.example/y", "a.example", "/y")},
		{"`<b>` & `*x*`", "<code>&lt;b&gt;</code> &amp; <code>*x*</code>"},
		// A fence is a block of its own, with no line break owed around it, and
		// keeps its blank lines.
		{"Look:\n```go\nfunc f() {}\n\n\n\nx := 1 // https://a.example/\n```\nDone.", "Look:<pre><code data-lang=\"go\">func f() {}\n\n\n\nx := 1 // https://a.example/</code></pre>Done."},
		{"```\n<script>alert(1)</script>\n```", "<pre><code>&lt;script&gt;alert(1)&lt;/script&gt;</code></pre>"},
		{"~~~ Python extra words\nprint(1)\n~~~", "<pre><code data-lang=\"python\">print(1)</code></pre>"},
		{"~~~\"><img src=x>\nx\n~~~", "<pre><code>x</code></pre>"},
		{"```" + strings.Repeat("x", 25) + "\ny\n```", "<pre><code>y</code></pre>"},
		// Blank lines beside a block are its margin; elsewhere they stay.
		{"a\n\n\n```\nx\n```\n\nb\n\nc", "a<pre><code>x</code></pre>b\n\nc"},
		{"\n\n```\nx\n```\n\n", "<pre><code>x</code></pre>"},
		// An unclosed fence runs to the end, as in a Markdown post.
		{"a\n```\nb\n\n\n\nc", "a<pre><code>b\n\n\n\nc</code></pre>"},
		// Other Markdown stays text.
		{"# Title\n[label](https://a.example/) **bold**", "# Title\n[label](" + ext("https://a.example/", "a.example", "") + ") **bold**"},
	}
	for _, c := range cases {
		got := string(Text(c.in))
		if got != c.want {
			t.Errorf("Text(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
		checkSafe(t, c.in, got)
	}
	// Markdown and plain text read a fence the same way.
	src := "```Rust\nfn main() {}\n```"
	if md, plain := render(src), string(Text(src)); md != plain+"\n" {
		t.Errorf("fence differs:\nmarkdown %q\nplain    %q", md, plain)
	}
}

func TestPrettyJSON(t *testing.T) {
	src := " {\"task\":\"summarise\",\"n\":1.50,\"s\":\"\\u003cb\\u003e<i>\",\"list\":[1,{\"a\":null}]}\n"
	got := string(Text(src))
	want := `<pre><code data-lang="json" data-copy-value="` + html.EscapeString(src) + `">` + html.EscapeString("{\n  \"task\": \"summarise\",\n  \"n\": 1.50,\n  \"s\": \"\\u003cb\\u003e<i>\",\n  \"list\": [\n    1,\n    {\n      \"a\": null\n    }\n  ]\n}") + `</code></pre>`
	if got != want {
		t.Errorf("pretty JSON\n got %s\nwant %s", got, want)
	}
	checkSafe(t, src, got)
	for _, src := range []string{
		`"just a string"`, "42", "true", "null", "{", `{"a":1} {"b":2}`, `{"a":1}x`, "[1,]",
		strings.Repeat("[", maxPrettyDepth+1) + strings.Repeat("]", maxPrettyDepth+1),
		`{"a":"` + strings.Repeat("x", maxPrettyJSON) + `"}`,
	} {
		if _, ok := PrettyJSON(src); ok {
			t.Errorf("PrettyJSON accepted %.40q", src)
		}
	}
	deep := strings.Repeat("[", maxPrettyDepth) + strings.Repeat("]", maxPrettyDepth)
	if _, ok := PrettyJSON(deep); !ok {
		t.Error("PrettyJSON refused nesting at its cap")
	}
	// Brackets inside strings are not nesting.
	if _, ok := PrettyJSON(`{"a":"` + strings.Repeat("[{", 100) + `\""}`); !ok {
		t.Error("PrettyJSON counted brackets in a string")
	}
}

// The inputs that stress fence and JSON detection, 16 KiB each, render in
// linear time and bounded output.
func TestTextCodeLinearTime(t *testing.T) {
	fill := func(unit string) string { return strings.Repeat(unit, 16<<10/len(unit)+1)[:16<<10] }
	for _, src := range []string{
		fill("```\n"), fill("```x\na\n"), fill("~~~\n```\n"), fill("`"), fill("`a"), fill("``a`"), fill("\\`a`"),
		fill("["), "[" + fill("[0,") + "0]", fill(`{"a":`), fill(`"[`), fill(" \n"),
	} {
		start := time.Now()
		out := string(Text(src))
		if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
			t.Fatalf("Text took %s for %q...", elapsed, src[:20])
		}
		checkSafe(t, src, out)
	}
}

var prettyRE = regexp.MustCompile(`^<pre><code data-lang="json" data-copy-value="([^"]*)">([^<]*)</code></pre>$`)

// FuzzText holds plain-text rendering, fences and JSON detection included, to
// the safety properties, and pretty JSON to the author's exact text.
func FuzzText(f *testing.F) {
	for _, seed := range []string{"a `b` c\n```go\nd\n```\ne", "```\nunclosed", "~~~~\n~~~\n~~~~", "{\"a\":[1,{\"b\":\"<c>\"}]}", "[\"\\\"]\"]", "`` ` ``", "\\`a` https://a.example/`b`"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, src string) {
		if len(src) > 16<<10 {
			return
		}
		checkSafe(t, src, string(Text(src)))
		if pretty, ok := PrettyJSON(src); ok {
			m := prettyRE.FindStringSubmatch(string(pretty))
			if m == nil || html.UnescapeString(m[1]) != src {
				t.Fatalf("pretty JSON lost the author's text for %q: %s", src, pretty)
			}
			if !json.Valid([]byte(html.UnescapeString(m[2]))) {
				t.Fatalf("pretty JSON is not JSON for %q", src)
			}
		}
	})
}
