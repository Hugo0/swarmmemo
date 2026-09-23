package roomstyle

import (
	"encoding/json"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestSanitizedCSSParsesAlikeInBrowser sanitizes a few tens of thousands of
// generated hostile stylesheets (soup of CSS fragments, and rules built from
// hooks, properties and values chosen to confuse a tokenizer), then has
// Chromium parse each output with testdata/differential.cjs and checks that it
// reads what the sanitizer meant: the same rules, every selector scoped, no
// fetch but the allowed forms, no page-zone property from the body list. It
// needs Playwright, so it runs only when asked:
//
//	SWARMMEMO_ROOMSTYLE_BROWSER=1 PLAYWRIGHT_MODULE=... CHROMIUM_PATH=... NODE=... \
//	go test -run TestSanitizedCSSParsesAlikeInBrowser ./internal/roomstyle
func TestSanitizedCSSParsesAlikeInBrowser(t *testing.T) {
	if os.Getenv("SWARMMEMO_ROOMSTYLE_BROWSER") != "1" {
		t.Skip("set SWARMMEMO_ROOMSTYLE_BROWSER=1 with PLAYWRIGHT_MODULE and CHROMIUM_PATH to run")
	}
	seed, _ := strconv.ParseInt(os.Getenv("ROOMSTYLE_DIFF_SEED"), 10, 64) // another seed explores further
	r := rand.New(rand.NewSource(seed))
	var inputs []string
	paths, _ := filepath.Glob("examples/*.css")
	for _, path := range paths {
		if src, err := os.ReadFile(path); err == nil {
			inputs = append(inputs, string(src))
		}
	}
	for i := 0; i < 30000; i++ {
		var b strings.Builder
		for j := 0; j < 1+r.Intn(3); j++ {
			b.WriteString(diffRule(r))
		}
		inputs = append(inputs, b.String())
	}
	for i := 0; i < 60000; i++ {
		in := diffSoup(r)
		switch r.Intn(4) {
		case 0:
			in = ".post-body p{" + in + "}"
		case 1:
			in = ".post{" + in + "}"
		case 2:
			in += "{color:red}"
		}
		inputs = append(inputs, in)
	}
	type sanitized struct {
		In    string `json:"in"`
		Out   string `json:"out"`
		Rules int    `json:"rules"`
	}
	var cases []sanitized
	seen := map[string]bool{}
	for _, in := range inputs {
		out, _, err := SanitizeWith(in, room, roomOptions)
		if err != nil || strings.TrimSpace(out.CSS) == "" || seen[out.CSS] {
			continue
		}
		seen[out.CSS] = true
		values, _ := parseValues(out.CSS)
		cases = append(cases, sanitized{In: in, Out: out.CSS, Rules: ruleCount(parseRules(values, true))})
	}
	data, err := json.Marshal(map[string]any{"scope": ScopeClass(room), "cases": cases})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "cases.json")
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	node := os.Getenv("NODE")
	if node == "" {
		node = "node"
	}
	out, err := exec.Command(node, filepath.Join("testdata", "differential.cjs"), file).CombinedOutput()
	t.Logf("%s", out)
	if err != nil {
		t.Fatalf("the browser read sanitized CSS differently: %v", err)
	}
}

// ruleCount is how many rules the browser should see: every rule, plus those
// inside grouping rules and keyframes.
func ruleCount(list []rule) int {
	n := 0
	for _, r := range list {
		n++
		if r.at != "" && r.block != nil {
			switch asciiLower(r.at) {
			case "media", "supports", "container", "layer":
				n += ruleCount(parseRules(r.block.kids, false))
			case "keyframes":
				n += len(parseRules(r.block.kids, false))
			}
		}
	}
	return n
}

// Fragments for the soup generator.
var diffFragments = []string{
	"p", ".post", ".post-body", ".byline", " ", "  ", "\n", "\t", "{", "}", "(", ")", "[", "]", ";", ":", ",", "!important", "! important",
	"/*", "*/", "/**/", "\\", "\\\n", "\\0", "\\20", "\\7b", "\\7d", "\\3b", "\\29", "\\28", "\\22", "\\27", "\\5c", "\\2f", "\\3a", "\\40",
	"\"", "'", "\"x\"", "'y'", "url(", "url(x)", "url(/a/0123456789abcdef0123456789abcdef)", "url('/a/0123456789abcdef0123456789abcdef')",
	"u\\72l(", "\\75rl(", "URL(", "src(", "image-set(", "var(--x)", "var(--x,", "--x", "--x:", "env(", "attr(title)", "calc(", "-", "--", "+", ".", "#", "#x", "@", "@media", "@supports", "@layer", "@import", "@font-face", "@keyframes", "@container", "@scope", "@property", "@\\69mport", "&", "&:hover", ">", "~", "*", "|",
	"<!--", "-->", "\r", "\f", "\x00", " ", " ", "é", "�", "\x7f", "\x01", "\x0b",
	"position", "position:fixed", "z-index:9", "color:red", "background:", "content:", "display:none", "order:9", "a", "b", "1", "1e3", "-1px", "1px", "100%", "u+0-7f", "U+", "?",
	":has(", ":not(", ":is(", "::before", ":before", ":scope", ":where(", "nth-child(2n+1)", "e3", "-\\31", "\\-",
	"<", "</style>", "!", "=", "^=", "$=", "[href", "[x=\"", "i]", "}}", "{{", ";;",
}

func diffSoup(r *rand.Rand) string {
	var b strings.Builder
	n := 3 + r.Intn(25)
	for i := 0; i < n; i++ {
		b.WriteString(diffFragments[r.Intn(len(diffFragments))])
	}
	return b.String()
}

