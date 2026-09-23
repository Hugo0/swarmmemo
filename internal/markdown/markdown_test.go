package markdown

import (
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func render(src string) string { return string(Render(src, Options{})) }

func TestSubset(t *testing.T) {
	cases := []struct{ in, want string }{
		{"# Title\n\nBody *em* and **strong**.", "<h2>Title</h2>\n<p>Body <em>em</em> and <strong>strong</strong>.</p>\n"},
		{"## Sub\n### Deep\n#### Deeper", "<h3>Sub</h3>\n<h4>Deep</h4>\n<h4>Deeper</h4>\n"},
		{"a `x < y` b", "<p>a <code>x &lt; y</code> b</p>\n"},
		{"```go\nif a < b {\n\treturn\n}\n```", "<pre><code>if a &lt; b {\n\treturn\n}</code></pre>\n"},
		{"- one\n- two\n  - nested\n- three", "<ul>\n<li>one</li>\n<li>two<ul>\n<li>nested</li>\n</ul>\n</li>\n<li>three</li>\n</ul>\n"},
		{"3. c\n4. d", "<ol start=\"3\">\n<li>c</li>\n<li>d</li>\n</ol>\n"},
		{"> quoted\n> more", "<blockquote>\n<p>quoted\nmore</p>\n</blockquote>\n"},
		{"a\n\n---\n\nb", "<p>a</p>\n<hr>\n<p>b</p>\n"},
		{"| a | b |\n|:--|--:|\n| 1 | 2 |", "<div class=\"md-table\"><table>\n<thead><tr><th class=\"md-left\">a</th><th class=\"md-right\">b</th></tr></thead>\n<tbody>\n<tr><td class=\"md-left\">1</td><td class=\"md-right\">2</td></tr>\n</tbody>\n</table></div>\n"},
		{"[docs](https://example.com/docs)", "<p><a href=\"https://example.com/docs\" rel=\"nofollow noopener ugc\"><bdi>docs</bdi></a><bdi class=\"md-host\" dir=\"ltr\">example.com</bdi></p>\n"},
		{"see https://example.com/a_(b). ok", "<p>see <a href=\"https://example.com/a_(b)\" rel=\"nofollow noopener ugc\"><bdi>https://example.com/a_(b)</bdi></a>. ok</p>\n"},
		{"[protocol](/docs)", "<p><a href=\"/docs\"><bdi>protocol</bdi></a></p>\n"},
		{"line one  \nline two", "<p>line one<br>\nline two</p>\n"},
		{"\\*not em\\*", "<p>*not em*</p>\n"},
		{"snake_case_name stays", "<p>snake_case_name stays</p>\n"},
		{"***both***", "<p><em><strong>both</strong></em></p>\n"},
	}
	for _, c := range cases {
		if got := render(c.in); got != c.want {
			t.Errorf("Render(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

func TestTitleSummarySlug(t *testing.T) {
	src := "# Post with *one* request\n\nNo API key, no [signup](https://example.com).\n\nMore."
	if got := Title(src); got != "Post with one request" {
		t.Fatalf("title %q", got)
	}
	if got := Summary(src); got != "No API key, no signup." {
		t.Fatalf("summary %q", got)
	}
	if got := Title("Plain first line\n# Later"); got != "" {
		t.Fatalf("a post that does not open with a heading has no heading title, got %q", got)
	}
	if got := Slug("Post with one HTTP request — fast!", 60); got != "post-with-one-http-request-fast" {
		t.Fatalf("slug %q", got)
	}
	if got := Slug("日本語", 60); got != "" {
		t.Fatalf("non-ASCII slug %q", got)
	}
	if got := Slug(strings.Repeat("ab-", 100), 60); len(got) > 60 || strings.HasSuffix(got, "-") {
		t.Fatalf("slug bound %q", got)
	}
	if got := Clip("one two three four five", 12); got != "one two…" {
		t.Fatalf("clip %q", got)
	}
	if got := string(Render("# Title\n\nbody", Options{SkipTitle: true})); got != "<p>body</p>\n" {
		t.Fatalf("skip title %q", got)
	}
	if got := string(Render("## Install\n\n[go](#Install)", Options{Anchors: true})); !strings.Contains(got, `<h3 id="md-install">`) || !strings.Contains(got, `href="#md-install"`) {
		t.Fatalf("anchors %q", got)
	}
}

// tagRE lists every tag in the output. Only this file's fixed vocabulary may appear.
var tagRE = regexp.MustCompile(`<(/?)([a-zA-Z0-9]+)([^>]*)>`)
var allowedTags = map[string]bool{"p": true, "h2": true, "h3": true, "h4": true, "em": true, "strong": true, "code": true, "pre": true, "ul": true, "ol": true, "li": true, "blockquote": true, "hr": true, "br": true, "a": true, "bdi": true, "div": true, "table": true, "thead": true, "tbody": true, "tr": true, "th": true, "td": true}
var attrRE = regexp.MustCompile(`^(?: (?:href="(?:https?://|/|#md-)[^"]*"|rel="nofollow noopener ugc"|class="md-(?:host|table|left|right|center)"|dir="ltr"|start="\d+"|id="md-[a-z0-9-]+"))*$`)

// checkSafe asserts the structural safety properties on any output.
func checkSafe(t testing.TB, src, out string) {
	t.Helper()
	for _, m := range tagRE.FindAllStringSubmatch(out, -1) {
		if !allowedTags[strings.ToLower(m[2])] {
			t.Fatalf("unexpected tag %q in output for %q:\n%s", m[0], src, out)
		}
		if m[1] == "" && !attrRE.MatchString(m[3]) {
			t.Fatalf("unexpected attributes %q for %q:\n%s", m[3], src, out)
		}
	}
	lower := strings.ToLower(out)
	// A protocol-relative href leaves the site without the external-host badge.
	if strings.Contains(out, `href="//`) {
		t.Fatalf("protocol-relative href for %q:\n%s", src, out)
	}
	for _, bad := range []string{"javascript:", "vbscript:", "data:", "<script", "<img", "<iframe", "<svg", "<style", "onerror", "onload", "srcdoc", "style="} {
		if strings.Contains(lower, bad) && !strings.Contains(strings.ToLower(src), bad) {
			t.Fatalf("output introduced %q for %q", bad, src)
		}
		// When the input contains the string it must only survive as escaped text,
		// never inside a tag.
		for _, m := range tagRE.FindAllString(lower, -1) {
			if strings.Contains(m, bad) {
				t.Fatalf("%q reached a tag for %q: %s", bad, src, m)
			}
		}
	}
	// Tags must balance: the renderer never leaves an element open.
	var stack []string
	for _, m := range tagRE.FindAllStringSubmatch(out, -1) {
		name := strings.ToLower(m[2])
		if name == "br" || name == "hr" {
			continue
		}
		if m[1] == "" {
			stack = append(stack, name)
			continue
		}
		if len(stack) == 0 || stack[len(stack)-1] != name {
			t.Fatalf("unbalanced </%s> for %q:\n%s", name, src, out)
		}
		stack = stack[:len(stack)-1]
	}
	if len(stack) != 0 {
		t.Fatalf("unclosed %v for %q:\n%s", stack, src, out)
	}
	if !utf8.ValidString(out) && utf8.ValidString(src) {
		t.Fatalf("invalid UTF-8 output for %q", src)
	}
	if len(out) > 40*len(src)+64 {
		t.Fatalf("output amplification %d -> %d for %q", len(src), len(out), src)
	}
}

func TestXSSCorpus(t *testing.T) {
	corpus := []string{
		"<script>alert(1)</script>",
		"<img src=x onerror=alert(1)>",
		"<a href=\"javascript:alert(1)\">x</a>",
		"[x](javascript:alert(1))",
		"[x](JaVaScRiPt:alert(1))",
		"[x](java\tscript:alert(1))",
		"[x]( javascript:alert(1) )",
		"[x](data:text/html;base64,PHNjcmlwdD4=)",
		"[x](vbscript:msgbox)",
		"[x](//evil.example/path)",
		"[x](/\\evil.example)",
		"[x](https://user:pass@evil.example)",
		"[x](https://swarmmemo.com@evil.example/)",
		"[x](https://evil.example\" onmouseover=\"alert(1))",
		"[x](https://evil.example/\"><script>alert(1)</script>)",
		"[x](https://еvil.example/)", // Cyrillic е: non-ASCII host refused
		"[x](https://evil.example/\u202egnp.exe)",
		"<javascript:alert(1)>",
		"<https://ok.example/\"onmouseover=alert(1)>",
		"![x](https://evil.example/pixel.png)",
		"![x](javascript:alert(1))",
		"[x](https://swarmmemo.com/w/lobby/main?text=spam)",
		"[x](/w64/lobby/main/aGk)",
		"[x](/c64/eyJ9)",
		"[x](https://swarmmemo.com/%77/lobby/main?text=x)",
		"[x](https://swarmmemo.com/./w/lobby/main?text=x)",
		"[x](/v1/command)",
		"https://swarmmemo.com/w/lobby/main?text=spam",
		"`<script>`",
		"```\n</code></pre><script>alert(1)</script>\n```",
		"# <img src=x onerror=alert(1)>",
		"| <b>a</b> | b |\n|---|---|\n| <i onclick=x>1</i> | 2 |",
		"> <iframe srcdoc=x>",
		"- <svg onload=alert(1)>",
		"**[x](https://ok.example)**",
		"[**x**](https://ok.example)",
		"[x [y](https://a.example)](https://b.example)",
		"[x](https://a.example 'title\" onmouseover=\"x')",
		"\u202eevil [link](https://ok.example) text",
		"\u2067[x](https://ok.example)\u2069",
		"*a **b* c**",
		"**unclosed *emphasis",
		"[unclosed link(https://x.example",
		"```\nunclosed fence",
		strings.Repeat("> ", 500) + "deep quote",
		strings.Repeat("- ", 500) + "deep list",
		strings.Repeat("  ", 200) + "- indented",
		strings.Repeat("[", 5000) + strings.Repeat("]", 5000),
		strings.Repeat("`", 3000),
		strings.Repeat("*a", 4000),
		strings.Repeat("_", 8000),
		"|" + strings.Repeat(" a |", 200) + "\n|" + strings.Repeat("---|", 200),
		"| a | b |\n|---|---|\n" + strings.Repeat("|\n", 5000),
		"| a |\n|---|\n" + strings.Repeat("| "+strings.Repeat("x|", 40)+"\n", 200),
		"&lt;script&gt; &amp; &#x3C;",
		"\x00\x01\x7f",
		"\xff\xfe invalid utf8",
	}
	for _, src := range corpus {
		out := render(src)
		checkSafe(t, src, out)
		checkSafe(t, src, string(Render(src, Options{Anchors: true, SkipTitle: true})))
	}
	// Specific refusals must leave no link at all.
	for _, src := range []string{"[x](javascript:alert(1))", "[x](//evil.example/path)", "[x](https://user:pass@evil.example)", "[x](https://swarmmemo.com/w/lobby/main?text=spam)", "[x](https://еvil.example/)", "<javascript:alert(1)>", "[x](/v1/command)", "[x](https://swarmmemo.com/%77/lobby/main?text=x)"} {
		if out := render(src); strings.Contains(out, "<a ") {
			t.Errorf("refused destination rendered a link for %q: %s", src, out)
		}
	}
	// Encoded slashes must not re-encode into a protocol-relative (off-site)
	// href that renders as a same-site link without the host badge.
	for _, raw := range []string{"/%2Fevil.example/{", "/%2F%2Fevil.example/{", "/%2f/evil.example/ä", "/%2F%2Fevil.example/a\"b"} {
		if href, _, ok := SafeURL(raw); ok && (strings.HasPrefix(href, "//") || !strings.HasPrefix(href, "/")) {
			t.Errorf("SafeURL(%q) = %q: off-site href accepted as same-site", raw, href)
		}
		if out := render("[Docs](" + raw + ")"); strings.Contains(out, `href="//`) {
			t.Errorf("protocol-relative href for %q: %s", raw, out)
		}
	}
	// Every same-site href the renderer emits stays on this site.
	for _, raw := range []string{"/docs", "/%2F%2Fevil.example/", "/.//evil.example"} {
		if href, host, ok := SafeURL(raw); ok && (host != "" || strings.HasPrefix(href, "//")) {
			t.Errorf("SafeURL(%q) = %q, %q", raw, href, host)
		}
	}
	// Raw HTML is shown, escaped, as text.
	if out := render("<b>x</b>"); out != "<p>&lt;b&gt;x&lt;/b&gt;</p>\n" {
		t.Fatalf("raw HTML %q", out)
	}
	// An image is never embedded; it degrades to a nofollow link.
	if out := render("![alt](https://example.com/i.png)"); strings.Contains(out, "<img") || !strings.Contains(out, `<a href="https://example.com/i.png"`) {
		t.Fatalf("image %q", out)
	}
	// The destination host sits in its own left-to-right isolate, after a label
	// that is isolated too, so an override cannot reorder what the reader checks.
	out := render("\u202e[click](https://real.example)")
	if !strings.Contains(out, "<bdi>click</bdi></a><bdi class=\"md-host\" dir=\"ltr\">real.example</bdi>") {
		t.Fatalf("bidi isolation %q", out)
	}
}

func TestLinearTime(t *testing.T) {
	inputs := []string{
		strings.Repeat("[", 16000),
		strings.Repeat("`a", 8000),
		strings.Repeat("*a ", 5000),
		strings.Repeat("a_", 8000),
		strings.Repeat("[a](", 4000),
		strings.Repeat("<http", 3000),
		strings.Repeat("> - ", 4000),
		strings.Repeat("|", 16000) + "\n" + strings.Repeat("-|", 8000),
		strings.Repeat("https://a.b/(", 1200),
	}
	for _, src := range inputs {
		start := time.Now()
		out := render(src)
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Fatalf("render took %s for %q...", elapsed, src[:20])
		}
		checkSafe(t, src, out)
	}
}

// TestScaling compares an adversarial shape with a benign baseline of the same
// or a quarter of its size. Each shape was once quadratic, or cost per byte
// that grew with maxURLBytes.
func TestScaling(t *testing.T) {
	fill := func(unit string, n int) string { return strings.Repeat(unit, n/len(unit)+1)[:n] }
	const n = 16 << 10
	cases := []struct {
		name            string
		baseline, shape string
		limit           int // shape may cost at most limit times the baseline
	}{
		// Every newline rebuilt the pending text run: 4x the input was ~16x the time.
		{"long paragraph", fill("a\n", n/4), fill("a\n", n), 8},
		{"lazy quote", "> a\n" + fill("b\n", n/4), "> a\n" + fill("b\n", n), 8},
		// Every trimmed ")" recounted the URL: cost grew with the tail length.
		{"bare URL tails", fill(strings.Repeat("-http://_", 113)+strings.Repeat(")", 64)+" ", n), fill(strings.Repeat("-http://_", 113)+strings.Repeat(")", 2000)+" ", n), 3},
		// Every "[](" scanned maxURLBytes through the ones after it.
		{"open destinations", fill("[](xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx", n), fill("[](", n), 4},
	}
	cost := func(src string) time.Duration {
		best := time.Hour
		for k := 0; k < 5; k++ {
			start := time.Now()
			_ = Render(src, Options{})
			_ = PlainText(src)
			best = min(best, time.Since(start))
		}
		return best
	}
	for _, c := range cases {
		base, shape := cost(c.baseline), cost(c.shape)
		if shape > time.Millisecond && shape > time.Duration(c.limit)*base {
			t.Errorf("%s: baseline %v, shape %v (limit %dx)", c.name, base, shape, c.limit)
		}
	}
}

func FuzzRender(f *testing.F) {
	for _, seed := range []string{"# T\n\n*a* **b** `c` [d](https://e.example) <https://f.example>\n\n- g\n  - h\n1. i\n\n> j\n\n```\nk\n```\n\n| l | m |\n|---|:-:|\n| n | o |\n\n---", "[x](javascript:alert(1))", "<script>", "\u202e[x](https://y.example)", "***a**b*", "- [x](/docs)\n\n  para"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, src string) {
		if len(src) > 16<<10 {
			return
		}
		checkSafe(t, src, render(src))
		checkSafe(t, src, string(Render(src, Options{Anchors: true, SkipTitle: true})))
		_ = Title(src)
		_ = Summary(src)
		plain := PlainText(src)
		if strings.Contains(plain, "<a href") {
			t.Fatalf("plain text contains markup")
		}
	})
}
