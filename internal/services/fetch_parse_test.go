package services_test

import (
	"strings"
	"testing"
	"time"
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
		{"<ol><li>a<li>b</ol>", "- a\n- b"},
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

// The converter's work is linear in the page: adversarial pages of the
// largest size a fetch reads convert quickly.
func TestHTMLToTextIsLinear(t *testing.T) {
	n := services.FetchBodyBytes
	for name, doc := range map[string]string{
		"open tags":    strings.Repeat("<p>", n/3),
		"open links":   strings.Repeat("<a href=x>y", n/11),
		"raw no close": strings.Repeat("<title>", n/7),
		"near close":   "<script>" + strings.Repeat("</scrip", n/7),
		"attributes":   "<a " + strings.Repeat("b=c ", n/4),
		"lt run":       strings.Repeat("<", n),
		"entities":     strings.Repeat("&amp;", n/5),
		"nested skip":  strings.Repeat("<nav>", n/5),
		"pre":          strings.Repeat("<pre>x", n/6),
		"cells":        strings.Repeat("<td>x", n/5),
		"short links":  strings.Repeat("<a href=x>y</a>", n/15),
	} {
		start := time.Now()
		_, text := services.HTMLToTextForTest(doc, "https://site.example/"+strings.Repeat("d", 1900)+"/")
		if len(text) > services.FetchTextMax+64<<10 {
			t.Errorf("%s: %d bytes of text", name, len(text))
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("%s: %v", name, d)
		}
	}
	for name, body := range map[string]string{
		"stars":  "User-agent: *\nDisallow: /" + strings.Repeat("*a", 500) + "\n",
		"many":   "User-agent: *\n" + strings.Repeat("Disallow: /a*b*c*d\n", 5000),
		"agents": strings.Repeat("User-agent: *\n", 20000) + strings.Repeat("Disallow: /a*a*a*b\n", 2000),
		"heavy":  "User-agent: *\n" + strings.Repeat("Disallow: /"+strings.Repeat("*a", 500)+"b\n", 2000),
	} {
		start := time.Now()
		services.RobotsAllowedForTest(body, "/"+strings.Repeat("a", services.FetchURLBytes))
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("robots %s: %v", name, d)
		}
	}
}
