package services_test

import (
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"swarmmemo/internal/services"
)

func TestRobotsRules(t *testing.T) {
	const robots = `# comment
User-agent: Googlebot
Disallow: /

User-agent: *
Disallow: /tmp
Allow: /tmp/public

User-agent: other
User-agent: SwarmMemoFetch/1.0
Disallow: /*.pdf$
Disallow: /search?
Allow: /search?q=allowed
Disallow: /admin
Disallow:
`
	cases := map[string]bool{
		"/":                 true,
		"/tmp/x":            true, // the * group does not apply when ours exists
		"/doc.pdf":          false,
		"/doc.pdf?x=1":      true, // $ anchors the end
		"/a/b/c.pdf":        false,
		"/search?q=no":      false,
		"/search?q=allowed": true, // longer allow wins
		"/search":           true,
		"/admin":            false,
		"/administrator":    false,
		"/Admin":            true, // paths are case-sensitive
	}
	for path, want := range cases {
		if got := services.RobotsAllowedForTest(robots, path); got != want {
			t.Errorf("%s: allowed %v, want %v", path, got, want)
		}
	}
	star := "User-agent: *\nDisallow: /private\nAllow: /private/ok\nDisallow: /p*x\n"
	for path, want := range map[string]bool{"/private/a": false, "/private/ok/1": true, "/pages/x1": false, "/pages/y": true, "/": true} {
		if got := services.RobotsAllowedForTest(star, path); got != want {
			t.Errorf("* group %s: %v, want %v", path, got, want)
		}
	}
	if !services.RobotsAllowedForTest("", "/anything") || !services.RobotsAllowedForTest("garbage\x00\xff", "/") {
		t.Fatal("no rules allow everything")
	}
	if services.RobotsAllowedForTest("User-agent: SwarmMemoFetch\nDisallow: /\n", "/x") {
		t.Fatal("a full disallow for our token")
	}
	if !services.RobotsAllowedForTest("User-agent: SwarmMemoFetchX\nDisallow: /\n", "/x") {
		t.Fatal("another token is not ours")
	}
}

func TestHTMLToText(t *testing.T) {
	cases := []struct{ in, want string }{
		{"<p>a</p><p>b</p>", "a\n\nb"},
		{"x <!-- hidden --> y", "x y"},
		{"<h1>T</h1>text", "# T\n\ntext"},
		{"<ol><li>a<li>b</ol>", "1. a\n2. b"},
		{"<ul><li>a<ul><li>b</li></ul></li><li>c</li></ul>", "- a\n  - b\n- c"},
		{"run <code>go test</code> now", "run `go test` now"},
		{"<a href=/x>the <code>x</code> page</a>", "[the x page](https://site.example/x)"},
		{"<head><title>t</title><meta charset=utf-8><body><h1>Kept</h1>", "# Kept"}, // </head> may be left out
		{"<form><p>inside a form</p><button>Go</button></form>", "inside a form"},
		{"<div role=navigation><a href=/m>Menu</a></div>rest", "rest"},
		{"<a href='javascript:alert(1)'>click</a>", "click"},
		{"<a href='data:text/html,x'>d</a> and <a href=\"https://e.example/p?q=1#f\">e</a>", "d and [e](https://e.example/p?q=1)"},
		{"<scr<script>ipt>alert(1)</script>", "ipt>alert(1)"}, // a tag "scr" whose attribute is "<script", as a browser reads it
		{"<SCRIPT>x</SCRIPT >y", "y"},
		{"<style>a{}</STYLE>z", "z"},
		{"a&lt;b&gt;c &amp;amp; &#x41;", "a<b>c &amp; A"},
		{"<table><tr><td>1</td><td>2</td></tr><tr><td>3</td></tr></table>", "1 | 2\n3"},
		{"<img src=x alt='a cat'>", "[image: a cat]"},
		{"<template><p>t</p></template>ok", "ok"},
		{"<div><nav><nav>n</nav>m</nav>after</div>", "after"},
		{"unclosed <b", "unclosed"},
		{"<p>tab\tand\n\nnewlines   collapse</p>", "tab and newlines collapse"},
	}
	for _, c := range cases {
		if _, got := services.HTMLToTextForTest(c.in, "https://site.example/dir/"); got != c.want {
			t.Errorf("%q: got %q, want %q", c.in, got, c.want)
		}
	}
	if title, _ := services.HTMLToTextForTest("<head><title> Hi  there </title></head>", ""); title != "Hi there" {
		t.Fatalf("title %q", title)
	}
}