// Pieces for the rule generator.
var diffSelectors = []string{".post", ".post-body", ".post-body p", ".post-body > p", ".byline", ".post-meta", ":scope", ":scope .feed", ".post-body a:hover", ".post-body :is(p,a)", ".post-body:has(> p)", ".post-body ~ .post-footer", ".post-body + .post-footer", ".post .post-body *", "p", "a[href^=\"/\"]", ".post-body [title=\"x\\\"y\"]", ".post-body::before", ".post-body p::first-line", "*", ".post-body .md-table td", ".\\70 ost", ".po\\st-body", ".post-\\62 ody p", ".post-body\\ p", ".post-body/**/p", ".post-body\tp", ".nav a", "body", ".post-body:not(.post-text)"}
var diffProperties = []string{"color", "background", "background-image", "position", "z-index", "margin", "margin-left", "display", "content", "font-family", "--x", "--ink", "transform", "opacity", "width", "height", "grid-template-columns", "cursor", "list-style", "border-image", "mask", "filter", "src", "letter-spacing", "text-decoration", "font", "animation", "inset", "all", "white-space", "c\\olor", "posit\\ion", "\\-\\-x", "-webkit-transform"}
var diffValues = []string{"red", "#fff", "1px", "-1px", "0", "calc(1px + 2px)", "calc(1px - 2px)", "var(--x)", "url(/a/0123456789abcdef0123456789abcdef)", "url('/a/0123456789abcdef0123456789abcdef')", "url( /a/0123456789abcdef0123456789abcdef )", "url(/a/0123456789abcdef0123456789abcdef?x)", "url(/w/lobby/main?text=x)", "url(//evil.example/x)", "u\\rl(//e)", "\"str\"", "'s\\'t'", "\"a\\\nb\"", "\"unterminated", "fixed", "absolute", "none", "block", "contents", "attr(title)", "env(safe-area-inset-top)", "image-set(\"x\" 1x)", "-webkit-image-set(url(/a/0123456789abcdef0123456789abcdef) 1x)", "cross-fade(url(x), url(y))", "element(#x)", "paint(x)", "rgb(0 0 0 / 50%)", "rgba(0,0,0,.5)", "transparent", "currentcolor", "\\75rl(x)", "1e3px", "+1px", "-0px", "infinity", "cos(180deg)", "sibling-index()", "if(style(--x:1): red; else: blue)", "attr(data-x type(<color>))", "inherit", "revert-layer", "!important", "! important", "!IMPORTANT", "/* c */red", "red/**/!important", "{", "}", ";", "(", ")", "[", "]", "\\", "\\\n", "<!--", "-->", "\x00", "\u00a0", "\ufeff", "@media", "url(data:image/png;base64,iVBORw0KGgo=)", "url(\"data:image/svg+xml,<svg/>\")", "light-dark(red, blue)", "color-mix(in srgb, red, transparent)", "rgb(from red r g b / 0)", "anchor(top)", "anchor-size(width)", "counter(x)", "counters(x, '.')", "var(--x, url(//e))", "progress(1px from 0px to 2px)", "random(1px, 2px)"}
var diffWrappers = []string{"@media (min-width:1px){%s}", "@supports (display:grid){%s}", "@layer x{%s}", "@container (min-width:1px){%s}", "@media screen and (hover:hover){@supports selector(:has(a)){%s}}", "@scope (.post){%s}", "@starting-style{%s}", "@font-face{font-family:f;src:url(/a/0123456789abcdef0123456789abcdef)}%s", "@keyframes k{from{color:red}to{transform:none}}%s", "@import url(x);%s", "%s", "%s", "%s", "@media{%s", "@page{%s}", "@property --x{syntax:'<color>';inherits:true;initial-value:red}%s", "@view-transition{navigation:auto}%s", "@position-try --x{top:0}%s", "@counter-style x{symbols:a}%s", "@font-palette-values --x{font-family:f}%s", "@function --f(){result:red}%s", "@mixin --m(){color:red}%s", ".post{%s}", ".post-body{%s}"}

func diffDeclarations(r *rand.Rand) string {
	var b strings.Builder
	for i := 0; i < 1+r.Intn(4); i++ {
		b.WriteString(diffProperties[r.Intn(len(diffProperties))])
		b.WriteString([]string{":", ": ", " :", ":\n"}[r.Intn(4)])
		for j := 0; j < 1+r.Intn(3); j++ {
			if j > 0 {
				b.WriteString([]string{" ", ",", "", "/**/"}[r.Intn(4)])
			}
			b.WriteString(diffValues[r.Intn(len(diffValues))])
		}
		b.WriteString([]string{";", ";", "", "!important;"}[r.Intn(4)])
	}
	return b.String()
}

func diffRule(r *rand.Rand) string {
	sel := diffSelectors[r.Intn(len(diffSelectors))]
	if r.Intn(3) == 0 {
		sel += "," + diffSelectors[r.Intn(len(diffSelectors))]
	}
	rule := sel + "{" + diffDeclarations(r) + "}"
	if r.Intn(4) == 0 {
		rule += "{" + diffDeclarations(r) + "}" // nested / stray block
	}
	if r.Intn(5) == 0 {
		rule = sel + "{" + diffDeclarations(r) + diffSelectors[r.Intn(len(diffSelectors))] + "{" + diffDeclarations(r) + "}}" // CSS nesting
	}
	return strings.Replace(diffWrappers[r.Intn(len(diffWrappers))], "%s", rule, 1)
}