// FuzzHTMLToText: any page converts without panicking, in bounded output,
// to valid UTF-8 with no script or style text.
func FuzzHTMLToText(f *testing.F) {
	for _, seed := range []string{"<p>a</p>", "<a href=/x>y</a>", "<script>s</script>", "<!--", "<![CDATA[x]]>", "<pre><pre>x</pre>", "<a href='", "<<<>>>", "<title>t", "\xff\xfe<p>"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, doc string) {
		if len(doc) > 1<<16 {
			return
		}
		valid := strings.ToValidUTF8(doc, "�")
		title, text := services.HTMLToTextForTest(valid, "https://site.example/a/")
		if !utf8.ValidString(text) || !utf8.ValidString(title) {
			t.Fatalf("invalid UTF-8 out of %q", doc)
		}
		if len(text) > 8*len(valid)+1024 || len(title) > 300 {
			t.Fatalf("output %d bytes from %d", len(text), len(valid))
		}
		if strings.Contains(valid, "<script>SECRET</script>") && !strings.Contains(valid, "<![CDATA[") && strings.Contains(text, "SECRET") && !strings.Contains(strings.ReplaceAll(valid, "<script>SECRET</script>", ""), "SECRET") {
			t.Fatalf("script text leaked from %q: %q", doc, text)
		}
	})
}

// FuzzRobots: any robots.txt parses without panicking, and a pattern with
// no wildcard is a plain prefix rule.
func FuzzRobots(f *testing.F) {
	for _, seed := range []string{"User-agent: *\nDisallow: /", "User-agent: SwarmMemoFetch\nAllow: /a*b$\n", "Disallow: *\n", "user-agent:*\r\ndisallow:/x$$"} {
		f.Add(seed, "/a/b")
	}
	f.Fuzz(func(t *testing.T, body, path string) {
		if len(body) > 1<<16 || len(path) > 4096 {
			return
		}
		_ = services.RobotsAllowedForTest(body, path)
		rule := strings.Map(func(r rune) rune {
			if r == '*' || r == '$' || r == '\n' || r == '\r' || r == '#' {
				return -1
			}
			return r
		}, path)
		rule = strings.TrimSpace(rule)
		if rule == "" || len(rule) > 1024 || !strings.HasPrefix(rule, "/") || !utf8.ValidString(rule) {
			return
		}
		got := services.RobotsAllowedForTest("User-agent: *\nDisallow: "+rule+"\n", path)
		if want := !strings.HasPrefix(path, rule); got != want {
			t.Fatalf("Disallow %q on %q: allowed %v", rule, path, got)
		}
	})
}

// The converter's work is linear in the page, counted, not timed: on
// adversarial pages at four sizes up to the largest a fetch reads, its work
// per byte of page (convertHTML) does not grow with the size, as it would
// about tenfold per step if any part were quadratic, and at the largest size
// it stays under htmlWorkPerByte. Each page is tried smallest first and
// stops at its first failure, so a quadratic converter fails in
// milliseconds instead of running for minutes on the big pages.
func TestHTMLToTextIsLinear(t *testing.T) {
	const (
		htmlWorkPerByte = 16  // at the largest size; the worst page here ("lt run") measures 5
		growthMax       = 1.5 // per-byte work at 10x the size over that at 1x: linear is at most 1, quadratic 10
	)
	n := services.FetchBodyBytes
	base := "https://site.example/" + strings.Repeat("d", 1900) + "/"
	// A link resolves against the page's URL: the link pages below make
	// every resolution yield no text, one against a base far longer than a
	// fetch's URL can be.
	bases := map[string]string{"bad links, huge base": "https://site.example/" + strings.Repeat("d/", 3*services.FetchURLBytes)}
	for name, page := range map[string]func(n int) string{
		"open tags":    func(n int) string { return strings.Repeat("<p>", n/3) },
		"open links":   func(n int) string { return strings.Repeat("<a href=x>y", n/11) },
		"raw no close": func(n int) string { return strings.Repeat("<title>", n/7) },
		"near close":   func(n int) string { return "<script>" + strings.Repeat("</scrip", n/7) },
		"attributes":   func(n int) string { return "<a " + strings.Repeat("b=c ", n/4) },
		"lt run":       func(n int) string { return strings.Repeat("<", n) },
		"entities":     func(n int) string { return strings.Repeat("&amp;", n/5) },
		"nested skip":  func(n int) string { return strings.Repeat("<nav>", n/5) },
		"pre":          func(n int) string { return strings.Repeat("<pre>x", n/6) },
		"cells":        func(n int) string { return strings.Repeat("<td>x", n/5) },
		"short links":  func(n int) string { return strings.Repeat("<a href=x>y</a>", n/15) },
		"comments":     func(n int) string { return strings.Repeat("<!--", n/4) },
		"cdata":        func(n int) string { return strings.Repeat("<![CDATA[", n/9) },
		"open quote":   func(n int) string { return strings.Repeat("<a href=\"x", n/10) },
		"digit tags":   func(n int) string { return strings.Repeat("<1", n/2) },
		// Links whose resolution yields no text: a target fetch refuses to
		// print, an empty label, an attribute left open.
		"bad links":            func(n int) string { return strings.Repeat("<a href=(>", n/10) },
		"empty links":          func(n int) string { return strings.Repeat("<a href=x></a>", n/14) },
		"unclosed attrs":       func(n int) string { return strings.Repeat("<a href=( title=\"></a>", n/22) },
		"bad links, huge base": func(n int) string { return strings.Repeat("<a href=(>", n/10) },
	} {
		prev := 0.0
		pageBase := base
		if b, ok := bases[name]; ok {
			pageBase = b
		}
		for _, size := range []int{n / 1000, n / 100, n / 10, n} {
			doc := page(size)
			text, work := services.HTMLToTextWorkForTest(doc, pageBase)
			if len(text) > services.FetchOffsetMax+4<<10 { // all of it reachable with offset, never more
				t.Errorf("%s at %d bytes: %d bytes of text", name, len(doc), len(text))
			}
			perByte := float64(work) / float64(len(doc))
			t.Logf("%s at %d bytes: work %d, %.2f per byte", name, len(doc), work, perByte)
			if prev > 0 && perByte > growthMax*prev {
				t.Errorf("%s at %d bytes: %.2f work per byte, %.2f at a tenth the size: not linear", name, len(doc), perByte, prev)
				break
			}
			if size == n && perByte > htmlWorkPerByte {
				t.Errorf("%s at %d bytes: %.2f work per byte, over %d", name, len(doc), perByte, htmlWorkPerByte)
			}
			prev = perByte
		}
	}
	// robots.txt: parsing is linear in the file, and a decision's matches
	// take at most (len(path)+2) x (len(pattern)+1) steps for each rule
	// kept, which the caps on the rules kept bound (fetch_robots.go).
	path := "/" + strings.Repeat("a", services.FetchURLBytes)
	limit := (len(path) + 2) * services.RobotsPatternCapForTest
	for name, body := range map[string]string{
		"stars":  "User-agent: *\nDisallow: /" + strings.Repeat("*a", 500) + "\n",
		"many":   "User-agent: *\n" + strings.Repeat("Disallow: /a*b*c*d\n", 5000),
		"agents": strings.Repeat("User-agent: *\n", 20000) + strings.Repeat("Disallow: /a*a*a*b\n", 2000),
		"heavy":  "User-agent: *\n" + strings.Repeat("Disallow: /"+strings.Repeat("*a", 500)+"b\n", 2000),
		// One star then a long tail that fails at its end: every start
		// the star tries matches most of the tail, the worst case.
		"long tail": "User-agent: *\n" + strings.Repeat("Disallow: /*"+strings.Repeat("a", 1000)+"b\n", 40),
	} {
		parse, match, bound := services.RobotsStepsForTest(body, path)
		t.Logf("robots %s: parse %d steps for %d bytes, match %d steps, bound %d", name, parse, len(body), match, bound)
		if parse > 2*len(body) {
			t.Errorf("robots %s: parsing %d bytes took %d steps", name, len(body), parse)
		}
		if match > bound || bound > limit {
			t.Errorf("robots %s: matching took %d steps, bound %d, cap %d", name, match, bound, limit)
		}
	}
}

// The link budget (C1201) leaves a page whose links write their URLs as it
// was: every link of a long page is a link, also against a page URL with a
// long query, which resolving does not read. A page that spends the budget
// keeps the text of its later links.
func TestHTMLLinkBudget(t *testing.T) {
	var b strings.Builder
	for i := range 40000 {
		b.WriteString("<li><a href=\"/p/" + strconv.Itoa(i) + "\">item " + strconv.Itoa(i) + "</a></li>\n")
	}
	base := "https://site.example/list?" + strings.Repeat("q=1&", 400)
	_, text := services.HTMLToTextForTest(b.String(), base)
	if got := strings.Count(text, "](https://site.example/p/"); got != 40000 {
		t.Errorf("%d of 40000 links kept as links", got)
	}
	if !strings.Contains(text, "[item 39999](https://site.example/p/39999)") {
		t.Errorf("last link: %q", text[max(0, len(text)-200):])
	}

	hostile := strings.Repeat("<a href=(>", services.FetchBodyBytes/10-4) + "<p><a href=/x>the end</a>"
	_, text = services.HTMLToTextForTest(hostile, "https://site.example/"+strings.Repeat("d", 1900)+"/")
	if !strings.HasSuffix(text, "the end") || strings.Contains(text, "](") {
		t.Errorf("past the budget: %q", text[max(0, len(text)-200):])
	}
}
